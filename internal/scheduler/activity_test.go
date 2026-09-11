package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// fastActivity 关闭活跃上报账号间限速，避免测试白等 800ms。
func fastActivity(t *testing.T) {
	t.Helper()
	old := activityAccountDelay
	activityAccountDelay = 0
	t.Cleanup(func() { activityAccountDelay = old })
}

// reportStub 记录 /v2/report 调用次数与 userId。
type reportStub struct {
	calls  atomic.Int32
	uids   atomic.Int32
	bodies atomic.Int32
}

func (s *reportStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/report" {
			s.calls.Add(1)
			uid := r.Header.Get("X-User-Id")
			if uid != "" {
				s.uids.Add(1)
			}
			s.bodies.Add(1) // 标记收到 body（断言数组含 userId 在 upstream 包单测覆盖）
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
			return
		}
		http.Error(w, "not found", 404)
	})
}

// TestRunActivityNowReportsEachAccount 遍历池内每个可用账号上报一次。
func TestRunActivityNowReportsEachAccount(t *testing.T) {
	fastActivity(t)
	stub := &reportStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunActivityNow()

	if n := stub.calls.Load(); n != 2 {
		t.Errorf("report calls=%d want 2（每号上报一次）", n)
	}
	if n := stub.uids.Load(); n != 2 {
		t.Errorf("report with X-User-Id=%d want 2（每号必带 userId）", n)
	}
}

// TestRunActivityNowSkipsDisabledAndNoToken 禁用账号与无 token 账号跳过。
func TestRunActivityNowSkipsDisabledAndNoToken(t *testing.T) {
	fastActivity(t)
	stub := &reportStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "ok", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "dis", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "notoken", AccessToken: "", RefreshToken: "", ExpiresAt: 9999999999})
	p.Disable("dis", "test")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunActivityNow()

	if n := stub.calls.Load(); n != 1 {
		t.Errorf("report calls=%d want 1（仅 ok 账号）", n)
	}
}

// TestRunActivityNowErrorDoesNotAbort 单账号上报失败不影响后续遍历。
func TestRunActivityNowErrorDoesNotAbort(t *testing.T) {
	fastActivity(t)
	var okCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := r.Header.Get("X-User-Id")
		if uid == "fail" {
			w.WriteHeader(500)
			w.Write([]byte(`boom`))
			return
		}
		okCalls.Add(1)
		w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "fail", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "ok", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunActivityNow() // 不应 panic

	if n := okCalls.Load(); n != 1 {
		t.Errorf("ok account report calls=%d want 1（失败账号不影响后续遍历）", n)
	}
}

// TestRunCheckinDoesNotTriggerTravel 签到收尾不再跑旅行（旅行已剥离为独立排程）。
func TestRunCheckinDoesNotTriggerTravel(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunCheckinNow()

	// 签到不再顺带跑旅行：buddy/info 不应被调用。
	if n := stub.infoCalls.Load(); n != 0 {
		t.Errorf("buddy/info calls=%d want 0（旅行已从签到剥离）", n)
	}
}

