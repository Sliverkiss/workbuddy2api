package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

type openAIMessage struct {
	Content   string       `json:"content"`
	Reasoning string       `json:"reasoning_content"`
	ToolCalls []openAITool `json:"tool_calls"`
}
type openAITool struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type openAIEnvelope struct {
	ID      string `json:"id"`
	Choices []struct {
		Index   int           `json:"index"`
		Delta   openAIMessage `json:"delta"`
		Message openAIMessage `json:"message"`
		Finish  string        `json:"finish_reason"`
	} `json:"choices"`
	Usage map[string]any `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func anthropicUsage(usage map[string]any) messageObject {
	number := func(key string) int { v, _ := usage[key].(float64); return int(v) }
	cached := number("cache_read_input_tokens")
	if cached == 0 {
		cached = number("prompt_cache_hit_tokens")
	}
	if cached == 0 {
		if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
			v, _ := details["cached_tokens"].(float64)
			cached = int(v)
		}
	}
	created := number("cache_creation_input_tokens")
	input := number("prompt_tokens") - cached - created
	if input < 0 {
		input = 0
	}
	return messageObject{"input_tokens": input, "output_tokens": number("completion_tokens"), "cache_read_input_tokens": cached, "cache_creation_input_tokens": created}
}
func anthropicStop(reason string, tools bool) string {
	if reason == "length" {
		return "max_tokens"
	}
	if tools {
		return "tool_use"
	}
	if reason == "content_filter" {
		return "refusal"
	}
	return "end_turn"
}
func toolBlock(tool openAITool) (messageObject, error) {
	if tool.ID == "" || tool.Function.Name == "" {
		return nil, fmt.Errorf("upstream tool call is missing id or name")
	}
	args := tool.Function.Arguments
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	var input map[string]any
	if json.Unmarshal([]byte(args), &input) != nil || input == nil {
		return nil, fmt.Errorf("upstream tool arguments are not a complete JSON object")
	}
	return messageObject{"type": "tool_use", "id": tool.ID, "name": tool.Function.Name, "input": input}, nil
}
func messageShell(id, model string, content []any, usage messageObject) messageObject {
	if id == "" {
		id = fmt.Sprintf("msg_%d", time.Now().UnixNano())
	}
	return messageObject{"id": id, "type": "message", "role": "assistant", "model": model, "content": content, "stop_reason": nil, "stop_sequence": nil, "usage": usage}
}
func convertMessagesResponse(raw []byte, model string) (messageObject, error) {
	var resp openAIEnvelope
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("invalid upstream response")
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%s", resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("upstream response has no choices")
	}
	choice := resp.Choices[0]
	blocks := make([]any, 0)
	if choice.Message.Reasoning != "" {
		blocks = append(blocks, messageObject{"type": "thinking", "thinking": choice.Message.Reasoning, "signature": ""})
	}
	if choice.Message.Content != "" {
		blocks = append(blocks, messageObject{"type": "text", "text": choice.Message.Content})
	}
	tools := false
	for _, tool := range choice.Message.ToolCalls {
		block, err := toolBlock(tool)
		if err != nil {
			if choice.Finish == "length" {
				continue
			}
			return nil, err
		}
		blocks = append(blocks, block)
		tools = true
	}
	if len(blocks) == 0 {
		blocks = append(blocks, messageObject{"type": "text", "text": ""})
	}
	out := messageShell(resp.ID, model, blocks, anthropicUsage(resp.Usage))
	out["stop_reason"] = anthropicStop(choice.Finish, tools)
	return out, nil
}

// messagesWriter translates synchronously in Write: client backpressure and
// disconnection errors propagate to the existing upstream reader without a new
// goroutine or a second HTTP hop. Text/thinking flush as each delta arrives.
// Tool JSON is buffered until complete so malformed or truncated calls cannot
// cause a client to execute a partial command.
type messagesWriter struct {
	target        http.ResponseWriter
	header        http.Header
	model         string
	stream        bool
	status        int
	buffer        bytes.Buffer
	started, done bool
	writeErr      error
	reason        string
	usage         map[string]any
	blockType     string
	blockIndex    int
	tools         map[int]*openAITool
	toolBytes     int
}

func newMessagesWriter(w http.ResponseWriter, model string, stream bool) *messagesWriter {
	return &messagesWriter{target: w, header: make(http.Header), model: model, stream: stream, status: 200, blockIndex: -1, tools: make(map[int]*openAITool)}
}
func (w *messagesWriter) Header() http.Header    { return w.header }
func (w *messagesWriter) WriteHeader(status int) { w.status = status }
func (w *messagesWriter) Flush()                 {} // emit flushes the actual downstream events.
func (w *messagesWriter) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if !w.stream || w.status >= 400 {
		return w.buffer.Write(p)
	}
	w.buffer.Write(p)
	for {
		data := w.buffer.Bytes()
		index := bytes.IndexByte(data, '\n')
		if index < 0 {
			break
		}
		line := strings.TrimRight(string(w.buffer.Next(index+1)), "\r\n")
		if err := w.line(line); err != nil {
			w.writeErr = err
			return 0, err
		}
	}
	return len(p), nil
}
func (w *messagesWriter) emit(kind string, data messageObject) error {
	data["type"] = kind
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	w.target.Header().Set("Content-Type", "text/event-stream")
	w.target.Header().Set("Cache-Control", "no-cache")
	w.target.Header().Set("X-Accel-Buffering", "no")
	if _, err = fmt.Fprintf(w.target, "event: %s\ndata: %s\n\n", kind, raw); err != nil {
		return err
	}
	if fl, ok := w.target.(http.Flusher); ok {
		fl.Flush()
	}
	return nil
}
func (w *messagesWriter) start(id string) error {
	if w.started {
		return nil
	}
	w.started = true
	return w.emit("message_start", messageObject{"message": messageShell(id, w.model, []any{}, anthropicUsage(w.usage))})
}
func (w *messagesWriter) closeBlock() error {
	if w.blockType == "" {
		return nil
	}
	if w.blockType == "thinking" {
		// Non-Claude upstreams do not issue Anthropic cryptographic signatures.
		if err := w.emit("content_block_delta", messageObject{"index": w.blockIndex, "delta": messageObject{"type": "signature_delta", "signature": ""}}); err != nil {
			return err
		}
	}
	w.blockType = ""
	return w.emit("content_block_stop", messageObject{"index": w.blockIndex})
}
func (w *messagesWriter) text(kind, text string) error {
	if text == "" {
		return nil
	}
	if w.blockType != kind {
		if err := w.closeBlock(); err != nil {
			return err
		}
		w.blockIndex++
		w.blockType = kind
		block := messageObject{"type": kind}
		if kind == "text" {
			block["text"] = ""
		} else {
			block["thinking"] = ""
			block["signature"] = ""
		}
		if err := w.emit("content_block_start", messageObject{"index": w.blockIndex, "content_block": block}); err != nil {
			return err
		}
	}
	delta := messageObject{"type": kind + "_delta", kind: text}
	return w.emit("content_block_delta", messageObject{"index": w.blockIndex, "delta": delta})
}
func (w *messagesWriter) streamError(msg string) error {
	w.done = true
	return w.emit("error", messageObject{"error": messageObject{"type": "api_error", "message": msg}})
}
func (w *messagesWriter) line(line string) error {
	if w.done {
		return nil
	}
	if strings.HasPrefix(line, ":") {
		return w.emit("ping", messageObject{})
	}
	if !strings.HasPrefix(line, "data:") {
		return nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "[DONE]" {
		return w.end()
	}
	var chunk openAIEnvelope
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return w.streamError("invalid upstream stream event")
	}
	if chunk.Error != nil {
		return w.streamError(chunk.Error.Message)
	}
	if chunk.Usage != nil {
		w.usage = chunk.Usage
	}
	if err := w.start(chunk.ID); err != nil {
		return err
	}
	for _, choice := range chunk.Choices {
		if choice.Index != 0 {
			continue
		}
		if choice.Finish != "" {
			w.reason = choice.Finish
		}
		if err := w.text("thinking", choice.Delta.Reasoning); err != nil {
			return err
		}
		if err := w.text("text", choice.Delta.Content); err != nil {
			return err
		}
		for _, delta := range choice.Delta.ToolCalls {
			tool := w.tools[delta.Index]
			if tool == nil {
				tool = &openAITool{Index: delta.Index}
				w.tools[delta.Index] = tool
			}
			if delta.ID != "" {
				tool.ID = delta.ID
			}
			if delta.Function.Name != "" {
				tool.Function.Name = delta.Function.Name
			}
			tool.Function.Arguments += delta.Function.Arguments
			w.toolBytes += len(delta.Function.Arguments)
			if w.toolBytes > 8<<20 {
				return w.streamError("upstream tool arguments exceed 8 MB")
			}
		}
	}
	return nil
}
func (w *messagesWriter) end() error {
	if w.done {
		return nil
	}
	if !w.started || w.reason == "" {
		return w.streamError("upstream stream ended without a finish reason")
	}
	if err := w.closeBlock(); err != nil {
		return err
	}
	indexes := make([]int, 0, len(w.tools))
	for i := range w.tools {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	blocks := make([]messageObject, 0, len(indexes))
	// Validate every call before emitting any tool block.
	for _, index := range indexes {
		block, err := toolBlock(*w.tools[index])
		if err != nil {
			if w.reason == "length" {
				continue
			}
			return w.streamError(err.Error())
		}
		blocks = append(blocks, block)
	}
	for _, block := range blocks {
		w.blockIndex++
		input, _ := json.Marshal(block["input"])
		block["input"] = messageObject{}
		if err := w.emit("content_block_start", messageObject{"index": w.blockIndex, "content_block": block}); err != nil {
			return err
		}
		if err := w.emit("content_block_delta", messageObject{"index": w.blockIndex, "delta": messageObject{"type": "input_json_delta", "partial_json": string(input)}}); err != nil {
			return err
		}
		if err := w.emit("content_block_stop", messageObject{"index": w.blockIndex}); err != nil {
			return err
		}
	}
	if err := w.emit("message_delta", messageObject{"delta": messageObject{"stop_reason": anthropicStop(w.reason, len(blocks) > 0), "stop_sequence": nil}, "usage": anthropicUsage(w.usage)}); err != nil {
		return err
	}
	w.done = true
	return w.emit("message_stop", messageObject{})
}
func (w *messagesWriter) finish() {
	if w.writeErr != nil {
		return
	}
	if w.status >= 400 {
		var resp openAIEnvelope
		msg := http.StatusText(w.status)
		if json.Unmarshal(w.buffer.Bytes(), &resp) == nil && resp.Error != nil {
			msg = resp.Error.Message
		}
		writeMessageError(w.target, w.status, msg)
		return
	}
	if w.stream {
		if w.buffer.Len() > 0 {
			if err := w.line(w.buffer.String()); err != nil {
				return
			}
		}
		if !w.done {
			_ = w.streamError("upstream stream interrupted")
		}
		return
	}
	response, err := convertMessagesResponse(w.buffer.Bytes(), w.model)
	if err != nil {
		writeMessageError(w.target, 502, err.Error())
		return
	}
	writeJSON(w.target, 200, response)
}
