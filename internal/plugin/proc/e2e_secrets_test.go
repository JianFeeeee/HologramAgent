package proc

// 端到端：插件的**变量库**能力（SDK `Secrets()`）。
//
// ## 这条链路为什么要真机验证
//
// 它与 settings 的 setSecret 是**同一个坑的翻版**：新增 proc method 要改
// **4 个地方**（协议常量 / 外层路由 / 内层分发 / 能力表），少一处就报
// "未知 method"。单测只能证明各段对，证不了接上了。
//
// 而且它跨了**三层**：
//
//	① 插件调 sdk.Secrets().Store
//	② 模板 procSecrets → RPC "secrets.store"
//	③ 内核 coreHandler.secrets → sdk.SecretsAPI（secrets_impl）
//	④ → internal/security.Global() 单例（真加密落盘）
//
// ## 为什么要有这个 API（去重的意义）
//
// 在此之前 commondev 插件自带一套 AES-256-GCM 存储，与内核完全重复。
// 两套库**互不可见**是这个设计最有害的地方 —— 现在统一到一个单例。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	internalSDK "github.com/JianFeeeee/HologramAgent/internal/sdk"
	internalSecurity "github.com/JianFeeeee/HologramAgent/internal/security"
	pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

// secretsE2EPlugin 用真实模板编译：只做变量库往返。
const secretsE2EPlugin = `package main

import (
	sdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

type secPlugin struct{ name string }

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &secPlugin{name: name}, nil
}

func (p *secPlugin) Name() string { return p.name }
func (p *secPlugin) Stop() error  { return nil }

func (p *secPlugin) Start(s *sdk.PluginSDK) error {
	s.RegisterTool("e2e_secrets", sdk.ToolDef{
		Description: "变量库往返",
		Parameters:  map[string]interface{}{"type": "object"},
	}, func(args map[string]interface{}) (interface{}, error) {
		sec := s.Secrets()
		if sec == nil || !sec.Available() {
			return "UNAVAILABLE", nil
		}
		alias, err := sec.Store("e2e-secret-value-9876543210", "e2e")
		if err != nil {
			return "STORE_ERR: " + err.Error(), nil
		}
		// ★ 只应拿到别名，拿不到明文
		if alias == "" {
			return "EMPTY_ALIAS", nil
		}
		out, n, _ := sec.Fill("token=@{" + alias + "}")
		if n != 1 {
			return "FILL_MISS", nil
		}
		return out, nil
	})
	return nil
}
`

// ★★ 判据：真实模板插件的变量库往返全链路可用。
func TestE2E_RealTemplateSecretsRoundTrip(t *testing.T) {
	bin := buildPluginWithRealTemplate(t, secretsE2EPlugin)

	dir := t.TempDir()
	setTestCodec(t)
	// 装配**进程级单例**（内核对所有插件共享的那一份）
	if _, err := internalSecurity.InitGlobal(internalSecurity.Options{
		Path:    filepath.Join(dir, "variables.json"),
		KeyFn:   func() ([]byte, error) { return []byte("0123456789abcdef0123456789abcdef"), nil },
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { internalSecurity.SetGlobal(nil) })
	_ = internalConfig.NewConfigRegistry

	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	core := newFakeCore()
	core.secretsAPI = internalSDK.NewSecretsAPI()
	p := New("e2e", bin, dir, nil, host, nil)
	if err := p.Start(core); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	core.mu.Lock()
	handler, ok := core.tools["e2e_secrets"]
	core.mu.Unlock()
	if !ok {
		t.Fatal("插件未注册 e2e_secrets 工具")
	}

	res, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	got, _ := res.(string)
	if strings.HasPrefix(got, "UNAVAILABLE") || strings.HasPrefix(got, "STORE_ERR") ||
		strings.HasPrefix(got, "EMPTY_ALIAS") || strings.HasPrefix(got, "FILL_MISS") {
		t.Fatalf("变量库往返失败: %s", got)
	}
	// Fill 应把别名换回明文
	if !strings.Contains(got, "e2e-secret-value-9876543210") {
		t.Fatalf("Fill 未换回明文: %q", got)
	}
	// 单例里应真的登记了
	if internalSecurity.Global().Count() != 1 {
		t.Fatalf("单例应有 1 条，实际 %d", internalSecurity.Global().Count())
	}
	t.Logf("✅ 插件经内核变量库完成往返: %s", got)
}

// ★★ 判据：插件写入的是**内核那一个**变量库（不是自己又建了一套）。
//
// 这是去重的核心断言：插件 Store 之后，内核侧能立刻看到。
func TestE2E_PluginAndKernelShareOneVault(t *testing.T) {
	bin := buildPluginWithRealTemplate(t, secretsE2EPlugin)

	dir := t.TempDir()
	setTestCodec(t)
	st, err := internalSecurity.InitGlobal(internalSecurity.Options{
		Path:    filepath.Join(dir, "variables.json"),
		KeyFn:   func() ([]byte, error) { return []byte("0123456789abcdef0123456789abcdef"), nil },
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { internalSecurity.SetGlobal(nil) })

	host, _ := NewHost()
	defer host.Close()
	core := newFakeCore()
	core.secretsAPI = internalSDK.NewSecretsAPI()
	p := New("e2e", bin, dir, nil, host, nil)
	if err := p.Start(core); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	core.mu.Lock()
	handler := core.tools["e2e_secrets"]
	core.mu.Unlock()
	if _, err := handler(map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}

	// ★ 内核侧（同一个单例）应看到插件登记的那条
	infos := st.List()
	if len(infos) != 1 {
		t.Fatalf("内核单例应有 1 条（插件写的），实际 %d —— 说明插件自建了一套库", len(infos))
	}
	if infos[0].Note != "e2e" {
		t.Errorf("备注未透传: %q", infos[0].Note)
	}
	// 内核的 Fill 也能用它
	if out, n, _ := st.Fill("x=@{" + infos[0].Alias + "}"); n != 1 ||
		!strings.Contains(out, "e2e-secret-value") {
		t.Errorf("内核 Fill 用不了插件登记的变量: n=%d out=%q", n, out)
	}
	t.Logf("✅ 插件与内核共用同一个变量库（别名 %s）", infos[0].Alias)
}

// ★ 判据：安全模块未启用时 Available()==false（而非 nil panic）。
func TestE2E_SecretsUnavailableWhenDisabled(t *testing.T) {
	bin := buildPluginWithRealTemplate(t, secretsE2EPlugin)

	dir := t.TempDir()
	internalSecurity.SetGlobal(nil)
	t.Cleanup(func() { internalSecurity.SetGlobal(nil) })

	host, _ := NewHost()
	defer host.Close()
	core := newFakeCore()
	core.secretsAPI = internalSDK.NewSecretsAPI()
	p := New("e2e", bin, dir, nil, host, nil)
	if err := p.Start(core); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	core.mu.Lock()
	handler := core.tools["e2e_secrets"]
	core.mu.Unlock()
	res, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatalf("不该报错（应优雅报告不可用）: %v", err)
	}
	if got, _ := res.(string); got != "UNAVAILABLE" {
		t.Fatalf("未启用时应报 UNAVAILABLE，实际 %q", got)
	}
	_ = os.Remove
	_ = pubsdk.SecretInfo{}
}
