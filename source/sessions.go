package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

// 登录会话持久化：保存在 data/sessions.json，只保存 token 的 SHA-256 哈希（不保存明文 token）。
// 重启程序后，未过期的会话仍然有效；登出、删除账号、修改密码会删除对应会话。

type persistedSession struct {
	UserID   int64     `json:"user_id"`
	Username string    `json:"username"`
	Created  time.Time `json:"created"`
}

// tokenKey 会话在内存和文件中的键（token 的哈希）
func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (a *server) sessionsFile() string {
	return filepath.Join(a.paths.data, "sessions.json")
}

// loadSessions 启动时读取会话文件，丢弃已过期的会话
func (a *server) loadSessions() {
	raw, err := os.ReadFile(a.sessionsFile())
	if err != nil {
		return // 没有会话文件表示没有已保存的登录
	}
	var m map[string]persistedSession
	if err := json.Unmarshal(raw, &m); err != nil {
		log.Printf("[警告] 读取登录状态失败，需要重新登录：%v", err)
		return
	}
	now := time.Now()
	for k, s := range m {
		if now.Sub(s.Created) > sessionTTL {
			continue
		}
		a.sessions[k] = session{UserID: s.UserID, Username: s.Username, Created: s.Created}
	}
}

// saveSessionsLocked 把当前会话写入文件（调用方须持有 a.mu）
func (a *server) saveSessionsLocked() {
	m := make(map[string]persistedSession, len(a.sessions))
	for k, s := range a.sessions {
		m[k] = persistedSession{UserID: s.UserID, Username: s.Username, Created: s.Created}
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		log.Printf("[警告] 保存登录状态失败：%v", err)
		return
	}
	tmp := a.sessionsFile() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		log.Printf("[警告] 保存登录状态失败：%v", err)
		return
	}
	if err := os.Rename(tmp, a.sessionsFile()); err != nil {
		log.Printf("[警告] 保存登录状态失败：%v", err)
	}
}

// deleteSession 删除一个会话（token 为原始值）并保存
func (a *server) deleteSession(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.sessions[tokenKey(token)]; ok {
		delete(a.sessions, tokenKey(token))
		a.saveSessionsLocked()
	}
}
