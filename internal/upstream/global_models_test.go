package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
)

// globalModelsSrv 返回一段 global 模型目录探测服务：record 逐条记录请求路径与鉴权头，
// respond 按路径决定响应。用于断言探测的 base/路径/鉴权头与缓存/回落行为。
// v3-config-merge 后探测两路并发，记录须并发安全（mu 保护）。
func globalModelsSrv(t *testing.T, calls *[]string, authz *string, respond func(path string) (int, string)) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*calls = append(*calls, r.URL.Path)
		if authz != nil {
			*authz = r.Header.Get("Authorization")
		}
		mu.Unlock()
		status, body := respond(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

// modelsResp 构造 /console|v2/enterprises/personal/models 形态响应（对象数组）。
func modelsResp(ids ...string) string {
	var sb strings.Builder
	sb.WriteString(`{"code":0,"data":{"models":[`)
	for i, id := range ids {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"id":"` + id + `","name":"` + id + `"}`)
	}
	sb.WriteString(`]}}`)
	return sb.String()
}

// globalModelsClient 构造探测用 Client：global base 指向 fake 服务、GlobalEnabled=true。
func globalModelsClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return &Client{
		HTTP:           &http.Client{},
		ChatBaseGlobal: strings.TrimSuffix(srv.URL, "/"),
		GlobalEnabled:  true,
	}
}

// TestFetchGlobalModelsProbePureDynamic 探测命中：走 global base + /v3/config（主）与
// /v2（企业补充）并发双路 + Bearer 鉴权头，结果为纯探测名单去重（重复 id 只出现一次，
// disabled 不入），不合并任何静态名单（探测未返回的 default-model 等历史名单成员不出现）。
func TestFetchGlobalModelsProbePureDynamic(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var calls []string
	var gotAuthz string
	srv := globalModelsSrv(t, &calls, &gotAuthz, func(path string) (int, string) {
		return 200, `{"code":0,"data":{"models":[
			{"id":"gpt-5.4","name":"GPT-5.4"},
			{"id":"probe-only-x","name":"Probe X"},
			{"id":"gpt-5.4","name":"dup"},
			{"id":"disabled-y","disabled":true}
		]}}`
	})
	defer srv.Close()

	got := globalModelsClient(t, srv).FetchGlobalModels(globalAcct())

	// Base：探测必须落在 global base（httptest 服务地址即被注成 ChatBaseGlobal）。
	if !strings.HasPrefix(srv.URL, "http://") {
		t.Fatal("unexpected srv.URL")
	}
	// v3-config-merge：/v3/config（主）+ /v2（企业首选）各一次，v2 200 即不打 /console。
	if len(calls) != 2 || !containsStr(calls, "/v3/config") || !containsStr(calls, "/v2/enterprises/personal/models") {
		t.Fatalf("probe calls=%v want [/v3/config /v2/enterprises/personal/models]", calls)
	}
	if gotAuthz != "Bearer at" {
		t.Errorf("probe authz=%q want Bearer at", gotAuthz)
	}
	// 纯动态去重：gpt-5.4 去重为 1（两路同 id 以 v3 条目为主，不重复）；probe-only-x 保留；
	// disabled-y 不入；静态历史名单成员（default-model 等）不出现（未探测到即无）。
	counts := map[string]int{}
	for _, id := range got {
		counts[id]++
	}
	if counts["gpt-5.4"] != 1 || counts["probe-only-x"] != 1 {
		t.Errorf("probed models dedupe failed: %v", counts)
	}
	if _, ok := counts["disabled-y"]; ok {
		t.Errorf("disabled model disabled-y should not appear")
	}
	if _, ok := counts["default-model"]; ok {
		t.Errorf("static legacy name default-model must not appear (pure dynamic)")
	}
}

// TestFetchGlobalModelsFailureReturnsNil 探测两路全失败（v3 + v2 + console 均 500）→
// 空名单（无静态回落）。
func TestFetchGlobalModelsFailureReturnsNil(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var calls []string
	srv := globalModelsSrv(t, &calls, nil, func(path string) (int, string) {
		return 500, `{"code":500,"msg":"boom"}`
	})
	defer srv.Close()

	got := globalModelsClient(t, srv).FetchGlobalModels(globalAcct())

	// v3 一路 + 企业家族两路（v2 500 → console 500）= 3 个请求。
	if len(calls) != 3 || !containsStr(calls, "/v3/config") ||
		!containsStr(calls, "/v2/enterprises/personal/models") ||
		!containsStr(calls, "/console/enterprises/personal/models") {
		t.Fatalf("fallback calls=%v want [/v3/config /v2/... /console/...]", calls)
	}
	if len(got) != 0 {
		t.Errorf("failure result=%v want empty (no static fallback)", got)
	}
}

// TestFetchGlobalModelsCache 成功后再调用命中 1h 缓存：零新上游请求，结果不变。
func TestFetchGlobalModelsCache(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var calls []string
	srv := globalModelsSrv(t, &calls, nil, func(path string) (int, string) {
		return 200, modelsResp("gpt-5.4", "probe-only-x")
	})
	defer srv.Close()

	c := globalModelsClient(t, srv)
	first := c.FetchGlobalModels(globalAcct())
	second := c.FetchGlobalModels(globalAcct())

	if len(calls) != 2 {
		t.Errorf("cache: probe calls=%d want 2 (v3+v2 once, second hit cache)", len(calls))
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("cached result differs from first")
	}
}

// TestFetchGlobalModelsNegativeCache 失败后进入负缓存：冷却期内再次调用零新请求（仍空名单）。
func TestFetchGlobalModelsNegativeCache(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var calls []string
	srv := globalModelsSrv(t, &calls, nil, func(path string) (int, string) {
		return 500, `{"code":500,"msg":"boom"}`
	})
	defer srv.Close()

	c := globalModelsClient(t, srv)
	first := c.FetchGlobalModels(globalAcct())
	second := c.FetchGlobalModels(globalAcct())

	// 首次 = v3 + 企业家族（v2+console）= 3 个请求；负缓存内二次零新请求。
	if len(calls) != 3 {
		t.Errorf("negative cache: probe calls=%d want 3 (v3 + family attempted once)", len(calls))
	}
	if len(first) != 0 || len(second) != 0 {
		t.Errorf("negative-cache results should be empty (no static fallback): %v %v", first, second)
	}
}

// TestGlobalModelInfosSnapshotReadOnly 只读快照的三个契约（issue #176 T1）：
//   - (a) 冷客户端（从未探测）→ 快照 nil，零上游请求（绝不主动探测）；
//   - (b) 一次 FetchGlobalModelInfos 预热（v3 + /v2 并发两请求）→ 快照返回
//     同一批全字段 infos（含 credits 原文）；
//   - (c) 预热后反复读快照 → fake 请求计数不变（只读，与 Fetch* 的 miss 即探测
//     语义相反——本方法服务 /v1/stats，缓存冷就冷，不发起网络）。
func TestGlobalModelInfosSnapshotReadOnly(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var calls []string
	srv := globalModelsSrv(t, &calls, nil, func(path string) (int, string) {
		return 200, `{"code":0,"data":{"models":[{"id":"hy3","name":"Hy3","credits":"x0.05"}]}}`
	})
	defer srv.Close()

	c := globalModelsClient(t, srv)

	// (a) 冷：快照 nil，零上游请求。
	if got := c.GlobalModelInfosSnapshot(); got != nil {
		t.Fatalf("cold snapshot = %+v, want nil", got)
	}
	if len(calls) != 0 {
		t.Fatalf("cold snapshot made %d upstream calls, want 0 (must not probe)", len(calls))
	}

	// (b) 预热：v3/config + /v2 企业路各一次 → 快照返回同批 infos。
	infos := c.FetchGlobalModelInfos(globalAcct())
	if len(infos) != 1 || infos[0].ID != "hy3" || infos[0].Credits != "x0.05" {
		t.Fatalf("warm FetchGlobalModelInfos = %+v, want [{hy3 x0.05}]", infos)
	}
	snap := c.GlobalModelInfosSnapshot()
	if len(snap) != 1 || snap[0].ID != "hy3" || snap[0].Credits != "x0.05" {
		t.Fatalf("snapshot = %+v, want same infos as fetch", snap)
	}
	warmCalls := len(calls)

	// (c) 只读：再读 N 次零新请求。
	for i := 0; i < 5; i++ {
		if got := c.GlobalModelInfosSnapshot(); got == nil || got[0].ID != "hy3" {
			t.Fatalf("snapshot read #%d = %+v, want cached infos", i, got)
		}
	}
	if len(calls) != warmCalls {
		t.Errorf("snapshot reads made %d new upstream calls, want 0 (read-only)", len(calls)-warmCalls)
	}
}

// TestFetchGlobalModelsParseNarrowTable 兼容窄表形态：data 直接是字符串数组。
func TestFetchGlobalModelsParseNarrowTable(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var calls []string
	srv := globalModelsSrv(t, &calls, nil, func(path string) (int, string) {
		return 200, `{"code":0,"data":["gpt-5.4","narrow-only"]}`
	})
	defer srv.Close()

	got := globalModelsClient(t, srv).FetchGlobalModels(globalAcct())

	counts := map[string]int{}
	for _, id := range got {
		counts[id]++
	}
	if counts["gpt-5.4"] != 1 || counts["narrow-only"] != 1 || counts["default-model"] != 0 {
		t.Errorf("narrow-table parse not as expected (pure dynamic): %v", counts)
	}
}
