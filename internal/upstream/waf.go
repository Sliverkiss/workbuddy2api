// waf.go 出站请求体 腾讯云 WAF 特征中和（wafNeutralize）。
//
// 背景（对应 issue #119「国际版 WAF 特征拦截」）：global 域上游节点前置 腾讯云 WAF
// 托管规则，对请求 body 做关键词级 XSS/注入检测。前端项目源码天然携带 <script>、
// <template>、onerror= 等标记，确定性触发 → 上游 403 → 客户端见 529。CN 域不拦，
// 属两端风控策略差异（非账号状态、非本网关转发逻辑缺陷）。
//
// 在特征串内部插入零宽空格 U+200B（wafBreak）。实测 腾讯云 WAF 匹配前做归一化——
// 剥离空白与反斜杠，所以插普通空格/反斜杠全部无效（"< script" / "<\script" 仍 403），
// 而零宽空格与全角字符在归一化后存活："<"+ZWSP+"script>" 正常放行。选零宽空格：
// 对模型 tokenizer 基本不可见（阅读无损）、四类规则统一可用、幂等（ZWSP 不属 [\s\\]，
// 破坏后不再命中）。
//
// 三条铁律（与 sanitize.go 同哲学）：
//   - 只作用于出站序列化副本：PrepareBody 在 json.Unmarshal 出的 obj 上改写，原始
//     src 不动；会话粘性/查找的指纹在原文上计算，不受影响。
//   - 模型可读：中和后文本仍能被模型逐字读回（编辑类工具调用依赖回读内容）。
//   - 幂等：同串重复中和不产生二次破坏。
//
// 注意：本仓库 payload.go 用默认 json.Marshal（SetEscapeHTML=true），出站时 "<" 会被
// 转成 "<"。规则 1/2 同时匹配裸 "<" 与字面量 "<"，覆盖「中和前已被上层
// json 序列化成转义形」的内容（如字符串化的 tool_calls.arguments）。
package upstream

import (
	"regexp"
	"strings"
)

// wafBreak 特征破坏字符：零宽空格 U+200B。
const wafBreak = "\u200b"

