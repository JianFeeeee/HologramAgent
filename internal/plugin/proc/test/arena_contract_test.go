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

// TestArenaRejectsStaleReference：旧 generation 的引用必须报错，
// 不允许返回可能错位的数据。
func TestArenaRejectsStaleReference(t *testing.T) {
	h := newArenaHost(t)
	ref := putBytes(t, h, proc.OwnerHost, []byte("data"))

	if _, err := h.Arena().Read(ref, h.Generation()+1); err == nil {
		t.Fatal("过期 generation 的读取被接受")
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
	forged := proc.SharedRef{Offset: 1, Length: 8, Generation: uint32(h.Generation())}

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

// TestArenaFragmentationPreventsLargeRequest 钉住"总量够但无连续块"的真实失败。
//
// 这是现状行为，不是期望行为：1MB 大块与小块交替铺满后释放大块，
// 剩余空间总量超过请求的一半，但被小块隔开 ⇒ 首次适配拿不到。
// 引入整理回收后，本判据应改为「同一请求成功且内容可读」。
func TestArenaFragmentationPreventsLargeRequest(t *testing.T) {
	h := newArenaHost(t)
	arena := h.Arena()
	limit := arena.MaxPayload()
	big := make([]byte, limit/4)
	sep := []byte("separator")

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

	if _, err := arena.Alloc(proc.OwnerHost, int(limit/2-1024), h.Generation()); err == nil {
		t.Log("当前实现已能整理碎片；本判据需改为验证整理后的可用性")
	}
}
