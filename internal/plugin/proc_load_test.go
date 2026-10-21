//go:build linux || darwin

package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/plugin/proc"
	isdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// 子进程插件经 Registry 加载的接线测试（Part 3 收尾）。
//
// 这里验证的是"内核侧接线"，不是共享段语义本身——后者由
// internal/plugin/proc 的 34 项测试（含 -race）覆盖。

// 共享段 Host 必须惰性创建且**全局唯一**。
//
// 每插件一段会让「内核 ctx → 段 → 插件改 → 回读 ctx」在多插件下
// 退化成副本模型，lost update 原样复现（§8.4 实测 35.8~36.8%）。
func TestRegistry_ProcHostIsSharedAndLazy(t *testing.T) {
	r := NewRegistry()
	defer r.closeProcHost()

	if r.procHost != nil {
		t.Error("共享段应惰性创建，未加载 .bin 插件时不该存在")
	}

	h1, err := r.ensureProcHost()
	if err != nil {
		t.Fatalf("创建共享段: %v", err)
	}
	h2, err := r.ensureProcHost()
	if err != nil {
		t.Fatalf("二次获取共享段: %v", err)
	}
	if h1 != h2 {
		t.Fatal("共享段必须全局唯一——每插件一段会退化成副本模型，lost update 复现")
	}
	if h1.ShmSize() <= 0 {
		t.Errorf("共享段大小应为正数，实际 %d", h1.ShmSize())
	}
}

func TestRegistry_CloseProcHostIsIdempotent(t *testing.T) {
	r := NewRegistry()
	if _, err := r.ensureProcHost(); err != nil {
		t.Fatalf("创建共享段: %v", err)
	}
	r.closeProcHost()
	r.closeProcHost() // 二次关闭不应 panic
	if r.procHost != nil {
		t.Error("关闭后 procHost 应为 nil")
	}
}

// loadProc 对非 proc 目录返回 nil,nil（交由后续探测通道）。
func TestRegistry_LoadProcSkipsNonProcDir(t *testing.T) {
	r := NewRegistry()
	defer r.closeProcHost()

	plg, err := r.loadProc(t.TempDir(), "demo", nil)
	if plg != nil || err != nil {
		t.Fatalf("无 plugin.bin 应返回 nil,nil，实际 plg=%v err=%v", plg, err)
	}
	if r.procHost != nil {
		t.Error("非 proc 目录不该触发共享段创建")
	}
}

// 缺可执行权限时报明确错误——常见于经 zip/hmap 分发丢失权限位。
func TestRegistry_LoadProcRejectsNonExecutable(t *testing.T) {
	r := NewRegistry()
	defer r.closeProcHost()

	dir := t.TempDir()
	path := filepath.Join(dir, binEntry)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := r.loadProc(dir, "demo", nil)
	if err == nil {
		t.Fatal("缺少可执行权限应报错")
	}
	// 错误消息须给出可直接执行的修复指令
	if !strings.Contains(err.Error(), "chmod +x") {
		t.Errorf("错误消息应含 chmod +x 修复指令，实际: %v", err)
	}
}

// 构造出的插件必须能满足 registry 的 sdk.Plugin 接口（含 Close 供重载 kill 进程）。
func TestRegistry_LoadProcReturnsAdapter(t *testing.T) {
	r := NewRegistry()
	defer r.closeProcHost()

	dir := t.TempDir()
	path := filepath.Join(dir, binEntry)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec cat\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	plg, err := r.loadProc(dir, "demo", nil)
	if err != nil {
		t.Fatalf("loadProc: %v", err)
	}
	if plg == nil {
		t.Fatal("应返回插件实体")
	}
	if plg.Name() != "demo" {
		t.Errorf("Name() = %q, want demo", plg.Name())
	}
	// Close 必须可见：registry.closeDynamic 靠它真正 kill 子进程。
	// 对比 cabi 路径的 Close 只做 dlclose，而 dlclose 对 Go c-shared 是 no-op
	// （§1.1，热重载静默失效的根因）。
	if _, ok := plg.(interface{ Close() error }); !ok {
		t.Error("proc 插件须暴露 Close()，否则重载时子进程不会被回收")
	}
	if r.procHost == nil {
		t.Error("加载 .bin 插件应触发共享段创建")
	}
}

// procCore 必须满足 proc.CoreSDK，且**刻意不暴露**内核内部机制。
//
// 这是 §3.8 的核心：权限梯度从「C ABI 表达能力的意外产物」
// 变成显式声明并强制的策略。
//
// 守住的不变量：procCore 用**命名字段**持有内核 SDK。若日后有人改成
// 嵌入 *isdk.PluginSDK，全部方法会被提升，外部插件就能经类型断言拿到
// 这些内核能力——下面的断言会当场拦住。
func TestProcCore_SatisfiesCoreSDKAndWithholdsInternals(t *testing.T) {
	var _ proc.CoreSDK = procCore{}

	var c interface{} = procCore{}

	if _, has := c.(interface{ Selftest() *isdk.VirtualInstance }); has {
		t.Error("procCore 不应暴露 Selftest（§3.8 权限梯度）")
	}
	if _, has := c.(interface{ Supervisor() isdk.SupervisorAPI }); has {
		t.Error("procCore 不应暴露 Supervisor（§3.8 权限梯度）")
	}
	if _, has := c.(interface{ Tracker() isdk.TrackerAPI }); has {
		t.Error("procCore 不应暴露 Tracker（§3.8 权限梯度）")
	}
	if _, has := c.(interface{ Adapter() isdk.AdapterAPI }); has {
		t.Error("procCore 不应暴露 Adapter（§3.8 权限梯度）")
	}
	if _, has := c.(interface{ Indexer() isdk.IndexerAPI }); has {
		t.Error("procCore 不应暴露 Indexer（§3.8 权限梯度）")
	}
	if _, has := c.(interface{ Status() isdk.StatusAPI }); has {
		t.Error("procCore 不应暴露 Status（§3.8 权限梯度）")
	}
}

// nil SDK 下各能力访问器必须返回真 nil（而非类型化 nil）。
//
// corehandler 用 `if xxx == nil` 判断能力不可用并返回 errUnavailable；
// 类型化 nil 会让判空失效，插件收到的是 panic 而不是"能力不可用"。
func TestProcCore_NilCapabilitiesAreTrueNil(t *testing.T) {
	c := newProcCore(&isdk.PluginSDK{})

	if c.Settings() != nil {
		t.Error("Settings() 应为真 nil")
	}
	if c.Memory() != nil {
		t.Error("Memory() 应为真 nil")
	}
	if c.TextMemory() != nil {
		t.Error("TextMemory() 应为真 nil")
	}
	if c.DocMemory() != nil {
		t.Error("DocMemory() 应为真 nil")
	}
	if c.Knowledge() != nil {
		t.Error("Knowledge() 应为真 nil")
	}
	if c.LLM() != nil {
		t.Error("LLM() 应为真 nil")
	}
	if c.PluginMgr() != nil {
		t.Error("PluginMgr() 应为真 nil")
	}
	// 无 IOManager 时同步注入返回空串，不 panic
	if got := c.InjectInputSync("s", "c", "t"); got != "" {
		t.Errorf("无 IOManager 时应返回空串，实际 %q", got)
	}
}
