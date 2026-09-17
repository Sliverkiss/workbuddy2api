// Package logfmt 统一网关日志的 uid 截断与模块前缀约定。
//
// 约定：
//   - uid 统一截 8 位：与 chat 流水行（internal/server/logging.go uidPrefix）对齐，
//     日志行只留 uid 前 8 位。全量 uid 可从 data/state.json 查（54 个号无 8 位前缀碰撞）。
//   - 模块前缀：调度四类已有天然前缀（travel/activity/checkin/keepalive）保持；
//     其他补 [pool]/[auth]/[server] 等 [mod] 方括号前缀，redisstore/session 已有保持。
//   - 级别语义：正常流转不打级别字样（保持简洁）；可疑/降级/失败行加 WARN:/ERR: 前缀。
//
// 本包不引入日志库，只提供 UID8 截断 / Label 账号标签 / Pad 显示宽对齐三个纯字符串
// helper，供各包替代裸写 [:8] 与手算表格列宽（防 uid 短于 8 越界、防中文昵称错位）。
package logfmt

import (
	"strings"
	"unicode/utf8"
)

// Truncate 截断字符串到 n 字节上限（先 TrimSpace，与旧 upstream/内部实现口径
// 一致），切点落在多字节字符中间时回退到 UTF-8 rune 边界——错误 body 多为中文
// （"将在 … 重置"），按字节切会出半截序列乱码。短于 n 原样返回；n<=0 返回空串。
func Truncate(s string, n int) string {
	// 入口守卫：n<=0 一律返回空串（与上方文档承诺一致）。
	// 缺这道守卫时负数会落到 len(s) > n 分支（对负数恒成立），再经 s[:n] 触发
	// 「slice bounds out of range [:-1]」panic——rune 回退循环的 n>0 条件挡不住它。
	// 与同文件 Pad 的 width<=0 守卫同口径：本包 helper 的定位正是「防越界」，
	// 调用点一旦传入计算得来的 n（而非字面量）即成崩溃路径。
	if n <= 0 {
		return ""
	}
	s = strings.TrimSpace(s)
	if len(s) > n {
		// s[n] 是切点后的首字节：是 rune 的后续字节（continuation）说明切点落在
		// 多字节字符中间，逐字节回退到 rune 边界（该字符整个让出）。
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		return s[:n]
	}
	return s
}

// UID8 返回 uid 的前 8 位；空 uid 返回 "-"（与 server.uidPrefix 对齐）。
//
// 用于调度类与非调度类日志行，把 <task> <full-uid>: ... 改为 <task> <uid8>: ...
// 全量 uid 留在 state.json 供排查，日志里 8 位足够唯一定位。
func UID8(uid string) string {
	if uid == "" {
		return "-"
	}
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// Label 返回日志里的账号标签，形如 "示例昵称甲(a1b2c3d4)"；昵称为空时退回 "a1b2c3d4"。
//
// 为什么需要：uid8 是机器标识，排障时人眼无法直接判断"刚才那个 429/6004 是哪个号"，
// 必须再拿 uid8 去 auths/ 或 data/state.json 反查昵称，一条日志要多跳一步。昵称随
// 登录落在 auths/<uid>.json 的 account.nickname，这里把它与 uid8 拼成可直接辨认的
// 标签——昵称认人、uid8 供 grep，两者都保留。
//
// uid 与 nick 同时为空时返回 "-"（与 UID8 口径一致，避免打出 "(-)"）。
func Label(uid, nick string) string {
	short := UID8(uid)
	nick = strings.TrimSpace(nick)
	if nick == "" {
		return short
	}
	return nick + "(" + short + ")"
}

// DisplayWidth 返回 s 的终端显示列宽：CJK / 全角 / emoji 记 2 列，其余记 1 列。
//
// 存在意义：账号昵称是用户自定的中文（"猫" 是 3 字节但占 2 列，"sample" 是 6 字节占
// 6 列），用 len()（字节数）做表格对齐会导致列宽忽宽忽窄。Go 标准库没有显示宽度函数，
// 本仓库不引 go-runewidth（保持零第三方依赖），故内置这份覆盖常见宽字符区段的判定。
func DisplayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// Pad 把 s 右补空格到 width 显示列宽；已超宽或 width<=0 时原样返回（不截断）。
// 只补不截：截断会丢信息，超宽时让该行自然变宽，保持内容完整。
func Pad(s string, width int) string {
	if width <= 0 {
		return s
	}
	if d := width - DisplayWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// runeWidth 单个 rune 的显示列宽。区段判定取自 Unicode East Asian Width 的
// Wide/Fullwidth 集合（与 go-runewidth 的默认表口径一致），只保留实际会用到的段。
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 0x20 || (r >= 0x7f && r < 0xa0):
		// 控制字符（含 DEL/C1）不占位：日志里若混入 \t \r 不破坏列宽计算。
		return 0
	case r < 0x1100:
		return 1
	case r <= 0x115f: // Hangul Jamo 初声
		return 2
	case r == 0x2329 || r == 0x232a:
		return 2
	case r >= 0x2e80 && r <= 0xa4cf && r != 0x303f: // CJK 部首…Yi（303f 是窄字符）
		return 2
	case r >= 0xac00 && r <= 0xd7a3: // Hangul 音节
		return 2
	case r >= 0xf900 && r <= 0xfaff: // CJK 兼容表意
		return 2
	case r >= 0xfe30 && r <= 0xfe6f: // CJK 兼容形式
		return 2
	case r >= 0xff00 && r <= 0xff60: // 全角 ASCII
		return 2
	case r >= 0xffe0 && r <= 0xffe6: // 全角符号
		return 2
	case r >= 0x1f300 && r <= 0x1f9ff: // emoji
		return 2
	case r >= 0x20000 && r <= 0x3fffd: // CJK 扩展 B 及以后
		return 2
	}
	return 1
}
