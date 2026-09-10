package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
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

// travelStub 模拟 growth 域全部端点，记录调用次数与请求参数。
type travelStub struct {
	buddy       string // /buddy/info 的 data.buddy 原文（"null" 或对象）
	state       string // /travel/status 的 data 原文
	firstStatus int    // /buddy/first 的 HTTP 状态码（非 0 时按业务错误返回）

	infoCalls, statusCalls, departCalls atomic.Int32
	claimCalls, firstCalls, agreeCalls  atomic.Int32
	location, record                    atomic.Int64
}

func (s *travelStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activity/growth/buddy/info":
			s.infoCalls.Add(1)
			fmt.Fprintf(w, `{"code":0,"msg":"ok","data":{"buddy":%s}}`, s.buddy)
		case "/activity/growth/buddy/travel/status":
			s.statusCalls.Add(1)
			fmt.Fprintf(w, `{"code":0,"msg":"ok","data":%s}`, s.state)
		case "/activity/growth/buddy/travel/depart":
			s.departCalls.Add(1)
			var body struct {
				LocationID int `json:"location_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.location.Store(int64(body.LocationID))
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case "/activity/growth/buddy/travel/claim":
			s.claimCalls.Add(1)
			var body struct {
				RecordID int64 `json:"record_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.record.Store(body.RecordID)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"reward_credit":9}}`))
		case "/activity/growth/buddy/first":
			s.firstCalls.Add(1)
			if s.firstStatus >= 400 {
				w.WriteHeader(s.firstStatus)
				w.Write([]byte(`{"code":400,"msg":"first_buddy task not completed yet"}`))
				return
			}
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"buddy":{"id":1,"name":"档案喵"}}}`))
		case "/activity/growth/buddy/agreement":
			s.agreeCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"agreed":true}}`))
		default:
			http.Error(w, "not found", 404)
		}
	})
}

func (s *travelStub) server() *httptest.Server {
	return httptest.NewServer(s.handler())
}

// billingAndGrowthServer 同时模拟 billing（签到/余额/刷新）与 growth（旅行）端点，
// 供「签到收尾顺带跑旅行」这类跨域用例使用。
func billingAndGrowthServer(stub *travelStub) *httptest.Server {
	growth := stub.handler()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":500,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			growth.ServeHTTP(w, r)
		}
	}))
}

// fastTravel 关闭账号间限速，避免测试白等 800ms。
func fastTravel(t *testing.T) {
	t.Helper()
	old := travelAccountDelay
	travelAccountDelay = 0
	t.Cleanup(func() { travelAccountDelay = old })
}

// newTravelScheduler 构造 travel 相关依赖齐全的调度器。
func newTravelScheduler(t *testing.T, srv *httptest.Server, uids ...string) (*Scheduler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22}}), p
}

// TestRunCheckinNowTriggersTravel 签到收尾顺带跑一趟旅行（无猫 → 同意协议 + 领养）。
func TestRunCheckinNowTriggersTravel(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunCheckinNow()

	if n := stub.infoCalls.Load(); n != 1 {
		t.Errorf("buddy/info calls=%d want 1（签到收尾应顺带跑一趟旅行）", n)
	}
	if n := stub.firstCalls.Load(); n != 1 {
		t.Errorf("buddy/first calls=%d want 1（无猫应尝试领养）", n)
	}
	if n := stub.agreeCalls.Load(); n != 1 {
		t.Errorf("buddy/agreement calls=%d want 1", n)
	}
}

// TestRunCheckinTravelCoversAccountsJustReenabled 签到解冻的账号当轮即参与旅行。
func TestRunCheckinTravelCoversAccountsJustReenabled(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: `{"id":7,"name":"档案喵"}`,
		state: `{"state":"idle","daily_limit_reached":false}`}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	s, p := newTravelScheduler(t, srv, "u1")
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")

	s.RunCheckinNow()

	// 签到查到余额 500 解冻 → 收尾的旅行覆盖到该账号并派出。
	if n := stub.departCalls.Load(); n != 1 {
		t.Errorf("depart calls=%d want 1（刚解冻账号应被本轮旅行覆盖）", n)
	}
}

// TestRunKeepaliveDoesNotTriggerTravel 22 点保活不触发旅行：旅行只搭签到便车。
func TestRunKeepaliveDoesNotTriggerTravel(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunKeepaliveNow()

	if n := stub.infoCalls.Load(); n != 0 {
		t.Errorf("buddy/info calls=%d want 0（保活不触发旅行）", n)
	}
	if n := stub.firstCalls.Load(); n != 0 {
		t.Errorf("buddy/first calls=%d want 0", n)
	}
}

