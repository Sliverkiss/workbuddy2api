// gateway.go 网关观测接口：健康 / 池状态 / 模型 / 对话测试 / 请求行 / stdout 行。
// chat-test 与 models 走同源回环（复用真实 /v1 路由，零逻辑复制）。
package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/server"
)

// ReqRow 请求级表格日志行（JSON 与 Node 版 requestLogs 行契约一致）。
type ReqRow struct {
	Ts        int64    `json:"ts"`
	Seq       int64    `json:"seq"`
	Clock     string   `json:"clock"`
	Model     string   `json:"model"`
	Mode      string   `json:"mode"`
	Status    int      `json:"status"`
	UID       string   `json:"uid"`
	TTFBms    *int64   `json:"ttfbMs"`
	Tokens    *int     `json:"tokens"`
	TokPerSec *float64 `json:"tokPerSec"`
	TotalSec  float64  `json:"totalSec"`
	Raw       string   `json:"raw"`
}

// StdoutLine 进程 stderr 日志行（log.Printf 镜像）。
type StdoutLine struct {
	Ts  int64  `json:"ts"`
	Raw string `json:"raw"`
}

// StdoutSink io.Writer：按行切分写入环形缓冲（跨 Write 的半行先缓存）。
type StdoutSink struct {
	h    *Handler
	mu   sync.Mutex
	pend string
}

func (s *StdoutSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.pend += string(p)
	for {
		i := strings.IndexByte(s.pend, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(s.pend[:i], "\r")
		s.pend = s.pend[i+1:]
		if line != "" {
			s.h.stdoutLines.add(StdoutLine{Ts: nowMs(), Raw: line})
		}
	}
	s.mu.Unlock()
	return len(p), nil
}

// loopback 带鉴权的同源回环请求。
func (h *Handler) loopback(method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.cfg.LoopbackBase+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if h.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.cfg.APIKey)
	}
	cli := &http.Client{Timeout: 130 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, nil
}

// gwHealth 健康检查。字段与前端侧栏契约对齐:reachable/isWorkbuddy2api/
// legacyCompatible/httpStatus(侧栏据此渲染 已连接/旧版/非本网关/不可达)。
func (h *Handler) gwHealth(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	status, raw, err := h.loopback(http.MethodGet, "/healthz", nil)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"reachable": false, "isWorkbuddy2api": false, "legacyCompatible": false,
			"httpStatus": 0, "error": err.Error(),
		})
		return
	}
	var hb struct {
		Healthy int    `json:"healthy"`
		Total   int    `json:"total"`
		Service string `json:"service"`
	}
	_ = json.Unmarshal(raw, &hb)
	isWB := hb.Service == server.ServiceName
	legacy := !isWB && status >= 200 && status < 300 // 旧版镜像 healthz 无 service 字段
	writeJSON(w, http.StatusOK, map[string]any{
		"reachable": true, "isWorkbuddy2api": isWB, "legacyCompatible": legacy,
		"httpStatus": status, "service": hb.Service,
		"healthy": hb.Healthy, "total": hb.Total, "latencyMs": time.Since(t0).Milliseconds(),
	})
}

// gwStatus 池状态（直接取 pool，与 /status 同字段，免回环）。
func (h *Handler) gwStatus(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	writeJSON(w, http.StatusOK, map[string]any{
		"status": map[string]any{
			"accounts":       h.cfg.Pool.List(),
			"total":          total,
			"healthy":        healthy,
			"cooling":        cooling,
			"disabled":       disabled,
			"in_flight_full": inFlightFull,
		},
	})
}

// gwModels 模型列表。前端契约 {ok, models:[{id,...}]}(Node 版同源),
// 将 /v1/models 的 OpenAI 信封 {object,data} 解包为 models。
func (h *Handler) gwModels(w http.ResponseWriter, r *http.Request) {
	status, raw, err := h.loopback(http.MethodGet, "/v1/models", nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if status != http.StatusOK {
		snippet := string(raw)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "models": []any{}, "error": snippet})
		return
	}
	var body struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "models": []any{}, "error": err.Error()})
		return
	}
	if body.Data == nil {
		body.Data = []json.RawMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": body.Data})
}

func (h *Handler) gwChatTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	if err := readJSON(r, &body); err != nil || body.Model == "" || body.Prompt == "" {
		writeErr(w, http.StatusBadRequest, "model 与 prompt 必填")
		return
	}
	t0 := time.Now()
	status, raw, err := h.loopback(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    body.Model,
		"messages": []map[string]string{{"role": "user", "content": body.Prompt}},
		"stream":   false,
	})
	ms := time.Since(t0).Milliseconds()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if status != http.StatusOK {
		snippet := string(raw)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("网关 %d: %s", status, snippet))
		return
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		writeErr(w, http.StatusBadGateway, "响应解析: "+err.Error())
		return
	}
	content := ""
	if len(resp.Choices) > 0 {
		content = resp.Choices[0].Message.Content
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "content": content, "usage": resp.Usage, "ms": ms, "model": body.Model,
	})
}

func (h *Handler) gwRequestLogs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 300
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	rows := h.reqRows.listFiltered(limit, func(row ReqRow) bool { return row.Seq > after })
	var cursor int64
	for _, row := range rows {
		if row.Seq > cursor {
			cursor = row.Seq
		}
	}
	if cursor == 0 {
		cursor = after
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"logs": rows, "cursor": cursor, "collecting": true, "containerState": "running", "error": nil,
	})
}

func (h *Handler) gwStdout(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 200
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"lines": h.stdoutLines.list(limit), "collecting": true,
	})
}
