package upstream

import (
	"encoding/json"
	"testing"
)

func TestPrepareBodyConvertsMaxCompletionTokens(t *testing.T) {
	out := PrepareBodyOptWithEffortsAndDefault([]byte(`{"model":"deepseek-v4.1-flash","messages":[],"max_completion_tokens":128000}`), false, nil, nil)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"] != float64(128000) {
		t.Fatalf("max_tokens=%v, want 128000", got["max_tokens"])
	}
	if _, ok := got["max_completion_tokens"]; ok {
		t.Fatal("alias remains")
	}
}

func TestPrepareBodyPrefersMaxTokens(t *testing.T) {
	out := PrepareBodyOptWithEffortsAndDefault([]byte(`{"messages":[],"max_tokens":64000,"max_completion_tokens":128000}`), false, nil, nil)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"] != float64(64000) {
		t.Fatalf("max_tokens=%v, want 64000", got["max_tokens"])
	}
	if _, ok := got["max_completion_tokens"]; ok {
		t.Fatal("alias remains")
	}
}
