package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─── resolveGateway：地址与密钥解析 ──────────────────────────────────────

// TestResolveGatewayFromConfig 从 config.json 取端口与 api_key（主路径）。
func TestResolveGatewayFromConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"listen":"127.0.0.1:7999","api_key":"sk-abc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB2A_CONFIG", cfg)
	t.Setenv("WB2A_URL", "")

	base, key, err := resolveGateway("")
	if err != nil {
		t.Fatalf("resolveGateway: %v", err)
	}
	if base != "http://127.0.0.1:7999" {
		t.Errorf("baseURL = %q, want http://127.0.0.1:7999", base)
	}
	if key != "sk-abc" {
		t.Errorf("apiKey = %q, want sk-abc", key)
	}
}

// TestResolveGatewayListenForms listen 的几种写法都要能取出端口。
func TestResolveGatewayListenForms(t *testing.T) {
	cases := []struct {
		listen string
		want   string
	}{
		{`{"listen":":7863"}`, "http://127.0.0.1:7863"},
		{`{"listen":"0.0.0.0:8080"}`, "http://127.0.0.1:8080"},
		{`{"listen":"127.0.0.1:9000"}`, "http://127.0.0.1:9000"},
		{`{"listen":"[::]:7863"}`, "http://127.0.0.1:7863"}, // IPv6 通配 → 收敛回环
		{`{"listen":"::"}`, "http://127.0.0.1:7863"},        // 裸 :: （手工切冒号会拼出非法基址）
		{`{"listen":"localhost:9999"}`, "http://localhost:9999"},
		// 非数字非地址的整串按 host 处理（normalizeListen 语义：SplitHostPort 失败且
		// 非纯数字 → 视为 host，缺端口补默认）。与 cmd/acct 逐字一致。
		{`{"listen":"bad"}`, "http://bad:7863"},
		{`{}`, "http://127.0.0.1:7863"}, // 缺字段 → 默认端口
	}
	for _, c := range cases {
		dir := t.TempDir()
		cfg := filepath.Join(dir, "config.json")
		_ = os.WriteFile(cfg, []byte(c.listen), 0o600)
		t.Setenv("WB2A_CONFIG", cfg)
		t.Setenv("WB2A_URL", "")

		base, _, err := resolveGateway("")
		if err != nil {
			t.Fatalf("listen=%s: %v", c.listen, err)
		}
		if base != c.want {
			t.Errorf("listen=%s → %q, want %q", c.listen, base, c.want)
		}
	}
}

// TestResolveGatewayURLOverride WB2A_URL 优先于配置文件。
func TestResolveGatewayURLOverride(t *testing.T) {
	t.Setenv("WB2A_URL", "http://example.com:1234/") // 带尾斜杠应被去掉
	t.Setenv("WB2A_API_KEY", "sk-env")
	t.Setenv("WB2A_CONFIG", "/nonexistent/config.json")

	base, key, err := resolveGateway("")
	if err != nil {
		t.Fatalf("resolveGateway: %v", err)
	}
	if base != "http://example.com:1234" {
		t.Errorf("baseURL = %q", base)
	}
	if key != "sk-env" {
		t.Errorf("apiKey = %q, want sk-env", key)
	}
}

// TestResolveGatewayMissingConfig 配置文件缺失且无 WB2A_URL 时报错，
// 且错误信息要指明可用 WB2A_URL（否则用户不知如何绕过）。
func TestResolveGatewayMissingConfig(t *testing.T) {
	t.Setenv("WB2A_URL", "")
	t.Setenv("WB2A_CONFIG", filepath.Join(t.TempDir(), "nope.json"))
	_, _, err := resolveGateway("")
	if err == nil {
		t.Fatal("配置缺失应报错")
	}
	if !strings.Contains(err.Error(), "WB2A_URL") {
		t.Errorf("错误信息应提示 WB2A_URL，得到: %v", err)
	}
}

