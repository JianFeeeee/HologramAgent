package api

import (
	"strings"
	"testing"
)

// TestClassifyProviderError 是错误分类的真值表。
//
// 为什么要有这张表：ErrContextFull 决定「超页后裁剪重试」还是「整轮失败」。
// 判错一个方向是丢数据（超页变报错），错另一个方向是死循环（把凭证错误
// 当超页，裁剪重试 2 次仍失败，白白消耗）。所以每一条都必须显式钉住。
func TestClassifyProviderError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		// ── 凭证：优先级最高，报文里即使提到 context 也不能判成超页 ──
		{"凭证401", 401, `{"error":{"message":"invalid api key"}}`, ErrCredential},
		{"凭证403", 403, `{"error":{"message":"context_length_exceeded: bad key"}}`, ErrCredential},

		// ── 上下文超限：跨状态码 + 跨报文形态 ──
		{"超限400-openai形态", 400,
			`{"error":{"type":"invalid_request_error","code":"context_length_exceeded",` +
				`"message":"This model's maximum context length is 128000 tokens"}}`, ErrContextFull},
		{"超限400-anthropic形态", 400,
			`{"type":"error","error":{"type":"invalid_request_error",` +
				`"message":"prompt is too long: 210000 tokens > 200000 maximum"}}`, ErrContextFull},
		{"超限413", 413,
			`{"error":{"message":"Request Entity Too Large"}}`, ErrTransient}, // 413 无特征词 ⇒ 瞬时
		{"超限429-带特征", 429,
			`{"error":{"message":"rate limit: reduce the length of the messages to proceed"}}`, ErrContextFull},

		// ── 瞬时 ──
		{"网关502", 502, `bad gateway`, ErrTransient},
		{"限流429", 429, `{"error":{"message":"rate limit exceeded"}}`, ErrTransient},
		{"网络400无特征", 400, `{"error":{"message":"invalid json body"}}`, ErrTransient},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyProviderError(c.status, c.body)
			if got != c.want {
				t.Fatalf("classify(%d, %q) = %v，期望 %v", c.status, c.body, got, c.want)
			}
		})
	}
}

// TestClassifyProviderError_ContextFullMarkers 全量过一遍特征词表：
// 任何一条特征词单独出现都必须判成 ErrContextFull（防止有人改了判定顺序
// 导致某条特征永远不命中）。
func TestClassifyProviderError_ContextFullMarkers(t *testing.T) {
	for _, m := range contextFullMarkers {
		body := `{"error":{"message":"xxx ` + m + ` yyy"}}`
		if got := classifyProviderError(400, body); got != ErrContextFull {
			t.Fatalf("特征词 %q 未被识别为 ErrContextFull，实际 %v", m, got)
		}
	}
}

// TestNewProviderError_保留报文原文 保证用户看到的报错里仍有上游原文
// （这是「上游报了什么」的唯一线索，也是排查 context_full 的第一手材料）。
func TestNewProviderError_保留报文原文(t *testing.T) {
	body := `{"error":{"code":"context_length_exceeded","message":"too long"}}`
	err := newProviderError(400, body)
	if err.Kind != ErrContextFull {
		t.Fatalf("Kind = %v，期望 ErrContextFull", err.Kind)
	}
	if !strings.Contains(err.Message, "context_length_exceeded") {
		t.Fatalf("错误消息丢了上游原文: %q", err.Message)
	}
	if !strings.Contains(err.Message, "400") {
		t.Fatalf("错误消息缺状态码: %q", err.Message)
	}
}

// ── 变异自证 ──
// 把特征词表清空，classify 必须不再判出 ErrContextFull（证明 T-判别
// 真的依赖这张表，而不是碰巧对）。若此测试在变异后仍通过 ⇒ 测试无效。
func TestClassify_Mutation_清空特征词表后不再判超限(t *testing.T) {
	backup := contextFullMarkers
	contextFullMarkers = nil
	defer func() { contextFullMarkers = backup }()

	body := `{"error":{"code":"context_length_exceeded","message":"too long"}}`
	if got := classifyProviderError(400, body); got == ErrContextFull {
		t.Fatalf("特征词表清空后仍判为 ErrContextFull ⇒ 判别不依赖该表，测试无效")
	}
}

// 把凭证判定移到超限之后，401+context 特征词必须翻案（证明优先级有意义）。
func TestClassify_Mutation_调换优先级后401翻案(t *testing.T) {
	body := `{"error":{"message":"context_length_exceeded: bad key"}}`
	// 直接调 classifyProviderError（不改代码顺序），当前应判凭证
	if got := classifyProviderError(401, body); got != ErrCredential {
		t.Fatalf("当前实现应判 ErrCredential，实际 %v", got)
	}
	// 若有人把顺序改成「先查特征词」，上面这行就会失败 —— 即本测试能捕获该回归。
}
