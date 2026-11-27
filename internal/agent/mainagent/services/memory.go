package services

import (
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/memory"
	"github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/memory/distill"
	"github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/memory/pipeline"
	"github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/memory/social"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/pkg/generation"
)

func (s *Services) initMemory(d Deps) {
	// ★ 读取前检查（用户指定的做法："结构体成员初始置 nil 加读取前检查"）。
	//
	// 为何要检查 CfgReg：它是 **Group 0** 的产物（不由任何调度模块提供），
	// 所以不可能通过 Requires 声明它 —— 依赖图保证不了它非 nil。
	// 而上面那行 `d.Cfg.GetString(...)` 在 nil 时会**panic**，
	// 栈信息指向 strings.Split 之类无关处，排查成本高。
	if d.Cfg == nil {
		log.Printf("[main] memory: CfgReg 为 nil（Group 0 必须先建好配置中心）")
		return
	}
	if d.DataDir == "" {
		log.Printf("[main] memory: DataDir 为空（Group 0 必须先设置）")
		return
	}
	memDB, err := memory.NewGraphDB(filepath.Join(d.DataDir, "memory", "graph.db"))
	if err != nil {
		log.Printf("[homed] warning: memory init failed: %v", err)
		memDB = nil
	} else {
		log.Printf("[homed] graph memory initialized")
	}
	memIdx := memory.NewIndexer(memDB)
	memIdx.Sync() // 启动时立即同步，避免前 30 分钟空窗
	socialStore := social.New(memDB)

	distiller := pipeline.NewDistiller(memDB, d.DataDir, pipeline.DistillerConfig{
		Interval:      10 * time.Minute,
		RetentionDays: 7,
		BatchSize:     50,
	})
	// ★ 必须在 distiller.Start() **之前**注入：Start 会起 distillLoop，
	// 而第一次 tick 在 Interval 之后。若先 Start 再注入，头一个周期
	// （实测配置 10 分钟）会白跑一次 jieba 路 —— 而那条路实测产出 0 条。
	if memDB != nil {
		distiller.SetSplitter(initDistillSplitter(d.Cfg))
	}
	if memDB != nil {
		// 这里**故意不写 defer distiller.Stop()**：函数 return 即触发 defer，
		// 而 Stop() → cancel() 会让刚启动的 distillLoop 立刻退出，
		// 规则蒸馏管线启动即死、10min 心跳从不运行（旧 main() 拆分时的残留）。
		// 停机由 AddCloser 负责（见下方）。
		distiller.Start()
	}

	// ★ 四个产物各自挂到**具名设施**上（不再打包成 kstate.MemoryBundle）：
	// 「记忆子系统」的打包类型一旦住进挂载点，挂载点就持有模块知识
	// （用户裁定 2026-11-17）。拆开之后"只填了 db 忘了 indexer"不再可能：
	// 每个名字要么装着、要么缺失（Require 返回 ok=false，消费方响亮报错）。
	if memDB != nil {
		s.Memory = memDB
	}
	s.Indexer = memIdx
	s.Social = socialStore
	s.Distiller = distiller
	// ★ 共享词嵌入**不再由本包构造**（2026-11-20）。
	//
	// 它是内核设施（`FacEmbedder`，由 `kernelmod/multimodal` 提供、全进程一份），
	// 装配点把它挂到基座上；本类型只经基座 syscall 用（docs §3.4）。
	//
	// 此前这里自己 `NewStaticEmbedder` ⇒ 同一个 fastText 模型被加载**两次**
	// （实测日志里两组 "loaded 50000 words, dim=300"，各占数百 MB）。
	// 蒸馏器的语义验证需要向量化能力，所以把基座的句柄交给它 ——
	// 但**不在这里 new**。s.Embedder 置 nil：本类型不再持有实现对象。
	//
	// 蒸馏器的语义验证在装配完成后由 installState 接线（`SetEmbedder` 传
	// 基座 syscall 的适配器）。这里不能做 —— 那一刻基座还没挂载语义设施。

	// ★ 伪关机登记（迁移数据目录用，见 kernelboot/lifecycle.go）。
	// 只登记**持有写句柄**的 distiller —— 它每 10min 写 memory/graph.db，
	// 迁移期间一旦写入就会搬走"写了一半的库"。
	s.quiesce("distiller",
		func() error { return distiller.Pause() },
		func() error { return distiller.Resume() })

	s.Closers = append(s.Closers, func() {
		// 与原本两个 defer 同序（LIFO）：先停蒸馏器，再关图库。
		if memDB != nil {
			distiller.Stop()
			memDB.Close()
		}
	})
}

func initDistillSplitter(cfgReg kernelapi.ConfigStore) pipeline.RecordSplitter {
	if cfgReg == nil {
		return nil
	}
	providerName := cfgReg.GetString("core.memory.distill.generation.provider", "")
	if providerName == "" {
		return nil
	}
	opts := map[string]string{}
	const optPrefix = "core.memory.distill.generation.options."
	if lister, ok := cfgReg.(interface{ List(prefix string) []string }); ok {
		for _, key := range lister.List(optPrefix) {
			opts[strings.TrimPrefix(key, optPrefix)] = cfgReg.GetString(key, "")
		}
	}
	provider, err := generation.Open(providerName, generation.Config{Options: opts})
	if err != nil {
		log.Printf("[homed] warning: opening distillation provider %q failed: %v (distillation falls back to jieba; registered: %s)",
			providerName, err, strings.Join(generation.Names(), ", "))
		return nil
	}
	if !provider.Info().SupportsJSONSchema {
		// 拆分器的零幻觉完全依赖 schema 约束。不支持 schema 的 provider
		// 会拿到自由文本，解析必炸 —— 与其让它运行时反复失败，不如现在就拒。
		log.Printf("[homed] warning: distillation provider %q does not support JSON Schema, splitting is unreliable (falling back to jieba)",
			providerName)
		provider.Close()
		return nil
	}
	log.Printf("[homed] distill generation active: provider=%s model=%s",
		providerName, provider.Info().Model)
	return distill.NewExtractor(provider, nil)
}
