// Package credentials manages WorkBuddy OAuth credentials at runtime.
package credentials

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

const (
	defaultBase    = "https://copilot.tencent.com"
	clientUA       = "CLI/2.63.2 CodeBuddy/2.63.2"
	originReferer  = "https://www.codebuddy.cn"
	flowTTL        = 15 * time.Minute
	requestTimeout = 30 * time.Second
)

var (
	ErrPending  = errors.New("login pending")
	ErrNotFound = errors.New("login flow not found")
	validUID    = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

type Config struct {
	AuthDir      string
	Pool         *pool.Pool
	UpstreamBase string
}

type Manager struct {
	cfg   Config
	mu    sync.Mutex
	flows map[string]*loginFlow
}

type loginFlow struct {
	mu      sync.Mutex
	state   string
	client  *http.Client
	created time.Time
}

type LoginStart struct {
	ID      string `json:"id"`
	AuthURL string `json:"auth_url"`
}

func New(cfg Config) *Manager {
	if cfg.UpstreamBase == "" {
		cfg.UpstreamBase = defaultBase
	}
	return &Manager{cfg: cfg, flows: make(map[string]*loginFlow)}
}

func (m *Manager) Start(ctx context.Context) (LoginStart, error) {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: requestTimeout, Jar: jar}
	data, status, err := m.doJSON(ctx, client, http.MethodPost, "/v2/plugin/auth/state?platform=CLI", nil, bytes.NewReader([]byte("{}")))
	if err != nil {
		return LoginStart{}, fmt.Errorf("auth state (%d): %w", status, err)
	}
	var result struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if json.Unmarshal(data, &result) != nil || result.State == "" || result.AuthURL == "" {
		return LoginStart{}, errors.New("auth state response missing state or authUrl")
	}
	id, err := randomID()
	if err != nil {
		return LoginStart{}, err
	}
	m.mu.Lock()
	m.gcLocked(time.Now())
	m.flows[id] = &loginFlow{state: result.State, client: client, created: time.Now()}
	m.mu.Unlock()
	return LoginStart{ID: id, AuthURL: result.AuthURL}, nil
}

func (m *Manager) Poll(ctx context.Context, id string) (*auth.Auth, error) {
	m.mu.Lock()
	m.gcLocked(time.Now())
	flow := m.flows[id]
	m.mu.Unlock()
	if flow == nil {
		return nil, ErrNotFound
	}
	flow.mu.Lock()
	defer flow.mu.Unlock()

	escapedState := url.QueryEscape(flow.state)
	tokRaw, status, err := m.doJSON(ctx, flow.client, http.MethodGet, "/v2/plugin/auth/token?state="+escapedState, nil, nil)
	if err != nil {
		if status > 0 && status < 500 {
			return nil, ErrPending
		}
		return nil, fmt.Errorf("token endpoint: %w", err)
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if json.Unmarshal(tokRaw, &tok) != nil || tok.AccessToken == "" {
		return nil, ErrPending
	}

	var account struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	headers := func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+tok.AccessToken) }
	acctRaw, _, err := m.doJSON(ctx, flow.client, http.MethodGet, "/v2/plugin/login/account?state="+escapedState, headers, nil)
	if err != nil || json.Unmarshal(acctRaw, &account) != nil || !validUID.MatchString(account.UID) {
		return nil, errors.New("login account response missing a safe uid")
	}

	a := &auth.Auth{
		AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken,
		ExpiresAt: time.Now().Unix() + tok.ExpiresIn, Domain: tok.Domain,
		UID: account.UID, EnterpriseID: account.EnterpriseID, Nickname: account.Nickname,
		FilePath: filepath.Join(m.cfg.AuthDir, "workbuddy-"+account.UID+".json"),
	}
	if err := os.MkdirAll(m.cfg.AuthDir, 0o700); err != nil {
		return nil, err
	}
	if err := a.SaveAtomic(); err != nil {
		return nil, fmt.Errorf("save credential: %w", err)
	}
	if err := m.reload(); err != nil {
		return nil, err
	}
	m.cfg.Pool.Reactivate(a.UID)
	m.mu.Lock()
	delete(m.flows, id)
	m.mu.Unlock()
	return a, nil
}

func (m *Manager) Delete(uid string) error {
	auths, err := auth.LoadDir(m.cfg.AuthDir)
	if err != nil {
		return err
	}
	for _, a := range auths {
		if a.UID != uid {
			continue
		}
		if err := os.Remove(a.FilePath); err != nil {
			return err
		}
		return m.reload()
	}
	return os.ErrNotExist
}

func (m *Manager) reload() error {
	auths, err := auth.LoadDir(m.cfg.AuthDir)
	if err != nil {
		return err
	}
	m.cfg.Pool.SyncToDir(auths)
	return nil
}

func (m *Manager) doJSON(ctx context.Context, client *http.Client, method, path string, extraHeaders func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, m.cfg.UpstreamBase+path, body)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", originReferer)
	req.Header.Set("Referer", originReferer+"/")
	req.Header.Set("User-Agent", clientUA)
	if extraHeaders != nil {
		extraHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("upstream http %d", resp.StatusCode)
	}
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, err
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

func (m *Manager) gcLocked(now time.Time) {
	for id, flow := range m.flows {
		if now.Sub(flow.created) > flowTTL {
			delete(m.flows, id)
		}
	}
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
