package agent

// semantic_surface.go —— 基座对类型的**语义计算 syscall**（docs §3.4）。
//
// ## 边界
//
//	归基座（纯计算）              归类型（语义与持久化）
//	────────────────────────────  ──────────────────────────────
//	句 → 稀疏向量                  何时给一条事件算向量
//	句 → 稠密向量                  向量存在哪（context.json / ColdDoc / 块表）
//	图 → 稠密向量                  向量是否落盘、何时失效重算
//	两个稀疏向量 → 余弦            空间隔离判据（DenseFP == queryFP）
//	两个稠密向量 → 余弦            阈值（0.20 / >0.01）、融合权重、FuseVectors
//	空间身份（provider/失败原因）   文本⊕块向量怎么合
//
// ★ 为什么是**批量**而不是单发（用户裁定 2026-11-20）：「输入一个句子输出
// 一个向量、输入两个向量返回相似度，支持一次输入若干个句子」。内核侧
// 退化成无状态 call-reply 之后，N 个句子的向量化是 N 个独立计算，一次
// call 拿全部结果。单发写 `EmbedTexts([]string{x})[0]`，语义完全一致。
//
// ★ 为什么类型不能自己 import 实现包：fastText 模型 50k 词 × 300 维
// （生产实测数百 MB），每个类型各建一份就是同一模型被加载 N 次。
// 状态只在**内核设施**里有一份。

import (
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	agentProvider "github.com/JianFeeeee/HologramAgent/internal/agent/provider"
	"github.com/JianFeeeee/HologramAgent/internal/artifacts"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/semantic"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
)

// MountSemantic 挂载本实例的**语义计算设施**（装配期调用，幂等）。
//
// ★ 为什么是挂载而不是 AgentConfig 字段（docs §2 结论 4）：`AgentConfig` 是
// 配置项透传，不许出现设施句柄；设施由装配点交给基座字段。形状与
// `MountDevice` / `SetBuiltinTools` 一致。
//
// ★ 为什么子实例也挂同一份：设施在内核里是**一份**（fastText 模型数百 MB），
// 各 agent 只是消费者。这与 LLM 设施"只继承句柄、不继承源表"同口径。
func (a *Agent) MountSemantic(embedder Embedder, multimodal kernelapi.MultimodalSpace) {
	if a == nil {
		return
	}
	a.semanticMu.Lock()
	defer a.semanticMu.Unlock()
	if embedder != nil {
		a.embedder = embedder
	}
	if multimodal != nil {
		a.multimodal = multimodal
	}
}

// MountSemanticOps 挂载**归一化判定**与**三元组提取器**（装配期调用，幂等）。
//
// ★ 与 MountSemantic 分开：那两个是"句→向量"的计算，这两个是"名字能不能
// 落库 / 文本→三元组"的判定面。它们的实现分别在记忆包与 nlp 包，都不是
// `FacEmbedder` 的产物，来源不同就不该塞进同一个挂载点。
//
// 契约在 `internal/artifacts`（叶子包），基座只认识接口 —— 它不知道
// nlp 的依存句法、也不知道实体名的长度上限。
func (a *Agent) MountSemanticOps(extractor artifacts.Extractor, norm artifacts.Norm,
	memOps kernelapi.MemoryOps) {
	if a == nil {
		return
	}
	a.semanticMu.Lock()
	defer a.semanticMu.Unlock()
	if extractor != nil {
		a.extractor = extractor
	}
	// ★ Norm 是值类型（两个函数指针）：零值语义是"未装配 ⇒ 放行"，
	//   所以只在真给了判定函数时才覆盖，否则保留零值（行为与历史一致）。
	if norm.ValidName != nil || norm.IsNoiseEntity != nil {
		a.norm = norm
	}
	if memOps != nil {
		a.memOps = memOps
	}
}

