// 账号状态演进与查询：禁用/12153 连续计数判定、成功与错误入账、复活解冻，
// 以及状态查询（Status/AvailableUIDs/PickByUIDForModel/CountsDetailed/ServableNow/List）。
package pool

import (
	"log"
	"sort"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

// Disable 永久禁用（session 死亡 / 账号级授权封禁），需人工重登后手工恢复或文件替换。
// 经 disableLocked：置 disabled 并清冷却域（until/coolKind/softStreak/modelCooldowns），
// 熔断器保留（熔断是连续 5xx 信号，与授权/session 正交，见 transition.go）。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		p.disableLocked(e, reason)
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
		p.dirty.Store(true)
		return false
	}
	e.sessionDeadFails = 0
	p.disableLocked(e, sessionDeadReason)
	return true
}

// ClearSessionDead 清连续 12153 计数——账号被证明未死的任何时刻调用：
// refresh 成功（RunKeepaliveNow）、chat 成功（NoteSuccess）、手工复活（ReviveDisabled）。
func (p *Pool) ClearSessionDead(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && e.sessionDeadFails != 0 {
		e.sessionDeadFails = 0
		p.dirty.Store(true)
	}
}

// ReviveDisabled 人工/端点复活入口：清除 disabled + reason + 连续 12153 计数，
// 账号回到池子（若无其他冷却/熔断则立即可选，健康检查自然接管）。
// **不改** Disabled 在选号/状态端点的既有语义：disabled 号依然不参与选号，
// 直到被本方法复活。不存在的 uid 为空操作。
// 注意：不动 manualDisabled —— 自动禁用与手动停用是独立的两位，本方法只解系统判定，
// 运维意图要由 SetManualDisabled(uid,false) 单独解除（否则一次 revive 会悄悄
// 把运维明确摘除的号放回选号池）。返回 true 表示本次确实清除了自动禁用。
func (p *Pool) ReviveDisabled(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || !e.disabled {
		return false
	}
	e.disabled = false
	e.reason = ""
	e.sessionDeadFails = 0
	p.dirty.Store(true)
	return true
}

// SetManualDisabled 运维手动停用/恢复（issue #138/#118）：置位时只摘除选号流量，
// 账号仍在池里——签到、token 保活、排程任务照常执行，凭证与积分是活的。
// 与自动禁用（Disabled）互相独立：本方法不清 disabled，也不清冷却/熔断维度；
// 恢复时同理只清 manualDisabled。两位都清空后账号自然回到选号池。
// 幂等：重复置位/清除不报错，重复操作只更新原因文案。
// 返回 (found, changed)：uid 不存在 → (false,false)；状态无变化 → (true,false)。
func (p *Pool) SetManualDisabled(uid string, disabled bool, reason string) (found, changed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false, false
	}
	if e.manualDisabled == disabled && (!disabled || e.manualReason == reason) {
		return true, false
	}
	p.setManualDisabledLocked(e, disabled, reason)
	return true, true
}

