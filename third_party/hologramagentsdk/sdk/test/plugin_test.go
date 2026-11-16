package test

import (
	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
	"testing"
)

func TestRegisterStageGlobalDefault(t *testing.T) {
	called := false
	regStage := func(stage sdk.Stage, handler sdk.StageHandler) {
		called = true
	}
	s := sdk.New("test", nil, nil, regStage, nil, nil)
	s.RegisterStage(sdk.StageBeforeToolcall, func(ctx *sdk.StageContext) error { return nil })

	if !called {
		t.Error("global scope: handler not registered")
	}
}

func TestRegisterStageGlobalExplicit(t *testing.T) {
	called := false
	regStage := func(stage sdk.Stage, handler sdk.StageHandler) {
		called = true
	}
	s := sdk.New("test", nil, nil, regStage, nil, nil)
	s.RegisterStage(sdk.StageBeforeToolcall, func(ctx *sdk.StageContext) error { return nil }, sdk.StageScopeGlobal)

	if !called {
		t.Error("global scope: handler not registered")
	}
}

func TestRegisterStageOwnToolsMatch(t *testing.T) {
	var registered sdk.StageHandler
	regStage := func(stage sdk.Stage, handler sdk.StageHandler) {
		registered = handler
	}
	s := sdk.New("myplugin", nil, nil, regStage, nil, nil)
	s.RegisterStage(sdk.StageBeforeToolcall, func(ctx *sdk.StageContext) error { return nil }, sdk.StageScopeOwnTools)

	if registered == nil {
		t.Fatal("handler not registered")
	}

	ctx := &sdk.StageContext{}
	ctx.ToolCalls = []sdk.ToolCall{{Plugin: "myplugin", Name: "my_tool"}}
	ctx.ToolResults = nil

	err := registered(ctx)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestRegisterStageOwnToolsSkipOtherPlugin(t *testing.T) {
	var registered sdk.StageHandler
	regStage := func(stage sdk.Stage, handler sdk.StageHandler) {
		registered = handler
	}
	s := sdk.New("myplugin", nil, nil, regStage, nil, nil)

	callCount := 0
	s.RegisterStage(sdk.StageBeforeToolcall, func(ctx *sdk.StageContext) error {
		callCount++
		return nil
	}, sdk.StageScopeOwnTools)

	if registered == nil {
		t.Fatal("handler not registered")
	}

	ctx := &sdk.StageContext{}
	ctx.ToolCalls = []sdk.ToolCall{{Plugin: "other", Name: "other_tool"}}

	err := registered(ctx)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if callCount != 0 {
		t.Error("handler should not be called for other plugin's tool")
	}
}

func TestRegisterStageOwnToolsNonToolcallDegrades(t *testing.T) {
	regStage := func(stage sdk.Stage, handler sdk.StageHandler) {
		if stage != sdk.StagePreAction {
			t.Errorf("expected sdk.StagePreAction, got %s", stage)
		}
	}
	s := sdk.New("test", nil, nil, regStage, nil, nil)
	s.RegisterStage(sdk.StagePreAction, func(ctx *sdk.StageContext) error { return nil }, sdk.StageScopeOwnTools)
}

func TestRegisterStageOwnToolsStageBeforeToolcallNoToolCalls(t *testing.T) {
	var registered sdk.StageHandler
	regStage := func(stage sdk.Stage, handler sdk.StageHandler) {
		registered = handler
	}
	s := sdk.New("myplugin", nil, nil, regStage, nil, nil)

	callCount := 0
	s.RegisterStage(sdk.StageBeforeToolcall, func(ctx *sdk.StageContext) error {
		callCount++
		return nil
	}, sdk.StageScopeOwnTools)

	if registered == nil {
		t.Fatal("handler not registered")
	}

	ctx := &sdk.StageContext{}

	err := registered(ctx)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if callCount != 0 {
		t.Error("handler should not be called when ToolCalls is empty")
	}
}

func TestRegisterStageOwnToolsStageAfterToolcallMatch(t *testing.T) {
	var registered sdk.StageHandler
	regStage := func(stage sdk.Stage, handler sdk.StageHandler) {
		registered = handler
	}
	s := sdk.New("myplugin", nil, nil, regStage, nil, nil)

	callCount := 0
	s.RegisterStage(sdk.StageAfterToolcall, func(ctx *sdk.StageContext) error {
		callCount++
		return nil
	}, sdk.StageScopeOwnTools)

	if registered == nil {
		t.Fatal("handler not registered")
	}

	ctx := &sdk.StageContext{}
	ctx.ToolResults = []sdk.ToolResult{{Plugin: "myplugin", Name: "my_tool"}}

	err := registered(ctx)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if callCount != 1 {
		t.Error("handler should be called for own plugin's tool result")
	}
}

func TestRegisterStageOwnToolsStageAfterToolcallSkip(t *testing.T) {
	var registered sdk.StageHandler
	regStage := func(stage sdk.Stage, handler sdk.StageHandler) {
		registered = handler
	}
	s := sdk.New("myplugin", nil, nil, regStage, nil, nil)

	callCount := 0
	s.RegisterStage(sdk.StageAfterToolcall, func(ctx *sdk.StageContext) error {
		callCount++
		return nil
	}, sdk.StageScopeOwnTools)

	ctx := &sdk.StageContext{}
	ctx.ToolResults = []sdk.ToolResult{{Plugin: "other", Name: "other_tool"}}

	err := registered(ctx)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if callCount != 0 {
		t.Error("handler should not be called for other plugin's tool result")
	}
}

func TestRegisterStageOwnToolsNilRegStage(t *testing.T) {
	s := sdk.New("test", nil, nil, nil, nil, nil)
	s.RegisterStage(sdk.StageBeforeToolcall, func(ctx *sdk.StageContext) error { return nil }, sdk.StageScopeOwnTools)
}

func TestToolDefCleaner(t *testing.T) {
	called := false
	def := sdk.ToolDef{
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
	def := sdk.ToolDef{
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
	def := sdk.ToolDef{
		Name:        "test_default",
		Description: "A test tool with defaults",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}
	if def.NoMemory {
		t.Error("NoMemory should default to false")
	}
}

func TestToolDefRegisterPreservesNoMemory(t *testing.T) {
	var capturedDef sdk.ToolDef
	regTool := func(name string, def sdk.ToolDef, handler sdk.ToolHandler) error {
		capturedDef = def
		return nil
	}
	s := sdk.New("test", nil, regTool, nil, nil, nil)
	def := sdk.ToolDef{
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
