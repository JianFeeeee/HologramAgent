package core

// provider 回退时**model 必须跟着 provider 走** + 403 分两类处置。
//
// 真机现场（2026-10-27）：
//   deepseek 402 余额不足
//     → 回退 llmsproxy，但 req 里仍带 model="deepseek-flash"
//     → llmsproxy 报 403 model_not_allowed
//     → llmsproxy 被整源标记 unavailable（冷却）
//     → 视觉回退 resolveModalFallback 跳过它
//     → 用户看到"配置了视觉能力却说没有视觉"
//
// 这两条判据分别钉住：换 provider 要换 model；model 级错误不能把整源判死。

import (
	"errors"
	"testing"
)

func TestIsModelNotAllowedRecognizesGatewayPhrases(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{`api error 403: {"error":{"message":"model \"deepseek-flash\" is not allowed for this key","type":"model_not_allowed"}}`, true},
		{`api error 403: {"error":{"type":"model_not_allowed"}}`, true},
		{`api error 404: {"error":{"message":"model_not_found"}}`, true},
		{`api error 400: The model ` + "`gpt-9`" + ` does not exist or you do not have access to it`, true},
		{`api error 401: invalid api key`, false}, // 凭证问题：整源不可用
		{`api error 403: forbidden`, false},       // 权限问题：整源不可用
		{``, false},
	}
	for _, c := range cases {
		var err error
		if c.msg != "" {
			err = errors.New(c.msg)
		}
		if got := isModelNotAllowed(err); got != c.want {
			t.Errorf("isModelNotAllowed(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}
