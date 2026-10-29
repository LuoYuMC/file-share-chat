package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	errUserExists   = errors.New("用户名已存在")
	errHostExists   = errors.New("该域名已在列表中")
	errHostNotFound = errors.New("该域名不在列表中")
	errHostListFull = errors.New("域名列表已满（最多 50 个）")
	errNotFound     = errors.New("记录不存在")

	errGroupNotFound   = errors.New("群聊不存在或你已不在该群")
	errGroupOwnerOnly  = errors.New("只有群主可以执行该操作")
	errGroupLimit      = errors.New("已达到可创建的小群数量上限")
	errAlreadyMember   = errors.New("该用户已在群中")
	errNotMember       = errors.New("该用户不在群中")
	errOwnerTarget     = errors.New("不能对群主执行该操作")
	errOwnerLeave      = errors.New("群主不能退出群聊")
	errGroupMuted      = errors.New("你已被群主禁言，暂时不能在本群发言")
	errInviteUsed      = errors.New("该邀请已处理")
	errMsgNotFound     = errors.New("消息不存在")
	errNotSender       = errors.New("只能撤回自己发送的消息")
	errRecallExpired   = errors.New("消息发出已超过 2 分钟，无法撤回")
	errAlreadyRecalled = errors.New("消息已撤回")
)

const (
	maxAnnouncements  = 200
	recallWindowSec   = 120        // 消息发出后可撤回的时间（秒）
	pageSize          = 100        // 首次加载、加载更早消息时每次返回的条数
	incrementalSize   = 200        // 增量拉取时最多返回的条数
	maxGroupNameLen   = 20         // 小群名称最多字数
	kindRecalled      = "recalled" // 引用类型：原消息已被撤回
	quoteRecalledText = "原消息已被撤回"  // 引用原消息被撤回后的显示文字
	pbkdf2Iterations  = 200000
	maxHostDomains    = 50          // 解析域名列表的最大数量
	defaultHostIP     = "127.0.0.1" // 解析域名的默认目标 IP
	adminUsername     = "admin"     // 主管理员账号（系统保留名）
	defaultServerPort = 80          // 服务端口未设置（管理页面留空）时使用的端口
	defaultAdminPass  = "admin123"  // 主管理员默认密码，可在“账号与密码”中修改
)

type User struct {
	ID         int64   `json:"id"`
	Username   string  `json:"username"`
	PWHash     string  `json:"pw_hash"`
	CreatedAt  int64   `json:"created_at"`
	IsAdmin    bool    `json:"is_admin,omitempty"`    // 主管理员
	SubAdmin   bool    `json:"sub_admin,omitempty"`   // 副管理员：除对主管理员的操作外，权限与主管理员相同
	Muted      bool    `json:"muted,omitempty"`       // 全局禁言：私聊和所有群聊都不能发送（可以看消息）
	BigMuted   bool    `json:"big_muted,omitempty"`   // 大群禁言：只影响大群发言，与全局禁言、私聊、小群互不影响
	Pending    bool    `json:"pending,omitempty"`     // 新注册、尚未被管理员处理（用于红点提示）
	Alias      string  `json:"alias,omitempty"`       // 别名，由管理员设置，仅用于显示
	Blocked    []int64 `json:"blocked,omitempty"`     // 本用户拉黑的用户 ID（私聊：对方无法再向本用户发消息）
	GroupLimit *int    `json:"group_limit,omitempty"` // 可创建的小群数量上限（nil 表示默认 1；主/副管理员不受限）
}

// IsStaff 是否拥有管理员权限（主管理员或副管理员）
func (u User) IsStaff() bool { return u.IsAdmin || u.SubAdmin }

type Message struct {
	ID        int64   `json:"id"`
	UserID    int64   `json:"user_id"`
	Username  string  `json:"username"`
	Content   string  `json:"content"`
	CreatedAt int64   `json:"created_at"`
	Mentions  []int64 `json:"mentions,omitempty"`   // 被 @ 的用户 ID
	Kind      string  `json:"kind,omitempty"`       // "" 或 "text"：纯文本；"html"：HTML 消息
	Quote     *Quote  `json:"quote,omitempty"`      // 引用的消息（发送时的快照）
	Recalled  bool    `json:"recalled,omitempty"`   // 已撤回（内容已清除）
	RecallSeq int64   `json:"recall_seq,omitempty"` // 撤回序号（客户端据此增量同步撤回）
}

// Quote 被引用消息的快照（引用时保存，之后原消息变化不影响引用内容）
type Quote struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Content  string `json:"content"` // HTML 消息统一为 “HTML消息”
	Kind     string `json:"kind,omitempty"`
}

