// 隔离号池（quarantine）：频繁命中上游内容审核（moderation_blocked，「内容未通过
// 安全审核」等）的账号临时出池。与冷却/熔断/降权的本质区别——**静默到期不放行**：
// 到期只是获得「可被探活」资格，须由探活 prober（internal/scheduler）用良性请求实测，
// 通过（ReleaseQuarantine）才回选号池；仍被拦（QuarantineExtend）则继续静默并按
// ×2^hits 退避。这样被上游标记的号不会随每次静默到期反复污染号池。
//
// 入口约定（迁移矩阵，transition.go 风格）：
//
//	quarantined          ← NoteContentBlocked 达阈（连续 moderation_blocked 且期间无成功）
//	quarantined 清除      ← ReleaseQuarantine（探活通过）/ ReviveDisabled（运维复活）
//	quarantineUntil      ← 进隔离（base×2^hits）/ QuarantineExtend（探活仍拦，退避续默）
//	contentBlockedStreak ← NoteContentBlocked；NoteSuccess 清
//
// 正交性：与 disabled/manualDisabled 独立两位，互不清除（授权恢复不证明审核解除）；
// 冷却域（clearCoolingLocked）不动隔离字段。ExploreDisabled 复活见 ReviveDisabled。
package pool

import (
	"log"
	"sort"
	"time"

	"workbuddy2api/internal/logfmt"
)

// SetQuarantine 注入隔离参数（main 从 config 解析后调用）。非正值保留原值（用默认）。
//
// probeDelay 语义：进隔离到「首次可探活」的等待（默认 1m），**不是惩罚时长**——
// 惩罚时长是探活失败后的 silence 指数退避。这是「恶性错误一次即隔离」可行的前提：
// 内容级误伤（良性内容必过探活）只损失该号几分钟容量，不会被单项错误罚 1 小时。
func (p *Pool) SetQuarantine(threshold int, probeDelay, silence, silenceMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.quarantineThreshold = threshold
	}
	if probeDelay > 0 {
		p.quarantineProbeDelay = probeDelay
	}
	if silence > 0 {
		p.quarantineSilence = silence
	}
	if silenceMax > 0 {
		p.quarantineSilenceMax = silenceMax
	}
}

// SetQuarantineEnabled 隔离机制总开关（config quarantine.enabled，默认 true）。
// false 时 NoteContentBlocked 退化为空操作——已隔离的账号**保持隔离**（既有状态
// 不因关停翻转而清空，避免"关一下开一下"重置隔离期），但不会再有新号进隔离；
// 探活 loop 由 main 侧同时关停，此时隔离号只能经 ReviveDisabled 手工放行。
func (p *Pool) SetQuarantineEnabled(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.quarantineOn = on
}

// NoteContentBlocked 记录一次内容审核错误（moderation_blocked）。内容是**恶性错误**：
// 账号一旦被判提权审核，经它的一切请求都可能被拦（用户感知为"这个模型坏了"），
// 故默认阈值 = 1（defaultQuarantineThreshold）——出现一次即进隔离。
// 单次误伤的代价由「probe delay + 探活放行」兜住：进隔离 1 分钟后即可探活，
// 内容级误伤（良性内容必过）→ 该号几分钟后回池；账号级标记才吃 silence 退避。
//
// 计数语义（阈值 >1 时才有意义，保留以支持调参与向后兼容）：
// streak+1，成功即清零；达阈 → 进隔离并清零计数。
// 已在隔离中（防御分支：隔离号本无真实流量）：只清计数不重复隔离。
func (p *Pool) NoteContentBlocked(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.quarantineOn {
		return // 总开关关闭：不计数不隔离（回到无隔离行为）
	}
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	e.contentBlockedStreak++
	if e.contentBlockedStreak < p.quarantineThresholdOr() {
		p.dirty.Store(true)
		return
	}
	e.contentBlockedStreak = 0
	if e.quarantined {
		p.dirty.Store(true)
		return
	}
	p.enterQuarantineLocked(e, quarantineReasonText)
}

// EnterQuarantine 以指定原因让账号进隔离（非内容审核来源，如入池探活连续无结论）。
// quarantineOn 关闭时空操作（与 NoteContentBlocked 同口径）。
func (p *Pool) EnterQuarantine(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.quarantineOn {
		return
	}
	e, ok := p.byUID[uid]
	if !ok || e.quarantined {
		return
	}
	p.enterQuarantineLocked(e, reason)
}

