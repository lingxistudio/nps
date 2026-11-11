package client

// npc.conf 的就地改写：客户端 API 修改服务端列表 / 连接参数后写回配置文件，重启依然生效。
// 只做行级的 key=value 替换，且限定在 [common] 段内，注释、其它段（隧道 / p2p / secret）与行尾符原样保留。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// confSectionCommon 是客户端主配置所在的段名
const confSectionCommon = "[common]"

// SetConfValues 把 kv 写入 path 的 section 段：
//   - 段内已存在（未注释）的同名项就地替换，保留缩进；
//   - 段内不存在则插入到该段末尾；
//   - 段不存在则在文件末尾追加该段。
//
// 段外的同名项（例如隧道里的 password=xxx）不会被改动。
func SetConfValues(path, section string, kv map[string]string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("config path must not be empty")
	}
	if len(kv) == 0 {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(raw)
	// 记事本等编辑器可能写入 UTF-8 BOM，会把首个段名变成 "\ufeff[common]" 而匹配不上。
	// 这里直接丢掉 BOM：写回后的文件不带 BOM，也避免同一个段被当成两个。
	text = strings.TrimPrefix(text, "\ufeff")
	crlf := strings.Contains(text, "\r\n")
	// 统一去掉 \r 后按行处理，落盘时再还原行尾符
	lines := strings.Split(strings.Replace(text, "\r\n", "\n", -1), "\n")

	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == section {
			start = i
			break
		}
	}
	if start < 0 {
		// 段不存在：去掉末尾空行后追加段与全部键
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, section)
		for _, k := range sortedKeys(kv) {
			lines = append(lines, k+"="+kv[k])
		}
		return writeConfFile(path, lines, crlf)
	}

	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
			end = i
			break
		}
	}

	// 段内插入位置：段末尾之前（跳过尾部空行，避免把键插到下一段前面）
	insertAt := end
	for insertAt > start+1 && strings.TrimSpace(lines[insertAt-1]) == "" {
		insertAt--
	}

	pending := make([]string, 0, len(kv))
	for _, k := range sortedKeys(kv) {
		if !setKeyInSection(lines, start, end, k, kv[k]) {
			pending = append(pending, k+"="+kv[k])
		}
	}
	if len(pending) > 0 {
		rest := append([]string{}, lines[insertAt:]...)
		lines = append(append(append([]string{}, lines[:insertAt]...), pending...), rest...)
	}
	return writeConfFile(path, lines, crlf)
}

// setKeyInSection 在 [from, to) 行范围内替换 key，成功返回 true
func setKeyInSection(lines []string, from, to int, key, value string) bool {
	for i := from + 1; i < to; i++ {
		body := lines[i]
		trimmed := strings.TrimLeft(body, " \t")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		name, _, ok := strings.Cut(trimmed, "=")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		indent := body[:len(body)-len(trimmed)]
		lines[i] = indent + key + "=" + value
		return true
	}
	return false
}

func sortedKeys(kv map[string]string) []string {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// writeConfFile 以原子方式落盘，保留原文件权限；npcf.conf 含 vkey，默认收紧到 0600
func writeConfFile(path string, lines []string, crlf bool) error {
	sep := "\n"
	if crlf {
		sep = "\r\n"
	}
	out := strings.Join(lines, sep)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += sep
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, ".npc-conf-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = tmp.WriteString(out); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
