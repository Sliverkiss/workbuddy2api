// Package pool 账号池：单一状态机（健康/冷却/熔断）+ 在途租约 + 三因子加权挑选 + state.json 持久化。
//
// 每个账号只有三个正交状态维度：
//  1. 健康维度（唯一权威）：healthy = !disabled && !until 生效 && !breakerUntil 生效
//     - until：按错误类型的即时冷却（CoolSoft 429 / CoolHard 余额耗尽）
//     - breakerUntil：连续失败（fails）累计触发熔断的指数退避截止
//  2. 并发维度：inFlight（在途租约，运行态）
//  3. 统计维度：successCount / errTotal（累计，供成功率权重）/ lastUsed / lastSuccess / lastErr
//
// 挑选策略：healthy 账号中按三因子权重（credits 占比 ×10 + 闲置补偿 + 成功率 ×3）取 Top5，
// 再在 Top5 内按同一三因子权重加权随机（credits 只是权重的一个因子，不单独决定短名单）。
package pool

import (
	"encoding/json"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
)

// CoolKind 冷却类型。
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

	// inFlight 单账号在途请求数（运行态，不持久化）。用 atomic 避免 Pick 热路径拿写锁。
	inFlight atomic.Int64
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
}

// stateFile 持久化格式。
type stateFile struct {
	Accounts map[string]stateAccount `json:"accounts"`
}

// flushInterval 后台落盘周期。
var flushInterval = 5 * time.Second

// persistLogEvery 连续落盘失败每 N 次打一条提醒（flusher 5s 一把 ≈ 1 分钟一次），
// 避免磁盘持续满/权限丢失时日志刷屏。
const persistLogEvery = 12

// snapshot 池状态快照（Redis 镜像用）。与本地 state.json 同源（stateFile），
// 额外带 savedAt 时间戳供"择新恢复"（比较本地与 Redis 快照的新旧）。
type snapshot struct {
	stateFile
	SavedAt time.Time `json:"saved_at"`
}

// Pool 账号池。
type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
	dirty   atomic.Bool // 内存有变更待落盘

	// store 池状态快照镜像（redisstore.Store）；nil = 无需镜像（未配置 Redis / Noop 之外也可能 nil）。
	// SaveState/LoadState 经它接线，与本地 state.json 并存作启动恢复备份。
	store StoreSnapshotter

	// 熔断器调优（SetBreaker 注入；默认值见 defaultBreaker*）。
	breakerThreshold   int
	breakerCooldown    time.Duration
	breakerCooldownMax time.Duration

	// softRateMax 软冷却指数退避的封顶（SetSoftRateMax 注入；默认 defaultSoftRateMax）。
	softRateMax time.Duration

	// 三因子加权调优（SetWeights 注入；默认值见 defaultIdle*）。
	idleWeightPerHour float64
	idleWeightMax     float64

	// maxInFlight 单账号最大在途请求数；0 = 不限（租约关闭）。
	maxInFlight int

	// randInt64N 仅供测试注入确定性随机源；nil 时用 math/rand/v2 全局源。
	// 生产代码不应设置此字段。
	randInt64N func(n int64) int64

	// persistFails 本地 state.json 连续落盘失败计数（仅 saveLocked 在持锁下读写，无需 atomic）。
	// 用于落盘失败的日志节流：首败/每 N 次提醒/恢复各打一条，避免磁盘满时刷屏。
	persistFails int
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

// softStreakShiftMax 软冷却退避的最大左移位数（防 1<<streak 溢出成负数/零）。
// 无论 streak 累积多少，封顶逻辑总会先生效，此值只是溢出兜底。
const softStreakShiftMax = 16

// StoreSnapshotter 池状态快照镜像的最小接口（redisstore.Store 满足；Noop 空实现安全）。
// 与本地 state.json 并存，作启动恢复备份：快照比本地新才采用，否则本地优先。
type StoreSnapshotter interface {
	SaveState(data []byte)
	LoadState() ([]byte, bool)
}

