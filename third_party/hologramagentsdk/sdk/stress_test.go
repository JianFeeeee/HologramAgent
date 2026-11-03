package sdk

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// SDK 公开接口的并发压力测试（1.1.0 媒体接口上线后新增）。
//
// 为什么这一层需要压测：SDK 是**被多个 goroutine 同时使用的共享对象**。
// 一个插件的典型形态是 Start() 里起若干后台 goroutine（轮询、监听、定时器），
// 它们各自持同一个 *PluginSDK 往里注入消息；内核侧同时还有 stage 扇出、
// 工具调用、以及读 AutoRestart() 决定崩溃后是否重启。
// 单线程单测全绿不代表这些并发路径成立。
//
// 关注点不是吞吐数字，而是不变量：
//   1. 注入调用不丢、不串（媒体块必须与文本配对，不能张冠李戴）
//   2. 状态字段的读写不产生数据竞争（-race 下必须干净）
//   3. handler 注册/执行在并发下"恰好一次"
//   4. 跨进程 JSON 序列化对新媒体类型必须字节级往返一致
//
// 媒体接口尤其需要 3 与 4：媒体块要经 JSON 过子进程边界，
// 而 []byte 在 JSON 里是 base64，往返不一致的后果是图片静默损坏。

// ---------- 测试替身 ----------

// recordingInjector 记录每一次注入调用，用于验证"不丢不串"。
type recordingInjector struct {
	mu    sync.Mutex
	calls []injectCall

	// 计数用原子量：并发路径上只增不减，可在不持锁时安全读。
	nText, nMedia, nInterrupt, nSync atomic.Int64
}

type injectCall struct {
	kind    string // text / media / interruptMedia / sync ...
	source  string
	channel string
	text    string
	blocks  []ContentBlock
	opts    InjectOptions // 调用点声明的记忆/裁剪行为
}

func (r *recordingInjector) record(c injectCall) {
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
}

func (r *recordingInjector) InjectInterruptText(s, c, t string) {
	r.nInterrupt.Add(1)
	r.record(injectCall{kind: "interruptText", source: s, channel: c, text: t})
}

func (r *recordingInjector) InjectText(s, c, t string) {
	r.nText.Add(1)
	r.record(injectCall{kind: "text", source: s, channel: c, text: t})
}

func (r *recordingInjector) InjectTextNoMemory(s, c, t string) {
	r.nText.Add(1)
	r.record(injectCall{kind: "textNoMem", source: s, channel: c, text: t})
}

func (r *recordingInjector) InjectInputSync(s, c, t string) string {
	r.nSync.Add(1)
	r.record(injectCall{kind: "sync", source: s, channel: c, text: t})
	return "reply:" + t
}

func (r *recordingInjector) SetToolBlocks(blocks []ContentBlock) {
	r.record(injectCall{kind: "toolBlocks", blocks: blocks})
}

func (r *recordingInjector) InjectInputMedia(s, c, t string, b []ContentBlock) {
	r.nMedia.Add(1)
	r.record(injectCall{kind: "media", source: s, channel: c, text: t, blocks: b})
}

func (r *recordingInjector) InjectInputMediaSync(s, c, t string, b []ContentBlock) string {
	r.nMedia.Add(1)
	r.nSync.Add(1)
	r.record(injectCall{kind: "mediaSync", source: s, channel: c, text: t, blocks: b})
	return "reply:" + t
}

func (r *recordingInjector) InjectInterruptMedia(s, c, t string, b []ContentBlock) {
	r.nMedia.Add(1)
	r.record(injectCall{kind: "interruptMedia", source: s, channel: c, text: t, blocks: b})
}

// ---- 带 InjectOptions 的注入：记录 opts 以便测试断言标志位确实传到了内核 ----

func (r *recordingInjector) InjectTextOpts(s, c, t string, o InjectOptions) {
	r.nText.Add(1)
	r.record(injectCall{kind: "textOpts", source: s, channel: c, text: t, opts: o})
}

func (r *recordingInjector) InjectInterruptTextOpts(s, c, t string, o InjectOptions) {
	r.nInterrupt.Add(1)
	r.record(injectCall{kind: "interruptTextOpts", source: s, channel: c, text: t, opts: o})
}

func (r *recordingInjector) InjectInputSyncOpts(s, c, t string, o InjectOptions) string {
	r.nSync.Add(1)
	r.record(injectCall{kind: "syncOpts", source: s, channel: c, text: t, opts: o})
	return "reply:" + t
}

