package core

import (
	"context"
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// stubProvider 是一个可声明多模态能力的假 provider。
// 记录收到的请求，供断言「回退链是否真的调了它」。
type stubProvider struct {
	name    string
	vision  bool
	audio   bool
	reply   string
	err     error
	calls   int
	lastReq *agentAPI.CompletionRequest
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) Chat(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	s.calls++
	s.lastReq = req
	if s.err != nil {
		return nil, s.err
	}
	return &agentAPI.CompletionResponse{Content: s.reply}, nil
}
func (s *stubProvider) ChatStream(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	ch := make(chan agentAPI.StreamChunk)
	close(ch)
	return ch, nil
}
func (s *stubProvider) MaxContextTokens() int { return 8192 }
func (s *stubProvider) SupportsVision() bool  { return s.vision }
func (s *stubProvider) SupportsAudio() bool   { return s.audio }

// plainProvider 不实现 ModalProvider，用于验证「未声明即按不支持处理」。
type plainProvider struct{ name string }

func (p *plainProvider) Name() string { return p.name }
func (p *plainProvider) Chat(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	return &agentAPI.CompletionResponse{Content: "ok"}, nil
}
func (p *plainProvider) ChatStream(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	ch := make(chan agentAPI.StreamChunk)
	close(ch)
	return ch, nil
}
func (p *plainProvider) MaxContextTokens() int { return 8192 }

const testPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="
const testWAV = "data:audio/wav;base64,UklGRiQAAABXQVZF"

func imageBlock() agentAPI.ContentBlock {
	return agentAPI.ContentBlock{
		Type:     "image_url",
		ImageURL: &agentAPI.ImageURL{URL: testPNG, Detail: "auto"},
	}
}

func audioBlock() agentAPI.ContentBlock {
	return agentAPI.ContentBlock{
		Type:     "audio_url",
		AudioURL: &agentAPI.AudioURL{URL: testWAV},
	}
}

// newFallbackAgent 组装一个只带 provider/manager/inputCfg 的最小 Agent。
// 不走 New()：那会拉起 embedder、记忆、后台循环，与本测试无关。
func newFallbackAgent(main agentAPI.Provider, mgr *agentAPI.ProviderManager, cfg types.InputProcessingConfig) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	return &Agent{
		provider:        main,
		providerManager: mgr,
		inputCfg:        cfg,
		ctx:             ctx,
		cancel:          cancel,
	}
}

func TestPrepareToolBlocks_VisionCapableProviderPassesThrough(t *testing.T) {
	main := &stubProvider{name: "vision-main", vision: true}
	mgr := agentAPI.NewProviderManager()
	mgr.Register("vision-main", main)
	a := newFallbackAgent(main, mgr, types.InputProcessingConfig{})

	native, fallbackText := a.prepareToolBlocks([]agentAPI.ContentBlock{imageBlock()})

	if len(native) != 1 || native[0].Type != "image_url" {
		t.Fatalf("能看图的主模型应原样透传 image_url，得到 %+v", native)
	}
	if fallbackText != "" {
		t.Fatalf("不该触发回退，却返回了文字: %q", fallbackText)
	}
	if main.calls != 0 {
		t.Fatalf("不该额外调用 provider，实际调了 %d 次", main.calls)
	}
}

func TestPrepareToolBlocks_TextOnlyProviderFallsBackToTranscription(t *testing.T) {
	main := &stubProvider{name: "text-main"} // vision=false
	vis := &stubProvider{name: "vis-src", vision: true, reply: "一只橘猫坐在窗台上"}
	mgr := agentAPI.NewProviderManager()
	mgr.Register("text-main", main)
	mgr.Register("vis-src", vis)

	a := newFallbackAgent(main, mgr, types.InputProcessingConfig{
		Image: types.ImageProcessingConfig{
			FallbackProvider: "vis-src",
			DescribePrompt:   "描述这张图",
		},
	})

	native, fallbackText := a.prepareToolBlocks([]agentAPI.ContentBlock{imageBlock()})

	if len(native) != 0 {
		t.Fatalf("纯文本主模型不该收到原生块，得到 %+v", native)
	}
	if !strings.Contains(fallbackText, "一只橘猫坐在窗台上") {
		t.Fatalf("回退文字应含转写内容，得到 %q", fallbackText)
	}
	// 关键：模型必须知道这是二手转写而非自己直接看到的
	if !strings.Contains(fallbackText, "not directly perceived by the current model") {
		t.Fatalf("回退文字必须标注来源，得到 %q", fallbackText)
	}
	if vis.calls != 1 {
		t.Fatalf("应调用视觉源 1 次，实际 %d", vis.calls)
	}
	if main.calls != 0 {
		t.Fatalf("不该拿图去问纯文本主模型，实际调了 %d 次", main.calls)
	}
}

