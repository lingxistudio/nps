package client

// 多服务端连接的生命周期管理：给每个服务端独立的可停止守护协程，并对外暴露状态快照与变更能力。
// 客户端 API（client/api.go）与命令行启动都通过 Manager 操作服务端列表，避免两套逻辑。

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"ehang.io/nps/lib/config"
	"ehang.io/nps/lib/version"
	"github.com/astaxie/beego/logs"
)

// 服务端连接状态
const (
	ServerStateConnecting = "connecting"
	ServerStateOnline     = "online"
	ServerStateOffline    = "offline"
	ServerStateStopped    = "stopped"
)

// 客户端工作模式
const (
	ModeFile    = "file"
	ModeCommand = "command"
)

var (
	// ErrServerExists 服务端地址已在列表里
	ErrServerExists = errors.New("server already exists")
	// ErrServerNotFound 服务端地址不在列表里
	ErrServerNotFound = errors.New("server not found")
	// ErrLastServer 不允许删掉最后一个服务端（会导致所有隧道中断）
	ErrLastServer = errors.New("refusing to remove the last server, pass force=true to do it anyway")
)

// CliConfig 是命令行 / SDK 模式下的连接参数（该模式没有配置文件可写，改动只在内存生效）
type CliConfig struct {
	VKey           string
	Tp             string
	ProxyUrl       string
	DisconnectTime int
	TlsVerify      bool
}

// ServerInfo 是单个服务端的运行状态快照
type ServerInfo struct {
	Addr       string    `json:"addr"`
	Primary    bool      `json:"primary"`
	State      string    `json:"state"`
	Message    string    `json:"message,omitempty"`
	StateSince time.Time `json:"state_since"`
	Connects   int       `json:"connects"`
	Reconnects int       `json:"reconnects"`
}

// ClientStatus 是整体运行状态快照
type ClientStatus struct {
	Version          string       `json:"version"`
	UptimeSeconds    int          `json:"uptime_seconds"`
	Mode             string       `json:"mode"`
	ConfigPath       string       `json:"config_path,omitempty"`
	ConnType         string       `json:"conn_type"`
	VKeyMasked       string       `json:"vkey_masked"`
	TlsVerify        bool         `json:"tls_verify"`
	AutoReconnection bool         `json:"auto_reconnection"`
	ServerCount      int          `json:"server_count"`
	OnlineCount      int          `json:"online_count"`
	Hosts            int          `json:"hosts"`
	Tasks            int          `json:"tasks"`
	Servers          []ServerInfo `json:"servers"`
}

// ApplyResult 是服务端列表变更的结果
type ApplyResult struct {
	Servers   []ServerInfo `json:"servers"`
	Persisted bool         `json:"persisted"`
}

// ConfigUpdate 是 PATCH /api/v1/config 的请求体，字段用指针以区分「未传」与「传了空值」
type ConfigUpdate struct {
	VKey              *string `json:"vkey,omitempty"`
	ConnType          *string `json:"conn_type,omitempty"`
	ProxyUrl          *string `json:"proxy_url,omitempty"`
	TlsVerify         *bool   `json:"tls_verify,omitempty"`
	AutoReconnection  *bool   `json:"auto_reconnection,omitempty"`
	DisconnectTimeout *int    `json:"disconnect_timeout,omitempty"`
}

// serverRunner 是单个服务端的守护协程句柄与状态
type serverRunner struct {
	addr string
	stop chan struct{}
	done chan struct{}
	once sync.Once

	mu         sync.Mutex
	primary    bool
	state      string
	message    string
	stateSince time.Time
	connects   int
	reconnects int
}

func newServerRunner(addr string, primary bool) *serverRunner {
	return &serverRunner{
		addr:       addr,
		primary:    primary,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		state:      ServerStateConnecting,
		stateSince: time.Now(),
	}
}

// newRunnerFrom 创建新 runner 并继承上一次的连接计数，重连统计不会因为重建对象而归零
func newRunnerFrom(addr string, primary bool, prev *serverRunner) *serverRunner {
	r := newServerRunner(addr, primary)
	if prev != nil {
		prev.mu.Lock()
		r.connects = prev.connects
		r.reconnects = prev.reconnects
		prev.mu.Unlock()
	}
	return r
}

