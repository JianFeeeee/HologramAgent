package kernelboot

// 把既有的内核 `init*` 函数**适配**成 Module，而不必重写它们的签名。
//
// ## 为什么不直接改 init* 的签名
//
// `init*` 函数是**既有代码**，各有不同签名（有的返回 1 个值、有的 2 个、
// 有的带 close 函数、有的只做副作用）。逐个改签名意味着：
//   · 大面积改动（评审困难、易引入回归）
//   · 且 `init*` 函数本身是清晰的（`initLuaVM(cfg) (*VM, func())`）——
//     为迁就调度器的接口而把它改丑，是**倒置的取舍**
//
// 所以本文件提供**适配层**：声明式地描述"这个模块的名字/依赖/产物/怎么调用"，
// `init*` 函数一行都不改。
//
// ## 设计：FuncModule
//
// ```go
// kernelboot.FuncModule(kernelboot.FuncSpec{
//     Name:     "lua",
//     Requires: []string{"config"},
//     Provides: []string{"lua"},
//     Init: func(ks *kernelboot.KernelState) error {
//         vm, closeFn := initLuaVM(ks.Cfg)   // 依赖的产物：直接读字段
//         ks.LuaVM = vm                       // 自己的产物：写回字段
//         ks.AddCloser(closeFn)               // 关闭函数统一登记
//         return nil
//     },
// })
// ```
//
// ## 为什么"关闭函数"也要作为产物发布
//
// `init*` 常返回 `(value, func())`：value 要用，close 要在停机时调。
// 若适配器只发布 value，**close 就丢了** ⇒ 资源泄漏（实测有先例：
// tracker 的 overlay 挂载、memory stack 的关闭）。
//
// 把 close 也当产物（约定名 `<facility>.close`）的好处：
// 停机时统一遍历容器调用所有 close，**不依赖人记得在 main 末尾手写**。
// 这正好顺手解决了 main() 里那一堆 `defer closeMem()` 的散落问题。

// FuncSpec 描述一个由函数适配的模块。
type FuncSpec struct {
	Name     string
	Requires []string
	Provides []string
	// Init 执行装配：从 ctx 取依赖、调既有函数、把产物 Put 回 ctx。
	Init func(ks *KernelState) error
}

// funcModule 是 FuncSpec 的 Module 实现。
type funcModule struct {
	spec FuncSpec
}

func (m *funcModule) Name() string       { return m.spec.Name }
func (m *funcModule) Provides() []string { return m.spec.Provides }
func (m *funcModule) Requires() []string { return m.spec.Requires }
func (m *funcModule) Init(ks *KernelState) error {
	if m.spec.Init == nil {
		return nil
	}
	return m.spec.Init(ks)
}

// FuncModule 创建一个由函数适配的模块。
func FuncModule(spec FuncSpec) Module {
	return &funcModule{spec: spec}
}

// --- 关闭函数的统一回收 ---
//
// 约定：模块在 Init 里用 ks.AddCloser(fn) 注册关闭函数。
// 停机时由 KernelState.CloseAll 按**注册顺序的逆序**执行（LIFO，与 defer 一致）。
//
// ★ 为何不再用"设施名以 .close 结尾"的约定：那个约定要靠字符串判断，
// 而结构体方案下直接 AddCloser 更直白（且不会因为设施改名而失效）。
