package scheduler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// newCheckinFixture 构建带 fake 上游的调度器：remain 为签到后余额；
// already=true 时 daily-checkin 返回业务错误（模拟上游"今天已签到"）。
func newCheckinFixture(t *testing.T, remain int64, already bool) (*Scheduler, *pool.Pool, *fakeUpstream) {
	t.Helper()
	f := &fakeUpstream{resourceRemain: remain, already: already}
	srv := f.server()
	t.Cleanup(srv.Close)

	p := pool.New("")
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	return New(Config{Pool: p, Upstream: up}), p, f
}

func TestCheckinAllReportsOutcomes(t *testing.T) {
	s, p, _ := newCheckinFixture(t, 500, false)
	p.Add(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("CheckinAll: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("outcomes=%d want 1", len(out))
	}
	oc := out[0]
	if oc.UID != "u1" || oc.Status != CheckinOK {
		t.Errorf("outcome=%+v want u1/ok", oc)
	}
	if oc.Credits == nil || *oc.Credits != 500 {
		t.Errorf("credits=%v want 500", oc.Credits)
	}
	if oc.Nickname != "nick" {
		t.Errorf("nickname=%q", oc.Nickname)
	}
}

func TestCheckinAllMarksAlreadyCheckin(t *testing.T) {
	s, p, _ := newCheckinFixture(t, 300, true)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("CheckinAll: %v", err)
	}
	if out[0].Status != CheckinAlready {
		t.Errorf("status=%s want already（上游 code!=0 今天已签到）", out[0].Status)
	}
	// 已签到也要照常查余额并解冻：账号余额恢复即应复活。
	if out[0].Credits == nil || *out[0].Credits != 300 {
		t.Errorf("credits=%v want 300", out[0].Credits)
	}
}

// TestCheckinAlreadyHasNoErrorDetail "今天已签到"是幂等成功，回执里不该出现
// 上游 400 报文——否则调用方会把正常状态读成失败。
func TestCheckinAlreadyHasNoErrorDetail(t *testing.T) {
	s, p, _ := newCheckinFixture(t, 300, true)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("CheckinAll: %v", err)
	}
	if out[0].Status != CheckinAlready {
		t.Fatalf("status=%s want already", out[0].Status)
	}
	if out[0].Detail != "" {
		t.Errorf("detail=%q want empty（已签到不是错误，不该带报错正文）", out[0].Detail)
	}
}

func TestCheckinAllSkipsDisabledAndCredentialLess(t *testing.T) {
	s, p, f := newCheckinFixture(t, 100, false)
	p.Add(&auth.Auth{UID: "off", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "norefresh", AccessToken: "at", ExpiresAt: 9999999999})
	p.Disable("off", "12153 session dead")

	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("CheckinAll: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("outcomes=%d want 2", len(out))
	}
	for _, oc := range out {
		if oc.Status != CheckinSkipped {
			t.Errorf("%s status=%s want skipped (%s)", oc.UID, oc.Status, oc.Detail)
		}
	}
	if f.checkinCalls.Load() != 0 {
		t.Errorf("checkin calls=%d want 0（禁用/无凭证都不该打上游）", f.checkinCalls.Load())
	}
}

func TestCheckinAllRefreshesExpiredToken(t *testing.T) {
	s, p, f := newCheckinFixture(t, 100, false)
	// ExpiresAt=1 → 早已过期，必须先刷新再签到，否则签到必然 401。
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	if _, err := s.CheckinAll(); err != nil {
		t.Fatalf("CheckinAll: %v", err)
	}
	if f.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d want 1", f.refreshCalls.Load())
	}
	if f.checkinCalls.Load() != 1 {
		t.Errorf("checkin calls=%d want 1", f.checkinCalls.Load())
	}
	if a.AccessToken != "new" {
		t.Errorf("token=%q want new", a.AccessToken)
	}
}

// TestCheckinAllStillChecksInWhenRefreshFailsButTokenAlive 刷新接口抖动但 access token
// 还没真正过期时，签到仍应照常执行，不能因"提前补票"失败而白白跳过当天签到。
func TestCheckinAllStillChecksInWhenRefreshFailsButTokenAlive(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 100, refreshFails: true}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	// 10 分钟内过期 → 触发 NeedsRefresh(10m)，但此刻 token 仍有效（尚未过期）。
	a := &auth.Auth{UID: "u1", AccessToken: "still-valid", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(5 * time.Minute).Unix()}
	p.Add(a)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("CheckinAll: %v", err)
	}
	if f.checkinCalls.Load() != 1 {
		t.Fatalf("checkin calls=%d want 1（token 仍有效就必须签到）", f.checkinCalls.Load())
	}
	if out[0].Status != CheckinOK {
		t.Errorf("status=%s want ok (%s)", out[0].Status, out[0].Detail)
	}
}

// TestCheckinAllFailsWhenRefreshFailsAndTokenExpired 真过期且刷新失败 → fail，
// 不做无谓的上游签到调用。
func TestCheckinAllFailsWhenRefreshFailsAndTokenExpired(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 100, refreshFails: true}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "expired", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("CheckinAll: %v", err)
	}
	if f.checkinCalls.Load() != 0 {
		t.Errorf("checkin calls=%d want 0（token 已过期，签到必然 401）", f.checkinCalls.Load())
	}
	if out[0].Status != CheckinFail {
		t.Errorf("status=%s want fail", out[0].Status)
	}
}

// TestCheckinAllBusyRejectsConcurrent 并发签到必须被拒（ErrBusy），
// 避免手动接口与定时/启动补签同时重放上游。
func TestCheckinAllBusyRejectsConcurrent(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 所有请求都阻塞在 release 上，保证首个签到在后续调用期间仍持锁。
		enterOnce.Do(func() { close(entered) })
		<-release
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":10,"CycleCapacityUsed":0}]}}}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	done := make(chan error, 1)
	go func() {
		_, err := s.CheckinAll()
		done <- err
	}()
	waitEntered(t, entered)

	if _, err := s.CheckinAll(); !errors.Is(err, ErrBusy) {
		t.Errorf("err=%v want ErrBusy", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Errorf("first CheckinAll: %v", err)
	}

	// 首个签到结束后互斥已归还：应能再次签到（release 已关闭，请求直接放行）。
	if _, err := s.CheckinAll(); err != nil {
		t.Errorf("after release CheckinAll: %v", err)
	}
}

// TestRunCheckinNowSwallowsConcurrent 定时入口与手动签到撞车只记日志，不 panic、不阻塞。
func TestRunCheckinNowSwallowsConcurrent(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enterOnce.Do(func() { close(entered) })
		<-release
		w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	done := make(chan error, 1)
	go func() {
		_, err := s.CheckinAll()
		done <- err
	}()
	waitEntered(t, entered)

	s.RunCheckinNow() // 撞车：必须吞掉 ErrBusy 并立即返回
	close(release)
	if err := <-done; err != nil {
		t.Errorf("CheckinAll: %v", err)
	}
}

// waitEntered 等待首次上游请求抵达，超时即失败（避免 fake 行为变化时测试静默挂死）。
func waitEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("checkin never reached upstream")
	}
}
