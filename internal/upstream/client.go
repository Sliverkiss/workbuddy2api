// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone           ErrKind = iota // 成功
	ErrHardCredit                    // 余额不足（402 或 body 关键词）→ 长冷却
	ErrSoftRate                      // 429 软限流 → 短冷却
	ErrSessionDead                   // 401 + 12153 offline session 失效 → 禁用
	ErrNotFound                      // 404 上游偶发 → 短冷却，不累计错误计数（防雪崩）
	ErrServer                        // 5xx 上游故障
	ErrContentBlocked                // 内容策略拦截（400 + 审核文案）→ 不罚账号，走降级重试
	ErrBadParams                     // 请求体解析失败（400 + Unmarshal chat params failed / 11101）→ 不罚账号，仍轮转
	ErrAccountFault                  // 账号级授权/配额故障（11140 request illegal / 14017 trial not activated）→ 冷却轮换，不无限重试
	ErrClient                        // 其他 4xx / 业务错误
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrContentBlocked:
		return "content_blocked"
	case ErrBadParams:
		return "bad_params"
	case ErrAccountFault:
		return "account_fault"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// matchMode 描述错误分类 marker 的匹配通道。6 组词表 + 文案分类共用同一匹配器，
// 消除「小写 Contains + 原文 Contains 双通道」在 Classification 各分支的手写循环重复。
type matchMode uint8

const (
	// matchFold 大小写不敏感：lower(body) 含 lower(pat) 或 body 原字串含 pat。
	// 双通道与历史手写循环逐字等价（小写比较 + 中文原文比较）。
	matchFold matchMode = iota
	// matchExact 大小写敏感的字面包含。
	matchExact
	// matchLower 在 lower(body) 上做包含匹配（pat 须已小写）。
	matchLower
)

// errorRule 一条错误分类规则：命中 patterns 中任一 marker 即归类为 kind。
type errorRule struct {
	kind     ErrKind
	mode     matchMode
	patterns []string
}

// matchPattern 报告 body 是否命中单个 marker pattern。
func matchPattern(p string, mode matchMode, body, lower string) bool {
	switch mode {
	case matchFold:
		return strings.Contains(lower, strings.ToLower(p)) || strings.Contains(body, p)
	case matchExact:
		return strings.Contains(body, p)
	case matchLower:
		return strings.Contains(lower, p)
	default:
		return false
	}
}

// hit 报告 rule 是否命中 body（任一 marker 命中即真）。
func (r errorRule) hit(body, lower string) bool {
	for _, p := range r.patterns {
		if matchPattern(p, r.mode, body, lower) {
			return true
		}
	}
	return false
}

// firstHit 返回第一条命中的 marker 原文（供「哪个词命中」的场景）；无命中返回 ""。
func (r errorRule) firstHit(body, lower string) string {
	for _, p := range r.patterns {
		if matchPattern(p, r.mode, body, lower) {
			return p
		}
	}
	return ""
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardRule 余额不足关键词（大小写不敏感 + 中文原文双通道）。
var hardRule = errorRule{kind: ErrHardCredit, mode: matchFold, patterns: []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}}

// softRateRule 限流/节流关键词（小写比较 + 中文原文比较双通道）。
// 上游在状态码非 429 时也会返回限流语义（如 200 + code 11140
// "The model provider is rate-limiting requests."、400 + "rate limit"），
// 此类响应若不识别，账号既不被冷却也不喂熔断，下次请求仍会被选中（issue #28）。
//
// 词表按子串匹配，宁缺毋滥：只收录明确指向「请求速率/模型用量被节流」的措辞。
// 连字符形式（rate-limiting / rate-limited）需单列——Contains 不跨 '-'。
// "too many" 会命中 "too many tokens" 这类客户端参数错误，代价是该号被软冷却
// 一个 SoftCooldown（默认 60s）后自愈，远小于漏判限流导致反复选中同一号的代价。
var softRateRule = errorRule{kind: ErrSoftRate, mode: matchFold, patterns: []string{
	"rate limit", // rate limit / rate limits / rate limiting
	"rate-limiting",
	"rate-limited",
	"too many requests",
	"too many",
	"usage limit", // usage limit reached / model usage limit exceeded（用量节流，非计费余额）
	"请求过于频繁", "限流",
}}

var sessionDeadRule = errorRule{kind: ErrSessionDead, mode: matchExact, patterns: []string{"Offline user session not found", "12153"}}

// contentBlockedRule 内容策略拦截关键词（大小写不敏感子串匹配）。
//
// 定位：上游按逐字精确指纹审核，system 来源的模板句（如 Claude Code/Codex
// 注入指令）触发 HTTP 400 + 以下文案。这是「误报」（合法流量被审核误杀），
// 非账号问题——该账号余额健康、未限流、session 未死，故 ErrContentBlocked
// 在 applyErrorPolicy 中不罚账号（无冷却/熔断/NoteError），改由网关降级重试。
var contentBlockedRule = errorRule{kind: ErrContentBlocked, mode: matchLower, patterns: []string{
	"blocked by security policy",
	"unapproved channel",
	"illegal api invocation",
}}

// contentBlockedClientMsg 内容拦截返回给调用方的固定文案。
// [关键词] 填分类词（色情 / nsfw / 暴力 等），绝不填业务 code、账号、冷却、upstream 前缀。
const contentBlockedClientMsg = "触发网站风控违禁词，无法调用模型：内容命中网关内容防火墙规则[%s]，已被拦截。请修改内容后重试。"

const contentBlockedFallbackKeyword = "违禁词"

// contentBlockedKeywords 审核分类词，按优先级扫描上游文案（大小写不敏感）。
// 只收录可直接展示给调用方的分类标签，不收录错误码（如 11128）。
var contentBlockedKeywords = []string{
	"色情", "porn", "nsfw", "adult",
	"暴力", "violence",
	"政治", "politics",
	"赌博", "gambling",
	"毒品", "drug",
	"违禁词",
}

// ContentBlockedClientMessage 把上游内容拦截改写成网关防火墙口径，不含账号/错误码。
func ContentBlockedClientMessage(body string) string {
	return fmt.Sprintf(contentBlockedClientMsg, contentBlockedKeyword(body))
}

