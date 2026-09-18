// main_test.go acct 工具的纯函数契约：listen 归一化 + 状态文案。
package main

import (
	"testing"
)

// TestNormalizeListen listen 的各种写法都要收敛成本机可访问的 http 基址。
// 通配地址（空 host / 0.0.0.0 / ::）必须收敛到回环——本工具总是与网关同机运行，
// 用 0.0.0.0 发请求在部分平台会失败。
func TestNormalizeListen(t *testing.T) {
	cases := []struct{ in, want string }{
		{":7863", "http://127.0.0.1:7863"},
		{"0.0.0.0:7863", "http://127.0.0.1:7863"},
		{"::", "http://127.0.0.1:7863"},
		{"[::]:7863", "http://127.0.0.1:7863"},
		{"127.0.0.1:7863", "http://127.0.0.1:7863"},
		{"localhost:9999", "http://localhost:9999"},
		{"", "http://127.0.0.1:7863"},           // 空 → 默认端口
		{"127.0.0.1:", "http://127.0.0.1:7863"}, // 有 host 无端口
		{"7863", "http://127.0.0.1:7863"},       // 只有端口（无冒号 host 段）
	}
	for _, c := range cases {
		if got := normalizeListen(c.in); got != c.want {
			t.Errorf("normalizeListen(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestStateLabel 双位状态文案：叠加态两个都列，不合并。
func TestStateLabel(t *testing.T) {
	cases := []struct {
		name string
		in   accountStatus
		want string
	}{
		{"正常", accountStatus{}, "正常"},
		{"仅手动", accountStatus{ManualDisabled: true}, "手动停用"},
		{"仅自动", accountStatus{Disabled: true}, "自动禁用"},
		{"叠加", accountStatus{ManualDisabled: true, Disabled: true}, "手动停用 + 自动禁用"},
		{"带原因", accountStatus{ManualDisabled: true, ManualReason: "观察"}, "手动停用(观察)"},
		{"冷却", accountStatus{Cooling: true}, "冷却中"},
		{"三者", accountStatus{ManualDisabled: true, Disabled: true, Cooling: true},
			"手动停用 + 自动禁用 + 冷却中"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stateLabel(c.in); got != c.want {
				t.Errorf("stateLabel() = %q, want %q", got, c.want)
			}
		})
	}
}
