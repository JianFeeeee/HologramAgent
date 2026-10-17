package security

// 并发压测 —— ★ 本文件存在的理由
//
// 安全模块挂在**热路径**上：
//
//	每次 LLM 调用   → provider 装饰器调 Replace
//	每次工具调用    → StageHost.ExecuteTool 调 FillMap
//	每次上下文写入  → Append 调 Replace
//
// 而内核是**并发**的：多 agent、多任务、调度器并行、插件回调在各自 goroutine。
// 这意味着 Store 的每个方法都会被并发调用。
//
// ★ 初版判据全是**单线程**的，`-race` 通过毫无意义（没有并发就没有竞态可报）。
// 本文件补上真正的并发压力：
//
//	· 同一明文并发 Submit（考验"同别名合并"在竞态下是否仍成立）
//	· 并发 Put 同一条目（考验 Hits++ 是否丢更新/竞态）
//	· 并发 Fill 与 Put 交错（考验读写锁边界）
//	· 并发 Forget 与 Fill（考验"删了但仍在读"）
//	· 并发落盘（考验 tmp 文件 + rename 是否互相踩）

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func stressStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s := New(Options{
		Path:    filepath.Join(dir, "vars.json"),
		KeyFn:   testKey,
		Enabled: true,
	})
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	return s
}

// ★★ 判据：**同一明文并发 Submit ⇒ 仍只有一个条目**（合并语义在竞态下成立）。
//
// 这是 S11（同变量自动合并）在并发下的形态。若合并只在单线程下成立，
// 并行 agent 会让别名表膨胀、且同一密钥在不同上下文里显示成不同的名字。
func TestStress_ConcurrentSubmitSamePlainMergesToOne(t *testing.T) {
	s := stressStore(t)
	const plain = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	const goroutines = 64
	var wg sync.WaitGroup
	var errs atomic.Int64
	aliases := make([]string, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a, err := s.Submit(plain, fmt.Sprintf("g%d", i))
			if err != nil {
				errs.Add(1)
				return
			}
			aliases[i] = a
		}(i)
	}
	wg.Wait()

	if errs.Load() > 0 {
		t.Fatalf("%d 个 goroutine 提交失败", errs.Load())
	}
	// ① 只有一个条目
	if got := s.Count(); got != 1 {
		t.Fatalf("★ 同明文并发提交应合并为 1 条，实际 %d 条 —— "+
			"合并语义在并发下不成立，并行 agent 会让别名表膨胀", got)
	}
	// ② 所有 goroutine 拿到**同一个**别名
	for i, a := range aliases {
		if a != aliases[0] {
			t.Fatalf("★ 第 %d 个 goroutine 拿到不同别名：%s vs %s", i, a, aliases[0])
		}
	}
}

// ★★ 判据：并发 Put 同一条目时 **Hits 不丢更新**。
//
// Go 里 `prev.Hits++` 在持锁区内是安全的，但若有人把锁移出（"优化"），
// 就会静默丢计数 —— 而计数是"这个凭据被用了多少次"的唯一可观测，
// 丢了就再也看不出异常用量。
func TestStress_ConcurrentPutDoesNotLoseHits(t *testing.T) {
	s := stressStore(t)
	const plain = "secret-value-for-hits"
	alias, err := s.Alias(plain)
	if err != nil {
		t.Fatal(err)
	}

	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Put(alias, plain, "submitted", ""); err != nil {
				t.Errorf("Put 失败: %v", err)
			}
		}()
	}
	wg.Wait()

	infos := s.List()
	if len(infos) != 1 {
		t.Fatalf("应只有 1 条，实际 %d", len(infos))
	}
	// 每个 Put 都 +1（首次 Put 建条目时 Hits=1，其余各 +1）
	if infos[0].Hits != n {
		t.Errorf("★ Hits 丢更新：期望 %d，实际 %d", n, infos[0].Hits)
	}
}

