package client

// 客户端本地 REST API：查询连接状态、增删服务端、修改连接参数并热重连。
// 设计目标与 nps 服务端 /api/v1 保持一致：默认关闭（需 api_key），Bearer / X-Api-Key 鉴权，
// 来源白名单、每 IP 限流、只读模式、请求体上限，密码类字段只写不读。

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"ehang.io/nps/lib/common"
	"ehang.io/nps/lib/crypt"
	"ehang.io/nps/lib/version"
	"github.com/astaxie/beego/logs"
)

const (
	// apiMaxBodyBytes 单次请求体上限
	apiMaxBodyBytes = 1 << 20
	// apiDefaultRate 每个来源 IP 每分钟允许的请求数
	apiDefaultRate = 300
	// apiRateMaxSources 限流窗口的 map 上限，防止伪造来源把内存撑爆
	apiRateMaxSources = 8192
)

// 错误码：调用方按 code 分支处理，message 只用于展示
const (
	apiCodeDisabled        = "api_disabled"
	apiCodeUnauthorized    = "unauthorized"
	apiCodeForbidden       = "forbidden"
	apiCodeNotFound        = "not_found"
	apiCodeInvalidArgument = "invalid_argument"
	apiCodeConflict        = "conflict"
	apiCodeRateLimited     = "rate_limited"
	apiCodeReadOnly        = "read_only"
	apiCodeTooLarge        = "payload_too_large"
)

// ApiOptions 是客户端 API 的启动参数
type ApiOptions struct {
	// Addr 监听地址，建议 127.0.0.1:8089，只在本机或内网暴露
	Addr string
	// Key 调用凭据，为空时不会启动 API
	Key string
	// AllowIP 来源白名单，支持 ip / CIDR，逗号分隔；为空表示不限制
	AllowIP string
	// ReadOnly 只读模式，禁止所有写操作
	ReadOnly bool
	// Manager 服务端连接管理器
	Manager *Manager
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type apiResponse struct {
	OK        bool        `json:"ok"`
	Data      interface{} `json:"data,omitempty"`
	Error     *apiError   `json:"error,omitempty"`
	RequestID string      `json:"request_id,omitempty"`
}

// StartApiServer 监听并启动 API，返回的 *http.Server 可用于关闭
func StartApiServer(opt ApiOptions) (*http.Server, error) {
	opt.Addr = strings.TrimSpace(opt.Addr)
	opt.Key = strings.TrimSpace(opt.Key)
	if opt.Addr == "" {
		return nil, errors.New("api addr must not be empty")
	}
	if opt.Key == "" {
		return nil, errors.New("api key must not be empty, refusing to start an unauthenticated api")
	}
	if opt.Manager == nil {
		return nil, errors.New("api manager must not be nil")
	}
	ln, err := net.Listen("tcp", opt.Addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{
		Handler:           newApiHandler(opt),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logs.Error("[api] serve error: %s", err)
		}
	}()
	logs.Info("[api] client api listening on %s", ln.Addr().String())
	return srv, nil
}

// apiHandler 是 API 的入口，独立于 Manager 便于单测
type apiHandler struct {
	opt   ApiOptions
	allow interface {
		Empty() bool
		Contains(net.IP) bool
	}
	started time.Time

	mu        sync.Mutex
	windows   map[string]*apiRateWindow
	lastSweep time.Time
}

type apiRateWindow struct {
	count int
	reset time.Time
}

func newApiHandler(opt ApiOptions) *apiHandler {
	return &apiHandler{
		opt:     opt,
		allow:   common.NewIPList(opt.AllowIP),
		started: time.Now(),
		windows: make(map[string]*apiRateWindow),
	}
}

// apiReply 负责统一响应格式
type apiReply struct {
	w         http.ResponseWriter
	requestID string
	done      bool
}

func (r *apiReply) ok(data interface{}) {
	r.write(http.StatusOK, apiResponse{OK: true, Data: data})
}

func (r *apiReply) fail(status int, code, message string) {
	r.write(status, apiResponse{OK: false, Error: &apiError{Code: code, Message: message}})
}

func (r *apiReply) write(status int, payload apiResponse) {
	if r.done {
		return
	}
	r.done = true
	payload.RequestID = r.requestID
	r.w.Header().Set("Content-Type", "application/json; charset=utf-8")
	r.w.WriteHeader(status)
	_ = json.NewEncoder(r.w).Encode(payload)
}

func (a *apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reply := &apiReply{w: w, requestID: crypt.GetRandomString(8)}
	w.Header().Set("X-Request-Id", reply.requestID)
	w.Header().Set("Cache-Control", "no-store")

	path := strings.TrimSuffix(r.URL.Path, "/")
	// 探活接口不需要凭据，也不返回敏感信息
	if path == "/api/v1/health" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			reply.fail(http.StatusMethodNotAllowed, apiCodeInvalidArgument, "method not allowed")
			return
		}
		reply.ok(map[string]interface{}{
			"status":         "ok",
			"role":           "npc",
			"version":        version.VERSION,
			"uptime_seconds": int(time.Since(a.started).Seconds()),
			"time":           time.Now().Unix(),
		})
		return
	}

	write := r.Method != http.MethodGet && r.Method != http.MethodHead
	if !a.guard(reply, r, write) {
		return
	}

	switch path {
	case "/api/v1/status":
		if !a.allowMethod(reply, r, http.MethodGet) {
			return
		}
		reply.ok(a.opt.Manager.Status())
	case "/api/v1/servers":
		a.handleServers(reply, r)
	case "/api/v1/config":
		a.handleConfig(reply, r)
	case "/api/v1/reconnect":
		if !a.allowMethod(reply, r, http.MethodPost) {
			return
		}
		res, err := a.opt.Manager.Reconnect()
		if err != nil {
			reply.fail(http.StatusBadRequest, apiCodeInvalidArgument, err.Error())
			return
		}
		logs.Info("[api] reconnect requested from %s", r.RemoteAddr)
		reply.ok(res)
	default:
		reply.fail(http.StatusNotFound, apiCodeNotFound, "unknown api path, see /api/v1/status")
	}
}

