package upstream

import (
	"strings"
	"testing"
)

// issue #142（接续 PR #141）：非 delta message 帧的 content latch 语义。
// 规约：
//   1. 单帧 message：非空 content 取一次；
//   2. 重复 message 帧：content 不重复累加（#134/#137 已修，回归保护）；
//   3. delta 优先：delta 已取正文则 message 帧整体跳过（latch，#134 语义）；
//   4. 空 content 帧不吞 latch：content="" 不置位，后续非空 content 帧仍可取
//      （#141 报告的边界：帧1 message.content="" / 帧2 message.content="Hello"
//      期望输出 "Hello"，master 现状输出 ""——空帧吞掉 latch）；
//   5. 空 content 帧的 role/reasoning_content/tool_calls 仍照常合并
//      （只有 content 的 latch 受影响，不得误伤 #137 的同构透出）。

func TestAggregateMessageFormSingleFrame(t *testing.T) {
	raw := `data: {"id":"x1","object":"chat.completion.chunk","created":1,"model":"glm-5.2","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"}}],"usage":null}
data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Hello" {
		t.Errorf("content=%q want Hello", msg["content"])
	}
}

func TestAggregateMessageFormDuplicateNotDoubled(t *testing.T) {
	raw := `data: {"id":"x1","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"}}]}
data: {"id":"x1","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"}}]}
data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Hello" {
		t.Errorf("content=%q want Hello (duplicate message frames must not double)", msg["content"])
	}
}

func TestAggregateMessageFormDeltaTakesPrecedence(t *testing.T) {
	raw := `data: {"id":"x1","choices":[{"index":0,"delta":{"role":"assistant","content":"Delta body"}}]}
data: {"id":"x1","choices":[{"index":0,"message":{"role":"assistant","content":"Message body"}}]}
data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Delta body" {
		t.Errorf("content=%q want Delta body (delta precedence)", msg["content"])
	}
}

// TestAggregateMessageFormEmptyContentKeepsLatch RED：空 content 帧不得置位
// gotAnyContent——否则后续真正带正文的 message 帧被 `!gotAnyContent` 守卫拒掉。
func TestAggregateMessageFormEmptyContentKeepsLatch(t *testing.T) {
	raw := `data: {"id":"x1","choices":[{"index":0,"message":{"role":"assistant","content":""}}]}
data: {"id":"x1","choices":[{"index":0,"message":{"content":"Hello"}}]}
data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Hello" {
		t.Errorf("content=%q want Hello (empty content frame must not consume the latch)", msg["content"])
	}
}

// TestAggregateMessageFormEmptyFrameStillMergesOtherFields：空 content 帧的
// role / reasoning_content / tool_calls 仍照常并入（#137 同构透出不受本修复影响）。
func TestAggregateMessageFormEmptyFrameStillMergesOtherFields(t *testing.T) {
	raw := `data: {"id":"x1","choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning_content":"think think","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]}}]}
data: {"id":"x1","choices":[{"index":0,"message":{"content":"Hello"}}]}
data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["role"] != "assistant" {
		t.Errorf("role=%v want assistant", msg["role"])
	}
	if msg["reasoning_content"] != "think think" {
		t.Errorf("reasoning_content=%v", msg["reasoning_content"])
	}
	calls, ok := msg["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls=%#v", msg["tool_calls"])
	}
	if fn, ok := calls[0]["function"].(map[string]any); !ok || fn["name"] != "get_weather" {
		t.Errorf("fn=%v", calls[0]["function"])
	}
	if msg["content"] != "Hello" {
		t.Errorf("content=%q want Hello (empty content frame must not consume the latch)", msg["content"])
	}
}