type PrivateMessage struct {
	ID         int64  `json:"id"`
	FromID     int64  `json:"from_id"`
	ToID       int64  `json:"to_id"`
	Content    string `json:"content"`
	CreatedAt  int64  `json:"created_at"`
	Kind       string `json:"kind,omitempty"`
	Quote      *Quote `json:"quote,omitempty"`
	Recalled   bool   `json:"recalled,omitempty"`
	RecallSeq  int64  `json:"recall_seq,omitempty"`
	GroupID    int64  `json:"group_id,omitempty"`    // 群邀请（Kind 为 invite）：邀请加入的小群 ID，Content 为群名称
	InviteDone bool   `json:"invite_done,omitempty"` // 群邀请已被同意（只能同意一次）
}

// kindInvite 小群邀请：只由服务端在群主拉入用户时生成，客户端不能直接发送
const kindInvite = "invite"

// GroupMember 小群成员。Muted 只影响本小群内的发言，与大群禁言互不影响。
type GroupMember struct {
	UserID   int64 `json:"user_id"`
	Muted    bool  `json:"muted,omitempty"`
	JoinedAt int64 `json:"joined_at"`
}

// Group 小群（群主 + 成员）。每位用户最多只能创建一个小群。
type Group struct {
	ID        int64         `json:"id"`
	Name      string        `json:"name"`
	OwnerID   int64         `json:"owner_id"`
	CreatedAt int64         `json:"created_at"`
	Members   []GroupMember `json:"members"`
}

// GroupMessage 小群消息
type GroupMessage struct {
	ID        int64   `json:"id"`
	GroupID   int64   `json:"group_id"`
	UserID    int64   `json:"user_id"`
	Username  string  `json:"username"`
	Content   string  `json:"content"`
	CreatedAt int64   `json:"created_at"`
	Mentions  []int64 `json:"mentions,omitempty"` // 被 @ 的成员 ID（仅小群成员之间）
	Kind      string  `json:"kind,omitempty"`
	Quote     *Quote  `json:"quote,omitempty"`
	Recalled  bool    `json:"recalled,omitempty"`
	RecallSeq int64   `json:"recall_seq,omitempty"`
}

type Announcement struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	Username  string `json:"username"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"created_at"`
	Kind      string `json:"kind,omitempty"` // "" 或 "text"：纯文本；"html"：HTML（用 iframe 显示）
}

// HostEntry 解析域名列表中的一项：域名 → 目标 IP
type HostEntry struct {
	Domain string `json:"domain"`
	IP     string `json:"ip"`
}

type dbData struct {
	NextUserID     int64            `json:"next_user_id"`
	NextMsgID      int64            `json:"next_msg_id"`
	NextPrivID     int64            `json:"next_priv_id"`
	NextAnnID      int64            `json:"next_ann_id"`
	Users          []User           `json:"users"`
	Messages       []Message        `json:"messages"`
	PrivateMsgs    []PrivateMessage `json:"private_messages"`
	Announcements  []Announcement   `json:"announcements"`
	Hosts          []HostEntry      `json:"hosts"`
	RecallSeq      int64            `json:"recall_seq"` // 撤回事件的全局序号
	NextGroupID    int64            `json:"next_group_id"`
	NextGroupMsgID int64            `json:"next_group_msg_id"`
	Groups         []Group          `json:"groups"`
	GroupMsgs      []GroupMessage   `json:"group_messages"`
	ServerPort     int              `json:"server_port,omitempty"` // 管理页面设置的服务端口（0 表示未设置，使用默认 80）
	// 旧版字段，只在启动时迁移到 Hosts
	LegacyDomain   string   `json:"domain,omitempty"`
	LegacyHostList []string `json:"host_list,omitempty"`
}

// Store 把所有数据保存在 data/app.json 中（每次修改后整体原子写回）。
type Store struct {
	mu   sync.Mutex
	path string
	db   dbData
}

