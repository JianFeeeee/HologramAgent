package mainagent

// statussections_memory.go —— 记忆三段的求值（数据句柄在主 agent 手里）。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/memory"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// memoryStatusOf 读图库统计（句柄给了才有一段）。
//
// `*memory.Indexer` 内部持图库；基座只认 `kernelapi.MemoryIndexer`，
// 所以这里做一次本地类型断言（不为它加接口）。
func memoryStatusOf(indexer kernelapi.MemoryIndexer) sdk.MemoryStatus {
	if idx, ok := indexer.(*memory.Indexer); ok && idx != nil {
		if db := idx.DB(); db != nil {
			return memoryStatusOfDB(db)
		}
	}
	return sdk.MemoryStatus{}
}

// memoryStatusOfDB 把图库 Introspect 结果转成状态段。
func memoryStatusOfDB(db kernelapi.GraphMemory) sdk.MemoryStatus {
	if db == nil {
		return sdk.MemoryStatus{}
	}
	st := sdk.MemoryStatus{Available: true}
	if i, err := db.Introspect(); err == nil && i != nil {
		readInt := func(k string) int {
			switch v := i[k].(type) {
			case int:
				return v
			case int64:
				return int(v)
			case float64:
				return int(v)
			}
			return 0
		}
		// 口径与 `GraphDB.Introspect` 的字段名对齐（entity_count / relation_count）。
		st.EntityCount = readInt("entity_count")
		st.RelationCount = readInt("relation_count")
	}
	return st
}

// documentsStatusOf 把冷文档归档的统计转成状态段。
func documentsStatusOf(store kernelapi.DocStore) sdk.DocumentStatus {
	if store == nil {
		return sdk.DocumentStatus{}
	}
	st := sdk.DocumentStatus{Available: true}
	stats := store.Stats()
	if v, ok := stats["doc_count"].(int); ok {
		st.DocCount = v
	}
	if v, ok := stats["index_count"].(int); ok {
		st.VectorCount = v
	}
	return st
}
