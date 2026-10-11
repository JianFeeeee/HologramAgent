//go:build linux

package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// guard 的 **worker 生命周期**压力测试：spawnWorkerOnce 的退出码语义。
//
// 这块此前零覆盖，而它是 guard 状态机的输入 —— 退出码判错会让
// 「恢复成功」被当成「崩溃」，反之则会让真正的崩溃被当成正常重启。
// 后者更危险：failures 永不累加 ⇒ 永不进 failback ⇒ agent 反复崩溃却没人管。
//
// 这里用真实的子进程（/bin/sh 脚本）模拟各种退出情形，
// 而不是 mock —— 因为退出码→workerOutcome 的映射涉及 exec 包的真实行为，
// mock 掉就测不到「非零退出码能否被正确分类」。

// fakeExe 生成一个可执行脚本：按给定方式退出。
func fakeExe(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testCfg(t *testing.T, dir string) *guardConfig {
	t.Helper()
	cfg := loadGuardConfig(dir)
	// 加速：心跳检查间隔压到 50ms（默认 5s，测试等不起）
	cfg.heartbeatInterval = 50 * time.Millisecond
	cfg.heartbeatTimeout = 10 * time.Second
	return cfg
}

// TestSpawnWorkerOnceClassifiesExitCodes 钉住退出码 → workerOutcome 的映射。
//
// 每个分支错了都会造成实际故障：
//   - exitRecovered(43) 认成 crash  → 恢复成功了却当失败，failures 累加
//   - exitRecoveryFailed(44) 认成 recovered → 恢复失败了却当成功，**永不再恢复**
//   - exitRestartRequested(42) 认成 crash → 正常重启被计入崩溃次数
func TestSpawnWorkerOnceClassifiesExitCodes(t *testing.T) {
	dataDir := t.TempDir()
	hbPath := filepath.Join(dataDir, "heartbeat")
	sigCh := make(chan os.Signal, 1)
	cfg := testCfg(t, dataDir)

	cases := []struct {
		name   string
		script string
		want   workerOutcome
	}{
		{"正常退出", "#!/bin/sh\nexit 0\n", workerCleanExit},
		{"请求重启", "#!/bin/sh\nexit 42\n", workerRestartRequested},
		{"恢复成功", "#!/bin/sh\nexit 43\n", workerRecovered},
		{"恢复失败", "#!/bin/sh\nexit 44\n", workerCrash},
		{"崩溃(1)", "#!/bin/sh\nexit 1\n", workerCrash},
		{"被信号杀死", "#!/bin/sh\nkill -9 $$\n", workerCrash},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exe := fakeExe(t, "worker.sh", c.script)
			got, err := spawnWorkerOnce(exe, dataDir, "normal", hbPath, cfg, sigCh)
			if err != nil {
				t.Fatalf("spawnWorkerOnce 返回 spawnErr=%v", err)
			}
			if got != c.want {
				t.Errorf("退出码场景 %q → outcome=%v, want %v", c.name, got, c.want)
			}
		})
	}
}

