// Package modules 是内核模块的**登记表**（不含任何具体模块的实现）。
//
// ## 设计意图（用户设计）
//
// > 每个内核模块显式各自声明依赖 … 最先初始化的 core 自然不依赖任何模块，
// > 下一步初始化的依赖 core 以此类推
//
// ## 怎么加一个新模块（这是本设计的**验收标准**）
//
//	① 新建一个文件（如 modules/mymod.go）
//	② 写一个类型，实现 kernelboot.Module（Name/Requires/Provides/Init）
//	③ 在同文件的 init() 里调 modules.Register(&MyMod{})
//	④ 在 `internal/kernelmod/all/all.go` 加一行空白 import —— **仅此而已**
//
// **不需要**编辑任何中央清单：依赖写在模块**自己**的 Requires() 里。
//
// ★ 为何要"防漏 import"的判据（见 `internal/kernelboot/modules/registry_test.go`）：
// 忘了 import ⇒ 模块**根本不被编译进来** ⇒ 它不参与装配 ⇒ 依赖它的模块报
// "缺依赖"（响亮），但**不依赖它的模块会静默少了那个设施**
// （如插件注册表没建 ⇒ 插件全部加载不了，而错误信息指向插件而非根因）。
//
// 判据有两条（缺一不可）：
//
//	· `TestEveryRegisteredModuleIsCompiledIn` —— 用 AST 扫源码树里
//	  「目录+类型名 复合键 + Name/Requires/Provides/Init 四方法齐全」的类型，
//	  取其 Name() 与登记表做集合比对（独立真相源）。
//	  ★ 刻意**不**扫 `kernelmod.Register(...)` 调用点：删掉某模块的 init() 后
//	  调用点消失，源码侧与登记表侧会同时少一个 ⇒ 比对照样相等 ⇒ 假绿。
//	  手写模块名清单同理（清单会跟着漏），两者都不用。
//	· `TestRealRegistryAssemblesWholeMachine` 等 —— 用真实登记表装配整机、
//	  断言每个模块都进图且设施就绪、并严格逆序关闭（见同目录测试文件）
package modules

import (
	"fmt"
	"sort"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
)

var (
	regMu    sync.Mutex
	registry []kernelboot.Module
	seenName = map[string]bool{}
)

// Register 由模块在自己的 init() 里调用（自注册）。
//
// 重复注册同一模块名会**记录**并在 BuildModules 时报错 —— 不用 panic：
// panic 发生在 init 阶段，栈信息对使用者毫无意义（他只想知道哪个模块重名）。
func Register(m kernelboot.Module) {
	if m == nil {
		return
	}
	regMu.Lock()
	defer regMu.Unlock()
	if seenName[m.Name()] {
		dupErr = append(dupErr, m.Name())
		return
	}
	seenName[m.Name()] = true
	registry = append(registry, m)
}

// dupErr 累积重名模块（BuildModules 时统一报告）。
var dupErr []string

// BuildModules 返回全部已登记模块（按名字排序，保证可复现）。
//
// ## 为什么排序
//
// 分层结果的**层内顺序**若依赖 map/切片顺序，同一份声明两次构建会得到
// 不同顺序 ⇒ 不可审计（设计 §2.5：图要能进 PR diff 比对）。
// 排序让结果稳定。
func BuildModules() ([]kernelboot.Module, error) {
	regMu.Lock()
	defer regMu.Unlock()
	if len(dupErr) > 0 {
		names := append([]string(nil), dupErr...)
		sort.Strings(names)
		return nil, fmt.Errorf("kernelboot/modules: 模块名重复注册: %v"+
			"（同一个模块被 import 了两次？或两个模块取了同名）", names)
	}
	out := append([]kernelboot.Module(nil), registry...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	if len(out) == 0 {
		return nil, fmt.Errorf("kernelboot/modules: 没有任何模块被登记 —— " +
			"通常是没有 import 模块包（见本文件顶部的'怎么加一个新模块'）")
	}
	return out, nil
}

// Names 返回已登记模块名（诊断/判据用）。
func Names() []string {
	regMu.Lock()
	defer regMu.Unlock()
	out := make([]string, 0, len(registry))
	for _, m := range registry {
		out = append(out, m.Name())
	}
	sort.Strings(out)
	return out
}