// TestResolveGatewayBadJSON 配置文件非法 JSON 时报错而非静默用默认值。
func TestResolveGatewayBadJSON(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	_ = os.WriteFile(cfg, []byte(`{not json`), 0o600)
	t.Setenv("WB2A_URL", "")
	t.Setenv("WB2A_CONFIG", cfg)
	if _, _, err := resolveGateway(""); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

// ─── fetch：端点各状态处理 ───────────────────────────────────────────────

func TestFetchOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/stats" {
			t.Errorf("请求路径 = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"enabled":true,"uptime_sec":60,"total":{"requests":5},
		                        "models":[{"model":"m1","requests":5}]}`))
	}))
	defer srv.Close()

	st, err := fetch(srv.URL, "k", 5*time.Second)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !st.Enabled || st.Total.Requests != 5 || len(st.Models) != 1 {
		t.Errorf("解析结果异常: %+v", st)
	}
}

// TestFetchNotFound 旧版网关无该端点：错误信息要能指导用户，而不是裸 404。
func TestFetchNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, err := fetch(srv.URL, "k", 5*time.Second)
	if err == nil {
		t.Fatal("404 应报错")
	}
	if !strings.Contains(err.Error(), "/v1/stats") {
		t.Errorf("错误应点明缺失端点，得到: %v", err)
	}
	if !strings.Contains(err.Error(), "较新版本") {
		t.Errorf("错误应给出可操作指引，得到: %v", err)
	}
}

// TestFetchUnauthorized 401 提示检查 api_key。
func TestFetchUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := fetch(srv.URL, "bad", 5*time.Second)
	if err == nil {
		t.Fatal("401 应报错")
	}
	if !strings.Contains(err.Error(), "api_key") {
		t.Errorf("错误应提示 api_key，得到: %v", err)
	}
}

// TestFetchServerError 500 等仍报错并带响应片段。
func TestFetchServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	_, err := fetch(srv.URL, "k", 5*time.Second)
	if err == nil {
		t.Fatal("500 应报错")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("错误应含状态码，得到: %v", err)
	}
}

// TestFetchConnectionError 网关未启动时错误信息应含地址，便于判断连错对象。
func TestFetchConnectionError(t *testing.T) {
	_, err := fetch("http://127.0.0.1:1", "k", 2*time.Second)
	if err == nil {
		t.Fatal("连接失败应报错")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("错误应含目标地址，得到: %v", err)
	}
}

// ─── 渲染：字段缺失与未启用 ───────────────────────────────────────────────

// TestRenderDisabled 网关返回未启用载荷时透传其原始说明。
func TestRenderDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"enabled":false,"message":"统计载荷缺失"}`))
	}))
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := renderOnce(srv.URL, "k", 5*time.Second, false, "requests", layout{}); err != nil {
			t.Errorf("renderOnce: %v", err)
		}
	})
	if !strings.Contains(out, "未启用") {
		t.Errorf("应提示未启用，得到: %s", out)
	}
	if !strings.Contains(out, "统计载荷缺失") {
		t.Errorf("应保留网关原始说明，得到: %s", out)
	}
}

// TestRenderNoData 统计窗口内无请求（models 为空）时给出提示而非空表头。
func TestRenderNoData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"enabled":true,"uptime_sec":5,"total":{},"models":[]}`))
	}))
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := renderOnce(srv.URL, "k", 5*time.Second, false, "requests", layout{}); err != nil {
			t.Errorf("renderOnce: %v", err)
		}
	})
	if !strings.Contains(out, "暂无数据") {
		t.Errorf("应提示暂无数据，得到: %s", out)
	}
	if strings.Contains(out, "NaN") || strings.Contains(out, "+Inf") {
		t.Errorf("零值不应产生 NaN/Inf，得到: %s", out)
	}
}

// TestRenderJSONPassesThrough -json 输出可被再次解析，且字段完整。
func TestRenderJSONPassesThrough(t *testing.T) {
	payload := `{"enabled":true,"uptime_sec":60,"total":{"model":"(all)","requests":7,
	            "cache_hit_rate":0.5},"models":[{"model":"a","requests":7}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := renderOnce(srv.URL, "k", 5*time.Second, true, "requests", layout{}); err != nil {
			t.Errorf("renderOnce: %v", err)
		}
	})
	var got statsResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("-json 输出应为合法 JSON: %v\n%s", err, out)
	}
	if !got.Enabled || got.Total.Requests != 7 || len(got.Models) != 1 {
		t.Errorf("JSON 字段丢失: %+v", got)
	}
}

