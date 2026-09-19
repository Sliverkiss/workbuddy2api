package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// newProbationTestPool 构造「一个老号 + 一个观察号」的测试池：
// 先 Add 的老号（probationOn 还是池级默认 false）不带观察标记，随后注入观察池策略
// 再 Add 的新号才被标记——这正是生产里「既有账号 vs 新进账号」的区分方式。
func newProbationTestPool(t *testing.T, oldUID, newUID string) *Pool {
	t.Helper()
	p := New("")
	if oldUID != "" {
		p.Add(&auth.Auth{UID: oldUID})
	}
	p.SetProbation(true, 3, 5*time.Minute)
	if newUID != "" {
		p.Add(&auth.Auth{UID: newUID})
	}
	return p
}

// probationSt 读取账号的观察池字段（走 Status 脱敏层，与 /status 同口径）。
func probationSt(t *testing.T, p *Pool, uid string) Status {
	t.Helper()
	st, ok := p.Status(uid)
	if !ok {
		t.Fatalf("uid %s not found", uid)
	}
	return st
}

// TestProbationNewAccountIsProbeGated 新号入池即进观察池：未验活前不进任何真实流量
// 路径（选号 / 兜底 / AvailableUIDs / ServableNow），也不误伤它自己的可用性统计。
func TestProbationNewAccountIsProbeGated(t *testing.T) {
	p := newProbationTestPool(t, "", "n1")
	st := probationSt(t, p, "n1")
	if !st.Probation || st.ProbationPasses != 0 {
		t.Fatalf("new account should enter probation with 0 passes: %+v", st)
	}
	if a := p.Pick(""); a != nil {
		t.Fatalf("unverified probation account must NOT be picked")
	}
	if a := p.pickEarliestExpiryLocked(nil, time.Now(), ""); a != nil {
		t.Fatalf("unverified probation account must NOT be picked via fallback")
	}
	if got := p.AvailableUIDs(); len(got) != 0 {
		t.Fatalf("AvailableUIDs must exclude unverified probation account, got %v", got)
	}
	if p.ServableNow() {
		t.Fatalf("ServableNow must be false when only account is unverified probation")
	}
	if got := p.ProbationNeedingProbe(); len(got) != 1 || got[0] != "n1" {
		t.Fatalf("ProbationNeedingProbe=%v want [n1]", got)
	}
}

// TestProbationUnverifiedExcludedFromExpiryFallback 兜底路径（pickEarliestExpiryLocked）
// 只按 expiry 选号、会绕开 healthy()——未验活的观察号若处于软冷却，必须被显式排除，
// 否则"不拿未知风险的号接用户请求"这条约束会被兜底击穿。
func TestProbationUnverifiedExcludedFromExpiryFallback(t *testing.T) {
	p := newProbationTestPool(t, "", "n1")
	p.mu.Lock()
	e := p.byUID["n1"]
	e.until = time.Now().Add(30 * time.Minute) // 软冷却：有 expiry，本该进兜底候选
	e.coolKind = CoolSoft
	p.mu.Unlock()
	if a := p.pickEarliestExpiryLocked(nil, time.Now(), ""); a != nil {
		t.Fatalf("unverified probation account in soft cooldown must NOT enter expiry fallback")
	}
	// 对照：同样形态但已验活（passes>=1）的观察号可以进兜底（它已被证明可用）
	p.mu.Lock()
	p.byUID["n1"].probationPasses = 1
	p.mu.Unlock()
	if a := p.pickEarliestExpiryLocked(nil, time.Now(), ""); a == nil {
		t.Fatalf("verified probation account in soft cooldown should be usable as fallback")
	}
}

// TestProbationProbePassEnablesCanary 入池探活通过 → 记一次进度并取得搭车资格
// （healthy 转真、退出待探活队列、可被 pick 选中）。
func TestProbationProbePassEnablesCanary(t *testing.T) {
	p := newProbationTestPool(t, "", "n1")
	p.NoteProbationProbePassed("n1")
	st := probationSt(t, p, "n1")
	if !st.Probation || st.ProbationPasses != 1 {
		t.Fatalf("probe pass should add one progress (still in probation): %+v", st)
	}
	if got := p.ProbationNeedingProbe(); len(got) != 0 {
		t.Fatalf("verified account should leave the probe queue, got %v", got)
	}
	if !p.healthyForPick(t, "n1") {
		t.Fatalf("verified probation account should be healthy (canary-eligible)")
	}
	if a := p.Pick(""); a == nil || a.UID != "n1" {
		t.Fatalf("verified probation account should be pickable (canary), got %v", a)
	}
}

