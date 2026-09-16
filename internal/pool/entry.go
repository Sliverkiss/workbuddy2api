// Package pool 账号池：单一状态机（健康/冷却/熔断/连败降权）+ 在途租约 + 三因子加权挑选 + state.json 持久化。
package pool

import (
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
)

type CoolKind int

const (
	CoolHard CoolKind = iota // 余额不足 → 冷却到次日 04:00（等签到恢复）
	CoolSoft                 // 429 → 短冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard_credit"
	case CoolSoft:
		return "soft_rate"
	}
	return "unknown"
}

// degradeReason 连败降权（issue #114）写 reason 的固定文案：与冷却域的
// reason（"429 rate limit" / "waf 403 block" / "余额不足"）共用一个字段，
// 运维在 /status 一处即可看到「为什么被降权/冷却」，不新增台账字段。
const degradeReason = "consecutive failures"

// Status 单个账号对外暴露的状态（脱敏）。
type Status struct {
	UID           string    `json:"uid"`
	Realm         string    `json:"realm,omitempty"`
	Nickname      string    `json:"nickname,omitempty"`
	Credits       int64     `json:"credits"`
	Cooling       bool      `json:"cooling"`
	CoolKind      string    `json:"cool_kind,omitempty"`
	CoolRemaining int64     `json:"cool_remaining_sec,omitempty"`
	Until         time.Time `json:"until,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	SoftStreak    int       `json:"soft_streak,omitempty"` // 连续软冷却次数（指数退避指数，见 entry.softStreak）
	// RateLimitedModels 当前仍在限额的模型列表（issue #36 限额台账）。
	// 仅「带解析时间 6004」触发的模型级独立冷却（modelCooldowns 未到期条目）时非空，
	// 每模型一行；运维据此看到"账号 A 的模型 X 还在限额中，预计 Z 时间恢复"。到期即消失（零回归）。
	RateLimitedModels []RateLimitedModel `json:"rate_limited_models,omitempty"`
	// ModelCosts 每模型实测成本台账（P1-anti-monopoly 可观测性）：运维据此自查
	//「为什么总选它」——tier 0（免费）垄断 / tier 2 单价排序一眼可见。
	// 仅 modelCostTTL 内的有效观测，每模型一行（cost_per_1k + last_seen +
	// samples）；无观测/全部过期 → nil（tier 1 未知层）。过期即消失（零回归，
	// 只读遍历零风险）。tier 不单独落字段（可由 per1k≤0 推出，零冗余）。
	ModelCosts []ModelCostStatus `json:"model_costs,omitempty"`
	Disabled          bool               `json:"disabled"`
	DisabledReason    string             `json:"disabled_reason,omitempty"` // 仅 disabled 账号：禁用原因（运维可见）
	SuccessCount      int64              `json:"success_count,omitempty"`
	ErrTotal          int64              `json:"err_total,omitempty"`
	LastSuccessTime   time.Time          `json:"last_success,omitempty"`
	LastErrTime       time.Time          `json:"last_err,omitempty"`
	// ConsecutiveFails 连续失败计数（连败降权用，见 entry.consecutiveFails）。
	// 零值也透出（运维口径：与 err_total/session_dead_fails 一致，零值缺失会让人
	// 误以为"没记录"，实际是零值被 omitempty 省略）。
	ConsecutiveFails int       `json:"consecutive_fails"`
	DegradeUntil     time.Time `json:"degrade_until,omitempty"` // 连败降权截止（非零且未过 = 降权中）
	// 运行态（不持久化）：在途请求数 + 熔断器状态。
	InFlight     int       `json:"in_flight"`
	BreakerFails int       `json:"breaker_fails"`
	BreakerUntil time.Time `json:"breaker_until,omitempty"`
}

// RateLimitedModel 单个被限流模型的台账行（issue #36）。
type RateLimitedModel struct {
	Model string `json:"model"`
	// Until 冷却到期时刻 = 该模型的独立冷却截止（modelCooldowns[m].Until，截断后），
	// 多模型限流时不再等于 Status.Until（账号级）。
	Until time.Time `json:"until,omitempty"`
	// ResetAt 上游「将在 … 重置」的原始墙钟（未经 soft_rate_max 截断）；未截断时
	// Until==ResetAt（两者同值）。截断/未截断都透出，台账始终可见上游权威时点。
	ResetAt time.Time `json:"reset_at,omitempty"`
	// Reason 触发原因（透出运维可读文案，同 Status.Reason）。
	Reason string `json:"reason,omitempty"`
}

// ModelCostStatus 单个 (账号, 模型) 的成本台账行（P1-anti-monopoly 可观测性）。
// tier 不单独落字段：cost_per_1k ≤ 0 即 tier 0（免费），> 0 即 tier 2（收费），
// 无观测即 tier 1——由调用方/面板按值推出，避免双表示漂移。
type ModelCostStatus struct {
	Model string `json:"model"`
	// CostPer1k 实测每千 token 单价（EMA 平滑值）。≤0 = 实测免费（tier 0）。
	CostPer1k float64 `json:"cost_per_1k"`
	// LastSeen 最近一次观测时刻（过期即从台账消失，同 modelCostTTL 口径）。
	LastSeen time.Time `json:"last_seen"`
	// Samples 累计观测次数（EMA 收敛度参考）。
	Samples int `json:"samples,omitempty"`
}

type entry struct {
	a       *auth.Auth
	credits int64
	// creditsExpiring 即将过期（签到时按 expiringSoon 窗口判定）的可用积分子集，
	// 是 credits 的一部分（credits = creditsExpiring + 长期积分）。选号权重对其
	// 额外加成：优先消耗快过期积分，避免官方活动赠送的奖励积分到期作废
	// （issue:积分过期）。持久化（stateAccount.CreditsExpiring）：重启后到下次
	// 签到之间第四因子（weightOf ×8）不应失忆——签到 09:00/21:00 定期刷新，
	// 窗口外重启会丢快过期积分偏好，可能让奖励积分到期作废。
	creditsExpiring int64
	successCount    int64     // 累计成功
	// errTotal 累计错误（终身累计，仅状态展示用；选号权重不消费——原「成功率」
	// 因子已删，见 pick.weightOf 注释与 success-ema-review）。
	errTotal    int64         // 累计错误（终身累计，供状态展示；选号权重不消费，原成功率因子已删）
	lastErr     time.Time     // 最近一次错误时间
	lastSuccess     time.Time // 最近一次成功时间
	coolKind        CoolKind
	until           time.Time // 冷却截止（即时冷却：CoolSoft 429 / CoolHard 余额耗尽）
	disabled        bool
	reason          string
	lastUsed        time.Time // 最近被选中时刻（防并发撞号）
	// usedSeq 单调递增的选中序号：每次被 pick 选中时取 p.pickSeq 自增值。
	// Windows 等平台 time.Now() 精度有限（~0.5ms），高并发/快速连续选号时多个
	// 账号 lastUsed 完全相等，基于 wall-clock 的 LRU/防惊群判定失效（高并发/低精度时钟下：
	// lastUsed 全等 → LRU Before 全 false → 恒选 candsAll[0] → 集中单号）。
	// usedSeq 提供严格全序，与时间精度无关。运行态，不持久化。
	usedSeq uint64
	// breakerUntil / fails / retryCount 为熔断器运行态。
	// breakerUntil + retryCount 持久化（stateAccount.BreakerUntil/RetryCount）：
	// breakerUntil 持久化以避免熔断期重启失忆（账号立即回到可选池再撞 5xx 雷区），
	// retryCount 持久化以保留"越熔越长"的退避累积（重启归零会失去累积保护）。
	// fails 不持久化——短期计数，重启从 0 累计可接受（达 breakerThreshold=3 才熔断）。
	// fails 是唯一的"连续失败"计数器：任何错误喂入，达到 breakerThreshold 触发熔断（指数退避），
	// 跨入口累计，成功/熔断/统一复活时清零（保留 retryCount 驱动退避指数）。
	breakerUntil time.Time // 熔断截止（指数退避）
	fails        int       // 连续失败计数（熔断用，唯一权威）
	retryCount   int       // 已熔断次数（指数退避的指数）
	// softStreak 连续软冷却次数（CoolSoft），独立于熔断器 fails 的**冷却域**计数器：
	// fails 会被熔断触发清零、且被 hard 冷却与 NoteError 污染，无法表达"连续软限流"。
	// 重置点只有两处（都是账号被证明恢复的时刻）：NoteSuccess、reviveCoolingLocked。
	// 持久化（stateAccount.SoftStreak）：重启后软限流仍在退避，不因重启回到基数。
	softStreak int
	// modelCooldowns 6004 模型级 limit 的**独立**冷却表：model → 该模型的冷却截止/重置。
	// 与 until（全账号级）正交：6004 只写本表、不写 until，因此多个模型同时 6004 时
	// 各自独立计时，互不覆盖（A 触发后 B 再触发，A 的冷却截止不被 B 覆盖——这是
	// 单 until 字段做不到的）。only 6004 触发时记录；空 map = 无模型级限流（不豁免）。
	// 持久化语义（stateAccount.ModelCooldowns）：重启后恢复，恢复时惰性过滤已过期
	// 条目。PR #96 把 6004 改成精确对齐上游重置墙钟后，单模型冷却可长达数小时，
	// 跨重启是常态；不持久化会导致 healthyForModel 重启失忆、重新踩 6004 雷区。
	modelCooldowns map[string]modelCooldown
	// sessionDeadFails 连续 12153（ErrSessionDead）计数。12153 在真实环境会被临时性触发
	// （网络抖动/上游闪断/refresh 竞态），一次失败就永久禁用太粗暴——连续达到阈值才判死。
	// 持久化（stateAccount.SessionDeadFails）：上游持续 session dead 时重启归零会导致
	// 重学（再吃 2 次失败才禁用，期间每次都白打一轮上游）；清零点（refresh/chat 成功、
	// 手工复活）同样落盘，重启后不残留旧计数。
	sessionDeadFails int
	// consecutiveFails 连续失败计数（连败降权，issue #114）——「不知道原因的兜底」：
	// 覆盖 ErrClient（未知 4xx）与传输层失败（连不上上游）这类 applyErrorPolicy
	// default 分支不罚号的形态。与 sessionDeadFails 同构但独立计数：12153 的终态
	// 是 Disable，这里的终态是临时出池（degradeUntil）。清零点：NoteSuccess。
	// 持久化（stateAccount.ConsecutiveFails + DegradeUntil）：restart 归零会让
	// 「上游持续故障 + 频繁重启」的组合重新学满 5 次；degradeUntil 持久化让降权期
	// 重启不失忆（与 breakerUntil 同口径）。
	consecutiveFails int
	// degradeUntil 连败降权截止：非零且未到期时该账号不参与 normal 选号（临时
	// 出池）。与冷却/熔断**取更长者不叠加**（healthy/degraded 判定统一取最远
	// 截止），到期自动回池，无需显式复位。落盘仅未过期条目（同 breakerUntil）。
	degradeUntil time.Time
	// inFlight 单账号在途请求数（运行态，不持久化）。用 atomic 避免 Pick 热路径拿写锁。
	inFlight atomic.Int64

	// modelCost 实测扣费账本：model → 观测。由每次成功请求的 usage.credit
	// 折算而来（上游没有"按模型的用量"接口，get-user-resource 只给套餐级积分
	// 汇总，只能实测）。选号时据此把「该模型上免费/便宜的号」排在前面。
	// 持久化（stateAccount.ModelCosts，P1-anti-monopoly）：重启后成本知识保留；
	// 落盘/恢复按 modelCostTTL 惰性过滤，陈旧观测不复活（同 modelCooldowns 口径）。
	modelCost map[string]modelCostEntry
}

// modelCostOf 返回该账号在指定 model 上的有效成本观测；无观测或观测过期返回 ok=false。
func (e *entry) modelCostOf(model string, now time.Time) (modelCostEntry, bool) {
	if model == "" {
		return modelCostEntry{}, false
	}
	mc, ok := e.modelCost[model]
	if !ok || mc.LastSeen.IsZero() {
		return modelCostEntry{}, false
	}
	if now.Sub(mc.LastSeen) > modelCostTTL {
		return modelCostEntry{}, false // 过期：时段性优惠（夜间免费）不得跨时段生效
	}
	return mc, true
}

// healthy 报告账号当前是否可选（未禁用、未处于任一冷却/熔断/连败降权期）。
// 连败降权与冷却/熔断同入本判定（取更长者不叠加：三个截止是并列的或门，
// 只要任一未到期即不可选，天然「并存取更远者」——不需要显式比较长短）。
func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		return false
	}
	if !e.degradeUntil.IsZero() && now.Before(e.degradeUntil) {
		return false
	}
	return true
}

// modelExempt 报告账号是否处于「6004 模型级软冷却」形态：存在任一有效的 6004
// 模型级冷却（modelCooldowns 非空），且尚未禁用、未熔断。
// 此形态下账号仅对限流中的模型不可用，对其他模型仍可选（issue #31）。
// 本谓词仅供探活侧使用（ServableNow/ServableForRealm）：/healthz 无请求模型
// 上下文，用「存在豁免形态」表达"该账号还有别的模型可服务"；
// chat 侧按请求模型细粒度判定（healthyForModel：全账号健康且该模型不在独立
// 冷却内才放行），探活存在性语义与选号在豁免账号上口径一致。
// 调用方负责 now 与冷却有效性的判断（本方法只看形态，不看冷却是否已过期）。
func (e *entry) modelExempt() bool {
	return len(e.modelCooldowns) > 0 &&
		!e.disabled && e.breakerUntil.IsZero()
}

// modelCooled 报告账号对指定 model 是否正处 6004 模型级冷却（该模型的独立冷却未过期）。
// 空 reqModel / 未记录 → false（不因模型级维度限制账号）。
func (e *entry) modelCooled(now time.Time, reqModel string) bool {
	if reqModel == "" {
		return false
	}
	mc, ok := e.modelCooldowns[reqModel]
	if !ok {
		return false
	}
	return !mc.Until.IsZero() && now.Before(mc.Until)
}

// healthyForModel 报告账号对指定 model 是否可选（含 6004 模型级独立冷却判定）。
//
// 优先级（全账号级先判，模型级后判）：
//   - 全账号不可用（disabled / 账号级 until / breakerUntil，见 healthy）→ 永不可选；
//     账号整体不可用时查该模型的独立冷却没有意义，直接短路返回 false。
//   - 仅全账号健康时，才查该模型是否正处 6004 独立冷却
//     （modelCooldowns[reqModel] 未过期）→ 不可选；
//   - 否则可选。
//
// 模型级维度只锁定触发模型：多模型同时 6004 时各自独立，被 B 限流的账号对 A 请求
// 仍可选（A 不在 modelCooldowns 拦截且账号级 healthy 成立）。空 reqModel /
// 未记录模型 → 等价 healthy。6004 从不写账号级 until（见 CooldownSoftForModel），
// 因此不存在「账号级冷却因病 6004 而起、应豁免其他模型」的形态。
func (e *entry) healthyForModel(now time.Time, reqModel string) bool {
	if !e.healthy(now) { // 全账号级（disabled/until/breakerUntil）先判
		return false
	}
	if e.modelCooled(now, reqModel) { // 全账号健康时再查该模型的 6004 独立冷却
		return false
	}
	return true
}

// pruneExpiredModelCosts 删除 modelCost 中已过期的条目（惰性清理，与
// pruneExpiredModelCooldowns 同形同调用点）。
//
// 为什么必须有它：modelCost 此前只在**读**（modelCostOf）、**落盘**（persist）、
// **恢复**（persist）、**status**（state.go）四处做"过期过滤"，内存里的条目本身
// 从不回收——即「map 只增不减」。而 entry.modelCost 的注释明确声称
// 「落盘/恢复按 modelCostTTL 惰性过滤，陈旧观测不复活（同 modelCooldowns 口径）」，
// 模型级冷却表正是靠 pruneExpiredModelCooldowns 在 pick 写锁路径做真正删除的
//（见 pick.go「map 不无限膨胀」）。两者口径不一致：一旦某模型的观测过期，它就会
// 永久占据一条内存（进程重启才清），并在后续每一轮 pick 的遍历、每次 status 遍历里
// 被反复判定为过期（只是没人删）。
//
// 观测只在成功请求路径写入（NoteModelCost），且 model 不与目录校验，故增长受
// "历史服务过的模型名"限制——不是无界泄漏，但同样是"只增不减"的无回收表。
// 调用方必须已持有 p.mu 写锁。
func (e *entry) pruneExpiredModelCosts(now time.Time) {
	if len(e.modelCost) == 0 {
		return
	}
	for m, mc := range e.modelCost {
		if mc.LastSeen.IsZero() || now.Sub(mc.LastSeen) > modelCostTTL {
			delete(e.modelCost, m)
		}
	}
}

// pruneExpiredModelCooldowns 删除 modelCooldowns 中已过期的条目（惰性清理）。
// pick 写锁路径与 revive 调用，防止 map 无限膨胀；status 只读遍历天然跳过过期项，
// 无需清理。调用方必须已持有 p.mu 写锁。
func (e *entry) pruneExpiredModelCooldowns(now time.Time) {
	if len(e.modelCooldowns) == 0 {
		return
	}
	for m, mc := range e.modelCooldowns {
		if mc.Until.IsZero() || !now.Before(mc.Until) {
			delete(e.modelCooldowns, m)
		}
	}
}

// expiry 返回账号当前仍在生效的最近冷却/熔断/降权截止时间（三个截止取最早者）；不在冷却期返回零值。
// 供全冷却兜底选取"最早到期"账号用。连败降权计入兜底口径：降权号参与兜底（其失败
// 形态是「不知道原因」，到期放行半开试探正是兜底语义——CoolHard 才被排除）。
func (e *entry) expiry(now time.Time) time.Time {
	var t time.Time
	if !e.until.IsZero() && now.Before(e.until) {
		t = e.until
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		if t.IsZero() || e.breakerUntil.Before(t) {
			t = e.breakerUntil
		}
	}
	if !e.degradeUntil.IsZero() && now.Before(e.degradeUntil) {
		if t.IsZero() || e.degradeUntil.Before(t) {
			t = e.degradeUntil
		}
	}
	return t
}

// fallbackKind 报告兜底账号属于哪一类冷却（soft：即时软冷却/连败降权；breaker：熔断期）。
// 只对参与兜底的账号调用（CoolHard 已被 pickEarliestExpiryLocked 排除）。判定口径：
// 若熔断截止是当前生效的最近截止（含"仅有熔断无软冷却"），记为 breaker；否则记为 soft
// （连败降权与软冷却同归 soft：都按各自截止到期放行，兜底处置无差异）。
func (e *entry) fallbackKind(now time.Time) string {
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		if e.until.IsZero() || !now.Before(e.until) || e.breakerUntil.Before(e.until) {
			return "breaker"
		}
	}
	return "soft"
}

// stateAccount 单个账号的持久化状态（JSON tag 全小写下划线，向后兼容：缺字段零值）。
type stateAccount struct {
	Credits      int64     `json:"credits"`
	Disabled     bool      `json:"disabled"`
	Reason       string    `json:"reason,omitempty"`
	Until        time.Time `json:"until,omitempty"`
	CoolKind     CoolKind  `json:"cool_kind"`
	SuccessCount int64     `json:"success_count,omitempty"`
	// err_total 累计错误计数。旧版 err_count（连续错误）仍可读：加载时映射到 err_total，
	// 仅作一次性迁移，不再回写 err_count。
	// 运维可见的运行态计数（err_total/soft_streak/session_dead_fails/credits_expiring）
	// 不用 omitempty：零值缺失会让人误以为"没记录"，实际是零值被省略。
	// 旧文件的 success_ema/error_ema 字段读取时被忽略（无害遗留，JSON 多余键
	// 自然丢弃，不报错不迁移——原成功率 EMA 因子已删，见 success-ema-review §4）。
	ErrTotal    int64     `json:"err_total"`
	ErrCount    int       `json:"err_count,omitempty"` // 兼容旧文件的迁移源，仅读取
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastErr     time.Time `json:"last_err,omitempty"`
	// SoftStreak 连续软冷却次数（软退避指数）。旧 state.json 缺此字段 → 零值，
	// 退避从基数重新开始（向后兼容）。
	SoftStreak int `json:"soft_streak"`
	// SessionDeadFails 连续 12153 计数（判定 session 死亡的进度）。持久化以保留
	// 「重启后连续计数继续累计」——上游持续 session dead 时重启归零会重学 2 次失败。
	// 零值也显式写出（运维口径，见 err_total 注释）。
	SessionDeadFails int `json:"session_dead_fails"`
	// ConsecutiveFails 连续失败计数（连败降权进度，见 entry.consecutiveFails）。
	// 零值也显式写出（运维口径，同上）。
	ConsecutiveFails int `json:"consecutive_fails"`
	// DegradeUntil 连败降权截止（issue #114）。仅未过期才持久化（落盘/恢复均惰性
	// 过滤），避免降权期重启失忆；过期/零值不写。指针语义同 BreakerUntil。
	DegradeUntil *time.Time `json:"degrade_until,omitempty"`

	// BreakerUntil 熔断截止（指数退避）。仅未过期才持久化（落盘/恢复均惰性过滤），
	// 避免熔断期重启失忆：breakerUntil 在未来时重启后仍阻断选号。过期/零值不写。
	// 用 *time.Time（而非 time.Time）：Go 的 omitempty 对非指针 time.Time 的零值
	// 不生效（会序列化成 0001-01-01T00:00:00Z）；指针 nil 才能真正被 omitempty 省略，
	// 与落盘"过期不写"的口径一致。
	BreakerUntil *time.Time `json:"breaker_until,omitempty"`
	// RetryCount 已熔断次数（指数退避的指数）。持久化以保留"越熔越长"的退避累积——
	// 重启归零会让反复熔断只从最小退避开始。仅在 BreakerUntil 未过期时才有意义，
	// 恢复时若 BreakerUntil 已过期则 retryCount 归零（不保留无用退避指数）。
	RetryCount int `json:"retry_count,omitempty"`
	// CreditsExpiring 快过期积分子集（credits 的子集）。持久化以保留第四因子
	// （weightOf ×8）的快过期积分偏好——重启后到下次签到之间不应失忆。
	// 零值也显式写出（运维口径，见 err_total 注释）。
	CreditsExpiring int64 `json:"credits_expiring"`
	// ModelCooldowns 6004 模型级独立冷却表（model → 冷却记录）。持久化：
	// PR #96 把 6004 改成精确对齐上游重置墙钟后，单模型冷却可长达数小时，
	// 跨重启是常态；不持久化导致每次重启 healthyForModel 失忆、重新踩一遍
	// 6004 雷区（选号撞限流号耗尽 MaxRotate → 429）。恢复时惰性过滤已过期条目。
	ModelCooldowns map[string]stateModelCooldown `json:"model_cooldowns,omitempty"`
	// ModelCosts 实测扣费账本（model → 单价观测，见 entry.modelCost）。持久化
	// （P1-anti-monopoly）：重启后成本知识保留，限免/夜间免费的跨重启窗口不再
	// 重新付学费探测。落盘/恢复均按 modelCostTTL 惰性过滤（6h 外不写不恢复——
	// 陈旧价格不复活）；恢复侧剔除非法值（负 per1k/零 LastSeen 的结构破损条目）。
	// 与运行态 modelCostEntry 字段一一对应（单一表示，内存与落盘同构不搞两套）。
	ModelCosts map[string]stateModelCost `json:"model_costs,omitempty"`
}

// stateModelCooldown 单个 (账号, 模型) 的 6004 独立冷却持久化记录，与运行态
// modelCooldown 同构（Until/ResetAt/Reason 字段名与语义对齐），落盘/恢复往返无损。
type stateModelCooldown struct {
	Until   time.Time `json:"until,omitempty"`
	ResetAt time.Time `json:"reset_at,omitempty"`
	Reason  string    `json:"reason,omitempty"`
}

// stateModelCost 单个 (账号, 模型) 的成本观测持久化记录，与运行态 modelCostEntry
// 字段一一对应（CostPer1k/LastSeen/Samples），落盘/恢复往返无损（EMA 值随
// JSON 浮点原样保留，往返不引入漂移）。
type stateModelCost struct {
	CostPer1k float64   `json:"cost_per_1k"`
	LastSeen  time.Time `json:"last_seen"`
	Samples   int       `json:"samples,omitempty"`
}

// modelCostTTL 成本观测的有效期。取 6 小时：既覆盖"夜间免费"这类时段性优惠的
// 单次会话，又不至于让昨天的价格决定今天的选择——过期的免费观测若永久有效，
// 白天会把已开始收费的号继续当成免费。
const modelCostTTL = 6 * time.Hour

// modelCostEntry 运行时成本账本（持久化镜像 stateModelCost 与其字段一一对应）。
type modelCostEntry struct {
	CostPer1k float64
	LastSeen  time.Time
	Samples   int
}

// modelCooldown 单个 (账号, 模型) 的模型级独立冷却记录。**已持久化**（stateAccount.ModelCooldowns
// → stateModelCooldown，见 2f4c77b）：Until/ResetAt/Reason 三字段落盘往返无损，Hits 不落盘
// （见字段注释）。本注释此前写「运行态，不持久化」，是 908abbd 引入本结构体时的旧状态描述，
// 在 2f4c77b 加上持久化后未同步更新，与上方 stateModelCooldown 的「落盘/恢复往返无损」自相矛盾。
// 承载两种「该模型在此账号上不可用」语义：
//   - 6004 模型级限流：Until 对齐上游重置墙钟；ResetAt 记录权威恢复时刻。
//   - 11102 该后端无此模型：Until 为指数退避 TTL（6h 起、封顶 24h）；Hits 记录
//     累计命中次数驱动退避（6004 无 hits 概念，Hits 恒 0）。
type modelCooldown struct {
	// Until 该模型的冷却截止（6004：now+min(resetAt-now, soft_rate_max)；11102：now+退避 TTL）。
	Until time.Time
	// ResetAt 上游「将在 … 重置」的原始墙钟（未经 soft_rate_max 截断）。
	// 与 Until 的区别同旧 softRateReset：Until 可能截断，ResetAt 是上游权威恢复时刻。
	// 11102 无重置文案，ResetAt 恒零值。
	ResetAt time.Time
	// Reason 触发原因（透出运维可读文案，同 Status.Reason）。
	Reason string
	// Hits 11102 负缓存的累计命中次数（驱动指数退避）。运行态不落盘（同 modelCost 口径：
	// 重启后从 6h 基数重新学习）；6004 条目 Hits 恒 0。持久化来回不会写入该字段。
	Hits int
}

// stateFile 持久化格式。
type stateFile struct {
	Accounts map[string]stateAccount `json:"accounts"`
}

// defaultBreaker* 熔断器默认参数（FreeBuff2API 参考口径）。
const (
	defaultBreakerThreshold   = 3
	defaultBreakerCooldown    = 30 * time.Minute
	defaultBreakerCooldownMax = 6 * time.Hour
)

// defaultSoftRateMax 软冷却指数退避的默认封顶：softRateMax 未注入（<=0）时按此值算，
// 避免测试/裸用池时退避无上限。
const defaultSoftRateMax = 2 * time.Hour

// 11102「该后端无此模型」负缓存的退避参数（吸收 model_blocks.py 语义，复用 modelCooldowns
// 机制承载）。首次命中冷却 6h，半开到期放行重试；再命中按 Hits 指数退避（×2^min(hits-1,6)），
// 封顶 24h（最多一天再试一次）；该模型请求成功即清。6004 限流不参与本退避（各自独立语义）。
const (
	modelBlockBaseTTL = 6 * time.Hour
	modelBlockMaxTTL  = 24 * time.Hour
	modelBlockShift   = 6 // 2^6=64 倍后封顶：6h×64>24h，实际封顶锚定 24h
)

// sessionDeadThreshold 连续 ErrSessionDead（12153）达到该次数才永久禁用。
// 12153 会被临时性触发（网络抖动/上游闪断/refresh 竞态），一次失败即禁用的旧行为
// 会误杀健康账号（P0-1：13 个 disabled 号全是误判）。3 次连续才判死：容忍偶发抖动，
// 又不会让真正的死 session 留在池里反复被选中。
const sessionDeadThreshold = 3

// 连败降权（issue #114「累计错误率高/连续失败 N 次的账号移出候选池一段时间」）
// 的默认参数，与熔断器参数族同风格（SetDegrade 注入，默认值在此）。
//   - defaultDegradeThreshold=5：比熔断阈值 3 宽——熔断管 5xx（ErrServer，确定性
//     上游故障），连败兜底管的是 ErrClient/传输层这类「不知道原因」的失败，判据
//     更弱，阈值须更保守以免误伤（偶发失败清零逻辑下，5 连败已是很强的异常信号）。
//   - defaultDegradeCooldown=10m：出池时长。取软冷却封顶（2h）与熔断基数（30m）
//     之间：长于单次软冷却（60s 级），短于熔断基数——连败的证据强度低于熔断，
//     惩罚不应重于熔断。
//   - defaultDegradeCooldownMax=2h：降权时长的**上限钳制**（非指数退避封顶——
//     连败降权为固定时长，见 degrade.go 注释「不做指数升级」），对齐
//     defaultSoftRateMax 的量级。仅当显式配置的 degrade_cooldown 大于该值时钳制。
const (
	defaultDegradeThreshold   = 5
	defaultDegradeCooldown    = 10 * time.Minute
	defaultDegradeCooldownMax = 2 * time.Hour
)

// sessionDeadReason 12153 判定为 session 死亡时的持久化 reason。
const sessionDeadReason = "12153 session dead"

// SessionDeadThreshold 暴露连续 12153 的禁用阈值（供 scheduler 日志/运维文档引用）。
func SessionDeadThreshold() int { return sessionDeadThreshold }

// softStreakShiftMax 软冷却退避的最大左移位数（防 1<<streak 溢出成负数/零）。
// 无论 streak 累积多少，封顶逻辑总会先生效，此值只是溢出兜底。
const softStreakShiftMax = 16

// defaultIdle* 闲置补偿默认参数（claude-api selectWeightedRandom 参考口径）。
const (
	defaultIdleWeightPerHour = 0.5
	defaultIdleWeightMax     = 5.0
)
