package credentials

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

func TestLoginSaveHotReloadAndDelete(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/plugin/auth/state", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, map[string]any{"state": "state-1", "authUrl": "https://login.example/auth"})
	})
	mux.HandleFunc("GET /v2/plugin/auth/token", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, map[string]any{"accessToken": "access", "refreshToken": "refresh", "expiresIn": 3600, "domain": ""})
	})
	mux.HandleFunc("GET /v2/plugin/login/account", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access" {
			t.Fatalf("missing account bearer header")
		}
		writeEnvelope(w, map[string]any{"uid": "u-1", "nickname": "tester"})
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()

	dir := t.TempDir()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u-1", AccessToken: "old", ExpiresAt: 9999999999})
	p.Disable("u-1", "old session dead")
	m := New(Config{AuthDir: dir, Pool: p, UpstreamBase: upstream.URL})
	start, err := m.Start(context.Background())
	if err != nil || start.ID == "" || start.AuthURL == "" {
		t.Fatalf("Start()=%+v err=%v", start, err)
	}
	a, err := m.Poll(context.Background(), start.ID)
	if err != nil || a.UID != "u-1" {
		t.Fatalf("Poll()=%+v err=%v", a, err)
	}
	if _, err := os.Stat(a.FilePath); err != nil {
		t.Fatalf("credential not saved: %v", err)
	}
	if _, ok := p.Status("u-1"); !ok {
		t.Fatal("credential was not hot-loaded into pool")
	}
	if st, _ := p.Status("u-1"); st.Disabled {
		t.Fatal("fresh OAuth credential must reactivate a previously disabled account")
	}
	if err := m.Delete("u-1"); err != nil {
		t.Fatalf("Delete(): %v", err)
	}
	if _, ok := p.Status("u-1"); ok {
		t.Fatal("deleted credential remains in pool")
	}
}

func writeEnvelope(w http.ResponseWriter, data any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
}
