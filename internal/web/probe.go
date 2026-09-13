// probe.go 只读探测：签到状态 / 猫猫档案 / 旅行状态 / 积分富版，逐项降级。
// 与 Node 版 probeStatusOne 同轨：每项失败只记 errors，不影响其他项。
package web

import (
	"net/http"
	"time"

	"workbuddy2api/internal/auth"
)

const (
	probePacingMs  = 800 * time.Millisecond // 全量探测账号间限速
	refreshSkewSec = 10 * 60                // token 提前刷新窗口（与网关 RefreshSkew 一致）
)

// ensureFresh token 临近过期先刷新（写回凭证文件）；刷新失败不阻断，各探测项自行报错。
func (h *Handler) ensureFresh(a *auth.Auth) {
	if a.NeedsRefresh(refreshSkewSec * time.Second) {
		if err := h.cfg.Upstream.RefreshToken(a); err == nil {
			_ = a.SaveAtomic()
		}
	}
}

// probeStatusOne 单账号四项探测（读-only，不派出/不领奖）。
func (h *Handler) probeStatusOne(a *auth.Auth) *LiveStatus {
	ls := &LiveStatus{Ts: nowMs(), Errors: map[string]string{}}
	h.ensureFresh(a)

	checked, err := h.cfg.Upstream.CheckinStatus(a)
	if err != nil {
		ls.Errors["checkin"] = err.Error()
	} else {
		ls.Checkin = &CheckinView{Checked: checked}
	}

	buddy, err := h.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		ls.Errors["buddy"] = err.Error()
	} else if buddy == nil {
		ls.Buddy = &BuddyView{Has: false}
	} else {
		ls.Buddy = &BuddyView{Has: true, Name: buddy.Name}
	}

	st, err := h.cfg.Upstream.TravelStatus(a)
	if err != nil {
		ls.Errors["travel"] = err.Error()
	} else {
		ls.Travel = travelView(st)
	}

	rich, err := h.cfg.Upstream.UserResourceRich(a)
	if err != nil {
		ls.Errors["credits"] = err.Error()
	} else {
		ls.Credits = &CreditView{
			Source: rich.Source, Total: rich.Total, Used: rich.Used, Remain: rich.Remain,
			Packages: rich.Packages,
		}
		h.st.addSnapshot(a.UID, rich.Remain, ls.Ts)
		// 池内余额同步（/status 展示与签到解冻同口径）
		h.cfg.Pool.SetCredits(a.UID, int64(rich.Remain))
	}

	h.st.setLiveStatus(a.UID, ls)
	h.st.markRefreshed(a.UID, ls.Ts)
	return ls
}

func (h *Handler) probeOne(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	entries, _ := scanAuthDir(h.cfg.AuthDir)
	e := findAuth(entries, uid)
	if e == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	h.probeMu.Lock()
	ls := h.probeStatusOne(e.auth)
	h.probeMu.Unlock()
	h.st.addEvent(Event{Ts: nowMs(), Kind: "probe", UID: e.auth.UID, Nickname: e.auth.Nickname, Status: "ok", Msg: "状态探测"})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": e.auth.UID, "status": ls})
}

func (h *Handler) probeAll(w http.ResponseWriter, r *http.Request) {
	entries, _ := scanAuthDir(h.cfg.AuthDir)
	type result struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
		OK       bool   `json:"ok"`
		Error    string `json:"error,omitempty"`
	}
	results := []result{}
	h.probeMu.Lock()
	first := true
	for i := range entries {
		if entries[i].auth == nil {
			continue
		}
		if !first {
			time.Sleep(probePacingMs)
		}
		first = false
		a := entries[i].auth
		ls := h.probeStatusOne(a)
		res := result{UID: a.UID, Nickname: a.Nickname, OK: len(ls.Errors) == 0}
		if !res.OK {
			for k, v := range ls.Errors {
				res.Error = k + ": " + v
				break
			}
		}
		results = append(results, res)
	}
	h.probeMu.Unlock()
	probed := 0
	for _, r := range results {
		if r.OK {
			probed++
		}
	}
	h.st.addEvent(Event{Ts: nowMs(), Kind: "probe", Status: "ok", Msg: "全量状态探测"})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "total": len(results), "probed": probed, "failed": len(results) - probed, "results": results,
	})
}
