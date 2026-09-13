// 账号状态演进与查询：禁用/12153 连续计数判定、成功与错误入账、复活解冻，
// 以及状态查询（Status/AvailableUIDs/PickByUID/CountsDetailed/ServableNow/List）。
package pool

import (
	"sort"
	"time"

	"workbuddy2api/internal/auth"
)

func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
		p.dirty.Store(true)
	}
}

// NoteSessionDead 记录一次 ErrSessionDead（12153）——**不立即禁用**。
// 旧行为一次 12153 即 Disable，但 12153 会被临时性触发（网络抖动/上游闪断/refresh
// 竞态），一次失败就永久杀号会误杀健康账号（P0-1 侦察：13 个 disabled 号全部 refresh
// 成功，是历史误判的受害者）。改为连续 sessionDeadThreshold 次才禁用：
// 计数 +1，达到阈值 → Disable（reason=12153 session dead）并清计数；
// refresh 成功 / 任意成功 / 手工复活 → ClearSessionDead 清计数。
// 返回 true 表示本次已达阈值并完成禁用。
// 即使账号已 disabled，计数仍累计并返回 false 前 N-1 次——但 keepalive 会跳过
// disabled 号，实际只有「已 disabled 后复活且计数未清」这类场景才会走到这里。
func (p *Pool) NoteSessionDead(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.sessionDeadFails++
	if e.sessionDeadFails < sessionDeadThreshold {
		return false
	}
	e.disabled = true
	e.reason = sessionDeadReason
	e.sessionDeadFails = 0
	p.dirty.Store(true)
	return true
}

// ClearSessionDead 清连续 12153 计数——账号被证明未死的任何时刻调用：
// refresh 成功（RunKeepaliveNow）、chat 成功（NoteSuccess）、手工复活（ReviveDisabled）。
func (p *Pool) ClearSessionDead(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.sessionDeadFails = 0
	}
}

// ReviveDisabled 人工/端点复活入口：清除 disabled + reason + 连续 12153 计数，
// 账号回到池子（若无其他冷却/熔断则立即可选，健康检查自然接管）。
// **不改** Disabled 在选号/状态端点的既有语义：disabled 号依然不参与选号，
// 直到被本方法复活。不存在的 uid 为空操作。
func (p *Pool) ReviveDisabled(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && e.disabled {
		e.disabled = false
		e.reason = ""
		e.sessionDeadFails = 0
		p.dirty.Store(true)
	}
}

// reviveCoolingLocked 只清冷却（until/coolKind/reason/softStreak）并更新 credits，不动熔断器
// （fails/retryCount/breakerUntil）。签到解冻走这里：签到成功只证明余额恢复与
// billing 通道健康，不证明 chat 通道健康，熔断（连续 5xx 信号）不应被签到覆盖。
// softStreak 属**冷却域**（与 until/coolKind 同域），故随冷却一并清零——与"解冻只清冷却
// 不清熔断"的既有 C5 语义一致；硬冷却（CoolHard）本就不参与 streak，这里清的是历史软冷却累积。
// 调用方必须已持有 p.mu。
func (p *Pool) ReenableIfCredits(uid string, remain int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if remain > 0 && !e.disabled {
			p.reviveCoolingLocked(e, remain)
		} else {
			e.credits = remain
		}
		p.dirty.Store(true)
	}
}

// NoteError 记录一次错误：喂入唯一的连续失败计数器 fails + 累计错误 errTotal。
// 达到 breakerThreshold 触发熔断（指数退避），连续失败语义整体并入熔断器（不再有独立的 err 冷却）。
func (p *Pool) NoteError(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errTotal++
		e.lastErr = time.Now()
		p.recordBreakerFailureLocked(e)
		p.dirty.Store(true)
	}
}

