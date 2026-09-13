// Package pool 账号池：单一状态机（健康/冷却/熔断）+ 在途租约 + 三因子加权挑选 + state.json 持久化。
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

// Status 单个账号对外暴露的状态（脱敏）。
type Status struct {
	UID             string    `json:"uid"`
	Nickname        string    `json:"nickname,omitempty"`
	Credits         int64     `json:"credits"`
	Cooling         bool      `json:"cooling"`
	CoolKind        string    `json:"cool_kind,omitempty"`
	CoolRemaining   int64     `json:"cool_remaining_sec,omitempty"`
	Until           time.Time `json:"until,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	SoftStreak      int       `json:"soft_streak,omitempty"` // 连续软冷却次数（指数退避指数，见 entry.softStreak）
	Disabled        bool      `json:"disabled"`
	DisabledReason  string    `json:"disabled_reason,omitempty"` // 仅 disabled 账号：禁用原因（运维可见）
	SuccessCount    int64     `json:"success_count,omitempty"`
	ErrTotal        int64     `json:"err_total,omitempty"`
	LastSuccessTime time.Time `json:"last_success,omitempty"`
	LastErrTime     time.Time `json:"last_err,omitempty"`
	// 运行态（不持久化）：在途请求数 + 熔断器状态。
	InFlight     int       `json:"in_flight"`
	BreakerFails int       `json:"breaker_fails"`
	BreakerUntil time.Time `json:"breaker_until,omitempty"`
}
type entry struct {
	a            *auth.Auth
	credits      int64
	successCount int64     // 累计成功
	errTotal     int64     // 累计错误（供成功率权重 successRate = successCount/(successCount+errTotal)，不清零）
	lastErr      time.Time // 最近一次错误时间
	lastSuccess  time.Time // 最近一次成功时间
	coolKind     CoolKind
	until        time.Time // 冷却截止（即时冷却：CoolSoft 429 / CoolHard 余额耗尽）
	disabled     bool
	reason       string
	lastUsed     time.Time // 最近被选中时刻（防并发撞号）
	// breakerUntil / fails / retryCount 为熔断器运行态（不持久化）。
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
	// softRateModel 触发 6004 模型级限流时的模型名（issue #31 模型豁免）。
	// 仅当冷却由「带解析时间的 6004」触发时记录；空 = 普通软冷却（不豁免）。
	// 运行态语义（不持久化）：重启清零，退化为现状。
	softRateModel string
	// sessionDeadFails 连续 12153（ErrSessionDead）计数。12153 在真实环境会被临时性触发
	// （网络抖动/上游闪断/refresh 竞态），一次失败就永久禁用太粗暴——连续达到阈值才判死。
	// 运行态语义（不持久化，与 inFlight 同语义）：重启清零可接受——重启后首个 keepalive
	// 成功即清计数，误判号不会因重启前的历史累积被继续追杀。
	sessionDeadFails int
	// inFlight 单账号在途请求数（运行态，不持久化）。用 atomic 避免 Pick 热路径拿写锁。
	inFlight atomic.Int64

	// modelCost 实测扣费账本：model → 观测（运行态，不持久化）。
	// 由每次成功请求的 usage.credit 折算而来（上游没有"按模型的用量"接口，
	// get-user-resource 只给套餐级积分汇总，只能实测）。选号时据此把
	// 「该模型上免费/便宜的号」排在前面。
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

// healthy 报告账号当前是否可选（未禁用、未处于任一冷却/熔断期）。
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
	return true
}

// modelExempt 报告账号是否处于「6004 模型级软冷却」形态：冷却由带解析时间的
// 6004 触发（coolKind==CoolSoft 且 softRateModel 非空），且尚未禁用、未熔断。
// 此形态下账号仅对 softRateModel 不可用，对其他模型仍可选（issue #31）。
// healthyForModel 与 ServableNow 共用本谓词，保证 chat 选号与探活口径一致。
// 调用方负责 now 与冷却有效性的判断（本方法只看形态，不看 until 是否已过）。
func (e *entry) modelExempt() bool {
	return e.coolKind == CoolSoft && e.softRateModel != "" &&
		!e.disabled && e.breakerUntil.IsZero()
}

// healthyForModel 报告账号对指定 model 是否可选（含模型级豁免）：
// 冷却为由 6004 触发的**模型级**软冷却（softRateModel 非空）且请求模型不同
// （softRateModel != reqModel）时，跳过冷却判定——该模型限流不代表账号在其他
// 模型下不可用（issue #31）。空 reqModel / 未记录模型 / 同模型 → 与 healthy 一致。
func (e *entry) healthyForModel(now time.Time, reqModel string) bool {
	if !e.healthy(now) && reqModel != "" && e.modelExempt() && e.softRateModel != reqModel {
		// 非 healthy 但属于可豁免场景：仍受 disabled/breakerUntil 约束（modelExempt 已含）。
		return true
	}
	return e.healthy(now)
}

// expiry 返回账号当前仍在生效的最近冷却/熔断截止时间（两个截止取较早者）；不在冷却期返回零值。
// 供全冷却兜底选取"最早到期"账号用。
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
	return t
}

// fallbackKind 报告兜底账号属于哪一类冷却（soft：即时软冷却；breaker：熔断期）。
// 只对参与兜底的账号调用（CoolHard 已被 pickEarliestExpiryLocked 排除）。判定口径：
// 若熔断截止是当前生效的最近截止（含"仅有熔断无软冷却"），记为 breaker；否则记为 soft。
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
	ErrTotal    int64     `json:"err_total,omitempty"`
	ErrCount    int       `json:"err_count,omitempty"` // 兼容旧文件的迁移源，仅读取
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastErr     time.Time `json:"last_err,omitempty"`
	// SoftStreak 连续软冷却次数（软退避指数）。旧 state.json 缺此字段 → 零值，
	// 退避从基数重新开始（向后兼容）。
	SoftStreak int `json:"soft_streak,omitempty"`

	// ModelCost 实测扣费账本（model → 观测）。仅内存态，重启后重新学习：
	// 成本会随上游活动（限免期/夜间免费/折扣）变化，持久化旧值反而是脏数据。
	ModelCost map[string]stateModelCost `json:"-"`
}

// stateModelCost 单个 (账号, 模型) 的实测成本观测。
type stateModelCost struct {
	// CostPer1k 每千 token 的 credit 消耗（EMA 平滑）。0 = 免费。
	// 用"每千 token"归一而非"单次 credit"：扣费随请求长度变化，
	// 不同长度的请求之间不可比。
	CostPer1k float64
	// LastSeen 最近观测时刻，超过 modelCostTTL 视为失效。
	LastSeen time.Time
	// Samples 观测次数（供排查）。
	Samples int
}

// modelCostTTL 成本观测的有效期。取 6 小时：既覆盖"夜间免费"这类时段性优惠的
// 单次会话，又不至于让昨天的价格决定今天的选择——过期的免费观测若永久有效，
// 白天会把已开始收费的号继续当成免费。
const modelCostTTL = 6 * time.Hour

// modelCostEntry 运行时成本账本（与 stateModelCost 同构，独立于持久化结构，
// 避免账本污染 state.json）。
type modelCostEntry struct {
	CostPer1k float64
	LastSeen  time.Time
	Samples   int
}

// stateFile 持久化格式。
type stateFile struct {
	Accounts map[string]stateAccount `json:"accounts"`
}

// flushInterval 后台落盘周期。
const (
	defaultBreakerThreshold   = 3
	defaultBreakerCooldown    = 30 * time.Minute
	defaultBreakerCooldownMax = 6 * time.Hour
)

// defaultSoftRateMax 软冷却指数退避的默认封顶：softRateMax 未注入（<=0）时按此值算，
// 避免测试/裸用池时退避无上限。
const defaultSoftRateMax = 2 * time.Hour

// sessionDeadThreshold 连续 ErrSessionDead（12153）达到该次数才永久禁用。
// 12153 会被临时性触发（网络抖动/上游闪断/refresh 竞态），一次失败即禁用的旧行为
// 会误杀健康账号（P0-1：13 个 disabled 号全是误判）。3 次连续才判死：容忍偶发抖动，
// 又不会让真正的死 session 留在池里反复被选中。
const sessionDeadThreshold = 3

// sessionDeadReason 12153 判定为 session 死亡时的持久化 reason。
const sessionDeadReason = "12153 session dead"

// SessionDeadThreshold 暴露连续 12153 的禁用阈值（供 scheduler 日志/运维文档引用）。
func SessionDeadThreshold() int { return sessionDeadThreshold }

// softStreakShiftMax 软冷却退避的最大左移位数（防 1<<streak 溢出成负数/零）。
// 无论 streak 累积多少，封顶逻辑总会先生效，此值只是溢出兜底。
const softStreakShiftMax = 16

// StoreSnapshotter 池状态快照镜像的最小接口（redisstore.Store 满足；Noop 空实现安全）。
// 与本地 state.json 并存，作启动恢复备份：快照比本地新才采用，否则本地优先。
const (
	defaultIdleWeightPerHour = 0.5
	defaultIdleWeightMax     = 5.0
)

// New 构建池；stateFp 非空时尝试加载旧状态，并启动后台周期性落盘 goroutine。
