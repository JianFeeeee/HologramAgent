//go:build !linux && !windows

package agentcli

import (
	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

func init() {
	plugin.RegisterFactory("agentcli", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return &stubPlugin{}, nil
	})
	plugin.RegisterPluginMeta("agentcli", "终端交互", "Agent CLI")
}

type stubPlugin struct{}

func (p *stubPlugin) Name() string                   { return "agentcli" }
func (p *stubPlugin) Start(sdk *sdk.PluginSDK) error { return nil }
func (p *stubPlugin) Stop() error                    { return nil }