func (r *serverRunner) stopNow() {
	r.once.Do(func() {
		close(r.stop)
	})
}

func (r *serverRunner) stopped() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

func (r *serverRunner) setPrimary(v bool) {
	r.mu.Lock()
	r.primary = v
	r.mu.Unlock()
}

// onState 是 TRPClient 的状态回调，state/message 直接来自连接过程
func (r *serverRunner) onState(state, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch state {
	case ServerStateOnline:
		r.connects++
		if r.connects > 1 {
			r.reconnects++
		}
		message = ""
	case ServerStateConnecting:
		message = ""
	}
	if r.state == state && r.message == message {
		return
	}
	r.state = state
	r.message = message
	r.stateSince = time.Now()
}

func (r *serverRunner) info() ServerInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return ServerInfo{
		Addr:       r.addr,
		Primary:    r.primary,
		State:      r.state,
		Message:    r.message,
		StateSince: r.stateSince,
		Connects:   r.connects,
		Reconnects: r.reconnects,
	}
}

// Manager 维护服务端列表与其连接状态。
// 配置文件模式（confPath 非空）下变更会写回 npc.conf；命令行模式只在内存生效。
type Manager struct {
	mu       sync.Mutex
	opMu     sync.Mutex // 串行化「改列表 / 重启」这类会动协程的操作
	confPath string
	cfg      *config.Config
	cli      *CliConfig
	servers  []string
	runners  []*serverRunner
	started  time.Time

	done     chan struct{}
	closeOne sync.Once
	wg       sync.WaitGroup

	// customSupervise 供测试注入，替换真实连接逻辑
	customSupervise func(*serverRunner)
}

// NewFileManager 从配置文件创建管理器
func NewFileManager(path string) (*Manager, error) {
	cnf, err := config.NewConfig(path)
	if err != nil || cnf.CommonConfig == nil {
		return nil, fmt.Errorf("config file %s loading error: %v", path, err)
	}
	servers := cnf.CommonConfig.Servers
	if len(servers) == 0 && cnf.CommonConfig.Server != "" {
		servers = []string{cnf.CommonConfig.Server}
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("config file %s has no server_addr", path)
	}
	normalized, err := normalizeServers(servers)
	if err != nil {
		return nil, err
	}
	return &Manager{
		confPath: path,
		cfg:      cnf,
		servers:  normalized,
		started:  time.Now(),
		done:     make(chan struct{}),
	}, nil
}

// NewCliManager 从命令行参数创建管理器
func NewCliManager(cli CliConfig, servers []string) (*Manager, error) {
	normalized, err := normalizeServers(servers)
	if err != nil {
		return nil, err
	}
	if len(normalized) == 0 {
		return nil, errors.New("at least one server is required")
	}
	c := cli
	return &Manager{
		cli:     &c,
		servers: normalized,
		started: time.Now(),
		done:    make(chan struct{}),
	}, nil
}

// Start 启动全部服务端守护协程并阻塞，直到 Stop 被调用
func (m *Manager) Start() {
	m.applyLocalServices()
	m.launchAll()
	<-m.done
}

// Stop 停止所有服务端连接
func (m *Manager) Stop() {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	old := m.runners
	m.runners = nil
	m.mu.Unlock()
	for _, r := range old {
		r.stopNow()
	}
	m.closeOne.Do(func() { close(m.done) })
}

// Mode 返回 file / command
func (m *Manager) Mode() string {
	if m.confPath != "" {
		return ModeFile
	}
	return ModeCommand
}

// ConfPath 返回配置文件路径，命令行模式为空
func (m *Manager) ConfPath() string {
	return m.confPath
}

// FileConfig 返回配置文件模式的配置快照，命令行模式为 nil
func (m *Manager) FileConfig() *config.Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// CliSettings 返回命令行模式的参数快照
func (m *Manager) CliSettings() (CliConfig, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cli == nil {
		return CliConfig{}, false
	}
	return *m.cli, true
}

