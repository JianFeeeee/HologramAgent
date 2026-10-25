package proc

import (
	"strings"
	"testing"
)

// 回归判据：内核代插件回收扩容块时必须用**插件自己的 ownerID**。
//
// 病根（2026-10-25 线上）：插件经 arena.alloc 申请扩容块时块头 owner 是
// 它自己的 ownerID，而内核三处归还全写着 Free(OwnerHost, ref)。归属校验
// 必然失败、错误又被 `_ =` 吞掉 ⇒ 每返回一次大结果就永久泄漏一块。
// 4MB 的 arena 被大工具结果（单次 64KB+ 的返回）慢慢吃满后，连内核标定的
// 64KB 调用帧都分不出来，所有插件工具一起报「arena: 空间不足」。
//
// 判据两半：
//  1. Free(OwnerHost, 插件块) 必须失败 —— 确认这条路径本来就是错的
//  2. Free(插件owner, 插件块) 必须成功且 Stats 归零 —— 确认修法真的回收
//
// 变异：把 invokeTool / invokeCleaner / invokeOutput 里任一处改回
// Free(OwnerHost, ...)，E2E 判据立即变红。
func TestArena_ExpandBlockReclaimedWithPluginOwner(t *testing.T) {
	a := newTestArena(t, 1<<20)
	const pluginOwner = 7

	ref, err := a.Alloc(pluginOwner, 100000, 1)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}

	// ① 旧写法必须失败：这就是泄漏机制本身。
	err = a.Free(OwnerHost, ref)
	if err == nil {
		t.Fatal("Free(OwnerHost, 插件块) 竟然成功了——归属校验失效，本判据失去意义")
	}
	if !strings.Contains(err.Error(), "不属于调用者") {
		t.Fatalf("期望归属校验错误，实际: %v", err)
	}
	if used, _ := a.Stats(); used == 0 {
		t.Fatal("旧路径失败后块却被回收了？")
	}

	// ② 正确写法：用插件自己的 ownerID 归还。
	if err := a.Free(pluginOwner, ref); err != nil {
		t.Fatalf("按插件 owner 归还应成功: %v", err)
	}
	if used, _ := a.Stats(); used != 0 {
		t.Fatalf("回收后 arena 应归零，实际 used=%d", used)
	}
}

// 扩容块归还后空间必须可复用：连做 30 轮「申请 200KB → 归还」，
// 总分配量远超 arena 容量，但 used 每轮都应回到 0。
func TestArena_ExpandBlocksDoNotAccumulate(t *testing.T) {
	a := newTestArena(t, 1<<20)
	const owner = 3
	for i := 0; i < 30; i++ {
		ref, err := a.Alloc(owner, 200*1024, 1)
		if err != nil {
			t.Fatalf("第 %d 轮分配失败（累计分配 %dKB 未回收？）: %v", i, i*200, err)
		}
		if err := a.Free(owner, ref); err != nil {
			t.Fatalf("第 %d 轮归还失败: %v", i, err)
		}
		if used, _ := a.Stats(); used != 0 {
			t.Fatalf("第 %d 轮后 used=%d，块未回收", i, used)
		}
	}
}
