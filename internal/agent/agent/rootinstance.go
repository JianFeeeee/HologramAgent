package agent

import (
	"fmt"
	"path/filepath"

	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

// RootHost 是内核造**根实例**时的宿主（registry.Host 的一种实现）。
//
// InstanceManager 管子实例（list/get/destroy/send/workdir）；主 agent 不是子实例，
// 但两者共用同一条 Construct 路径。差别只有两处：装配料与产物去向。
type RootHost struct {
	seed AgentConfig
	root *Agent
}

// NewRootHost 构造根宿主（装配点用）。
func NewRootHost(seed AgentConfig) *RootHost {
	return &RootHost{seed: seed}
}

// RootSeed 返回内核准备好的根实例装配料。
func (h *RootHost) RootSeed() any {
	if h == nil {
		return nil
	}
	return h.seed
}

// SetRoot 交回造好的根实例。
func (h *RootHost) SetRoot(instance any) error {
	if h == nil {
		return fmt.Errorf("roothost: 宿主未就绪")
	}
	a, ok := instance.(*Agent)
	if !ok || a == nil {
		return fmt.Errorf("roothost: 交回的是 %T，不是 *Agent", instance)
	}
	h.root = a
	return nil
}

// Root 返回已交回的根实例（未交回 ⇒ nil）。
func (h *RootHost) Root() any {
	if h == nil {
		return nil
	}
	return h.root
}

func (h *RootHost) OwnerID() string { return string(h.seed.ID) }

func (h *RootHost) DataDir() string { return h.seed.DataDir }

func (h *RootHost) WorkDirOf(typeName, id string) string {
	t := typeName
	if t == "" {
		t = "unknown"
	}
	return filepath.Join(h.seed.DataDir, "agents", sanitizeDirName(t), sanitizeDirName(id))
}

func (h *RootHost) noChildren() error {
	return fmt.Errorf("roothost: 我是根实例的宿主，不管理任何子实例（类型装配错误）")
}

func (h *RootHost) ListChildren(typeName string) []registry.ChildBrief { return nil }

func (h *RootHost) GetChild(id string) (registry.ChildBrief, bool) {
	return registry.ChildBrief{}, false
}

func (h *RootHost) DestroyChild(id string, residual string, channels string) (string, error) {
	return "", h.noChildren()
}

func (h *RootHost) SendToChild(id, text string) (string, error) {
	return "", h.noChildren()
}

// AssignChannels 对根宿主无意义：根实例不归属任何父级通道。
//
// ★ 明确报错而不是静默成功：根宿主被调到这里说明调用方以为它有子实例。
func (h *RootHost) AssignChannels(id string, channels []string, capacity int) error {
	return h.noChildren()
}

func (h *RootHost) RegisterInbound(channel string) error { return h.noChildren() }

func (h *RootHost) UnregisterInbound(channel string) {}

// NewlyCreated 实现 registry.NewlyCreatedHost：交回刚造好的根实例。
//
// ★ 两阶段装配（construct → Initialize）需要它。根宿主与实例管理器
// 各有一个实现，语义相同："我刚刚造出来的那个实例是什么"。
func (h *RootHost) NewlyCreated() any {
	if h == nil {
		return nil
	}
	return h.root
}

// RollbackConstruct 实现 registry.RollbackHost：根实例装配失败时停掉它。
//
// ★ 根实例**不能**走 DestroyChild（它不在 children 表里），所以单独一条。
func (h *RootHost) RollbackConstruct(_ string, _ string) {
	if h == nil || h.root == nil {
		return
	}
	h.root.Stop()
	h.root = nil
}
