// 本地修复的回归测试（同步上游时不可丢）：
//  1. 裸键名（无冒号）兜底缩写 —— 2026-09-13 实验 F4：assistant 消息反引号引用
//     裸键名即触发 11128，剥离层的正则要求冒号、对裸串无效。
//  2. reasoning_content 与 content 同等净化 —— 思维链回填字段实测也带指纹。
package upstream

import (
	"strings"
	"testing"
)

func TestLocalBareHeaderAbbreviated(t *testing.T) {
	for _, in := range []string{
		"引用 `x-anthropic-billing-header` 这个键",
		"lower: x-anthropic-billing-header",
		"mixed: X-Anthropic-Billing-Header",
	} {
		out := sanitizeText(in)
		if strings.Contains(strings.ToLower(out), "x-anthropic-billing-header") {
			t.Fatalf("裸键名未被兜底: in=%q out=%q", in, out)
		}
		if !strings.Contains(strings.ToLower(out), "x-anthropic-billing-hdr") {
			t.Fatalf("裸键名未缩写为 hdr 形态: in=%q out=%q", in, out)
		}
	}
	// 键值形态仍走整段删除（不留 hdr 残骸）
	out := sanitizeText("prefix x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli; suffix")
	if strings.Contains(strings.ToLower(out), "x-anthropic-billing") {
		t.Fatalf("键值形态应整段删除: out=%q", out)
	}
}

func TestLocalReasoningContentSanitized(t *testing.T) {
	msgs := []any{
		map[string]any{
			"role":              "assistant",
			"content":           nil,
			"reasoning_content": "上文出现过 `x-anthropic-billing-header` 键名",
		},
	}
	if !sanitizeMessages(msgs) {
		t.Fatal("reasoning_content 中的指纹未被净化")
	}
	rc := msgs[0].(map[string]any)["reasoning_content"].(string)
	if strings.Contains(rc, "x-anthropic-billing-header") {
		t.Fatalf("reasoning_content 指纹残留: %q", rc)
	}
}
