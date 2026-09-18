// admin.go 运维管理端点（issue #138 / #118）：账号的临时停用 / 恢复 / 复活。
//
// 设计要点（与维护者在 issue #118 预告的方案一致）：
//   - 手动停用是**独立状态位** manual_disabled，与自动禁用 disabled 并列、互不影响。
//     复用同一字段会让运维意图被签到解冻、refresh 成功等自动复活路径意外解除。
//   - 语义是「对话流量摘除」而非「账号冻结」：不碰冷却/熔断维度，签到与保活照常，
//     凭证和积分都是活的；恢复时拿到的是停用期间真实发生的状态。
//   - 默认关闭（config admin.enabled），开启后与 /status 共用同一把 api_key 鉴权。
//   - 幂等：面板重试不会报错；重复调用只更新原因文案。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// adminState 管理端点的统一响应体：回显操作后的双位状态，面板据此直接更新 UI，
// 不必再打一次 /status。
type adminState struct {
	UID            string `json:"uid"`
	ManualDisabled bool   `json:"manual_disabled"`
	ManualReason   string `json:"manual_reason,omitempty"`
	// Disabled 保留在响应里让面板能区分「手动摘除」与「系统判定坏了」——
	// 恢复按钮的语义对两者不同（enable 解手动位，revive 解自动位）。
	Disabled bool `json:"disabled"`
	Changed  bool `json:"changed"`
}

// adminGuard 统一前置：开关关闭 → 404（不暴露管理面存在）；uid 为空 → 400。
// 返回 false 表示已写出响应，调用方应直接 return。
func (h *Handler) adminGuard(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !h.cfg.AdminEnabled {
		// 用 404 而非 403：未开启时这些路径"不存在"，避免探测出管理面。
		writeOpenAIError(w, http.StatusNotFound, "not_found", "not found")
		return "", false
	}
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "uid is required")
		return "", false
	}
	return uid, true
}

// adminReasonFromBody 读可选 JSON 体里的 reason 字段。
// 空体/非 JSON/无该字段都返回空串（端点不因体格式拒绝——无体是最常见调用形态）。
func adminReasonFromBody(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	// 限制读取量：reason 是短文本，避免畸形大请求占用内存。
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil || len(raw) == 0 {
		return ""
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	return strings.TrimSpace(body.Reason)
}

// adminAccountDisable 手动停用：把账号摘出选号池，但保留在池里
// （状态/冷却/成本台账继续归它管，签到与保活照常）。
func (h *Handler) adminAccountDisable(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.adminGuard(w, r)
	if !ok {
		return
	}
	reason := adminReasonFromBody(r)
	if reason == "" {
		reason = "manual"
	}
	found, changed := h.cfg.Pool.SetManualDisabled(uid, true, reason)
	if !found {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	stopped, stopReason, _ := h.cfg.Pool.ManualDisabledState(uid)
	writeJSON(w, http.StatusOK, adminState{
		UID: uid, ManualDisabled: stopped, ManualReason: stopReason,
		Disabled: h.accountAutoDisabled(uid), Changed: changed,
	})
}

// adminAccountEnable 解除手动停用。若账号仍被系统自动禁用（disabled），它**不会**
// 因此回到选号池——那需要 revive。响应里的 disabled 字段就是给面板看的提示。
func (h *Handler) adminAccountEnable(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.adminGuard(w, r)
	if !ok {
		return
	}
	found, changed := h.cfg.Pool.SetManualDisabled(uid, false, "")
	if !found {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	stopped, reason, _ := h.cfg.Pool.ManualDisabledState(uid)
	writeJSON(w, http.StatusOK, adminState{
		UID: uid, ManualDisabled: stopped, ManualReason: reason,
		Disabled: h.accountAutoDisabled(uid), Changed: changed,
	})
}

// adminAccountRevive 解除系统自动禁用（清 disabled + reason + 连续 12153 计数）。
// 不碰手动停用位：运维明确摘除的号不应被一次 revive 悄悄放回选号池。
func (h *Handler) adminAccountRevive(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.adminGuard(w, r)
	if !ok {
		return
	}
	if _, _, found := h.cfg.Pool.ManualDisabledState(uid); !found {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	changed := h.cfg.Pool.ReviveDisabled(uid)
	stopped, reason, _ := h.cfg.Pool.ManualDisabledState(uid)
	writeJSON(w, http.StatusOK, adminState{
		UID: uid, ManualDisabled: stopped, ManualReason: reason,
		Disabled: h.accountAutoDisabled(uid), Changed: changed,
	})
}

// accountAutoDisabled 读某账号当前的自动禁用位（供响应回显）。
// 复用 Pool.List 的单账号查询：轮询全部账号在小池下开销可忽略，
// 且避免为此在 pool 上再开一个只读访问器（保持接口面最小）。
func (h *Handler) accountAutoDisabled(uid string) bool {
	for _, st := range h.cfg.Pool.List() {
		if st.UID == uid {
			return st.Disabled
		}
	}
	return false
}
