// 探活 prober：两类队列共用同一套「最小良性 chat 请求」原语。
//
//  1. **隔离号池**：周期扫描「隔离中且静默期已过」的账号，实测通过即放行
//     （pool.ReleaseQuarantine），仍被内容审核拦截则续默退避（pool.QuarantineExtend，
//     ×2^hits 封顶）。这是隔离状态机「到期不放行、探活再回池」语义的执行端
//     （状态机本体见 internal/pool/quarantine.go）。
//  2. **新号观察池**：扫描「在观察池且尚未验活（passes==0）」的新号，实测通过即记
//     一次毕业进度（pool.NoteProbationProbePassed）——这是新号获得「可搭车」资格的
//     唯一入口；连续无结论达阈进隔离（死凭证号不该被无限探活）；被审核拦则按恶性
//     错误处理（pool.NoteContentBlocked，一次即隔离）。
//
// 两条队列共用 probeAccount 三分类原语：差别只在结果的动作，不在探测方式。
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
)

// 探活默认参数。
const (
	// defaultProbeInterval 探活轮询间隔：隔离静默期以小时计，5 分钟的轮询粒度足够
	// （放行延迟 ≤ interval + 探活耗时，对小时级静默可忽略）。观察池也用同一间隔：
	// 新号从入池到毕业要 1 次探活 + 若干次搭车成功，5m 粒度下数十分钟内可完成，
	// 无需为它单独加一轮请求节奏。
	defaultProbeInterval = 5 * time.Minute
	// probeTimeout 单次探活请求的总时长上限：探活是后台动作，超时即放弃本轮
	// （下轮再试），不占住 loop。
	probeTimeout = 60 * time.Second
	// defaultProbeModelCN / defaultProbeModelGlobal 探活默认模型：flash 档（便宜/
	// 免费、快），良性内容一行即可。可经 config quarantine.probe_model_* 覆盖。
	defaultProbeModelCN     = "deepseek-v4.1-flash"
	defaultProbeModelGlobal = "gpt-5.4"
	// quarantineProbeReason 探活仍被拦时续默写入的 reason（/status 可见）。
	quarantineProbeReason = "content moderation quarantine (probe blocked)"
)

// QuarantineProbeConfig 探活 prober 参数（cmd/server 从 config quarantine 段装配）。
type QuarantineProbeConfig struct {
	// Disabled 关停探活（config quarantine.enabled=false）。关停后隔离号**永不**
	// 自动放行（探活是唯一放行通道）——这是逃生门不是常规用法，运维须知。
	// 同时关停观察池入池探活（新号永远拿不到搭车资格，只吃主池全空时的兜底）。
	Disabled bool
	// Interval 轮询间隔；<=0 回落默认 5m。
	Interval time.Duration
	// ProbeModelCN / ProbeModelGlobal 按 realm 的探活模型；空回落默认 flash 档。
	ProbeModelCN     string
	ProbeModelGlobal string
}

// probeModelFor 按 realm 返回探活模型。
func (c QuarantineProbeConfig) probeModelFor(realm string) string {
	if realm == "global" {
		if c.ProbeModelGlobal != "" {
			return c.ProbeModelGlobal
		}
		return defaultProbeModelGlobal
	}
	if c.ProbeModelCN != "" {
		return c.ProbeModelCN
	}
	return defaultProbeModelCN
}

// probeBody 探活请求体：单轮 benign 一句话 + 极小 max_tokens。
// stream=false：探活只需要状态码/错误信封，无需流式（上游 /v2 对非流式返回 JSON）。
func probeBody(model string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"max_tokens": 16,
		"stream":     false,
	})
	return raw
}

// probeOutcome 探活结果三分类（隔离探活与观察池入池探活共用同一分类语义）。
type probeOutcome int

