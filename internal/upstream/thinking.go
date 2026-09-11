// thinking.go DeepSeek 思维链开启：出站请求体注入 thinking:{type:"enabled"}。
//
// 根因（issue #43，Hermes 逆向官方客户端 codebuddy.js 已确认）：
// 官方客户端对 deepseek 系模型标记 thinkingFormat:"deepseek" + requiresReasoningContentOnAssistantMessages，
// 发请求时「开思考」必须显式带 thinking:{type:"enabled"}，否则上游默认按不思考应答
// （思维链不返回）。网关 payload 层此前完全不感知该字段，透传请求没有这个开关
// → 上游不给思维链；glm/kimi 走其他 thinkingFormat（qwen 系 enable_thinking 或默认开）所以正常。
//
// 行为对齐官方客户端 case "deepseek" 分支：
//   - 请求体已有 thinking.type 非空 → 客户端显式控制，绝不覆盖（enabled/disabled 都尊重）；
//     disabled 时照抄客户端行为一并删除 reasoning_effort（snake/camel 双字段）。
//   - 无 thinking 字段，或 thinking 对象 type 为空/缺失，或有 reasoning_effort → 注入 {type:"enabled"}。
// 非 deepseek 模型（glm/kimi/qwen 等）→ 零改动。
package upstream

import (
	"strings"
)

// isDeepSeekModel 模型名以 deepseek 为前缀（不区分大小写）。
// 覆盖 deepseek-v4.1-flash / deepseek-v4-pro / deepseek-r1 等变体；
// 前缀匹配对齐官方 thinkingFormat:"deepseek" 的判定口径，避免漏注。
func isDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

// injectThinking 按 DeepSeek 思维链开关规则改写请求体。非 deepseek 零改动。
func injectThinking(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	th, ok := obj["thinking"].(map[string]any)
	if ok {
		typ, hasType := th["type"].(string)
		if hasType && strings.TrimSpace(typ) != "" {
			// 客户端显式控制（enabled/disabled 均为明确意图）：不改 type。
			// disabled 时照抄客户端 case "deepseek" 行为：删 reasoning_effort。
			if strings.EqualFold(strings.TrimSpace(typ), "disabled") {
				delete(obj, "reasoning_effort")
				delete(obj, "reasoningEffort")
			}
			return
		}
		// thinking 对象存在但 type 缺失/为空：客户端同样会补 enabled。
		th["type"] = "enabled"
		return
	}
	// 无 thinking（或 thinking 为非法非对象值）→ 注入 enabled。
	// 有 reasoning_effort 也走此分支（effort 保留给既有降级逻辑，开关照开）。
	obj["thinking"] = map[string]any{"type": "enabled"}
}