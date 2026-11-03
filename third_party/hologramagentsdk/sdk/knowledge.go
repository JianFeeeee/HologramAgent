package sdk

// KnowledgeAPI provides access to the knowledge store.
type KnowledgeAPI interface {
	Search(query string, topK int) ([]*Knowledge, error)
	Add(name, content string) error
	List() ([]string, error)
}

// Knowledge represents a knowledge entry.
type Knowledge struct {
	Name string `json:"name"`
	// Category 是该条目的父分类路径（如 "tech/go"），根下条目为空。
	//
	// 为何加这个字段：对外服务（kbtree）要做**暴露范围过滤**就必须知道
	// 每条结果属于哪个分类 —— 过滤只能发生在服务端（客户端过滤等于
	// 没过滤，范围外内容已经随响应发出去了）。
	// 之前这里只有 Name/Content，内核明明返回了 Category 却在
	// knowledge_impl.SearchIn 的拷贝里丢掉，导致外部无法按分类判定。
	Category string `json:"category,omitempty"`
	Content  string `json:"content"`
}
