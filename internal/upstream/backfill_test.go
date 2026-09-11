package upstream

import (
	"encoding/json"
	"testing"
)

// assistantRC 提取输出 messages 里各 assistant 消息的 reasoning_content（缺字段返回 ""+false）。
// 返回的切片与 messages 中 assistant 消息一一对应（非 assistant 消息跳过）。
func assistantRC(t *testing.T, out []byte) []string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	var got []string
	msgs, _ := m["messages"].([]any)
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		if v, ok := msg["reasoning_content"].(string); ok {
			got = append(got, v)
		} else {
			got = append(got, "<absent>")
		}
	}
	return got
}

// TestBackfillReasoningContentDeepSeek DeepSeek 多轮一致性：历史 assistant 消息有
// reasoning 痕迹时，所有 assistant 消息必须带 reasoning_content（string）。
// 对齐官方 requiresReasoningContentOnAssistantMessages 行为。
func TestBackfillReasoningContentDeepSeek(t *testing.T) {
	cases := []struct {
		name string
		body string
		// 只断言「assistant 消息数」与「每条是否有 reasoning_content 字段」，
		// 值是 string 即可（复制或空串，由具体用例断言）。
		wantCount int
		wantVals  []string // 与 assistant 消息一一对应；空串表示任意 string
	}{
		{"assistant 带 reasoning 无 reasoning_content → 复制",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"user","content":"u"},
				{"role":"assistant","content":"a","reasoning":"thought text"}]}`,
			1, []string{"thought text"}},
		{"assistant 带 reasoning_content 原样保留",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning_content":"already there"}]}`,
			1, []string{"already there"}},
		{"混合会话全量补上：无 reasoning 的 assistant 补空串",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"user","content":"u"},
				{"role":"assistant","content":"a1","reasoning":"t1"},
				{"role":"user","content":"u2"},
				{"role":"assistant","content":"a2"}]}`,
			2, []string{"t1", ""}},
		{"assistant reasoning 为空串视为无 reasoning 痕迹",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning":""}]}`,
			1, []string{"<absent>"}},
		{"多 assistant 都带 reasoning 全部复制",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a1","reasoning":"r1"},
				{"role":"assistant","content":"a2","reasoning":"r2"}]}`,
			2, []string{"r1", "r2"}},
		{"reasoning 非 string 值（数字）按空串处理",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning":123}]}`,
			1, []string{"<absent>"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			got := assistantRC(t, out)
			if len(got) != c.wantCount {
				t.Fatalf("assistant 消息数 = %d want %d (out=%s)", len(got), c.wantCount, out)
			}
			for i, want := range c.wantVals {
				if want == "" {
					continue // 任意 string（补空串/复制都接受，但必须存在）
				}
				if got[i] != want {
					t.Errorf("assistant[%d].reasoning_content = %q want %q (out=%s)", i, got[i], want, out)
				}
			}
		})
	}
}

// TestBackfillReasoningContentNoTrace 会话无任何 reasoning 痕迹 → 零改动：
// 不白白给 assistant 消息加 reasoning_content 字段。
func TestBackfillReasoningContentNoTrace(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"纯 text assistant 不动",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"user","content":"u"},
				{"role":"assistant","content":"plain answer"}]}`},
		{"无 assistant 消息不动",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"user","content":"u"}]}`},
		{"messages 缺失不动",
			`{"model":"deepseek-v4-flash"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			for _, rc := range assistantRC(t, out) {
				if rc != "<absent>" {
					t.Errorf("无 reasoning 痕迹却被加 reasoning_content=%q (out=%s)", rc, out)
				}
			}
		})
	}
}

// TestBackfillReasoningContentNonDeepSeek 非 deepseek 模型零改动：
// reasoning 字段保持原样，不新增 reasoning_content。
func TestBackfillReasoningContentNonDeepSeek(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[
		{"role":"assistant","content":"a","reasoning":"thought"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	for _, rc := range assistantRC(t, out) {
		if rc != "<absent>" {
			t.Errorf("非 deepseek 不应 backfill, got reasoning_content=%q (out=%s)", rc, out)
		}
	}
}

// TestBackfillReasoningContentBothFields 同时带 reasoning 与 reasoning_content：
// 以 reasoning_content 为准（不覆盖），reasoning 字段保留（兼容）——对齐客户端 matches 规则。
func TestBackfillReasoningContentBothFields(t *testing.T) {
	body := `{"model":"deepseek-v4-flash","messages":[
		{"role":"assistant","content":"a","reasoning":"t","reasoning_content":"existing"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	got := assistantRC(t, out)
	if len(got) != 1 || got[0] != "existing" {
		t.Errorf("reasoning_content 应以已有值为准: got %v (out=%s)", got, out)
	}
	// 同时确认 reasoning 字段仍原样保留。
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	msgs, _ := m["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	if r, ok := first["reasoning"].(string); !ok || r != "t" {
		t.Errorf("reasoning 字段被改动: %v (out=%s)", first, out)
	}
}

// TestBackfillComposesWithInjectThinking backfill 与 injectThinking 相互独立：
// 显式 disabled 时 reasoning_effort 被删，但 backfill 照常生效（多轮一致性不因关思维链而丢）。
func TestBackfillComposesWithInjectThinking(t *testing.T) {
	body := `{"model":"DEEPSEEK-v4-flash","thinking":{"type":"disabled"},"reasoning_effort":"high","messages":[
		{"role":"user","content":"u"},
		{"role":"assistant","content":"a","reasoning":"thought"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	got := assistantRC(t, out)
	if len(got) != 1 || got[0] != "thought" {
		t.Errorf("disabled 时 backfill 仍应生效: got %v (out=%s)", got, out)
	}
	if typ, present := getThinkingType(t, out); !present || typ != "disabled" {
		t.Errorf("thinking.type 应保留 disabled, got %q present=%v", typ, present)
	}
	for _, k := range []string{"reasoning_effort", "reasoningEffort"} {
		if _, ok := objFieldString(t, out, k); ok {
			t.Errorf("%s 应被删除（disabled 时）", k)
		}
	}
}