func TestPrepareToolBlocks_NoFallbackSourceReportsHonestly(t *testing.T) {
	main := &stubProvider{name: "text-main"}
	mgr := agentAPI.NewProviderManager()
	mgr.Register("text-main", main)
	a := newFallbackAgent(main, mgr, types.InputProcessingConfig{})

	native, fallbackText := a.prepareToolBlocks([]agentAPI.ContentBlock{imageBlock()})

	if len(native) != 0 {
		t.Fatalf("不该透传，得到 %+v", native)
	}
	// 这是本次修复的核心：没有能力也没有回退源时必须明说，
	// 而不是静默丢弃让模型以为自己看过图了。
	if !strings.Contains(fallbackText, "does not support images") {
		t.Fatalf("必须如实说明看不到图，得到 %q", fallbackText)
	}
	if !strings.Contains(fallbackText, "fallback_provider") {
		t.Fatalf("应给出可操作的配置提示，得到 %q", fallbackText)
	}
}

func TestPrepareToolBlocks_ProviderWithoutModalInterfaceTreatedAsTextOnly(t *testing.T) {
	main := &plainProvider{name: "legacy"} // 未实现 ModalProvider
	mgr := agentAPI.NewProviderManager()
	mgr.Register("legacy", main)
	a := newFallbackAgent(main, mgr, types.InputProcessingConfig{})

	native, fallbackText := a.prepareToolBlocks([]agentAPI.ContentBlock{imageBlock()})

	if len(native) != 0 {
		t.Fatalf("未声明能力的 provider 应按不支持处理，却透传了 %+v", native)
	}
	if fallbackText == "" {
		t.Fatal("应给出说明而非静默")
	}
}

func TestPrepareToolBlocks_MixedModalitySplitsCorrectly(t *testing.T) {
	// 主模型能看图但听不到音频——很多视觉模型正是这样。
	// 图应直视，只有音频走回退，不能一刀切全部降级。
	main := &stubProvider{name: "vision-only", vision: true}
	aud := &stubProvider{name: "aud-src", audio: true, reply: "背景有钢琴声"}
	mgr := agentAPI.NewProviderManager()
	mgr.Register("vision-only", main)
	mgr.Register("aud-src", aud)

	a := newFallbackAgent(main, mgr, types.InputProcessingConfig{
		Audio: types.AudioProcessingConfig{FallbackProvider: "aud-src"},
	})

	native, fallbackText := a.prepareToolBlocks([]agentAPI.ContentBlock{imageBlock(), audioBlock()})

	if fallbackText != "" {
		t.Fatalf("有原生块时转写应并入 native，不该走 content 分支，得到 %q", fallbackText)
	}
	var imgCount, textCount int
	for _, b := range native {
		switch b.Type {
		case "image_url":
			imgCount++
		case "text":
			textCount++
			if !strings.Contains(b.Text, "背景有钢琴声") {
				t.Fatalf("text 块应含音频转写，得到 %q", b.Text)
			}
		}
	}
	if imgCount != 1 {
		t.Fatalf("图应原样保留 1 个，得到 %d", imgCount)
	}
	if textCount != 1 {
		t.Fatalf("音频转写应产出 1 个 text 块，得到 %d", textCount)
	}
	if aud.calls != 1 {
		t.Fatalf("应调音频源 1 次，实际 %d", aud.calls)
	}
}

func TestTranscribeBlocks_EmptyReplyCountsAsFailure(t *testing.T) {
	// 回退源返回空串，往往意味着它上游也剥掉了媒体块。
	// 这种情况绝不能当成功——否则又是一次假成功。
	main := &stubProvider{name: "text-main"}
	vis := &stubProvider{name: "vis-src", vision: true, reply: "   "}
	mgr := agentAPI.NewProviderManager()
	mgr.Register("text-main", main)
	mgr.Register("vis-src", vis)

	a := newFallbackAgent(main, mgr, types.InputProcessingConfig{
		Image: types.ImageProcessingConfig{FallbackProvider: "vis-src"},
	})

	res := a.transcribeBlocksForFallback([]agentAPI.ContentBlock{imageBlock()})

	if res.Converted != 0 {
		t.Fatalf("空回复不应计入成功转写，得到 Converted=%d", res.Converted)
	}
	if !strings.Contains(res.Notice, "transcription failed") {
		t.Fatalf("应报告转写失败，得到 Notice=%q", res.Notice)
	}
}