// wafNeutralizeRules 按特征类别（而非逐特征）破坏形态：每条用捕获组夹住破坏点，
// replace 模板在组间插入 wafBreak。扩特征只需往交替组里加词。规则集与破坏边界均为
// 直连上游探针实测所得，逐条保留其定案理由。
var wafNeutralizeRules = []struct {
	pattern *regexp.Regexp
	replace string
}{
	// 1. 危险标签开头（含 json.Marshal 产生的 < 字面量转义形）。
	//    无 \b 词边界：腾讯云 WAF 归一化剥空格后是纯前缀匹配——"<imgsrc="、"<imgonerror" 这类
	//    「标签名后紧跟词字符」\b 不命中但 腾讯云 WAF 照拦。改前缀命中即插 ZWSP，误伤面（<imgx
	//    之类非标签词）只多一个不可见字符、阅读无损；漏伤代价是 403。词内连续性由 腾讯云 WAF
	//    归一化定义、不由 HTML 规范定义，边界必须跟它对齐。
	{regexp.MustCompile(`(?i)(<|\\u003c)([\s\\]*[!/?]?[\s\\]*(?:script|iframe|svg|template|object|embed|form|style|link|meta|base|img|input|body|html|doctype))`), "${1}" + wafBreak + "${2}"},
	// 2. script 分离形：腾讯云 WAF 归一化剥掉空白与反斜杠后，"< script" / "<scr ipt" / "<\script"
	//    全部还原为 "<script" 确定性 403，而规则 1 在 < 与标签名之间不容忍分隔符会穿透。
	//    分离符集 [\s\\] 恰为归一化剥掉的字符；只逐字母容忍 script（唯一实测标签签名）。
	{regexp.MustCompile(`(?i)(<|\\u003c)([\s\\]*[!/?]?[\s\\]*s[\s\\]*c[\s\\]*r[\s\\]*i[\s\\]*p[\s\\]*t)`), "${1}" + wafBreak + "${2}"},
	// 3. 行内事件处理器 on任意=。
	{regexp.MustCompile(`(?i)(\bon[a-z]+)(\s*=)`), "${1}" + wafBreak + "${2}"},
	// 4. 危险 URI 指令。
	{regexp.MustCompile(`(?i)(javascript|vbscript)(:)`), "${1}" + wafBreak + "${2}"},
	// 5. Vue 指令（v-on: 与 @click 简写；@ 后只认 click/冒号，不误伤邮箱）。
	{regexp.MustCompile(`(?i)(v-on|@)(:|click)`), "${1}" + wafBreak + "${2}"},
	// 6. bin/cat 命令路径形（cat 是唯一触发命令）：ZWSP 插在 bin/ 与 cat 之间；
	//    bin/ls、bin/sh、bin/rm、裸 cat 均放行。
	{regexp.MustCompile(`(?i)(s?bin/)(cat)`), "${1}" + wafBreak + "${2}"},
	// 7. shell 命令注入形：反引号/代码围栏上下文里的 curl/wget + 单连字符 flag + 参数
	//    触发 腾讯云 WAF 命令注入托管规则。flag 无值/长 flag/裸命令（无 backtick）不误伤。
	//    backtick 群与命令间允许 ≤40 字符（覆盖 ` curl 与 ```\n curl）。
	//    ZWSP 插在命令词**内部**（cur​l / wge​t，即末字母前）而非词前：ps2Api 原始规则把
	//    整词作一组、ZWSP 插词前，会被 [^`]* 前缀吞掉后重复命中而不幂等（多轮对话上游回显
	//    ZWSP 内容再发时 ZWSP 会累积）。词内破坏后 cu[\w]*r 再也接不上末字母，重复调用不再
	//    命中——满足幂等铁律，且 cur​l ≠ curl 同样穿不过 腾讯云 WAF（ZWSP 不在 腾讯云 WAF 归一化剥除集内）。
	//    curl / wget 拆两条以保持 cu…rl、wg…et 的原始配对（不放宽成 cu…t / wg…l）。
	{regexp.MustCompile("(?s)(`{1,3}[^`]{0,40}?cu[\\w]*r)(l)(\\s+-[^\\s-][^\\s]*\\s+[^\\s])"), "${1}" + wafBreak + "${2}${3}"},
	{regexp.MustCompile("(?s)(`{1,3}[^`]{0,40}?wg[\\w]*e)(t)(\\s+-[^\\s-][^\\s]*\\s+[^\\s])"), "${1}" + wafBreak + "${2}${3}"},
	// 8. JS 注入 sink 函数名：实测腾讯云 WAF 拦 alert/eval/confirm/String.fromCharCode
	//    （prompt / document.cookie 不拦）。issue #119 列的 alert() 属此类——这些词不含 <、
	//    不受 json 转义，原样出站，单独出现即 403（kill-switch 生产实测：关中和 403、开中和
	//    401 通过）；规则 1/2 只破标签、够不到函数名，是真实覆盖缺口。ZWSP 插末字母前
	//    （aler|t( → aler[ZWSP]t(）：xxx( 接不上末字母，重复中和不再命中 → 幂等；后随 ( 限制
	//    误伤（alerts/evaluate/confirmation 等普通词无紧邻左括号不命中）。破坏边界经生产 WAF
	//    实测 403→401 逐条验证。
	{regexp.MustCompile(`(?i)(aler)(t\s*\()`), "${1}" + wafBreak + "${2}"},
	{regexp.MustCompile(`(?i)(eva)(l\s*\()`), "${1}" + wafBreak + "${2}"},
	{regexp.MustCompile(`(?i)(confir)(m\s*\()`), "${1}" + wafBreak + "${2}"},
	{regexp.MustCompile(`(?i)(fromCharCod)(e\s*\()`), "${1}" + wafBreak + "${2}"},
}

// wafNeutralize 破坏字符串里的 WAF 特征形态（大小写不敏感）。不含特征的字符串原样返回。
// 不做命中预判：4 条规则对干净文本近零开销，且探测表认不出闭合标签 "</script>"-only 等
// 载荷，预判门会漏放。
func wafNeutralize(s string) string {
	for _, rule := range wafNeutralizeRules {
		s = rule.pattern.ReplaceAllString(s, rule.replace)
	}
	return s
}

