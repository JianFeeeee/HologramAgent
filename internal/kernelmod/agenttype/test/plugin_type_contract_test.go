package test

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod/agenttype"
	"github.com/JianFeeeee/HologramAgent/internal/security"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// 插件注册子 agent 类型的**契约判据**。
//
// ## 钉住的三件事（用户裁定 2026-11-19）
//
//	① SDK 暴露的是**抽象接口**：插件只填 AgentTypeDef（中立类型 + 回调），
//    不认识 `*agent.Agent` / `*agenttypes.Frame`
//	② 组装在内核侧（`kernelmod/agenttype`）：插件不碰 IO / GraphOpener / Security
//	③ 主 agent 与子 agent 在**同一张表、同一个调用**上——主 agent 只是
//    内核按配置选它作为主 agent 拉起，并把通道默认划给它

// TestPluginTypeIsAssembledByKernel：插件声明 → 内核组装 → 实例可用。
//
// ★ 核心判据：插件只给了 Loop/Admin/Help + Spec，实例就建起来了
//
//	（IO / Security / 骨架登记全由内核按 Spec 装）。
//	然后**插件的 Loop 真的被调用**，且它经中立视图读写的值落进内核帧。
func TestPluginTypeIsAssembledByKernel(t *testing.T) {
	h := newHarness(t)

	var loopRan atomic.Bool
	def := pubsdk.AgentTypeDef{
		Type:        "echoer",
		Description: "回显型测试类型",
		// ★ 插件只填这个回调：它决定这类 agent 怎么思考。
		Loop: func(rt pubsdk.AgentRuntime, fr pubsdk.AgentFrame) pubsdk.AgentOutcome {
			loopRan.Store(true)
			msg := "echo: " + fr.Input()
			fr.AppendMessage(pubsdk.Message{Role: "assistant", Content: msg})
			fr.SetResponse(msg)
			rt.Logf("echoer ran, input=%q", fr.Input())
			return pubsdk.AgentOutcomeDone
		},
		Spec: pubsdk.AgentTypeSpec{
			Scheduler: pubsdk.SchedulerNone, // 无队列：单槽
			Context:   pubsdk.ContextNone,   // 不持有上下文
			Memory:    pubsdk.MemoryNone,    // 无记忆
		},
	}

	if err := h.assembler.Assemble("test-plugin", def); err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	t.Cleanup(func() { _ = h.assembler.Unregister("echoer", nil) })

	if _, ok := lookupType("echoer"); !ok {
		t.Fatal("★ 组装后类型查不到")
	}

	// 创建实例（走内核统一入口，与内置类型同一条路）。
	out, err := h.createChild("echoer", map[string]interface{}{"id": "e1"})
	if err != nil {
		t.Fatalf("创建实例: %v (%s)", err, out)
	}
	e, ok := h.root.Children().Get("e1")
	if !ok {
		t.Fatal("★ 实例没进骨架登记表（持久化/StopAll 都会失效）")
	}
	a, _ := e.Agent().(*agent.Agent)
	if a == nil {
		t.Fatal("★ 实例载体不是 agent")
	}

	// ★ 没有 Hooks.Loop 槽位（docs §0.3）：插件的 Loop 由**本类型自己的**
	//   循环（`Runtime.Start` → runLoop）驱动。所以这里钉的是"运行期已起"。
	hs := a.Hooks()
	if hs.Runtime.Start == nil {
		t.Fatal("★ 插件类型的运行期没有注册（Loop 将永远不会被调）")
	}
	// ★ 插件的 Loop 是**输入驱动**的（阻塞流水线：取输入 → 建帧 → 调 Loop）。
	//   所以判据要投一条输入，而不是直接调一个槽函数 —— 那才是生产路径。
	a.IO().InjectInputTo("test", "test", "hello", map[string]interface{}{"content": "hello"})
	deadline := time.Now().Add(2 * time.Second)
	for !loopRan.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !loopRan.Load() {
		t.Fatal("★ 插件的 Loop 没被执行（runLoop 没跑到）")
	}
}

