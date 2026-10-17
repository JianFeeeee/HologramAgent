package proc

// 「插件自行声明的建链超时」——**真实消费者**判据（2026-10-16）。
//
// 设计见 docs/zh/kernel-modularization-design.md §2.4.2。
//
// ## 为什么必须测 proc.Spawn，而不是测 manifest 字段
//
// 本仓反复吃过的坑：**字段加了、解析对了，但没人读它**。
// 只测 `manifest.HandshakeTimeout()` 的换算，删掉 `Start()` 里传给 Spawn 的那一行
// 判据依然全绿 —— 那是假绿。
//
// 所以这里测**真实入口** `proc.Spawn` 的**可观测行为**：用同一个"慢握手"插件，
// 只改超时值，断言结果随之改变。
//
// ## 为什么需要专门的慢插件（slowshakeplugin）
//
// 现有的 echoplugin 等**立即**回握手 ⇒ 超时取多少都不会改变结果 ⇒
// 用它们写的判据必然假绿。要观测超时，被观测对象必须"慢到能让不同超时值分叉"。

import (
	"os"
	"strings"
	"testing"
	"time"
)

// spawnWithTimeout 起一个慢握手插件，返回结果（error 与耗时）。
//
// delayMS 由 SLOW_SHAKE_MS 环境变量传给插件（见 slowshakeplugin 的说明）。
func spawnWithTimeout(t *testing.T, timeout time.Duration, delayMS int) (err error, elapsed time.Duration) {
	t.Helper()
	bin := buildTestPlugin(t, "slowshakeplugin.go")

	// 环境变量经 os.Environ 传给子进程（Spawn 会继承并追加自己的）。
	// itoa 复用本包既有实现（shm_profile_test.go），避免重复定义。
	t.Setenv("SLOW_SHAKE_MS", itoa(delayMS))

	start := time.Now()
	p, err := Spawn("slowshake", bin, Options{
		Handler:          noopHandler,
		HandshakeTimeout: timeout,
	})
	elapsed = time.Since(start)
	if err == nil {
		p.Kill()
	}
	return err, elapsed
}

// ★★★ **核心判据**：声明的超时真的改变了 Spawn 的行为。
//
// 同一个慢插件（握手延迟 3s），只改超时：
//
//	超时 1s  ⇒ 必须**失败**（短超时被用上）
//	超时 10s ⇒ 必须**成功**（长超时被用上，证明"声明更长"真的有效）
//
// 这两条一起才构成证明：
//
//	· 只有第一条 ⇒ 可能超时参数根本没生效，"失败"另有原因
//	· 只有第二条 ⇒ 可能参数没生效、只是默认值足够长
func TestSpawnHonorsDeclaredHandshakeTimeout(t *testing.T) {
	const delayMS = 3000

	// ① 短超时（1s）必须失败
	errShort, elShort := spawnWithTimeout(t, 1*time.Second, delayMS)
	if errShort == nil {
		t.Fatal("★ 声明 1s 超时、插件延迟 3s，却握手成功了 —— 超时参数没被用上")
	}
	// 且必须**大约在** 1s 失败（不是等满 3s 或 10s 才失败）
	if elShort > 2500*time.Millisecond {
		t.Errorf("★ 1s 超时却等了 %v 才失败 —— 超时值未生效（像是用了默认值）", elShort)
	}
	if elShort < 500*time.Millisecond {
		t.Errorf("失败过快（%v）—— 失败原因可能不是超时，判据失去意义", elShort)
	}
	// 失败原因必须是超时（避免"因为别的原因失败"而假绿）
	if !strings.Contains(errShort.Error(), "超时") && !strings.Contains(errShort.Error(), "deadline") {
		t.Errorf("失败原因应指向超时: %v", errShort)
	}

	// ② 长超时（10s）必须成功
	errLong, elLong := spawnWithTimeout(t, 10*time.Second, delayMS)
	if errLong != nil {
		t.Fatalf("★ 声明 10s 超时、插件延迟 3s，却握手失败 —— 长超时未被用上: %v", errLong)
	}
	// 成功耗时必须 ≈ 插件延迟（3s），证明它确实等到了
	if elLong < 2500*time.Millisecond {
		t.Errorf("成功过快（%v）—— 插件真的延迟了吗？判据前提可疑", elLong)
	}
}

