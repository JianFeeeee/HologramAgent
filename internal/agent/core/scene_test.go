package core

import (
	"strings"
	"testing"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// TestSceneKeysFor 钉住当前场景的推导优先级：
// 注入点显式声明 > 通道 > 工具；并列命中且去重。
func TestSceneKeysFor(t *testing.T) {
	a := &Agent{io: agentIO.NewIOManager()}
	// 通道 + 工具：两个都能独立成立的触发条件，都要带上
	got := a.sceneKeysFor(&agentIO.InputEvent{Source: "qq"}, "qq_get_message")
	want := []string{"chan:qq", "tool:qq_get_message"}
	if len(got) != len(want) {
		t.Fatalf("sceneKeysFor = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sceneKeysFor[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// 注入点显式声明排最前；归一化生效；重复声明去重
	evt := &agentIO.InputEvent{
		Source:  "QQ",
		Payload: map[string]interface{}{"scene": " chan:qq/peer:group_1 "},
	}
	got = a.sceneKeysFor(evt, "")
	if len(got) != 2 || got[0] != "chan:qq/peer:group_1" || got[1] != "chan:qq" {
		t.Errorf("显式声明应排最前且通道场景归一: %v", got)
	}

	// 数组形式声明
	evt = &agentIO.InputEvent{
		Source:  "webui",
		Payload: map[string]interface{}{"scene": []interface{}{"chan:qq", "task:reminder"}},
	}
	got = a.sceneKeysFor(evt, "")
	if len(got) != 3 || got[0] != "chan:qq" || got[1] != "task:reminder" || got[2] != "chan:webui" {
		t.Errorf("数组声明未生效: %v", got)
	}

	// nil 事件不 panic
	if got := a.sceneKeysFor(nil, ""); len(got) != 0 {
		t.Errorf("nil 事件应无场景: %v", got)
	}
	// 未声明的 payload 键不影响
	evt = &agentIO.InputEvent{Source: "cli", Payload: map[string]interface{}{"recall_policy": "none"}}
	if got := a.sceneKeysFor(evt, ""); len(got) != 1 || got[0] != "chan:cli" {
		t.Errorf("无 scene 声明时应只有通道场景: %v", got)
	}
}

// TestSituationFeaturesFor 钉住指纹来源：全部是运行时可观察量，
// 不需要模型配合也不需要人工标注。
func TestSituationFeaturesFor(t *testing.T) {
	a := &Agent{io: agentIO.NewIOManager()}
	evt := &agentIO.InputEvent{
		Source:  "QQ",
		Payload: map[string]interface{}{"group_id": float64(1027993713)},
	}
	feats := a.situationFeaturesFor(evt, "帮我看看排班表", "qq_get_message")
	kinds := map[string]int{}
	for _, f := range feats {
		kinds[f.Kind]++
	}
	if kinds["chan"] != 1 || kinds["peer_group"] != 1 || kinds["tool"] != 1 || kinds["part"] != 1 {
		t.Fatalf("必备维度缺失: %+v", feats)
	}
	if kinds["topic"] == 0 {
		t.Errorf("话题软特征缺失: %+v", feats)
	}
	if kinds["topic"] > 3 {
		t.Errorf("话题最多 3 个，得到 %d", kinds["topic"])
	}

	sig := memory.NewSituation(feats...)
	keys := sig.Keys()
	// 归一化 + 数值 id 的转换
	found := false
	for _, k := range keys {
		if k == "chan:qq" {
			found = true
		}
	}
	if !found {
		t.Errorf("通道特征未归一化: %v", keys)
	}
	for _, k := range keys {
		if strings.Contains(k, "peer_group:1027993713") {
			found = true
		}
	}
	if !found {
		t.Errorf("数值型 group_id 未转成特征: %v", keys)
	}

	// 无事件时不 panic，且只有工具特征时也成立
	if feats := a.situationFeaturesFor(nil, "", "memory_recall"); len(feats) != 1 {
		t.Errorf("仅工具场景应有 1 个特征: %+v", feats)
	}
}

// TestWritePathAttachesBothPaths 钉住写侧的「两条路都挂」：
// 显式声明优先；否则挂本轮声明的 + 涌现的场景集合。
func TestTurnSceneKeysBothPaths(t *testing.T) {
	a := &Agent{io: agentIO.NewIOManager()}
	// 声明的通道场景与工具场景都在，涌现键（若有）追加在后
	evt := &agentIO.InputEvent{Source: "qq", Payload: map[string]interface{}{"scene": "chan:qq/peer:group_1"}}
	got := a.sceneKeysFor(evt, "qq_get_message")
	want := []string{"chan:qq/peer:group_1", "chan:qq", "tool:qq_get_message"}
	if len(got) != len(want) {
		t.Fatalf("声明侧场景数不对: %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("声明侧[%d] = %q want %q", i, got[i], want[i])
		}
	}
}
