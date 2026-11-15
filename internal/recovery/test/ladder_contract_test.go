package test

// internal/recovery 的**恢复梯子**判据 —— 面向接口的黑盒。
//
// 入口只有 `recovery.Ladder{ResultFile, Attempt, Probe, RestoreNetwork,
// RestoreConfig, Log}` 与 `Run(ctx)`，断言只看返回的 `*Result` 与落盘文件。
// 梯子内部怎么排序、怎么记录步骤都不在约束范围内 —— 但**步骤顺序对外
// 可见**（Result.Steps 是 guard 与运维读的），所以只钉"可分辨的先后"，
// 不钉具体文案。
//
// ## 判据要覆盖的职责
//
// 恢复梯子的核心契约是**短路**：某一级小修生效就立刻停，不做后续更重的动作。
//
//	probe → 还原 DNS/proxy → probe → 还原 config 快照 → probe
//
// 这不只是省时间：还原 config 快照会覆盖用户的配置改动，能不做就不该做。
// 原判据只测了 Task/Result 的文件往返，整条梯子零覆盖。

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/recovery"
)

// probeScript 是按调用次序返回预设结果的探针，跑完最后一个就一直重复它。
//
// 用序列而不是"永远返回同一个值"，才能表达"还原前不通、还原后通了"。
type probeScript struct {
	results []bool
	calls   atomic.Int32
}

func (p *probeScript) fn(ctx context.Context, label string) (bool, error) {
	i := int(p.calls.Add(1)) - 1
	if i >= len(p.results) {
		i = len(p.results) - 1
	}
	return p.results[i], nil
}

func (p *probeScript) count() int { return int(p.calls.Load()) }

// ladder 造一个只装配判据需要的钩子的梯子。
//
// restoreNet/restoreCfg 传 nil 时用"什么都不做"的实现，并把是否被调用
// 记进对应的计数器 —— 短路判据要能看出"更重的那一步没被执行"。
func newLadder(t *testing.T, probe recovery.Prober) (*recovery.Ladder, *int32, *int32) {
	t.Helper()
	var netCalls, cfgCalls int32
	l := &recovery.Ladder{
		ResultFile: filepath.Join(t.TempDir(), "result.json"),
		Attempt:    1,
		Probe:      probe,
		RestoreNetwork: func() ([]string, error) {
			atomic.AddInt32(&netCalls, 1)
			return []string{"/etc/resolv.conf"}, nil
		},
		RestoreConfig: func() (int, error) {
			atomic.AddInt32(&cfgCalls, 1)
			return 0, nil
		},
	}
	return l, &netCalls, &cfgCalls
}

// ---- 判据 1：初始探测就通 ⇒ 什么都不修 ----

// TestHealthyProbeSkipsAllRestores：rescue 源第一次就可达时，梯子必须立即返回，
// **不调用任何还原动作**。
//
// 这条守的是"别乱动"：网络/配置还原都会改用户的机器。没有这条判据，
// 一个"总是把所有还原跑一遍"的实现照样绿 —— 而它会在每次巡检里
// 覆盖用户配置。
func TestHealthyProbeSkipsAllRestores(t *testing.T) {
	probe := &probeScript{results: []bool{true}}
	l, netCalls, cfgCalls := newLadder(t, probe.fn)

	res := l.Run(context.Background())
	if !res.Success {
		t.Fatalf("★ 初始探测已通，结论却是失败：%+v", res)
	}
	if res.Cause != "healthy" {
		t.Fatalf("★ 初始探测已通，根因应为 healthy，实际 %q", res.Cause)
	}
	if n := atomic.LoadInt32(netCalls); n != 0 {
		t.Fatalf("★ 初始探测已通，却调用了网络还原 %d 次（会改用户的机器）", n)
	}
	if n := atomic.LoadInt32(cfgCalls); n != 0 {
		t.Fatalf("★ 初始探测已通，却调用了配置还原 %d 次（会覆盖用户配置）", n)
	}
	if n := probe.count(); n != 1 {
		t.Fatalf("★ 初始探测已通，却探测了 %d 次（应为 1）", n)
	}
}

