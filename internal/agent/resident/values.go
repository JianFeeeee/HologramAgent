package resident

// values.go —— **驻留子自己的**值类型（create / info / 残余处置 / 通道处置）。
//
// ★ 为什么归本包（docs §0.3 隔离铁律）：这些只描述驻留子的创建参数与
//   父对它的控制面决定，别的类型不需要它们。原先在公共值层 `agenttypes`，
//   于是"加一种子类型"就要回公共层加一批字段。

import (
	"time"
)

// InputchRecord 是 inputch 处理表的一条记录（子持有，父 pull）。
type InputchRecord struct {
	InputCh   string    `json:"inputch"`
	At        time.Time `json:"at"`
	Proactive bool      `json:"proactive"`
	Text      string    `json:"text"`
}

// ResidentOptions 是创建一个驻留子的参数（父的"创建"动作）。
type ResidentOptions struct {
	ID                string
	TaskPrompt        string
	InputChs          []string
	AllowedOutputs    []string
	Capacity          int
	WorkDir           string
	TempPath          string
	PreferredProvider string
	OffloadOwned      bool
	AllowedTools      []string
	DenyTools         []string
}

// ResidentInfo 是父对某个驻留子的可查询状态。
type ResidentInfo struct {
	ID             string          `json:"id"`
	State          string          `json:"state"`
	InputChs       []string        `json:"inputchs"`
	AllowedOutputs []string        `json:"allowed_outputs"`
	Rounds         int             `json:"rounds"`
	ContextFull    bool            `json:"context_full"`
	CreatedAt      time.Time       `json:"created_at"`
	TableSize      int             `json:"table_size"`
	OffloadOwned   bool            `json:"offload_owned,omitempty"`
	Table          []InputchRecord `json:"table,omitempty"`

	SchedReady   int    `json:"sched_ready"`
	SchedPending int    `json:"sched_pending"`
	SchedStack   int    `json:"sched_stack"`
	SchedQueues  [5]int `json:"sched_queues"`
}

// ResidualPolicy 是父对**残余任务**的显式决定。
type ResidualPolicy string

const (
	ResidualKeep     ResidualPolicy = "keep"
	ResidualDrop     ResidualPolicy = "drop"
	ResidualStash    ResidualPolicy = "stash"
	ResidualTakeover ResidualPolicy = "takeover"
)

// ChannelDisposition 是父对**驻留子持有的通道**的显式决定。
type ChannelDisposition string

const (
	ChannelOrphan   ChannelDisposition = ""
	ChannelHandover ChannelDisposition = "handover"
)

// OffloadOptions 是自动转投的判定与执行参数（主 agent 自己的决策参数）。
//
// 已归 mainagent（`mainagent/values.go`）。
