package auth

import "testing"

// IsGlobal 按 domain 后缀判域；空 domain 回落 CN（历史 CN 凭证无该字段）。
func TestIsGlobal(t *testing.T) {
	cases := []struct {
		domain string
		global bool
	}{
		{"copilot.tencent.com", false}, // CN 登录返回的 domain
		{"", false},                    // 空 domain 回落 CN
		{"www.codebuddy.cn", false},
		{"abc.workbuddy.ai", true}, // 国际版
		{"www.workbuddy.ai", true},
		{"copilot.tencent.com.evil.workbuddy.ai", true}, // 后缀匹配
	}
	for _, c := range cases {
		a := &Auth{Domain: c.domain}
		if got := a.IsGlobal(); got != c.global {
			t.Errorf("domain=%q: IsGlobal=%v, want %v", c.domain, got, c.global)
		}
		if want := map[bool]string{true: "global", false: "cn"}[c.global]; a.Realm() != want {
			t.Errorf("domain=%q: Realm=%q, want %q", c.domain, a.Realm(), want)
		}
	}
}
