package sdk

import (
	"testing"
)

func TestRegisterStageGlobalDefault(t *testing.T) {
	called := false
	regStage := func(stage Stage, handler StageHandler) {
		called = true
	}
	s := &PluginSDK{regStage: regStage, name: "test"}
	s.RegisterStage(StageBeforeToolcall, func(ctx *StageContext) error { return nil })

	if !called {
		t.Error("global scope: handler not registered")
	}
}

func TestRegisterStageGlobalExplicit(t *testing.T) {
	called := false
	regStage := func(stage Stage, handler StageHandler) {
		called = true
	}
	s := &PluginSDK{regStage: regStage, name: "test"}
	s.RegisterStage(StageBeforeToolcall, func(ctx *StageContext) error { return nil }, StageScopeGlobal)

	if !called {
		t.Error("global scope: handler not registered")
	}
}

func TestRegisterStageOwnToolsMatch(t *testing.T) {
	var registered StageHandler
	regStage := func(stage Stage, handler StageHandler) {
		registered = handler
	}
	s := &PluginSDK{regStage: regStage, name: "myplugin"}
	s.RegisterStage(StageBeforeToolcall, func(ctx *StageContext) error { return nil }, StageScopeOwnTools)

	if registered == nil {
		t.Fatal("handler not registered")
	}

	ctx := &StageContext{}
	ctx.ToolCalls = []ToolCall{{Plugin: "myplugin", Name: "my_tool"}}
	ctx.ToolResults = nil

	err := registered(ctx)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestRegisterStageOwnToolsSkipOtherPlugin(t *testing.T) {
	var registered StageHandler
	regStage := func(stage Stage, handler StageHandler) {
		registered = handler
	}
	s := &PluginSDK{regStage: regStage, name: "myplugin"}

	callCount := 0
	s.RegisterStage(StageBeforeToolcall, func(ctx *StageContext) error {
		callCount++
		return nil
	}, StageScopeOwnTools)

	if registered == nil {
		t.Fatal("handler not registered")
	}

	ctx := &StageContext{}
	ctx.ToolCalls = []ToolCall{{Plugin: "other", Name: "other_tool"}}

	err := registered(ctx)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if callCount != 0 {
		t.Error("handler should not be called for other plugin's tool")
	}
}

func TestRegisterStageOwnToolsNonToolcallDegrades(t *testing.T) {
	regStage := func(stage Stage, handler StageHandler) {
		if stage != StagePreAction {
			t.Errorf("expected StagePreAction, got %s", stage)
		}
	}
	s := &PluginSDK{regStage: regStage, name: "test"}
	s.RegisterStage(StagePreAction, func(ctx *StageContext) error { return nil }, StageScopeOwnTools)
}

func TestRegisterStageOwnToolsStageBeforeToolcallNoToolCalls(t *testing.T) {
	var registered StageHandler
	regStage := func(stage Stage, handler StageHandler) {
		registered = handler
	}
	s := &PluginSDK{regStage: regStage, name: "myplugin"}

	callCount := 0
	s.RegisterStage(StageBeforeToolcall, func(ctx *StageContext) error {
		callCount++
		return nil
	}, StageScopeOwnTools)

	if registered == nil {
		t.Fatal("handler not registered")
	}

	ctx := &StageContext{}

	err := registered(ctx)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if callCount != 0 {
		t.Error("handler should not be called when ToolCalls is empty")
	}
}

func TestRegisterStageOwnToolsStageAfterToolcallMatch(t *testing.T) {
	var registered StageHandler
	regStage := func(stage Stage, handler StageHandler) {
		registered = handler
	}
	s := &PluginSDK{regStage: regStage, name: "myplugin"}

	callCount := 0
	s.RegisterStage(StageAfterToolcall, func(ctx *StageContext) error {
		callCount++
		return nil
	}, StageScopeOwnTools)

	if registered == nil {
		t.Fatal("handler not registered")
	}

	ctx := &StageContext{}
	ctx.ToolResults = []ToolResult{{Plugin: "myplugin", Name: "my_tool"}}

	err := registered(ctx)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if callCount != 1 {
		t.Error("handler should be called for own plugin's tool result")
	}
}

func TestRegisterStageOwnToolsStageAfterToolcallSkip(t *testing.T) {
	var registered StageHandler
	regStage := func(stage Stage, handler StageHandler) {
		registered = handler
	}
	s := &PluginSDK{regStage: regStage, name: "myplugin"}

	callCount := 0
	s.RegisterStage(StageAfterToolcall, func(ctx *StageContext) error {
		callCount++
		return nil
	}, StageScopeOwnTools)

	ctx := &StageContext{}
	ctx.ToolResults = []ToolResult{{Plugin: "other", Name: "other_tool"}}

	err := registered(ctx)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if callCount != 0 {
		t.Error("handler should not be called for other plugin's tool result")
	}
}

func TestRegisterStageOwnToolsNilRegStage(t *testing.T) {
	s := &PluginSDK{name: "test"}
	s.RegisterStage(StageBeforeToolcall, func(ctx *StageContext) error { return nil }, StageScopeOwnTools)
}

func TestToolDefCleaner(t *testing.T) {
	called := false
	def := ToolDef{
		Name:        "test_clean",
		Description: "A test tool with cleaner",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		Cleaner: func(output string) string {
			called = true
			return "cleaned:" + output
		},
	}
	if def.Cleaner == nil {
		t.Fatal("Cleaner should not be nil")
	}
	result := def.Cleaner("raw output")
	if !called {
		t.Error("Cleaner was not called")
	}
	if result != "cleaned:raw output" {
		t.Errorf("expected 'cleaned:raw output', got '%s'", result)
	}
}

func TestToolDefNoMemory(t *testing.T) {
	def := ToolDef{
		Name:        "test_nomem",
		Description: "A test tool with NoMemory",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		NoMemory:    true,
	}
	if !def.NoMemory {
		t.Error("NoMemory should be true")
	}
	if def.Cleaner != nil {
		t.Error("Cleaner should be nil when not set")
	}
}

func TestToolDefNoMemoryDefaultFalse(t *testing.T) {
	def := ToolDef{
		Name:        "test_default",
		Description: "A test tool with defaults",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}
	if def.NoMemory {
		t.Error("NoMemory should default to false")
	}
}

func TestToolDefRegisterPreservesNoMemory(t *testing.T) {
	var capturedDef ToolDef
	regTool := func(name string, def ToolDef, handler ToolHandler) error {
		capturedDef = def
		return nil
	}
	s := &PluginSDK{regTool: regTool, name: "test"}
	def := ToolDef{
		Name:        "test_tool",
		Description: "test desc",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		NoMemory:    true,
		Cleaner:     func(s string) string { return s },
	}
	s.RegisterTool("test_tool", def, func(args map[string]interface{}) (interface{}, error) {
		return nil, nil
	})
	if !capturedDef.NoMemory {
		t.Error("NoMemory should be preserved through RegisterTool")
	}
	if capturedDef.Cleaner == nil {
		t.Error("Cleaner should be preserved through RegisterTool")
	}
}