// Servers 返回服务端状态列表（按连接优先级排序，第一个为主节点）
func (m *Manager) Servers() []ServerInfo {
	m.mu.Lock()
	runners := append([]*serverRunner(nil), m.runners...)
	m.mu.Unlock()
	out := make([]ServerInfo, 0, len(runners))
	for _, r := range runners {
		out = append(out, r.info())
	}
	return out
}

// Status 返回整体运行状态
func (m *Manager) Status() ClientStatus {
	m.mu.Lock()
	cfg, cli, servers := m.cfg, m.cli, append([]string(nil), m.servers...)
	m.mu.Unlock()

	st := ClientStatus{
		Version:       version.VERSION,
		UptimeSeconds: int(time.Since(m.started).Seconds()),
		Mode:          m.Mode(),
		ConfigPath:    m.confPath,
		Servers:       m.Servers(),
		ServerCount:   len(servers),
	}
	for _, s := range st.Servers {
		if s.State == ServerStateOnline {
			st.OnlineCount++
		}
	}
	switch {
	case cfg != nil && cfg.CommonConfig != nil:
		st.ConnType = cfg.CommonConfig.Tp
		st.VKeyMasked = maskSecret(cfg.CommonConfig.VKey)
		st.TlsVerify = cfg.CommonConfig.TlsVerify
		st.AutoReconnection = cfg.CommonConfig.AutoReconnection
		st.Hosts = len(cfg.Hosts)
		st.Tasks = len(cfg.Tasks)
	case cli != nil:
		st.ConnType = cli.Tp
		st.VKeyMasked = maskSecret(cli.VKey)
		st.TlsVerify = cli.TlsVerify
		st.AutoReconnection = true
	}
	return st
}

// ApplyServers 用新的服务端列表替换现有列表：保留仍存在的连接，停止被移除的，启动新增的。
func (m *Manager) ApplyServers(addrs []string) (ApplyResult, error) {
	return m.apply(addrs, true, false)
}

// AddServer 追加一个服务端
func (m *Manager) AddServer(addr string) (ApplyResult, error) {
	normalized, err := normalizeServers([]string{addr})
	if err != nil {
		return ApplyResult{}, err
	}
	if len(normalized) != 1 {
		return ApplyResult{}, errors.New("exactly one server address is required")
	}
	m.mu.Lock()
	servers := append([]string(nil), m.servers...)
	m.mu.Unlock()
	for _, v := range servers {
		if v == normalized[0] {
			return ApplyResult{}, ErrServerExists
		}
	}
	return m.apply(append(servers, normalized[0]), true, false)
}