// wafNeutralizeMessages 对 messages 的全部文本出口套用 wafNeutralize，任一改动返回 true。
// 出口与 sanitizeMessages 同口径（content string / 多模态 text part / reasoning_content /
// tool_calls[].function.arguments），刻意保持独立遍历：WAF 中和与指纹脱敏受不同开关控制、
// 可各自演进；两者都只改出站副本、原文不动。
func wafNeutralizeMessages(messages []any) bool {
	changed := false
	neutralize := func(s string) (string, bool) {
		n := wafNeutralize(s)
		return n, n != s
	}
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		if c, ok := m["content"]; ok {
			switch cv := c.(type) {
			case string:
				if n, ch := neutralize(cv); ch {
					m["content"] = n
					changed = true
				}
			case []any:
				for _, p := range cv {
					pm, ok := p.(map[string]any)
					if !ok {
						continue
					}
					if text, ok := pm["text"].(string); ok {
						if n, ch := neutralize(text); ch {
							pm["text"] = n
							changed = true
						}
					}
				}
			}
		}
		if rc, ok := m["reasoning_content"].(string); ok {
			if n, ch := neutralize(rc); ch {
				m["reasoning_content"] = n
				changed = true
			}
		}
		if tc, ok := m["tool_calls"].([]any); ok {
			for _, c := range tc {
				call, ok := c.(map[string]any)
				if !ok {
					continue
				}
				fn, ok := call["function"].(map[string]any)
				if !ok {
					continue
				}
				if args, ok := fn["arguments"].(string); ok {
					if n, ch := neutralize(args); ch {
						fn["arguments"] = n
						changed = true
					}
				}
			}
		}
	}
	return changed
}

// --- 入站对称处理（inbound）：剥离上游回显的 wafBreak ---
//
// 出站 wafNeutralize 往特征串里插 ZWSP 骗过腾讯云 WAF；上游模型读到后可能在响应里
// 原样回显这些 ZWSP——尤其编辑类工具调用（write/edit）的 tool_calls.arguments 里带回
// 被插了 ZWSP 的源码片段。若不剥离，客户端 agent 把响应写回文件时 ZWSP 会落进源码
// 造成不可见损坏（编辑/diff/编译/grep 都可能异常）。入站在所有响应文本出口统一剥掉，
// 保证代码经「出站中和 → 上游 → 入站剥离」后逐字节干净往返。
// 出口与 wafNeutralizeMessages 的入口严格对称：中和与剥离是一对（插入=出站、剥离=入站），
// 始终成对生效，无开关——出站插了 ZWSP 就必须在入站原样剥回，否则污染必然落地。

// wafStripBreaks 移除串内全部 wafBreak（U+200B），是 wafNeutralize 的逆操作。
// 幂等且零误伤：不含 ZWSP 的串走 Contains 短路原样返回（不产生分配）。
func wafStripBreaks(s string) string {
	if !strings.Contains(s, wafBreak) {
		return s
	}
	return strings.ReplaceAll(s, wafBreak, "")
}

// wafStripToolCallBreaks 剥离单个 tool_call 的 function.arguments 里的 wafBreak。
// arguments 是字符串化 JSON，编辑类工具的源码正文在此——是回写文件的主污染面。
// 非 map / 无 function / arguments 非字符串 → 静默跳过（形态不符即无可剥）。
func wafStripToolCallBreaks(call map[string]any) {
	fn, ok := call["function"].(map[string]any)
	if !ok {
		return
	}
	if args, ok := fn["arguments"].(string); ok {
		fn["arguments"] = wafStripBreaks(args)
	}
}

// wafStripFrameBreaks 剥离单个流式 chunk 里所有文本出口的 wafBreak：choices[].delta
// 的 content / reasoning_content / refusal，以及 tool_calls[].function.arguments。
// 就地改写传入的 obj（与 stripToolCallNames / normalizeFrame 的透出字段对齐）。
// 逐帧调用即可：ZWSP 是单个 3 字节 UTF-8 字符，必落在单帧的合法 JSON 串内、不跨帧
// 切分（跨帧会使该帧 JSON 非法、writeFrame 解析失败而原样透传），故逐帧剥离与整流
// 剥离等价——透传流不能缓冲整流。
func wafStripFrameBreaks(obj map[string]any) {
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		delta, _ := c["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if s, ok := delta["content"].(string); ok {
			delta["content"] = wafStripBreaks(s)
		}
		if s, ok := delta["reasoning_content"].(string); ok {
			delta["reasoning_content"] = wafStripBreaks(s)
		}
		if s, ok := delta["refusal"].(string); ok {
			delta["refusal"] = wafStripBreaks(s)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, tci := range tcs {
				if call, ok := tci.(map[string]any); ok {
					wafStripToolCallBreaks(call)
				}
			}
		}
	}
}