// TestRunTravelStateMachine 表驱动覆盖状态机全部分支：每趟只做一个动作。
func TestRunTravelStateMachine(t *testing.T) {
	const buddyPresent = `{"id":7,"name":"档案喵 R"}`

	cases := []struct {
		name         string
		buddy        string
		state        string
		firstStatus  int
		wantInfo     int32
		wantStatus   int32
		wantDepart   int32
		wantClaim    int32
		wantFirst    int32
		wantAgree    int32
		wantLocation int64
		wantRecord   int64
	}{
		{
			name: "无猫-领养成功", buddy: "null",
			wantInfo: 1, wantFirst: 1, wantAgree: 1,
		},
		{
			name: "无猫-门槛未达-静默跳过", buddy: "null", firstStatus: 400,
			wantInfo: 1, wantFirst: 1, wantAgree: 1,
		},
		{
			name: "有猫-idle-未达上限-派出", buddy: buddyPresent,
			state:    `{"state":"idle","daily_limit_reached":false}`,
			wantInfo: 1, wantStatus: 1, wantDepart: 1, wantLocation: 4,
		},
		{
			name: "有猫-idle-已达上限-跳过", buddy: buddyPresent,
			state:    `{"state":"idle","daily_limit_reached":true}`,
			wantInfo: 1, wantStatus: 1,
		},
		{
			name: "有猫-traveling-跳过", buddy: buddyPresent,
			state:    `{"state":"traveling","record_id":42}`,
			wantInfo: 1, wantStatus: 1,
		},
		{
			name: "有猫-arrived-领奖", buddy: buddyPresent,
			state:    `{"state":"arrived","record_id":42,"reward_credit":9}`,
			wantInfo: 1, wantStatus: 1, wantClaim: 1, wantRecord: 42,
		},
		{
			name: "有猫-arrived-缺record_id-跳过领奖", buddy: buddyPresent,
			state:    `{"state":"arrived","record_id":0}`,
			wantInfo: 1, wantStatus: 1,
		},
		{
			name: "有猫-未知状态-跳过", buddy: buddyPresent,
			state:    `{"state":"teleporting"}`,
			wantInfo: 1, wantStatus: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastTravel(t)
			stub := &travelStub{buddy: tc.buddy, state: tc.state, firstStatus: tc.firstStatus}
			srv := stub.server()
			defer srv.Close()

			s, _ := newTravelScheduler(t, srv, "u1")
			s.RunTravelNow()

			got := map[string]int32{
				"info": stub.infoCalls.Load(), "status": stub.statusCalls.Load(),
				"depart": stub.departCalls.Load(), "claim": stub.claimCalls.Load(),
				"first": stub.firstCalls.Load(), "agreement": stub.agreeCalls.Load(),
			}
			want := map[string]int32{
				"info": tc.wantInfo, "status": tc.wantStatus,
				"depart": tc.wantDepart, "claim": tc.wantClaim,
				"first": tc.wantFirst, "agreement": tc.wantAgree,
			}
			for k, w := range want {
				if got[k] != w {
					t.Errorf("%s calls=%d want %d", k, got[k], w)
				}
			}
			if tc.wantDepart > 0 && stub.location.Load() != tc.wantLocation {
				t.Errorf("location_id=%d want %d", stub.location.Load(), tc.wantLocation)
			}
			if tc.wantClaim > 0 && stub.record.Load() != tc.wantRecord {
				t.Errorf("record_id=%d want %d", stub.record.Load(), tc.wantRecord)
			}
		})
	}
}

// TestRunTravelAdoptThresholdTriedOncePerDay 门槛未达（400）当日只试一次，后续巡检静默跳过。
func TestRunTravelAdoptThresholdTriedOncePerDay(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null", firstStatus: 400}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()
	s.RunTravelNow()
	s.RunTravelNow()

	if n := stub.firstCalls.Load(); n != 1 {
		t.Errorf("first calls=%d want 1（当日只试一次）", n)
	}
	if n := stub.agreeCalls.Load(); n != 1 {
		t.Errorf("agreement calls=%d want 1", n)
	}
	if n := stub.infoCalls.Load(); n != 3 {
		t.Errorf("info calls=%d want 3（仍每趟查有无猫）", n)
	}
}

// TestRunTravelAdoptTriedExpiresNextDay 跨自然日后允许重新尝试领养。
func TestRunTravelAdoptTriedExpiresNextDay(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null", firstStatus: 400}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.markAdoptTried("u1") // 当日已试过
	s.RunTravelNow()
	if n := stub.firstCalls.Load(); n != 0 {
		t.Fatalf("first calls=%d want 0（当日已试过）", n)
	}

	s.adoptTried["u1"] = "2000-01-01" // 模拟昨日记录
	s.RunTravelNow()
	if n := stub.firstCalls.Load(); n != 1 {
		t.Errorf("first calls=%d want 1（跨日应重试）", n)
	}
}

