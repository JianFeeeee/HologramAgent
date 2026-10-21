package webui

import (
	"encoding/json"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// 星图数据源必须来自块体系（memory_blocks），而不是停写的旧表 entities。
//
// 判据：真实图谱里 memory_blocks 远多于 entities，且块侧仍在增长。
// 这里用合成数据钉住「有块时节点数 == 块数」，防未来又切回 nodes。
func TestGraphDataExposesBlocksForStarmap(t *testing.T) {
	// 模拟后端 GraphData 的返回：旧表 1 个节点，块表 3 个块 + 2 条边
	data := map[string]interface{}{
		"nodes": []map[string]interface{}{{"id": int64(1), "name": "旧表实体", "type": "Concept"}},
		"edges": []map[string]interface{}{},
		"memory_blocks": []memory.MemoryBlock{
			{ID: "blk_src_a", Modality: memory.BlockText, Text: "张三喜欢咖啡", Source: "sentence"},
			{ID: "blk_ent_b", Modality: memory.BlockText, Text: "张三", Source: "triple", SemanticType: "Person"},
			{ID: "blk_ent_c", Modality: memory.BlockText, Text: "咖啡", Source: "triple"},
		},
		"memory_block_edges": []memory.MemoryBlockEdge{
			{ID: 1, SourceKind: "block", SourceID: "blk_src_a", TargetKind: "block", TargetID: "blk_ent_b", Type: "contains"},
			{ID: 2, SourceKind: "block", SourceID: "blk_src_a", TargetKind: "block", TargetID: "blk_ent_c", Type: "contains"},
		},
	}
	out := graphDataForVisual(data)
	// 前端读的是 memory_blocks 字段
	blocks, ok := out["memory_blocks"]
	if !ok {
		t.Fatal("GraphData 必须下发 memory_blocks（前端星图的真实数据源）")
	}
	raw, _ := json.Marshal(blocks)
	var got []graphBlockView
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("memory_blocks 应下发 3 个，实际 %d", len(got))
	}
	if _, hasNodes := out["nodes"]; !hasNodes {
		t.Fatal("旧 nodes 字段应保留（向后兼容），只是前端不再依赖它")
	}
}
