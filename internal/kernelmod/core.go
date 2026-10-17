// Package kernelmod 是内核模块的**注册表**（用户设计的 core）。
//
// ## 用户的设计意图（原话）
//
// > 模块应该在 /modelname/ 独立文件夹下，根目录只有 core.go，
// > core 内有一个注册表注册各个模块，各个模块通过该注册表得知内核有哪些模块，
// > 从而声明对其他模块的依赖，注意注册表中的模块列表是乱序的，
// > 只是为了向模块展示内核注册了哪些模块
//
// ## 目录形状
//
//	internal/kernelmod/
//	├── core.go            ← 本文件：注册表（+ 唯一的 import 清单）
//	├── facilities.go      ← 设施名常量（模块间的协议）
//	├── config/mod.go      ← 每个模块一个独立文件夹
//	├── kernelapi/mod.go
//	├── lua/mod.go
//	├── llm/mod.go
//	├── memory/mod.go
//	├── supervisor/mod.go
//	├── tracker/mod.go
//	├── textmem/mod.go
//	├── docstore/mod.go
//	├── knowledge/{migration,store}.go
//	├── multimodal/mod.go
//	├── onnx/mod.go
//	├── stagehost/mod.go
//	└── agent/mod.go
//
// ## 职责边界（刻意做窄）
//
// 本文件**只做两件事**：
//
//	· `Register(m)` —— 模块在 init() 里自注册
//	· `Registered()` —— 返回已注册模块（**乱序**，仅供查询"内核有哪些模块"）
//
// 它**不做**依赖解析/分层/执行 —— 那些是 `internal/kernelboot` 的事。
// 分开的理由：注册表是"名册"，分层器是"调度器"。混在一起会让
// "只想知道有哪些模块"的人被迫拉进整个调度逻辑。
//
// ## ★ 为什么列表是"乱序"，以及这为什么是对的
//
// 注册顺序取决于 **Go 的 init() 执行顺序**（由 import 图决定，
// 规范未定义跨包的**具体**顺序）。若把注册顺序当成"可信的启动顺序"，
// 那就是**隐式顺序依赖** —— 正是本设计要消灭的东西（§1 的病 A）。
//
// 所以 `Registered()` **刻意不排序**，并明确"顺序无意义"：
//
//	· 模块只能用它回答"内核注册了哪些模块"
//	· **不能**用它推断先后 —— 先后只由 Requires/Provides 决定
//
// ★ 分层器（kernelboot.Build）内部会按名字排序以保证**结果可复现**，
// 那是分层器的实现细节，与"注册表乱序"不矛盾。
//
// ## ★ 关于下面那份 import 清单（为什么必须有，且它是**唯一**的一处）
//
// Go 要求"包被 import 才会编译进来"。模块各自在 init() 里注册，
// 但**必须有东西把它们引进来** —— 这就是下面那些空白导入。
//
// 为什么不能省（也解释了一个真实的 Go 约束）：
//
//	core.go 想 import lua/  ⇒ core → lua → core（lua 要调 Register）
//	⇒ **循环导入**，Go 禁止
//
// 解法：注册表 API 放在**本包**、模块 import 本包（单向），
// 而"把它们引进来"的清单只能放在**本包之外** —— 见
// `internal/kernelmod/all/all.go`（只做 import，无逻辑）。
//
// ⇒ 加模块 = **加一个文件夹 + 在 all.go 加一行**（唯一的一处编辑）。
package kernelmod

import (
	"sync"

	"github.com/JianFeeeee/HomeAgent/internal/kernelboot"
)

var (
	mu   sync.Mutex
	list []kernelboot.Module
	seen = map[string]bool{}
	dups []string
)

// Register 由模块在自己的 init() 里调用。
//
// 重复注册同名模块会被**记录**（不 panic）：panic 发生在 init 阶段，
// 栈信息对使用者毫无意义（他只想知道哪个模块重名）。由 Registered 报出。
func Register(m kernelboot.Module) {
	if m == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	name := m.Name()
	if seen[name] {
		dups = append(dups, name)
		return
	}
	seen[name] = true
	list = append(list, m)
}

// Registered 返回全部已注册模块。
//
// ★ **顺序无意义**（见包注释）：它取决于 init() 执行顺序。
// 只用它回答"内核注册了哪些模块"，**不要**用它推断先后。
func Registered() []kernelboot.Module {
	mu.Lock()
	defer mu.Unlock()
	out := make([]kernelboot.Module, len(list))
	copy(out, list)
	return out
}

// Duplicates 返回重复注册的模块名（诊断用）。
func Duplicates() []string {
	mu.Lock()
	defer mu.Unlock()
	out := make([]string, len(dups))
	copy(out, dups)
	return out
}

// Names 返回已注册模块名（诊断用；顺序同 Registered，无意义）。
func Names() []string {
	mu.Lock()
	defer mu.Unlock()
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.Name())
	}
	return out
}

// IsRegistered 报告某模块是否已注册（模块可用它判断依赖是否存在）。
func IsRegistered(name string) bool {
	mu.Lock()
	defer mu.Unlock()
	return seen[name]
}
