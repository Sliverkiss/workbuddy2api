package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestMessagesRequestToolRoundTrip(t *testing.T) {
	raw := []byte(`{"model":"cn:deepseek-v4.1-flash[1M]","max_tokens":1024,"stream":true,"system":[{"type":"text","text":"Keep source intact","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":""},{"type":"tool_use","id":"call_1","name":"read","input":{"path":"a.txt"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"file not found"}],"is_error":true},{"type":"text","text":"Try another path"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}],"tools":[{"name":"read","description":"Read file","input_schema":{"type":"object"}},{"name":"write","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"read","disable_parallel_tool_use":true},"output_config":{"effort":"high"}}`)
	out, model, stream, err := convertMessagesRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if model != "cn:deepseek-v4.1-flash" || !stream {
		t.Fatalf("model/stream: %s %v", model, stream)
	}
	messages := out["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages: %#v", messages)
	}
	assistant := messages[1].(map[string]any)
	if assistant["reasoning_content"] != "plan" {
		t.Fatal("lost thinking history")
	}
	tool := messages[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" {
		t.Fatal("lost tool result pairing")
	}
	content := messages[3].(map[string]any)["content"].([]any)
	if content[1].(map[string]any)["type"] != "image_url" {
		t.Fatal("lost image")
	}
	if len(out["tools"].([]any)) != 1 || out["tool_choice"] != "required" || out["parallel_tool_calls"] != false {
		t.Fatal("forced tool semantics lost")
	}
	if out["reasoning_effort"] != "high" {
		t.Fatal("lost effort")
	}
}

func TestMessagesRejectUnsupportedInputs(t *testing.T) {
	for _, raw := range []string{
		`null`, `{}`, `{"model":"m","max_tokens":0,"messages":[]}`,
		`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"document","source":{}}]}]}`,
		`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`,
		`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}],"output_config":{"format":{"type":"json_schema"}}}`,
	} {
		if _, _, _, err := convertMessagesRequest([]byte(raw)); err == nil {
			t.Errorf("accepted unsupported request: %s", raw)
		}
	}
}

func readMessageEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			var event map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatal(err)
			}
			events = append(events, event)
		}
	}
	return events
}
func TestMessagesStreamIncrementalAndTools(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newMessagesWriter(rec, "cn:m", true)
	frame := func(value any) {
		t.Helper()
		raw, _ := json.Marshal(value)
		raw = append(append([]byte("data: "), raw...), []byte("\n\n")...)
		for _, b := range raw {
			if _, err := w.Write([]byte{b}); err != nil {
				t.Fatal(err)
			}
		}
	}
	frame(messageObject{"id": "msg_1", "choices": []any{messageObject{"index": 0, "delta": messageObject{"reasoning_content": "plan"}}}})
	frame(messageObject{"choices": []any{messageObject{"index": 0, "delta": messageObject{"content": "hello"}}}})
	if !rec.Flushed || !strings.Contains(rec.Body.String(), "hello") || strings.Contains(rec.Body.String(), "message_stop") {
		t.Fatal("text must reach client before completion")
	}
	frame(messageObject{"choices": []any{messageObject{"index": 0, "delta": messageObject{"tool_calls": []any{
		messageObject{"index": 1, "id": "call_b", "function": messageObject{"name": "second", "arguments": "{\"b\":"}},
		messageObject{"index": 0, "id": "call_a", "function": messageObject{"name": "first", "arguments": "{\"a\":"}},
	}}}}})
	frame(messageObject{"choices": []any{messageObject{"index": 0, "delta": messageObject{"tool_calls": []any{
		messageObject{"index": 0, "function": messageObject{"arguments": "1}"}}, messageObject{"index": 1, "function": messageObject{"arguments": "2}"}},
	}}, "finish_reason": "tool_calls"}}})
	frame(messageObject{"choices": []any{}, "usage": messageObject{"prompt_tokens": 100, "completion_tokens": 12, "prompt_tokens_details": messageObject{"cached_tokens": 30}}})
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	w.finish()
	events := readMessageEvents(t, rec.Body.String())
	if events[0]["type"] != "message_start" || events[len(events)-1]["type"] != "message_stop" {
		t.Fatalf("bad lifecycle: %s", rec.Body.String())
	}
	starts, stops := 0, 0
	var toolIDs []string
	for _, event := range events {
		switch event["type"] {
		case "content_block_start":
			starts++
			block := event["content_block"].(map[string]any)
			if block["type"] == "tool_use" {
				toolIDs = append(toolIDs, block["id"].(string))
			}
		case "content_block_stop":
			stops++
		case "message_delta":
			if event["delta"].(map[string]any)["stop_reason"] != "tool_use" {
				t.Fatal("lost tool stop reason")
			}
			usage := event["usage"].(map[string]any)
			if usage["input_tokens"] != float64(70) || usage["output_tokens"] != float64(12) || usage["cache_read_input_tokens"] != float64(30) {
				t.Fatalf("incorrect usage: %#v", usage)
			}
		}
	}
	if starts != 4 || stops != starts || strings.Join(toolIDs, ",") != "call_a,call_b" {
		t.Fatalf("block lifecycle/order: %s", rec.Body.String())
	}
}

