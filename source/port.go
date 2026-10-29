// 服务端口设置（仅主管理员）：在管理页面修改端口，保存后服务自动切换到新端口。
//
// 端口优先级：命令行 -port（调试用）> 管理页面保存的端口 > 默认 80（留空时）。
// 切换流程：先在新端口上预先绑定成功（失败则报错，原端口不变、设置不保存）→ 保存设置 → 返回响应
// → 主循环关闭旧端口并改用新端口。hosts 文件与端口无关，不需要改动。
package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// portChange 管理页面保存新端口后交给主循环切换；ln 已在新端口上绑定好
type portChange struct {
	ln   net.Listener
	port int
}

var portDigitsRE = regexp.MustCompile(`^\d{1,5}$`)

var errPortRange = errors.New("端口须为 1–65535 之间的整数（留空则使用 80 端口）")

// parsePortInput 解析管理页面输入的端口：留空返回 0（使用默认端口）；否则必须是 1–65535 的整数
func parsePortInput(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	if !portDigitsRE.MatchString(raw) {
		return 0, errPortRange
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 65535 {
		return 0, errPortRange
	}
	return n, nil
}

// portInput 读取请求中的端口参数（允许字符串或数字；缺少该字段时 ok 为 false）
func portInput(body map[string]any) (string, bool) {
	v, exists := body["port"]
	if !exists {
		return "", false
	}
	switch x := v.(type) {
	case nil:
		return "", true
	case string:
		return x, true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	}
	return "", false
}

// currentPort 当前正在监听的端口
func (a *server) currentPort() int {
	a.portMu.Lock()
	defer a.portMu.Unlock()
	return a.port
}

// setPort 记录当前监听的端口（启动和切换完成时调用）
func (a *server) setPort(port int) {
	a.portMu.Lock()
	defer a.portMu.Unlock()
	a.port = port
}

// finishRestart 端口切换完成，允许再次修改端口
func (a *server) finishRestart() {
	a.restartPending.Store(false)
}

func (a *server) portState() map[string]any {
	value := ""
	if saved := a.store.SavedServerPort(); saved > 0 {
		value = strconv.Itoa(saved)
	}
	return map[string]any{
		"port":    a.currentPort(),         // 当前正在使用的端口
		"value":   value,                   // 管理页面保存的设置（留空为 ""）
		"default": defaultServerPort,       // 留空时使用的端口
		"pending": a.restartPending.Load(), // 正在切换端口
	}
}

func (a *server) adminGetPort(sess session) (any, error) {
	if _, err := a.requireMain(sess); err != nil {
		return nil, err
	}
	return a.portState(), nil
}

// adminSetPort 保存端口并自动切换（主管理员）
func (a *server) adminSetPort(sess session, body map[string]any) (any, error) {
	if _, err := a.requireMain(sess); err != nil {
		return nil, err
	}
	raw, ok := portInput(body)
	if !ok {
		return nil, bad(http.StatusBadRequest, "请输入端口号（留空则使用 80 端口）")
	}
	saved, err := parsePortInput(raw)
	if err != nil {
		return nil, bad(http.StatusBadRequest, err.Error())
	}
	target := effectivePort(saved)

	// 端口没有变化：只保存设置，不重启
	if target == a.currentPort() {
		if err := a.store.SetServerPort(saved); err != nil {
			return nil, err
		}
		return map[string]any{"port": target, "restart": false}, nil
	}

	if !a.restartPending.CompareAndSwap(false, true) {
		return nil, bad(http.StatusConflict, "服务正在切换端口，请稍后再试")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(a.cfg.host, strconv.Itoa(target)))
	if err != nil {
		a.restartPending.Store(false)
		return nil, bad(http.StatusBadRequest, fmt.Sprintf("端口 %d 无法使用（可能已被其他程序占用，或没有权限）：%v", target, err))
	}
	if err := a.store.SetServerPort(saved); err != nil {
		_ = ln.Close()
		a.restartPending.Store(false)
		return nil, err
	}
	// 稍等片刻再切换，确保本次响应先送达浏览器
	go func() {
		time.Sleep(500 * time.Millisecond)
		a.restartCh <- portChange{ln: ln, port: target}
	}()
	return map[string]any{"port": target, "restart": true}, nil
}