// TestProbationCanaryThrottledByInterval 搭车闸是**池级单闸**：主池有健康号时，
// 一次搭车之后窗口内的后续选号必须回到主池（不是每号一闸，观察号再多也如此）。
func TestProbationCanaryThrottledByInterval(t *testing.T) {
	oldGap := minPickGap
	minPickGap = 0
	defer func() { minPickGap = oldGap }()

	p := newProbationTestPool(t, "m1", "n1")
	p.NoteProbationProbePassed("n1")

	// 首次：lastCanaryAt 为零 → 窗口已开 → 本次请求改道给观察号
	if a := p.Pick(""); a == nil || a.UID != "n1" {
		t.Fatalf("first pick should be rerouted to the canary account, got %v", a)
	}
	// 窗口内后续：必须回到主池（搭车号再多也不能连续吃真实流量）
	for i := 0; i < 3; i++ {
		if a := p.Pick(""); a == nil || a.UID != "m1" {
			t.Fatalf("pick #%d should go back to the main pool, got %v", i+2, a)
		}
	}
	// 窗口过期（把 lastCanaryAt 拨回）→ 再给一次搭车机会
	p.mu.Lock()
	p.lastCanaryAt = time.Now().Add(-2 * time.Hour)
	p.mu.Unlock()
	if a := p.Pick(""); a == nil || a.UID != "n1" {
		t.Fatalf("after interval elapses the canary should fire again, got %v", a)
	}
}

// TestProbationNoCanaryWhenMainPoolEmpty 主池无健康候选时**不搭车**（约束 1：
// 搭车号失败时没得兜底，会把风险直接摊给用户）；此时观察号走兜底通道接单
// ——比 503 强——且不消耗搭车窗口（主池恢复后应立刻能搭车）。
func TestProbationNoCanaryWhenMainPoolEmpty(t *testing.T) {
	p := newProbationTestPool(t, "m1", "n1")
	p.NoteProbationProbePassed("n1")
	p.Disable("m1", "12153 session dead") // 主池全空

	if a := p.Pick(""); a == nil || a.UID != "n1" {
		t.Fatalf("empty main pool should fall back to verified probation account, got %v", a)
	}
	if !p.lastCanaryAt.IsZero() {
		t.Fatalf("fallback must NOT consume the canary window (lastCanaryAt=%v)", p.lastCanaryAt)
	}
}

// TestProbationGraduation 累计成功达阈毕业：probation=false，与老号同权；毕业状态
// 不因后续成功反复写盘/复位。
func TestProbationGraduation(t *testing.T) {
	p := newProbationTestPool(t, "m1", "n1")
	// 3 次进度（含入池探活那次的口径：探活与真实成功同权计次）
	p.NoteProbationProbePassed("n1")
	p.NoteSuccess("n1") // 真实成功（搭车/粘性/兜底任一）同样计次
	st := probationSt(t, p, "n1")
	if !st.Probation || st.ProbationPasses != 2 {
		t.Fatalf("after 2 successes should still be in probation with passes=2: %+v", st)
	}
	p.NoteSuccess("n1")
	st = probationSt(t, p, "n1")
	if st.Probation || st.ProbationPasses != 3 {
		t.Fatalf("3rd success should graduate (probation=false, passes kept): %+v", st)
	}
	if !p.healthyForPick(t, "n1") {
		t.Fatalf("graduated account must be healthy")
	}
}

// TestProbationProbeFailuresQuarantine 入池探活连续无结论（网络/限流等）达阈进隔离
// ——死凭证号不该被无限探活（探活本身也是对上游的请求）。
func TestProbationProbeFailuresQuarantine(t *testing.T) {
	p := newProbationTestPool(t, "", "n1")
	p.NoteProbationProbeInconclusive("n1")
	p.NoteProbationProbeInconclusive("n1")
	if st := probationSt(t, p, "n1"); st.Quarantined {
		t.Fatalf("below cap should not quarantine: %+v", st)
	}
	p.NoteProbationProbeInconclusive("n1") // 第 3 次达 probationProbeFailCap
	if st := probationSt(t, p, "n1"); !st.Quarantined {
		t.Fatalf("3 inconclusive probes should quarantine the dead-credential account")
	}
	if got := p.ProbationNeedingProbe(); len(got) != 0 {
		t.Fatalf("quarantined account must leave the probation probe queue, got %v", got)
	}
}

