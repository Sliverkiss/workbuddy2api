// webprobe_test.go Web 管理台探测扩展单测：套餐解析族择优 / 时戳归一 / 签到双端点 /
// 富版积分三路合并与 legacy 回退 / 旅行富字段。契约与 Workbuddy-Web selftest 同源。
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
)

const testNowMs = 1773388800000 // 2026-03-13 16:00:00 +08

// ---- ParseTsMs ----

func TestParseTsMs(t *testing.T) {
	cases := []struct {
		in   any
		want int64
	}{
		{float64(1773388800000), 1773388800000}, // 毫秒原样
		{float64(1773388800), 1773388800000},    // 秒 ×1000
		{"1773388800", 1773388800000},           // 数字字符串（秒）
		{float64(0), 0}, {nil, 0}, {"", 0}, {"abc", 0}, {float64(123), 0},
	}
	for i, c := range cases {
		if got := ParseTsMs(c.in); got != c.want {
			t.Errorf("case %d: ParseTsMs(%v) = %d, want %d", i, c.in, got, c.want)
		}
	}
	// 字符串本地时间：只断"能解析且量级合理"（机器时区不定）。
	if got := ParseTsMs("2026-03-13 16:00:00"); got < 1.7e12 || got > 1.9e12 {
		t.Errorf("本地时间字符串解析异常: %d", got)
	}
}

// ---- ParsePackage 族择优（Node selftest 488≠493 同源用例）----

func TestParsePackageFamilyPick(t *testing.T) {
	// legacy 形态：Cycle* 全 0 与 Capacity* 有效值同包共存 → Capacity 族必须胜出。
	raw := map[string]any{
		"PackageCode": "P1", "PackageName": "套餐1",
		"CycleCapacitySize": 0, "CycleCapacityRemain": 0, "CycleCapacityUsed": 0,
		"CapacitySize": 5, "CapacityRemain": 5, "CapacityUsed": 0,
	}
	p := ParsePackage(raw, testNowMs)
	if p.Remaining != 5 || p.Total != 5 {
		t.Errorf("族择优失败: remain=%v total=%v, want 5/5", p.Remaining, p.Total)
	}
}

func TestParsePackagePreciseRound2(t *testing.T) {
	// Precise 浮点:round2 抑制求和 artifact（3032.52000001 → 3032.52）。
	raw := map[string]any{
		"PackageCode": "P2",
		"CycleCapacitySizePrecise": 3032.520000000001, "CycleCapacityRemainPrecise": 3000.1,
		"CycleCapacityUsedPrecise": 32.420000000001,
	}
	p := ParsePackage(raw, testNowMs)
	if p.Total != 3032.52 || p.Remaining != 3000.1 || p.Used != 32.42 {
		t.Errorf("round2 失败: %+v", p)
	}
}

func TestParsePackageExpiry(t *testing.T) {
	soon := float64(testNowMs + 3*24*3600*1000)
	raw := map[string]any{
		"CapacitySize": 100, "CapacityRemain": 40, "DeductionEndTime": soon,
	}
	p := ParsePackage(raw, testNowMs)
	if p.ExpireAtMs != int64(soon) || p.Expired || !p.ExpiringSoon {
		t.Errorf("到期标记失败: %+v", p)
	}
	past := ParsePackage(map[string]any{
		"CapacitySize": 100, "CapacityRemain": 40, "ExpiredTime": float64(testNowMs - 1000),
	}, testNowMs)
	if !past.Expired || past.ExpiringSoon {
		t.Errorf("已过期判定失败: %+v", past)
	}
}

func TestParsePackageRemainDerived(t *testing.T) {
	// 只有 total/used → remaining 推导；全缺 → 全 0。
	p := ParsePackage(map[string]any{"CapacitySize": 100, "CapacityUsed": 30}, testNowMs)
	if p.Remaining != 70 {
		t.Errorf("remaining 推导失败: %v", p.Remaining)
	}
	zero := ParsePackage(map[string]any{}, testNowMs)
	if zero.Total != 0 || zero.Remaining != 0 || zero.Used != 0 {
		t.Errorf("空包应为全 0: %+v", zero)
	}
}

// ---- FindPackageArray 路径兼容 ----

func TestFindPackageArrayPaths(t *testing.T) {
	mk := func(data string) json.RawMessage { return json.RawMessage(data) }
	arr := FindPackageArray(mk(`{"Packages":[{"PackageCode":"A","CapacityRemain":1}]}`))
	if len(arr) != 1 {
		t.Error("顶层 Packages 未命中")
	}
	arr = FindPackageArray(mk(`{"Response":{"Data":{"Accounts":[{"PackageCode":"B"}]}}}`))
	if len(arr) != 1 || arr[0]["PackageCode"] != "B" {
		t.Error("Response.Data.Accounts 未命中")
	}
	if FindPackageArray(mk(`{"nothing":true}`)) != nil {
		t.Error("无包路径应返回 nil")
	}
}

// ---- CheckinStatus 双端点 ----

func newTestClient(base string) *Client {
	c := New()
	c.BillingBaseCN = base
	c.ChatBaseCN = base
	return c
}

func testAuth() *auth.Auth {
	return &auth.Auth{AccessToken: "tok", UID: "u1", Domain: "codebuddy.cn"}
}