// contentBlockedKeyword 从审核文案抽出分类关键词；抽不到则回「违禁词」。
func contentBlockedKeyword(body string) string {
	text := body
	var env struct {
		Msg string `json:"msg"`
	}
	if json.Unmarshal([]byte(body), &env) == nil && strings.TrimSpace(env.Msg) != "" {
		text = env.Msg
	}
	lower := strings.ToLower(text)
	for _, kw := range contentBlockedKeywords {
		if strings.Contains(lower, kw) {
			return kw
		}
	}
	return contentBlockedFallbackKeyword
}

// badParamsRule 请求体解析失败关键词（issue #41 连带）：HTTP 400 + 上游
// "Unmarshal chat params failed..."（code 11101）。这是"发给上游的 body 有问题"，
// 与账号健康无关——不罚号，但仍轮转（commit B）。
var badParamsRule = errorRule{kind: ErrBadParams, mode: matchExact, patterns: []string{
	"Unmarshal chat params failed",
	`"code":11101`,
}}

// alreadyCheckinRule "今天已签到"关键词（上游对重复签到返回 code!=0，
// 实测 code=10001/14001 "今天已签到"/"今日已签到"）。只对 *Error.Msg 做包含匹配，
// 网络层/解析层错误不在此识别（见 IsAlreadyCheckin）。
var alreadyCheckinRule = errorRule{mode: matchFold, patterns: []string{"已签到", "already"}}

// accountFaultRule 账号级授权/配额故障关键词（大小写不敏感子串匹配）。
//
// 定位：这类错误是**账号本身状态**决定的本机故障，不是请求格式、不是临时限流、
// 也不是内容误报——继续重试只会反复刷上游风控/配额检查，必须把该账号冷却轮换。
//   - "request illegal"（code 11140）→ 上游 auth/auth_forbidden，账号级授权风控
//     （实测 global 账号：同一规范化请求 A1 403/11140 vs A2 429/14017，差异全由账号
//     数据决定）。需重新 OAuth 登录才能恢复，短冷却只能阻止继续送死。
//   - code 14017（"trial not activated" / "The trial version is not yet activated"）→
//     上游 quota/quota_not_activated，register 未完成的试用未激活账号，同样账号级。
//
// 注意 11140 **不能**按 code 判定：该 code 也承载模型级限流文案（"The model provider
// is rate-limiting requests."），那种场景必须保持 ErrSoftRate（上方 softRateRule
// 先命中）。故此处只收 msg 关键词 "request illegal"（auth_forbidden 的真实文案），
// 120 与 private 均落同一分类。14017 文案唯一（无软限流歧义），可安全收录。
var accountFaultRule = errorRule{kind: ErrAccountFault, mode: matchFold, patterns: []string{
	"request illegal",
	"trial not activated",
	"trial version is not yet activated",
}}

// softRateResetLoc 上游 429 6004 文案中的重置时间固定按 UTC+8 解释（上游文案如此，
// 与容器时区无关）。
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// SoftRateResetLoc 暴露重置时间的固定时区（供测试构造/断言同一时区口径）。
func SoftRateResetLoc() *time.Location { return softRateResetLoc }

// modelRateLimitCode 明确指向「模型级 429 限流」的业务 code。
// 上游用它表达"该模型的使用量超限"（code 6004，msg 带「将在 … 重置」），
// 而不是账号整体被限流——账号健康，只是这个模型此刻被限（issue #31）。
const modelRateLimitCode = "6004"

// softRateResetRe 匹配「将在 … 重置」，捕获中间的时间串。
const softRateResetRe = `将在 (.+?) 重置`

// softRateTimeLayout 上游重置时间的格式（无时区后缀；时区固定 UTC+8）。
const softRateTimeLayout = "2006-01-02 15:04:05"

// IsModelRateLimit 报告 429 body 是否明确指向模型级限流（业务 code 6004）。
// 用于区分"账号级软限流"（按账号冷却）与"模型级用量限流"（切模型即可用）。
func IsModelRateLimit(body string) bool {
	// `"code":6004` / `"code": 6004` / `"code":"6004"` 均可命中（JSON 空格容差）。
	re := regexp.MustCompile(`"code"\s*:\s*"?` + modelRateLimitCode + `"?`)
	return re.MatchString(body)
}

// ParseSoftRateReset 从 429 body 解析「将在 … 重置」时间（上游 UTC+8 文案）。
// 成功返回解析出的**墙钟时刻**（按 UTC+8 解释），失败返回零值 + false。
// 内部先判 IsModelRateLimit：非模型级限流（非 6004）即使带"重置"字样也不返回——该重置
// 无冷却语义（如 11140 的通用限流提示），解析出来反而会错误收窄冷却。
func ParseSoftRateReset(body string) (time.Time, bool) {
	if !IsModelRateLimit(body) {
		return time.Time{}, false
	}
	re := regexp.MustCompile(softRateResetRe)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return time.Time{}, false
	}
	ts := strings.TrimSpace(m[1])
	ts = strings.TrimSuffix(ts, " UTC+8") // 去掉后缀，固定按 softRateResetLoc 解释
	t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
