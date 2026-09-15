package server

// Anthropic Messages adapter. The existing Chat Completions handler remains the
// owner of account selection, retries, cancellation and upstream accounting.
import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type messageObject = map[string]any

func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		writeMessageError(w, 401, "missing or invalid API key")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, h.cfg.MaxBodyBytes+1))
	if err != nil {
		writeMessageError(w, 400, "could not read request body")
		return
	}
	if int64(len(body)) > h.cfg.MaxBodyBytes {
		writeMessageError(w, 413, "request body too large")
		return
	}
	converted, model, stream, err := convertMessagesRequest(body)
	if err != nil {
		writeMessageError(w, 400, err.Error())
		return
	}
	// Preserve Claude Code's session across tool turns and account retries.
	if id := r.Header.Get("X-Claude-Code-Session-Id"); id != "" {
		converted["conversation_id"] = id
	}
	raw, err := json.Marshal(converted)
	if err != nil {
		writeMessageError(w, 400, "could not encode request")
		return
	}
	request := r.Clone(r.Context())
	request.Body = io.NopCloser(bytes.NewReader(raw))
	request.ContentLength = int64(len(raw))
	adapter := newMessagesWriter(w, model, stream)
	h.chatCompletions(adapter, request)
	adapter.finish()
}

func (h *Handler) authorized(r *http.Request) bool {
	if h.cfg.APIKey == "" {
		return true
	}
	// A supplied Authorization header takes precedence over x-api-key.
	if value := r.Header.Get("Authorization"); value != "" {
		return value == "Bearer "+h.cfg.APIKey
	}
	return r.Header.Get("X-Api-Key") == h.cfg.APIKey
}

func messageErrorType(status int) string {
	switch status {
	case 400, 404, 413, 422:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 429:
		return "rate_limit_error"
	case 503, 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}
func writeMessageError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, messageObject{"type": "error", "error": messageObject{"type": messageErrorType(status), "message": msg}})
}

