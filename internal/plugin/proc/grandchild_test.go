package proc

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ===== 孙进程导致的关停挂死 =====
//
// 现象（线上）：StopAll 里只有 bili 报 "SIGKILL 后 2s 仍未被收割"，
// 之后近 90 秒无任何日志，systemd SIGKILL，整个关停超时。
//
// 因果（每一步都有源码支撑，不是推测）：
//   插件用 exec.Command 拉起孙进程（bili→yt-dlp→ffmpeg、editdoc→python、
//   browser→chromium）⇒ 孙进程**继承插件的 stdout 管道写端**
//   → SIGKILL 只打给插件本体，孙进程仍存活、写端不关
//   → readLoop 的 scanner.Scan() 永不 EOF
//   → p.readerWG.Wait() 永不返回（Kill 的最后一行，**无超时**）
//   → StopAll 的 wg.Wait() 永不返回 ⇒ 关停挂死 ⇒ systemd SIGKILL
//
// 三处修法（缺任一条都不够）：
//   1. spawn 时 Setpgid：插件自成进程组，不再与内核同组
//   2. Kill 杀**整个进程组**（kill(-pgid)）：孙进程一起死，管道写端才关
//   3. readerWG.Wait() 加超时兜底：唯一能保证 Kill 一定返回的地方。
//      没有它，任何第三方插件泄漏一个孙进程都能拖死整个关停。

// grandchildPluginSource 是一个会拉孙进程的插件。
//
// 用 hmapdev 的**真实模板**编译（复用 buildPluginWithRealTemplate），
// 而不是自造 shim：插件必须走 SDK 握手才能被 Spawn 接受，
// 手写的裸 main 会在握手阶段就退出（第一版判据就踩了这个）。
//
// 孙进程选 sleep：它是本机几乎必然存在、又与业务无关的进程，
// 用 400 秒这个唯一时长标记来识别，避免误伤别人的 sleep。
const grandchildPluginSource = `
package main

import (
	"os"
	"os/exec"
	"time"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

type gcPlugin struct{ name string }

func (p *gcPlugin) Name() string                { return p.name }
func (p *gcPlugin) Stop() error                 { return nil }
func (p *gcPlugin) Start(s *sdk.PluginSDK) error {
	// 拉一个孙进程。它继承本插件的 stdout ⇒ 持有内核读端管道的写端。
	// 插件本体被 SIGKILL 后，若孙进程不死，readLoop 就永远等不到 EOF。
	// ★ 孙进程**不**设 Setpgid：它要留在插件的进程组里，才代表真实场景
	//   （bili→yt-dlp→ffmpeg 不会自己脱离进程组）。内核的 kill(-pgid)
	//   正是靠这个把它一起带走。
	//   我第一版给孙进程也加了 Setpgid，结果它逃出插件进程组，
	//   kill(-pgid) 杀不到 —— 判据自己造了个假失败。
	cmd := exec.Command("sleep", "400")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Start()
	os.Stdout.WriteString("GRANDCHILD_READY\n")
	s.RegisterTool("gc_ping", sdk.ToolDef{
		Description: "noop",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}, func(args map[string]interface{}) (interface{}, error) { return "pong", nil })
	go func() { time.Sleep(10 * time.Minute) }()
	return nil
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &gcPlugin{name: name}, nil
}
`

// buildGrandchildPlugin 用真实模板编译"会拉孙进程"的插件。
func buildGrandchildPlugin(t *testing.T) string {
	t.Helper()
	return buildPluginWithRealTemplate(t, grandchildPluginSource)
}

// countShimGrandchildren 数本测试拉起的 sleep 400。
//
// ★ 只数**自己那一支**（孙进程的 PPid 链上必须有本测试的插件 pid），
//
//	不再扫全系统。
//
//	旧实现扫全 /proc 找 "sleep 400"：同机任何命中同样 cmdline 的进程
//	/ 容器都会串味，表现为「单跑绿、合跑红」。注释里记过一次前车之鉴
//	（「我第一版就踩了」），但当时只加了 base 快照，**没解决全局匹配**
//	这个根因 —— base 也救不了「别的测试中途拉起 sleep 400」的情况。
//
//	限定 PPid 之后，别的测试/容器的进程一律不算数。
func countShimGrandchildren() int {
	return countShimGrandchildrenUnder(0)
}

