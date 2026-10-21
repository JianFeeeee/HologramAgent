package supervisor

import (
	"errors"

	"github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// sdkAdapter 实现 sdk.SupervisorAPI，将字符串签名的中立接口桥接到
// Daemon 的强类型签名（types.AgentID / types.SnapshotID）。
type sdkAdapter struct {
	d *Daemon
}

func NewSDKAdapter(d *Daemon) sdk.SupervisorAPI {
	return &sdkAdapter{d: d}
}

func (a *sdkAdapter) ListAgents() []sdk.AgentStatus {
	if a.d == nil {
		return nil
	}
	return a.d.ListAgents()
}

func (a *sdkAdapter) GetAgentStatus(id string) (*sdk.AgentStatus, error) {
	if a.d == nil {
		return nil, errors.New("supervisor not available")
	}
	return a.d.GetAgentStatus(types.AgentID(id))
}

func (a *sdkAdapter) PreActionSnapshot(id string) (*types.Snapshot, error) {
	if a.d == nil {
		return nil, errors.New("supervisor not available")
	}
	return a.d.PreActionSnapshot(types.AgentID(id))
}

func (a *sdkAdapter) Restart(id string) error {
	return a.d.Restart(types.AgentID(id))
}

func (a *sdkAdapter) RollbackAgent(id string, snapID string) error {
	if a.d == nil {
		return errors.New("supervisor not available")
	}
	return a.d.RollbackAgent(types.AgentID(id), types.SnapshotID(snapID))
}

var _ sdk.SupervisorAPI = (*sdkAdapter)(nil)
