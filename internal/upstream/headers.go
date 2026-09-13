// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"net/http"
	"strings"

	"workbuddy2api/internal/auth"
)

const (
	clientUA        = "CLI/2.63.2 CodeBuddy/2.63.2"
	originRefererCN = "https://www.codebuddy.cn"
)

func originRefererFor(a *auth.Auth) string {
	return originRefererCN
}

// userAgent 返回当前出站 UA：Client.UserAgent 非空则覆盖（全部出站请求生效），
// 空 = 保持现状 clientUA。指纹净化考虑：默认值不变，仅当用户显式配置才改写。
func (c *Client) userAgent() string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return clientUA
}

// resolveDeviceToken 解析本次请求的 X-Device-Token 取值。
// 优先级：auth.Auth.DeviceToken（每号）> Client.DeviceToken（config 全局）> 文件兜底。
// 三者皆空/读失败则返回空串（调用方不注入该头，优雅降级）。
// 为什么不放进 CommonHeaders：鉴权/刷新类头（refresh / FetchModels）给设备 token
// 无意义且可能被上游风控误判为异常客户端；只在 chat/billing 业务请求注入。
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil && a.DeviceToken != "" {
		return a.DeviceToken
	}
	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}
	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}
	return ""
}

// injectDeviceToken 在 req 注入 X-Device-Token 头（仅当取到非空 token）。
func (c *Client) injectDeviceToken(req *http.Request, a *auth.Auth) {
	if tok := c.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// CommonHeaders 设置所有 API 共享的请求头。
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.userAgent())
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	if a.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	// 用量归属头：真实桌面端发 X-Agent-Purpose="conversation" + X-IDE-Name/Type/X-Product
	// 识别 client，避免上游用量统计里 client/agentPurpose 为空。来源 xiaofan6ya/converter.py。
	// 默认（ClientName 空）保持 X-Product="SaaS" 兼容现状，不设 X-IDE-*（不突变归因）；
	// 配 ClientName（如 "WorkBuddy"）则四头跟随该值，对齐官方桌面端。
	c.injectAttribution(req)
	// 客户端 IP 透传：仅当 PassthroughIP=true 且本次请求 ClientIP 非空（见 handler 设置）。
	// 缺省 false（反代安全边界：不把内网/代理 IP 暴露给上游）。
	c.injectClientIP(req)
	// 设备风控头：auth 每号 > config 全局 > 文件兜底；空则不注入（见 resolveDeviceToken）。
	c.injectDeviceToken(req, a)
}

// injectAttribution 注入用量归属头（X-Agent-Purpose / X-IDE-* / X-Product）。
// 仅在 chat/completions 路径生效（ChatHeaders 调用）。ClientName 非空时全量跟随该值，
// 空则只保留 X-Product="SaaS"（旧行为，向后兼容）。
func (c *Client) injectAttribution(req *http.Request) {
	if c == nil || c.ClientName == "" {
		req.Header.Set("X-Product", "SaaS")
		return
	}
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", c.ClientName)
	req.Header.Set("X-IDE-Type", c.ClientName)
	req.Header.Set("X-Product", c.ClientName)
}

// injectClientIP 在 PassthroughIP 开启时把 ClientIP 透传给上游。
// 三个等价头（X-Forwarded-For/X-Real-IP/X-Client-IP）一并设，与桌面端透传一致。
func (c *Client) injectClientIP(req *http.Request) {
	if c == nil || !c.PassthroughIP || c.ClientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", c.ClientIP)
	req.Header.Set("X-Real-IP", c.ClientIP)
	req.Header.Set("X-Client-IP", c.ClientIP)
}

// ExtractClientIP 从入站请求提取客户端 IP 首段（X-Forwarded-For 首段，回落 X-Real-IP）。
// 供 handler 在 PassthroughIP 开启时填充 Client.ClientIP（chat 路径出站前设置、用后清空）。
// 取不到返回空串（handler 据此跳过透传）。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			// 取逗号前首段并 trim 空白。
			if xff[i] == ',' {
				return strings.TrimSpace(xff[:i])
			}
		}
		return strings.TrimSpace(xff)
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}

// BillingHeaders billing 接口请求头。
// UA 语义：默认**不设置**（保持现状，Go 客户端自带默认 UA）；仅当显式配置
// c.UserAgent 非空才覆盖——避免默认路径给 billing 引入新的 UA 指纹。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if c != nil && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
	// 设备风控头：billing 域（report/travel/balance/checkin）同样注入（见 resolveDeviceToken）。
	c.injectDeviceToken(req, a)
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
}