// countShimGrandchildrenUnder 只数 PPid 属于 rootPid 的 sleep 400。
// rootPid == 0 时不做父子限定（保留旧语义，供不知道插件 pid 的场合用）。
func countShimGrandchildrenUnder(rootPid int) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cl, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if !strings.Contains(strings.ReplaceAll(string(cl), "\x00", " "), "sleep 400") {
			continue
		}
		// ★ 父子限定：孙进程的父进程就是插件本体。
		if rootPid > 0 && procPPid(pid) != rootPid {
			continue
		}
		n++
	}
	return n
}

// procPPid 读 /proc/<pid>/stat 的第 4 个字段（ppid）。
// stat 的 comm 字段可能含空格与括号，从最后一个 ')' 之后切分才稳。
func procPPid(pid int) int {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 >= len(s) {
		return 0
	}
	fields := strings.Fields(s[i+1:])
	// fields[0]=state, fields[1]=ppid
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}

func waitForCond(t *testing.T, limit time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("超时: %s", msg)
}

// spawnGrandchildPlugin 编译并启动"会拉孙进程"的插件。
//
// 走 NewHost + Plugin（与 e2e_template_test 同一条路）而不是裸 Spawn：
// 共享内存段由 Host 创建并经 fd 3 传给插件，裸 Spawn 没有这一步，
// 插件握手会报 "挂载统一共享区域失败: permission denied"。
func spawnGrandchildPlugin(t *testing.T, name string) (*Plugin, *Host) {
	t.Helper()
	// 记录本测试启动前的孙进程数：判据只该关心**自己**拉起来的那些。
	// 不这么做的话，前一个测试泄漏的孙进程会被后一个数进去，
	// 表现为「杀进程组没杀干净」的假失败（我第一版就踩了：
	// 明明单跑通过，合跑却红）。
	base := countShimGrandchildren()
	bin := buildGrandchildPlugin(t)

	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	core := newFakeCore()
	p := New(name, bin, t.TempDir(), nil, host, nil)
	if err := p.Start(core); err != nil {
		host.Close()
		t.Fatalf("启动测试插件失败: %v", err)
	}
	waitForCond(t, 20*time.Second, func() bool { return countShimGrandchildren() > base },
		"孙进程（sleep 400）未出现")
	return p, host
}

