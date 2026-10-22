package proc

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// 本文件验证共享内存 stage 并发的正确性——**整个迁移最关键的一环**（§4.4 风险 3.4）。
//
// 对照基线（今日 C ABI 副本模型）：
//   - 内置插件（共享 *StageContext + RWMutex）：0% 丢失
//   - 外部插件（快照-副本-写回）：35.8~36.8% 丢失（实验 12），现网量级百分之几脏数据
//
// 目标：共享内存 + 锁仲裁下，跨进程并发改写收敛到 0% 丢失。

// newTestSegment 造一块内存段模拟 mmap 区域（单测无需真 mmap：
// 编解码与 arena 逻辑与底层是 mmap 还是普通内存无关）。
func newTestSegment(t *testing.T, size int) *Segment {
	t.Helper()
	buf := make([]byte, size)
	seg, err := NewSegment(buf)
	if err != nil {
		t.Fatalf("NewSegment: %v", err)
	}
	return seg
}

func TestSegment_AttachValidatesMagicAndVersion(t *testing.T) {
	buf := make([]byte, 8192)
	if _, err := NewSegment(buf); err != nil {
		t.Fatalf("NewSegment: %v", err)
	}
	if _, err := AttachSegment(buf); err != nil {
		t.Fatalf("AttachSegment 应成功: %v", err)
	}

	// 魔数损坏
	bad := make([]byte, len(buf))
	copy(bad, buf)
	bad[0] ^= 0xFF
	if _, err := AttachSegment(bad); err == nil {
		t.Error("魔数不匹配应报错（避免版本不一致时静默错读）")
	}

	// 版本不匹配
	badVer := make([]byte, len(buf))
	copy(badVer, buf)
	badVer[4] = 99
	if _, err := AttachSegment(badVer); err == nil {
		t.Error("版本不匹配应报错")
	}
}

// 全部 16 个字段可跨进程往返——今日经 C ABI 只有 10 个字段可见（§8.3）。
func TestSegment_RoundTripAllFields(t *testing.T) {
	seg := newTestSegment(t, 16384)

	resp := "短路响应"
	src := &pubsdk.StageContext{
		RawMessage:       "原始输入",
		UserID:           "u1",
		GroupID:          "g1",
		LLMText:          "模型输出",
		ReasoningContent: "思考过程", // C ABI 下外部插件看不到
		FinalText:        "最终文本",
		Response:         &resp,
		Phase:            pubsdk.StageAfterToolcall,
		NoMemory:         true,
		ContextMsgs:      []map[string]interface{}{{"role": "user", "content": "hi"}}, // C ABI 看不到
		ToolCalls:        []pubsdk.ToolCall{{ID: "t1", Name: "weather_query", Plugin: "weather"}},
		ToolResults:      []pubsdk.ToolResult{{CallID: "t1", Name: "weather_query", Success: true, Result: "晴"}},
		Memory:           []pubsdk.MemItem{{Role: "user", Content: "记忆", Score: 0.9}}, // C ABI 看不到
		TokenUsage:       map[string]int{"prompt": 100, "completion": 50},             // C ABI 看不到
		Errors:           []string{"err1"},                                            // C ABI 看不到
		Extra: map[string]interface{}{
			ExtraKeyMediaType:     "image",
			ExtraKeyInputSource:   "qq",
			ExtraKeyOutputChannel: "qq",
		},
	}

	if err := seg.WriteAll(src); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}

	var dst pubsdk.StageContext
	if err := seg.ReadInto(&dst); err != nil {
		t.Fatalf("ReadInto: %v", err)
	}

	if dst.RawMessage != src.RawMessage || dst.UserID != src.UserID || dst.GroupID != src.GroupID {
		t.Errorf("标量字段不一致: raw=%q uid=%q gid=%q", dst.RawMessage, dst.UserID, dst.GroupID)
	}
	if dst.ReasoningContent != "思考过程" {
		t.Errorf("ReasoningContent 应可见（C ABI 下不可见）: %q", dst.ReasoningContent)
	}
	if len(dst.ContextMsgs) != 1 {
		t.Errorf("ContextMsgs 应可见: %v", dst.ContextMsgs)
	}
	if len(dst.Memory) != 1 || dst.Memory[0].Score != 0.9 {
		t.Errorf("Memory 应可见: %v", dst.Memory)
	}
	if dst.TokenUsage["prompt"] != 100 {
		t.Errorf("TokenUsage 应可见: %v", dst.TokenUsage)
	}
	if len(dst.Errors) != 1 {
		t.Errorf("Errors 应可见: %v", dst.Errors)
	}
	if dst.Response == nil || *dst.Response != resp {
		t.Errorf("Response 应往返: %v", dst.Response)
	}
	if !dst.NoMemory {
		t.Error("NoMemory 标志应往返")
	}
	if len(dst.ToolResults) != 1 || dst.ToolResults[0].Result != "晴" {
		t.Errorf("ToolResults 应往返: %v", dst.ToolResults)
	}
	if dst.Extra[ExtraKeyMediaType] != "image" {
		t.Errorf("Extra 提升字段应往返: %v", dst.Extra)
	}
}

