package test

// cfgmgr 黑盒判据：公开 Start 注入工具注册回调，观察注册的工具契约。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/plugins/cfgmgr"
	sdkpkg "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

func TestStartRegistersConfigToolsWithConcurrencyMarkers(t *testing.T) {
	var names []string
	serial := map[string]bool{}
	parallel := map[string]bool{}
	reg := func(name string, def sdkpkg.ToolDef, handler sdkpkg.ToolHandler) error {
		names = append(names, name)
		if def.Serial {
			serial[name] = true
		}
		if def.ParallelSafe {
			parallel[name] = true
		}
		return nil
	}
	p := cfgmgr.New("contract-cfgmgr")
	sdk := sdkpkg.New("contract-cfgmgr", sdkpkg.SDKConfig{RegTool: reg})
	if err := p.Start(sdk); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := p.Name(); got != "contract-cfgmgr" {
		t.Fatalf("Name=%q", got)
	}

	for _, want := range []string{
		"config_list_plugins", "config_get", "config_set",
		"config_list_keys", "config_get_defs", "config_dump", "config_batch_set",
	} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("缺少工具 %s；实际 %v", want, names)
		}
	}
	// 写工具必须串行；读工具必须可并发。
	if !serial["config_set"] || !serial["config_batch_set"] {
		t.Error("写工具未声明 Serial")
	}
	for _, ro := range []string{"config_list_plugins", "config_get", "config_list_keys", "config_get_defs", "config_dump"} {
		if !parallel[ro] {
			t.Errorf("只读工具 %s 未声明 ParallelSafe", ro)
		}
	}
}

func TestStopIsSafe(t *testing.T) {
	p := cfgmgr.New("x")
	sdk := sdkpkg.New("x", sdkpkg.SDKConfig{RegTool: func(string, sdkpkg.ToolDef, sdkpkg.ToolHandler) error { return nil }})
	if err := p.Start(sdk); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
