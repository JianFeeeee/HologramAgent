package test

// internal/agent/registry 的黑盒判据：类型注册表 + 工具表 + 分发 + 持久化。
//
// ## 这一组判据钉的是什么
//
// 用户 2026-11-18 的两句质问逼出了这里的两条主线：
//
//	「为什么还是有硬编码的 rt，现有的回调注册式接口还不够用吗？」
//	「注册子 agent 类型的接口只对内置插件暴露，通过 eventbus 动态注册撤销」
//
// 于是必须能证明：
//
//	① 类型的「行为」全部来自注册项（Descriptor），不是基座里的 if
//	② 工具的名字/schema/归属/侧别全部来自注册表，不是手抄字面量
//	③ admin_child 三件套**随类型增删而变**（这才是"动态"的实际含义）
//	④ 撤销仍有存活实例的类型被拒（否则实例的管理实现变成悬空调用）
//	⑤ EventBus 路径只接受携带**真 Go 函数值**的载荷（跨进程插件结构上不行）
//	⑥ 创建参数落盘/读回是原子的，坏表不静默当成空表
//
// 全部走导出面，不读源码、不碰内部字段。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

// ───────────────────── ① 类型注册表 ─────────────────────

// regTestType 造一个可辨识的注册项（用闭包返回值判断"谁被执行了"）。
func regTestType(name string, mark *[]string) registry.Descriptor {
	return registry.Descriptor{
		Meta: registry.Meta{Type: name, Description: "测试类型 " + name},
		Actions: []registry.Action{
			{Name: "special", Description: "该类型独有动作"},
		},
		Construct: func(host registry.Host, arg map[string]interface{}) (string, error) {
			*mark = append(*mark, "construct:"+name)
			return "created:" + name, nil
		},
		Admin: func(host registry.Host, action string, arg map[string]interface{}) (string, error) {
			*mark = append(*mark, "admin:"+name+"/"+action)
			return "admin-ok", nil
		},
		Help: func(host registry.Host) string { return "help of " + name },
	}
}

// TestRegisterRejectsDuplicates：同名重复登记被拒（哪个生效不可判定 ⇒ 不许）。
func TestRegisterRejectsDuplicates(t *testing.T) {
	var mark []string
	d := regTestType("dup-check", &mark)
	if err := registry.Register(d); err != nil {
		t.Fatalf("首次注册失败：%v", err)
	}
	t.Cleanup(func() { _ = registry.Unregister("dup-check", nil) })

	if err := registry.Register(d); err == nil {
		t.Fatal("★ 同名重复注册未被拒 —— 两个实现声称同一类型是不可判定的故障")
	}
}

// TestRegisterRejectsMissingConstruct：没有 Construct 的类型登记不上。
//
// ★ 「登记一个创建不出来的类型」= 给模型一个永远失败的入口。
func TestRegisterRejectsMissingConstruct(t *testing.T) {
	err := registry.Register(registry.Descriptor{
		Meta: registry.Meta{Type: "no-construct"},
	})
	if err == nil {
		t.Fatal("★ 缺少 Construct 的类型被接受")
	}
	_ = registry.Unregister("no-construct", nil)
}

// TestUnregisterRejectedWithLiveInstances：有存活实例时撤销被拒，且列出 id。
//
// ★ 撤销后实例的 Admin/Dispose 实现立刻悬空 —— 必须显式两步。
func TestUnregisterRejectedWithLiveInstances(t *testing.T) {
	var mark []string
	if err := registry.Register(regTestType("live-check", &mark)); err != nil {
		t.Fatalf("注册失败：%v", err)
	}
	t.Cleanup(func() { _ = registry.Unregister("live-check", nil) })

	err := registry.Unregister("live-check", []string{"inst-a", "inst-b"})
	if err == nil {
		t.Fatal("★ 有存活实例时撤销未被拒")
	}
	for _, id := range []string{"inst-a", "inst-b"} {
		if !strings.Contains(err.Error(), id) {
			t.Fatalf("★ 拒绝理由没列出存活实例 id（缺 %q）：%v", id, err)
		}
	}
	if _, ok := registry.Lookup("live-check"); !ok {
		t.Fatal("★ 被拒的撤销居然把类型删掉了")
	}
}

// ───────────────────── ② 工具表：侧别与归属 ─────────────────────

// recordingHost 是 registry.Host 的最小实现（只回答"有哪些实例"）。
type recordingHost struct {
	children map[string]registry.ChildBrief
	order    []string
	mu       sync.Mutex
}

