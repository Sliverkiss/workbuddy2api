// web_test.go 管理台核心逻辑单测：环形缓冲 / 统计聚合 / 账号视图。
package web

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/config"
	"workbuddy2api/internal/pool"
)

func testHandler(t *testing.T) *Handler {
	t.Helper()
	dir := t.TempDir()
	h := NewHandler(Config{
		AuthDir:      dir,
		StateDir:     dir,
		Pool:         pool.New(filepath.Join(dir, "state.json")),
		Upstream:     nil, // 统计/视图测试不触上游
		LoopbackBase: "http://127.0.0.1:1",
		Schedule:     config.DefaultSchedule(),
	})
	return h
}

func TestRing(t *testing.T) {
	r := newRing[int](3)
	for i := 1; i <= 5; i++ {
		r.add(i)
	}
	got := r.list(0)
	if len(got) != 3 || got[0] != 3 || got[2] != 5 {
		t.Errorf("环序错: %v, want [3 4 5]", got)
	}
	got = r.list(2)
	if len(got) != 2 || got[0] != 4 {
		t.Errorf("limit 错: %v, want [4 5]", got)
	}
	got = r.listFiltered(1, func(v int) bool { return v >= 4 })
	if len(got) != 1 || got[0] != 5 {
		t.Errorf("filter 错: %v, want [5]", got)
	}
}

func TestStatsOverview(t *testing.T) {
	h := testHandler(t)
	// 写入凭证文件（昵称映射）
	authJSON := `{"auth":{"accessToken":"t","refreshToken":"r","expiresAt":9999999999,"domain":"codebuddy.cn"},"account":{"uid":"u1","nickname":"一号"}}`
	if err := os.WriteFile(filepath.Join(h.cfg.AuthDir, "workbuddy-u1.json"), []byte(authJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	// 请求行:两条今日(一模型一账号) + 一条昨日
	today := nowMs()
	yesterday := today - 86400_000
	tok60, tok100 := 60, 100
	rate235, rate40 := 23.5, 40.0
	h.reqRows.add(ReqRow{Ts: today, Seq: 1, Model: "deepseek-v4", Mode: "stream", Status: 200, UID: "u1", Tokens: &tok60, TokPerSec: &rate235, TotalSec: 2.6})
	h.reqRows.add(ReqRow{Ts: today, Seq: 2, Model: "glm-5.2", Mode: "sync", Status: 200, UID: "u2", Tokens: &tok100, TokPerSec: &rate40, TotalSec: 2.5})
	h.reqRows.add(ReqRow{Ts: yesterday, Seq: 3, Model: "deepseek-v4", Mode: "stream", Status: 503, UID: "u1", TotalSec: 0.4})
	// 积分:u1 已探测(含 7 天内到期包);快照 900→800(消耗 100)
	h.st.setLiveStatus("u1", &LiveStatus{
		Ts: today, Checkin: &CheckinView{Checked: true},
		Credits: &CreditView{
			Source: "new", Remain: 800, Total: 1000, Used: 200,
			Packages: nil,
		},
		Errors: map[string]string{},
	})
	h.st.addSnapshot("u1", 900, today-3600_000)
	h.st.addSnapshot("u1", 800, today)

	req := httptest.NewRequest("GET", "/api/stats/overview", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Tokens struct {
			Requests     int     `json:"requests"`
			OutputTokens int     `json:"outputTokens"`
			AvgTokPerSec float64 `json:"avgTokPerSec"`
			Today        struct {
				Requests     int `json:"requests"`
				OutputTokens int `json:"outputTokens"`
			} `json:"today"`
			ByModel []struct {
				Key    string `json:"key"`
				Tokens int    `json:"tokens"`
			} `json:"byModel"`
			ByAccount []struct {
				Key      string `json:"key"`
				Nickname string `json:"nickname"`
			} `json:"byAccount"`
		} `json:"tokens"`
		Credits struct {
			Probed     int     `json:"probed"`
			Remain     float64 `json:"remain"`
			TodayDelta float64 `json:"todayDelta"`
			Accounts   []struct {
				UID      string `json:"uid"`
				Nickname string `json:"nickname"`
			} `json:"accounts"`
		} `json:"credits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Tokens.Requests != 3 || resp.Tokens.OutputTokens != 160 {
		t.Errorf("tokens 汇总: %+v", resp.Tokens)
	}
	if resp.Tokens.Today.Requests != 2 || resp.Tokens.Today.OutputTokens != 160 {
		t.Errorf("今日: %+v", resp.Tokens.Today)
	}
	if resp.Tokens.AvgTokPerSec != 31.8 {
		t.Errorf("平均速率 = %v, want 31.8", resp.Tokens.AvgTokPerSec)
	}
	if len(resp.Tokens.ByModel) == 0 || resp.Tokens.ByModel[0].Key != "glm-5.2" {
		t.Errorf("byModel 降序: %+v", resp.Tokens.ByModel)
	}
	if len(resp.Tokens.ByAccount) == 0 {
		t.Fatalf("byAccount 空")
	}
	if resp.Credits.Probed != 1 || resp.Credits.Remain != 800 {
		t.Errorf("credits: %+v", resp.Credits)
	}
	if resp.Credits.TodayDelta != -100 {
		t.Errorf("todayDelta = %v, want -100", resp.Credits.TodayDelta)
	}
	if len(resp.Credits.Accounts) != 1 || resp.Credits.Accounts[0].Nickname != "一号" {
		t.Errorf("昵称映射: %+v", resp.Credits.Accounts)
	}
}

func TestAccountViewBrokenFile(t *testing.T) {
	h := testHandler(t)
	if err := os.WriteFile(filepath.Join(h.cfg.AuthDir, "workbuddy-bad.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.cfg.AuthDir, "workbuddy-u1.json"),
		[]byte(`{"auth":{"accessToken":"t","expiresAt":9999999999},"account":{"uid":"u1","nickname":"一号"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/accounts", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp struct {
		DirExists bool `json:"dirExists"`
		Accounts  []struct {
			UID       *string `json:"uid"`
			Nickname  string  `json:"nickname"`
			LoadError *string `json:"loadError"`
			Status    string  `json:"status"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !resp.DirExists || len(resp.Accounts) != 2 {
		t.Fatalf("accounts: %+v", resp)
	}
	var broken, good int
	for _, a := range resp.Accounts {
		if a.LoadError != nil {
			broken++
		} else if a.UID != nil && *a.UID == "u1" && a.Status == "ok" {
			good++
		}
	}
	if broken != 1 || good != 1 {
		t.Errorf("broken=%d good=%d, want 1/1", broken, good)
	}
}
