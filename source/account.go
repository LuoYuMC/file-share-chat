package main

// 账号管理与管理员对小群的操作（数据逻辑在 store.go）

import (
	"errors"
	"net/http"
	"strings"
)

// reservedUsernames 系统保留用户名：不能被注册或修改为
var reservedUsernames = []string{adminUsername, "所有人"}

// isReservedName 用户名是否为系统保留（不区分大小写）
func isReservedName(name string) bool {
	for _, r := range reservedUsernames {
		if strings.EqualFold(name, r) {
			return true
		}
	}
	return false
}

// userNames 用户ID → 当前用户名（用户名可修改，历史消息显示当前用户名）
func (a *server) userNames() map[int64]string {
	out := map[int64]string{}
	for _, u := range a.store.ListUsers() {
		out[u.ID] = u.Username
	}
	return out
}

// requireMain 校验当前用户是否为主管理员（副管理员不可用）
func (a *server) requireMain(sess session) (User, error) {
	u, ok := a.store.UserByID(sess.UserID)
	if !ok || !u.IsAdmin {
		return User{}, bad(http.StatusForbidden, "需要主管理员权限")
	}
	return u, nil
}

// changeUsername 修改自己的用户名（只需已登录；主管理员账号不可修改）
func (a *server) changeUsername(sess session, body map[string]any) (any, error) {
	u, ok := a.store.UserByID(sess.UserID)
	if !ok {
		return nil, bad(http.StatusUnauthorized, "请先登录")
	}
	if u.IsAdmin {
		return nil, bad(http.StatusForbidden, "主管理员账号的用户名不可修改")
	}
	name := strings.TrimSpace(strField(body, "username"))
	if !usernameRE.MatchString(name) {
		return nil, bad(http.StatusBadRequest, "用户名需为 2-32 位，可包含字母、数字、下划线、点或中划线")
	}
	if isReservedName(name) {
		return nil, bad(http.StatusBadRequest, "该用户名为系统保留，请换一个用户名")
	}
	if err := a.store.RenameUser(u.ID, name); err != nil {
		if errors.Is(err, errUserExists) {
			return nil, bad(http.StatusConflict, "用户名已存在")
		}
		return nil, err
	}
	updated, _ := a.store.UserByID(u.ID)
	return map[string]any{"user": userJSON(updated)}, nil
}

// deleteSelf 注销自己的账号（历史消息保留；小群的群主身份按 DeleteUser 的规则转移或解散）
func (a *server) deleteSelf(w http.ResponseWriter, sess session) (any, error) {
	u, ok := a.store.UserByID(sess.UserID)
	if !ok {
		return nil, bad(http.StatusUnauthorized, "请先登录")
	}
	if u.IsAdmin {
		return nil, bad(http.StatusForbidden, "主管理员账号不能注销")
	}
	if err := a.store.DeleteUser(u.ID); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, bad(http.StatusUnauthorized, "请先登录")
		}
		return nil, err
	}
	a.dropUserSessions(u.ID, "")
	setSessionCookie(w, "", -1)
	return map[string]any{"ok": true}, nil
}

// adminUserGroups 主管理员查看某用户创建（群主）的小群
func (a *server) adminUserGroups(sess session, r *http.Request) (any, error) {
	if _, err := a.requireMain(sess); err != nil {
		return nil, err
	}
	uid, err := queryInt(r, "user_id")
	if err != nil {
		return nil, err
	}
	gs := a.store.GroupsOwnedBy(uid)
	out := make([]map[string]any, 0, len(gs))
	for _, g := range gs {
		out = append(out, map[string]any{"id": g.ID, "name": g.Name, "member_count": len(g.Members),
			"created_at": g.CreatedAt})
	}
	return map[string]any{"groups": out}, nil
}

// adminDissolveGroup 主管理员解散某用户创建的小群（只能解散，不能查看群内消息）
func (a *server) adminDissolveGroup(sess session, body map[string]any) (any, error) {
	if _, err := a.requireMain(sess); err != nil {
		return nil, err
	}
	uid, ok := int64Field(body, "user_id")
	if !ok {
		return nil, bad(http.StatusBadRequest, "用户参数错误")
	}
	gid, ok := int64Field(body, "group_id")
	if !ok {
		return nil, bad(http.StatusBadRequest, "群参数错误")
	}
	if err := a.store.DissolveGroup(gid, uid); err != nil {
		return nil, mapStoreErr(err)
	}
	return map[string]any{"ok": true}, nil
}