// RemoveServer 移除一个服务端。最后一个需要 force=true 才允许移除。
func (m *Manager) RemoveServer(addr string, force bool) (ApplyResult, error) {
	normalized, err := normalizeServers([]string{addr})
	if err != nil {
		return ApplyResult{}, err
	}
	if len(normalized) != 1 {
		return ApplyResult{}, errors.New("exactly one server address is required")
	}
	m.mu.Lock()
	servers := append([]string(nil), m.servers...)
	m.mu.Unlock()
	idx := -1
	for i, v := range servers {
		if v == normalized[0] {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ApplyResult{}, ErrServerNotFound
	}
	if len(servers) == 1 && !force {
		return ApplyResult{}, ErrLastServer
	}
	rest := append(append([]string{}, servers[:idx]...), servers[idx+1:]...)
	if len(rest) == 0 {
		// 允许清空：停掉全部连接，但保留配置文件里的旧值不动
		m.opMu.Lock()
		defer m.opMu.Unlock()
		m.mu.Lock()
		old := m.runners
		m.runners = nil
		m.servers = nil
		m.mu.Unlock()
		for _, r := range old {
			r.stopNow()
		}
		persisted, err := m.persistServers(nil)
		if err != nil {
			return ApplyResult{}, err
		}
		return ApplyResult{Servers: []ServerInfo{}, Persisted: persisted}, nil
	}
	return m.apply(rest, true, false)
}

// Reconnect 断开所有服务端连接并按当前配置重新注册（不重写配置文件）
func (m *Manager) Reconnect() (ApplyResult, error) {
	m.mu.Lock()
	servers := append([]string(nil), m.servers...)
	m.mu.Unlock()
	return m.apply(servers, false, true)
}

// UpdateConfig 更新连接参数：配置文件模式写回 npc.conf 并重连，命令行模式改内存后重连
func (m *Manager) UpdateConfig(u ConfigUpdate) (bool, error) {
	kv, err := buildConfigValues(u)
	if err != nil {
		return false, err
	}
	if len(kv) == 0 {
		return m.confPath != "", nil
	}

	m.opMu.Lock()
	defer m.opMu.Unlock()

	persisted := false
	if m.confPath != "" {
		if err := writeConfAndValidate(m.confPath, confSectionCommon, kv); err != nil {
			return false, err
		}
		cnf, err := config.NewConfig(m.confPath)
		if err != nil || cnf.CommonConfig == nil {
			return false, fmt.Errorf("reload %s: %v", m.confPath, err)
		}
		m.mu.Lock()
		m.cfg = cnf
		m.mu.Unlock()
		persisted = true
	} else {
		m.mu.Lock()
		if m.cli == nil {
			m.cli = &CliConfig{}
		}
		if u.VKey != nil {
			m.cli.VKey = strings.TrimSpace(*u.VKey)
		}
		if u.ConnType != nil {
			m.cli.Tp = strings.ToLower(strings.TrimSpace(*u.ConnType))
		}
		if u.ProxyUrl != nil {
			m.cli.ProxyUrl = strings.TrimSpace(*u.ProxyUrl)
		}
		if u.TlsVerify != nil {
			m.cli.TlsVerify = *u.TlsVerify
		}
		if u.DisconnectTimeout != nil {
			m.cli.DisconnectTime = *u.DisconnectTimeout
		}
		m.mu.Unlock()
	}
	m.restartAllLocked()
	return persisted, nil
}

// buildConfigValues 校验并转换成 key=value，任何一个字段非法都整体拒绝
func buildConfigValues(u ConfigUpdate) (map[string]string, error) {
	kv := make(map[string]string, 6)
	if u.VKey != nil {
		v := strings.TrimSpace(*u.VKey)
		if v == "" || len(v) > 128 || strings.ContainsAny(v, " \t\r\n") {
			return nil, errors.New("vkey must be 1-128 characters without whitespace")
		}
		kv["vkey"] = v
	}
	if u.ConnType != nil {
		v := strings.ToLower(strings.TrimSpace(*u.ConnType))
		switch v {
		case "tcp", "kcp", "tls":
			kv["conn_type"] = v
		default:
			return nil, fmt.Errorf("conn_type %q is not supported, use tcp / kcp / tls", v)
		}
	}
	if u.ProxyUrl != nil {
		v := strings.TrimSpace(*u.ProxyUrl)
		if v != "" {
			parsed, err := url.Parse(v)
			if err != nil || parsed.Host == "" {
				return nil, fmt.Errorf("proxy_url %q is not a valid url", v)
			}
			switch strings.ToLower(parsed.Scheme) {
			case "socks5", "http", "https":
			default:
				return nil, fmt.Errorf("proxy_url scheme %q is not supported, use socks5 / http", parsed.Scheme)
			}
		}
		kv["proxy_url"] = v
	}
	if u.TlsVerify != nil {
		kv["tls_verify"] = boolString(*u.TlsVerify)
	}
	if u.AutoReconnection != nil {
		kv["auto_reconnection"] = boolString(*u.AutoReconnection)
	}
	if u.DisconnectTimeout != nil {
		v := *u.DisconnectTimeout
		if v < 1 || v > 3600 {
			return nil, errors.New("disconnect_timeout must be between 1 and 3600 seconds")
		}
		kv["disconnect_timeout"] = strconv.Itoa(v)
	}
	return kv, nil
}

// apply 是服务端列表变更的统一入口。persist 为 true 时先写配置文件（失败则不改运行态）；
// restartKept 为 true 时已存在的连接也重建（连接参数变化后需要）。
func (m *Manager) apply(addrs []string, persist, restartKept bool) (ApplyResult, error) {
	normalized, err := normalizeServers(addrs)
	if err != nil {
		return ApplyResult{}, err
	}
	if len(normalized) == 0 {
		return ApplyResult{}, errors.New("at least one server address is required")
	}

	m.opMu.Lock()
	defer m.opMu.Unlock()

	persisted := false
	if persist {
		if persisted, err = m.persistServers(normalized); err != nil {
			return ApplyResult{}, err
		}
	}

	m.mu.Lock()
	old := m.runners
	m.servers = normalized
	byAddr := make(map[string]*serverRunner, len(old))
	for _, r := range old {
		byAddr[r.addr] = r
	}
	ordered := make([]*serverRunner, 0, len(normalized))
	var toStart, toStop []*serverRunner
	for i, addr := range normalized {
		r := byAddr[addr]
		if r != nil && !restartKept && !r.stopped() {
			delete(byAddr, addr)
			r.setPrimary(i == 0)
			ordered = append(ordered, r)
			continue
		}
		nr := newRunnerFrom(addr, i == 0, byAddr[addr])
		ordered = append(ordered, nr)
		toStart = append(toStart, nr)
	}
	for _, r := range byAddr {
		toStop = append(toStop, r)
	}
	m.runners = ordered
	m.mu.Unlock()

	for _, r := range toStop {
		r.stopNow()
	}
	waitRunners(toStop, 8*time.Second)
	for _, r := range toStart {
		m.launch(r)
	}
	return ApplyResult{Servers: m.Servers(), Persisted: persisted}, nil
}

// restartAllLocked 用当前配置重建全部连接，调用方需持有 opMu
func (m *Manager) restartAllLocked() {
	m.mu.Lock()
	old := m.runners
	servers := append([]string(nil), m.servers...)
	m.mu.Unlock()
	// 先取旧连接计数，重建后继续累加
	prev := make(map[string]ServerInfo, len(old))
	for _, r := range old {
		info := r.info()
		prev[info.Addr] = info
	}
	for _, r := range old {
		r.stopNow()
	}
	waitRunners(old, 8*time.Second)

	m.mu.Lock()
	started := make([]*serverRunner, 0, len(servers))
	runners := make([]*serverRunner, 0, len(servers))
	for i, addr := range servers {
		prevInfo, ok := prev[addr]
		r := newServerRunner(addr, i == 0)
		if ok {
			r.connects = prevInfo.Connects
			r.reconnects = prevInfo.Reconnects
		}
		runners = append(runners, r)
		started = append(started, r)
	}
	m.runners = runners
	m.mu.Unlock()

	for _, r := range started {
		m.launch(r)
	}
}

// persistServers 把服务端列表写回 npc.conf；命令行模式不落盘
func (m *Manager) persistServers(addrs []string) (bool, error) {
	if m.confPath == "" {
		return false, nil
	}
	if err := writeConfAndValidate(m.confPath, confSectionCommon, map[string]string{
		"server_addr": strings.Join(addrs, ","),
	}); err != nil {
		return false, err
	}
	return true, nil
}

// writeConfAndValidate 写入配置后立刻回读校验；解析不过就回滚原文件，
// 避免把一份还能用的配置写成起不来的样子（例如重复段、非法行）。
func writeConfAndValidate(path, section string, kv map[string]string) error {
	orig, readErr := os.ReadFile(path)
	if err := SetConfValues(path, section, kv); err != nil {
		return fmt.Errorf("write %s: %v", path, err)
	}
	cnf, err := config.NewConfig(path)
	if err == nil && cnf.CommonConfig != nil {
		return nil
	}
	if err == nil {
		err = errors.New("parsed config has no [common] section")
	}
	if readErr == nil {
		if restoreErr := os.WriteFile(path, orig, 0o600); restoreErr != nil {
			logs.Error("[config] restore %s failed: %s", path, restoreErr)
		}
	}
	return fmt.Errorf("config %s is invalid after writing, changes rolled back: %v", path, err)
}

// launchAll 启动当前列表里的全部服务端（仅在 Start 时使用）
func (m *Manager) launchAll() {
	m.mu.Lock()
	if len(m.runners) > 0 {
		m.mu.Unlock()
		return
	}
	servers := append([]string(nil), m.servers...)
	runners := make([]*serverRunner, 0, len(servers))
	for i, addr := range servers {
		runners = append(runners, newServerRunner(addr, i == 0))
	}
	m.runners = runners
	m.mu.Unlock()
	for _, r := range runners {
		m.launch(r)
	}
}

func (m *Manager) launch(r *serverRunner) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(r.done)
		m.superviseServer(r)
	}()
}

