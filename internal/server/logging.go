// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/metrics"
)

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start  time.Time
	model  string
	mode   string // "stream" | "sync"
	uid    string // 完整 uid，展示时只取前 8 位
	ttfb   time.Duration
	toks   int // <0 表示 usage 缺失 → 显示 "-"
	status int

	logged bool

	// collector 统计收集器（nil = 未启用，record() 直接返回）。
	collector *metrics.Collector
	// usage 末帧完整 usage（供统计模块取缓存命中/扣费等明细）。
	usage *UsageDetail
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	s.record()
	logChatRow(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.status, s.toks)
}

// record 把本次请求写入统计收集器（collector 为 nil 时无操作）。
//
// 放在 done() 而不是各 return 点：done 是 defer 调用，任何出口（成功/失败/轮转
// 耗尽/panic 恢复）都会走到，统计不会漏记。
func (s *chatStat) record() {
	if s.collector == nil {
		return
	}
	d := metrics.Delta{
		Model:    s.model,
		Stream:   s.mode == "stream",
		OK:       s.status >= 200 && s.status < 300,
		TTFB:     s.ttfb,
		Latency:  time.Since(s.start),
		HasUsage: s.usage != nil,
	}
	if u := s.usage; u != nil {
		d.PromptTokens = u.PromptTokens
		d.CompletionTokens = u.CompletionTokens
		d.TotalTokens = u.TotalTokens
		d.CacheHitTokens = u.CacheHitTokens
		d.CacheMissTokens = u.CacheMissTokens
		d.CacheWriteTokens = u.CacheWriteTokens
		d.CacheReadTokens = u.CacheReadTokens
		d.CacheCreationTokens = u.CacheCreationTokens
		d.Credit = u.Credit
	}
	s.collector.Record(d)
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br        *bufio.Reader
	start     time.Time
	ttfb      time.Duration
	seen      bool // 已见过首个 data 帧（TTFB 只记一次）
	hasUsage  bool // 末帧是否带 usage
	hasCredit bool // 是否出现过带 credit 的 usage（缺失≠0，见 Credit() 注释）
	tokens    int
	credit    float64 // 末帧 usage.credit（本次真实扣费，供成本账本）
	prompt    int     // 末帧 usage.prompt_tokens（与 completion 合计折算单价）
	pend      []byte  // 已读未返回的行缓存

	// usage 末帧完整 usage 对象（供统计模块提取缓存命中/扣费等字段）。
	usage *UsageDetail
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.tokens, s.hasUsage }

// Credit 返回末帧 usage.credit（本次真实扣费）。ok=true 要求 usage 存在**且** credit
// 字段显式出现——字段缺失时 ok=false（缺失≠0：不能把"缺观测"当"0 成本"写入账本，
// 否则收费的号可能被误判 tier0 免费层）。显式 credit:0 仍是合法免费观测（ok=true）。
func (s *chatStatsReader) Credit() (float64, bool) { return s.credit, s.hasUsage && s.hasCredit }

// Usage 返回末帧完整 usage 明细（无 usage 时为 nil）。
func (s *chatStatsReader) Usage() *UsageDetail { return s.usage }