//
// 判定顺序自「严」到「宽」，每层的先后都有语义依据：
//  1. 402 / hardRule —— 计费额度耗尽，最严、最不可自愈，必须最先判。
//     "quota exceeded" 语义跨计费/限流两界，历史归 hard_credit，本次保持不变
//     （issue #28 已记录该反向误判风险，待上游原始响应确认后再定）。
//  2. sessionDeadRule —— 需要人工重登的终态。若 401 body 同时含 "12153" 与
//     "rate limit"（如网关错误页混排），归 session_dead：短冷却救不活失效 session，
//     误判为限流会让该死号留在池中反复被选中；且此层 marker 是精确词（12153 等），
//     比限流层的大范围子串更具体，具体优先于宽泛。
//  3. accountFaultRule —— 账号级授权/配额故障（11140 request illegal auth 风控、
//     14017 trial not activated register 未完成）。与 429 一起纳入轮换冷却，且必须
//     先于 softRate/status429 判定：14017 常带 429 状态码，若落到 status==429 兜底
//     会误归 soft_rate（"限流"语义不符：限流可指数退避等自愈，账号级故障等不来）。
//     11140 的 model 级限流变体（rate-limiting 文案）因 marker 不含该文案而天然
//     落到 softRateRule 层，不受影响。
//  4. softRateRule —— 非 429 状态码携带限流文案（issue #28 修复点）。
//     位于此处可覆盖 200/400/403/5xx 各状态码；429 且 body 含文案时在此短路，
//     结果同为 soft_rate，与下一层一致。
//  5. status==429 —— body 无文案时的兜底识别。
//  6. 404 / 5xx / 其他 4xx —— 与限流无关的常规分类。
func Classify(status int, body string) ErrKind {
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	if hardRule.hit(body, lower) {
		return ErrHardCredit
	}
	if sessionDeadRule.hit(body, lower) {
		return ErrSessionDead
	}
	if accountFaultRule.hit(body, lower) {
		return ErrAccountFault
	}
	if softRateRule.hit(body, lower) {
		return ErrSoftRate
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	// 内容策略拦截（HTTP 400 + 审核文案）：判在通用 ErrClient 之前。
	// 这是误报信号，不罚账号，由网关降级重试处理（见 handler.applyErrorPolicy）。
	// 请求体解析失败（HTTP 400 + Unmarshal chat params failed / code 11101）：
	// 这是"发给上游的 body 有问题"。网关侧截断已由 413 消灭（issue #41 commit A），
	// 剩余来源是客户端 JSON 本身畸形——换了账号照样 400，不该罚号（白白冷却好号）。
	// 归 ErrBadParams：不冷却/不熔断/不计错，但**仍然轮转**（不同账号可能有不同的
	// 模型权限，值得再试一次）。
	if status >= 400 {
		if contentBlockedRule.hit(body, lower) {
			return ErrContentBlocked
		}
		if badParamsRule.hit(body, lower) {
			return ErrBadParams
		}
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含余额关键词的情况已被上面 hardRule 捕获。
	return ErrNone
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client

	// ChatHTTP 聊天 SSE 专用 client：无总时长上限（Timeout=0），首字节由
	// Transport.ResponseHeaderTimeout 约束，流中空闲由 IdleTimeout 约束。
	// 与 HTTP 共享同一个 *http.Transport 实例，连接池不重复。
	ChatHTTP *http.Client

	// HeaderTimeout 聊天 SSE 首字节前（响应头）超时；<=0 表示未设置（回落 HTTP.Timeout）。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天 SSE 流中空闲超时；<=0 表示禁用空闲监控。
	IdleTimeout time.Duration

	// effortsMu/efforts 缓存各模型 supportedEfforts（FetchModels 刷新），供请求体 effort 降级。
	// 键按 realm 分层（map[realm]map[model][]efforts）：CN 探测结果不得被 global 同模型名
	// 请求复用（同名不同档位会错误降级，C-2）。global 侧暂无 efforts 探测 → 桶缺失即透传。
	effortsMu sync.RWMutex
	efforts   map[string]map[string][]string

	// defaultEfforts 缓存各模型 reasoning.defaultEffort（FetchModels 刷新），供
	// thinking.go 补档：缺显式 effort 时优先用模型声明默认档，空串回退硬编码 high。
	// 与 efforts 同 realm 分层桶（同 C-2 隔离原则），共用 effortsMu。
	defaultEfforts map[string]map[string]string

	// globalModels 缓存 global 模型名目录探测结果（成功 ∩ 静态 overlay；
	// 1h TTL + 5min 负缓存），见 global_models.go。按实例持有，测试新建 Client 即隔离。
	globalModels fetchGlobalModelsCache

	// SanitizeFingerprints 出站请求体黑名单指纹脱敏开关（默认 true；false 完全还原）。
	SanitizeFingerprints bool

	// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认 WorkBuddy
	// 三段式与 billingUA 单段式）。空 = 默认官方形态：chat/refresh/FetchModels 走
	// `WorkBuddy/<ver> WorkBuddy/<ver> CLI/<cliVer>`；billing/checkin 走 `WorkBuddy/<ver>`
	// （仅当 client_name 非空，见 billingUA）。
	// issue #42 深挖：官网「使用端」列基于出站请求的 UA/X-Product 服务端归因，
	// 官方 WorkBuddy 桌面 UA 见 defaultWorkBuddyUA。默认值已对齐官方（A 段变更），
	// 用户仍可显式配置完全自定义的 UA。
	UserAgent string

	// DeviceToken 设备风控 Token（X-Device-Token 头）兜底来源：config upstream.device_token。
	// 仅当 auth.Auth.DeviceToken 为空时才取此值；两者皆空则不注入该头。
	// 容器内无桌面端 Turing SDK，这是把外部（宿主/桌面端）生成的 token 注入的入口。
	// 另见 DeviceTokenFile 缓存读取：宿主可把 token 落 /app/data/device_token 共用。
	DeviceToken string

	// DeviceTokenFile 宿主落盘的 device token 文件路径（可选，空 = 不读文件）。
	// 读取频率限 5 分钟一次缓存（见 device_token.go），>1KB 或读失败则忽略。
	// 解析优先级：auth.Auth.DeviceToken > DeviceToken（config）> DeviceTokenFile（文件）。
	DeviceTokenFile string

	// ClientName 用量归属头取值（X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）。
	// 空（默认）= "WorkBuddy"：伪造官方桌面端指纹（X-IDE-* 四头 + X-Agent-Purpose，
	// 见 injectAttribution / attributionClientName）。显式配 "SaaS" 还原旧行为
	// （仅 X-Product="SaaS"，不设 X-IDE-*）；配其他值则四头跟随该值。
	ClientName string

	// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` + B 段的
	// X-IDE-Version）。空 = 内置默认 defaultClientVersion（对齐官方 5.5.4 分发包）。
	// config upstream.client_version 覆盖。
	ClientVersion string

	// CliVersion 出站 UA 中 `CLI/<ver>` 段版本。空 = 内置默认 defaultCliVersion
	// （对齐官方内置 CLI 2.137.1）。config upstream.cli_version 覆盖。
	CliVersion string

	// PassthroughIP 是否透传客户端 IP 给上游（X-Forwarded-For/X-Real-IP 首段）。
	// 缺省 false（反代安全边界）；handler 在 chat 路径按请求把 clientIP 参数传入 ChatStream，
	// 由 ChatHeaders 注入（不再挂共享字段，杜绝并发串扰）。
	PassthroughIP bool

	ChatBaseCN    string
	BillingBaseCN string

	// ChatBaseGlobal / BillingBaseGlobal 国际版（global realm）上游 base。
	// 空 = 缺省默认 https://www.workbuddy.ai（D5）。
	ChatBaseGlobal    string
	BillingBaseGlobal string

	// GlobalEnabled 是否启用 global realm 路由（config global.enabled，缺省 true）。
	// false 即显式逃生门：即使用户 auth 写了 realm=global 也**不**路由到 global base——
	// chatBase/billingBase 返回 CN base，路径也走 CN（双保险，与 auth.Realm() 的开关闸呼应）。
	GlobalEnabled bool

	// UsageBaseCN / UsageBaseGlobal 积分消耗明细（get-user-request-usage）所在主机：
	// 官网 usercenter 同源，与 billing base 不同（CN 实测仅在 www.workbuddy.cn 提供，
	// codebuddy.cn 同路径 404/400）。空 = 回落默认。
	UsageBaseCN     string
	UsageBaseGlobal string
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		// 聊天 SSE 首字节前硬上限（对短 RPC 无实际影响：其总时长 120s 更先到期）。
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:                 &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:             &http.Client{Timeout: 0, Transport: tr}, // 无总时长；首字节由 ResponseHeaderTimeout 管
		SanitizeFingerprints: true,
		ChatBaseCN:           "https://copilot.tencent.com",
		BillingBaseCN:        "https://www.codebuddy.cn",
		UsageBaseCN:          "https://www.workbuddy.cn",
	}
}

// chatHTTP 返回聊天专用 client；未设置（如测试只注入 HTTP）时回落 HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

// defaultGlobalBase 缺省 global base（D5：config 未覆盖时默认 workbuddy.ai）。
const defaultGlobalBase = "https://www.workbuddy.ai"

// globalChatBase 生效的 global chat base：Client.ChatBaseGlobal 非空取之，否则默认。
func (c *Client) globalChatBase() string {
	if c.ChatBaseGlobal != "" {
		return c.ChatBaseGlobal
	}
	return defaultGlobalBase
}

// globalBillingBase 生效的 global billing base：Client.BillingBaseGlobal 非空取之，否则默认。
func (c *Client) globalBillingBase() string {
	if c.BillingBaseGlobal != "" {
		return c.BillingBaseGlobal
	}
	return defaultGlobalBase
}

// globalOn 报告账号是否路由到 global 上游：GlobalEnabled 开且账号 Realm()==global。
// 双保险：config 开关是第一道闸（上游侧），auth.Realm() 的开关闸是第二道（账号侧）。
func (c *Client) globalOn(a *auth.Auth) bool {
	return c.GlobalEnabled && a != nil && a.Realm() == "global"
}

func (c *Client) chatBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return c.globalChatBase()
	}
	return c.ChatBaseCN
}