func (r *recordingInjector) InjectInputMediaOpts(s, c, t string, b []ContentBlock, o InjectOptions) {
	r.nMedia.Add(1)
	r.record(injectCall{kind: "mediaOpts", source: s, channel: c, text: t, blocks: b, opts: o})
}

func (r *recordingInjector) InjectInputMediaSyncOpts(s, c, t string, b []ContentBlock, o InjectOptions) string {
	r.nMedia.Add(1)
	r.nSync.Add(1)
	r.record(injectCall{kind: "mediaSyncOpts", source: s, channel: c, text: t, blocks: b, opts: o})
	return "reply:" + t
}

func (r *recordingInjector) InjectInterruptMediaOpts(s, c, t string, b []ContentBlock, o InjectOptions) {
	r.nMedia.Add(1)
	r.record(injectCall{kind: "interruptMediaOpts", source: s, channel: c, text: t, blocks: b, opts: o})
}

func (r *recordingInjector) snapshot() []injectCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]injectCall{}, r.calls...)
}

var _ IOInjector = (*recordingInjector)(nil)

// imageBlock 构造一个带可识别 URL 的图片块。
func imageBlock(tag string) ContentBlock {
	return ContentBlock{
		Type:     "image_url",
		ImageURL: &ImageURL{URL: "data:image/png;base64," + tag, Detail: "auto"},
	}
}

// ---------- 1. 媒体注入并发不丢不串 ----------

// 三个媒体注入方法在高并发下必须：调用数精确、且每次调用的 text 与 blocks 配对不错。
//
// "不串"是这里的关键断言。注入是插件里最容易被后台 goroutine 并发调用的入口，
// 若实现里出现任何共享中间状态（比如把 blocks 暂存到 SDK 字段再读出），
// 高并发下就会出现 A 的文本配上 B 的图——而两者单独看都"成功"了，不报错。
func TestStress_MediaInjectionConcurrentNoCrossTalk(t *testing.T) {
	const workers, perWorker = 32, 200

	inj := &recordingInjector{}
	s := &PluginSDK{name: "stress"}
	s.SetIOInjector(inj)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				// tag 唯一标识这次调用，文本与图片 URL 里都带上它。
				tag := fmt.Sprintf("w%d-i%d", w, i)
				switch i % 3 {
				case 0:
					s.InjectInputMedia("src", "ch", tag, []ContentBlock{imageBlock(tag)})
				case 1:
					if got := s.InjectInputMediaSync("src", "ch", tag, []ContentBlock{imageBlock(tag)}); got != "reply:"+tag {
						t.Errorf("同步注入回复错位: got %q want %q", got, "reply:"+tag)
					}
				default:
					s.InjectInterruptMedia("src", "ch", tag, []ContentBlock{imageBlock(tag)})
				}
			}
		}(w)
	}
	wg.Wait()

	total := int64(workers * perWorker)
	if got := inj.nMedia.Load(); got != total {
		t.Fatalf("媒体注入调用数 = %d，期望 %d（有调用丢失）", got, total)
	}

	// 逐条校验文本与媒体块配对：URL 必须含该次调用自己的 tag。
	seen := map[string]bool{}
	for _, c := range inj.snapshot() {
		if len(c.blocks) == 0 {
			continue
		}
		if c.blocks[0].ImageURL == nil {
			t.Fatalf("媒体块 ImageURL 丢失: %+v", c.blocks[0])
		}
		if !strings.HasSuffix(c.blocks[0].ImageURL.URL, c.text) {
			t.Fatalf("文本与媒体块错位: text=%q url=%q", c.text, c.blocks[0].ImageURL.URL)
		}
		if seen[c.text] {
			t.Fatalf("同一次调用被记录两次: %s", c.text)
		}
		seen[c.text] = true
	}
	if len(seen) != int(total) {
		t.Fatalf("去重后调用数 = %d，期望 %d", len(seen), total)
	}
}

// ---------- 2. 注入期间热替换 injector ----------

