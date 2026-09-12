// tasks.go 手动批量任务：一键签到 / 保活 / 旅行 / 活跃 / 刷新积分。
// 语义与网关调度器同轨（禁用跳过、12153 连续计数、活跃 N 条同会话），
// 结果逐账号落事件日志；与 Node 版 tasks.mjs 返回契约一致。
package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

const taskPacing = 300 * time.Millisecond // 任务账号间基础限速

type taskResult struct {
	UID        string   `json:"uid"`
	Nickname   string   `json:"nickname"`
	Status     string   `json:"status"` // ok/already/skip/warn/fail
	Msg        string   `json:"msg"`              // 短标签(事件日志用)
	Detail     string   `json:"detail,omitempty"` // 长原因(前端 toast 展示)
	Remain     *float64 `json:"remain,omitempty"` // 任务后余额(checkin/credit)
	Reward     int64    `json:"reward,omitempty"` // 旅行领奖积分
	StreakDays int      `json:"streakDays,omitempty"`
	Credits    *CreditView `json:"credits,omitempty"`
}

type taskSummary struct {
	Total   int `json:"total"`
	OK      int `json:"ok"`
	Already int `json:"already"`
	Skip    int `json:"skip"`
	Warn    int `json:"warn"`
	Fail    int `json:"fail"`
}

var validKinds = map[string]bool{
	"checkin": true, "keepalive": true, "travel": true, "activity": true, "credit": true,
}

func (h *Handler) runTask(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if !validKinds[kind] {
		writeErr(w, http.StatusBadRequest, "未知任务类型: "+kind)
		return
	}
	entries, _ := scanAuthDir(h.cfg.AuthDir)
	results := []taskResult{}
	var sum taskSummary

	h.probeMu.Lock()
	first := true
	for i := range entries {
		a := entries[i].auth
		if a == nil {
			continue
		}
		// 与调度器同口径：禁用账号跳过（签到/保活也跳过——12153 判死需人工重登）
		if st, ok := h.cfg.Pool.Status(a.UID); ok && st.Disabled {
			continue
		}
		if !first {
			time.Sleep(taskPacing)
		}
		first = false
		var res taskResult
		switch kind {
		case "checkin":
			res = h.taskCheckin(a)
		case "keepalive":
			res = h.taskKeepalive(a)
		case "travel":
			res = h.taskTravel(a)
		case "activity":
			res = h.taskActivity(a)
		case "credit":
			res = h.taskCredit(a)
		}
		results = append(results, res)
		sum.Total++
		switch res.Status {
		case "ok":
			sum.OK++
		case "already":
			sum.Already++
		case "skip":
			sum.Skip++
		case "warn":
			sum.Warn++
		default:
			sum.Fail++
		}
		h.st.addEvent(Event{Ts: nowMs(), Kind: kind, UID: res.UID, Nickname: res.Nickname, Status: res.Status, Msg: res.Msg})
		h.st.markRefreshed(a.UID, nowMs())
	}
	h.probeMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"kind": kind, "results": results, "summary": sum})
}

// runTaskOne 单账号任务（账号卡下拉动作）。
func (h *Handler) runTaskOne(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	kind := r.PathValue("kind")
	if !validKinds[kind] {
		writeErr(w, http.StatusBadRequest, "未知任务类型: "+kind)
		return
	}
	entries, _ := scanAuthDir(h.cfg.AuthDir)
	e := findAuth(entries, uid)
	if e == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	h.probeMu.Lock()
	var res taskResult
	switch kind {
	case "checkin":
		res = h.taskCheckin(e.auth)
	case "keepalive":
		res = h.taskKeepalive(e.auth)
	case "travel":
		res = h.taskTravel(e.auth)
	case "activity":
		res = h.taskActivity(e.auth)
	case "credit":
		res = h.taskCredit(e.auth)
	}
	h.probeMu.Unlock()
	h.st.addEvent(Event{Ts: nowMs(), Kind: kind, UID: res.UID, Nickname: res.Nickname, Status: res.Status, Msg: res.Msg})
	h.st.markRefreshed(e.auth.UID, nowMs())
	writeJSON(w, http.StatusOK, map[string]any{"kind": kind, "result": res})
}