func openStore(path string) (*Store, error) {
	// 新数据文件不存在时直接返回，所以计数器的初始值必须在这里设好（否则第一个小群/小群消息 ID 为 0）
	s := &Store{path: path, db: dbData{NextUserID: 1, NextMsgID: 1, NextPrivID: 1, NextAnnID: 1,
		NextGroupID: 1, NextGroupMsgID: 1}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("无法读取数据文件：%w", err)
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &s.db); err != nil {
			return nil, fmt.Errorf("数据文件损坏（%s）：%w", path, err)
		}
	}
	if s.db.NextUserID <= 0 {
		s.db.NextUserID = 1
	}
	if s.db.NextMsgID <= 0 {
		s.db.NextMsgID = 1
	}
	if s.db.NextPrivID <= 0 {
		s.db.NextPrivID = 1
	}
	if s.db.NextAnnID <= 0 {
		s.db.NextAnnID = 1
	}
	if s.db.NextGroupID <= 0 {
		s.db.NextGroupID = 1
	}
	if s.db.NextGroupMsgID <= 0 {
		s.db.NextGroupMsgID = 1
	}
	// 迁移旧版：全局解析域名与纯域名列表 → 带 IP 的解析域名列表（默认 127.0.0.1）
	if s.db.LegacyDomain != "" || len(s.db.LegacyHostList) > 0 {
		names := append([]string(nil), s.db.LegacyHostList...)
		if s.db.LegacyDomain != "" {
			names = append(names, s.db.LegacyDomain)
		}
		for _, d := range names {
			if s.findHost(d) < 0 {
				s.db.Hosts = append(s.db.Hosts, HostEntry{Domain: d, IP: defaultHostIP})
			}
		}
		s.db.LegacyDomain = ""
		s.db.LegacyHostList = nil
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// saveLocked 调用方需持有 s.mu
func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(&s.db, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return fmt.Errorf("保存数据失败：%w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("保存数据失败：%w", err)
	}
	return nil
}

// findHost 调用方需持有 s.mu；返回下标，不存在返回 -1
func (s *Store) findHost(domain string) int {
	for i, h := range s.db.Hosts {
		if strings.EqualFold(h.Domain, domain) {
			return i
		}
	}
	return -1
}

// ServerPort 当前保存的服务端口（未设置时为默认 80）
func (s *Store) ServerPort() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return effectivePort(s.db.ServerPort)
}

// SavedServerPort 管理页面保存的原始设置（0 表示留空，即使用默认端口）
func (s *Store) SavedServerPort() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.ServerPort
}

// effectivePort 把保存值换算为实际端口：0（留空）使用默认端口 80
func effectivePort(saved int) int {
	if saved <= 0 {
		return defaultServerPort
	}
	return saved
}

// SetServerPort 保存服务端口设置（0 表示留空，使用默认端口 80）
func (s *Store) SetServerPort(port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.db.ServerPort
	s.db.ServerPort = port
	if err := s.saveLocked(); err != nil {
		s.db.ServerPort = old
		return err
	}
	return nil
}

// HostList 返回解析域名列表的副本（始终非 nil，保证 JSON 为 [] 而不是 null）
func (s *Store) HostList() []HostEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]HostEntry, len(s.db.Hosts))
	copy(out, s.db.Hosts)
	return out
}

// AddHost 添加域名（调用方已规范化）
func (s *Store) AddHost(e HostEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.findHost(e.Domain) >= 0 {
		return errHostExists
	}
	if len(s.db.Hosts) >= maxHostDomains {
		return errHostListFull
	}
	s.db.Hosts = append(s.db.Hosts, e)
	return s.saveLocked()
}

// SetHostIP 修改某个域名的目标 IP
func (s *Store) SetHostIP(domain, ip string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.findHost(domain)
	if i < 0 {
		return errHostNotFound
	}
	s.db.Hosts[i].IP = ip
	return s.saveLocked()
}

// RemoveHost 从列表中删除域名
func (s *Store) RemoveHost(domain string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.findHost(domain)
	if i < 0 {
		return errHostNotFound
	}
	s.db.Hosts = append(s.db.Hosts[:i], s.db.Hosts[i+1:]...)
	return s.saveLocked()
}

func (s *Store) FindUser(name string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.db.Users {
		if strings.EqualFold(u.Username, name) {
			return u, true
		}
	}
	return User{}, false
}

func (s *Store) UserByID(id int64) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.db.Users {
		if u.ID == id {
			return u, true
		}
	}
	return User{}, false
}

// HasAdmin 是否已存在主管理员账号
func (s *Store) HasAdmin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.db.Users {
		if u.IsAdmin {
			return true
		}
	}
	return false
}

// CreateAdmin 创建主管理员账号。若已有名为 admin 的普通用户（早期版本注册的），先将其改名，
// 以免普通用户拿到管理员权限。返回被改名的旧用户名（没有则为空）。
func (s *Store) CreateAdmin(pwHash string) (renamed string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Users {
		if strings.EqualFold(s.db.Users[i].Username, adminUsername) {
			old := s.db.Users[i].Username
			newName := fmt.Sprintf("%s_%d", adminUsername, s.db.Users[i].ID)
			s.db.Users[i].Username = newName
			renamed = old + " → " + newName
		}
	}
	u := User{ID: s.db.NextUserID, Username: adminUsername, PWHash: pwHash, CreatedAt: time.Now().Unix(), IsAdmin: true}
	s.db.NextUserID++
	s.db.Users = append(s.db.Users, u)
	return renamed, s.saveLocked()
}

// updateUser 在锁内修改一个用户
func (s *Store) updateUser(id int64, fn func(u *User)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Users {
		if s.db.Users[i].ID == id {
			fn(&s.db.Users[i])
			return s.saveLocked()
		}
	}
	return errNotFound
}

