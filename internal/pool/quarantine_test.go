package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// newQuarantineTestPool 构造无持久化的最小测试池（单账号）。
// 注意：New 的池级默认 probationOn=false（策略由 SetProbation 注入），故这里的
// 账号是**普通号**，选号语义与既有测试一致。
func newQuarantineTestPool(t *testing.T, uid string) *Pool {
	t.Helper()
	p := New("")
	p.Add(&auth.Auth{UID: uid})
	return p
}

// quarantineSt 声明测试用的隔离状态读取（走 Status 脱敏层，与 /status 同口径）。
func quarantineSt(t *testing.T, p *Pool, uid string) Status {
	t.Helper()
	st, ok := p.Status(uid)
	if !ok {
		t.Fatalf("uid %s not found", uid)
	}
	return st
}

// TestQuarantineImmediateByDefault 默认阈值 = 1：内容是恶性错误，出现**一次**即隔离。
// 隔离后的等待是 probeDelay（1m，可探活资格），不是惩罚时长。
func TestQuarantineImmediateByDefault(t *testing.T) {
	p := newQuarantineTestPool(t, "u1")
	p.NoteContentBlocked("u1") // 单次
	st := quarantineSt(t, p, "u1")
	if !st.Quarantined {
		t.Fatalf("default threshold=1: a single moderation block must quarantine immediately")
	}
	if !st.Cooling || st.CoolKind != "quarantine" {
		t.Fatalf("quarantined account should show cooling/quarantine: %+v", st)
	}
	if st.QuarantineReason == "" {
		t.Fatalf("quarantine_reason should be set")
	}
	if d := time.Until(st.QuarantineUntil); d > defaultQuarantineProbeDelay+5*time.Second || d < defaultQuarantineProbeDelay-5*time.Second {
		t.Fatalf("first probe window should be ~probeDelay(%v), got %v", defaultQuarantineProbeDelay, d)
	}
	// 一次即隔离 ⇒ 该号立刻不可选
	if a := p.Pick(""); a != nil {
		t.Fatalf("quarantined account must not be picked")
	}
}

// TestQuarantineThresholdConfigurableAndSuccessReset 阈值可配（退回旧的"连续 N 次"行为）：
// 未达阈不隔离；成功清零（成功是「账号当前未被提权审核」的最强证据）；达阈进隔离。
func TestQuarantineThresholdConfigurableAndSuccessReset(t *testing.T) {
	p := newQuarantineTestPool(t, "u1")
	p.SetQuarantine(3, time.Minute, time.Hour, 24*time.Hour)
	// 2 次（< 阈值 3）：不隔离，计数累计
	p.NoteContentBlocked("u1")
	p.NoteContentBlocked("u1")
	if st := quarantineSt(t, p, "u1"); st.Quarantined || st.ContentBlockedStreak != 2 {
		t.Fatalf("before threshold: quarantined=%v streak=%d want false/2", st.Quarantined, st.ContentBlockedStreak)
	}
	// 成功清零
	p.NoteSuccess("u1")
	if st := quarantineSt(t, p, "u1"); st.Quarantined || st.ContentBlockedStreak != 0 {
		t.Fatalf("after success: quarantined=%v streak=%d want false/0", st.Quarantined, st.ContentBlockedStreak)
	}
	// 3 次达阈：进隔离
	for i := 0; i < 3; i++ {
		p.NoteContentBlocked("u1")
	}
	if st := quarantineSt(t, p, "u1"); !st.Quarantined {
		t.Fatalf("3 consecutive moderation blocks should quarantine")
	}
}

