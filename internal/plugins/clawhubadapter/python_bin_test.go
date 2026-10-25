package clawhubadapter

import (
	"os"
	"path/filepath"
	"testing"
)

// ── Python 解释器选择优先级 ──────────────────────────────────
//
// 背景（2026-10-25 真机事故）：bilibili skill 声明 requirements 含 httpx，
// 系统 Python 没装 ⇒ 启动即 ModuleNotFoundError。而此前解释器是**硬编码猜**的：
// 先 /usr/bin/python3，再 /usr/local/bin/python3。
//
// 「猜」没有正确解：不同部署的 Python 布局完全不同（发行版 Python、
// pyenv、uv、conda、venv…）。所以改为配置显式指定 + venv 探测兜底。

// 判据：配置了 python_bin 且可用 ⇒ 必须用它，优先于 venv 与系统解释器。
//
// 这是"配置生效"的唯一证据。没有它，配了却仍走系统解释器也不会被发现
// （正是本次事故的形态：静默降级）。
func TestPickPython_ConfiguredWins(t *testing.T) {
	dir := t.TempDir()
	// 造一个假的 venv 解释器，确保"配置优先于 venv"被真正验证
	venvBin := filepath.Join(dir, ".venv", "bin")
	os.MkdirAll(venvBin, 0755)
	venvPy := filepath.Join(venvBin, "python")
	os.WriteFile(venvPy, []byte("#!/bin/sh\n"), 0755)

	cfgPy := filepath.Join(dir, "custom-python")
	os.WriteFile(cfgPy, []byte("#!/bin/sh\n"), 0755)

	got := pickPythonFor("bilibili", dir, cfgPy)
	if got != cfgPy {
		t.Fatalf("配置的 python_bin 应最高优先，期望 %s，实际 %s", cfgPy, got)
	}
}

// 判据：没配 python_bin 但 skill 有 venv ⇒ 用 venv。
func TestPickPython_VenvWhenNotConfigured(t *testing.T) {
	dir := t.TempDir()
	venvBin := filepath.Join(dir, ".venv", "bin")
	os.MkdirAll(venvBin, 0755)
	venvPy := filepath.Join(venvBin, "python")
	os.WriteFile(venvPy, []byte("#!/bin/sh\n"), 0755)

	if got := pickPythonFor("x", dir, ""); got != venvPy {
		t.Fatalf("无配置时应优先 skill 自带 venv，期望 %s，实际 %s", venvPy, got)
	}
}

// 判据：配置了但**不存在** ⇒ 必须报错，不能静默退回系统解释器。
//
// 静默退回的后果：用户以为配好了，实际跑在缺依赖的 Python 上 ——
// 这正是本次要修的「静默降级」那一族。
func TestPickPython_MissingConfiguredIsLoud(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "no-such-python")
	_, err := resolvePython("x", dir, bad)
	if err == nil {
		t.Fatal("配置的 python_bin 不存在时必须报错（静默退回会让用户以为配置生效）")
	}
}