// DeleteUser 删除账号（历史消息保留，显示时使用消息中保存的用户名）。
// 同时：退出所有小群；若该用户是群主，群主转给最早加入的其他成员（并解除其禁言），
// 没有其他成员则解散该群；并从其他用户的黑名单中移除。
func (s *Store) DeleteUser(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, u := range s.db.Users {
		if u.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errNotFound
	}
	s.db.Users = append(s.db.Users[:idx], s.db.Users[idx+1:]...)
	for i := range s.db.Users {
		s.db.Users[i].Blocked = removeID(s.db.Users[i].Blocked, id)
	}
	dissolved := map[int64]bool{}
	var groups []Group
	for _, g := range s.db.Groups {
		var members []GroupMember
		for _, m := range g.Members {
			if m.UserID != id {
				members = append(members, m)
			}
		}
		if g.OwnerID == id {
			if len(members) == 0 {
				dissolved[g.ID] = true
				continue
			}
			members[0].Muted = false
			g.OwnerID = members[0].UserID
		}
		g.Members = members
		groups = append(groups, g)
	}
	s.db.Groups = groups
	if len(dissolved) > 0 {
		var msgs []GroupMessage
		for _, m := range s.db.GroupMsgs {
			if !dissolved[m.GroupID] {
				msgs = append(msgs, m)
			}
		}
		s.db.GroupMsgs = msgs
	}
	return s.saveLocked()
}

// SetMuted 设置全局禁言状态。处理过的用户不再计入待处理红点。
func (s *Store) SetMuted(id int64, muted bool) error {
	return s.updateUser(id, func(u *User) { u.Muted = muted; u.Pending = false })
}

// SetBigMuted 设置大群禁言状态（只影响大群发言）
func (s *Store) SetBigMuted(id int64, on bool) error {
	return s.updateUser(id, func(u *User) { u.BigMuted = on })
}

func (s *Store) SetSubAdmin(id int64, on bool) error {
	return s.updateUser(id, func(u *User) { u.SubAdmin = on })
}

func (s *Store) SetAlias(id int64, alias string) error {
	return s.updateUser(id, func(u *User) { u.Alias = alias })
}

func (s *Store) SetPassword(id int64, pwHash string) error {
	return s.updateUser(id, func(u *User) { u.PWHash = pwHash })
}

// PendingCount 待处理（新注册、尚未被管理员处理）的用户数
func (s *Store) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, u := range s.db.Users {
		if u.Pending {
			n++
		}
	}
	return n
}

// ListUsers 返回全部用户（按注册顺序）
func (s *Store) ListUsers() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]User(nil), s.db.Users...)
}

// AddUser 注册普通用户：默认禁言，并标记为待处理
func (s *Store) AddUser(name, pwHash string) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.db.Users {
		if strings.EqualFold(u.Username, name) {
			return User{}, errUserExists
		}
	}
	u := User{ID: s.db.NextUserID, Username: name, PWHash: pwHash, CreatedAt: time.Now().Unix(), Muted: true, Pending: true}
	s.db.NextUserID++
	s.db.Users = append(s.db.Users, u)
	return u, s.saveLocked()
}

func (s *Store) AddMessage(m Message) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m.ID = s.db.NextMsgID
	m.CreatedAt = time.Now().Unix()
	s.db.NextMsgID++
	s.db.Messages = append(s.db.Messages, m) // 不限制条数：所有消息都保存
	return m, s.saveLocked()
}

// FindMessage 按 ID 查找大群消息（用于引用）
func (s *Store) FindMessage(id int64) (Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.db.Messages {
		if m.ID == id {
			return m, true
		}
	}
	return Message{}, false
}

// ListMessages 大群消息分页（规则见 pageRange）；withRseq 为真时同时返回撤回同步列表
func (s *Store) ListMessages(after, before, rseq int64, withRseq bool) pageResult[Message] {
	s.mu.Lock()
	defer s.mu.Unlock()
	idf := func(m Message) int64 { return m.ID }
	items, more := pageRange(s.db.Messages, idf, after, before)
	res := pageResult[Message]{Items: items, HasMore: more, RSeq: s.db.RecallSeq, Recalled: []int64{}}
	if withRseq {
		res.Recalled = recalledSince(s.db.Messages, idf,
			func(m Message) int64 { return m.RecallSeq }, rseq)
	}
	return res
}

func (s *Store) AddPrivate(m PrivateMessage) (PrivateMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m.ID = s.db.NextPrivID
	m.CreatedAt = time.Now().Unix()
	s.db.NextPrivID++
	s.db.PrivateMsgs = append(s.db.PrivateMsgs, m) // 不限制条数：所有消息都保存
	return m, s.saveLocked()
}

