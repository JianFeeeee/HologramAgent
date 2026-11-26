package worker

// state.go —— 工作式子的**任务表**（本包自己的状态，不是基座字段）。
//
// §6.2 #4：类型状态归类型。原先叫 `workerState` 住在基座 `agentstate.go`；
// 现在它在自己的包里，基座只有 `subStates.worker any` 一格。

import (
	"strconv"
	"sync"

	"time"
)

// maxRetainedTasks 是保留的已完成子任务上限（防结果无限占用内存）。
const maxRetainedTasks = 20

// State 是一个父 agent 的**工作式子分区**（任务表 + 编号）。
type State struct {
	mu     sync.Mutex
	nextID int64
	seq    int64
	tasks  map[string]*ChildTaskState
}

// NewState 构造分区。
func NewState() *State {
	return &State{tasks: map[string]*ChildTaskState{}}
}

// NextID 分配一个新的子任务 id 并把它登记为「运行中」。
func (s *State) NextID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := "child_" + strconv.FormatInt(s.nextID, 10)
	s.tasks[id] = &ChildTaskState{Running: true}
	return id
}

// Snapshot 取一个任务的当前状态。
func (s *State) Snapshot(id string) (ChildTaskState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.tasks[id]
	if !ok {
		return ChildTaskState{}, false
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

// Bind 把任务的父 agent 记下来（`CallParent` 上报要用）。
func (s *State) Bind(id, parentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.tasks[id]
	if !ok {
		return errNoTask(id)
	}
	st.ParentID = parentID
	return nil
}

// ParentOf 返回任务的父 agent 标识。
func (s *State) ParentOf(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.tasks[id]; ok {
		return st.ParentID
	}
	return ""
}

type errNoTask string

func (e errNoTask) Error() string { return "worker: 任务 " + string(e) + " 不存在" }

// ChildTaskState 是一个工作式子任务的**当前状态**（本包自己的类型）。
//
// §0.3 隔离铁律：原先声明在公共值层 `agenttypes`（因为基座的状态面要转述它）。
// 它是**工作式子的内部状态**，别的类型不需要它，基座也只是"被查询时转一次"。
// 因此归本包，各持一份。
type ChildTaskState struct {
	// Running 为 true 表示任务在跑。
	Running bool `json:"running"`
	// Result 是终态结果文本（Running=false 时有效）。
	Result string `json:"result,omitempty"`
	// Delivered 标记结果是否已交付过（用于幂等应答）。
	//
	// 为什么不能"读到即删"：完成通知会写进持久上下文，
	// 模型之后还会再查；读一次就删会让第二次查询返回"不存在"，
	// 那是永远不会成功的可操作信号，模型只能一遍遍重试。
	Delivered bool `json:"delivered"`
	// Seq 是完成序号（越大越新；保留上限据此淘汰）。
	Seq int64 `json:"seq"`
	// ParentID 是发起该任务的父 agent（`CallParent` 上报要用它）。
	ParentID string `json:"parent_id"`
	// 时间戳：StartedAt / FinishedAt（状态面按它排"最近"）。
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}