// 内核在插件运行期间可能重新注入 API（重载、恢复、子进程重连握手）。
// 此时插件的后台 goroutine 仍在注入。这条路径若无同步就是对 s.io 的数据竞争，
// 在 -race 下会被抓出；生产表现是偶发 nil 解引用崩溃。
func TestStress_InjectorSwapDuringInjection(t *testing.T) {
	s := &PluginSDK{name: "stress"}
	s.SetIOInjector(&recordingInjector{})

	stop := make(chan struct{})
	var injectors, swapper sync.WaitGroup

	// 注入方：持续打直到 stop
	for w := 0; w < 8; w++ {
		injectors.Add(1)
		go func() {
			defer injectors.Done()
			for {
				select {
				case <-stop:
					return
				default:
					s.InjectInputMedia("src", "ch", "x", []ContentBlock{imageBlock("x")})
					s.InjectText("src", "ch", "y")
				}
			}
		}()
	}

	// 替换方：反复换 injector（含换成 nil——内核卸载 API 时的真实状态）
	swapper.Add(1)
	go func() {
		defer swapper.Done()
		for i := 0; i < 500; i++ {
			if i%7 == 0 {
				s.SetIOInjector(nil)
			} else {
				s.SetIOInjector(&recordingInjector{})
			}
		}
	}()

	// 先等替换跑完，再告知注入方退出。
	// 顺序写反了就是死锁：注入方只依 close(stop) 退出。
	swapper.Wait()
	close(stop)
	injectors.Wait()
	// 断言就是「没崩、-race 没报」。nil injector 时必须静默跳过而非 panic。
}

// ---------- 3. autoRestart 标志的并发读写 ----------

// SetAutoRestart 的文档用途是"插件有无法恢复的状态（如外部连接）时设为 false"——
// 而连接建立本身通常是异步的，所以这个写入天然发生在后台 goroutine。
// 内核侧 registry 在另一个 goroutine 读 AutoRestart() 决定崩溃后是否重启。
// 这是一对跨 goroutine 的读写，必须同步。
func TestStress_AutoRestartFlagConcurrent(t *testing.T) {
	s := &PluginSDK{name: "stress", autoRestart: true}

	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				s.SetAutoRestart(i%2 == 0)
			}
		}(w)
	}
	// 读方模拟内核 registry
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				_ = s.AutoRestart()
			}
		}()
	}
	wg.Wait()
}

// ---------- 4. stop / onRemove handler 的"恰好一次" ----------

// RunStopHandlers 的契约是"执行后清空，幂等"。内核在停止插件时可能并发触发
// （超时强杀与正常 Stop 竞争），handler 里往往是关连接、落盘——
// 执行两次的后果从"重复写文件"到"close 已关闭的 channel 直接 panic"。
func TestStress_StopHandlersExactlyOnce(t *testing.T) {
	const n = 300
	s := &PluginSDK{name: "stress"}

	var counters [n]atomic.Int64
	for i := 0; i < n; i++ {
		i := i
		s.RegisterStopHandler(func() { counters[i].Add(1) })
	}

	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.RunStopHandlers()
		}()
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if got := counters[i].Load(); got != 1 {
			t.Fatalf("handler %d 执行 %d 次，期望恰好 1 次", i, got)
		}
	}
}

// 注册与执行并发：已注册的 handler 一次都不能多跑，未跑到的也不能被丢。
// 断言用"每个 handler 的执行次数 <= 1"而非"总数相等"——
// 与 RunStopHandlers 竞争的注册可能落在快照之后，那属于合法的未执行。
func TestStress_StopHandlersRegisterWhileRunning(t *testing.T) {
	s := &PluginSDK{name: "stress"}
	const n = 500
	var counters [n]atomic.Int64

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			i := i
			s.RegisterStopHandler(func() { counters[i].Add(1) })
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			s.RunStopHandlers()
		}
	}()
	wg.Wait()
	s.RunStopHandlers() // 收尾：把剩下的都跑掉

	for i := 0; i < n; i++ {
		if got := counters[i].Load(); got > 1 {
			t.Fatalf("handler %d 被执行 %d 次（重复执行）", i, got)
		}
	}
}

func TestStress_OnRemoveHandlersExactlyOnce(t *testing.T) {
	const n = 200
	s := &PluginSDK{name: "stress"}

	var counters [n]atomic.Int64
	for i := 0; i < n; i++ {
		i := i
		s.RegisterOnRemoveHandler(func() { counters[i].Add(1) })
	}

	var wg sync.WaitGroup
	for w := 0; w < 12; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.RunOnRemoveHandlers()
		}()
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if got := counters[i].Load(); got != 1 {
			t.Fatalf("onRemove handler %d 执行 %d 次，期望恰好 1 次", i, got)
		}
	}
}

// ---------- 5. StageContext 并发读改写 ----------