// defaultIdle* 闲置补偿默认参数（claude-api selectWeightedRandom 参考口径）。
const (
	defaultIdleWeightPerHour = 0.5
	defaultIdleWeightMax     = 5.0
)

// New 构建池；stateFp 非空时尝试加载旧状态，并启动后台周期性落盘 goroutine。
func New(stateFp string) *Pool {
	p := &Pool{
		byUID:              map[string]*entry{},
		stateFp:            stateFp,
		breakerThreshold:   defaultBreakerThreshold,
		breakerCooldown:    defaultBreakerCooldown,
		breakerCooldownMax: defaultBreakerCooldownMax,
		idleWeightPerHour:  defaultIdleWeightPerHour,
		idleWeightMax:      defaultIdleWeightMax,
	}
	if stateFp != "" {
		p.load()
		p.startFlusher()
	}
	return p
}

// SetBreaker 注入熔断器参数（main 从 config 解析后调用）。非正值保留原值（用默认）。
func (p *Pool) SetBreaker(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.breakerThreshold = threshold
	}
	if cooldown > 0 {
		p.breakerCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.breakerCooldownMax = cooldownMax
	}
}

// SetSoftRateMax 注入软冷却指数退避的封顶时长（main 从 config 解析后调用）。
// 非正值保留原值（用默认 2h），风格同 SetBreaker。
func (p *Pool) SetSoftRateMax(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d > 0 {
		p.softRateMax = d
	}
}

// SetWeights 注入三因子加权的闲置补偿参数。非正值保留原值（用默认）。
func (p *Pool) SetWeights(idlePerHour, idleMax float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idlePerHour > 0 {
		p.idleWeightPerHour = idlePerHour
	}
	if idleMax > 0 {
		p.idleWeightMax = idleMax
	}
}

// SetMaxInFlight 注入单账号最大在途请求数；0 = 不限。负值保留原值。
func (p *Pool) SetMaxInFlight(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlight = n
	}
}

// SetStore 注入池状态快照镜像（redisstore.Store）。nil 表示不镜像（纯本地恢复）。
// 必须在 SyncToDir 之前调用，使"择新恢复"发生在账号对齐之前。
func (p *Pool) SetStore(s StoreSnapshotter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store = s
}

// RestoreFromSnapshot 择新恢复：比较本地 state.json 与 Redis 快照，采用较新者。
// 无快照、快照无 savedAt、或本地不存在/不可读时，都会被判定为"本地优先/跳过快照"，
// 同时打一条恢复来源日志。必须在 SyncToDir 之前调用（SyncToDir 只增删不入值）。
func (p *Pool) RestoreFromSnapshot() {
	store := p.store
	if store == nil || p.stateFp == "" {
		return
	}
	localInfo, localErr := os.Stat(p.stateFp)
	raw, ok := store.LoadState()
	if !ok {
		if localErr == nil {
			log.Printf("pool: 恢复来源=本地 state.json（无 Redis 快照）")
		}
		return
	}
	var snap snapshot
	if json.Unmarshal(raw, &snap) != nil || snap.SavedAt.IsZero() {
		// 快照无 savedAt：无法比较新旧，本地优先。
		log.Printf("pool: 恢复来源=本地 state.json（Redis 快照无 saved_at）")
		return
	}
	if localErr == nil && !localInfo.ModTime().After(snap.SavedAt) {
		// 快照不早于本地 → 采用快照。
		p.mu.Lock()
		p.applySnapshotLocked(snap)
		p.mu.Unlock()
		p.dirty.Store(true)
		log.Printf("pool: 恢复来源=Redis 快照 (saved_at=%s)", snap.SavedAt.Format(time.RFC3339))
		return
	}
	log.Printf("pool: 恢复来源=本地 state.json（较新于 Redis 快照 %s）", snap.SavedAt.Format(time.RFC3339))
}