// TestQuarantineExcludesFromPickAndFallback 隔离号不进正常选号，也**不经全冷却
// 兜底回流**——兜底正是「时间一过又放出来污染号池」的漏洞面。
func TestQuarantineExcludesFromPickAndFallback(t *testing.T) {
	p := newQuarantineTestPool(t, "u1")
	p.NoteContentBlocked("u1")
	// 静默期已过（模拟：直接把 quarantineUntil 拨到过去——到期 ≠ 放行）
	p.mu.Lock()
	p.byUID["u1"].quarantineUntil = time.Now().Add(-time.Minute)
	p.mu.Unlock()
	if a := p.Pick(""); a != nil {
		t.Fatalf("expired-quarantine account must NOT be picked (probe-gated)")
	}
	if a := p.pickEarliestExpiryLocked(nil, time.Now(), ""); a != nil {
		t.Fatalf("expired-quarantine account must NOT re-enter via earliest-expiry fallback")
	}
	if got := p.AvailableUIDs(); len(got) != 0 {
		t.Fatalf("AvailableUIDs should exclude quarantined account, got %v", got)
	}
	if p.ServableNow() {
		t.Fatalf("ServableNow must be false when only account is quarantined")
	}
	if got := p.QuarantineDueForProbe(); len(got) != 1 || got[0] != "u1" {
		t.Fatalf("QuarantineDueForProbe=%v want [u1]", got)
	}
}

// TestQuarantineProbeWindowGatesProbeList 静默期未到时不在探活队列；到期（含恰好
// 等于 now 的边界）才进队列。
func TestQuarantineProbeWindowGatesProbeList(t *testing.T) {
	p := newQuarantineTestPool(t, "u1")
	p.NoteContentBlocked("u1")
	if got := p.QuarantineDueForProbe(); len(got) != 0 {
		t.Fatalf("account inside probe delay must not be due for probe: %v", got)
	}
	p.mu.Lock()
	p.byUID["u1"].quarantineUntil = time.Now().Add(-time.Second)
	p.mu.Unlock()
	if got := p.QuarantineDueForProbe(); len(got) != 1 || got[0] != "u1" {
		t.Fatalf("account past probe delay should be due: %v", got)
	}
}

// TestQuarantineProbeReleaseAndExtend 探活放行（hits 清零）与续默退避（×2）。
func TestQuarantineProbeReleaseAndExtend(t *testing.T) {
	p := newQuarantineTestPool(t, "u1")
	p.NoteContentBlocked("u1")
	// 续默：探活仍被拦 → until 按 2^hits × silence 后移、hits 递增
	before := quarantineSt(t, p, "u1").QuarantineUntil
	p.QuarantineExtend("u1", "content moderation quarantine (probe blocked)")
	after := quarantineSt(t, p, "u1").QuarantineUntil
	if !after.After(before) {
		t.Fatalf("extend should push quarantine_until later: before=%v after=%v", before, after)
	}
	if d := time.Until(after); d < 2*time.Hour-time.Minute {
		t.Fatalf("first extend should be ~2×silence(1h)=2h, got %v", d)
	}
	if st := quarantineSt(t, p, "u1"); !st.Quarantined {
		t.Fatalf("extend must keep quarantined=true")
	}
	if st := quarantineSt(t, p, "u1"); st.QuarantineReason != "content moderation quarantine (probe blocked)" {
		t.Fatalf("extend should refresh reason, got %q", st.QuarantineReason)
	}
	// 放行：探活通过 → 隔离域全清（含 hits——再犯从基数起罚）
	p.ReleaseQuarantine("u1")
	if st := quarantineSt(t, p, "u1"); st.Quarantined || !st.QuarantineUntil.IsZero() || st.Cooling {
		t.Fatalf("release should clear quarantine: %+v", st)
	}
	if !p.healthyForPick(t, "u1") {
		t.Fatalf("released account should be pickable")
	}
	// 放行后再犯：等待回到 probeDelay 基数（hits 已清零，不背历史退避）
	p.NoteContentBlocked("u1")
	st := quarantineSt(t, p, "u1")
	if !st.Quarantined {
		t.Fatalf("re-offense should re-quarantine")
	}
	if d := time.Until(st.QuarantineUntil); d > defaultQuarantineProbeDelay+5*time.Second || d < defaultQuarantineProbeDelay-5*time.Second {
		t.Fatalf("re-quarantine window should restart from probeDelay(%v), got %v", defaultQuarantineProbeDelay, d)
	}
}

