package agent

// childvalues_alias.go —— 子 agent 值类型的**本地别名**。
//
// 值类型定义在叶子层 `agenttypes`（基座与类型包共用，避免循环依赖）；
// 本包做别名，于是既有写法（`ResidentOptions{...}` / `ResidualKeep`）
// 逐字不变，不需要在 1,300 行里改 200 处限定名。

import "github.com/JianFeeeee/HologramAgent/internal/agent/agenttypes"

type (
	InputchRecord      = agenttypes.InputchRecord
	ResidentOptions    = agenttypes.ResidentOptions
	ResidentInfo       = agenttypes.ResidentInfo
	ResidualPolicy     = agenttypes.ResidualPolicy
	ChannelDisposition = agenttypes.ChannelDisposition
)

const (
	ResidualKeep     = agenttypes.ResidualKeep
	ResidualDrop     = agenttypes.ResidualDrop
	ResidualStash    = agenttypes.ResidualStash
	ResidualTakeover = agenttypes.ResidualTakeover

	ChannelOrphan   = agenttypes.ChannelOrphan
	ChannelHandover = agenttypes.ChannelHandover
)
