// static.go 内嵌 SPA 静态托管：go:embed dist（前端构建产物）。
// dist 仅提交占位（.gitkeep），真实产物由 Dockerfile 的 node 构建阶段生成。
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// spa 静态托管 + SPA fallback：非 /api 前缀的 GET，文件缺失一律回退 index.html。
func (h *Handler) spa() http.HandlerFunc {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// 理论不可达（embed 编译期保证）；兜底 503。
		return func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "web dist not embedded", http.StatusServiceUnavailable)
		}
	}
	fileServer := http.FileServer(http.FS(sub))
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		p := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if p == "." || p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			p = "index.html" // SPA fallback（hash 路由天然兼容直达刷新）
		}
		if p == "index.html" {
			// FileServer 会对 /index.html 301 到 ./ 造成重定向环，直读直出。
			raw, err := fs.ReadFile(sub, "index.html")
			if err != nil {
				http.Error(w, "index.html missing", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(raw)
			return
		}
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		r2 := new(http.Request)
		*r2 = *r
		r2.URL.Path = "/" + p
		fileServer.ServeHTTP(w, r2)
	}
}