// TestSpawnWorkerOnceCleanExitOnSignal 保证收到 SIGTERM 时 guard 能优雅收尾。
//
// 部署流程（systemctl restart）会发 SIGTERM 给整个 cgroup。
// 若 guard 不把信号转给 worker，worker 会被 SIGKILL 强杀 →
// 正在进行的对话丢失，且留下未落盘的状态。
//
// 注：这里用 `exec sleep` 而不是 `sleep 300` —— 后者会让 sh fork 出子进程，
// guard 的 Signal(SIGTERM) 只发给 sh（它是 cmd.Process），sleep 变孤儿继续跑，
// 每个测试泄漏一个进程（实测会攒出好几个 sleep 300）。
// exec 让 sleep **替换** sh 进程，信号直达，退出干净。
func TestSpawnWorkerOnceCleanExitOnSignal(t *testing.T) {
	dataDir := t.TempDir()
	hbPath := filepath.Join(dataDir, "heartbeat")
	cfg := testCfg(t, dataDir)

	// exec sleep 300：sh 被 sleep 替换，SIGTERM 直达并退出
	exe := fakeExe(t, "sleeper.sh", "#!/bin/sh\nexec sleep 300\n")

	sigCh := make(chan os.Signal, 1)
	result := make(chan workerOutcome, 1)
	go func() {
		got, err := spawnWorkerOnce(exe, dataDir, "normal", hbPath, cfg, sigCh)
		if err != nil {
			t.Errorf("spawnErr=%v", err)
		}
		result <- got
	}()

	// 等 worker 确实起来了再发信号
	time.Sleep(300 * time.Millisecond)
	sigCh <- syscall.SIGTERM

	select {
	case got := <-result:
		if got != workerCleanExit {
			t.Errorf("收到信号后 outcome=%v, want workerCleanExit", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("guard 收到信号后没有在 15s 内返回（信号未转发给 worker？）")
	}

	// 确认没有留下孤儿进程（本次修复的核心）
	time.Sleep(200 * time.Millisecond)
	if n := countOrphanSleepers(); n > 0 {
		t.Errorf("遗留了 %d 个 sleep 进程 —— 测试脚本应用 exec 让信号直达", n)
	}
}

// countOrphanSleepers 统计当前系统里的 sleep 进程数（用于检测测试泄漏）。
func countOrphanSleepers() int {
	out, err := exec.Command("sh", "-c", "ps -eo args | grep -c '^sleep 300$'").Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// TestSpawnWorkerOnceFailsOnBadBinary 保证「拉不起来」返回 spawnErr 而非崩溃。
//
// 这条路径会触发 doLastResort —— 而默认的 restart_command 指向
// homeagent 单元，若 systemd 管的正是 guard 就会自杀循环。
// 至少先保证「拉不起来」被正确报告，而不是静默当成 crash 继续循环。
func TestSpawnWorkerOnceFailsOnBadBinary(t *testing.T) {
	dataDir := t.TempDir()
	cfg := testCfg(t, dataDir)
	sigCh := make(chan os.Signal, 1)

	// 不存在的路径
	got, err := spawnWorkerOnce("/nonexistent/homed", dataDir, "normal",
		filepath.Join(dataDir, "heartbeat"), cfg, sigCh)
	if err == nil {
		t.Errorf("二进制不存在时应返回 spawnErr，实际 outcome=%v err=nil", got)
	}
}

// TestSpawnWorkerOnceKillsHungWorker 覆盖「心跳陈旧 → SIGKILL 卡死 worker」。
//
// 压力测试要点：worker **进程还活着**但不响应（模拟死锁/IO 挂起）。
// 只看进程存活会永远等它，必须靠心跳/IPC 判定并杀掉。
func TestSpawnWorkerOnceKillsHungWorker(t *testing.T) {
	if os.Getenv("GUARD_STRESS") == "" {
		t.Skip("压力测试：需要 GUARD_STRESS=1（会真的起进程并杀它）")
	}

	dataDir := t.TempDir()
	hbPath := filepath.Join(dataDir, "heartbeat")
	cfg := testCfg(t, dataDir)
	cfg.heartbeatTimeout = 200 * time.Millisecond

	// worker 起来后不写心跳也不响应 IPC —— 模拟卡死
	exe := fakeExe(t, "hang.sh", "#!/bin/sh\ntrap '' TERM\nwhile true; do sleep 1; done\n")

	// 造一个陈旧心跳（IPC 不通时会回退到文件判定）
	old := time.Now().Add(-time.Hour)
	if err := os.WriteFile(hbPath, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(hbPath, old, old); err != nil {
		t.Fatal(err)
	}

	// 必须在超时内返回：若心跳判定失效，spawnWorkerOnce 会一直等 worker
	// 自然退出（而卡死 worker 永远不会退出）⇒ 测试会挂到 go test 超时。
	// 所以这里给它一个硬上限，超出即判失败并报清楚原因，而不是让整个包卡死。
	type res struct {
		outcome workerOutcome
		err     error
	}
	result := make(chan res, 1)
	go func() {
		got, err := spawnWorkerOnce(exe, dataDir, "normal", hbPath, cfg, make(chan os.Signal, 1))
		result <- res{got, err}
	}()

	start := time.Now()
	select {
	case r := <-result:
		elapsed := time.Since(start)
		if r.err != nil {
			t.Fatalf("spawnErr=%v", r.err)
		}
		if r.outcome != workerCrash {
			t.Errorf("卡死 worker 的 outcome=%v, want workerCrash", r.outcome)
		}
		// 阈值心跳是 200ms，留足余量但仍能抓住「退化成等自然退出」
		if elapsed > 10*time.Second {
			t.Errorf("杀掉卡死 worker 用了 %v，太慢（心跳判定可能失效）", elapsed)
		}
		t.Logf("卡死 worker 在 %v 内被判定并杀掉", elapsed)
	case <-time.After(20 * time.Second):
		// 主动杀掉残留进程，否则它会一直跑到测试二进制退出
		pkillPattern("hang.sh")
		t.Fatal("20s 内未判定卡死 worker —— 心跳/IPC 存活判定失效了")
	}
}

// TestExitCodeConstantsAreDistinct 保证退出码不冲突。
//
// 它们要与 worker_exit.go 里的实际取值一致；一旦有人改了其中之一而
// 另一处没跟上，spawnWorkerOnce 的 switch 会静默走到 default（判成 crash）。
func TestExitCodeConstantsAreDistinct(t *testing.T) {
	all := map[int]string{
		exitRestartRequested: "exitRestartRequested",
		exitRecovered:        "exitRecovered",
		exitRecoveryFailed:   "exitRecoveryFailed",
	}
	if len(all) != 3 {
		t.Fatalf("退出码数量=%d，应为 3（值冲突：%v）", len(all), all)
	}
	seen := map[int]string{}
	for code, name := range all {
		if prev, dup := seen[code]; dup {
			t.Errorf("退出码 %d 同时用于 %s 与 %s —— switch 会先匹配到谁？", code, prev, name)
		}
		seen[code] = name
		if code <= 0 || code > 125 {
			t.Errorf("%s = %d，超出 shell 约定的 1-125 范围", name, code)
		}
	}
}

// TestDetectOwnSystemdUnit 验证能识别出自己所属单元（拿不到时返回空）。
//
// 自指检测的前提是「知道自己是谁」。拿不到时必须返回空字符串，
// 让调用方跳过检测 —— 而不能猜一个单元名（猜错 = 拦错/杀错服务）。
func TestDetectOwnSystemdUnit(t *testing.T) {
	unit := detectOwnSystemdUnit()
	if unit == "" {
		t.Log("未检测到 systemd 单元（非 systemd 环境，跳过）")
		return
	}
	if !strings.HasSuffix(unit, ".service") {
		t.Errorf("检测到单元名 %q，应以 .service 结尾", unit)
	}
	if strings.ContainsAny(unit, "/ \t") {
		t.Errorf("单元名 %q 含非法字符", unit)
	}
	// ★ 必须在 systemd 环境里真的探测到**本进程所属**的单元。
	//
	//   只断言「非空 + 格式合法」会被「返回一个写死的假名字」骗过
	//   （变异测试实测：detectOwnSystemdUnit 直接 return "wrong.service"
	//   时所有测试仍然全绿）。
	//
	//   而「探测错单元」的后果很具体：自指防护会拿错误的 unit 去比对
	//   restart_command，于是「本该拦下的自杀重启」被放行。
	if !strings.Contains(unit, "homeagent") {
		t.Logf("检测到单元 %q —— 本进程应属于 homeagent.service。"+
			"若这是 CI/容器环境则属正常；若是生产则说明探测逻辑错了", unit)
	}
	// 与 cgroup 的事实交叉核对（不依赖 detectOwnSystemdUnit 自己的实现）
	if own, err := ownUnitFromCgroup(); err == nil && own != "" {
		if own != unit {
			t.Errorf("单元探测结果=%q，但 /proc/self/cgroup 显示=%q，两者不一致", unit, own)
		} else {
			t.Logf("单元探测与 cgroup 交叉核对一致: %s", unit)
		}
	}
}

// ownUnitFromCgroup 是独立于 detectOwnSystemdUnit 的第二实现，用于交叉核对。
// （测试里刻意重写一遍，而不是复用生产函数——复用就等于自己验自己。）
func ownUnitFromCgroup() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		idx := strings.Index(line, "::")
		if idx < 0 {
			continue
		}
		p := line[idx+2:]
		j := strings.LastIndex(p, "/")
		if j < 0 {
			continue
		}
		name := strings.TrimSpace(p[j+1:])
		if strings.HasSuffix(name, ".service") {
			return name, nil
		}
	}
	return "", nil
}

// TestSelfRestartDetection 覆盖自指检测的各形态（2026-10-11 加的真实防护）。
func TestSelfRestartDetection(t *testing.T) {
	if !isSelfRestartCommand([]string{"systemctl", "restart", "homeagent"}, "homeagent.service") {
		t.Error("未能识别 systemctl restart homeagent（guard 所在单元）为自指")
	}
	if !isSelfRestartCommand([]string{"systemctl", "restart", "homeagent.service"}, "homeagent.service") {
		t.Error("带 .service 后缀的自指命令未识别")
	}
	if isSelfRestartCommand([]string{"systemctl", "restart", "llmsproxy"}, "homeagent.service") {
		t.Error("误判 systemctl restart llmsproxy 为自指")
	}
	if isSelfRestartCommand([]string{"/usr/bin/my-script"}, "homeagent.service") {
		t.Error("非 systemctl 形态不应判为自指")
	}
	if isSelfRestartCommand(nil, "homeagent.service") {
		t.Error("空命令不应判为自指")
	}
	if isSelfRestartCommand([]string{"systemctl", "restart", "homeagent"}, "") {
		t.Error("单元名未知时不应判为自指（宁可不拦，也不要误杀）")
	}
	if isSelfRestartCommand([]string{"systemctl", "reload", "homeagent"}, "homeagent.service") {
		t.Error("reload 不应被当作 restart 自指")
	}
	if !isSelfRestartCommand([]string{"systemctl", "--user", "restart", "homeagent"}, "homeagent.service") {
		t.Error("带全局选项（--user）的 restart 未识别为自指")
	}
}

// TestGuardExitCodeDoesNotCollideWithWorker 保证 guard 自身退出码不与 worker 协议冲突。
func TestGuardExitCodeDoesNotCollideWithWorker(t *testing.T) {
	guard := exitGuardRestartRequested
	for _, w := range []int{exitRestartRequested, exitRecovered, exitRecoveryFailed} {
		if guard == w {
			t.Errorf("guard 退出码 %d 与 worker 退出码冲突", guard)
		}
	}
	if guard == 0 {
		t.Error("guard 退出码不能是 0（会被当成正常退出）")
	}
}

// pkillPattern 杀掉匹配指定脚本名的残留进程（测试失败时的清理）。
func pkillPattern(name string) {
	_ = exec.Command("pkill", "-f", name).Run()
}

// ══════════════════════════════════════════════════════════════════════
//  L2 自杀防护的**行为**测试（不只测判定函数）
//
//  前面那些只证明 isSelfRestartCommand 能识别自指，
//  但变异测试暴露过一个缺口：把 doLastResortGuarded 改成直接调 doLastRestort
//  （防护完全不生效）时，判定函数的测试仍然全绿。
//
//  所以这里用**真实子进程**验证行为：真的执行 doLastResortGuarded，
//  看它到底会不会执行 restart_command。
//
//  手法：把 RestartCommand 换成一个会写标记文件的命令。
//    - 非自指 → 应执行该命令 → 标记文件出现
//    - 自指   → 应改为 os.Exit(41) → 标记文件**不出现** + 退出码 41
//  （用一个带 --self-exit 开关的测试子进程来跑，避免真的退出测试进程）
// ══════════════════════════════════════════════════════════════════════

// l2Sep 是把 restart_command 各参数传给子进程的分隔符。
//
// ★ 为何不用 "\x00"：POSIX 环境变量里不能含 NUL 字节，
//
//	exec 会直接报 "environment variable contains NUL"（实测踩过）。
//	用 \x1f（单元分隔符）—— 命令参数里几乎不会出现，且合法。
const l2Sep = "\x1f"

// TestDoLastResortGuardedExitsOnSelfReference 是本组最关键的判据。
func TestDoLastResortGuardedExitsOnSelfReference(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "restart-command-ran")

	// ★ restart_command 必须保持 `systemctl restart <unit>` 形态：
	//   isSelfRestartCommand 是按「restart 动词 + 其后的单元名」判定的，
	//   换成 `touch <file>` 就根本不是 systemctl 命令，防护按设计不会触发
	//   （那是「命令形态不匹配」，不是「自指」）。
	//   而「执行 touch」又要求它真能产生可观察副作用，所以用
	//   `systemctl restart <unit>` 无法在测试里真的执行（会动真实服务）。
	//   折中：仍用 systemctl 形态的单元名，但把可执行体换成一个
	//   「无害的假 systemctl」——通过 PATH 前置一个临时目录实现。
	cfg := &guardConfig{
		LastResort:     "restart_app",
		RestartCommand: []string{"systemctl", "restart", "homeagent"},
	}
	binDir := fakeSystemctlDir(t, dir, marker)

	// 自指：restart_command 指向 homeagent（guard 自己的单元）
	out, code := runInSubprocess(t, "selfref", dir, cfg, "homeagent.service", binDir)

	// ★ 关键断言：restart_command 绝不能被执行
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("★ 自指时 restart_command 仍被执行了（自杀防护失效）")
	}
	if code != exitGuardRestartRequested {
		t.Errorf("自指时应以退出码 %d 退出（交给 systemd 重新拉起），实为 %d",
			exitGuardRestartRequested, code)
	}
	t.Logf("子进程输出:\n%s", out)
	t.Logf("自指时正确退出（code=%d），restart_command 未被执行", code)
}

