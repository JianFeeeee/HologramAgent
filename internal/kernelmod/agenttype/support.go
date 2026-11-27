package agenttype

// support.go —— 组装用的小工具（读 arg、造子 io、按 Spec 装记忆）。

import (
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

func strOf(arg map[string]interface{}, key string) string {
	if arg == nil {
		return ""
	}
	s, _ := arg[key].(string)
	return strings.TrimSpace(s)
}

func splitComma(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// childIOOf 造子 agent 的 io：自己的输入入口 + 共享通道登记表 + 父视图回退。
//
// ★ 与内置驻留子/rt **完全同一做法**（`SpawnResident` / `SpawnRTChild` 都这么写）：
//
//	不接共享登记表 ⇒ 子看不到任何通道；不接父视图 ⇒ output_send__* 与设备工具全废。
func childIOOf(root *agent.Agent) *agentIO.IOManager {
	childIO := agentIO.NewIOManager()
	if root == nil {
		return childIO
	}
	if reg := root.IO().ChannelRegistry(); reg != nil {
		childIO.SetChannelRegistry(reg)
	}
	childIO.SetParentIO(root.IO())
	return childIO
}

// applyMemory 按 Spec 装记忆形态。
//
//	none  不给记忆句柄（graphMemoryOf 得 nil ⇒ 记忆面全部不可达）
//	light 受限只读主库 + 自己的 temp 实例（与驻留子同一装配）
//	full  完整图记忆（与主 agent 同一来源）
func applyMemory(cfg *agent.AgentConfig, kind string, root *agent.Agent, workDir string) {
	// 具体实现在 memoryspec.go（避免本文件被记忆实现细节污染）。
	applyMemorySpec(cfg, root, workDir, memoryKindOf(kind))
}

// memoryKindOf 把 Spec 的记忆档位转成本包内部枚举。
func memoryKindOf(kind string) memKind {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "light":
		return memLight
	case "full":
		return memFull
	}
	return memNone
}

type memKind int

const (
	memNone memKind = iota
	memLight
	memFull
)
