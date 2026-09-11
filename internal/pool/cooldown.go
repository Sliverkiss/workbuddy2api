// 冷却与熔断：Cooldown/CooldownSoftForModel（软退避指数升级）、软冷却封顶、
// 熔断失败累计、签到解冻（ReenableIfCredits/reviveCoolingLocked）。
package pool

import (
	"time"
)

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
		// 非模型级冷却入口：清空 6004 模型豁免痕迹，避免上一次模型级限流的
		// softRateModel 泄漏到本次**账号级**限流上（否则换模型请求会错误绕过本次冷却）。
		e.softRateModel = ""
		p.recordBreakerFailureLocked(e) // 冷却入口也是熔断器的失败信号
		p.dirty.Store(true)
	}
}

// CooldownSoftForModel 429 的**模型级**软冷却入口（issue #31）。
// 区别于 Cooldown：当上游 6004 明说「将在 … 重置」时，把冷却截止精确设为
// resetAt（上游给定时间，不再靠固定基数+指数退避猜测），并记录触发模型 softRateModel，
// 后续该模型被豁免冷却（切模型立即可用，见 healthyForModel）。
//
// 收窄规则：
//   - resetAt 非零（6004 带解析时间）→ until = min(resetAt, now+softRateMax)，
//     softRateModel = model。指数退避**不适用**：重置时间已是上游权威，再指数放大
//     会无视它明说的恢复时刻（这恰是本 issue 的核心痛点）。
//   - resetAt 零值（6004 无时间文案 / 非 6004 的 soft）→ 完全退回 Cooldown 现状
//     （soft_streak 指数退避 + 封顶 soft_rate_max），softRateModel 保持空（不豁免）。
//
// 熔断信号照旧喂入（冷却与熔断正交，行为与 Cooldown 一致）；softStreak 仍递增
// （无论是否命中解析时间），single 一致性由 Cooldown 之外的语义保证：解析时间的
// 冷却**不**参与指数退避，但 softStreak 计数照常累加，后续无时间的 6004 从当前
// streak 继续退避——与任务书「指数退避逻辑保持不变，只在两个点收窄」的口径一致。
func (p *Pool) CooldownSoftForModel(uid string, base time.Duration, resetAt time.Time, model, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.softStreak++
		hasReset := !resetAt.IsZero()
		d := p.softDurationLocked(base, e.softStreak)
		if hasReset {
			// 有上游重置时间：直接取 min(重置墙钟, now+softRateMax)，不做指数放大。
			now := time.Now()
			cap := now.Add(p.softRateMaxOr())
			if resetAt.After(cap) {
				d = cap.Sub(now)
			} else if resetAt.After(now) {
				d = resetAt.Sub(now)
			} else {
				// 重置时间已过（时钟偏移/文案过期）：冷却极短，立即恢复。
				d = time.Millisecond
			}
		}
		e.until = time.Now().Add(d)
		e.coolKind = CoolSoft
		e.reason = reason
		if hasReset {
			e.softRateModel = model // 仅带解析时间的 6004 才记录模型（豁免画界）
		} else {
			e.softRateModel = ""
		}
		p.recordBreakerFailureLocked(e) // 冷却入口也是熔断器的失败信号
		p.dirty.Store(true)
	}
}

// softRateMaxOr 返回生效的 softRateMax（未注入时按默认 2h），供封顶计算。
// 调用方必须已持有 p.mu。
func (p *Pool) softRateMaxOr() time.Duration {
	if p.softRateMax > 0 {
		return p.softRateMax
	}
	return defaultSoftRateMax
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
func (p *Pool) reviveCoolingLocked(e *entry, credits int64) {
	e.credits = credits
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	e.softRateModel = "" // 冷却域清零时一并清模型豁免痕迹
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号非禁用时，清冷却（余额恢复）。
// 注意：不碰熔断器——熔断到期（breakerUntil 过期）或下次 chat 成功（NoteSuccess）才恢复。
