// webprobe.go Web 管理台只读探测扩展：签到状态双端点、积分余额富版（新接口三路
// 并行 + legacy 回退）、套餐统一解析（字段族择优 / 时戳归一 / 到期标记）。
// 契约移植自 Workbuddy-Web/server/upstream.mjs（已对 4 个生产账号实测）。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// ---------------------------------------------------------------------------
// 时戳与数值小工具
// ---------------------------------------------------------------------------

// ParseTsMs 时戳归一为毫秒：>1e12 视为 ms，>1e9 视为 s；字符串按本地时间解析；无法识别返回 0。
func ParseTsMs(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case float64:
		return normTs(t)
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return normTs(n)
		}
		// "2006-01-02 15:04:05" 本地时区解析
		if ts, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.Local); err == nil {
			return ts.UnixMilli()
		}
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return ts.UnixMilli()
		}
	}
	return 0
}

func normTs(n float64) int64 {
	if n > 1e12 {
		return int64(n)
	}
	if n > 1e9 {
		return int64(n * 1000)
	}
	return 0
}

func round2f(n float64) float64 { return math.Round(n*100) / 100 }

// ---------------------------------------------------------------------------
// 签到状态（新端点回退旧端点）
// ---------------------------------------------------------------------------

// CheckinStatus 查询今日是否已签到。新接口 checkin-activity-status 失败（如 404）
// 回退旧接口 checkin-status；字段兼容蛇形 today_checked_in 与驼峰 todayCheckedIn。
func (c *Client) CheckinStatus(a *auth.Auth) (bool, error) {
	var lastErr error
	for _, p := range []string{
		"/v2/billing/meter/checkin-activity-status",
		"/v2/billing/meter/checkin-status",
	} {
		data, err := c.billingJSON(a, http.MethodPost, p, map[string]any{})
		if err != nil {
			lastErr = err
			continue
		}
		var resp struct {
			CheckedSnake  *bool `json:"today_checked_in"`
			CheckedCamel  *bool `json:"todayCheckedIn"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			lastErr = err
			continue
		}
		if resp.CheckedSnake != nil {
			return *resp.CheckedSnake, nil
		}
		if resp.CheckedCamel != nil {
			return *resp.CheckedCamel, nil
		}
		return false, nil
	}
	return false, lastErr
}

// ---------------------------------------------------------------------------
// 积分套餐统一解析
// ---------------------------------------------------------------------------

// ExpiringSoonMs 「近期到期」窗口：7 天。
const ExpiringSoonMs = int64(7 * 24 * 3600 * 1000)

// Package 统一形状的积分套餐。
type Package struct {
	Code         string         `json:"code"`
	Name         string         `json:"name"`
	Total        float64        `json:"total"`
	Used         float64        `json:"used"`
	Remaining    float64        `json:"remaining"`
	ExpireAtMs   int64          `json:"expireAtMs,omitempty"`
	Expired      bool           `json:"expired"`
	ExpiringSoon bool           `json:"expiringSoon"`
	Raw          map[string]any `json:"raw"` // 原始字段透传，前端可查看全部细节
}

// 到期字段候选（新旧接口命名合集）。
var expireKeys = []string{
	"DeductionEndTime", "deductionEndTime", "ExpiredTime", "expiredTime",
	"CycleEndTime", "cycleEndTime", "PackageEndTime", "EndTime",
	"ExpireTime", "PackageExpireTime", "ValidEndTime",
}

// 数值字段族：legacy 响应同包同时携带 Cycle*（全 0）与 Capacity*（有效值），
// "首个有限值"会错拿 Cycle 的 0；语义是"按族择优"——首个有正值的族胜出。
var fieldFamilies = []struct {
	total, remain, used []string
}{
	{
		total:  []string{"CycleCapacitySizePrecise", "CycleCapacitySize", "CycleTotalCapacity"},
		remain: []string{"CycleCapacityRemainPrecise", "CycleCapacityRemain", "CycleRemainCapacity"},
		used:   []string{"CycleCapacityUsedPrecise", "CycleCapacityUsed", "CycleUsedCapacity"},
	},
	{
		total:  []string{"CapacitySizePrecise", "CapacitySize"},
		remain: []string{"CapacityRemainPrecise", "CapacityRemain"},
		used:   []string{"CapacityUsedPrecise", "CapacityUsed"},
	},
	{
		total:  []string{"SlicePeriodCapacitySizePrecise", "SlicePeriodCapacitySize"},
		remain: []string{"SlicePeriodCapacityRemainPrecise", "SlicePeriodCapacityRemain"},
		used:   []string{"SlicePeriodCapacityUsedPrecise", "SlicePeriodCapacityUsed"},
	},
}

// firstNumber 取首个可解析为有限数值的字段；ok=false 表示全缺。
func firstNumber(obj map[string]any, keys []string) (float64, bool) {
	for _, k := range keys {
		v, ok := obj[k]
		if !ok || v == nil {
			continue
		}
		switch n := v.(type) {
		case float64:
			return n, true
		case int:
			return float64(n), true
		case int64:
			return float64(n), true
		case json.Number:
			if f, err := n.Float64(); err == nil {
				return f, true
			}
		case string:
			if strings.TrimSpace(n) == "" {
				continue
			}
			if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

func firstValue(obj map[string]any, keys []string) any {
	for _, k := range keys {
		if v, ok := obj[k]; ok && v != nil && v != "" {
			return v
		}
	}
	return nil
}

func firstString(obj map[string]any, keys []string) string {
	if v := firstValue(obj, keys); v != nil {
		return fmt.Sprintf("%v", v)
	}
	return ""
}

// optval 可选数值：区分"字段缺失"与"值为 0"（Node 版 ?? 空值合并语义）。
type optval struct {
	v  float64
	ok bool
}

// ParsePackage 单个套餐对象 → 统一形状；SlicePeriodUsageDetails 首项并入主对象候选。
func ParsePackage(raw map[string]any, nowMs int64) Package {
	var slice map[string]any
	for _, k := range []string{"SlicePeriodUsageDetails", "slicePeriodUsageDetails"} {
		if arr, ok := raw[k].([]any); ok && len(arr) > 0 {
			if m, ok := arr[0].(map[string]any); ok {
				slice = m
			}
			break
		}
	}
	pickFrom := func(keys []string) optval {
		if n, ok := firstNumber(raw, keys); ok {
			return optval{n, true}
		}
		if slice != nil {
			if n, ok := firstNumber(slice, keys); ok {
				return optval{n, true}
			}
		}
		return optval{}
	}
	// 族择优：首个 total/remain/used 任一为正的族；全族皆空 → 取首个有有限值的族。
	type entry struct{ t, r, u optval }
	var chosen entry
	found := false
	for _, fam := range fieldFamilies {
		t, r, u := pickFrom(fam.total), pickFrom(fam.remain), pickFrom(fam.used)
		if (t.ok && t.v > 0) || (r.ok && r.v > 0) || (u.ok && u.v > 0) {
			chosen = entry{t, r, u}
			found = true
			break
		}
		if !found && (t.ok || r.ok || u.ok) {
			chosen = entry{t, r, u}
			found = true
		}
	}
	// total:rawTotal ?? (rawRemain+rawUsed 若双双在场,否则 rawRemain ?? rawUsed ?? 0)
	var total float64
	switch {
	case chosen.t.ok:
		total = chosen.t.v
	case chosen.r.ok && chosen.u.ok:
		total = chosen.r.v + chosen.u.v
	case chosen.r.ok:
		total = chosen.r.v
	case chosen.u.ok:
		total = chosen.u.v
	}
	uVal := chosen.u.v // 缺省按 0
	remaining := total - uVal
	if chosen.r.ok {
		remaining = chosen.r.v
	}
	used := total - remaining
	if chosen.u.ok {
		used = chosen.u.v
	}
	clamp := func(n float64) float64 {
		if n < 0 {
			return 0
		}
		return round2f(n)
	}
	expireAtMs := ParseTsMs(firstValue(raw, expireKeys))
	return Package{
		Code:         firstString(raw, []string{"PackageCode", "packageCode"}),
		Name:         firstString(raw, []string{"PackageName", "packageName"}),
		Total:        clamp(total),
		Used:         clamp(used),
		Remaining:    clamp(remaining),
		ExpireAtMs:   expireAtMs,
		Expired:      expireAtMs > 0 && expireAtMs <= nowMs,
		ExpiringSoon: expireAtMs > nowMs && expireAtMs-nowMs <= ExpiringSoonMs,
		Raw:          raw,
	}
}

// atPath 按路径取嵌套值（map[string]any）。
func atPath(obj map[string]any, path ...string) any {
	var cur any = obj
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

// 包数组候选路径（相对信封 data；兼容 summary 的 Packages 与明细的 Accounts 两种命名）。
var pkgArrayPaths = [][]string{
	{"Packages"}, {"data", "Packages"}, {"Response", "Data", "Packages"}, {"data", "Response", "Data", "Packages"},
	{"packages"}, {"data", "packages"},
	{"Accounts"}, {"data", "Accounts"}, {"Response", "Data", "Accounts"}, {"data", "Response", "Data", "Accounts"},
	{"accounts"}, {"data", "accounts"},
}

// FindPackageArray 在信封 data 里按候选路径找第一个数组。
func FindPackageArray(data json.RawMessage) []map[string]any {
	if len(data) == 0 {
		return nil
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil
	}
	for _, p := range pkgArrayPaths {
		v := atPath(root, p...)
		arr, ok := v.([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		out := make([]map[string]any, 0, len(arr))
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// SumPackages 聚合套餐三元组（round2 抑制浮点 artifact）。
func SumPackages(packages []Package) (total, used, remain float64) {
	for _, p := range packages {
		total += p.Total
		used += p.Used
		remain += p.Remaining
	}
	clamp := func(n float64) float64 {
		if n < 0 {
			return 0
		}
		return round2f(n)
	}
	return clamp(total), clamp(used), clamp(remain)
}

// ---------------------------------------------------------------------------
// 积分余额富版：新接口三路 → merge（detail 优先，按 code 去重）→ legacy 回退
// ---------------------------------------------------------------------------

// ResourceRich 富版积分余额。
type ResourceRich struct {
	Source   string    `json:"source"` // "new" | "legacy"
	Total    float64   `json:"total"`
	Used     float64   `json:"used"`
	Remain   float64   `json:"remain"`
	Packages []Package `json:"packages"`
}

// paid/free 套餐码（credits.rs 契约；过滤用，避免无关套餐混入）。
var paidPackageCodes = []string{
	"TCACA_code_002_AkiJS3ZHF5", "TCACA_code_023_4xbGhMrE6q", "TCACA_code_026_BaESVICNoi",
	"TCACA_code_027_0FCGVA6vSa", "TCACA_code_009_0XmEQc2xOf", "TCACA_code_038_OhvqZtiPKr",
}
var freePackageCodes = []string{
	"TCACA_code_008_cfWoLwvjU4", "TCACA_code_007_nzdH5h4Nl0", "TCACA_code_028_NtpWi0jzXs",
	"TCACA_code_029_6wCGEWquYy", "TCACA_code_030_BjSt89qTvr",
}

// resourceBase 域名路由：workbuddy.cn 域账号发往 www.workbuddy.cn，其余发往 billingBase。
// 令牌域与 X-Domain 不一致会被上游拒绝。
func (c *Client) resourceBase(a *auth.Auth) string {
	d := strings.ToLower(strings.TrimSpace(a.Domain))
	if d == "workbuddy.cn" || d == "www.workbuddy.cn" {
		return "https://www.workbuddy.cn"
	}
	return c.billingBase(a)
}

// billingJSONAt 指定 base 发 billing 域请求（resourceBase 路由需要）。
func (c *Client) billingJSONAt(base string, a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		return nil, err
	}
	c.BillingHeaders(req, a)
	return c.doJSON(req)
}

// UserResourceRich 富版查询：summary/paid/free 三路顺序请求（量小无需并发），
// 任一路有包数组即按新接口出数；三路全败 → legacy get-user-resource 兜底。
func (c *Client) UserResourceRich(a *auth.Auth) (*ResourceRich, error) {
	base := c.resourceBase(a)
	nowMs := time.Now().UnixMilli()
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	dayEnd := dayStart.Add(24*time.Hour - time.Millisecond)

	try := func(path string, body any) []map[string]any {
		data, err := c.billingJSONAt(base, a, http.MethodPost, path, body)
		if err != nil {
			return nil
		}
		return FindPackageArray(data)
	}
	summaryRaw := try("/billing/meter/get-user-resource-summary", map[string]any{})
	paidRaw := try("/billing/meter/get-user-resource-paid-packages", map[string]any{
		"PageNumber": 1, "PageSize": 200, "Status": []int{0, 3},
		"PackageCodes": paidPackageCodes, "NeedRenewInfo": true,
	})
	freeRaw := try("/billing/meter/get-user-resource-free-packages", map[string]any{
		"PageNumber": 1, "PageSize": 200, "Status": []int{0, 3},
		"SlicePeriodStartTime": dayStart.Format("2006-01-02 15:04:05"),
		"SlicePeriodEndTime":   dayEnd.Format("2006-01-02 15:04:05"),
		"PackageCodes":         freePackageCodes,
	})

	if summaryRaw != nil || paidRaw != nil || freeRaw != nil {
		var detail []Package
		for _, r := range append(paidRaw, freeRaw...) {
			detail = append(detail, ParsePackage(r, nowMs))
		}
		detailCodes := map[string]bool{}
		for _, p := range detail {
			if p.Code != "" {
				detailCodes[p.Code] = true
			}
		}
		packages := detail
		for _, r := range summaryRaw {
			p := ParsePackage(r, nowMs)
			if p.Code == "" || !detailCodes[p.Code] {
				packages = append(packages, p)
			}
		}
		total, used, remain := SumPackages(packages)
		return &ResourceRich{Source: "new", Total: total, Used: used, Remain: remain, Packages: packages}, nil
	}

	// 新接口全失败（404/鉴权/网络）→ legacy 单接口兜底（与 UserResource 同请求体）。
	legacy, err := c.userResourceLegacy(a)
	if err != nil {
		return nil, err
	}
	return legacy, nil
}

// userResourceLegacy legacy get-user-resource（包解析升级为统一形状）。
func (c *Client) userResourceLegacy(a *auth.Auth) (*ResourceRich, error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	data, err := c.billingJSON(a, http.MethodPost, billingMeterPath, body)
	if err != nil {
		return nil, err
	}
	rawArr := FindPackageArray(data)
	packages := make([]Package, 0, len(rawArr))
	nowMs := time.Now().UnixMilli()
	for _, r := range rawArr {
		packages = append(packages, ParsePackage(r, nowMs))
	}
	total, used, remain := SumPackages(packages)
	return &ResourceRich{Source: "legacy", Total: total, Used: used, Remain: remain, Packages: packages}, nil
}
