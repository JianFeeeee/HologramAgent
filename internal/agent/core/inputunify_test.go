package core

import (
	"path/filepath"
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HomeAgent/internal/agent/io"
	"github.com/JianFeeeee/HomeAgent/internal/memory"
	"github.com/JianFeeeee/HomeAgent/internal/memory/document"
	pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

// 统一输入主干（processInput / resolveInput / injectedBlocks）与
// 模型可调用工具的媒体接线测试（块 union 化之后）。
//
// 这层此前的结构性缺陷：text 与 image/audio 各有一个 process 函数，
// 媒体那条缺了去重、no_memory、通道 Cleaner、中断语义、EventRawInput 五项。
// 归一成一条主干后，这些行为对所有模态一致——下面的断言就是这个不变量。

func newInputTestAgent(t *testing.T) *Agent {
	t.Helper()
	dir := t.TempDir()

	g, err := memory.NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { g.Close() })

	return &Agent{memory: g}
}

// ---------- injectedBlocks ----------

// 内核内部注入直接给 []agentAPI.ContentBlock；经公共 SDK 的 IOInjector 过来的是
// []pubsdk.ContentBlock。两者字段一致但 Go 不会自动转换，只认一种的后果是
// 另一种被静默丢弃——插件注入的图到 payload 就断了，且不报错。
func TestInjectedBlocks_AcceptsBothStaticTypes(t *testing.T) {
	t.Run("内核类型", func(t *testing.T) {
		blocks, kind := injectedBlocks(map[string]interface{}{
			"media_blocks": []agentAPI.ContentBlock{
				{Type: "text", Text: "看图"},
				{Type: "image_url", ImageURL: &agentAPI.ImageURL{URL: "data:image/png;base64,AAA"}},
			},
		})
		if len(blocks) != 2 {
			t.Fatalf("blocks = %d，期望 2", len(blocks))
		}
		if kind != "image" {
			t.Errorf("mediaType = %q，期望 image", kind)
		}
	})

	t.Run("公共SDK类型", func(t *testing.T) {
		blocks, kind := injectedBlocks(map[string]interface{}{
			"media_blocks": []pubsdk.ContentBlock{
				{Type: "text", Text: "听音频"},
				{Type: "audio_url", AudioURL: &pubsdk.AudioURL{URL: "data:audio/wav;base64,BBB"}},
			},
		})
		if len(blocks) != 2 {
			t.Fatalf("blocks = %d，期望 2（公共 SDK 类型被静默丢弃）", len(blocks))
		}
		if kind != "audio" {
			t.Errorf("mediaType = %q，期望 audio", kind)
		}
		// 转换必须保留 URL，否则块到了模型手上是空的
		if blocks[1].AudioURL == nil || blocks[1].AudioURL.URL != "data:audio/wav;base64,BBB" {
			t.Errorf("AudioURL 转换丢失: %+v", blocks[1].AudioURL)
		}
	})

	t.Run("图优先于音频", func(t *testing.T) {
		_, kind := injectedBlocks(map[string]interface{}{
			"media_blocks": []agentAPI.ContentBlock{
				{Type: "audio_url", AudioURL: &agentAPI.AudioURL{URL: "a"}},
				{Type: "image_url", ImageURL: &agentAPI.ImageURL{URL: "b"}},
			},
		})
		if kind != "image" {
			t.Errorf("mediaType = %q，期望 image", kind)
		}
	})

	t.Run("无媒体块", func(t *testing.T) {
		blocks, kind := injectedBlocks(map[string]interface{}{"content": "纯文本"})
		if blocks != nil || kind != "" {
			t.Errorf("无 media_blocks 时应返回 (nil,\"\")，实际 (%v,%q)", blocks, kind)
		}
	})

	t.Run("ImageURL 的 Detail 透传", func(t *testing.T) {
		blocks, _ := injectedBlocks(map[string]interface{}{
			"media_blocks": []pubsdk.ContentBlock{
				{Type: "image_url", ImageURL: &pubsdk.ImageURL{URL: "u", Detail: "high"}},
			},
		})
		if len(blocks) != 1 || blocks[0].ImageURL.Detail != "high" {
			t.Errorf("Detail 未透传: %+v", blocks)
		}
	})
}

// ---------- resolveInput ----------

