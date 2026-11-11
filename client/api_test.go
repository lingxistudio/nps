package client

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type testResp struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func newTestApi(t *testing.T, opt ApiOptions) (*httptest.Server, *Manager) {
	t.Helper()
	m := newTestManager(t, []string{"a.com:8024"})
	waitState(t, m, 1)
	if opt.Manager == nil {
		opt.Manager = m
	}
	if opt.Key == "" {
		opt.Key = "secret-key"
	}
	srv := httptest.NewServer(newApiHandler(opt))
	t.Cleanup(srv.Close)
	return srv, m
}

func doApi(t *testing.T, method, url, key, body string) (*http.Response, testResp) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var out testResp
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode response %s: %v", raw, err)
		}
	}
	return resp, out
}

func TestApiAuthAndHealth(t *testing.T) {
	srv, _ := newTestApi(t, ApiOptions{})

	// 探活不需要凭据
	resp, out := doApi(t, http.MethodGet, srv.URL+"/api/v1/health", "", "")
	if resp.StatusCode != http.StatusOK || !out.OK {
		t.Fatalf("health should be public, got %d %+v", resp.StatusCode, out)
	}

	resp, out = doApi(t, http.MethodGet, srv.URL+"/api/v1/status", "", "")
	if resp.StatusCode != http.StatusUnauthorized || out.Error == nil || out.Error.Code != apiCodeUnauthorized {
		t.Fatalf("expect 401 unauthorized, got %d %+v", resp.StatusCode, out)
	}

	resp, _ = doApi(t, http.MethodGet, srv.URL+"/api/v1/status", "wrong-key", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expect 401 for wrong key, got %d", resp.StatusCode)
	}

	resp, out = doApi(t, http.MethodGet, srv.URL+"/api/v1/status", "secret-key", "")
	if resp.StatusCode != http.StatusOK || !out.OK {
		t.Fatalf("expect 200, got %d %+v", resp.StatusCode, out)
	}
	var st ClientStatus
	if err := json.Unmarshal(out.Data, &st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if st.ServerCount != 1 || st.Servers[0].Addr != "a.com:8024" || st.VKeyMasked != "1***" {
		t.Fatalf("unexpected status: %+v", st)
	}
}

func TestApiServerListOperations(t *testing.T) {
	srv, m := newTestApi(t, ApiOptions{})
	base := srv.URL + "/api/v1/servers"

	resp, out := doApi(t, http.MethodPut, base, "secret-key", `{"servers":["a.com:8024","b.com:8025"]}`)
	if resp.StatusCode != http.StatusOK || !out.OK {
		t.Fatalf("put servers failed: %d %+v", resp.StatusCode, out)
	}
	if got := len(m.Servers()); got != 2 {
		t.Fatalf("expect 2 servers, got %d", got)
	}

	resp, out = doApi(t, http.MethodPost, base, "secret-key", `{"addr":"b.com:8025"}`)
	if resp.StatusCode != http.StatusConflict || out.Error.Code != apiCodeConflict {
		t.Fatalf("expect 409 for duplicate, got %d %+v", resp.StatusCode, out)
	}

	resp, out = doApi(t, http.MethodPost, base, "secret-key", `{"addr":"no-port"}`)
	if resp.StatusCode != http.StatusBadRequest || out.Error.Code != apiCodeInvalidArgument {
		t.Fatalf("expect 400 for invalid addr, got %d %+v", resp.StatusCode, out)
	}

	resp, out = doApi(t, http.MethodDelete, base+"?addr=a.com:8024", "secret-key", "")
	if resp.StatusCode != http.StatusOK || !out.OK {
		t.Fatalf("delete failed: %d %+v", resp.StatusCode, out)
	}
	if got := len(m.Servers()); got != 1 {
		t.Fatalf("expect 1 server, got %d", got)
	}

	resp, out = doApi(t, http.MethodDelete, base+"?addr=b.com:8025", "secret-key", "")
	if resp.StatusCode != http.StatusConflict || !strings.Contains(out.Error.Message, "force") {
		t.Fatalf("expect 409 for last server, got %d %+v", resp.StatusCode, out)
	}
	resp, _ = doApi(t, http.MethodDelete, base+"?addr=b.com:8025&force=true", "secret-key", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("force delete failed: %d", resp.StatusCode)
	}
	if got := len(m.Servers()); got != 0 {
		t.Fatalf("expect empty list, got %d", got)
	}
}

func TestApiConfigUpdate(t *testing.T) {
	srv, m := newTestApi(t, ApiOptions{})
	base := srv.URL + "/api/v1/config"

	resp, out := doApi(t, http.MethodGet, base, "secret-key", "")
	if resp.StatusCode != http.StatusOK || !out.OK {
		t.Fatalf("get config failed: %d %+v", resp.StatusCode, out)
	}
	if strings.Contains(string(out.Data), `"vkey":"`) {
		t.Fatalf("raw vkey must never be returned: %s", string(out.Data))
	}

	resp, out = doApi(t, http.MethodPatch, base, "secret-key", `{"conn_type":"tls","disconnect_timeout":120}`)
	if resp.StatusCode != http.StatusOK || !out.OK {
		t.Fatalf("patch config failed: %d %+v", resp.StatusCode, out)
	}
	if cli, _ := m.CliSettings(); cli.Tp != "tls" || cli.DisconnectTime != 120 {
		t.Fatalf("config not applied: %+v", cli)
	}

	// 非法值整体拒绝，不允许改一半
	resp, out = doApi(t, http.MethodPatch, base, "secret-key", `{"conn_type":"ws","disconnect_timeout":120}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expect 400 for bad conn_type, got %d %+v", resp.StatusCode, out)
	}
	if cli, _ := m.CliSettings(); cli.Tp != "tls" {
		t.Fatalf("rejected update must not change config: %+v", cli)
	}

	// 未知字段直接 400，避免拼错字段名却静默生效
	resp, _ = doApi(t, http.MethodPatch, base, "secret-key", `{"vkeys":"x"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expect 400 for unknown field, got %d", resp.StatusCode)
	}
}

func TestApiReconnectAndReadOnly(t *testing.T) {
	srv, m := newTestApi(t, ApiOptions{ReadOnly: true})
	resp, out := doApi(t, http.MethodPost, srv.URL+"/api/v1/reconnect", "secret-key", "")
	if resp.StatusCode != http.StatusForbidden || out.Error.Code != apiCodeReadOnly {
		t.Fatalf("expect 403 read_only, got %d %+v", resp.StatusCode, out)
	}
	resp, _ = doApi(t, http.MethodGet, srv.URL+"/api/v1/status", "secret-key", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read operations should still work in read only mode, got %d", resp.StatusCode)
	}

	// 只读关闭后重连应成功，且连接被重建
	srv2, m2 := newTestApi(t, ApiOptions{})
	resp, out = doApi(t, http.MethodPost, srv2.URL+"/api/v1/reconnect", "secret-key", "")
	if resp.StatusCode != http.StatusOK || !out.OK {
		t.Fatalf("reconnect failed: %d %+v", resp.StatusCode, out)
	}
	servers := waitState(t, m2, 1)
	if servers[0].Connects < 2 {
		t.Fatalf("expect connection to be rebuilt, got %+v", servers[0])
	}
	_ = m
}

func TestApiAllowIP(t *testing.T) {
	srv, _ := newTestApi(t, ApiOptions{AllowIP: "10.0.0.0/8"})
	resp, out := doApi(t, http.MethodGet, srv.URL+"/api/v1/status", "secret-key", "")
	if resp.StatusCode != http.StatusForbidden || out.Error.Code != apiCodeForbidden {
		t.Fatalf("expect 403 forbidden, got %d %+v", resp.StatusCode, out)
	}
}

func TestStartApiServerRequiresKey(t *testing.T) {
	m := newTestManager(t, []string{"a.com:8024"})
	if _, err := StartApiServer(ApiOptions{Addr: "127.0.0.1:0", Manager: m}); err == nil {
		t.Fatalf("empty api key must be refused")
	}
	if _, err := StartApiServer(ApiOptions{Addr: "", Key: "k", Manager: m}); err == nil {
		t.Fatalf("empty api addr must be refused")
	}
	srv, err := StartApiServer(ApiOptions{Addr: "127.0.0.1:0", Key: "k", Manager: m})
	if err != nil {
		t.Fatalf("start api: %v", err)
	}
	defer srv.Close()
}
