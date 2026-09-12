// accounts.go 账号目录扫描（含损坏文件）/ 账号视图组装 / 删除凭证。
package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// dirEntry 目录扫描条目；loadError 非空表示文件损坏（auth 为 nil）。
type dirEntry struct {
	file      string // 完整路径
	base      string
	auth      *auth.Auth
	loadError string
	mtimeMs   int64
}

// scanAuthDir 扫描 workbuddy*.json；BOM 容错（Windows 编辑器产物）。
func scanAuthDir(dir string) (entries []dirEntry, dirExists bool) {
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil {
		return nil, false
	}
	st, err := os.Stat(dir)
	dirExists = err == nil && st.IsDir()
	for _, f := range files {
		e := dirEntry{file: f, base: filepath.Base(f)}
		if fi, err := os.Stat(f); err == nil {
			e.mtimeMs = fi.ModTime().UnixMilli()
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			e.loadError = err.Error()
			entries = append(entries, e)
			continue
		}
		raw = []byte(strings.TrimPrefix(string(raw), "\xef\xbb\xbf")) // BOM strip
		a, err := auth.Parse(raw)
		if err != nil {
			e.loadError = err.Error()
			entries = append(entries, e)
			continue
		}
		a.FilePath = f
		e.auth = a
		entries = append(entries, e)
	}
	// 稳定排序：昵称 → 文件名
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			ni, nj := entries[i].base, entries[j].base
			if entries[i].auth != nil && entries[i].auth.Nickname != "" {
				ni = entries[i].auth.Nickname
			}
			if entries[j].auth != nil && entries[j].auth.Nickname != "" {
				nj = entries[j].auth.Nickname
			}
			if nj < ni {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
	return entries, dirExists
}

// findAuth 按完整 uid（或 uid8 前缀）找账号。
func findAuth(entries []dirEntry, uid string) *dirEntry {
	for i := range entries {
		if entries[i].auth != nil && entries[i].auth.UID == uid {
			return &entries[i]
		}
	}
	for i := range entries {
		if entries[i].auth != nil && uid8(entries[i].auth.UID) == uid {
			return &entries[i]
		}
	}
	return nil
}

// accountView 单账号对外视图（字段名与 Node 版逐字一致）。
func (h *Handler) accountView(e *dirEntry) map[string]any {
	v := map[string]any{
		"file":      e.base,
		"loadError": nil,
	}
	if e.loadError != "" {
		v["uid"] = nil
		v["nickname"] = filepath.Base(e.base)
		v["loadError"] = e.loadError
		v["status"] = "error"
		v["disabled"] = false
		v["expired"] = false
		v["liveStatus"] = nil
		v["credits"] = nil
		v["lastCredit"] = nil
		v["lastRefreshMs"] = e.mtimeMs
		return v
	}
	a := e.auth
	// 池状态
	status := "ok"
	disabled := false
	if st, ok := h.cfg.Pool.Status(a.UID); ok {
		if st.Disabled {
			status = "disabled"
			disabled = true
		} else if st.Cooling {
			status = "cooling"
		}
	}
	// token 过期
	nowSec := time.Now().Unix()
	expired := a.ExpiresAt > 0 && a.ExpiresAt <= nowSec
	var expiresInSec any
	if a.ExpiresAt > 0 {
		expiresInSec = a.ExpiresAt - nowSec
	}
	// 上一次刷新：探测/任务标记与文件 mtime 取大
	lastRefresh := e.mtimeMs
	if mark := h.st.refreshMark(a.UID); mark > lastRefresh {
		lastRefresh = mark
	}
	// 积分缓存（探测富版优先；池内余额兜底 remain）
	var credits map[string]any
	source := any(nil)
	sessionDeadFails := 0
	ls := h.st.liveStatus(a.UID)
	if ls != nil && ls.Credits != nil {
		credits = map[string]any{"total": ls.Credits.Total, "used": ls.Credits.Used, "remain": ls.Credits.Remain}
		source = ls.Credits.Source
	} else if st, ok := h.cfg.Pool.Status(a.UID); ok && st.Credits > 0 {
		credits = map[string]any{"total": st.Credits, "used": 0, "remain": st.Credits}
	}
	if st, ok := h.cfg.Pool.Status(a.UID); ok {
		sessionDeadFails = st.SessionDeadFails
	}
	var live any
	if ls != nil {
		live = ls
	}
	v["uid"] = a.UID
	v["nickname"] = a.Nickname
	v["status"] = status
	v["disabled"] = disabled
	v["expired"] = expired
	v["expiredAtMs"] = a.ExpiresAt * 1000
	v["expiresInSec"] = expiresInSec
	v["sessionDeadFails"] = sessionDeadFails
	v["lastRefreshMs"] = lastRefresh
	v["lastCredit"] = credits
	v["credits"] = credits
	v["source"] = source
	v["liveStatus"] = live
	return v
}

func (h *Handler) accounts(w http.ResponseWriter, r *http.Request) {
	entries, dirExists := scanAuthDir(h.cfg.AuthDir)
	views := make([]map[string]any, 0, len(entries))
	for i := range entries {
		views = append(views, h.accountView(&entries[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authDir":   h.cfg.AuthDir,
		"dirExists": dirExists,
		"accounts":  views,
	})
}

func (h *Handler) deleteAccount(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	entries, _ := scanAuthDir(h.cfg.AuthDir)
	e := findAuth(entries, uid)
	if e == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	if err := os.Remove(e.file); err != nil {
		writeErr(w, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	// 重扫目录对齐池（已删除文件账号剔除，状态保留）
	if auths, err := auth.LoadDir(h.cfg.AuthDir); err == nil {
		h.cfg.Pool.SyncToDir(auths)
	}
	h.st.addEvent(Event{Ts: nowMs(), Kind: "delete", UID: e.auth.UID, Nickname: e.auth.Nickname, Status: "ok", Msg: "删除凭证 " + e.base})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
