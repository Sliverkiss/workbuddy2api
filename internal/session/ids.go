// ids.go 会话头族 ID 的解析与生成（issue #35 后台聚合）。
//
// 官方 CodeBuddy CLI 出站头族（X-Conversation-ID / X-Conversation-Request-ID /
// X-Request-ID / X-B3-*），后台按 X-Conversation-Request-ID（对话轮）聚合请求；
// 本文件提供 conversationId 提取、消息级 32 hex messageID、以及"同一会话键
// 稳定复用"的 conversationRequestID 纯派生（加盐），供 handler 轮转循环外生成、
// 循环内复用（换号/重试/降级全部同 ID → 后台不再碎片化）。
package session

import (
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
)

// ResolveConversationID 从请求体提取会话头族的 conversationId（snake/camel 双形态，
// 复用 ExtractKey 的识别顺序：metadata 优先、snake 优先于 camel）。
// 与 ExtractKey 的差异：**只认 conversationId，绝不回落 user_id**——X-Conversation-ID
// 语义是"对话 ID"，user_id 回落会污染后台按对话聚合的判据。
// 缺失返回 ""（不伪造：透传客户端原值优先，客户端没给就不发）。
func ResolveConversationID(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
	}
	if v := strOrEmpty(obj["conversation_id"]); v != "" {
		return v
	}
	return strOrEmpty(obj["conversationId"])
}

// NewMessageID 生成消息级 ID：32 位 hex（UUID v4 去横线的长度形态），对齐官方
// X-Request-ID / X-Conversation-Message-ID。crypto/rand 失败（理论上不可能）时回落
// math/rand/v2 双 uint64 拼 32 hex——恒 32 hex、恒合法，可安全用作 B3 TraceId。
func NewMessageID() string {
	b := make([]byte, 16)
	if _, err := cryptorand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	// 熵源故障的极端兜底：仍保证 32 hex（fallbackID 不 panic、不空串）。
	return fmt.Sprintf("%016x%016x", uint64(rand.Uint64())|1, rand.Uint64())
}

// deriveSalt 进程启动随机盐：对所有稳定聚合 ID 的纯派生统一加盐，使派生值无法
// 按外部可控的键内容（会话键/轮级键）被预计算；重启换新（重启时旧对话轮/会话已
// 结束，不构成断档）。会话级（RequestIDForKey）与轮级（TurnRequestID）共用同一盐。
var deriveSalt = NewMessageID()

// RequestIDForKey 返回会话键的稳定 conversationRequestID：sha256(盐|键) 前 16 字节
// 的 hex，纯派生（无缓存、无 TTL、内存不随键数增长）。
//   - 同 key：进程内恒派生同值（一次 user send/同会话多轮聚合）；
//   - 异 key：各自独立，互不相同；
//   - 空 key：每次生成新值（无会话则无"会话内稳定"语义——调用方应在请求级
//     捕获复用，handler 在轮转循环外取一次即天然共享）。
//
// 返回值恒为 32 hex（NewMessageID 形态），可直接用作 B3 TraceId（16/32 hex 合法）。
func RequestIDForKey(key string) string {
	if key == "" {
		return NewMessageID()
	}
	sum := sha256.Sum256([]byte(deriveSalt + "|" + key))
	return hex.EncodeToString(sum[:16])
}

// TurnKey 派生「对话轮级」聚合键：body 里**最后一条** role=="user" 消息的
// 「序号 + 文本」。
//
// 为什么需要它：无会话键的客户端（OpenAI 兼容协议——dsh / Codex / Cherry Studio
// 等的请求体里既无 conversationId 也无 metadata 键）会让 ExtractKey 恒返回空串，
// 会话头族的聚合主键便只能逐请求新生成，agent 多轮在上游用量明细里仍是一条请求
// 一条记录。本函数给这类客户端一个**不依赖客户端配合**的轮级键：一次用户发送内的
// 所有上游调用（tool call 多轮 / 换号重试 / 降级重发）body 里最后一条 user 消息
// 恒定 → 同键；用户发下一条消息 → 换键。
//
// 为什么不取第一条 user 消息：首条在整个会话内不变，会把一次会话的所有轮并进
// 同一个聚合键（跨对话轮混并）。取最后一条才对齐官方 X-Conversation-Request-ID
// 的「对话轮」语义。序号一并入键：两次不同轮里内容相同的提问（"继续"）不会被并成
// 一轮。
//
// 无 body / 无 messages / 无 user 消息 / 该消息无文本 → ""（调用方回落请求级随机
// ID，不伪造聚合键）。
func TurnKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	for i := len(obj.Messages) - 1; i >= 0; i-- {
		if obj.Messages[i].Role != "user" {
			continue
		}
		text := contentText(obj.Messages[i].Content)
		if text == "" {
			// 最后一条 user 消息没有文本（纯图片等）→ 本轮不建立聚合键。
			// 不继续往前找：整轮内该消息位置恒定，往前找反而会让键随 step 漂移。
			return ""
		}
		return fmt.Sprintf("u%d:%s", i, text)
	}
	return ""
}

// contentText 取消息 content 的文本：字符串形态直接返回；数组形态（多模态 parts）
// 拼接各 part 的 text 字段。无文本（纯图片 / null / 未知形态）返回 ""。
func contentText(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	switch s[0] {
	case '"':
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return ""
		}
		return str
	case '[':
		var parts []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &parts); err != nil {
			return ""
		}
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// TurnRequestID 返回轮级键对应的聚合 ID：sha256(盐|键) 前 16 字节的 hex（32 位，
// 与 NewMessageID 同形态，可直接作 B3 TraceId）。与会话级 RequestIDForKey 共用同一
// 派生盐（deriveSalt），两者皆纯派生、无缓存、内存有界。
// 空键返回新随机值（无轮可聚合时保持原有的「每请求独立」行为）。
func TurnRequestID(turnKey string) string {
	if turnKey == "" {
		return NewMessageID()
	}
	sum := sha256.Sum256([]byte(deriveSalt + "|" + turnKey))
	return hex.EncodeToString(sum[:16])
}
