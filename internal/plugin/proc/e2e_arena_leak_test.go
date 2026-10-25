package proc

import "testing"

const arenaLeakPluginSource = `package main

import (
	"strings"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

type leakPlugin struct{ name string }

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &leakPlugin{name: name}, nil
}

func (p *leakPlugin) Name() string { return p.name }

func (p *leakPlugin) Stop() error { return nil }

func (p *leakPlugin) Start(s *sdk.PluginSDK) error {
	// 返回 200KB：远超内核标定的 64KB 结果预算，必然走 arenaPut 扩容块路径
	// （模板里 frameOutput 放不下 → arenaAlloc → 打 sharedRefFlagExpand）。
	s.RegisterTool("leak_big", sdk.ToolDef{
		Description: "返回超大结果，触发扩容块",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}, func(args map[string]interface{}) (interface{}, error) {
		return strings.Repeat("x", 200*1024), nil
	})
	return nil
}
`

func TestE2E_ExpandBlocksReclaimed(t *testing.T) {
	bin := buildPluginWithRealTemplate(t, arenaLeakPluginSource)

	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	core := newFakeCore()
	p := New("leak", bin, t.TempDir(), nil, host, nil)
	if err := p.Start(core); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	core.mu.Lock()
	h, ok := core.tools["leak_big"]
	core.mu.Unlock()
	if !ok {
		t.Fatal("插件应注册 leak_big 工具")
	}

	// 连调 10 次 ≈ 2MB 扩容块。arena 只有 4MB：旧实现每次泄漏 200KB，
	// 第 ~19 次就耗尽；这里跑 10 次足以让旧实现在 used 上留下非零。
	for i := 0; i < 10; i++ {
		res, err := h(map[string]interface{}{})
		if err != nil {
			t.Fatalf("第 %d 次调用失败（arena 已被前面的泄漏吃掉？）: %v", i, err)
		}
		s, _ := res.(string)
		if len(s) != 200*1024 {
			t.Fatalf("第 %d 次结果长度 %d，期望 %d", i, len(s), 200*1024)
		}
		if used, _ := host.Arena().Stats(); used != 0 {
			t.Fatalf("第 %d 次调用后 arena used=%d，扩容块未归还（泄漏）", i, used)
		}
	}
}