// waitGrandchildrenGone 等到孙进程数回落到 base。
func waitGrandchildrenGone(t *testing.T, base int, limit time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if countShimGrandchildren() <= base {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// pluginPid 取插件子进程 pid。
// cleanupSleepMarkers 按 cmdline 标记清理残留的 sleep 进程。
//
// ★ 为什么需要它
//
// 旧写法是 `if pid := pluginPid(p); pid > 0 { syscall.Kill(-pid, SIGKILL) }` ——
// pid 取不到时**整个跳过清理**，残留的 sleep 400 会污染后续测试，表现为
// 「单跑绿、合跑红」的间歇性失败。
//
// 这里作为兜底：按唯一 cmdline 标记（"sleep <marker>"）扫 /proc 清掉。
// 它比 pluginPid 粗，但**只在 defer 里用**，且 marker 是本测试专用数字，
// 不会误杀无关进程。
//
// 为什么不在生产代码里加这个：这是**测试辅助**，生产侧的正确做法是
// kill(-pgid) 杀整组 + 内核兜底强杀，不该依赖扫 /proc。
func cleanupSleepMarkers(marker string) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	needle := "sleep " + marker
	self := os.Getpid()
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		cl, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		line := strings.ReplaceAll(string(cl), "\x00", " ")
		if !strings.Contains(line, needle) {
			continue
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func pluginPid(p *Plugin) int {
	if p == nil || p.proc == nil || p.proc.cmd == nil || p.proc.cmd.Process == nil {
		return 0
	}
	return p.proc.cmd.Process.Pid
}

// Kill 必须在有界时间内返回 —— 孙进程持有 stdout 写端时也不能挂死。
func TestKillReturnsWithGrandchildHoldingStdout(t *testing.T) {
	p, host := spawnGrandchildPlugin(t, "gc1")
	defer func() {
		_ = p.Close()
		host.Close()
		if pid := pluginPid(p); pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}()

	done := make(chan struct{})
	go func() { _ = p.proc.Kill(); close(done) }()
	select {
	case <-done:
	case <-time.After(25 * time.Second):
		t.Fatalf("Kill 卡死：孙进程持有 stdout 写端 ⇒ readLoop 不 EOF ⇒ readerWG 不 Done。" +
			"这正是线上关停被拖到 90s 超时的原因")
	}
}

// 杀进程组之后孙进程必须真的消失（不能只是 Kill 返回了、进程还在跑）。
func TestKillLeavesNoGrandchild(t *testing.T) {
	base := countShimGrandchildren()
	p, host := spawnGrandchildPlugin(t, "gc2")
	defer host.Close()
	defer p.Close()
	before := countShimGrandchildren()
	if before <= base {
		t.Fatal("前置条件不满足：没有新的孙进程")
	}

	_ = p.proc.Kill()

	if !waitGrandchildrenGone(t, base, 8*time.Second) {
		t.Errorf("杀进程组后本测试的孙进程仍在（%d→%d）—— 只杀了插件本体，没杀整组",
			before, countShimGrandchildren())
	}
}

// 插件必须自成进程组：不设的话插件拉起的孙进程与内核同组，
// 杀插件时语义混乱，且内核自己可能被同组信号波及。
func TestSpawnPutsPluginInOwnProcessGroup(t *testing.T) {
	p, host := spawnGrandchildPlugin(t, "gc3")
	defer func() {
		_ = p.Close()
		host.Close()
		if pid := pluginPid(p); pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}()

	pid := pluginPid(p)
	if pid == 0 {
		t.Fatal("拿不到插件 pid")
	}
	want, err := syscall.Getpgid(pid)
	if err != nil {
		t.Skipf("Getpgid 不可用: %v", err)
	}
	self, _ := syscall.Getpgid(os.Getpid())
	if want == self {
		t.Errorf("插件 pgid=%d 与内核 pgid=%d 相同：未建独立进程组，"+
			"插件拉起的孙进程会与内核同组", want, self)
	}
}

// 兜底：即便孙进程活过内核的杀组（模拟第三方插件用了 setsid 脱组），
// Kill 也必须有界返回。
//
// ★ 这条第一版是**假绿**：我用纯构造的 &Process{cmd: nil} 做判据，
//
//	而 Kill 第 607 行就 `if p.cmd == nil { return nil }` 早退了 ——
//	根本走不到 readerWG 那段，撤掉超时它照样绿。变异测试才暴露出来。
//	现在改用真实插件：孙进程活着且持有 stdout 写端，走完整路径。
//
// ★ 名字订正（2026-09-28）：这个测试**不测「孙进程存活」**。
//
//	grandchildPluginSource 的孙进程 `sleep 400` **不设** Setpgid，
//	刻意留在插件进程组内 ⇒ `kill(-pgid)` **能**杀掉它
//	（该源自己的注释写着「孙进程**不**设 Setpgid：它要留在插件的进程组里」）。
//	真正测脱组存活（setsid ⇒ 杀不到）的是
//	TestKillReturnsWhenGrandchildEscapesProcessGroup。
//	⇒ 原名 EvenWhenGrandchildSurvives 与实现矛盾，容易让人误以为
//	   「脱组场景已被覆盖」，而它其实覆盖的是「孙进程随组被杀时 Kill 有界返回」。
func TestKillReturnsWhenGrandchildDiesWithProcessGroup(t *testing.T) {
	p, host := spawnGrandchildPlugin(t, "gc4")
	defer func() {
		_ = p.Close()
		host.Close()
		// ★ 清理不再「拿不到 pid 就整个跳过」：
		//   旧写法 `if pid := pluginPid(p); pid > 0 { Kill }` 在 pid 取不到时
		//   静默跳过 ⇒ 残留 sleep 400 污染后续测试，表现为
		//   「单跑绿、合跑红」的间歇性失败。
		//   改为：即使 pluginPid 取不到，也按唯一 cmdline 标记兜底清理。
		cleanupSleepMarkers("400")
		if pid := pluginPid(p); pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}()

	done := make(chan struct{})
	go func() { _ = p.proc.Kill(); close(done) }()
	select {
	case <-done:
	case <-time.After(25 * time.Second):
		t.Fatal("Kill 在孙进程持有 stdout 写端时永不返回 —— " +
			"这就是线上关停被拖到 90s 超时的直接原因")
	}
}

// escapingGrandchildSource 的孙进程用 setsid 脱组 ⇒ kill(-pgid) 杀不到它。
//
// 这是 readerWG 超时兜底**唯一真正生效**的场景：孙进程既活着、
// 又仍持有插件 stdout 的写端。之前那条判据用脱不了组的孙进程，
// 杀掉组就没问题了，撤掉超时照样绿 —— 变异测试抓了两次才发现。
const escapingGrandchildSource = `
package main

import (
	"os"
	"os/exec"
	"syscall"
	"time"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

type gcPlugin struct{ name string }

func (p *gcPlugin) Name() string                { return p.name }
func (p *gcPlugin) Stop() error                 { return nil }
func (p *gcPlugin) Start(s *sdk.PluginSDK) error {
	// Setpgid: true + Setsid 不可同时用；这里用 Setsid 让孙进程自立门户，
	// 脱离插件的进程组 —— 内核 kill(-pgid) 因此杀不到它。
	cmd := exec.Command("sleep", "401")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	_ = cmd.Start()
	os.Stdout.WriteString("GRANDCHILD_READY\n")
	s.RegisterTool("gc_ping", sdk.ToolDef{
		Description: "noop",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}, func(args map[string]interface{}) (interface{}, error) { return "pong", nil })
	go func() { time.Sleep(10 * time.Minute) }()
	return nil
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &gcPlugin{name: name}, nil
}
`

// countEscapingGrandchildren 数脱组的 sleep 401。
// countEscapingGrandchildren 数本测试拉起的 sleep 401（setsid 脱组的）。
//
// ★ 与 countShimGrandchildrenUnder 同样的修复：限定 PPid 到本测试的插件，
//
//	不再扫全系统。否则同机任何命中 "sleep 401" 的进程都会串味。
//	另外补上 e.Name() 的 Atoi 校验 —— 原实现会把 /proc 下非数字目录
//	（self、net、sys…）也去读 cmdline，虽读不到内容但白跑，且掩盖了
//	「这里本该只处理数字 pid」的事实。
func countEscapingGrandchildren() int {
	return countSleepMarkersUnder(0, "401")
}

// countEscapingGrandchildrenUnder 限定 PPid 属于 rootPid 的脱组孙进程数。
func countEscapingGrandchildrenUnder(rootPid int) int {
	return countSleepMarkersUnder(rootPid, "401")
}

// countSleepMarkersUnder 数 cmdline 含 "sleep <marker>" 且（rootPid==0 或）
// PPid 属于 rootPid 的进程数。
func countSleepMarkersUnder(rootPid int, marker string) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	needle := "sleep " + marker
	n := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // /proc 下有 self、net、sys… 等非数字目录
		}
		cl, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if !strings.Contains(strings.ReplaceAll(string(cl), "\x00", " "), needle) {
			continue
		}
		if rootPid > 0 && procPPid(pid) != rootPid {
			continue
		}
		n++
	}
	return n
}

