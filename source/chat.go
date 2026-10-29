package main

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 聊天记录搜索、侧边栏的最新消息预览与未读数（只统计，不改变消息数据）

const (
	previewRunes  = 20  // 侧边栏预览截取的字数，超出部分用 ... 代替
	searchLimit   = 100 // 搜索每页返回的条数（按时间从新到旧，可“加载更多”）
	searchMaxRune = 50  // 搜索关键词最长字数
	snippetRunes  = 60  // 搜索结果中显示的上下文字数
)

// previewOf 生成侧边栏预览的文字（不含发送人前缀）
func previewOf(content, kind string, recalled bool) string {
	switch {
	case recalled:
		return "消息已撤回"
	case kind == "html":
		return "[HTML消息]"
	case kind == kindInvite:
		return "[群邀请]"
	}
	s := strings.Join(strings.Fields(content), " ")
	r := []rune(s)
	if len(r) > previewRunes {
		return string(r[:previewRunes]) + "..."
	}
	return s
}

// searchable 只有普通文本消息可以被搜索（HTML、群邀请、已撤回的不参与）
func searchable(content, kind string, recalled bool) bool {
	if recalled || strings.TrimSpace(content) == "" {
		return false
	}
	return kind == "" || kind == kindText
}

// snippetAround 取包含关键词的一段文字作为搜索结果摘要
func snippetAround(content, q string) string {
	rs := []rune(content)
	idx := 0
	lc, lq := strings.ToLower(content), strings.ToLower(q)
	if len(lc) == len(content) {
		if b := strings.Index(lc, lq); b >= 0 {
			idx = utf8.RuneCountInString(content[:b])
		}
	}
	start := idx - snippetRunes/4
	if start < 0 {
		start = 0
	}
	end := start + snippetRunes
	if end > len(rs) {
		end = len(rs)
	}
	s := strings.Join(strings.Fields(string(rs[start:end])), " ")
	if start > 0 {
		s = "..." + s
	}
	if end < len(rs) {
		s += "..."
	}
	return s
}

// searchHit 一条搜索结果。scope：group 大群、small 小群、private 私聊
type searchHit struct {
	Scope     string `json:"scope"`
	GroupID   int64  `json:"group_id,omitempty"`
	GroupName string `json:"group_name,omitempty"`
	PeerID    int64  `json:"peer_id,omitempty"`
	MsgID     int64  `json:"msg_id"`
	FromID    int64  `json:"from_id"`
	Sender    string `json:"sender"`
	Snippet   string `json:"snippet"`
	CreatedAt int64  `json:"created_at"`
}

// searchScope 搜索范围：all 为全部聊天；否则只搜索一个会话（kind: g 大群、s 小群、p 私聊）
type searchScope struct {
	all  bool
	kind string
	id   int64
}

func (sc searchScope) wantsBig() bool { return sc.all || sc.kind == "g" }

func (sc searchScope) wantsSmall(gid int64) bool { return sc.all || (sc.kind == "s" && sc.id == gid) }

func (sc searchScope) wantsPeer(peer int64) bool { return sc.all || (sc.kind == "p" && sc.id == peer) }

// parseSearchScope 解析 chat 参数：留空为全部；g / s<群ID> / p<用户ID> 为单个会话（与前端的会话键一致）
func parseSearchScope(v string) (searchScope, error) {
	switch {
	case v == "":
		return searchScope{all: true}, nil
	case v == "g":
		return searchScope{kind: "g"}, nil
	case len(v) > 1 && (v[0] == 's' || v[0] == 'p'):
		if id, err := strconv.ParseInt(v[1:], 10, 64); err == nil {
			return searchScope{kind: v[:1], id: id}, nil
		}
	}
	return searchScope{}, bad(http.StatusBadRequest, "搜索范围参数错误")
}

