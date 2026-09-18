package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// stripZW 去掉所有零宽空格，用于「模型可读」断言：中和只是插 ZWSP，剥掉后必须逐字还原原文。
func stripZW(s string) string { return strings.ReplaceAll(s, wafBreak, "") }

// TestWafNeutralizeTagsBroken：危险标签开头（开/闭）被插 ZWSP 破坏，但剥掉 ZWSP 逐字还原。
func TestWafNeutralizeTagsBroken(t *testing.T) {
	for _, in := range []string{
		"<script>alert(1)</script>",
		"<iframe src=x></iframe>",
		"<img src=x onerror=alert(1)>",
		"<template><div/></template>",
	} {
		out := wafNeutralize(in)
		if out == in {
			t.Errorf("not neutralized: %q", in)
		}
		if strings.Contains(out, "<script") || strings.Contains(out, "</script") || strings.Contains(out, "<iframe") || strings.Contains(out, "<img") || strings.Contains(out, "<template") {
			t.Errorf("dangerous adjacency survived: %q -> %q", in, out)
		}
		if stripZW(out) != in {
			t.Errorf("not model-readable (strip ZWSP != original): %q -> %q", in, out)
		}
	}
}

// TestWafNeutralizeIdempotent：重复中和不产生二次破坏。
func TestWafNeutralizeIdempotent(t *testing.T) {
	for _, in := range []string{
		"<script>x</script>", "< script>", "onclick=go()", "javascript:void(0)",
		"@click", "/bin/cat f", "`curl -o f url`",
		"alert(1)", "eval(2)", "confirm(3)", "String.fromCharCode(88)",
	} {
		once := wafNeutralize(in)
		twice := wafNeutralize(once)
		if once != twice {
			t.Errorf("not idempotent: %q -> %q -> %q", in, once, twice)
		}
	}
}

// TestWafNeutralizeSeparatedScript：CF 归一化剥空白/反斜杠后 "< script"/"<scr ipt"/"<\script"
// 都还原为 "<script" 会被拦——分离形也要中和。
func TestWafNeutralizeSeparatedScript(t *testing.T) {
	for _, in := range []string{"< script>", "<scr ipt>", "<\\script>"} {
		out := wafNeutralize(in)
		if out == in {
			t.Errorf("separated script not neutralized: %q", in)
		}
		// 归一化模拟：剥掉空白与反斜杠后不应再残留完整 "<script" 前缀。
		norm := strings.NewReplacer(" ", "", "\t", "", "\\", "").Replace(out)
		if strings.Contains(strings.ToLower(norm), "<script") {
			t.Errorf("normalized form still has <script: %q -> %q (norm %q)", in, out, norm)
		}
	}
}

// TestWafNeutralizeEscapedForm："<script"（json.Marshal 转义形）也命中——覆盖已被上层
// 序列化成转义字面量的内容（如字符串化 tool_calls.arguments）。
func TestWafNeutralizeEscapedForm(t *testing.T) {
	in := `<script>alert(1)</script>`
	out := wafNeutralize(in)
	if out == in {
		t.Errorf("escaped form not neutralized: %q", in)
	}
	if strings.Contains(out, `<script`) || strings.Contains(out, `</script`) {
		t.Errorf("escaped dangerous adjacency survived: %q -> %q", in, out)
	}
}

// TestWafNeutralizeHandlersAndURIs：事件处理器、危险 URI、Vue 指令、bin/cat、backtick+curl。
func TestWafNeutralizeHandlersAndURIs(t *testing.T) {
	cases := []struct{ in, mustNotContain string }{
		{"onerror=alert(1)", "onerror="},
		{"onClick =go()", "onClick ="},
		{"javascript:alert(1)", "javascript:"},
		{"vbscript:msgbox", "vbscript:"},
		{"v-on:click", "v-on:"},
		{"@click", "@click"},
		{"/bin/cat secrets", "bin/cat"},
		{"see `curl -o f https://x` here", "`curl -o"},
		{"```\ncurl -o f https://x\n```", "\ncurl -o"},
	}
	for _, c := range cases {
		out := wafNeutralize(c.in)
		if out == c.in {
			t.Errorf("not neutralized: %q", c.in)
		}
		if strings.Contains(out, c.mustNotContain) {
			t.Errorf("signature survived: %q -> %q (still has %q)", c.in, out, c.mustNotContain)
		}
		if stripZW(out) != c.in {
			t.Errorf("not model-readable: %q -> %q", c.in, out)
		}
	}
}