// nil Response 与空字符串 Response 必须可区分（短路语义依赖此）。
func TestSegment_ResponseNilVsEmpty(t *testing.T) {
	seg := newTestSegment(t, 8192)

	if err := seg.WriteAll(&pubsdk.StageContext{RawMessage: "x"}); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	var d1 pubsdk.StageContext
	if err := seg.ReadInto(&d1); err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if d1.Response != nil {
		t.Errorf("未设置的 Response 应为 nil，实际 %q", *d1.Response)
	}

	empty := ""
	seg2 := newTestSegment(t, 8192)
	if err := seg2.WriteAll(&pubsdk.StageContext{Response: &empty}); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	var d2 pubsdk.StageContext
	if err := seg2.ReadInto(&d2); err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if d2.Response == nil {
		t.Error("显式设为空串的 Response 不应读成 nil（短路语义会丢）")
	} else if *d2.Response != "" {
		t.Errorf("Response 应为空串，实际 %q", *d2.Response)
	}
}

// 只读插件的 WriteDirty 必须零写入——**这是消除 lost update 的核心断言**。
func TestSegment_WriteDirty_ReadOnlyPluginWritesNothing(t *testing.T) {
	seg := newTestSegment(t, 16384)
	base := &pubsdk.StageContext{
		RawMessage:  "查天气",
		ToolResults: []pubsdk.ToolResult{{CallID: "c1", Name: "weather_query", Result: "已清洗"}},
	}
	if err := seg.WriteAll(base); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}

	// 插件侧：读入 → 只读 → 写回
	var local pubsdk.StageContext
	if err := seg.ReadInto(&local); err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	snap := TakeSnapshot(&local)
	_ = local.ToolResults[0].Result // 只读，不改

	n, err := seg.WriteDirty(&local, snap)
	if err != nil {
		t.Fatalf("WriteDirty: %v", err)
	}
	if n != 0 {
		t.Fatalf("只读插件应零写回，实际写回 %d 个字段（会覆盖他人改写）", n)
	}
}

// 原地改切片元素必须被识别为脏 —— C ABI 侧修 11.3 时踩过的坑。
func TestSegment_WriteDirty_InPlaceSliceMutationDetected(t *testing.T) {
	seg := newTestSegment(t, 16384)
	if err := seg.WriteAll(&pubsdk.StageContext{
		ToolResults: []pubsdk.ToolResult{{CallID: "c1", Result: "带\x1b[31mANSI\x1b[0m"}},
	}); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}

	var local pubsdk.StageContext
	if err := seg.ReadInto(&local); err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	snap := TakeSnapshot(&local)
	local.ToolResults[0].Result = "带ANSI" // 原地改元素（sanitizer 的实际行为）

	n, err := seg.WriteDirty(&local, snap)
	if err != nil {
		t.Fatalf("WriteDirty: %v", err)
	}
	if n != 1 {
		t.Fatalf("原地改切片元素应被识别为 1 个脏字段，实际 %d", n)
	}

	var after pubsdk.StageContext
	if err := seg.ReadInto(&after); err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if after.ToolResults[0].Result != "带ANSI" {
		t.Errorf("清洗结果未写回: %v", after.ToolResults[0].Result)
	}
}

