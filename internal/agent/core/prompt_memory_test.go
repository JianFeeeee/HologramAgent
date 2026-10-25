package core

// 三层记忆的**提示词诚实性**判据。
//
// 这类判据的目的是防止「提示词在说谎」——它比不说更糟：
// 模型按提示词行事，而实现不是那样，于是行为与预期系统性错位。
//
// 生产实测（2026-10-22 日志）：agent 几乎不调用 doc_query，即使调用也
// 只看返回值、不知道原文已被回填到对话时序。根因是：
//   · 工具描述只有「查询文档记忆。返回相关文档摘要」一句（且与真实行为不符）
//   · 系统提示词从未解释 L1/L2/L3 是什么

import (
	"strings"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory/document"
)

// ① doc_query 的描述必须覆盖五项，且与真实行为一致。
func TestDocQueryDescriptionCoversRealBehavior(t *testing.T) {
	a := newToolTestAgent(t)
	desc := findToolDescription(t, a, "doc_query")

	mustHave := []struct {
		name string
		sub  string
	}{
		{"L2 是什么", "历史记忆层"},
		{"何时该用", "何时用"},
		{"调用后行为=回填时序", "插回你的对话时序"},
		{"原始时间戳", "原始时间戳"},
		{"与 memory_recall 分工", "memory_recall"},
		{"副作用=消费式取出", "消费式"},
	}
	for _, m := range mustHave {
		if !strings.Contains(desc, m.sub) {
			t.Errorf("doc_query 描述缺少「%s」（应含 %q）：\n%s", m.name, m.sub, desc)
		}
	}
	// 不得再声称"返回摘要"—— 那是旧实现的谎
	if strings.Contains(desc, "返回相关文档摘要") {
		t.Error("doc_query 描述仍声称『返回相关文档摘要』，与真实行为（回填原文到时序）不符")
	}
}

// ② doc_commit 描述必须说清「适合/不适合写什么」，并与另外两层分工。
func TestDocCommitDescriptionExplainsScope(t *testing.T) {
	a := newToolTestAgent(t)
	desc := findToolDescription(t, a, "doc_commit")

	for _, sub := range []string{"适合写什么", "不适合写什么", "memory_commit", "knowledge_create"} {
		if !strings.Contains(desc, sub) {
			t.Errorf("doc_commit 描述缺少 %q：\n%s", sub, desc)
		}
	}
}

// ③ 系统提示词必须有三层记忆段落，且语义与实现一致。
//
// ★ 这条是**跨文件一致性**判据：提示词说"按原始时间戳插回时序"，
//
//	实现就必须真的那样做。若哪天 restoreDocEvents 被改回造合成事件，
//	这里必须红 —— 否则提示词又开始说谎。
func TestSystemPromptExplainsThreeLayers(t *testing.T) {
	a := newToolTestAgent(t)
	// memContext 为空即可：三层记忆段落是内核无条件注入的
	prompt := a.buildSystemPrompt("", "测试输入")

	for _, sub := range []string{
		"【三层记忆】",
		"对话时序（L1",
		"历史记忆（L2",
		"图记忆（L3",
		"doc_query",
		"memory_recall",
	} {
		if !strings.Contains(prompt, sub) {
			t.Errorf("系统提示词缺少三层记忆段落的 %q", sub)
		}
	}
	// 提示词承诺「按原始时间戳插回」，实现必须真的这么做
	if !strings.Contains(prompt, "原始时间戳") {
		t.Error("提示词没有说明 doc_query 会按原始时间戳回填")
	}
	if !strings.Contains(prompt, "插回你的对话时序") {
		t.Error("提示词没有说明 doc_query 把事件插回时序（模型会以为只是返回摘要）")
	}
}

// ④ doc_commit 不得再有 tags 参数（Doc.Tags 已删除，留着会骗模型）。
func TestDocCommitHasNoTagsParam(t *testing.T) {
	a := newToolTestAgent(t)
	params := findToolParams(t, a, "doc_commit")
	if _, ok := params["tags"]; ok {
		t.Error("doc_commit 仍有 tags 参数，但 Doc.Tags 已删除 —— 参数会静默丢弃")
	}
}

// ---- helpers ----

func findToolDescription(t *testing.T, a *Agent, name string) string {
	t.Helper()
	for _, raw := range a.buildToolDefs() {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		fn, ok := m["function"].(map[string]interface{})
		if !ok {
			continue
		}
		if fn["name"] == name {
			d, _ := fn["description"].(string)
			return d
		}
	}
	t.Fatalf("工具 %s 不在 buildToolDefs 里", name)
	return ""
}

func findToolParams(t *testing.T, a *Agent, name string) map[string]interface{} {
	t.Helper()
	for _, raw := range a.buildToolDefs() {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		fn, ok := m["function"].(map[string]interface{})
		if !ok {
			continue
		}
		if fn["name"] == name {
			p, _ := fn["parameters"].(map[string]interface{})
			props, _ := p["properties"].(map[string]interface{})
			return props
		}
	}
	t.Fatalf("工具 %s 不在 buildToolDefs 里", name)
	return nil
}