func TestResolveInput_UnifiesAllModalities(t *testing.T) {
	a := newInputTestAgent(t)

	t.Run("用户上传图片", func(t *testing.T) {
		in, ok := a.resolveInput(&agentIO.InputEvent{
			Source:  "qq",
			Type:    "image",
			Payload: map[string]interface{}{"data": "AAAA", "mime": "image/png"},
		})
		if !ok {
			t.Fatal("图片输入被判为无效")
		}
		if in.mediaType != "image" || in.captureTool != "input_image" {
			t.Errorf("mediaType=%q captureTool=%q", in.mediaType, in.captureTool)
		}
		if in.text == "" {
			t.Error("纯媒体输入应有 alt 文案作为文本落点")
		}
		if len(in.blocks) == 0 {
			t.Error("图片应转成内容块")
		}
	})

	t.Run("插件注入的媒体", func(t *testing.T) {
		in, ok := a.resolveInput(&agentIO.InputEvent{
			Source: "myplugin",
			Type:   "text",
			Payload: map[string]interface{}{
				"content": "帮我看看这张图",
				"media_blocks": []pubsdk.ContentBlock{
					{Type: "image_url", ImageURL: &pubsdk.ImageURL{URL: "data:image/png;base64,AAA"}},
				},
			},
		})
		if !ok {
			t.Fatal("带媒体的文本输入被判为无效")
		}
		if in.text != "帮我看看这张图" {
			t.Errorf("text = %q", in.text)
		}
		if len(in.blocks) != 1 || in.mediaType != "image" {
			t.Errorf("blocks=%d mediaType=%q —— 插件注入的媒体到 payload 就断了", len(in.blocks), in.mediaType)
		}
		if in.captureTool != "inject_myplugin" {
			t.Errorf("captureTool = %q，期望带来源便于溯源", in.captureTool)
		}
	})

	t.Run("只带图不带字也合法", func(t *testing.T) {
		_, ok := a.resolveInput(&agentIO.InputEvent{
			Source: "myplugin",
			Type:   "text",
			Payload: map[string]interface{}{
				"media_blocks": []agentAPI.ContentBlock{
					{Type: "image_url", ImageURL: &agentAPI.ImageURL{URL: "u"}},
				},
			},
		})
		if !ok {
			t.Error("只带媒体不带文本应视为有效输入（插件注入常这样）")
		}
	})

	t.Run("文本与媒体都空才无效", func(t *testing.T) {
		if _, ok := a.resolveInput(&agentIO.InputEvent{
			Source:  "cli",
			Type:    "text",
			Payload: map[string]interface{}{"content": ""},
		}); ok {
			t.Error("空输入应被拒")
		}
	})

	t.Run("纯文本", func(t *testing.T) {
		in, ok := a.resolveInput(&agentIO.InputEvent{
			Source:  "cli",
			Type:    "text",
			Payload: map[string]interface{}{"content": "你好"},
		})
		if !ok || in.text != "你好" || len(in.blocks) != 0 || in.mediaType != "" {
			t.Errorf("纯文本路径异常: ok=%v in=%+v", ok, in)
		}
	})
}

// ---------- 模型工具侧：media_refs 结构化传递 ----------

// 模型只知道块 ID（从对话或 memory_recall 的「关联媒体」读到）。
// 它不再需要自己拼任何标记：块引用作为结构化字段随三元组提交。
func TestResolveMediaRefsAndNoMarkerText(t *testing.T) {
	a := newInputTestAgent(t)
	b := mediaBlock("image/png", []byte("marker-bytes"))
	if err := a.memory.PutMemoryBlocks([]memory.MemoryBlock{b}); err != nil {
		t.Fatalf("PutMemoryBlocks: %v", err)
	}

	t.Run("短引用补全", func(t *testing.T) {
		got := a.resolveMediaRefs([]string{b.ID[:12]})
		if len(got) != 1 || got[0] != b.ID {
			t.Fatalf("短引用应补全为完整块 ID，得到 %v", got)
		}
	})

	t.Run("无法解析的引用被丢弃", func(t *testing.T) {
		if got := a.resolveMediaRefs([]string{"ffffffffffff"}); len(got) != 0 {
			t.Errorf("不存在的块引用不该保留: %v", got)
		}
	})

	t.Run("无图库时返回nil", func(t *testing.T) {
		bare := &Agent{}
		if got := bare.resolveMediaRefs([]string{b.ID}); got != nil {
			t.Errorf("无图库时应返回 nil: %v", got)
		}
	})
}

