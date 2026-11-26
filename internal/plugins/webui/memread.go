package webui

// memread.go —— WebUI 记忆面板**自己读库**（docs §12.1）。
//
// ## 为什么不再经 `sdk.MemoryAPI`
//
// 按 docs §12：插件不许依赖内核记忆面。路径规则是公开的：
//
//	<data_dir>/memory/graph.db        主图库（SQLite）
//	<data_dir>/memory/text/           文本记忆（JSONL）
//	<data_dir>/memory/documents/      冷文档
//
// WebUI 是**只读观察者**：一律以 `mode=ro` 打开。写记忆（commit/purge）
// 归 agent 的工具面（`graph_commit`）与内核自己的整理流水线，不是面板的职责。

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// memDBPath 按 docs §12.2 的规则拼出图库路径。
func memDBPath(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "memory", "graph.db")
}

// openMemRO 以**只读**打开图库。
func openMemRO(dataDir string) (*sql.DB, error) {
	p := memDBPath(dataDir)
	if p == "" {
		return nil, fmt.Errorf("数据目录未配置")
	}
	db, err := sql.Open("sqlite3", p+"?mode=ro&_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("打开图库失败（%s）: %w", p, err)
	}
	return db, nil
}

// memEntityRow / memRelationRow 是面板的输出形状。
type memEntityRow struct {
	Name         string `json:"name"`
	Type         string `json:"type,omitempty"`
	MentionCount int    `json:"mention_count,omitempty"`
}

type memRelationRow struct {
	SourceName   string  `json:"source_name"`
	TargetName   string  `json:"target_name"`
	RelationType string  `json:"relation_type"`
	Confidence   float64 `json:"confidence,omitempty"`
}

// recallMemory 按关键词查实体与关系（只读 SQL）。
func recallMemory(dataDir string, keywords []string, depth int) ([]memEntityRow, []memRelationRow, error) {
	db, err := openMemRO(dataDir)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	if len(keywords) == 0 {
		return nil, nil, fmt.Errorf("需要关键词")
	}
	like := "%" + keywords[0] + "%"
	rows, err := db.Query(
		`SELECT name, COALESCE(type,''), COALESCE(mention_count,0)
		   FROM entities WHERE name LIKE ? ORDER BY mention_count DESC LIMIT 80`, like)
	if err != nil {
		return nil, nil, err
	}
	var entities []memEntityRow
	for rows.Next() {
		var e memEntityRow
		if err := rows.Scan(&e.Name, &e.Type, &e.MentionCount); err != nil {
			rows.Close()
			return nil, nil, err
		}
		entities = append(entities, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	rels, err := db.Query(
		`SELECT COALESCE(se.name,''), COALESCE(te.name,''), COALESCE(r.relation_type,''), COALESCE(r.confidence,0)
		   FROM relations r
		   LEFT JOIN entities se ON se.id = r.source_id
		   LEFT JOIN entities te ON te.id = r.target_id
		  WHERE se.name LIKE ? OR te.name LIKE ? LIMIT 200`, like, like)
	if err != nil {
		return entities, nil, nil
	}
	defer rels.Close()
	var relations []memRelationRow
	for rels.Next() {
		var r memRelationRow
		if err := rels.Scan(&r.SourceName, &r.TargetName, &r.RelationType, &r.Confidence); err != nil {
			continue
		}
		relations = append(relations, r)
	}
	_ = depth
	return entities, relations, nil
}

// graphOverview 返回图库规模（面板总览）。
func graphOverview(dataDir string) (map[string]interface{}, error) {
	db, err := openMemRO(dataDir)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	out := map[string]interface{}{"path": memDBPath(dataDir)}
	for _, t := range []string{"entities", "relations", "sentences", "memory_blocks"} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + t).Scan(&n); err == nil {
			out[t] = n
		}
	}
	return out, nil
}

// textMemEvent 是文本记忆的一条记录（JSONL 一行）。
type textMemEvent struct {
	Timestamp int64    `json:"timestamp"`
	Source    string   `json:"source"`
	Input     string   `json:"input"`
	Response  string   `json:"response,omitempty"`
	ToolsUsed []string `json:"tools_used,omitempty"`
	AgentID   string   `json:"agent_id,omitempty"`
}

// recentTextMemory 读最近 n 条文本记忆（自己扫 JSONL）。
func recentTextMemory(dataDir string, n int) ([]textMemEvent, map[string]interface{}, error) {
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
	stats := map[string]interface{}{"files": len(files), "total_size": totalBytes, "path": dir}

	var out []textMemEvent
	for _, name := range files {
		if len(out) >= n {
			break
		}
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var lines []textMemEvent
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			var ev textMemEvent
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

// graphDataFromDB 自己从 SQLite 拼出图谱数据（面板只读）。
func graphDataFromDB(dataDir string) (map[string]interface{}, error) {
	db, err := openMemRO(dataDir)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	out := map[string]interface{}{}
	entRows, err := db.Query(`SELECT name, COALESCE(type,''), COALESCE(mention_count,0) FROM entities`)
	if err == nil {
		var ents []memEntityRow
		for entRows.Next() {
			var e memEntityRow
			if err := entRows.Scan(&e.Name, &e.Type, &e.MentionCount); err == nil {
				ents = append(ents, e)
			}
		}
		entRows.Close()
		out["entities"] = ents
	}
	relRows, err := db.Query(`SELECT COALESCE(se.name,''), COALESCE(te.name,''), COALESCE(r.relation_type,''), COALESCE(r.confidence,0)
	    FROM relations r LEFT JOIN entities se ON se.id=r.source_id LEFT JOIN entities te ON te.id=r.target_id`)
	if err == nil {
		var rels []memRelationRow
		for relRows.Next() {
			var r memRelationRow
			if err := relRows.Scan(&r.SourceName, &r.TargetName, &r.RelationType, &r.Confidence); err == nil {
				rels = append(rels, r)
			}
		}
		relRows.Close()
		out["relations"] = rels
	}
	return out, nil
}

// blocksChangedSince 查窗口内变动的记忆块（星图轻量轮询用）。
func blocksChangedSince(dataDir string, since int, limit int) ([]map[string]interface{}, error) {
	db, err := openMemRO(dataDir)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, COALESCE(modality,''), COALESCE(text,''), COALESCE(mime,''), COALESCE(source,''), COALESCE(tool,''), COALESCE(scene,'')
	    FROM memory_blocks WHERE updated_at >= datetime('now', ?) ORDER BY updated_at DESC LIMIT ?`,
		fmt.Sprintf("-%d seconds", since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]interface{}
	for rows.Next() {
		var id, modality, text, mime, source, tool, scene string
		if err := rows.Scan(&id, &modality, &text, &mime, &source, &tool, &scene); err != nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"id": id, "modality": modality, "text": text,
			"mime": mime, "source": source, "tool": tool, "scene": scene,
		})
	}
	return out, nil
}
