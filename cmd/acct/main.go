// acct — 账号运维工具：临时停用 / 恢复 / 复活（issue #138/#118）。
//
// 用法:
//
//	acct list                          # 列出账号与双位状态
//	acct disable <uid> [reason]        # 临时停用（对话流量摘除，保留在池里）
//	acct enable  <uid>                 # 解除手动停用
//	acct revive  <uid>                 # 解除系统自动禁用
//
// 配置:
//
//	-config 配置路径（默认 config.json），从中读 listen 与 api_key
//	-server 直接指定网关地址（覆盖 config 的 listen），如 http://127.0.0.1:7863
//
// 为什么走 HTTP 而不是直接改 state.json：手动停用是**运行中进程的内存状态**，
// 由池的定时 flush 落盘（5s 周期）。外部直接改文件会在下一次 flush 被覆盖——
// 这正是 issue #138 里面板侧绕不过去的死路。经端点操作才能可靠生效并被持久化。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type accountStatus struct {
	UID            string `json:"uid"`
	Realm          string `json:"realm"`
	Nickname       string `json:"nickname"`
	Credits        int64  `json:"credits"`
	Disabled       bool   `json:"disabled"`
	DisabledReason string `json:"disabled_reason"`
	ManualDisabled bool   `json:"manual_disabled"`
	ManualReason   string `json:"manual_reason"`
	Cooling        bool   `json:"cooling"`
}

type adminState struct {
	UID            string `json:"uid"`
	ManualDisabled bool   `json:"manual_disabled"`
	ManualReason   string `json:"manual_reason"`
	Disabled       bool   `json:"disabled"`
	Changed        bool   `json:"changed"`
}

// configWire 只声明本工具需要的字段（避免耦合完整配置结构）。
type configWire struct {
	Listen string `json:"listen"`
	APIKey string `json:"api_key"`
}

func main() {
	var (
		cfgPath = flag.String("config", "config.json", "path to config json")
		server  = flag.String("server", "", "gateway base URL (overrides config listen)")
	)
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	base, apiKey, err := resolveTarget(*cfgPath, *server)
	if err != nil {
		fmt.Fprintf(os.Stderr, "acct: %v\n", err)
		os.Exit(1)
	}

	op := args[0]
	switch op {
	case "list", "ls":
		if err := listAccounts(base, apiKey); err != nil {
			fatalf("%v", err)
		}
	case "disable", "enable", "revive":
		if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
			fmt.Fprintf(os.Stderr, "acct: %s 需要一个 uid\n", op)
			os.Exit(2)
		}
		uid := strings.TrimSpace(args[1])
		var reason string
		if op == "disable" && len(args) > 2 {
			reason = strings.TrimSpace(strings.Join(args[2:], " "))
		}
		if err := doAdmin(base, apiKey, op, uid, reason); err != nil {
			fatalf("%v", err)
		}
	default:
		fmt.Fprintf(os.Stderr, "acct: 未知子命令 %q\n", op)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `acct — 账号运维工具

用法:
  acct list                    列出账号与状态（含 manual_disabled / disabled 双位）
  acct disable <uid> [reason]  临时停用：摘出选号池，但仍留在池里
  acct enable  <uid>           解除手动停用
  acct revive  <uid>           解除系统自动禁用

选项:
  -config <path>  配置路径（默认 config.json，读 listen 与 api_key）
  -server <url>   直接指定网关地址，如 http://127.0.0.1:7863

注意：enable 只解手动位。若账号同时被系统自动禁用（disabled），
需要额外执行 revive 才能回到选号池。
`)
}

// resolveTarget 定出网关地址与 api_key：-server 优先，否则读 config 的 listen。
// api_key 始终来自 config（-server 只换地址，共享同一把 key）。
func resolveTarget(cfgPath, serverOverride string) (string, string, error) {
	var cfg configWire
	if raw, err := os.ReadFile(cfgPath); err == nil {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return "", "", fmt.Errorf("parse %s: %w", cfgPath, err)
		}
	} else if serverOverride == "" {
		return "", "", fmt.Errorf("读配置失败（%v）；可用 -server 直接指定网关地址", err)
	}

	base := strings.TrimSpace(serverOverride)
	if base == "" {
		base = normalizeListen(cfg.Listen)
	}
	return strings.TrimRight(base, "/"), cfg.APIKey, nil
}