// creditCache 积分富版结果写缓存 + 快照 + 池同步。
func (h *Handler) creditCache(a *auth.Auth, rich *upstream.ResourceRich) *CreditView {
	cv := &CreditView{Source: rich.Source, Total: rich.Total, Used: rich.Used, Remain: rich.Remain, Packages: rich.Packages}
	now := nowMs()
	if ls := h.st.liveStatus(a.UID); ls != nil {
		ls.Credits = cv
		ls.Ts = now
		h.st.setLiveStatus(a.UID, ls)
	} else {
		h.st.setLiveStatus(a.UID, &LiveStatus{Ts: now, Credits: cv, Errors: map[string]string{}})
	}
	h.st.addSnapshot(a.UID, rich.Remain, now)
	h.cfg.Pool.SetCredits(a.UID, int64(rich.Remain))
	return cv
}

// taskCheckin 签到 + 余额刷新 + 解冻（调度器 RunCheckinNow 同轨）。
func (h *Handler) taskCheckin(a *auth.Auth) taskResult {
	res := taskResult{UID: a.UID, Nickname: a.Nickname}
	h.ensureFresh(a)
	err := h.cfg.Upstream.DailyCheckin(a)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "已签到") || strings.Contains(strings.ToLower(msg), "already") {
			res.Status = "already"
			res.Msg = "今日已签到"
		} else {
			res.Status = "fail"
			res.Msg = "签到失败"
			res.Detail = msg
			return res
		}
	} else {
		res.Status = "ok"
		res.Msg = "签到成功"
	}
	// 已签到/签到成功都走余额刷新（调度器同轨：业务错误也继续查余额）
	if rich, err := h.cfg.Upstream.UserResourceRich(a); err == nil {
		res.Credits = h.creditCache(a, rich)
		remain := rich.Remain
		res.Remain = &remain
		h.cfg.Pool.ReenableIfCredits(a.UID, int64(rich.Remain))
		if res.Status == "ok" {
			res.Msg = fmt.Sprintf("签到成功,剩余 %.2f 分", rich.Remain)
		}
	}
	return res
}

// taskKeepalive 刷新 token；12153 连续计数判死（调度器同轨）。
func (h *Handler) taskKeepalive(a *auth.Auth) taskResult {
	res := taskResult{UID: a.UID, Nickname: a.Nickname}
	if a.RefreshToken == "" {
		res.Status = "skip"
		res.Msg = "无 refreshToken"
		res.Detail = "凭证无 refreshToken"
		return res
	}
	if err := h.cfg.Upstream.RefreshToken(a); err != nil {
		res.Msg = err.Error()
		res.Detail = err.Error()
		var ue *upstream.Error
		if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
			h.cfg.Pool.NoteSessionDead(a.UID)
			res.Msg = "session 死亡(12153)"
			res.Detail = "刷新返回 12153(session 死亡),连续 3 次将禁用,需重新登录"
		}
		res.Status = "fail"
		return res
	}
	h.cfg.Pool.ClearSessionDead(a.UID)
	if err := a.SaveAtomic(); err != nil {
		res.Status = "warn"
		res.Msg = "写回失败"
		res.Detail = "token 刷新成功但凭证写盘失败: " + err.Error()
		return res
	}
	res.Status = "ok"
	res.Msg = "token 已刷新"
	return res
}

// taskCredit 仅刷新积分。
func (h *Handler) taskCredit(a *auth.Auth) taskResult {
	res := taskResult{UID: a.UID, Nickname: a.Nickname}
	h.ensureFresh(a)
	rich, err := h.cfg.Upstream.UserResourceRich(a)
	if err != nil {
		res.Status = "fail"
		res.Msg = "积分查询失败"
		res.Detail = err.Error()
		return res
	}
	res.Credits = h.creditCache(a, rich)
	remain := rich.Remain
	res.Remain = &remain
	h.cfg.Pool.ReenableIfCredits(a.UID, int64(rich.Remain))
	res.Status = "ok"
	res.Msg = fmt.Sprintf("剩余 %.2f 分", rich.Remain)
	return res
}