func TestTranscribeBlocks_CapsBlockCount(t *testing.T) {
	// see_video 能一次注入 10 帧。上限存在的理由不再是“逐帧调用慢”（现已合包），
	// 而是图越多单请求体积越大、上游越慢且易超限。
	main := &stubProvider{name: "text-main"}
	vis := &stubProvider{name: "vis-src", vision: true, reply: "逐帧描述…"}
	mgr := agentAPI.NewProviderManager()
	mgr.Register("text-main", main)
	mgr.Register("vis-src", vis)

	a := newFallbackAgent(main, mgr, types.InputProcessingConfig{
		Image: types.ImageProcessingConfig{FallbackProvider: "vis-src"},
	})

	blocks := make([]agentAPI.ContentBlock, 10)
	for i := range blocks {
		blocks[i] = imageBlock()
	}
	res := a.transcribeBlocksForFallback(blocks)

	if res.Converted != modalFallbackMaxBlocks {
		t.Fatalf("应只转写 %d 个，实际 %d", modalFallbackMaxBlocks, res.Converted)
	}
	if res.Skipped != 10-modalFallbackMaxBlocks {
		t.Fatalf("应跳过 %d 个，实际 %d", 10-modalFallbackMaxBlocks, res.Skipped)
	}
	// 批量合包：上限内的帧应合成**一次**调用，而不是每帧一次。
	// 生产实测逐帧调用使 see_video 6 帧拖到 363s 且 3 帧超时。
	if vis.calls != 1 {
		t.Fatalf("多帧应合包为 1 次调用，实际 %d 次", vis.calls)
	}
	// 且那一次请求里应带满上限数量的 image 块（加一个 text 提示块）
	if vis.lastReq == nil || len(vis.lastReq.Messages) != 1 {
		t.Fatal("应只发一条 user 消息")
	}
	imgBlocks := 0
	for _, b := range vis.lastReq.Messages[0].Blocks {
		if b.Type == "image_url" {
			imgBlocks++
		}
	}
	if imgBlocks != modalFallbackMaxBlocks {
		t.Fatalf("单次请求应带 %d 个 image 块，实际 %d", modalFallbackMaxBlocks, imgBlocks)
	}
	// 跳过的部分也必须告知，否则模型以为自己看全了整段视频
	if !strings.Contains(res.Notice, "未转写") {
		t.Fatalf("应告知有块未转写，得到 %q", res.Notice)
	}
}

func TestTranscribeBlocks_MultiImageBatchedIntoOneCall(t *testing.T) {
	// 上限以内的多张图（典型：see_video 4 帧）同样只能一次调用。
	main := &stubProvider{name: "text-main"}
	vis := &stubProvider{name: "vis-src", vision: true, reply: "1) 开场 2) 中段 3) 结尾"}
	mgr := agentAPI.NewProviderManager()
	mgr.Register("text-main", main)
	mgr.Register("vis-src", vis)

	a := newFallbackAgent(main, mgr, types.InputProcessingConfig{
		Image: types.ImageProcessingConfig{FallbackProvider: "vis-src"},
	})

	res := a.transcribeBlocksForFallback([]agentAPI.ContentBlock{
		imageBlock(), imageBlock(), imageBlock(),
	})

	if vis.calls != 1 {
		t.Fatalf("3 张图应合为 1 次调用，实际 %d", vis.calls)
	}
	if res.Converted != 3 {
		t.Fatalf("应计入 3 个已转写，实际 %d", res.Converted)
	}
	if res.Skipped != 0 {
		t.Fatalf("不该有跳过，实际 %d", res.Skipped)
	}
	// 多张时提示词应补上张数，否则模型容易只描述第一张
	prompt := vis.lastReq.Messages[0].Blocks[0].Text
	if !strings.Contains(prompt, "3 张") {
		t.Fatalf("多张提示词应声明张数，得到 %q", prompt)
	}
	// 帧序列：插件显式给了 detail 就尊重它（see_video 本来就传 low），
	// 只在未指定时才由回退链按张数选默认。
	for _, b := range vis.lastReq.Messages[0].Blocks {
		if b.Type == "image_url" && b.ImageURL.Detail != "auto" {
			t.Fatalf("应保留插件显式指定的 detail=auto，得到 %q", b.ImageURL.Detail)
		}
	}
}