// 复刻现网场景（实验 13）：sanitizer 改写 + weather 只读并发，清洗结果不得被覆盖。
// 这是 C ABI 副本模型下量级百分之几脏数据的直接来源。
func TestSegment_ProductionScenario_SanitizerNotOverwrittenByWeather(t *testing.T) {
	seg := newTestSegment(t, 16384)
	dirty := "天气：晴 \x1b[31m28°C\x1b[0m"
	clean := "天气：晴 28°C"

	if err := seg.WriteAll(&pubsdk.StageContext{
		RawMessage:  "查天气",
		Phase:       pubsdk.StageAfterToolcall,
		ToolResults: []pubsdk.ToolResult{{CallID: "c1", Name: "weather_query", Result: dirty}},
	}); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}

	lock := newStageLock()

	// sanitizer：拿锁 → 读 → 清洗 → 写脏字段 → 放锁
	runSanitizer := func() error {
		if err := lock.Acquire("sanitizer"); err != nil {
			return err
		}
		defer lock.Release("sanitizer")
		var local pubsdk.StageContext
		if err := seg.ReadInto(&local); err != nil {
			return err
		}
		snap := TakeSnapshot(&local)
		if len(local.ToolResults) > 0 {
			s, _ := local.ToolResults[0].Result.(string)
			local.ToolResults[0].Result = strings.NewReplacer("\x1b[31m", "", "\x1b[0m", "").Replace(s)
		}
		_, err := seg.WriteDirty(&local, snap)
		return err
	}

	// weather：拿锁 → 读 → 只读 → 零写回 → 放锁
	runWeather := func() error {
		if err := lock.Acquire("weather"); err != nil {
			return err
		}
		defer lock.Release("weather")
		var local pubsdk.StageContext
		if err := seg.ReadInto(&local); err != nil {
			return err
		}
		snap := TakeSnapshot(&local)
		if len(local.ToolResults) > 0 {
			_ = local.ToolResults[0].Result // 只读
		}
		n, err := seg.WriteDirty(&local, snap)
		if err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("weather 只读却写回 %d 个字段", n)
		}
		return nil
	}

	// 并发扇出（保留原始设计），weather 后完成是最坏情形
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs <- runSanitizer() }()
	go func() { defer wg.Done(); errs <- runWeather() }()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("插件执行失败: %v", err)
		}
	}

	var final pubsdk.StageContext
	if err := seg.ReadInto(&final); err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	got, _ := final.ToolResults[0].Result.(string)
	if got != clean {
		t.Fatalf("清洗结果被覆盖：期望 %q，实际 %q", clean, got)
	}
}