// ★★★ 判据：**未声明超时（<=0）⇒ 用默认值**（存量插件行为逐字不变）。
//
// 硬约束：现网 42 个插件的 plugin.json 都没有该字段。
// 若"未声明"被当成"立即超时"，它们会**全部加载失败** —— 那是破坏性变更。
func TestSpawnUsesDefaultWhenTimeoutNotDeclared(t *testing.T) {
	// 延迟 3s：若默认值是 10s（现状），必须成功
	err, el := spawnWithTimeout(t, 0, 3000)
	if err != nil {
		t.Fatalf("★ 未声明超时（0）应回退到默认 %v，却失败: %v", DefaultHandshakeTimeout, err)
	}
	if el < 2500*time.Millisecond {
		t.Errorf("成功过快（%v），判据前提可疑", el)
	}

	// 负数同样回退默认（不是"立即超时"）
	err2, _ := spawnWithTimeout(t, -5*time.Second, 3000)
	if err2 != nil {
		t.Fatalf("★ 负数超时应回退默认而非立即失败: %v", err2)
	}
}

// ★★★ 判据：默认值必须是 10s（唯一真相源，且与文档一致）。
//
// 这条独立存在是因为**默认值漂移**很难被发现：
// 改大/改小都能通过前面所有行为判据（它们只关心"回退发生了"），
// 而实际影响是"轻插件白等"或"重插件被误杀"。
func TestDefaultHandshakeTimeoutValue(t *testing.T) {
	if DefaultHandshakeTimeout != 10*time.Second {
		t.Errorf("默认建链超时应为 10s（文档与部署预期），实际 %v", DefaultHandshakeTimeout)
	}
	// 包内旧名必须与导出名一致（防止两处漂移）
	if defaultHandshakeTimeout != DefaultHandshakeTimeout {
		t.Errorf("defaultHandshakeTimeout 与导出名不一致: %v vs %v",
			defaultHandshakeTimeout, DefaultHandshakeTimeout)
	}
}

// 保证 os 包被引用（环境变量传递路径）。
var _ = os.Getenv

// ★★★ **接线判据**：走真实入口 `proc.NewWithTimeout → Plugin.Start`，
// 证明声明的超时**穿过构造函数与 Start** 到达 Spawn。
//
// ## 为什么必须独立存在（本会话第 10 次同类教训）
//
// 上面那条 `TestSpawnHonorsDeclaredHandshakeTimeout` 直接把
// `Options.HandshakeTimeout` 喂给 `Spawn` —— 它证明的是"**Spawn 会用这个值**"，
// **没有覆盖** `NewWithTimeout(timeout) → Plugin.handshakeTimeout → Start 里传给 Spawn`
// 这一段接线。实测：把 `Start()` 里那一行删掉，那条判据**依然全绿**。
//
// 而真实路径是：`dynamic_proc.go` 读 manifest → `NewWithTimeout` → `Start` → `Spawn`。
// 断掉其中任何一环，"插件自行声明超时"就是空谈 —— 正是本仓最怕的
// "字段加了没人读"。
func TestPluginStartPassesDeclaredTimeoutToSpawn(t *testing.T) {
	const delayMS = 3000

	// ① 短超时（1s）+ 慢插件（3s）⇒ Start 必须失败
	bin := buildTestPlugin(t, "slowshakeplugin.go")
	t.Setenv("SLOW_SHAKE_MS", itoa(delayMS))

	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	p := NewWithTimeout("slow1", bin, t.TempDir(), nil, host, nil, 1*time.Second)
	start := time.Now()
	errShort := p.Start(newFakeCore())
	elShort := time.Since(start)
	if errShort == nil {
		p.Close()
		t.Fatal("★ 声明 1s 超时、插件延迟 3s，Start 却成功了 —— " +
			"NewWithTimeout 的超时没传到 Spawn（接线断了）")
	}
	if elShort > 2500*time.Millisecond {
		t.Errorf("★ 1s 超时却等了 %v —— 接线可能断了（用了默认值）", elShort)
	}

	// ② 长超时（10s）+ 同一个慢插件 ⇒ Start 必须成功
	host2, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host2.Close()

	p2 := NewWithTimeout("slow2", bin, t.TempDir(), nil, host2, nil, 10*time.Second)
	if err := p2.Start(newFakeCore()); err != nil {
		t.Fatalf("★ 声明 10s 超时、插件延迟 3s，Start 却失败 —— "+
			"接线可能把超时弄成了 0 或极小值: %v", err)
	}
	defer p2.Close()

	// ③ 不声明的路径（New）必须等价于用默认值 —— 存量插件零改动
	//    用 3s 延迟 + 默认 10s ⇒ 必须成功
	host3, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host3.Close()

	p3 := New("slow3", bin, t.TempDir(), nil, host3, nil) // 不传超时
	if err := p3.Start(newFakeCore()); err != nil {
		t.Fatalf("★ 不声明超时的插件（存量路径）应走默认 10s 并成功: %v", err)
	}
	defer p3.Close()
}
