package main

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 小群、撤回、黑名单的接口处理（数据逻辑在 store.go）

// storeStatus 把存储层的业务错误映射为 HTTP 状态码
var storeStatus = map[error]int{
	errGroupNotFound:   http.StatusNotFound,
	errMsgNotFound:     http.StatusNotFound,
	errNotFound:        http.StatusNotFound,
	errGroupOwnerOnly:  http.StatusForbidden,
	errNotSender:       http.StatusForbidden,
	errGroupMuted:      http.StatusForbidden,
	errGroupLimit:      http.StatusBadRequest,
	errAlreadyMember:   http.StatusBadRequest,
	errNotMember:       http.StatusBadRequest,
	errOwnerTarget:     http.StatusBadRequest,
	errOwnerLeave:      http.StatusBadRequest,
	errAlreadyRecalled: http.StatusBadRequest,
	errRecallExpired:   http.StatusBadRequest,
}

func mapStoreErr(err error) error {
	for e, code := range storeStatus {
		if errors.Is(err, e) {
			return bad(code, e.Error())
		}
	}
	return err
}

// queryInt 读取必填的正整数查询参数
func queryInt(r *http.Request, name string) (int64, error) {
	v, err := strconv.ParseInt(r.URL.Query().Get(name), 10, 64)
	if err != nil || v <= 0 {
		return 0, bad(http.StatusBadRequest, "参数错误")
	}
	return v, nil
}

// memberMuted 用户在该小群中是否被禁言
func memberMuted(g Group, uid int64) bool {
	for _, m := range g.Members {
		if m.UserID == uid {
			return m.Muted
		}
	}
	return false
}

// toMessage 小群消息转为通用消息结构（复用 messageJSON）
func toMessage(m GroupMessage) Message {
	return Message{ID: m.ID, UserID: m.UserID, Username: m.Username, Content: m.Content,
		CreatedAt: m.CreatedAt, Mentions: m.Mentions, Kind: m.Kind, Quote: m.Quote, Recalled: m.Recalled}
}

