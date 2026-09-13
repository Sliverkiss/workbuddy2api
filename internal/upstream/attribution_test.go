// attribution_test.go 用量归属头 + 客户端 IP 透传单测。
package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
)

// chatHeadersReq 构造一个 chat 请求并应用 ChatHeaders，发到测试 server，
// 返回 server 捕获到的所有头。便于断言归属/IP 头。
func chatHeadersReq(t *testing.T, c *Client, a *auth.Auth) http.Header {
	t.Helper()
	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()
	c.ChatBaseCN = srv.URL
	c.ChatHTTP = srv.Client()
	c.HTTP = srv.Client()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v2/chat/completions", nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	c.ChatHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	return captured
}

// TestAgentPurposeHeadersSet ClientName 配 WorkBuddy 时四头齐全跟随该值。
func TestAgentPurposeHeadersSet(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{ClientName: "WorkBuddy"}
	h := chatHeadersReq(t, c, a)
	for _, tc := range []struct {
		header string
		want   string
	}{
		{"X-Agent-Purpose", "conversation"},
		{"X-IDE-Name", "WorkBuddy"},
		{"X-IDE-Type", "WorkBuddy"},
		{"X-Product", "WorkBuddy"},
	} {
		if got := h.Get(tc.header); got != tc.want {
			t.Errorf("%s = %q want %q", tc.header, got, tc.want)
		}
	}
}

// TestProductDefaultSaaS ClientName 空（缺省）时 X-Product=SaaS 且不设 X-IDE-*（旧行为）。
func TestProductDefaultSaaS(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{} // ClientName 空
	h := chatHeadersReq(t, c, a)
	if got := h.Get("X-Product"); got != "SaaS" {
		t.Errorf("X-Product = %q want %q", got, "SaaS")
	}
	// X-IDE-Name/Type 不应被设置。
	for _, hdr := range []string{"X-IDE-Name", "X-IDE-Type", "X-Agent-Purpose"} {
		if got := h.Get(hdr); got != "" {
			t.Errorf("%s = %q want empty (not set in SaaS default)", hdr, got)
		}
	}
}

// TestProductWorkBuddy_WhenConfigured client_name=WorkBuddy 时 X-Product 跟随（等价覆盖首测，保独立命名）。
func TestProductWorkBuddy_WhenConfigured(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{ClientName: "WorkBuddy"}
	h := chatHeadersReq(t, c, a)
	if got := h.Get("X-Product"); got != "WorkBuddy" {
		t.Errorf("X-Product = %q want %q", got, "WorkBuddy")
	}
	// client_name 其他值也应跟随。
	c2 := &Client{ClientName: "MyEditor"}
	h2 := chatHeadersReq(t, c2, a)
	if got := h2.Get("X-Product"); got != "MyEditor" {
		t.Errorf("X-Product = %q want %q", got, "MyEditor")
	}
}

// TestIPNotForwarded_ByDefault PassthroughIP 缺省 false：不注入任何 IP 头。
func TestIPNotForwarded_ByDefault(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	// 即使 ClientIP 被外部误设，PassthroughIP 关闭也不透传。
	c := &Client{ClientIP: "10.0.0.1"}
	h := chatHeadersReq(t, c, a)
	for _, hdr := range []string{"X-Forwarded-For", "X-Real-IP", "X-Client-IP"} {
		if got := h.Get(hdr); got != "" {
			t.Errorf("%s = %q want empty (passthrough off)", hdr, got)
		}
	}
}

// TestIPForwarded_WhenEnabled PassthroughIP=true 且 ClientIP 非空时三头透传。
func TestIPForwarded_WhenEnabled(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{PassthroughIP: true, ClientIP: "203.0.113.5"}
	h := chatHeadersReq(t, c, a)
	for _, hdr := range []string{"X-Forwarded-For", "X-Real-IP", "X-Client-IP"} {
		if got := h.Get(hdr); got != "203.0.113.5" {
			t.Errorf("%s = %q want %q", hdr, got, "203.0.113.5")
		}
	}
}

// TestExtractClientIP 从入站请求取 X-Forwarded-For 首段，回落 X-Real-IP，皆空返回空。
func TestExtractClientIP(t *testing.T) {
	for _, tc := range []struct {
		name string
		xff  string
		real string
		want string
	}{
		{"xff_single", "1.2.3.4", "", "1.2.3.4"},
		{"xff_multi_first", "1.2.3.4, 5.6.7.8", "", "1.2.3.4"},
		{"real_fallback", "", "9.9.9.9", "9.9.9.9"},
		{"none_empty", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.real != "" {
				req.Header.Set("X-Real-IP", tc.real)
			}
			if got := ExtractClientIP(req); got != tc.want {
				t.Errorf("ExtractClientIP = %q want %q", got, tc.want)
			}
		})
	}
}
