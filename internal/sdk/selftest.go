package sdk

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/knowledge"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	coldcontext "github.com/JianFeeeee/HologramAgent/internal/memory/coldcontext"
	"github.com/JianFeeeee/HologramAgent/internal/memory/text"
)

// VirtualInstance 是完全隔离的虚拟存储集合，供内置插件（如 healthcheck）
// 做不污染生产存储的"写→查→删"往返自检。所有写入发生在独立临时目录，
// 由 Cleanup 统一销毁。
type VirtualInstance struct {
	Memory     MemoryAPI
	Knowledge  KnowledgeAPI
	DocMemory  DocMemoryAPI
	TextMemory TextMemoryAPI

	dir     string
	mu      sync.Mutex
	created bool
}

// newBaseDir 返回一个隔离的临时根目录（hot 前缀，避免与生产数据混淆）。
func newBaseDir(scope string) (string, error) {
	return os.MkdirTemp("", "homeagent_selftest_"+scope+"_")
}

// NewVirtualInstance 创建完全隔离的测试实例，使用独立临时目录。
func NewVirtualInstance(scope string) (*VirtualInstance, error) {
	if scope == "" {
		scope = "hc"
	}
	base, err := newBaseDir(scope)
	if err != nil {
		return nil, err
	}

	v := &VirtualInstance{dir: base, created: true}
	if err := v.initLocked(); err != nil {
		os.RemoveAll(base)
		return nil, err
	}
	return v, nil
}

// initLocked 初始化各隔离存储。调用方须持 v.mu。
func (v *VirtualInstance) initLocked() error {
	memDB, err := memory.NewGraphDB(filepath.Join(v.dir, "graph.db"))
	if err != nil {
		return fmt.Errorf("virtual graph db: %w", err)
	}
	v.Memory = NewGraphMemory(memDB)

	ks := knowledge.NewStore(filepath.Join(v.dir, "knowledge"))
	if err := ks.Start(); err != nil {
		return fmt.Errorf("virtual knowledge store: %w", err)
	}
	v.Knowledge = NewKnowledge(ks)

	ds := coldcontext.NewStore(filepath.Join(v.dir, "documents"), memory.TokenizeWords)
	if err := ds.Start(); err != nil {
		return fmt.Errorf("virtual doc store: %w", err)
	}
	v.DocMemory = NewDocMemory(ds)

	tm := text.New(filepath.Join(v.dir, "text"))
	if err := tm.Start(); err != nil {
		return fmt.Errorf("virtual text memory: %w", err)
	}
	v.TextMemory = NewTextMemory(tm)
	return nil
}

// Cleanup 关闭并销毁该虚拟实例的全部临时存储。之后实例不可再用。
func (v *VirtualInstance) Cleanup() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.created {
		return
	}
	if tm, ok := v.TextMemory.(*textMemoryImpl); ok && tm.tm != nil {
		tm.tm.Stop()
	}
	if ds, ok := v.DocMemory.(*docMemoryImpl); ok && ds.ds != nil {
		ds.ds.Stop()
	}
	if ks, ok := v.Knowledge.(*knowledgeImpl); ok && ks.ks != nil {
		ks.ks.Stop()
	}
	if gm, ok := v.Memory.(*graphMemory); ok && gm.db != nil {
		gm.db.Close()
	}
	os.RemoveAll(v.dir)
	v.created = false
}

// Reset 清理当前实例并重建一个全新的隔离实例，用于每轮自检前重置状态。
// 返回新实例；失败时旧实例已被清理、返回 err，调用方需重新创建。
func (v *VirtualInstance) Reset(scope string) (*VirtualInstance, error) {
	v.Cleanup()
	return NewVirtualInstance(scope)
}