// taskTravel 推进猫猫旅行一趟：无猫领养 → idle 派出 → arrived 领奖 → traveling 跳过。
// 动作完成后全量探测刷新缓存（卡片即时更新）。
func (h *Handler) taskTravel(a *auth.Auth) taskResult {
	res := taskResult{UID: a.UID, Nickname: a.Nickname}
	h.ensureFresh(a)

	buddy, err := h.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		res.Status = "fail"
		res.Msg = "查询猫档案失败"
		res.Detail = err.Error()
		return res
	}
	if buddy == nil {
		// 无猫：协议 + 领养（门槛未达 → skip，当日不再重试语义由调度器持有，手动触发只报结果）
		_ = h.cfg.Upstream.BuddyAgreement(a)
		if err := h.cfg.Upstream.BuddyFirst(a); err != nil {
			if upstream.IsBuddyTaskIncomplete(err) {
				res.Status = "skip"
				res.Msg = "领养门槛未达标"
				res.Detail = "领养门槛未达标(对话量不足),请先累积对话活跃"
				return res
			}
			res.Status = "fail"
			res.Msg = "领养失败"
			res.Detail = err.Error()
			return res
		}
		res.Msg = "领养成功"
	}

	st, err := h.cfg.Upstream.TravelStatus(a)
	if err != nil {
		res.Status = "fail"
		res.Msg = "查询旅行状态失败"
		res.Detail = err.Error()
		return res
	}
	switch st.State {
	case "arrived":
		reward, err := h.cfg.Upstream.TravelClaim(a, st.RecordID)
		if err != nil {
			res.Status = "fail"
			res.Msg = "领奖失败"
			res.Detail = err.Error()
			return res
		}
		res.Status = "ok"
		res.Reward = reward
		res.Msg = fmt.Sprintf("领取奖励 %d 分", reward)
	case "traveling":
		res.Status = "skip"
		loc := ""
		if st.Location != nil {
			loc = "@" + st.Location.Name
		}
		res.Msg = "旅行中" + loc
		res.Detail = "猫猫仍在旅行中" + loc + ",归来后再领奖"
	default: // idle / 其他
		if st.DailyLimitReached {
			res.Status = "skip"
			res.Msg = "今日已派出"
			res.Detail = "今日派出次数已达上限"
			return res
		}
		if err := h.cfg.Upstream.TravelDepart(a, 4); err != nil {
			res.Status = "fail"
			res.Msg = "派出失败"
			res.Detail = err.Error()
			return res
		}
		res.Status = "ok"
		if res.Msg == "领养成功" {
			res.Msg = "领养成功并派出"
		} else {
			res.Msg = "已派出"
		}
	}
	// 动作后探测刷新（旅行/积分/签到状态即时反映到卡片）
	h.probeStatusOne(a)
	return res
}

// taskActivity 对话活跃上报 N 条（同会话多轮）+ streak 回读自检（调度器同轨）。
func (h *Handler) taskActivity(a *auth.Auth) taskResult {
	res := taskResult{UID: a.UID, Nickname: a.Nickname}
	if a.AccessToken == "" {
		res.Status = "skip"
		res.Msg = "无 accessToken"
		res.Detail = "凭证无 accessToken"
		return res
	}
	count := h.cfg.Schedule.ActivityReportCount
	if count <= 0 {
		count = 1
	}
	cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
	ok := 0
	for i := 1; i <= count; i++ {
		rid := fmt.Sprintf("%s-r%d", cid, i)
		if err := h.cfg.Upstream.ReportChatActivity(a, cid, rid); err != nil {
			res.Status = "fail"
			res.Msg = fmt.Sprintf("上报失败 %d/%d", i, count)
			res.Detail = fmt.Sprintf("第 %d/%d 条上报失败: %v", i, count, err)
			return res
		}
		ok++
		if i < count {
			time.Sleep(1500 * time.Millisecond)
		}
	}
	// N 条发满 → streak 回读（发现 200 静默丢弃）
	days, err := h.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		res.Status = "warn"
		res.Msg = "连登回读失败"
		res.Detail = fmt.Sprintf("上报 %d 条成功,连登回读失败: %v", ok, err)
		return res
	}
	if days == 0 {
		res.Status = "warn"
		res.Msg = "连登 0 天"
		res.Detail = fmt.Sprintf("上报 %d 条成功但连登 0 天(疑似静默丢弃)", ok)
		return res
	}
	res.Status = "ok"
	res.StreakDays = days
	res.Msg = fmt.Sprintf("上报 %d 条,连登 %d 天", ok, days)
	return res
}