// ★★ 判据：**并发 Fill 与 Put 交错**不 panic、不返回坏值。
//
// Fill 走 RLock，Put 走 Lock —— 边界若错（如 Fill 里改了 map），
// race detector 会报，或读到半写状态。
func TestStress_ConcurrentFillAndPut(t *testing.T) {
	s := stressStore(t)
	const plain = "concurrent-fill-put-secret"
	alias, err := s.Submit(plain, "n")
	if err != nil {
		t.Fatal(err)
	}
	ref := "@{" + alias + "}"

	// 写者与读者**分别**用两个 WaitGroup。
	//
	// ★ 初版用一个 wg 同时等两者，读者又等 stop，而 stop 在 <-done 之后
	// 才关 ⇒ **测试自身死锁**（挂了 600s 才发现）。这不是 Store 的问题，
	// 但同样值得记：并发测试的等待结构写错，会把"测试挂了"伪装成"被测对象挂了"。
	var writers, readers sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 8; i++ {
		writers.Add(1)
		go func(i int) {
			defer writers.Done()
			for j := 0; j < 200; j++ {
				if _, err := s.Submit(fmt.Sprintf("other-secret-%d-%d", i, j), ""); err != nil {
					t.Errorf("Submit: %v", err)
					return
				}
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				out, n, _ := s.Fill("x=" + ref)
				if n != 1 {
					t.Errorf("★ 并发下 Fill 失败：n=%d out=%q", n, out)
					return
				}
				if !strings.Contains(out, plain) {
					t.Errorf("★ 并发下 Fill 返回坏值: %q", out)
					return
				}
			}
		}()
	}

	writers.Wait() // 等写者跑完
	close(stop)    // 再停读者
	readers.Wait()
}

// ★★ 判据：**并发 Forget 与 Fill** 不 panic（"删了但仍在读"）。
//
// 这是最容易出事的组合之一：Fill 读到的条目可能刚好被 Forget 删掉。
// 期望：要么填出明文，要么原样返回引用 —— 绝不能 panic 或吐垃圾。
func TestStress_ConcurrentForgetAndFill(t *testing.T) {
	s := stressStore(t)

	var wg sync.WaitGroup
	for round := 0; round < 40; round++ {
		plain := fmt.Sprintf("volatile-secret-%d-aaaaaaaaaaaaaaaa", round)
		alias, err := s.Submit(plain, "")
		if err != nil {
			t.Fatal(err)
		}
		ref := "@{" + alias + "}"

		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _, _ = s.Fill("v=" + ref)
		}()
		go func() {
			defer wg.Done()
			_ = s.Forget(alias)
		}()
	}
	wg.Wait() // 只要不 panic / 不报 race 就算过
}

// ★★ 判据：**并发落盘不损坏文件**（tmp + rename 的原子性）。
//
// 每次 Put 都会写盘。并发写若直接写目标文件，会互相截断 ——
// 而后果是"变量库损坏 ⇒ 拒绝启动"，用户的所有凭据都读不出来。
func TestStress_ConcurrentSaveKeepsFileValid(t *testing.T) {
	s := stressStore(t)

	const n = 120
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Submit(fmt.Sprintf("save-stress-%d-value", i), ""); err != nil {
				t.Errorf("Submit: %v", err)
			}
		}(i)
	}
	wg.Wait()

	// 文件必须能被**重新解析**（模拟重启加载）
	if err := s.saveLocked(); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	s2 := New(Options{Path: s.path, KeyFn: testKey, Enabled: true})
	if err := s2.Load(); err != nil {
		t.Fatalf("★ 并发写后变量库损坏（重启就读不出来）: %v", err)
	}
	if s2.Count() != n {
		t.Errorf("重载后应有 %d 条，实际 %d", n, s2.Count())
	}
	// 权限仍须 0600
	if fi, err := os.Stat(s.path); err == nil {
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("并发写后权限变了: %04o", fi.Mode().Perm())
		}
	}
}

// ★★ 判据：`Replace` 与 `Fill` 并发（出向拦截 vs 入向回填同时发生）。
//
// 真实场景：一个 agent 在出向（扫明文），另一个在入向（填别名）。
func TestStress_ConcurrentReplaceAndFill(t *testing.T) {
	s := stressStore(t)
	const plain = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	var writers, readers sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 6; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for j := 0; j < 100; j++ {
				out, evts := s.Replace("token="+plain, "stress", Patterns)
				if len(evts) == 0 {
					t.Errorf("★ 并发下 Replace 未命中（模式库被改？）")
					return
				}
				if strings.Contains(out, plain) {
					t.Errorf("★ 并发下 Replace 泄露明文: %q", out)
					return
				}
			}
		}()
	}
	readers.Add(1)
	go func() {
		defer readers.Done()
		alias, _ := s.Alias(plain)
		ref := "@{" + alias + "}"
		for {
			select {
			case <-stop:
				return
			default:
			}
			out, n, _ := s.Fill("x=" + ref)
			// n==0 是允许的（Replace 可能还没登记完）；但填出来就必须是明文
			if n == 1 && !strings.Contains(out, plain) {
				t.Errorf("★ 并发下 Fill 返回坏值: %q", out)
				return
			}
		}
	}()

	writers.Wait()
	close(stop)
	readers.Wait()
}
