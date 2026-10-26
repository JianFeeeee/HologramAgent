package core

// N2c 验收：**轻量内核 profile**。
//
// 设计 docs/zh/resident-subagent-design.md §16.0（窄接口 + nil 即禁用）与 §5.5。
//
// 轻量内核（驻留子）的记忆装配：
//   - graph = *memory.LightMemory（读 temp∪main，只写 temp）
//   - memory = nil ⇒ 既有的 `if a.memory != nil` 关卡自动禁掉**全部**整理面：
//     记忆整理流水线（distill.go 的 archive/review/merge 循环）、记忆块与媒体桥
//     （graphmedia.go / medialoop.go）、记忆整理工具（merge/delete/purge/edit/block_merge）
//
// 因此这里要钉住四件事：
//   ① 子的写入只落 temp，主库不受影响；
//   ② 子的读是并集（看得到主库 + 自己的 temp）；
//   ③ 整理类工具**不进子的工具表**；
//   ④ 即便被直调，整理类操作也明确报"轻量内核不支持"（纵深防御，不静默降级）。

import (
	"path/filepath"
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// newLightAgent 构造一个轻量内核 agent（驻留子形态）：有 LightMemory，没有整理面。
func newLightAgent(t *testing.T, main *memory.GraphDB, tempPath string) *Agent {
	t.Helper()
	light, err := memory.NewLightMemory(main, tempPath, true)
	if err != nil {
		t.Fatal(err)
	}
	a := New(AgentConfig{
		ID:              "sub-1",
		Provider:        &scriptProvider{},
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
		LightMemory:     light, // 轻量内核：只给图记忆共同面
	})
	t.Cleanup(func() { light.Close() })
	return a
}

func TestLightProfile_MemoryFaceWiring(t *testing.T) {
	dir := t.TempDir()
	main, err := memory.NewGraphDB(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()
	if _, _, err := main.Commit([]memory.Triple{
		{Subject: "主记忆实体", Relation: "属于", Object: "主库"},
	}, "sess", 1); err != nil {
		t.Fatal(err)
	}

	a := newLightAgent(t, main, filepath.Join(dir, "sub.db"))

	// ① 整理面必须为 nil —— 这正是"nil 即禁用"的开关。
	if a.memory != nil {
		t.Fatal("轻量内核不该有整理面（a.memory 必须为 nil）")
	}
	if a.graphMem() == nil {
		t.Fatal("轻量内核必须有图记忆共同面")
	}

	// ② 写入只落 temp：主库不得出现子才知道的实体。
	if _, _, _, err := a.commitTriplesWithMedia([]memory.Triple{
		{Subject: "子独有实体", Relation: "来自", Object: "子的temp"},
	}, "sess", 1, nil); err != nil {
		t.Fatalf("子的写入应成功（落 temp）: %v", err)
	}
	mainRes, err := main.Recall([]string{"子独有实体"}, nil, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range mainRes.Entities {
		if e.Name == "子独有实体" {
			t.Fatalf("子的写入不该进主库：%v", e.Name)
		}
	}

	// ③ 读是并集：主库的实体与 temp 的实体都要看得到。
	got := a.executeMemoryTool(agentAPI.ToolCall{
		ID: "c1", Name: "memory_recall",
		Arguments: map[string]interface{}{"query_intent": "主记忆实体,子独有实体"},
	}, nil)
	if !strings.Contains(got, "主记忆实体") {
		t.Fatalf("子应看得到主记忆：%s", got)
	}
	if !strings.Contains(got, "子独有实体") {
		t.Fatalf("子应看得到自己的 temp：%s", got)
	}
}

func TestLightProfile_OrganizeToolsAbsentAndRefused(t *testing.T) {
	dir := t.TempDir()
	main, err := memory.NewGraphDB(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()

	a := newLightAgent(t, main, filepath.Join(dir, "sub.db"))

	// ① 整理类工具不进子的工具表（不是进去再报不可用）。
	names := toolNames(a)
	for _, banned := range []string{
		"memory_merge", "memory_delete_entity", "memory_block_merge",
		"memory_purge", "memory_edit",
	} {
		if hasTool(names, banned) {
			t.Fatalf("轻量内核不该声明整理类工具 %s：%v", banned, names)
		}
	}
	// 对照：共同面/无关能力照常在。
	if !hasTool(names, "input_channels") {
		t.Fatalf("轻量内核仍应有共同面工具：%v", names)
	}

	// ② 纵深防御：即便被直调，整理类操作也必须明确报"轻量内核不支持"。
	for _, tool := range []string{
		"memory_merge", "memory_delete_entity", "memory_block_merge",
		"memory_purge", "memory_edit", "memory_introspect",
	} {
		got := a.executeMemoryTool(agentAPI.ToolCall{
			ID: "x", Name: tool,
			Arguments: map[string]interface{}{
				"name": "任意", "source": "a", "target": "b", "criteria": map[string]interface{}{},
			},
		}, nil)
		if !strings.Contains(got, "lightweight kernel") {
			t.Fatalf("%s 在轻量内核里必须明确报不支持，实际 %q", tool, got)
		}
	}
}

// 对照：完整内核（根 agent）仍有整理面与整理工具。
func TestFullProfile_KeepsOrganizeFace(t *testing.T) {
	dir := t.TempDir()
	main, err := memory.NewGraphDB(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()

	a := New(AgentConfig{
		ID:              "root",
		Provider:        &scriptProvider{},
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
		Memory:          main,
	})
	if a.memory == nil {
		t.Fatal("根 agent 必须有整理面")
	}
	if a.graphMem() == nil {
		t.Fatal("根 agent 必须有图记忆共同面")
	}
	names := toolNames(a)
	if !hasTool(names, "memory_merge") {
		t.Fatalf("根 agent 应保留整理类工具：%v", names)
	}
}
