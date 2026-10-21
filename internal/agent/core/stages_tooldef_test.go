package core

import (
	"sync"
	"testing"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// 无拷贝查询必须与 ToolDef(...).字段 **完全等价**。
//
// 这类优化最危险的失败模式是"优化悄悄改了语义"：比如并发判据原本
// 读 ParallelSafe，优化后读成了别的字段，于是工具能并发的批次悄悄
// 退化成串行 —— 没有任何报错。
func TestNoCopyQueriesMatchToolDef(t *testing.T) {
	sh := NewStageHost()
	noop := func(map[string]interface{}) (interface{}, error) { return nil, nil }
	cases := []sdk.ToolDef{
		{Name: "plain"},
		{Name: "parallel", ParallelSafe: true},
		{Name: "serial", Serial: true},
		{Name: "both", ParallelSafe: true, Serial: true},
		{Name: "nomem", NoMemory: true},
		{Name: "all", ParallelSafe: true, NoMemory: true},
		{Name: "serial_nomem", Serial: true, NoMemory: true},
	}
	for _, d := range cases {
		if err := sh.RegisterTool(d.Name, d, noop); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range cases {
		def := sh.ToolDef(d.Name)
		if def == nil {
			t.Fatalf("%s: ToolDef 返回 nil", d.Name)
		}
		safe, found := sh.ConcurrencySafeOf(d.Name)
		if !found {
			t.Errorf("%s: ConcurrencySafeOf 未找到", d.Name)
			continue
		}
		if want := def.ParallelSafe && !def.Serial; safe != want {
			t.Errorf("%s: ConcurrencySafeOf=%v，ToolDef 算出的应为 %v（ParallelSafe=%v Serial=%v）",
				d.Name, safe, want, def.ParallelSafe, def.Serial)
		}
		nm, ok := sh.NoMemoryOf(d.Name)
		if !ok {
			t.Errorf("%s: NoMemoryOf 未找到", d.Name)
			continue
		}
		if nm != def.NoMemory {
			t.Errorf("%s: NoMemoryOf=%v，ToolDef.NoMemory=%v", d.Name, nm, def.NoMemory)
		}
		if sh.HasTool(d.Name) != true {
			t.Errorf("%s: HasTool 应为 true", d.Name)
		}
	}
	// 不存在的工具
	if safe, found := sh.ConcurrencySafeOf("nope"); found || safe {
		t.Errorf("不存在的工具：found=%v safe=%v，应为 false/false", found, safe)
	}
	if sh.HasTool("nope") {
		t.Error("HasTool 对不存在的工具返回 true")
	}
}

// 并发查询必须无竞态，且与串行结果一致。
func TestNoCopyQueriesConcurrent(t *testing.T) {
	sh := NewStageHost()
	noop := func(map[string]interface{}) (interface{}, error) { return nil, nil }
	for i := 0; i < 50; i++ {
		name := "t" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		_ = sh.RegisterTool(name, sdk.ToolDef{Name: name, ParallelSafe: i%2 == 0}, noop)
	}
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				name := "t" + string(rune('a'+i%26)) + string(rune('0'+i/26))
				safe, found := sh.ConcurrencySafeOf(name)
				if !found {
					t.Errorf("并发下 %s 未找到", name)
					return
				}
				if safe != (i%2 == 0) {
					t.Errorf("并发下 %s safe=%v，期望 %v", name, safe, i%2 == 0)
					return
				}
				_ = sh.HasTool(name)
				_, _ = sh.NoMemoryOf(name)
			}
		}()
	}
	wg.Wait()
}
