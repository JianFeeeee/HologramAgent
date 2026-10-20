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

	"github.com/JianFeeeee/HomeAgent/internal/kernelboot"
	"github.com/JianFeeeee/HomeAgent/internal/kernelmod"
	"github.com/JianFeeeee/HomeAgent/internal/nlp"
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
	// 空路径 = 走内置规则引擎（不发 ONNX 推理）。旧日志打成
	// "initialized (model: )" —— 空括号看着像配置泄漏了，实为正常回退。
	if modelPath == "" {
		log.Printf("[homed] dep parser ready (内置规则引擎；未配置 core.agent.onnx_model_path)")
	} else {
		log.Printf("[homed] dep parser ready (ONNX model: %s)", modelPath)
	}
	return nil
}

func init() { kernelmod.Register(Module{}) }
