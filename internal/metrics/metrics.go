// Package metrics 按模型维度的请求量/token/缓存命中/扣费统计。
//
// 设计取态：网关是唯一能看到**所有**请求（含绕过面板的其他客户端）的位置，
// 因此统计在这里采集，通过 /v1/stats 暴露给面板。
//
// 为什么按模型分组：不同模型的定价、上下文长度、缓存行为差异极大
// （cache 命中率直接影响实际扣费），混在一起看没有决策价值。
package metrics

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ModelStats 单个模型的累计统计。
//
// 所有计数都是**累计值**（进程启动至今），面板取两次快照的差值即可算速率。
type ModelStats struct {
	Model string `json:"model"`

	// ── 请求量 ──────────────────────────────────────────
	Requests  int64 `json:"requests"`  // 总请求数
	Success   int64 `json:"success"`   // 2xx
	Failed    int64 `json:"failed"`    // 非 2xx / 传输失败
	Streaming int64 `json:"streaming"` // 其中流式请求数

	// ── 延迟（毫秒，累计和，由面板算平均）──────────────
	// 用累计和而非滑动窗口：无需后台 goroutine，重启后仍能从持久化恢复。
	TTFBSumMS    int64 `json:"ttfb_sum_ms"`    // 首字延迟累加（仅流式有值）
	TTFBCount    int64 `json:"ttfb_count"`     // 有 TTFB 采样的请求数
	LatencySumMS int64 `json:"latency_sum_ms"` // 端到端耗时累加
	LatencyCount int64 `json:"latency_count"`

	// ── Token ──────────────────────────────────────────
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	UsageReported    int64 `json:"usage_reported"` // 带 usage 的请求数（算平均值用）

	// ── 缓存（prompt cache）────────────────────────────
	// 上游按 prompt_cache_hit/miss 区分计费，命中部分通常便宜得多。
	CacheHitTokens   int64 `json:"cache_hit_tokens"`
	CacheMissTokens  int64 `json:"cache_miss_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	// CacheReadTokens/CacheCreationTokens 是另一套命名（部分模型用），一并记录。
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`

	// ── 扣费 ───────────────────────────────────────────
	// CreditMilli 用「毫」为单位累计（上游 credit 是两位小数），避免浮点误差。
	CreditMilli int64 `json:"credit_milli"`

	// ── 吞吐（由面板按 token/耗时算）───────────────────
	// 这里额外记录"有首字到结束"的时长，用于算纯生成速率（剔除排队等待）。
	GenSumMS int64 `json:"gen_sum_ms"` // 首字→结束的毫秒累加
	GenCount int64 `json:"gen_count"`

	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// Snapshot 一次采样的完整视图。
type Snapshot struct {
	// Models 按模型名索引的累计统计。
	Models map[string]*ModelStats `json:"models"`
	// Total 全模型汇总（便于面板直接展示总量）。
	Total ModelStats `json:"total"`
	// Since 统计起点（进程启动或上次重置）。
	Since time.Time `json:"since"`
	// Now 快照时刻（面板用它和 Since 算运行时长）。
	Now time.Time `json:"now"`
}

// delta 单个请求的观测值，由 handler 填充。
type Delta struct {
	Model    string
	Stream   bool
	OK       bool
	TTFB     time.Duration // 流式首字延迟；同步请求为 0
	Latency  time.Duration // 端到端
	HasUsage bool

	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64

	CacheHitTokens      int64
	CacheMissTokens     int64
	CacheWriteTokens    int64
	CacheReadTokens     int64
	CacheCreationTokens int64

	Credit float64
}

// Collector 线程安全的统计收集器。
type Collector struct {
	mu     sync.Mutex
	models map[string]*ModelStats
	since  time.Time

	// stateFile 非空时落盘（重启后累计值不丢）。
	stateFile string
	// dirty 有未落盘变更。
	dirty bool
	// flushEvery 每 N 次记录落盘一次（避免每个请求都写盘）。
	flushEvery int
	sinceFlush int
}

// New 构建收集器；stateFile 为空表示纯内存（不持久化）。
func New(stateFile string) *Collector {
	c := &Collector{
		models:     map[string]*ModelStats{},
		since:      time.Now(),
		stateFile:  stateFile,
		flushEvery: 20,
	}
	if stateFile != "" {
		c.load()
	}
	return c
}

// Record 记录一次请求。
func (c *Collector) Record(d Delta) {
	model := d.Model
	if model == "" {
		model = "(unknown)"
	}
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	m, ok := c.models[model]
	if !ok {
		m = &ModelStats{Model: model, FirstSeen: now}
		c.models[model] = m
	}
	m.LastSeen = now

	m.Requests++
	if d.OK {
		m.Success++
	} else {
		m.Failed++
	}
	if d.Stream {
		m.Streaming++
	}

	if d.TTFB > 0 {
		m.TTFBSumMS += d.TTFB.Milliseconds()
		m.TTFBCount++
	}
	if d.Latency > 0 {
		ms := d.Latency.Milliseconds()
		m.LatencySumMS += ms
		m.LatencyCount++
		// 生成时长 = 端到端 - 首字等待（流式才有意义）。
		if d.TTFB > 0 && d.Latency > d.TTFB {
			m.GenSumMS += (d.Latency - d.TTFB).Milliseconds()
			m.GenCount++
		}
	}

	if d.HasUsage {
		m.UsageReported++
		m.PromptTokens += d.PromptTokens
		m.CompletionTokens += d.CompletionTokens
		m.TotalTokens += d.TotalTokens
		m.CacheHitTokens += d.CacheHitTokens
		m.CacheMissTokens += d.CacheMissTokens
		m.CacheWriteTokens += d.CacheWriteTokens
		m.CacheReadTokens += d.CacheReadTokens
		m.CacheCreationTokens += d.CacheCreationTokens
	}
	// credit 以「毫」累计：上游给两位小数（0.02），×1000 后是整数。
	if d.Credit != 0 {
		m.CreditMilli += int64(d.Credit*1000 + 0.5)
	}

	c.dirty = true
	c.sinceFlush++
	if c.stateFile != "" && c.sinceFlush >= c.flushEvery {
		c.saveLocked()
	}
}

// Snapshot 返回当前累计统计的深拷贝（含全模型汇总）。
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := Snapshot{
		Models: make(map[string]*ModelStats, len(c.models)),
		Since:  c.since,
		Now:    time.Now(),
	}
	total := &ModelStats{Model: "(all)"}
	for name, m := range c.models {
		cp := *m
		out.Models[name] = &cp
		addInto(total, &cp)
	}
	out.Total = *total
	return out
}

