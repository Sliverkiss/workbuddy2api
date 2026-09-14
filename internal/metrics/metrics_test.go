package metrics

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestRecordAggregatesByModel 统计必须按模型分开累计，且各计数正确。
func TestRecordAggregatesByModel(t *testing.T) {
	c := New("")

	c.Record(Delta{
		Model: "glm-5.1", Stream: true, OK: true,
		TTFB: 500 * time.Millisecond, Latency: 2 * time.Second,
		HasUsage: true, PromptTokens: 10, CompletionTokens: 100, TotalTokens: 110,
		CacheHitTokens: 40, CacheMissTokens: 60, Credit: 0.02,
	})
	c.Record(Delta{
		Model: "glm-5.1", Stream: false, OK: true,
		Latency:  1 * time.Second,
		HasUsage: true, PromptTokens: 5, CompletionTokens: 50, TotalTokens: 55,
		CacheHitTokens: 0, CacheMissTokens: 5, Credit: 0.01,
	})
	c.Record(Delta{
		Model: "deepseek-v4.1-flash", Stream: true, OK: false,
		TTFB: 300 * time.Millisecond, Latency: 500 * time.Millisecond,
	})

	snap := c.Snapshot()
	if len(snap.Models) != 2 {
		t.Fatalf("应有 2 个模型, 得到 %d", len(snap.Models))
	}

	glm := snap.Models["glm-5.1"]
	if glm == nil {
		t.Fatal("glm-5.1 统计缺失")
	}
	if glm.Requests != 2 || glm.Success != 2 || glm.Streaming != 1 {
		t.Errorf("glm-5.1 请求计数错误: %+v", glm)
	}
	if glm.PromptTokens != 15 || glm.CompletionTokens != 150 || glm.TotalTokens != 165 {
		t.Errorf("glm-5.1 token 错误: prompt=%d completion=%d total=%d",
			glm.PromptTokens, glm.CompletionTokens, glm.TotalTokens)
	}
	if glm.CacheHitTokens != 40 || glm.CacheMissTokens != 65 {
		t.Errorf("glm-5.1 缓存错误: hit=%d miss=%d", glm.CacheHitTokens, glm.CacheMissTokens)
	}
	if glm.CreditMilli != 30 { // 0.02 + 0.01 = 0.03 → 30 毫
		t.Errorf("glm-5.1 扣费错误: %d 毫, want 30", glm.CreditMilli)
	}

	ds := snap.Models["deepseek-v4.1-flash"]
	if ds.Requests != 1 || ds.Failed != 1 {
		t.Errorf("失败计数错误: %+v", ds)
	}

	// 汇总应为两模型之和。
	if snap.Total.Requests != 3 {
		t.Errorf("汇总请求数 = %d, want 3", snap.Total.Requests)
	}
	if snap.Total.CompletionTokens != 150 {
		t.Errorf("汇总输出 token = %d, want 150", snap.Total.CompletionTokens)
	}
}

// TestDeriveComputesRates 派生指标（平均 TTFB / 吞吐 / 命中率）计算正确。
func TestDeriveComputesRates(t *testing.T) {
	m := &ModelStats{
		Model:    "glm-5.1",
		Requests: 2, Success: 2,
		TTFBSumMS: 1000, TTFBCount: 2, // 平均 500ms
		LatencySumMS: 3000, LatencyCount: 2, // 平均 1500ms
		GenSumMS: 2000, GenCount: 2, // 生成共 2s
		PromptTokens: 100, CompletionTokens: 200, TotalTokens: 300,
		CacheHitTokens: 300, CacheMissTokens: 100, // 命中率 75%
		CreditMilli: 30, // 0.03
	}
	d := Derive(m)

	if d.AvgTTFBMS != 500 {
		t.Errorf("平均 TTFB = %v, want 500", d.AvgTTFBMS)
	}
	if d.AvgLatencyMS != 1500 {
		t.Errorf("平均延迟 = %v, want 1500", d.AvgLatencyMS)
	}
	// 吞吐 = 200 token / 2s = 100 tok/s
	if d.TokensPerSec != 100 {
		t.Errorf("吞吐 = %v tok/s, want 100", d.TokensPerSec)
	}
	// 命中率 = 300/(300+100) = 0.75
	if d.CacheHitRate != 0.75 {
		t.Errorf("命中率 = %v, want 0.75", d.CacheHitRate)
	}
	if d.Credit != 0.03 {
		t.Errorf("扣费 = %v, want 0.03", d.Credit)
	}
	if d.CreditPerReq != 0.015 {
		t.Errorf("每请求扣费 = %v, want 0.015", d.CreditPerReq)
	}
}

// TestDeriveZeroSafe 零值不应产生 NaN/Inf（面板显示会是 "NaN"）。
func TestDeriveZeroSafe(t *testing.T) {
	d := Derive(&ModelStats{Model: "x"})
	if d.AvgTTFBMS != 0 || d.AvgLatencyMS != 0 || d.TokensPerSec != 0 || d.CacheHitRate != 0 || d.CreditPerReq != 0 {
		t.Errorf("零值派生应全为 0: %+v", d)
	}
}

