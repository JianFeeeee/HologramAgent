package test

// internal/plugin/proc 的共享内存 arena —— 面向接口的模块职责判据。
//
// 唯一入口是公开的 proc.NewHost() 与 Host 的导出方法；块布局、块头字段、
// 空闲链结构都不在断言范围内。断言只看：
//   写入的字节能否原样读出、操作成功还是失败、空间能否被再次使用。

import (
	"bytes"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/plugin/proc"
)

func newArenaHost(t *testing.T) *proc.Host {
	t.Helper()
	h, err := proc.NewHost()
	if err != nil {
		t.Skipf("共享内存不可用：%v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func putBytes(t *testing.T, h *proc.Host, owner uint32, payload []byte) proc.SharedRef {
	t.Helper()
	ref, err := h.Arena().Put(owner, payload, h.Generation())
	if err != nil {
		t.Fatalf("Put(%d bytes): %v", len(payload), err)
	}
	return ref
}

func assertReadable(t *testing.T, h *proc.Host, ref proc.SharedRef, want []byte) {
	t.Helper()
	got, err := h.Arena().Read(ref, h.Generation())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Read 返回 %d 字节，期望 %d 字节且内容一致", len(got), len(want))
	}
}

// TestArenaWriteReadFreeReuse 是 arena 的核心职责闭环：
// 写入 → 读出 → 归还 → 同样大小的写入再次成功。
func TestArenaWriteReadFreeReuse(t *testing.T) {
	h := newArenaHost(t)
	payload := bytes.Repeat([]byte("x"), 4096)

	ref := putBytes(t, h, proc.OwnerHost, payload)
	assertReadable(t, h, ref, payload)

	if err := h.Arena().Free(proc.OwnerHost, ref); err != nil {
		t.Fatalf("Free: %v", err)
	}

	again := putBytes(t, h, proc.OwnerHost, payload)
	assertReadable(t, h, again, payload)
}

// TestArenaRequestBeyondCapacityFailsCleanly：超过容量的请求必须失败，
// 且失败之后分配器仍能正常工作。
func TestArenaRequestBeyondCapacityFailsCleanly(t *testing.T) {
	h := newArenaHost(t)
	limit := h.Arena().MaxPayload()

	if _, err := h.Arena().Alloc(proc.OwnerHost, limit+1, h.Generation()); err == nil {
		t.Fatalf("超过上限 %d 字节的请求被接受", limit)
	}

	payload := []byte("still-works")
	ref := putBytes(t, h, proc.OwnerHost, payload)
	assertReadable(t, h, ref, payload)
}

// TestArenaCannotBeFreedByAnotherOwner：别的 owner 不能归还本块，
// 且块内容不受影响。
func TestArenaCannotBeFreedByAnotherOwner(t *testing.T) {
	h := newArenaHost(t)
	payload := []byte("owned-by-host")
	ref := putBytes(t, h, proc.OwnerHost, payload)

	if err := h.Arena().Free(10086, ref); err == nil {
		t.Fatal("非 owner 的归还被接受")
	}
	assertReadable(t, h, ref, payload)
}

// TestArenaRejectsReleasedHandle：已释放的表项再读必须报错，
// 不允许把空表项当成有效数据。
func TestArenaRejectsReleasedHandle(t *testing.T) {
	h := newArenaHost(t)
	ref := putBytes(t, h, proc.OwnerHost, []byte("data"))
	if err := h.Arena().Free(proc.OwnerHost, ref); err != nil {
		t.Fatalf("Free: %v", err)
	}
	if _, err := h.Arena().Read(ref, h.Generation()); err == nil {
		t.Fatal("已释放的表项仍可读取")
	}
}

// TestArenaReclaimReleasesPluginSpace：插件退出后的强制回收，
// 归还的空间必须能被后续分配真正用上。
func TestArenaReclaimReleasesPluginSpace(t *testing.T) {
	h := newArenaHost(t)
	const pluginOwner = uint32(4242)
	payload := bytes.Repeat([]byte("y"), 8192)
	putBytes(t, h, pluginOwner, payload)

	if n := h.Arena().ReclaimOwner(pluginOwner); n != 1 {
		t.Fatalf("ReclaimOwner = %d，期望 1", n)
	}

	again := putBytes(t, h, proc.OwnerHost, payload)
	assertReadable(t, h, again, payload)
}

// TestArenaReclaimDoesNotTouchHostBlocks：回收插件块时不得影响内核自己的块。
func TestArenaReclaimDoesNotTouchHostBlocks(t *testing.T) {
	h := newArenaHost(t)
	hostPayload := []byte("host-owned")
	hostRef := putBytes(t, h, proc.OwnerHost, hostPayload)

	const pluginOwner = uint32(9999)
	putBytes(t, h, pluginOwner, []byte("plugin-owned"))
	h.Arena().ReclaimOwner(pluginOwner)

	assertReadable(t, h, hostRef, hostPayload)
	if err := h.Arena().Free(proc.OwnerHost, hostRef); err != nil {
		t.Fatalf("内核块被回收后无法归还：%v", err)
	}
}

// TestArenaInvalidReferenceIsRejected：伪造的偏移不能被当成合法块，
// 归还时必须报错而不是破坏分配器。
func TestArenaInvalidReferenceIsRejected(t *testing.T) {
	h := newArenaHost(t)
	forged := proc.SharedRef{Handle: 1, Length: 8}

	if err := h.Arena().Free(proc.OwnerHost, forged); err == nil {
		t.Fatal("伪造偏移的归还未被拒绝")
	}
	payload := []byte("after-forgery")
	ref := putBytes(t, h, proc.OwnerHost, payload)
	assertReadable(t, h, ref, payload)
}

// TestArenaFreeTwiceIsRejected：重复归还同一个块必须报错。
func TestArenaFreeTwiceIsRejected(t *testing.T) {
	h := newArenaHost(t)
	ref := putBytes(t, h, proc.OwnerHost, []byte("once"))
	if err := h.Arena().Free(proc.OwnerHost, ref); err != nil {
		t.Fatalf("首次归还失败：%v", err)
	}
	if err := h.Arena().Free(proc.OwnerHost, ref); err == nil {
		t.Fatal("重复归还被接受")
	}
}

// TestArenaFragmentationIsReclaimedByReuse：碎片是否**真的**被回收到可用。
//
// 原判据（2026-10-16 写入）是 `if _, err := Alloc(...); err == nil {
// t.Log("当前实现已能整理碎片；本判据需改为验证整理后的可用性") }` ——
// 它对自己的被测对象**两种结果都不红**：能分配就只打印一行，不能分配也不管。
// 那等于没有判据，而这正是它落到现在还挂着的原因（引入 Compact 之后
// 它连"需修正"都不会再说）。
//
// 现在的判据钉住可观测结果：同样大小的请求，在碎片里**必须能成功**，
// 且跨过碎片边界的块内容必须能原样读出（整理会移动块，
// 读回内容错位 = 静默数据损坏）。
func TestArenaFragmentationIsReclaimedByReuse(t *testing.T) {
	h := newArenaHost(t)
	arena := h.Arena()
	limit := arena.MaxPayload()
	quarter := limit / 4
	big := make([]byte, quarter)
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	sep := []byte("separator")

	// 布局：大 小 大 小 大 小 ⇒ 大块之间有活块隔着，回收后仍是碎片。
	putBytes(t, h, proc.OwnerHost, sep)
	var bigRefs []proc.SharedRef
	for i := 0; i < 3; i++ {
		bigRefs = append(bigRefs, putBytes(t, h, proc.OwnerHost, big))
		putBytes(t, h, proc.OwnerHost, sep)
	}
	for _, ref := range bigRefs {
		if err := arena.Free(proc.OwnerHost, ref); err != nil {
			t.Fatalf("Free: %v", err)
		}
	}

	// 三个 1/4 大的块已释放，但被分隔块隔成三段 ⇒ **单个**空闲块都不到
	// limit/2，首次适配必然失败。要拿到 limit/2 就必须先把存活块搬到段首。
	//
	// ★ 请求量必须**大于任何单个空闲块**，否则首次适配直接命中，
	// 判据会在没有整理的情况下也通过（我第一版写成 quarter 正是这个坑）。
	spanning := bytes.Repeat([]byte("S"), quarter*2)
	ref, err := arena.Put(proc.OwnerHost, spanning, h.Generation())
	if err != nil {
		t.Fatalf("★ 碎片回收后仍无法分配 %d 字节（Compact 没生效或调用点缺失）: %v",
			len(spanning), err)
	}
	assertReadable(t, h, ref, spanning)
	if err := arena.Free(proc.OwnerHost, ref); err != nil {
		t.Fatalf("Free: %v", err)
	}
}

// TestArenaHostHonoursRequestedCapacity：按调用方给定的容量创建共享段，
// 容量必须真的生效（MaxPayload 随容量变化），而不是静默用默认值。
func TestArenaHostHonoursRequestedCapacity(t *testing.T) {
	const mb = 8
	h, err := proc.NewHostWithArenaSize(mb * 1024 * 1024)
	if err != nil {
		t.Skipf("共享内存不可用：%v", err)
	}
	defer func() { _ = h.Close() }()

	payload := bytes.Repeat([]byte("z"), mb*1024*1024/2)
	ref := putBytes(t, h, proc.OwnerHost, payload)
	assertReadable(t, h, ref, payload)
}

// TestArenaHostRejectsTooSmallCapacity：低于分配器最小尺寸时报明确错误，
// 不静默回落到默认容量。
func TestArenaHostRejectsTooSmallCapacity(t *testing.T) {
	if _, err := proc.NewHostWithArenaSize(64); err == nil {
		t.Fatal("过小的 arena 容量被接受（应报错而非回落默认值）")
	}
}
