package pluginhost

import "sync"

// rwMutex 是 sync.RWMutex 的薄包装。
//
// 为什么不直接用 sync.RWMutex：PluginCatalog 要能被**复制/传递**时
// 保持清晰的所有权语义（指针字段而非嵌入值），避免"复制了含锁的结构体"
// 这个经典的 vet 警告（copylocks）。
type rwMutex struct{ mu sync.RWMutex }

func newRWMutex() *rwMutex { return &rwMutex{} }

func (m *rwMutex) RLock()   { m.mu.RLock() }
func (m *rwMutex) RUnlock() { m.mu.RUnlock() }
func (m *rwMutex) Lock()    { m.mu.Lock() }
func (m *rwMutex) Unlock()  { m.mu.Unlock() }
