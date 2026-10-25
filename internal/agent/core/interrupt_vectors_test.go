package core

// 中断向量表的判据。
//
// 向量表的价值在于"规则随条目声明，不再靠调用方自律"。但这只有在
// **表本身正确**、且**调用点真的走表**时才成立。所以这里钉三件事：
//
//	V1  表内每条的 ID 与键一致、必备字段齐全、级别是内核级
//	V2  panic 的递归保护真的生效（这是原先硬编码、最容易漏的规则）
//	V3  三个内核中断源确实走向量表，而不是绕过它直接调 raiseKernelInterrupt
//
// V3 是最关键的：若某天有人图省事又加一个裸调用点，向量表就退化成
// 一份"看起来完整、实际没人用"的文档——那种表格比没有更糟。

import (
	"strings"
	"testing"
)

// ── V1：表结构完整性 ──

func TestVectors_TableIsWellFormed(t *testing.T) {
	if len(interruptVectors) == 0 {
		t.Fatal("向量表为空——三个内核中断源必须登记")
	}
	for key, v := range interruptVectors {
		if v.ID != key {
			t.Errorf("向量 %q 的 ID 字段是 %q，与键不一致（手滑会让人查错条目）", key, v.ID)
		}
		if v.Source == nil {
			t.Errorf("向量 %q 缺 Source（来源标识将为空，日志无法定位）", key)
		}
		if v.Build == nil {
			t.Errorf("向量 %q 缺 Build（无法构造中断正文）", key)
		}
		// Guard 允许为 nil（表示无条件产生），但显式 nil 应该是少数。
		if v.Level != LevelCritical {
			t.Errorf("向量 %q 的级别是 %d，内核中断源必须是 L4（LevelCritical）", key, int(v.Level))
		}
	}
	// 反向：三个源必须都在表里（防漏登记）
	for _, id := range []VectorID{VectorTaskPanic, VectorContextOverflow, VectorChildContextFull} {
		if _, ok := interruptVectors[id]; !ok {
			t.Errorf("中断源 %q 未登记进向量表", id)
		}
	}
}

// 未登记的 ID 必须被识别出来（不静默降级成默认级别）。
func TestVectors_UnknownIDIsRejected(t *testing.T) {
	if _, ok := lookupVector(VectorID("no_such_vector")); ok {
		t.Fatal("未登记的 ID 不应被 lookupVector 认领")
	}
}

// 未登记时 raiseVector 必须**响亮失败**，而不是静默发一个没人认领的中断。
func TestVectors_RaiseUnknownPanics(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("raiseVector 未登记 ID 时必须 panic（宁可响亮失败，不要静默发出无人认领的中断）")
		}
	}()
	a.raiseVector(VectorID("no_such_vector"), vectorArgs{})
}

// ── V2：panic 的递归保护真的生效 ──

// 这是向量化最实质的收益：原先硬编码在 reportTaskPanic 里的规则。
// L4 任务自己 panic 时不得再产生 L4——否则同一个 panic 自我放大成中断风暴。
func TestVectors_PanicGuardBlocksRecursiveL4(t *testing.T) {
	v, _ := lookupVector(VectorTaskPanic)
	if v.Guard == nil {
		t.Fatal("panic 向量必须有 Guard（递归保护）")
	}

	l4 := &Task{Class: TaskInterrupt, Level: LevelCritical}
	if v.Guard(vectorArgs{Task: l4, Panic: "boom"}) {
		t.Fatal("L4 任务 panic 时不得再产生 L4（会自我放大成中断风暴）")
	}

	// 正常任务必须放行——否则上一条可能是"永远拒绝"的假绿。
	normal := &Task{Class: TaskQueued}
	if !v.Guard(vectorArgs{Task: normal, Panic: "boom"}) {
		t.Fatal("普通任务 panic 必须放行")
	}
	// L3 中断任务放行（它自己不是 L4，不会造成 L4 自我放大）。
	l3 := &Task{Class: TaskInterrupt, Level: LevelInteractive}
	if !v.Guard(vectorArgs{Task: l3, Panic: "boom"}) {
		t.Fatal("L3 中断任务 panic 必须放行")
	}
	// nil 任务不得 panic（防御）
	if v.Guard(vectorArgs{Task: nil}) {
		t.Fatal("nil 任务不应产生 panic 中断")
	}
}

