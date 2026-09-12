package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func TestNextFire(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, loc)
	next := nextFire(now, []int{9, 21})
	if next.Hour() != 21 || next.Day() != 27 {
		t.Errorf("next=%v want 21:00 same day", next)
	}
	now = time.Date(2026, 7, 27, 22, 0, 0, 0, loc)
	next = nextFire(now, []int{9, 21})
	if next.Hour() != 9 || next.Day() != 28 {
		t.Errorf("next=%v want 09:00 next day", next)
	}
	now = time.Date(2026, 7, 27, 9, 0, 0, 0, loc)
	next = nextFire(now, []int{9})
	if next.Day() != 28 {
		t.Errorf("exact match should roll to next day: %v", next)
	}
}

func TestNextFireMergesSchedules(t *testing.T) {
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.Local)
	next := nextFire(now, []int{9, 21, 22})
	if next.Hour() != 21 {
		t.Errorf("next=%v want 21 (earliest of 21/22)", next)
	}
}

// TestNextWakeKeepaliveOnly 签到已过点时按保活整点唤醒。
func TestNextWakeKeepaliveOnly(t *testing.T) {
	s := New(Config{CheckinHours: []int{9}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskKeepalive {
		t.Errorf("kinds=%v want [keepalive]", kinds)
	}
}

// TestNextWakeSameInstantFiresAll 签到与保活配到同一整点时两类任务都要执行。
func TestNextWakeSameInstantFiresAll(t *testing.T) {
	s := New(Config{
		CheckinHours:     []int{9, 22},
		TravelHours:      []int{}, // 禁用旅行时点干扰（仅测签到+保活同整点）
		ActivityHours:    []int{}, // 禁用活跃时点干扰
		KeepaliveHours:   []int{22},
		TravelDisabled:   true,
		ActivityDisabled: true,
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 21, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if !hasKind(kinds, taskCheckin) || !hasKind(kinds, taskKeepalive) {
		t.Errorf("kinds=%v want checkin+keepalive（同一时刻两任务）", kinds)
	}

	// 22 点过后下一次是次日 09:00，且只含签到（旅行/活跃已禁用）。
	at, kinds = s.nextWake(time.Date(2026, 9, 11, 22, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 12, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestNextWakeNothingScheduled 两类任务全空时返回零值，Run 只等退出信号。
func TestNextWakeNothingScheduled(t *testing.T) {
	s := &Scheduler{cfg: Config{}}
	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("at=%v kinds=%v want zero/nil", at, kinds)
	}
}

// TestNextWakeCheckinDisabled 显式禁用签到后，排程里不再有签到时点（保活照常）。
func TestNextWakeCheckinDisabled(t *testing.T) {
	s := New(Config{CheckinDisabled: true, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（不应再有 21 点签到）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskKeepalive {
		t.Errorf("kinds=%v want [keepalive]", kinds)
	}
}

// TestNextWakeKeepaliveDisabled 显式禁用保活后，排程里不再有保活时点（签到照常）。
func TestNextWakeKeepaliveDisabled(t *testing.T) {
	s := New(Config{KeepaliveDisabled: true, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（不应再有 22 点保活）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestNextWakeBothDisabledNothingScheduled 四类任务都显式禁用 → 无可唤醒时点。
func TestNextWakeBothDisabledNothingScheduled(t *testing.T) {
	s := New(Config{
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		CheckinHours:      []int{9, 21},
		KeepaliveHours:    []int{22},
	})
	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("at=%v kinds=%v want zero/nil", at, kinds)
	}
}

// TestRunAllDisabledNoSpinNoCalls 四类任务全禁用：Run 不空转（只等退出信号），
// 且不能触发任何上游请求。
func TestRunAllDisabledNoSpinNoCalls(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no upstream call expected", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{
		Pool:              p,
		Upstream:          up,
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.Run(ctx) // 阻塞到 ctx 取消为止（无时点可等，不构造 timer）
	elapsed := time.Since(start)

	if calls.Load() != 0 {
		t.Errorf("upstream calls=%d want 0（四类全禁用）", calls.Load())
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("Run returned after %v, before ctx done（不应提前返回）", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run took %v（不应空转/忙等）", elapsed)
	}
}

func hasKind(kinds []taskKind, k taskKind) bool {
	for _, v := range kinds {
		if v == k {
			return true
		}
	}
	return false
}

// fakeUpstream 同时模拟 billing 与 refresh。
type fakeUpstream struct {
	checkinCalls   atomic.Int32
	refreshCalls   atomic.Int32
	resourceRemain int64
}

func (f *fakeUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			f.checkinCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":` +
				jsonI64(f.resourceRemain) + `,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			f.refreshCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
}

func jsonI64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestRunCheckinReenablesCoolingAccount(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   []int{9, 21},
		KeepaliveHours: []int{22},
	})
	s.RunCheckinNow()
	if f.checkinCalls.Load() != 1 {
		t.Errorf("checkin calls=%d", f.checkinCalls.Load())
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("account should be reenabled after checkin with credits: %+v", st)
	}
	if st.Credits != 500 {
		t.Errorf("credits=%d want 500", st.Credits)
	}
}

func TestRunKeepaliveRefreshesTokens(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d", f.refreshCalls.Load())
	}
	if a.AccessToken != "new" {
		t.Errorf("token not updated: %s", a.AccessToken)
	}
}

func TestRunKeepaliveSessionDeadDisables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	// P0-1：12153 连续 N 次才禁用。前 2 次刷新失败不应杀号（误判防护）。
	s.RunKeepaliveNow()
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("第 1 次 12153 不应禁用: %+v", st)
	}
	s.RunKeepaliveNow()
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("第 2 次 12153 不应禁用: %+v", st)
	}
	// 第 3 次连续 12153 → 禁用。
	s.RunKeepaliveNow()
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("第 3 次连续 12153 应禁用: %+v", st)
	}
	if st.DisabledReason != "12153 session dead" {
		t.Errorf("disabled_reason=%q want 12153 session dead", st.DisabledReason)
	}
}

// TestRunKeepaliveSessionDeadResetBySuccess 两次 12153 后刷新成功 → 计数清零，
// 再来的 12153 从第 1 次重新计（不会因历史失败被继续追杀）。
func TestRunKeepaliveSessionDeadResetBySuccess(t *testing.T) {
	var fails atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fails.Add(1) == 3 { // 第 3 次（本次调度循环的第二轮）刷新成功
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
			return
		}
		w.WriteHeader(401)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow() // 12153 #1
	s.RunKeepaliveNow() // 12153 #2
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("precondition: 前 2 次不应禁用: %+v", st)
	}
	s.RunKeepaliveNow() // 刷新成功 → 清计数
	// 接下来连续 2 次 12153：从新计数重新算，仍不应禁用（历史计数已清）。
	s.RunKeepaliveNow() // 12153 #1（新计数）
	s.RunKeepaliveNow() // 12153 #2（新计数）
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("刷新成功清计数后连续 2 次 12153 不应禁用: %+v", st)
	}
	s.RunKeepaliveNow() // 12153 #3（新计数）→ 禁用
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatalf("新计数第 3 次 12153 应禁用: %+v", st)
	}
}

func TestCheckinErrorDoesNotCrash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	// 不应 panic
	s.RunCheckinNow()
	s.RunKeepaliveNow()
	_ = errors.New("unused")
}

// checkinStub 配置化的签到上游：可控制签到响应（ok/already/fail）、刷新是否失败、
// 余额返回值。各分支命中后 atomic 计数，便于并发安全断言。
type checkinStub struct {
	checkinBody    string // /daily-checkin 返回的完整 body（含 code/msg）
	checkinStatus  int    // /daily-checkin HTTP 状态码（0=200）
	refreshFail    bool   // /token/refresh 是否失败（返回 12153 session dead）
	refreshCalls   atomic.Int32
	resourceRemain int64
}

func (s *checkinStub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			if s.checkinStatus != 0 {
				w.WriteHeader(s.checkinStatus)
			}
			w.Write([]byte(s.checkinBody))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":` +
				jsonI64(s.resourceRemain) + `,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			s.refreshCalls.Add(1)
			if s.refreshFail {
				w.WriteHeader(401)
				w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
}

// newCheckinS 构造 Pool+Upstream+Scheduler，账号 token 未过期（不触发预刷新）。
func newCheckinS(t *testing.T, stub *checkinStub) (*Scheduler, *pool.Pool) {
	t.Helper()
	srv := stub.server()
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up}), p
}

// TestCheckinAllOK 签到成功 → ok、余额回填、credits 指针有值。
func TestCheckinAllOK(t *testing.T) {
	s, p := newCheckinS(t, &checkinStub{
		checkinBody:    `{"code":0,"msg":"ok","data":{}}`,
		resourceRemain: 500,
	})
	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(out) != 1 || out[0].Status != CheckinOK {
		t.Fatalf("out=%+v want ok", out)
	}
	if out[0].Credits == nil || *out[0].Credits != 500 {
		t.Errorf("credits=%v want 500", out[0].Credits)
	}
	if st, _ := p.Status("u1"); st.Credits != 500 {
		t.Errorf("pool credits=%d want 500", st.Credits)
	}
}

// TestCheckinAllAlready "今天已签到"记 already、不回填 400 报文到 detail。
func TestCheckinAllAlready(t *testing.T) {
	s, _ := newCheckinS(t, &checkinStub{
		checkinBody:    `{"code":14001,"msg":"今天已签到"}`,
		resourceRemain: 300,
	})
	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out[0].Status != CheckinAlready {
		t.Errorf("status=%q want already", out[0].Status)
	}
	if out[0].Detail != "" {
		t.Errorf("已签到不应回填 detail，got=%q", out[0].Detail)
	}
}

// TestCheckinAllFail 签到上游 500 → fail、detail 填报错。
func TestCheckinAllFail(t *testing.T) {
	s, _ := newCheckinS(t, &checkinStub{
		checkinBody:    `boom`,
		checkinStatus:  500,
		resourceRemain: 300,
	})
	out, _ := s.CheckinAll()
	if out[0].Status != CheckinFail {
		t.Errorf("status=%q want fail", out[0].Status)
	}
	if out[0].Detail == "" {
		t.Error("fail 应填 detail")
	}
}

// TestCheckinAllSkipsDisabled 禁用账号记 skipped，不参与签到。
func TestCheckinAllSkipsDisabled(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 100}
	srv := stub.server()
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "dis", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "ok", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Disable("dis", "test")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	out, _ := s.CheckinAll()
	m := map[string]CheckinStatus{}
	for _, o := range out {
		m[o.UID] = o.Status
	}
	if m["dis"] != CheckinSkipped {
		t.Errorf("dis=%q want skipped", m["dis"])
	}
	if m["ok"] != CheckinOK {
		t.Errorf("ok=%q want ok", m["ok"])
	}
}

// TestCheckinAllSkipsNoCredentials 无 refreshToken 的账号记 skipped(no credentials)。
func TestCheckinAllSkipsNoCredentials(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 100}
	srv := stub.server()
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "notoken", AccessToken: "", RefreshToken: "", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	out, _ := s.CheckinAll()
	if out[0].Status != CheckinSkipped {
		t.Errorf("status=%q want skipped", out[0].Status)
	}
	if out[0].Detail != "no credentials" {
		t.Errorf("detail=%q want no credentials", out[0].Detail)
	}
}

// TestCheckinAllRefreshBeforeExpiry token 临近过期 → 签到前先刷新，刷新成功后继续签到。
func TestCheckinAllRefreshBeforeExpiry(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 100}
	srv := stub.server()
	defer srv.Close()
	p := pool.New("")
	// ExpiresAt 5 分钟后过期，落在 checkinRefreshSkew(10min) 窗口内 → 触发预刷新。
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(5 * time.Minute).Unix()}
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	out, _ := s.CheckinAll()
	if stub.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d want 1（过期窗口应预刷新）", stub.refreshCalls.Load())
	}
	if out[0].Status != CheckinOK {
		t.Errorf("status=%q want ok（刷新成功后继续签到）", out[0].Status)
	}
	if a.AccessToken != "new" {
		t.Errorf("token 未刷新: %s", a.AccessToken)
	}
}

// TestCheckinAllRefreshFlakyContinues 刷新抖动失败但 token 未真过期 → 继续签到（不阻断）。
func TestCheckinAllRefreshFlakyContinues(t *testing.T) {
	stub := &checkinStub{
		checkinBody:    `{"code":0,"msg":"ok","data":{}}`,
		refreshFail:    true,
		resourceRemain: 100,
	}
	srv := stub.server()
	defer srv.Close()
	p := pool.New("")
	// token 5 分钟后过期（在窗口内 → 尝试刷新），但 NeedsRefresh(0) 仍 false（未真过期）。
	// 刷新返回 12153 但不是真 session dead 的终态——token 仍有效，继续签到。
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(5 * time.Minute).Unix()}
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	out, _ := s.CheckinAll()
	if out[0].Status != CheckinOK {
		t.Errorf("status=%q want ok（刷新抖动不应阻断签到）", out[0].Status)
	}
	if stub.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d want 1", stub.refreshCalls.Load())
	}
}

// TestCheckinAllRefreshTrulyExpiredFails 刷新失败且 token 真过期 → 记 fail。
func TestCheckinAllRefreshTrulyExpiredFails(t *testing.T) {
	stub := &checkinStub{
		checkinBody: `{"code":0,"msg":"ok","data":{}}`,
		refreshFail: true,
	}
	srv := stub.server()
	defer srv.Close()
	p := pool.New("")
	// ExpiresAt 已是过去 → NeedsRefresh(0) 为 true（真过期），刷新失败即 fail。
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	out, _ := s.CheckinAll()
	if out[0].Status != CheckinFail {
		t.Errorf("status=%q want fail（真过期 + 刷新失败）", out[0].Status)
	}
	if out[0].Detail == "" || !strings.HasPrefix(out[0].Detail, "refresh:") {
		t.Errorf("detail=%q want refresh: 前缀", out[0].Detail)
	}
}

// TestCheckinAllBusy 并发第二次调用返回 ErrBusy（TryLock 串行化）。
func TestCheckinAllBusy(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 100}
	s, _ := newCheckinS(t, stub)
	// 手动持锁模拟一次签到正在执行，再调 CheckinAll 应得 ErrBusy。
	s.checkinMu.Lock()
	defer s.checkinMu.Unlock()
	_, err := s.CheckinAll()
	if !errors.Is(err, ErrBusy) {
		t.Errorf("err=%v want ErrBusy", err)
	}
}

// TestCheckinAllReenablesCoolingAccount 冷却账号签到成功 + 余额恢复 → 解冻。
func TestCheckinAllReenablesCoolingAccount(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 500}
	srv := stub.server()
	defer srv.Close()
	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out[0].Status != CheckinOK {
		t.Errorf("status=%q want ok", out[0].Status)
	}
	if st, _ := p.Status("u1"); st.Cooling {
		t.Errorf("签到 + 余额恢复应解冻: %+v", st)
	}
}