// 句子文本必须保持原样：媒体归属走结构化块边，不往文本里贴 marker。
func TestMemoryCommit_DoesNotPolluteSentenceText(t *testing.T) {
	a := newInputTestAgent(t)
	b := mediaBlock("image/png", []byte("clean-sentence"))
	if err := a.memory.PutMemoryBlocks([]memory.MemoryBlock{b}); err != nil {
		t.Fatal(err)
	}

	sentence := "用户发来一张图。"
	triples := []memory.Triple{{
		Subject: "用户", Relation: "发来", Object: "图片",
		SentenceText: sentence,
		MediaDigests: a.resolveMediaRefs([]string{b.ID[:12]}),
	}}
	if _, _, _, err := a.commitTriplesWithMedia(triples, "s1", 0, nil); err != nil {
		t.Fatal(err)
	}

	res, err := a.memory.Recall([]string{"用户"}, nil, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Relations) == 0 {
		t.Fatal("召回为空")
	}
	if res.Relations[0].SentenceText != sentence {
		t.Errorf("句子文本被污染: %q", res.Relations[0].SentenceText)
	}
	// ★ 挂载点是「原句块 ID」
	blocks, err := a.memory.BlocksForNode("block",
		memory.SentenceBlockID(res.Relations[0].SentenceText))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].ID != b.ID {
		t.Errorf("块应挂到句子，实际 %+v", blocks)
	}
}

// ---------- resolveMediaRefs ----------

func TestResolveMediaRefs(t *testing.T) {
	a := newInputTestAgent(t)
	b1 := mediaBlock("image/png", []byte("one"))
	b2 := mediaBlock("image/png", []byte("two"))
	if err := a.memory.PutMemoryBlocks([]memory.MemoryBlock{b1, b2}); err != nil {
		t.Fatal(err)
	}

	got := a.resolveMediaRefs([]string{b1.ID[:10], b2.ID, b1.ID, "ffffffffffff"})
	if len(got) != 2 {
		t.Fatalf("got = %v，期望 2 条（去重 + 丢弃无法解析的）", got)
	}
	for _, id := range got {
		if !strings.HasPrefix(id, "blk_media_") {
			t.Errorf("应返回完整块 ID，实际 %q", id)
		}
	}

	if a.resolveMediaRefs(nil) != nil {
		t.Error("空输入应返回 nil")
	}
	bare := &Agent{}
	if bare.resolveMediaRefs([]string{b1.ID}) != nil {
		t.Error("无图库时应返回 nil")
	}
}

// ---------- 文档持有的一等记忆块 ----------

func TestDocCommit_StoresBlocks(t *testing.T) {
	// doc_commit 带媒体时，媒体应作为一等块直接存在文档上，
	// 并随 doc 一起持久化（块自带字节，不靠外部引用保活）。
	dir := t.TempDir()
	ds := document.NewStore(filepath.Join(dir, "docs"), memory.TokenizeWords)
	if err := ds.Start(); err != nil {
		t.Fatal(err)
	}
	defer ds.Stop()

	b1 := mediaBlock("image/png", []byte("doc-one"))
	b2 := mediaBlock("image/png", []byte("doc-two"))

	doc := &document.Doc{ID: "doc_x", Summary: "s", Content: "c", Blocks: []memory.MemoryBlock{b1, b2}}
	if err := ds.Insert(doc); err != nil {
		t.Fatal(err)
	}

	blocks := ds.Blocks()
	if len(blocks) != 2 {
		t.Fatalf("文档应持有 2 个块，实际 %d", len(blocks))
	}
	seen := map[string]bool{}
	for _, b := range blocks {
		seen[b.ID] = true
	}
	if !seen[b1.ID] || !seen[b2.ID] {
		t.Errorf("块身份不对: %+v", blocks)
	}
}

// ---------- 文档持有块标签（doc_query 展示用） ----------

func TestBlockLabelsForDoc(t *testing.T) {
	a := newInputTestAgent(t)
	b := mediaBlock("image/png", []byte("ctx-bytes"))

	t.Run("从文档持有的一等块渲染", func(t *testing.T) {
		got := a.blockLabelsForDoc(&document.Doc{ID: "doc_1", Blocks: []memory.MemoryBlock{b}})
		if !strings.Contains(got, shortID(b.ID)) {
			t.Errorf("标签应含短块 ID: %q", got)
		}
		if !strings.Contains(got, "image/png") {
			t.Errorf("标签应含 MIME: %q", got)
		}
	})

	t.Run("无块时为空", func(t *testing.T) {
		if got := a.blockLabelsForDoc(&document.Doc{ID: "doc_x", Content: "普通正文"}); got != "" {
			t.Errorf("应返回空串，实际 %q", got)
		}
	})
}

