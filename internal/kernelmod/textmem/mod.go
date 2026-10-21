// Package textmem 是 text-memory 模块：文本记忆。
//
// ## ★ 显式依赖 onnx-parser —— 一条**签名看不到**的依赖
//
// 候选消费管线会启动蒸馏循环，而蒸馏路径在**运行期**读
// `nlp.defaultParser`（包级单例，由 onnx 模块写）：
//
//	distiller.Start() → go distillLoop() → distillOnce()
//	  → extractKeyTriples() → nlp.NewExtractor(nil)
//	    → 构造时快照 defaultParser（parser.go:60）
//
// 原 main 的顺序是**反的**（蒸馏在 226 行启动、解析器 289 行才装上），
// 目前**碰巧安全** —— `distillLoop` 用 ticker，首个 tick 要等
// `Interval = 10 分钟`，那时解析器早装好了。
//
// ★ 但"碰巧安全"不是保证：Interval 变小或解析器装配更后 ⇒ **静默降级**
// （defaultParser 为 nil ⇒ POS 模板抽不出 ⇒ 图记忆实体退化成整句复合值
// ⇒ 跨维度检索全失效，基线 0/5。该后果由 pipeline.go:60 注释记录。）
//
// ⇒ 声明出来就是**结构保证**：两者不同层，分层器保证先后。
package textmem

import (
	"log"
	"path/filepath"

	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	"github.com/JianFeeeee/HologramAgent/internal/memory/text"
)

// Module 提供文本记忆。
type Module struct{}

func (Module) Name() string { return "text-memory" }
func (Module) Requires() []string {
	// onnx-parser：见包注释（隐藏依赖，显式声明）。
	return []string{kernelmod.FacConfig, kernelmod.FacMemory,
		kernelmod.FacKernelIO, kernelmod.FacONNX}
}
func (Module) Provides() []string { return []string{kernelmod.FacTextMem} }

func (Module) Init(ks *kernelboot.KernelState) error {
	textMem := text.New(filepath.Join(ks.Cfg.Daemon.DataDir, "memory", "text"))
	if err := textMem.Start(); err != nil {
		log.Printf("[homed] warning: text memory start: %v", err)
		ks.TextMemory = textMem
		return nil
	}
	log.Printf("[homed] text memory active at %s", filepath.Join(ks.Cfg.Daemon.DataDir, "memory", "text"))
	ks.TextMemory = textMem
	// ★ 伪关机登记（迁移数据目录用）：text memory 的 rotationLoop 会周期性写盘。
	ks.AddQuiescer("textmem",
		func() error { return textMem.Pause() },
		func() error { return textMem.Resume() })

	ks.AddCloser(textMem.Stop)
	return nil
}

func init() { kernelmod.Register(Module{}) }