// ManualDisabledState 读单个账号的手动停用态（供端点回显）。uid 不存在时 ok=false。
func (p *Pool) ManualDisabledState(uid string) (disabled bool, reason string, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, found := p.byUID[uid]
	if !found {
		return false, "", false
	}
	return e.manualDisabled, e.manualReason, true
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号非禁用时，清冷却域（余额恢复）。
// 迁移经 transition.reviveCoolingLocked：只清冷却域（until/coolKind/softStreak/
// modelCooldowns）并更新 credits，不动熔断器（fails/retryCount/breakerUntil）——
// 签到成功只证明余额恢复与 billing 通道健康，不证明 chat 通道健康，熔断（连续 5xx
// 信号）不应被签到覆盖。remain==0 或禁用时只更新 credits（不动冷却/禁用）。
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

// NoteError 记录一次错误：喂入唯一的连续失败计数器 fails + 累计错误 errTotal
// （仅状态展示；原成功率 EMA 已删，此处不再喂）。达到 breakerThreshold 触发熔断
// （指数退避），连续失败语义整体并入熔断器（不再有独立的 err 冷却）。
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

// ModelCost 读取账号在某模型上的实测扣费观测（CostPer1k 与是否存在有效观测）。
// DeptestOnly: 生产只写不读（NoteModelCost 有调用），读取侧仅
// handler_cost_test / global_e2e_test 断言账本内容（账本内容现经 state.json
// 持久化，但 /status 透出走 statusOf 的只读遍历，不经本方法）。跨包
// （internal/server）测试引用，迁 export_test.go 不可行（对包外不可见）。
// 无观测或观测过期（modelCostTTL）时 ok=false。
func (p *Pool) ModelCost(uid, model string) (per1k float64, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, exists := p.byUID[uid]
	if !exists {
		return 0, false
	}
	mc, ok := e.modelCostOf(model, time.Now())
	if !ok {
		return 0, false
	}
	return mc.CostPer1k, true
}

// NoteModelCost 记录一次实测扣费观测，更新该 (账号, 模型) 的成本账本，并顺带
// 扣减账号余额（credits/creditsExpiring，见下方「P1-A」段）。credit 为上游
// usage.credit（本次真实扣费=消耗量），tokens 为本次请求的 token 总数
// （prompt+completion，用于折算单位成本）。tokens<=0 时不记录：无法折算单价，
// 记进去会污染账本。
//
// 用 EMA 平滑（alpha=0.3，约 5 次观测收敛）：单次异常值不主导选号决策。
// 账本持久化到 state.json（stateAccount.ModelCosts，P1-anti-monopoly）：重启后
// 成本知识保留，限免/夜间免费的跨重启窗口不再重新付学费探测；落盘/恢复均按
// modelCostTTL 惰性过滤——陈旧价格（时段性优惠）不跨 TTL 复活。
// 限免结束事件：tier 0 观测（per1k≤0）被 credit>0 观测覆盖时打一条明确日志
// （运维据此知道"免费午餐结束了"），判定在写入口做、只看覆盖前值。
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
	// P1-A credits 签到外回写：credit 是本次请求的**消耗量**（上游 usage.credit，
	// handler 侧 stats.Credit()/usageCreditTotal），不是剩余余额。顺手扣减 credits
	// 与 creditsExpiring，让三因子里的两个余额因子随消耗实时收敛——旧口径只在
	// 签到（每天 09:00/21:00 两次）刷新，两次签到之间（最长 12h）高消耗号持续
	// 高权重直到打空撞 402；global 账号不签到，credits 曾是终身冻结。
	// 签到仍定期覆盖（ReenableIfCredits/SetCreditsDetailed 以 authoritative 余额
	// 重置），扣减只是两次签到之间的内插估计；credit=0（免费请求）不动余额。
	if credit > 0 {
		d := int64(credit + 0.5) // 四舍五入，与测试口径一致（2.5 → 3）
		if d > e.credits {
			d = e.credits // 钳 0：扣穿（对账延迟/消费早于记账）不产生负余额
		}
		e.credits -= d
		if e.creditsExpiring > 0 {
			if d > e.creditsExpiring {
				d = e.creditsExpiring
			}
			e.creditsExpiring -= d
		}
	}
	if e.modelCost == nil {
		e.modelCost = make(map[string]modelCostEntry)
	}
	const alpha = 0.3
	prev, seen := e.modelCost[model]
	if !seen {
		e.modelCost[model] = modelCostEntry{CostPer1k: per1k, LastSeen: time.Now(), Samples: 1}
	} else {
		// 限免结束事件（判定在写入口，只看覆盖前值）：此前 tier 0（实测免费，
		// per1k≤0）且本次实测收费（per1k>0）——账号在该模型上的免费窗口结束，
		// EMA 混合后单价转正，下一轮选号即降 tier 2。打一条日志让运维看得见
		// 「免费午餐结束」这一关键状态迁移。
		if prev.CostPer1k <= 0 && per1k > 0 {
			log.Printf("[pool] model %s on uid %s: free tier ended, now %.3f credits/1k", model, logfmt.UID8(uid), per1k)
		}
		e.modelCost[model] = modelCostEntry{
			CostPer1k: prev.CostPer1k*(1-alpha) + per1k*alpha,
			LastSeen:  time.Now(),
			Samples:   prev.Samples + 1,
		}
	}
	p.dirty.Store(true) // 账本已持久化（P1-anti-monopoly）：写入口统一置脏
}

