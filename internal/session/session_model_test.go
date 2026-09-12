package session

import (
	"testing"
	"time"
)

// TestResolveForModelReassignsOnModelLimit 核心语义：会话绑定的账号在**该模型**上
// 不可用时，必须重分配——即使它在别的模型上仍然可用。
//
// 回归背景：粘性绑定只记 uid，而同一个会话可能换模型。账号被 6004 模型级限额后
// 对其他模型仍可用（pool.healthyForModel 的 softRateModel 豁免），此时若只按账号级
// 可用性校验，会话会被钉在这个号上反复失败——正是"限额后换不动号"的观感来源。
func TestResolveForModelReassignsOnModelLimit(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	// 模型维度可用性：hy4-preview 上只剩 a2（a1 被 6004 限额）；其他模型两个都在。
	r.cfg.AvailableForModel = func(model string) []string {
		if model == "hy4-preview" {
			return []string{"a2"}
		}
		return []string{"a1", "a2"}
	}

	// 模拟该会话此前在别的模型上绑定了 a1。
	r.Bind("c1", "a1")

	// 在 hy4-preview 上解析：a1 已限额 → 必须重分配到 a2。
	got, ok := r.ResolveForModel("c1", "hy4-preview")
	if !ok {
		t.Fatal("a1 在目标模型被限额，应能重分配到 a2")
	}
	if got != "a2" {
		t.Errorf("ResolveForModel(hy4-preview)=%s want a2（绑定号在该模型不可用）", got)
	}

	// 同一会话切换到未限额的模型：a1 仍应可用（模型级限额不污染其他模型）。
	r.Bind("c2", "a1")
	got2, ok2 := r.ResolveForModel("c2", "glm-5.3")
	if !ok2 || got2 != "a1" {
		t.Errorf("ResolveForModel(glm-5.3)=%s ok=%v want a1/true（不应被其他模型的限额影响）", got2, ok2)
	}
}

// TestResolveForModelFallsBackToAvailable 未注入 AvailableForModel 时回落 Available
// （无模型维度，行为与引入前一致）。
func TestResolveForModelFallsBackToAvailable(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1"}, time.Minute)
	got, ok := r.ResolveForModel("c1", "any-model")
	if !ok || got != "a1" {
		t.Errorf("无 AvailableForModel 时应回落 Available，got %s ok=%v", got, ok)
	}
}

// TestResolveForModelNoAvailableReturnsFalse 目标模型上无可用账号时返回 ok=false，
// 让 handler 回落普通轮换，而不是分配一个必然失败的号。
func TestResolveForModelNoAvailableReturnsFalse(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1"}, time.Minute)
	r.cfg.AvailableForModel = func(model string) []string {
		if model == "hy4-preview" {
			return nil
		}
		return []string{"a1"}
	}
	if _, ok := r.ResolveForModel("c1", "hy4-preview"); ok {
		t.Error("该模型无可用账号时应返回 ok=false")
	}
}

// TestResolveKeepsOldBehavior Resolve() 保留无模型语义（等价于空模型名），
// 保证既有调用方行为不变。
func TestResolveKeepsOldBehavior(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1"}, time.Minute)
	r.cfg.AvailableForModel = func(model string) []string { return []string{"a1"} }
	got, ok := r.Resolve("c1")
	if !ok || got != "a1" {
		t.Errorf("Resolve()=%s ok=%v want a1/true", got, ok)
	}
}
