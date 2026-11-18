package test

// 子 agent **创建/销毁/软重启/持久化**的对外契约判据。
//
// ## 钉的是什么（用户 2026-11-18 的裁定直接落成判据）
//
//   「创建子 agent 执行在主 agent？不应该在内核吗？」
//   「注意只有按我上面设计的完全回调注册化模块化后……主 agent 的软重启也能实现」
//   「驻留子的持久化部分你没做」
//
// 于是必须能证明：
//
//	① 创建/销毁/列出/注入都走**内核侧实例管理器**（admin_child 的通用动作）
//	② admin_child 只做**查表分发**，类型行为来自注册项而不是基座 if
//	③ 驻留子在进程重启后**被恢复**（实例表，而非只认识驻留子的旧登记表）
//	④ 坏实例表**响亮报错**，不静默当成"没有子 agent"
//	⑤ RestartInstance = 停这一个 + 用**同一份创建参数**重建，其余实例不受影响
//
// 全部走导出面：agent.Instantiate / SpawnResident / Instances() / RestoreChildren /
// Instances().RestartInstance / registry.Dispatch。

import (
	_ "github.com/JianFeeeee/HologramAgent/internal/agent/mainagent"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/security"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// childHarness 是真 agent + 真图记忆 + 真 dataDir + 一个已登记的 inputch。
//
// 为什么这些都要真的：驻留子的创建会 Assign inputch、开主库只读句柄、
// 建 temp 库、落盘登记表 —— 少一样就不是生产路径。
type childHarness struct {
	agent   *agent.Agent
	iom     *io.IOManager
	graph   *memory.GraphDB
	dataDir string
}

func newChildHarness(t *testing.T) *childHarness {
	t.Helper()
	dir := t.TempDir()

	g, err := memory.NewGraphDB(filepath.Join(dir, "main-graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })

	iom := io.NewIOManager()
	iom.RegisterInputChannel("inbox", io.ChannelDef{})

	a, instErr := agent.Instantiate(agent.TypeNameMain, agent.AgentConfig{
		Security: security.NewDisabledRuntime().AsGate(),
		ID:       types.AgentID("root"),
		IO:       iom,
		Memory:   g,
		DataDir:  dir,
		// ★ 图记忆装配面：驻留子/rt 要开"主库只读 + temp 实例"。
		//   生产由 kernelmod/agent 注入（FacGraphOpener）；判据这里用同一实现。
		GraphOpener: memory.NewGraphOpener(),
	})
	if instErr != nil {
		t.Fatalf("Instantiate(main): %v", instErr)
	}
	t.Cleanup(a.Stop)

	return &childHarness{agent: a, iom: iom, graph: g, dataDir: dir}
}

// adminChild 走**真实的工具分发路径**（不是直接调内部方法）。
func (h *childHarness) adminChild(t *testing.T, typeName, action string, extra map[string]interface{}) (string, error) {
	t.Helper()
	arg := map[string]interface{}{}
	for k, v := range extra {
		arg[k] = v
	}
	m := h.agent.Instances()
	if m == nil {
		return "", errNoInstances
	}
	return registry.Dispatch(m, typeName, action, arg)
}

type simpleErr string

func (e simpleErr) Error() string { return string(e) }

const errNoInstances = simpleErr("实例管理器未就绪")

// ---- 判据 1：创建/列出/销毁都经内核实例管理器 ----

// TestAdminChildCreateListDestroyViaKernel：三个通用动作都由内核实现。
//
// ★ 这一条证明"创建不在主 agent 里"：agent.Instances() 是**内核侧**的
// 实例管理器，admin_child 的通用动作全部经它。
func TestAdminChildCreateListDestroyViaKernel(t *testing.T) {
	h := newChildHarness(t)

	out, err := h.adminChild(t, "resident", "create", map[string]interface{}{
		"id":          "r1",
		"input_chs":   "inbox",
		"task_prompt": "整理收件箱",
	})
	if err != nil {
		t.Fatalf("create: %v；输出=%q", err, out)
	}
	if !strings.Contains(out, "r1") {
		t.Fatalf("★ 创建输出里没有实例 id：%q", out)
	}

	list, err := h.adminChild(t, "resident", "list", nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(list, "r1") {
		t.Fatalf("★ list 里看不到刚创建的实例：%q", list)
	}

	// 内核侧登记表必须能看到它（与工具输出同一个事实来源）。
	briefs := h.agent.Instances().ListChildren("resident")
	if len(briefs) != 1 || briefs[0].ID != "r1" {
		t.Fatalf("★ 实例管理器列出 %+v，期望一个 r1", briefs)
	}

	destroy, err := h.adminChild(t, "resident", "destroy", map[string]interface{}{"id": "r1"})
	if err != nil {
		// 销毁失败要说明原因，不能静默
		t.Fatalf("destroy: %v；输出=%q", err, destroy)
	}
	if got := h.agent.Instances().ListChildren("resident"); len(got) != 0 {
		t.Fatalf("★ 销毁后仍有实例：%+v", got)
	}
}

// TestAdminChildUnregisteredTypeIsLoud：未注册类型必须响亮报错并列出可用类型。
func TestAdminChildUnregisteredTypeIsLoud(t *testing.T) {
	h := newChildHarness(t)
	_, err := h.adminChild(t, "no-such-type", "list", nil)
	if err == nil {
		t.Fatal("★ 未注册类型未被拒")
	}
	if !strings.Contains(err.Error(), "resident") {
		t.Fatalf("★ 拒绝理由没列出可用类型（应含 resident）：%v", err)
	}
}

// TestAdminChildInspectReportsRealState：inspect 读的是实例的真实状态。
func TestAdminChildInspectReportsRealState(t *testing.T) {
	h := newChildHarness(t)
	if _, err := h.adminChild(t, "resident", "create", map[string]interface{}{
		"id": "r1", "input_chs": "inbox",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	out, err := h.adminChild(t, "resident", "inspect", map[string]interface{}{"id": "r1"})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !strings.Contains(out, "r1") || !strings.Contains(out, "resident") {
		t.Fatalf("★ inspect 输出不含 id/type：%q", out)
	}
}

// ---- 判据 2：持久化 —— 重启后被恢复 ----

// TestResidentRestoredAfterRestart：进程重启（新 agent 实例、同一个 dataDir）
// 必须把驻留子**重建出来**。
//
// ★ 这是用户明确点名的那条缺陷：「驻留子的持久化部分你没做」。
//
//	原实现的 `RestoreResidents` **零调用方** ⇒ 重启后全部消失。
func TestResidentRestoredAfterRestart(t *testing.T) {
	h := newChildHarness(t)

	if _, err := h.adminChild(t, "resident", "create", map[string]interface{}{
		"id":          "r1",
		"input_chs":   "inbox",
		"task_prompt": "长期任务",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := h.agent.Instances().ListChildren("resident"); len(got) != 1 {
		t.Fatalf("前置：创建后应有 1 个实例，实际 %+v", got)
	}

	// 模拟进程重启：新的 agent、同一个 dataDir、同一个 inputch 名。
	iom2 := io.NewIOManager()
	iom2.RegisterInputChannel("inbox", io.ChannelDef{})
	a2, err := agent.Instantiate(agent.TypeNameMain, agent.AgentConfig{
		Security:    security.NewDisabledRuntime().AsGate(),
		ID:          types.AgentID("root2"),
		IO:          iom2,
		Memory:      h.graph,
		DataDir:     h.dataDir,
		GraphOpener: memory.NewGraphOpener(),
	})
	if err != nil {
		t.Fatalf("Instantiate(root2): %v", err)
	}
	t.Cleanup(a2.Stop)

	n := a2.RestoreChildren()
	if n == 0 {
		t.Fatalf("★ 重启后没有恢复任何子 agent（实例表 %s）",
			filepath.Join(h.dataDir, "agents", "instances.json"))
	}
	briefs := a2.Instances().ListChildren("resident")
	if len(briefs) != 1 || briefs[0].ID != "r1" {
		t.Fatalf("★ 恢复后实例列表 = %+v，期望一个 r1", briefs)
	}
}

// TestCorruptedInstanceTableIsLoud：坏实例表必须报错且**不阻断启动**。
//
// ★ 静默当空表的后果与"重启后全部消失"同形：看起来正常，没人会查。
func TestCorruptedInstanceTableIsLoud(t *testing.T) {
	h := newChildHarness(t)
	p := filepath.Join(h.dataDir, "agents", "instances.json")
	if err := writeBroken(p); err != nil {
		t.Fatalf("写坏表: %v", err)
	}

	n, errs := h.agent.Instances().Restore()
	if n != 0 {
		t.Fatalf("★ 坏表居然恢复了 %d 个实例", n)
	}
	if len(errs) == 0 {
		t.Fatal("★ 坏表被静默当成空表（没有任何错误报告）")
	}
}

// ---- 判据 3：软重启 —— 只重启这一个实例 ----

// TestRestartInstanceRebuildsOnlyTarget：软重启只影响目标实例。
//
// ★ 用户裁定：主 agent 的软重启用于「强行停止跑飞的主 agent」。
//
//	粒度错了（连带重启别的实例）就等于"用内核重启来治一个实例的病"。
func TestRestartInstanceRebuildsOnlyTarget(t *testing.T) {
	h := newChildHarness(t)
	for _, id := range []string{"r1", "r2"} {
		if _, err := h.adminChild(t, "resident", "create", map[string]interface{}{
			"id": id, "input_chs": "inbox",
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	if _, err := h.agent.Instances().RestartInstance("r1"); err != nil {
		t.Fatalf("RestartInstance: %v", err)
	}

	briefs := h.agent.Instances().ListChildren("resident")
	if len(briefs) != 2 {
		t.Fatalf("★ 软重启后实例数 = %d，期望 2（r2 不该受影响）", len(briefs))
	}
	ids := []string{briefs[0].ID, briefs[1].ID}
	if !(ids[0] == "r1" && ids[1] == "r2") {
		t.Fatalf("★ 软重启后实例列表 = %v，期望 [r1 r2]", ids)
	}
}

// TestRestartUnknownInstanceIsLoud：重启不存在的实例必须报错。
func TestRestartUnknownInstanceIsLoud(t *testing.T) {
	h := newChildHarness(t)
	if _, err := h.agent.Instances().RestartInstance("never-existed"); err == nil {
		t.Fatal("★ 重启不存在的实例没报错（假装成功会让用户以为跑飞的实例被停掉了）")
	}
}

// writeBroken 写一个语法上坏掉的实例表（判据用）。
func writeBroken(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("{ not json"), 0o600)
}

// ---- 判据 4：类型撤销仍有存活实例时被拒 ----

// TestUnregisterLiveTypeRejectedFromKernel：从内核侧撤销仍有实例的类型被拒。
//
// ★ 撤掉后实例的 Admin/Dispose 实现立刻悬空 —— 必须显式两步。
func TestUnregisterLiveTypeRejectedFromKernel(t *testing.T) {
	h := newChildHarness(t)
	if _, err := h.adminChild(t, "resident", "create", map[string]interface{}{
		"id": "r1", "input_chs": "inbox",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	alive := []string{}
	for _, b := range h.agent.Instances().ListChildren("resident") {
		alive = append(alive, b.ID)
	}
	if err := registry.Unregister("resident", alive); err == nil {
		t.Fatal("★ 有存活实例时撤销未被拒")
	}
	if _, ok := registry.Lookup("resident"); !ok {
		t.Fatal("★ 被拒的撤销居然删掉了内置类型 resident —— 后续判据会全部失效")
	}
}
