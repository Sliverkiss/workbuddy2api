// misc.go 日志查询 / 只读配置展示 / JSON body 解析。
package web

import (
	"encoding/json"
	"net/http"
	"strconv"
)

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}

// logs 任务事件查询：?kind=&limit=（kind 空 = 全部）。
func (h *Handler) logs(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 200
	}
	evs := h.st.events(kind, limit)
	if evs == nil {
		evs = []Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": evs})
}

// getConfig 只读配置展示（网关 config.json 容器内只读挂载，在线编辑无意义）。
func (h *Handler) getConfig(w http.ResponseWriter, r *http.Request) {
	_, dirExists := scanAuthDir(h.cfg.AuthDir)
	s := h.cfg.Schedule
	writeJSON(w, http.StatusOK, map[string]any{
		"authDir":   h.cfg.AuthDir,
		"dirExists": dirExists,
		"version":   h.cfg.Version,
		"apiKeyRequired": h.cfg.APIKey != "",
		"schedule": map[string]any{
			"checkin_enabled":       s.CheckinEnabled,
			"checkin_hours":         s.CheckinHours,
			"keepalive_enabled":     s.KeepaliveEnabled,
			"keepalive_hours":       s.KeepaliveHours,
			"travel_enabled":        s.TravelEnabled,
			"travel_hours":          s.TravelHours,
			"activity_enabled":      s.ActivityEnabled,
			"activity_hours":        s.ActivityHours,
			"activity_report_count": s.ActivityReportCount,
		},
	})
}
