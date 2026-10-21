package core

import (
	"sync"
	"sync/atomic"
	"testing"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

func TestStageHostRegisterTool(t *testing.T) {
	host := NewStageHost()

	err := host.RegisterTool("test_tool", sdk.ToolDef{Name: "test_tool"}, func(args map[string]interface{}) (interface{}, error) {
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	defs := host.GetToolDefs()
	if len(defs) != 1 {
		t.Errorf("expected 1 tool def, got %d", len(defs))
	}
	if defs[0].Name != "test_tool" {
		t.Errorf("expected test_tool, got %s", defs[0].Name)
	}
}

func TestStageHostRegisterToolDuplicate(t *testing.T) {
	host := NewStageHost()
	host.RegisterTool("dup", sdk.ToolDef{Name: "dup"}, nil)
	err := host.RegisterTool("dup", sdk.ToolDef{Name: "dup"}, nil)
	if err == nil {
		t.Error("expected error on duplicate tool")
	}
}

func TestStageHostExecuteTool(t *testing.T) {
	host := NewStageHost()

	host.RegisterTool("hello", sdk.ToolDef{Name: "hello"}, func(args map[string]interface{}) (interface{}, error) {
		return "world", nil
	})

	result, err := host.ExecuteTool("hello", nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.(string) != "world" {
		t.Errorf("expected world, got %v", result)
	}

	_, err = host.ExecuteTool("nonexistent", nil)
	if err == nil {
		t.Error("expected error for nonexistent tool")
	}
}

func TestStageHostRunStage(t *testing.T) {
	host := NewStageHost()

	var called bool
	host.RegisterStage(sdk.StageOnInput, func(ctx *sdk.StageContext) error {
		called = true
		return nil
	})

	ctx := &sdk.StageContext{RawMessage: "hello"}
	host.RunStage(sdk.StageOnInput, ctx)

	if !called {
		t.Error("stage handler was not called")
	}
}

func TestStageHostRunStageParallel(t *testing.T) {
	host := NewStageHost()

	// Two handlers that both try to set Response under Lock.
	// Only the first to acquire Lock actually wins; the second sees IsResponded() and skips.
	host.RegisterStage(sdk.StageOnInput, func(ctx *sdk.StageContext) error {
		ctx.Lock()
		if ctx.Response == nil {
			resp := "from-first"
			ctx.Response = &resp
		}
		ctx.Unlock()
		return nil
	})
	host.RegisterStage(sdk.StageOnInput, func(ctx *sdk.StageContext) error {
		ctx.Lock()
		if ctx.Response == nil {
			resp := "from-second"
			ctx.Response = &resp
		}
		ctx.Unlock()
		return nil
	})

	ctx := &sdk.StageContext{RawMessage: "hello"}
	host.RunStage(sdk.StageOnInput, ctx)

	if ctx.Response == nil {
		t.Fatal("expected a response to be set")
	}
	if *ctx.Response != "from-first" && *ctx.Response != "from-second" {
		t.Errorf("expected either from-first or from-second, got %s", *ctx.Response)
	}
}

func TestStageHostRunStageConcurrency(t *testing.T) {
	host := NewStageHost()

	var counter int32
	n := 10
	for i := 0; i < n; i++ {
		host.RegisterStage(sdk.StageAfterOutput, func(ctx *sdk.StageContext) error {
			atomic.AddInt32(&counter, 1)
			return nil
		})
	}

	host.RunStage(sdk.StageAfterOutput, &sdk.StageContext{})

	if int(counter) != n {
		t.Errorf("expected %d handlers called, got %d", n, counter)
	}
}

func TestStageHostRunStageAll(t *testing.T) {
	host := NewStageHost()

	var mu sync.Mutex
	count := 0
	host.RegisterStage(sdk.StageAfterOutput, func(ctx *sdk.StageContext) error {
		mu.Lock()
		count++
		mu.Unlock()
		return nil
	})
	host.RegisterStage(sdk.StageAfterOutput, func(ctx *sdk.StageContext) error {
		mu.Lock()
		count++
		mu.Unlock()
		return nil
	})

	host.RunStage(sdk.StageAfterOutput, &sdk.StageContext{})

	if count != 2 {
		t.Errorf("expected 2 handlers called, got %d", count)
	}
}

func TestStageHostEmpty(t *testing.T) {
	host := NewStageHost()

	if host.ToolCount() != 0 {
		t.Errorf("expected 0 tools, got %d", host.ToolCount())
	}

	defs := host.GetToolDefs()
	if len(defs) != 0 {
		t.Errorf("expected 0 tool defs, got %d", len(defs))
	}

	_, err := host.ExecuteTool("anything", nil)
	if err == nil {
		t.Error("expected error on empty host")
	}

	host.RunStage(sdk.StageOnInput, &sdk.StageContext{})
}

func TestStageHostMultipleTools(t *testing.T) {
	host := NewStageHost()

	host.RegisterTool("tool1", sdk.ToolDef{Name: "tool1"}, func(args map[string]interface{}) (interface{}, error) {
		return "from_p1", nil
	})
	host.RegisterTool("tool2", sdk.ToolDef{Name: "tool2"}, func(args map[string]interface{}) (interface{}, error) {
		return "from_p2", nil
	})

	if host.ToolCount() != 2 {
		t.Errorf("expected 2 tools, got %d", host.ToolCount())
	}

	r1, _ := host.ExecuteTool("tool1", nil)
	if r1.(string) != "from_p1" {
		t.Errorf("expected from_p1, got %v", r1)
	}

	r2, _ := host.ExecuteTool("tool2", nil)
	if r2.(string) != "from_p2" {
		t.Errorf("expected from_p2, got %v", r2)
	}
}

func TestStageHostToolDefLookup(t *testing.T) {
	host := NewStageHost()
	host.RegisterTool("tool_a", sdk.ToolDef{Name: "tool_a", NoMemory: true}, nil)
	host.RegisterTool("tool_b", sdk.ToolDef{Name: "tool_b"}, nil)

	def := host.ToolDef("tool_a")
	if def == nil {
		t.Fatal("expected tool_a to be found")
	}
	if !def.NoMemory {
		t.Error("tool_a should have NoMemory=true")
	}

	def = host.ToolDef("tool_b")
	if def == nil {
		t.Fatal("expected tool_b to be found")
	}
	if def.NoMemory {
		t.Error("tool_b should have NoMemory=false")
	}

	def = host.ToolDef("nonexistent")
	if def != nil {
		t.Errorf("expected nil for nonexistent tool, got %v", def)
	}
}

func TestStageHostToolDefNoMemoryStored(t *testing.T) {
	host := NewStageHost()
	host.RegisterTool("mem_tool", sdk.ToolDef{Name: "mem_tool", NoMemory: true}, nil)
	host.RegisterTool("normal_tool", sdk.ToolDef{Name: "normal_tool", NoMemory: false}, nil)

	defs := host.GetToolDefs()
	found := map[string]bool{}
	for _, d := range defs {
		found[d.Name] = d.NoMemory
	}

	if v, ok := found["mem_tool"]; !ok {
		t.Error("mem_tool not found in defs")
	} else if !v {
		t.Error("mem_tool.NoMemory should be true")
	}

	if v, ok := found["normal_tool"]; !ok {
		t.Error("normal_tool not found in defs")
	} else if v {
		t.Error("normal_tool.NoMemory should be false")
	}
}
