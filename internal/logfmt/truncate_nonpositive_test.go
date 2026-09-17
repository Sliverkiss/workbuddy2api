package logfmt

import "testing"

// TestTruncateNonPositiveReturnsEmpty 钉住 Truncate 自身文档承诺的契约：
// 「短于 n 原样返回；n<=0 返回空串」。
//
// n<0 此前不返回空串而是 panic：len(s) > n 对负数恒成立，随后 n 不满足 rune
// 回退循环的 n>0 条件，直接 s[:n] 触发 slice bounds out of range。该 helper 所在
// 包的存在目的正是「防越界」（见包注释），同文件的 Pad 也有 width<=0 守卫，此处
// 缺同一道守卫——调用点一旦传入计算得来的 n（而非字面量）即成崩溃路径。
func TestTruncateNonPositiveReturnsEmpty(t *testing.T) {
	inputs := []string{"", "abc", "中文昵称甲"}
	for _, n := range []int{0, -1, -5, -1000} {
		for _, s := range inputs {
			got := Truncate(s, n)
			if got != "" {
				t.Errorf("Truncate(%q, %d) = %q，期望空串（n<=0 契约）", s, n, got)
			}
		}
	}
}

// TestTruncatePositiveBehaviorUnchanged 正数路径行为不得改变（回归锚）。
func TestTruncatePositiveBehaviorUnchanged(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"abcdefghijklmn", 10, "abcdefghij"},
		{"  短  ", 10, "短"},
		{"中文截断", 4, "中"}, // 切点落在第 2 个汉字中间 → 回退 rune 边界
		{"", 10, ""},
	}
	for _, c := range cases {
		if got := Truncate(c.s, c.n); got != c.want {
			t.Errorf("Truncate(%q, %d) = %q，期望 %q", c.s, c.n, got, c.want)
		}
	}
}
