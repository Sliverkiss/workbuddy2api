package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// ─── /v1/stats 倍率透出（issue #176）──────────────────────────────────────
//
// 与 /v1/models 同源（模型目录只读缓存），缺失≠免费：目录未下发 / 缓存冷 /
// 无匹配条目 → JSON 整体省略 credits 键，绝不输出 "x0.00"。

// warmCNCatalog 用 fullFieldsModelsBody（hy3 → "x0.05"）预热 CN 目录缓存：
// 一次 modelList 即拉取并落缓存（fetchDynamicModels 内部触发）。
func warmCNCatalog(t *testing.T, h *Handler) {
	t.Helper()
	got := h.modelList()
	found := false
	for _, m := range got {
		if id, ok := m["id"].(string); ok && id == "cn:hy3" {
			found = true
		}
	}
	if !found {
		t.Fatalf("预热失败：modelList 无 cn:hy3：%v", got)
	}
}

// findModelRow 从快照里取指定模型的载荷行。
func findModelRow(t *testing.T, snap MetricsSnapshot, model string) ModelStatPayload {
	t.Helper()
	for _, m := range snap.Models {
		if m.Model == model {
			return m
		}
	}
	t.Fatalf("快照缺模型 %q：%+v", model, snap.Models)
	return ModelStatPayload{}
}

// TestStatsCreditsEnrichedFromCNCatalog CN 目录命中：cn:hy3 行透出倍率原文
// "x0.05"；目录外模型 / total 行保持空（S1/S4）。
func TestStatsCreditsEnrichedFromCNCatalog(t *testing.T) {
	resetModelsCache()
	resetMetricsForTest(t)

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, fullFieldsModelsBody, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})
	warmCNCatalog(t, h)

	recordChatMetric(&chatStat{model: "cn:hy3", mode: "sync", status: 200}, time.Second)
	recordChatMetric(&chatStat{model: "cn:not-in-catalog", mode: "sync", status: 200}, time.Second)

	snap := MetricsSnapshotOf()
	h.enrichCredits(&snap)

	if got := findModelRow(t, snap, "cn:hy3").Credits; got != "x0.05" {
		t.Errorf("cn:hy3 credits = %q, want x0.05（目录命中，原文透出）", got)
	}
	if got := findModelRow(t, snap, "cn:not-in-catalog").Credits; got != "" {
		t.Errorf("cn:not-in-catalog credits = %q, want 空串（目录外模型省略）", got)
	}
	if snap.Total.Credits != "" {
		t.Errorf("total credits = %q, want 空串（跨倍率聚合无意义）", snap.Total.Credits)
	}
}

// TestStatsCreditsOmittedWhenCacheCold 目录缓存冷：credits 键整体不出现在
// JSON 里（缺失≠免费，不是 "x0.00" 也不是 ""），且零上游调用（S2/S3）。
func TestStatsCreditsOmittedWhenCacheCold(t *testing.T) {
	resetModelsCache()
	resetMetricsForTest(t)

	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, fullFieldsModelsBody, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})

	recordChatMetric(&chatStat{model: "cn:hy3", mode: "sync", status: 200}, time.Second)

	snap := MetricsSnapshotOf()
	h.enrichCredits(&snap)

	raw, err := json.Marshal(snap.Models[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"credits"`) {
		t.Errorf("冷缓存下 JSON 应整体省略 credits 键（缺失≠免费），得到 %s", raw)
	}
	if calls != 0 {
		t.Errorf("enrichCredits 发起 %d 次上游调用，want 0（只读快照，绝不探测）", calls)
	}
}

// TestStatsCreditsGlobalRealm global realm：global: 前缀键查 global 目录
// （v2 探测对象形态，fake 透传 fullFieldsModelsBody）→ 倍率命中（S5 global 路径）。
func TestStatsCreditsGlobalRealm(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()
	resetMetricsForTest(t)

	cf := newGlobalModelsHandlerFake(t, 200, fullFieldsModelsBody)
	p := testPoolWith(
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})
	// 预热 global 目录：一次 modelList 触发探测并落 Client 缓存。
	got := h.modelList()
	found := false
	for _, m := range got {
		if id, ok := m["id"].(string); ok && id == "global:hy3" {
			found = true
		}
	}
	if !found {
		t.Fatalf("预热失败：modelList 无 global:hy3：%v", got)
	}

	recordChatMetric(&chatStat{model: "global:hy3", mode: "sync", status: 200}, time.Second)

	snap := MetricsSnapshotOf()
	h.enrichCredits(&snap)

	if got := findModelRow(t, snap, "global:hy3").Credits; got != "x0.05" {
		t.Errorf("global:hy3 credits = %q, want x0.05（global 目录命中）", got)
	}
}

// TestStatsCreditsKeyNormalization 键归一：裸名（→cn realm）与 cn: 前缀同获倍率；
// "-" 与未知前缀查不到 → 空串（S5 边界形态）。
func TestStatsCreditsKeyNormalization(t *testing.T) {
	resetModelsCache()
	resetMetricsForTest(t)

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, fullFieldsModelsBody, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})
	warmCNCatalog(t, h)

	for _, key := range []string{"hy3", "cn:hy3", "-", "weird:hy3"} {
		recordChatMetric(&chatStat{model: key, mode: "sync", status: 200}, time.Second)
	}

	snap := MetricsSnapshotOf()
	h.enrichCredits(&snap)

	if got := findModelRow(t, snap, "hy3").Credits; got != "x0.05" {
		t.Errorf("裸名 hy3 credits = %q, want x0.05（裸名 → cn realm）", got)
	}
	if got := findModelRow(t, snap, "cn:hy3").Credits; got != "x0.05" {
		t.Errorf("cn:hy3 credits = %q, want x0.05", got)
	}
	if got := findModelRow(t, snap, "-").Credits; got != "" {
		t.Errorf("\"-\" credits = %q, want 空串（不查目录）", got)
	}
	if got := findModelRow(t, snap, "weird:hy3").Credits; got != "" {
		t.Errorf("weird:hy3 credits = %q, want 空串（未知前缀查不到）", got)
	}
}