const (
	// probeInconclusive 网络/限流/5xx/session 等非审核失败：既不能证明「审核已解除」
	// （不能放行），也不构成审核拦截（不该按恶性错误加重）。两条队列对此都只做
	// 「保持现状、下轮再试」（观察池另加连续无结论计数，防死凭证号被无限探活）。
	probeInconclusive probeOutcome = iota
	// probePassed 200：良性内容通过 → 账号未被提权审核（或标记已解除）。
	probePassed
	// probeBlocked 仍被内容审核拦截（moderation_blocked）→ 账号级标记未解除。
	probeBlocked
)

// probeAccount 对单个账号发一次最小良性 chat 请求并三分类。
// 返回值：结果分类 / 本次使用的探活模型（日志用）/ 具体错误（inconclusive 时非 nil）。
//
// token 前置刷新与 handler chat 路径同口径：探活必打 chat，token 过期只会得到 401
// 噪音；刷新失败本身无法证明审核状态（按 inconclusive 返回）。
func (s *Scheduler) probeAccount(a *auth.Auth, qc QuarantineProbeConfig) (probeOutcome, string, error) {
	model := qc.probeModelFor(a.Realm())
	if a.NeedsRefresh(checkinRefreshSkew) {
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			return probeInconclusive, model, fmt.Errorf("refresh: %w", err)
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("WARN: [probe] %s: save auth failed: %v", logfmt.Label(a.UID, a.Nickname), err)
		}
	}
	meta := upstream.ChatMeta{
		// 轮级兜底键形态：探活请求独立成键（不复用任何会话 ID），后台聚合不与
		// 真实流量混淆。
		ConversationRequestID: fmt.Sprintf("probe-%x", time.Now().UnixNano()),
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	rc, status, respBody, err := s.cfg.Upstream.ChatStreamContext(ctx, a, probeBody(model), "", meta)
	if rc != nil {
		rc.Close()
	}
	if err != nil {
		var ue *upstream.Error
		if errors.As(err, &ue) && ue.Kind == upstream.ErrModerationBlocked {
			return probeBlocked, model, nil
		}
		return probeInconclusive, model, err
	}
	if status >= 400 {
		// 防御分支（ChatStreamContext ≥400 应带 *Error）：本地再分类一次。
		kind := upstream.Classify(status, string(respBody))
		if kind == upstream.ErrModerationBlocked {
			return probeBlocked, model, nil
		}
		return probeInconclusive, model, fmt.Errorf("http %d %s", status, kind)
	}
	return probePassed, model, nil
}

// RunQuarantineProbeNow 立即执行一轮隔离探活：对每个静默期已过的隔离号发一次良性
// chat 请求并按结果分流（放行/续默/下轮再试）。单号失败不影响其余号（逐号独立）。
// 返回本轮处理的账号数。供探活 loop 与运维手动触发共用。
func (s *Scheduler) RunQuarantineProbeNow(qc QuarantineProbeConfig) int {
	uids := s.cfg.Pool.QuarantineDueForProbe()
	if len(uids) == 0 {
		return 0
	}
	processed := 0
	for _, uid := range uids {
		a := s.cfg.Pool.AuthByUID(uid)
		if a == nil {
			continue // 账号已被移出池（SyncToDir 剔除）：隔离状态随删除自然消失
		}
		processed++
		s.probeQuarantinedAccount(a, qc)
	}
	return processed
}

// RunProbationProbeNow 立即执行一轮观察池入池探活：对每个「在观察池且未验活」的
// 新号发一次良性 chat 请求。返回本轮处理的账号数。
func (s *Scheduler) RunProbationProbeNow(qc QuarantineProbeConfig) int {
	uids := s.cfg.Pool.ProbationNeedingProbe()
	if len(uids) == 0 {
		return 0
	}
	processed := 0
	for _, uid := range uids {
		a := s.cfg.Pool.AuthByUID(uid)
		if a == nil {
			continue
		}
		processed++
		s.probeProbationAccount(a, qc)
	}
	return processed
}

