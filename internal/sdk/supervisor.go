package sdk

import (
	"time"

	"github.com/JianFeeeee/HomeAgent/pkg/types"
)

// SupervisorAPI provides access to agent lifecycle management.
type SupervisorAPI interface {
	ListAgents() []AgentStatus
	GetAgentStatus(id string) (*AgentStatus, error)
	PreActionSnapshot(id string) (*types.Snapshot, error)
	RollbackAgent(id string, snapID string) error
	// Restart 主动请求重建本单元（热更新入口：换二进制后不必 systemctl restart）。
	Restart(id string) error
}

// AgentStatus is a neutral snapshot of a supervised agent.
type AgentStatus struct {
	ID           types.AgentID            `json:"id"`
	State        types.AgentState         `json:"state"`
	Health       types.HealthStatus       `json:"health"`
	Uptime       time.Duration            `json:"uptime,omitempty"`
	Network      types.NetworkCheckResult `json:"network,omitempty"`
	TrackerStats map[string]interface{}   `json:"tracker_stats,omitempty"`
}
