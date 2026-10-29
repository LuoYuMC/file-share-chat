package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// 本程序写入 hosts 的记录都带这个标记，便于识别与清理
const hostsMark = "# [file-share-chat]"

// 与所选域名冲突的原有记录，会临时被注释掉，恢复时去掉此前缀
const hostsOff = "#[file-share-chat-off] "

var hostsMu sync.Mutex

// needsElevation 判断 hosts 是否不可写（即没有管理员权限）
func needsElevation(hostsPath string) bool {
	f, err := os.OpenFile(hostsPath, os.O_WRONLY, 0)
	if err != nil {
		return true
	}
	_ = f.Close()
	return false
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			out = append(out, b)
			break
		}
		out = append(out, b[:i+1])
		b = b[i+1:]
	}
	return out
}

// hasHostname 判断一行 hosts 记录的域名列表中是否包含 domain（忽略注释与大小写）
func hasHostname(line []byte, domain string) bool {
	body := line
	if i := bytes.IndexByte(body, '#'); i >= 0 {
		body = body[:i]
	}
	fields := strings.Fields(string(body))
	for i := 1; i < len(fields); i++ { // fields[0] 是 IP
		if strings.EqualFold(fields[i], domain) {
			return true
		}
	}
	return false
}

// applyHosts 先清除本程序之前写入的所有记录（并恢复被临时注释的原记录），
// 然后为 entries 中的每一项写入 "目标IP 域名"，并注释掉其它与这些域名冲突的原记录。
// entries 为空时仅恢复原状。
func applyHosts(hostsPath, backupPath string, entries []HostEntry) error {
	hostsMu.Lock()
	defer hostsMu.Unlock()

	raw, err := os.ReadFile(hostsPath)
	if err != nil {
		return fmt.Errorf("无法读取 hosts 文件：%w", err)
	}
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		_ = os.WriteFile(backupPath, raw, 0644) // 首次修改前备份原始内容
	}

	var out [][]byte
	for _, line := range splitLines(raw) {
		if bytes.Contains(line, []byte(hostsMark)) {
			continue
		}
		if bytes.HasPrefix(line, []byte(hostsOff)) {
			line = line[len(hostsOff):]
		}
		out = append(out, line)
	}

	// 去重（域名不区分大小写），同一域名只保留第一条
	seen := map[string]bool{}
	var names []HostEntry
	for _, e := range entries {
		k := strings.ToLower(strings.TrimSpace(e.Domain))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		ip := strings.TrimSpace(e.IP)
		if ip == "" {
			ip = defaultHostIP
		}
		names = append(names, HostEntry{Domain: k, IP: ip})
	}

	for _, e := range names {
		for i, line := range out {
			if hasHostname(line, e.Domain) {
				out[i] = append([]byte(hostsOff), line...)
			}
		}
	}

	if n := len(out); n > 0 && !bytes.HasSuffix(out[n-1], []byte("\n")) {
		out[n-1] = append(bytes.Clone(out[n-1]), '\r', '\n')
	}
	for _, e := range names {
		out = append(out, []byte(fmt.Sprintf("%s %s %s\r\n", e.IP, e.Domain, hostsMark)))
	}

	if err := writeHostsFile(hostsPath, bytes.Join(out, nil)); err != nil {
		return fmt.Errorf("无法写入 hosts 文件（%s）：%v。%s", hostsPath, err, hostsDiagnosis(hostsPath))
	}
	flushDNS()
	return nil
}

// writeHostsFile 写入 hosts 文件。
// 遇到只读属性（权限被拒绝）时清除只读属性后重试；遇到文件被其它程序占用时等待后重试。
func writeHostsFile(path string, data []byte) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if err = os.WriteFile(path, data, 0644); err == nil {
			return nil
		}
		if os.IsPermission(err) {
			if fi, serr := os.Stat(path); serr == nil && fi.Mode().Perm()&0200 == 0 {
				_ = os.Chmod(path, 0644) // 清除只读属性
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	return err
}

// hostsDiagnosis 给出写入失败时的诊断信息
func hostsDiagnosis(path string) string {
	var notes []string
	if isElevated() {
		notes = append(notes, "程序已是管理员身份运行")
	} else {
		notes = append(notes, "程序没有以管理员身份运行，请右键 exe 选择“以管理员身份运行”")
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0200 == 0 {
		notes = append(notes, "hosts 文件为只读，且清除只读属性失败")
	}
	return "诊断：" + strings.Join(notes, "；") +
		"。常见原因：360、腾讯电脑管家等安全软件的 hosts 保护拦截了修改，请将本程序加入信任或关闭其 hosts 保护后重试"
}
