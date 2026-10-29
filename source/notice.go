package main

import (
	"bytes"
	_ "embed"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 80 端口提示页：用户通过 IP 或域名访问 80 端口时显示的页面。
// 内容（提示文字、前往聊天的完整地址）由主管理员与副管理员设置，保存在 data/app.json 中。

//go:embed static/notice.html
var noticeHTML string

var noticeTmpl = template.Must(template.New("notice").Parse(noticeHTML))

const (
	defaultNoticeMessage = "本程序聊天与文件服务运行在 2147 端口，请点击下方按钮前往。"
	maxNoticeMessage     = 500
	maxNoticeTarget      = 300
)

// NoticeConfig 提示页内容。Target 为空时“前往聊天”按钮不可用。
type NoticeConfig struct {
	Message string `json:"message"`
	Target  string `json:"target"`
	Format  string `json:"format,omitempty"` // "text"（默认）或 "html"（用 iframe 显示）
}

// Notice 返回当前提示页设置；从未设置过时使用默认文字，地址为空。
func (s *Store) Notice() NoticeConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db.Notice == nil {
		return NoticeConfig{Message: defaultNoticeMessage, Format: kindText}
	}
	n := *s.db.Notice
	if n.Format == "" {
		n.Format = kindText
	}
	return n
}

// SetNotice 保存提示页设置。
func (s *Store) SetNotice(n NoticeConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Notice = &n
	return s.saveLocked()
}

// validateNotice 校验并规范化提示页设置。
func validateNotice(message, target, format string) (NoticeConfig, error) {
	message = strings.TrimSpace(message)
	target = strings.TrimSpace(target)
	kind, err := parseKind(map[string]any{"kind": format})
	if err != nil {
		return NoticeConfig{}, err
	}
	if kind == kindHTML {
		if utf8.RuneCountInString(message) > maxHTMLLen {
			return NoticeConfig{}, bad(400, "HTML 内容最多 50000 字")
		}
	} else if utf8.RuneCountInString(message) > maxNoticeMessage {
		return NoticeConfig{}, bad(400, "提示文字最多 500 字")
	}
	if target != "" {
		if utf8.RuneCountInString(target) > maxNoticeTarget {
			return NoticeConfig{}, bad(400, "前往聊天的地址最多 300 字")
		}
		u, err := url.Parse(target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return NoticeConfig{}, bad(400, "前往聊天的地址必须是完整的 http:// 或 https:// 地址，例如 http://192.168.1.5:2147/")
		}
	}
	return NoticeConfig{Message: message, Target: target, Format: kind}, nil
}

func (a *server) adminGetNotice(sess session) (any, error) {
	if _, err := a.requireMain(sess); err != nil {
		return nil, err
	}
	n := a.store.Notice()
	return map[string]any{"message": n.Message, "target": n.Target, "format": n.Format}, nil
}

func (a *server) adminSetNotice(sess session, body map[string]any) (any, error) {
	if _, err := a.requireMain(sess); err != nil {
		return nil, err
	}
	n, err := validateNotice(strField(body, "message"), strField(body, "target"), strField(body, "format"))
	if err != nil {
		return nil, err
	}
	if err := a.store.SetNotice(n); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "message": n.Message, "target": n.Target, "format": n.Format}, nil
}

// serveNotice 处理 80 端口的所有请求：返回提示页（不提供接口与文件）。
func (a *server) serveNotice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "不支持的请求方法", http.StatusMethodNotAllowed)
		return
	}
	n := a.store.Notice()
	var buf bytes.Buffer
	data := struct {
		Message, Target string
		HTML            bool
	}{n.Message, n.Target, n.Format == kindHTML}
	if err := noticeTmpl.Execute(&buf, data); err != nil {
		http.Error(w, "服务器内部错误", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(buf.Bytes())
	}
}