// Acquire 为账号占一个在途名额；false 表示该账号已达上限（或不存在）。
// 必须在成功 Pick 后调用；调用方负责 defer Release。
func (p *Pool) Acquire(uid string) bool {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	limit := p.maxInFlight
	p.mu.RUnlock()
	if !ok {
		return false
	}
	if limit <= 0 {
		// 不限：计数仍累加（供状态观测），但永不拒绝。
		e.inFlight.Add(1)
		return true
	}
	for {
		cur := e.inFlight.Load()
		if cur >= int64(limit) {
			return false
		}
		if e.inFlight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Release 释放一个在途名额。幂等减到 0 为止（防重复释放扣成负数）。
func (p *Pool) Release(uid string) {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	p.mu.RUnlock()
	if !ok {
		return
	}
	for {
		cur := e.inFlight.Load()
		if cur <= 0 {
			return
		}
		if e.inFlight.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// SetRandomSource 仅供测试注入确定性随机源；生产代码不应调用。
// 注入源取 n∈[0,n) 后，pickWeighted 的抽签结果完全可预测。
func (p *Pool) SetRandomSource(fn func(n int64) int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.randInt64N = fn
}

// startFlusher 每 flushInterval 检查 dirty 标志，有变更则 saveLocked 落盘。
func (p *Pool) startFlusher() {
	interval := flushInterval // 在启动 goroutine 前同步读取，避免与测试对 flushInterval 的恢复写竞争
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			p.mu.Lock()
			if p.dirty.Swap(false) {
				p.saveLocked()
			}
			p.mu.Unlock()
		}
	}()
}

// Flush 同步把内存状态落盘（幂等：无变更不写盘）。供进程退出前调用。
func (p *Pool) Flush() {
	p.mu.Lock()
	if p.dirty.Swap(false) {
		p.saveLocked()
	}
	p.mu.Unlock()
}

// Add 加入账号；已存在则保留原状态、更新凭证（upsert 单账号，不影响其他账号）。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upsertLocked(a)
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
// 剔除结果持久化回 state.json，避免已删账号在下次启动时被 load() 复活。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[string]bool, len(auths))
	for _, a := range auths {
		seen[a.UID] = true
		p.upsertLocked(a)
	}
	changed := false
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
			changed = true
		}
	}
	if changed {
		p.saveLocked()
	}
}

// upsertLocked 更新或插入单个账号；已存在则只换凭证、保留 credits/cooling 状态。
// 调用方必须已持有 p.mu；Add 与 SyncToDir 共用此 upsert 逻辑。
func (p *Pool) upsertLocked(a *auth.Auth) {
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// Pick 返回 healthy 中积分最高的账号；无可用返回 nil。
func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
// 挑选策略：healthy 账号中按三因子权重取前 5 名，再在 Top5 内按同一权重加权随机抽签，
// 意图是打散热点，避免永远打同一个账号。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	return p.pick(tried)
}

// pick 在 healthy 候选集中按三因子权重加权随机选出账号，并记录 lastUsed（防并发撞号）。
// 候选集是 top5 近似：先按三因子权重（weightOf）降序取前 5（credits 只是权重的一个因子，
// 闲置补偿与成功率同样决定谁进短名单），再在 top5 内做防撞号过滤。
// 并发防雪崩：跳过 lastUsed 距今 < minPickGap 的账号（除非 top5 全部刚被用过，
// 此时退回最近最少使用 LRU 账号），迫使高并发请求发散，而不是全部撞同一高分账号。
// minPickGap=0（测试用）时过滤恒通过，退化为纯加权随机。
func (p *Pool) pick(tried map[string]bool) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue // 在途占满：跳过（max=0 不限时不触发）
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		// 全冷却兜底：无 healthy 候选时，从冷却账号里选 until 最早到期的一个
		// （熔断/冷却共用 expiry 口径，取较早截止者）。禁用的账号永不参与兜底。
		return p.pickEarliestExpiryLocked(tried, now)
	}
	// top5 短名单按三因子权重降序截断（而非 credits 单纯降序）：否则闲置补偿 + 成功率
	// 根本进不了短名单决策，低 credits 但高成功率/久置的账号会永远排不进 top5。
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	// 权重只算一次：顶 5 截断要排序，若在 sort 比较器里现算 weightOf 会翻成 O(n log n) 次
	// 冗余浮点计算（46 账号约 500 次）。先做 O(n) 预计算，再按 (权重, uid) 排序。
	type weighted struct {
		e *entry
		w float64
	}
	ws := make([]weighted, len(cands))
	for i, e := range cands {
		ws[i] = weighted{e: e, w: p.weightOf(e, maxCredits, now)}
	}
	sort.Slice(ws, func(i, j int) bool {
		if ws[i].w != ws[j].w {
			return ws[i].w > ws[j].w
		}
		return ws[i].e.a.UID < ws[j].e.a.UID
	})
	cands = cands[:0]
	for _, c := range ws {
		cands = append(cands, c.e)
	}
	if len(cands) > 5 {
		cands = cands[:5]
	}

	eligible := make([]*entry, 0, len(cands))
	for _, e := range cands {
		if now.Sub(e.lastUsed) >= minPickGap {
			eligible = append(eligible, e)
		}
	}
	var e *entry
	if len(eligible) == 0 {
		// top5 全部刚被用过：LRU 兜底，维持发散且不 starve 任一候选。
		e = cands[0]
		for _, c := range cands[1:] {
			if c.lastUsed.Before(e.lastUsed) {
				e = c
			}
		}
	} else {
		e = p.pickWeighted(eligible) // eligible 保序 = top5 降序子集
	}
	e.lastUsed = time.Now()
	return e.a
}

