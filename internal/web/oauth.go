// oauth.go OAuth 设备登录会话管理：begin/poll/cancel。
// state 暂存内存（有效期 10 分钟，重启即废）；完成即落盘 auths/workbuddy-<uid>.json 并入池。
package web

import (
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"workbuddy2api/internal/auth"
)

type oauthSession struct {
	state     string
	authURL   string
	createdAt int64
}

const oauthSessionTTLMs = 10 * 60 * 1000

func (h *Handler) oauthBegin(w http.ResponseWriter, r *http.Request) {
	st, err := h.cfg.Upstream.OAuthStart()
	if err != nil {
		writeErr(w, http.StatusBadGateway, "申请登录 state: "+err.Error())
		return
	}
	h.oauthMu.Lock()
	h.oauthSessions[st.State] = &oauthSession{state: st.State, authURL: st.AuthURL, createdAt: nowMs()}
	h.oauthMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"state": st.State, "authUrl": st.AuthURL,
		"expiresInSec": oauthSessionTTLMs / 1000, "intervalSec": 2,
	})
}

func (h *Handler) oauthPoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State string `json:"state"`
	}
	if err := readJSON(r, &body); err != nil || body.State == "" {
		writeErr(w, http.StatusBadRequest, "缺少 state")
		return
	}
	h.oauthMu.Lock()
	sess, ok := h.oauthSessions[body.State]
	h.oauthMu.Unlock()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": "会话不存在或已过期,请重新开始"})
		return
	}
	if nowMs()-sess.createdAt > oauthSessionTTLMs {
		h.oauthMu.Lock()
		delete(h.oauthSessions, body.State)
		h.oauthMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "timeout"})
		return
	}
	token, done, err := h.cfg.Upstream.OAuthPollToken(body.State)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	if !done {
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}
	// 授权完成：取账号信息 → 落盘 → 入池
	acct := h.cfg.Upstream.OAuthAccount(body.State, token.AccessToken)
	a := &auth.Auth{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		ExpiresAt:    time.Now().Unix() + token.ExpiresIn,
		Domain:       token.Domain,
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Nickname:     acct.Nickname,
	}
	if a.UID == "" {
		a.UID = fmt.Sprintf("unknown-%d", nowMs())
	}
	a.FilePath = filepath.Join(h.cfg.AuthDir, "workbuddy-"+a.UID+".json")
	if err := a.SaveAtomic(); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": "凭证写盘: " + err.Error()})
		return
	}
	h.cfg.Pool.Add(a)
	h.oauthMu.Lock()
	delete(h.oauthSessions, body.State)
	h.oauthMu.Unlock()
	h.st.addEvent(Event{Ts: nowMs(), Kind: "oauth", UID: a.UID, Nickname: a.Nickname, Status: "ok", Msg: "OAuth 登录入池"})
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "authorized",
		"saved":  map[string]any{"uid": a.UID, "nickname": a.Nickname},
	})
}

func (h *Handler) oauthCancel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State string `json:"state"`
	}
	_ = readJSON(r, &body)
	h.oauthMu.Lock()
	delete(h.oauthSessions, body.State)
	h.oauthMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
