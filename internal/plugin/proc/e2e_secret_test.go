package proc

// 端到端：插件的**加密存储**能力（settings.setSecret/getSecret）。
//
// ## 为何必须做真机端到端
//
// 这条链路上有 5 段，任何一段漏掉都表现为「插件拿不到值」：
//
//	① 插件调 sdk.Settings().SetSecret
//	② 模板 procSettings.SetSecret → RPC "settings.setSecret"
//	③ proc 协议常量 MethodSettingsSetSecret
//	④ coreHandler 分发 → settingsSetSecret（可选接口断言）
//	⑤ ConfigRegistry.PluginSettings.SetSecret → 真加密落盘
//
// 单测只能证明 ⑤ 对。②③④ 是新写的，必须让**真实模板编译出的插件**
// 真的跑一遍 —— 否则就是「模板 e2e 报了 missing method，而我只在
// 内核侧修好了接口」这类错位（本次实际踩到过一次）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	internalSDK "github.com/JianFeeeee/HologramAgent/internal/sdk"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// secretE2EPlugin 是一个**用真实模板**编译的插件：它只做两件事 ——
// 把密钥写进加密存储，然后读回来并通过返回值报告。
const secretE2EPlugin = `package main

import (
	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

type secretPlugin struct{ name string }

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &secretPlugin{name: name}, nil
}

func (p *secretPlugin) Name() string { return p.name }

func (p *secretPlugin) Stop() error { return nil }

func (p *secretPlugin) Start(s *sdk.PluginSDK) error {
	s.RegisterTool("e2e_secret", sdk.ToolDef{
		Description: "加密存储往返测试",
		Parameters:  map[string]interface{}{"type": "object"},
	}, func(args map[string]interface{}) (interface{}, error) {
		sett := s.Settings()
		if sett == nil {
			return "NO_SETTINGS", nil
		}
		if err := sett.SetSecret("blob", "e2e-secret-value-9876543210"); err != nil {
			return "SET_ERR: " + err.Error(), nil
		}
		got, err := sett.GetSecret("blob")
		if err != nil {
			return "GET_ERR: " + err.Error(), nil
		}
		return got, nil
	})
	return nil
}
`

// ★★ 判据：真实模板编译出的插件，其 SetSecret/GetSecret 全链路可用，
// 且**磁盘上是密文**、插件读回是明文。
func TestE2E_RealTemplateSecretRoundTrip(t *testing.T) {
	bin := buildPluginWithRealTemplate(t, secretE2EPlugin)

	// 用**真实** ConfigRegistry（真加密）+ 真主密钥
	dir := t.TempDir()
	setTestCodec(t)
	reg := internalConfig.NewConfigRegistry(filepath.Join(dir, "config.db"))
	defer reg.Close()

	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	core := newFakeCore()
	// 用**生产同款**适配器（sdk.NewSettings），而不是直接塞 PluginSettings ——
	// 后者缺 DataDir 等成员，不是完整 SettingsAPI；用生产适配器才能
	// 顺带验证「内核装配的那一层」也是通的。
	core.settingsAPI = internalSDK.NewSettings("e2e", reg)
	p := New("e2e", bin, dir, nil, host, nil)
	if err := p.Start(core); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	core.mu.Lock()
	handler, ok := core.tools["e2e_secret"]
	core.mu.Unlock()
	if !ok {
		t.Fatal("插件未注册 e2e_secret 工具")
	}

	res, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatalf("工具调用失败: %v", err)
	}
	got, _ := res.(string)
	if strings.HasPrefix(got, "SET_ERR") || strings.HasPrefix(got, "GET_ERR") {
		t.Fatalf("加密存储往返失败: %s", got)
	}
	if got != "e2e-secret-value-9876543210" {
		t.Fatalf("插件应读回明文，实际 %q", got)
	}

	// ★ 磁盘上必须是密文（这才是本特性的目的）
	var raw string
	if err := reg.DB().QueryRow(
		`SELECT value FROM config_e2e WHERE key = 'blob'`).Scan(&raw); err != nil {
		t.Fatalf("读原始值失败: %v", err)
	}
	if strings.Contains(raw, "e2e-secret-value") {
		t.Fatalf("磁盘上出现明文（加密未生效）: %q", raw)
	}
	if !strings.HasPrefix(raw, "enc:v1:") {
		t.Fatalf("磁盘值应为 enc:v1: 密文，实际 %q", raw)
	}
	t.Logf("✅ 插件读回明文(%d 字节)，磁盘存密文(%d 字节)", len(got), len(raw))
	_ = os.Remove
	_ = pubsdk.ConfigDef{}
}

// setTestCodec 装配一把测试主密钥。
func setTestCodec(t *testing.T) {
	t.Helper()
	internalConfig.ResetTestCodec(t)
}