// NoteSuccess 成功请求累加成功计数、刷新 lastSuccess，并清空连续失败与熔断运行态。
// 二进制模型：清 fails + retryCount + breakerUntil；不碰 until/coolKind（那些是即时冷却，各自到期）。
// 额外清 softStreak：成功是账号已恢复的最强证据，连续软限流计数就此归零、退避回到基数。
// 同样清 sessionDeadFails：成功证明 session 未死（与 ClearSessionDead 语义一致）。
// 连败降权（issue #114）同样按「成功是恢复的最强证据」清零：consecutiveFails 归零、
// degradeUntil 清空——成功即回池，不等降权到期（与 NoteSuccess 清 breakerUntil 同口径）。
// **不碰 modelCooldowns**：6004 模型级 limit 每模型独立计时，其他模型成功不得抹掉
// 本模型的冷却截止（这正是"每模型独立"的语义）。模型级冷却只由到期/复活/账号级
// 冷却（Cooldown/reviveCoolingLocked）清除。
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
		e.consecutiveFails = 0
		e.degradeUntil = time.Time{}
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
	return p.availableUIDsLocked("", func(e *entry, now time.Time) bool { return e.healthy(now) })
}

// AvailableUIDsForModel 同 AvailableUIDs，但把健康口径换成 healthyForModel：
// 在该模型上被 6004 限流的账号不列入，而在**其他模型**被限流的账号照常列入
// （issue #31 模型豁免）。
// DeptestOnly: 仅 cost_test.go 引用；生产经 wiring.go 走
// AvailableUIDsForModelRealm（带 realm 维度）。保留作 ForModelRealm 的
// realm=="" 退化语义锚点测试。
// 供会话粘性按模型分配与命中校验；model 为空时等价于 AvailableUIDs。
func (p *Pool) AvailableUIDsForModel(model string) []string {
	return p.availableUIDsLocked("",
		func(e *entry, now time.Time) bool { return e.healthyForModel(now, model) })
}

// availableUIDsLocked 是 AvailableUIDs 四变体（AvailableUIDs/ForModel/ForRealm/
// ForModelRealm）共用的遍历实现：realm 过滤（""=全池）+ 可替换健康口径（healthy /
// healthyForModel）+ 在途占满过滤，输出按 UID 排序（稳定）。调用方必须不持锁。
func (p *Pool) availableUIDsLocked(realm string, health func(e *entry, now time.Time) bool) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !health(e, now) {
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

// PickByUIDForModel 若 uid 当前 healthy（含模型级 6004 豁免口径）且未占满在途名额，
// 返回其凭证（记录 lastUsed 防撞号）；否则返回 nil。供会话粘性路由命中校验与直取使用。
// 绑定号在当前模型被 6004 限流时返回 nil，让调用方（handler）解绑并回落普通轮换——
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
	// 粘性路径同样推进 usedSeq/pickSeq：粘性重度使用的账号在 LRU 兜底
	// （pick 按 usedSeq 选最旧）眼中不再是"最旧"，与 pick 的严格全序语义对齐
	// （entry.usedSeq 注释声明「每次被选中时取 pickSeq 自增值」，粘性命中也是选中）。
	p.pickSeq++
	e.usedSeq = p.pickSeq
	return e.a
}

// CountsDetailed 返回 total/healthy/cooling/disabled/inFlightFull 五类计数。
// cooling 含常规冷却（until）与熔断期（breakerUntil）。
// 注意：healthy 口径不含 inFlight 维度（是状态机权威判定，只看 disabled/until/breakerUntil）；
// inFlightFull 是 healthy 的子集——healthy 里已达在途上限的账号数，供 /status 透出满载度。
// 与 ServableNow 的区别见该函数注释。
func (p *Pool) CountsDetailed() (total, healthy, cooling, disabled, inFlightFull int) {
	return p.countsDetailedForRealm("")
}

// CountsDetailedForRealm 同 CountsDetailed，但仅统计 Realm()==realm 的账号；
// realm=="" 退化为全池（现状语义，走同一遍历 helper 避免重复代码）。
// 双 realm 共存时供 /status 按域分组暴露 CN/global 各自可用性。
func (p *Pool) CountsDetailedForRealm(realm string) (total, healthy, cooling, disabled, inFlightFull int) {
	return p.countsDetailedForRealm(realm)
}