// pickEarliestExpiryLocked 全冷却兜底：在非禁用的软冷却/熔断账号中选截止最早的一个。
// 分级：disabled 永不参与；CoolHard（余额耗尽，等签到的号）同样排除——调了必 402，浪费轮换并产生噪音日志；
// CoolSoft 与熔断号允许参与（可能已恢复，失败成本仅一轮换）。
// 被 tried 排除、在途占满的账号同样跳过（维持请求级轮换 + 租约语义）。无任何可用返回 nil。
func (p *Pool) pickEarliestExpiryLocked(tried map[string]bool, now time.Time) *auth.Auth {
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if e.disabled {
			continue // 禁用的账号永不参与兜底
		}
		if e.coolKind == CoolHard && !e.until.IsZero() && now.Before(e.until) {
			continue // 余额耗尽号（处于有效 hard 冷却期）不参与兜底：等签到恢复，调了必 402
		}
		if p.inFlightFull(e) {
			continue
		}
		exp := e.expiry(now)
		if exp.IsZero() {
			continue
		}
		if best == nil || exp.Before(best.expiry(now)) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	log.Printf("pool: fallback_earliest_expiry uid=%s until=%s kind=%s", best.a.UID, best.expiry(now).Format(time.RFC3339), best.fallbackKind(now))
	best.lastUsed = time.Now()
	return best.a
}

// inFlightFull 报告账号是否已占满在途名额（max=0 不限 → 恒 false）。
// 调用方需已持 p.mu（读锁或写锁均可，本方法只读 p.maxInFlight）。
func (p *Pool) inFlightFull(e *entry) bool {
	if p.maxInFlight <= 0 {
		return false
	}
	return e.inFlight.Load() >= int64(p.maxInFlight)
}

// minPickGap 防并发撞号窗口：同一账号在该窗口内不重复被选中（除非 top5 全部刚被用过）。
// 生产默认 100ms；纯加权分布测试可临时置 0 关闭防撞号。
var minPickGap = 100 * time.Millisecond

