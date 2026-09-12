// stats.go 概览统计聚合：Token（请求行环）+ 积分（探测缓存 + 快照）。
// 移植自 Workbuddy-Web/server/stats.mjs，口径逐字一致。
package web

import (
	"math"
	"net/http"
	"sort"
	"time"

	"workbuddy2api/internal/upstream"
)

func dayStartMs(now time.Time) int64 {
	d := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return d.UnixMilli()
}

func round2s(n float64) float64 { return math.Round(n*100) / 100 }

func (h *Handler) statsOverview(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	nowMsV := now.UnixMilli()
	todayStart := dayStartMs(now)
	entries, _ := scanAuthDir(h.cfg.AuthDir)
	nickOf := map[string]string{}
	for i := range entries {
		if entries[i].auth != nil {
			nickOf[entries[i].auth.UID] = entries[i].auth.Nickname
		}
	}

	// ---- Token 统计（请求行环）----
	rows := h.reqRows.list(0)
	type agg struct {
		Key      string `json:"key"`
		Requests int    `json:"requests"`
		Tokens   int    `json:"tokens"`
	}
	modelMap := map[string]*agg{}
	acctMap := map[string]*agg{}
	var outputTokens, todayReq, todayTok int
	var rateSum float64
	var rateN int
	for _, row := range rows {
		tok := 0
		if row.Tokens != nil {
			tok = *row.Tokens
		}
		outputTokens += tok
		if row.TokPerSec != nil {
			rateSum += *row.TokPerSec
			rateN++
		}
		if row.Ts >= todayStart {
			todayReq++
			todayTok += tok
		}
		mk := row.Model
		if mk == "" {
			mk = "unknown"
		}
		m := modelMap[mk]
		if m == nil {
			m = &agg{Key: mk}
			modelMap[mk] = m
		}
		m.Requests++
		m.Tokens += tok
		if row.UID != "" {
			a := acctMap[row.UID]
			if a == nil {
				a = &agg{Key: row.UID}
				acctMap[row.UID] = a
			}
			a.Requests++
			a.Tokens += tok
		}
	}
	sortAggs := func(m map[string]*agg) []*agg {
		out := make([]*agg, 0, len(m))
		for _, v := range m {
			out = append(out, v)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Tokens > out[j].Tokens })
		if len(out) > 6 {
			out = out[:6]
		}
		return out
	}
	var avgRate any
	if rateN > 0 {
		avgRate = math.Round(rateSum/float64(rateN)*10) / 10
	}
	byAccount := []map[string]any{}
	for _, a := range sortAggs(acctMap) {
		byAccount = append(byAccount, map[string]any{
			"key": a.Key, "requests": a.Requests, "tokens": a.Tokens, "nickname": nickOf[a.Key],
		})
	}

	// ---- 积分统计（探测缓存 + 快照）----
	liveAll := h.st.allLiveStatus()
	var total, used, remain float64
	probed := 0
	var expiringAmount float64
	var soonestAt int64
	creditAccounts := []map[string]any{}
	for uid, ls := range liveAll {
		if ls == nil || ls.Credits == nil {
			continue
		}
		probed++
		total += ls.Credits.Total
		used += ls.Credits.Used
		remain += ls.Credits.Remain
		creditAccounts = append(creditAccounts, map[string]any{
			"uid": uid, "nickname": nickOf[uid],
			"remain": ls.Credits.Remain, "total": ls.Credits.Total, "used": ls.Credits.Used,
		})
		for _, p := range ls.Credits.Packages {
			if !p.Expired && p.ExpireAtMs > 0 && p.Remaining > 0 && p.ExpireAtMs-nowMsV <= upstream.ExpiringSoonMs {
				expiringAmount += p.Remaining
				if soonestAt == 0 || p.ExpireAtMs < soonestAt {
					soonestAt = p.ExpireAtMs
				}
			}
		}
	}
	sort.Slice(creditAccounts, func(i, j int) bool {
		return creditAccounts[i]["remain"].(float64) > creditAccounts[j]["remain"].(float64)
	})
	if len(creditAccounts) > 6 {
		creditAccounts = creditAccounts[:6]
	}

	// 今日变化：各账号「今日起始余额 − 最新余额」求和（负值为净增）
	var todayDelta float64
	hasToday := false
	for _, arr := range h.st.snapshots() {
		if len(arr) == 0 {
			continue
		}
		firstIdx := -1
		for i, s := range arr {
			if s.Ts >= todayStart {
				firstIdx = i
				break
			}
		}
		if firstIdx < 0 {
			continue
		}
		hasToday = true
		base := arr[firstIdx].Remain
		if firstIdx > 0 {
			base = arr[firstIdx-1].Remain
		}
		todayDelta += arr[len(arr)-1].Remain - base
	}
	var todayDeltaV any
	if hasToday {
		todayDeltaV = round2s(todayDelta)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"generatedAt": nowMsV,
		"tokens": map[string]any{
			"requests":     len(rows),
			"outputTokens": outputTokens,
			"avgTokPerSec": avgRate,
			"today":        map[string]any{"requests": todayReq, "outputTokens": todayTok},
			"byModel":      sortAggs(modelMap),
			"byAccount":    byAccount,
		},
		"credits": map[string]any{
			"probed":  probed,
			"total":   round2s(total),
			"used":    round2s(used),
			"remain":  round2s(remain),
			"expiring7d": map[string]any{
				"amount": round2s(expiringAmount), "soonestAt": soonestAt,
			},
			"todayDelta": todayDeltaV,
			"accounts":   creditAccounts,
		},
	})
}
