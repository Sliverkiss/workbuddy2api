package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{402, ``, ErrHardCredit},
		{400, `{"code":1,"msg":"余额不足"}`, ErrHardCredit},
		{403, `insufficient credits`, ErrHardCredit},
		{200, `{"code":10001,"msg":"积分不足，请充值"}`, ErrHardCredit},
		{400, `{"code":1,"msg":"额度用尽"}`, ErrHardCredit},
		{429, ``, ErrSoftRate},
		// 限流文案（issue #28）：状态码不是 429 时也必须识别为软限流，
		// 否则账号不会被冷却，下次请求仍会被选中。
		{200, `{"code":11140,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`, ErrSoftRate},
		{400, `rate limit`, ErrSoftRate},
		{403, `usage limit reached`, ErrSoftRate},
		// "model usage limit exceeded" 不是余额语义（无 credit/quota/积分/额度 等计费词），
		// 属于模型侧用量节流 → 短冷却（误判为硬冷却会把有余量的号停到次日 04:00）。
		{200, `{"code":1,"msg":"model usage limit exceeded"}`, ErrSoftRate},
		{200, `{"code":1,"msg":"too many requests"}`, ErrSoftRate},
		{500, `rate-limited upstream`, ErrSoftRate}, // 限流文案优先于 5xx 分类
		// 反向锚定：不得回归。
		{400, `Illegal API invocation from an unapproved channel`, ErrClient},
		{200, `quota exceeded`, ErrHardCredit},
		// session 死亡优先于限流文案（401+12153 需人工重登，短冷却无意义）。
		{401, `{"code":12153,"msg":"Offline user session not found, rate limit"}`, ErrSessionDead},
		{401, `Offline user session not found`, ErrSessionDead},
		{401, `{"code":12153,"msg":"Offline user session not found"}`, ErrSessionDead},
		{401, `{"code":9999,"msg":"bad token"}`, ErrClient},
		{500, `boom`, ErrServer},
		{503, `unavailable`, ErrServer},
		{200, ``, ErrNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testClient(fn rtFunc) *Client {
	return &Client{
		HTTP:          &http.Client{Transport: fn},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
	}
}

func TestRefreshSuccess(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("X-Refresh-Token") != "oldrt" {
			return nil, errors.New("missing X-Refresh-Token")
		}
		return jsonResp(200, `{"code":0,"msg":"ok","data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":3600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
	if a.ExpiresAt <= 1 {
		t.Errorf("expiresAt not advanced: %d", a.ExpiresAt)
	}
}

func TestRefreshPreservesExpiryWhenOmitted(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"accessToken":"newat"}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.ExpiresAt != 1753600000 {
		t.Errorf("expiresAt should be preserved, got %d", a.ExpiresAt)
	}
	if a.RefreshToken != "rt" {
		t.Errorf("refreshToken should be preserved, got %s", a.RefreshToken)
	}
}

func TestRefreshSessionDead(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 401,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":12153,"msg":"Offline user session not found"}`)),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	err := c.RefreshToken(a)
	if err == nil {
		t.Fatal("want error")
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}
	if ue.Kind != ErrSessionDead {
		t.Errorf("kind=%v want ErrSessionDead", ue.Kind)
	}
}

func TestChatStreamSendsHeadersAndStreamTrue(t *testing.T) {
	var gotAuth, gotUID, gotProduct string
	var gotBody []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-User-Id")
		gotProduct = r.Header.Get("X-Product")
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", EnterpriseID: "e1"}
	rc, status, respBody, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if respBody != nil {
		t.Errorf("200 response should carry nil body, got %q", respBody)
	}
	rc.Close()
	if gotAuth != "Bearer at" || gotUID != "u1" || gotProduct != "SaaS" {
		t.Errorf("headers: auth=%q uid=%q product=%q", gotAuth, gotUID, gotProduct)
	}
	if !bytes.Contains(gotBody, []byte(`"stream":true`)) {
		t.Errorf("stream not forced: %s", gotBody)
	}
}

func TestFetchModelsEffortsDriveBodyDowngrade(t *testing.T) {
	var outbound []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":131072,"maxOutputTokens":8192,"reasoning":{"effort":"high","supportedEfforts":["low","high"]}}
			],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`), nil
		default:
			outbound, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		}
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	infos, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("infos=%+v", infos)
	}
	// ModelInfo.Efforts 应携带 supportedEfforts
	if len(infos[0].Efforts) != 2 || infos[0].Efforts[0] != "low" {
		t.Errorf("infos[0].Efforts=%v", infos[0].Efforts)
	}

	// glm-5.2 只支持 low/high，请求 max → 降级为 high
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","reasoning_effort":"max","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	var m map[string]any
	if err := json.Unmarshal(outbound, &m); err != nil {
		t.Fatalf("outbound unmarshal: %v (%s)", err, outbound)
	}
	if got, _ := m["reasoning_effort"].(string); got != "high" {
		t.Errorf("reasoning_effort=%v want high (outbound=%s)", m["reasoning_effort"], outbound)
	}
}

func TestChatStreamHardCreditError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(402, `{"code":1,"msg":"余额不足"}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	_, status, respBody, err := c.ChatStream(a, []byte(`{}`))
	if status != 402 {
		t.Errorf("status=%d", status)
	}
	if err != nil {
		t.Fatalf("hard credit should return body via status, not err: %v", err)
	}
	// caller classifies via returned body
	if Classify(status, string(respBody)) != ErrHardCredit {
		t.Errorf("body=%q not classified hard credit", respBody)
	}
}

