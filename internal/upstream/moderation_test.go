package upstream

import "testing"

// moderationBlockRule 分类回归：内容审核拦截（账号级信号）必须命中 ErrModerationBlocked，
// 且先于 404/5xx 状态码兜底——生产实测形态以 500 + "Internal error" 信封携带
// （statusCode 400 在 data 内），不前置会被 5xx 吞成 ErrServer（喂熔断且不进隔离计数）。
func TestClassifyModerationBlocked(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{
			name:   "400 生产实测形态（statusCode 400 在 data 内）",
			status: 400,
			body:   `{"code":-32603,"message":"Internal error","data":{"code":0,"message":"内容未通过安全审核，请调整后重试。","request_id":"wb2-18d6636971444eac-85","type":"invalid_request_error","details":"400 内容未通过安全审核，请调整后重试。","statusCode":400,"category":"internal"}}`,
			want:   ErrModerationBlocked,
		},
		{
			name:   "500 同文案（防被 5xx 兜底吞成 ErrServer）",
			status: 500,
			body:   `{"code":-32603,"message":"Internal error","data":{"message":"内容未通过安全审核，请调整后重试。","statusCode":400}}`,
			want:   ErrModerationBlocked,
		},
		{
			name:   "变体措辞「内容审核未通过」",
			status: 400,
			body:   `{"data":{"message":"内容审核未通过，请调整后重试。"}}`,
			want:   ErrModerationBlocked,
		},
		{
			name:   "11128 指纹误报仍是 ErrContentBlocked（不走隔离通道）",
			status: 400,
			body:   `{"error":{"data":{"code":11128,"msg":"blocked by security policy"}}}`,
			want:   ErrContentBlocked,
		},
		{
			name:   "良性 400 不受审核词表误伤",
			status: 400,
			body:   `{"code":11101,"msg":"Unmarshal chat params failed"}`,
			want:   ErrBadParams,
		},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("%s: Classify(%d,…)=%s want %s", c.name, c.status, got, c.want)
		}
	}
}

// gateway_hint 覆盖：moderation_blocked 有 hint（轮转/隔离指向）。
func TestGatewayHintModerationBlocked(t *testing.T) {
	if h := GatewayHint(ErrModerationBlocked, "", HintContext{}); h == "" {
		t.Errorf("ErrModerationBlocked should carry a gateway hint")
	}
}
