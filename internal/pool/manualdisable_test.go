// manualdisable_test.go 运维手动停用（issue #138/#118）的状态位语义测试。
//
// 覆盖三条不变量：
//  1. 手动停用后账号不被选号，但仍留在池里（状态可读、签到/保活路径不受阻）；
//  2. 手动停用与自动禁用互相独立——签到解冻/refresh 复活不会解除运维意图，
//     revive 也不会解除手动停用；
//  3. 手动停用状态持久化，重启后保留。
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestManualDisabledStopsSelection 停用后不参与选号（含全冷却兜底路径），
// 但仍在池里且状态可读。
func TestManualDisabledStopsSelection(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	if got := p.Pick(""); got == nil {
		t.Fatal("precondition: 停用前应可选")
	}

	found, changed := p.SetManualDisabled("u1", true, "观察几天")
	if !found || !changed {
		t.Fatalf("SetManualDisabled = (%v,%v), want (true,true)", found, changed)
	}

	// 仍在池里：状态可读、计数把它算作 disabled
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("停用后账号应从池里消失了吗？仍应可读状态")
	}
	if !st.ManualDisabled {
		t.Fatalf("manual_disabled 未置位: %+v", st)
	}
	if st.ManualReason != "观察几天" {
		t.Errorf("manual_reason=%q want %q", st.ManualReason, "观察几天")
	}
	if st.Disabled {
		t.Error("手动停用不应置自动禁用位")
	}

	// 不参与正常选号
	if got := p.Pick(""); got != nil {
		t.Fatalf("停用后不应被选中, got %+v", got)
	}
	// 也不参与全冷却兜底（pickEarliestExpiryLocked 路径）
	if got := p.pick(nil, "", ""); got != nil {
		t.Fatalf("停用后不应参与兜底选号, got %+v", got)
	}

	// 计数口径：total 含它、healthy 不含、disabled 含（与自动禁用同归一类）
	total, healthy, _, disabled, _ := p.CountsDetailed()
	if total != 1 || healthy != 0 || disabled != 1 {
		t.Fatalf("计数 = total%d healthy%d disabled%d, want 1/0/1", total, healthy, disabled)
	}

	// 恢复后立即可选
	if _, changed := p.SetManualDisabled("u1", false, ""); !changed {
		t.Fatal("恢复应报告状态变化")
	}
	if got := p.Pick(""); got == nil || got.UID != "u1" {
		t.Fatalf("恢复后应回到池子, got %+v", got)
	}
}

// TestManualDisabledIsIdempotent 重复调用幂等：第二次不报告变化、不报错。
func TestManualDisabledIsIdempotent(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	if _, changed := p.SetManualDisabled("u1", true, "r1"); !changed {
		t.Fatal("首次停用应报告变化")
	}
	if found, changed := p.SetManualDisabled("u1", true, "r1"); !found || changed {
		t.Fatalf("重复停用 = (%v,%v), want (true,false)", found, changed)
	}
	// 同状态但原因不同 → 仍算变化（文案要更新给运维看）
	if _, changed := p.SetManualDisabled("u1", true, "r2"); !changed {
		t.Fatal("原因变化应报告变化")
	}
	if _, reason, _ := p.ManualDisabledState("u1"); reason != "r2" {
		t.Errorf("reason=%q want r2", reason)
	}
	// 未知 uid：found=false
	if found, _ := p.SetManualDisabled("nope", true, "x"); found {
		t.Error("未知 uid 应返回 found=false")
	}
}