// FindPrivate 按 ID 查找私聊消息（用于引用）
func (s *Store) FindPrivate(id int64) (PrivateMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.db.PrivateMsgs {
		if m.ID == id {
			return m, true
		}
	}
	return PrivateMessage{}, false
}

// PrivateLatest 返回与 uid 有私聊往来的每个用户的最后一条消息（用于侧边栏的私聊红点）
func (s *Store) PrivateLatest(uid int64) map[int64]PrivateMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int64]PrivateMessage{}
	for _, m := range s.db.PrivateMsgs {
		var peer int64
		switch uid {
		case m.FromID:
			peer = m.ToID
		case m.ToID:
			peer = m.FromID
		default:
			continue
		}
		if prev, ok := out[peer]; !ok || m.ID > prev.ID {
			out[peer] = m
		}
	}
	return out
}

// ListPrivate 两个用户之间的私聊分页（规则同 ListMessages）
func (s *Store) ListPrivate(a, b, after, before, rseq int64, withRseq bool) pageResult[PrivateMessage] {
	s.mu.Lock()
	defer s.mu.Unlock()
	var conv []PrivateMessage
	for _, m := range s.db.PrivateMsgs {
		if (m.FromID == a && m.ToID == b) || (m.FromID == b && m.ToID == a) {
			conv = append(conv, m)
		}
	}
	idf := func(m PrivateMessage) int64 { return m.ID }
	items, more := pageRange(conv, idf, after, before)
	res := pageResult[PrivateMessage]{Items: items, HasMore: more, RSeq: s.db.RecallSeq, Recalled: []int64{}}
	if withRseq {
		res.Recalled = recalledSince(conv, idf, func(m PrivateMessage) int64 { return m.RecallSeq }, rseq)
	}
	return res
}

func (s *Store) AddAnnouncement(an Announcement) (Announcement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	an.ID = s.db.NextAnnID
	an.CreatedAt = time.Now().Unix()
	s.db.NextAnnID++
	s.db.Announcements = append(s.db.Announcements, an)
	if n := len(s.db.Announcements); n > maxAnnouncements {
		s.db.Announcements = append([]Announcement(nil), s.db.Announcements[n-maxAnnouncements:]...)
	}
	return an, s.saveLocked()
}

func (s *Store) DeleteAnnouncement(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, an := range s.db.Announcements {
		if an.ID == id {
			s.db.Announcements = append(s.db.Announcements[:i], s.db.Announcements[i+1:]...)
			return s.saveLocked()
		}
	}
	return errNotFound
}

// ListAnnouncements 返回全部公告（发布时间升序）
func (s *Store) ListAnnouncements() []Announcement {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Announcement{}, s.db.Announcements...)
}

// ---------------------------------------------------------------- 分页 / 撤回 / 黑名单 / 小群

// pageRange 对按 ID 升序排列的记录分页（二分查找）：
// before>0 取 id<before 的最近 pageSize 条（加载更早消息）；after>0 取 id>after 的前 incrementalSize 条（增量）；
// 否则取最新 pageSize 条（首次加载）。返回副本；hasMore 表示之前还有更早的记录。
func pageRange[T any](all []T, id func(T) int64, after, before int64) ([]T, bool) {
	n := len(all)
	if before > 0 {
		hi := sort.Search(n, func(i int) bool { return id(all[i]) >= before })
		lo := hi - pageSize
		if lo < 0 {
			lo = 0
		}
		return append([]T{}, all[lo:hi]...), lo > 0
	}
	if after > 0 {
		lo := sort.Search(n, func(i int) bool { return id(all[i]) > after })
		hi := lo + incrementalSize
		if hi > n {
			hi = n
		}
		return append([]T{}, all[lo:hi]...), false
	}
	lo := n - pageSize
	if lo < 0 {
		lo = 0
	}
	return append([]T{}, all[lo:]...), lo > 0
}

// recalledSince 返回会话中撤回序号大于 cursor 的消息 ID（客户端据此更新已显示的消息）
func recalledSince[T any](all []T, id func(T) int64, seq func(T) int64, cursor int64) []int64 {
	out := []int64{}
	for _, m := range all {
		if seq(m) > cursor {
			out = append(out, id(m))
		}
	}
	return out
}

type pageResult[T any] struct {
	Items    []T
	HasMore  bool
	Recalled []int64
	RSeq     int64
}

func checkRecallable(uid, sender, created int64, recalled bool) error {
	if recalled {
		return errAlreadyRecalled
	}
	if sender != uid {
		return errNotSender
	}
	if time.Now().Unix()-created > recallWindowSec {
		return errRecallExpired
	}
	return nil
}