func convertMessagesRequest(raw []byte) (messageObject, string, bool, error) {
	var in messageObject
	if err := json.Unmarshal(raw, &in); err != nil || in == nil {
		return nil, "", false, fmt.Errorf("body must be a JSON object")
	}
	model, _ := in["model"].(string)
	model = strings.TrimSpace(model)
	// Claude Code context-window hints are not part of an upstream model ID.
	model = strings.TrimSuffix(model, "[1m]")
	model = strings.TrimSuffix(model, "[1M]")
	if model == "" {
		return nil, "", false, fmt.Errorf("model is required")
	}
	stream, _ := in["stream"].(bool)
	if v, ok := in["stream"]; ok {
		if _, ok := v.(bool); !ok {
			return nil, "", false, fmt.Errorf("stream must be a boolean")
		}
	}
	maxTokens, ok := in["max_tokens"].(float64)
	if !ok || maxTokens <= 0 || maxTokens != float64(int64(maxTokens)) {
		return nil, "", false, fmt.Errorf("max_tokens must be a positive integer")
	}
	out := messageObject{"model": model, "stream": stream, "max_tokens": maxTokens}
	var messages []any
	if system, ok := in["system"]; ok {
		parts, err := messageParts(system, false)
		if err != nil {
			return nil, "", false, fmt.Errorf("system: %w", err)
		}
		messages = append(messages, messageObject{"role": "system", "content": parts})
	}
	inputs, ok := in["messages"].([]any)
	if !ok || len(inputs) == 0 {
		return nil, "", false, fmt.Errorf("messages must be a non-empty array")
	}
	for i, value := range inputs {
		m, ok := value.(map[string]any)
		if !ok {
			return nil, "", false, fmt.Errorf("messages[%d] must be an object", i)
		}
		role, _ := m["role"].(string)
		// Some Claude Code releases include inline system/developer messages.
		// Preserve their position and normalize them for the OpenAI upstream.
		if role == "system" || role == "developer" {
			parts, err := messageParts(m["content"], false)
			if err != nil {
				return nil, "", false, fmt.Errorf("messages[%d]: %w", i, err)
			}
			messages = append(messages, messageObject{"role": "system", "content": parts})
			continue
		}
		if role != "user" && role != "assistant" {
			return nil, "", false, fmt.Errorf("messages[%d]: unsupported role %q", i, role)
		}
		blocks, err := anthropicBlocks(m["content"])
		if err != nil {
			return nil, "", false, fmt.Errorf("messages[%d]: %w", i, err)
		}
		msg := messageObject{"role": role}
		var content, calls, results []any
		var thinking strings.Builder
		for _, block := range blocks {
			switch block["type"] {
			case "text", "image":
				part, err := messagePart(block, role == "user")
				if err != nil {
					return nil, "", false, err
				}
				content = append(content, part)
			case "thinking":
				if role != "assistant" {
					return nil, "", false, fmt.Errorf("thinking requires assistant role")
				}
				text, _ := block["thinking"].(string)
				thinking.WriteString(text)
			case "redacted_thinking": // Opaque Anthropic-only state cannot be reused upstream.
			case "tool_use":
				if role != "assistant" {
					return nil, "", false, fmt.Errorf("tool_use requires assistant role")
				}
				id, _ := block["id"].(string)
				name, _ := block["name"].(string)
				input, ok := block["input"].(map[string]any)
				if id == "" || name == "" || !ok {
					return nil, "", false, fmt.Errorf("tool_use requires id, name and object input")
				}
				arguments, _ := json.Marshal(input)
				calls = append(calls, messageObject{"id": id, "type": "function", "function": messageObject{"name": name, "arguments": string(arguments)}})
			case "tool_result":
				if role != "user" {
					return nil, "", false, fmt.Errorf("tool_result requires user role")
				}
				id, _ := block["tool_use_id"].(string)
				if id == "" {
					return nil, "", false, fmt.Errorf("tool_result requires tool_use_id")
				}
				value := block["content"]
				if value == nil {
					value = ""
				}
				parts, err := messageParts(value, true)
				if err != nil {
					return nil, "", false, fmt.Errorf("tool_result: %w", err)
				}
				if block["is_error"] == true {
					parts = append([]any{messageObject{"type": "text", "text": "Tool execution failed:"}}, parts...)
				}
				results = append(results, messageObject{"role": "tool", "tool_call_id": id, "content": parts})
			default:
				return nil, "", false, fmt.Errorf("unsupported content block type %q", block["type"])
			}
		}
		// Tool results must immediately follow the assistant tool calls, before new user text.
		messages = append(messages, results...)
		if len(content) > 0 || len(calls) > 0 || thinking.Len() > 0 || len(results) == 0 {
			msg["content"] = content
			if len(content) == 0 {
				msg["content"] = ""
			}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
			}
			if thinking.Len() > 0 {
				msg["reasoning_content"] = thinking.String()
			}
			messages = append(messages, msg)
		}
	}
	out["messages"] = messages
	for _, key := range []string{"temperature", "top_p", "top_k", "metadata"} {
		if v, ok := in[key]; ok {
			out[key] = v
		}
	}
	if v, ok := in["stop_sequences"]; ok {
		out["stop"] = v
	}
	if v, ok := in["thinking"].(map[string]any); ok {
		switch v["type"] {
		case "disabled":
			out["thinking"] = messageObject{"type": "disabled"}
		case "enabled", "adaptive":
			out["thinking"] = messageObject{"type": "enabled"}
		}
	}
	if v, ok := in["output_config"].(map[string]any); ok {
		if effort, ok := v["effort"].(string); ok {
			out["reasoning_effort"] = effort
		}
		if v["format"] != nil {
			return nil, "", false, fmt.Errorf("output_config.format is not supported by this adapter")
		}
	}
	if values, exists := in["tools"]; exists {
		list, ok := values.([]any)
		if !ok {
			return nil, "", false, fmt.Errorf("tools must be an array")
		}
		tools := make([]any, 0, len(list))
		for _, v := range list {
			tool, ok := v.(map[string]any)
			if !ok {
				return nil, "", false, fmt.Errorf("tool must be an object")
			}
			typ, _ := tool["type"].(string)
			if typ != "" && typ != "custom" {
				return nil, "", false, fmt.Errorf("server-side tool type %q is not supported", typ)
			}
			name, _ := tool["name"].(string)
			schema, ok := tool["input_schema"].(map[string]any)
			if name == "" || !ok {
				return nil, "", false, fmt.Errorf("tool requires name and input_schema")
			}
			fn := messageObject{"name": name, "parameters": schema}
			if description, ok := tool["description"].(string); ok {
				fn["description"] = description
			}
			tools = append(tools, messageObject{"type": "function", "function": fn})
		}
		out["tools"] = tools
	}
	if value, exists := in["tool_choice"]; exists {
		choice, ok := value.(map[string]any)
		if !ok {
			return nil, "", false, fmt.Errorf("tool_choice must be an object")
		}
		switch choice["type"] {
		case "auto", "none":
			out["tool_choice"] = choice["type"]
		case "any":
			out["tool_choice"] = "required"
		case "tool":
			name, _ := choice["name"].(string)
			if name == "" {
				return nil, "", false, fmt.Errorf("tool_choice.tool requires name")
			}
			// Upstream accepts only string tool_choice. Restrict the offered schema to
			// the requested tool so "required" retains forced-tool semantics.
			var selected []any
			tools, _ := out["tools"].([]any)
			for _, v := range tools {
				t := v.(map[string]any)
				if t["function"].(map[string]any)["name"] == name {
					selected = append(selected, t)
				}
			}
			if len(selected) != 1 {
				return nil, "", false, fmt.Errorf("selected tool is not defined")
			}
			out["tools"] = selected
			out["tool_choice"] = "required"
		default:
			return nil, "", false, fmt.Errorf("unsupported tool_choice type")
		}
		if value, ok := choice["disable_parallel_tool_use"].(bool); ok {
			out["parallel_tool_calls"] = !value
		}
	}
	return out, model, stream, nil
}

