package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// getThinkingType 从输出 body 提取 thinking.type（缺字段返回空串 + 是否存在）。
func getThinkingType(t *testing.T, out []byte) (typ string, present bool) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	th, ok := m["thinking"].(map[string]any)
	if !ok {
		return "", false
	}
	s, ok := th["type"].(string)
	if !ok {
		return "", true
	}
	return s, true
}

// TestInjectThinkingDeepSeekEnabled 开思考开关注入：deepseek 系模型请求体不带
// thinking 时必须注入 {type:"enabled"}，否则上游默认按不思考应答（思维链不显示）。
func TestInjectThinkingDeepSeekEnabled(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantTyp string
	}{
		{"deepseek 无 thinking 注入 enabled",
			`{"model":"deepseek-v4-flash","messages":[]}`, "enabled"},
		{"DeepSeek 大小写不敏感",
			`{"model":"DeepSeek-v4.1-flash","messages":[]}`, "enabled"},
		{"DEEPSEEK 全大写不敏感",
			`{"model":"DEEPSEEK-R1","messages":[]}`, "enabled"},
		{"deepseek 带 reasoning_effort 无 thinking 注入 enabled",
			`{"model":"deepseek-v4-flash","reasoning_effort":"medium","messages":[]}`, "enabled"},
		{"deepseek thinking 对象 type 空 补 enabled",
			`{"model":"deepseek-v4-flash","thinking":{},"messages":[]}`, "enabled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			typ, present := getThinkingType(t, out)
			if !present {
				t.Fatalf("thinking 字段缺失 (out=%s)", out)
			}
			if typ != c.wantTyp {
				t.Errorf("thinking.type = %q want %q (out=%s)", typ, c.wantTyp, out)
			}
		})
	}
}

// objFieldString 提取顶层字段 string 值。
func objFieldString(t *testing.T, out []byte, key string) (string, bool) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	s, ok := m[key].(string)
	return s, ok
}

// TestInjectThinkingDeepSeekExplicitControl 客户端显式控制时必须尊重：
// thinking.type 非空（enabled/disabled）都不得被覆盖。
func TestInjectThinkingDeepSeekExplicitControl(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantTyp string
		wantEff string // 期望 reasoning_effort 值；"" 且 wantEffAbsent=true 表示应删除
	}{
		{"已有 enabled 不动",
			`{"model":"deepseek-v4-flash","thinking":{"type":"enabled"},"messages":[]}`,
			"enabled", ""},
		{"已有 enabled 且带 reasoning_effort 保持（不删 effort）",
			`{"model":"deepseek-v4-flash","thinking":{"type":"enabled"},"reasoning_effort":"high","messages":[]}`,
			"enabled", "high"},
		{"已有 disabled 保留（尊重关闭意图）",
			`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"messages":[]}`,
			"disabled", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			typ, present := getThinkingType(t, out)
			if !present {
				t.Fatalf("thinking 字段缺失 (out=%s)", out)
			}
			if typ != c.wantTyp {
				t.Errorf("thinking.type = %q want %q (out=%s)", typ, c.wantTyp, out)
			}
			if c.wantEff != "" {
				if eff, _ := objFieldString(t, out, "reasoning_effort"); eff != c.wantEff {
					t.Errorf("reasoning_effort = %q want %q (out=%s)", eff, c.wantEff, out)
				}
			}
		})
	}
}

// TestInjectThinkingDisabledDeletesEffort 显式 disabled 时 reasoning_effort 一并删除
// （照抄官方客户端 case "deepseek" 行为：默认分支 delete reasoning_effort）。
// snow/camel 双字段都删。
func TestInjectThinkingDisabledDeletesEffort(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"disabled + snake effort 删除",
			`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"reasoning_effort":"high","messages":[]}`},
		{"disabled + camel effort 删除",
			`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"reasoningEffort":"high","messages":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			typ, present := getThinkingType(t, out)
			if !present || typ != "disabled" {
				t.Fatalf("thinking.type = %q present=%v want disabled (out=%s)", typ, present, out)
			}
			for _, k := range []string{"reasoning_effort", "reasoningEffort"} {
				if _, ok := objFieldString(t, out, k); ok {
					t.Errorf("%s 应被删除（显式 disabled 时）(out=%s)", k, out)
				}
			}
		})
	}
}

// TestInjectThinkingSkipNonDeepSeek 非 deepseek 模型零改动：无 thinking 不得凭空添加，
// 已有 thinking 原样保留。
func TestInjectThinkingSkipNonDeepSeek(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"glm 无 thinking 不注入", `{"model":"glm-5.2","messages":[]}`},
		{"glm 已有 thinking 保留", `{"model":"glm-5.2","thinking":{"type":"enabled"},"messages":[]}`},
		{"kimi 无 thinking 不注入", `{"model":"kimi-k2.5","messages":[]}`},
		{"qwen 系不注入", `{"model":"qwen2.5-coder-32b","messages":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 记录输入是否已带 thinking 及值，输出必须逐字节语义不变。
			var in map[string]any
			if err := json.Unmarshal([]byte(c.body), &in); err != nil {
				t.Fatalf("unmarshal input: %v", err)
			}
			inTh, inHadThink := in["thinking"]
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("unmarshal out: %v (out=%s)", err, out)
			}
			outTh, outHadThink := got["thinking"]
			if inHadThink != outHadThink {
				t.Errorf("thinking 出现性变化: in=%v(had=%v) out=%v(had=%v)",
					inTh, inHadThink, outTh, outHadThink)
			}
			if inHadThink {
				// 保留的 thinking 必须原值相等（JSON 语义级）。
				inJSON, _ := json.Marshal(inTh)
				outJSON, _ := json.Marshal(outTh)
				if string(inJSON) != string(outJSON) {
					t.Errorf("thinking 被改动: in=%s out=%s", inJSON, outJSON)
				}
			}
			// 非 deepseek 不得新增其他字段（除强制 stream 这一既有行为）。
			if len(got) != len(in)+1 {
				for k := range got {
					if _, had := in[k]; !had {
						t.Errorf("非 deepseek 新增字段 %q (out=%s)", k, out)
					}
				}
			}
		})
	}
}

// TestInjectThinkingStringPreserved 注入不得破坏 model/messages 等既有字段。
func TestInjectThinkingStringPreserved(t *testing.T) {
	out := PrepareBodyOptWithEfforts(
		[]byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`),
		false, nil)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["model"] != "deepseek-v4-flash" || m["temperature"] != 0.7 {
		t.Errorf("既有字段被改动: %v", m)
	}
	if _, ok := m["messages"].([]any); !ok {
		t.Errorf("messages 结构破坏: %v", m)
	}
	if !strings.Contains(string(out), `"stream":true`) {
		t.Errorf("stream 未强制: %s", out)
	}
}