// StageContext 是全部 stage handler 共享的可变状态，字段全导出、靠调用方自觉
// 持 Lock/RLock。媒体链路让 Extra 成为新热点（media_blocks 挂在这里），
// 而 map 的并发写在 Go 里是直接 fatal，recover 都接不住。
//
// 这条测试锁定的不变量：按约定持锁的并发读改写不丢更新、不 fatal。
func TestStress_StageContextConcurrentExtraAndFinalText(t *testing.T) {
	ctx := &StageContext{Extra: map[string]interface{}{}}

	const workers, rounds = 16, 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				// 写：模拟插件往 Extra 塞媒体块并追加文本（读-改-写）
				ctx.Lock()
				ctx.Extra[fmt.Sprintf("k%d-%d", w, i)] = []ContentBlock{imageBlock("x")}
				ctx.FinalText += "."
				ctx.Unlock()

				// 读：模拟另一个 handler 检查是否已被响应
				_ = ctx.IsResponded()
				ctx.RLock()
				_ = len(ctx.Extra)
				ctx.RUnlock()
			}
		}(w)
	}
	wg.Wait()

	ctx.RLock()
	defer ctx.RUnlock()
	if len(ctx.Extra) != workers*rounds {
		t.Fatalf("Extra 键数 = %d，期望 %d（出现 lost update）", len(ctx.Extra), workers*rounds)
	}
	if len(ctx.FinalText) != workers*rounds {
		t.Fatalf("FinalText 长度 = %d，期望 %d（出现 lost update）", len(ctx.FinalText), workers*rounds)
	}
}

// ---------- 6. OwnTools scope 包装器的并发正确性 ----------

// StageScopeOwnTools 的包装闭环里要读 ctx.ToolCalls 判断归属。
// 并发下若判断与执行之间状态被改写，就会出现"别人的工具触发了我的 handler"——
// 后果是插件对不属于自己的工具结果动手，且没有任何错误。
func TestStress_OwnToolsScopeNoCrossPluginLeak(t *testing.T) {
	var registered StageHandler
	s := &PluginSDK{
		name:     "mine",
		regStage: func(stage Stage, h StageHandler) { registered = h },
	}

	var fired atomic.Int64
	s.RegisterStage(StageBeforeToolcall, func(ctx *StageContext) error {
		fired.Add(1)
		ctx.RLock()
		defer ctx.RUnlock()
		// 触发了就必须确实是自己的工具
		if len(ctx.ToolCalls) == 0 || ctx.ToolCalls[0].Plugin != "mine" {
			t.Errorf("handler 被别的插件的工具触发: %+v", ctx.ToolCalls)
		}
		return nil
	}, StageScopeOwnTools)

	if registered == nil {
		t.Fatal("handler 未注册")
	}

	const workers, rounds = 16, 100
	var wg sync.WaitGroup
	var mineCount atomic.Int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				// 每个 goroutine 用自己的 ctx——真实内核里 stage 扇出共享同一个
				// ctx，但那部分的并发由内核 host 仲裁；这里验证包装器本身。
				owner := "other"
				if (w+i)%2 == 0 {
					owner = "mine"
					mineCount.Add(1)
				}
				ctx := &StageContext{Extra: map[string]interface{}{}}
				ctx.ToolCalls = []ToolCall{{Plugin: owner, Name: "t"}}
				if err := registered(ctx); err != nil {
					t.Errorf("handler 返回错误: %v", err)
				}
			}
		}(w)
	}
	wg.Wait()

	if got, want := fired.Load(), mineCount.Load(); got != want {
		t.Fatalf("handler 触发 %d 次，期望 %d 次（漏触发或跨插件触发）", got, want)
	}
}

// ---------- 7. 媒体类型的 JSON 往返（跨进程边界的真实形态） ----------

