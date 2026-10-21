package cmd

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// 本文件是**真实工具**的并发压测。
//
// 为什么需要它：阶段 2 的并发判据（core/parallelsched_test.go）用的是
// **假设备** —— 它验证的是"内核会不会并发调度"，但没有验证
// **真实插件工具在真并发下是否安全**。而这正是 ParallelSafe 声明的风险面：
// 声明错一个工具，1000 个并发调用会同时打进去。
//
// 判据全是**不变量**，不写性能阈值（阈值会随机器波动，变成"红/绿随运气"
// 的假信号）。
//
// 跑法：go test ./internal/plugins/cmd/ -run TestStress -timeout 600s
//      -short 时跳过。

// runOnce 直接调 cmd_run 的 handler，返回其 stdout。
//
// ⚠️ cmd_run 的真实返回是 **map[string]interface{}**（含 status/stdout/
// exit_code/command），**不是 string**。我第一版按 string 断言，导致
// 1000 次全判"输出为空"—— 那是判据写错，不是工具串扰。
// 既有测试（TestCmdRunEcho）同样用 json.Marshal 取值，与此一致。
func runOnce(h sdk.ToolHandler, i int) (string, error) {
	res, err := h(map[string]interface{}{
		"command": fmt.Sprintf("echo seq%d", i),
	})
	if err != nil {
		return "", err
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("cmd_run 返回类型不是 map：%T", res)
	}
	if e, hasErr := m["error"]; hasErr {
		return "", fmt.Errorf("cmd_run 报错：%v", e)
	}
	s, _ := m["stdout"].(string)
	return s, nil
}

// TestStress_RealTool1000Concurrent 真实 cmd_run 1000 并发。
//
// 判据：
//  1. 1000 次全部成功，**零错误**（真并发下最常见的失败是共享状态竞争）
//  2. 每次输出**各不相同**且与自己的入参对应 —— 若 handler 有共享 buffer
//     竞争，输出会串（这是"执行体不安全"最典型的症状）
//  3. 耗时不应随并发数线性恶化到不可用（只做宽松上界，不做精确基准）
//  4. -race 无竞态
func TestStress_RealTool1000Concurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test; run with -run TestStress")
	}
	const n = 1000

	// ⚠️ 用**既有**的 setupPlugin（plugin_test.go 里的真实装配），
	// 不另造一套 —— 压测必须跑在真实注册路径上，否则测的是替身。
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatalf("装配插件失败: %v", err)
	}
	h, ok := tc.handlers["cmd_run"]
	if !ok {
		t.Fatal("取不到 cmd_run 的 handler")
	}
	// 顺带确认它真的声明了并发安全（否则 1000 并发会被内核整批串行，
	// 本用例就测不到真并发）
	if !tc.defs["cmd_run"].ParallelSafe {
		t.Fatal("cmd_run 未声明 ParallelSafe —— 内核会整批串行，本压测失去意义")
	}

	// 预热：首次调用会加载配置/建目录，不计入压测
	if _, err := runOnce(h, -1); err != nil {
		t.Fatalf("预热失败: %v", err)
	}

	// 每个 goroutine 只写自己那个下标（无共享变量），故无需加锁 ——
	// 这也是检查项 map_write_in_goroutine 想确认的：按索引分槽写是所有权清晰。
	results := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	var inFlight, maxInFlight int32

	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// 记录并发峰值：证明确实并发了（否则本用例测不到并发）
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				old := atomic.LoadInt32(&maxInFlight)
				if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
					break
				}
			}
			results[idx], errs[idx] = runOnce(h, idx)
			atomic.AddInt32(&inFlight, -1)
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	// ① 零错误
	fails := 0
	for i, e := range errs {
		if e != nil {
			if fails < 3 {
				t.Errorf("第 %d 次并发调用失败: %v", i, e)
			}
			fails++
		}
	}
	if fails > 0 {
		t.Errorf("共 %d/%d 次失败", fails, n)
	}

	// ② 输出与入参一一对应（无串扰）
	mismatched := 0
	for i := 0; i < n; i++ {
		want := fmt.Sprintf("seq%d", i)
		if errs[i] != nil {
			continue
		}
		if !containsStr(results[i], want) {
			if mismatched < 3 {
				t.Errorf("第 %d 次输出不含自己的标记 %q：%q（串扰？）", i, want,
					truncate(results[i], 80))
			}
			mismatched++
		}
	}
	if mismatched > 0 {
		t.Errorf("共 %d 次输出与入参不对应（共享状态竞争）", mismatched)
	}

	peak := atomic.LoadInt32(&maxInFlight)
	t.Logf("1000 并发真实 cmd_run：耗时 %v，并发峰值 %d，失败 %d", elapsed, peak, fails)
	if peak < 10 {
		t.Errorf("并发峰值仅 %d —— 可能被串行化了，本用例测不到真并发", peak)
	}
}

// TestStress_RealToolSerialVsConcurrent 串行 vs 并发的耗时对比。
//
// 只做**观察性**记录（不做通过判据）：机器差异太大，阈值无意义。
// 它的价值在于：如果并发比串行**慢很多**，说明 handler 内部有锁竞争
// 或资源争抢，值得深挖。
func TestStress_RealToolSerialVsConcurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test; run with -run TestStress")
	}
	const n = 200

	// ⚠️ 用**既有**的 setupPlugin（plugin_test.go 里的真实装配），
	// 不另造一套 —— 压测必须跑在真实注册路径上，否则测的是替身。
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatalf("装配插件失败: %v", err)
	}
	h, ok := tc.handlers["cmd_run"]
	if !ok {
		t.Fatal("取不到 cmd_run 的 handler")
	}
	// 顺带确认它真的声明了并发安全（否则 1000 并发会被内核整批串行，
	// 本用例就测不到真并发）
	if !tc.defs["cmd_run"].ParallelSafe {
		t.Fatal("cmd_run 未声明 ParallelSafe —— 内核会整批串行，本压测失去意义")
	}
	if _, err := runOnce(h, -1); err != nil {
		t.Fatalf("预热失败: %v", err)
	}

	// 串行
	t0 := time.Now()
	for i := 0; i < n; i++ {
		if _, err := runOnce(h, i); err != nil {
			t.Fatalf("串行第 %d 次失败: %v", i, err)
		}
	}
	dSerial := time.Since(t0)

	// 并发（2 并发：轻并发，便于对比是否有争抢）
	t1 := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if _, err := runOnce(h, idx); err != nil {
				t.Errorf("并发第 %d 次失败: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()
	dConc := time.Since(t1)

	t.Logf("%d 次：串行 %v（%v/次），高并发 %v（%v/次）",
		n, dSerial, dSerial/n, dConc, dConc/n)
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