// ---------- blockLabel ----------

// 媒体标签的唯一生成处：只含 MIME 与短块 ID，不含任何生成的描述。
func TestBlockLabel(t *testing.T) {
	b := mediaBlock("image/png", []byte("labelled"))
	got := blockLabel(&b)
	if !strings.Contains(got, "image/png") {
		t.Errorf("标签应含 MIME: %q", got)
	}
	if !strings.Contains(got, shortID(b.ID)) {
		t.Errorf("必须带短块 ID 供反查: %q", got)
	}
	if got := blockLabel(nil); got != "" {
		t.Errorf("nil 应返回空串，实际 %q", got)
	}
}

// ---------- 模型工具端到端：memory_commit / doc_commit / doc_query ----------

func newToolTestAgent(t *testing.T) *Agent {
	t.Helper()
	dir := t.TempDir()

	g, err := memory.NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { g.Close() })

	ds := document.NewStore(filepath.Join(dir, "documents"), memory.TokenizeWords)
	if err := ds.Start(); err != nil {
		t.Fatalf("doc store: %v", err)
	}
	t.Cleanup(func() { ds.Stop() })

	emb := memory.NewStaticEmbedder("")
	a := &Agent{
		id:       "tester",
		memory:   g,
		docStore: ds,
		context:  NewRelevanceContext("", emb),
	}
	return a
}

// memory_commit 带 media_refs：三元组入库后必须能从句子反查回那份字节。
func TestToolMemoryCommit_BindsMedia(t *testing.T) {
	a := newToolTestAgent(t)
	b := mediaBlock("image/png", []byte("commit-bytes"))
	if err := a.memory.PutMemoryBlocks([]memory.MemoryBlock{b}); err != nil {
		t.Fatal(err)
	}

	out := a.executeMemoryTool(agentAPI.ToolCall{
		Name: "memory_commit",
		Arguments: map[string]interface{}{
			"triples": []interface{}{
				map[string]interface{}{
					"subject":       "配色方案",
					"relation":      "参考",
					"object":        "三色带图",
					"media_digests": []interface{}{b.ID[:12]},
				},
			},
		},
	}, nil)
	if !strings.Contains(out, "关联") {
		t.Errorf("返回值应告知模型媒体已关联: %q", out)
	}

	res, err := a.memory.Recall([]string{"配色方案"}, nil, 2, "")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	// ★ 断言改用**原句文本**（2026-10-04）：
	//   SentenceID 是旧 sentences 表行号，退场后恒为 0。
	if len(res.Relations) == 0 || res.Relations[0].SentenceText == "" {
		t.Fatal("没有句子落点 —— 媒体引用无从挂起")
	}
	// ★ 挂载点是「原句块 ID」
	blocks, err := a.memory.BlocksForNode("block",
		memory.SentenceBlockID(res.Relations[0].SentenceText))
	if err != nil {
		t.Fatalf("BlocksForNode: %v", err)
	}
	if len(blocks) != 1 || blocks[0].ID != b.ID {
		t.Errorf("句子块 = %+v，期望 [%s]", blocks, b.ID)
	}
	// 字节随块可取回（块即 union）
	data, err := a.memory.BlockData(b.ID)
	if err != nil || string(data) != "commit-bytes" {
		t.Errorf("块字节应可取回: %q / %v", data, err)
	}
}

// 不带 media_refs 时行为与本特性上线前一致（不多写句子、不报错）。
func TestToolMemoryCommit_WithoutMedia(t *testing.T) {
	a := newToolTestAgent(t)
	out := a.executeMemoryTool(agentAPI.ToolCall{
		Name: "memory_commit",
		Arguments: map[string]interface{}{
			"triples": []interface{}{
				map[string]interface{}{"subject": "甲方", "relation": "签署", "object": "合同"},
			},
		},
	}, nil)
	if strings.Contains(out, "失败") {
		t.Errorf("普通提交不该失败: %q", out)
	}
	if strings.Contains(out, "关联") {
		t.Errorf("无媒体时不该提媒体: %q", out)
	}
}