// TestManualDisabledIndependentFromAutoDisable 与自动禁用互相独立：
// 签到解冻（ReenableIfCredits）与 refresh 复活（ReviveDisabled）都不解除手动停用。
func TestManualDisabledIndependentFromAutoDisable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	// 先被系统自动禁用（连续 12153 达阈）
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatal("precondition: 应已自动禁用")
	}
	// 再叠加手动停用
	p.SetManualDisabled("u1", true, "叠加")

	st, _ := p.Status("u1")
	if !st.Disabled || !st.ManualDisabled {
		t.Fatalf("叠加态应两位都为真: %+v", st)
	}
	if st.DisabledReason != "12153 session dead" && st.DisabledReason == "" {
		t.Errorf("叠加态应同时透出自动禁用原因, got %q", st.DisabledReason)
	}
	if st.ManualReason != "叠加" {
		t.Errorf("叠加态应同时透出手动原因, got %q", st.ManualReason)
	}

	// 系统侧复活：只清自动位，手动位保留 → 仍不可选
	p.ReviveDisabled("u1")
	st, _ = p.Status("u1")
	if st.Disabled {
		t.Error("revive 应清自动禁用位")
	}
	if !st.ManualDisabled {
		t.Error("revive 不应清手动停用位（运维意图）")
	}
	if got := p.Pick(""); got != nil {
		t.Fatalf("手动位仍在，不应可选, got %+v", got)
	}

	// 签到解冻路径同样不解除手动停用
	p.ReenableIfCredits("u1", 100)
	st, _ = p.Status("u1")
	if !st.ManualDisabled {
		t.Fatal("签到解冻不应解除手动停用")
	}
	if got := p.Pick(""); got != nil {
		t.Fatalf("签到回血后仍应保持摘除, got %+v", got)
	}

	// 只有显式 enable 才回池
	p.SetManualDisabled("u1", false, "")
	if got := p.Pick(""); got == nil {
		t.Fatal("enable 后应回池")
	}
}

// TestManualDisabledPreservesCoolingDimensions 停用不碰冷却/熔断维度：
// 恢复后拿到的是停用期间真实发生的状态，而不是被清空的一刀切。
func TestManualDisabledPreservesCoolingDimensions(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	// 制造软冷却 + 一次失败计数
	p.Cooldown("u1", CoolSoft, 10*time.Minute, "429 rate limit")
	p.NoteError("u1")

	before, _ := p.Status("u1")
	p.SetManualDisabled("u1", true, "临时摘除")
	after, _ := p.Status("u1")

	if after.Until != before.Until {
		t.Errorf("停用不应改冷却截止: %v → %v", before.Until, after.Until)
	}
	if after.BreakerFails != before.BreakerFails {
		t.Errorf("停用不应改熔断计数: %d → %d", before.BreakerFails, after.BreakerFails)
	}
}

// TestManualDisabledPersists 手动停用落盘持久化——重启保留运维意图
// （这正是该功能要解决的痛点：旧权宜做法改 state.json 会被 5s flush 覆盖）。
func TestManualDisabledPersists(t *testing.T) {
	dir := t.TempDir()
	state := dir + "/state.json"

	p := New(state)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetManualDisabled("u1", true, "重启也要保留")
	p.Flush()

	// 模拟重启：新池读同一 state 文件
	p2 := New(state)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.Add(&auth.Auth{UID: "u2"})

	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("no status after reload")
	}
	if !st.ManualDisabled {
		t.Fatal("重启后手动停用状态丢失")
	}
	if st.ManualReason != "重启也要保留" {
		t.Errorf("manual_reason=%q 未保留", st.ManualReason)
	}
	// 未停用的账号不受影响
	if st2, _ := p2.Status("u2"); st2.ManualDisabled {
		t.Error("u2 不应被连带停用")
	}
	// 重启后仍不可选
	if got := p2.Pick(""); got == nil || got.UID != "u2" {
		t.Fatalf("重启后应只能选到 u2, got %+v", got)
	}
	// enable 后落盘，再重启确认已清
	p2.SetManualDisabled("u1", false, "")
	p2.Flush()
	p3 := New(state)
	p3.Add(&auth.Auth{UID: "u1"})
	if st3, _ := p3.Status("u1"); st3.ManualDisabled {
		t.Fatal("清除后重启不应复活手动停用位")
	}
}

// TestManualDisabledRealmScoped 停用某个域的账号不影响另一域。
func TestManualDisabledRealmScoped(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "gl1", Domain: "www.workbuddy.ai"})

	p.SetManualDisabled("cn1", true, "只摘 CN")

	if got := p.pick(nil, "", "cn"); got != nil {
		t.Fatalf("CN 域应无可选账号, got %+v", got)
	}
	if got := p.pick(nil, "", "global"); got == nil || got.UID != "gl1" {
		t.Fatalf("Global 域应不受影响, got %+v", got)
	}
}
