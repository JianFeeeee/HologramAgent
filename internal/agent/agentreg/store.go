// Package agentreg 是 **agent 记录表**的实现（唯一事实来源，见 §3.2.2）。
//
// ## 它替换掉的是四张表
//
//	security/outputs.go      outputsByAgent   输出通道白名单
//	security/principal.go    principals.byID  子工具 allow/deny
//	agent/instances.go       recs             创建参数（软重启/持久化）
//	agent/child/registry.go  存活实例登记      （保留：它管的是**对象**，不是事实）
//
// 同一个事件（"某个 agent 实例不在了"）原先要触发四处清理，散在三个包、
// 三条路径。漏一处就是残留：同名重建继承上一代的输出授权、继承上一代的
// 工具策略、表单调增长。今天的正确性**靠人记得**。
//
// 现在：一次 `Remove(id)` 删掉全部事实。
//
// ## 为什么实现住在 internal/agent 而不是内核模块里
//
// 它只依赖 `kernelapi` + `kernelapi/model`（纯数据与只读接口），
// 不知道 agent 类型、不知道 eventloop —— 所以它可以被**任何**模块装配，
// 包括在 security 之前（security 只读 `AgentIndex`）。
package agentreg

import (
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
)

// Store 是记录表（并发安全）。
type Store struct {
	mu   sync.RWMutex
	recs map[string]*model.AgentRecord
	// onChange 是变更回调（持久化用；nil = 不落盘）。
	onChange func([]model.AgentRecord)
}

// 编译期确认接口实现。
var (
	_ kernelapi.AgentRegistry = (*Store)(nil)
	_ kernelapi.AgentIndex    = (*Store)(nil)
)

// New 构造一张空表。
func New() *Store { return &Store{recs: map[string]*model.AgentRecord{}} }

// SetOnChange 设置变更回调（装配期一次；用于落盘）。
func (s *Store) SetOnChange(fn func([]model.AgentRecord)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// Register 登记（或覆盖）一条记录。
func (s *Store) Register(rec model.AgentRecord) error {
	if s == nil || rec.ID == "" {
		return nil
	}
	cp := rec
	s.mu.Lock()
	s.recs[cp.ID] = &cp
	cb := s.onChange
	recs := s.snapshotLocked()
	s.mu.Unlock()
	if cb != nil {
		cb(recs)
	}
	return nil
}

// Update 就地改一条记录（不存在时忽略 —— 与"漏登记"同语义：没有事实就没有约束）。
func (s *Store) Update(id string, fn func(*model.AgentRecord)) {
	if s == nil || id == "" || fn == nil {
		return
	}
	s.mu.Lock()
	rec, ok := s.recs[id]
	if ok {
		fn(rec)
	}
	cb := s.onChange
	recs := s.snapshotLocked()
	s.mu.Unlock()
	if ok && cb != nil {
		cb(recs)
	}
}

// Remove 删除一条记录（**唯一的清理点**）。
func (s *Store) Remove(id string) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	_, ok := s.recs[id]
	delete(s.recs, id)
	cb := s.onChange
	recs := s.snapshotLocked()
	s.mu.Unlock()
	if ok && cb != nil {
		cb(recs)
	}
}

// Facts 返回某 agent 的父声明事实（不存在 ⇒ ok=false，调用方按 fail-open 处理）。
func (s *Store) Facts(id string) (model.AgentFacts, bool) {
	if s == nil || id == "" {
		return model.AgentFacts{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.recs[id]
	if !ok {
		return model.AgentFacts{}, false
	}
	// 拷贝切片：调用方可能长期持有，而登记表是可变的。
	f := rec.Facts
	f.AllowedOutputs = clone(f.AllowedOutputs)
	f.AllowedTools = clone(f.AllowedTools)
	f.DenyTools = clone(f.DenyTools)
	return f, true
}

// Present 报告某 agent 是否有记录。
func (s *Store) Present(id string) bool {
	if s == nil || id == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.recs[id]
	return ok
}

// Records 返回全部记录的深拷贝（状态面与持久化用）。
func (s *Store) Records() []model.AgentRecord {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked()
}

// record 取一条记录（内部用，返回拷贝）。
func (s *Store) record(id string) (model.AgentRecord, bool) {
	if s == nil || id == "" {
		return model.AgentRecord{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.recs[id]
	if !ok {
		return model.AgentRecord{}, false
	}
	return *rec, true
}

// snapshotLocked 生成记录快照（调用方须持锁）。
func (s *Store) snapshotLocked() []model.AgentRecord {
	if len(s.recs) == 0 {
		return nil
	}
	out := make([]model.AgentRecord, 0, len(s.recs))
	for _, r := range s.recs {
		cp := *r
		cp.Arg = cloneArg(r.Arg)
		cp.Facts.AllowedOutputs = clone(r.Facts.AllowedOutputs)
		cp.Facts.AllowedTools = clone(r.Facts.AllowedTools)
		cp.Facts.DenyTools = clone(r.Facts.DenyTools)
		out = append(out, cp)
	}
	return out
}

func clone(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func cloneArg(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}

	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
