// Package all 把全部内核模块**引入编译**（Go 要求 import 才会编译进来）。
//
// ## 为什么这层是必要的（一个真实的 Go 约束）
//
// 模块各自在 init() 里 `kernelmod.Register(...)`，但必须有东西把它们
// 引进来。若把 import 写在 `kernelmod/core.go` 里：
//
//	core.go import lua/  ⇒ core → lua → core（lua 要调 Register 必须 import core）
//	⇒ **循环导入**，Go 禁止
//
// 所以注册表 API 在 `kernelmod` 包，模块 import 它（单向），
// 而"引进来"的清单放在**本子包**（`kernelmod/all`）—— 它 import 两者。
//
// ## 加一个新模块要做什么（**唯一的一处编辑**）
//
//	① 新建 internal/kernelmod/<name>/mod.go，实现 kernelboot.Module
//	② 在**本文件**加一行空白 import
//
// 依赖写在模块自己的 Requires() 里 —— **没有**中央依赖清单。
package all

import (
	// 每个模块一个独立文件夹（用户设计）。
	//
	// ★ 顺序**无关**：模块注册顺序由 init() 决定，而注册表刻意乱序
	// （见 kernelmod/core.go）。真正的先后只由 Requires/Provides 决定。
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/agent"
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/config"
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/docstore"
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/kernelapi"
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/knowledge"
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/llm"
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/lua"
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/memory"
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/migration"
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/multimodal"
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/onnx"
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/pluginhost"
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/stagehost"
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/supervisor"
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/textmem"
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/tracker"
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/update"
)