// 媒体块与附件要经 JSON 过子进程边界。[]byte 在 JSON 里是 base64，
// 往返不一致的后果是图片字节静默损坏——落进 CAS 后 digest 校验才会发现，
// 而那时已经无从追查是谁改坏的。
func TestStress_MediaTypesJSONRoundTripAtScale(t *testing.T) {
	// 覆盖真实会遇到的边界：空、单字节、含 0x00、全 0xFF、超过 base64 分组边界的长度
	sizes := []int{0, 1, 2, 3, 255, 256, 1023, 4096, 65537}
	for _, n := range sizes {
		data := make([]byte, n)
		for i := range data {
			data[i] = byte(i * 7 % 256)
		}
		att := MediaAttachment{
			Digest: strings.Repeat("a", 64),
			MIME:   "image/png",
			Data:   data,
			Name:   "图片-名字 with space & 符号.png",
		}
		b, err := json.Marshal(att)
		if err != nil {
			t.Fatalf("size=%d marshal: %v", n, err)
		}
		var back MediaAttachment
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("size=%d unmarshal: %v", n, err)
		}
		if len(back.Data) != n {
			t.Fatalf("size=%d 往返后长度 = %d", n, len(back.Data))
		}
		for i := range data {
			if back.Data[i] != data[i] {
				t.Fatalf("size=%d 第 %d 字节损坏: %02x != %02x", n, i, back.Data[i], data[i])
			}
		}
		if back.Name != att.Name || back.MIME != att.MIME || back.Digest != att.Digest {
			t.Fatalf("size=%d 元数据往返不一致: %+v", n, back)
		}
	}
}

// omitempty 必须真的生效：读路径上内核不回 Data，若序列化仍产出 "data":null
// 之类的键，跨进程消息会凭空变大，且插件侧无法区分"没有字节"与"空字节"。
func TestStress_MediaTypesOmitEmpty(t *testing.T) {
	cases := []struct {
		name    string
		v       interface{}
		absent  []string
		present []string
	}{
		{
			name:    "Triple 无媒体",
			v:       Triple{Subject: "甲方", Relation: "签署", Object: "合同"},
			absent:  []string{"media_digests", "sentence_text", "confidence", "subject_type", "object_type"},
			present: []string{"subject", "relation", "object"},
		},
		{
			name:    "Triple 带媒体",
			v:       Triple{Subject: "甲方", Relation: "包含", Object: "图", MediaDigests: []string{"abc12345"}, SentenceText: "句子"},
			absent:  []string{"confidence"},
			present: []string{"media_digests", "sentence_text"},
		},
		{
			name:    "Doc 读路径无字节",
			v:       Doc{ID: "d1", Title: "标题", Content: "正文", Attachments: []MediaAttachment{{Digest: "abc12345", MIME: "image/png"}}},
			absent:  []string{"\"data\"", "media_digests", "score"},
			present: []string{"attachments", "digest", "mime"},
		},
		{
			name:    "TextEvent 无附件",
			v:       TextEvent{Role: "user", Content: "hi"},
			absent:  []string{"attachments", "channel"},
			present: []string{"role", "content"},
		},
		{
			name:    "ContentBlock 纯文本",
			v:       ContentBlock{Type: "text", Text: "hi"},
			absent:  []string{"image_url", "audio_url"},
			present: []string{"type", "text"},
		},
		{
			name:    "ContentBlock 图片",
			v:       imageBlock("AAA"),
			absent:  []string{"audio_url", "\"text\""},
			present: []string{"image_url", "detail"},
		},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.v)
		if err != nil {
			t.Fatalf("%s marshal: %v", c.name, err)
		}
		s := string(b)
		for _, k := range c.absent {
			if strings.Contains(s, k) {
				t.Errorf("%s: 不该出现的键 %s —— %s", c.name, k, s)
			}
		}
		for _, k := range c.present {
			if !strings.Contains(s, k) {
				t.Errorf("%s: 缺少键 %s —— %s", c.name, k, s)
			}
		}
	}
}

// 媒体块在并发序列化下必须各自独立：ImageURL/AudioURL 是指针，
// 若某处复用同一个指针再改写，序列化结果会互相污染。
func TestStress_ContentBlockConcurrentMarshal(t *testing.T) {
	const workers, rounds = 16, 300
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				tag := fmt.Sprintf("w%d-i%d", w, i)
				blocks := []ContentBlock{
					{Type: "text", Text: tag},
					imageBlock(tag),
					{Type: "audio_url", AudioURL: &AudioURL{URL: "data:audio/wav;base64," + tag}},
				}
				b, err := json.Marshal(blocks)
				if err != nil {
					t.Errorf("marshal: %v", err)
					return
				}
				var back []ContentBlock
				if err := json.Unmarshal(b, &back); err != nil {
					t.Errorf("unmarshal: %v", err)
					return
				}
				if len(back) != 3 {
					t.Errorf("块数 = %d", len(back))
					return
				}
				if back[0].ImageURL != nil || back[0].AudioURL != nil {
					t.Errorf("文本块被填了媒体指针: %+v", back[0])
				}
				if back[1].ImageURL == nil || !strings.HasSuffix(back[1].ImageURL.URL, tag) {
					t.Errorf("图片块 URL 错位: %+v", back[1].ImageURL)
				}
				if back[1].AudioURL != nil {
					t.Errorf("图片块被填了音频指针")
				}
				if back[2].AudioURL == nil || !strings.HasSuffix(back[2].AudioURL.URL, tag) {
					t.Errorf("音频块 URL 错位: %+v", back[2].AudioURL)
				}
			}
		}(w)
	}
	wg.Wait()
}

