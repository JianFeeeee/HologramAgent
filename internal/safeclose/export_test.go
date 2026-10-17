package safeclose

import "sync"

// newOnce 仅供测试构造（同包外无法直接 new(sync.Once) 吗？其实可以，
// 这里保留一个显式入口让测试可读）。
func NewOnceForTest() *sync.Once { return &sync.Once{} }