// normalizeListen 把配置里的 listen（":7863" / "0.0.0.0:7863" / "127.0.0.1:7863"）
// 归一成本机可访问的 http 基址。监听通配地址时收敛到回环——本工具总是和网关同机运行，
// 往 0.0.0.0 / :: 发请求在部分平台会直接失败。
//
// 用 net.SplitHostPort 而非手工切冒号：IPv6 字面量（"::" / "[::]:7863"）本身含冒号，
// `strings.LastIndex(listen, ":")` 会把 "::" 切成 host=":" port=""，拼出
// "http://::7863" 这种非法基址（测试 TestNormalizeListen 抓到过）。
func normalizeListen(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return "http://127.0.0.1:7863"
	}
	host, port := "", ""
	if h, p, err := net.SplitHostPort(listen); err == nil {
		host, port = h, p
	} else {
		// 无冒号（"7863"）或畸形：把纯数字整体当端口，否则当 host。
		if _, convErr := strconv.Atoi(listen); convErr == nil {
			port = listen
		} else {
			// SplitHostPort 失败也可能是 "[::]:x" 这类缺端口的写法，退一步处理。
			host = strings.Trim(strings.TrimSuffix(listen, ":"), "[]")
		}
	}
	if port == "" {
		port = "7863"
	}
	switch host {
	// ":" 是裸 "::" 经 SplitHostPort 的产物（Go 把 "::" 解析为 host=":"）；
	// 这些写法都表示「监听全部网卡」，统一收敛到回环。
	case "", "0.0.0.0", "::", ":":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func httpDo(base, apiKey, method, path string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("请求网关失败（%v）；确认服务在运行、-server/-config 指向正确", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// listAccounts 拉 /status 并打印账号表。
func listAccounts(base, apiKey string) error {
	code, raw, err := httpDo(base, apiKey, http.MethodGet, "/status", nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("GET /status 返回 %d: %s", code, firstLine(raw))
	}
	var body struct {
		Accounts []accountStatus `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("解析 /status: %w", err)
	}
	if len(body.Accounts) == 0 {
		fmt.Println("（池里没有账号）")
		return nil
	}
	sort.Slice(body.Accounts, func(i, j int) bool {
		return body.Accounts[i].UID < body.Accounts[j].UID
	})
	fmt.Printf("%-38s %-7s %-18s %8s  %s\n", "UID", "REALM", "NICKNAME", "CREDITS", "STATE")
	for _, a := range body.Accounts {
		fmt.Printf("%-38s %-7s %-18s %8d  %s\n",
			a.UID, strings.ToUpper(a.Realm), truncate(a.Nickname, 18), a.Credits, stateLabel(a))
	}
	return nil
}

// stateLabel 把双位状态压成一行可读文案。叠加态两个都列（不合并）。
func stateLabel(a accountStatus) string {
	var parts []string
	if a.ManualDisabled {
		s := "手动停用"
		if a.ManualReason != "" {
			s += "(" + a.ManualReason + ")"
		}
		parts = append(parts, s)
	}
	if a.Disabled {
		s := "自动禁用"
		if a.DisabledReason != "" {
			s += "(" + a.DisabledReason + ")"
		}
		parts = append(parts, s)
	}
	if a.Cooling {
		parts = append(parts, "冷却中")
	}
	if len(parts) == 0 {
		return "正常"
	}
	return strings.Join(parts, " + ")
}

// doAdmin 调管理端点并把结果打成一行。
func doAdmin(base, apiKey, op, uid, reason string) error {
	action := op
	path := "/admin/accounts/" + uid + "/" + action
	var payload []byte
	if op == "disable" && reason != "" {
		payload, _ = json.Marshal(map[string]string{"reason": reason})
	}
	code, raw, err := httpDo(base, apiKey, http.MethodPost, path, payload)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusNotFound:
		// 两种 404：admin 开关没开 / uid 不存在。都不该靠猜，把原文给出来。
		return fmt.Errorf("404：%s（可能是 admin.enabled 未开启，或该 uid 不在池里）", firstLine(raw))
	case http.StatusUnauthorized:
		return fmt.Errorf("401：api_key 不对（检查 config.json 的 api_key）")
	case http.StatusOK:
	default:
		return fmt.Errorf("%d: %s", code, firstLine(raw))
	}

	var st adminState
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("解析响应: %w", err)
	}
	verb := map[string]string{"disable": "已停用", "enable": "已解除停用", "revive": "已复活"}[op]
	if !st.Changed {
		verb = map[string]string{"disable": "本就是停用态", "enable": "本就是启用态", "revive": "本就未被自动禁用"}[op]
	}
	fmt.Printf("%s %s: %s\n", verb, st.UID, stateLabel(accountStatus{
		UID: st.UID, ManualDisabled: st.ManualDisabled, ManualReason: st.ManualReason,
		Disabled: st.Disabled,
	}))
	// 提示遗留的另一个位：enable 不解除自动禁用，反之亦然。
	if op == "enable" && st.Disabled {
		fmt.Println("提示：该账号仍被系统自动禁用，要回到选号池还需执行 acct revive " + st.UID)
	}
	if op == "revive" && st.ManualDisabled {
		fmt.Println("提示：该账号仍处于手动停用，要回到选号池还需执行 acct enable " + st.UID)
	}
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func firstLine(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 200)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "acct: "+format+"\n", args...)
	os.Exit(1)
}