// Recall 撤回消息：只能撤回自己发送的、发出后 2 分钟内的消息。撤回后清除内容并记录撤回序号。
// scope：group（大群）、private（私聊）、small（小群）
func (s *Store) Recall(scope string, id, uid int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch scope {
	case "group":
		for i := range s.db.Messages {
			m := &s.db.Messages[i]
			if m.ID != id {
				continue
			}
			if err := checkRecallable(uid, m.UserID, m.CreatedAt, m.Recalled); err != nil {
				return err
			}
			s.db.RecallSeq++
			m.Recalled, m.RecallSeq = true, s.db.RecallSeq
			m.Content, m.Kind, m.Quote, m.Mentions = "", kindText, nil, nil
			for j := range s.db.Messages {
				if q := s.db.Messages[j].Quote; q != nil && q.ID == id {
					markQuoteRecalled(q)
				}
			}
			return s.saveLocked()
		}
	case "private":
		for i := range s.db.PrivateMsgs {
			m := &s.db.PrivateMsgs[i]
			if m.ID != id {
				continue
			}
			if err := checkRecallable(uid, m.FromID, m.CreatedAt, m.Recalled); err != nil {
				return err
			}
			s.db.RecallSeq++
			m.Recalled, m.RecallSeq = true, s.db.RecallSeq
			m.Content, m.Kind, m.Quote = "", kindText, nil
			for j := range s.db.PrivateMsgs {
				if q := s.db.PrivateMsgs[j].Quote; q != nil && q.ID == id {
					markQuoteRecalled(q)
				}
			}
			return s.saveLocked()
		}
	case "small":
		for i := range s.db.GroupMsgs {
			m := &s.db.GroupMsgs[i]
			if m.ID != id {
				continue
			}
			if err := checkRecallable(uid, m.UserID, m.CreatedAt, m.Recalled); err != nil {
				return err
			}
			s.db.RecallSeq++
			m.Recalled, m.RecallSeq = true, s.db.RecallSeq
			m.Content, m.Kind, m.Quote = "", kindText, nil
			for j := range s.db.GroupMsgs {
				if q := s.db.GroupMsgs[j].Quote; q != nil && q.ID == id {
					markQuoteRecalled(q)
				}
			}
			return s.saveLocked()
		}
	}
	return errMsgNotFound
}

// markQuoteRecalled 引用的原消息被撤回后，引用框只显示“原消息已被撤回”
func markQuoteRecalled(q *Quote) {
	q.Kind, q.Content = kindRecalled, quoteRecalledText
}

// GroupLimitValue 用户可创建的小群数量上限（未设置时默认 1）
func (u User) GroupLimitValue() int {
	if u.GroupLimit == nil {
		return 1
	}
	return *u.GroupLimit
}

// SetGroupLimit 设置用户可创建的小群数量上限（主/副管理员设置，0 表示不允许创建）
func (s *Store) SetGroupLimit(id int64, limit int) error {
	return s.updateUser(id, func(u *User) { v := limit; u.GroupLimit = &v })
}

// RenameGroup 群主重命名小群
func (s *Store) RenameGroup(gid, actor int64, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.groupOwnedLocked(gid, actor)
	if err != nil {
		return err
	}
	g.Name = name
	return s.saveLocked()
}

// DissolveGroup 群主解散小群：删除群及其全部消息（成员之后无法再访问）
func (s *Store) DissolveGroup(gid, actor int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.groupOwnedLocked(gid, actor); err != nil {
		return err
	}
	groups := s.db.Groups[:0]
	for _, g := range s.db.Groups {
		if g.ID != gid {
			groups = append(groups, g)
		}
	}
	s.db.Groups = groups
	msgs := s.db.GroupMsgs[:0]
	for _, m := range s.db.GroupMsgs {
		if m.GroupID != gid {
			msgs = append(msgs, m)
		}
	}
	s.db.GroupMsgs = msgs
	return s.saveLocked()
}

// SetBlocked 把 target 加入（或移出）owner 的黑名单
func (s *Store) SetBlocked(owner, target int64, on bool) error {
	return s.updateUser(owner, func(u *User) {
		if !on {
			u.Blocked = removeID(u.Blocked, target)
			return
		}
		for _, id := range u.Blocked {
			if id == target {
				return
			}
		}
		u.Blocked = append(u.Blocked, target)
	})
}

func removeID(ids []int64, id int64) []int64 {
	var out []int64
	for _, v := range ids {
		if v != id {
			out = append(out, v)
		}
	}
	return out
}

func containsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func groupIndex(groups []Group, id int64) int {
	for i := range groups {
		if groups[i].ID == id {
			return i
		}
	}
	return -1
}

func memberIndex(g *Group, uid int64) int {
	for i := range g.Members {
		if g.Members[i].UserID == uid {
			return i
		}
	}
	return -1
}

