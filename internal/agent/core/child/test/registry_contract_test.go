package test

// child.Registry 的契约判据：**输入 → 输出**，不碰内部字段。
//
// ## 为什么这个包必须有判据（2026-11-04）
//
// `internal/agent/core/child` 是本轮重构的核心产物（把 residents / rtChildren
// 统一成一张登记表），419 行，但 CI 覆盖率实测 **0.0%** —— 一个判据都没有。
//
// 而这个包的注释里明确写着它要消灭的缺陷形态：
//
//	StopRTChild（单数）注销入站登记，StopRTChildren（复数）漏了，
//	残留随 create/destroy 单调累积且只能重启清。
//
// 也就是说：它存在的**理由**就是"单数与复数共用同一条清理路径"。
// 那条不变量没有判据，等于这次重构的核心承诺无人守。
//
// ## 判据放在哪里
//
// `child/test/`（黑箱）。理由见 engineering-lessons §5.3.1：
// Go 按目录划分包实例，子目录里写 `package child` 也访问不到未导出符号，
// 所以这个位置**物理上强制**只能调导出接口。

import (
	"context"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/core/child"
)

// fakeEntry 是最小的 Entry 实现，只记录自己被怎么处理过。
//
// 它属于**测试替身**，不是绕过生产路径：生产里 Entry 的实现是 rtChild /
// residentChild，它们对骨架的用法就是"Base 返回公共部分 + Dispose 收尾"。
type fakeEntry struct {
	base       *child.Child
	kind       child.Kind
	disposed   int
	disposeMsg string
}

func newFake(id string, kind child.Kind) *fakeEntry {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeEntry{
		base: &child.Child{ID: id, Ctx: ctx, Cancel: cancel},
		kind: kind,
	}
}

func (f *fakeEntry) Base() *child.Child { return f.base }
func (f *fakeEntry) Kind() child.Kind   { return f.kind }
func (f *fakeEntry) Dispose() string {
	f.disposed++
	return f.disposeMsg
}

// TestAddRejectsDuplicateID 钉住：同名登记必须被拒（不静默覆盖）。
//
// 静默覆盖会让上一代的残留（通道 Owner、主体策略、留档）被当成这一代的 ——
// 这正是 registry.go 里 Add 的注释写明要避免的。
//
// 变异：把 `if _, dup := r.byID[b.ID]; dup { return ErrChildExists }`
// 改成覆盖 ⇒ 本例红。
func TestAddRejectsDuplicateID(t *testing.T) {
	var r child.Registry
	if err := r.Add(newFake("a", "rt")); err != nil {
		t.Fatalf("首次登记应成功: %v", err)
	}
	err := r.Add(newFake("a", "rt"))
	if err == nil {
		t.Fatal("★ 同名重复登记没有被拒 —— 上一代残留会被当成这一代的")
	}
	if r.Len() != 1 {
		t.Fatalf("重复登记后 Len()=%d，期望 1（不许被覆盖或追加）", r.Len())
	}
}

// TestAddRejectsEmptyOrNil 钉住输入校验三条。
//
// 变异：删掉 Add 开头的守卫 ⇒ 本例红（会登记一个 id 为空的子，
// 之后 Get("") 能查到它，清理路径也会拿空 id 去撤销策略）。
func TestAddRejectsEmptyOrNil(t *testing.T) {
	var r child.Registry
	if err := r.Add(nil); err == nil {
		t.Fatal("★ nil Entry 被接受了")
	}
	e := newFake("x", "rt")
	e.base = nil
	if err := r.Add(e); err == nil {
		t.Fatal("★ Base() 为 nil 的 Entry 被接受了")
	}
	if err := r.Add(newFake("", "rt")); err == nil {
		t.Fatal("★ ID 为空的 Entry 被接受了")
	}
	if r.Len() != 0 {
		t.Fatalf("三次非法输入后 Len()=%d，期望 0", r.Len())
	}
}

// TestAddWritesKindAndCreatedAt 钉住输出：Add 会把 Kind 与 CreatedAt 写回 Base。
//
// 这是 Base 的**对外可观测状态**（状态面/排序都读它），属于契约。
func TestAddWritesKindAndCreatedAt(t *testing.T) {
	var r child.Registry
	e := newFake("a", "resident")
	if err := r.Add(e); err != nil {
		t.Fatal(err)
	}
	if e.base.Kind != "resident" {
		t.Fatalf("Base.Kind = %q，期望 resident（Add 应写入 Entry.Kind()）", e.base.Kind)
	}
	if e.base.CreatedAt == 0 {
		t.Fatal("★ Base.CreatedAt 未被写入 —— 状态面排序会失去依据")
	}
}

