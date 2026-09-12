package upstream

import (
	"encoding/json"
	"testing"

	"workbuddy2api/internal/auth"
)

// console 路径要求首条 system：无 system 时补，有则不动。
func TestEnsureConsoleSystem(t *testing.T) {
	// 无 system → 补
	src := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}`)
	out := ensureConsoleSystem(src)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("want 2 msgs, got %d", len(msgs))
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("first msg role=%v, want system", msgs[0].(map[string]any)["role"])
	}
	// 已有 system → 不动
	src2 := []byte(`{"model":"x","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`)
	if got := string(ensureConsoleSystem(src2)); got != string(src2) {
		t.Errorf("system present: body changed: %s", got)
	}
}

// chatURL 按域切路径。
func TestChatURL(t *testing.T) {
	c := New()
	if got := c.chatURL(&auth.Auth{Domain: "copilot.tencent.com"}); got != "https://copilot.tencent.com/v2/chat/completions" {
		t.Errorf("cn url=%q", got)
	}
	if got := c.chatURL(&auth.Auth{Domain: "x.workbuddy.ai"}); got != "https://www.workbuddy.ai/console/chat/completions" {
		t.Errorf("global url=%q", got)
	}
}

// prepareBodyFor：global 无 system 补 system，CN 不动。
func TestPrepareBodyFor(t *testing.T) {
	c := New()
	src := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	g := c.prepareBodyFor(&auth.Auth{Domain: "x.workbuddy.ai"}, src)
	var gobj map[string]any
	_ = json.Unmarshal(g, &gobj)
	if gobj["messages"].([]any)[0].(map[string]any)["role"] != "system" {
		t.Error("global: system not prepended")
	}
	cn := c.prepareBodyFor(&auth.Auth{Domain: "copilot.tencent.com"}, src)
	var cobj map[string]any
	_ = json.Unmarshal(cn, &cobj)
	if cobj["messages"].([]any)[0].(map[string]any)["role"] != "user" {
		t.Error("cn: messages changed unexpectedly")
	}
}