// Reset 清空统计（面板"重置统计"用）。
func (c *Collector) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.models = map[string]*ModelStats{}
	c.since = time.Now()
	c.dirty = true
	c.saveLocked()
}

// Flush 强制落盘（进程退出前调用）。
func (c *Collector) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dirty {
		c.saveLocked()
	}
}

// addInto 把 src 的计数累加进 dst（汇总用）。
func addInto(dst, src *ModelStats) {
	dst.Requests += src.Requests
	dst.Success += src.Success
	dst.Failed += src.Failed
	dst.Streaming += src.Streaming
	dst.TTFBSumMS += src.TTFBSumMS
	dst.TTFBCount += src.TTFBCount
	dst.LatencySumMS += src.LatencySumMS
	dst.LatencyCount += src.LatencyCount
	dst.PromptTokens += src.PromptTokens
	dst.CompletionTokens += src.CompletionTokens
	dst.TotalTokens += src.TotalTokens
	dst.UsageReported += src.UsageReported
	dst.CacheHitTokens += src.CacheHitTokens
	dst.CacheMissTokens += src.CacheMissTokens
	dst.CacheWriteTokens += src.CacheWriteTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.CacheCreationTokens += src.CacheCreationTokens
	dst.CreditMilli += src.CreditMilli
	dst.GenSumMS += src.GenSumMS
	dst.GenCount += src.GenCount
	if dst.FirstSeen.IsZero() || (!src.FirstSeen.IsZero() && src.FirstSeen.Before(dst.FirstSeen)) {
		dst.FirstSeen = src.FirstSeen
	}
	if src.LastSeen.After(dst.LastSeen) {
		dst.LastSeen = src.LastSeen
	}
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

type stateFile struct {
	Since  time.Time              `json:"since"`
	Models map[string]*ModelStats `json:"models"`
}

// saveLocked 原子落盘。调用方必须已持锁。
// 落盘失败只打日志不阻断（统计是观测功能，不应影响转发）。
func (c *Collector) saveLocked() {
	c.dirty = false
	c.sinceFlush = 0
	if c.stateFile == "" {
		return
	}
	raw, err := json.MarshalIndent(stateFile{Since: c.since, Models: c.models}, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(c.stateFile); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := c.stateFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, c.stateFile)
}

// load 启动时恢复累计统计（文件缺失/损坏时静默从零开始）。
func (c *Collector) load() {
	raw, err := os.ReadFile(c.stateFile)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	if sf.Models != nil {
		c.models = sf.Models
	}
	if !sf.Since.IsZero() {
		c.since = sf.Since
	}
}

// ---------------------------------------------------------------------------
// 展示辅助（面板可直接用的派生指标）
// ---------------------------------------------------------------------------

// Derived 单模型的派生指标（平均/速率），面板表格直接用。
type Derived struct {
	Model string `json:"model"`

	Requests  int64 `json:"requests"`
	Success   int64 `json:"success"`
	Failed    int64 `json:"failed"`
	Streaming int64 `json:"streaming"`

	// AvgTTFBMS 平均首字延迟（毫秒）；无采样为 0。
	AvgTTFBMS float64 `json:"avg_ttfb_ms"`
	// AvgLatencyMS 平均端到端耗时（毫秒）。
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	// TokensPerSec 生成速率 = 输出 token / 生成秒数（剔除首字等待）。
	TokensPerSec float64 `json:"tokens_per_sec"`

	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	CacheHitTokens   int64 `json:"cache_hit_tokens"`
	CacheMissTokens  int64 `json:"cache_miss_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	// CacheHitRate 缓存命中率 = hit / (hit + miss)，无数据为 0。
	CacheHitRate float64 `json:"cache_hit_rate"`

	// Credit 累计扣费（元/积分，两位小数）。
	Credit float64 `json:"credit"`
	// CreditPerReq 平均每请求扣费。
	CreditPerReq float64 `json:"credit_per_req"`

	LastSeen *time.Time `json:"last_seen,omitempty"`
}

// Derive 把累计统计换算成派生指标。
func Derive(m *ModelStats) Derived {
	d := Derived{
		Model:            m.Model,
		Requests:         m.Requests,
		Success:          m.Success,
		Failed:           m.Failed,
		Streaming:        m.Streaming,
		PromptTokens:     m.PromptTokens,
		CompletionTokens: m.CompletionTokens,
		TotalTokens:      m.TotalTokens,
		CacheHitTokens:   m.CacheHitTokens + m.CacheReadTokens,
		CacheMissTokens:  m.CacheMissTokens,
		CacheWriteTokens: m.CacheWriteTokens + m.CacheCreationTokens,
		Credit:           float64(m.CreditMilli) / 1000,
	}
	if m.TTFBCount > 0 {
		d.AvgTTFBMS = float64(m.TTFBSumMS) / float64(m.TTFBCount)
	}
	if m.LatencyCount > 0 {
		d.AvgLatencyMS = float64(m.LatencySumMS) / float64(m.LatencyCount)
	}
	// 生成速率：优先用"首字→结束"的纯生成时长；无 TTFB 采样时退回端到端耗时。
	if m.GenCount > 0 && m.GenSumMS > 0 {
		d.TokensPerSec = float64(m.CompletionTokens) / (float64(m.GenSumMS) / 1000)
	} else if m.LatencySumMS > 0 {
		d.TokensPerSec = float64(m.CompletionTokens) / (float64(m.LatencySumMS) / 1000)
	}
	if total := d.CacheHitTokens + d.CacheMissTokens; total > 0 {
		d.CacheHitRate = float64(d.CacheHitTokens) / float64(total)
	}
	if m.Requests > 0 {
		d.CreditPerReq = d.Credit / float64(m.Requests)
	}
	if !m.LastSeen.IsZero() {
		t := m.LastSeen
		d.LastSeen = &t
	}
	return d
}

// DerivedSnapshot 面板用的完整派生视图。
type DerivedSnapshot struct {
	Models []Derived `json:"models"`
	Total  Derived   `json:"total"`
	Since  time.Time `json:"since"`
	Now    time.Time `json:"now"`
	// UptimeSec 统计持续时间（秒），面板算速率用。
	UptimeSec int64 `json:"uptime_sec"`
}

// Derived 生成面板视图（模型按请求数降序）。
func (c *Collector) Derived() DerivedSnapshot {
	snap := c.Snapshot()
	out := DerivedSnapshot{
		Models: make([]Derived, 0, len(snap.Models)),
		Since:  snap.Since,
		Now:    snap.Now,
	}
	for _, m := range snap.Models {
		out.Models = append(out.Models, Derive(m))
	}
	sort.Slice(out.Models, func(i, j int) bool {
		if out.Models[i].Requests != out.Models[j].Requests {
			return out.Models[i].Requests > out.Models[j].Requests
		}
		return out.Models[i].Model < out.Models[j].Model
	})
	out.Total = Derive(&snap.Total)
	out.Total.Model = "(all)"
	if !snap.Since.IsZero() {
		out.UptimeSec = int64(snap.Now.Sub(snap.Since).Seconds())
	}
	return out
}

// String 便于日志调试。
func (d Derived) String() string {
	return fmt.Sprintf("%s req=%d ttfb=%.0fms tok/s=%.1f cache=%.0f%% credit=%.2f",
		d.Model, d.Requests, d.AvgTTFBMS, d.TokensPerSec, d.CacheHitRate*100, d.Credit)
}