// 多插件高并发累加同一字段：总写入次数必须等于最终长度（零丢失零撕裂）。
// 对应实验 8（5 进程 × 300 轮），这里在单进程内用 goroutine 模拟并发扇出，
// 验证共享段 + 锁仲裁 + 脏字段写回三者组合的正确性。
func TestSegment_ConcurrentAppend_NoLostUpdate(t *testing.T) {
	// arena 需容纳 append-only 的中间垃圾：每轮写入长度递增，
	// 5 插件 × 40 轮 → 最长 200 字符，累计约 200*201/2 = 20100 字节，留足余量。
	seg := newTestSegment(t, 128*1024)
	if err := seg.WriteAll(&pubsdk.StageContext{FinalText: ""}); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}

	lock := newStageLock()
	tags := []string{"A", "B", "C", "D", "E"}
	const iters = 40

	var wg sync.WaitGroup
	errCh := make(chan error, len(tags)*iters)

	for _, tag := range tags {
		wg.Add(1)
		go func(tag string) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if err := lock.Acquire(tag); err != nil {
					errCh <- err
					return
				}
				var local pubsdk.StageContext
				if err := seg.ReadInto(&local); err != nil {
					lock.Release(tag)
					errCh <- err
					return
				}
				snap := TakeSnapshot(&local)
				local.FinalText += tag // 读-改-写
				if _, err := seg.WriteDirty(&local, snap); err != nil {
					lock.Release(tag)
					errCh <- err
					return
				}
				if err := lock.Release(tag); err != nil {
					errCh <- err
					return
				}
			}
		}(tag)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("并发写入失败: %v", err)
		}
	}

	var final pubsdk.StageContext
	if err := seg.ReadInto(&final); err != nil {
		t.Fatalf("ReadInto: %v", err)
	}

	// 关键断言：各标记出现次数之和 == 最终长度 ⇒ 无丢失、无撕裂
	total := 0
	counts := map[string]int{}
	for _, tag := range tags {
		c := strings.Count(final.FinalText, tag)
		counts[tag] = c
		total += c
	}
	if total != len(final.FinalText) {
		t.Fatalf("出现撕裂：各标记计数之和 %d != 最终长度 %d（counts=%v）",
			total, len(final.FinalText), counts)
	}
	want := len(tags) * iters
	if total != want {
		t.Fatalf("出现 lost update：期望 %d 次写入全部保留，实际 %d（counts=%v）",
			want, total, counts)
	}
	for tag, c := range counts {
		if c != iters {
			t.Errorf("插件 %s 的写入丢失：期望 %d 次，实际 %d 次", tag, iters, c)
		}
	}
}

// arena 用尽必须显式报错，不得静默截断（§4.4 风险登记）。
func TestSegment_ArenaExhaustionReturnsError(t *testing.T) {
	seg := newTestSegment(t, headerSize+ctxSize+256) // 极小 arena
	big := strings.Repeat("x", 1024)
	err := seg.WriteAll(&pubsdk.StageContext{FinalText: big})
	if err == nil {
		t.Fatal("arena 不足应报错，而非静默截断")
	}
	if !strings.Contains(err.Error(), "arena 空间不足") {
		t.Errorf("错误信息应说明 arena 不足，实际: %v", err)
	}
}

// 压实回收 append-only 垃圾，且不破坏现存字段。
func TestSegment_CompactReclaimsGarbage(t *testing.T) {
	seg := newTestSegment(t, 32*1024)
	if err := seg.WriteAll(&pubsdk.StageContext{FinalText: "初始"}); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}

	// 反复改写同一字段，制造 append-only 垃圾
	for i := 0; i < 50; i++ {
		var local pubsdk.StageContext
		if err := seg.ReadInto(&local); err != nil {
			t.Fatalf("ReadInto: %v", err)
		}
		snap := TakeSnapshot(&local)
		local.FinalText = fmt.Sprintf("第%d次改写内容", i)
		if _, err := seg.WriteDirty(&local, snap); err != nil {
			t.Fatalf("WriteDirty: %v", err)
		}
	}

	usedBefore := seg.ArenaUsed()
	var beforeCtx pubsdk.StageContext
	if err := seg.ReadInto(&beforeCtx); err != nil {
		t.Fatalf("ReadInto: %v", err)
	}

	reclaimed := seg.Compact()
	if reclaimed == 0 {
		t.Error("应回收到垃圾空间")
	}
	if seg.ArenaUsed() >= usedBefore {
		t.Errorf("压实后已用空间应下降：%d → %d", usedBefore, seg.ArenaUsed())
	}

	var afterCtx pubsdk.StageContext
	if err := seg.ReadInto(&afterCtx); err != nil {
		t.Fatalf("压实后 ReadInto: %v", err)
	}
	if afterCtx.FinalText != beforeCtx.FinalText {
		t.Errorf("压实破坏了字段内容：%q → %q", beforeCtx.FinalText, afterCtx.FinalText)
	}
}