// superviseServer 是单个服务端的守护循环：连接失败 / 断开后按需重连，stop 后退出
func (m *Manager) superviseServer(r *serverRunner) {
	// 只有被显式停止才写 stopped；因为配置或网络原因退出时保留 offline + 失败原因
	defer func() {
		if r.stopped() {
			r.onState(ServerStateStopped, "")
		}
	}()
	if m.customSupervise != nil {
		m.customSupervise(r)
		return
	}
	m.mu.Lock()
	cfg, cli := m.cfg, m.cli
	m.mu.Unlock()

	if cfg != nil {
		startFromServer(cfg, r)
		return
	}
	if cli == nil {
		r.onState(ServerStateStopped, "no configuration")
		return
	}
	SetTlsVerify(cli.TlsVerify)
	for {
		if r.stopped() {
			return
		}
		r.onState(ServerStateConnecting, "")
		cl := NewRPClient(r.addr, cli.VKey, cli.Tp, cli.ProxyUrl, nil, cli.DisconnectTime)
		cl.SetStateHook(r.onState)
		cl.Start()
		if r.stopped() {
			return
		}
		r.onState(ServerStateOffline, "connection closed, reconnecting")
		logs.Warn("[%s] connection closed, reconnecting in 5 seconds...", r.addr)
		if !sleepOrStop(5*time.Second, r.stop) {
			return
		}
	}
}

