package pool

import (
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestNoteSessionDeadThresholdNotReached 前 2 次连续 12153 不 Disable（误判防护）。
func TestNoteSessionDeadThresholdNotReached(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if p.NoteSessionDead("u1") {
		t.Fatal("第 1 次 12153 不应禁用")
	}
	if p.NoteSessionDead("u1") {
		t.Fatal("第 2 次 12153 不应禁用")
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if st.Disabled {
		t.Fatalf("连续 2 次 12153 不应禁用: %+v", st)
	}
	// 未达阈值时账号仍可选（keepalive 失败不污染选号）。
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("账号应保持可选, got %+v", got)
	}
}

// TestNoteSessionDeadDisablesAtThird 连续第 3 次 12153 → 禁用并清计数。
func TestNoteSessionDeadDisablesAtThird(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	if !p.NoteSessionDead("u1") {
		t.Fatal("第 3 次 12153 应禁用")
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("第 3 次后应 disabled: %+v", st)
	}
	if st.Reason != "12153 session dead" {
		t.Errorf("reason=%q want 12153 session dead", st.Reason)
	}
	// 禁用后不再可选。
	if p.Pick() != nil {
		t.Fatal("禁用账号不可被选中")
	}
}

// TestClearSessionDeadResetsCount 中间成功（refresh 成功）清计数，后续从 1 重新计。
func TestClearSessionDeadResetsCount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.ClearSessionDead("u1") // 模拟 refresh 成功：清计数
	if p.NoteSessionDead("u1") {
		t.Fatal("清计数后第 1 次不应禁用")
	}
	if p.NoteSessionDead("u1") {
		t.Fatal("清计数后第 2 次不应禁用")
	}
	if !p.NoteSessionDead("u1") {
		t.Fatal("清计数后第 3 次应禁用（从 1 重新计够 3 次）")
	}
}

// TestNoteSuccessClearsSessionDeadCount 任意成功（chat 成功）也是 session 未死的强证据，
// 同样清计数——与 refresh 成功口径一致。
func TestNoteSuccessClearsSessionDeadCount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.NoteSuccess("u1")
	if p.NoteSessionDead("u1") {
		t.Fatal("成功清计数后第 1 次不应禁用")
	}
}

// TestReviveDisabled 复活入口：清 disabled + reason + 误判计数，账号回到池子。
func TestReviveDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1") // 触发禁用
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatal("precondition: 应已禁用")
	}
	p.ReviveDisabled("u1")
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if st.Disabled {
		t.Fatalf("revive 应清 disabled: %+v", st)
	}
	if st.Reason != "" {
		t.Errorf("reason=%q want 空（revive 清 reason）", st.Reason)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("复活后账号应回到池子, got %+v", got)
	}
	// 误判计数一并清零：复活后重新计满 3 次才禁用。
	p.NoteSessionDead("u1")
	if p.NoteSessionDead("u1") {
		t.Fatal("复活后第 2 次不应禁用（应从新计数）")
	}
	if !p.NoteSessionDead("u1") {
		t.Fatal("复活后第 3 次应禁用（从新计数够 3 次）")
	}
}

// TestReviveDisabledPersists 复活清 disabled + reason 落盘持久化（重启后不回退）。
func TestReviveDisabledPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p.ReviveDisabled("u1")
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Disabled || st.Reason != "" {
		t.Fatalf("revive 应持久化（disabled=%v reason=%q）ok=%v", st.Disabled, st.Reason, ok)
	}
}

// TestStatusDisabledReasonDisabled when disabled, Status 透出 disabled_reason。
func TestStatusDisabledReasonDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	st, _ := p.Status("u1")
	if !st.Disabled || st.DisabledReason != "12153 session dead" {
		t.Errorf("disabled_reason=%q want 12153 session dead (disabled=%v)", st.DisabledReason, st.Disabled)
	}
}

// TestStatusDisabledReasonClearedByRevive 复活后 disabled_reason 归空。
func TestStatusDisabledReasonClearedByRevive(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p.ReviveDisabled("u1")
	st, _ := p.Status("u1")
	if st.DisabledReason != "" {
		t.Errorf("revive 后 disabled_reason=%q want 空", st.DisabledReason)
	}
	if st.Reason != "" {
		t.Errorf("revive 后 reason=%q want 空", st.Reason)
	}
}