// SearchChats 在指定范围内搜索文本：全部可见聊天（大群、所在的小群、与自己有关的私聊），或某一个会话
func (s *Store) SearchChats(uid int64, q string, sc searchScope) []searchHit {
	s.mu.Lock()
	defer s.mu.Unlock()
	lq := strings.ToLower(q)
	match := func(content, kind string, recalled bool) bool {
		return searchable(content, kind, recalled) && strings.Contains(strings.ToLower(content), lq)
	}
	var hits []searchHit
	for _, m := range s.db.Messages {
		if sc.wantsBig() && match(m.Content, m.Kind, m.Recalled) {
			hits = append(hits, searchHit{Scope: "group", MsgID: m.ID, FromID: m.UserID,
				Sender: m.Username, Snippet: snippetAround(m.Content, q), CreatedAt: m.CreatedAt})
		}
	}
	mine := map[int64]Group{}
	for _, g := range s.db.Groups {
		if memberIndex(&g, uid) >= 0 {
			mine[g.ID] = g
		}
	}
	for _, m := range s.db.GroupMsgs {
		g, ok := mine[m.GroupID]
		if !ok || !sc.wantsSmall(m.GroupID) || !match(m.Content, m.Kind, m.Recalled) {
			continue
		}
		hits = append(hits, searchHit{Scope: "small", GroupID: g.ID, GroupName: g.Name, MsgID: m.ID,
			FromID: m.UserID, Sender: m.Username, Snippet: snippetAround(m.Content, q), CreatedAt: m.CreatedAt})
	}
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
		if !sc.wantsPeer(peer) || !match(m.Content, m.Kind, m.Recalled) {
			continue
		}
		hits = append(hits, searchHit{Scope: "private", PeerID: peer, MsgID: m.ID, FromID: m.FromID,
			Snippet: snippetAround(m.Content, q), CreatedAt: m.CreatedAt})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].CreatedAt != hits[j].CreatedAt {
			return hits[i].CreatedAt > hits[j].CreatedAt
		}
		return hits[i].MsgID > hits[j].MsgID
	})
	return hits
}

// chatPreview 一个会话的最新消息与未读数
type chatPreview struct {
	LastID   int64  `json:"last_id"`
	LastFrom int64  `json:"last_from"`
	LastName string `json:"last_name,omitempty"` // 小群 / 大群的发送人名称
	Preview  string `json:"preview"`
	Unread   int    `json:"unread"`
	Mention  bool   `json:"mention,omitempty"` // 未读消息中有人 @ 了我（红点显示为 @）
}

// mentionsUser 消息是否 @ 了该用户（与客户端气泡高亮的规则一致：@ 了该用户，或非 HTML 消息中包含 @所有人）
func mentionsUser(mentions []int64, content, kind string, uid int64) bool {
	for _, id := range mentions {
		if id == uid {
			return true
		}
	}
	return kind != kindHTML && strings.Contains(content, "@所有人")
}