// TestQuarantineSilenceBackoffCap 续默退避 ×2^hits 封顶 24h。
func TestQuarantineSilenceBackoffCap(t *testing.T) {
	p := newQuarantineTestPool(t, "u1")
	p.SetQuarantine(1, time.Minute, time.Hour, 24*time.Hour)
	cases := []struct {
		hits int
		want time.Duration
	}{
		{0, time.Hour},
		{1, 2 * time.Hour},
		{2, 4 * time.Hour},
		{3, 8 * time.Hour},
		{4, 16 * time.Hour},
		{5, 24 * time.Hour}, // 封顶
		{99, 24 * time.Hour},
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range cases {
		if got := p.quarantineDurationLocked(c.hits); got != c.want {
			t.Errorf("silence(hits=%d)=%v want %v", c.hits, got, c.want)
		}
	}
}

// TestQuarantinePersistenceRoundtrip 隔离状态落盘/恢复——**到期条目不过滤**
// （静默到期仍须探活，重启不得让被标记号直接回池）。
func TestQuarantinePersistenceRoundtrip(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteContentBlocked("u1")
	// 拨到过去 + 落盘：模拟「静默已过、待探活」时重启
	p.mu.Lock()
	p.byUID["u1"].quarantineUntil = time.Now().Add(-time.Minute)
	p.mu.Unlock()
	p.Flush()
	p.Close()

	// 同目录新池恢复：隔离态必须在（含已到期的 until）
	p2 := New(fp)
	defer p2.Close()
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || !st.Quarantined {
		t.Fatalf("quarantine state must survive restart (even past-silence)")
	}
	if got := p2.QuarantineDueForProbe(); len(got) != 1 || got[0] != "u1" {
		t.Fatalf("restarted pool should list expired-quarantine account for probe: %v", got)
	}
	if a := p2.Pick(""); a != nil {
		t.Fatalf("restarted quarantined account must not be picked")
	}
	// 落盘文件里应看到 quarantined 字段（持久化格式自检）
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var sf struct {
		Accounts map[string]struct {
			Quarantined    bool `json:"quarantined"`
			QuarantineHits int  `json:"quarantine_hits"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("parse state: %v", err)
	}
	if !sf.Accounts["u1"].Quarantined || sf.Accounts["u1"].QuarantineHits != 1 {
		t.Fatalf("state.json quarantine fields missing/incorrect")
	}
}

// TestReviveDisabledClearsQuarantine 运维复活 = 放行证据（与探活通过同级）。
func TestReviveDisabledClearsQuarantine(t *testing.T) {
	p := newQuarantineTestPool(t, "u1")
	p.Disable("u1", "12153 session dead")
	p.NoteContentBlocked("u1")
	if st := quarantineSt(t, p, "u1"); !st.Quarantined {
		t.Fatalf("precondition: quarantined")
	}
	if !p.ReviveDisabled("u1") {
		t.Fatalf("revive should succeed")
	}
	if st := quarantineSt(t, p, "u1"); st.Quarantined || st.Disabled {
		t.Fatalf("revive should clear both disabled and quarantine: %+v", st)
	}
}

// TestQuarantineDisabledSwitch 总开关关闭：不计数不隔离。
func TestQuarantineDisabledSwitch(t *testing.T) {
	p := newQuarantineTestPool(t, "u1")
	p.SetQuarantineEnabled(false)
	for i := 0; i < 5; i++ {
		p.NoteContentBlocked("u1")
	}
	if st := quarantineSt(t, p, "u1"); st.Quarantined || st.ContentBlockedStreak != 0 {
		t.Fatalf("disabled switch should make NoteContentBlocked a no-op: %+v", st)
	}
}

// healthyForPick 测试辅助：Pick 路径的可选性（healthy 口径）。
func (p *Pool) healthyForPick(t *testing.T, uid string) bool {
	t.Helper()
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	return e.healthy(time.Now())
}