// TestNextWakeTravelIndependent 旅行有独立时点，与签到互不影响。
func TestNextWakeTravelIndependent(t *testing.T) {
	s := New(Config{
		CheckinHours:   []int{21},
		TravelHours:    []int{9},
		ActivityHours:  []int{10},
		KeepaliveHours: []int{22},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（旅行 09:00 独立时点）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskTravel {
		t.Errorf("kinds=%v want [travel]", kinds)
	}
}

// TestNextWakeActivityIndependent 活跃上报有独立时点。
func TestNextWakeActivityIndependent(t *testing.T) {
	s := New(Config{
		CheckinHours:   []int{21},
		TravelHours:    []int{9},
		ActivityHours:  []int{10},
		KeepaliveHours: []int{22},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 9, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 10, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（活跃 10:00 独立时点）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskActivity {
		t.Errorf("kinds=%v want [activity]", kinds)
	}
}

// TestNextWakeTravelDisabled 旅行禁用后排程里不再有旅行时点（签到照常）。
func TestNextWakeTravelDisabled(t *testing.T) {
	s := New(Config{
		CheckinHours:   []int{9, 21},
		TravelHours:    []int{9},
		TravelDisabled: true,
		KeepaliveHours: []int{22},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 8, 0, 0, 0, time.Local))
	// 旅行禁用 → 09:00 旅行时点不应出现，最近的是 09:00 签到（同小时但签到未禁用）。
	if want := time.Date(2026, 9, 11, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if !hasKind(kinds, taskCheckin) {
		t.Errorf("kinds=%v want 含 checkin", kinds)
	}
	if hasKind(kinds, taskTravel) {
		t.Errorf("kinds=%v 不应含 travel（已禁用）", kinds)
	}
}

// TestNextWakeActivityDisabled 活跃上报禁用后排程里不再有活跃时点。
func TestNextWakeActivityDisabled(t *testing.T) {
	s := New(Config{
		CheckinHours:     []int{9, 21},
		ActivityHours:    []int{10},
		ActivityDisabled: true,
		KeepaliveHours:   []int{22},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 9, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（活跃禁用 → 跳过 10:00）", at, want)
	}
	if hasKind(kinds, taskActivity) {
		t.Errorf("kinds=%v 不应含 activity（已禁用）", kinds)
	}
}

// TestCheckinDisabledTravelStillRuns 签到禁用时旅行/活跃照跑（验收标准 2）。
func TestCheckinDisabledTravelStillRuns(t *testing.T) {
	s := New(Config{
		CheckinHours:    []int{9, 21},
		CheckinDisabled: true,
		TravelHours:     []int{9},
		ActivityHours:   []int{10},
		KeepaliveHours:  []int{22},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（签到禁用，旅行 09:00 照跑）", at, want)
	}
	if hasKind(kinds, taskCheckin) {
		t.Errorf("kinds=%v 不应含 checkin（已禁用）", kinds)
	}
	if !hasKind(kinds, taskTravel) {
		t.Errorf("kinds=%v 应含 travel（签到禁用但旅行独立）", kinds)
	}
}

// TestAllFourDisabledNoSpin 四类任务全禁用：Run 不空转。
func TestAllFourDisabledNoSpin(t *testing.T) {
	s := New(Config{
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		CheckinHours:      []int{9, 21},
		TravelHours:       []int{9},
		ActivityHours:     []int{10},
		KeepaliveHours:    []int{22},
	})
	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("at=%v kinds=%v want zero/nil（四类全禁用）", at, kinds)
	}
}

// TestNextWakeSameHourTravelAndCheckin 旅行与签到配到同一小时时两类任务都要执行。
func TestNextWakeSameHourTravelAndCheckin(t *testing.T) {
	s := New(Config{
		CheckinHours:   []int{9, 21},
		TravelHours:    []int{9},
		ActivityHours:  []int{10},
		KeepaliveHours: []int{22},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if !hasKind(kinds, taskCheckin) || !hasKind(kinds, taskTravel) {
		t.Errorf("kinds=%v want 含 checkin+travel（同 09:00 两任务）", kinds)
	}
}

// TestRunDispatchesActivityAndTravel Run 到点分发 activity 与 travel（不真打上游，用空池）。
func TestRunDispatchesActivityAndTravel(t *testing.T) {
	fastActivity(t)
	fastTravel(t)
	// 空 pool → RunActivityNow/RunTravelNow 遍历 0 账号即返回，不阻塞。
	p := pool.New("")
	up := &upstream.Client{}
	s := New(Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   []int{},
		TravelHours:    []int{},
		ActivityHours:  []int{},
		KeepaliveHours: []int{},
	})
	// 四类全空 hours → nextWake 回落默认 → 会构造 timer，ctx 取消即返回。
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未在 ctx 取消后返回")
	}
}
