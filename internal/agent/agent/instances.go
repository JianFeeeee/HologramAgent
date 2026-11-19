package agent

// instances.go —— **内核侧实例管理器**（用户裁定 2026-11-18）。
//
// ## 用户原话
//
// > 「创建子 agent 执行在主 agent？不应该在内核吗？」
//
// > 「你好好看看 todo 呢？驻留子的持久化部分你没做」
//
// ## 归属（为什么创建在这里而不是在主 agent 里）
//
// 创建子 agent = 装配一个 agent 实例 + 登记 + 绑定上行通路 + 起它的运行期。
// 这四件事**一项都不属于主 agent**：
//
//	装配      用 AgentConfig + 类型的 Construct
//	登记      子 agent 唯一登记表（child.Registry）
//	上行通路  父的 IOManager + 那条入站 inputch（BuildCallParent）
//	运行期   实例自己的 RuntimeHooks
//
// 主 agent 只是**调用方**：模型说"创建一个 resident"，内核执行，结果回给它。
//
// ## 本文件做什么
//
// 它实现 `registry.Host`：把类型注册表里的 Descriptor 与基座的子 agent登记表
// 接起来。于是 `admin_child` 的通用动作（list/create/destroy/send/inspect）
// 全部在内核侧完成，类型只提供 Construct / Admin / Help。
//
// ## 软重启（用户裁定同一条）
//
// `RestartInstance` = 停该实例 + 用**保存的创建参数**重建。
// 它是"强行停止跑飞的主 agent"的实现形态：不需要内核重启，其余实例不受影响。

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	agentregimpl "github.com/JianFeeeee/HologramAgent/internal/agent/agentreg"

	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"

	childpkg "github.com/JianFeeeee/HologramAgent/internal/agent/agent/child"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

// InstanceManager 是内核侧的**实例管理器**（`registry.Host` 的实现）。
//
// 它挂在**根 agent** 上（根是内核装配出来的唯一常驻 agent），但语义上它管理
// 的是"这台内核下的所有子 agent"，不是"根 agent 的私有物"。
type InstanceManager struct {
	// host 是实例管理所需的**最小出生面**（不是 `*Agent`）。
	//
	// ## 为什么不是 `*Agent`（用户裁定，§9 C+D ③）
	//
	// 实例管理做四件事：装配（用类型的 Construct）、登记（记录表）、
	// 上行通路（注入）、运行期（类型自己的 RuntimeHooks）。这四件
	// **一项都不属于"某个 agent"** —— 它管的是"这台内核下的所有子 agent"。
	//
	// 持有 `*Agent` 的后果是：实例管理器能顺手读父 agent 的任意私有状态，
	// 而"哪些能力是它真需要的"就再也没人知道了。收成接口后，
	// 这个答案写在 `InstanceHost` 的五个方法里。
	host InstanceHost

	// deps 是宿主在装配期交给的**外部动作**（见 HostDeps 的说明）。
	deps HostDeps

	// own / ownMu 是**独立使用时**的私有记录表（内核未提供 FacAgentRegistry）。
	ownMu sync.Mutex
	own   kernelapi.AgentRegistry

	// recs 的原存放处（`mu` + `recs` 两张结构）已并入 **agent 记录表**
	// （§3.2.2 五表合一）：创建参数是 `AgentRecord.Arg`，生命周期是
	// `AgentRecord.Lifecycle`，父声明事实是 `AgentRecord.Facts`。
	//
	// ⇒ 本类型只剩"把类型注册表接到记录表"的转发职责（见 remember/forget）。
	// 记录表的写入侧由 kernelapi.AgentRegistry 提供；本包不自己存一份。
}

// InstanceHost 是实例管理器需要的**全部**出生能力（由根 agent 实现）。
//
// ★ 五个方法就是它的全部需求：身份、数据目录、子登记表、日志、
//
//	以及两个"按类型销毁"的命令。少一个都编译不过 —— 这正是接口的价值
//	（比"它能访问 *Agent"精确得多）。
type InstanceHost interface {
	// OwnerID 是宿主标识（错误文案与审计用）。
	OwnerID() string
	// DataDir 是宿主工作目录（类型 Construct 派生 temp 路径要用）。
	DataDir() string
	// Children 是子 agent 的唯一登记表。
	Children() *childpkg.Registry
	// AgentsRegistry 是 agent 记录表（可能 nil ⇒ 实例管理器自建私有表）。
	AgentsRegistry() kernelapi.AgentRegistry
	// Logf 写一行带身份标签的日志。
	Logf(format string, args ...any)
}

