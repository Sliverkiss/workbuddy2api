// metrics.go 请求统计聚合（/v1/stats 数据源）。
//
// 设计要点：
//   - **单一埋点**：唯一写入口是 chatStat.done()，流式/非流式/错误路径全汇于此，
//     天然覆盖全路径，不需要在每个 return 前重复记账。
//   - **只采信上游 usage**：token / cache / credit 一律来自上游末帧 usage，缺失时
//     用 hasUsage 区分「缺观测」与「显式 0」，不做 rune 估算（与成本账本同纪律）。
//   - **按模型聚合**：模型名含 realm 前缀原样入键（global:xxx 与裸名分开统计）。
//   - **有界内存**：模型键数量受上游目录限制（不是无界增长）；另设容量上限兜底，
//     超限时丢弃新键并记一次 WARN，避免异常模型名刷爆内存。
//   - **零外部依赖**：纯内存累加，进程重启即清零（since 随进程启动时间）。
package server

import (
	"log"
	"net/http"
	"sort"
	"sync"
	"time"
)

// metricsCap 模型键容量上限。上游目录规模远小于此值；上限只为兜底异常模型名。
const metricsCap = 512

// modelMetrics 单模型的累加器（全字段原子性由 metricsMu 保证，无需 atomic）。
type modelMetrics struct {
	requests  int64
	success   int64
	failed    int64
	streaming int64

	ttfbSumMS  float64 // TTFB 累计（仅成功且有观测的请求）
	ttfbCount  int64
	latSumMS   float64 // 端到端耗时累计（全部请求）
	genSecSum  float64 // 生成秒数累计（供 tokens/s）
	promptTok  int64
	compTok    int64
	cacheHit   int64
	cacheMiss  int64
	cacheWrite int64
	credit     float64

	lastSeen time.Time
}

// metricsStore 全局聚合表。
type metricsStore struct {
	mu      sync.Mutex
	since   time.Time
	byModel map[string]*modelMetrics
	warned  bool // 容量超限只告警一次，避免刷屏
}

var globalMetrics = &metricsStore{
	since:   time.Now(),
	byModel: make(map[string]*modelMetrics),
}

// recordChatMetric 把一次请求的观测累加进聚合表。由 chatStat.done() 调用。
//
// total 为端到端耗时（TTFB 与生成吞吐的分母口径均由此派生）。模型名为空/"-" 时
// 归入 "-" 键（仍计入 total，不丢弃观测）。
func recordChatMetric(s *chatStat, total time.Duration) {
	model := s.model
	if model == "" {
		model = "-"
	}

	m := globalMetrics
	m.mu.Lock()
	defer m.mu.Unlock()

	mm, ok := m.byModel[model]
	if !ok {
		if len(m.byModel) >= metricsCap {
			if !m.warned {
				m.warned = true
				logMetricsCapWarn(model)
			}
			return
		}
		mm = &modelMetrics{}
		m.byModel[model] = mm
	}

	mm.requests++
	if s.status == 200 {
		mm.success++
	} else {
		mm.failed++
	}
	if s.mode == "stream" {
		mm.streaming++
	}

	totalMS := float64(total.Milliseconds())
	mm.latSumMS += totalMS

	// TTFB 只在有观测时累加（流式首帧才有；非流式恒 0，不计入均值分母，
	// 否则会把非流式的 0 拉低均值，失真）。
	if s.ttfb > 0 {
		mm.ttfbSumMS += float64(s.ttfb.Milliseconds())
		mm.ttfbCount++
	}

	// token / cache / credit 只在 hasUsage 时累加：缺失≠0。
	if s.hasUsage {
		mm.promptTok += int64(s.prompt)
		// toks<0 是「观测缺失」哨兵（非流式路径：usage 存在但缺 completion_tokens 时
		// completionTokens 返回 -1，此时 hasUsage 仍为真）。不设此防护会把 -1 累加进
		// 总量，越积越偏——真值只可能 ≥0，故负值一律不计。
		if s.toks > 0 {
			mm.compTok += int64(s.toks)
		}
		mm.cacheHit += int64(s.cacheHit)
		mm.cacheMiss += int64(s.cacheMiss)
		mm.cacheWrite += int64(s.cacheWr)
		// 生成吞吐分母：总耗时减去 TTFB（纯生成时间）。TTFB 缺失时退回总耗时。
		gen := totalMS
		if s.ttfb > 0 {
			gen = totalMS - float64(s.ttfb.Milliseconds())
		}
		if gen > 0 {
			mm.genSecSum += gen / 1000.0
		}
	}
	if s.hasCredit {
		mm.credit += s.credit
	}

	mm.lastSeen = time.Now()
}

