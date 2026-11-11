package client

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ehang.io/nps/lib/config"
)

// newTestManager 创建一个不会真正拨号的 Manager：supervise 直接标记在线并等停止信号
func newTestManager(t *testing.T, servers []string) *Manager {
	t.Helper()
	m, err := NewCliManager(CliConfig{VKey: "123", Tp: "tcp", DisconnectTime: 60}, servers)
	if err != nil {
		t.Fatalf("NewCliManager: %v", err)
	}
	m.customSupervise = func(r *serverRunner) {
		r.onState(ServerStateOnline, "")
		<-r.stop
	}
	m.launchAll()
	return m
}

func waitState(t *testing.T, m *Manager, count int) []ServerInfo {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		servers := m.Servers()
		online := 0
		for _, s := range servers {
			if s.State == ServerStateOnline {
				online++
			}
		}
		if len(servers) == count && online == count {
			return servers
		}
		time.Sleep(10 * time.Millisecond)
	}
	return m.Servers()
}

func TestManagerAddAndRemoveServer(t *testing.T) {
	m := newTestManager(t, []string{"a.com:8024"})
	servers := waitState(t, m, 1)
	if len(servers) != 1 || servers[0].State != ServerStateOnline || !servers[0].Primary {
		t.Fatalf("unexpected initial servers: %+v", servers)
	}

	// 规范化：去掉空白、端口补全、去重
	res, err := m.AddServer(" b.com:8025 ")
	if err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if len(res.Servers) != 2 || res.Servers[1].Addr != "b.com:8025" {
		t.Fatalf("unexpected servers after add: %+v", res.Servers)
	}
	if res.Persisted {
		t.Fatalf("command mode must not persist")
	}
	waitState(t, m, 2)

	if _, err := m.AddServer("b.com:8025"); !errors.Is(err, ErrServerExists) {
		t.Fatalf("expect ErrServerExists, got %v", err)
	}
	if _, err := m.AddServer("b.com"); err == nil {
		t.Fatalf("expect error for address without port")
	}
	if _, err := m.RemoveServer("c.com:8024", false); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("expect ErrServerNotFound, got %v", err)
	}

	res, err = m.RemoveServer("a.com:8024", false)
	if err != nil {
		t.Fatalf("RemoveServer: %v", err)
	}
	if len(res.Servers) != 1 {
		t.Fatalf("unexpected servers after remove: %+v", res.Servers)
	}
	// 剩余节点升为主节点
	waitState(t, m, 1)
	if s := m.Servers()[0]; !s.Primary || s.Addr != "b.com:8025" {
		t.Fatalf("remaining server should become primary: %+v", s)
	}

	if _, err := m.RemoveServer("b.com:8025", false); !errors.Is(err, ErrLastServer) {
		t.Fatalf("expect ErrLastServer, got %v", err)
	}
	res, err = m.RemoveServer("b.com:8025", true)
	if err != nil {
		t.Fatalf("force remove: %v", err)
	}
	if len(res.Servers) != 0 {
		t.Fatalf("expect empty server list, got %+v", res.Servers)
	}
}

func TestManagerApplyServersDedupe(t *testing.T) {
	m := newTestManager(t, []string{"a.com:8024"})
	waitState(t, m, 1)
	res, err := m.ApplyServers([]string{"a.com:8024, a.com:8024", "b.com:8025"})
	if err != nil {
		t.Fatalf("ApplyServers: %v", err)
	}
	if len(res.Servers) != 2 {
		t.Fatalf("duplicate address should be collapsed, got %+v", res.Servers)
	}
	if _, err := m.ApplyServers(nil); err == nil {
		t.Fatalf("empty server list should be rejected")
	}
	if _, err := m.ApplyServers([]string{"[::1]:8024"}); err != nil {
		t.Fatalf("ipv6 address should be accepted: %v", err)
	}
}

