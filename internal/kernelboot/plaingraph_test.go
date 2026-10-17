package kernelboot

// 编译期图 → 运行期启动计划的判据（设计 §2.5，用户要求：
// "启动顺序是编译期间按照声明的依赖顺序生成的启动依赖图排列的"）。
//
// ## 这组判据守什么
//
//  ① 运行期**读**图里的顺序，不重算（层号被真正使用）
//  ② 图与声明**漂移**时拒绝启动（§2.5.2 的病 A）
//  ③ 图与二进制**描述不同模块集**时拒绝启动
//  ④ 自校验能抓损坏（已有判据，这里补"层号参与校验"）

import (
	"strings"
	"testing"
)

// 造一个可分层的小世界。
func planTestModules() []Module {
	return []Module{
		&fakeModule{name: "config", provides: []string{"config"}},
		&fakeModule{name: "lua", provides: []string{"lua"}, requires: []string{"config"}},
		&fakeModule{name: "llm", provides: []string{"llm"}, requires: []string{"config", "lua"}},
	}
}

// ★★★ 判据：**PlanFromGraph 按图中的层号排计划**（而不是重算）。
func TestPlanFromGraphUsesEmbeddedLayers(t *testing.T) {
	mods := planTestModules()
	built, err := Build(mods)
	if err != nil {
		t.Fatal(err)
	}
	edges := planToEdgesForTest(built)
	gf := decodeGraphOrFatal(t, EncodeGraph(edges))

	plan, err := PlanFromGraph(gf, mods)
	if err != nil {
		t.Fatalf("PlanFromGraph: %v", err)
	}
	if len(plan.Layers) != len(built.Layers) {
		t.Fatalf("层数不符: %d != %d", len(plan.Layers), len(built.Layers))
	}
	for i := range built.Layers {
		got := names(plan.Layers[i])
		want := names(built.Layers[i])
		if !equalStrs(got, want) {
			t.Errorf("第 %d 层不符: %v != %v", i, got, want)
		}
	}
}

// ★★★ 判据：**层号真的被使用**（有人重算而不读图 ⇒ 报红）。
//
// 做法：手工把图里 lua 的层号改成 0（与声明矛盾）。
// 若 PlanFromGraph 重算而不读，它会"照常工作"；读了图并校验 ⇒ 报漂移。
func TestPlanFromGraphDetectsLayerDrift(t *testing.T) {
	mods := planTestModules()
	built, _ := Build(mods)
	edges := planToEdgesForTest(built)

	// 篡改：把 lua 标成第 0 层（它实际在第 1 层）
	for i := range edges {
		if edges[i].Name == "lua" {
			edges[i].Layer = 0
		}
	}
	gf := decodeGraphOrFatal(t, EncodeGraph(edges))

	_, err := PlanFromGraph(gf, mods)
	if err == nil {
		t.Fatal("★ 图里的层号与声明矛盾，却照常构造了计划 —— " +
			"运行期必须用图里的顺序，并校验它与声明一致（否则旧图会给出错误顺序）")
	}
	if !strings.Contains(err.Error(), "漂移") {
		t.Errorf("错误应点明是\"漂移\"（改了声明没重出图）: %v", err)
	}
}

// ★★★ 判据：**图与二进制模块集不一致时拒绝启动**。
func TestPlanFromGraphDetectsModuleSetMismatch(t *testing.T) {
	mods := planTestModules()
	built, _ := Build(mods)
	edges := planToEdgesForTest(built)

	// ① 二进制里少了一个模块（模拟"图是新的、二进制是旧的"）
	gf := decodeGraphOrFatal(t, EncodeGraph(edges))
	if _, err := PlanFromGraph(gf, mods[:2]); err == nil {
		t.Fatal("★ 图描述了二进制里没有的模块，却通过了 —— 会把旧芯片的图配新二进制")
	}

	// ② 图里少了一个模块
	short := make([]Module, 0, 2)
	short = append(short, mods[0], mods[1])
	shortBuilt, _ := Build(short)
	gf2 := decodeGraphOrFatal(t, EncodeGraph(planToEdgesForTest(shortBuilt)))
	if _, err := PlanFromGraph(gf2, mods); err == nil {
		t.Fatal("★ 二进制里有图里没有的模块，却通过了")
	}
}

// ★★★ 判据：**层号进了二进制**（编解码往返保真）。
func TestGraphRoundTripsLayer(t *testing.T) {
	mods := planTestModules()
	built, _ := Build(mods)
	edges := planToEdgesForTest(built)
	gf := decodeGraphOrFatal(t, EncodeGraph(edges))

	byName := map[string]int{}
	for _, e := range gf.Modules {
		byName[e.Name] = e.Layer
	}
	for _, m := range mods {
		_ = m
	}
	if byName["config"] != 0 || byName["lua"] != 1 || byName["llm"] != 2 {
		t.Fatalf("层号未正确往返: %v", byName)
	}
}

// ★★★ 判据：**编码顺序 = 启动顺序**（按 (layer,name)，层内可复现）。
func TestGraphEncodeOrderIsStartupOrder(t *testing.T) {
	mods := planTestModules()
	built, _ := Build(mods)
	gf := decodeGraphOrFatal(t, EncodeGraph(planToEdgesForTest(built)))

	var order []string
	for _, e := range gf.Modules {
		order = append(order, e.Name)
	}
	// config 必须在 lua 之前，lua 必须在 llm 之前（按层号排好的顺序）
	idx := func(n string) int {
		for i, s := range order {
			if s == n {
				return i
			}
		}
		return -1
	}
	if !(idx("config") < idx("lua") && idx("lua") < idx("llm")) {
		t.Fatalf("★ 解码后的顺序不是启动顺序: %v", order)
	}
}

// ---- 测试辅助 ----

func planToEdgesForTest(p *Plan) []GraphEdge {
	var out []GraphEdge
	for layer, mods := range p.Layers {
		for _, m := range mods {
			out = append(out, GraphEdge{
				Name: m.Name(), Provides: m.Provides(),
				Requires: m.Requires(), Layer: layer,
			})
		}
	}
	return out
}

func decodeGraphOrFatal(t *testing.T, data []byte) *GraphFile {
	t.Helper()
	gf, err := DecodeGraph(data)
	if err != nil {
		t.Fatalf("DecodeGraph: %v", err)
	}
	return gf
}
