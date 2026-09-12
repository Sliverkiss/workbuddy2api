package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestNoteModelCostAndPreferFree 成本账本的核心验收：同一模型上已实测免费的号
// 必须优先于已实测收费的号，且这个优先级要压过积分因子。
func TestNoteModelCostAndPreferFree(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "free"})
	p.Add(&auth.Auth{UID: "paid"})
	// paid 积分远高于 free：若只看积分，paid 必胜；成本分层必须压过积分。
	p.SetCredits("free", 1)
	p.SetCredits("paid", 1_000_000)

	p.NoteModelCost("free", "hy4-preview", 0, 1000)   // 免费
	p.NoteModelCost("paid", "hy4-preview", 2.9, 1000) // 收费

	for i := 0; i < 50; i++ {
		a := p.PickExcludingForModel(nil, "hy4-preview")
		if a == nil || a.UID != "free" {
			t.Fatalf("第 %d 次选中 %v，want free（免费号应优先于高积分收费号）", i, a)
		}
	}
}

// TestModelCostCheaperPaidWins 同为收费时，单价低的优先。
func TestModelCostCheaperPaidWins(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "cheap"})
	p.Add(&auth.Auth{UID: "pricey"})
	p.NoteModelCost("cheap", "hy4-preview", 0.3, 1000)
	p.NoteModelCost("pricey", "hy4-preview", 5.0, 1000)
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForModel(nil, "hy4-preview")
		if a == nil || a.UID != "cheap" {
			t.Fatalf("选中 %v, want cheap（单价低的优先）", a)
		}
	}
}

// TestModelCostUnknownBeatsKnownPaid 无观测的号优先于已知收费的号。
// 这是学习闭环的前提：新号的限免状态只能靠实测发现，若已知收费的号恒压过未知号，
// 那台免费的号永远轮不到，也就永远学不到"它其实是免费的"。
func TestModelCostUnknownBeatsKnownPaid(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "unknown"})
	p.Add(&auth.Auth{UID: "paid"})
	p.SetCredits("unknown", 1)
	p.SetCredits("paid", 1_000_000)
	p.NoteModelCost("paid", "hy4-preview", 2.9, 1000)

	for i := 0; i < 50; i++ {
		a := p.PickExcludingForModel(nil, "hy4-preview")
		if a == nil || a.UID != "unknown" {
			t.Fatalf("选中 %v, want unknown（未知号需有机会被实测）", a)
		}
	}
}

// TestModelCostFreeBeatsUnknown 已确认免费的号压过未观测的号。
func TestModelCostFreeBeatsUnknown(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "knownfree"})
	p.Add(&auth.Auth{UID: "unknown"})
	p.SetCredits("knownfree", 1)
	p.SetCredits("unknown", 1_000_000)
	p.NoteModelCost("knownfree", "hy4-preview", 0, 1000)

	for i := 0; i < 50; i++ {
		a := p.PickExcludingForModel(nil, "hy4-preview")
		if a == nil || a.UID != "knownfree" {
			t.Fatalf("选中 %v, want knownfree（已确认免费 > 未知）", a)
		}
	}
}

// TestModelCostStaleIgnored 过期观测失效：防止"夜间免费"在白天仍被当作免费
// （否则会一直用那个已开始收费的号）。
func TestModelCostStaleIgnored(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelCost("u1", "hy4-preview", 0, 1000)
	p.mu.Lock()
	mc := p.byUID["u1"].modelCost["hy4-preview"]
	mc.LastSeen = time.Now().Add(-2 * modelCostTTL) // 手工做旧
	p.byUID["u1"].modelCost["hy4-preview"] = mc
	p.mu.Unlock()

	p.mu.RLock()
	_, ok := p.byUID["u1"].modelCostOf("hy4-preview", time.Now())
	p.mu.RUnlock()
	if ok {
		t.Error("过期的成本观测应失效（夜间免费白天不该仍算免费）")
	}
}