// ---------- 8. 注册面的并发 ----------

// 插件在 Start() 里起多个 goroutine 分别注册工具是常见写法。
// def.Plugin 的默认填充若不是每次调用独立的，就会出现工具归属错乱——
// 表现是 OwnTools scope 失效、WebUI 里工具挂在别的插件名下。
func TestStress_RegisterToolConcurrentPluginDefaulting(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{} // toolName -> def.Plugin

	s := &PluginSDK{
		name: "mine",
		regTool: func(name string, def ToolDef, h ToolHandler) error {
			mu.Lock()
			got[name] = def.Plugin
			mu.Unlock()
			return nil
		},
	}

	const workers, perWorker = 16, 100
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				name := fmt.Sprintf("tool_w%d_i%d", w, i)
				def := ToolDef{Description: "d", Parameters: map[string]interface{}{}}
				// 一半显式指定归属，一半靠 SDK 填默认值
				if i%2 == 0 {
					def.Plugin = "explicit"
				}
				if err := s.RegisterTool(name, def, func(map[string]interface{}) (interface{}, error) {
					return nil, nil
				}); err != nil {
					t.Errorf("RegisterTool: %v", err)
				}
			}
		}(w)
	}
	wg.Wait()

	if len(got) != workers*perWorker {
		t.Fatalf("注册工具数 = %d，期望 %d", len(got), workers*perWorker)
	}
	for name, owner := range got {
		want := "mine"
		if isEvenSuffix(name) {
			want = "explicit"
		}
		if owner != want {
			t.Fatalf("工具 %s 归属 = %q，期望 %q", name, owner, want)
		}
	}
}

// isEvenSuffix 判断 tool_wX_iY 里的 Y 是否为偶数。
func isEvenSuffix(name string) bool {
	idx := strings.LastIndex(name, "_i")
	if idx < 0 {
		return false
	}
	n := 0
	if _, err := fmt.Sscanf(name[idx+2:], "%d", &n); err != nil {
		return false
	}
	return n%2 == 0
}

// nil 依赖下所有便捷方法必须静默降级而非 panic。
//
// 这是"媒体存储可关闭"在 SDK 层的对应物：内核未注入某个 API 时
// （精简部署、插件权限不足、子进程握手尚未完成），插件的调用不该崩。
func TestStress_NilDependenciesDegradeSilently(t *testing.T) {
	s := &PluginSDK{name: "bare"}

	const workers = 16
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s.InjectText("s", "c", "t")
				s.InjectTextNoMemory("s", "c", "t")
				s.InjectInterruptText("s", "c", "t")
				if got := s.InjectInputSync("s", "c", "t"); got != "" {
					t.Errorf("无 injector 时同步注入应返回空串，got %q", got)
				}
				s.InjectInputMedia("s", "c", "t", []ContentBlock{imageBlock("x")})
				if got := s.InjectInputMediaSync("s", "c", "t", nil); got != "" {
					t.Errorf("无 injector 时媒体同步注入应返回空串，got %q", got)
				}
				s.InjectInterruptMedia("s", "c", "t", nil)

				// getter 全部应返回 nil 而非 panic
				_ = s.Memory()
				_ = s.TextMemory()
				_ = s.DocMemory()
				_ = s.Knowledge()
				_ = s.LLM()
				_ = s.Social()
				_ = s.Events()
				_ = s.PluginMgr()
				_ = s.Settings()

				// 注册面无 registrar 时应返回 nil error
				if err := s.RegisterTool("t", ToolDef{}, nil); err != nil {
					t.Errorf("无 registrar 时 RegisterTool 应返回 nil，got %v", err)
				}
				if err := s.RegisterPluginAPI("a"); err != nil {
					t.Errorf("无 registrar 时 RegisterPluginAPI 应返回 nil，got %v", err)
				}
				s.RegisterStage(StageOnInput, func(*StageContext) error { return nil })
			}
		}()
	}
	wg.Wait()
}