// MetricsSnapshot 是 /v1/stats 的响应载荷（字段名与社区面板约定一致）。
type MetricsSnapshot struct {
	Enabled   bool               `json:"enabled"`
	Message   string             `json:"message,omitempty"`
	Since     time.Time          `json:"since"`
	Now       time.Time          `json:"now"`
	UptimeSec int64              `json:"uptime_sec"`
	Total     ModelStatPayload   `json:"total"`
	Models    []ModelStatPayload `json:"models"`
}

// ModelStatPayload 单模型派生统计。
type ModelStatPayload struct {
	Model string `json:"model"`

	Requests  int64 `json:"requests"`
	Success   int64 `json:"success"`
	Failed    int64 `json:"failed"`
	Streaming int64 `json:"streaming"`

	AvgTTFBMS    float64 `json:"avg_ttfb_ms"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	TokensPerSec float64 `json:"tokens_per_sec"`

	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	CacheHitTokens   int64   `json:"cache_hit_tokens"`
	CacheMissTokens  int64   `json:"cache_miss_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CacheHitRate     float64 `json:"cache_hit_rate"`

	Credit       float64 `json:"credit"`
	CreditPerReq float64 `json:"credit_per_req"`

	// Credits 上游积分倍率原文（如 "x0.06"），与 /v1/models 的 credits 同源同值；
	// 目录未下发 / 缓存冷 → 空串，JSON 整体省略（缺失≠免费，不输出 "x0.00"）。
	// 由 stats handler 从模型目录只读缓存合入（enrichCredits），不参与聚合。
	Credits string `json:"credits,omitempty"`

	LastSeen *time.Time `json:"last_seen,omitempty"`
}

// MetricsSnapshotOf 生成当前聚合快照。models 按请求数降序（面板表格默认序）。
func MetricsSnapshotOf() MetricsSnapshot {
	now := time.Now()
	m := globalMetrics
	m.mu.Lock()
	defer m.mu.Unlock()

	out := MetricsSnapshot{
		Enabled:   true,
		Since:     m.since,
		Now:       now,
		UptimeSec: int64(now.Sub(m.since).Seconds()),
		Models:    make([]ModelStatPayload, 0, len(m.byModel)),
	}

	// total 由各模型累加得出（与 models 同口径，避免两处算法分叉）。
	var tot modelMetrics
	for name, mm := range m.byModel {
		out.Models = append(out.Models, deriveModelStat(name, mm))
		tot.requests += mm.requests
		tot.success += mm.success
		tot.failed += mm.failed
		tot.streaming += mm.streaming
		tot.ttfbSumMS += mm.ttfbSumMS
		tot.ttfbCount += mm.ttfbCount
		tot.latSumMS += mm.latSumMS
		tot.genSecSum += mm.genSecSum
		tot.promptTok += mm.promptTok
		tot.compTok += mm.compTok
		tot.cacheHit += mm.cacheHit
		tot.cacheMiss += mm.cacheMiss
		tot.cacheWrite += mm.cacheWrite
		tot.credit += mm.credit
		if mm.lastSeen.After(tot.lastSeen) {
			tot.lastSeen = mm.lastSeen
		}
	}
	out.Total = deriveModelStat("total", &tot)

	sort.Slice(out.Models, func(i, j int) bool {
		if out.Models[i].Requests != out.Models[j].Requests {
			return out.Models[i].Requests > out.Models[j].Requests
		}
		return out.Models[i].Model < out.Models[j].Model
	})
	return out
}

// deriveModelStat 把累加器折算为派生统计（均值、比率、吞吐）。
func deriveModelStat(name string, mm *modelMetrics) ModelStatPayload {
	p := ModelStatPayload{
		Model:            name,
		Requests:         mm.requests,
		Success:          mm.success,
		Failed:           mm.failed,
		Streaming:        mm.streaming,
		PromptTokens:     mm.promptTok,
		CompletionTokens: mm.compTok,
		TotalTokens:      mm.promptTok + mm.compTok,
		CacheHitTokens:   mm.cacheHit,
		CacheMissTokens:  mm.cacheMiss,
		CacheWriteTokens: mm.cacheWrite,
		Credit:           mm.credit,
	}
	if mm.requests > 0 {
		p.AvgLatencyMS = mm.latSumMS / float64(mm.requests)
		p.CreditPerReq = mm.credit / float64(mm.requests)
	}
	if mm.ttfbCount > 0 {
		p.AvgTTFBMS = mm.ttfbSumMS / float64(mm.ttfbCount)
	}
	if mm.genSecSum > 0 {
		p.TokensPerSec = float64(mm.compTok) / mm.genSecSum
	}
	// 命中率分母 = 命中 + 未命中（不含 write：写入是「为后续命中付的费」，
	// 计入分母会把首次请求的命中率压低，失真）。
	if denom := mm.cacheHit + mm.cacheMiss; denom > 0 {
		p.CacheHitRate = float64(mm.cacheHit) / float64(denom)
	}
	if !mm.lastSeen.IsZero() {
		t := mm.lastSeen
		p.LastSeen = &t
	}
	return p
}