// ChatSummary 计算侧边栏需要的数据。
// privSeen 为 nil 时不统计私聊未读（首次进入，只取最新消息）；否则每个对方按 privSeen[对方] 之后的消息计数（缺省为 0）。
// grpSeen 中出现的小群才统计未读；未出现的小群（尚未初始化）未读数为 0。
// 大群只返回最新消息，不统计未读。
func (s *Store) ChatSummary(uid int64, privSeen map[int64]int64, countPriv bool, grpSeen map[int64]int64, bigSeen int64, hasBigSeen bool) (chatPreview, map[int64]chatPreview, map[int64]chatPreview) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var big chatPreview
	if n := len(s.db.Messages); n > 0 {
		m := s.db.Messages[n-1]
		big = chatPreview{LastID: m.ID, LastFrom: m.UserID, LastName: m.Username,
			Preview: previewOf(m.Content, m.Kind, m.Recalled)}
		// 大群：已读位置之后有人 @ 我（或 @所有人）
		if hasBigSeen {
			for i := n - 1; i >= 0 && s.db.Messages[i].ID > bigSeen; i-- {
				x := s.db.Messages[i]
				if x.UserID != uid && !x.Recalled && mentionsUser(x.Mentions, x.Content, x.Kind, uid) {
					big.Mention = true
					break
				}
			}
		}
	}

	priv := map[int64]chatPreview{}
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
		p := priv[peer]
		if m.ID > p.LastID {
			p.LastID = m.ID
			p.LastFrom = m.FromID
			p.Preview = previewOf(m.Content, m.Kind, m.Recalled)
		}
		if countPriv && m.FromID != uid && !m.Recalled && m.ID > privSeen[peer] {
			p.Unread++
		}
		priv[peer] = p
	}

	mine := map[int64]bool{}
	for _, g := range s.db.Groups {
		if memberIndex(&g, uid) >= 0 {
			mine[g.ID] = true
		}
	}
	groups := map[int64]chatPreview{}
	for _, m := range s.db.GroupMsgs {
		if !mine[m.GroupID] {
			continue
		}
		p := groups[m.GroupID]
		if m.ID > p.LastID {
			p.LastID = m.ID
			p.LastFrom = m.UserID
			p.LastName = m.Username
			p.Preview = previewOf(m.Content, m.Kind, m.Recalled)
		}
		seen, ok := grpSeen[m.GroupID]
		if ok && m.UserID != uid && !m.Recalled && m.ID > seen {
			p.Unread++
			if mentionsUser(m.Mentions, m.Content, m.Kind, uid) {
				p.Mention = true
			}
		}
		groups[m.GroupID] = p
	}
	return big, priv, groups
}

// searchChats GET /api/search?q=关键词
func (a *server) searchChats(r *http.Request, sess session) (any, error) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		return nil, bad(http.StatusBadRequest, "请输入搜索内容")
	}
	if utf8.RuneCountInString(q) > searchMaxRune {
		return nil, bad(http.StatusBadRequest, "搜索内容最多 50 个字")
	}
	sc, err := parseSearchScope(r.URL.Query().Get("chat"))
	if err != nil {
		return nil, err
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, bad(http.StatusBadRequest, "分页参数错误")
		}
		offset = n
	}
	all := a.store.SearchChats(sess.UserID, q, sc) // 按时间从新到旧
	hits := []searchHit{}
	if offset < len(all) {
		end := offset + searchLimit
		if end > len(all) {
			end = len(all)
		}
		hits = append(hits, all[offset:end]...)
	}
	return map[string]any{"hits": hits, "has_more": offset+len(hits) < len(all)}, nil
}

// chatSummary POST /api/chat/summary  {"priv": {"对方ID": 已读ID} | null, "groups": {"小群ID": 已读ID}}
func (a *server) chatSummary(body map[string]any, sess session) (any, error) {
	var privSeen map[int64]int64
	countPriv := false
	if raw, ok := body["priv"].(map[string]any); ok {
		countPriv = true
		privSeen = seenMap(raw)
	}
	grpSeen := map[int64]int64{}
	if raw, ok := body["groups"].(map[string]any); ok {
		grpSeen = seenMap(raw)
	}
	bigSeen, hasBigSeen := body["big"].(float64) // 大群已读位置（客户端在首次进入后发送）
	big, priv, groups := a.store.ChatSummary(sess.UserID, privSeen, countPriv, grpSeen, int64(bigSeen), hasBigSeen)
	outPriv := map[string]chatPreview{}
	for k, v := range priv {
		outPriv[strconv.FormatInt(k, 10)] = v
	}
	outGroups := map[string]chatPreview{}
	for k, v := range groups {
		outGroups[strconv.FormatInt(k, 10)] = v
	}
	return map[string]any{"big": big, "private": outPriv, "groups": outGroups}, nil
}

// seenMap 把 JSON 对象 {"id": 数字} 转为 map[int64]int64
func seenMap(raw map[string]any) map[int64]int64 {
	out := map[int64]int64{}
	for k, v := range raw {
		id, err := strconv.ParseInt(k, 10, 64)
		if err != nil {
			continue
		}
		if f, ok := v.(float64); ok {
			out[id] = int64(f)
		}
	}
	return out
}
