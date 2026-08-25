package server

import (
	"io"
	"strings"
	"testing"
	"time"
)

// TestChatStatsReaderStreamTokens 验证流式 SSE 的 token 统计（按 UTF-8 字符）与透传。
func TestChatStatsReaderStreamTokens(t *testing.T) {
	sse := "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好世界\"}}]}\n\n" +
		"data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"abc\"}}]}\n\n" +
		"data: [DONE]\n\n"
	stats := newChatStatsReaderSince(strings.NewReader(sse), time.Now().Add(-time.Second))

	// 读取所有数据（模拟上游透传）
	got, err := io.ReadAll(stats)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// 透传必须原样返回原始 SSE 字节
	if string(got) != sse {
		t.Fatalf("passthrough mismatch:\n got %q\nwant %q", string(got), sse)
	}
	// "你好世界" = 4 字符，abc = 3 字符，共 7
	if stats.Tokens() != 7 {
		t.Fatalf("tokens = %d, want 7", stats.Tokens())
	}
	if stats.TTFB().Milliseconds() < 900 {
		t.Fatalf("ttfb = %v, want ~1000ms from fixed baseline", stats.TTFB())
	}
}

// TestChatStatsReaderUsesUsage 验证末帧带 usage 时优先采信精确 token 数。
func TestChatStatsReaderUsesUsage(t *testing.T) {
	sse := "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n\n" +
		"data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{}}],\"usage\":{\"completion_tokens\":42}}\n\n" +
		"data: [DONE]\n\n"
	stats := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.ReadAll(stats)
	// usage 的 42 优先于字符统计（"你好" 仅 2 字符）
	if stats.Tokens() != 42 {
		t.Fatalf("tokens = %d, want 42 (usage overrides)", stats.Tokens())
	}
}

// TestChatStatsReaderReasoningTokens 验证 reasoning_content 也计入 token。
func TestChatStatsReaderReasoningTokens(t *testing.T) {
	sse := "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"思考过程\"}}]}\n\n" +
		"data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"结论\"}}]}\n\n" +
		"data: [DONE]\n\n"
	stats := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.ReadAll(stats)
	// "思考过程"=4 + "结论"=2 = 6
	if stats.Tokens() != 6 {
		t.Fatalf("tokens = %d, want 6 (reasoning + content)", stats.Tokens())
	}
}

// TestChatStatsReaderFirstContentTTFB 验证首个内容帧才触发 TTFB（跳过 role/meta 帧）。
func TestChatStatsReaderFirstContentTTFB(t *testing.T) {
	base := time.Now().Add(-5 * time.Second)
	// 首帧只有 role（无 content），不应触发 TTFB；次帧有 content 应触发。
	sse := "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"
	stats := newChatStatsReaderSince(strings.NewReader(sse), base)
	_, _ = io.ReadAll(stats)
	if !stats.seen {
		t.Fatal("content seen should be true")
	}
	// TTFB 应从基准（5s 前）起算，约 5000ms
	if tt := stats.TTFB().Milliseconds(); tt < 4900 || tt > 5100 {
		t.Fatalf("ttfb = %dms, want ~5000ms", tt)
	}
}

// TestParseModelFromBody 验证从请求体提取模型名。
func TestParseModelFromBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"has model", `{"model":"hy3","messages":[]}`, "hy3"},
		{"no model", `{"messages":[]}`, "-"},
		{"invalid json", `not-json`, "-"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseModelFromBody([]byte(c.body)); got != c.want {
				t.Fatalf("parseModelFromBody(%q) = %q, want %q", c.body, got, c.want)
			}
		})
	}
}

// TestChatStatsReaderTTFBStartsAtBaseline 验证 TTFB 在无任何内容时的默认行为。
func TestChatStatsReaderTTFBStartsAtBaseline(t *testing.T) {
	stats := newChatStatsReaderSince(strings.NewReader(""), time.Now().Add(-time.Second))
	_, _ = io.ReadAll(stats)
	if stats.TTFB() != 0 {
		t.Fatalf("ttfb = %v, want 0 when no content", stats.TTFB())
	}
}