func anthropicBlocks(value any) ([]messageObject, error) {
	if text, ok := value.(string); ok {
		return []messageObject{{"type": "text", "text": text}}, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("content must be a string or block array")
	}
	blocks := make([]messageObject, 0, len(list))
	for _, v := range list {
		block, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("content block must be an object")
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}
func messageParts(value any, images bool) ([]any, error) {
	blocks, err := anthropicBlocks(value)
	if err != nil {
		return nil, err
	}
	parts := make([]any, 0, len(blocks))
	for _, block := range blocks {
		p, err := messagePart(block, images)
		if err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	return parts, nil
}
func messagePart(block messageObject, images bool) (messageObject, error) {
	switch block["type"] {
	case "text":
		text, ok := block["text"].(string)
		if !ok {
			return nil, fmt.Errorf("text block requires text")
		}
		return messageObject{"type": "text", "text": text}, nil
	case "image":
		if !images {
			return nil, fmt.Errorf("images require user content or tool results")
		}
		src, _ := block["source"].(map[string]any)
		var url string
		switch src["type"] {
		case "base64":
			media, _ := src["media_type"].(string)
			data, _ := src["data"].(string)
			if media != "" && data != "" {
				url = "data:" + media + ";base64," + data
			}
		case "url":
			url, _ = src["url"].(string)
		}
		if url == "" {
			return nil, fmt.Errorf("unsupported or missing image source")
		}
		return messageObject{"type": "image_url", "image_url": messageObject{"url": url}}, nil
	default:
		return nil, fmt.Errorf("unsupported content block type %q", block["type"])
	}
}
