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
	"path/filepath"
	"sort"
	"strings"
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
	// lastCreated 是"最近一次 construct 造出来的实例"（两阶段装配用）。
	createdMu   sync.Mutex
	lastCreated any

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
	m := &InstanceManager{host: host}
	// ★ 装配宿主交给的**外部动作**（docs §10：生命周期全部内核执行）。
	//
	// 这三件事原先写在 `Agent.Stop()` 里（基座各自清自己的），于是
	// "谁负责清理"有两个答案 —— 实测已出现同名重建漏撤销安全视图
	// （继承上一代 guest 档 = 提权）。现在只有一个执行点：本管理器的销毁链。
	//
	// 钩子都是**宿主自己的动作**（它才知道自己的通道名、策略视图、记录表），
	// 所以由宿主在装配期交出来；管理器不认识任何 agent 类型的语义。
	if a, ok := host.(*Agent); ok {
		m.deps = HostDeps{
			RevokePolicy: func(childID string) {
				if reg := a.agentsRegistry(); reg != nil {
					reg.Remove(childID)
				}
			},
			// UnregisterInbound：注销该子在父侧登记的入站 inputch。
			// 通道名由子的 Entry 自己给出（那是类型知识），这里只按名注销。
			UnregisterInbound: func(channelName string) {
				if a.io == nil || a.io.ChannelRegistry() == nil {
					return
				}
				a.io.ChannelRegistry().Unregister(channelName)
			},
		}
		if a.io != nil {
			m.deps.ChannelRegistry = a.io.ChannelRegistry()
		}
		m.deps.RouteTo = func(agentID string, evt *agentIO.InputEvent) bool {
			return a.RouteInputTo(agentID, evt)
		}
		m.deps.SkipReply = func(evt *agentIO.InputEvent, reason string) {
			a.EmitSkippedReply(evt, reason)
		}
	}
	return m
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

// WorkDirOf 返回某个实例的**工作数据目录**（内核派生，类型只消费）。
//
// ## 用户裁定（2026-11-19）
//
// > 「内核持有的 agent 表里就有 agent 的工作数据目录」
// > 「目录是内核统一管理的」
//
// 布局：`<DataDir>/agents/<Type>/<ID>/`。三个组成部分都来自内核知道的事实
// （数据根、类型名、实例 id），所以**任何类型都不需要也不允许自己拼**。
//
// ★ 为什么按 type 分一层：同一 id 换个类型重建时不继承上一代的残留文件
//
//	（那些文件属于另一种语义，读了就是错的状态）。这是内核的目录纪律，
//	不是任何类型的约定。
//
// 未配置 DataDir ⇒ 返回空（单测/独立工具不落盘）。
func (m *InstanceManager) WorkDirOf(typeName, id string) string {
	if m == nil || id == "" {
		return ""
	}
	root := m.DataDir()
	if strings.TrimSpace(root) == "" {
		return ""
	}
	t := strings.TrimSpace(typeName)
	if t == "" {
		t = "unknown"
	}
	// 目录名不能带路径分隔符（类型名来自注册表，但仍做一次规范化：
	// 一个坏类型名不该把数据写到根外面去）。
	return filepath.Join(root, "agents", sanitizeDirName(t), sanitizeDirName(id))
}

