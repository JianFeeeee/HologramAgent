package test

// 判据：**L1 渲染必须从新往旧填充**，不能丢最新几轮。
//
// ## 要钉的契约
//
// `ContextTuning.ProtectedCount` 的语义是「裁剪时**无条件保留**的最近事件
// 条数」，`PruneWithProtected` 保护的正是尾部 N 条。而渲染（`buildMessages`
// → `renderL1Messages`）原实现从**最旧**往新走、预算耗尽就 `return` ——
// 保留旧对话、丢掉最新几轮。
//
// 两者冲突的后果是记忆时序错乱：`PruneWithProtected` 刚把最旧的淘汰进 L2，
// 渲染又把尾部切掉 ⇒ 模型看得到很久以前、看不到刚刚说过的。
//
// ## 判据怎么起
//
// 走真实的 `Agent` 装配 + 真实的 `InjectTextSync`，然后从 **provider 收到的
// 请求**里读 messages 序列 —— 那是模型真正看到的东西，也是唯一可观察出口。
// 不读 `context.events`、不调 `renderL1Messages`（那是实现）。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/agent/core"
	"github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// recordingProbe 记录每次请求的 messages，并按内容回一条答复。
type recordingProbe struct {
	mu       sync.Mutex
	requests []*api.CompletionRequest
}

func (p *recordingProbe) Name() string          { return "recording" }
func (p *recordingProbe) MaxContextTokens() int { return 2000 } // 小窗口，便于压出预算

func (p *recordingProbe) record(req *api.CompletionRequest) string {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	return "ok"
}

func (p *recordingProbe) last() *api.CompletionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) == 0 {
		return nil
	}
	return p.requests[len(p.requests)-1]
}

func (p *recordingProbe) Chat(ctx context.Context, req *api.CompletionRequest) (*api.CompletionResponse, error) {
	return &api.CompletionResponse{Content: p.record(req)}, nil
}

func (p *recordingProbe) ChatStream(ctx context.Context, req *api.CompletionRequest) (<-chan api.StreamChunk, error) {
	out := make(chan api.StreamChunk, 2)
	out <- api.StreamChunk{Content: p.record(req)}
	out <- api.StreamChunk{Done: true, FinishReason: "stop"}
	close(out)
	return out, nil
}

// newRenderAgent 起一个已 Start 的 agent，带一个记录用 provider。
func newRenderAgent(t *testing.T, protected int) (*core.Agent, *io.IOManager, *recordingProbe) {
	t.Helper()
	iom := io.NewIOManager()
	prov := &recordingProbe{}
	pm := api.NewProviderManager()
	pm.Register(prov.Name(), prov)

	a := core.New(core.AgentConfig{
		ID:              "l1order",
		Provider:        prov,
		ProviderManager: pm,
		IO:              iom,
	})
	a.Start()
	t.Cleanup(a.Stop)

	iom.RegisterInputChannel("webui", io.ChannelDef{})
	if err := iom.AssignInputChannel("webui", "l1order", 16); err != nil {
		t.Fatalf("AssignInputChannel: %v", err)
	}
	return a, iom, prov
}

// historyText 从请求里按顺序取出所有 user 消息（最新那条 = 当前输入）。
func historyText(req *api.CompletionRequest) []string {
	var out []string
	for _, m := range req.Messages {
		if m.Role == "user" {
			out = append(out, m.Content)
		}
	}
	return out
}

// injectTurn 同步注入一轮并等回执。
func injectTurn(t *testing.T, iom *io.IOManager, text string) {
	t.Helper()
	out := make(chan *io.OutputEvent, 1)
	go func() { out <- iom.InjectTextSyncTo("webui", "webui", text) }()
	select {
	case <-out:
	case <-time.After(5 * time.Second):
		t.Fatalf("注入 %q 后无回执", text)
	}
}

