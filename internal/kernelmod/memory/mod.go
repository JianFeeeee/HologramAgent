// Package memory 是 memory 模块：图记忆 + 索引 + 社交图 + 蒸馏管线 + 共享词嵌入。
//
// ## 为什么依赖 kernel-io
//
// 本模块产出 MemoryBundle（4 个同生共死的东西打包）。它**不**直接读 IO，
// 但"记忆子系统就绪"在语义上包含它后续要被候选消费管线使用，而那条管线需要 iom。
//
// ★ 这是**刻意声明**的依赖（不是签名要求的）：让"记忆子系统在 IO 就绪后才建"
// 成为结构保证 —— 若将来 initMemoryStack 内部要用 iom，不会因为"忘了声明"
// 而静默拿 nil。
package memory

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/JianFeeeee/HomeAgent/internal/config"
	"github.com/JianFeeeee/HomeAgent/internal/kernelboot"
	"github.com/JianFeeeee/HomeAgent/internal/kernelmod"
	"github.com/JianFeeeee/HomeAgent/internal/memory"
	"github.com/JianFeeeee/HomeAgent/internal/memory/distill"
	"github.com/JianFeeeee/HomeAgent/internal/memory/pipeline"
	"github.com/JianFeeeee/HomeAgent/internal/memory/social"
	"github.com/JianFeeeee/HomeAgent/pkg/generation"
)

// Module 提供图记忆子系统。
type Module struct{}

func (Module) Name() string       { return "memory" }
func (Module) Requires() []string { return []string{kernelmod.FacKernelIO} }
func (Module) Provides() []string { return []string{kernelmod.FacMemory} }

func (Module) Init(ks *kernelboot.KernelState) error {
	// ★ 读取前检查（用户指定的做法："结构体成员初始置 nil 加读取前检查"）。
	//
	// 为何要检查 CfgReg：它是 **Group 0** 的产物（不由任何调度模块提供），
	// 所以不可能通过 Requires 声明它 —— 依赖图保证不了它非 nil。
	// 而上面那行 `ks.CfgReg.GetString(...)` 在 nil 时会**panic**，
	// 栈信息指向 strings.Split 之类无关处，排查成本高。
	if ks.CfgReg == nil {
		return fmt.Errorf("memory 模块：ks.CfgReg 为 nil —— " +
			"Group 0 必须先建好配置中心（NewConfigRegistry）再启动本调度器")
	}
	if ks.DataDir == "" {
		return fmt.Errorf("memory 模块：ks.DataDir 为空 —— 它是 Group 0 的产物，必须先设置")
	}
	memDB, err := memory.NewGraphDB(filepath.Join(ks.DataDir, "memory", "graph.db"))
	if err != nil {
		log.Printf("[homed] warning: memory init failed: %v", err)
		memDB = nil
	} else {
		log.Printf("[homed] graph memory initialized")
	}
	memIdx := memory.NewIndexer(memDB)
	memIdx.Sync() // 启动时立即同步，避免前 30 分钟空窗
	socialStore := social.New(memDB)

	distiller := pipeline.NewDistiller(memDB, ks.DataDir, pipeline.DistillerConfig{
		Interval:      10 * time.Minute,
		RetentionDays: 7,
		BatchSize:     50,
	})
	// ★ 必须在 distiller.Start() **之前**注入：Start 会起 distillLoop，
	// 而第一次 tick 在 Interval 之后。若先 Start 再注入，头一个周期
	// （实测配置 10 分钟）会白跑一次 jieba 路 —— 而那条路实测产出 0 条。
	if memDB != nil {
		distiller.SetSplitter(initDistillSplitter(ks.CfgReg))
	}
	if memDB != nil {
		// 这里**故意不写 defer distiller.Stop()**：函数 return 即触发 defer，
		// 而 Stop() → cancel() 会让刚启动的 distillLoop 立刻退出，
		// 规则蒸馏管线启动即死、10min 心跳从不运行（旧 main() 拆分时的残留）。
		// 停机由 AddCloser 负责（见下方）。
		distiller.Start()
	}

	ks.Memory = &kernelboot.MemoryBundle{
		DB:        memDB,
		Indexer:   memIdx,
		Social:    socialStore,
		Distiller: distiller,
	}
	// 共享词嵌入：蒸馏与 Agent 上下文复用同一实例
	// （避免同一模型被二次加载，约 200k×300 维 ≈ 数百 MB）。
	ks.Embedder = memory.NewStaticEmbedder(
		strings.Split(ks.CfgReg.GetString("core.agent.embedding_model_path", ""), ",")...)
	distiller.SetEmbedder(ks.Embedder)

	// ★ 伪关机登记（迁移数据目录用，见 kernelboot/lifecycle.go）。
	// 只登记**持有写句柄**的 distiller —— 它每 10min 写 memory/graph.db，
	// 迁移期间一旦写入就会搬走"写了一半的库"。
	if ks.Memory != nil && ks.Memory.Distiller != nil {
		ks.AddQuiescer("distiller",
			func() error { return ks.Memory.Distiller.Pause() },
			func() error { return ks.Memory.Distiller.Resume() })
	}

	ks.AddCloser(func() {
		// 与原 main 的两个 defer 同序（LIFO）：先停蒸馏器，再关图库。
		if memDB != nil {
			distiller.Stop()
			memDB.Close()
		}
	})
	return nil
}

// initDistillSplitter 按配置打开蒸馏用的生成侧 provider 并组装拆分器。
//
// 返回 nil 表示不启用（未配置 provider、或打开失败）—— 此时蒸馏回退既有的
// jieba/ONNX 路。**不因为模型不可用而让蒸馏停摆**：那条路虽然实测产出 0 条，
// 但它至少不会让对话记忆丢失。
func initDistillSplitter(cfgReg *config.ConfigRegistry) pipeline.RecordSplitter {
	if cfgReg == nil {
		return nil
	}
	providerName := cfgReg.GetString("core.memory.distill.generation.provider", "")
	if providerName == "" {
		return nil
	}
	opts := map[string]string{}
	const optPrefix = "core.memory.distill.generation.options."
	for _, key := range cfgReg.List(optPrefix) {
		opts[strings.TrimPrefix(key, optPrefix)] = cfgReg.GetString(key, "")
	}
	provider, err := generation.Open(providerName, generation.Config{Options: opts})
	if err != nil {
		log.Printf("[homed] warning: 蒸馏生成 provider %q 打开失败: %v（蒸馏回退 jieba 路；已注册: %s）",
			providerName, err, strings.Join(generation.Names(), ", "))
		return nil
	}
	if !provider.Info().SupportsJSONSchema {
		// 拆分器的零幻觉完全依赖 schema 约束。不支持 schema 的 provider
		// 会拿到自由文本，解析必炸 —— 与其让它运行时反复失败，不如现在就拒。
		log.Printf("[homed] warning: 蒸馏生成 provider %q 不支持 JSON Schema，拆分不可靠（蒸馏回退 jieba 路）",
			providerName)
		provider.Close()
		return nil
	}
	log.Printf("[homed] distill generation active: provider=%s model=%s",
		providerName, provider.Info().Model)
	return distill.NewExtractor(provider, nil)
}

func init() { kernelmod.Register(Module{}) }