// ── V3：调用点确实走向量表 ──

// 向量化若只建表、不改调用点，就是"看起来完整、实际没人用"的文档。
// 这里用行为验证：触发一次 panic 中断，检查它带着向量表声明的 source。
func TestVectors_PanicSourceComesFromTable(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	if a.sched == nil {
		t.Skip("未启用调度器")
	}

	v, _ := lookupVector(VectorTaskPanic)
	want := v.Source(vectorArgs{})

	// 直接构造中断任务并登记（绕过需运行中任务的抢占判定）。
	a.reportTaskPanic(&Task{ID: 42, Class: TaskQueued}, "boom")

	snap := a.DumpScheduler()
	var got string
	if snap.Immediate != nil && snap.Immediate.Event != nil {
		got = snap.Immediate.Event.Source
	} else {
		for _, q := range snap.InterruptQueues {
			for _, it := range q {
				if it.Event != nil {
					got = it.Event.Source
				}
			}
		}
	}
	if got != want {
		t.Fatalf("panic 中断的 source=%q，期望向量表声明的 %q（说明调用点没走向量表）", got, want)
	}
}

// ★ 超页中断必须是**空中断**：不携带任何正文（2026-10-25 按原始设计修正）。
//
// 这条判据推翻的是更早的一条（原本断言正文非空、含"已裁剪 N 条"、
// 含"查不到不等于不存在"）。那个写法把 L4 变成了"裁剪已完成"的回执：
// 裁剪在 raise 之前就做完了，中断只剩下通知作用。
//
// 原始设计：L4 空中断把任务抢占下来 ⇒ 任务边界处做语义相关性裁剪。
// 中断是**触发源**，裁剪是它引发的动作。带正文会让模型误以为那是
// 一条需要回应的用户消息（进而多跑一整轮推理）。
//
// 变异验证：把 Build 改回返回非空文本 ⇒ 本判据必须变红。
func TestVectors_OverflowIsAirInterrupt(t *testing.T) {
	v, ok := lookupVector(VectorContextOverflow)
	if !ok {
		t.Fatal("超页向量必须已登记")
	}
	if v.Build == nil {
		t.Fatal("超页向量的 Build 不能为 nil（lookupVector 契约要求存在）")
	}
	body := v.Build(vectorArgs{})
	if body != "" {
		t.Fatalf("超页中断必须是空中断（正文为空），实际：%q\n"+
			"带正文会让它变成一条要模型回应的消息，进而多烧一整轮推理", body)
	}
	// 级别仍必须是 L4：抢占能力来自级别，与"有没有正文"无关。
	if v.Level != LevelCritical {
		t.Fatalf("超页中断必须是 L4，实际 level=%v", v.Level)
	}
}

// 驻留子中断的来源必须带子标识（父要据此定位是哪个子），正文必须给出三种处置动作。
func TestVectors_ChildVectorCarriesIdentity(t *testing.T) {
	v, _ := lookupVector(VectorChildContextFull)
	if got := v.Source(vectorArgs{ChildID: "kid-1"}); got != "child/kid-1" {
		t.Fatalf("驻留子中断来源应为 child/kid-1，实际 %q", got)
	}
	body := v.Build(vectorArgs{ChildID: "kid-1"})
	for _, want := range []string{"kid-1", "压缩", "回收", "销毁"} {
		if !strings.Contains(body, want) {
			t.Fatalf("驻留子中断正文缺 %q（父需要完整的处置选项）：%q", want, body)
		}
	}
}