func TestCheckinStatusFallback(t *testing.T) {
	var hitNew, hitOld int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost { // 上游签到状态仅接受 POST {}（GET 返回 404）
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.URL.Path {
		case "/v2/billing/meter/checkin-activity-status":
			hitNew++
			w.WriteHeader(http.StatusNotFound) // 新端点 404 → 回退
		case "/v2/billing/meter/checkin-status":
			hitOld++
			fmt.Fprint(w, `{"code":0,"msg":"","data":{"todayCheckedIn":true}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	checked, err := newTestClient(srv.URL).CheckinStatus(testAuth())
	if err != nil || !checked {
		t.Errorf("回退失败: checked=%v err=%v", checked, err)
	}
	if hitNew != 1 || hitOld != 1 {
		t.Errorf("端点命中次数: new=%d old=%d, want 1/1", hitNew, hitOld)
	}
}

func TestCheckinStatusSnakeCase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"msg":"","data":{"today_checked_in":true}}`)
	}))
	defer srv.Close()
	checked, err := newTestClient(srv.URL).CheckinStatus(testAuth())
	if err != nil || !checked {
		t.Errorf("蛇形字段失败: checked=%v err=%v", checked, err)
	}
}

// ---- UserResourceRich 三路合并 + legacy 回退 ----

func TestUserResourceRichMerge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data string
		switch r.URL.Path {
		case "/billing/meter/get-user-resource-summary":
			// summary 含与 detail 同 code 的包（应被去重）+ 独有包
			data = `{"Packages":[
				{"PackageCode":"P1","PackageName":"同码","CapacitySize":999,"CapacityRemain":999},
				{"PackageCode":"P3","PackageName":"独有","CapacitySize":40,"CapacityRemain":40}]}`
		case "/billing/meter/get-user-resource-paid-packages":
			data = `{"Packages":[{"PackageCode":"P1","PackageName":"付费","CapacitySize":800,"CapacityRemain":800}]}`
		case "/billing/meter/get-user-resource-free-packages":
			data = `{"Accounts":[{"PackageCode":"P2","PackageName":"免费","CapacitySize":0,"CapacityRemain":0}]}`
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"code":0,"msg":"","data":%s}`, data)
	}))
	defer srv.Close()
	rich, err := newTestClient(srv.URL).UserResourceRich(testAuth())
	if err != nil {
		t.Fatalf("rich: %v", err)
	}
	if rich.Source != "new" {
		t.Errorf("source = %s, want new", rich.Source)
	}
	// 期望:P1 取 detail(800 而非 999)+ P2(0)+ P3 独有(40)→ remain 840
	if rich.Remain != 840 {
		t.Errorf("remain = %v, want 840(detail 优先去重)", rich.Remain)
	}
	if len(rich.Packages) != 3 {
		t.Errorf("packages = %d, want 3", len(rich.Packages))
	}
}

func TestUserResourceRichLegacyFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/billing/meter/get-user-resource" {
			// legacy:Cycle* 全 0 + Capacity* 有效（族择优实战）
			fmt.Fprint(w, `{"code":0,"msg":"","data":{"Response":{"Data":{"Accounts":[
				{"PackageName":"包1","CycleCapacitySize":0,"CycleCapacityRemain":0,"CapacitySize":493,"CapacityRemain":488}
			]}}}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound) // 新接口三路全 404
	}))
	defer srv.Close()
	rich, err := newTestClient(srv.URL).UserResourceRich(testAuth())
	if err != nil {
		t.Fatalf("legacy: %v", err)
	}
	if rich.Source != "legacy" || rich.Remain != 488 {
		t.Errorf("legacy 回退: source=%s remain=%v, want legacy/488", rich.Source, rich.Remain)
	}
}

// ---- TravelStatus 富字段（秒 → 毫秒归一）----

func TestTravelStatusRich(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/activity/growth/buddy/travel/status" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"code":0,"msg":"","data":{
			"state":"arrived","daily_limit_reached":true,"record_id":123,"reward_credit":8,
			"location":{"id":4,"name":"古镇客栈"},"buddy_id":77,
			"depart_at":1773380000,"arrive_at":"1773388800","server_now":1773388800000}}`)
	}))
	defer srv.Close()
	st, err := newTestClient(srv.URL).TravelStatus(testAuth())
	if err != nil {
		t.Fatalf("travel: %v", err)
	}
	if st.State != "arrived" || st.RecordID != 123 || st.RewardCredit != 8 {
		t.Errorf("基础字段: %+v", st)
	}
	if st.Location == nil || st.Location.Name != "古镇客栈" || st.Location.ID != 4 {
		t.Errorf("location: %+v", st.Location)
	}
	if st.DepartAtMs != 1773380000000 {
		t.Errorf("depart 秒归一: %d", st.DepartAtMs)
	}
	if st.ArriveAtMs != 1773388800000 {
		t.Errorf("arrive 字符串秒归一: %d", st.ArriveAtMs)
	}
	if st.ServerNowMs != 1773388800000 {
		t.Errorf("server_now 毫秒原样: %d", st.ServerNowMs)
	}
	if st.BuddyID != 77 {
		t.Errorf("buddy_id: %d", st.BuddyID)
	}
}
