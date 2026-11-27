package mainagent

// install.go —— 主 agent 私有服务面的**装配**（记忆整理 / 媒体 / 插件 / 人格）。
//
// ★ 这是主 agent 自己的装配，不是基座的：基座不认识"记忆整理面"是什么。

import (
	"time"

	agentPkg "github.com/JianFeeeee/HologramAgent/internal/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	mainmem "github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/memory"
	"github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/services"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
)

// installState 装入主 agent 的专属服务面（记忆/媒体/插件/人格）。
//
// ★ 这是**主 agent 的**装配（记忆整理/媒体/插件/预算统计/设备）。
// 驻留子、rtchild、工作式子不调它 ⇒ 它们连一个用不到的句柄都不持有。
// installState 装入主 agent 的专属服务面。
//
// ★ 这些服务（图记忆 / 冷文档 / 文本记忆 / 变更追踪 / 多模态）
//
//	**是主 agent 自己的实现**，由 `services.Install` 自己创建并持有，
//	不再由内核填进 AgentConfig（2026-11-20 迁移）。
func installState(m *Module, cfg agent.AgentConfig) {
	if m == nil || m.a == nil {
		return
	}
	st := m.st
	// ★ 服务来源有两条，**显式给的优先**：
	//
	//	① cfg 里给了（探针/独立工具显式注入，测试就是这么用的）
	//	② 没给 ⇒ 由本类型自己装配（`services.Install`，生产路径）
	//
	// 迁移前这些全由内核装配点填进 cfg；现在生产路径由主 agent 自己装。
	setIfNil := func(dst *any, src any) {}
	_ = setIfNil
	// ★ 语义计算的两个句柄**从基座取**（docs §3.1 / §3.4）：
	// 它们是内核设施（FacEmbedder / FacMultimodal），由装配点挂到基座上，
	// 类型侧只经基座 syscall 使用。主 agent 的私有服务需要持有它们，
	// 是因为记忆面（索引器 / 文档库 / 跨模态召回）要传给自己的实现。
	st.sem = m.a
	// ★ 服务来源有两条（2026-11-20 起**都不经 AgentConfig**）：
	//
	//	① 显式注入：探针 / 独立工具 / 判据用 `InjectServices(...)` 挂上来
	//	② 生产：由本类型自己装配（`services.Install`）
	//
	// 迁移前两者都走 AgentConfig 字段，于是"记忆子系统"又出现在基座
	// 的构造参数里（docs §2 结论 4 禁止）。现在只有这条 if 在分叉。
	if st.injected {
		// 显式注入的句柄已在 InjectServices 里就位，这里不再覆盖。
	} else {
		// ★ 只给**配置读面 + 数据目录**：主 agent 的私有服务不认识 kstate。
		var cfgStore kernelapi.ConfigStore
		if m.a != nil {
			cfgStore = m.a.Cfg()
		}
		sv := services.Install(services.Deps{Cfg: cfgStore, DataDir: cfg.DataDir})
		m.sv = sv
		if sv != nil {
			// ★ typed-nil 归一化：`*memory.Indexer(nil)` 装进 `kernelapi.MemoryIndexer`
			//   接口后**不是 nil**（经典陷阱）。不折成真 nil，`if st.indexer != nil`
			//   会放行，随后 `BuildContextInScene` 解引用空指针 panic。
			// 图库句柄（本类型私有）。
			m.graph = sv.Memory
			if sv.Indexer != nil {
				st.indexer = sv.Indexer
			}
			if sv.Tracker != nil {
				st.tracker = sv.Tracker
			}
			if sv.DocStore != nil {
				st.coldcontextStore = sv.DocStore
			}
			if sv.Social != nil {
				st.social = sv.Social
			}
			if sv.TextMemory != nil {
				st.textMem = sv.TextMemory
			}
		}
	}
	// ★ 稠密空间的接线（原在基座的 `install_context.go`，随阶段 6 删文件一起丢失）。
	//
	// 三处都要接，缺一个就是一类静默降级：
	//   ctx         L1 事件裁剪/页剔除/冷召回估算的余弦
	//   indexer     场景召回的相关性排序（缺它退回"时间倒序"，rt 标签霸榜）
	//   coldcontext  L2 文档检索 + 跨模态视觉路（缺它整条视觉路死）
	//
	// ★ 必须用**基座 syscall**而不是直接拿空间对象（docs §1 结论 5）：
	// 类型不持内核对象。所以这里传的是包一层 syscall 的适配器。
	if m.a.DenseFingerprint() != "" && st.indexer != nil {
		st.indexer.SetDenseSpace(m.DenseCall())
	}
	if m.a.DenseFingerprint() != "" && st.coldcontextStore != nil {
		st.coldcontextStore.SetDenseSpace(m.DenseCall())
		st.coldcontextStore.BuildDenseIndex(m.DenseCall())
	}

	// ★ 基座的面：直接问基座要（它是挂载点，不是配置字段）。
	st.stageHost = m.a.StageHost()
	st.pluginReg = m.a.PluginHost()
	st.pluginDir = m.a.PluginDirPath()
	// ★ 主 agent 私有值由**本类型**装配（不经 AgentConfig，docs §2 结论）。
	//   基座侧只留一份 `any` 以便状态面/工具面取用，语义归本包。
	if v, ok := m.a.MainValues().FusionCfg.(CrossModalFusionConfig); ok {
		st.fusionCfg = v
	}
	if p, ok := m.a.MainValues().Personality.(*agentPkg.Personality); ok {
		st.personality = p
	}
	if v, ok := m.a.MainValues().PersonaStore.(PersonaStore); ok {
		st.personaStore = v
	}
	// ★ 蒸馏器的语义验证用**基座 syscall 的适配器**（docs §3.4）：
	// 它需要"句 → 向量"，而词嵌入是内核设施（全进程一份）。
	// 传实现对象会让本类型持有内核资产 —— 传适配器则只是一次转发。
	if m.sv != nil && m.sv.Distiller != nil {
		m.sv.Distiller.SetEmbedder(embedderFaceOf(m.a))
	}
	st.startTime = time.Now()
	st.pluginHealth = kit.NewPluginHealthTracker()
	st.noMergeMarkers = make(map[string]int)
}

// InjectServices 显式注入主 agent 的私有服务句柄（探针 / 独立工具 / 判据用）。
//
// ★ 生产路径**不走这里**：生产由 `services.Install` 自己装配（见 installState）。
// 这个方法存在的原因是判据要能只换掉一个依赖（例如注入一个 TF-IDF 回退的
// coldcontext 来钉住归档语义），而不必跑整套装配。
//
// ★ 为什么不是 AgentConfig 字段（docs §2 结论 4）：`AgentConfig` 是配置项
// 透传，记忆子系统是**主 agent 的私有实现**，不是设施句柄也不是配置。
func (m *Module) InjectServices(mem *mainmem.GraphDB, indexer kernelapi.MemoryIndexer,
	tracker kernelapi.Tracker, docs kernelapi.DocStore, social kernelapi.SocialStore,
	text kernelapi.TextMemory) {
	if m == nil {
		return
	}
	st := m.st
	if st == nil {
		return
	}
	m.graph = mem
	if indexer != nil {
		st.indexer = indexer
	}
	if tracker != nil {
		st.tracker = tracker
	}
	if docs != nil {
		st.coldcontextStore = docs
	}
	if social != nil {
		st.social = social
	}
	if text != nil {
		st.textMem = text
	}
	st.injected = true
}
