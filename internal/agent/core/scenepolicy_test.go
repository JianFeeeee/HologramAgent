package core

// ScenePolicy 声明项的判据（R6）。
//
// 缺口事实（穷举确认，非查漏）：ChannelDef 已有 NoMemory/ContextPolicy/
// RecallPolicy 三件套，唯独没有「这条通道是否参与场面识别」；
// situationFeaturesFor 里只要 evt.Source != "" 就无条件塞 chan 特征。
// ⇒ chan:system / chan:kernel / chan:timer 这类内部信噪通道
// 也在参与场面聚类（现网 65 个键里就有 chan:system、chan:kernel、chan:timer）。
//
// 判据参照物在生产代码之外：期望值是「声明 none 的输入不产生任何场面特征」、
// 「未声明的输入行为逐字节不变」这两条不变量，不引用被测实现。

import (
	"testing"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

func scenePolicyNoneEvent() *agentIO.InputEvent {
	return &agentIO.InputEvent{
		Source:  "system",
		Payload: map[string]interface{}{"scene_policy": "none"},
	}
}

// R6 核心：声明 none 的输入不产生任何场面特征。
func TestScenePolicyNone_NoSituationFeatures(t *testing.T) {
	a := &Agent{io: agentIO.NewIOManager()}
	evt := scenePolicyNoneEvent()

	if feats := a.situationFeaturesFor(evt, "帮我看下定时器", ""); len(feats) != 0 {
		t.Fatalf("声明 scene_policy=none 的输入仍产出了 %d 个场面特征: %+v —— "+
			"内部信噪通道会参与场面聚类，把无关场面撑出来", len(feats), feats)
	}
}

// none 必须连时段(part) 都不产：一个不参与场面识别的通道
// 不该在 situation_evidence / scene_features 里留下任何足迹。
func TestScenePolicyNone_NoPartFeatureLeak(t *testing.T) {
	a := &Agent{io: agentIO.NewIOManager()}
	feats := a.situationFeaturesFor(scenePolicyNoneEvent(), "任意内容", "")
	for _, f := range feats {
		if f.Kind == "part" {
			t.Errorf("scene_policy=none 仍产出了时段特征 %q", f.Key())
		}
	}
}

// none 时工具场景也不该派生。
func TestScenePolicyNone_NoToolScene(t *testing.T) {
	a := &Agent{io: agentIO.NewIOManager()}
	keys := a.sceneKeysFor(scenePolicyNoneEvent(), "cmd_run")
	for _, k := range keys {
		if k == "tool:cmd_run" || k == "chan:system" {
			t.Errorf("scene_policy=none 仍派生了场景键 %q", k)
		}
	}
}

// 声明 none 时，显式 payload["scene"] 也不该被采纳——
// 否则通道声明形同虚设（注入点声明与通道声明必须一致，通道是更宽的闸）。
func TestScenePolicyNone_OverridesExplicitScene(t *testing.T) {
	a := &Agent{io: agentIO.NewIOManager()}
	evt := scenePolicyNoneEvent()
	evt.Payload["scene"] = "chan:qq"

	keys := a.sceneKeysFor(evt, "")
	for _, k := range keys {
		if k == "chan:qq" {
			t.Error("通道声明 scene_policy=none 后，注入点显式声明的 chan:qq 仍被采纳；" +
				"通道级闸门应覆盖注入点级声明")
		}
	}
	if feats := a.situationFeaturesFor(evt, "", ""); len(feats) != 0 {
		t.Errorf("scene_policy=none 仍产出特征 %+v", feats)
	}
}

// 未声明时行为必须逐字节不变（零值 = 保持现状 = 参与）。
// 这是「纯追加」承诺的护栏：老插件不填 ScenePolicy，行为不能有任何变化。
func TestScenePolicyUnset_BehavesExactlyAsBefore(t *testing.T) {
	a := &Agent{io: agentIO.NewIOManager()}
	evt := &agentIO.InputEvent{
		Source:  "qq",
		Payload: map[string]interface{}{},
	}

	feats := a.situationFeaturesFor(evt, "老大在吗", "")
	if len(feats) == 0 {
		t.Fatal("未声明 scene_policy 的输入不应失去场面特征（零值必须等价既有行为）")
	}
	var hasChan bool
	for _, f := range feats {
		if f.Kind == "chan" {
			hasChan = true
		}
	}
	if !hasChan {
		t.Errorf("未声明时应照旧产出 chan 特征，实际: %+v", feats)
	}

	keys := a.sceneKeysFor(evt, "")
	if len(keys) == 0 || keys[0] != "chan:qq" {
		t.Errorf("未声明时应照旧派生 chan:qq，实际: %v", keys)
	}
}

// 显式 auto 与未声明等价。
func TestScenePolicyAuto_SameAsUnset(t *testing.T) {
	a := &Agent{io: agentIO.NewIOManager()}
	mk := func(policy string) []string {
		payload := map[string]interface{}{}
		if policy != "" {
			payload["scene_policy"] = policy
		}
		return a.sceneKeysFor(&agentIO.InputEvent{Source: "qq", Payload: payload}, "")
	}
	unset, auto := mk(""), mk(pubsdk.ScenePolicyAuto)

	if len(unset) != len(auto) {
		t.Fatalf("auto 与未声明不等价: unset=%v auto=%v", unset, auto)
	}
	if len(auto) == 0 || auto[0] != "chan:qq" {
		t.Fatalf("auto 应照旧派生 chan:qq，实际: %v", auto)
	}
}

// SDK 常量与校验函数：形状须与 ContextPolicy/RecallPolicy 一致。
func TestScenePolicySDKShape(t *testing.T) {
	if !pubsdk.ValidScenePolicy("") {
		t.Error("空串应等价默认，校验须通过")
	}
	if !pubsdk.ValidScenePolicy(pubsdk.ScenePolicyNone) {
		t.Error("none 应合法")
	}
	if !pubsdk.ValidScenePolicy(pubsdk.ScenePolicyAuto) {
		t.Error("auto 应合法")
	}
	if pubsdk.ValidScenePolicy("prune") {
		t.Error("未知取值应被拒（照 ContextPolicy 的严格度）")
	}
}