func (a *apiHandler) allowMethod(reply *apiReply, r *http.Request, methods ...string) bool {
	for _, m := range methods {
		if r.Method == m || (m == http.MethodGet && r.Method == http.MethodHead) {
			return true
		}
	}
	reply.w.Header().Set("Allow", strings.Join(methods, ", "))
	reply.fail(http.StatusMethodNotAllowed, apiCodeInvalidArgument, "method not allowed")
	return false
}

// guard 校验来源白名单、限流与凭据；write 为 true 时还要求非只读模式
func (a *apiHandler) guard(reply *apiReply, r *http.Request, write bool) bool {
	ip := apiIPFromString(r.RemoteAddr)
	if !a.allow.Empty() && !a.allow.Contains(ip) {
		logs.Warn("[api] %s rejected by api_allow_ip, path %s", r.RemoteAddr, r.URL.Path)
		reply.fail(http.StatusForbidden, apiCodeForbidden, "source ip is not in api_allow_ip")
		return false
	}
	if !a.rateAllow(ip, apiDefaultRate) {
		reply.fail(http.StatusTooManyRequests, apiCodeRateLimited, "too many requests, please slow down")
		return false
	}
	if !a.authorized(r) {
		reply.w.Header().Set("WWW-Authenticate", `Bearer realm="npc api"`)
		logs.Warn("[api] unauthorized request from %s, path %s", r.RemoteAddr, r.URL.Path)
		reply.fail(http.StatusUnauthorized, apiCodeUnauthorized, "invalid or missing api credential")
		return false
	}
	if write && a.opt.ReadOnly {
		reply.fail(http.StatusForbidden, apiCodeReadOnly, "the api is running in read-only mode")
		return false
	}
	return true
}