// TestChatStreamReadsMultipleChunksOverRealTransport 走真实 net/http 传输层，
// 回归 defer cancel() 导致第二块起 body Read 返回 context canceled 的断流 bug。
func TestChatStreamReadsMultipleChunksOverRealTransport(t *testing.T) {
	const frames = 6
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("http.ResponseWriter does not implement http.Flusher")
			return
		}
		for i := 1; i <= frames; i++ {
			if _, err := fmt.Fprintf(w, "data: chunk-%d\n\n", i); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	c.IdleTimeout = 5 * time.Second

	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	defer rc.Close()

	buf := make([]byte, 1)
	var got string
	for i := 0; i < frames; i++ {
		if _, err := io.ReadFull(rc, buf); err != nil {
			t.Fatalf("read %d: %v (real transport body must not be cut)", i, err)
		}
		got += string(buf)
	}
	if strings.Contains(got, "context canceled") {
		t.Fatalf("body read hit context canceled, got %q", got)
	}
}

func TestUserResourceAggregation(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Method != http.MethodPost {
			return nil, errors.New("want POST")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"ProductCode":"p_tcaca"`)) {
			return nil, errors.New("missing ProductCode: " + string(body))
		}
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"TotalCount":2,"TotalDosage":3000,"Accounts":[
			{"PackageName":"签到包","CapacitySize":2000,"CapacityRemain":1200,"CapacityUsed":800,"CycleCapacitySize":2000,"CycleCapacityRemain":1200,"CycleCapacityUsed":800},
			{"PackageName":"体验包","CapacitySize":1000,"CapacityRemain":300,"CapacityUsed":700,"CycleCapacitySize":1000,"CycleCapacityRemain":300,"CycleCapacityUsed":700}
		]}}}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	remain, err := c.UserResource(a)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	if remain != 1500 {
		t.Errorf("remain=%d want 1500", remain)
	}
}

func TestUserResourceNegativeClamped(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":-50,"CycleCapacityUsed":150}
		]}}}}`), nil
	})
	remain, err := c.UserResource(&auth.Auth{AccessToken: "at"})
	if err != nil || remain != 0 {
		t.Errorf("remain=%d err=%v, want 0 (clamped)", remain, err)
	}
}

func TestDailyCheckinAlready(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/daily-checkin") {
			return nil, errors.New("wrong path")
		}
		return jsonResp(200, `{"code":14001,"msg":"今日已签到"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil || !strings.Contains(err.Error(), "已签到") {
		t.Errorf("err=%v", err)
	}
}

func TestBasesAlwaysCN(t *testing.T) {
	c := testClient(nil)
	cn := &auth.Auth{Domain: ""}
	other := &auth.Auth{Domain: "example.com"}
	if c.chatBase(cn) != "https://chat.example" || c.billingBase(cn) != "https://billing.example" {
		t.Error("cn bases wrong")
	}
	// 恒 CN：domain 不同不改变上游 host。
	if c.chatBase(other) != c.chatBase(cn) || c.billingBase(other) != c.billingBase(cn) {
		t.Error("bases must be CN regardless of domain")
	}
}

func TestNewChatClientNoTotalTimeoutAndSharedTransport(t *testing.T) {
	c := New()
	if c.ChatHTTP == nil {
		t.Fatal("ChatHTTP should be initialized")
	}
	if c.ChatHTTP.Timeout != 0 {
		t.Errorf("ChatHTTP.Timeout=%v want 0 (no total cap)", c.ChatHTTP.Timeout)
	}
	// 共享同一个 Transport 实例，连接池不重复。
	if c.ChatHTTP.Transport != c.HTTP.Transport {
		t.Errorf("ChatHTTP and HTTP must share the same *http.Transport")
	}
	htr, ok := c.ChatHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type=%T", c.ChatHTTP.Transport)
	}
	if htr.ResponseHeaderTimeout != 120*time.Second {
		t.Errorf("ResponseHeaderTimeout=%v want 120s", htr.ResponseHeaderTimeout)
	}
}

func TestChatStreamRoutesToChatHTTP(t *testing.T) {
	// 显式注入 ChatHTTP（可辨识标记），验证 ChatStream 走它而非 HTTP。
	chatHit, httpHit := false, false
	c := testClient(func(*http.Request) (*http.Response, error) {
		httpHit = true
		return jsonResp(200, `{}`), nil
	})
	c.ChatHTTP = &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		chatHit = true
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, status, _, err := c.ChatStream(a, []byte(`{}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	if !chatHit {
		t.Error("ChatStream should use ChatHTTP")
	}
	if httpHit {
		t.Error("ChatStream must not use HTTP")
	}
}

func TestChatHTTPNilFallsBackToHTTP(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	if c.chatHTTP() != c.HTTP {
		t.Error("chatHTTP() should fall back to HTTP when ChatHTTP is nil")
	}
}
