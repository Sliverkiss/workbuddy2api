// weboauth.go OAuth 设备登录上游调用（cmd/login 流程的库化移植，供 Web 管理台使用）。
// 端点全部在 chatBase（copilot.tencent.com），信封为 {code,msg,data}。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// OAuthState 设备登录起始响应。
type OAuthState struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

// OAuthToken 轮询完成的令牌包。
type OAuthToken struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

// OAuthAccount 账号信息（拿不到不阻断，token 已是权威）。
type OAuthAccount struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// pluginJSON 发插件域请求并解信封；bearer 为空不带鉴权。
func (c *Client) pluginJSON(method, rawURL string, body any, bearer string) (json.RawMessage, error) {
	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return c.doJSON(req)
}

// OAuthStart 申请设备登录 state + 授权 URL。
func (c *Client) OAuthStart() (*OAuthState, error) {
	data, err := c.pluginJSON(http.MethodPost, c.ChatBaseCN+"/v2/plugin/auth/state?platform=CLI", map[string]any{}, "")
	if err != nil {
		return nil, err
	}
	var st OAuthState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	if st.State == "" || st.AuthURL == "" {
		return nil, fmt.Errorf("auth state: missing state or authUrl")
	}
	return &st, nil
}

// OAuthPollToken 轮询授权结果。
// done=false 表示等待用户授权（业务 code 非 0 的 "login ing" 也归一到此）；
// 传输/5xx 错误上抛。
func (c *Client) OAuthPollToken(state string) (token *OAuthToken, done bool, err error) {
	data, err := c.pluginJSON(http.MethodGet,
		c.ChatBaseCN+"/v2/plugin/auth/token?state="+url.QueryEscape(state), nil, "")
	if err != nil {
		if ue, ok := err.(*Error); ok {
			if ue.Status == 0 || ue.Status >= 500 {
				return nil, false, ue // 传输/服务端错误上抛
			}
			return nil, false, nil // 业务 code 非 0（如 11217 login ing）= waiting for login
		}
		return nil, false, err // 非 *Error（网络层）上抛
	}
	var tok OAuthToken
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return nil, false, nil
	}
	return &tok, true, nil
}

// OAuthAccount 取账号信息（失败返回零值，不阻断）。
func (c *Client) OAuthAccount(state, accessToken string) *OAuthAccount {
	data, err := c.pluginJSON(http.MethodGet,
		c.ChatBaseCN+"/v2/plugin/login/account?state="+url.QueryEscape(state), nil, accessToken)
	var acct OAuthAccount
	if err == nil {
		_ = json.Unmarshal(data, &acct)
	}
	return &acct
}