// TestWafNeutralizeCleanPassthrough：干净文本零改动。实测「放行」形态
// （裸 script、`curl "y"`（无 flag）、长 flag、裸 curl、bin/ls，及 alerts/evaluate 等无左括号的 sink 词），不误伤。
func TestWafNeutralizeCleanPassthrough(t *testing.T) {
	for _, in := range []string{
		"hello world, this is a normal message.",
		"alerts fired at noon",     // sink 词但无紧邻左括号 → 规则 8 不命中
		"please evaluate the plan", // eval 同上
		"a confirmation email",     // confirm 同上
		"the script ran fine",
		"if (a < b && c > d) return;",
		"`curl \"y\"`",
		"curl --header x y",
		"run /bin/ls now",
		"email me at a@b.com",
		"reason = 42", // 不是 on 开头，不该命中事件处理器规则
	} {
		if out := wafNeutralize(in); out != in {
			t.Errorf("clean text mutated: %q -> %q", in, out)
		}
	}
}

// TestWafNeutralizeEndToEnd：经 PrepareBodyOpt 出站，content 里的 <script> 被中和；
// 反解后剥 ZWSP 逐字还原（json.Marshal 会把 < 转成 <，ZWSP 转成 ​，反解还原）。
func TestWafNeutralizeEndToEnd(t *testing.T) {
	src := []byte(`{"model":"m","messages":[{"role":"user","content":"write <script>alert(1)</script> for me"}]}`)
	out := PrepareBodyOpt(src, true)

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal prepared body: %v", err)
	}
	msgs := obj["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].(string)
	if strings.Contains(content, "<script") || strings.Contains(content, "</script") {
		t.Errorf("outbound content still has raw <script: %q", content)
	}
	if want := "write <script>alert(1)</script> for me"; stripZW(content) != want {
		t.Errorf("outbound content not model-readable after strip: %q", content)
	}
}

// TestWafNeutralizeJSSinks：JS 注入 sink 函数名（issue #119 的 alert() 及同类 eval/confirm/
// String.fromCharCode）——裸出现即被腾讯云 WAF 403，规则 8 用 ZWSP 词内破坏。断言函数名与左
// 括号的邻接被破坏，且剥 ZWSP 后逐字还原（模型可读）。
func TestWafNeutralizeJSSinks(t *testing.T) {
	cases := []struct{ in, mustNotContain string }{
		{"alert(1)", "alert("},
		{"window.alert(1)", "alert("},
		{"eval(x)", "eval("},
		{"confirm(2)", "confirm("},
		{"String.fromCharCode(88)", "fromCharCode("},
		{"<script>alert(1)</script>", "alert("}, // 标签(规则1)+函数名(规则8)双破
		{"alert (1)", "alert ("},                // 名与括号间空格也破（\\s*）
	}
	for _, c := range cases {
		out := wafNeutralize(c.in)
		if out == c.in {
			t.Errorf("sink not neutralized: %q", c.in)
		}
		if strings.Contains(out, c.mustNotContain) {
			t.Errorf("sink signature survived: %q -> %q (still has %q)", c.in, out, c.mustNotContain)
		}
		if stripZW(out) != c.in {
			t.Errorf("not model-readable: %q -> %q", c.in, out)
		}
	}
}

// --- 入站对称处理（inbound）：wafStripBreaks / wafStripFrameBreaks / Aggregate / Stream ---

