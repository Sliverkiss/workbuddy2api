// device_token_test.go X-Device-Token 注入 + 文件兜底读取单测。
package upstream

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestDeviceTokenInjected_WhenSet auth.Auth.DeviceToken 非空时 chat/billing 请求均注入。
func TestDeviceTokenInjected_WhenSet(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1", DeviceToken: "tok-from-auth"}
	// 用 server 端验证而非 RoundTripper 捕获：更贴近真实注入路径。
	for _, tc := range []struct {
		name      string
		apply     func(c *Client, req *http.Request)
		wantPath  string
	}{
		{"chat", func(c *Client, req *http.Request) { c.ChatHeaders(req, a, "") }, "/v2/chat/completions"},
		{"billing", func(c *Client, req *http.Request) { c.BillingHeaders(req, a) }, "/v2/report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("X-Device-Token")
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"code":0}`))
			}))
			defer srv.Close()
			c := &Client{
				HTTP:          srv.Client(),
				ChatHTTP:      srv.Client(),
				ChatBaseCN:    srv.URL,
				BillingBaseCN: srv.URL,
			}
			req, _ := http.NewRequest(http.MethodPost, srv.URL+tc.wantPath, nil)
			tc.apply(c, req)
			resp, err := c.HTTP.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			resp.Body.Close()
			if got != "tok-from-auth" {
				t.Errorf("X-Device-Token = %q want %q", got, "tok-from-auth")
			}
		})
	}
}

// TestDeviceTokenNotInjected_WhenEmpty 所有来源皆空时不注入该头。
func TestDeviceTokenNotInjected_WhenEmpty(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"} // DeviceToken 空
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Device-Token")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()
	c := &Client{
		HTTP:          srv.Client(),
		ChatHTTP:      srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
		// DeviceToken / DeviceTokenFile 皆空
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if got != "" {
		t.Errorf("X-Device-Token = %q want empty (not injected)", got)
	}
}

// TestDeviceTokenFromConfigOrFile_Overrides 优先级：auth > config > 文件。
// auth 有值时覆盖 config；auth 空时 config 兜底；auth 与 config 皆空时读文件。
func TestDeviceTokenFromConfigOrFile_Overrides(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "device_token")
	if err := os.WriteFile(fp, []byte("tok-from-file\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	cases := []struct {
		name    string
		auth    string
		cfg     string
		want    string
	}{
		{"auth_over_config", "tok-auth", "tok-config", "tok-auth"},
		{"config_when_auth_empty", "", "tok-config", "tok-config"},
		{"file_when_auth_and_config_empty", "", "", "tok-from-file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 每个用例用独立缓存：device token 文件缓存 5 分钟，case 间会串扰。
			save := resetDeviceTokenFileCache(fp)
			defer save()
			a := &auth.Auth{AccessToken: "at", UID: "u1", DeviceToken: tc.auth}
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("X-Device-Token")
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"code":0}`))
			}))
			defer srv.Close()
			c := &Client{
				HTTP:           srv.Client(),
				ChatHTTP:       srv.Client(),
				ChatBaseCN:     srv.URL,
				BillingBaseCN:  srv.URL,
				DeviceToken:    tc.cfg,
				DeviceTokenFile: fp,
			}
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v2/chat/completions", nil)
			c.ChatHeaders(req, a, "")
			resp, err := c.HTTP.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			resp.Body.Close()
			if got != tc.want {
				t.Errorf("X-Device-Token = %q want %q", got, tc.want)
			}
		})
	}
}

// TestDeviceTokenFileTooLarge 文件超过 1KB 时忽略不注入。
func TestDeviceTokenFileTooLarge(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "device_token")
	if err := os.WriteFile(fp, []byte(strings.Repeat("x", 2048)), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	save := resetDeviceTokenFileCache(fp)
	defer save()
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{DeviceTokenFile: fp}
	if tok := c.resolveDeviceToken(a); tok != "" {
		t.Errorf("resolveDeviceToken() = %q want empty (file too large)", tok)
	}
}

// resetDeviceTokenFileCache 替换全局 device token 文件缓存并返回恢复函数。
// 文件缓存 5 分钟 TTL，测试间需清空避免串扰。
func resetDeviceTokenFileCache(path string) (restore func()) {
	dtFileCache.mu.Lock()
	origPath := dtFileCache.path
	origTok := dtFileCache.token
	origRead := dtFileCache.readAt
	origErr := dtFileCache.lastErr
	dtFileCache.path = path
	dtFileCache.token = ""
	dtFileCache.readAt = time.Time{}
	dtFileCache.lastErr = nil
	dtFileCache.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			dtFileCache.mu.Lock()
			dtFileCache.path = origPath
			dtFileCache.token = origTok
			dtFileCache.readAt = origRead
			dtFileCache.lastErr = origErr
			dtFileCache.mu.Unlock()
		})
	}
}
