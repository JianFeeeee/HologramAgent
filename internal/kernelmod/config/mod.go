// Package config 是 config 模块：提供配置快照（ks.Cfg）。
//
// ## ★ 它解决了"cfg 是无主模块"的问题
//
// 原先 `cfg := cfgReg.ToConfig()` 之后，cfg 是**游离的局部变量**，
// 被手工传给 11 个 init* —— 谁需要它就靠人肉排在那一行之后。
// 现在它住在 `ks.Cfg`，任何模块直接读即可。
//
// ## 依赖：图上为空，但隐含"必须在 Group 0 之后"
//
// 它读 core.llm.* 里的**密文** ⇒ 必须在安全块（装好密钥）之后。
// 但这不是**图内依赖**（安全块不由任何调度模块提供；声明 crypto 会让
// Build 报"依赖了不存在的设施"）。更本质：那是**时序**要求，不是**数据**依赖。
//
// ⇒ 由 Group 0 阶段保证：Group 0 串行跑完 ⇒ 才启动本调度器。
package config

import (
	"fmt"

	"github.com/JianFeeeee/HomeAgent/internal/kernelboot"
	"github.com/JianFeeeee/HomeAgent/internal/kernelmod"
)

// Module 提供配置快照。
type Module struct{}

func (Module) Name() string       { return "config" }
func (Module) Requires() []string { return nil } // 前提由 Group 0 保证
func (Module) Provides() []string { return []string{kernelmod.FacConfig} }

func (Module) Init(ks *kernelboot.KernelState) error {
	// ★ 检查而不是假设（用户说的"nil 加读取前检查"）：
	// ks.Cfg 为 nil 说明 Group 0 段没跑或顺序错了。响亮报错远好于让后续
	// 模块拿到 nil —— 那会在很远的地方变成"模型没有配置"之类的模糊症状。
	if ks.Cfg == nil {
		return fmt.Errorf("config 模块：ks.Cfg 为 nil —— Group 0 必须先完成 " +
			"`cfg := cfgReg.ToConfig()`（且必须在安全块之后，否则密文解不开 ⇒ 全 provider 401）")
	}
	return nil
}

func init() { kernelmod.Register(Module{}) }