// TotalTokens 返回本次请求总 token 数（prompt + completion），供成本单价折算。
func (s *chatStatsReader) TotalTokens() int { return s.prompt + s.tokens }

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	// 用宽松 map 解析：usage 字段名在不同模型/区域间有差异（见 UsageDetail 注释），
	// 结构体标签写死会漏字段。
	var chunk struct {
		Usage map[string]any `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil || chunk.Usage == nil {
		return
	}
	s.hasUsage = true
	u := ParseUsage(chunk.Usage)
	s.usage = u
	s.tokens = int(u.CompletionTokens)
	s.prompt = int(u.PromptTokens)
	// hasCredit 语义：仅当 credit 字段显式出现**且非 null**（含显式 0）才置位。
	// 缺失≠0 —— 不能把「缺观测」当「0 成本」写入账本；credit:null 同样视为缺失
	//（上游用 null 表达"本次无扣费观测"，与显式 0 语义不同）。
	if v, ok := chunk.Usage["credit"]; ok && v != nil {
		s.hasCredit = true
		s.credit = u.Credit
	}
}

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// usageCreditTotal 从聚合响应提取本次真实扣费与总 token 数（供成本账本）。
// ok=false 表示 usage 缺失或字段类型不符——此时不记录观测，避免污染账本。
func usageCreditTotal(resp map[string]any) (credit float64, total int, ok bool) {
	u, isMap := resp["usage"].(map[string]any)
	if !isMap {
		return 0, 0, false
	}
	c, hasCredit := u["credit"].(float64)
	pt, hasPrompt := u["prompt_tokens"].(float64)
	ct, hasCompletion := u["completion_tokens"].(float64)
	if !hasCredit || (!hasPrompt && !hasCompletion) {
		return 0, 0, false
	}
	return c, int(pt) + int(ct), true
}

// UsageDetail 上游 usage 的归一化视图。
//
// 为什么用宽松 map 而非结构体标签：usage 字段名在不同模型/区域间有差异
// （OpenAI 风格的 prompt_cache_hit_tokens 与 Anthropic 风格的
// cache_read_input_tokens 都出现过），写死标签会漏字段。
// 缺失字段一律按 0 处理（不猜、不估算）。
type UsageDetail struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64

	CacheHitTokens      int64
	CacheMissTokens     int64
	CacheWriteTokens    int64
	CacheReadTokens     int64
	CacheCreationTokens int64

	// Credit 上游返回的实际扣费（部分模型带 credit 字段）。
	Credit float64
}

// ParseUsage 把上游 usage 对象归一化为 UsageDetail。
func ParseUsage(u map[string]any) *UsageDetail {
	d := &UsageDetail{
		PromptTokens:     int64Field(u, "prompt_tokens"),
		CompletionTokens: int64Field(u, "completion_tokens"),
		TotalTokens:      int64Field(u, "total_tokens"),

		// OpenAI 风格缓存字段
		CacheHitTokens:   int64Field(u, "prompt_cache_hit_tokens"),
		CacheMissTokens:  int64Field(u, "prompt_cache_miss_tokens"),
		CacheWriteTokens: int64Field(u, "prompt_cache_write_tokens"),
		// Anthropic 风格缓存字段
		CacheReadTokens:     int64Field(u, "cache_read_input_tokens"),
		CacheCreationTokens: int64Field(u, "cache_creation_input_tokens"),

		Credit: float64Field(u, "credit"),
	}
	// 两套命名都有时取较大值，避免重复累计导致命中率虚高
	// （同一份数据被两个字段名各报一次的场景）。
	if v := int64Field(u, "cached_tokens"); v > d.CacheHitTokens {
		d.CacheHitTokens = v
	}
	// total 缺失时用 prompt+completion 补齐（便于面板直接展示）。
	if d.TotalTokens == 0 {
		d.TotalTokens = d.PromptTokens + d.CompletionTokens
	}
	return d
}

// int64Field 从 map 取整数字段（兼容 JSON number 的 float64 与字符串数字）。
func int64Field(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

// float64Field 从 map 取浮点字段。
func float64Field(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	}
	return 0
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
func uidPrefix(uid string) string {
	if uid == "" {
		return "-"
	}
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// logChatRow 打印一行请求级表格日志（直接输出 stdout，无 log 时间戳前缀）。
// toks<0 表示 usage 缺失，显示 "-"。
func logChatRow(ttfb, total time.Duration, model, mode, uid string, status int, toks int) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	if len(model) > 11 {
		model = model[:11]
	}
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1f", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	fmt.Fprintf(os.Stdout, "| #%03d | %s | %s | %s | %d | uid=%s | TTFB=%s | tok=%s | %stok/s | total=%.1fs |\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		uidPrefix(uid),
		ttfbMS,
		tokField,
		tokpsField,
		total.Seconds(),
	)
}
