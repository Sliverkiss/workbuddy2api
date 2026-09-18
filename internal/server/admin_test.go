// admin_test.go 运维管理端点的契约测试（issue #138/#118）。
//
// 覆盖：开关默认关闭（404 且不暴露管理面）、鉴权与 /status 同源、三个操作的状态迁移、
// 幂等、未知 uid、以及「enable 不解除自动禁用 / revive 不解除手动停用」的正交语义。
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
)

// adminResp 与 adminState 同构（测试侧独立声明，避免测试跟着实现改字段名）。
type adminResp struct {
	UID            string `json:"uid"`
	ManualDisabled bool   `json:"manual_disabled"`
	ManualReason   string `json:"manual_reason"`
	Disabled       bool   `json:"disabled"`
	Changed        bool   `json:"changed"`
}

func doAdmin(t *testing.T, h *Handler, method, path string, body string) (*httptest.ResponseRecorder, adminResp) {
	t.Helper()
	var rdr *bytes.Reader
	if body == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader([]byte(body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, rdr))
	var out adminResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode admin response: %v body=%s", err, rec.Body)
		}
	}
	return rec, out
}

// TestAdminDisabledByDefault 开关默认关闭：路径一律 404（而非 403），
// 不向外暴露"这里存在管理面"。
func TestAdminDisabledByDefault(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p}) // AdminEnabled 零值 false

	for _, path := range []string{
		"/admin/accounts/u1/disable",
		"/admin/accounts/u1/enable",
		"/admin/accounts/u1/revive",
	} {
		rec, _ := doAdmin(t, h, "POST", path, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: code=%d want 404 when admin disabled", path, rec.Code)
		}
	}
	// 状态位未被触碰
	if st, _ := p.Status("u1"); st.ManualDisabled {
		t.Fatal("关闭状态下不应有任何状态变更")
	}
}

// TestAdminRequiresSameAPIKey 管理端点与 /status 共用同一把 key。
func TestAdminRequiresSameAPIKey(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, APIKey: "secret", AdminEnabled: true})

	// 无 key → 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/accounts/u1/disable", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401 without key", rec.Code)
	}
	// 错 key → 401
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/accounts/u1/disable", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401 with wrong key", rec.Code)
	}
	// 正确 key → 200
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/admin/accounts/u1/disable", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 with correct key, body=%s", rec.Code, rec.Body)
	}
}

// TestAdminDisableEnableRoundTrip 停用 → 状态可读且不参与选号 → 恢复 → 可选。
func TestAdminDisableEnableRoundTrip(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, AdminEnabled: true})

	rec, out := doAdmin(t, h, "POST", "/admin/accounts/u1/disable", `{"reason":"观察几天"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable code=%d body=%s", rec.Code, rec.Body)
	}
	if !out.ManualDisabled || !out.Changed {
		t.Fatalf("disable 响应 = %+v", out)
	}
	if out.ManualReason != "观察几天" {
		t.Errorf("manual_reason=%q", out.ManualReason)
	}
	if out.Disabled {
		t.Error("disable 不应置自动禁用位")
	}

	// 生效：不再被选中
	if got := p.Pick(""); got != nil {
		t.Fatalf("停用后仍被选中: %+v", got)
	}
	// 状态位可读（仍在池里）
	st, ok := p.Status("u1")
	if !ok || !st.ManualDisabled {
		t.Fatalf("状态应可读且 manual_disabled=true: %+v ok=%v", st, ok)
	}

	// 恢复
	_, out = doAdmin(t, h, "POST", "/admin/accounts/u1/enable", "")
	if out.ManualDisabled || !out.Changed {
		t.Fatalf("enable 响应 = %+v", out)
	}
	if got := p.Pick(""); got == nil || got.UID != "u1" {
		t.Fatalf("恢复后应可选, got %+v", got)
	}
}

// TestAdminDisableWithoutBody 无请求体是最常见的调用形态（curl / CLI），必须可用。
func TestAdminDisableWithoutBody(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, AdminEnabled: true})

	rec, out := doAdmin(t, h, "POST", "/admin/accounts/u1/disable", "")
	if rec.Code != http.StatusOK || !out.ManualDisabled {
		t.Fatalf("code=%d out=%+v", rec.Code, out)
	}
	// 默认原因文案
	if out.ManualReason != "manual" {
		t.Errorf("无 reason 时 default=%q, want manual", out.ManualReason)
	}
}

// TestAdminIdempotent 面板重试场景：重复调用不报错，第二次 changed=false。
func TestAdminIdempotent(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, AdminEnabled: true})

	_, first := doAdmin(t, h, "POST", "/admin/accounts/u1/disable", `{"reason":"r"}`)
	if !first.Changed {
		t.Fatal("首次应 changed=true")
	}
	rec, second := doAdmin(t, h, "POST", "/admin/accounts/u1/disable", `{"reason":"r"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("重复调用应 200（幂等）, got %d", rec.Code)
	}
	if second.Changed || !second.ManualDisabled {
		t.Fatalf("重复停用 = %+v, want changed=false manual_disabled=true", second)
	}
}

