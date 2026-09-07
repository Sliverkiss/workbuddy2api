// login.go — WorkBuddy OAuth login (CN + GLOBAL realms).
//
// Based on upstream Sliverkiss/workbuddy2api cmd/login (CN only), extended with
// the GLOBAL realm (workbuddy.ai) — same /v2/plugin/* endpoints, different base:
//
//	login url [cn|global]   → POST {base}/v2/plugin/auth/state?platform=CLI
//	login poll [cn|global]  → GET {base}/v2/plugin/auth/token?state=
//
// Global base/origin per Maquer/workbuddy-checkin login.sh:
//   GLOBAL_AUTH_BASE = https://www.workbuddy.ai (same path layout as CN)
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"time"
)

const (
	upstreamBaseCN     = "https://copilot.tencent.com"
	upstreamBaseGlobal = "https://www.workbuddy.ai"
	clientUA           = "CLI/2.63.2 CodeBuddy/2.63.2"
	originCN           = "https://www.codebuddy.cn"
	originGlobal       = "https://www.workbuddy.ai"

	stateFileCN     = "C:/Users/Administrator/Desktop/Mod/workbuddy2api/.wb2api-login-state-cn.json"
	stateFileGlobal = "C:/Users/Administrator/Desktop/Mod/workbuddy2api/.wb2api-login-state-global.json"
)

func bases(region string) (base, origin, stateFile string) {
	if region == "global" {
		return upstreamBaseGlobal, originGlobal, stateFileGlobal
	}
	return upstreamBaseCN, originCN, stateFileCN
}

func commonHeaders(origin string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		req.Header.Set("User-Agent", clientUA)
	}
}

type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	os.Exit(1)
}

type loginState struct {
	State string `json:"state"`
}

func main() {
	if len(os.Args) < 2 {
		fatal("usage: login <url|poll> [cn|global] (default global)")
	}
	sub := os.Args[1]
	region := "global"
	if len(os.Args) >= 3 {
		region = os.Args[2]
	}
	base, origin, stateFile := bases(region)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}

	switch sub {
	case "url":
		h := commonHeaders(origin)
		data, _, err := doJSON(client, http.MethodPost, base+"/v2/plugin/auth/state?platform=CLI", h, bytes.NewReader([]byte("{}")))
		if err != nil {
			fatal("auth state failed: %v", err)
		}
		var st struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		}
		if err := json.Unmarshal(data, &st); err != nil || st.State == "" {
			fatal("auth state: missing state (authUrl may be region-local login page)")
		}
		raw, _ := json.Marshal(loginState{State: st.State})
		if err := os.WriteFile(stateFile, raw, 0o600); err != nil {
			fatal("write state: %v", err)
		}
		// Upstream returns authUrl for CN sometimes empty on global; build fallback.
		url := st.AuthURL
		if url == "" {
			url = base + "/login?state=" + st.State + "&platform=CLI"
		}
		fmt.Println(url)

	case "poll":
		raw, err := os.ReadFile(stateFile)
		if err != nil {
			fatal("read state: %v (run `login url %s` first)", err, region)
		}
		var ls loginState
		if err := json.Unmarshal(raw, &ls); err != nil {
			fatal("parse state: %v", err)
		}
		h := commonHeaders(origin)
		tokRaw, status, errTok := doJSON(client, http.MethodGet, base+"/v2/plugin/auth/token?state="+ls.State, h, nil)
		if errTok != nil {
			if status == 0 || status >= 500 {
				fatal("token endpoint error: %v", errTok)
			}
			fatal("login not completed yet. Finish browser login first, then re-run `login poll %s`", region)
		}
		var tok struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			Domain       string `json:"domain"`
		}
		if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
			fatal("login not completed yet. Finish browser login first, then re-run `login poll %s`", region)
		}
		acctHeaders := func(r *http.Request) {
			h(r)
			r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		}
		var acct struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		}
		if acctRaw, _, errAcct := doJSON(client, http.MethodGet, base+"/v2/plugin/login/account?state="+ls.State, acctHeaders, nil); errAcct == nil {
			_ = json.Unmarshal(acctRaw, &acct)
		}
		if tok.Domain == "" && region == "global" {
			tok.Domain = "www.workbuddy.ai"
		}
		out := map[string]any{
			"access_token":  tok.AccessToken,
			"refresh_token": tok.RefreshToken,
			"expires_in":    tok.ExpiresIn,
			"domain":        tok.Domain,
			"uid":           acct.UID,
			"enterprise_id": acct.EnterpriseID,
			"nickname":      acct.Nickname,
			"region":        region,
		}
		oraw, _ := json.Marshal(out)
		fmt.Println(string(oraw))
		os.Remove(stateFile)

	default:
		fatal("unknown subcommand %q (want url|poll [cn|global])", sub)
	}
}