// ---- 判据 2：网络还原生效 ⇒ 停在第一步，不碰配置 ----

// TestNetworkRestoreShortCircuits：网络还原后探测通过，梯子必须停在那一级。
//
// 这条是短路契约的主体：配置快照还原是更重、更有破坏性的动作，
// 网络已经修好时再跑它就是在做无谓的风险敞口。
func TestNetworkRestoreShortCircuits(t *testing.T) {
	probe := &probeScript{results: []bool{false, true}}
	l, netCalls, cfgCalls := newLadder(t, probe.fn)

	res := l.Run(context.Background())
	if !res.Success {
		t.Fatalf("★ 网络还原后已可达，结论却是失败：%+v", res)
	}
	if res.Cause != "network_config" {
		t.Fatalf("★ 期望根因 network_config，实际 %q", res.Cause)
	}
	if atomic.LoadInt32(netCalls) != 1 {
		t.Fatalf("★ 网络还原应被调用 1 次，实际 %d", atomic.LoadInt32(netCalls))
	}
	if n := atomic.LoadInt32(cfgCalls); n != 0 {
		t.Fatalf("★ 网络已修好，却仍执行了配置还原 %d 次 —— 短路失效", n)
	}
	// 还原动过的文件必须回传给 guard（它据此写日志/展示）。
	if len(res.RestoredNet) != 1 || res.RestoredNet[0] != "/etc/resolv.conf" {
		t.Fatalf("★ 还原过的网络文件没回传：%+v", res.RestoredNet)
	}
	if probe.count() != 2 {
		t.Fatalf("★ 探测次数应为 2（初始 + 网络后），实际 %d", probe.count())
	}
}

// ---- 判据 3：网络无效 ⇒ 才走到配置还原 ----

// TestConfigRestoreIsLastResort：网络还原后仍不通，才允许还原配置快照。
//
// 同时钉住"配置还原过什么"必须回传：guard 要据此判断是否覆盖了用户改动。
func TestConfigRestoreIsLastResort(t *testing.T) {
	probe := &probeScript{results: []bool{false, false, true}}
	l, netCalls, cfgCalls := newLadder(t, probe.fn)
	var restoredKeys int32 = 3
	l.RestoreConfig = func() (int, error) {
		atomic.AddInt32(cfgCalls, 1)
		return int(restoredKeys), nil
	}

	res := l.Run(context.Background())
	if !res.Success {
		t.Fatalf("★ 配置还原后已可达，结论却是失败：%+v", res)
	}
	if res.Cause != "config" {
		t.Fatalf("★ 期望根因 config，实际 %q", res.Cause)
	}
	if atomic.LoadInt32(cfgCalls) != 1 {
		t.Fatalf("★ 配置还原应被调用 1 次，实际 %d", atomic.LoadInt32(cfgCalls))
	}
	if !res.ConfigRestored {
		t.Fatal("★ 还原了 3 个键，结论里却标记未还原")
	}
	if probe.count() != 3 {
		t.Fatalf("★ 探测次数应为 3（初始 + 网络后 + 配置后），实际 %d", probe.count())
	}
	_ = netCalls
}

// ---- 判据 4：全部手段用尽 ⇒ 如实报失败 ----

