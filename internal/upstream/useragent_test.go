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

// TestUserAgentDefaultEmptyKeepsClientUA 默认（UserAgent/client_name 空）行为：
// chat/refresh 路径 UA=默认 WorkBuddy 三段式；billing 路径（report/travel/balance）
// 不设 UA 头（保持现状，Go 客户端自带默认 UA）。
func TestUserAgentDefaultEmptyKeepsClientUA(t *testing.T) {
	for _, tc := range []struct {
		name   string
		call   func(c *Client) error
		wantUA string
	}{
		{
			name: "chat",
			call: func(c *Client) error {
				rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"glm-5.2","messages":[]}`), "")
				if status != 200 {
					t.Fatalf("chat status=%d", status)
				}
				if rc != nil {
					rc.Close()
				}
				return err
			},
			wantUA: defaultUAString,
		},
		{
			name: "billing_report",
			call: func(c *Client) error {
				return c.ReportChatActivity(&auth.Auth{AccessToken: "at", UID: "u1"}, "cid", "")
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
	if rc, status, _, err := c.ChatStream(a, []byte(`{"model":"deepseek-v4-flash","messages":[]}`), ""); status != 200 || err != nil {
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

// --- A 段：UA 对齐官方 WorkBuddy 三段式 ---

const (
	defaultUAString      = "WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1"
	explicitString       = "MyCustomAgent/3.1"
	clientVerUAString    = "WorkBuddy/6.0.0 WorkBuddy/6.0.0 CLI/2.137.1"
	billingUAWorkBuddy   = "WorkBuddy/5.5.4"
	billingUACustomVer   = "WorkBuddy/6.0.0"
	billingUAAgentString = "BillingAgent/1"
)

// TestUserAgentDefaultWorkBuddyShape 默认（无任何配置）聊天/刷新出站 UA =
// 官方 WorkBuddy 三段式，旧值 `CLI/2.63.2 CodeBuddy/2.63.2` 已被对齐替换。
func TestUserAgentDefaultWorkBuddyShape(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{
		HTTP:          &http.Client{Transport: uaCaptureTransport{ua: new(string)}},
		ChatHTTP:      &http.Client{Transport: uaCaptureTransport{ua: new(string)}},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
	}
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"deepseek-v4-flash","messages":[]}`), "")
	if status != 200 || err != nil {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if ua := c.chatLastUA(); ua != defaultUAString {
		t.Errorf("chat UA = %q want %q", ua, defaultUAString)
	}
	if rc != nil {
		rc.Close()
	}
}

// chatLastUA 从最近一次聊天请求捕获 UA（当前测试 Client 的 ChatHTTP transport 记录）。
func (c *Client) chatLastUA() string {
	if t, ok := c.ChatHTTP.Transport.(uaCaptureTransport); ok && t.ua != nil {
		return *t.ua
	}
	return ""
}

// TestUserAgentExplicitOverride config user_agent 非空时以用户显式值为准（兼容旧覆盖逻辑）。
func TestUserAgentExplicitOverride(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1", RefreshToken: "rt"}
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("User-Agent"); got != explicitString {
				t.Errorf("UA = %q want %q (path=%s)", got, explicitString, r.URL.Path)
			}
			return jsonResp(200, `{"code":0,"data":{"accessToken":"nat","refreshToken":"nrt"}}`), nil
		})},
		ChatHTTP:      &http.Client{Transport: uaCaptureTransport{ua: new(string)}},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		UserAgent:     explicitString,
	}
	if rc, status, _, err := c.ChatStream(a, []byte(`{"model":"deepseek-v4-flash","messages":[]}`), ""); status != 200 || err != nil {
		t.Errorf("chat: status=%d err=%v", status, err)
	} else if rc != nil {
		rc.Close()
	}
	if got := c.chatLastUA(); got != explicitString {
		t.Errorf("chat UA = %q want %q", got, explicitString)
	}
	if err := c.RefreshToken(a); err != nil {
		t.Errorf("refresh: %v", err)
	}
}