func copyGroup(g Group) Group {
	g.Members = append([]GroupMember(nil), g.Members...)
	return g
}

// CreateGroup 创建小群（群主自动成为成员）。limit < 0 表示不限制；否则群主已创建的小群数不能达到 limit。
func (s *Store) CreateGroup(name string, owner int64, limit int) (Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit >= 0 {
		n := 0
		for _, g := range s.db.Groups {
			if g.OwnerID == owner {
				n++
			}
		}
		if n >= limit {
			return Group{}, errGroupLimit
		}
	}
	now := time.Now().Unix()
	g := Group{ID: s.db.NextGroupID, Name: name, OwnerID: owner, CreatedAt: now,
		Members: []GroupMember{{UserID: owner, JoinedAt: now}}}
	s.db.NextGroupID++
	s.db.Groups = append(s.db.Groups, g)
	if err := s.saveLocked(); err != nil {
		return Group{}, err
	}
	return copyGroup(g), nil
}

// GroupFor 返回 uid 参与的小群（不是成员则返回 false）
func (s *Store) GroupFor(gid, uid int64) (Group, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := groupIndex(s.db.Groups, gid)
	if i < 0 || memberIndex(&s.db.Groups[i], uid) < 0 {
		return Group{}, false
	}
	return copyGroup(s.db.Groups[i]), true
}

// GroupsOf 返回 uid 参与的全部小群（按创建顺序）
func (s *Store) GroupsOf(uid int64) []Group {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Group
	for _, g := range s.db.Groups {
		if memberIndex(&g, uid) >= 0 {
			out = append(out, copyGroup(g))
		}
	}
	return out
}

// groupOwnedLocked 调用方需持有锁：要求 actor 是该群群主
func (s *Store) groupOwnedLocked(gid, actor int64) (*Group, error) {
	i := groupIndex(s.db.Groups, gid)
	if i < 0 {
		return nil, errGroupNotFound
	}
	g := &s.db.Groups[i]
	if memberIndex(g, actor) < 0 {
		return nil, errGroupNotFound
	}
	if g.OwnerID != actor {
		return nil, errGroupOwnerOnly
	}
	return g, nil
}

// GroupAddMember 群主拉入成员
func (s *Store) GroupAddMember(gid, actor, target int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.groupOwnedLocked(gid, actor)
	if err != nil {
		return err
	}
	if memberIndex(g, target) >= 0 {
		return errAlreadyMember
	}
	g.Members = append(g.Members, GroupMember{UserID: target, JoinedAt: time.Now().Unix()})
	return s.saveLocked()
}

// GroupKick 群主踢出成员（不能踢出群主自己）
func (s *Store) GroupKick(gid, actor, target int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.groupOwnedLocked(gid, actor)
	if err != nil {
		return err
	}
	if target == g.OwnerID {
		return errOwnerTarget
	}
	i := memberIndex(g, target)
	if i < 0 {
		return errNotMember
	}
	g.Members = append(g.Members[:i], g.Members[i+1:]...)
	return s.saveLocked()
}

// GroupSetMute 群主禁言 / 解除禁言成员（只影响本群）
func (s *Store) GroupSetMute(gid, actor, target int64, muted bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.groupOwnedLocked(gid, actor)
	if err != nil {
		return err
	}
	if target == g.OwnerID {
		return errOwnerTarget
	}
	i := memberIndex(g, target)
	if i < 0 {
		return errNotMember
	}
	g.Members[i].Muted = muted
	return s.saveLocked()
}

// GroupLeave 成员退出小群（群主不能退出）
func (s *Store) GroupLeave(gid, uid int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := groupIndex(s.db.Groups, gid)
	if i < 0 {
		return errGroupNotFound
	}
	g := &s.db.Groups[i]
	j := memberIndex(g, uid)
	if j < 0 {
		return errGroupNotFound
	}
	if g.OwnerID == uid {
		return errOwnerLeave
	}
	g.Members = append(g.Members[:j], g.Members[j+1:]...)
	return s.saveLocked()
}

// GroupLastMessage 小群最后一条消息（用于侧边栏未读红点）
func (s *Store) GroupLastMessage(gid int64) (GroupMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.db.GroupMsgs) - 1; i >= 0; i-- {
		if s.db.GroupMsgs[i].GroupID == gid {
			return s.db.GroupMsgs[i], true
		}
	}
	return GroupMessage{}, false
}

// FindGroupMessage 按 ID 查找小群消息（用于引用）
func (s *Store) FindGroupMessage(id int64) (GroupMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.db.GroupMsgs {
		if m.ID == id {
			return m, true
		}
	}
	return GroupMessage{}, false
}

