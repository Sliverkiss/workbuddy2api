package upstream

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// uaCaptureTransport 记录出站请求的 User-Agent。
type uaCaptureTransport struct {
	ua *string
}

func (t uaCaptureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	*t.ua = r.Header.Get("User-Agent")
	return jsonResp(200, `{"code":0}`), nil
}

// TestUserAgentDefaultEmptyKeepsClientUA 默认（UserAgent 空）行为与现状完全一致：
// chat/refresh 路径 UA=clientUA；billing 路径（report/travel/balance）不设 UA 头
// （保持现状，Go 客户端自带默认 UA）。
func TestUserAgentDefaultEmptyKeepsClientUA(t *testing.T) {
	for _, tc := range []struct {
		name   string
		call   func(c *Client) error
		wantUA string
	}{
		{
			name: "chat",
			call: func(c *Client) error {
				rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"glm-5.2","messages":[]}`))
				if status != 200 {
					t.Fatalf("chat status=%d", status)
				}
				if rc != nil {
					rc.Close()
				}
				return err
			},
			wantUA: clientUA,
		},
		{
			name: "billing_report",
			call: func(c *Client) error {
				return c.ReportChatActivity(&auth.Auth{AccessToken: "at", UID: "u1"}, "cid")
			},
			wantUA: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ua string
			c := &Client{
				HTTP:          &http.Client{Transport: uaCaptureTransport{ua: &ua}},
				ChatHTTP:      &http.Client{Transport: uaCaptureTransport{ua: &ua}},
				ChatBaseCN:    "https://chat.example",
				BillingBaseCN: "https://billing.example",
			}
			if err := tc.call(c); err != nil {
				t.Fatalf("call: %v", err)
			}
			if ua != tc.wantUA {
				t.Errorf("UA = %q want %q", ua, tc.wantUA)
			}
		})
	}
}

// TestUserAgentOverrideAllOutbound 显式设置后 chat/billing/refresh 全路径覆盖。
// 用 env 别名直接验证 fields 传输到 headers 的行为。
func TestUserAgentOverrideAllOutbound(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1", RefreshToken: "rt"}
	ua := "WorkBuddy/9.9.9"
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("User-Agent"); got != ua {
				t.Errorf("UA = %q want %q (path=%s)", got, ua, r.URL.Path)
			}
			return jsonResp(200, `{"code":0}`), nil
		})},
		ChatHTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("User-Agent"); got != ua {
				t.Errorf("Chat UA = %q want %q", got, ua)
			}
			return jsonResp(200, `{"code":0}`), nil
		})},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		UserAgent:     ua,
	}
	// chat
	if rc, status, _, err := c.ChatStream(a, []byte(`{"model":"deepseek-v4-flash","messages":[]}`)); status != 200 || err != nil {
		t.Errorf("chat: status=%d err=%v", status, err)
	} else if rc != nil {
		rc.Close()
	}
	// refresh（RefreshHeaders→CommonHeaders）
	c.HTTP.Transport = rtFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("User-Agent"); got != ua {
			t.Errorf("Refresh UA = %q want %q", got, ua)
		}
		return jsonResp(200, `{"code":0,"data":{"accessToken":"nat","refreshToken":"nrt"}}`), nil
	})
	if err := c.RefreshToken(a); err != nil {
		t.Errorf("refresh: %v", err)
	}
}

// TestUserAgentOverrideBilling 余额/签到类 billing 请求同样覆盖。
func TestUserAgentOverrideBilling(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("User-Agent"); got != "CustomAgent/1" {
				t.Errorf("Billing UA = %q want CustomAgent/1", got)
			}
			return jsonResp(200, `{"code":0,"data":{"response":{"data":{"accounts":[{"PackageName":"x","CycleCapacitySize":100,"CycleCapacityUsed":0}]}}}}`), nil
		})},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		UserAgent:     "CustomAgent/1",
	}
	if _, err := c.UserResource(a); err != nil {
		t.Errorf("userResource: %v", err)
	}
}

// TestFetchModelsUsesConfiguredUA FetchModels 手工 Set UA 也走覆盖。
func TestFetchModelsUsesConfiguredUA(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if !strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models") {
				t.Errorf("path=%s", r.URL.Path)
			}
			if got := r.Header.Get("User-Agent"); got != "FetchAgent/2" {
				t.Errorf("FetchModels UA = %q want FetchAgent/2", got)
			}
			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM","maxInputTokens":131072,"maxOutputTokens":8192,"reasoning":{"effort":"high","supportedEfforts":[]},"disabled":false}],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`), nil
		})},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		UserAgent:     "FetchAgent/2",
	}
	if _, err := c.FetchModels(a); err != nil {
		t.Errorf("fetchModels: %v", err)
	}
}

var _ = io.Discard