// NewInstanceManager 构造实例管理器（根 agent 装配期一次）。
func NewInstanceManager(host InstanceHost) *InstanceManager {
	return &InstanceManager{host: host}
}

// registry 取 agent 记录表的写面（未装配 ⇒ nil）。
//
// ★ 创建参数不再由本类型持有：它住在 `AgentRecord.Arg`。
//
//	一条记录同时承载"重建所需参数"与"父声明事实" ⇒ 清理点只有 Remove 一处。
func (m *InstanceManager) registry() kernelapi.AgentRegistry {
	if m == nil || m.host == nil {
		return nil
	}
	if reg := m.host.AgentsRegistry(); reg != nil {
		return reg
	}
	// ★ 独立使用（单测/裁剪版内核）时自建一张私有表：
	//   **同一个实现**（agentreg.Store），不是第二套代码 —— 所以"有内核表"
	//   与"没有内核表"两条路径的行为必然一致。
	//
	// 为什么不能"没有表就不持久化"：实例管理器存在的意义就是跨重启恢复；
	// 缺表时静默不落盘会让单测通过而生产失效（正是本仓反复踩的形态）。
	m.ownMu.Lock()
	if m.own == nil {
		m.own = agentregimpl.New()
	}
	reg := m.own
	m.ownMu.Unlock()
	return reg
}

// Host 实现 registry.Host。
var _ registry.Host = (*InstanceManager)(nil)

// OwnerID 返回宿主标识。
func (m *InstanceManager) OwnerID() string {
	if m == nil || m.host == nil {
		return ""
	}
	return m.host.OwnerID()
}

// DataDir 返回宿主工作目录（类型 Construct 派生 temp 路径要用）。
func (m *InstanceManager) DataDir() string {
	if m == nil || m.host == nil {
		return ""
	}
	return m.host.DataDir()
}

