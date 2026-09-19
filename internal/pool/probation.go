// 新号观察池（probation）：新账号进池不直接吃主池流量，走「入池探活 → 极小份额
// 搭车真实请求 → 达阈毕业」三级。动机：新号的凭证/模型权限/限流画像/风控状态全是
// 未知数，直接按三因子权重与老号抢流量，会把「未知风险」摊到用户请求上。
//
// 为什么不用影子测试（复制真实流量给新号）——本仓的流量纪律是「零新增上游请求」：
// 影子会让同一出口 IP 的请求量翻倍，直接踩上游 WAF/风控（WAF 403 修复的教训），
// 新号本身又处在风控敏感期。故这里走**搭车改道**（与 costTier 探索同一哲学）：
// 把一个既有真实请求改道给观察号，零新增上游请求。
//
// 为什么不能只做探活——探活只证明「凭证有效 + 模型可服务」，证明不了真实负载下的
// 限流/计费行为。故搭车段是必需的：它同时是探活与真实验证。
//
// 用户侧稳定性约束（三条，缺一不可）：
//  1. 搭车仅在**主池存在健康候选**时放行——搭车号失败时同请求轮转仍能兜回主池，
//     用户最多多等一次退避（rotateBackoff）；
//  2. 池级单闸节流（canaryInterval）：观察号再多，对主池流量的总侵入有上界；
//  3. 未验活（passes==0）的号不参与任何真实流量（healthy() 恒 false），主池全空
//     时也不会被兜底选中——不会用「可能被审核拦」的号去接用户请求。
package pool

import (
	"log"
	"sort"
	"time"

	"workbuddy2api/internal/logfmt"
)

// SetProbation 注入新号观察池参数（main 从 config 解析后调用）。
// promoteSuccesses<=0 保留默认；canaryInterval<=0 保留默认。
func (p *Pool) SetProbation(enabled bool, promoteSuccesses int, canaryInterval time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probationOn = enabled
	if promoteSuccesses > 0 {
		p.probationPromote = promoteSuccesses
	}
	if canaryInterval > 0 {
		p.canaryInterval = canaryInterval
	}
}

// canaryDueLocked 报告「搭车窗口」是否开启：观察池总闸打开且距上次搭车 ≥ canaryInterval。
// 调用方必须已持有 p.mu（pick 写锁 / PickByUIDForModel 写锁）。
func (p *Pool) canaryDueLocked(now time.Time) bool {
	if !p.probationOn {
		return false
	}
	iv := p.canaryInterval
	if iv <= 0 {
		iv = defaultCanaryInterval
	}
	return now.Sub(p.lastCanaryAt) >= iv
}

// probationPromoteOrLocked 返回生效的毕业阈值（未注入时按默认 3）。调用方必须已持有 p.mu。
func (p *Pool) probationPromoteOrLocked() int {
	if p.probationPromote > 0 {
		return p.probationPromote
	}
	return defaultProbationPromote
}

// ProbationNeedingProbe 返回「在观察池且尚未验活（passes==0）」的账号 UID 列表
// （按 UID 排序）——探活 prober 的入池门禁队列。已验活但未毕业的号不在列（它们
// 靠搭车积累成功次数，不需要反复探活）。
func (p *Pool) ProbationNeedingProbe() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.probationOn {
		return nil
	}
	var uids []string
	for uid, e := range p.byUID {
		if !e.probation || e.probationPasses > 0 {
			continue
		}
		if e.disabled || e.manualDisabled || e.quarantined {
			continue // 禁用/停用/隔离中的号不走入池探活（隔离有各自的探活通道）
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// NoteProbationProbePassed 入池探活通过：观察号通过第一道门（凭证/模型可用），
// 记一次进度（探活本身是一次真实成功），达阈直接毕业。
// 非观察号/uid 不存在时空操作。
func (p *Pool) NoteProbationProbePassed(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || !e.probation {
		return
	}
	p.probationProgressLocked(e, "probe passed")
}

// NoteProbationProbeInconclusive 入池探活无结论（网络/限流/5xx 等非审核失败）：
// 连续 probeFails 达 probationProbeFailCap 即进隔离——死凭证号不该被无限探活
// （探活也是对上游的请求）。任意一次探活通过（NoteProbationProbePassed）或
// 真实成功（NoteSuccess）会清零本计数。
func (p *Pool) NoteProbationProbeInconclusive(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || !e.probation {
		return
	}
	e.probeFails++
	if e.probeFails < probationProbeFailCap {
		p.dirty.Store(true)
		return
	}
	e.probeFails = 0
	p.enterQuarantineLocked(e, probationProbeFailReason)
}

// probationProgressLocked 观察号累计一次成功：passes++，达阈毕业（probation=false，
// 与老号同权）。probeFails 一并清零（成功证明探活通道可用）。调用方必须已持有 p.mu。
func (p *Pool) probationProgressLocked(e *entry, source string) {
	e.probationPasses++
	e.probeFails = 0
	if e.probationPasses >= p.probationPromoteOrLocked() {
		e.probation = false
		log.Printf("[pool] probation graduate acct=%s passes=%d (%s): full pool weight",
			logfmt.Label(e.a.UID, e.a.Nickname), e.probationPasses, source)
	} else {
		log.Printf("[pool] probation progress acct=%s passes=%d/%d (%s)",
			logfmt.Label(e.a.UID, e.a.Nickname), e.probationPasses, p.probationPromoteOrLocked(), source)
	}
	p.dirty.Store(true)
}

// ProbationCount 返回观察池中尚未毕业的账号数（/status 汇总用）。
func (p *Pool) ProbationCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, e := range p.byUID {
		if e.probation {
			n++
		}
	}
	return n
}
