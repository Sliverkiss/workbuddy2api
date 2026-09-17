package upstream

import (
	"strings"
	"testing"
)

// aggMsgFrame 造一帧「非 delta」形态：完整消息放在 choices[0].message。
func aggMsgFrame(content string) string {
	return "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"" + content + "\"}}]}\n\n"
}

// aggDeltaFrame 造一帧 delta 形态。
func aggDeltaFrame(content string) string {
	return "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"" + content + "\"}}]}\n\n"
}

// aggContent 取聚合结果正文。
func aggContent(t *testing.T, stream string) string {
	t.Helper()
	resp, err := Aggregate(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	s, _ := msg["content"].(string)
	return s
}

// TestAggregateEmptyMessageFrameThenReal 空 content 的 message 帧不得消费「只采一次」名额，
// 否则紧随其后的真正文帧被守卫挡掉，整条回复静默变空。
//
// 成因：mergeMessageFields 的 latch 判定只看 content 键存在（`msg["content"].(string)` 成功
// 即置 gotAnyContent=true），空串也置位。而 role/元信息先行的空 content 帧是流式常见形态。
// 修复前 content="" → RED（正文全丢）。
func TestAggregateEmptyMessageFrameThenReal(t *testing.T) {
	stream := aggMsgFrame("") + aggMsgFrame("真正文") + "data: [DONE]\n\n"
	if got := aggContent(t, stream); got != "真正文" {
		t.Errorf("content = %q，期望 %q（空 content 帧不应 latch，正文不得丢）", got, "真正文")
	}
}

// TestAggregateEmptyDeltaThenFullMessage 同理：空 content 的 delta 帧不得 latch 掉后续整条 message。
func TestAggregateEmptyDeltaThenFullMessage(t *testing.T) {
	stream := aggDeltaFrame("") + aggMsgFrame("真正文") + "data: [DONE]\n\n"
	if got := aggContent(t, stream); got != "真正文" {
		t.Errorf("content = %q，期望 %q（空 delta 不应 latch，整条 message 正文不得丢）", got, "真正文")
	}
}

// TestAggregateEmptyContentFramesOnly 全流只有空 content 帧：正文为空属正常，且不得报错。
func TestAggregateEmptyContentFramesOnly(t *testing.T) {
	stream := aggDeltaFrame("") + aggMsgFrame("") + "data: [DONE]\n\n"
	if got := aggContent(t, stream); got != "" {
		t.Errorf("content = %q，期望空串", got)
	}
}

// TestAggregateRealMessageStillTakenOnce 非空整条 message 的「只采一次」语义必须保持
// （作者 PR #134 的 latch 修复不得回退）。
func TestAggregateRealMessageStillTakenOnce(t *testing.T) {
	stream := aggMsgFrame("abc") + aggMsgFrame("abc") + aggMsgFrame("abc") + "data: [DONE]\n\n"
	if got := aggContent(t, stream); got != "abc" {
		t.Errorf("content = %q，期望 %q（message 整条只采一次）", got, "abc")
	}
}

// TestAggregateDeltaStreamUnaffected 正常 delta 流聚合不受影响。
func TestAggregateDeltaStreamUnaffected(t *testing.T) {
	stream := aggDeltaFrame("你好") + aggDeltaFrame("，世界") + "data: [DONE]\n\n"
	if got := aggContent(t, stream); got != "你好，世界" {
		t.Errorf("content = %q，期望 %q", got, "你好，世界")
	}
}