// TestSortModels 各排序键都应生效，且同值时不丢行。
func TestSortModels(t *testing.T) {
	rows := []modelStat{
		{Model: "a", Requests: 1, AvgTTFBMS: 300, TotalTokens: 10, Credit: 5},
		{Model: "b", Requests: 9, AvgTTFBMS: 100, TotalTokens: 30, Credit: 1},
		{Model: "c", Requests: 5, AvgTTFBMS: 200, TotalTokens: 20, Credit: 9},
	}
	cases := []struct{ key, want string }{
		{"requests", "b"},
		{"ttfb", "a"},
		{"tokens", "b"},
		{"credit", "c"},
		{"unknown", "b"}, // 未知键回落 requests
	}
	for _, c := range cases {
		rs := append([]modelStat(nil), rows...)
		sortModels(rs, c.key)
		if len(rs) != 3 {
			t.Fatalf("key=%s 排序后行数 = %d", c.key, len(rs))
		}
		if rs[0].Model != c.want {
			t.Errorf("key=%s 首行 = %s, want %s", c.key, rs[0].Model, c.want)
		}
	}
}

// ─── 单表渲染 ─────────────────────────────────────────────────────────────

// testNow 是表格测试共用的"当前时刻"，与 mkModel 的时间基准配合，
// 让"最后活动"这类相对时间列的渲染结果完全确定、可断言。
var testNow = time.Date(2026, 9, 15, 21, 30, 0, 0, time.UTC)

// mkModel 造一行统计，只填关心的字段。
//
// 刻意**不填** Streaming / LastSeen / CacheWriteTokens：这三列都有 show 条件，
// 全零时自动隐藏 —— 于是既有测试看到的仍是原来的 9 列，不受新增列干扰。
// 需要它们的用例自行赋值（见 mkRichModel）。
func mkModel(name string, req int64, failed int64) modelStat {
	return modelStat{
		Model: name, Requests: req, Success: req - failed, Failed: failed,
		AvgTTFBMS: 4500, AvgLatencyMS: 6900, TokensPerSec: 240,
		PromptTokens: 40_000_000, CompletionTokens: 77_000, TotalTokens: 40_077_000,
		CacheHitTokens: 39_000_000, CacheMissTokens: 1_000_000, CacheHitRate: 0.973,
		Credit: 31.73, CreditPerReq: 0.242,
	}
}

// mkRichModel 在 mkModel 之上补齐三个"填充性列"所需的字段。
func mkRichModel(name string, req, failed, streaming int64, lastSeen string) modelStat {
	m := mkModel(name, req, failed)
	m.Streaming = streaming
	if lastSeen != "" {
		s := lastSeen
		m.LastSeen = &s
	}
	return m
}

// TestTableSingleModelNoTotalRow 单模型时不出现合计行 —— 那一行本身就是汇总。
func TestTableSingleModelNoTotalRow(t *testing.T) {
	rows := []modelStat{mkModel("deepseek-v4.1-flash", 131, 0)}
	table := buildTable(rows, mkModel("(all)", 131, 0), "requests", 0, 0, testNow)

	joined := strings.Join(table, "\n")
	if strings.Contains(joined, "合计") {
		t.Errorf("单模型不应有合计行:\n%s", joined)
	}
	// 表头 + 分隔线 + 1 数据行 = 3 行
	if len(table) != 3 {
		t.Errorf("行数 = %d, want 3:\n%s", len(table), joined)
	}
}

// TestTableMultiModelHasTotalRow 多模型时出现分隔线 + 合计行。
func TestTableMultiModelHasTotalRow(t *testing.T) {
	rows := []modelStat{mkModel("a", 131, 0), mkModel("b", 70, 0)}
	table := buildTable(rows, mkModel("(all)", 201, 0), "requests", 0, 0, testNow)

	joined := strings.Join(table, "\n")
	if !strings.Contains(joined, "合计") {
		t.Errorf("多模型应有合计行:\n%s", joined)
	}
	// 表头 + 分隔线 + 2 数据 + 分隔线 + 合计 = 6 行
	if len(table) != 6 {
		t.Errorf("行数 = %d, want 6:\n%s", len(table), joined)
	}
}

// TestTableFailedColumnHidden 全部成功时不出现失败列（常态下省 4 列宽度）；
// 任一模型有失败则出现。
func TestTableFailedColumnHidden(t *testing.T) {
	noFail := buildTable([]modelStat{mkModel("a", 10, 0)}, mkModel("(all)", 10, 0), "requests", 0, 0, testNow)
	if strings.Contains(strings.Join(noFail, "\n"), "失败") {
		t.Errorf("无失败时不应出现失败列:\n%s", strings.Join(noFail, "\n"))
	}

	withFail := buildTable([]modelStat{mkModel("a", 10, 2)}, mkModel("(all)", 10, 2), "requests", 0, 0, testNow)
	if !strings.Contains(strings.Join(withFail, "\n"), "失败") {
		t.Errorf("有失败时应出现失败列:\n%s", strings.Join(withFail, "\n"))
	}
}

