package memory

import (
	"os"
	"path/filepath"
	"testing"
)

// prodCopySrc 返回生产库副本路径。
//
// ★ 优先读环境变量 HA_PROD_DB；未设置时用一个**占位路径**，
//
//	  它在 CI 与绝大多数机器上都不存在 ⇒ 上层 os.ReadFile 失败 ⇒ t.Skip。
//
//	  取快照用（不要直接指生产库，判据会写）：
//		sqlite3 <生产库> ".backup /tmp/graph-copy.db"
//		HA_PROD_DB=/tmp/graph-copy.db go test -run ProdCopy ./internal/memory/
func prodCopySrc() string {
	if p := os.Getenv("HA_PROD_DB"); p != "" {
		return p
	}
	return "/data/homeagent-snapshots/graph.db"
}

// TestProdCopy_DeleteEntityRealData 拿**生产库副本**跑一次真实删除。
//
// ★ 为什么需要这条：合成判据的库结构与生产库不同，
//
//	而 2026-10-05 的 FOREIGN KEY 缺陷正是**只在真实数据分布下暴露**
//	（entities 与 relations 的 id 序列错开、且旧表确有被引用的行）。
//	「合成库全绿 + 生产库报错」——这次是真实发生的。
//
// 无生产库时自动跳过（CI 与开发机不该依赖本机路径）。
func TestProdCopy_DeleteEntityRealData(t *testing.T) {
	// ★ 库路径走环境变量 + 默认占位：CI 与别人的机器上不存在 ⇒ 自动 Skip。
	//
	//   为什么不写死本机路径：公开仓库里写死 `/home/<user>/...` 等于
	//   泄露本机目录结构，也让这条判据在别处永远跑不了。
	src := prodCopySrc()
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Skip("无生产库副本，跳过：" + err.Error())
	}
	p := filepath.Join(t.TempDir(), "g.db")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := NewGraphDB(p)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	var name string
	if err := g.db.QueryRow(`SELECT text_content FROM memory_blocks
	   WHERE text_content != '' AND modality='text' LIMIT 1`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	count := func(q string) int { var n int; g.db.QueryRow(q).Scan(&n); return n }
	q := func(s string) string {
		return `SELECT count(*) FROM memory_blocks WHERE text_content='` + s + `'`
	}
	nb := count(q(name))
	if nb == 0 {
		t.Fatalf("前提不成立：找不到同名块")
	}
	eb := count(`SELECT count(*) FROM memory_block_edges
	   WHERE (source_kind='block' AND source_id IN (SELECT id FROM memory_blocks WHERE text_content='` + name + `'))
	      OR (target_kind='block' AND target_id IN (SELECT id FROM memory_blocks WHERE text_content='` + name + `'))`)

	res, err := g.DeleteEntity(name)
	if err != nil {
		t.Fatalf("生产数据上删除失败（这正是 2026-10-05 修的 FOREIGN KEY 缺陷）：%v", err)
	}
	t.Logf("删除 %q：块 %d、边 %d；返回 %+v", name, nb, eb, res)
	if res.Blocks != nb {
		t.Errorf("回报 Blocks=%d，实际同名块 %d", res.Blocks, nb)
	}
	if after := count(q(name)); after != 0 {
		t.Errorf("同名块残留 %d", after)
	}
	var dangling int
	g.db.QueryRow(`SELECT count(*) FROM memory_block_edges e
	  WHERE (e.source_kind='block' AND NOT EXISTS(SELECT 1 FROM memory_blocks b WHERE b.id=e.source_id))
	     OR (e.target_kind='block' AND NOT EXISTS(SELECT 1 FROM memory_blocks b WHERE b.id=e.target_id))`).Scan(&dangling)
	if dangling != 0 {
		t.Errorf("删除后留下 %d 条悬空块边", dangling)
	}
}
