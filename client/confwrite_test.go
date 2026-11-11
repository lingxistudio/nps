package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempConf(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "npc.conf")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp conf: %v", err)
	}
	return path
}

// 只改 [common] 段内的同名项，注释、行尾符与其它段保持原样
func TestSetConfValuesCommonSection(t *testing.T) {
	src := "# 注释保留\r\n[common]\r\nserver_addr=a.com:8024\r\nvkey=123\r\n\r\n[tcp]\r\nmode=tcp\r\npassword=keep-me\r\n"
	path := writeTempConf(t, src)

	if err := SetConfValues(path, confSectionCommon, map[string]string{
		"server_addr": "a.com:8025,b.com:8025",
		"vkey":        "456",
	}); err != nil {
		t.Fatalf("SetConfValues: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read conf: %v", err)
	}
	got := string(raw)
	for _, want := range []string{
		"# 注释保留\r\n",
		"server_addr=a.com:8025,b.com:8025\r\n",
		"vkey=456\r\n",
		"password=keep-me",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in result, got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "vkey=123") {
		t.Fatalf("old vkey not replaced:\n%s", got)
	}
}

// 段内缺失的键插入到该段末尾，而不是追加到下一个段后面
func TestSetConfValuesInsertIntoSection(t *testing.T) {
	src := "[common]\nserver_addr=a.com:8024\n\n[tcp]\nmode=tcp\n"
	path := writeTempConf(t, src)

	if err := SetConfValues(path, confSectionCommon, map[string]string{"conn_type": "tls"}); err != nil {
		t.Fatalf("SetConfValues: %v", err)
	}
	raw, _ := os.ReadFile(path)
	got := string(raw)
	commonIdx := strings.Index(got, "[common]")
	connIdx := strings.Index(got, "conn_type=tls")
	tcpIdx := strings.Index(got, "[tcp]")
	if commonIdx < 0 || connIdx < 0 || tcpIdx < 0 {
		t.Fatalf("missing expected entries:\n%s", got)
	}
	if !(commonIdx < connIdx && connIdx < tcpIdx) {
		t.Fatalf("conn_type should be inside [common], got:\n%s", got)
	}
}

// 段不存在时追加新段
func TestSetConfValuesAppendSection(t *testing.T) {
	path := writeTempConf(t, "[tcp]\nmode=tcp\n")
	if err := SetConfValues(path, confSectionCommon, map[string]string{"server_addr": "a.com:8024"}); err != nil {
		t.Fatalf("SetConfValues: %v", err)
	}
	raw, _ := os.ReadFile(path)
	got := string(raw)
	if !strings.Contains(got, "[common]\nserver_addr=a.com:8024") {
		t.Fatalf("expected new [common] section, got:\n%s", got)
	}
	if !strings.HasPrefix(got, "[tcp]") {
		t.Fatalf("existing content should be kept first, got:\n%s", got)
	}
}

// 配置文件不存在时创建全新文件
func TestSetConfValuesCreateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "npc.conf")
	if err := SetConfValues(path, confSectionCommon, map[string]string{"server_addr": "a.com:8024"}); err != nil {
		t.Fatalf("SetConfValues: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read created conf: %v", err)
	}
	if !strings.Contains(string(raw), "[common]") || !strings.Contains(string(raw), "server_addr=a.com:8024") {
		t.Fatalf("unexpected content:\n%s", string(raw))
	}
}

// 带 UTF-8 BOM 的配置（记事本另存为常见情况）也要能改到原来的段，而不是追加一个重复段
func TestSetConfValuesWithBOM(t *testing.T) {
	src := "\ufeff[common]\nserver_addr=a.com:8024\n\n[tcp]\nmode=tcp\n"
	path := writeTempConf(t, src)
	if err := SetConfValues(path, confSectionCommon, map[string]string{"server_addr": "b.com:8025"}); err != nil {
		t.Fatalf("SetConfValues: %v", err)
	}
	raw, _ := os.ReadFile(path)
	got := string(raw)
	if strings.Contains(got, "\ufeff") {
		t.Fatalf("BOM should be dropped when writing back")
	}
	if n := strings.Count(got, "[common]"); n != 1 {
		t.Fatalf("expect a single [common] section, got %d:\n%s", n, got)
	}
	if !strings.Contains(got, "server_addr=b.com:8025") {
		t.Fatalf("existing section not updated:\n%s", got)
	}
	if !strings.Contains(got, "[tcp]\nmode=tcp") {
		t.Fatalf("tunnel section must be kept:\n%s", got)
	}
}