// isMember 用户是否为该小群成员
func isMember(g Group, uid int64) bool {
	for _, m := range g.Members {
		if m.UserID == uid {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 小群

func (a *server) listGroups(sess session) (any, error) {
	groups := a.store.GroupsOf(sess.UserID)
	out := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		item := map[string]any{"id": g.ID, "name": g.Name, "owner_id": g.OwnerID,
			"member_count": len(g.Members), "muted": memberMuted(g, sess.UserID),
			"created_at": g.CreatedAt, "last_id": int64(0), "last_from": int64(0), "last_at": int64(0)}
		if last, ok := a.store.GroupLastMessage(g.ID); ok {
			item["last_id"] = last.ID
			item["last_from"] = last.UserID
			item["last_at"] = last.CreatedAt
		}
		out = append(out, item)
	}
	return map[string]any{"groups": out}, nil
}

func (a *server) createGroup(sess session, body map[string]any) (any, error) {
	u, ok := a.store.UserByID(sess.UserID)
	if !ok {
		return nil, bad(http.StatusUnauthorized, "请先登录")
	}
	if u.Pending {
		return nil, bad(http.StatusForbidden, "待处理用户暂不能创建小群，请等待管理员处理")
	}
	name := strings.TrimSpace(strField(body, "name"))
	if n := utf8.RuneCountInString(name); n < 1 || n > maxGroupNameLen {
		return nil, bad(http.StatusBadRequest, "群名称需为 1-20 个字")
	}
	limit := -1 // 主/副管理员不限制数量
	if !u.IsStaff() {
		limit = u.GroupLimitValue()
	}
	g, err := a.store.CreateGroup(name, u.ID, limit)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	return map[string]any{"group": a.groupDetailJSON(g, u.ID)}, nil
}

// groupDetailJSON 小群详情：成员（含用户名、别名、禁言状态）以及当前用户的身份
func (a *server) groupDetailJSON(g Group, uid int64) map[string]any {
	users := map[int64]User{}
	for _, u := range a.store.ListUsers() {
		users[u.ID] = u
	}
	members := make([]map[string]any, 0, len(g.Members))
	for _, m := range g.Members {
		u := users[m.UserID]
		members = append(members, map[string]any{"id": m.UserID, "username": u.Username, "alias": u.Alias,
			"muted": m.Muted, "is_owner": m.UserID == g.OwnerID})
	}
	return map[string]any{"id": g.ID, "name": g.Name, "owner_id": g.OwnerID, "is_owner": g.OwnerID == uid,
		"my_muted": memberMuted(g, uid), "members": members}
}

func (a *server) groupDetail(sess session, r *http.Request) (any, error) {
	gid, err := queryInt(r, "id")
	if err != nil {
		return nil, err
	}
	g, ok := a.store.GroupFor(gid, sess.UserID)
	if !ok {
		return nil, bad(http.StatusNotFound, errGroupNotFound.Error())
	}
	return map[string]any{"group": a.groupDetailJSON(g, sess.UserID)}, nil
}

// groupMemberAction 处理需要 group_id + user_id 的群主操作
func (a *server) groupMemberAction(sess session, body map[string]any, act func(gid, target int64) error) (any, error) {
	gid, ok := int64Field(body, "group_id")
	if !ok {
		return nil, bad(http.StatusBadRequest, "群参数错误")
	}
	target, ok := int64Field(body, "user_id")
	if !ok {
		return nil, bad(http.StatusBadRequest, "用户参数错误")
	}
	if err := act(gid, target); err != nil {
		return nil, mapStoreErr(err)
	}
	return map[string]any{"ok": true}, nil
}

// groupInvite 群主拉入用户：不直接入群，而是发送一条群邀请（私聊中的蓝色“邀请你进入xxx”），对方点击同意后才会加入
func (a *server) groupInvite(sess session, body map[string]any) (any, error) {
	gid, ok := int64Field(body, "group_id")
	if !ok {
		return nil, bad(http.StatusBadRequest, "群参数错误")
	}
	target, ok := int64Field(body, "user_id")
	if !ok {
		return nil, bad(http.StatusBadRequest, "用户参数错误")
	}
	inviter, ok := a.store.UserByID(sess.UserID)
	if !ok {
		return nil, bad(http.StatusUnauthorized, "请先登录")
	}
	if inviter.Muted {
		return nil, bad(http.StatusForbidden, "你已被管理员禁言，暂时不能发送邀请")
	}
	g, ok := a.store.GroupFor(gid, sess.UserID)
	if !ok {
		return nil, bad(http.StatusNotFound, errGroupNotFound.Error())
	}
	if g.OwnerID != sess.UserID {
		return nil, bad(http.StatusForbidden, "只有群主可以拉入用户")
	}
	if target == sess.UserID {
		return nil, bad(http.StatusBadRequest, "不能邀请自己")
	}
	tu, ok := a.store.UserByID(target)
	if !ok {
		return nil, bad(http.StatusNotFound, "用户不存在")
	}
	if tu.Pending {
		return nil, bad(http.StatusBadRequest, "待处理用户暂不能被拉入小群")
	}
	if isMember(g, target) {
		return nil, bad(http.StatusBadRequest, "该用户已在群中")
	}
	if containsID(tu.Blocked, sess.UserID) {
		return nil, bad(http.StatusForbidden, "对方已将你加入黑名单，无法发送邀请")
	}
	m, err := a.store.AddPrivate(PrivateMessage{FromID: sess.UserID, ToID: target, Kind: kindInvite,
		Content: g.Name, GroupID: gid})
	if err != nil {
		return nil, err
	}
	return map[string]any{"message": privateJSON(m)}, nil
}

// acceptInvite 被邀请人点击邀请并同意：加入小群
func (a *server) acceptInvite(sess session, body map[string]any) (any, error) {
	id, ok := int64Field(body, "id")
	if !ok {
		return nil, bad(http.StatusBadRequest, "参数错误")
	}
	gid, err := a.store.AcceptInvite(id, sess.UserID)
	switch {
	case errors.Is(err, errAlreadyMember):
		return nil, bad(http.StatusBadRequest, "你已在该群中")
	case errors.Is(err, errGroupNotFound):
		return nil, bad(http.StatusNotFound, "该小群已解散")
	case errors.Is(err, errInviteUsed):
		return nil, bad(http.StatusBadRequest, "该邀请已处理")
	case errors.Is(err, errNotFound):
		return nil, bad(http.StatusNotFound, "邀请不存在")
	case err != nil:
		return nil, err
	}
	return map[string]any{"ok": true, "group_id": gid}, nil
}

func (a *server) groupKick(sess session, body map[string]any) (any, error) {
	return a.groupMemberAction(sess, body, func(gid, target int64) error {
		return a.store.GroupKick(gid, sess.UserID, target)
	})
}

func (a *server) groupMute(sess session, body map[string]any) (any, error) {
	muted, _ := body["muted"].(bool)
	return a.groupMemberAction(sess, body, func(gid, target int64) error {
		return a.store.GroupSetMute(gid, sess.UserID, target, muted)
	})
}

func (a *server) groupLeave(sess session, body map[string]any) (any, error) {
	gid, ok := int64Field(body, "group_id")
	if !ok {
		return nil, bad(http.StatusBadRequest, "群参数错误")
	}
	if err := a.store.GroupLeave(gid, sess.UserID); err != nil {
		return nil, mapStoreErr(err)
	}
	return map[string]any{"ok": true}, nil
}

// ---------------------------------------------------------------- 小群消息

func (a *server) listGroupMessages(r *http.Request, sess session) (any, error) {
	gid, err := queryInt(r, "group")
	if err != nil {
		return nil, err
	}
	g, ok := a.store.GroupFor(gid, sess.UserID)
	if !ok {
		return nil, bad(http.StatusNotFound, errGroupNotFound.Error())
	}
	after, before, rseq, withRseq, err := parseListOpts(r)
	if err != nil {
		return nil, err
	}
	res := a.store.ListGroupMessages(gid, after, before, rseq, withRseq && before == 0)
	names := a.nameMap()
	unames := a.userNames()
	out := make([]map[string]any, 0, len(res.Items))
	for _, m := range res.Items {
		j := messageJSON(toMessage(m), names[m.UserID])
		if n, ok := unames[m.UserID]; ok {
			j["username"] = n
		}
		out = append(out, j)
	}
	return map[string]any{"messages": out, "muted": memberMuted(g, sess.UserID), "has_more": res.HasMore,
		"recalled_ids": res.Recalled, "rseq": res.RSeq}, nil
}

func (a *server) sendGroupMessage(sess session, body map[string]any) (any, error) {
	gid, ok := int64Field(body, "group_id")
	if !ok {
		return nil, bad(http.StatusBadRequest, "群参数错误")
	}
	u, ok := a.store.UserByID(sess.UserID)
	if !ok {
		return nil, bad(http.StatusUnauthorized, "请先登录")
	}
	if u.Muted {
		return nil, bad(http.StatusForbidden, "你已被管理员禁言，暂时不能发言（私聊和所有群聊）")
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
	g, ok := a.store.GroupFor(gid, u.ID)
	if !ok {
		return nil, bad(http.StatusNotFound, errGroupNotFound.Error())
	}
	var mentions []int64
	if kind == kindText {
		var members []User
		for _, m := range g.Members {
			if mu, ok := a.store.UserByID(m.UserID); ok {
				members = append(members, mu)
			}
		}
		mentions = resolveMentionsAmong(content, u.ID, members)
	}
	var quote *Quote
	if qid, ok := int64Field(body, "quote_id"); ok && qid > 0 {
		qm, found := a.store.FindGroupMessage(qid)
		if !found || qm.GroupID != gid {
			return nil, bad(http.StatusNotFound, "被引用的消息不存在")
		}
		quote = quoteFor(qm.ID, qm.Username, qm.Content, qm.Kind, qm.Recalled)
	}
	m, err := a.store.AddGroupMessage(GroupMessage{GroupID: gid, UserID: u.ID, Username: u.Username,
		Content: content, Kind: kind, Quote: quote, Mentions: mentions})
	if err != nil {
		return nil, mapStoreErr(err)
	}
	return map[string]any{"message": messageJSON(toMessage(m), u.Alias)}, nil
}

// ---------------------------------------------------------------- 撤回 / 黑名单

// recallMessage 撤回消息（大群 group / 私聊 private / 小群 small）
func (a *server) recallMessage(sess session, body map[string]any) (any, error) {
	id, ok := int64Field(body, "id")
	if !ok {
		return nil, bad(http.StatusBadRequest, "消息参数错误")
	}
	if err := a.store.Recall(strField(body, "scope"), id, sess.UserID); err != nil {
		return nil, mapStoreErr(err)
	}
	return map[string]any{"ok": true}, nil
}

// setBlacklist 私聊中将用户加入 / 移出黑名单（对方无法再向本用户发送私聊消息）
func (a *server) setBlacklist(sess session, body map[string]any) (any, error) {
	target, ok := int64Field(body, "user_id")
	if !ok {
		return nil, bad(http.StatusBadRequest, "用户参数错误")
	}
	if target == sess.UserID {
		return nil, bad(http.StatusBadRequest, "不能将自己加入黑名单")
	}
	if _, ok := a.store.UserByID(target); !ok {
		return nil, bad(http.StatusNotFound, "用户不存在")
	}
	blocked, _ := body["blocked"].(bool)
	if err := a.store.SetBlocked(sess.UserID, target, blocked); err != nil {
		return nil, mapStoreErr(err)
	}
	return map[string]any{"ok": true}, nil
}

// parseListOpts 解析消息列表参数：after（增量）、before（加载更早）、rseq（撤回同步游标，出现即返回撤回列表）
func parseListOpts(r *http.Request) (after, before, rseq int64, withRseq bool, err error) {
	after, err = parseAfter(r)
	if err != nil {
		return 0, 0, 0, false, err
	}
	q := r.URL.Query()
	if s := q.Get("before"); s != "" {
		if before, err = strconv.ParseInt(s, 10, 64); err != nil || before < 0 {
			return 0, 0, 0, false, bad(http.StatusBadRequest, "before 参数错误")
		}
	}
	if s := q.Get("rseq"); s != "" {
		if rseq, err = strconv.ParseInt(s, 10, 64); err != nil || rseq < 0 {
			return 0, 0, 0, false, bad(http.StatusBadRequest, "rseq 参数错误")
		}
		withRseq = true
	}
	return after, before, rseq, withRseq, nil
}

// quoteFor 生成引用信息：引用的原消息已撤回时只显示“原消息已被撤回”
func quoteFor(id int64, username, content, kind string, recalled bool) *Quote {
	if recalled {
		return &Quote{ID: id, Username: username, Kind: kindRecalled, Content: quoteRecalledText}
	}
	return quoteOf(id, username, content, kind)
}

// groupRename 群主重命名小群
func (a *server) groupRename(sess session, body map[string]any) (any, error) {
	gid, ok := int64Field(body, "group_id")
	if !ok {
		return nil, bad(http.StatusBadRequest, "群参数错误")
	}
	name := strings.TrimSpace(strField(body, "name"))
	if n := utf8.RuneCountInString(name); n < 1 || n > maxGroupNameLen {
		return nil, bad(http.StatusBadRequest, "群名称需为 1-20 个字")
	}
	if err := a.store.RenameGroup(gid, sess.UserID, name); err != nil {
		return nil, mapStoreErr(err)
	}
	return map[string]any{"ok": true}, nil
}

// groupDissolve 群主解散小群（群及全部消息被删除）
func (a *server) groupDissolve(sess session, body map[string]any) (any, error) {
	gid, ok := int64Field(body, "group_id")
	if !ok {
		return nil, bad(http.StatusBadRequest, "群参数错误")
	}
	if err := a.store.DissolveGroup(gid, sess.UserID); err != nil {
		return nil, mapStoreErr(err)
	}
	return map[string]any{"ok": true}, nil
}
