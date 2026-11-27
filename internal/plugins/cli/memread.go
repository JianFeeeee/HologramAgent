package cli

// memread.go —— CLI 的 `/memory` 子命令**自己读库**（docs §12）。
//
// ## 为什么不再经 `sdk.MemoryAPI`
//
// 按 docs §12.1：插件不许依赖内核记忆面。路径规则是公开的：
//
//	<data_dir>/memory/graph.db        主图库（SQLite）
//	<data_dir>/memory/text/           文本记忆（目录）
//	<data_dir>/memory/documents/      冷文档（目录）
//
// 插件自己按规则打开、自己查、自己关 —— 于是"内核有几种 agent 类型"
// 与"插件能不能看记忆"彻底解耦。

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// memPath 按 docs §12.2 的规则拼出图库路径。
func memPath(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "memory", "graph.db")
}

// openMemRO 以**只读**打开图库（CLI 是观察者，不该拿到写句柄）。
func openMemRO(dataDir string) (*sql.DB, error) {
	p := memPath(dataDir)
	if p == "" {
		return nil, fmt.Errorf("数据目录未配置")
	}
	db, err := sql.Open("sqlite3", p+"?mode=ro&_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("打开图库失败（路径 %s）: %w", p, err)
	}
	return db, nil
}

// memEntity / memRelation 是 `/memory query` 的输出形状。
type memEntity struct {
	Name         string `json:"name"`
	Type         string `json:"type,omitempty"`
	MentionCount int    `json:"mention_count,omitempty"`
}

type memRelation struct {
	SourceName   string  `json:"source_name"`
	TargetName   string  `json:"target_name"`
	RelationType string  `json:"relation_type"`
	Confidence   float64 `json:"confidence,omitempty"`
}

// queryMemory 按关键词查实体与关系（**只读 SQL**，不碰内核实现）。
func queryMemory(dataDir string, keywords []string, depth int) ([]memEntity, []memRelation, error) {
	db, err := openMemRO(dataDir)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	if len(keywords) == 0 {
		return nil, nil, fmt.Errorf("需要关键词")
	}
	// 实体：按名字模糊匹配，按 mention_count 降序（与召回的相关性口径一致）。
	like := "%" + keywords[0] + "%"
	rows, err := db.Query(
		`SELECT name, COALESCE(type,''), COALESCE(mention_count,0)
		   FROM entities WHERE name LIKE ? ORDER BY mention_count DESC LIMIT 50`, like)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var entities []memEntity
	names := map[string]bool{}
	for rows.Next() {
		var e memEntity
		if err := rows.Scan(&e.Name, &e.Type, &e.MentionCount); err != nil {
			return nil, nil, err
		}
		entities = append(entities, e)
		names[e.Name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(entities) == 0 {
		return nil, nil, nil
	}

	// 关系：任一端的实体在上面命中即可。
	rels, err := db.Query(
		`SELECT COALESCE(se.name,''), COALESCE(te.name,''), COALESCE(r.relation_type,''), COALESCE(r.confidence,0)
		   FROM relations r
		   LEFT JOIN entities se ON se.id = r.source_id
		   LEFT JOIN entities te ON te.id = r.target_id
		  WHERE se.name LIKE ? OR te.name LIKE ? LIMIT 100`, like, like)
	if err != nil {
		return entities, nil, nil // 关系表结构差异不该让 query 整体失败
	}
	defer rels.Close()
	var relations []memRelation
	for rels.Next() {
		var r memRelation
		if err := rels.Scan(&r.SourceName, &r.TargetName, &r.RelationType, &r.Confidence); err != nil {
			continue
		}
		relations = append(relations, r)
	}
	_ = depth
	_ = names
	return entities, relations, nil
}

// memoryStats 返回图库的规模（`/memory graph`）。
func memoryStats(dataDir string) (map[string]interface{}, error) {
	db, err := openMemRO(dataDir)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	out := map[string]interface{}{"path": memPath(dataDir)}
	for _, t := range []string{"entities", "relations", "sentences", "memory_blocks"} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + t).Scan(&n); err == nil {
			out[t] = n
		}
	}
	var ts int64
	if err := db.QueryRow("SELECT COALESCE(MAX(created_at),0) FROM entities").Scan(&ts); err == nil && ts > 0 {
		out["last_write"] = time.Unix(ts, 0).Format(time.RFC3339)
	}
	return out, nil
}

// textEvent 是文本记忆的一条原始 I/O 记录（JSONL 一行）。
type textEvent struct {
	Timestamp int64    `json:"timestamp"`
	Source    string   `json:"source"`
	Input     string   `json:"input"`
	Response  string   `json:"response,omitempty"`
	ToolsUsed []string `json:"tools_used,omitempty"`
	AgentID   string   `json:"agent_id,omitempty"`
}

// recentTextEvents 读最近 n 条文本记忆（**自己扫 JSONL**，不经内核 API）。
//
// 路径规则见 docs §12.2：`<data_dir>/memory/text/*.jsonl`（按时间旋转，
// 文件名含时间戳 ⇒ 按名字倒序即时间倒序）。
func recentTextEvents(dataDir string, n int) ([]textEvent, map[string]interface{}, error) {
	dir := filepath.Join(dataDir, "memory", "text")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("读取文本记忆目录失败（%s）: %w", dir, err)
	}
	var files []string
	var totalBytes int64
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		files = append(files, e.Name())
		if info, err := e.Info(); err == nil {
			totalBytes += info.Size()
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))

	stats := map[string]interface{}{
		"files":      len(files),
		"total_size": totalBytes,
		"path":       dir,
	}
	var out []textEvent
	for _, name := range files {
		if len(out) >= n {
			break
		}
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		// 一个文件可能很大：只要尾部 n 条，逐行读后取末 n 条（文件通常不大）。
		var lines []textEvent
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			var ev textEvent
			if err := json.Unmarshal(sc.Bytes(), &ev); err == nil {
				lines = append(lines, ev)
			}
		}
		f.Close()
		for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
			out = append(out, lines[i])
		}
	}
	return out, stats, nil
}