// TestL1RenderKeepsNewestTurnsUnderBudget：预算吃紧时，被丢掉的必须是
// **更旧**的对话，而不是最新的几轮。
//
// 构造：连做 12 轮，每轮输入带足够长的填充把窗口顶满（provider
// MaxContextTokens=2000 ⇒ 预算很小）。然后看最后一轮请求里出现的 user 消息：
//
//	· 必须含最后一轮之前的若干**近期**轮次
//	· 不得出现「只有早期轮次、近期轮次全没了」这种倒挂
//
// ★ 变异：把本函数的填充方向改回「从最旧往新 + 放不下就 return」⇒ 本例红。
func TestL1RenderKeepsNewestTurnsUnderBudget(t *testing.T) {
	a, iom, prov := newRenderAgent(t, 0)
	_ = a

	// ★ 每轮必须满足两条约束，否则判据测不到取舍方向：
	//	① 单组成本 < 总预算/4 —— 否则会被判成"异常超大帧"跳过，
	//	   那是另一条规则（上图实测：700 字节的组 cost=710 > budget/4=266
	//	   全部被 skip，最新那条恰好留下，看起来像"方向对了"）；
	//	② 全部轮次总和 > 总预算 —— 否则根本不会触发舍弃。
	//
	// provider 窗口 2000 ⇒ 上下文预算约 1067，budget/4≈266。
	// ★ 实测本仓 EstimateTokens 对这段文本是「1 token ≈ 1 字节」，
	//   所以每轮取 200 字节：单组 200 < 266（不会被当成超大帧跳过），
	//   12 轮合计 2400 > 1067（一定触发舍弃）。
	turnText := func(i int) string {
		return fmtTurn(i) + strings.Repeat("x", 200)
	}

	const turns = 12
	for i := 0; i < turns; i++ {
		injectTurn(t, iom, turnText(i))
	}

	req := prov.last()
	if req == nil {
		t.Fatal("provider 从未被调用")
	}
	users := historyText(req)

	// 当前输入必须是最后一条（这是 buildMessages 的既有契约）。
	if len(users) == 0 || users[len(users)-1] != turnText(turns-1) {
		t.Fatalf("★ 最后一条 user 消息不是当前输入 —— 请求构造被改坏了")
	}
	// 历史里必须能看到**倒数第二轮**（最新的一条历史）。
	// 它就是"最新时序"的最小证据：连它都被丢了，说明渲染在切尾巴。
	wantRecent := turnText(turns - 2)
	foundRecent := false
	for _, u := range users[:len(users)-1] {
		if u == wantRecent {
			foundRecent = true
		}
	}
	if !foundRecent {
		t.Fatalf("★ 预算吃紧时最新一轮历史（第 %d 轮）被丢掉了 —— "+
			"渲染在切尾部而不是切头部（模型会看到旧对话、看不到刚说过的）",
			turns-1)
	}
}

// fmtTurn 给每轮一个可区分的开头。
func fmtTurn(i int) string {
	return "第" + string(rune('A'+i)) + "轮："
}

// TestL1RenderPrefersNewerWhenBudgetTight 反向判据：预算紧张时，
// **更旧**的内容应当先被舍弃（不是反过来）。
//
// 与上一条成对：上一条钉"最新的在"，这一条钉"最旧的不在"。
// 只测前者会放过"全部塞进去（根本没做预算）"的实现。
func TestL1RenderPrefersNewerWhenBudgetTight(t *testing.T) {
	_, iom, prov := newRenderAgent(t, 0)

	const turns = 12
	for i := 0; i < turns; i++ {
		injectTurn(t, iom, fmtTurn(i)+strings.Repeat("y", 200))
	}

	req := prov.last()
	users := historyText(req)
	if len(users) < 2 {
		t.Fatalf("只渲染出 %d 条 user 消息，判定不了取舍方向", len(users))
	}
	// 第 0 轮最旧，预算 2000 token 装不下 12 轮 ⇒ 它必须已被舍弃。
	oldest := fmtTurn(0) + strings.Repeat("y", 200)
	for _, u := range users {
		if u == oldest {
			t.Fatalf("★ 预算只够装几轮，最旧的第 0 轮却仍在上下文里 —— " +
				"填充方向反了（应当先保最新，再往旧补）")
		}
	}
}

// 一条**不写**的判据：`protected` 分支压过预算
//
// 实现里尾部 `effectiveProtectedCount()` 条是无条件纳入的（不看预算）。
// 实测这条分支在当前参数下**从未被行使**：`effectiveProtectedCount` 本身
// 就按「预算 / 单条平均成本」收紧过（见该函数注释），所以"保护 N 条"
// 天然装得下。把它改成 `protected := false` —— 上面两条判据**仍然全绿**。
//
// 也就是说：这里的"无条件"是一道纵深防御，不是可观察契约。
// 按 §0 规则 2 不为它建断言；真需要钉的是"最新几轮必须在"，那是上面
// TestL1RenderKeepsNewestTurnsUnderBudget 做的事。