// sentence_text 必须透传：丢了它，图谱就回不到原文。
func TestToolMemoryCommit_CarriesSentenceText(t *testing.T) {
	a := newToolTestAgent(t)
	a.executeMemoryTool(agentAPI.ToolCall{
		Name: "memory_commit",
		Arguments: map[string]interface{}{
			"triples": []interface{}{
				map[string]interface{}{
					"subject":       "李四",
					"relation":      "住在",
					"object":        "杭州",
					"sentence_text": "李四搬到杭州已经三年了。",
				},
			},
		},
	}, nil)
	res, _ := a.memory.Recall([]string{"李四"}, nil, 2, "")
	if len(res.Relations) == 0 {
		t.Fatal("召回为空")
	}
	if res.Relations[0].SentenceText != "李四搬到杭州已经三年了。" {
		t.Errorf("SentenceText = %q", res.Relations[0].SentenceText)
	}
}

// doc_commit 带 media_refs：媒体成为文档直接持有的一等块；正文保持原样。
func TestToolDocCommit_BindsMedia(t *testing.T) {
	a := newToolTestAgent(t)
	b := mediaBlock("image/png", []byte("doc-commit-bytes"))
	if err := a.memory.PutMemoryBlocks([]memory.MemoryBlock{b}); err != nil {
		t.Fatal(err)
	}

	out := a.executeDocTool(agentAPI.ToolCall{
		Name: "doc_commit",
		Arguments: map[string]interface{}{
			"content":       "这是一篇带图的笔记正文。",
			"summary":       "带图笔记",
			"media_digests": []interface{}{b.ID[:12]},
		},
	})
	if !strings.Contains(out, "关联") {
		t.Errorf("返回值应告知模型媒体已关联: %q", out)
	}

	docs := a.docStore.RecentDocs(5)
	if len(docs) == 0 {
		t.Fatal("文档未写入")
	}
	d := docs[0]
	if strings.Contains(d.Content, "image/png") {
		t.Errorf("正文不该被媒体标记污染: %q", d.Content)
	}
	var held bool
	for _, blk := range d.Blocks {
		if blk.ID == b.ID {
			held = true
		}
	}
	if !held {
		t.Errorf("文档应持有一等记忆块 [%s]，实际 %+v", b.ID, d.Blocks)
	}
}

// doc_query 必须把媒体说明附在返回值里，否则模型检索到带图文档也不知道有图。
func TestToolDocQuery_ShowsMedia(t *testing.T) {
	a := newToolTestAgent(t)
	b := mediaBlock("image/png", []byte("query-bytes"))
	if err := a.memory.PutMemoryBlocks([]memory.MemoryBlock{b}); err != nil {
		t.Fatal(err)
	}

	a.executeDocTool(agentAPI.ToolCall{
		Name: "doc_commit",
		Arguments: map[string]interface{}{
			"content":       "紫蓝红三色带配色说明正文",
			"summary":       "紫蓝红三色带",
			"media_digests": []interface{}{b.ID},
		},
	})

	a.executeDocTool(agentAPI.ToolCall{
		Name:      "doc_query",
		Arguments: map[string]interface{}{"query": "紫蓝红三色带 配色说明", "top_k": float64(3)},
	})

	// 正文进的是 cold_storage 事件（工具返回值只给引用编号），媒体说明也在那里。
	var found bool
	for _, e := range a.context.Recent(10) {
		if strings.Contains(e.Response, shortID(b.ID)) {
			found = true
		}
	}
	if !found {
		t.Error("doc_query 未把媒体说明带进上下文 —— 模型不知道这篇文档带过图")
	}
}

// 引用了不存在的块时三个工具的行为与本特性上线前完全一致（不报错、不挂边）。
func TestTools_UnknownRefDegrades(t *testing.T) {
	a := newToolTestAgent(t)

	out := a.executeMemoryTool(agentAPI.ToolCall{
		Name: "memory_commit",
		Arguments: map[string]interface{}{
			"triples": []interface{}{
				map[string]interface{}{
					"subject": "无存储", "relation": "仍可", "object": "提交",
					"media_digests": []interface{}{"aabbccddeeff"},
				},
			},
		},
	}, nil)
	if strings.Contains(out, "失败") {
		t.Errorf("引用未知块时提交不该失败: %q", out)
	}

	out = a.executeDocTool(agentAPI.ToolCall{
		Name: "doc_commit",
		Arguments: map[string]interface{}{
			"content":       "引用未知块的文档",
			"media_digests": []interface{}{"aabbccddeeff"},
		},
	})
	if strings.Contains(out, "失败") {
		t.Errorf("引用未知块时文档写入不该失败: %q", out)
	}
}