// TestStopOneAndStopAllShareCleanup 钉住本包存在的**理由**：
// 单数与复数走同一条清理路径，四步副作用一步不少。
//
// 变异：让 StopAll 复制一份循环体、漏掉 UnregisterInbound ⇒ 本例红。
func TestStopOneAndStopAllShareCleanup(t *testing.T) {
	type trace struct {
		disposed  int
		revoked   []string
		unreg     []string
		onDispose int
	}

	run := func(stop func(*child.Registry, child.StopDeps), useAll bool) trace {
		var r child.Registry
		var tr trace
		a := newFake("a", "rt")
		b := newFake("b", "rt")
		a.base.InboundCh = "child/a"
		b.base.InboundCh = "child/b"
		_ = r.Add(a)
		_ = r.Add(b)

		deps := child.StopDeps{
			RevokePolicy:      func(id string) { tr.revoked = append(tr.revoked, id) },
			UnregisterInbound: func(ch string) { tr.unreg = append(tr.unreg, ch) },
			OnDispose:         func(child.Entry) { tr.onDispose++ },
		}
		if useAll {
			if n := r.StopAll("", deps); n != 2 {
				t.Fatalf("StopAll 停了 %d 个，期望 2", n)
			}
		} else {
			for _, id := range []string{"a", "b"} {
				if err := r.StopOne(id, deps); err != nil {
					t.Fatalf("StopOne(%s): %v", id, err)
				}
			}
		}
		tr.disposed = a.disposed + b.disposed
		return tr
	}

	one := run(nil, false)
	all := run(nil, true)

	for _, c := range []struct {
		name     string
		one, all trace
	}{{"stopOne vs stopAll", one, all}} {
		if c.one.disposed != 2 || c.all.disposed != 2 {
			t.Fatalf("[%s] Dispose 次数 %d/%d，各期望 2", c.name, c.one.disposed, c.all.disposed)
		}
		if len(c.one.revoked) != 2 || len(c.all.revoked) != 2 {
			t.Fatalf("[%s] RevokePolicy 次数 %d/%d，各期望 2 ——\n"+
				"主体策略不撤销会让同名重建继承旧策略（engineering-lessons §3.2）",
				c.name, len(c.one.revoked), len(c.all.revoked))
		}
		if len(c.one.unreg) != 2 || len(c.all.unreg) != 2 {
			t.Fatalf("[%s] UnregisterInbound 次数 %d/%d，各期望 2 ——\n"+
				"这就是历史上「单数注销、复数漏了」的缺陷形态",
				c.name, len(c.one.unreg), len(c.all.unreg))
		}
		if c.one.onDispose != 2 || c.all.onDispose != 2 {
			t.Fatalf("[%s] OnDispose 次数 %d/%d，各期望 2", c.name, c.one.onDispose, c.all.onDispose)
		}
	}
}

// TestStopOneUnknownReturnsError 钉住单数语义：不存在时返回错误。
//
// 与 StopAll 的"返回停了多少个"形成对比 —— 两者语义不同是有意的。
func TestStopOneUnknownReturnsError(t *testing.T) {
	var r child.Registry
	if err := r.StopOne("nope", child.StopDeps{}); err == nil {
		t.Fatal("★ StopOne 对不存在的 id 返回了 nil")
	}
}

// TestStopAllFiltersByKind 钉住 StopAll 的过滤语义。
//
// 变异：忽略 kind 参数（总是停全部）⇒ 本例红。
func TestStopAllFiltersByKind(t *testing.T) {
	var r child.Registry
	rt := newFake("rt1", "rt")
	res := newFake("res1", "resident")
	_ = r.Add(rt)
	_ = r.Add(res)

	n := r.StopAll("rt", child.StopDeps{})
	if n != 1 {
		t.Fatalf("StopAll(rt) 停了 %d 个，期望 1", n)
	}
	if rt.disposed != 1 {
		t.Fatal("★ rt 子没有被清理")
	}
	if res.disposed != 0 {
		t.Fatal("★ 驻留子被误清理了 —— StopAll 没按 kind 过滤")
	}
	if r.Len() != 1 {
		t.Fatalf("剩余 %d 个，期望 1（驻留子应还在）", r.Len())
	}
}

// TestRemoveReturnsEntryOnce 钉住：Remove 摘出并返回，第二次得 nil。
//
// 变异：Remove 不删除（只查）⇒ 本例红。
func TestRemoveReturnsEntryOnce(t *testing.T) {
	var r child.Registry
	e := newFake("a", "rt")
	_ = r.Add(e)
	if got := r.Remove("a"); got == nil {
		t.Fatal("★ 首次 Remove 返回 nil")
	}
	if got := r.Remove("a"); got != nil {
		t.Fatal("★ 第二次 Remove 仍返回了条目 —— 没真正删除")
	}
	if r.Len() != 0 {
		t.Fatalf("Len()=%d，期望 0", r.Len())
	}
}

// TestByKindAndIDs 钉住查询视图。
func TestByKindAndIDs(t *testing.T) {
	var r child.Registry
	_ = r.Add(newFake("a", "rt"))
	_ = r.Add(newFake("b", "resident"))
	_ = r.Add(newFake("c", "rt"))

	if n := len(r.ByKind("rt")); n != 2 {
		t.Fatalf("ByKind(rt) 返回 %d 个，期望 2", n)
	}
	ids := r.IDs()
	if len(ids) != 3 {
		t.Fatalf("IDs() 返回 %d 个，期望 3", len(ids))
	}
	for i := 1; i < len(ids); i++ {
		if ids[i-1] > ids[i] {
			t.Fatalf("IDs() 未排序: %v（调用方据此做稳定输出）", ids)
		}
	}
}

// TestChildStopIsIdempotent 钉住 Base().Stop() 幂等且会 cancel ctx。
//
// 变异：Stop 不 cancel ⇒ 本例红（正在跑的轮次不会在安全点退出）。
func TestChildStopIsIdempotent(t *testing.T) {
	e := newFake("a", "rt")
	e.base.Stop()
	e.base.Stop() // 第二次不得 panic
	if !e.base.Stopped() {
		t.Fatal("Stop 后 Stopped() 为 false")
	}
	if e.base.Ctx.Err() == nil {
		t.Fatal("★ Stop 没有 cancel ctx —— 正在跑的轮次不会退出")
	}
}