// pickWeighted 三因子加权随机（claude-api selectWeightedRandom 参考口径）：
//
//		weight = credits 比例 × 10 + idleWeight + successRate × 3
//
//	  - credits 比例 = 该号 credits / 候选集内最大 credits（避免量纲爆炸）
//	  - idleWeight = min(距 lastUsed 小时数 × idleWeightPerHour, idleWeightMax)；从未使用给满分
//	  - successRate = successCount/(successCount+errTotal)；无请求记录给 1.5（中性偏信任）
//
// credits 全 0 时仍按 idle+successRate 加权（不退化均匀随机）。
// 权重为浮点，用 int64 定点（×1e6）抽签可保持确定性随机源注入（randInt64N 语义不变）。
// 随机源优先用 p.randInt64N（仅供测试注入确定性），nil 时回退 math/rand/v2 全局源。
func (p *Pool) pickWeighted(cands []*entry) *entry {
	now := time.Now()
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}

	const scale = 1_000_000 // 定点放大：int64 累加权重大整数抽签
	weights := make([]int64, len(cands))
	var total int64
	for i, e := range cands {
		w := p.weightOf(e, maxCredits, now)
		weights[i] = int64(w * scale)
		total += weights[i]
	}

	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	if total <= 0 {
		return cands[int(rnd(int64(len(cands))))]
	}
	r := rnd(total)
	var acc int64
	for i, e := range cands {
		acc += weights[i]
		if r < acc {
			return e
		}
	}
	return cands[len(cands)-1]
}

// weightOf 计算单个账号的三因子权重。
func (p *Pool) weightOf(e *entry, maxCredits int64, now time.Time) float64 {
	w := 1.0

	// 1. credits 比例 ×10（会计入 mid-credit 锚点，避免全员 0 时 credits 项为 0）。
	if maxCredits > 0 {
		w += float64(e.credits) / float64(maxCredits) * 10
	}

	// 2. 闲置补偿。
	if e.lastUsed.IsZero() {
		w += p.idleWeightMax // 从未使用 → 满分
	} else {
		hours := now.Sub(e.lastUsed).Hours()
		idleW := hours * p.idleWeightPerHour
		if idleW > p.idleWeightMax {
			idleW = p.idleWeightMax
		}
		if idleW < 0 {
			idleW = 0 // lastUsed 在未来（时钟回拨）时钳 0
		}
		w += idleW
	}

	// 3. 成功率 ×3。
	totalReq := e.successCount + e.errTotal
	if totalReq > 0 {
		w += float64(e.successCount) / float64(totalReq) * 3
	} else {
		w += 1.5 // 无请求记录 → 中性偏信任
	}
	return w
}

// SetCredits 更新账号余额。
func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
		p.dirty.Store(true)
	}
}

// Cooldown 冷却账号至 now+d（即时冷却：CoolSoft 429 / CoolHard 余额耗尽）。
// 冷却入口同时是熔断器的失败信号：喂入 fails，达到阈值按指数退避熔断（与 until 正交）。
//
// CoolSoft 额外做**连续软限流指数退避**：同一账号连续触发软冷却时，实际时长按
// d << (softStreak-1) 逐次翻倍（封顶 softRateMax，见 softDurationLocked）。
// 首 streak=1 → 实际时长 = d，单次调用语义与旧行为一致。
// 这就是与熔断器并存的双重升级，且是**有意为之**：软退避管"近期被限流"，
// 在 600s 起按分钟~小时级放大；熔断管"病态反复失败"，按 30m→1h→2h→6h 长期封禁。
// 二者喂入路径共用本入口但计数器独立（softStreak vs fails），互不污染。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if kind == CoolSoft {
			e.softStreak++
			d = p.softDurationLocked(d, e.softStreak)
		}
		e.until = time.Now().Add(d)
		e.coolKind = kind
		e.reason = reason
		p.recordBreakerFailureLocked(e) // 冷却入口也是熔断器的失败信号
		p.dirty.Store(true)
	}
}

// softDurationLocked 按连续软冷却次数把基数 d 指数放大：d << (streak-1)，封顶 softRateMax。
// softRateMax 未注入（<=0）时按 defaultSoftRateMax 算。streak<=1 时原样返回 d。
// 左移位数受 softStreakShiftMax 限制，避免 streak 极大时移位溢出。
// 调用方必须已持有 p.mu。
func (p *Pool) softDurationLocked(d time.Duration, streak int) time.Duration {
	if streak <= 1 {
		return d
	}
	shift := streak - 1
	if shift > softStreakShiftMax {
		shift = softStreakShiftMax
	}
	d <<= shift
	max := p.softRateMax
	if max <= 0 {
		max = defaultSoftRateMax
	}
	if d > max || d <= 0 { // d<=0：左移溢出成负数/零，同样按封顶兜底
		d = max
	}
	return d
}