// TestProbationModerationBlockQuarantinesNewAccount 观察号被内容审核拦截 = 恶性错误：
// 一次即进隔离（阈值默认 1），且隔离**不清**观察标记（放行后仍是新号，须继续试炼）。
func TestProbationModerationBlockQuarantinesNewAccount(t *testing.T) {
	p := newProbationTestPool(t, "", "n1")
	p.NoteProbationProbePassed("n1") // 先取得搭车资格
	p.NoteContentBlocked("n1")       // 被审核拦（恶性错误）
	st := probationSt(t, p, "n1")
	if !st.Quarantined {
		t.Fatalf("moderation block must quarantine the probation account immediately")
	}
	if !st.Probation {
		t.Fatalf("quarantine must NOT clear the probation mark (still a new account after release)")
	}
	p.ReleaseQuarantine("n1")
	st = probationSt(t, p, "n1")
	if !st.Probation || st.ProbationPasses != 1 {
		t.Fatalf("after release the account should keep its probation progress: %+v", st)
	}
}

// TestProbationPersistenceRoundtrip 观察池状态落盘/恢复：已毕业号不得因重启被打回
// 观察池；在池号的搭车进度不重学。
func TestProbationPersistenceRoundtrip(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.SetProbation(true, 3, 5*time.Minute)
	p.Add(&auth.Auth{UID: "n1"})
	p.Add(&auth.Auth{UID: "n2"})
	p.NoteProbationProbePassed("n1") // n1: passes=1（在池）
	for i := 0; i < 3; i++ {
		p.NoteProbationProbePassed("n2") // n2: 达阈毕业
	}
	p.Flush()
	p.Close()

	p2 := New(fp)
	defer p2.Close()
	p2.Add(&auth.Auth{UID: "n1"})
	p2.Add(&auth.Auth{UID: "n2"})
	if st := probationSt(t, p2, "n1"); !st.Probation || st.ProbationPasses != 1 {
		t.Fatalf("in-probation progress must survive restart: %+v", st)
	}
	if st := probationSt(t, p2, "n2"); st.Probation || st.ProbationPasses != 3 {
		t.Fatalf("graduated account must stay graduated across restart: %+v", st)
	}
	// 落盘格式自检
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var sf struct {
		Accounts map[string]struct {
			Probation       bool `json:"probation"`
			ProbationPasses int  `json:"probation_passes"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("parse state: %v", err)
	}
	if !sf.Accounts["n1"].Probation || sf.Accounts["n1"].ProbationPasses != 1 {
		t.Fatalf("state.json probation fields missing for n1: %+v", sf.Accounts["n1"])
	}
	if sf.Accounts["n2"].Probation {
		t.Fatalf("state.json should record n2 as graduated")
	}
}

// TestProbationDisabledSwitch 观察池关停：新号直接按老号同权进池（回到旧行为）。
func TestProbationDisabledSwitch(t *testing.T) {
	p := New("")
	p.SetProbation(false, 0, 0)
	p.Add(&auth.Auth{UID: "n1"})
	st := probationSt(t, p, "n1")
	if st.Probation {
		t.Fatalf("probation disabled: new account must not be gated: %+v", st)
	}
	if a := p.Pick(""); a == nil || a.UID != "n1" {
		t.Fatalf("probation disabled: new account should be immediately pickable, got %v", a)
	}
}

// TestProbationCounts 汇总计数（/status 概览用）。
func TestProbationCounts(t *testing.T) {
	p := newProbationTestPool(t, "m1", "n1")
	p.Add(&auth.Auth{UID: "n2"})
	if got := p.ProbationCount(); got != 2 {
		t.Fatalf("ProbationCount=%d want 2", got)
	}
	for i := 0; i < 3; i++ {
		p.NoteProbationProbePassed("n1")
	}
	if got := p.ProbationCount(); got != 1 {
		t.Fatalf("ProbationCount after graduation=%d want 1", got)
	}
}