func (h *recordingHost) ListChildren(typeName string) []registry.ChildBrief {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]registry.ChildBrief, 0, len(h.order))
	for _, id := range h.order {
		b := h.children[id]
		if typeName != "" && b.Type != typeName {
			continue
		}
		out = append(out, b)
	}
	return out
}

func (h *recordingHost) GetChild(id string) (registry.ChildBrief, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, ok := h.children[id]
	return b, ok
}

func (h *recordingHost) DestroyChild(id, residual, channels string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.children, id)
	return "destroyed " + id, nil
}

func (h *recordingHost) SendToChild(id, text string) (string, error) {
	return "sent to " + id, nil
}

func (h *recordingHost) OwnerID() string { return "owner-1" }
func (h *recordingHost) DataDir() string { return "/tmp/x" }

// TestToolsForSideSeparatesParentAndChild：侧别过滤必须把两侧分开。
//
// ★ 这是"基座不再写 `if a.hooks.IsChild`"的机制底座：工具在哪一侧可见
//
//	是**注册时声明的事实**，不是基座猜的类型结论。
func TestToolsForSideSeparatesParentAndChild(t *testing.T) {
	name := "side-check-" + t.Name()
	if err := registry.RegisterTool(registry.Tool{
		Name: name, Description: "任意侧", Side: registry.SideAny,
		Handle: func(registry.ToolCtx, api.ToolCall) string { return "any" },
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterTool(registry.Tool{
		Name: name + "-parent", Description: "父侧", Side: registry.SideParent,
		Handle: func(registry.ToolCtx, api.ToolCall) string { return "parent" },
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterTool(registry.Tool{
		Name: name + "-child", Description: "子侧", Side: registry.SideChild,
		Handle: func(registry.ToolCtx, api.ToolCall) string { return "child" },
	}); err != nil {
		t.Fatal(err)
	}

	has := func(side registry.Side, want string) bool {
		for _, x := range registry.ToolsFor(side) {
			if x.Name == want {
				return true
			}
		}
		return false
	}
	if !has(registry.SideParent, name) || !has(registry.SideChild, name) {
		t.Fatal("★ SideAny 的工具应两侧都可见")
	}
	if !has(registry.SideParent, name+"-parent") {
		t.Fatal("★ 父侧工具在父侧不可见")
	}
	if has(registry.SideChild, name+"-parent") {
		t.Fatal("★ 父侧工具漏到子侧")
	}
	if !has(registry.SideChild, name+"-child") {
		t.Fatal("★ 子侧工具在子侧不可见")
	}
	if has(registry.SideParent, name+"-child") {
		t.Fatal("★ 子侧工具漏到父侧")
	}
}

// TestDispatchToolReturnsFalseForUnknown：未登记的工具必须 handled=false。
//
// ★ 这是"device 不吞未知工具"的同一条纪律：插件工具不能因为查表而消失。
func TestDispatchToolReturnsFalseForUnknown(t *testing.T) {
	if out, handled := registry.DispatchTool(registry.ToolCtx{}, api.ToolCall{Name: "definitely-not-registered"}); handled || out != "" {
		t.Fatalf("★ 未知工具被注册表吞掉：handled=%v out=%q", handled, out)
	}
}

// TestToolOwnerDeclaredAtRegistration：归属来自注册声明，不是手抄表。
func TestToolOwnerDeclaredAtRegistration(t *testing.T) {
	name := "owner-check-" + t.Name()
	if err := registry.RegisterTool(registry.Tool{
		Name: name, Owner: "someplugin", Side: registry.SideAny,
		Handle: func(registry.ToolCtx, api.ToolCall) string { return "" },
	}); err != nil {
		t.Fatal(err)
	}
	if got := registry.ToolOwner(name); got != "someplugin" {
		t.Fatalf("★ ToolOwner(%q) = %q，期望 someplugin", name, got)
	}
	if got := registry.ToolOwner("never-registered-tool"); got != "" {
		t.Fatalf("★ 未登记工具的归属应为空，实际 %q", got)
	}
}

// ───────────────────── ③ admin_child 三件套随类型增删 ─────────────────────

// adminToolNames 从生成的声明里取出工具名。
func adminToolNames(defs []registry.ToolDef) map[string]registry.ToolDef {
	out := make(map[string]registry.ToolDef, len(defs))
	for _, d := range defs {
		out[d.Name] = d
	}
	return out
}

// TestChildAdminToolsGrowWithTypes：注册新类型后 admin_child 的 type 枚举与
// 动作枚举**自动**包含它（这才叫动态）。
func TestChildAdminToolsGrowWithTypes(t *testing.T) {
	var mark []string
	if err := registry.Register(regTestType("dyn-type", &mark)); err != nil {
		t.Fatalf("注册失败：%v", err)
	}
	t.Cleanup(func() { _ = registry.Unregister("dyn-type", nil) })

	defs := adminToolNames(registry.ChildAdminTools())
	main, ok := defs["admin_child"]
	if !ok {
		t.Fatal("★ admin_child 声明缺失")
	}
	props, _ := main.Properties["type"].(map[string]interface{})
	enum, _ := props["enum"].([]string)
	if !containsStr(enum, "dyn-type") {
		t.Fatalf("★ 新注册类型的 type 枚举没出现：%v", enum)
	}
	actProps, _ := main.Properties["action"].(map[string]interface{})
	actEnum, _ := actProps["enum"].([]string)
	if !containsStr(actEnum, "special") {
		t.Fatalf("★ 新类型的独有动作没进 action 枚举：%v", actEnum)
	}
	if !containsStr(actEnum, registry.ActionCreate) {
		t.Fatalf("★ 通用动作 create 不在枚举里：%v", actEnum)
	}

	// 撤销之后必须**立刻**从枚举里消失（不是下次重启才消失）。
	if err := registry.Unregister("dyn-type", nil); err != nil {
		t.Fatalf("撤销失败：%v", err)
	}
	defs2 := adminToolNames(registry.ChildAdminTools())
	props2, _ := defs2["admin_child"].Properties["type"].(map[string]interface{})
	enum2, _ := props2["enum"].([]string)
	if containsStr(enum2, "dyn-type") {
		t.Fatalf("★ 撤销后 type 枚举仍含 dyn-type：%v", enum2)
	}
}

// TestDispatchRoutesGenericAndTypeActions：通用动作归内核，独有动作转发类型。
func TestDispatchRoutesGenericAndTypeActions(t *testing.T) {
	var mark []string
	if err := registry.Register(regTestType("dispatch-type", &mark)); err != nil {
		t.Fatalf("注册失败：%v", err)
	}
	t.Cleanup(func() { _ = registry.Unregister("dispatch-type", nil) })

	h := &recordingHost{children: map[string]registry.ChildBrief{}}

	// create → Descriptor.Construct
	out, err := registry.Dispatch(h, "dispatch-type", registry.ActionCreate, nil)
	if err != nil || out != "created:dispatch-type" {
		t.Fatalf("★ create 没走到类型自己的 Construct：out=%q err=%v", out, err)
	}
	// 独有动作 → Descriptor.Admin
	if _, err := registry.Dispatch(h, "dispatch-type", "special", nil); err != nil {
		t.Fatalf("★ 类型独有动作分发失败：%v", err)
	}
	// 未知动作 → 响亮报错 + 列出可用动作
	_, err = registry.Dispatch(h, "dispatch-type", "no-such-action", nil)
	if err == nil {
		t.Fatal("★ 未知动作未被拒")
	}
	if !strings.Contains(err.Error(), "special") {
		t.Fatalf("★ 未知动作的错误里没列出可用动作：%v", err)
	}
	// 未注册类型 → 响亮报错
	if _, err := registry.Dispatch(h, "nope-type", registry.ActionList, nil); err == nil {
		t.Fatal("★ 未注册类型未被拒")
	}

	want := []string{"construct:dispatch-type", "admin:dispatch-type/special"}
	if len(mark) != len(want) {
		t.Fatalf("★ 执行轨迹 = %v，期望 %v", mark, want)
	}
	for i := range want {
		if mark[i] != want[i] {
			t.Fatalf("★ 执行轨迹 = %v，期望 %v", mark, want)
		}
	}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ───────────────────── ④ EventBus 注册路径 ─────────────────────

// TestRegisterEventRejectsNonBuiltinPublisher：非内置插件发注册事件被拒。
//
// ★ 这一道是**运行期事实**（谁能承载 Go 函数值），静态策略表表达不了。
func TestRegisterEventRejectsNonBuiltinPublisher(t *testing.T) {
	var mark []string
	// ★ 载荷按**真实发布路径**的形态构造（internal/plugin/registry.go 的
	//   `PublishAs` 就是这么发的）：`plugin` + `descriptor` 两个键。
	//   用构造器的话，测试测的是构造器而不是发布链路的实际载荷。
	payload := map[string]interface{}{
		"plugin":                 "some-lua-plugin",
		registry.EventPayloadKey: regTestType("evt-type", &mark),
	}

	_, _, err := registry.HandleRegisterEvent(registry.EventTypeRegister, payload, func(name string) bool {
		return name == "webui" // 只把 webui 当内置
	})
	if err == nil {
		t.Fatal("★ 非内置插件注册类型未被拒")
	}
	if _, ok := registry.Lookup("evt-type"); ok {
		_ = registry.Unregister("evt-type", nil)
		t.Fatal("★ 被拒的注册居然生效了")
	}
}

// TestRegisterEventAcceptsRealDescriptorFromBuiltin：内置插件携带真结构体可注册。
func TestRegisterEventAcceptsRealDescriptorFromBuiltin(t *testing.T) {
	var mark []string
	payload := map[string]interface{}{
		"plugin":                 "webui",
		registry.EventPayloadKey: regTestType("evt-builtin", &mark),
	}

	_, def, err := registry.HandleRegisterEvent(registry.EventTypeRegister, payload, func(string) bool { return true })
	if err != nil {
		t.Fatalf("★ 内置插件注册被误拒：%v", err)
	}
	// ★ 本函数**不组装**（registry 是纯注册表，不 import agent 实现）：
	//   组装交给 kernelmod/agenttype。所以这里判"声明被原样交出来了"。
	if def == nil {
		t.Fatal("★ 注册事件没有交出类型声明")
	}
	// ★ 本函数把注册**交给了组装器**：这里模拟组装器（真实现是
	//   `kernelmod/agenttype`，那里才 import agent 实现）。
	//
	// 判据要钉的语义是"事件层收下了真结构体并交出去"，而不是"registry
	// 自己能把中立声明变成类型"—— 后者会把 agent 实现塞进纯注册表。
	if d, ok := def.(registry.Descriptor); ok {
		if err := registry.Register(d); err != nil {
			t.Fatalf("注册失败: %v", err)
		}
		t.Cleanup(func() { _ = registry.Unregister("evt-builtin", nil) })
		if _, ok := registry.Lookup("evt-builtin"); !ok {
			t.Fatal("★ 注册成功后类型查不到")
		}
	}
}

// TestRegisterEventRejectsSerializedPayload：JSON 反序列化来的载荷必须被拒。
//
// ★ 这是方案 B 的结构性证据：函数值无法从字节还原，所以跨进程插件
//
//	**在结构上**就注册不出类型 —— 不是靠一句约定。
func TestRegisterEventRejectsSerializedPayload(t *testing.T) {
	// 跨进程插件能送到的**全部**东西就是这样的 map：名字、说明、动作列表，
	// 但没有一个函数值（函数无法序列化 —— 这也正是下面 marshal 那一步
	// 对真 Descriptor 会直接报错的原因）。
	var mark []string
	serializable := map[string]interface{}{
		"Type":        "serialized",
		"Description": "跨进程插件声称的类型",
		"Actions":     []interface{}{map[string]interface{}{"Name": "special"}},
	}
	raw, err := json.Marshal(serializable)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var asMap map[string]interface{}
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// 反证：对**真** Descriptor 做同样的序列化会失败（函数值不可序列化）。
	if _, err := json.Marshal(regTestType("serialized-real", &mark)); err == nil {
		t.Fatal("★ Descriptor 竟然可被 JSON 序列化 —— 方案 B 的「函数值无法过进程边界」不成立")
	}
	payload := map[string]interface{}{
		"plugin":                 "webui",
		registry.EventPayloadKey: asMap,
	}
	if _, _, err := registry.HandleRegisterEvent(registry.EventTypeRegister, payload, func(string) bool { return true }); err == nil {
		t.Fatal("★ JSON 形态的载荷（无真 Go 函数值）被接受了 —— 方案 B 的结构性保证失效")
	}
	if _, ok := registry.Lookup("serialized"); ok {
		_ = registry.Unregister("serialized", nil)
	}
}

// ───────────────────── ⑤ 持久化 ─────────────────────

// TestSaveLoadRoundTrip：创建参数能原样写回。
func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	recs := []registry.Record{
		{Type: "resident", ID: "b", Arg: map[string]interface{}{"id": "b", "task_prompt": "做B"}, Created: "2026-11-18T00:00:00Z"},
		{Type: "rt", ID: "a", Arg: map[string]interface{}{"id": "a", "inputch": "mic"}, Created: "2026-11-18T00:00:01Z"},
	}
	if err := registry.Save(dir, recs); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := registry.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("★ 读回 %d 条，期望 2 条", len(got))
	}
	// 稳定序（按 id 排序）
	if got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("★ 落盘顺序不稳定：%v %v", got[0].ID, got[1].ID)
	}
	if got[1].Arg["task_prompt"] != "做B" {
		t.Fatalf("★ 参数没原样读回：%v", got[1].Arg)
	}
}

// TestLoadMissingFileIsEmptyNotError：文件不存在 ⇒ 空表，不是错误。
func TestLoadMissingFileIsEmptyNotError(t *testing.T) {
	recs, err := registry.Load(t.TempDir())
	if err != nil {
		t.Fatalf("★ 文件不存在却报错：%v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("★ 空目录读回 %d 条", len(recs))
	}
}

// TestLoadCorruptedFileIsLoud：坏表必须报错，不能静默当成空表。
//
// ★ 静默当空表的后果与本次修的缺陷同形：「重启后全部实例消失」
//
//	看起来像正常行为，没人会去查。
func TestLoadCorruptedFileIsLoud(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agents")
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "instances.json"), []byte("{坏掉的 json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Load(dir); err == nil {
		t.Fatal("★ 损坏的实例表被静默当成空表")
	}
}

// TestSaveSkipsIncompleteRecords：残缺记录不落盘（宁可少存一条，不写坏读侧）。
func TestSaveSkipsIncompleteRecords(t *testing.T) {
	dir := t.TempDir()
	if err := registry.Save(dir, []registry.Record{
		{Type: "resident", ID: "", Arg: nil},   // 缺 id
		{Type: "", ID: "x", Arg: nil},          // 缺 type
		{Type: "resident", ID: "ok", Arg: nil}, // 完整
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := registry.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("★ 残缺记录没被跳过：%+v", got)
	}
}

// TestRestoreSkipsUnregisteredTypeButKeepsOthers：一个类型不可用不阻断其余恢复。
//
// ★ 「一次局部故障放大成全部实例都没了」正是本仓反复栽过的形态。
func TestRestoreSkipsUnregisteredTypeButKeepsOthers(t *testing.T) {
	var mark []string
	if err := registry.Register(regTestType("restore-ok", &mark)); err != nil {
		t.Fatalf("注册失败：%v", err)
	}
	t.Cleanup(func() { _ = registry.Unregister("restore-ok", nil) })

	h := &recordingHost{children: map[string]registry.ChildBrief{}}
	ok, errs := registry.Restore(h, []registry.Record{
		{Type: "gone-type", ID: "x1"},
		{Type: "restore-ok", ID: "x2"},
	})
	if ok != 1 {
		t.Fatalf("★ 成功恢复 %d 个，期望 1 个", ok)
	}
	if len(errs) == 0 {
		t.Fatal("★ 未注册类型的失败没被报告（静默跳过）")
	}
	if len(mark) != 1 || mark[0] != "construct:restore-ok" {
		t.Fatalf("★ 恢复轨迹 = %v", mark)
	}
}

// TestRestoreDoesNotDuplicateExistingInstance：已存在的实例不被重复创建。
func TestRestoreDoesNotDuplicateExistingInstance(t *testing.T) {
	var mark []string
	if err := registry.Register(regTestType("restore-dup", &mark)); err != nil {
		t.Fatalf("注册失败：%v", err)
	}
	t.Cleanup(func() { _ = registry.Unregister("restore-dup", nil) })

	h := &recordingHost{
		children: map[string]registry.ChildBrief{"x1": {ID: "x1", Type: "restore-dup"}},
		order:    []string{"x1"},
	}
	ok, _ := registry.Restore(h, []registry.Record{{Type: "restore-dup", ID: "x1"}})
	if ok != 0 {
		t.Fatalf("★ 已存在的实例被重复创建（ok=%d）", ok)
	}
	if len(mark) != 0 {
		t.Fatalf("★ Construct 被重复调用：%v", mark)
	}
}

// WorkDirOf 返回内核派生的实例工作目录（判据用固定前缀即可）。
func (h *recordingHost) WorkDirOf(typeName, id string) string {
	if typeName == "" || id == "" {
		return ""
	}
	return "/tmp/test-agents/" + typeName + "/" + id
}