// EmbedTexts 批量把句子转成**稀疏**向量（fastText 词向量合成）。
//
// 返回切片与入参一一对应；空串 ⇒ 该位置为空向量（长度仍相等，调用方
// 可以按下标配对，不必额外判空切片）。未装配 ⇒ 返回等长的空向量切片。
func (a *Agent) EmbedTexts(texts []string) []map[string]float64 {
	if len(texts) == 0 {
		return nil
	}
	if a == nil || a.embedder == nil {
		// 未装配 ⇒ 等长空向量：调用方按 len() 配对不会越界，
		// 而"算不出相似度"由 Similarity 的零向量规则自然得到 0。
		return make([]map[string]float64, len(texts))
	}
	return a.embedder.EmbedTexts(texts)
}

// EmbedTextClean 是"先清洗再批量向量化"（调用方几乎总是要这个：原始输入
// 常带首尾标点，直接向量化会把噪声计入特征）。
func (a *Agent) EmbedTextClean(texts []string) []map[string]float64 {
	if len(texts) == 0 {
		return nil
	}
	cleaned := make([]string, len(texts))
	for i, t := range texts {
		cleaned[i] = semantic.CleanText(t)
	}
	return a.EmbedTexts(cleaned)
}

// EmbedderLoaded 报告稀疏词嵌入是否就绪（未就绪 ⇒ 只有 TF-IDF 词频回退）。
func (a *Agent) EmbedderLoaded() bool {
	if a == nil {
		return false
	}
	a.semanticMu.RLock()
	defer a.semanticMu.RUnlock()
	return a.embedder != nil && a.embedder.Loaded()
}

// Similarity 计算两个稀疏向量的余弦相似度（任一为零向量 ⇒ 0）。
//
// ★ 阈值不在这里：**"够不够相似"是类型的判定**（场景路用 0.20，
// 块召回用 >0.01，实体合并另有阈值）。基座只回答"余弦是多少"。
func (a *Agent) Similarity(x, y map[string]float64) float64 {
	return semantic.Cosine(x, y)
}

// EmbedTextsDense 批量把句子转成**稠密**向量（多模态空间的文本编码器）。
//
// 返回 (向量组, ok)：ok=false 表示空间未装配/未加载 ⇒ 调用方必须显式
// 降级到稀疏路，而不是拿一堆零向量去算（那会静默得到"全都不相关"）。
func (a *Agent) EmbedTextsDense(texts []string) ([][]float64, bool) {
	if a == nil || a.multimodal == nil || !a.multimodal.Loaded() || len(texts) == 0 {
		return nil, false
	}
	out := make([][]float64, len(texts))
	for i, t := range texts {
		v, err := a.multimodal.VectorizeDense(t)
		if err != nil || len(v) == 0 {
			// 单句失败不整体失败：其余句子的向量仍可用（调用方按 nil 跳过）。
			continue
		}
		out[i] = v
	}
	return out, true
}

// DenseSimilarity 计算两个稠密向量的余弦（长度不等 ⇒ 0）。
//
// ★ 长度不等意味着**两个不同的向量空间**，比值没有意义。静默给 0 是
// 唯一正确的行为 —— 拿新空间的查询向量比旧空间的块向量，结果"看起来还行
// 但全是错的"，而且不会报错。
func (a *Agent) DenseSimilarity(x, y []float64) float64 {
	return semantic.DenseCosine(x, y)
}

// ImageDense 把一张图片转成稠密向量（多模态空间的图像编码器）。
//
// ok=false ⇒ 空间未装配/未加载/该 provider 不支持图像。稀疏词嵌入
// **不提供**图像能力（fastText 是纯文本模型），所以走这条路前先看
// MultimodalStatus。
func (a *Agent) ImageDense(img []byte, mime string) ([]float64, bool) {
	if a == nil || a.multimodal == nil || !a.multimodal.Loaded() {
		return nil, false
	}
	if len(img) == 0 {
		return nil, false
	}
	v, err := a.multimodal.EmbedImageDense(img, mime)
	if err != nil || len(v) == 0 {
		return nil, false
	}
	return v, true
}

// DenseFingerprint 返回当前稠密空间的指纹（"" ⇒ 未装配/未加载）。
//
// ★ 类型用它做**空间隔离**：库里同时可能存在两个空间的块（迁移期），
// 拿新空间的查询向量比旧空间的块向量，余弦值没有任何意义。判据必须
// 来自**空间对象**而不是库 —— 库只能说明过去，不能说明现在。
func (a *Agent) DenseFingerprint() string {
	if a == nil || a.multimodal == nil || !a.multimodal.Loaded() {
		return ""
	}
	return a.multimodal.Fingerprint()
}

