package kernelboot

// 伪关机 / 唤醒（迁移窗口）的判据（2026-10-17）。
//
// ## 背景
//
// 用户要求：「迁移模块要先让 core 暂停其他模块运行（伪关机）完成迁移后再唤醒」。
//
// 数据目录迁移 21G 要几分钟，期间任何写者（distiller 每 10min 写 graph.db、
// textmem 滚动写、插件写 plugin_data）都会污染数据。
//
// ## 这组判据守什么
//
//  1. Quiesce 必须**逆序**暂停（后启动的先停：先停消费者再停生产者）
//  2. Pause 失败必须**回滚**（不能留半关状态）
//  3. Resume 必须**逆序**恢复（与暂停相反 = 原启动顺序）
//  4. End 必须**幂等**（重复调用不重复 Resume）
//  5. **失败也必须唤醒**（否则内核永久冻结，比不迁移更糟）

import (
	"errors"
	"sync"
	"testing"
)

func newTestKS() *KernelState { return NewKernelState() }

// ★★★ 判据：Quiesce 逆序暂停、Resume 逆序恢复。
func TestQuiescePausesInReverseOrder(t *testing.T) {
	ks := newTestKS()
	var mu sync.Mutex
	var pausedOrder, resumedOrder []string

	add := func(name string) {
		ks.AddQuiescer(name,
			func() error {
				mu.Lock()
				pausedOrder = append(pausedOrder, name)
				mu.Unlock()
				return nil
			},
			func() error {
				mu.Lock()
				resumedOrder = append(resumedOrder, name)
				mu.Unlock()
				return nil
			})
	}
	// 模拟启动顺序：a → b → c
	add("a")
	add("b")
	add("c")

	paused, err := ks.Quiesce()
	if err != nil {
		t.Fatal(err)
	}
	// 暂停必须逆序：c, b, a（先停最后启动的）
	want := []string{"c", "b", "a"}
	if len(pausedOrder) != 3 || pausedOrder[0] != "c" || pausedOrder[2] != "a" {
		t.Fatalf("★ Quiesce 顺序错：实际 %v，应为 %v（后启动的先停，先停消费者）",
			pausedOrder, want)
	}

	if err := ks.Resume(paused); err != nil {
		t.Fatal(err)
	}
	// 恢复必须与暂停相反 = 原启动顺序 a, b, c
	if len(resumedOrder) != 3 || resumedOrder[0] != "a" || resumedOrder[2] != "c" {
		t.Fatalf("★ Resume 顺序错：实际 %v，应为 [a b c]（与暂停相反 = 启动顺序）", resumedOrder)
	}
}

// ★★★ 判据：Pause 失败必须**回滚已暂停的**（不留半关状态）。
func TestQuiesceRollsBackOnFailure(t *testing.T) {
	ks := newTestKS()
	var mu sync.Mutex
	resumed := map[string]int{}

	ks.AddQuiescer("ok1", func() error { return nil }, func() error {
		mu.Lock()
		resumed["ok1"]++
		mu.Unlock()
		return nil
	})
	ks.AddQuiescer("boom", func() error { return errors.New("停不下来") }, func() error {
		mu.Lock()
		resumed["boom"]++
		mu.Unlock()
		return nil
	})

	// 逆序：boom 先被暂停 ⇒ 它失败 ⇒ ok1 还没被暂停（因为逆序时 ok1 在 boom 之后）
	// 为了真正测到"回滚"，让失败发生在**后面的**那个
	ks2 := newTestKS()
	ks2.AddQuiescer("boom", func() error { return errors.New("停不下来") }, nil)
	ks2.AddQuiescer("ok1", func() error { return nil }, func() error {
		mu.Lock()
		resumed["ok1"]++
		mu.Unlock()
		return nil
	})
	// 逆序暂停：ok1 先（成功）→ boom（失败）⇒ 应回滚 ok1
	if _, err := ks2.Quiesce(); err == nil {
		t.Fatal("Pause 失败时 Quiesce 必须返回错误（不能假装成功）")
	}
	if resumed["ok1"] == 0 {
		t.Fatal("★ Pause 失败后未回滚已暂停的 ok1 ⇒ 内核处于**半关**状态")
	}
}

// ★★★ 判据：End 幂等（重复调用不重复 Resume）。
func TestMigrateSessionEndIsIdempotent(t *testing.T) {
	ks := newTestKS()
	var mu sync.Mutex
	n := 0
	ks.AddQuiescer("w", func() error { return nil }, func() error {
		mu.Lock()
		n++
		mu.Unlock()
		return nil
	})

	sess, err := ks.BeginMigration(nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.End()
	sess.End()
	sess.End()

	mu.Lock()
	defer mu.Unlock()
	if n != 1 {
		t.Fatalf("★ End 不幂等：Resume 被调用 %d 次（应为 1）", n)
	}
}

// fakePlugins 模拟插件层的运行态停启（**可逆**，与 StopAll 相反）。
type fakePlugins struct {
	mu      sync.Mutex
	running []string
	stops   int
	starts  int
	failOn  string // 停这个插件时失败
}

func (f *fakePlugins) StopPluginRuntime(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name == f.failOn {
		return errors.New("停不下来")
	}
	f.stops++
	for i, n := range f.running {
		if n == name {
			f.running = append(f.running[:i], f.running[i+1:]...)
			break
		}
	}
	return nil
}

func (f *fakePlugins) StartPluginRuntime(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.running = append(f.running, name)
	return nil
}

func (f *fakePlugins) RunningPlugins() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.running))
	copy(out, f.running)
	return out
}

// ★★★ 判据：伪关机停掉全部插件，唤醒后**全部恢复**（可逆）。
//
// 这是"用运行态停启而不是 StopAll"的核心保证：
// StopAll 置 shuttingDown 之后插件**永久起不来**，用于伪关机就是关了就死。
func TestMigrationStopsAndRestoresAllPlugins(t *testing.T) {
	ks := newTestKS()
	fp := &fakePlugins{running: []string{"webui", "rss", "calendar", "commondev"}}

	sess, err := ks.BeginMigration(fp)
	if err != nil {
		t.Fatal(err)
	}
	if got := fp.RunningPlugins(); len(got) != 0 {
		t.Fatalf("★ 伪关机后仍有插件在运行: %v（迁移期间会写数据）", got)
	}
	if fp.stops != 4 {
		t.Fatalf("应停 4 个插件，实际 %d", fp.stops)
	}

	sess.End()
	if got := fp.RunningPlugins(); len(got) != 4 {
		t.Fatalf("★ 唤醒后插件未全部恢复: %v（应 4 个）—— 这正是用 StopAll 会造成的后果", got)
	}
}

// ★★★ 判据：停插件失败必须**回滚已停的**（不留半关）。
func TestMigrationRollsBackPluginsOnFailure(t *testing.T) {
	ks := newTestKS()
	// 逆序停：commondev → calendar → rss → webui
	// 让 calendar 失败 ⇒ 已停的 commondev 必须被拉回
	fp := &fakePlugins{running: []string{"webui", "rss", "calendar", "commondev"}, failOn: "calendar"}

	if _, err := ks.BeginMigration(fp); err == nil {
		t.Fatal("停插件失败时 BeginMigration 必须返回错误")
	}
	got := fp.RunningPlugins()
	if len(got) != 4 {
		t.Fatalf("★ 回滚不完整：只剩 %v（应全部 4 个）—— 内核处于半关状态", got)
	}
}