// 文件模式：变更要写回 npc.conf，并保留其它配置项
func TestManagerFileModePersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "npc.conf")
	src := "[common]\nserver_addr=a.com:8024\nvkey=123\nconn_type=tcp\n\n[tcp]\nmode=tcp\nserver_port=9000\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write conf: %v", err)
	}
	m, err := NewFileManager(path)
	if err != nil {
		t.Fatalf("NewFileManager: %v", err)
	}
	m.customSupervise = func(r *serverRunner) {
		r.onState(ServerStateOnline, "")
		<-r.stop
	}
	m.launchAll()
	waitState(t, m, 1)

	res, err := m.AddServer("b.com:8025")
	if err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if !res.Persisted {
		t.Fatalf("file mode should persist changes")
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "server_addr=a.com:8024,b.com:8025") {
		t.Fatalf("server_addr not written back:\n%s", string(raw))
	}
	if !strings.Contains(string(raw), "server_port=9000") {
		t.Fatalf("tunnel section must be kept:\n%s", string(raw))
	}

	persisted, err := m.UpdateConfig(ConfigUpdate{ConnType: strPtr("kcp"), DisconnectTimeout: intPtr(90)})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if !persisted {
		t.Fatalf("file mode config update should persist")
	}
	raw, _ = os.ReadFile(path)
	got := string(raw)
	if !strings.Contains(got, "conn_type=kcp") || !strings.Contains(got, "disconnect_timeout=90") {
		t.Fatalf("config update not written:\n%s", got)
	}
	st := m.Status()
	if st.Mode != ModeFile || st.ConnType != "kcp" || st.ServerCount != 2 {
		t.Fatalf("unexpected status: %+v", st)
	}
	if st.VKeyMasked == "123" || st.VKeyMasked == "" {
		t.Fatalf("vkey must be masked in status, got %q", st.VKeyMasked)
	}
}

func TestUpdateConfigValidation(t *testing.T) {
	m := newTestManager(t, []string{"a.com:8024"})
	cases := []ConfigUpdate{
		{ConnType: strPtr("ws")},
		{VKey: strPtr("")},
		{VKey: strPtr("has space")},
		{ProxyUrl: strPtr("ftp://127.0.0.1:21")},
		{DisconnectTimeout: intPtr(0)},
		{DisconnectTimeout: intPtr(99999)},
	}
	for i, c := range cases {
		if _, err := m.UpdateConfig(c); err == nil {
			t.Fatalf("case %d should be rejected: %+v", i, c)
		}
	}
	// 合法更新在命令行模式下只改内存
	persisted, err := m.UpdateConfig(ConfigUpdate{VKey: strPtr("newkey"), ProxyUrl: strPtr("socks5://u:p@127.0.0.1:1080")})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if persisted {
		t.Fatalf("command mode should not persist")
	}
	if view := (&apiHandler{opt: ApiOptions{Manager: m, Key: "k"}}).configView(); view["proxy_url_masked"] != "socks5://u:***@127.0.0.1:1080" {
		t.Fatalf("proxy url password must be masked, got %v", view["proxy_url_masked"])
	}
}

func strPtr(s string) *string { return &s }
func intPtr(v int) *int       { return &v }

// 带 BOM 的配置也要能热更新（否则会写出重复 [common] 段导致重启失败）
func TestUpdateConfigWithBOMConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "npc.conf")
	src := "\ufeff[common]\nserver_addr=a.com:8024\nvkey=123\nconn_type=tcp\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write conf: %v", err)
	}
	m, err := NewFileManager(path)
	if err != nil {
		t.Fatalf("NewFileManager: %v", err)
	}
	m.customSupervise = func(r *serverRunner) { <-r.stop }
	m.launchAll()
	waitState(t, m, 1)

	if _, err := m.UpdateConfig(ConfigUpdate{ConnType: strPtr("kcp")}); err != nil {
		t.Fatalf("UpdateConfig on BOM config: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if n := strings.Count(string(raw), "[common]"); n != 1 {
		t.Fatalf("expect a single [common] section, got %d:\n%s", n, string(raw))
	}
	if !strings.Contains(string(raw), "conn_type=kcp") {
		t.Fatalf("conn_type not updated:\n%s", string(raw))
	}
	// 配置仍可被正常解析
	if cnf, err := config.NewConfig(path); err != nil || cnf.CommonConfig == nil || cnf.CommonConfig.Tp != "kcp" {
		t.Fatalf("config unreadable after update: %v", err)
	}
}