// recordBreakerFailureLocked 累计一次熔断失败；达到阈值则按指数退避熔断。
// 熔断与冷却（until）解耦：冷却按错误类别给固定时长，熔断则对"反复失败"逐次加长封禁。
// 调用方必须已持有 p.mu。
func (p *Pool) recordBreakerFailureLocked(e *entry) {
	e.fails++
	if e.fails < p.breakerThreshold {
		return
	}
	d := p.breakerCooldown
	for i := 0; i < e.retryCount; i++ {
		d *= 2
		if d >= p.breakerCooldownMax {
			d = p.breakerCooldownMax
			break
		}
	}
	// 触发熔断：重置失败计数供下一轮重新累计；retryCount 递增放大退避指数。
	e.fails = 0
	e.retryCount++
	e.breakerUntil = time.Now().Add(d)
}

// CooldownUntilTomorrow4AM 冷却到下一个 04:00（本地时区）。
// 用于 ErrHardCredit 场景：积分耗尽账号等签到任务（09:00/21:00）恢复。
func (p *Pool) CooldownUntilTomorrow4AM(uid string, reason string) {
	now := time.Now()
	p.Cooldown(uid, CoolHard, nextDay4AM(now).Sub(now), reason)
}

// nextDay4AM 返回 now 之后最近的一个 04:00（与 now 同一时区）。
// now 在当天 04:00 之前（凌晨 00:00~04:00）时返回当天 04:00——此时签到尚未执行，
// 该窗内触发的硬冷却等当天签到即可恢复；返回次日会白冷约一天。
// 04:00 整及之后返回次日 04:00。
// time.Date 对日溢出自动进位（月末→下月 1 号、年末→下年 1 号），天然覆盖跨日/跨月/跨年。
func nextDay4AM(now time.Time) time.Time {
	if now.Hour() < 4 {
		return time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	}
	return time.Date(now.Year(), now.Month(), now.Day()+1, 4, 0, 0, 0, now.Location())
}

// Disable 永久禁用（session 死亡），需人工重登后手工恢复或文件替换。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
		p.dirty.Store(true)
	}
}