// DenseDim 返回当前稠密空间的维度（0 ⇒ 未装配/未加载）。
func (a *Agent) DenseDim() int {
	if a == nil || a.multimodal == nil || !a.multimodal.Loaded() {
		return 0
	}
	return a.multimodal.Dim()
}

// MultimodalStatus 报告稠密空间的身份（状态面用）。
//
// ok 为 false 时 reason 说清**为什么**（未配置 provider / 打开失败 / 模型缺失），
// 不能只说 "false" —— 运维否则无法区分"没配"与"配了但坏了"。
func (a *Agent) MultimodalStatus() (ok bool, provider, reason string) {
	ks := kstate.Get()
	if ks == nil {
		if a != nil && a.multimodal != nil && a.multimodal.Loaded() {
			return true, "", ""
		}
		return false, "", "内核未装配"
	}
	impl, found := ks.Require(kernelapi.FacMultimodalStatus)
	if !found {
		if a != nil && a.multimodal != nil && a.multimodal.Loaded() {
			return true, "", ""
		}
		return false, "", "多模态状态未登记"
	}
	st, _ := impl.(*kernelapi.MultimodalStatus)
	if st == nil {
		return false, "", "多模态状态类型不符"
	}
	if a != nil && a.multimodal != nil && a.multimodal.Loaded() {
		return true, st.Provider, ""
	}
	if st.Provider == "" {
		return false, "", "未配置 core.multimodal.provider"
	}
	if st.Error != "" {
		return false, st.Provider, st.Error
	}
	return false, st.Provider, "provider 未就绪"
}

// MountSurface 挂载本实例的**面**（装配点调用，幂等）。
//
// ★ 这批字段的共同点（docs §0.1 结论 4/5）：它们全是**设施句柄**
// （IO 面 / 阶段管道 / 安全闸 / 记录表 / 插件表 / LLM 设施），
// 不是配置项、也不是身份声明 ⇒ 不进 `AgentConfig`，由装配点在构造后交给基座。
//
// ★ 子实例的差别（docs §9.6 硬边界 2）：子只继承 `LLM` 设施句柄，
// **不继承** `Provider` / `ProviderManager` —— 源表是内核的东西，
// 各 agent 靠自己的 ID 分开记账与选源。
func (a *Agent) MountSurface(io *agentIO.IOManager, stageHost *kit.StageHostType,
	gate kernelapi.SecurityGate, agents kernelapi.AgentRegistry,
	pluginReg kernelapi.PluginHost, pluginDir string, llm kernelapi.LLMClient) {
	if a == nil {
		return
	}
	a.semanticMu.Lock()
	defer a.semanticMu.Unlock()
	if io != nil {
		a.io = io
	}
	if stageHost != nil {
		a.stageHost = stageHost
	}
	if gate != nil {
		a.gate = gate
	}
	if agents != nil {
		a.agents = agents
	}
	if pluginReg != nil {
		a.pluginReg = pluginReg
	}
	if pluginDir != "" {
		a.pluginDir = pluginDir
	}
	if llm != nil {
		a.llm = llm
	}
	// ★ 挂载后必须接线（输入路由 + L4 夹取）—— 见 wireIO 的注释。
	//   IO 不再经 AgentConfig 传入，所以 New 里那段接线搬到了这里；
	//   漏掉的后果是"通道归属路由与 L4 夹取在装配后静默失效"。
	a.wireIO()
}

// MountProviders 挂载**模型源**（只有主 agent 与需要独立选源的根实例用）。
//
// ★ 子实例**不许**调它（docs §9.6 硬边界 2）：子看不见"有几条源"这件事，
// 它只用 `MountSurface` 给的 `LLM` 设施句柄。绕过去的后果是同时丢掉候选链、
// 跨源回退、可用性标记与错误分类 —— 而且不会报错，只是"配了优先源却没生效"。
func (a *Agent) MountProviders(p agentAPI.Provider, pm *agentProvider.ProviderManager) {
	if a == nil {
		return
	}
	a.semanticMu.Lock()
	defer a.semanticMu.Unlock()
	if p != nil {
		a.provider = p
	}
	if pm != nil {
		a.providerManager = pm
	}
}