func (a *apiHandler) authorized(r *http.Request) bool {
	if v := strings.TrimSpace(r.Header.Get("X-Api-Key")); v != "" {
		return apiKeyEqual(v, a.opt.Key)
	}
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return false
	}
	scheme, credential, ok := strings.Cut(header, " ")
	if !ok {
		return false
	}
	switch strings.ToLower(scheme) {
	case "bearer", "token":
		return apiKeyEqual(strings.TrimSpace(credential), a.opt.Key)
	}
	return false
}

// apiKeyEqual 使用常量时间比较，避免通过响应时间猜出密钥
func apiKeyEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// rateAllow 固定窗口限流，按来源 IP 每分钟计数
func (a *apiHandler) rateAllow(ip net.IP, limit int) bool {
	if limit <= 0 || ip == nil {
		return true
	}
	key := ip.String()
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if now.Sub(a.lastSweep) >= time.Minute {
		for k, w := range a.windows {
			if now.After(w.reset) {
				delete(a.windows, k)
			}
		}
		a.lastSweep = now
	}
	w, ok := a.windows[key]
	if !ok || now.After(w.reset) {
		if !ok && len(a.windows) >= apiRateMaxSources {
			return false
		}
		a.windows[key] = &apiRateWindow{count: 1, reset: now.Add(time.Minute)}
		return true
	}
	w.count++
	return w.count <= limit
}

// decode 解析 JSON 请求体，未知字段直接报错，避免字段拼错却静默生效
func (a *apiHandler) decode(reply *apiReply, r *http.Request, dst interface{}) bool {
	if r.ContentLength > apiMaxBodyBytes {
		reply.fail(http.StatusRequestEntityTooLarge, apiCodeTooLarge, "request body must be smaller than 1MB")
		return false
	}
	if r.Body == nil {
		reply.fail(http.StatusBadRequest, apiCodeInvalidArgument, "request body is required")
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, apiMaxBodyBytes+1))
	if err != nil {
		reply.fail(http.StatusBadRequest, apiCodeInvalidArgument, "read request body: "+err.Error())
		return false
	}
	if len(body) == 0 {
		reply.fail(http.StatusBadRequest, apiCodeInvalidArgument, "request body is required")
		return false
	}
	if len(body) > apiMaxBodyBytes {
		reply.fail(http.StatusRequestEntityTooLarge, apiCodeTooLarge, "request body must be smaller than 1MB")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		reply.fail(http.StatusBadRequest, apiCodeInvalidArgument, "invalid json body: "+err.Error())
		return false
	}
	return true
}

func (a *apiHandler) handleServers(reply *apiReply, r *http.Request) {
	manager := a.opt.Manager
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		reply.ok(map[string]interface{}{
			"servers":   manager.Servers(),
			"persisted": manager.ConfPath() != "",
		})
	case http.MethodPut:
		var req struct {
			Servers []string `json:"servers"`
		}
		if !a.decode(reply, r, &req) {
			return
		}
		res, err := manager.ApplyServers(req.Servers)
		if err != nil {
			reply.fail(apiStatusFor(err), apiCodeFor(err), err.Error())
			return
		}
		logs.Info("[api] server list replaced from %s: %s", r.RemoteAddr, strings.Join(addrsOf(res.Servers), ","))
		reply.ok(res)
	case http.MethodPost:
		var req struct {
			Addr string `json:"addr"`
		}
		if !a.decode(reply, r, &req) {
			return
		}
		res, err := manager.AddServer(req.Addr)
		if err != nil {
			reply.fail(apiStatusFor(err), apiCodeFor(err), err.Error())
			return
		}
		logs.Info("[api] server %s added from %s", req.Addr, r.RemoteAddr)
		reply.ok(res)
	case http.MethodDelete:
		addr := strings.TrimSpace(r.URL.Query().Get("addr"))
		if addr == "" {
			reply.fail(http.StatusBadRequest, apiCodeInvalidArgument, "query parameter addr is required")
			return
		}
		force := apiParseBool(r.URL.Query().Get("force"))
		res, err := manager.RemoveServer(addr, force)
		if err != nil {
			reply.fail(apiStatusFor(err), apiCodeFor(err), err.Error())
			return
		}
		logs.Info("[api] server %s removed from %s", addr, r.RemoteAddr)
		reply.ok(res)
	default:
		a.allowMethod(reply, r, http.MethodGet, http.MethodPut, http.MethodPost, http.MethodDelete)
	}
}