// countsDetailedForRealm 是两函数共用的遍历实现；realm=="" 不加谓词。
func (p *Pool) countsDetailedForRealm(realm string) (total, healthy, cooling, disabled, inFlightFull int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		total++
		switch {
		// 手动停用与自动禁用同归 disabled 计数：对「多少号不参与选号」这个运维
		// 问题二者等价，分开会让 total/healthy/cooling/disabled 不闭合。
		// 具体是哪一种看 /status 账号级的 manual_disabled/disabled 两位。
		case e.disabled || e.manualDisabled:
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
// 对触发模型不可用、对其他模型仍可选，探活与 chat 必须同口径，否则"全号被某模型限流
// 但换模型可用"时 chat 实际 200 而 /healthz 误报 503。chat 侧按请求模型细粒度判定
// （healthyForModel：全账号健康且该模型不在独立冷却内才放行，模型豁免作用于选号），
// 探活侧没有请求模型上下文，取「存在豁免形态」的存在性语义——豁免账号（未禁用、
// 未熔断、存在模型级冷却条目）至少还剩触发模型之外的模型可用，ServableNow 计入。
// 注意与 chat 判定在"账号级 until 冷却 + 模型豁免并存"时并不完全重合：modelExempt
// 不检查 until，而 healthyForModel 会先判 until 再查模型冷却；该混合形态现实中不可达
// （plain Cooldown 会清空 modelCooldowns，6004 不写 until），此处仅为探活存在性语义，
// 不构成 chat 选号路径。
func (p *Pool) ServableNow() bool {
	return p.servableLocked("")
}

// ServableForRealm 报告某 realm 是否可服务：存在至少一个该 realm 的 healthy 且未占满在途名额的账号。
// 与 ServableNow 同口径（healthy 或模型豁免、排除 inFlightFull），仅叠加 Realm()==realm 谓词。
// realm=="" 退化为 ServableNow（现状语义）。供 /healthz 按 realm 暴露 CN/global 各自可达性。
func (p *Pool) ServableForRealm(realm string) bool {
	return p.servableLocked(realm)
}

// servableLocked 是 ServableNow / ServableForRealm 共用的遍历实现：
// 存在至少一个（realm 匹配、未占满在途名额、healthy 或模型豁免形态）的账号即 true。
// realm=="" 不加 realm 谓词（全池）。调用方必须不持锁。
func (p *Pool) servableLocked(realm string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
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
	// reason 过期清理：非 disabled 账号若 until 已过期/零值，reason 清空（与落盘
	// 清理 cooledReasonLocked 同口径）。disabled 账号的 reason 是禁用原因，保留。
	_, reason := cooledReasonLocked(e, now)
	st := Status{
		UID: uid,
		// 限额台账（issue #36）：仅「带解析时间 6004 的模型级软冷却」仍在生效时非空，
		// 每模型一行（modelCooldowns 内未到期的条目），多模型同时限流全部展示。
		// 到期判据 = 该模型的独立冷却 until 未过；条件满足才输出，随到期自然消失，
		// 普通软冷却（无模型级表）/硬冷却不产生台账（零回归）。
		RateLimitedModels: p.rateLimitedModelsLocked(e, now),
		// 成本台账（P1-anti-monopoly）：每模型一行（modelCost 内 TTL 未过期的
		// 条目），运维据此自查「为什么总选它」；只读遍历零风险，过期即消失。
		ModelCosts: p.modelCostsStatusLocked(e, now),
		Realm:             e.a.Realm(),
		Nickname:          e.a.Nickname,
		Credits:           e.credits,
		// Cooling 口径含连败降权（degradeUntil）：降权期账号不可选，运维在 /status
		// 应看到它处于非健康态（CoolRemaining 取三截止最远者，与 healthy 或门同口径）。
		Cooling: now.Before(e.until) || now.Before(e.breakerUntil) || now.Before(e.degradeUntil),
		Reason:            reason,
		Disabled:          e.disabled,
		ManualDisabled:    e.manualDisabled,
		SuccessCount:      e.successCount,
		ErrTotal:          e.errTotal,
		LastSuccessTime:   e.lastSuccess,
		LastErrTime:       e.lastErr,
		ConsecutiveFails:  e.consecutiveFails,
		DegradeUntil:      e.degradeUntil,
		Until:             e.until,
		SoftStreak:        e.softStreak,
		InFlight:          int(e.inFlight.Load()),
		BreakerFails:      e.fails,
		BreakerUntil:      e.breakerUntil,
	}
	if st.Disabled {
		// 禁用账号透出禁用原因（运维看不到为什么死）。
		st.DisabledReason = e.reason
	}
	if st.ManualDisabled {
		// 手动停用原因。与 DisabledReason 分开两个字段：叠加态下运维要能同时看到
		//「我为什么摘它」和「系统为什么判它坏」，合并成一个字段会互相覆盖。
		st.ManualReason = e.manualReason
	}
	if st.Cooling {
		// 冷却剩余秒数（向上取整，避免 0 显示为已到期）。口径与 Cooling 判定一致：
		// 取 until / breakerUntil / degradeUntil 中更远的截止（发现 5——熔断冷却的号
		// 原实现只算 until，显示"冷却中却 0 秒恢复"；BreakerUntil 虽单独透出，两口径
		// 不一致误导排查）。全部过期不会进入本分支（Cooling=false）。
		remain := time.Until(e.until)
		if b := time.Until(e.breakerUntil); b > remain {
			remain = b
		}
		if d := time.Until(e.degradeUntil); d > remain {
			remain = d
		}
		st.CoolRemaining = int64(remain.Seconds() + 0.999)
		if st.CoolRemaining < 0 {
			st.CoolRemaining = 0
		}
		st.CoolKind = e.coolKind.String()
		// 纯降权形态（无生效的 until/熔断）时 reason 取连败文案：降权由 NoteFailures
		// 触发，不写 until/reason（coolKind 也不是它写的），运维在 /status 需要看到
		// "为什么非健康"。有生效冷却时以冷却 reason 为准（冷却通常语义更具体）。
		if st.Reason == "" && now.Before(e.degradeUntil) {
			st.Reason = degradeReason
			st.CoolKind = "degrade"
		}
	}
	return st
}

// rateLimitedModelsLocked 构建单账号的限额台账行，从 modelCooldowns 遍历输出——
// 每模型一行（含该模型的独立 until + 上游原始 resetAt），多模型同时 6004 全部展示。
// 有效期判据 = 该模型的独立冷却 until 未过；随到期自然消失（与 /status 观感一致）。
// 无模型级冷却（普通软冷却/硬冷却）→ nil（零回归）。调用方必须已持有 p.mu。
func (p *Pool) rateLimitedModelsLocked(e *entry, now time.Time) []RateLimitedModel {
	if len(e.modelCooldowns) == 0 {
		return nil
	}
	// 先排序模型名，保证 /status 输出稳定（map 遍历无序）。
	models := make([]string, 0, len(e.modelCooldowns))
	for m := range e.modelCooldowns {
		models = append(models, m)
	}
	sort.Strings(models)
	rows := make([]RateLimitedModel, 0, len(models))
	for _, m := range models {
		mc := e.modelCooldowns[m]
		if !mc.Until.IsZero() && now.Before(mc.Until) {
			row := RateLimitedModel{
				Model:  m,
				Until:  mc.Until,
				Reason: mc.Reason,
			}
			// 上游「将在 … 重置」的原始墙钟：无论是否被 soft_rate_max 截断都透出——
			// 未截断时 Until==ResetAt（两者同值），截断时 ResetAt 是真实恢复时刻，
			// 台账据此始终可见上游权威时点（omitempty 仅在无 ResetAt 的旧数据上省略）。
			if !mc.ResetAt.IsZero() {
				row.ResetAt = mc.ResetAt
			}
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return rows
}

// modelCostsStatusLocked 构建单账号的成本台账行，从 modelCost 遍历输出——
// 每模型一行（单价/最近观测/样本数），仅 modelCostTTL 内的有效观测，过期即
// 消失（与 modelCostOf 读取口径一致；与 rateLimitedModelsLocked 同构）。
// 先排序模型名保证 /status 输出稳定（map 遍历无序）。无观测 → nil（零回归）。
// 调用方必须已持有 p.mu。
func (p *Pool) modelCostsStatusLocked(e *entry, now time.Time) []ModelCostStatus {
	if len(e.modelCost) == 0 {
		return nil
	}
	models := make([]string, 0, len(e.modelCost))
	for m, mc := range e.modelCost {
		if mc.LastSeen.IsZero() || now.Sub(mc.LastSeen) > modelCostTTL {
			continue // 过期/零值：不进台账（与选号读取侧同口径）
		}
		models = append(models, m)
	}
	if len(models) == 0 {
		return nil
	}
	sort.Strings(models)
	rows := make([]ModelCostStatus, 0, len(models))
	for _, m := range models {
		mc := e.modelCost[m]
		rows = append(rows, ModelCostStatus{
			Model:     m,
			CostPer1k: mc.CostPer1k,
			LastSeen:  mc.LastSeen,
			Samples:   mc.Samples,
		})
	}
	return rows
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------