func TestMessagesStreamRejectsTruncatedCallsAndMissingFinish(t *testing.T) {
	for _, reason := range []string{"tool_calls", "length", ""} {
		rec := httptest.NewRecorder()
		w := newMessagesWriter(rec, "m", true)
		chunk := messageObject{"choices": []any{messageObject{"index": 0, "delta": messageObject{"tool_calls": []any{messageObject{"index": 0, "id": "a", "function": messageObject{"name": "run", "arguments": "{\"command\":"}}}}, "finish_reason": reason}}}
		raw, _ := json.Marshal(chunk)
		_, _ = w.Write(append(append([]byte("data: "), raw...), []byte("\n\ndata: [DONE]\n\n")...))
		w.finish()
		body := rec.Body.String()
		if strings.Contains(body, "\"type\":\"tool_use\"") {
			t.Fatal("invalid tool emitted")
		}
		if reason == "length" {
			if !strings.Contains(body, "max_tokens") {
				t.Fatal(body)
			}
		} else if !strings.Contains(body, "event: error") || strings.Contains(body, "event: message_stop") {
			t.Fatal(body)
		}
	}
}

func TestMessagesHTTPAuthErrorsAndSuccess(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "test", AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	defer p.Close()
	h := NewHandler(Config{Pool: p, APIKey: "secret", Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })})
	for _, stream := range []bool{false, true} {
		body := messageObject{"model": "cn:glm-5.2", "max_tokens": 50, "stream": stream, "messages": []any{messageObject{"role": "user", "content": "hi"}}}
		raw, _ := json.Marshal(body)
		for _, authHeader := range []string{"", "X-Api-Key", "Authorization"} {
			// Pool anti-herding affects immediate repeated picks; reset its state.
			p.SetRandomSource(func(n int64) int64 { return 0 })
			req := httptest.NewRequest("POST", "/v1/messages?beta=true", bytes.NewReader(raw))
			if authHeader == "X-Api-Key" {
				req.Header.Set(authHeader, "secret")
			}
			if authHeader == "Authorization" {
				req.Header.Set(authHeader, "Bearer secret")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if authHeader == "" {
				if rec.Code != 401 || !strings.Contains(rec.Body.String(), "authentication_error") {
					t.Fatal(rec.Body.String())
				}
				continue
			}
			if rec.Code != 200 {
				t.Fatalf("%d: %s", rec.Code, rec.Body.String())
			}
			if stream {
				if !strings.Contains(rec.Body.String(), "event: message_stop") {
					t.Fatal(rec.Body.String())
				}
			} else {
				var result map[string]any
				_ = json.Unmarshal(rec.Body.Bytes(), &result)
				if result["type"] != "message" || result["stop_reason"] != "end_turn" {
					t.Fatal(result)
				}
			}
		}
	}
	h.cfg.MaxBodyBytes = 4
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader("12345"))
	req.Header.Set("X-Api-Key", "secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 413 {
		t.Fatal(rec.Code)
	}
}

func TestMessagesPreservesCancellation(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "test", AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	defer p.Close()
	type contextKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "request"))
	defer cancel()
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	observed := false
	up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		cancel()
		observed = r.Context().Value(contextKey{}) == "request" && r.Context().Err() == context.Canceled
		return nil, r.Context().Err()
	})}
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !observed {
		t.Fatal("request context not propagated")
	}
}

func TestMessagesErrorTranslation(t *testing.T) {
	for _, stream := range []bool{true, false} {
		rec := httptest.NewRecorder()
		w := newMessagesWriter(rec, "m", stream)
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"please retry"}}`)
		w.finish()
		if rec.Code != 429 || !strings.Contains(rec.Body.String(), "rate_limit_error") || strings.Contains(rec.Body.String(), "event:") {
			t.Fatal(rec.Body.String())
		}
	}
}

func TestMessagesNonStreamToolAndUsage(t *testing.T) {
	raw := []byte(`{"id":"msg_1","choices":[{"message":{"reasoning_content":"plan","content":"Done","tool_calls":[{"id":"call_a","function":{"name":"read","arguments":"{\"path\":\"a.txt\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`)
	result, err := convertMessagesResponse(raw, "cn:m")
	if err != nil {
		t.Fatal(err)
	}
	if result["stop_reason"] != "tool_use" || len(result["content"].([]any)) != 3 {
		t.Fatal(result)
	}
}

// Compile-time assertion: adapter must be flushable for upstream.Stream.
var _ http.Flusher = (*messagesWriter)(nil)

func TestMessagesInlineSystem(t *testing.T) {
	out, _, _, err := convertMessagesRequest([]byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"},{"role":"system","content":"inline instructions"},{"role":"developer","content":"developer instructions"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 3 || msgs[1].(map[string]any)["role"] != "system" || msgs[2].(map[string]any)["role"] != "system" {
		t.Fatal(msgs)
	}
}