func TestTranscribeBlocks_MultiImageDefaultsToLowDetail(t *testing.T) {
	// 未指定 detail 的多张图（帧序列）用 low 控住体积与耗时。
	main := &stubProvider{name: "text-main"}
	vis := &stubProvider{name: "vis-src", vision: true, reply: "三帧描述"}
	mgr := agentAPI.NewProviderManager()
	mgr.Register("text-main", main)
	mgr.Register("vis-src", vis)

	a := newFallbackAgent(main, mgr, types.InputProcessingConfig{
		Image: types.ImageProcessingConfig{FallbackProvider: "vis-src"},
	})

	bare := agentAPI.ContentBlock{Type: "image_url", ImageURL: &agentAPI.ImageURL{URL: testPNG}}
	a.transcribeBlocksForFallback([]agentAPI.ContentBlock{bare, bare, bare})

	for _, b := range vis.lastReq.Messages[0].Blocks {
		if b.Type == "image_url" && b.ImageURL.Detail != "low" {
			t.Fatalf("多张未指定时应默认 low，得到 %q", b.ImageURL.Detail)
		}
	}
}

func TestTranscribeBlocks_SingleImageUsesHighDetail(t *testing.T) {
	// 单张图（see_picture）要看清细节，不吝惜 token。
	main := &stubProvider{name: "text-main"}
	vis := &stubProvider{name: "vis-src", vision: true, reply: "一只橘猫"}
	mgr := agentAPI.NewProviderManager()
	mgr.Register("text-main", main)
	mgr.Register("vis-src", vis)

	a := newFallbackAgent(main, mgr, types.InputProcessingConfig{
		Image: types.ImageProcessingConfig{FallbackProvider: "vis-src"},
	})

	// Detail 置空，让回退链自己定
	a.transcribeBlocksForFallback([]agentAPI.ContentBlock{
		{Type: "image_url", ImageURL: &agentAPI.ImageURL{URL: testPNG}},
	})

	for _, b := range vis.lastReq.Messages[0].Blocks {
		if b.Type == "image_url" && b.ImageURL.Detail != "high" {
			t.Fatalf("单张应用 high detail，得到 %q", b.ImageURL.Detail)
		}
	}
}

func TestResolveModalFallback_RejectsProviderNotDeclaringCapability(t *testing.T) {
	// 配置指向一个没声明 vision 的源：照用只会重演静默剥离。
	// 应拒绝它，并继续找真正声明了能力的源。
	main := &stubProvider{name: "text-main"}
	wrong := &stubProvider{name: "wrong-src"} // vision=false
	right := &stubProvider{name: "right-src", vision: true}
	mgr := agentAPI.NewProviderManager()
	mgr.Register("text-main", main)
	mgr.Register("wrong-src", wrong)
	mgr.Register("right-src", right)

	a := newFallbackAgent(main, mgr, types.InputProcessingConfig{
		Image: types.ImageProcessingConfig{FallbackProvider: "wrong-src"},
	})

	p, name := a.resolveModalFallback("image")
	if name != "right-src" {
		t.Fatalf("应跳过未声明能力的 wrong-src 而选中 right-src，得到 %q", name)
	}
	if p == nil {
		t.Fatal("应返回可用 provider")
	}
}

func TestResolveModalFallback_ScansForCapableSourceWhenUnconfigured(t *testing.T) {
	// 用户可能只在源上声明了 vision 却忘了填 fallback_provider。
	// 静默失败比多找一个能用的源更糟。
	main := &stubProvider{name: "text-main"}
	vis := &stubProvider{name: "some-vision-src", vision: true}
	mgr := agentAPI.NewProviderManager()
	mgr.Register("text-main", main)
	mgr.Register("some-vision-src", vis)

	a := newFallbackAgent(main, mgr, types.InputProcessingConfig{})

	_, name := a.resolveModalFallback("image")
	if name != "some-vision-src" {
		t.Fatalf("未配置时应扫出声明了能力的源，得到 %q", name)
	}
}

func TestPrepareToolBlocks_TextBlocksAlwaysPassThrough(t *testing.T) {
	main := &stubProvider{name: "text-main"}
	mgr := agentAPI.NewProviderManager()
	mgr.Register("text-main", main)
	a := newFallbackAgent(main, mgr, types.InputProcessingConfig{})

	native, fallbackText := a.prepareToolBlocks([]agentAPI.ContentBlock{
		{Type: "text", Text: "纯文字说明"},
	})

	if len(native) != 1 || native[0].Text != "纯文字说明" {
		t.Fatalf("text 块应无条件直通，得到 %+v / %q", native, fallbackText)
	}
}
