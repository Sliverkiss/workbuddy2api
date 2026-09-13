// Package web Workbuddy 账号池 Web 管理台：/api/* JSON 接口 + 内嵌 SPA 静态托管。
// 全部功能委托网关自身服务（pool / upstream / scheduler 同源上游客户端），
// 不引入任何外部进程依赖（无 Node、无 docker logs 采集）。
package web

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/config"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/upstream"
)

// Config 管理台依赖。
type Config struct {
	AuthDir  string
	StateDir string // web-state.json 所在目录（取 gateway state_file 的目录）
	Pool     *pool.Pool
	Upstream *upstream.Client
	APIKey   string // 与网关 /v1 同一把 key；空 = /api 不鉴权
	// LoopbackBase 网关自身回环地址（chat-test/models/status 复用真实路由），
	// 如 http://127.0.0.1:7863。
	LoopbackBase string
	Schedule     config.Schedule
	Version      string
}

// Handler 管理台路由（/api/* + SPA）。
type Handler struct {
	cfg Config
	mux *http.ServeMux

	st          *webState
	reqRows     *ring[ReqRow]
	stdoutLines *ring[StdoutLine]

	probeMu sync.Mutex // 串行化探测与任务（上游限速语义与 Node 版 withLock 一致）

	oauthMu       sync.Mutex
	oauthSessions map[string]*oauthSession
}

// NewHandler 构建并注册全部 /api 路由与 SPA 托管。
func NewHandler(cfg Config) *Handler {
	h := &Handler{
		cfg:           cfg,
		mux:           http.NewServeMux(),
		st:            loadState(cfg.StateDir + "/web-state.json"),
		reqRows:       newRing[ReqRow](500),
		stdoutLines:   newRing[StdoutLine](300),
		oauthSessions: map[string]*oauthSession{},
	}
	// 账号与探测
	h.mux.HandleFunc("GET /api/accounts", h.api(h.accounts))
	h.mux.HandleFunc("DELETE /api/accounts/{uid}", h.api(h.deleteAccount))
	h.mux.HandleFunc("POST /api/accounts/{uid}/probe", h.api(h.probeOne))
	h.mux.HandleFunc("POST /api/accounts/{uid}/tasks/{kind}", h.api(h.runTaskOne))
	h.mux.HandleFunc("POST /api/probe/all", h.api(h.probeAll))
	// 任务与日志
	h.mux.HandleFunc("POST /api/tasks/{kind}/run", h.api(h.runTask))
	h.mux.HandleFunc("GET /api/logs", h.api(h.logs))
	// 配置与统计
	h.mux.HandleFunc("GET /api/config", h.api(h.getConfig))
	h.mux.HandleFunc("GET /api/stats/overview", h.api(h.statsOverview))
	// OAuth 设备登录
	h.mux.HandleFunc("POST /api/oauth/begin", h.api(h.oauthBegin))
	h.mux.HandleFunc("POST /api/oauth/poll", h.api(h.oauthPoll))
	h.mux.HandleFunc("POST /api/oauth/cancel", h.api(h.oauthCancel))
	// 网关观测（同源回环 + 环形缓冲）
	h.mux.HandleFunc("GET /api/gateway/health", h.api(h.gwHealth))
	h.mux.HandleFunc("GET /api/gateway/status", h.api(h.gwStatus))
	h.mux.HandleFunc("GET /api/gateway/models", h.api(h.gwModels))
	h.mux.HandleFunc("POST /api/gateway/chat-test", h.api(h.gwChatTest))
	h.mux.HandleFunc("GET /api/gateway/request-logs", h.api(h.gwRequestLogs))
	h.mux.HandleFunc("GET /api/gateway/stdout", h.api(h.gwStdout))
	// SPA 兜底（非 /api 的 GET 一律回退 index.html）
	h.mux.HandleFunc("GET /", h.spa())
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// api 包装：/api/* 统一鉴权（与网关 /v1 同一把 Bearer key）。
func (h *Handler) api(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") || strings.TrimPrefix(authz, "Bearer ") != h.cfg.APIKey {
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error": map[string]any{"message": "missing or invalid API key", "type": "api_error", "code": "invalid_api_key"},
				})
				return
			}
		}
		next(w, r)
	}
}

// ---- 订阅入口（main.go 接线）----

// AttachRequestRows 订阅 server 包请求级表格日志（每请求一行结构化记录）。
// 哨兵值转换：TTFBms<=0 / Tokens<0 / TokPerSec<0 → nil（Node 版 null 语义）。
func (h *Handler) AttachRequestRows() {
	server.SetChatRowHook(func(row server.ChatRow) {
		var ttfbPtr *int64
		if row.TTFBms > 0 {
			v := row.TTFBms
			ttfbPtr = &v
		}
		var tokPtr *int
		if row.Tokens >= 0 {
			v := row.Tokens
			tokPtr = &v
		}
		var tpsPtr *float64
		if row.TokPerSec >= 0 {
			v := math.Round(row.TokPerSec*10) / 10 // 与表格行 1 位小数口径一致
			tpsPtr = &v
		}
		h.reqRows.add(ReqRow{
			Ts:        time.Now().UnixMilli(),
			Seq:       row.Seq,
			Clock:     row.Clock,
			Model:     row.Model,
			Mode:      row.Mode,
			Status:    row.Status,
			UID:       row.UID,
			TTFBms:    ttfbPtr,
			Tokens:    tokPtr,
			TokPerSec: tpsPtr,
			TotalSec:  row.TotalSec,
			Raw:       row.Raw,
		})
	})
}

// StdoutSink 返回 io.Writer：main.go 用 log.SetOutput(io.MultiWriter(os.Stderr, sink))
// 把进程 stderr 日志（启动/调度器/任务行）镜像进环形缓冲，供"网关 stdout"标签页。
func (h *Handler) StdoutSink() *StdoutSink {
	return &StdoutSink{h: h}
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func nowMs() int64 { return time.Now().UnixMilli() }

// uid8 展示用 uid 截断（与网关表格日志同口径）。
func uid8(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}