// prepareBody 组装出站请求体（脱敏开关由 Client.SanitizeFingerprints 控制）。
// 显式传 realm 使 effort 降级按域取桶：CN 探测信息不得作用到 global 请求（C-2）。
// conversationID 为网关解析出的会话标识（用于 prompt_cache_key 注入的会话段；
// body 里自带 conversation_id 时以 body 为准）。uid8 来自账号 UID，是跨账号硬隔离段。
func (c *Client) prepareBody(body []byte, realm, uid, conversationID string) []byte {
	efforts, defs := c.effortsSnapshot(realm), c.defaultEffortsSnapshot(realm)
	if realmKey(realm) == "global" {
		// global 域降级源 = 远端探测桶（权威）∪ 产品静态兜底表（全局 21 名内档位如
		// deepseek-v4.1-flash ['high']）。当前探测桶为空时也按静态表降级，不全程透传
		// （issue #84：往 WorkBuddy 上游发 low/max 非法，须降级到 high）。
		efforts, defs = globalEffortMap(efforts, defs)
	}
	body = PrepareBodyOptWithEffortsAndDefault(body, c.SanitizeFingerprints, efforts, defs)
	// prompt_cache_key 注入（P0 费用优化，费用降 ~17×）：按账号隔离的稳定缓存键，
	// 让同一客户端对同一账号的连续请求命中上游前缀缓存。
	body = InjectPromptCacheKey(body, uid, conversationID)
	return body
}

// effortsSnapshot 返回指定 realm 的 effort 能力缓存副本；该域无探测 → nil（透传不降级）。
func (c *Client) effortsSnapshot(realm string) map[string][]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	bucket, ok := c.efforts[realmKey(realm)]
	if !ok || len(bucket) == 0 {
		return nil
	}
	cp := make(map[string][]string, len(bucket))
	for k, v := range bucket {
		cp[k] = v
	}
	return cp
}

// defaultEffortsSnapshot 返回指定 realm 的模型 defaultEffort 缓存副本；
// 该域无探测或无声明默认档 → nil（thinking.go 回退硬编码 high）。
func (c *Client) defaultEffortsSnapshot(realm string) map[string]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	bucket, ok := c.defaultEfforts[realmKey(realm)]
	if !ok || len(bucket) == 0 {
		return nil
	}
	cp := make(map[string]string, len(bucket))
	for k, v := range bucket {
		cp[k] = v
	}
	return cp
}

// realmKey 归一化 efforts 缓存键：cn/global。空 realm 视为 cn（老调用/无前缀模型名）。
func realmKey(realm string) string {
	if realm == "" {
		return "cn"
	}
	return realm
}

// storeEfforts 按 realm 写入 effort 能力缓存桶（efforts + defaultEfforts），并发安全。
// 供 CN FetchModels 与 global 探测共用：拉取到的模型档位落桶后，出站请求体 normalizeReasoningEffort
// 才能按域降级。efforts 与 defs 均空时删除该 realm 桶（等价「该域无可降级档位」）。
// 调用方负责在「无新数据」时跳过写（CN 侧空桶不清既有桶，见 FetchModels 尾部）。
func (c *Client) storeEfforts(realm string, efforts map[string][]string, defs map[string]string) {
	c.effortsMu.Lock()
	defer c.effortsMu.Unlock()
	if c.efforts == nil {
		c.efforts = make(map[string]map[string][]string)
	}
	if c.defaultEfforts == nil {
		c.defaultEfforts = make(map[string]map[string]string)
	}
	k := realmKey(realm)
	if len(efforts) == 0 && len(defs) == 0 {
		delete(c.efforts, k)
		delete(c.defaultEfforts, k)
		return
	}
	c.efforts[k] = efforts
	c.defaultEfforts[k] = defs
}