// applyLocalServices 启动 secret / p2p 本地监听（与具体服务端无关，只做一次）
func (m *Manager) applyLocalServices() {
	m.mu.Lock()
	cfg := m.cfg
	cli := m.cli
	m.mu.Unlock()
	if cfg != nil && cfg.CommonConfig != nil {
		SetTlsVerify(cfg.CommonConfig.TlsVerify)
		for _, v := range cfg.LocalServer {
			go StartLocalServer(v, cfg.CommonConfig)
		}
		return
	}
	if cli != nil {
		SetTlsVerify(cli.TlsVerify)
	}
}

// normalizeServers 校验并规范化服务端地址：必须 host:port，去重且保持顺序
func normalizeServers(addrs []string) ([]string, error) {
	out := make([]string, 0, len(addrs))
	seen := make(map[string]bool, len(addrs))
	for _, raw := range addrs {
		for _, part := range config.SplitServerAddr(raw) {
			addr, err := normalizeServerAddr(part)
			if err != nil {
				return nil, err
			}
			if seen[addr] {
				continue
			}
			seen[addr] = true
			out = append(out, addr)
		}
	}
	return out, nil
}

func normalizeServerAddr(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("server address must not be empty")
	}
	if strings.ContainsAny(addr, " \t\r\n") {
		return "", fmt.Errorf("server address %q must not contain whitespace", addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("server address %q must be host:port: %v", addr, err)
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return "", fmt.Errorf("server address %q has empty host", addr)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", fmt.Errorf("server address %q has an invalid port", addr)
	}
	return net.JoinHostPort(host, strconv.Itoa(p)), nil
}

// waitRunners 等待协程退出，超时后放弃（不阻塞调用方太久）
func waitRunners(runners []*serverRunner, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for _, r := range runners {
		left := time.Until(deadline)
		if left <= 0 {
			return
		}
		t := time.NewTimer(left)
		select {
		case <-r.done:
			t.Stop()
		case <-t.C:
			return
		}
	}
}

// sleepOrStop 可被 stop 打断的 sleep，返回 false 表示期间收到了停止信号
func sleepOrStop(d time.Duration, stop <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-stop:
		return false
	case <-t.C:
		return true
	}
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// maskSecret 只保留首字符，避免 API 把 vkey 原样吐回去
func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	return s[:1] + "***"
}