// TestDoLastResortGuardedRunsForNonSelfReference 反向判据：
// 非自指时**必须**照常执行 restart_command —— 否则防护把正常恢复也堵死了。
func TestDoLastResortGuardedRunsForNonSelfReference(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "restart-command-ran")

	cfg := &guardConfig{
		LastResort:     "restart_app",
		RestartCommand: []string{"systemctl", "restart", "homeagent"},
	}
	binDir := fakeSystemctlDir(t, dir, marker)

	out, code := runInSubprocess(t, "nonself", dir, cfg, "some-other.service", binDir)

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("非自指时 restart_command 应被执行（防护过度拦截）")
	}
	if code == exitGuardRestartRequested {
		t.Errorf("非自指时不应走 os.Exit 路径（实为 code=%d）", code)
	}
	if strings.Contains(out, "自身单元") {
		t.Errorf("非自指时不应打印自指告警：\n%s", out)
	}
	t.Log("非自指时照常执行 restart-command（防护未误伤）")
}

// runInSubprocess 在子进程里跑 doLastResortGuarded，返回 (合并输出, 退出码)。
func runInSubprocess(t *testing.T, tag, dir string, cfg *guardConfig, unit string, binDir string) (string, int) {
	t.Helper()
	outFile := filepath.Join(dir, "out-"+tag+".txt")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe,
		"-test.run=TestDoLastResortGuardedExitsOnSelfReference",
		"-test.v")
	cmd.Env = append(os.Environ(),
		"GUARD_L2_PROBE=1",
		"L2_PROBE_UNIT="+unit,
		"L2_PROBE_RESTART="+strings.Join(cfg.RestartCommand, l2Sep),
		"L2_PROBE_OUT="+outFile,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	b, _ := cmd.CombinedOutput()
	code := cmd.ProcessState.ExitCode()
	out := string(b)
	if f, err := os.ReadFile(outFile); err == nil {
		out += string(f)
	}
	return out, code
}

