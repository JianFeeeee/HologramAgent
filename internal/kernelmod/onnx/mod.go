// Package onnx 是 onnx-parser 模块：依存句法分析器（失败退回规则引擎）。
//
// ## ★ 它为什么是独立模块
//
// 它写的是**包级单例** `nlp.defaultParser`（internal/nlp/parser.go:15），
// 而该单例由**运行期**读取（`nlp.NewExtractor(nil)` 构造时快照它，
// 由蒸馏路径 `extractKeyTriples` 调用）。
//
// 这正是设计 §2.8.2 说的"签名看不到的隐藏依赖"：没有参数传递，
// 而是**包级变量 + 运行期读**。
//
// ⇒ 独立成模块，让 text-memory 能**显式声明**对它的依赖
// （见 kernelmod/textmem 的说明）。这样"碰巧安全"变成"结构保证"。
package onnx

import (
	"log"
	"path/filepath"

	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	"github.com/JianFeeeee/HologramAgent/internal/nlp"
)

// Module 初始化依存句法分析器。
type Module struct{}

func (Module) Name() string       { return "onnx-parser" }
func (Module) Requires() []string { return []string{kernelmod.FacConfig} }
func (Module) Provides() []string { return []string{kernelmod.FacONNX} }

func (Module) Init(ks *kernelboot.KernelState) error {
	modelPath := ks.CfgReg.GetString("core.agent.onnx_model_path", "")
	onnxParser, err := nlp.NewONNXParser(nlp.ONNXConfig{
		ModelPath: modelPath,
		DataDir:   filepath.Join(ks.Cfg.Daemon.DataDir, "nlp"),
	})
	if err != nil {
		log.Printf("[homed] warn: ONNX parser init: %v, using fallback", err)
		return nil
	}
	nlp.SetDefaultParser(onnxParser)
	// 日志必须报**实际解析出的**模型路径，不能回显配置值：
	// core.agent.onnx_model_path 留空时内核会用**二进制内嵌**的 dep_parser.onnx
	// （解到 <data>/nlp 后加载）。此前只打配置值，于是出现
	// "initialized (model: )" —— 看着像 ONNX 没启用，实际内嵌模型一直在跑。
	log.Printf("[homed] dep parser ready (ONNX，模型: %s)", onnxParser.ModelPath())
	return nil
}

func init() { kernelmod.Register(Module{}) }