// TestDerivedSortsByRequests 模型按请求数降序（面板首屏看热点模型）。
func TestDerivedSortsByRequests(t *testing.T) {
	c := New("")
	for i := 0; i < 5; i++ {
		c.Record(Delta{Model: "hot", OK: true})
	}
	for i := 0; i < 2; i++ {
		c.Record(Delta{Model: "cold", OK: true})
	}
	c.Record(Delta{Model: "mid", OK: true})
	c.Record(Delta{Model: "mid", OK: true})
	c.Record(Delta{Model: "mid", OK: true})

	d := c.Derived()
	want := []string{"hot", "mid", "cold"}
	for i, w := range want {
		if d.Models[i].Model != w {
			t.Errorf("排序[%d] = %s, want %s", i, d.Models[i].Model, w)
		}
	}
}

// TestUnknownModelBucketed 空模型名归入 (unknown)，避免 map 空键。
func TestUnknownModelBucketed(t *testing.T) {
	c := New("")
	c.Record(Delta{OK: true})
	snap := c.Snapshot()
	if _, ok := snap.Models["(unknown)"]; !ok {
		t.Errorf("空模型应归入 (unknown), 得到 %v", snap.Models)
	}
}

// TestPersistence 落盘后重新加载，累计值不丢。
func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "metrics.json")

	c := New(fp)
	// 写足够多让 flushEvery(20) 触发落盘。
	for i := 0; i < 25; i++ {
		c.Record(Delta{Model: "glm-5.1", OK: true, HasUsage: true, CompletionTokens: 10})
	}
	c.Flush()

	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("统计文件应已生成: %v", err)
	}

	c2 := New(fp)
	snap := c2.Snapshot()
	m := snap.Models["glm-5.1"]
	if m == nil {
		t.Fatal("重启后模型统计丢失")
	}
	if m.Requests != 25 || m.CompletionTokens != 250 {
		t.Errorf("重启后计数错误: requests=%d completion=%d", m.Requests, m.CompletionTokens)
	}
}

// TestPersistenceCorruptFileStartsFresh 文件损坏时从零开始，不 panic。
func TestPersistenceCorruptFileStartsFresh(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "metrics.json")
	if err := os.WriteFile(fp, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := New(fp)
	c.Record(Delta{Model: "m", OK: true})
	if snap := c.Snapshot(); snap.Models["m"].Requests != 1 {
		t.Errorf("损坏文件应被忽略并从零开始: %+v", snap.Models)
	}
}

// TestReset 重置清空全部统计。
func TestReset(t *testing.T) {
	c := New("")
	c.Record(Delta{Model: "m", OK: true})
	if len(c.Snapshot().Models) != 1 {
		t.Fatal("前置条件失败")
	}
	c.Reset()
	if snap := c.Snapshot(); len(snap.Models) != 0 || snap.Total.Requests != 0 {
		t.Errorf("重置后应为空: %+v", snap)
	}
}

// TestConcurrentRecord 并发记录不应丢数据或触发竞态（-race 下有完整意义）。
func TestConcurrentRecord(t *testing.T) {
	c := New("")
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.Record(Delta{
				Model: "m", OK: true, HasUsage: true,
				CompletionTokens: 1, PromptTokens: 1, TotalTokens: 2,
			})
		}(i)
	}
	wg.Wait()

	m := c.Snapshot().Models["m"]
	if m.Requests != n {
		t.Errorf("并发丢数据: requests=%d, want %d", m.Requests, n)
	}
	if m.CompletionTokens != n {
		t.Errorf("并发丢 token: %d, want %d", m.CompletionTokens, n)
	}
}

// TestSnapshotIsDeepCopy 快照必须是拷贝：修改快照不应影响内部状态。
func TestSnapshotIsDeepCopy(t *testing.T) {
	c := New("")
	c.Record(Delta{Model: "m", OK: true})
	snap := c.Snapshot()
	snap.Models["m"].Requests = 999
	if again := c.Snapshot(); again.Models["m"].Requests != 1 {
		t.Error("快照未深拷贝：外部修改泄漏进了内部状态")
	}
}

// TestTokensPerSecFallsBackToLatency 无 TTFB 采样（同步请求）时吞吐退回端到端耗时。
func TestTokensPerSecFallsBackToLatency(t *testing.T) {
	m := &ModelStats{
		CompletionTokens: 100,
		LatencySumMS:     2000, LatencyCount: 1, // 2s
		// GenCount = 0（无流式）
	}
	d := Derive(m)
	if d.TokensPerSec != 50 { // 100/2s
		t.Errorf("吞吐 = %v, want 50（应退回端到端耗时）", d.TokensPerSec)
	}
}

// TestCacheNamingVariantsBothCounted 两套缓存命名（OpenAI/Anthropic）都要计入。
func TestCacheNamingVariantsBothCounted(t *testing.T) {
	c := New("")
	// OpenAI 风格
	c.Record(Delta{Model: "m", OK: true, HasUsage: true,
		CacheHitTokens: 10, CacheMissTokens: 90})
	// Anthropic 风格（cache_read_input_tokens）
	c.Record(Delta{Model: "m", OK: true, HasUsage: true,
		CacheReadTokens: 50, CacheMissTokens: 50})

	d := Derive(c.Snapshot().Models["m"])
	// hit = 10 + 50 = 60, miss = 140 → 60/200 = 30%
	if d.CacheHitTokens != 60 {
		t.Errorf("命中 token = %d, want 60（两套命名都要计）", d.CacheHitTokens)
	}
	if d.CacheHitRate < 0.299 || d.CacheHitRate > 0.301 {
		t.Errorf("命中率 = %v, want 0.3", d.CacheHitRate)
	}
}