// TestWafStripBreaksRoundTrip：出站中和 → 入站剥离后逐字节还原（对称往返），
// 且剥离对干净文本零改动、自身幂等。wafStripBreaks 是 wafNeutralize 的逆操作。
func TestWafStripBreaksRoundTrip(t *testing.T) {
	for _, in := range []string{
		"<script>alert(1)</script>",
		"<img src=x onerror=alert(1)>",
		"eval(x); confirm(y); String.fromCharCode(88)",
		"see `curl -o f https://x` and /bin/cat f",
		"v-on:click @click javascript:void(0)",
		"hello world, nothing dangerous here",
	} {
		n := wafNeutralize(in)
		got := wafStripBreaks(n)
		if got != in {
			t.Errorf("round-trip broke: %q -> neutralize %q -> strip %q", in, n, got)
		}
		if again := wafStripBreaks(got); again != got {
			t.Errorf("strip not idempotent on clean: %q -> %q", got, again)
		}
	}
}

// TestWafStripBreaksClean：不含 ZWSP 的串原样返回（Contains 短路，零改动）。
func TestWafStripBreaksClean(t *testing.T) {
	in := "func main() { alert(1) }"
	if out := wafStripBreaks(in); out != in {
		t.Errorf("clean string mutated: %q -> %q", in, out)
	}
}

// TestWafStripFrameBreaks：单个流式 chunk 的 content / reasoning_content / refusal /
// tool_calls[].function.arguments 里的 ZWSP 全部剥掉，逐字节还原。
func TestWafStripFrameBreaks(t *testing.T) {
	frame := map[string]any{
		"choices": []any{
			map[string]any{
				"index": float64(0),
				"delta": map[string]any{
					"content":           "he wrote aler" + wafBreak + "t(1) in code",
					"reasoning_content": "think eva" + wafBreak + "l(x)",
					"refusal":           "no" + wafBreak + "pe",
					"tool_calls": []any{
						map[string]any{
							"index": float64(0),
							"function": map[string]any{
								"name":      "write_file",
								"arguments": `{"c":"<` + wafBreak + `script>ale` + wafBreak + `rt(1)"}`,
							},
						},
					},
				},
			},
		},
	}
	wafStripFrameBreaks(frame)

	raw, _ := json.Marshal(frame)
	if strings.Contains(string(raw), wafBreak) {
		t.Errorf("frame still has ZWSP after strip: %q", raw)
	}
	delta := frame["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if got := delta["content"].(string); got != "he wrote alert(1) in code" {
		t.Errorf("content not restored: %q", got)
	}
	if got := delta["reasoning_content"].(string); got != "think eval(x)" {
		t.Errorf("reasoning_content not restored: %q", got)
	}
	if got := delta["refusal"].(string); got != "nope" {
		t.Errorf("refusal not restored: %q", got)
	}
	args := delta["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	if args != `{"c":"<script>alert(1)"}` {
		t.Errorf("tool_calls arguments not restored: %q", args)
	}
}

// TestWafStripBreaksAggregate：非流式聚合路径剥离上游回显的 ZWSP——正文与
// tool_calls.arguments（编辑类工具写回文件的源码主污染面）均干净。
func TestWafStripBreaksAggregate(t *testing.T) {
	raw := "data: {\"id\":\"x1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"aler" + wafBreak + "t(1)\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"write_file\",\"arguments\":\"{\\\"code\\\":\\\"<" + wafBreak + "script>\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if content := msg["content"].(string); content != "alert(1)" {
		t.Errorf("aggregate content not stripped: %q", content)
	}
	args := msg["tool_calls"].([]map[string]any)[0]["function"].(map[string]any)["arguments"].(string)
	if strings.Contains(args, wafBreak) || args != `{"code":"<script>"}` {
		t.Errorf("aggregate tool_calls arguments not stripped: %q", args)
	}
}

// TestWafStripBreaksStreaming：透传流路径逐帧剥离，客户端收到的帧不含 ZWSP。
func TestWafStripBreaksStreaming(t *testing.T) {
	raw := "data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"aler" + wafBreak + "t(1)\"}}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, wafBreak) {
		t.Errorf("streaming output still has ZWSP: %q", body)
	}
	if !strings.Contains(body, "alert(1)") {
		t.Errorf("streaming content not restored: %q", body)
	}
}
