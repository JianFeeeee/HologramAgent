package memory

// "nil 加读取前检查" 的判据（用户指定的做法）。
//
// ## 为什么需要判据守它
//
// `KernelState` 的字段是**类型化的**（用户设计），好处是拼错/取错类型是
// 编译错误。但**跨模块的"未就绪"仍表现为 nil** —— 而 nil 解引用会
// **panic**，栈信息指向无关处（如 `strings.Split`），排查成本高。
//
// 用户明确要求："结构体成员初始置 nil 加**读取前检查**"。
// 本判据确保每个模块对**不由依赖图保证**的字段（Group 0 产物）做检查。
//
// ★ 为什么要判据而不是靠自觉：`ks.CfgReg` 是 Group 0 的产物，
// **无法通过 Requires 声明**（Group 0 不是调度模块）⇒ 依赖图保证不了它。
// 这正是最容易忘、忘了就 panic 的地方。

import (
	"path/filepath"
	"strings"
	"testing"

	internalConfig "github.com/JianFeeeee/HomeAgent/internal/config"
	"github.com/JianFeeeee/HomeAgent/internal/kernelboot"
)

// ★★★ 判据：**CfgReg 为 nil 时给可操作错误，而不是 panic**。
func TestMemoryModuleRejectsNilCfgReg(t *testing.T) {
	dir := t.TempDir()
	ks := &kernelboot.KernelState{DataDir: dir} // CfgReg 故意为 nil

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("★ CfgReg 为 nil 时 panic 了（应返回可操作错误）: %v\n"+
				"   用户要求的是「nil 加读取前检查」，panic 的栈信息指向无关处", r)
		}
	}()

	err := (Module{}).Init(ks)
	if err == nil {
		t.Fatal("★ CfgReg 为 nil 却成功返回 —— 后面会在读配置时 panic")
	}
	// 错误必须点出**是哪个字段**以及**该做什么**
	msg := err.Error()
	if !strings.Contains(msg, "CfgReg") {
		t.Errorf("错误应点名字段 CfgReg（便于定位）: %v", err)
	}
	if !strings.Contains(msg, "Group 0") {
		t.Errorf("错误应指出根因（Group 0 必须先跑）: %v", err)
	}
}

// ★★★ 判据：**DataDir 为空时也拒绝**（同类检查）。
func TestMemoryModuleRejectsEmptyDataDir(t *testing.T) {
	dir := t.TempDir()
	reg := newTestReg(t, dir)
	ks := &kernelboot.KernelState{CfgReg: reg} // DataDir 空

	err := (Module{}).Init(ks)
	if err == nil {
		t.Fatal("★ DataDir 为空却成功返回")
	}
	if !strings.Contains(err.Error(), "DataDir") {
		t.Errorf("错误应点名字段 DataDir: %v", err)
	}
}

func newTestReg(t *testing.T, dir string) *internalConfig.ConfigRegistry {
	t.Helper()
	return internalConfig.NewConfigRegistry(filepath.Join(dir, "config.db"))
}