// TestMain 在子进程模式下（GUARD_L2_PROBE=1）执行 L2 逻辑后退出，
// 根本不进入任何测试。
//
// ★ 为何必须用 TestMain 而不是「在测试函数开头判断环境变量后 os.Exit」：
//
//	测试函数里 os.Exit 会终止整个测试二进制——父进程想看的退出码根本没机会产生，
//	而且会把「父进程把子进程的重定向输出读回来」这条链路也一并打断。
//	TestMain 才是「子进程不做测试、直接干活并退出」的正确入口。
func TestMain(m *testing.M) {
	if os.Getenv("GUARD_L2_PROBE") != "" {
		runL2ProbeChild()
		return
	}
	os.Exit(m.Run())
}

// runL2ProbeChild 是子进程里的实际执行体（由 TestMain 在 GUARD_L2_PROBE 时调用）。
func runL2ProbeChild() {
	unit := os.Getenv("L2_PROBE_UNIT")
	outFile := os.Getenv("L2_PROBE_OUT")
	restart := os.Getenv("L2_PROBE_RESTART")
	if restart == "" || outFile == "" {
		os.Exit(90)
	}
	cfg := &guardConfig{
		LastResort:     "restart_app",
		RestartCommand: strings.Split(restart, l2Sep),
	}

	// log 输出重定向到文件，便于父进程断言
	if f, err := os.Create(outFile); err == nil {
		old := os.Stdout
		os.Stdout = f
		defer func() { os.Stdout = old; f.Close() }()
		log.SetOutput(f)
	}

	doLastResortGuarded(cfg, unit)
	// 走到这里说明没有 os.Exit —— 记录一下
	os.Exit(0)
}

// fakeSystemctlDir 造一个「假 systemctl」放到临时 PATH 最前面：
// 它被调用时写出 marker 文件，然后正常退出（exit 0）。
//
// ★ 为何需要它：L2 自指防护的判定依据是 restart_command 里
//
//	「restart 动词 + 单元名」这两个形态特征（isSelfRestartCommand）。
//	若测试里用 `touch <marker>` 之类，命令形态根本不匹配 systemctl，
//	防护按设计不会触发 —— 测的就不是自指防护了。
//	而真跑 `systemctl restart homeagent` 会**真的重启生产服务**。
//	故用 PATH 前置一个假 systemctl：形态对、副作用可观察、绝不动真服务。
func fakeSystemctlDir(t *testing.T, dir, marker string) string {
	t.Helper()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ntouch \"" + marker + "\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "systemctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return binDir
}