// TestRunTravelSkipsDisabledAndFailedAccounts 禁用账号不查；单账号出错不影响后续账号。
func TestRunTravelSkipsDisabledAndFailedAccounts(t *testing.T) {
	fastTravel(t)
	var deadCalls, okDepart, disabledCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("X-User-Id") == "disabled":
			disabledCalls.Add(1)
			w.WriteHeader(401)
			w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
		case r.Header.Get("X-User-Id") == "dead":
			deadCalls.Add(1)
			w.WriteHeader(401)
			w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
		case r.URL.Path == "/activity/growth/buddy/info":
			w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1}}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/status":
			w.Write([]byte(`{"code":0,"data":{"state":"idle","daily_limit_reached":false}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/depart":
			okDepart.Add(1)
			w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	s, p := newTravelScheduler(t, srv, "disabled", "dead", "ok")
	p.Disable("disabled", "test")
	s.RunTravelNow()

	if n := deadCalls.Load(); n != 1 {
		t.Errorf("dead account calls=%d want 1（401 跳过本轮，不强刷 token）", n)
	}
	if n := disabledCalls.Load(); n != 0 {
		t.Errorf("disabled account calls=%d want 0", n)
	}
	// 前列账号失败不应中断遍历：末位 ok 账号照常完成派出。
	if n := okDepart.Load(); n != 1 {
		t.Errorf("ok account depart calls=%d want 1（失败账号不影响后续遍历）", n)
	}
	st, ok := p.Status("ok")
	if !ok {
		t.Fatal("ok 账号应在池中")
	}
	if st.Disabled {
		t.Errorf("ok 账号不应被影响: %+v", st)
	}
}

// TestRunTravelDisabledAccountSkipsAllCalls 禁用账号一个请求都不发。
func TestRunTravelDisabledAccountSkipsAllCalls(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := stub.server()
	defer srv.Close()

	s, p := newTravelScheduler(t, srv, "u1")
	p.Disable("u1", "test")
	s.RunTravelNow()

	if n := stub.infoCalls.Load(); n != 0 {
		t.Errorf("info calls=%d want 0（禁用账号应跳过）", n)
	}
}

// TestRunTravelActionErrorsDoNotAbort 各环节上游报错只影响本账号本轮：不 panic、不中断遍历。
func TestRunTravelActionErrorsDoNotAbort(t *testing.T) {
	fastTravel(t)
	var okDepart atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := r.Header.Get("X-User-Id")
		fail := func() {
			w.WriteHeader(500)
			w.Write([]byte(`{"code":500,"msg":"boom"}`))
		}
		switch {
		case uid == "bdinfo": // 查有无猫失败
			fail()
		case uid == "status" && r.URL.Path == "/activity/growth/buddy/travel/status": // 查状态失败
			fail()
		case uid == "depart" && r.URL.Path == "/activity/growth/buddy/travel/depart": // 派出发失败
			fail()
		case uid == "claim" && r.URL.Path == "/activity/growth/buddy/travel/claim": // 领奖失败
			fail()
		case uid == "agree" && r.URL.Path == "/activity/growth/buddy/agreement": // 同意协议失败
			fail()
		case uid == "first" && r.URL.Path == "/activity/growth/buddy/first": // 领养非门槛类失败
			fail()
		case r.URL.Path == "/activity/growth/buddy/info":
			if uid == "claim" {
				w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1}}}`))
				return
			}
			if uid == "depart" || uid == "status" || uid == "ok" {
				w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1}}}`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"buddy":null}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/status":
			if uid == "claim" {
				w.Write([]byte(`{"code":0,"data":{"state":"arrived","record_id":42}}`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"state":"idle","daily_limit_reached":false}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/depart":
			okDepart.Add(1)
			w.Write([]byte(`{"code":0,"data":{}}`))
		case r.URL.Path == "/activity/growth/buddy/agreement":
			w.Write([]byte(`{"code":0,"data":{"agreed":true}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "agree", "bdinfo", "claim", "depart", "first", "ok", "status")
	s.RunTravelNow() // 不应 panic

	// 末位账号（uid 排序后 ok 排在 status 前，但都在失败账号之后）照常完成派出。
	if n := okDepart.Load(); n == 0 {
		t.Errorf("depart calls=%d want >0（个别账号失败不应中断遍历）", n)
	}
}

// TestRunTravelLoopCancelsWhenDisabled 无任何整点任务时 Run 不空转，ctx 取消即返回。
func TestRunTravelLoopCancelsWhenDisabled(t *testing.T) {
	s := &Scheduler{cfg: Config{}, adoptTried: map[string]string{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未在 ctx 取消后返回")
	}
}

// TestRunTravelLoopStopsOnCancel 有排程在等计时器时 ctx 取消应立即退出。
func TestRunTravelLoopStopsOnCancel(t *testing.T) {
	s := New(Config{CheckinHours: []int{9}, KeepaliveHours: []int{22}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond) // 让 Run 进入 select 等待
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未在 ctx 取消后返回")
	}
}

// TestTravelDayAlignsCST 每日重置按 CST 自然日判定（UTC 17:00 已是次日 CST）。
func TestTravelDayAlignsCST(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{"UTC 凌晨 = CST 当日", time.Date(2026, 9, 11, 2, 0, 0, 0, time.UTC), "2026-09-11"},
		{"UTC 16:00 = CST 次日 00:00", time.Date(2026, 9, 11, 16, 0, 0, 0, time.UTC), "2026-09-12"},
		{"UTC 15:59 仍在 CST 当日", time.Date(2026, 9, 11, 15, 59, 0, 0, time.UTC), "2026-09-11"},
	}
	for _, c := range cases {
		if got := travelDay(c.in); got != c.want {
			t.Errorf("%s: travelDay=%s want %s", c.name, got, c.want)
		}
	}
}