// GlobalEffortSnapshot 导出 global 域 effort 能力缓存（探测下发 ∪ 静态兜底合并后的桶），
// 供 /v1/models 输出 reasoning_supported_efforts / reasoning_default_effort。
// 返回副本；桶未填充（无 global 账号或从未探测）→ nil（调用方回落静态兜底表）。
func (c *Client) GlobalEffortSnapshot() (efforts map[string][]string, defaults map[string]string) {
	return c.effortsSnapshot("global"), c.defaultEffortsSnapshot("global")
}

func (c *Client) billingBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return c.globalBillingBase()
	}
	return c.BillingBaseCN
}

// billing 域端点路径（billingBase + path）。balance/checkin 与 report（report.go）同域，
// 统一走 billingJSON 发请求。
const (
	billingMeterPath   = "/billing/meter/get-user-resource"    // global 首选（R9：国际版无 /v2 前缀）
	dailyCheckinPath   = "/billing/meter/daily-checkin"        // global 首选
	billingMeterPathV2 = "/v2/billing/meter/get-user-resource" // CN 现状 / global fallback
	dailyCheckinPathV2 = "/v2/billing/meter/daily-checkin"
)

// billingMeterPaths 按 realm 返回 billing/meter 域路径候选序列：
// global → [无 /v2, 有 /v2]（404 时 fallback）；cn → [有 /v2]（现状逐字，零回归）。
// 仅作用于 get-user-resource / daily-checkin（/billing/meter/* 族）；report /v2/report 不参与，
// 其他 billing 端点（growth 等）路径不含 /billing/meter 前缀，走原常量不受影响。
func (c *Client) billingMeterPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{billingMeterPath, billingMeterPathV2}
	}
	return []string{billingMeterPathV2}
}

// checkinMeterPaths 同上，针对 daily-checkin。
func (c *Client) checkinMeterPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{dailyCheckinPath, dailyCheckinPathV2}
	}
	return []string{dailyCheckinPathV2}
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// refreshIOTimeout 单次 refresh 网络调用的总时长上限。
// 远小于 HTTP.Client.Timeout(120s)：refresh 持锁窗口内做网络 I/O，超时越短，
// 单账号 hang 对该账号相关操作的阻塞越短（issue:持锁 120s I/O → 池级停滞）。
const refreshIOTimeout = 30 * time.Second

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。
//
// 并发安全模型（两段式，缩小持锁窗口）：
//   - 锁内仅做「读 refreshToken 快照」与「校验未变后写回新 token」两小段内存操作；
//   - 网络 I/O（doJSON）在**锁外**执行，带 30s ctx 超时——避免上游 hang 时长时间
//     独占 a.mu，阻塞同账号的 SaveAtomic / 其他刷新（issue:持锁 120s I/O）。
//   - 写回前重新校验快照一致性：若锁外期间另一 goroutine 已完成刷新（refreshToken
//     已变），本次结果直接采用（新 token 已生效），不再重复写回。
func (c *Client) RefreshToken(a *auth.Auth) error {
	// 第 1 段（锁内）：读快照。
	a.Lock()
	rtSnapshot := a.RefreshToken
	atBefore := a.AccessToken
	a.Unlock()
	if strings.TrimSpace(rtSnapshot) == "" {
		return fmt.Errorf("no refreshToken")
	}

	url := c.chatBase(a) + "/v2/plugin/auth/token/refresh"
	ctx, cancel := context.WithTimeout(context.Background(), refreshIOTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	// RefreshHeaders 读取 a 的字段（domain/uid 等）注入请求头——需在锁内取快照值，
	// 用一个显式逐字段拷贝的临时 auth 构造头（不拷贝 sync.Mutex，避免 vet copies-lock）。
	a.Lock()
	hdrSnapshot := auth.Auth{
		AccessToken:  a.AccessToken,
		RefreshToken: rtSnapshot,
		ExpiresAt:    a.ExpiresAt,
		Domain:       a.Domain,
		UID:          a.UID,
		EnterpriseID: a.EnterpriseID,
		Nickname:     a.Nickname,
		DeviceToken:  a.DeviceToken,
	}
	a.Unlock()
	c.RefreshHeaders(req, &hdrSnapshot)

	// 网络 I/O（锁外，30s 上限）。
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}

	// 第 2 段（锁内）：校验快照一致后写回。
	a.Lock()
	defer a.Unlock()
	if a.AccessToken != atBefore && a.RefreshToken != rtSnapshot {
		// 锁外期间另一 goroutine 已完成刷新：新 token 已生效，本次结果不必再写
		// （两个并发刷新拿到的新 token 都有效，后写会覆盖先写，但二者等价可用；
		// 提前返回避免无意义覆盖与 ExpiresAt 抖动）。
		return nil
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// chatPath 按 realm 返回 chat 端点路径（不含 base）：
// global → /console/chat/completions（404/405 时由 ChatStream fallback /v2/chat/completions）；
// cn → /v2/chat/completions（现状逐字，零回归）。
func (c *Client) chatPath(a *auth.Auth) string {
	if c.globalOn(a) {
		return globalChatConsolePath
	}
	return chatCompletionsPath
}

// 路径常量：CN 现状路径（chatCompletionsPath）与 global 双候选路径。
const (
	chatCompletionsPath   = "/v2/chat/completions"
	globalChatConsolePath = "/console/chat/completions"
)

// chatFallbackHTTPStatus global chat fallback 只在 404/405 时发生（R9：上游新旧路径分叉）。
func chatFallbackHTTPStatus(status int) bool { return status == 404 || status == 405 }

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 等价于 ChatStreamContext(context.Background(), ...)：不带调用方取消语义。
// 新调用方应优先用 ChatStreamContext 传入请求 ctx（客户端断连即中断在途调用、释放租约）。
//
// global realm：先打 /console/chat/completions，404/405 时同一 base 二次换 /v2/chat/completions
// （上游新旧路径分叉，PLAN R9 fallback 顺序）。cn：/v2/chat/completions 现状不变。
func (c *Client) ChatStream(a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	return c.ChatStreamContext(context.Background(), a, body, clientIP, meta)
}

// ChatStreamContext 同 ChatStream，但从 ctx 派生请求 context：调用方（handler）传入
// r.Context() 后，客户端断连/请求取消会立即中断在途上游调用、释放连接与账号在途名额，
// 不再空转到 IdleTimeout。ctx 为 nil 时回落 Background。
func (c *Client) ChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var cancel context.CancelFunc
	// global 首次路径 404/405 时换 fallback 路径重试；ensureConsoleSystem 在 prepareBody 后统一套用
	// 全局脚本：首条消息非 system 时前置兜底 system（防 console 域上游 code 11-128）。
	prepared := c.prepareBody(body, a.Realm(), a.UID, meta.ConversationID)
	if c.globalOn(a) {
		prepared = ensureConsoleSystem(prepared)
	}
	for attempt, path := range c.chatPaths(a) {
		url := c.chatBase(a) + path
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(prepared))
		if err != nil {
			return nil, 0, nil, err
		}
		c.ChatHeaders(req, a, clientIP, meta)
		// 从调用方 ctx 派生：保留取消传播（父 ctx 取消 → 本 ctx 取消），
		// 同时 monitorBody.Close 仍能独立 cancel 本分支（空闲掐流）。
		reqCtx, cancel := context.WithCancel(ctx)
		req = req.WithContext(reqCtx)
		resp, err := c.chatHTTP().Do(req)
		if err != nil {
			cancel()
			log.Printf("ERR: [upstream] chat_stream uid=%s: transport error: %v", logfmt.UID8(a.UID), err)
			return nil, 0, nil, err
		}
		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			cancel()
			kind := Classify(resp.StatusCode, string(raw))
			log.Printf("WARN: [upstream] chat_stream uid=%s: upstream %d %s body=%s",
				logfmt.UID8(a.UID), resp.StatusCode, kind, truncate(string(raw), 200))
			// global 首次路径 404/405 → 换 fallback 路径重试；其余状态码直接返回。
			if attempt < len(c.chatPaths(a))-1 && chatFallbackHTTPStatus(resp.StatusCode) {
				continue
			}
			return nil, resp.StatusCode, raw, nil
		}
		// 成功分支：cancel 所有权交给 monitorBody（其 Close 会 cancel）；
		// IdleTimeout<=0 时 monitorBody 原样返回底流、无人调 cancel——可接受：
		// 取消传播由 http.Transport 在 body Close / 父 ctx 取消时处理，连接正常清理。
		return monitorBody(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
	}
	cancel()
	return nil, 0, nil, nil
}