// TestTableColumnsAligned 全帧行宽一致 —— 这是对齐的回归断言。
//
// 用代码而非肉眼保证：含中文表头的行不比数据行窄/宽。若有人改回 %-10s 式
// 按 rune 补齐、或把分隔线写成固定长度，此测试立刻失败。
func TestTableColumnsAligned(t *testing.T) {
	cases := [][]modelStat{
		{mkModel("deepseek-v4.1-flash", 131, 0)},
		{mkModel("a", 131, 0), mkModel("中文模型名", 70, 3)},
		{mkModel("超长模型名超长模型名超长模型名超长模型名超长模型名", 5, 0)},
		{mkModel("x", 0, 0)}, // 全零行：占位符也要保持等宽
	}
	for i, rows := range cases {
		tbl := buildTable(rows, mkModel("(all)", 131, 0), "requests", 0, 0, testNow)
		want := displayWidth(tbl[0])
		for j, line := range tbl {
			if got := displayWidth(line); got != want {
				t.Errorf("case %d 行 %d 宽度 = %d, want %d\n  %q", i, j, got, want, line)
			}
		}
	}
}

// TestTableSeparatorAlignsBars 分隔线上的 + 必须与各行的 | 落在同一显示列。
//
// 这是"竖线分隔"能否看齐的核心：仅比较总宽度不够 —— 宽度相等但每段分配
// 不同（如 + 比 | 多占一列）同样会错位。故按显示列逐一比对。
func TestTableSeparatorAlignsBars(t *testing.T) {
	rows := []modelStat{mkModel("a", 131, 0), mkModel("中文模型名较长", 70, 3)}
	tbl := buildTable(rows, mkModel("(all)", 201, 3), "requests", 0, 0, testNow)

	// 找出所有含竖线的行（表头与每个数据行），以及所有分隔线行。
	var dataLines, sepLines []string
	for _, l := range tbl {
		switch {
		case strings.Contains(l, "|"):
			dataLines = append(dataLines, l)
		case strings.Contains(l, "+") && strings.Contains(l, "-"):
			sepLines = append(sepLines, l)
		}
	}
	if len(dataLines) == 0 || len(sepLines) == 0 {
		t.Fatalf("未找到竖线行或分隔线行:\n%s", strings.Join(tbl, "\n"))
	}

	want := barColumns(sepLines[0], '+')
	for _, l := range dataLines {
		if got := barColumns(l, '|'); !equalInts(got, want) {
			t.Errorf("竖线列位 %v 与分隔线 + 列位 %v 不一致:\n  %q", got, want, l)
		}
	}
	for _, l := range sepLines[1:] {
		if got := barColumns(l, '+'); !equalInts(got, want) {
			t.Errorf("分隔线之间列位不一致: %v vs %v", got, want)
		}
	}
}

// TestTableSeparatorWidthMatchesRows 分隔线与数据行等宽（TestTableColumnsAligned
// 已覆盖等宽，此处单独断言以便失败信息更直白）。
func TestTableSeparatorWidthMatchesRows(t *testing.T) {
	tbl := buildTable([]modelStat{mkModel("m", 5, 0)}, mkModel("(all)", 5, 0), "requests", 0, 0, testNow)
	if len(tbl) < 3 {
		t.Fatalf("表至少应有表头+分隔线+1 行，得到 %d 行", len(tbl))
	}
	hdr, sep := displayWidth(tbl[0]), displayWidth(tbl[1])
	if hdr != sep {
		t.Errorf("表头宽 %d ≠ 分隔线宽 %d\n  %q\n  %q", hdr, sep, tbl[0], tbl[1])
	}
}

// TestTableUsesOnlyASCIIBorder 边框只允许 ASCII —— 防回归到 ─(U+2500)。
//
// U+2500 的 East Asian Width 是 Ambiguous：终端可按 1 或 2 列渲染，导致
// 分隔线与表格对不齐（这正是改用 ASCII 的原因）。故断言分隔线字符集。
func TestTableUsesOnlyASCIIBorder(t *testing.T) {
	tbl := buildTable([]modelStat{mkModel("m", 5, 0), mkModel("n", 7, 0)}, mkModel("(all)", 12, 0), "requests", 0, 0, testNow)
	for _, l := range tbl {
		for _, r := range l {
			if r > 0x7F {
				// 允许表头/数据里的 CJK，只对"线"字符设限：
				// 制表符区块（U+2500–U+257F）一律不允许出现在表格里。
				if r >= 0x2500 && r <= 0x257F {
					t.Errorf("表格出现制表符 U+%04X（歧义宽度，应改用 ASCII）: %q", r, l)
				}
			}
		}
	}
}