// MountTypeFaces 挂载**类型自己的面**（渲染面 / 上下文统计 / 记忆整理能力）。
//
// ★ 为什么它们也在基座上而不是 AgentConfig（docs §0.2 三问）：
//
//	基座要用它们（渲染工具清单、状态面读上下文统计），但基座不认识实现
//	⇒ 只按窄接口持用，实现在类型的包里。形状与 `SetBuiltinTools` 一致。
//
// ★ 这些**不是设施**（不由 kernelmod 提供），而是类型的实现体交出的闭包：
//
//	ToolDefs    工具清单渲染（渲染归类型，docs §0.3）
//	ContextStats 上下文统计（事件数 / 窗口 / 累计 token）
//	DepParser   依存句法分析器的只读事实（状态面用）
func (a *Agent) MountTypeFaces(toolDefs ToolDefsFace, stats ContextStats,
	depParser func() (enabled bool, modelPath string)) {
	if a == nil {
		return
	}
	a.semanticMu.Lock()
	defer a.semanticMu.Unlock()
	if toolDefs != nil {
		a.toolDefs = toolDefs
	}
	if stats != nil {
		a.contextStats = stats
	}
	if depParser != nil {
		a.depParser = depParser
	}
}

// MountMainValues 挂载**主 agent 私有值**（人格 / 人格落库面 / 跨模态融合权重）。
//
// ★ 基座只是"代为持有"：它读不出这些值的语义（`any` 形态），
//
//	类型自己用 `Wrap(a).st` 取回真值。这样基座包不 import 主 agent 的类型。
func (a *Agent) MountMainValues(fusionCfg any, personality any, personaStore any) {
	if a == nil {
		return
	}
	a.semanticMu.Lock()
	defer a.semanticMu.Unlock()
	a.fusionCfg = fusionCfg
	a.personality = personality
	a.personaStore = personaStore
}

// toolDefsFace 返回类型交出的工具清单渲染面（nil ⇒ 用内核工具表兜底）。
func (a *Agent) toolDefsFace() ToolDefsFace {
	if a == nil {
		return nil
	}
	a.semanticMu.RLock()
	defer a.semanticMu.RUnlock()
	return a.toolDefs
}

// contextStatsFace 返回类型交出的上下文统计面（nil ⇒ 状态面跳过该段）。
func (a *Agent) contextStatsFace() ContextStats {
	if a == nil {
		return nil
	}
	a.semanticMu.RLock()
	defer a.semanticMu.RUnlock()
	return a.contextStats
}

// depParserFace 返回依存句法分析器的只读事实闭包（nil ⇒ 未启用）。
func (a *Agent) depParserFace() func() (bool, string) {
	if a == nil {
		return nil
	}
	a.semanticMu.RLock()
	defer a.semanticMu.RUnlock()
	return a.depParser
}

// MainValuesView 是主 agent 私有值的只读视图（基座不解释语义，只代为持有）。
type MainValuesView struct {
	FusionCfg    any
	Personality  any
	PersonaStore any
}

// MainValues 返回主 agent 私有值的只读视图（未挂载 ⇒ 全 nil）。
func (a *Agent) MainValues() MainValuesView {
	if a == nil {
		return MainValuesView{}
	}
	a.semanticMu.RLock()
	defer a.semanticMu.RUnlock()
	return MainValuesView{
		FusionCfg:    a.fusionCfg,
		Personality:  a.personality,
		PersonaStore: a.personaStore,
	}
}

// PluginHost 返回插件注册表读面（未挂载 ⇒ nil）。
func (a *Agent) PluginHost() kernelapi.PluginHost {
	if a == nil {
		return nil
	}
	a.semanticMu.RLock()
	defer a.semanticMu.RUnlock()
	return a.pluginReg
}

// PluginDirPath 返回插件目录（plgreload 用；未挂载 ⇒ ""）。
func (a *Agent) PluginDirPath() string {
	if a == nil {
		return ""
	}
	a.semanticMu.RLock()
	defer a.semanticMu.RUnlock()
	return a.pluginDir
}
