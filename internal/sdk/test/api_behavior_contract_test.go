package test

// SDK 接口**行为**判据（黑盒：只调公开接口，观察可测量的效果）。
//
// 为什么需要这一层：`public_contract_test.go` 只覆盖了配置与代理，
// 而 SDK 里真正决定插件能不能干活的那几个面（类型注册 / stage 注册 /
// 工具注册 / 注入通道）**一个都没有判据**。它们的失效方式又恰恰是静默的：
//
//	regAgentType == nil  ⇒ Register 返回错误（这一条还算响亮）
//	regStage     == nil  ⇒ 空操作，插件以为注册了
//	regTool      == nil  ⇒ 返回 nil，插件以为注册成功
//
// 所以这里不测"有没有这个函数"，只测"调用之后世界发生了什么变化"。

import (
	"errors"
	"testing"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// ── 类型注册面 ──

// TestAgentTypesRegisterAndUnregisterAreObservable 判类型的注册/撤销是**双向**的。
//
// 只判"Register 返回 nil"没有意义：真正的契约是撤销之后**查不到**，
// 否则 `AgentTypes().Unregister` 就是个假口子（类型永远留在表里，
// 插件卸载后还能被别人以同名注册失败）。
func TestAgentTypesRegisterAndUnregisterAreObservable(t *testing.T) {
	var got *sdk.AgentTypeDef
	var unregArg string
	s := sdk.New("contract-agenttype", nil, nil, nil, nil, nil)
	s.SetAgentTypeRegistrar(
		func(def sdk.AgentTypeDef) error {
			got = &def
			return nil
		},
		func(name string) error {
			unregArg = name
			return nil
		},
	)

	api := s.AgentTypes()
	def := sdk.AgentTypeDef{Type: "zz_probe", Description: "判据用探针类型"}
	if err := api.Register(def); err != nil {
		t.Fatalf("Register 返回错误: %v", err)
	}
	// ★ 判据钉的是"载荷被原样交出"：类型名与说明缺一个，
	//   `admin_child` 的 type 枚举就会出现空说明或空名字。
	if got == nil || got.Type != def.Type || got.Description != def.Description {
		t.Fatalf("注册载荷没有原样交给内核: %+v", got)
	}

	if err := api.Unregister("zz_probe"); err != nil {
		t.Fatalf("Unregister 返回错误: %v", err)
	}
	if unregArg != "zz_probe" {
		t.Fatalf("撤销没有传到内核（收到 %q）", unregArg)
	}
}

// TestAgentTypesFailLoudlyWhenUnwired 判"没给注册口时注册**必须**响亮失败。
//
// ★ 这条是两档 SDK 的守门判据：外部插件结构上拿不到 regAgentType，
// 若这里静默返回 nil，插件会以为"我注册成功了"，而它的类型永远不会存在 ——
// 症状要到运行期 `admin_child(type="...")` 失败时才暴露，且指向错误的地方。
func TestAgentTypesFailLoudlyWhenUnwired(t *testing.T) {
	s := sdk.New("contract-agenttype-unwired", nil, nil, nil, nil, nil)
	err := s.AgentTypes().Register(sdk.AgentTypeDef{Type: "zz_unwired"})
	if err == nil {
		t.Fatal("未注入注册口时 Register 返回了 nil：插件会误以为注册成功")
	}
}

// TestAgentTypesPropagatesRegistrarError 判内核的拒绝理由**不被吞掉**。
//
// 内核会因"重名"或"非内置插件"拒绝注册；吞掉错误会让插件无从修正。
func TestAgentTypesPropagatesRegistrarError(t *testing.T) {
	sentinel := errors.New("内核拒绝：非内置插件")
	s := sdk.New("contract-agenttype-reject", nil, nil, nil, nil, nil)
	s.SetAgentTypeRegistrar(
		func(sdk.AgentTypeDef) error { return sentinel },
		func(string) error { return nil },
	)
	err := s.AgentTypes().Register(sdk.AgentTypeDef{Type: "zz_reject"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("内核的拒绝理由被吞掉或改写: %v", err)
	}
}

// ── stage 注册面 ──

// TestRegisterStageDeliversToRegistrar 判 stage 注册把 (阶段, handler) 原样交出。
//
// ★ 判据钉两个字段：阶段名错 ⇒ 插件注册的 handler 永远不会被调到；
//
//	handler 为 nil ⇒ 阶段到了却什么也不做（比没注册更难查）。
func TestRegisterStageDeliversToRegistrar(t *testing.T) {
	type reg struct {
		stage   sdk.Stage
		handler sdk.StageHandler
	}
	var got []reg
	s := sdk.New("contract-stage", nil, nil, func(stage sdk.Stage, h sdk.StageHandler) {
		got = append(got, reg{stage, h})
	}, nil, nil)

	h := func(*sdk.StageContext) error { return nil }
	s.RegisterStage(sdk.StageOnToolcall, h)
	s.RegisterStage(sdk.StageBeforeOutput, h)

	if len(got) != 2 {
		t.Fatalf("注册了 2 个阶段，内核收到 %d 个", len(got))
	}
	if got[0].stage != sdk.StageOnToolcall || got[1].stage != sdk.StageBeforeOutput {
		t.Fatalf("阶段名被改写: %q, %q", got[0].stage, got[1].stage)
	}
	for i, r := range got {
		if r.handler == nil {
			t.Fatalf("第 %d 个 stage 的 handler 为 nil（阶段到了也不会执行）", i)
		}
	}
}

// ── 工具注册面 ──

// TestRegisterToolDeliversNameDefHandler 判工具注册三要素都被交出。
//
// 缺 handler ⇒ 工具在表里但调用即崩；缺 def ⇒ 模型不知道该工具怎么用。
func TestRegisterToolDeliversNameDefHandler(t *testing.T) {
	var (
		gotName string
		gotDef  sdk.ToolDef
		gotH    sdk.ToolHandler
	)
	s := sdk.New("contract-tool", nil, func(name string, def sdk.ToolDef, h sdk.ToolHandler) error {
		gotName, gotDef, gotH = name, def, h
		return nil
	}, nil, nil, nil)

	def := sdk.ToolDef{
		Description: "判据用工具",
		Parameters:  map[string]interface{}{"type": "object"},
	}
	if err := s.RegisterTool("zz_probe_tool", def, func(map[string]interface{}) (interface{}, error) {
		return "ok", nil
	}); err != nil {
		t.Fatalf("RegisterTool 返回错误: %v", err)
	}
	if gotName != "zz_probe_tool" {
		t.Fatalf("工具名被改写: %q", gotName)
	}
	if gotDef.Description != def.Description {
		t.Fatalf("工具说明被改写: %q", gotDef.Description)
	}
	if gotH == nil {
		t.Fatal("handler 为 nil（工具在表里但调用即崩）")
	}
}

// TestRegisterToolPropagatesRegistrarError 判内核拒绝工具注册时理由不被吞。
func TestRegisterToolPropagatesRegistrarError(t *testing.T) {
	sentinel := errors.New("工具名冲突")
	s := sdk.New("contract-tool-reject", nil, func(string, sdk.ToolDef, sdk.ToolHandler) error {
		return sentinel
	}, nil, nil, nil)

	err := s.RegisterTool("zz_dup", sdk.ToolDef{Description: "d"}, func(map[string]interface{}) (interface{}, error) {
		return "", nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("内核的拒绝理由被吞掉或改写: %v", err)
	}
}
