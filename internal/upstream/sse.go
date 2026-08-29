// sse.go 处理上游 SSE 流：聚合成单个 OpenAI 响应，或透传给客户端。
package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Aggregate 读取完整 SSE 流，聚合 delta.content 为单个 OpenAI chat.completion 响应。
// 分片/半行由 bufio.Reader.ReadString 处理；遇到 "data: [DONE]" 结束。
// tool_calls 以流式 delta 到达（按 index 合并：首片带 id/type/name，后续只带 arguments 片段）。
func Aggregate(r io.Reader) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id, model     string
		created       float64
		content       strings.Builder
		reasoning     strings.Builder
		role          = "assistant"
		finishReason  = "stop"
		usage         map[string]any
		gotAnyContent bool
		toolCalls     = map[int]map[string]any{}
		toolOrder     []int
	)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				// drain nothing; done
			} else {
				var chunk map[string]any
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					if v, ok := chunk["id"].(string); ok && id == "" {
						id = v
					}
					if v, ok := chunk["model"].(string); ok && model == "" {
						model = v
					}
					if v, ok := chunk["created"].(float64); ok && created == 0 {
						created = v
					}
					if u, ok := chunk["usage"].(map[string]any); ok {
						usage = u
					}
					if ch, ok := chunk["choices"].([]any); ok {
						for _, ci := range ch {
							c, _ := ci.(map[string]any)
							if c == nil {
								continue
							}
							if fr, ok := c["finish_reason"].(string); ok && fr != "" {
								finishReason = fr
							}
							if delta, ok := c["delta"].(map[string]any); ok {
								if r2, ok := delta["role"].(string); ok && r2 != "" {
									role = r2
								}
								if txt, ok := delta["content"].(string); ok {
									content.WriteString(txt)
									gotAnyContent = true
								}
								if rc, ok := delta["reasoning_content"].(string); ok {
									reasoning.WriteString(rc)
								}
								if tcs, ok := delta["tool_calls"].([]any); ok {
									for _, tc := range tcs {
										call, ok := tc.(map[string]any)
										if !ok {
											continue
										}
										idx := 0
										if v, ok := call["index"].(float64); ok {
											idx = int(v)
										}
										merged, seen := toolCalls[idx]
										if !seen {
											merged = map[string]any{"index": idx}
											toolCalls[idx] = merged
											toolOrder = append(toolOrder, idx)
										}
										mergeToolCallDelta(merged, call)
									}
								}
							}
							// 有的上游把完整消息放在 message 里（非 delta）
							if msg, ok := c["message"].(map[string]any); ok && !gotAnyContent {
								if txt, ok := msg["content"].(string); ok {
									content.WriteString(txt)
								}
							}
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// sortInts 升序排序（避免引 sort 包只为三行）。
func sortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

// 流式策略：逐帧透传（规范化后空 content 噪声已除，TUI 单 part 连续渲染，
// 恢复与上游一致的平滑流式）。如 TUI 思考区再现一词一行，将下方 mustFlush
// 策略改回按阈值合并即可（git 历史 sse.go.bak4-* 有完整实现）。

// Stream 透传上游 SSE 到 w：可合并的 delta 帧按阈值批量合并后下发，其余帧
// （finish/usage/tool_calls/[DONE] 等）先冲刷待批再原样透传，保证至少写一个 [DONE]。
// 调用方必须先设置过 status 200；本函数自设 SSE headers。
func Stream(w http.ResponseWriter, r io.Reader) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	var (
		base       map[string]any // 批次首帧模板（保留 id/model/created/role 等元数据）
		hasC, hasR bool
		sbC, sbR   strings.Builder
		sawDone    bool
	)

	// normalizeFrame 以 OpenAI 流式规范白名单重建帧：仅保留标准字段，
	// 剔除上游噪声（finish_reason:"" → null、空 content/refusal、空 tool_calls 列表、
	// null function_call/extra_fields、顶层未知字段），空 delta 键一律省略，
	// 保证任意标准客户端按规范解析。
	normalizeFrame := func(obj map[string]any) map[string]any {
		out := map[string]any{}
		for _, k := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
			if v, ok := obj[k]; ok && v != nil {
				out[k] = v
			}
		}
		if _, ok := out["object"]; !ok {
			out["object"] = "chat.completion.chunk"
		}
		if _, ok := out["id"]; !ok {
			out["id"] = "chatcmpl-wb2api"
		}
		if chs, ok := obj["choices"].([]any); ok {
			nchs := make([]any, 0, len(chs))
			for _, ci := range chs {
				c, ok := ci.(map[string]any)
				if !ok {
					continue
				}
				nc := map[string]any{}
				if idx, ok := c["index"]; ok {
					nc["index"] = idx
				}
				delta := map[string]any{}
				if d, ok := c["delta"].(map[string]any); ok {
					if v, ok := d["role"].(string); ok && v != "" {
						delta["role"] = v
					}
					if v, ok := d["content"].(string); ok && v != "" {
						delta["content"] = v
					}
					if v, ok := d["reasoning_content"].(string); ok && v != "" {
						delta["reasoning_content"] = v
					}
					if v, ok := d["refusal"].(string); ok && v != "" {
						delta["refusal"] = v
					}
					if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
						delta["tool_calls"] = tcs
					}
					if fc, ok := d["function_call"]; ok && fc != nil {
						// 空占位 function_call（name/arguments 全空）视为噪声剔除
						keep := false
						if fcm, ok2 := fc.(map[string]any); ok2 {
							n, _ := fcm["name"].(string)
							a, _ := fcm["arguments"].(string)
							keep = n != "" || a != ""
						} else {
							keep = true
						}
						if keep {
							delta["function_call"] = fc
						}
					}
				}
				nc["delta"] = delta
				if fr, ok := c["finish_reason"].(string); ok && fr != "" {
					nc["finish_reason"] = fr
				} else {
					nc["finish_reason"] = nil
				}
				nchs = append(nchs, nc)
			}
			out["choices"] = nchs
		}
		if u, ok := obj["usage"]; ok {
			out["usage"] = u
		} else {
			out["usage"] = nil
		}
		return out
	}

	flush := func() error {
		if base == nil {
			return nil
		}
		if chs, ok := base["choices"].([]any); ok && len(chs) > 0 {
			if ch0, ok := chs[0].(map[string]any); ok {
				delta, _ := ch0["delta"].(map[string]any)
				if delta == nil {
					delta = map[string]any{}
					ch0["delta"] = delta
				}
				// 空串字段直接删 key：纯 reasoning 帧不带 content:""，避免客户端
				// 每帧因空 content 建多余 text part/边界（表现为频繁换行）。
				if hasC {
					if s := sbC.String(); s != "" {
						delta["content"] = s
					} else {
						delete(delta, "content")
					}
				}
				if hasR {
					if s := sbR.String(); s != "" {
						delta["reasoning_content"] = s
					} else {
						delete(delta, "reasoning_content")
					}
				}
			}
		}
		raw, err := json.Marshal(normalizeFrame(base))
		if err != nil {
			return err
		}
		base, hasC, hasR = nil, false, false
		sbC.Reset()
		sbR.Reset()
		if _, werr := io.WriteString(w, "data: "+string(raw)+"\n\n"); werr != nil {
			return werr
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	// mergeable 判断帧能否并入批次：仅含 content/reasoning_content 的普通 delta 帧，
	// finish_reason/usage/tool_calls/function_call 帧一律透传。
	mergeable := func(payload string) (map[string]any, bool) {
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			return nil, false
		}
		if u, has := obj["usage"]; has && u != nil {
			return nil, false
		}
		chs, ok := obj["choices"].([]any)
		if !ok || len(chs) == 0 {
			return nil, false
		}
		ch0, ok := chs[0].(map[string]any)
		if !ok {
			return nil, false
		}
		if fr, ok := ch0["finish_reason"].(string); ok && fr != "" {
			return nil, false
		}
		delta, ok := ch0["delta"].(map[string]any)
		if !ok {
			return nil, false
		}
		// 空 tool_calls 列表（上游每帧都带 "tool_calls":[]）不算工具帧
		if tcs, has := delta["tool_calls"]; has {
			if list, ok := tcs.([]any); ok {
				if len(list) > 0 {
					return nil, false
				}
			} else if tcs != nil {
				return nil, false
			}
		}
		if fc, has := delta["function_call"]; has && fc != nil {
			return nil, false
		}
		_, hc := delta["content"].(string)
		_, hr := delta["reasoning_content"].(string)
		if !hc && !hr {
			return nil, false
		}
		return obj, true
	}

	writePassthrough := func(payload string) error {
		if _, werr := io.WriteString(w, "data: "+payload+"\n\n"); werr != nil {
			return werr
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "data: [DONE]"):
			if ferr := flush(); ferr != nil {
				return ferr
			}
			sawDone = true
			if werr := writePassthrough("[DONE]"); werr != nil {
				return werr
			}
		case strings.HasPrefix(trimmed, "data: "):
			payload := strings.TrimPrefix(trimmed, "data: ")
			if obj, ok := mergeable(payload); ok {
				// 冲刷策略：逐帧即发（平滑流式）。规范化已剥空 content 噪声，
				// TUI 不再因帧边界断行；切换点（思考↔回答）由下一帧冲刷保证顺序。
				if base != nil {
					if ferr := flush(); ferr != nil {
						return ferr
					}
				}
				if base == nil {
					base = obj
				}
				if chs, ok := obj["choices"].([]any); ok && len(chs) > 0 {
					if ch0, ok := chs[0].(map[string]any); ok {
						delta, _ := ch0["delta"].(map[string]any)
						if c, ok := delta["content"].(string); ok && c != "" {
							hasC = true
							sbC.WriteString(c)
						}
						if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
							hasR = true
							sbR.WriteString(rc)
						}
					}
				}
			} else {
				if ferr := flush(); ferr != nil {
					return ferr
				}
				// 透传帧同样按规范白名单重建（finish/usage/tool_calls 帧等）
				var pobj map[string]any
				if json.Unmarshal([]byte(payload), &pobj) == nil {
					praw, err := json.Marshal(normalizeFrame(pobj))
					if err == nil {
						payload = string(praw)
					}
				}
				if werr := writePassthrough(payload); werr != nil {
					return werr
				}
			}
		case trimmed != "":
			// 注释/其他行：冲刷待批后原样透传
			if ferr := flush(); ferr != nil {
				return ferr
			}
			if _, werr := io.WriteString(w, line); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
		}
		// 空行（帧分隔）吞掉：本函数自产 "\n\n"
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	if ferr := flush(); ferr != nil {
		return ferr
	}
	if !sawDone {
		if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
	}
	return nil
}