// chatPaths 返回按 realm 的 chat 路径候选序列：
// global → [console, /v2]（向 Fallback 迭代）；cn → [/v2]（单元素，现状）。
func (c *Client) chatPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{globalChatConsolePath, chatCompletionsPath}
	}
	return []string{chatCompletionsPath}
}

// ModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens）。
type ModelInfo struct {
	ID             string
	Name           string
	ContextWindow  int64    // = maxInputTokens
	MaxTokens      int64    // = maxOutputTokens
	Efforts        []string // reasoning.supportedEfforts（空=未知/固定档）
	DefaultEffort  string   // reasoning.defaultEffort（空=未声明，thinking.go 回退硬编码）
	SupportsImages bool     // 顶层 supportsImages（多模态能力，透出到 /v1/models）
}

// 模型目录端点路径常量（按 realm 切）：
// CN 现状 /console/enterprises/personal/models 逐字保留（零回归）；
// global 走 /v2/enterprises/personal/models（PR #20 实测 /console 500、/v2 200 含
// credits 倍率的完整模型表）。modelsPath 按 globalOn 分发。
const (
	cnModelsPath     = "/console/enterprises/personal/models"
	globalModelsPath = "/v2/enterprises/personal/models"
)

// modelsPath 按 realm 返回动态模型目录端点路径（不含 base）。
// CN → /console/enterprises/personal/models（现状，零回归）；
// global → /v2/enterprises/personal/models（国际版实测可用路径，见 global_models.go
// probe 家族）：governed by globalOn（config global.enabled + 账号 realm 双闸）。
func (c *Client) modelsPath(a *auth.Auth) string {
	if c.globalOn(a) {
		return globalModelsPath
	}
	return cnModelsPath
}

// nonChatModel 判定是否非对话模型（应从模型列表过滤掉）。
// 来源：harness buddy.ts:547-555。三类规则：
//   - id 前缀 nes-/completion-/codewise-：嵌入/补全/代码专用模型，选了报 code=11102。
//   - maxOutputTokens ≤ 256：tiny 输出非对话模型。
//   - tags 含 text-to-image：图片生成模型，非本网关用途。
func nonChatModel(id string, maxOutputTokens int64, tags []string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range [...]string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	if maxOutputTokens > 0 && maxOutputTokens <= 256 {
		return true
	}
	for _, t := range tags {
		if t == "text-to-image" {
			return true
		}
	}
	return false
}

