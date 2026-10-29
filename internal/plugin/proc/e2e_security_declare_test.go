package proc

// 端到端：插件的**安全策略声明**能力（sdk `Security()`）。
//
// ## 这条链路为什么要真机验证
//
// 与 variable store（e2e_secrets）同型：新增一个 proc method 要改
// **4 个地方**（协议常量 / 外层分派 / 内层分发 / 能力表），少一处就报
// "未知 method"。单测只能证明各段自洽，证明不了接上了。
//
// 而且它跨了**四层**：
//
//	① 插件调 sdk.Security().DeclareProfile("qq.guest", …)
//	② 模板 → RPC "security.declareProfile"
//	③ 内核 coreHandler.handleSecurity → sdk.SecurityAPI（security_impl）
//	④ → security.Runtime.DeclareProfile（进程级单例，真正被 CheckTool 读）
//
// ## 最关键的一点：Owner 由内核填
//
// 插件在参数里可以写任意 Owner（甚至写别的插件名），但宿主用
// `PluginName()` 覆盖它 —— 本判据正面验证「插件声明自己的档位成功」，
// 负面验证（冒名被拒）在 internal/sdk/security_impl_test.go。

import (
	"strings"
	"testing"

	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	internalSDK "github.com/JianFeeeee/HologramAgent/internal/sdk"
	internalSecurity "github.com/JianFeeeee/HologramAgent/internal/security"
)

// securityE2EPlugin 用真实模板编译：只做一次档位声明。
const securityE2EPlugin = `package main

import (
	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

type secPlugin struct{ name string }

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &secPlugin{name: name}, nil
}

func (p *secPlugin) Name() string { return p.name }
func (p *secPlugin) Stop() error  { return nil }

func (p *secPlugin) Start(s *sdk.PluginSDK) error {
	sec := s.Security()
	s.RegisterTool("e2e_declare", sdk.ToolDef{
		Description: "声明安全档位",
		Parameters:  map[string]interface{}{"type": "object"},
	}, func(args map[string]interface{}) (interface{}, error) {
		if sec == nil {
			return "UNAVAILABLE", nil
		}
		err := sec.DeclareProfile(sdk.SecurityProfile{
			Name:      "e2eproc.guest",
			DenyTools: []string{"cmd_*"},
		})
		if err != nil {
			return "DECLARE_ERR: " + err.Error(), nil
		}
		return "OK", nil
	})
	return nil
}
`

// ★★ 判据：真实模板插件的档位声明能穿透四层到达 Runtime。
func TestE2E_RealTemplateSecurityDeclare(t *testing.T) {
	bin := buildPluginWithRealTemplate(t, securityE2EPlugin)

	dir := t.TempDir()
	setTestCodec(t)

	// 装配进程级 Runtime（生产同款单例；工具判定读的就是它）
	prev := internalSecurity.RuntimeGlobal()
	rt := internalSecurity.NewRuntime(nil)
	internalSecurity.SetRuntimeGlobal(rt)
	t.Cleanup(func() { internalSecurity.SetRuntimeGlobal(prev) })
	_ = internalConfig.NewConfigRegistry

	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	core := newFakeCore()
	// ★ 用内核侧真实插件名注入 —— 这就是 Owner 不可伪造的来源。
	core.securityAPI = internalSDK.NewSecurityAPI("e2eproc")
	p := New("e2e", bin, dir, nil, host, nil)
	if err := p.Start(core); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	core.mu.Lock()
	handler, ok := core.tools["e2e_declare"]
	core.mu.Unlock()
	if !ok {
		t.Fatal("插件工具未注册")
	}

	out, err := handler(nil)
	if err != nil {
		t.Fatalf("工具调用失败: %v", err)
	}
	got, _ := out.(string)
	if got != "OK" {
		t.Fatalf("声明应成功，实际: %q", got)
	}

	// ★ 核心断言：档位真的进了 Runtime（而不是只"没报错"）
	profiles := rt.Profiles()
	found := false
	for _, n := range profiles {
		if n == "e2eproc.guest" {
			found = true
		}
	}
	if !found {
		t.Fatalf("★ 档位未登记到 Runtime，已有: %v —— RPC 链路某一段没接上", profiles)
	}

	// 再验证它真的能约束工具（否则"登记了但没用"
	//
	// ★ 这里我曾把断言写反（`if d.Allowed { Fail("应放行") }`）——
	// 把“期望放行”写成了“若放行则失败”。现在明确：
	//   未激活档位 ⇒ cmd_run 必须**放行**（档位是按需激活的）
	if d := rt.CheckTool("cmd_run"); !d.Allowed {
		t.Fatalf("未激活档位时 cmd_run 应放行（档位按需激活），实际被拒: %s", d.Reason)
	}
	if err := rt.ActivateProfile("e2eproc.guest", 0); err != nil {
		t.Fatalf("激活档位失败: %v", err)
	}
	if d := rt.CheckTool("cmd_run"); d.Allowed {
		t.Fatal("激活 e2eproc.guest 后 cmd_run 必须被拒 —— 档位声明了却不生效")
	}
}

// 未装配声明面时，插件应拿到明确错误而不是静默成功。
func TestE2E_SecurityDeclareWithoutAPI(t *testing.T) {
	core := newFakeCore()
	core.securityAPI = nil // 模拟宿主未接声明面

	h := &coreHandler{sdk: core}
	if _, err := h.Handle(MethodSecurityDeclareProfile, []byte(`{}`)); err == nil {
		t.Fatal("未装配声明面时必须报错（静默成功会让插件误以为已登记）")
	} else if !strings.Contains(err.Error(), "not wired") {
		t.Fatalf("错误信息应说明未装配，实际: %v", err)
	}
}
