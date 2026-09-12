package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/upstream"
)

func int64p(v int64) *int64 { return &v }

// TestCheckinEndpointReportsSummary POST /checkin 汇总各账号结果并透出明细。
func TestCheckinEndpointReportsSummary(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	called := 0
	h := NewHandler(Config{
		Pool:     p,
		Upstream: upstream.New(),
		APIKey:   "secret",
		Checkin: func() ([]scheduler.CheckinOutcome, error) {
			called++
			return []scheduler.CheckinOutcome{
				{UID: "u1", Nickname: "nick", Status: scheduler.CheckinOK, Credits: int64p(1000)},
				{UID: "u2", Status: scheduler.CheckinAlready},
				{UID: "u3", Status: scheduler.CheckinFail, Detail: "boom"},
				{UID: "u4", Status: scheduler.CheckinSkipped, Detail: "disabled"},
			}, nil
		},
	})

	// 无 token → 401（与 /status 同级，须走同一鉴权），且不得触发签到。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/checkin", nil))
	if rec.Code != 401 {
		t.Fatalf("no token: code=%d want 401", rec.Code)
	}
	if called != 0 {
		t.Fatal("checkin must not run without valid api key")
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/checkin", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if called != 1 {
		t.Fatalf("checkin calls=%d want 1", called)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("not json: %v", err)
	}
	want := map[string]float64{"total": 4, "ok": 1, "already": 1, "failed": 1, "skipped": 1}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s=%v want %v", k, got[k], v)
		}
	}
	accounts, ok := got["accounts"].([]any)
	if !ok || len(accounts) != 4 {
		t.Fatalf("accounts=%v", got["accounts"])
	}
	first, _ := accounts[0].(map[string]any)
	if first["uid"] != "u1" || first["status"] != "ok" || first["credits"] != float64(1000) {
		t.Errorf("first account=%v", first)
	}
	// 已签到账号不应被当成失败暴露给调用方。
	second, _ := accounts[1].(map[string]any)
	if second["status"] != "already" {
		t.Errorf("second account=%v", second)
	}
}

// TestCheckinEndpointBusyReturns409 已有签到在跑 → 409，提示可重试。
func TestCheckinEndpointBusyReturns409(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool:     p,
		Upstream: upstream.New(),
		Checkin:  func() ([]scheduler.CheckinOutcome, error) { return nil, scheduler.ErrBusy },
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/checkin", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "already running") {
		t.Errorf("body=%s", rec.Body.String())
	}
}

// TestCheckinEndpointUnavailable 未注入签到函数时明确 503，而不是 500 或空指针。
func TestCheckinEndpointUnavailable(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/checkin", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503", rec.Code)
	}
}

// TestCheckinEndpointRejectsGet 签到有副作用，只接受 POST（Go 1.22 路由按方法匹配）。
func TestCheckinEndpointRejectsGet(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool:     p,
		Upstream: upstream.New(),
		Checkin:  func() ([]scheduler.CheckinOutcome, error) { return nil, nil },
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/checkin", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405", rec.Code)
	}
}
