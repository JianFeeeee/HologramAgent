package memory

// 蒸馏器接线回归判据（从 cmd/homed/bootstrap_test.go 迁移而来）。
//
// ## 为什么这条判据必须存在（历史 bug）
//
// `main()` 拆分时，initMemoryStack 函数体内残留一句 `defer distiller.Stop()`，
// 函数一 return 就 cancel 掉刚启动的循环 ⇒ **规则蒸馏 10min 心跳从不运行**。
//
// ★ 该缺陷**不会让任何单测变红** —— pipeline 的 `TestDistillOnce*` 直接调
// `distillOnce`，**绕过了 Start/Stop 接线**。只有按「启动阶段函数」的真实
// 调用方式断言（调用 → 检查仍在跑 → 调 cleanup → 检查已停），才照得出来。
//
// ## 迁移说明
//
// 本判据原先针对 `cmd/homed` 的 `initMemoryStack`。模块化后该逻辑搬进
// `internal/kernelmod/memory.Module.Init`，所以判据跟着搬 —— 且**必须保留**：
// 迁移过程中"忘了 AddCloser"或"改成 defer"是同一类缺陷的新形态。
//
// 判据改为直接跑真实模块（经 kernelboot.Run），确保测的是**真实入口**。

import (
	"os"
	"path/filepath"
	"testing"

	internalConfig "github.com/JianFeeeee/HomeAgent/internal/config"
	"github.com/JianFeeeee/HomeAgent/internal/kernelboot"
)

// ★★★ 判据：模块 Init 返回后蒸馏器**仍在运行**（无 defer Stop 残留）。
func TestMemoryModuleKeepsDistillerRunning(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	// ★ 需要真 cfgReg：memory 模块读 core.agent.embedding_model_path，
	// 而它对 nil 会现在**明确报错**（而不是 panic 在 strings.Split）。
	reg := internalConfig.NewConfigRegistry(filepath.Join(dir, "config.db"))
	defer reg.Close()
	ks := &kernelboot.KernelState{DataDir: dir, CfgReg: reg}

	if err := (Module{}).Init(ks); err != nil {
		t.Fatalf("memory 模块 Init: %v", err)
	}
	if ks.Memory == nil || ks.Memory.Distiller == nil {
		t.Fatal("memory 模块未产出蒸馏器")
	}
	if ks.Memory.DB == nil {
		t.Skip("图库未初始化，无法验证蒸馏接线")
	}
	// ★ 关键断言：Init 已返回，蒸馏循环必须还在跑
	if ks.Memory.Distiller.Stopped() {
		t.Fatal("★ memory 模块 Init 返回后蒸馏循环已被停掉" +
			"（defer Stop 残留？）—— 规则蒸馏 10min 心跳将从不运行")
	}

	// 关闭函数是唯一的停机点（经 AddCloser 登记，由 KernelState.CloseAll 逆序执行）
	if err := ks.CloseAll(); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if !ks.Memory.Distiller.Stopped() {
		t.Fatal("CloseAll 之后蒸馏器应已停止（关闭函数未登记？）")
	}
}

// ★★★ 判据：**关闭函数经 AddCloser 登记**（不是 defer）。
//
// 这是上一条的机制保证：若有人把 AddCloser 改回 defer，
// 上一条判据报红；本判据报"没登记"，两条互相印证、定位更快。
func TestMemoryModuleRegistersCloser(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "memory"), 0o755)
	reg := internalConfig.NewConfigRegistry(filepath.Join(dir, "config.db"))
	defer reg.Close()
	ks := &kernelboot.KernelState{DataDir: dir, CfgReg: reg}
	if err := (Module{}).Init(ks); err != nil {
		t.Fatal(err)
	}
	// 内存模块应恰好登记 1 个关闭函数（停蒸馏器 + 关图库，合在一个闭包里）
	if len(ks.Closers) != 1 {
		t.Fatalf("★ 应登记 1 个关闭函数，实际 %d —— "+
			"若为 0 说明改成了 defer（Init 返回即停机）", len(ks.Closers))
	}
}