// NoteModelCost 记录一次实测扣费观测，更新该 (账号, 模型) 的成本账本。
// credit 为上游 usage.credit（本次真实扣费），tokens 为本次请求的 token 总数
// （prompt+completion，用于折算单位成本）。tokens<=0 时不记录：无法折算单价，
// 记进去会污染账本。
//
// 用 EMA 平滑（alpha=0.3，约 5 次观测收敛）：单次异常值不主导选号决策。
// 账本仅内存态——成本随上游活动（限免期/夜间免费/折扣）变化，持久化旧值
// 反而是脏数据；重启后重新学习，代价只是前几次请求无偏好。
func (p *Pool) NoteModelCost(uid, model string, credit float64, tokens int) {
	if uid == "" || model == "" || tokens <= 0 {
		return
	}
	// 单价按每千 token 归一，消除请求长度差异。
	per1k := credit / float64(tokens) * 1000
	if per1k < 0 {
		per1k = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	if e.modelCost == nil {
		e.modelCost = make(map[string]modelCostEntry)
	}
	const alpha = 0.3
	prev, seen := e.modelCost[model]
	if !seen {
		e.modelCost[model] = modelCostEntry{CostPer1k: per1k, LastSeen: time.Now(), Samples: 1}
	} else {
		e.modelCost[model] = modelCostEntry{
			CostPer1k: prev.CostPer1k*(1-alpha) + per1k*alpha,
			LastSeen:  time.Now(),
			Samples:   prev.Samples + 1,
		}
	}
}

// NoteSuccess 成功请求累加成功计数、刷新 lastSuccess，并清空连续失败与熔断运行态。
// 二进制模型：清 fails + retryCount + breakerUntil；不碰 until/coolKind（那些是即时冷却，各自到期）。
// 额外清 softStreak：成功是账号已恢复的最强证据，连续软限流计数就此归零、退避回到基数。
// 同样清 sessionDeadFails：成功证明 session 未死（与 ClearSessionDead 语义一致）。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.successCount++
		e.lastSuccess = time.Now()
		e.fails = 0
		e.retryCount = 0
		e.breakerUntil = time.Time{}
		e.softStreak = 0
		e.sessionDeadFails = 0
		p.dirty.Store(true)
	}
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// AvailableUIDs 返回当前 healthy 且未占满在途名额的账号 UID 列表（按 UID 排序，稳定输出）。
// 供会话粘性路由（internal/session）做快路径命中校验 + 双段分配；无可用返回空切片。
func (p *Pool) AvailableUIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// AvailableUIDsForModel 同 AvailableUIDs，但把健康口径换成 healthyForModel：
// 在该模型上被 6004 限流的账号不列入，而在**其他模型**被限流的账号照常列入
// （issue #31 模型豁免）。
// 供会话粘性按模型分配与命中校验；model 为空时等价于 AvailableUIDs。
func (p *Pool) AvailableUIDsForModel(model string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// PickByUIDForModel 同 PickByUID，但用 healthyForModel 校验：绑定号在当前模型被
// 6004 限流时返回 nil，让调用方（handler）解绑并回落普通轮换。
// 这是粘性能"换得动"的关键：绑定只记 uid，若只按账号级 healthy 校验，
// 被模型级限额的号（账号整体仍健康）会被持续选中直到轮换次数耗尽。
func (p *Pool) PickByUIDForModel(uid, model string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	now := time.Now()
	if !e.healthyForModel(now, model) {
		return nil
	}
	if p.inFlightFull(e) {
		return nil
	}
	e.lastUsed = now
	return e.a
}

// PickByUID 若 uid 当前 healthy 且未占满在途名额，返回其凭证（记录 lastUsed 防撞号）；
// 否则返回 nil。供会话粘性路由命中校验与直取使用。
func (p *Pool) PickByUID(uid string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	now := time.Now()
	if !e.healthy(now) {
		return nil
	}
	if p.inFlightFull(e) {
		return nil
	}
	e.lastUsed = now
	return e.a
}

// CountsDetailed 返回 total/healthy/cooling/disabled/inFlightFull 五类计数。
// cooling 含常规冷却（until）与熔断期（breakerUntil）。
// 注意：healthy 口径不含 inFlight 维度（是状态机权威判定，只看 disabled/until/breakerUntil）；
// inFlightFull 是 healthy 的子集——healthy 里已达在途上限的账号数，供 /status 透出满载度。
// 与 ServableNow 的区别见该函数注释。
func (p *Pool) CountsDetailed() (total, healthy, cooling, disabled, inFlightFull int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		total++
		switch {
		case e.disabled:
			disabled++
		case !e.healthy(now):
			cooling++
		default:
			healthy++
			if p.inFlightFull(e) {
				inFlightFull++
			}
		}
	}
	return total, healthy, cooling, disabled, inFlightFull
}

// ServableNow 报告池当前是否可服务：存在至少一个（对任意模型）healthy 且未占满在途名额的账号。
// 与 CountsDetailed 的 healthy 口径不同：healthy 只看 disabled/until/breakerUntil（状态机权威判定），
// 不看 inFlight；ServableNow 额外叠加在途维度，与 chat 的真实可达性（Pick 会跳过 inFlightFull 账号）对齐。
// 专供 /healthz 用，避免"全账号 healthy 但都占满"时探活误报 200 而 chat 返回 503 的口径裂缝。
//
// 模型级豁免（issue #31 的探活侧补齐）：6004 模型级软冷却中的账号（modelExempt 形态）
// 对触发模型不可用、对其他模型仍可选——chat 的 healthyForModel 已按此放行切模型请求，
// 探活必须同口径，否则"全号被 v4.1 限流但 glm 可用"时 chat 实际 200 而 /healthz 误报 503。
// /healthz 无请求模型上下文，取"存在可服务模型"的存在性语义（与 chat 可达性等价）。
func (p *Pool) ServableNow() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if p.inFlightFull(e) {
			continue
		}
		if e.healthy(now) || e.modelExempt() {
			return true
		}
	}
	return false
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}
func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	st := Status{
		UID:             uid,
		Nickname:        e.a.Nickname,
		Credits:         e.credits,
		Cooling:         now.Before(e.until) || now.Before(e.breakerUntil),
		Reason:          e.reason,
		Disabled:        e.disabled,
		SuccessCount:    e.successCount,
		ErrTotal:        e.errTotal,
		LastSuccessTime: e.lastSuccess,
		LastErrTime:     e.lastErr,
		Until:           e.until,
		SoftStreak:      e.softStreak,
		InFlight:        int(e.inFlight.Load()),
		BreakerFails:    e.fails,
		BreakerUntil:    e.breakerUntil,
	}
	if st.Disabled {
		// 禁用账号透出禁用原因（运维看不到为什么死）。
		st.DisabledReason = e.reason
	}
	if st.Cooling {
		// 冷却剩余秒数（向上取整，避免 0 显示为已到期）。
		st.CoolRemaining = int64(time.Until(e.until).Seconds() + 0.999)
		if st.CoolRemaining < 0 {
			st.CoolRemaining = 0
		}
		st.CoolKind = e.coolKind.String()
	}
	return st
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------