// TestExhaustedLadderReportsFailure：三级都失败时结论必须是失败，
// 且 `QuickChatOK` 不能报 true。
//
// 这条守的是"不许乐观"：guard 靠这个结论决定是否升级到更重的手段
// （拉起 failback worker / 通知人）。报成成功就没人来处理了。
func TestExhaustedLadderReportsFailure(t *testing.T) {
	probe := &probeScript{results: []bool{false}}
	l, _, cfgCalls := newLadder(t, probe.fn)

	res := l.Run(context.Background())
	if res.Success {
		t.Fatalf("★ 全部手段都失败，结论却报成功：%+v", res)
	}
	if res.QuickChatOK {
		t.Fatal("★ 梯子用尽却报 QuickChatOK=true —— guard 会以为已恢复")
	}
	if res.Cause != "unreachable" {
		t.Fatalf("★ 全试过仍不可达，根因应保持 unreachable，实际 %q "+
			"—— guard 会按错误的根因选下一步", res.Cause)
	}
	if atomic.LoadInt32(cfgCalls) != 1 {
		t.Fatalf("★ 前两级都没修好时，配置还原应被执行 1 次，实际 %d",
			atomic.LoadInt32(cfgCalls))
	}
	// 走过的步骤要能看出"试过哪些级"，否则运维无从判断该升级到哪一步。
	if len(res.Steps) < 3 {
		t.Fatalf("★ 步骤记录过少（%d 条），无法判断试过哪几级：%v", len(res.Steps), res.Steps)
	}
	joined := strings.Join(res.Steps, "|")
	for _, want := range []string{"network", "config"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("★ 步骤记录里没有 %q 这一级：%v", want, res.Steps)
		}
	}
}

// ---- 判据 5：结论必须落盘（guard 读文件，不共享内存）----

// TestRunPersistsResultForGuard：Run 完成后 ResultFile 必须可被 LoadResult 读回，
// 且内容与返回值一致。
//
// 这是 guard↔worker 的**跨进程协议**：两个进程之间只有这个文件。
// 不落盘 ⇒ guard 永远等不到结论。
func TestRunPersistsResultForGuard(t *testing.T) {
	probe := &probeScript{results: []bool{true}}
	l, _, _ := newLadder(t, probe.fn)

	res := l.Run(context.Background())

	got, err := recovery.LoadResult(l.ResultFile)
	if err != nil {
		t.Fatalf("★ Run 之后结论文件读不回来（guard 收不到）：%v", err)
	}
	if got.Attempt != res.Attempt || got.Success != res.Success || got.Cause != res.Cause {
		t.Fatalf("★ 落盘内容与返回值不一致：盘上 %+v，返回 %+v", got, res)
	}
}

// ---- 判据 6：探测报错 ≠ 探测为假，两者都必须继续往下走 ----

// TestProbeErrorStillAdvancesLadder：探测函数返回 error（而不是 false）时，
// 梯子必须当作"不可达"继续往下修，不能直接中止。
//
// 真实场景里"探测失败"最常见的原因就是网络本身坏了 —— 恰恰是梯子该干活
// 的时候。中止在那里等于自愈在最需要它的时刻放弃。
func TestProbeErrorStillAdvancesLadder(t *testing.T) {
	var calls atomic.Int32
	probe := func(ctx context.Context, label string) (bool, error) {
		if calls.Add(1) == 1 {
			return false, errProbe
		}
		return true, nil
	}
	var netCalls int32
	l := &recovery.Ladder{
		ResultFile: filepath.Join(t.TempDir(), "result.json"),
		Attempt:    2,
		Probe:      probe,
		RestoreNetwork: func() ([]string, error) {
			atomic.AddInt32(&netCalls, 1)
			return nil, nil
		},
		RestoreConfig: func() (int, error) { return 0, nil },
	}

	res := l.Run(context.Background())
	if atomic.LoadInt32(&netCalls) != 1 {
		t.Fatal("★ 探测报错时梯子没有继续执行网络还原 —— 自愈在最需要时放弃")
	}
	if !res.Success {
		t.Fatalf("★ 网络还原后可达，结论却是失败：%+v", res)
	}
}

// ---- 支撑 ----

type probeError struct{}

func (probeError) Error() string { return "probe: quickchat unreachable" }

var errProbe error = probeError{}
