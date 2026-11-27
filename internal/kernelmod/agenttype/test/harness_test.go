package test

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

func newIO(t *testing.T) *io.IOManager {
	t.Helper()
	iom := io.NewIOManager()
	iom.RegisterInputChannel("inbox", io.ChannelDef{})
	return iom
}

// registryDispatch 走**内核统一入口**创建实例（与内置类型同一条路）。
func registryDispatch(im *agent.InstanceManager, typeName string, arg map[string]interface{}) (string, error) {
	return registry.Dispatch(im, typeName, registry.ActionCreate, arg)
}

// lookupType 查类型是否已注册（避免直接 import registry 的判据噪声）。
func lookupType(name string) (any, bool) {
	d, ok := registry.Lookup(name)
	return d, ok
}
