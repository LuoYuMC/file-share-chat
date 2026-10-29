package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	sessionCookie      = "sid"
	sessionTTL         = 7 * 24 * time.Hour
	maxBodyBytes       = 1 << 20
	maxMessageLen      = 30000 // 单条消息（大群、私聊、小群；纯文本与 HTML 相同）的字数上限
	maxAnnouncementLen = 2000
	maxHTMLLen         = 50000 // HTML 类型（公告、提示）的字数上限
	kindText           = "text"
	kindHTML           = "html"
)

var (
	usernameRE = regexp.MustCompile(`^[\p{L}\p{N}_.\-]{2,32}$`)
	aliasRE    = regexp.MustCompile(`^[\p{L}\p{N}_.\-]{1,20}$`)
	mentionRE  = regexp.MustCompile(`@([\p{L}\p{N}_.\-]{1,32})`)
	domainRE   = regexp.MustCompile(`^([a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	// 这些顶级域会被浏览器强制使用 HTTPS（HSTS），会导致 http://域名/ 无法访问
	badTLDs = map[string]bool{"dev": true, "app": true, "page": true, "new": true, "foo": true,
		"google": true, "android": true, "chrome": true, "gle": true}
	// 页面内可预览的类型（SVG 作为图片可能含脚本，因此不以图片预览；仅作为文本显示）
	previewImageExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true}
	previewVideoExt = map[string]bool{".mp4": true, ".webm": true, ".ogv": true, ".mov": true}
	previewAudioExt = map[string]bool{".mp3": true, ".wav": true, ".flac": true, ".ogg": true, ".oga": true,
		".opus": true, ".m4a": true, ".aac": true}
	// 文本预览：只读取、不提供保存接口；统一以 text/plain 返回，避免 HTML/JS/SVG 被浏览器执行
	previewTextExt = map[string]bool{
		".txt": true, ".log": true, ".md": true, ".csv": true, ".tsv": true, ".json": true, ".xml": true,
		".html": true, ".htm": true, ".css": true, ".js": true, ".ts": true, ".svg": true,
		".ps1": true, ".bat": true, ".cmd": true, ".sh": true, ".py": true, ".go": true, ".java": true,
		".c": true, ".h": true, ".cpp": true, ".hpp": true, ".cs": true, ".rs": true, ".sql": true,
		".yaml": true, ".yml": true, ".ini": true, ".conf": true, ".cfg": true, ".toml": true, ".env": true,
	}
)

// maxTextPreviewBytes 文本预览的最大文件大小（超过则不提供预览，避免页面卡顿）
const maxTextPreviewBytes = 2 << 20

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func bad(status int, msg string) error { return &apiError{status: status, msg: msg} }

type session struct {
	UserID   int64
	Username string
	Created  time.Time
}

type server struct {
	cfg   *config
	paths pathSet
	store *Store

	mu       sync.Mutex
	syncMu   sync.Mutex // 串行化“修改状态 + 重写 hosts”，避免并发写入
	sessions map[string]session

	portMu         sync.Mutex
	port           int             // 当前监听的端口
	restartCh      chan portChange // 管理页面修改端口后，通知主循环切换端口
	restartPending atomic.Bool     // 正在等待切换端口（防止重复提交）
}

func newServer(cfg *config, p pathSet, store *Store) *server {
	s := &server{cfg: cfg, paths: p, store: store, sessions: map[string]session{}, restartCh: make(chan portChange, 1)}
	s.loadSessions() // 恢复上次运行时保存的登录状态（重启后仍保持登录）
	return s
}

// ServeHTTP 入口（聊天与文件服务，端口见管理页面设置，默认 80）：
//  1. /api/ 开头：接口（/api/file 为文件预览，需要登录）
//  2. 其他任何请求：返回网页 index.html
func (a *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[panic] %v", rec)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "服务器内部错误"})
		}
	}()

	if strings.HasPrefix(r.URL.Path, "/api/") {
		if r.URL.Path == "/api/file" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			a.serveFile(w, r)
			return
		}
		a.handleAPI(w, r)
		return
	}
	a.servePage(w, r)
}

func (a *server) servePage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "不支持的请求方法"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(indexHTML)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(indexHTML)
	}
}

// siteURL 生成访问地址：端口为 80 时省略端口号
func siteURL(host string, port int) string {
	if port == 80 {
		return "http://" + host + "/"
	}
	return fmt.Sprintf("http://%s:%d/", host, port)
}

// previewKind 返回文件的预览类型：image / video / audio / text；不支持预览返回空字符串
func previewKind(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case previewImageExt[ext]:
		return "image"
	case previewVideoExt[ext]:
		return "video"
	case previewAudioExt[ext]:
		return "audio"
	case previewTextExt[ext]:
		return "text"
	}
	return ""
}

// serveText 返回文本内容（只读）：超过大小上限或含二进制内容（NUL 字节）时拒绝
func serveText(w http.ResponseWriter, full string, fi os.FileInfo) {
	if fi.Size() > maxTextPreviewBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "文本文件超过 2 MB，无法预览"})
		return
	}
	data, err := os.ReadFile(full)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "文件无法读取"})
		return
	}
	if bytes.IndexByte(data, 0) >= 0 {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "该文件不是文本文件，无法预览"})
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// serveFile 返回文件内容供页面预览（仅登录用户；图片、视频、音频、文本；视频和音频支持拖动所需的 Range 请求）
func (a *server) serveFile(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := a.currentUser(r); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "请先登录"})
		return
	}
	full, ok := resolveFile(a.paths.files, r.URL.Query().Get("path"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "文件不存在"})
		return
	}
	name := filepath.Base(full)
	kind := previewKind(name)
	if kind == "" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "该文件类型不支持预览"})
		return
	}
	f, err := os.Open(full)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "文件无法读取"})
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "文件无法读取"})
		return
	}
	if kind == "text" {
		f.Close()
		serveText(w, full, fi)
		return
	}
	h := w.Header()
	h.Set("Content-Type", guessType(name))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// ---------------------------------------------------------------- 会话

func (a *server) newSession(u User) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions[tokenKey(token)] = session{UserID: u.ID, Username: u.Username, Created: time.Now()}
	a.saveSessionsLocked()
	return token, nil
}

func (a *server) currentUser(r *http.Request) (string, session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return "", session{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key := tokenKey(c.Value)
	s, ok := a.sessions[key]
	if !ok {
		return "", session{}, false
	}
	if time.Since(s.Created) > sessionTTL {
		delete(a.sessions, key)
		a.saveSessionsLocked()
		return "", session{}, false
	}
	return c.Value, s, true
}

// dropUserSessions 删除某账号的所有会话，保留 keepToken（当前设备）
func (a *server) dropUserSessions(userID int64, keepToken string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	keepKey := ""
	if keepToken != "" {
		keepKey = tokenKey(keepToken)
	}
	changed := false
	for key, s := range a.sessions {
		if s.UserID == userID && key != keepKey {
			delete(a.sessions, key)
			changed = true
		}
	}
	if changed {
		a.saveSessionsLocked()
	}
}

func setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge,
	})
}

// ---------------------------------------------------------------- API 基础

func (a *server) handleAPI(w http.ResponseWriter, r *http.Request) {
	payload, err := a.dispatchAPI(w, r)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			writeJSON(w, ae.status, map[string]string{"error": ae.msg})
			return
		}
		log.Printf("[error] %s %s: %v", r.Method, r.URL.Path, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "服务器内部错误：" + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// readBody 解析 POST 的 JSON 请求体（要求 Content-Type 为 application/json，防止跨站表单提交）
func readBody(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	body := map[string]any{}
	if r.Method != http.MethodPost {
		return body, nil
	}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if !strings.Contains(ct, "application/json") {
		return nil, bad(http.StatusUnsupportedMediaType, "请求类型必须为 application/json")
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return nil, bad(http.StatusRequestEntityTooLarge, "请求内容过大")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return body, nil
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, bad(http.StatusBadRequest, "请求格式错误")
	}
	if body == nil {
		body = map[string]any{}
	}
	return body, nil
}

func strField(body map[string]any, key string) string {
	switch v := body[key].(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

func int64Field(body map[string]any, key string) (int64, bool) {
	switch v := body[key].(type) {
	case float64:
		return int64(v), v == float64(int64(v))
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	}
	return 0, false
}

func checkNewPassword(pw string) error {
	if n := utf8.RuneCountInString(pw); n < 6 || n > 128 {
		return bad(http.StatusBadRequest, "密码长度需为 6-128 位")
	}
	return nil
}

// validContent 去掉首尾空白并检查长度
func validContent(s string, max int, emptyMsg string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", bad(http.StatusBadRequest, emptyMsg)
	}
	if utf8.RuneCountInString(s) > max {
		return "", bad(http.StatusBadRequest, fmt.Sprintf("内容长度不能超过 %d 字", max))
	}
	return s, nil
}

// parseKind 读取消息类型："" 或 "text" 为纯文本，"html" 为 HTML。
func parseKind(body map[string]any) (string, error) {
	switch strField(body, "kind") {
	case "", kindText:
		return kindText, nil
	case kindHTML:
		return kindHTML, nil
	}
	return "", bad(http.StatusBadRequest, "内容类型错误")
}

// requireHTMLSender HTML 消息只允许主管理员和副管理员发送（纯文本不受限制）
func (a *server) requireHTMLSender(sess session, kind string) error {
	if kind != kindHTML {
		return nil
	}
	u, ok := a.store.UserByID(sess.UserID)
	if !ok || !u.IsStaff() {
		return bad(http.StatusForbidden, "只有主管理员和副管理员可以发送HTML消息")
	}
	return nil
}

// checkKindContent 按类型校验内容：HTML 最多 htmlMax 字，纯文本最多 textMax 字
func checkKindContent(kind, s string, textMax, htmlMax int, emptyMsg string) (string, error) {
	max := textMax
	if kind == kindHTML {
		max = htmlMax
	}
	return validContent(s, max, emptyMsg)
}

// quoteOf 生成引用快照（之后原消息变化不影响引用内容）。HTML 消息统一显示为“HTML消息”，其余截取前 60 字。
func quoteOf(id int64, username, content, kind string) *Quote {
	q := &Quote{ID: id, Username: username, Kind: kindText, Content: "HTML消息"}
	if kind == kindHTML {
		q.Kind = kindHTML
	} else {
		r := []rune(content)
		if len(r) > 60 {
			r = r[:60]
		}
		q.Content = string(r)
	}
	return q
}

func (a *server) dispatchAPI(w http.ResponseWriter, r *http.Request) (any, error) {
	path := r.URL.Path
	method := r.Method
	body, err := readBody(w, r)
	if err != nil {
		return nil, err
	}

	// 公开接口
	switch {
	case path == "/api/me" && method == http.MethodGet:
		_, s, ok := a.currentUser(r)
		if !ok {
			return map[string]any{"user": nil}, nil
		}
		u, ok := a.store.UserByID(s.UserID)
		if !ok {
			return map[string]any{"user": nil}, nil
		}
		return map[string]any{"user": userJSON(u)}, nil
	case path == "/api/register" && method == http.MethodPost:
		return a.register(w, body)
	case path == "/api/login" && method == http.MethodPost:
		return a.login(w, body)
	case path == "/api/logout" && method == http.MethodPost:
		if tok, _, ok := a.currentUser(r); ok {
			a.deleteSession(tok)
		}
		setSessionCookie(w, "", -1)
		return map[string]any{"ok": true}, nil
	}

	// 以下接口需要登录
	token, sess, ok := a.currentUser(r)
	if !ok {
		return nil, bad(http.StatusUnauthorized, "请先登录")
	}
	switch {
	case path == "/api/password" && method == http.MethodPost:
		return a.changePassword(sess, token, body)
	case path == "/api/account/username" && method == http.MethodPost:
		return a.changeUsername(sess, body)
	case path == "/api/account/delete" && method == http.MethodPost:
		return a.deleteSelf(w, sess)
	case path == "/api/messages" && method == http.MethodGet:
		return a.listMessages(r, sess)
	case path == "/api/messages" && method == http.MethodPost:
		return a.sendMessage(sess, body)
	case path == "/api/private" && method == http.MethodGet:
		return a.listPrivate(r, sess)
	case path == "/api/private" && method == http.MethodPost:
		return a.sendPrivate(sess, body)
	case path == "/api/private/latest" && method == http.MethodGet:
		return a.privateLatest(sess), nil
	case path == "/api/search" && method == http.MethodGet:
		return a.searchChats(r, sess)
	case path == "/api/chat/summary" && method == http.MethodPost:
		return a.chatSummary(body, sess)
	case path == "/api/recall" && method == http.MethodPost:
		return a.recallMessage(sess, body)
	case path == "/api/blacklist" && method == http.MethodPost:
		return a.setBlacklist(sess, body)
	case path == "/api/groups" && method == http.MethodGet:
		return a.listGroups(sess)
	case path == "/api/groups" && method == http.MethodPost:
		return a.createGroup(sess, body)
	case path == "/api/groups/detail" && method == http.MethodGet:
		return a.groupDetail(sess, r)
	case path == "/api/groups/invite" && method == http.MethodPost:
		return a.groupInvite(sess, body)
	case path == "/api/invites/accept" && method == http.MethodPost:
		return a.acceptInvite(sess, body)
	case path == "/api/groups/kick" && method == http.MethodPost:
		return a.groupKick(sess, body)
	case path == "/api/groups/mute" && method == http.MethodPost:
		return a.groupMute(sess, body)
	case path == "/api/groups/rename" && method == http.MethodPost:
		return a.groupRename(sess, body)
	case path == "/api/groups/dissolve" && method == http.MethodPost:
		return a.groupDissolve(sess, body)
	case path == "/api/groups/leave" && method == http.MethodPost:
		return a.groupLeave(sess, body)
	case path == "/api/group-messages" && method == http.MethodGet:
		return a.listGroupMessages(r, sess)
	case path == "/api/group-messages" && method == http.MethodPost:
		return a.sendGroupMessage(sess, body)
	case path == "/api/users" && method == http.MethodGet:
		return a.listUsers(sess.UserID), nil
	case path == "/api/files" && method == http.MethodGet:
		files, err := listFiles(a.paths.files)
		if err != nil {
			return nil, err
		}
		return map[string]any{"files": files}, nil
	case path == "/api/announcements" && method == http.MethodGet:
		return a.listAnnouncements(), nil
	case path == "/api/announcements" && method == http.MethodPost:
		return a.addAnnouncement(sess, body)
	case path == "/api/announcements/delete" && method == http.MethodPost:
		return a.deleteAnnouncement(sess, body)
	case path == "/api/admin/pending" && method == http.MethodGet:
		return a.adminPending(sess)
	case path == "/api/admin/users" && method == http.MethodGet:
		return a.adminListUsers(sess)
	case path == "/api/admin/mute" && method == http.MethodPost:
		return a.adminSetMute(sess, body)
	case path == "/api/admin/bigmute" && method == http.MethodPost:
		return a.adminSetBigMute(sess, body)
	case path == "/api/admin/user-groups" && method == http.MethodGet:
		return a.adminUserGroups(sess, r)
	case path == "/api/admin/groups/dissolve" && method == http.MethodPost:
		return a.adminDissolveGroup(sess, body)
	case path == "/api/admin/users/delete" && method == http.MethodPost:
		return a.adminDeleteUser(sess, body)
	case path == "/api/admin/subadmin" && method == http.MethodPost:
		return a.adminSetSubAdmin(sess, body)
	case path == "/api/admin/group-limit" && method == http.MethodPost:
		return a.adminSetGroupLimit(sess, body)
	case path == "/api/admin/alias" && method == http.MethodPost:
		return a.adminSetAlias(sess, body)
	case path == "/api/admin/port" && method == http.MethodGet:
		return a.adminGetPort(sess)
	case path == "/api/admin/port" && method == http.MethodPost:
		return a.adminSetPort(sess, body)
	case path == "/api/admin/hosts" && method == http.MethodGet:
		return a.adminListHosts(sess)
	case path == "/api/admin/hosts/add" && method == http.MethodPost:
		return a.adminAddHost(sess, body)
	case path == "/api/admin/hosts/update" && method == http.MethodPost:
		return a.adminUpdateHost(sess, body)
	case path == "/api/admin/hosts/remove" && method == http.MethodPost:
		return a.adminRemoveHost(sess, body)
	}
	return nil, bad(http.StatusNotFound, "接口不存在")
}

// ---------------------------------------------------------------- 账号

func userJSON(u User) map[string]any {
	return map[string]any{
		"id": u.ID, "username": u.Username, "alias": u.Alias,
		"is_admin": u.IsAdmin, "sub_admin": u.SubAdmin, "is_staff": u.IsStaff(), "muted": u.Muted,
		"big_muted": u.BigMuted, "pending": u.Pending,
	}
}

func (a *server) register(w http.ResponseWriter, body map[string]any) (any, error) {
	username := strings.TrimSpace(strField(body, "username"))
	password := strField(body, "password")
	if !usernameRE.MatchString(username) {
		return nil, bad(http.StatusBadRequest, "用户名需为 2-32 位，可包含字母、数字、下划线、点或中划线")
	}
	if isReservedName(username) {
		return nil, bad(http.StatusBadRequest, "该用户名为系统保留，请换一个用户名")
	}
	if err := checkNewPassword(password); err != nil {
		return nil, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	u, err := a.store.AddUser(username, hash)
	if errors.Is(err, errUserExists) {
		return nil, bad(http.StatusConflict, "用户名已存在")
	}
	if err != nil {
		return nil, err
	}
	token, err := a.newSession(u)
	if err != nil {
		return nil, err
	}
	setSessionCookie(w, token, int(sessionTTL.Seconds()))
	return map[string]any{"user": userJSON(u)}, nil
}

func (a *server) login(w http.ResponseWriter, body map[string]any) (any, error) {
	username := strings.TrimSpace(strField(body, "username"))
	password := strField(body, "password")
	u, ok := a.store.FindUser(username)
	if !ok || !verifyPassword(password, u.PWHash) {
		return nil, bad(http.StatusUnauthorized, "用户名或密码错误")
	}
	token, err := a.newSession(u)
	if err != nil {
		return nil, err
	}
	setSessionCookie(w, token, int(sessionTTL.Seconds()))
	return map[string]any{"user": userJSON(u)}, nil
}

func (a *server) changePassword(sess session, token string, body map[string]any) (any, error) {
	u, ok := a.store.UserByID(sess.UserID)
	if !ok {
		return nil, bad(http.StatusUnauthorized, "请先登录")
	}
	oldPw := strField(body, "old_password")
	newPw := strField(body, "new_password")
	if !verifyPassword(oldPw, u.PWHash) {
		return nil, bad(http.StatusBadRequest, "原密码不正确")
	}
	if err := checkNewPassword(newPw); err != nil {
		return nil, err
	}
	if newPw == oldPw {
		return nil, bad(http.StatusBadRequest, "新密码不能与原密码相同")
	}
	hash, err := hashPassword(newPw)
	if err != nil {
		return nil, err
	}
	if err := a.store.SetPassword(u.ID, hash); err != nil {
		return nil, err
	}
	// 修改密码后，该账号在其它设备上的登录失效；当前设备保持登录
	a.dropUserSessions(u.ID, token)
	return map[string]any{"ok": true}, nil
}

// nameMap 返回 用户ID → 别名（显示时使用当前别名）
func (a *server) nameMap() map[int64]string {
	out := map[int64]string{}
	for _, u := range a.store.ListUsers() {
		out[u.ID] = u.Alias
	}
	return out
}

// listUsers 所有用户（侧边栏私聊列表用）；blocked 表示当前用户是否已将其加入黑名单，blocked_me 表示其是否已将当前用户加入黑名单
func (a *server) listUsers(uid int64) map[string]any {
	users := a.store.ListUsers()
	var blocked []int64
	for _, u := range users {
		if u.ID == uid {
			blocked = u.Blocked
		}
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		if u.Pending && u.ID != uid { // 待处理账号不出现在私聊、拉入等列表中
			continue
		}
		out = append(out, map[string]any{
			"id": u.ID, "username": u.Username, "alias": u.Alias,
			"is_admin": u.IsAdmin, "sub_admin": u.SubAdmin, "muted": u.Muted, "big_muted": u.BigMuted,
			"blocked":    containsID(blocked, u.ID),
			"blocked_me": containsID(u.Blocked, uid), // 对方已将当前用户加入黑名单
		})
	}
	return map[string]any{"users": out}
}

// ---------------------------------------------------------------- 聊天（大群）

func messageJSON(m Message, alias string) map[string]any {
	mentions := m.Mentions
	if mentions == nil {
		mentions = []int64{}
	}
	kind := m.Kind
	if kind == "" {
		kind = kindText
	}
	return map[string]any{
		"id": m.ID, "user_id": m.UserID, "username": m.Username, "alias": alias,
		"content": m.Content, "created_at": m.CreatedAt, "mentions": mentions,
		"kind": kind, "quote": m.Quote, "recalled": m.Recalled,
	}
}

// muteFlags 返回用户的全局禁言与大群禁言状态
func (a *server) muteFlags(userID int64) (global, big bool) {
	u, ok := a.store.UserByID(userID)
	return ok && u.Muted, ok && u.BigMuted
}

func parseAfter(r *http.Request) (int64, error) {
	s := r.URL.Query().Get("after")
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, bad(http.StatusBadRequest, "after 参数错误")
	}
	return v, nil
}

func (a *server) listMessages(r *http.Request, sess session) (any, error) {
	after, before, rseq, withRseq, err := parseListOpts(r)
	if err != nil {
		return nil, err
	}
	res := a.store.ListMessages(after, before, rseq, withRseq && before == 0)
	names := a.nameMap()
	unames := a.userNames()
	out := make([]map[string]any, 0, len(res.Items))
	for _, m := range res.Items {
		j := messageJSON(m, names[m.UserID])
		if n, ok := unames[m.UserID]; ok {
			j["username"] = n
		}
		out = append(out, j)
	}
	gm, bm := a.muteFlags(sess.UserID)
	return map[string]any{"messages": out, "muted": gm, "big_muted": bm, "has_more": res.HasMore,
		"recalled_ids": res.Recalled, "rseq": res.RSeq}, nil
}

// resolveMentions 解析消息中的 @用户名 或 @别名，返回被提到的用户 ID（不含发送者自己）
func (a *server) resolveMentions(content string, self int64) []int64 {
	return resolveMentionsAmong(content, self, a.store.ListUsers())
}

// resolveMentionsAmong 在给定的候选用户中解析 @（大群为全部用户，小群为本群成员）
func resolveMentionsAmong(content string, self int64, candidates []User) []int64 {
	found := mentionRE.FindAllStringSubmatch(content, -1)
	if len(found) == 0 {
		return nil
	}
	want := map[string]bool{}
	for _, f := range found {
		want[strings.ToLower(f[1])] = true
	}
	var ids []int64
	for _, u := range candidates {
		if u.ID == self {
			continue
		}
		if want[strings.ToLower(u.Username)] || (u.Alias != "" && want[strings.ToLower(u.Alias)]) {
			ids = append(ids, u.ID)
		}
	}
	return ids
}

func (a *server) sendMessage(sess session, body map[string]any) (any, error) {
	u, ok := a.store.UserByID(sess.UserID)
	if !ok {
		return nil, bad(http.StatusUnauthorized, "请先登录")
	}
	if u.Muted {
		return nil, bad(http.StatusForbidden, "你已被管理员禁言，暂时不能发言（私聊和所有群聊）")
	}
	if u.BigMuted {
		return nil, bad(http.StatusForbidden, "你已被管理员禁言大群，暂时不能在大群发言")
	}
	kind, err := parseKind(body)
	if err != nil {
		return nil, err
	}
	if err := a.requireHTMLSender(sess, kind); err != nil {
		return nil, err
	}
	content, err := checkKindContent(kind, strField(body, "content"), maxMessageLen, maxMessageLen, "消息不能为空")
	if err != nil {
		return nil, err
	}
	var mentions []int64
	if kind == kindText {
		mentions = a.resolveMentions(content, u.ID)
	}
	var quote *Quote
	if qid, ok := int64Field(body, "quote_id"); ok && qid > 0 {
		qm, found := a.store.FindMessage(qid)
		if !found {
			return nil, bad(http.StatusNotFound, "被引用的消息不存在")
		}
		quote = quoteFor(qm.ID, qm.Username, qm.Content, qm.Kind, qm.Recalled)
	}
	m, err := a.store.AddMessage(Message{UserID: u.ID, Username: u.Username, Content: content,
		Mentions: mentions, Kind: kind, Quote: quote})
	if err != nil {
		return nil, err
	}
	return map[string]any{"message": messageJSON(m, u.Alias)}, nil
}

// ---------------------------------------------------------------- 私聊

func privateJSON(m PrivateMessage) map[string]any {
	kind := m.Kind
	if kind == "" {
		kind = kindText
	}
	return map[string]any{"id": m.ID, "from_id": m.FromID, "to_id": m.ToID, "content": m.Content,
		"created_at": m.CreatedAt, "kind": kind, "quote": m.Quote, "recalled": m.Recalled,
		"group_id": m.GroupID, "invite_done": m.InviteDone}
}

func (a *server) listPrivate(r *http.Request, sess session) (any, error) {
	with, ok := int64Field(map[string]any{"with": r.URL.Query().Get("with")}, "with")
	if !ok {
		return nil, bad(http.StatusBadRequest, "用户参数错误")
	}
	if _, ok := a.store.UserByID(with); !ok {
		return nil, bad(http.StatusNotFound, "用户不存在")
	}
	after, before, rseq, withRseq, err := parseListOpts(r)
	if err != nil {
		return nil, err
	}
	res := a.store.ListPrivate(sess.UserID, with, after, before, rseq, withRseq && before == 0)
	out := make([]map[string]any, 0, len(res.Items))
	for _, m := range res.Items {
		out = append(out, privateJSON(m))
	}
	return map[string]any{"messages": out, "has_more": res.HasMore, "recalled_ids": res.Recalled,
		"rseq": res.RSeq}, nil
}

// privateLatest 每个有私聊往来的用户的最后一条消息（侧边栏据此显示私聊红点）
func (a *server) privateLatest(sess session) any {
	latest := a.store.PrivateLatest(sess.UserID)
	out := make([]map[string]any, 0, len(latest))
	for peer, m := range latest {
		out = append(out, map[string]any{"user_id": peer, "last_id": m.ID, "last_from": m.FromID})
	}
	return map[string]any{"peers": out}
}

func (a *server) sendPrivate(sess session, body map[string]any) (any, error) {
	to, ok := int64Field(body, "to")
	if !ok {
		return nil, bad(http.StatusBadRequest, "用户参数错误")
	}
	if to == sess.UserID {
		return nil, bad(http.StatusBadRequest, "不能给自己发送私聊")
	}
	recipient, ok := a.store.UserByID(to)
	if !ok {
		return nil, bad(http.StatusNotFound, "用户不存在")
	}
	if containsID(recipient.Blocked, sess.UserID) {
		return nil, bad(http.StatusForbidden, "对方已将你加入黑名单，无法发送私聊消息")
	}
	kind, err := parseKind(body)
	if err != nil {
		return nil, err
	}
	if err := a.requireHTMLSender(sess, kind); err != nil {
		return nil, err
	}
	// 全局禁言：私聊和所有群聊都不能发言
	if sender, ok := a.store.UserByID(sess.UserID); ok && sender.Muted {
		return nil, bad(http.StatusForbidden, "你已被管理员禁言，暂时不能发言（私聊和所有群聊）")
	}
	content, err := checkKindContent(kind, strField(body, "content"), maxMessageLen, maxMessageLen, "消息不能为空")
	if err != nil {
		return nil, err
	}
	var quote *Quote
	if qid, ok := int64Field(body, "quote_id"); ok && qid > 0 {
		// 只能引用本对话中的消息
		qm, found := a.store.FindPrivate(qid)
		if !found || !((qm.FromID == sess.UserID && qm.ToID == to) || (qm.FromID == to && qm.ToID == sess.UserID)) {
			return nil, bad(http.StatusNotFound, "被引用的消息不存在")
		}
		name := "已删除用户"
		if fu, ok := a.store.UserByID(qm.FromID); ok {
			name = fu.Username
		}
		quote = quoteFor(qm.ID, name, qm.Content, qm.Kind, qm.Recalled)
	}
	m, err := a.store.AddPrivate(PrivateMessage{FromID: sess.UserID, ToID: to, Content: content,
		Kind: kind, Quote: quote})
	if err != nil {
		return nil, err
	}
	return map[string]any{"message": privateJSON(m)}, nil
}

// ---------------------------------------------------------------- 公告

func (a *server) listAnnouncements() map[string]any {
	names := a.nameMap()
	unames := a.userNames()
	list := a.store.ListAnnouncements()
	out := make([]map[string]any, 0, len(list))
	for _, an := range list {
		name := an.Username
		if n, ok := unames[an.UserID]; ok {
			name = n
		}
		kind := an.Kind
		if kind == "" {
			kind = kindText
		}
		out = append(out, map[string]any{
			"id": an.ID, "user_id": an.UserID, "username": name, "alias": names[an.UserID],
			"content": an.Content, "created_at": an.CreatedAt, "kind": kind,
		})
	}
	return map[string]any{"announcements": out}
}

func (a *server) addAnnouncement(sess session, body map[string]any) (any, error) {
	actor, err := a.requireStaff(sess)
	if err != nil {
		return nil, err
	}
	kind, err := parseKind(body)
	if err != nil {
		return nil, err
	}
	content, err := checkKindContent(kind, strField(body, "content"), maxAnnouncementLen, maxHTMLLen, "公告内容不能为空")
	if err != nil {
		return nil, err
	}
	an, err := a.store.AddAnnouncement(Announcement{UserID: actor.ID, Username: actor.Username,
		Content: content, Kind: kind})
	if err != nil {
		return nil, err
	}
	return map[string]any{"announcement": map[string]any{
		"id": an.ID, "user_id": an.UserID, "username": an.Username, "alias": actor.Alias,
		"content": an.Content, "created_at": an.CreatedAt, "kind": kind,
	}}, nil
}
func (a *server) deleteAnnouncement(sess session, body map[string]any) (any, error) {
	if _, err := a.requireStaff(sess); err != nil {
		return nil, err
	}
	id, ok := int64Field(body, "id")
	if !ok {
		return nil, bad(http.StatusBadRequest, "参数错误")
	}
	if err := a.store.DeleteAnnouncement(id); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, bad(http.StatusNotFound, "公告不存在")
		}
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// ---------------------------------------------------------------- 管理员 / 副管理员

// requireStaff 校验当前用户是否为主管理员或副管理员（每次都从数据库读取，角色变化立即生效）
func (a *server) requireStaff(sess session) (User, error) {
	u, ok := a.store.UserByID(sess.UserID)
	if !ok || !u.IsStaff() {
		return User{}, bad(http.StatusForbidden, "需要管理员权限")
	}
	return u, nil
}

// checkTarget 副管理员不能操作主管理员（主管理员可以操作所有人）
func checkTarget(actor, target User) error {
	if target.IsAdmin && !actor.IsAdmin {
		return bad(http.StatusForbidden, "副管理员无权操作主管理员")
	}
	return nil
}

// targetUser 从请求中读取 user_id 对应的用户
func (a *server) targetUser(body map[string]any) (User, error) {
	id, ok := int64Field(body, "user_id")
	if !ok {
		return User{}, bad(http.StatusBadRequest, "用户参数错误")
	}
	u, ok := a.store.UserByID(id)
	if !ok {
		return User{}, bad(http.StatusNotFound, "用户不存在")
	}
	return u, nil
}

func (a *server) adminPending(sess session) (any, error) {
	if _, err := a.requireStaff(sess); err != nil {
		return nil, err
	}
	return map[string]any{"count": a.store.PendingCount()}, nil
}

func (a *server) adminListUsers(sess session) (any, error) {
	if _, err := a.requireStaff(sess); err != nil {
		return nil, err
	}
	users := a.store.ListUsers()
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, map[string]any{
			"id": u.ID, "username": u.Username, "alias": u.Alias,
			"is_admin": u.IsAdmin, "sub_admin": u.SubAdmin, "muted": u.Muted, "big_muted": u.BigMuted,
			"pending": u.Pending, "created_at": u.CreatedAt,
			"group_limit": u.GroupLimitValue(),
		})
	}
	return map[string]any{"users": out, "pending": a.store.PendingCount()}, nil
}

func (a *server) adminSetMute(sess session, body map[string]any) (any, error) {
	actor, err := a.requireStaff(sess)
	if err != nil {
		return nil, err
	}
	target, err := a.targetUser(body)
	if err != nil {
		return nil, err
	}
	if err := checkTarget(actor, target); err != nil {
		return nil, err
	}
	if target.IsAdmin {
		return nil, bad(http.StatusBadRequest, "不能禁言主管理员账号")
	}
	muted, _ := body["muted"].(bool)
	if err := a.store.SetMuted(target.ID, muted); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// adminSetBigMute 主/副管理员设置大群禁言（只影响大群发言，不影响私聊与小群）
func (a *server) adminSetBigMute(sess session, body map[string]any) (any, error) {
	actor, err := a.requireStaff(sess)
	if err != nil {
		return nil, err
	}
	target, err := a.targetUser(body)
	if err != nil {
		return nil, err
	}
	if err := checkTarget(actor, target); err != nil {
		return nil, err
	}
	if target.IsAdmin {
		return nil, bad(http.StatusBadRequest, "不能禁言主管理员账号")
	}
	on, _ := body["muted"].(bool)
	if err := a.store.SetBigMuted(target.ID, on); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// adminDeleteUser 删除账号。历史消息保留；该账号的登录会话全部失效。
func (a *server) adminDeleteUser(sess session, body map[string]any) (any, error) {
	actor, err := a.requireMain(sess)
	if err != nil {
		return nil, err
	}
	target, err := a.targetUser(body)
	if err != nil {
		return nil, err
	}
	if target.ID == actor.ID {
		return nil, bad(http.StatusBadRequest, "不能删除自己的账号")
	}
	if err := checkTarget(actor, target); err != nil {
		return nil, err
	}
	if target.IsAdmin {
		return nil, bad(http.StatusBadRequest, "不能删除主管理员账号")
	}
	if err := a.store.DeleteUser(target.ID); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, bad(http.StatusNotFound, "用户不存在")
		}
		return nil, err
	}
	a.dropUserSessions(target.ID, "")
	return map[string]any{"ok": true}, nil
}

func (a *server) adminSetSubAdmin(sess session, body map[string]any) (any, error) {
	actor, err := a.requireMain(sess)
	if err != nil {
		return nil, err
	}
	target, err := a.targetUser(body)
	if err != nil {
		return nil, err
	}
	if err := checkTarget(actor, target); err != nil {
		return nil, err
	}
	if target.IsAdmin {
		return nil, bad(http.StatusBadRequest, "主管理员身份不可更改")
	}
	on, _ := body["sub_admin"].(bool)
	if err := a.store.SetSubAdmin(target.ID, on); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

func (a *server) adminSetAlias(sess session, body map[string]any) (any, error) {
	actor, err := a.requireStaff(sess)
	if err != nil {
		return nil, err
	}
	target, err := a.targetUser(body)
	if err != nil {
		return nil, err
	}
	if err := checkTarget(actor, target); err != nil {
		return nil, err
	}
	alias := strings.TrimSpace(strField(body, "alias"))
	if alias != "" && !aliasRE.MatchString(alias) {
		return nil, bad(http.StatusBadRequest, "别名为 1-20 位字母、数字、下划线、点或中划线，留空可清除")
	}
	if err := a.store.SetAlias(target.ID, alias); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// updateHosts 在 syncMu 保护下执行状态修改（mutate，可为 nil），然后按解析域名列表重写 hosts。
// 状态修改失败时返回 err（不写 hosts）；hosts 写入失败只返回 hostsErr，已保存的状态保持不变。
func (a *server) updateHosts(mutate func() error) (hostsErr error, err error) {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	if mutate != nil {
		if err := mutate(); err != nil {
			return nil, err
		}
	}
	return applyHosts(a.cfg.hostsPath, a.paths.hostsBackup, a.store.HostList()), nil
}

// addHostsError 把 hosts 写入失败的原因及需要手动添加的全部记录写入响应
func addHostsError(st map[string]any, hostsErr error, entries []HostEntry) {
	st["hosts_error"] = hostsErr.Error()
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, e.IP+" "+e.Domain)
	}
	st["manual_lines"] = lines
	if len(lines) > 0 {
		st["manual_line"] = lines[len(lines)-1]
	}
}

func (a *server) hostListState(hostsErr error) map[string]any {
	entries := a.store.HostList()
	st := map[string]any{"hosts": entries, "max": maxHostDomains}
	if hostsErr != nil {
		addHostsError(st, hostsErr, entries)
	}
	return st
}

func (a *server) adminListHosts(sess session) (any, error) {
	if _, err := a.requireMain(sess); err != nil {
		return nil, err
	}
	return a.hostListState(nil), nil
}

// normalizeIP 空值使用默认 IP；返回规范化后的 IP，格式不正确时 ok 为 false
func normalizeIP(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultHostIP, true
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return "", false
	}
	return ip.String(), true
}

func validDomain(d string) bool {
	if len(d) < 4 || len(d) > 253 || !domainRE.MatchString(d) {
		return false
	}
	tld := d[strings.LastIndex(d, ".")+1:]
	return !badTLDs[tld]
}

func (a *server) adminAddHost(sess session, body map[string]any) (any, error) {
	if _, err := a.requireMain(sess); err != nil {
		return nil, err
	}
	domain := strings.ToLower(strings.TrimSpace(strField(body, "domain")))
	if !validDomain(domain) {
		return nil, bad(http.StatusBadRequest, "域名格式不正确，请使用类似 demo.local 的域名（不要用 .dev / .app 等）")
	}
	ip, ok := normalizeIP(strField(body, "ip"))
	if !ok {
		return nil, bad(http.StatusBadRequest, "IP 地址格式不正确")
	}
	hostsErr, err := a.updateHosts(func() error { return a.store.AddHost(HostEntry{Domain: domain, IP: ip}) })
	if errors.Is(err, errHostExists) {
		return nil, bad(http.StatusConflict, err.Error())
	}
	if errors.Is(err, errHostListFull) {
		return nil, bad(http.StatusBadRequest, err.Error())
	}
	if err != nil {
		return nil, err
	}
	return a.hostListState(hostsErr), nil
}

func (a *server) adminUpdateHost(sess session, body map[string]any) (any, error) {
	if _, err := a.requireMain(sess); err != nil {
		return nil, err
	}
	domain := strings.ToLower(strings.TrimSpace(strField(body, "domain")))
	ip, ok := normalizeIP(strField(body, "ip"))
	if !ok {
		return nil, bad(http.StatusBadRequest, "IP 地址格式不正确")
	}
	hostsErr, err := a.updateHosts(func() error { return a.store.SetHostIP(domain, ip) })
	if errors.Is(err, errHostNotFound) {
		return nil, bad(http.StatusNotFound, err.Error())
	}
	if err != nil {
		return nil, err
	}
	return a.hostListState(hostsErr), nil
}

func (a *server) adminRemoveHost(sess session, body map[string]any) (any, error) {
	if _, err := a.requireMain(sess); err != nil {
		return nil, err
	}
	domain := strings.ToLower(strings.TrimSpace(strField(body, "domain")))
	hostsErr, err := a.updateHosts(func() error { return a.store.RemoveHost(domain) })
	if errors.Is(err, errHostNotFound) {
		return nil, bad(http.StatusNotFound, err.Error())
	}
	if err != nil {
		return nil, err
	}
	return a.hostListState(hostsErr), nil
}

// adminSetGroupLimit 主/副管理员设置用户可创建的小群数量（非负整数）。
// 只禁止再创建，不会解散该用户已有的小群。
func (a *server) adminSetGroupLimit(sess session, body map[string]any) (any, error) {
	actor, err := a.requireStaff(sess)
	if err != nil {
		return nil, err
	}
	target, err := a.targetUser(body)
	if err != nil {
		return nil, err
	}
	if err := checkTarget(actor, target); err != nil {
		return nil, err
	}
	if target.IsAdmin {
		return nil, bad(http.StatusBadRequest, "主管理员不受小群数量限制")
	}
	limit, ok := int64Field(body, "limit")
	if !ok || limit < 0 || limit > 1<<31-1 { // 只要求非负整数，仅防止溢出
		return nil, bad(http.StatusBadRequest, "请输入非负整数")
	}
	if err := a.store.SetGroupLimit(target.ID, int(limit)); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}