// FetchModels 调上游动态模型接口。
// 字段名与上游实际返回对齐：maxInputTokens（非 contextWindow）、maxOutputTokens（非 maxTokens）。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	url := c.chatBase(a) + c.modelsPath(a)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.CommonHeaders(req, a) // 复用共享请求头（Origin/Referer/UA/Accept/Content-Type）
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID              string   `json:"id"`
				Name            string   `json:"name"`
				MaxInputTokens  int64    `json:"maxInputTokens"`
				MaxOutputTokens int64    `json:"maxOutputTokens"`
				Disabled        bool     `json:"disabled"`
				SupportsImages  bool     `json:"supportsImages"`
				Tags            []string `json:"tags"`
				Reasoning       struct {
					Effort           string   `json:"effort"`
					DefaultEffort    string   `json:"defaultEffort"`
					SupportedEfforts []string `json:"supportedEfforts"`
				} `json:"reasoning"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	// dynMap 收集模型字段；nonChatModel 过滤在写入 dynMap 前执行，
	// 确保非对话条目（nes-/completion-/codewise- 前缀、maxOutputTokens≤256、
	// tags 含 text-to-image）根本不进返回列表（来源：harness buddy.ts:547-555）。
	type dynEntry struct {
		ID              string
		Name            string
		MaxInputTokens  int64
		MaxOutputTokens int64
		Disabled        bool
		Efforts         []string
		DefaultEffort   string
		SupportsImages  bool
	}
	dynMap := make(map[string]dynEntry, len(env.Data.Models))
	for _, m := range env.Data.Models {
		if nonChatModel(m.ID, m.MaxOutputTokens, m.Tags) {
			continue
		}
		dynMap[m.ID] = dynEntry{
			ID: m.ID, Name: m.Name,
			MaxInputTokens: m.MaxInputTokens, MaxOutputTokens: m.MaxOutputTokens,
			Disabled: m.Disabled, Efforts: m.Reasoning.SupportedEfforts,
			DefaultEffort: m.Reasoning.DefaultEffort, SupportsImages: m.SupportsImages,
		}
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		m, ok := dynMap[id]
		if !ok || m.Disabled {
			continue
		}
		out = append(out, ModelInfo{
			ID:             m.ID,
			Name:           m.Name,
			ContextWindow:  m.MaxInputTokens,
			MaxTokens:      m.MaxOutputTokens,
			Efforts:        m.Efforts,
			DefaultEffort:  m.DefaultEffort,
			SupportsImages: m.SupportsImages,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	// 刷新 effort 能力缓存（供请求体降级；无 supportedEfforts 的模型不入 efforts 桶）。
	// 空桶时跳过写：避免「某探测无档位数据」清掉既有桶（例：cn 桶已有档位，再次探测返回全无等级 → 不应清空）。
	cache := make(map[string][]string, len(out))
	defCache := make(map[string]string, len(out))
	for _, mi := range out {
		if len(mi.Efforts) > 0 {
			cache[mi.ID] = mi.Efforts
		}
		if mi.DefaultEffort != "" {
			defCache[mi.ID] = mi.DefaultEffort
		}
	}
	if len(cache) == 0 && len(defCache) == 0 {
		return out, nil
	}
	// 按探测账号的 realm 写入对应桶：CN 探测只进 cn 桶，global 同模型名不被污染（C-2）。
	c.storeEfforts(a.Realm(), cache, defCache)
	return out, nil
}

// billingMeterJSON 按 realm 候选路径发 billing/meter 域请求，ErrNotFound 时换下一候选路径
// （global：/billing/meter/* → /v2/billing/meter/*；cn：单路径 /v2/billing/meter/* 现状）。
func (c *Client) billingMeterJSON(a *auth.Auth, paths []string, method string, body any) (json.RawMessage, error) {
	var lastErr error
	for i, p := range paths {
		data, err := c.billingJSON(a, method, p, body)
		if err != nil {
			lastErr = err
			var ue *Error
			if i < len(paths)-1 && errors.As(err, &ue) && ue.Kind == ErrNotFound {
				continue // /billing/meter/* 404 → 换 /v2/billing/meter/*
			}
			return nil, err
		}
		return data, nil
	}
	return nil, lastErr
}

// UserResource 查询账号当前可花费积分余额（所有套餐 CycleCapacity 聚合，负值钳 0）。
func (c *Client) UserResource(a *auth.Auth) (remain int64, err error) {
	remain, _, err = c.UserResourceDetailed(a, 0)
	return remain, err
}

// CreditBuckets 按到期紧迫度拆分的积分余额（供 pool 优先消耗快过期积分）。
// 背景（issue:积分过期）：套餐/奖励积分按 PackageEndTime 分批过期，总量口径的
// remain 会让"明天就作废"的积分与"30 天后才过期"的积分被无差别选号，
// 导致快过期积分没优先用掉、白白作废。拆桶后选号可优先消耗 Expiring。
type CreditBuckets struct {
	// Expiring 在 soon 窗口内（<= now+soon）即将过期的可用积分。
	Expiring int64
	// Stable 其余有效积分（到期时间更远或无到期时间）。
	Stable int64
}

// Total 返回两桶合计可用积分（= UserResource 的 remain 口径）。
func (b CreditBuckets) Total() int64 { return b.Expiring + b.Stable }

// packageEndLayout 上游 PackageEndTime 的时间格式（与请求体过滤串同口径）。
const packageEndLayout = "2006-01-02 15:04:05"

// UserResourceDetailed 同 UserResource，但按到期时间把余额拆成 CreditBuckets。
// soon>0 时把到期时间 <= now+soon 的套餐余额计入 Expiring；soon<=0 时全部归 Stable。
// PackageEndTime 解析失败/缺失的套餐保守归入 Stable（不误标为快过期而插队）。
// 单套餐取数口径（Cycle* 优先）与 UserResource 完全一致，保证向后兼容。
func (c *Client) UserResourceDetailed(a *auth.Auth, soon time.Duration) (remain int64, buckets CreditBuckets, err error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}
	data, err := c.billingMeterJSON(a, c.billingMeterPaths(a), http.MethodPost, body)
	if err != nil {
		return 0, CreditBuckets{}, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					PackageEndTime      string `json:"PackageEndTime"` // "2006-01-02 15:04:05"，缺省/空 = 无到期
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, CreditBuckets{}, fmt.Errorf("resource parse: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		var r int64
		switch {
		case acct.CycleCapacitySize > 0:
			r = acct.CycleCapacityRemain
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			r = acct.CycleCapacityRemain
		default:
			r = acct.CapacityRemain
		}
		if r < 0 {
			r = 0
		}
		remain += r
		// 分桶：仅 soon>0 且能解析出有效到期时间、且确实在窗口内 → Expiring。
		if soon > 0 && r > 0 && acct.PackageEndTime != "" {
			// 上游时间为 UTC+8 墙钟（与 softRateResetLoc 同口径，官网展示时区）。
			if end, perr := time.ParseInLocation(packageEndLayout, acct.PackageEndTime, softRateResetLoc); perr == nil {
				if !end.After(now.Add(soon)) {
					buckets.Expiring += r
					continue
				}
			}
		}
		buckets.Stable += r
	}
	return remain, buckets, nil
}

// CreditBalance 账号积分余额（所有套餐聚合，负值钳 0）。
// Size 为 0 表示上游未给出周期容量（此时 Remain 即绝对剩余，无法表达百分比）。
type CreditBalance struct {
	Remain int64 `json:"remain"`
	Size   int64 `json:"size"`
}

// CreditBalance 账号积分余额，通过 /status 与 /v1/usage 对外透出。
// 与 UserResource 的差异：本方法额外聚合 size（周期包容量），供百分比展示；
// TotalDosage 是累计发放量、只增不减，不参与（拿它算百分比会偏差）。
// realm 感知继承 billingMeterPaths（global 打 workbuddy.ai，CN 维持 /v2）。
// 上游不可达/解析失败时返回错误且不改动池内余额（不把好数据覆盖成 0）。
func (c *Client) CreditBalance(a *auth.Auth) (CreditBalance, error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	data, err := c.billingMeterJSON(a, c.billingMeterPaths(a), http.MethodPost, body)
	if err != nil {
		return CreditBalance{}, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return CreditBalance{}, fmt.Errorf("resource parse: %w", err)
	}
	var cr CreditBalance
	var size int64
	for _, acct := range resp.Response.Data.Accounts {
		var r, s int64
		switch {
		case acct.CycleCapacitySize > 0:
			r = acct.CycleCapacityRemain
			s = acct.CycleCapacitySize
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			r = acct.CycleCapacityRemain
			s = acct.CycleCapacitySize
		default:
			r = acct.CapacityRemain
			s = acct.CapacitySize
		}
		if r < 0 {
			r = 0
		}
		cr.Remain += r
		size += s
	}
	// 包容量尚未下发时 size 可能小于 remain，钳住避免百分比为负。
	if size < cr.Remain {
		size = cr.Remain
	}
	cr.Size = size
	return cr, nil
}

// ResourceSummary 查询账号积分套餐的完整聚合口径（remain=剩余可花积分、used=已用、
// size=总量、packs=套餐数），供运维工具（cmd/credit）按 realm 展示真实余额。
// 与 UserResource 的差异：UserResource 只取 remain；本方法额外聚合 used/size/packs，
// 且 TotalDosage 作 size 下限（与 cmd/credit 历史口径一致，见其 packageRemainUsed）。
//
// realm 感知继承 billingMeterPaths：global 账号打 workbuddy.ai /billing/meter/*（404
// fallback /v2），CN 账号维持 /v2/billing/meter/get-user-resource（现状逐字，零回归）。
func (c *Client) ResourceSummary(a *auth.Auth) (remain, used, size int64, packs int, err error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	data, err := c.billingMeterJSON(a, c.billingMeterPaths(a), http.MethodPost, body)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				TotalDosage int64 `json:"TotalDosage"`
				Accounts    []struct {
					PackageName         string `json:"PackageName"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("resource parse: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		r, u, s := packageRemainUsed(respAccount{
			CapacityRemain:      acct.CapacityRemain,
			CapacityUsed:        acct.CapacityUsed,
			CapacitySize:        acct.CapacitySize,
			CycleCapacityRemain: acct.CycleCapacityRemain,
			CycleCapacityUsed:   acct.CycleCapacityUsed,
			CycleCapacitySize:   acct.CycleCapacitySize,
		})
		remain += r
		used += u
		size += s
	}
	packs = len(resp.Response.Data.Accounts)
	// TotalDosage 作 size 下限（历史口径：已消耗的不该比总剂量小）。
	if size > 0 {
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	if dosage := resp.Response.Data.TotalDosage; dosage > size {
		size = dosage
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	return remain, used, size, packs, nil
}

// respAccount 供 packageRemainUsed 解析的套餐字段（与 cmd/credit resourcePackage 同构）。
type respAccount struct {
	CapacityRemain      int64
	CapacityUsed        int64
	CapacitySize        int64
	CycleCapacityRemain int64
	CycleCapacityUsed   int64
	CycleCapacitySize   int64
}

// packageRemainUsed 聚合单套餐的 remain/used/size（历史口径见 cmd/credit/billing.go，
// 迁移至此作为单一事实来源）。Cycle 期套餐优先：用 CycleCapacity 三字段，
// used 取 CycleUsed 与 size-remain 的较大者；否则回退 Capacity 三字段。
func packageRemainUsed(a respAccount) (remain, used, size int64) {
	if a.CycleCapacitySize > 0 {
		remain = a.CycleCapacityRemain
		size = a.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if a.CycleCapacityUsed > used {
			used = a.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	remain = a.CapacityRemain
	used = a.CapacityUsed
	size = a.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	_, err := c.billingMeterJSON(a, c.checkinMeterPaths(a), http.MethodPost, map[string]any{})
	return err
}

// IsAlreadyCheckin 报告 err 是否表示"今天已签到"（上游幂等拒绝重复签到）。
// 只认带分类的 *Error（业务 code 或 HTTP 错误）：网络层/解析层错误不得当作幂等成功，
// 否则停机补签遇到抖动会误记为 already，账号当天实际未签到却被判定正常。
func IsAlreadyCheckin(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) {
		return false
	}
	return alreadyCheckinRule.hit(ue.Msg, strings.ToLower(ue.Msg))
}

// UsageRec 一条积分消耗明细（按请求）。
type UsageRec struct {
	RequestTime string  // "2006-01-02 15:04:05"（上游本地 = CST）
	Credit      float64 // 本次扣减积分（如 0.12）
	Model       string
}

// UsageRecords 拉取账号在 [begin, end] 内的按请求积分消耗明细
// （POST /billing/meter/get-user-request-usage，官网「积分消耗明细」同源；
// 注意在 UsageBaseCN 主机、无 /v2 前缀，与 get-user-resource 的 /v2 路径不同）。
// 带分页与上限保护：单账号/区间最多 maxUsagePages×pageSize 条，超出截断（趋势聚合仍成样）。
func (c *Client) UsageRecords(a *auth.Auth, begin, end time.Time) ([]UsageRec, error) {
	const (
		pageSize     = 100
		maxUsagePage = 10 // 1000 条/账号/区间封顶；响应含提示词快照，控制带宽
	)
	var out []UsageRec
	total := 0
	for page := 1; page <= maxUsagePage; page++ {
		body := map[string]any{
			"startTime": begin.Format("2006-01-02") + " 00:00:00",
			"endTime":   end.Format("2006-01-02") + " 23:59:59",
			"pageNum":   page,
			"pageSize":  pageSize,
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.UsageBaseCN+"/billing/meter/get-user-request-usage", bytes.NewReader(raw))
		if err != nil {
			cancel()
			return nil, err
		}
		c.BillingHeaders(req, a)
		data, err := c.doJSON(req)
		if err != nil {
			cancel()
			return nil, err
		}
		var shape struct {
			Total int `json:"total"`
			Data  []struct {
				RequestTime string      `json:"requestTime"`
				Credit      json.Number `json:"credit"`
				Model       string      `json:"model"`
			} `json:"data"`
		}
		err = json.Unmarshal(data, &shape)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("request usage parse: %w", err)
		}
		total = shape.Total
		for _, r := range shape.Data {
			v, err := r.Credit.Float64()
			if err != nil {
				continue
			}
			out = append(out, UsageRec{RequestTime: r.RequestTime, Credit: v, Model: r.Model})
		}
		if len(shape.Data) == 0 || len(out) >= total {
			break
		}
	}
	return out, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
