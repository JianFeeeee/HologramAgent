package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JianFeeeee/HomeAgent/internal/memory"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
	pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

// fakeGraphMemory 是一个最小 MemoryAPI 桩，只为把 GraphData 喂给 handler。
type fakeGraphMemory struct {
	data map[string]interface{}
}

func (f *fakeGraphMemory) Recall([]string, int) ([]pubsdk.Entity, []pubsdk.Relation, error) {
	return nil, nil, nil
}
func (f *fakeGraphMemory) Commit([]pubsdk.Triple) error { return nil }
func (f *fakeGraphMemory) Introspect() (map[string]interface{}, error) {
	return nil, nil
}
func (f *fakeGraphMemory) MergeEntities(string, string) (int, error) { return 0, nil }
func (f *fakeGraphMemory) Purge(map[string]string, string) (int, error) {
	return 0, nil
}
func (f *fakeGraphMemory) GraphData() (map[string]interface{}, error) { return f.data, nil }

// BlocksChangedSince 是 pulse 端点现在的数据源（不再走 GraphData 全量 + 过滤）。
//
// 桩要**忠实反映过滤语义**：只回窗口内的块。若这里偷懒返回全部，
// 就把「窗口过滤」这条判据架空了——测的是桩，不是产品。
func (f *fakeGraphMemory) BlocksChangedSince(since time.Time, limit int) ([]sdk.MemoryBlockView, error) {
	blocks, _ := f.data["memory_blocks"].([]memory.MemoryBlock)
	var out []sdk.MemoryBlockView
	for _, b := range blocks {
		if b.UpdatedAt.Before(since) && b.CreatedAt.Before(since) {
			continue
		}
		out = append(out, sdk.MemoryBlockView{
			ID: b.ID, Modality: string(b.Modality), Text: b.Text,
			HasData: len(b.Data) > 0, MIME: b.MIME,
			Source: b.Source, Tool: b.Tool, Scene: b.Scene,
			CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt,
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// graphFixture 造一份与生产实例同构的图谱快照：
// 节点带 updated_at（pulse 端点按它过滤），memory_blocks 带稠密 vector。
func graphFixture() map[string]interface{} {
	now := time.Now()
	old := now.Add(-72 * time.Hour)
	return map[string]interface{}{
		"nodes": []map[string]interface{}{
			{"id": 1, "name": "小宅", "type": "Concept", "mention_count": 228,
				"created_at": old, "updated_at": now.Add(-8 * time.Hour)},
			{"id": 2, "name": "刚刚学到的东西", "type": "Concept", "mention_count": 1,
				"created_at": now, "updated_at": now},
		},
		"edges": []map[string]interface{}{},
		// ★ memory_blocks 才是 pulse 的过滤源（2026-10-05 修）。
		//   刻意放两个：一个 8h 前（默认 900s 窗口外）、一个刚更新（窗口内）。
		//   旧节点（nodes/小宅）的 updated_at 也刻意设成 8h 前 ——
		//   修复前 pulse 读的是它，而它永远落在窗口外 ⇒ 恒返回空。
		"memory_blocks": []memory.MemoryBlock{
			{ID: "b_old", Modality: memory.BlockModality("text"), Text: "老块",
				Vector: []float64{0.1, 0.2, 0.3},
				CreatedAt: old, UpdatedAt: now.Add(-8 * time.Hour)},
			{ID: "b_new", Modality: memory.BlockModality("image"), Text: "刚学到的东西",
				Data: []byte("png-bytes"), MIME: "image/png",
				Vector: []float64{0.1, 0.2, 0.3},
				CreatedAt: now, UpdatedAt: now},
		},
	}
}

func newGraphHandler(m *fakeGraphMemory) *Handler {
	return &Handler{memory: m}
}

// TestHandleMemoryGraph_StripsVector 钉住「不下发稠密向量」。
//
// 生产实测：8 块记忆的 vector 占 79,314 B / 408,146 B = 19%，而星图
// （本接口唯一消费者）从不读 vector。这部分纯属白付带宽 + 堆内存。
func TestHandleMemoryGraph_StripsVector(t *testing.T) {
	h := newGraphHandler(&fakeGraphMemory{data: graphFixture()})
	rr := httptest.NewRecorder()
	h.handleMemoryGraph(rr, httptest.NewRequest(http.MethodGet, "/api/v1/memory/graph", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if bytes.Contains(rr.Body.Bytes(), []byte(`"vector"`)) {
		t.Fatalf("response still contains vector field:\n%s", rr.Body.String())
	}
	// 其余字段必须还在（不能顺手把整个 memory_blocks 删掉）。
	for _, want := range []string{`"has_data":true`, `"id":"b_new"`, `"nodes"`, `"edges"`} {
		if !bytes.Contains(rr.Body.Bytes(), []byte(want)) {
			t.Fatalf("response missing %s:\n%s", want, rr.Body.String())
		}
	}
}

// TestHandleMemoryGraphPulse_OnlyRecent 钉住 pulse 端点只回窗口内变动的节点。
//
// 这是星图「新记忆生长」的数据源；它必须比全量图谱小两个数量级。
func TestHandleMemoryGraphPulse_OnlyRecent(t *testing.T) {
	h := newGraphHandler(&fakeGraphMemory{data: graphFixture()})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/memory/graph/pulse", nil)
	h.handleMemoryGraphPulse(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Nodes []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success {
		t.Fatalf("success = false")
	}
	// 默认窗口 900s：8h 前的「小宅」不该在里面，刚更新的应该在。
	if len(resp.Data.Nodes) != 1 {
		t.Fatalf("expected 1 recent node, got %d: %+v", len(resp.Data.Nodes), resp.Data.Nodes)
	}
	if resp.Data.Nodes[0].ID != "b_new" {
		t.Fatalf("expected block id=b_new, got %q", resp.Data.Nodes[0].ID)
	}
}

// TestHandleMemoryGraphPulse_SinceWidens 钉住 since 参数能放大窗口。
func TestHandleMemoryGraphPulse_SinceWidens(t *testing.T) {
	h := newGraphHandler(&fakeGraphMemory{data: graphFixture()})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/memory/graph/pulse?since=86400", nil)
	h.handleMemoryGraphPulse(rr, req)
	var resp struct {
		Data struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data.Nodes) != 2 {
		t.Fatalf("since=86400 should include both nodes, got %d", len(resp.Data.Nodes))
	}
}

// TestHandleMemoryGraphPulse_NoVector 钉住 pulse 响应同样不带 vector。
func TestHandleMemoryGraphPulse_NoVector(t *testing.T) {
	h := newGraphHandler(&fakeGraphMemory{data: graphFixture()})
	rr := httptest.NewRecorder()
	h.handleMemoryGraphPulse(rr, httptest.NewRequest(http.MethodGet, "/api/v1/memory/graph/pulse", nil))
	if bytes.Contains(rr.Body.Bytes(), []byte(`"vector"`)) {
		t.Fatalf("pulse response contains vector:\n%s", rr.Body.String())
	}
}

// TestGraphDataForVisual_PassthroughWhenTypeMismatch 钉住断言失败时不吞数据。
//
// graphDataForVisual 用类型断言识别 memory_blocks。若 GraphData 换了
// 返回类型（比如改成 []*MemoryBlock），断言不中时必须原样透传而不是
// 把字段弄丢 —— 宁可多发 vector，也不能让整个图谱接口变空。
func TestGraphDataForVisual_PassthroughWhenTypeMismatch(t *testing.T) {
	in := map[string]interface{}{
		"nodes":         []map[string]interface{}{{"id": 1}},
		"memory_blocks": []*memory.MemoryBlock{{ID: "x"}},
	}
	out := graphDataForVisual(in)
	if out["memory_blocks"] == nil {
		t.Fatalf("memory_blocks dropped on type mismatch")
	}
	if _, ok := out["nodes"]; !ok {
		t.Fatalf("nodes dropped")
	}
}

// TestGraphPulse_IgnoresFrozenLegacyNodes 钉住 pulse 的**过滤源**是块表而非旧表。
//
// ★ 这条是 2026-10-05 清理时补的，针对一个真实的生产静默失效：
//
//	/memory/graph/pulse 恒返回 {"nodes":[]}，
//	而 /memory/graph 同一时刻有 1294 个节点。
//
// 根因：pulse 原实现按 `out["nodes"]` 的 updated_at 过滤，而 nodes 来自
// **旧表 entities**（graph.go:1445）——旧表停双写后不再增长，
// 生产最后更新停在 2026-10-03，而 pulse 默认窗口只有 900s
// ⇒ 1294 个节点**永远**落在窗口外 ⇒ 恒空。
// 症状形态：HTTP 200 + success:true，无任何报错。
//
// ★ 为什么上面两条判据抓不住：它们的 fixture 里 nodes 与 memory_blocks
//
//	「恰好都有一条落在窗口内」，所以读哪个源都能过。
//	这条刻意让**旧表节点全部落在窗口外、块有一个在窗口内**，
//	于是「读错源」与「读对源」的结果**必然不同**。
func TestGraphPulse_IgnoresFrozenLegacyNodes(t *testing.T) {
	now := time.Now()
	frozen := now.Add(-72 * time.Hour) // 旧表：三天前，之后再没更新过

	fixture := map[string]interface{}{
		// 旧表节点：全部在窗口外（模拟停双写后的冻结状态）
		"nodes": []map[string]interface{}{
			{"id": 1, "name": "旧实体", "type": "Concept", "mention_count": 228,
				"created_at": frozen, "updated_at": frozen},
		},
		"edges": []map[string]interface{}{},
		// 块表：有一条刚更新（活图谱的真实形态）
		"memory_blocks": []memory.MemoryBlock{
			{ID: "b_live", Modality: memory.BlockModality("text"), Text: "刚写入的块",
				CreatedAt: now, UpdatedAt: now},
		},
	}

	h := newGraphHandler(&fakeGraphMemory{data: fixture})
	rr := httptest.NewRecorder()
	h.handleMemoryGraphPulse(rr, httptest.NewRequest(http.MethodGet, "/api/v1/memory/graph/pulse", nil))

	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Nodes []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, rr.Body.String())
	}
	if !resp.Success {
		t.Fatal("success = false")
	}
	if len(resp.Data.Nodes) != 1 {
		t.Fatalf("旧表节点已冻结三天、块表有一条刚更新 ⇒ 应回 1 个（来自块表），"+
			"实际 %d 个：%+v\n★ 若为 0，说明 pulse 又退回读旧表了 —— "+
			"生产上这表现为「星图不跟随 agent 动」且无任何报错",
			len(resp.Data.Nodes), resp.Data.Nodes)
	}
	if resp.Data.Nodes[0].ID != "b_live" {
		t.Fatalf("应回块表的 b_live，实际 %q", resp.Data.Nodes[0].ID)
	}
}