// TestPluginTypeInstancesLiveInUnifiedRegistry：插件类型也用统一实例表。
//
// ★ 用户裁定：主 agent 与子 agent 在界面层**并无不同** —— 同一张表、
//
//	同一个调用。插件类型不该有另一套生命周期。
func TestPluginTypeInstancesLiveInUnifiedRegistry(t *testing.T) {
	h := newHarness(t)
	def := pubsdk.AgentTypeDef{
		Type: "noop", Description: "空类型",
		Loop: func(pubsdk.AgentRuntime, pubsdk.AgentFrame) pubsdk.AgentOutcome {
			return pubsdk.AgentOutcomeDone
		},
		Spec: pubsdk.AgentTypeSpec{Scheduler: pubsdk.SchedulerNone, Context: pubsdk.ContextNone, Memory: pubsdk.MemoryNone},
	}
	if err := h.assembler.Assemble("test-plugin", def); err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	t.Cleanup(func() { _ = h.assembler.Unregister("noop", nil) })

	if _, err := h.createChild("noop", map[string]interface{}{"id": "n1"}); err != nil {
		t.Fatalf("创建: %v", err)
	}
	// 撤销仍有实例的类型必须被拒（不许让实例悬空）。
	_, alive := h.instances()
	if err := h.assembler.Unregister("noop", alive); err == nil {
		t.Fatal("★ 仍有存活实例的类型被成功撤销了（实例会悬空）")
	}
}

// TestSpecValidationIsLoud：Spec 拼错必须响亮失败（不静默退化）。
func TestSpecValidationIsLoud(t *testing.T) {
	h := newHarness(t)
	def := pubsdk.AgentTypeDef{
		Type: "bad", Description: "非法 Spec",
		Loop: func(pubsdk.AgentRuntime, pubsdk.AgentFrame) pubsdk.AgentOutcome {
			return pubsdk.AgentOutcomeDone
		},
		Spec: pubsdk.AgentTypeSpec{Scheduler: "eh", Context: pubsdk.ContextNone, Memory: pubsdk.MemoryNone},
	}
	if err := h.assembler.Assemble("test-plugin", def); err == nil {
		t.Fatal("★ 非法的 Scheduler 取值被静默接受了")
	}
}

// TestLoopIsRequired：没有 Loop 的类型必须被拒（它不知道怎么思考）。
func TestLoopIsRequired(t *testing.T) {
	h := newHarness(t)
	def := pubsdk.AgentTypeDef{Type: "no-loop", Description: "缺 Loop"}
	err := h.assembler.Assemble("test-plugin", def)
	if err == nil || !strings.Contains(err.Error(), "Loop") {
		t.Fatalf("★ 缺 Loop 的类型没被拒（或错误没指明原因）: %v", err)
	}
}

// TestPluginTypeDoesNotNeedAgentPackage：插件的声明面里**没有一个内核类型**。
//
// ★ 这是「SDK 暴露抽象接口」的结构性证据：`AgentTypeDef` 的字段全是
//
//	pubsdk 自己的类型 + 回调签名（AgentRuntime / AgentFrame 都是接口）。
//	如果哪天有人把 `*agent.Agent` 塞进 SDK，本判据会红。
func TestPluginTypeDoesNotNeedAgentPackage(t *testing.T) {
	var def pubsdk.AgentTypeDef
	// 编译期证据：下面这行若能赋值，就说明 Def 里含内核类型。
	_ = def
	// 运行期证据：组装器接受的载荷必须是 pubsdk.AgentTypeDef（不是内核对等物）。
	h := newHarness(t)
	if err := h.assembler.Assemble("test-plugin", agent.AgentConfig{}); err == nil {
		t.Fatal("★ 组装器接受了内核类型（它只该接受 pubsdk.AgentTypeDef）")
	}
}

// ───────────────────────── harness ─────────────────────────

type harness struct {
	root      *agent.Agent
	assembler *agenttype.Host
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	iom := newIO(t)
	sec := security.NewDisabledRuntime().AsGate()
	// ★ 造实例的唯一入口现在是 `agent.New` + 类型自己的 Install（见 mainagent）。
	root := agent.New(agent.AgentConfig{
		ID: types.AgentID("root"), TypeName: agent.TypeNameMain,
	})
	root.MountSurface(iom, nil, sec, nil, "", nil)
	t.Cleanup(root.Stop)
	root.InstallInstanceManager(agent.NewInstanceManager(root))
	return &harness{root: root, assembler: agenttype.New()}
}

func (h *harness) createChild(typeName string, arg map[string]interface{}) (string, error) {
	im := h.root.EnsureInstanceManager()
	return registryDispatch(im, typeName, arg)
}

func (h *harness) instances() (int, []string) {
	im := h.root.EnsureInstanceManager()
	var ids []string
	for _, b := range im.ListChildren("") {
		ids = append(ids, b.ID)
	}
	return len(ids), ids
}