// barColumns 返回 s 中所有目标字符所在的显示列号（0-based）。
func barColumns(s string, target rune) []int {
	var out []int
	col := 0
	for _, r := range s {
		if r == target {
			out = append(out, col)
		}
		col += runeWidth(r)
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDisplayWidth CJK 按 2 列计 —— 这是上面所有对齐的前提。
func TestDisplayWidth(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"abc", 3},
		{"总请求", 6},
		{"首字", 4},
		{"tok/s", 5},
		{"97.3%", 5},
		{"", 0},
		{"a中", 3},
	}
	for _, c := range cases {
		if got := displayWidth(c.in); got != c.want {
			t.Errorf("displayWidth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestPadAndTruncate 补齐与截断都按显示宽度而非 rune 数。
func TestPadAndTruncate(t *testing.T) {
	if got := padRight("中", 4); got != "中  " {
		t.Errorf("padRight = %q", got)
	}
	if got := padLeft("中", 4); got != "  中" {
		t.Errorf("padLeft = %q", got)
	}
	if got := padRight("abcd", 3); got != "abcd" {
		t.Errorf("已超宽不应截断，得到 %q", got)
	}
	// 上限 6 列：省略号占 2 列 → 内容预算 4 列 → 2 个汉字。
	got := truncateWidth("中文模型名很长", 6)
	if displayWidth(got) > 6 {
		t.Errorf("truncateWidth 超出上限: %q (宽度 %d)", got, displayWidth(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("截断应加省略号，得到 %q", got)
	}
}

// TestBuildFrameStructure 帧头部与尾注结构。
func TestBuildFrameStructure(t *testing.T) {
	st := &statsResponse{
		Enabled: true, UptimeSec: 5340,
		Since:  "2026-09-14T20:43:52.1042102+08:00",
		Total:  mkModel("(all)", 131, 0),
		Models: []modelStat{mkModel("deepseek-v4.1-flash", 131, 0)},
	}
	frame := buildFrame(st, "requests", nil, layout{})

	if !strings.Contains(frame[0], "网关请求统计") {
		t.Errorf("首行应为标题，得到 %q", frame[0])
	}
	// 措辞必须是「窗口」而非「运行」：该时长来自持久化的 since（跨重启保留），
	// 不是进程 uptime —— 写成"运行"会让用户误以为进程已跑这么久。
	if !strings.Contains(frame[0], "窗口 1h29m") {
		t.Errorf("标题应含统计窗口时长，得到 %q", frame[0])
	}
	if strings.Contains(frame[0], "运行") {
		t.Errorf("标题不应出现「运行」（会被误读为进程 uptime），得到 %q", frame[0])
	}
	// 同时给出起始时刻，进一步消除歧义。
	if !strings.Contains(frame[0], "自 09-14 20:43") {
		t.Errorf("标题应含窗口起始时刻，得到 %q", frame[0])
	}
	if !strings.Contains(frame[len(frame)-1], "账号积分") {
		t.Errorf("尾注应说明扣费单位，得到 %q", frame[len(frame)-1])
	}
	// 表体存在（含模型名）。
	if !strings.Contains(strings.Join(frame, "\n"), "deepseek-v4.1-flash") {
		t.Errorf("帧应含模型行:\n%s", strings.Join(frame, "\n"))
	}
}

// TestBuildFrameTitleWithoutSince since 缺失或不可解析时只显示窗口时长，
// 不应因解析失败而丢掉整个标题信息，也不应崩。
func TestBuildFrameTitleWithoutSince(t *testing.T) {
	cases := []string{"", "not-a-timestamp"}
	for _, since := range cases {
		st := &statsResponse{
			Enabled: true, UptimeSec: 3600, Since: since,
			Total:  mkModel("(all)", 1, 0),
			Models: []modelStat{mkModel("m", 1, 0)},
		}
		frame := buildFrame(st, "requests", nil, layout{})
		if !strings.Contains(frame[0], "窗口 1h0m") {
			t.Errorf("since=%q 时仍应显示窗口时长，得到 %q", since, frame[0])
		}
		if strings.Contains(frame[0], "自 ") {
			t.Errorf("since=%q 不可解析时不应出现起始时刻片段，得到 %q", since, frame[0])
		}
	}
}

// TestBuildFrameFetchError 拉取失败时错误进帧内（而非 stderr），
// 且帧结构仍成立 —— watch 模式依赖行数稳定才不覆盖错位。
func TestBuildFrameFetchError(t *testing.T) {
	frame := buildFrame(&statsResponse{}, "requests", fmt.Errorf("连接网关失败（http://x）: refused"), layout{})
	joined := strings.Join(frame, "\n")
	if !strings.Contains(joined, "连接网关失败") {
		t.Errorf("错误应出现在帧内:\n%s", joined)
	}
	if len(frame) < 3 {
		t.Errorf("帧结构应保持（标题+分隔线+错误），得到 %d 行", len(frame))
	}
}

// TestBuildFrameNoData models 为空时提示，且不产生表格。
func TestBuildFrameNoData(t *testing.T) {
	frame := buildFrame(&statsResponse{Enabled: true, UptimeSec: 5}, "requests", nil, layout{})
	joined := strings.Join(frame, "\n")
	if !strings.Contains(joined, "暂无数据") {
		t.Errorf("应提示暂无数据:\n%s", joined)
	}
}

// TestValidateFlags watch/json 互斥 + -sort 白名单。
func TestValidateFlags(t *testing.T) {
	if err := validateFlags(0, true, "requests"); err != nil {
		t.Errorf("仅 -json 应通过，得到 %v", err)
	}
	if err := validateFlags(5*time.Second, false, "requests"); err != nil {
		t.Errorf("仅 -watch 应通过，得到 %v", err)
	}
	if err := validateFlags(5*time.Second, true, "requests"); err == nil {
		t.Error("-watch 与 -json 同时使用应报错")
	} else if !strings.Contains(err.Error(), "json") {
		t.Errorf("错误应点明冲突的选项，得到 %v", err)
	}

	// -sort：五个合法值全通过。
	for _, k := range []string{"requests", "ttfb", "tokens", "credit", "credits"} {
		if err := validateFlags(0, false, k); err != nil {
			t.Errorf("-sort=%s 应通过，得到 %v", k, err)
		}
	}
	// 非法值必须报错，不能静默落到默认排序（用户会以为按 ttfb 排了）。
	for _, bad := range []string{"", "invalid", "Requests", "req"} {
		err := validateFlags(0, false, bad)
		if err == nil {
			t.Errorf("-sort=%q 应报错（静默回落会掩盖用户笔误）", bad)
			continue
		}
		if !strings.Contains(err.Error(), "sort") {
			t.Errorf("-sort 错误应点明选项名，得到 %v", err)
		}
	}
}

// TestNormalizeListen 与 cmd/acct 的 TestNormalizeListen 同表：两处实现必须逐字
// 一致，任一处的 IPv6 边界先走样都会导致同机两个工具解析出不同地址。
func TestNormalizeListen(t *testing.T) {
	cases := []struct{ in, want string }{
		{":7863", "http://127.0.0.1:7863"},
		{"0.0.0.0:7863", "http://127.0.0.1:7863"},
		{"::", "http://127.0.0.1:7863"},
		{"[::]:7863", "http://127.0.0.1:7863"},
		{"127.0.0.1:7863", "http://127.0.0.1:7863"},
		{"localhost:9999", "http://localhost:9999"},
		{"", "http://127.0.0.1:7863"},           // 空 → 默认端口
		{"127.0.0.1:", "http://127.0.0.1:7863"}, // 有 host 无端口
		{"7863", "http://127.0.0.1:7863"},       // 只有端口（无冒号 host 段）
	}
	for _, c := range cases {
		if got := normalizeListen(c.in); got != c.want {
			t.Errorf("normalizeListen(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestResolveGatewayServerOverride -server flag 只覆盖地址，key 仍从配置读。
func TestResolveGatewayServerOverride(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	_ = os.WriteFile(cfg, []byte(`{"listen":"127.0.0.1:1111","api_key":"from-config"}`), 0o600)
	t.Setenv("WB2A_CONFIG", cfg)
	t.Setenv("WB2A_URL", "http://127.0.0.1:2222")
	t.Setenv("WB2A_API_KEY", "")

	// -server 覆盖地址；key 来自配置文件（否则「只想换地址」会意外 401）。
	base, key, err := resolveGateway("http://127.0.0.1:3333/")
	if err != nil {
		t.Fatalf("resolveGateway: %v", err)
	}
	if base != "http://127.0.0.1:3333" {
		t.Errorf("-server 应覆盖地址且去掉尾斜杠，得到 %q", base)
	}
	if key != "from-config" {
		t.Errorf("-server 下 key 应来自 config.json，得到 %q", key)
	}

	// 显式 WB2A_API_KEY 优先于文件。
	t.Setenv("WB2A_API_KEY", "from-env")
	if _, key, _ := resolveGateway("http://127.0.0.1:3333"); key != "from-env" {
		t.Errorf("WB2A_API_KEY 应优先于 config.json，得到 %q", key)
	}
}

// TestRewriteFrameEscapes 覆盖重绘的转义序列：上移量等于上一帧行数，
// 每行擦到行尾，帧尾清屏。这是"原地覆盖"的核心 —— 若上移量算错，
// 帧就会错位或堆叠（Windows 上原本的表现）。
func TestRewriteFrameEscapes(t *testing.T) {
	// 第二帧（上一帧 5 行，本帧 3 行）：
	lines := []string{"a", "b", "c"}
	got := captureStdout(t, func() { rewriteFrame(lines, 5, true) })

	if !strings.HasPrefix(got, "\033[5A") {
		n := len(got)
		if n > 12 {
			n = 12
		}
		t.Errorf("应以光标上移 5 行开头，得到 %q", got[:n])
	}
	if n := strings.Count(got, "\033[K"); n != len(lines) {
		t.Errorf("每行应有一个 \\033[K（%d 个），得到 %d", len(lines), n)
	}
	if !strings.Contains(got, "\033[J") {
		t.Error("帧尾应有 \\033[J 清除多余旧行")
	}
	if !strings.Contains(got, "a") || !strings.Contains(got, "c") {
		t.Errorf("应包含全部行内容，得到 %q", got)
	}

	// 首帧（prevLines=0）不应上移光标 —— 否则会吃掉已有输出。
	first := captureStdout(t, func() { rewriteFrame(lines, 0, true) })
	if strings.Contains(first, "[0A") {
		t.Errorf("首帧不应包含上移转义，得到 %q", first)
	}
}

// ─── 倍率列与 -sort credits（issue #176）──────────────────────────────────

// TestSortModelsCredits -sort credits 的排序契约（C4）：
//   - 倍率降序（x0.06 > x0.03 > x0.00）；
//   - 缺失/不可解析（""）严格低于一切真实倍率（含 x0.00 免费模型）——
//     「未知」排在「免费」之后，与展示侧 "-" 的语义一致；
//   - 同倍率按模型名升序稳定排列。
func TestSortModelsCredits(t *testing.T) {
	rows := []modelStat{
		{Model: "a", Credits: "x0.06"},
		{Model: "b", Credits: "x0.03"},
		{Model: "c", Credits: ""},
		{Model: "d", Credits: "x0.00"},
	}
	sortModels(rows, "credits")
	want := []string{"a", "b", "d", "c"}
	for i, m := range rows {
		if m.Model != want[i] {
			t.Errorf("credits 排序第 %d 行 = %s, want %s（完整序：%v）", i, m.Model, want[i], rows)
		}
	}

	// 同倍率（x0.06）按名升序。
	tie := []modelStat{{Model: "zeta", Credits: "x0.06"}, {Model: "alpha", Credits: "x0.06"}}
	sortModels(tie, "credits")
	if tie[0].Model != "alpha" || tie[1].Model != "zeta" {
		t.Errorf("同倍率应按名升序：%v", tie)
	}

	// 全缺失：哨兵 -1 平局 → 纯名升序（退化但确定）。
	none := []modelStat{{Model: "b", Credits: ""}, {Model: "a", Credits: ""}}
	sortModels(none, "credits")
	if none[0].Model != "a" || none[1].Model != "b" {
		t.Errorf("全缺失应按名升序：%v", none)
	}
}

// TestCreditsRate creditsRate 的宽容解析（"x0.06 credits" 等上游形态）与
// 哨兵 -1（缺失/不可解析/负数）。
func TestCreditsRate(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"x0.06", 0.06},
		{"x0.06 credits", 0.06},
		{"x0.00", 0},
		{"x1", 1},
		{"", -1},
		{"x", -1},
		{"xabc", -1},
		{"x-0.5", -1},
		{"credits", -1},
	}
	for _, c := range cases {
		if got := creditsRate(c.in); got != c.want {
			t.Errorf("creditsRate(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestTableCreditsColumn 倍率列的渲染契约（C1/C2/C3）：
//   - (a) 任一行有倍率 → 列出现，位置在扣费左侧；缺失行渲染 "-"，
//     命中行渲染原文；
//   - (b) 全表无倍率（旧网关 / 冷缓存）→ 列整体隐藏，布局与既有 9 列一致。
func TestTableCreditsColumn(t *testing.T) {
	// (a) 有倍率。
	rich := mkModel("a", 10, 0)
	rich.Credits = "x0.06"
	plain := mkModel("b", 5, 0)
	tbl := buildTable([]modelStat{rich, plain}, mkModel("(all)", 15, 0), "requests", 0, 0, testNow)

	joined := strings.Join(tbl, "\n")
	if !strings.Contains(joined, "倍率") {
		t.Fatalf("有倍率时应出现倍率列:\n%s", joined)
	}
	if !strings.Contains(joined, "x0.06") {
		t.Errorf("倍率命中行应渲染原文 x0.06:\n%s", joined)
	}
	// 倍率列的 | 位置必须紧跟扣费列左侧：header 行里 倍率 的显示列区间终点 +1
	// 即为它与扣费之间的 |。
	hdr := tbl[0]
	bars := barColumns(hdr, '|')
	ri := strings.Index(hdr, "倍率")
	ki := strings.Index(hdr, "扣费")
	if ri < 0 || ki < 0 {
		t.Fatalf("表头缺 倍率/扣费：%q", hdr)
	}
	if ri >= ki {
		t.Fatalf("倍率应在扣费左侧（倍率@%d 扣费@%d）：%q", ri, ki, hdr)
	}
	// 倍率列右边界（它右侧最近的 |）== 扣费列左边界（其左侧最近的 |）。
	// byte 偏移须换算成显示列（CJK 占 2 列）才能与 barColumns 同口径比较。
	riCol := displayWidth(hdr[:ri])
	nextBar := -1
	for _, b := range bars {
		if b > riCol {
			nextBar = b
			break
		}
	}
	kiCol := displayWidth(hdr[:ki])
	prevBar := -1
	for _, b := range bars {
		if b < kiCol {
			prevBar = b
		}
	}
	if nextBar < 0 || nextBar != prevBar {
		t.Errorf("倍率与扣费应相邻（倍率右界 |@%d，扣费左界 |@%d）：%q", nextBar, prevBar, hdr)
	}

	// (b) 全表无倍率 → 列隐藏，且列数（| 个数）与既有 9 列布局一致。
	hidden := buildTable([]modelStat{mkModel("a", 10, 0)}, mkModel("(all)", 10, 0), "requests", 0, 0, testNow)
	hj := strings.Join(hidden, "\n")
	if strings.Contains(hj, "倍率") {
		t.Errorf("全表无倍率时不应出现倍率列:\n%s", hj)
	}
	baseBars := strings.Count(hidden[0], "|")
	richBars := strings.Count(tbl[0], "|")
	if richBars != baseBars+1 {
		t.Errorf("有倍率应恰好多一列（| %d → %d）", baseBars, richBars)
	}
}

// ─── 格式化辅助 ───────────────────────────────────────────────────────────

func TestFormatHelpers(t *testing.T) {
	if got := fmtInt(1234567); got != "1,234,567" {
		t.Errorf("fmtInt = %q", got)
	}
	if got := fmtInt(999); got != "999" {
		t.Errorf("fmtInt = %q", got)
	}
	if got := fmtInt(-1234); got != "-1,234" {
		t.Errorf("fmtInt 负数 = %q", got)
	}
	if got := fmtTokens(25430343); got != "25.43M" {
		t.Errorf("fmtTokens = %q", got)
	}
	if got := fmtTokens(865800); got != "865.8K" {
		t.Errorf("fmtTokens = %q", got)
	}
	if got := fmtTokens(42); got != "42" {
		t.Errorf("fmtTokens = %q", got)
	}
	if got := fmtMillis(4280); got != "4.28s" {
		t.Errorf("fmtMillis = %q", got)
	}
	if got := fmtMillis(650); got != "650ms" {
		t.Errorf("fmtMillis = %q", got)
	}
	if got := fmtMillis(0); got != "-" {
		t.Errorf("fmtMillis(0) = %q，应为占位符", got)
	}
	if got := fmtRate(0); got != "-" {
		t.Errorf("fmtRate(0) = %q，应为占位符", got)
	}
	if got := humanDuration(81 * time.Minute); got != "1h21m" {
		t.Errorf("humanDuration = %q", got)
	}
	if got := humanDuration(45 * time.Second); got != "45s" {
		t.Errorf("humanDuration = %q", got)
	}
}

// captureStdout 捕获标准输出（被测函数直接 fmt.Printf）。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}