// enterQuarantineLocked 进隔离原语（NoteContentBlocked / EnterQuarantine 共用）：
// quarantined=true；quarantineUntil = now + probeDelay（首次可探活时刻，**不是**
// 惩罚时长）；hits++ 驱动探活失败后的退避；reason 落盘供 /status 溯源。
// 调用方必须已持有 p.mu，且已确认账号未处于隔离态。
func (p *Pool) enterQuarantineLocked(e *entry, reason string) {
	delay := p.quarantineProbeDelay
	if delay <= 0 {
		delay = defaultQuarantineProbeDelay
	}
	e.quarantined = true
	e.quarantineUntil = time.Now().Add(delay)
	e.quarantineReason = reason
	e.quarantineHits++
	p.dirty.Store(true)
	log.Printf("WARN: [pool] quarantine enter acct=%s reason=%q probe_delay=%s hits=%d",
		logfmt.Label(e.a.UID, e.a.Nickname), reason, delay, e.quarantineHits)
}

// ReleaseQuarantine 探活通过/运维复活：清除隔离域（quarantined/until/reason/hits）。
// hits 一并清零——放行即证明账号当前健康，再犯从基数重新起罚（不背历史退避）。
// 返回 true 表示本次确实解除了隔离。
func (p *Pool) ReleaseQuarantine(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || !e.quarantined {
		return false
	}
	e.clearQuarantineLocked()
	p.dirty.Store(true)
	log.Printf("[pool] quarantine release acct=%s (probe passed / manual revive)", logfmt.Label(uid, e.a.Nickname))
	return true
}

// QuarantineExtend 探活仍被内容审核拦截：续默退避——静默期 = base × 2^min(hits,shift)
// 封顶 silenceMax（hits 此前已含进隔离那次自增），并再 hits++ 驱动下次续默继续翻倍。
// 不清除隔离位，reason 刷新为续默文案（/status 可见「探活失败仍在静默」）。
// uid 不存在或未隔离时空操作。
func (p *Pool) QuarantineExtend(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || !e.quarantined {
		return
	}
	silence := p.quarantineDurationLocked(e.quarantineHits)
	e.quarantineUntil = time.Now().Add(silence)
	e.quarantineHits++
	if reason != "" {
		e.quarantineReason = reason
	}
	p.dirty.Store(true)
	log.Printf("WARN: [pool] quarantine extend acct=%s silence=%s hits=%d (probe blocked again by moderation)",
		logfmt.Label(uid, e.a.Nickname), silence, e.quarantineHits)
}

// QuarantineDueForProbe 返回「隔离中且静默期已过」的账号 UID 列表（按 UID 排序，
// 稳定输出）——探活 prober 每轮的处理对象。静默期未到的号不在列（等待期）；
// quarantinedUntil 恰好等于 now 的号在列（now.Before 判定的自然边界）。
func (p *Pool) QuarantineDueForProbe() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var uids []string
	for uid, e := range p.byUID {
		if !e.quarantined {
			continue
		}
		if !e.quarantineUntil.IsZero() && now.Before(e.quarantineUntil) {
			continue // 静默期未过：还在等
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// QuarantinedCount 返回当前隔离中的账号数（/status 汇总用）。
func (p *Pool) QuarantinedCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, e := range p.byUID {
		if e.quarantined {
			n++
		}
	}
	return n
}

// clearQuarantineLocked 清隔离域：隔离位/静默截止/原因/退避计数全归零。
// 调用方必须已持有 p.mu。
func (e *entry) clearQuarantineLocked() {
	e.quarantined = false
	e.quarantineUntil = time.Time{}
	e.quarantineReason = ""
	e.quarantineHits = 0
}

// quarantineThresholdOr 返回生效的隔离阈值（未注入时按默认 3）。调用方必须已持有 p.mu。
func (p *Pool) quarantineThresholdOr() int {
	if p.quarantineThreshold > 0 {
		return p.quarantineThreshold
	}
	return defaultQuarantineThreshold
}

// quarantineDurationLocked 按历史隔离次数计算静默时长：base × 2^min(hits, shift)，
// 封顶 silenceMax。hits=0（首次）原样返回 base。左移溢出由 shift 上限 + 封顶兜底。
// 调用方必须已持有 p.mu。
func (p *Pool) quarantineDurationLocked(hits int) time.Duration {
	d := p.quarantineSilence
	if d <= 0 {
		d = defaultQuarantineSilence
	}
	if hits > 0 {
		shift := hits
		if shift > quarantineShiftMax {
			shift = quarantineShiftMax
		}
		d <<= shift
	}
	max := p.quarantineSilenceMax
	if max <= 0 {
		max = defaultQuarantineMax
	}
	if d > max || d <= 0 { // d<=0：左移溢出成负数/零，同样按封顶兜底
		d = max
	}
	return d
}
