package upstream

import (
	"strings"
	"testing"
)

// msgFrame 造一个「非 delta」形态的数据帧：有的上游把**完整消息**放在 choices[0].message
// （不是增量 delta），这正是 sse.go Aggregate 里 `if msg, ok := c["message"]...` 分支针对的形态。
func msgFrame(content string) string {
	return "data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"" + content + "\"}}]}\n\n"
}

// aggregateContent 取聚合结果的 choices[0].message.content。
func aggregateContent(t *testing.T, stream string) string {
	t.Helper()
	resp, err := Aggregate(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	choices, ok := resp["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("choices 形态异常: %#v", resp["choices"])
	}
	msg, ok := choices[0].(map[string]any)["message"].(map[string]any)
	if !ok {
		t.Fatal("message 形态异常")
	}
	s, _ := msg["content"].(string)
	return s
}

// TestAggregateNonDeltaMessageFrameTakenOnce 非 delta 的 message 帧只应被取一次。
//
// message 分支携带的是**完整消息**（见 sse.go 该分支注释），因此同一份完整消息在多个
// 数据帧里重复下发时，不能把每帧都累加进 content。该分支的守卫是 `!gotAnyContent`，
// 但它只读不写 gotAnyContent（只有 delta.content 路径会置位）——多帧同内容时守卫失效，
// 正文被重复拼接，客户端看到的内容翻倍。
func TestAggregateNonDeltaMessageFrameTakenOnce(t *testing.T) {
	stream := msgFrame("Hello") + msgFrame("Hello") + "data: [DONE]\n\n"
	got := aggregateContent(t, stream)
	if got != "Hello" {
		t.Errorf("content = %q，期望 %q\n"+
			"（message 帧携带完整消息，重复下发不应累加；守卫 !gotAnyContent 只读未写导致失效）",
			got, "Hello")
	}
}

// TestAggregateNonDeltaMessageFrameSingle 单帧形态（常见情形）行为不得改变。
func TestAggregateNonDeltaMessageFrameSingle(t *testing.T) {
	stream := msgFrame("世界") + "data: [DONE]\n\n"
	if got := aggregateContent(t, stream); got != "世界" {
		t.Errorf("content = %q，期望 %q", got, "世界")
	}
}

// TestAggregateDeltaWinsOverMessageFrame delta 已有内容时，message 帧必须被忽略（既有守卫语义）。
func TestAggregateDeltaWinsOverMessageFrame(t *testing.T) {
	stream := "data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"delta\"}}]}\n\n" +
		msgFrame("FULL-MESSAGE") +
		"data: [DONE]\n\n"
	if got := aggregateContent(t, stream); got != "delta" {
		t.Errorf("content = %q，期望 %q（delta 优先，message 帧应被忽略）", got, "delta")
	}
}