// ListChildren 列出实例简报（按 type 过滤，空 = 全部）。
func (m *InstanceManager) ListChildren(typeName string) []registry.ChildBrief {
	if m == nil || m.host == nil {
		return nil
	}
	out := make([]registry.ChildBrief, 0, 8)
	for _, e := range m.host.Children().All() {
		if typeName != "" && string(e.Kind()) != typeName {
			continue
		}
		chs := e.Channels()
		out = append(out, registry.ChildBrief{
			ID:       e.Base().ID,
			Type:     string(e.Kind()),
			State:    e.State(),
			InputChs: chs,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// GetChild 取单个实例简报。
func (m *InstanceManager) GetChild(id string) (registry.ChildBrief, bool) {
	if m == nil || m.host == nil {
		return registry.ChildBrief{}, false
	}
	e, ok := m.host.Children().Get(id)
	if !ok {
		return registry.ChildBrief{}, false
	}
	return registry.ChildBrief{
		ID:       e.Base().ID,
		Type:     string(e.Kind()),
		State:    e.State(),
		InputChs: e.Channels(),
	}, true
}

// DestroyChild 销毁一个实例（骨架清理 + 撤销主体策略 + 注销入站登记）。
func (m *InstanceManager) DestroyChild(id, residual string) (string, error) {
	if m == nil || m.host == nil {
		return "", fmt.Errorf("instances: 管理器未就绪")
	}
	e, ok := m.host.Children().Get(id)
	if !ok {
		return "", fmt.Errorf("instances: 实例 %q 不存在", id)
	}
	kind := string(e.Kind())

	// ★ 类型独有语义（残余处置 + 通道去向）走它自己的 Admin：
	//   内核只按 type 查表调用，**不认识任何 type**。
	//
	//   原先这里有一个 `switch Kind { case KindResident: ... case KindRT: ... }`
	//   —— 每加一种类型就要回来加一条 case，且基座被迫 import 两个类型包。
	if d, ok := registry.Lookup(kind); ok && d.Admin != nil {
		if _, err := d.Admin(m, "destroy", map[string]interface{}{
			"id":       id,
			"residual": residual,
		}); err != nil {
			return "", err
		}
	}
	// 骨架级清理（Entry.Dispose + 策略撤销 + 注销入站登记）只写一遍。
	//
	// ★ 类型的 Admin 可能已经把实例**完整拆除**（驻留子的 destroy 要
	//   先取残余再拆，中间不许它再收新任务 —— 那必须是原子的一步）。
	//   所以这里只在实例仍在登记表里时才补骨架清理。
	if _, still := m.host.Children().Get(id); still {
		if err := m.host.Children().StopOne(id, childStopDeps(m)); err != nil {
			return "", err
		}
	}
	m.Forget(id)
	return fmt.Sprintf("已销毁 %s。", id), nil
}

// HostDeps 是宿主（根 agent）在装配期交给实例管理器的**外部动作**。
//
// ★ 为什么不是 `InstanceHost` 接口的新方法：那会让"实例管理器需要父的什么"
//
//	无限增长（每加一种收尾动作就加一格），而接口是实现者必须全填的契约。
//	这些是**装配期给进来的函数值**：没给就跳过（骨架自己那部分照做）。
//
// ★ 为什么必须由父提供 `RevokePolicy`：主体表以调用方给的名字为键，
//
//	同名重建会继承旧策略，且表随 create/destroy 单调增长（lessons §3.2）。
type HostDeps struct {
	// RevokePolicy 撤销父为该子声明的主体策略。
	RevokePolicy func(childID string)
	// UnregisterInbound 注销该子在父侧的入站 inputch（复数路径曾漏过）。
	UnregisterInbound func(channelName string)
	// OnDispose 类型 Dispose 之后的父侧收尾（如把残余转回父队列）。
	OnDispose func(e childpkg.Entry)
	// ChannelRegistry 是通道登记表（注销入站要用）。
	ChannelRegistry *agentIO.ChannelRegistry
}

// childStopDeps 收集"停止一个子还需要什么"（骨架不 import agent 包）。
//
// ★ 三项都由宿主提供：
//
//	RevokePolicy      撤销父为该子声明的主体策略（同名重建会继承旧策略）
//	UnregisterInbound 注销该子在父侧的入站 inputch（复数路径曾漏过这一步）
//	OnDispose         类型 Dispose 之后的父侧收尾（如把残余转回父队列）
func childStopDeps(m *InstanceManager) childpkg.StopDeps {
	d := m.deps
	out := childpkg.StopDeps{}
	if d.RevokePolicy != nil {
		out.RevokePolicy = d.RevokePolicy
	}
	if d.UnregisterInbound != nil {
		out.UnregisterInbound = d.UnregisterInbound
	}
	if d.OnDispose != nil {
		out.OnDispose = d.OnDispose
	}
	return out
}

// SendToChild 向实例注入一条输入（复用现有 inputch 注入原语）。
func (m *InstanceManager) SendToChild(id, text string) (string, error) {
	if m == nil || m.host == nil {
		return "", fmt.Errorf("instances: 管理器未就绪")
	}
	e, ok := m.host.Children().Get(id)
	if !ok {
		return "", fmt.Errorf("instances: 实例 %q 不存在", id)
	}
	chs := e.Channels()
	ch := ""
	if len(chs) > 0 {
		ch = chs[0]
	} else {
		ch = "sub/" + id
	}
	a, _ := e.Agent().(*Agent)
	if a == nil || a.io == nil {
		return "", fmt.Errorf("instances: 实例 %q 没有可用 IO", id)
	}
	// 父 → 子：在**子的** io 上投递，走它自己的 inputch（复用 InjectOptions）。
	a.io.InjectInterruptTextOpts(m.host.OwnerID(), ch, text,
		agentIO.InjectOptions{Priority: "L4"})
	return fmt.Sprintf("已向 %s 注入一条输入（会打断它当前处理）。", id), nil
}

// RecreateChild 用保存的创建参数重建实例（软重启用）。
func (m *InstanceManager) RecreateChild(typeName, id string) (string, error) {
	reg := m.registry()
	if reg == nil {
		return "", fmt.Errorf("instances: 记录表未装配（无法重建 %q）", id)
	}
	var found model.AgentRecord
	for _, r := range reg.Records() {
		if r.ID == id {
			found = r
			break
		}
	}
	if found.ID == "" {
		return "", fmt.Errorf("instances: 实例 %q 没有创建参数快照（无法重建）", id)
	}
	arg := found.Arg
	if arg == nil {
		arg = map[string]interface{}{}
	}
	arg["id"] = id
	d, ok := registry.Lookup(typeName)
	if !ok {
		return "", fmt.Errorf("instances: 类型 %q 未注册（无法重建 %q）", typeName, id)
	}
	return d.Construct(m, arg)
}

// remember 记录创建参数（软重启与持久化共用）。
//
// ★ 落点是 **agent 记录**（`AgentRecord.Arg`）：创建参数与父声明事实是同一条
//
//	记录的两个字段，所以"重建所需信息"和"授权约束"不会各存一处。
//
// Remember 记录一个实例的创建参数（类型的持久化路径用）。
func (m *InstanceManager) Remember(typeName, id string, arg map[string]interface{}) {
	if m == nil || id == "" {
		return
	}
	reg := m.registry()
	if reg == nil {
		return
	}
	cp := make(map[string]interface{}, len(arg))
	for k, v := range arg {
		cp[k] = v
	}
	// ★ 用 **Register**（upsert）而不是 Update：创建流程里记录的落点可能
	//   晚于本调用（子的 New 在本函数之后才跑），Update 会因"记录还不存在"
	//   静默不写 —— 那正是持久化看起来成功、恢复却找不到参数的原因。
	if !reg.Present(id) {
		_ = reg.Register(model.AgentRecord{
			ID:        id,
			Type:      typeName,
			Created:   time.Now().Format(time.RFC3339),
			Arg:       cp,
			Lifecycle: "running",
		})
	} else {
		reg.Update(id, func(rec *model.AgentRecord) {
			rec.Type = typeName
			rec.Arg = cp
			if rec.Created == "" {
				rec.Created = time.Now().Format(time.RFC3339)
			}
		})
	}
	// 落盘（软重启与进程重启共用同一份快照）。
	m.persist()
}

// forget 忘掉一个实例（**创建参数与父声明事实一并消失**）。
// Forget 从记录表摘掉一个实例。
func (m *InstanceManager) Forget(id string) {
	if m == nil || id == "" {
		return
	}
	if reg := m.registry(); reg != nil {
		reg.Remove(id)
	}
	m.persist()
}

func (m *InstanceManager) Records() []registry.Record {
	reg := m.registry()
	if reg == nil {
		return nil
	}
	all := reg.Records()
	out := make([]registry.Record, 0, len(all))
	for _, r := range all {
		// 只列**有类型**的（根 agent 自己也在表里，但它不是"子实例"）。
		if r.Type == "" {
			continue
		}
		out = append(out, registry.Record{Type: r.Type, ID: r.ID, Arg: r.Arg, Created: r.Created})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// persist 把创建参数快照落盘（<data>/agents/instances.json）。
func (m *InstanceManager) persist() {
	if m == nil || m.host == nil {
		return
	}
	if err := registry.Save(m.host.DataDir(), m.Records()); err != nil {
		// 落盘失败**不阻断**创建/销毁：实例已经建好/拆掉了，
		// 硬失败会让一次成功的操作看起来失败。但必须留痕。
		log.Printf("[instances] 落盘失败（%v）—— 重启后这些实例不会被恢复", err)
	}
}

// Restore 按落盘快照重建实例（**进程启动时**调用一次）。
//
// ★ 这是"驻留子持久化"的落点：原先 `RestoreResidents` 零调用方，
//
//	重启后所有驻留子消失。现在恢复逻辑在**实例层**，不按类型各写一份：
//	类型只要 Construct 幂等，就能被恢复。
func (m *InstanceManager) Restore() (int, []string) {
	if m == nil || m.host == nil {
		return 0, nil
	}
	recs, err := registry.Load(m.host.DataDir())
	if err != nil {
		log.Printf("[instances] %v（跳过恢复，避免把坏表当成空表）", err)
		return 0, []string{err.Error()}
	}
	ok, errs := registry.Restore(m, recs)
	for _, e := range errs {
		log.Printf("[instances] 恢复跳过: %s", e)
	}
	if ok > 0 {
		m.host.Logf("restored %d child agent(s) from instances.json", ok)
	}
	return ok, errs
}

// Records 返回创建参数快照（按 id 排序）。

// Instances 返回实例管理器（未装配时 nil）。
func (a *Agent) Instances() *InstanceManager {
	if a == nil {
		return nil
	}
	return a.instances
}

// InstallInstanceManager 装入实例管理器（根 agent 装配期一次）。
// WithDeps 由宿主在装配期注入外部动作（可为零值）。
func (m *InstanceManager) WithDeps(d HostDeps) *InstanceManager {
	if m != nil {
		m.deps = d
	}
	return m
}

func (a *Agent) InstallInstanceManager(m *InstanceManager) {
	if a != nil {
		a.instances = m
	}
}

// EnsureInstanceManager 返回实例管理器，必要时创建（幂等）。
func (a *Agent) EnsureInstanceManager() *InstanceManager {
	if a == nil {
		return nil
	}
	if a.instances == nil {
		a.instances = NewInstanceManager(a)
	}
	return a.instances
}

// RestartInstance 是**软重启**单个实例（用户裁定 2026-11-18）。
//
// 语义：停掉这一个实例 + 用**保存的创建参数**重建；内核不停、其余实例不受影响。
//
// ★ 为什么要它：主 agent 跑飞（工具死循环、上下文爆掉、状态卡死）时，
//
//	重启整个内核会打断所有驻留子与在途任务。只重启该实例才是对的粒度。
//
// ★ 为什么失败要响亮：做不到就必须说"没做到"。假装成功会让用户以为
//
//	跑飞的实例已经被停掉 —— 那比直接报错危险得多。
func (m *InstanceManager) RestartInstance(id string) (string, error) {
	if m == nil || m.host == nil {
		return "", fmt.Errorf("instances: 管理器未就绪")
	}
	if _, ok := m.GetChild(id); !ok {
		return "", fmt.Errorf("instances: 实例 %q 不存在", id)
	}
	reg := m.registry()
	if reg == nil {
		return "", fmt.Errorf("instances: 记录表未装配（无法软重启 %q）", id)
	}
	var rec model.AgentRecord
	hasRec := false
	for _, r := range reg.Records() {
		if r.ID == id && r.Type != "" {
			rec, hasRec = r, true
			break
		}
	}
	if !hasRec {
		return "", fmt.Errorf("instances: 实例 %q 没有创建参数快照（无法软重启）", id)
	}
	// ① 停（残余明确丢弃：它可能已经跑飞，转回队列只会把坏任务扩散）。
	if _, err := m.DestroyChild(id, "drop"); err != nil {
		return "", fmt.Errorf("instances: 软重启 %q 失败于停止阶段: %w", id, err)
	}
	// ② 用同一份参数重建。
	arg := rec.Arg
	if arg == nil {
		arg = map[string]interface{}{}
	}
	arg["id"] = id
	d, found := registry.Lookup(rec.Type)
	if !found {
		return "", fmt.Errorf("instances: 类型 %q 已撤销（无法重建 %q）", rec.Type, id)
	}
	out, err := d.Construct(m, arg)
	if err != nil {
		return "", fmt.Errorf("instances: 软重启 %q 失败于重建阶段: %w", id, err)
	}
	return out, nil
}

// Root 返回宿主（装配点与状态面用）。
//
// ★ 返回**接口**而不是 `*Agent`：实例管理器不持有 agent，
//
//	所以"顺手读父的私有状态"这条路从结构上不存在。
func (m *InstanceManager) Root() InstanceHost {
	if m == nil {
		return nil
	}
	return m.host
}