// reviveCoolingLocked 只清冷却（until/coolKind/reason/softStreak）并更新 credits，不动熔断器
// （fails/retryCount/breakerUntil）。签到解冻走这里：签到成功只证明余额恢复与
// billing 通道健康，不证明 chat 通道健康，熔断（连续 5xx 信号）不应被签到覆盖。
// softStreak 属**冷却域**（与 until/coolKind 同域），故随冷却一并清零——与"解冻只清冷却
// 不清熔断"的既有 C5 语义一致；硬冷却（CoolHard）本就不参与 streak，这里清的是历史软冷却累积。
// 调用方必须已持有 p.mu。
func (p *Pool) reviveCoolingLocked(e *entry, credits int64) {
	e.credits = credits
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号非禁用时，清冷却（余额恢复）。
// 注意：不碰熔断器——熔断到期（breakerUntil 过期）或下次 chat 成功（NoteSuccess）才恢复。
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

// NoteSuccess 成功请求累加成功计数、刷新 lastSuccess，并清空连续失败与熔断运行态。
// 二进制模型：清 fails + retryCount + breakerUntil；不碰 until/coolKind（那些是即时冷却，各自到期）。
// 额外清 softStreak：成功是账号已恢复的最强证据，连续软限流计数就此归零、退避回到基数。
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

// ServableNow 报告池当前是否可服务：存在至少一个 healthy 且未占满在途名额的账号。
// 与 CountsDetailed 的 healthy 口径不同：healthy 只看 disabled/until/breakerUntil（状态机权威判定），
// 不看 inFlight；ServableNow 额外叠加在途维度，与 chat 的真实可达性（Pick 会跳过 inFlightFull 账号）对齐。
// 专供 /healthz 用，避免"全账号 healthy 但都占满"时探活误报 200 而 chat 返回 503 的口径裂缝。
func (p *Pool) ServableNow() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if e.healthy(now) && !p.inFlightFull(e) {
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

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	p.applyAccountsLocked(sf.Accounts)
}

// applyAccountsLocked 用持久化账号状态覆盖/插入 byUID（placeholder 凭证，Add 时换全）。
// 本地 load() 与 Redis 快照恢复共用；调用方必须已持有 p.mu。
func (p *Pool) applyAccountsLocked(accounts map[string]stateAccount) {
	for uid, s := range accounts {
		// err_total 优先；旧文件的 err_count（连续错误）作一次性迁移源映射进来（二者取较大者，
		// 尽最大可能保留历史观测信号——旧语义下 err_count 也真实发生过错误，不应丢）。
		errTotal := s.ErrTotal
		if int64(s.ErrCount) > errTotal {
			errTotal = int64(s.ErrCount)
		}
		p.byUID[uid] = &entry{
			a:            &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:      s.Credits,
			disabled:     s.Disabled,
			reason:       s.Reason,
			until:        s.Until,
			coolKind:     s.CoolKind,
			successCount: s.SuccessCount,
			errTotal:     errTotal,
			lastErr:      s.LastErr,
			lastSuccess:  s.LastSuccess,
			softStreak:   s.SoftStreak,
		}
	}
}

// applySnapshotLocked 用 Redis 快照覆盖内存状态（已在择新判定后采用）。调用方必须已持有 p.mu。
func (p *Pool) applySnapshotLocked(s snapshot) {
	p.byUID = map[string]*entry{}
	p.applyAccountsLocked(s.Accounts)
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := p.stateOverviewLocked()
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		p.notePersistFail(err)
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		p.notePersistFail(err)
		return
	}
	if err := os.Rename(tmp, p.stateFp); err != nil {
		p.notePersistFail(err)
		return
	}
	if p.persistFails > 0 {
		// 从连续失败中恢复：打一条恢复日志，避免"错误打完却无人知道已恢复"。
		log.Printf("pool: state.json 落盘恢复（此前连续失败 %d 次）", p.persistFails)
		p.persistFails = 0
	}

	// 同步镜像一份快照到 Redis（fire-and-forget），与本地 state.json 并存作恢复备份。
	if p.store != nil {
		snapRaw, err := json.Marshal(snapshot{stateFile: sf, SavedAt: time.Now()})
		if err == nil {
			p.store.SaveState(snapRaw)
		}
	}
}

// notePersistFail 记录一次本地 state.json 落盘失败，并按节流规则决定是否打日志：
// 首败（状态成功→失败）打完整错误、每 persistLogEvery 次连续失败打一条提醒、
// 其余连续失败静默（flusher 5s 一把，磁盘持续满时不刷屏）。
// 恢复成功的日志由 saveLocked 在成功路径统一打。与 redisstore 三处异步写的
// "失败仅打日志、不向上抛"范式对齐，但落盘失败对运维是盲区，故多一层节流（notification）。
func (p *Pool) notePersistFail(err error) {
	if p.persistFails == 0 {
		log.Printf("pool: state.json 落盘失败: %v", err)
	} else if p.persistFails%persistLogEvery == 0 {
		log.Printf("pool: state.json 连续落盘失败 %d 次: %v", p.persistFails, err)
	}
	p.persistFails++
}

// stateOverviewLocked 收集当前内存状态为 stateFile（供落盘 + 快照镜像复用）。调用方必须已持 p.mu。
func (p *Pool) stateOverviewLocked() stateFile {
	sf := stateFile{Accounts: map[string]stateAccount{}}
	for uid, e := range p.byUID {
		sf.Accounts[uid] = stateAccount{
			Credits:      e.credits,
			Disabled:     e.disabled,
			Reason:       e.reason,
			Until:        e.until,
			CoolKind:     e.coolKind,
			SuccessCount: e.successCount,
			ErrTotal:     e.errTotal,
			LastSuccess:  e.lastSuccess,
			LastErr:      e.lastErr,
			SoftStreak:   e.softStreak,
		}
	}
	return sf
}