// ⑤ 预填充注入的文案必须：给真实内容 + 明确引导用 doc_query。
//
// 断点史：旧文案只给「来自 4 个来源的 61 条对话 相关度:0.78」——
// 统计描述，没有任何内容，也没有一个字提示可以取全文。
// 生产日志里 agent 从不使用预填充给的信息，直接原因就是这个。
//
// ★ 用**真实归档路径**（ContextToDoc）构造文档，不手工拼 DocEvent：
//
//	块 ID 由 entryTextBlockID 派生，手工拼的 ID 不匹配会被判为非本事件块，
//	那样测的是"我拼的数据对不对"，不是生产过程。
func TestPrefillMarkdownCarriesContentAndGuide(t *testing.T) {
	a := newToolTestAgent(t)

	if _, err := a.docStore.ContextToDoc("context_archived", []document.ContextEntry{{
		Timestamp: time.Date(2026, 7, 8, 9, 10, 0, 0, time.UTC),
		Source:    "webui",
		Content:   "配色方案怎么定？",
		Response:  "紫蓝红三色带的配色方案按六比三比一，主色紫、辅色蓝、点缀红。",
	}}, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	doc := a.docStore.RecentDocs(1)[0]

	md := a.crossModalMarkdown([]CrossModalHit{{Doc: doc, DocScore: 0.8, Fused: 0.8}})

	if md == "" {
		t.Fatal("预填充文案为空")
	}
	// 1) 必须给真实内容（正文片段），不是模板摘要
	if !strings.Contains(md, "紫蓝红") && !strings.Contains(md, "配色方案") {
		t.Errorf("预填充文案没有真实正文片段：\n%s", md)
	}
	// 2) 不得再出现 jieba 模板摘要
	if strings.Contains(md, "涉及:") {
		t.Errorf("预填充文案仍是 jieba 模板摘要：\n%s", md)
	}
	// 3) 必须有时间
	if !strings.Contains(md, "2026-07-08") {
		t.Errorf("预填充文案没有时间：\n%s", md)
	}
	// 4) ★ 必须明确引导使用 doc_query
	if !strings.Contains(md, "doc_query") {
		t.Errorf("预填充文案没有引导使用 doc_query：\n%s", md)
	}
	// 5) 必须说明它来自历史层而非当前对话
	if !strings.Contains(md, "L2") && !strings.Contains(md, "历史记忆") {
		t.Errorf("预填充文案没有说明来源层：\n%s", md)
	}
}

// ⑥ 技术性内容不得留在系统提示词里（收敛到 skill / 工具描述）。
//
// 背景（用户 2026-10-22）：agent 说话人机味重。查明主因不是性格设定，
// 而是系统提示词里堆了**模型无法据以行动的实现细节**：
//
//	【工具执行顺序】1272 字（ParallelSafe 声明、批内降级、内核判据…）
//	【三层记忆】   1069 字（L1/L2/L3 编号、消费式删除、向量空间…）
//
// 合计占提示词的绝大部分，而且全是"说明书腔"。模型学不到怎么用它，
// 却会照着这种语气说话。收敛后两段合计不到 500 字。
//
// 本判据钉住：这些术语不得回到系统提示词。
func TestSystemPromptHasNoImplementationJargon(t *testing.T) {
	a := newToolTestAgent(t)
	prompt := a.buildSystemPrompt("", "测试输入")

	jargon := []string{
		"ParallelSafe",  // 工具声明项，模型不判断也改不了
		"batchRunnable", // 内核内部函数名
		"消费式",           // L2 取出语义的实现说法
		"DenseCosine",   // 算法名
		"fingerprint",   // 向量空间标识
		"向量空间",          // 实现概念
		"批内降级",          // 内核调度细节
		"自激",            // 设计隐患，模型无需知道
	}
	for _, j := range jargon {
		if strings.Contains(prompt, j) {
			t.Errorf("系统提示词含实现术语 %q —— 技术内容应下沉到 skill / 工具描述，\n"+
				"留在这里只会让回复带上说明书腔，且模型无法据此行动", j)
		}
	}
}

// ⑦ 提示词长度上限（防止技术说明再次淤积）。
//
// 历史：收敛前 2309 → 收敛后 1723 → 上限取 2000（留 277 字增删余地）。
//
// ★ 2026-10-25 的一次真实教训：
//   我曾为 seq 加【加速序列】115 字，超限后把上限调到 2100 —— 那是错的。
//   用户裁定"seq 知识应在 skill 中"，删掉该块后回到 1907 字，上限也还原 2000。
//
//   所以这条判据超限时的**正确反应是删内容或下沉到 skill**，不是调数字。
//   领域知识（某个工具什么时候该用、参数怎么填）属于 skill；
//   这里只该放"模型不看文档也会做错"的通用规则。
func TestSystemPromptStaysCompact(t *testing.T) {
	a := newToolTestAgent(t)
	prompt := a.buildSystemPrompt("", "测试输入")

	const maxRunes = 2000
	if n := len([]rune(prompt)); n > maxRunes {
		t.Errorf("系统提示词 %d 字，超过上限 %d —— \n"+
			"新加的内容先问：模型会据此做什么不同的事？答不上来就该进 skill 或工具描述",
			n, maxRunes)
	}
}
