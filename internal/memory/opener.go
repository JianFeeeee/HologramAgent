package memory

// opener.go —— 图记忆的**装配面**（供类型模块在装配点开子视图）。
//
// ## 为什么需要它（2026-11-17）
//
// 驻留子与 rtchild 要装配自己的图记忆面：
//
//	主库受限只读句柄  OpenGraphDBReadOnly(main.Path())
//	temp 实例（可选）  NewLightMemory(ro, tempPath, allowWrite)
//
// 这两个都是**实现构造**。agent 直接调它们 = agent import 实现包，
// 而"类型模块不 import 实现"是本轮拆包的硬指标。所以由本包把这两个构造
// 挂到设施表（`kernelapi.FacGraphOpener`），类型模块经 `Require` 取。
//
// 这不只是"换个地方调"：它把"**谁来构造**"从基座移到装配侧。
// 基座只知道"我有一个 GraphMemory 面"，不知道它是 LightMemory 还是 GraphDB。

import (
	"fmt"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"

	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
)

// GraphOpener 实现 kernelapi.GraphMemoryOpener。
type GraphOpener struct{}

// NewGraphOpener 构造装配面（memory 模块在 Init 里 Provide 一次）。
func NewGraphOpener() *GraphOpener { return &GraphOpener{} }

// OpenReadOnly 开出主库的**结构上写不进**的只读视图。
func (GraphOpener) OpenReadOnly(mainPath string) (kernelapi.GraphMemory, error) {
	if mainPath == "" {
		return nil, fmt.Errorf("memory: main graph path is empty")
	}
	ro, err := OpenGraphDBReadOnly(mainPath)
	if err != nil {
		return nil, err
	}
	return ro, nil
}

// OpenLight 用只读主库 + temp 路径构造轻量实例（读 temp∪main、只写 temp）。
func (GraphOpener) OpenLight(mainRO kernelapi.GraphMemory, tempPath string, allowWrite bool) (any, error) {
	db, ok := mainRO.(*GraphDB)
	if !ok || db == nil {
		// 不是本包的只读句柄 ⇒ 装配写错了（调用方应传 OpenReadOnly 的返回值）。
		return nil, fmt.Errorf("memory: OpenLight needs a read-only handle opened by this module (got %T)", mainRO)
	}
	return NewLightMemory(db, tempPath, allowWrite)
}

// Close 关闭由本接口打开的句柄（幂等）。
func (GraphOpener) Close(m any) error {
	if m == nil {
		return nil
	}
	if c, ok := m.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// ExportTriples 导出某库里的全部活跃三元组（`*GraphDB.ExportTriples`）。
func (GraphOpener) ExportTriples(db any, since int) ([]model.Triple, error) {
	g, ok := db.(*GraphDB)
	if !ok || g == nil {
		return nil, nil
	}
	return g.ExportTriples(since)
}

// LightCommon 取轻量实例的**共同面**（满足 kernelapi.GraphMemory 的那一面）。
//
// ★ 用 `*LightMemory` 自己的 GraphMemory 实现：它内嵌了读 temp∪main 的
//
//	`graphFace`（只实现 Recall/Commit 那一组方法）。
func (GraphOpener) LightCommon(light any) kernelapi.GraphMemory {
	lm, ok := light.(*LightMemory)
	if !ok || lm == nil {
		return nil
	}
	if gf, ok := any(lm).(kernelapi.GraphMemory); ok {
		return gf
	}
	// 方法集不满足契约 ⇒ 装配错误（响亮返回 nil，由调用方报错）。
	return nil
}

// LightTemp 取轻量实例的 temp 库句柄（`*LightMemory.Temp()`）。
func (GraphOpener) LightTemp(light any) kernelapi.GraphMemory {
	lm, ok := light.(*LightMemory)
	if !ok || lm == nil {
		return nil
	}
	return lm.Temp()
}

// 编译期确认。
var _ kernelapi.GraphMemoryOpener = GraphOpener{}