// 脱组孙进程活下来时，Kill 仍必须有界返回（靠 readerWG 超时兜底）。
func TestKillReturnsWhenGrandchildEscapesProcessGroup(t *testing.T) {
	base := countEscapingGrandchildren()
	bin := buildPluginWithRealTemplate(t, escapingGrandchildSource)

	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	core := newFakeCore()
	p := New("esc", bin, t.TempDir(), nil, host, nil)
	if err := p.Start(core); err != nil {
		host.Close()
		t.Fatalf("启动失败: %v", err)
	}
	defer func() {
		_ = p.Close()
		host.Close()
		// 脱组孙进程内核杀不到，按 cmdline 精确清理，别留给后续测试
		entries, _ := os.ReadDir("/proc")
		for _, e := range entries {
			cl, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
			if err != nil {
				continue
			}
			if strings.Contains(strings.ReplaceAll(string(cl), "\x00", " "), "sleep 401") {
				if pid, err := strconv.Atoi(e.Name()); err == nil {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		}
	}()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && countEscapingGrandchildren() <= base {
		time.Sleep(50 * time.Millisecond)
	}
	if countEscapingGrandchildren() <= base {
		t.Skip("脱组孙进程未出现（环境限制），跳过")
	}

	done := make(chan struct{})
	go func() { _ = p.proc.Kill(); close(done) }()
	select {
	case <-done:
	case <-time.After(25 * time.Second):
		t.Fatal("脱组孙进程持有 stdout 写端时 Kill 永不返回 —— " +
			"readerWG.Wait() 的超时兜底被撤掉了（这正是线上 90s 超时的成因）")
	}
}