// probeQuarantinedAccount 对单个隔离号执行探活。结果三分：
//   - 200（上游接受良性内容）→ ReleaseQuarantine 放行回池；
//   - moderation_blocked（仍被审核拦）→ QuarantineExtend 续默退避；
//   - 其他错误（网络/限流/session 等）→ 不动隔离状态，下轮再试——这些错误不能
//     证明「审核已解除」（不能放行），也不是审核拦截（不该续默加重）。
func (s *Scheduler) probeQuarantinedAccount(a *auth.Auth, qc QuarantineProbeConfig) bool {
	outcome, model, err := s.probeAccount(a, qc)
	switch outcome {
	case probeBlocked:
		// 仍被内容审核拦截：续默退避（探活失败的唯一动作）。
		s.cfg.Pool.QuarantineExtend(a.UID, quarantineProbeReason)
		return true
	case probePassed:
		// 200：良性内容通过 → 账号未被标记（或标记已解除），放行回池。
		log.Printf("[probe] %s: probe passed model=%s → release from quarantine", logfmt.Label(a.UID, a.Nickname), model)
		s.cfg.Pool.ReleaseQuarantine(a.UID)
		return true
	default:
		// 其他错误（网络/限流/5xx/session 等）：保持隔离，下轮再试。
		log.Printf("WARN: [probe] %s: probe inconclusive (%v), keep quarantined", logfmt.Label(a.UID, a.Nickname), err)
		return false
	}
}

// probeProbationAccount 对单个观察号执行入池探活。结果三分：
//   - 200 → NoteProbationProbePassed：记一次毕业进度（这就是新号拿到搭车资格的门）；
//   - moderation_blocked → NoteContentBlocked：内容是**恶性错误**，一次即进隔离
//     （新号被上游标记时，绝不能放它去接用户请求）；
//   - 其他错误 → NoteProbationProbeInconclusive：连续达阈进隔离——死凭证号不该被
//     无限探活（探活本身也是对上游的请求）。
func (s *Scheduler) probeProbationAccount(a *auth.Auth, qc QuarantineProbeConfig) bool {
	outcome, model, err := s.probeAccount(a, qc)
	switch outcome {
	case probeBlocked:
		log.Printf("WARN: [probe] %s: probation probe blocked by moderation → quarantine", logfmt.Label(a.UID, a.Nickname))
		s.cfg.Pool.NoteContentBlocked(a.UID)
		return true
	case probePassed:
		log.Printf("[probe] %s: probation probe passed model=%s (canary eligible)", logfmt.Label(a.UID, a.Nickname), model)
		s.cfg.Pool.NoteProbationProbePassed(a.UID)
		return true
	default:
		log.Printf("WARN: [probe] %s: probation probe inconclusive (%v)", logfmt.Label(a.UID, a.Nickname), err)
		s.cfg.Pool.NoteProbationProbeInconclusive(a.UID)
		return false
	}
}

// RunProbeLoop 探活后台循环：每 interval 一轮，先隔离探活（到期放行/续默）再观察池
// 入池探活（新号验活），ctx 取消即退出。main 里 `go sch.RunProbeLoop(ctx, qc)` 启动。
//
// 两轮共用同一 interval：隔离探活的粒度由静默期（小时级）决定，观察池由「新号多久
// 能拿到搭车资格」决定，5m 对两者都是合理粒度；分成两个 ticker 只会让探活请求的
// 时间分布更碎片化，没有收益。
func (s *Scheduler) RunProbeLoop(ctx context.Context, qc QuarantineProbeConfig) {
	if qc.Disabled {
		log.Printf("[probe] 探活已禁用（quarantine.enabled=false）：隔离号不会自动放行，新号不会拿到搭车资格")
		return
	}
	interval := qc.Interval
	if interval <= 0 {
		interval = defaultProbeInterval
	}
	log.Printf("[probe] 探活已启用：interval=%s probe_model(cn=%s global=%s)",
		interval, qc.probeModelFor("cn"), qc.probeModelFor("global"))
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := s.RunQuarantineProbeNow(qc); n > 0 {
				log.Printf("[probe] quarantine probe round: %d account(s) due", n)
			}
			if n := s.RunProbationProbeNow(qc); n > 0 {
				log.Printf("[probe] probation probe round: %d account(s) unverified", n)
			}
		}
	}
}
