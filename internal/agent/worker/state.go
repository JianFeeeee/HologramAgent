package worker

// state.go —— 工作式子的**任务表**（本包自己的状态，不是基座字段）。
//
// §6.2 #4：类型状态归类型。原先叫 `workerState` 住在基座 `agentstate.go`；
// 现在它在自己的包里，基座只有 `subStates.worker any` 一格。

import (
	"strconv"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agenttypes"
)

// maxRetainedTasks 是保留的已完成子任务上限（防结果无限占用内存）。
const maxRetainedTasks = 20

// State 是一个父 agent 的**工作式子分区**（任务表 + 编号）。
type State struct {
	mu     sync.Mutex
	nextID int64
	seq    int64
	tasks  map[string]*agenttypes.ChildTaskState
}

// NewState 构造分区。
func NewState() *State {
	return &State{tasks: map[string]*agenttypes.ChildTaskState{}}
}

// NextID 分配一个新的子任务 id 并把它登记为「运行中」。
func (s *State) NextID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := "child_" + strconv.FormatInt(s.nextID, 10)
	s.tasks[id] = &agenttypes.ChildTaskState{Running: true}
	return id
}

// Snapshot 取一个任务的当前状态。
func (s *State) Snapshot(id string) (agenttypes.ChildTaskState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.tasks[id]
	if !ok {
		return agenttypes.ChildTaskState{}, false
	}
	return *st, true
}

// Finish 写入任务终态并做有界淘汰。
func (s *State) Finish(id, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.tasks[id]; st != nil {
		st.Running = false
		st.Result = result
		s.seq++
		st.Seq = s.seq
	}
	s.evictLocked()
}

// MarkDelivered 记一次"结果已交付"，返回本次是否是第一次交付。
//
// ★ 幂等语义：重复查询不是失败，要明确告诉模型"结果已给过"，
// 否则它会认定任务未完成而无限重试（实测单轮 35 次调用、持续 514 秒）。
func (s *State) MarkDelivered(id string) (first bool, result string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, exists := s.tasks[id]
	if !exists {
		return false, "", false
	}
	first = !st.Delivered
	st.Delivered = true
	return first, st.Result, true
}

// evictLocked 淘汰最旧的已完成子任务。调用方必须持有 s.mu。
func (s *State) evictLocked() {
	for len(s.tasks) > maxRetainedTasks {
		oldestID := ""
		var oldestSeq int64
		for id, st := range s.tasks {
			if st.Running {
				continue
			}
			if oldestID == "" || st.Seq < oldestSeq {
				oldestID, oldestSeq = id, st.Seq
			}
		}
		if oldestID == "" {
			return
		}
		delete(s.tasks, oldestID)
	}
}