// sanitizeDirName 把任意标识符规范成安全的单层目录名。
func sanitizeDirName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, string(filepath.Separator), "_")
	if s == "" || s == "." || s == ".." {
		return "unknown"
	}
	return s
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
func (m *InstanceManager) DestroyChild(id, residual, channels string) (string, error) {
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
	// ★ 通道去向：**基座预定义的策略**，类型在销毁时只声明用哪个（不声明 =
	//   悬空）。类型不碰通道登记表 —— 那是内核 IO 设施，归属与处置归基座。
	//
	// ★ 顺序：**先处置通道，再调类型的 Admin**。通道是基座的资源，基座先
	//   收走自己的部分，类型再收它自己的（残余任务、记忆、temp）。
	//   反过来做的话，类型看到通道还在就会顺手处置（正是改造前的形态：
	//   `resident` 里 45 行 AssignAndDrainStash / Orphan / Stash），
	//   两处都处置 ⇒ 暂存被交出两次。
	chMsg := m.applyChannelPolicy(id, e.Channels(), channels)

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
	if chMsg != "" {
		return fmt.Sprintf("已销毁 %s。（%s）", id, chMsg), nil
	}
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
	// RouteTo 把一条输入投给指定实例（通道交接时交出暂存任务用）。
	RouteTo func(agentID string, evt *agentIO.InputEvent) bool
	// SkipReply 给同步调用方补终态（投递失败时不许让它永久挂起）。
	SkipReply func(evt *agentIO.InputEvent, reason string)
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

// ── 软重启：**快照必须在销毁之前取**（2026-11-26 实测缺陷） ──
//
// # 缺陷实况
//
// `restartChild`（registry 包）的序列是「先 DestroyChild，再 RecreateChild」。
// 而 `DestroyChild` 末尾会调 `m.Forget(id)` —— 那是**正确的**：
// 被销毁的实例不该在重启后被恢复。
//
// 但两者合起来就成了：
//
//	DestroyChild → Forget(id)     ← 创建参数快照被删
//	RecreateChild → 读快照        ← 已经不在了
//	⇒「实例 %q 没有创建参数快照（无法重建）」
//
// 也就是说 **软重启永远不可能成功**，而它在功能清单里是“主 agent 跑飞时
// 唯一的止血手段”。日志里的报错指向“没有快照”，看起来像记录没写，
// 实际是记录刚被销毁路径删掉。
//
// # 修法
//
// 重启不是“销毁 + 创建”两个独立动作，而是一个**原子语义**：
// 快照在销毁前取出，销毁的那次 Forget 记为“本次重启的内部动作”，
// 重建时用预先取出的快照。让管理器拥有这个语义，而不是让调用方
// 记住“先备份再销毁”——那正是会忘的形态。

// RecreateChild 完成一次**软重启**：快照 → 销毁 → 用快照重建。
//
// 实现 `registry.RestartHost`。整个序列在本函数内完成，因为“销毁会删掉
// 创建参数快照”这件事只有本类型知道（见上面「快照必须在销毁之前取」）。
func (m *InstanceManager) RecreateChild(typeName, id string) (string, error) {
	// ① 先取快照 —— 必须在销毁之前。销毁链末尾的 `Forget(id)` 会删掉它。
	snap := m.snapshotOf(id)
	if snap == nil {
		return "", fmt.Errorf("instances: 实例 %q 没有创建参数快照（无法重建）", id)
	}

	// ② 销毁现有实例（Forget 在这里发生，属预期）。
	if _, err := m.DestroyChild(id, "drop", ChannelPolicyRelease); err != nil {
		return "", fmt.Errorf("instances: 重启 %q 失败于销毁阶段: %w", id, err)
	}

	// ③ 用**预先取出的**快照重建（不能再读记录表：那张条目已被 ② 删掉）。
	snap["id"] = id
	d, ok := registry.Lookup(typeName)
	if !ok {
		return "", fmt.Errorf("instances: 类型 %q 未注册（无法重建 %q）", typeName, id)
	}
	out, _, err := registry.Instantiate(d, m, snap)
	if err != nil {
		return "", err
	}
	// 面已挂好（construct 内部做的）⇒ 现在可以起运行期。
	registry.StartInstance(m.NewlyCreated())
	return out, nil
}

// snapshotOf 取出一个实例的创建参数副本（不存在 ⇒ nil）。
func (m *InstanceManager) snapshotOf(id string) map[string]interface{} {
	reg := m.registry()
	if reg == nil {
		return nil
	}
	for _, r := range reg.Records() {
		if r.ID == id && r.Arg != nil {
			cp := make(map[string]interface{}, len(r.Arg))
			for k, v := range r.Arg {
				cp[k] = v
			}
			return cp
		}
	}
	return nil
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
	// ★ 工作目录**由内核分配**，调用方给的一律丢弃（见 WorkDirOf 的注释）。
	//   这样"一条记录 = 一个实例 = 一个目录"不会被模型参数破坏。
	wd := m.WorkDirOf(typeName, id)
	if wd != "" {
		cp[registry.ArgWorkDir] = wd
	} else {
		delete(cp, registry.ArgWorkDir)
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
		wd := r.WorkDir
		if wd == "" {
			wd = m.WorkDirOf(r.Type, r.ID)
		}
		out = append(out, registry.Record{
			Type: r.Type, ID: r.ID, Arg: r.Arg, Created: r.Created,
			WorkDir: wd, Lifecycle: r.Lifecycle,
		})
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
	// ★ 记下"恢复之前谁已经在跑"：只对**本次新建出来**的实例调恢复回调。
	//   否则热重载路径（实例已存在 ⇒ Construct 被判重跳过）会被重复恢复，
	//   把运行中的数据在内存里叠加一遍。
	before := map[string]bool{}
	for _, e := range m.host.Children().All() {
		before[e.Base().ID] = true
	}

	ok, errs := registry.Restore(m, recs)
	for _, e := range errs {
		log.Printf("[instances] 恢复跳过: %s", e)
	}

	// ★ 恢复回调由**内核驱动**（用户裁定 2026-11-19）：
	//   实例建好之后，把它的工作目录交给它自己挂的 `RuntimeHooks.Restore`。
	//   内核不认识目录里存什么，也不替类型决定读的顺序。
	for _, e := range m.host.Children().All() {
		if before[e.Base().ID] {
			continue
		}
		if ag, isAgent := e.Agent().(*Agent); isAgent && ag != nil {
			if err := ag.RestoreInstance(m.WorkDirOf(string(e.Kind()), e.Base().ID)); err != nil {
				log.Printf("[instances] %s(%s) 恢复回调失败: %v", e.Base().ID, e.Kind(), err)
			}
		}
	}
	// ★ **根实例自己也走同一个回调**（2026-11-25）。
	//
	// 此前主 agent 的上下文是靠 `Initialize` 里 `OpenContext()` 顺带读盘恢复的
	// —— 那是**第二条路径**：子走 `RestoreInstance(dir)`、主走 `OpenContext()`，
	// 两种时机、两种失败语义。而"要不要恢复、从哪读"对类型来说完全一样。
	//
	// 现在：内核把根实例的目录也交给它自己的 `Runtime.Restore`。
	// 主 agent 于是**没有任何特殊接口** —— 它只是"被内核选中、被默认拉起的
	// 那个类型"（docs §0.1：按 `core.agent.main_type` 选定主 agent）。
	if rootHost := m.Root(); rootHost != nil {
		if ag, isAgent := rootHost.(*Agent); isAgent && ag != nil {
			// 目录由内核派生（与子实例同一条规则）。
			dir := m.WorkDirOf(ag.TypeName(), string(ag.ID()))
			if err := ag.RestoreInstance(dir); err != nil {
				log.Printf("[instances] 根实例 %s 恢复回调失败: %v", ag.ID(), err)
			}
		}
	}

	// ★ 成功数与失败数必须**互斥**（2026-11-25 实测缺陷）。
	//
	// 此前这里无条件报 `restored %d`，而 `ok` 计的是 Instantiate 返回成功；
	// 现场却有 6 条「划入 inputch cli: inputch 未注册」—— 于是日志同时说
	// "恢复了 6 个"和"跳过了 6 个"。两个数字都来自同一次循环，却矛盾。
	//
	// 计数说谎比不报更糟：它让人相信恢复成功了（本次就是靠它一度误判
	// "重启恢复没问题"）。
	// ★ 不在这里打"恢复了几条"：调用方（内核的恢复挂载点）负责报告。
	//
	// 两处都报的后果（实测）：同一次恢复打出两行
	// `restored 6 child agent(s) from instances.json` +
	// `restored 6 child agent(s) after plugins ready` —— 看起来像恢复了 12 个。
	// **一个事实只报一次**，报在哪一层由"谁最清楚完整上下文"决定。
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
	if _, err := m.DestroyChild(id, "drop", ChannelPolicyRelease); err != nil {
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
	out, _, err := registry.Instantiate(d, m, arg)
	if err != nil {
		return "", fmt.Errorf("instances: 软重启 %q 失败于重建阶段: %w", id, err)
	}
	registry.StartInstance(m.NewlyCreated())
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

// ── registry.NewlyCreatedHost 的实现（两阶段装配用）──

// NewlyCreated 返回**刚刚由 construct 造出来的**实例（any）。
//
// ★ 为什么要这个：`registry.Instantiate` 要在 construct 之后调
//
//	`Descriptor.Initialize(instance)`，而注册表不 import agent 包（会让
//	注册表变成基座的一部分）。所以"刚造的那个"由宿主交出去。
//
// ★ 怎么算"刚造"：construct 走 `Children().Add` 登记，所以"登记时间最新
//
//	且之前不存在"的那个就是它。这里记一个游标比遍历更可靠。
func (m *InstanceManager) NewlyCreated() any {
	if m == nil {
		return nil
	}
	m.createdMu.Lock()
	defer m.createdMu.Unlock()
	return m.lastCreated
}

// MarkCreated 由**类型的 construct** 在登记成功后调用，交出刚造的实例。
//
// ★ 类型自己调它看起来像"多一步"，但那是**交接**而不是"知道注册表怎么查"：
//
//	实例管理器是宿主的公开能力（`host.(*agent.InstanceManager).MarkCreated(a)`），
//	与 `Children().Add(...)` 同层。类型不需要 registry.Lookup 自己。
func (m *InstanceManager) MarkCreated(a *Agent) { m.markCreated(a) }

// markCreated 由 construct 路径在登记成功后调用（见 markCreatedFromChildren）。
func (m *InstanceManager) markCreated(a *Agent) {
	if m == nil || a == nil {
		return
	}
	m.createdMu.Lock()
	m.lastCreated = a
	m.createdMu.Unlock()
}

// RollbackConstruct 回滚一次失败的装配（Initialize 失败时由 registry 调）。
//
// ★ 必须做：实例已经 children.Add、可能已 Assign 了 inputch。不回滚就是
//
//	"创建成功但跑不了"的僵尸 —— 它占着 id 与通道，重建时报"已存在"。
func (m *InstanceManager) RollbackConstruct(typeName, id string) {
	if m == nil || id == "" {
		return
	}
	if _, err := m.DestroyChild(id, "drop", ChannelPolicyRelease); err != nil {
		log.Printf("[instances] 回滚 %s(%s) 失败: %v", typeName, id, err)
	}
}

// 通道去向往基座预定义的几套策略（用户裁定 2026-11-27）。
//
// 类型在销毁时**只声明用哪个**（`admin_child` 的 `arg.channels`），
// 不声明走悬空。类型不拿 io 管理器、不碰通道登记表 —— 那属于分层逃逸：
// 通道归属是内核的资源分配，与"这个实例是哪种类型"无关。
const (
	// ChannelPolicyOrphan 悬空：归属清空 + Orphaned 标记，期间输入进暂存，
	// 等显式接回或划拨。（**默认**）
	ChannelPolicyOrphan = ""
	// ChannelPolicyHandover 接给宿主（父）并**交出暂存任务**。
	//
	// 语义：这个子不干了，它的通道和通道上没处理完的活都回到父手里。
	ChannelPolicyHandover = "handover"
	// ChannelPolicyRelease 悬空并清掉暂存（彻底放手）。
	ChannelPolicyRelease = "release"
)

// applyChannelPolicy 按声明执行通道去向（基座内部实现，不对外暴露）。
func (m *InstanceManager) applyChannelPolicy(id string, chans []string, decl string) string {
	if m == nil || len(chans) == 0 {
		return ""
	}
	reg := m.deps.ChannelRegistry
	if reg == nil {
		return "无通道登记表，通道未处置"
	}
	target := m.host.OwnerID()
	done, gone := 0, 0
	for _, ch := range chans {
		if ch == "" {
			continue
		}
		switch decl {
		case ChannelPolicyHandover:
			// 接回 + 交出暂存：暂存任务由父接手（推原事件保 ResponseCh）。
			drained, err := reg.AssignAndDrainStash(ch, target, 0)
			if err != nil {
				gone++
				continue
			}
			for _, evt := range drained {
				if evt == nil {
					continue
				}
				if m.deps.RouteTo != nil && !m.deps.RouteTo(target, evt) {
					if m.deps.SkipReply != nil {
						m.deps.SkipReply(evt, "channel_handover_unreachable")
					}
				}
			}
			done++
		case ChannelPolicyRelease:
			_ = reg.Orphan(ch)
			reg.TakeStash(ch) // 取出即清空；"丢弃"的留痕由声明方负责
			done++
		default: // ChannelPolicyOrphan
			if err := reg.Orphan(ch); err != nil {
				gone++
				continue
			}
			done++
		}
	}
	msg := fmt.Sprintf("通道处置 %d 条（%s）", done, channelPolicyName(decl))
	if gone > 0 {
		msg += fmt.Sprintf("；%d 条已不存在", gone)
	}
	return msg
}

// channelPolicyName 给策略一个可读名（摘要与日志用）。
func channelPolicyName(decl string) string {
	switch decl {
	case ChannelPolicyHandover:
		return "接回宿主并交出暂存"
	case ChannelPolicyRelease:
		return "释放并清暂存"
	default:
		return "悬空"
	}
}
