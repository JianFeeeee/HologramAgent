package core

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HomeAgent/internal/agent/api"
	"github.com/JianFeeeee/HomeAgent/internal/memory"
	"github.com/JianFeeeee/HomeAgent/internal/memory/document"
)

// 媒体记忆接线测试（块 union 化之后）：验证媒体从对话进入块、挂到 L0 事件、
// 随归档转到 L2 文档的完整链路。
//
// 核心断言不是"函数被调用了"，而是不变量：
//   1. 媒体进不了块时对话照常（它是记忆增强，不是对话必需品）
//   2. 块的身份是内容派生：同内容同块，跨层迁移身份不变
//   3. 图库为 nil 时全链路静默跳过，行为与本特性上线前一致

func newTestAgentWithMedia(t *testing.T) *Agent {
	t.Helper()
	dir := t.TempDir()
	g, err := memory.NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { g.Close() })

	emb := memory.NewStaticEmbedder()
	a := &Agent{
		memory:  g,
		context: NewRelevanceContext(filepath.Join(dir, "context.json"), emb),
	}
	return a
}

// dataURLString 造 data URL（media 包退场后由本测试自持，与 parseDataURL 对偶）。
func dataURLString(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// imageBlockURL 造一个带指定 URL 的图片块。
// 名字带 URL 后缀是为了不与 modalfallback_test.go 里固定用 testPNG 的
// imageBlock() 撞名——两者用途不同：那个验回退链，这个验入库。
func imageBlockURL(dataURL string) agentAPI.ContentBlock {
	return agentAPI.ContentBlock{
		Type:     "image_url",
		ImageURL: &agentAPI.ImageURL{URL: dataURL, Detail: "auto"},
	}
}

func TestCaptureBlockMedia_StoresDataURL(t *testing.T) {
	a := newTestAgentWithMedia(t)

	raw := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	blocks := []agentAPI.ContentBlock{
		{Type: "text", Text: "看这张图"},
		imageBlockURL(dataURLString("image/png", raw)),
	}

	got := a.captureBlockMedia(blocks, "multimodal_see_picture")
	if len(got) != 1 {
		t.Fatalf("应捕获 1 个媒体块，实际 %d", len(got))
	}
	b := got[0]
	// 字节就在块里（union），身份内容派生
	if string(b.Data) != string(raw) {
		t.Fatal("块字节与输入不一致")
	}
	if b.ID != memory.MediaBlockID("image/png", raw) {
		t.Errorf("块 ID 应内容派生: %q", b.ID)
	}
	if b.MIME != "image/png" || b.Tool != "multimodal_see_picture" || b.Modality != memory.BlockImage {
		t.Fatalf("元数据不对: %+v", b)
	}
	// capture 之后由 task/processInput 做 stage（见 task.go:397/1102）——
	// capture 本身只产出块，不暂存。
}

func TestCaptureBlockMedia_SkipsHTTPURL(t *testing.T) {
	// http(s) URL 拿不到字节就无法成块；"下载它再存"会把一次对话
	// 变成一次网络请求（超时、鉴权、SSRF 全来了），不在本层解决。
	a := newTestAgentWithMedia(t)

	blocks := []agentAPI.ContentBlock{
		imageBlockURL("https://example.com/x.png"),
	}
	if d := a.captureBlockMedia(blocks, "t"); len(d) != 0 {
		t.Fatalf("http URL 不该被捕获，实际 %d 个", len(d))
	}
}

func TestCaptureBlockMedia_NilMemoryIsNoop(t *testing.T) {
	// 块 union 化后 captureBlockMedia 不再依赖外部存储（字节就在块里），
	// 但 stage/drain、bindEventMedia、blockLabel 这些纯内存通路必须照常可用。
	a := &Agent{}
	blocks := []agentAPI.ContentBlock{imageBlockURL(dataURLString("image/png", []byte("x")))}
	got := a.captureBlockMedia(blocks, "t")
	if len(got) != 1 {
		t.Fatalf("捕获不依赖图库，应得 1 个块，实际 %v", got)
	}
	a.stageMediaBlocks(got...)
	if drained := a.drainMediaBlocks(); len(drained) != 1 {
		t.Fatal("stage/drain 不依赖图库，应正常工作")
	}
	// bindEventMedia 对空块列表也必须安全
	evt := &ContextEvent{}
	a.bindEventMedia(evt, nil)
	if len(evt.Blocks) != 0 || evt.ID != "" {
		t.Fatalf("空块列表时不该改动事件: %+v", evt)
	}
	if s := blockLabel(nil); s != "" {
		t.Fatalf("nil 块应产出空标签，得到 %q", s)
	}
}

func TestCaptureBlockMedia_AudioAndVideo(t *testing.T) {
	a := newTestAgentWithMedia(t)

	blocks := []agentAPI.ContentBlock{
		imageBlockURL(dataURLString("image/jpeg", []byte("frame"))),
		{Type: "audio_url", AudioURL: &agentAPI.AudioURL{URL: dataURLString("audio/wav", []byte("sound"))}},
	}
	got := a.captureBlockMedia(blocks, "multimodal_see_video")
	if len(got) != 2 {
		t.Fatalf("应捕获 2 个，实际 %d", len(got))
	}
	if got[0].Modality != memory.BlockImage || got[0].MIME != "image/jpeg" {
		t.Fatalf("图像块元数据不对: %+v", got[0])
	}
	if got[1].Modality != memory.BlockAudio || got[1].MIME != "audio/wav" {
		t.Fatalf("音频块元数据不对: %+v", got[1])
	}
}

func TestStageDrainMediaBlocks(t *testing.T) {
	a := newTestAgentWithMedia(t)

	b1 := mediaBlock("image/png", []byte("a"))
	b2 := mediaBlock("image/png", []byte("b"))
	b3 := mediaBlock("image/png", []byte("c"))

	a.stageMediaBlocks(b1, b2)
	a.stageMediaBlocks(b3)
	got := a.drainMediaBlocks()
	if len(got) != 3 {
		t.Fatalf("应累积 3 个，实际 %d", len(got))
	}
	// drain 后必须清空——否则下一轮对话会把上一轮的媒体又挂一遍
	if again := a.drainMediaBlocks(); again != nil {
		t.Fatalf("drain 后应为空，实际 %v", again)
	}
}

func TestBindEventMedia_CreatesBlocks(t *testing.T) {
	a := newTestAgentWithMedia(t)

	b := mediaBlock("image/png", []byte("img"))
	evt := &ContextEvent{Timestamp: time.Now(), Source: "qq", Input: "看图"}
	a.bindEventMedia(evt, []memory.MemoryBlock{b})

	if evt.ID == "" {
		t.Fatal("应懒生成事件 ID")
	}
	if len(evt.Blocks) != 1 || evt.Blocks[0].ID != b.ID {
		t.Fatalf("事件应持有一等记忆块: %+v", evt.Blocks)
	}
	if evt.Blocks[0].Modality != memory.BlockImage || evt.Blocks[0].MIME != "image/png" {
		t.Fatalf("块元数据不对: %+v", evt.Blocks[0])
	}
	// 字节随事件里的块一起走（union）
	if string(evt.Blocks[0].Data) != "img" {
		t.Fatalf("块字节应随事件: %q", evt.Blocks[0].Data)
	}
}

func TestBindEventMedia_LazyIDOnlyWhenNeeded(t *testing.T) {
	// 绝大多数对话没有媒体，不该为它们都生成 ID 塞进 context.json
	a := newTestAgentWithMedia(t)
	evt := &ContextEvent{Input: "纯文本"}
	a.bindEventMedia(evt, nil)
	if evt.ID != "" {
		t.Fatalf("无媒体时不该生成 ID，得到 %q", evt.ID)
	}
}

func TestBlockLabel_NoGeneratedDescription(t *testing.T) {
	// 标签只用来告诉模型「这条记忆带着哪份媒体、可用该块 ID 取回字节」。
	// 它不包含任何生成的描述：描述式索引是旧机制，已彻底废弃。
	a := newTestAgentWithMedia(t)

	b := mediaBlock("image/png", []byte("img"))
	s := blockLabel(&b)
	if s == "" {
		t.Fatal("应产出标签")
	}
	if !strings.Contains(s, "image/png") {
		t.Fatalf("标签应含 MIME 标注: %q", s)
	}
	if !strings.Contains(s, shortID(b.ID)) {
		t.Fatalf("标签应含短块 ID 供反查: %q", s)
	}
	_ = a
}

func TestPrune_NilDocStoreStillArchives(t *testing.T) {
	// 归档链路不依赖任何外部媒体存储：块随事件走，归档时整体迁移。
	dir := t.TempDir()
	emb := memory.NewStaticEmbedder()
	docStore := document.NewStore(filepath.Join(dir, "docs"), memory.TokenizeWords)
	if err := docStore.Start(); err != nil {
		t.Fatal(err)
	}
	rc := NewRelevanceContext(filepath.Join(dir, "context.json"), emb)

	for i := 0; i < 15; i++ {
		rc.Append(ContextEvent{
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
			Source:    "qq",
			Input:     "内容",
		})
	}
	if n := rc.Prune("查询", 5, docStore); n == 0 {
		t.Fatal("无媒体参与时归档也应正常")
	}
}

func TestContextEvent_BlocksFieldRoundTrip(t *testing.T) {
	// context.json 加字段必须向后兼容：存量文件读回来 Blocks 为空、ID 为空，
	// 不影响任何既有行为。
	dir := t.TempDir()
	path := filepath.Join(dir, "context.json")

	// 写一份"存量格式"（无 id / blocks 字段）
	legacy := `[{"timestamp":"2026-09-04T10:00:00Z","source":"qq","input":"老数据","response":"回复"}]`
	if err := os.WriteFile(path, []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}

	emb := memory.NewStaticEmbedder()
	rc := NewRelevanceContext(path, emb)
	if rc.Len() != 1 {
		t.Fatalf("应读回 1 条，实际 %d", rc.Len())
	}

	// 新写入带记忆块的事件，再读回
	a := newTestAgentWithMedia(t)
	a.context = rc
	b := mediaBlock("image/png", []byte("img"))
	evt := ContextEvent{Timestamp: time.Now(), Source: "qq", Input: "新数据"}
	a.bindEventMedia(&evt, []memory.MemoryBlock{b})
	rc.Append(evt)
	rc.flush()

	rc2 := NewRelevanceContext(path, emb)
	if rc2.Len() != 2 {
		t.Fatalf("应有 2 条，实际 %d", rc2.Len())
	}
	var persisted int
	for _, e := range rc2.Recent(10) {
		persisted += len(e.Blocks)
	}
	if persisted != 1 {
		t.Fatalf("块应随 context.json 持久化，实际 %d 个", persisted)
	}
}

func TestPruneMigratesBlocksToDocument(t *testing.T) {
	// 一等记忆块的 L0→L2 迁移：块随事件离开 Context、进入 Document，
	// 身份（ID/模态/MIME/向量）原样保留；同一块不能同时留在两层。
	// 这条路径不依赖任何引用账本。
	dir := t.TempDir()
	emb := memory.NewStaticEmbedder()
	docStore := document.NewStore(filepath.Join(dir, "docs"), memory.TokenizeWords)
	if err := docStore.Start(); err != nil {
		t.Fatal(err)
	}
	rc := NewRelevanceContext(filepath.Join(dir, "context.json"), emb)

	block := mediaBlock("image/png", []byte("migrate-me"))
	block.Vector = []float64{0.1, 0.2, 0.3}
	block.Fingerprint = "qwen:test"
	rc.Append(ContextEvent{
		Timestamp: time.Now().Add(-time.Hour),
		Source:    "qq", Input: "很久以前的一张图",
		Blocks: []memory.MemoryBlock{block},
	})
	for i := 0; i < 12; i++ {
		rc.Append(ContextEvent{
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
			Source:    "qq", Input: "无关内容",
		})
	}

	if n := rc.Prune("完全不相关的查询", 5, docStore); n == 0 {
		t.Fatal("应有事件被归档")
	}

	// 块应已到达 L2，且身份不变。
	var found *document.Doc
	for _, d := range docStore.RecentDocs(20) {
		if len(d.Blocks) > 0 {
			found = d
			break
		}
	}
	if found == nil {
		t.Fatal("归档文档应持有一等记忆块")
	}
	if len(found.Blocks) != 1 {
		t.Fatalf("文档应有 1 个块，实际 %d", len(found.Blocks))
	}
	got := found.Blocks[0]
	if got.ID != block.ID || got.Modality != block.Modality || got.MIME != block.MIME ||
		got.Fingerprint != block.Fingerprint || len(got.Vector) != len(block.Vector) {
		t.Fatalf("块身份应原样迁移:\n got  %+v\n want %+v", got, block)
	}
	// 字节随块迁移（union）
	if string(got.Data) != string(block.Data) {
		t.Fatalf("块字节应原样迁移: %q vs %q", got.Data, block.Data)
	}

	// 同一块不能同时留在 L0。
	for _, e := range rc.Recent(100) {
		if len(e.Blocks) > 0 {
			t.Fatalf("块仍留在 L0（同一块同时存在于两层）: %+v", e.Blocks)
		}
	}
}