// TestUserAgentClientVersionOverride config client_version 生效：UA 的 WorkBuddy 段跟随
// 且成对相同（platform 段 = applicationName 段），CLI 段保持默认。
func TestUserAgentClientVersionOverride(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{
		HTTP:          &http.Client{Transport: uaCaptureTransport{ua: new(string)}},
		ChatHTTP:      &http.Client{Transport: uaCaptureTransport{ua: new(string)}},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		ClientVersion: "6.0.0",
	}
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"deepseek-v4-flash","messages":[]}`), "")
	if status != 200 || err != nil {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if got := c.chatLastUA(); got != clientVerUAString {
		t.Errorf("UA = %q want %q", got, clientVerUAString)
	}
	if rc != nil {
		rc.Close()
	}
}

// TestBillingUA_WhenClientNameSet billing/checkin 路径：client_name 非空时用单段
// `WorkBuddy/<clientVersion>`（不带 CLI 段，对齐官方 banner/check-in 显式头组）。
func TestBillingUA_WhenClientNameSet(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	// 默认 client_version → WorkBuddy/5.5.4
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("User-Agent"); got != billingUAWorkBuddy {
				t.Errorf("billing UA = %q want %q (path=%s)", got, billingUAWorkBuddy, r.URL.Path)
			}
			return jsonResp(200, `{"code":0,"data":{"response":{"data":{"accounts":[{"PackageName":"x","CycleCapacitySize":100,"CycleCapacityUsed":0}]}}}}`), nil
		})},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		ClientName:    "WorkBuddy",
	}
	if _, err := c.UserResource(a); err != nil {
		t.Errorf("userResource: %v", err)
	}
	// 自定义 client_version → WorkBuddy/6.0.0
	c2 := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("User-Agent"); got != billingUACustomVer {
				t.Errorf("billing UA = %q want %q", got, billingUACustomVer)
			}
			return jsonResp(200, `{"code":0,"data":{"response":{"data":{"accounts":[{"PackageName":"x","CycleCapacitySize":100,"CycleCapacityUsed":0}]}}}}`), nil
		})},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		ClientName:    "WorkBuddy",
		ClientVersion: "6.0.0",
	}
	if _, err := c2.UserResource(a); err != nil {
		t.Errorf("userResource v2: %v", err)
	}
	// 显式 user_agent 仍优先于 billingUA
	c3 := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("User-Agent"); got != billingUAAgentString {
				t.Errorf("billing UA = %q want %q", got, billingUAAgentString)
			}
			return jsonResp(200, `{"code":0,"data":{"response":{"data":{"accounts":[{"PackageName":"x","CycleCapacitySize":100,"CycleCapacityUsed":0}]}}}}`), nil
		})},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		ClientName:    "WorkBuddy",
		UserAgent:     billingUAAgentString,
	}
	if _, err := c3.UserResource(a); err != nil {
		t.Errorf("userResource v3: %v", err)
	}
}

// TestBillingUA_WhenClientNameEmpty client_name 空 = 保持现状：不设 UA（Go 默认 UA），
// 即使 version 配置了也不上 UA。
func TestBillingUA_WhenClientNameEmpty(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	var ua string
	const fullResp = `{"code":0,"data":{"response":{"data":{"accounts":[{"PackageName":"x","CycleCapacitySize":100,"CycleCapacityUsed":0}]}}}}`
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			ua = r.Header.Get("User-Agent")
			return jsonResp(200, fullResp), nil
		})},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		ClientVersion: "6.0.0", // 配置了版本但 client_name 空 → 仍不设 UA
	}
	if _, err := c.UserResource(a); err != nil {
		t.Errorf("userResource: %v", err)
	}
	if ua != "" {
		t.Errorf("billing UA = %q want empty (client_name unset)", ua)
	}
	if got := c.billingUA(); got != "" {
		t.Errorf("billingUA() = %q want empty", got)
	}
}

var _ = io.Discard
