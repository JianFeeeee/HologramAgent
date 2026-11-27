package ctx

// aliases.go —— 本包内部的短名（声明点全在**本包**的 values.go）。
//
// ★ 不再指向 `agenttypes`：上下文值类型归本包自己（docs §0.3 铁律）。

import "github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"

type (
	MemoryBlock = model.MemoryBlock
)

// Embedder 是词嵌入面（与基座的同名接口**结构相同**，可互换赋值）。
//
// ★ 为什么不统一成同一个具名类型：agent 包 import 本包，本包不能
//
//	反向 import agent（成环）。让两边的接口**结构一致**即可互通 ——
//	Go 的接口是结构化类型，赋值时自动满足。
//
// Embedder 是**语义计算面**（docs §3.4）：批量句进 → 向量出。
//
// ★ 只有批量一个方法：内核侧退化成无状态 call-reply 之后，一次 call 传 N 句
// 就是形状本身（不是优化选项）。单发场景写 len-1 的调用。开关断在
// "谁是计算者"，不在"一次几句"。实现由类型把基座 syscall 包一层提供。
type Embedder interface {
	EmbedTexts(texts []string) []map[string]float64
	Loaded() bool
	// DenseSimilarity 两个稠密向量的余弦。
	//
	// ★ 为什么在接口上而不是包级函数：度量口径由**基座**决定（换度量时
	// 类型不该跟着改）。实现是类型侧的薄适配器，转发基座 syscall。
	DenseSimilarity(a, b []float64) float64
}