// ResetMetrics 清空聚合（/v1/stats/reset），便于观察增量。since 重置为当前时刻。
func ResetMetrics() {
	m := globalMetrics
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byModel = make(map[string]*modelMetrics)
	m.since = time.Now()
	m.warned = false
}

// logMetricsCapWarn 容量超限告警（独立函数便于测试替换/断言，也避免 import log 污染
// 主体逻辑的阅读）。
func logMetricsCapWarn(model string) {
	log.Printf("WARN: [metrics] 模型键达上限 %d，丢弃新键 model=%q（异常模型名？）", metricsCap, model)
}

// enrichCredits 把上游积分倍率原文合入 stats 快照（/v1/stats 数据展示侧增强）。
//
// 数据源与 /v1/models 完全同源：CN 侧 cachedModelsSnapshot / global 侧
// GlobalModelInfosSnapshot，均为**只读快照**——缓存冷/过期 → nil，绝不发起上游
// 调用（maintainer 约束：网关只加工已有数据）。倍率是展示字段而非观测值，故
// 不进 recordChatMetric 聚合路径，快照出口统一合入。
//
// 键归一：stats 键是请求体 model 原文（含 realm 前缀），目录 id 是裸名——
// resolveModel 剥前缀后按 realm 查表；未知前缀/裸名含冒号/"-" 查不到 → 省略。
// total 行不参与（跨倍率聚合无意义）。
func (h *Handler) enrichCredits(snap *MetricsSnapshot) {
	cn := make(map[string]string) // bare id -> credits 原文
	for _, mi := range cachedModelsSnapshot() {
		if mi.Credits != "" {
			cn[mi.ID] = mi.Credits
		}
	}
	var global map[string]string
	if h.cfg.Upstream != nil {
		global = make(map[string]string)
		for _, mi := range h.cfg.Upstream.GlobalModelInfosSnapshot() {
			if mi.Credits != "" {
				global[mi.ID] = mi.Credits
			}
		}
	}
	for i := range snap.Models {
		realm, bare := resolveModel(snap.Models[i].Model)
		if bare == "" || bare == "-" {
			continue
		}
		if realm == "global" {
			snap.Models[i].Credits = global[bare]
		} else {
			snap.Models[i].Credits = cn[bare]
		}
	}
}

// fillStatFromUsage 把非流式聚合响应的 usage 观测填进 chatStat（与流式路径同口径）。
//
// 与 usageCreditTotal 的分工：那个函数服务成本账本（只取 credit + 总 token），
// 本函数服务 metrics（还要 prompt/cache 三段）。两者都读同一份 usage，但目标字段
// 不同，故不复用——强行合并会让账本依赖 metrics 的字段集，反之亦然。
func fillStatFromUsage(st *chatStat, resp map[string]any) {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return
	}
	st.hasUsage = true
	st.prompt = intFromUsage(u, "prompt_tokens")
	st.cacheHit = intFromUsage(u, "prompt_cache_hit_tokens")
	st.cacheMiss = intFromUsage(u, "prompt_cache_miss_tokens")
	st.cacheWr = intFromUsage(u, "prompt_cache_write_tokens")
	if c, ok := u["credit"].(float64); ok {
		st.credit = c
		st.hasCredit = true
	}
}

// intFromUsage 从 usage map 取整数字段；缺失或类型不符返回 0。
func intFromUsage(u map[string]any, key string) int {
	if v, ok := u[key].(float64); ok {
		return int(v)
	}
	return 0
}

// stats 处理 GET /v1/stats：返回按模型聚合的请求统计（社区面板数据源）。
// 聚合口径不变；出口处只读合入模型目录的积分倍率（enrichCredits，无上游调用）。
func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	snap := MetricsSnapshotOf()
	h.enrichCredits(&snap)
	writeJSON(w, http.StatusOK, snap)
}

// statsReset 处理 POST /v1/stats/reset：清空累计，便于观察增量。
func (h *Handler) statsReset(w http.ResponseWriter, r *http.Request) {
	ResetMetrics()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