func (a *apiHandler) handleConfig(reply *apiReply, r *http.Request) {
	manager := a.opt.Manager
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		reply.ok(a.configView())
	case http.MethodPatch:
		var upd ConfigUpdate
		if !a.decode(reply, r, &upd) {
			return
		}
		persisted, err := manager.UpdateConfig(upd)
		if err != nil {
			reply.fail(http.StatusBadRequest, apiCodeInvalidArgument, err.Error())
			return
		}
		logs.Info("[api] connection config updated from %s, persisted=%v", r.RemoteAddr, persisted)
		reply.ok(map[string]interface{}{
			"persisted": persisted,
			"servers":   manager.Servers(),
		})
	default:
		a.allowMethod(reply, r, http.MethodGet, http.MethodPatch)
	}
}

// configView 返回可公开的连接参数视图，密钥类字段一律打码
func (a *apiHandler) configView() map[string]interface{} {
	manager := a.opt.Manager
	st := manager.Status()
	view := map[string]interface{}{
		"mode":              st.Mode,
		"config_path":       st.ConfigPath,
		"conn_type":         st.ConnType,
		"vkey_masked":       st.VKeyMasked,
		"tls_verify":        st.TlsVerify,
		"auto_reconnection": st.AutoReconnection,
		"servers":           st.Servers,
		"server_count":      st.ServerCount,
		"api_addr":          a.opt.Addr,
		"api_read_only":     a.opt.ReadOnly,
	}
	if cfg := manager.FileConfig(); cfg != nil && cfg.CommonConfig != nil {
		view["disconnect_timeout"] = cfg.CommonConfig.DisconnectTime
		view["proxy_url_masked"] = maskProxyURL(cfg.CommonConfig.ProxyUrl)
		view["hosts"] = len(cfg.Hosts)
		view["tasks"] = len(cfg.Tasks)
	}
	if cli, ok := manager.CliSettings(); ok {
		view["disconnect_timeout"] = cli.DisconnectTime
		view["proxy_url_masked"] = maskProxyURL(cli.ProxyUrl)
	}
	return view
}

// maskProxyURL 只保留协议、用户名和主机，密码打码（url.String() 会把 *** 转义成 %2A%2A%2A）
func maskProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "***"
	}
	if u.User == nil {
		return u.String()
	}
	if _, hasPass := u.User.Password(); !hasPass {
		return u.String()
	}
	out := u.Scheme + "://" + u.User.Username() + ":***@" + u.Host + u.EscapedPath()
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

// apiStatusFor 把内部错误映射成 HTTP 状态码
func apiStatusFor(err error) int {
	switch {
	case errors.Is(err, ErrServerExists):
		return http.StatusConflict
	case errors.Is(err, ErrServerNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrLastServer):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

func apiCodeFor(err error) string {
	switch {
	case errors.Is(err, ErrServerExists), errors.Is(err, ErrLastServer):
		return apiCodeConflict
	case errors.Is(err, ErrServerNotFound):
		return apiCodeNotFound
	default:
		return apiCodeInvalidArgument
	}
}

func addrsOf(servers []ServerInfo) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.Addr)
	}
	return out
}

func apiParseBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// apiIPFromString 从 "ip:port" 里取出 IP
func apiIPFromString(addr string) net.IP {
	addr = strings.TrimSpace(addr)
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}
	return net.ParseIP(strings.Trim(addr, "[]"))
}
