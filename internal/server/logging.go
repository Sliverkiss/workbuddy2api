package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// chatSeq 是进程级对话序号，用于在日志中标识每次对话。
var chatSeq atomic.Int64

// chatLogHeaderOnce 保证表头只打印一次。
var chatLogHeaderOnce sync.Once

// chatStatsReader 在数据流经时解析 SSE 内容：记录首个内容 token 的 TTFB、
// 累计输出 token（按 UTF-8 字符估算），同时把原始字节原样返回给下游透传。
type chatStatsReader struct {
	br     *bufio.Reader
	ttfb   time.Duration
	start  time.Time
	seen   bool
	tokens int
	pend   []byte // 未返回的已读行缓存
}

// newChatStatsReaderSince 以指定时刻为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个内容 token 到达时间。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回累计输出 token 数。
func (s *chatStatsReader) Tokens() int { return s.tokens }

// parseSSELine 解析一行 "data: {...}"，累计 content/reasoning_content 字符数。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data: ") || strings.TrimPrefix(line, "data: ") == "[DONE]" {
		return
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
		return
	}
	// 首帧若带 usage（某些上游），直接采信精确 token 数。
	if chunk.Usage.CompletionTokens > 0 {
		s.tokens = chunk.Usage.CompletionTokens
		if !s.seen {
			s.seen = true
			s.ttfb = time.Since(s.start)
		}
	}
	for _, c := range chunk.Choices {
		content := c.Delta.Content
		if content == "" {
			content = c.Message.Content
		}
		reasoning := c.Delta.ReasoningContent
		if reasoning == "" {
			reasoning = c.Message.ReasoningContent
		}
		if content != "" && !s.seen {
			s.seen = true
			s.ttfb = time.Since(s.start)
		}
		s.tokens += utf8.RuneCountInString(content) + utf8.RuneCountInString(reasoning)
	}
}

// Read 返回原始数据，同时解析内容统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	// 优先返回缓存的完整行。
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

// parseModelFromBody 从请求 JSON 中取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// logChatRow 打印一行对话级表格日志（直接输出 stdout，无 log 时间戳前缀）。
// toks<0 表示无数据，显示 "-"。
func logChatRow(ttfb, total time.Duration, model, mode string, status int, toks int) {
	chatLogHeaderOnce.Do(func() {
		fmt.Fprintf(os.Stdout, "| #    | Time     | Fmt | Model       | Mode   | Stat |    TTFB |   Tok |  tok/s |   Total |\n")
		fmt.Fprintf(os.Stdout, "|------|----------|-----|-------------|--------|------|---------|-------|--------|---------|\n")
	})
	seq := chatSeq.Add(1)
	modelField := model
	if len(modelField) > 11 {
		modelField = modelField[:11]
	}
	tokField := "-"
	tokps := 0.0
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 && toks > 0 {
			tokps = float64(toks) / total.Seconds()
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	totalMS := "-"
	if total > 0 {
		totalMS = fmt.Sprintf("%dms", total.Milliseconds())
	}
	fmt.Fprintf(os.Stdout, "| #%03d | %s   | OAI | %-11s | %-6s | %4d | %7s | %5s | %6.1f | %6s |\n",
		seq,
		time.Now().Format("15:04:05"),
		modelField,
		mode,
		status,
		ttfbMS,
		tokField,
		tokps,
		totalMS,
	)
}