// TestModelCostEMASmoothing 观测按 EMA 平滑，单次异常不主导决策。
func TestModelCostEMASmoothing(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelCost("u1", "m", 1.0, 1000) // 首次：直接取实测
	p.mu.RLock()
	first := p.byUID["u1"].modelCost["m"].CostPer1k
	p.mu.RUnlock()
	if first != 1.0 {
		t.Fatalf("首次观测 CostPer1k=%v want 1.0", first)
	}
	p.NoteModelCost("u1", "m", 3.0, 1000) // 第二次：EMA 拉向 3.0 但不等于 3.0
	p.mu.RLock()
	second := p.byUID["u1"].modelCost["m"].CostPer1k
	samples := p.byUID["u1"].modelCost["m"].Samples
	p.mu.RUnlock()
	if second <= 1.0 || second >= 3.0 {
		t.Errorf("EMA 后 CostPer1k=%v, want 介于 1.0 与 3.0 之间", second)
	}
	if samples != 2 {
		t.Errorf("Samples=%d want 2", samples)
	}
}

// TestModelCostIgnoresInvalidInput 无法折算单价的输入不应写入账本（防污染）。
func TestModelCostIgnoresInvalidInput(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelCost("u1", "m", 5.0, 0)  // tokens=0
	p.NoteModelCost("", "m", 5.0, 1000) // 空 uid
	p.NoteModelCost("u1", "", 5.0, 1000)
	p.mu.RLock()
	n := len(p.byUID["u1"].modelCost)
	p.mu.RUnlock()
	if n != 0 {
		t.Errorf("非法输入不应写入成本账本，len=%d", n)
	}
}

// TestModelCostEmptyModelUnaffected 空模型名（PickExcluding 路径）不触发成本分层，
// 保持原三因子选号行为。
func TestModelCostEmptyModelUnaffected(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelCost("u1", "hy4-preview", 5.0, 1000)
	if a := p.PickExcludingForModel(nil, ""); a == nil {
		t.Error("空模型名不应被成本分层影响（应仍能选出账号）")
	}
}

// TestPickByUIDForModel 模型级可用性：绑定号在被限额的模型上返回 nil，
// 在其他模型上照常返回——handler 据此决定解绑还是沿用。
func TestPickByUIDForModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 触发一次带解析时间的 6004 冷却：softRateModel=hy4-preview，
	// 该账号对 hy4-preview 冷却、对其他模型豁免（issue #31 语义）。
	reset := time.Now().Add(time.Hour)
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "hy4-preview", "6004 model rate limit")

	// 先确认冷却真的落上了（否则后面两个断言是假阳性）。
	p.mu.RLock()
	recorded := p.byUID["u1"].softRateModel
	p.mu.RUnlock()
	if recorded != "hy4-preview" {
		t.Fatalf("softRateModel=%q want hy4-preview（6004 模型级冷却未记录模型）", recorded)
	}

	if a := p.PickByUIDForModel("u1", "hy4-preview"); a != nil {
		t.Error("被限额的模型上，PickByUIDForModel 应返回 nil（让 handler 解绑轮换）")
	}
	if a := p.PickByUIDForModel("u1", "glm-5.3"); a == nil {
		t.Error("其他模型应照常可用（模型豁免生效）")
	}
}

// TestAvailableUIDsForModel 按模型过滤可用账号：被该模型限额的号不列入，
// 被其他模型限额的号照常列入。
func TestAvailableUIDsForModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(time.Hour), "hy4-preview", "6004")

	uids := p.AvailableUIDsForModel("hy4-preview")
	if len(uids) != 1 || uids[0] != "u2" {
		t.Errorf("AvailableUIDsForModel(hy4-preview)=%v want [u2]", uids)
	}
	all := p.AvailableUIDsForModel("glm-5.3")
	if len(all) != 2 {
		t.Errorf("AvailableUIDsForModel(glm-5.3)=%v want 两个账号（u1 对其他模型豁免）", all)
	}
}