// TestAdminOrthogonalSemantics enable 不解除自动禁用；revive 不解除手动停用。
func TestAdminOrthogonalSemantics(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, AdminEnabled: true})

	// 造出自动禁用
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatal("precondition: 应已自动禁用")
	}

	// enable（解手动位）对自动禁用无效：账号仍不可选，但响应要如实透出 disabled
	_, out := doAdmin(t, h, "POST", "/admin/accounts/u1/enable", "")
	if !out.Disabled {
		t.Error("enable 后响应应如实透出 disabled=true（面板据此提示还需 revive）")
	}
	if got := p.Pick(""); got != nil {
		t.Fatalf("自动禁用仍在，不应可选, got %+v", got)
	}

	// revive 解自动禁用
	_, out = doAdmin(t, h, "POST", "/admin/accounts/u1/revive", "")
	if out.Disabled || !out.Changed {
		t.Fatalf("revive 响应 = %+v", out)
	}
	if got := p.Pick(""); got == nil {
		t.Fatal("revive 后应回池")
	}

	// 反过来：revive 不解除手动停用
	p.SetManualDisabled("u1", true, "运维摘除")
	_, out = doAdmin(t, h, "POST", "/admin/accounts/u1/revive", "")
	if !out.ManualDisabled {
		t.Error("revive 不应解除手动停用")
	}
	if got := p.Pick(""); got != nil {
		t.Fatalf("手动位仍在，不应可选, got %+v", got)
	}
}

// TestAdminUnknownUID uid 不存在 → 404（三个端点一致）。
func TestAdminUnknownUID(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, AdminEnabled: true})

	for _, op := range []string{"disable", "enable", "revive"} {
		rec, _ := doAdmin(t, h, "POST", "/admin/accounts/nope/"+op, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s unknown uid: code=%d want 404", op, rec.Code)
		}
		assertJSONErrorCode(t, rec.Body.String(), "not_found")
	}
}

// TestAdminStatusExposesManualDisabled /status 透出 manual_disabled 双位状态。
func TestAdminStatusExposesManualDisabled(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, AdminEnabled: true})
	p.SetManualDisabled("u1", true, "面板摘除")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code=%d", rec.Code)
	}
	var body struct {
		Accounts []adminResp `json:"accounts"`
		Disabled int         `json:"disabled"`
		Healthy  int         `json:"healthy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if len(body.Accounts) != 1 {
		t.Fatalf("accounts=%d", len(body.Accounts))
	}
	a := body.Accounts[0]
	if !a.ManualDisabled {
		t.Error("status 应透出 manual_disabled=true")
	}
	if a.Disabled {
		t.Error("手动停用不应同时置 disabled")
	}
	if a.ManualReason != "面板摘除" {
		t.Errorf("manual_reason=%q", a.ManualReason)
	}
	// 计数口径：手动停用归入 disabled（与自动禁用同类）
	if body.Disabled != 1 || body.Healthy != 0 {
		t.Errorf("counts disabled=%d healthy=%d, want 1/0", body.Disabled, body.Healthy)
	}
}