// AddGroupMessage 发送小群消息：要求是本群成员且未被本群禁言
func (s *Store) AddGroupMessage(m GroupMessage) (GroupMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := groupIndex(s.db.Groups, m.GroupID)
	if i < 0 {
		return GroupMessage{}, errGroupNotFound
	}
	g := &s.db.Groups[i]
	j := memberIndex(g, m.UserID)
	if j < 0 {
		return GroupMessage{}, errGroupNotFound
	}
	if g.Members[j].Muted {
		return GroupMessage{}, errGroupMuted
	}
	m.ID = s.db.NextGroupMsgID
	m.CreatedAt = time.Now().Unix()
	s.db.NextGroupMsgID++
	s.db.GroupMsgs = append(s.db.GroupMsgs, m)
	if err := s.saveLocked(); err != nil {
		return GroupMessage{}, err
	}
	return m, nil
}

// ListGroupMessages 小群消息分页（规则同 ListMessages）
func (s *Store) ListGroupMessages(gid, after, before, rseq int64, withRseq bool) pageResult[GroupMessage] {
	s.mu.Lock()
	defer s.mu.Unlock()
	var conv []GroupMessage
	for _, m := range s.db.GroupMsgs {
		if m.GroupID == gid {
			conv = append(conv, m)
		}
	}
	idf := func(m GroupMessage) int64 { return m.ID }
	items, more := pageRange(conv, idf, after, before)
	res := pageResult[GroupMessage]{Items: items, HasMore: more, RSeq: s.db.RecallSeq, Recalled: []int64{}}
	if withRseq {
		res.Recalled = recalledSince(conv, idf, func(m GroupMessage) int64 { return m.RecallSeq }, rseq)
	}
	return res
}

// ---------------------------------------------------------------- 密码（PBKDF2-HMAC-SHA256）

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	hLen := sha256.Size
	numBlocks := (keyLen + hLen - 1) / hLen
	dk := make([]byte, 0, numBlocks*hLen)
	for block := 1; block <= numBlocks; block++ {
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		var idx [4]byte
		binary.BigEndian.PutUint32(idx[:], uint32(block))
		mac.Write(idx[:])
		u := mac.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			mac.Reset()
			mac.Write(u)
			u = mac.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk := pbkdf2SHA256([]byte(pw), salt, pbkdf2Iterations, 32)
	return fmt.Sprintf("pbkdf2_sha256$%d$%s$%s", pbkdf2Iterations, hex.EncodeToString(salt), hex.EncodeToString(dk)), nil
}

func verifyPassword(pw, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2_sha256" {
		return false
	}
	var iter int
	if _, err := fmt.Sscanf(parts[1], "%d", &iter); err != nil || iter <= 0 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(pw), salt, iter, len(want))
	return hmac.Equal(got, want)
}

// RenameUser 修改用户名（不区分大小写，不能与其他用户重复）
func (s *Store) RenameUser(id int64, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, u := range s.db.Users {
		if u.ID == id {
			idx = i
		} else if strings.EqualFold(u.Username, name) {
			return errUserExists
		}
	}
	if idx < 0 {
		return errNotFound
	}
	s.db.Users[idx].Username = name
	return s.saveLocked()
}

// GroupsOwnedBy 返回 uid 作为群主的全部小群（主管理员在用户管理中查看并可解散）
func (s *Store) GroupsOwnedBy(uid int64) []Group {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Group
	for _, g := range s.db.Groups {
		if g.OwnerID == uid {
			out = append(out, copyGroup(g))
		}
	}
	return out
}

// AcceptInvite 被邀请人同意群邀请：加入小群，并把该邀请标记为已处理。返回小群 ID。
func (s *Store) AcceptInvite(msgID, uid int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mi := -1
	for i := range s.db.PrivateMsgs {
		if s.db.PrivateMsgs[i].ID == msgID {
			mi = i
			break
		}
	}
	if mi < 0 {
		return 0, errNotFound
	}
	m := &s.db.PrivateMsgs[mi]
	if m.Kind != kindInvite || m.ToID != uid {
		return 0, errNotFound
	}
	if m.InviteDone {
		return 0, errInviteUsed
	}
	gi := groupIndex(s.db.Groups, m.GroupID)
	if gi < 0 {
		return 0, errGroupNotFound
	}
	g := &s.db.Groups[gi]
	if memberIndex(g, uid) >= 0 {
		m.InviteDone = true
		if err := s.saveLocked(); err != nil {
			return 0, err
		}
		return g.ID, errAlreadyMember
	}
	g.Members = append(g.Members, GroupMember{UserID: uid, JoinedAt: time.Now().Unix()})
	m.InviteDone = true
	if err := s.saveLocked(); err != nil {
		return 0, err
	}
	return g.ID, nil
}
