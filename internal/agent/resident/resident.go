package resident

// 驻留式子 agent 的**生命周期与控制面**（设计 docs/zh/resident-subagent-design.md §7/§9/§10）。
//
//	父 ──创建/发送消息/查看/压缩/回收/销毁──► 驻留子
//	子 ──主动消息(L3) / contextfull(L4)──► 父
//
// 层级关系：
//   - 父持**登记表**（residents），它是查看·发送·压缩·回收·销毁的寻址依据；
//   - 父 `Stop()` ⇒ 销毁全部子（**不留孤儿**）；
//   - 子的 `KernelSource` = 父；父则把子的 contextfull 当**内核级事件**（raiseKernelInterrupt）上报给自己；
//   - 子持有自己的 **inputch 处理表**（父 pull，不打断子）。

import (
	"encoding/json"
	"fmt"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	childpkg "github.com/JianFeeeee/HologramAgent/internal/agent/agent/child"
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	sched "github.com/JianFeeeee/HologramAgent/internal/agent/resident/sched"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

type residentChild struct {
	// ★ 公共部分（id / Kind / ctx / IO / 入站通道 / 状态 / 轮次 / stopped）
	// 来自 childpkg.Child 的**嵌入**：骨架统一管理生命周期，类型只管差异。
	*childpkg.Child

	agent *agent.Agent
	// parentAgent 是**父的**基座句柄（子到这里来反查父的载体 M）。
	parentAgent *agent.Agent
	// ctx 是本实例的上下文实现（驻留子整窗策略）。
	ctx ContextMgr
	// opener 是图记忆装配面（Dispose 里关句柄要用；不持有 *agent.Agent 的实现细节）。
	// light / mainRO 用**契约接口**而不是实现类型（判据 §8 第 3 条）：
	//
	//	light  读 temp∪main、只写 temp（LightMemory 的方法集是 GraphMemory 的子集）
	//	mainRO 主库的**结构上写不进**只读视图
	//
	// 类型模块只关心"能不能 Recall/Commit"，不关心它是 SQLite 还是别的实现。
	tempPath string
	dir      string
	inputChs []string
	allowed  []string
	// offloadOwned 标记这是内核为转投拉起的子（见 offload.go）。
	offloadOwned bool

	// mu 只保护**驻留子特有的**状态（不与 childpkg.Child 的 mu 混用：
	// 两把锁各自保护自己的字段，避免一处加锁跨到另一处的字段）。
	mu    sync.Mutex
	state string // running | contextfull | stopped

	// ── 创建参数（2026-11-17 新增：登记表持久化需要它们）──
	//
	// ★ 为什么在 residentChild 上再存一份（ResidentOptions 本来就在形参里）：
	// 登记表要写回「重建这个子所需的全部参数」，而那些只在 SpawnResident
	// 的形参里出现过 —— 子建起来之后就没了。运行态（轮次/处理表）可以由
	// context.json 恢复，但**父的决定**（任务提示词、授权范围、工具白名单）
	// 无法重算，必须留住。
	//
	// 这些字段只在创建时写一次、之后只读 ⇒ 不需要 mu 保护。
	taskPrompt        string
	capacity          int
	preferredProvider string
}

// ── childpkg.Entry 实现（骨架通过它管理生命周期） ──

// Base 返回公共部分。
func (rc *residentChild) Base() *childpkg.Child { return rc.Child }

// Kind 返回类型标签。
func (rc *residentChild) Kind() childpkg.Kind { return childpkg.KindResident }

// ── child.Entry 的通用面（骨架不再按具体类型断言）──

// Agent 返回载体 agent。
func (rc *residentChild) Agent() any { return rc.agent }

// State 返回运行态短名。
func (rc *residentChild) State() string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.state
}

// Channels 返回已划入的 inputch。
func (rc *residentChild) Channels() []string { return rc.inputChs }

// Disposition 返回通道处置策略：驻留子默认**悬空**（父可在销毁时显式指定
// handover，那条路径由 DestroyResidentWithTarget 传入，不走本默认值）。
func (rc *residentChild) Disposition() string { return string(ChannelOrphan) }

// OffloadOwned 报告本实例是否由转投拉起。
func (rc *residentChild) OffloadOwned() bool { return rc.offloadOwned }

// Dispose 是驻留子的销毁语义：残余处置 + 通道处置 + 记忆句柄释放。
//
// ## 与 rt 的关键差别（不能统一）
//
//	rt       ：无残余（未处理输入是过期状态，丢弃）；通道**悬空**不归还
//	驻留子   ：残余 keep/drop（不丢活）；通道按父的决定 **三选一**释放/挂起/接回
//
// 这三种通道处置是驻留子独有的语义（ChannelDisposition），
// 由父在销毁时决定，写在 `residentDisposition` 上供本方法读取。
func (rc *residentChild) Dispose() string {
	// 关记忆句柄：temp 与子同生共死（连同目录一起丢弃）。
	//
	// ★ 通道处置**不在这里**：它需要父的 sched / io（接回时要把暂存任务
	// 转进父的队列），而 residentChild 只有子自己的载体。所以那一段留在
	// Agent 侧（teardownResidentWith），由骨架的 OnDispose 钩子调用 ——
	// 强行搬进来会让本类型持有对父的隐式反向引用，比现在更糟。

	// ★ 这里**不删** temp 目录（2026-11-17 改）。
	//
	// 原先 Dispose 与 teardownResidentWith 两处都 RemoveAll —— 而 Dispose 是
	// 骨架的通用清理钩子，**进程停机**时也会走到。于是在 teardown 那边加了
	// "停机保留"之后，这里仍会把目录删掉，那处修复等于白做。
	//
	// 现在的分工：**Dispose 只关句柄，绝不删目录**；删不删目录由 Agent 侧
	// 按 persistTemp 决定（显式销毁才删）。这样"保留"只有一个决定点，
	// 不会出现两处各判一次而结论相反。
	return ""
}

// residentEntryOf 按 id 取驻留子（含"是否存在"）。
//
// 统一走 children 登记表，再做一次类型断言 —— 断言失败说明该 id 属于
// 别的类型（如 rtchild），对调用方而言同样"不是驻留子"。
func (m *M) residentEntryOf(id string) (*residentChild, bool) {
	e, ok := m.A.Children().Get(id)
	if !ok {
		return nil, false
	}
	// ★ 经 Entry 的回答取载体，不按具体类型断言：
	//   `Agent()` 由各类型实现，骨架不需要认识 *residentChild / *rtChild。
	//   （用户裁定：类型差异走注册，不走类型分支。）
	if e.Kind() != childpkg.KindResident {
		return nil, false
	}
	rc, _ := e.(*residentChild)
	return rc, true
}

// residentOf 按 id 取驻留子（不存在返回 nil）。
func residentOf(m *M, id string) *residentChild {
	rc, _ := m.residentEntryOf(id)
	return rc
}

// SpawnResident 创建一个驻留子（父的"创建"动作）。
func (m *M) SpawnResident(opts ResidentOptions) (ResidentInfo, error) {
	if strings.TrimSpace(opts.ID) == "" {
		return ResidentInfo{}, fmt.Errorf("驻留子必须有 id")
	}
	// ★ TempPath 不再需要（2026-11-20）：子 agent 不建自己的图库。
	// ★ 也不再检查"父有没有图记忆"：驻留子的图能力来自**主 agent 注册的
	//   槽内工具**（graph_recall / graph_commit），那由内核执行；父没有图库
	//   ⇒ 工具调用返回"无记忆"，而不是创建失败。让创建条件依赖父的内部
	//   实现，就是类型之间互相认识（docs §0.3 禁止）。

	if _, dup := m.A.Children().Get(opts.ID); dup {
		return ResidentInfo{}, fmt.Errorf("驻留子 %s 已存在（%w）", opts.ID, childpkg.ErrChildExists)
	}

	// ① 记忆：**不装配任何东西**。子 agent 的图记忆能力就是两个普通工具
	//    （`graph_recall` / `graph_commit`），由主 agent 注册、模型自己调。
	//    数据全在主 agent 那边，权限与只读隔离在工具的闭包里实现。
	//    ⇒ 这里没有 OpenReadOnly / OpenLight / temp 库 / handle。

	// ② 划入 inputch（登记表里记归属；一个插件的多个 inputch 可分别划给不同子）。
	if reg := m.A.IO().ChannelRegistry(); reg != nil {
		for _, ch := range opts.InputChs {
			if err := reg.Assign(ch, opts.ID, opts.Capacity); err != nil {
				return ResidentInfo{}, fmt.Errorf("划入 inputch %s: %w", ch, err)
			}
		}
	}

	// ③ 子的 io：**独立**的 IOManager（自己的输入通道入口），但共享通道登记表，
	// 并把父的 io 挂成"上级"——**输出通道（io 里的 Device）由插件登记在父的 io 上**，
	// 子若不继承这张视图，`output_send__<通道>` 一律被判"通道不存在或不可用"、
	// `output_list_channels` 为空、连 `output_send__*` 工具都不会生成
	// （现场联调：父侧通道装载完整、子侧 childIO 空壳）。
	// 回退是实时的（设备随资源生灭），授权仍由 opts.AllowedOutputs 白名单把关。
	childIO := agentIO.NewIOManager()
	if reg := m.A.IO().ChannelRegistry(); reg != nil {
		childIO.SetChannelRegistry(reg)
	}
	childIO.SetParentIO(m.A.IO())

	parentID := string(m.A.ID())
	child := agent.New(agent.AgentConfig{
		ID:       types.AgentID(opts.ID),
		TypeName: string(kindResident),
		// ★ 能力面（docs §8.1.1）：驻留子承接文本。
		AgentCap: agentIO.CapText,
		// SystemPrompt 必须**继承父的**：
		//
		// 拿不到它时子只能用 buildSystemPrompt 里的兜底句（一句人格描述）。
		// 而父的提示词里写着**回复投递规则** ——「面向 qq、wechat、a2a、acp 等异步
		// 通道时，必须显式调用 output_send__{通道名}；只返回纯文本会被直接丢弃，
		// 用户永远收不到，而你会误以为已经回复过了」，以及事实性约束、命令与文件
		// 操作策略等。缺了这些，子于异步通道（如 QQ 积压）处理完却发不出去，
		// 而且它自己不会意识到（提示词没告诉它）。
		//
		// 实测（2026-09-19）：webui 这类**同步**通道能回，是因为走 ResponseCh；
		// 而 QQ 是异步通道、必须子主动 output_send —— 这正是本字段必须接上的理由。
		// ★ 子实例**只继承设施句柄**，不继承裸 provider / 源表。
		//   设施是内核的一份，各 agent 靠自己 ID 分开记账与选源；
		//   类型不该看见"有几条源"这件事。
		//
		// ★ 设施句柄必须继承：只给裸 provider 的话，子实例的
		//   `LLM()` 是 nil ⇒ `CompleteLLMNoRetry` 落到"单源直调"分支，
		//   候选链、回退、可用性标记、错误分类全部失效（这正是
		//   "类型在裸调 provider"的真因，不是类型写错了调用）。
		//   设施是**内核的一份**，各 agent 只靠自己 ID 分开记账。
		// 驻留子可以有自己的优先源；留空则继承父的（父再留空 ⇒ 总默认源）。
		// ★ 同一个注册表指针（不是拷贝）：插件运行时更新的段立刻对子 agent 生效。
		// ★ 普通上下文也要落盘（2026-11-17）：与 temp 图记忆同一目录。
		//
		// 不接的后果：重启后子的 RelevanceContext 是空的 —— 它连「上一轮
		// 聊到哪」都不知道，等于每次重启丢失全部对话现场（图记忆也一起丢，
		// 见 teardownResidentWith 的保留语义）。
		// ★ 工作目录由**内核**分配（`WorkDir`），本类型只给文件名。
		//   与 temp 图库同目录：一个实例的现场就该在一起。
		WorkDir: opts.WorkDir,
		// ★ **不给 LightMemory**：驻留子的图记忆经由主 agent 的 `graph_*`
		//   槽内工具（handle + map），它不认识 GraphDB / LightMemory。
		AllowedOutputs: opts.AllowedOutputs,
		// ★ 白/黑名单进 AgentConfig（基座渲染侧要读；原先只存在子载体上，
		//   于是模型看得见父禁用的工具，执行时才被拒）。
		AllowedTools: opts.AllowedTools,
		DenyTools:    opts.DenyTools,
		KernelSource: parentID, // 子的 L4 只属于父
		ParentID:     parentID,
		TaskPrompt:   opts.TaskPrompt,
		// ★ 安全运行时必须继承（2026-10-30 补）。
		//
		// 驻留子是长期挂着、无人盯的执行体：它自己跑调度器、接自己的输入、
		// 长时间不被人工审阅。它是本仓**最需要闸门**的一类 agent，
		// 而原先它是唯一**完全没有闸门**的（子从 New 起 m.security 就是 nil）。
		// ★ 子的记录必须落进**同一张表**（父的记录表）：安全模块判定时读它，
		//   实例管理器持久化时也读它 —— 一张表，一个清理点（§3.2.2）。
	})

	// ★ 注册**驻留子自己**的 runtime 回调（用户裁定：类型注册自己的运行行为）。
	//   基座只持有一个槽位；主 agent 填 main 版，这里填 resident 版。
	//   漏了这一步的后果是"子 Start 之后什么都不跑"，而基座不会替它猜。
	// ★ **不在这里自己调 Initialize**（2026-11-25）：
	// 它由 `registry.Instantiate` 在 construct 返回后统一调 —— 类型不必
	// 查注册表、不必知道"我是怎么被造的"。此前这里 `registry.Lookup(kind)`
	// 再调一次，是"类型为了让自己工作必须知道注册表怎么查"的倒挂。
	h := child.Hooks()
	h.Runtime.Start = StartRuntime
	h.Runtime.Stop = StopRuntime
	// ★ 恢复回调：内核把本实例目录交过来（用户裁定 2026-11-19）。
	//   驻留子要恢复的是**整窗上下文**（它不裁，所以现场就是全部历史）。
	h.Runtime.Restore = func(x *agent.Agent, dir string) error {
		if x == nil {
			return nil
		}
		rc, ok := m.residentEntryOf(string(x.ID()))
		if !ok || rc == nil {
			return nil
		}
		return rc.loadContext(dir)
	}
	// ★ 面：子实例继承父的 IO 面（共享通道登记表 + 父视图回退）、阶段管道、
	//   安全闸、记录表。**不继承** Provider/ProviderManager（docs §9.6 硬边界 2）。
	child.MountSurface(childIO, m.A.StageHost(), m.A.Gate(), m.A.AgentsRegistry(), nil, "", m.A.LLM())
	// ★ 插件工具登记簿**继承父的**：工具是同一个内核的（归属不变），
	//   子只在自己的 io 上看到自己那条通道的工具。
	child.MountToolRegistry(m.A.ToolRegistry())
	child.SetSystemPrompt(m.A.SystemPrompt())
	child.SetPreferredProvider(firstNonEmpty(opts.PreferredProvider, m.A.PreferredProvider()))
	child.SetHooks(h)

	rc := &residentChild{
		Child: &childpkg.Child{
			ID:   opts.ID,
			Kind: childpkg.KindResident,
			IO:   childIO,
			// 驻留子的 ctx 用**子自己载体的**（它已 Start，有自己的调度循环）。
			// 与 rt 不同：rt 的 carrier 不 Start，ctx 由骨架单独派生。
			Ctx:      child.Ctx(),
			Cancel:   child.Cancel(),
			ParentID: string(m.A.ID()),
		},
		agent: child, parentAgent: m.A,
		tempPath: opts.TempPath, dir: filepath.Dir(opts.TempPath),
		inputChs:     append([]string(nil), opts.InputChs...),
		allowed:      append([]string(nil), opts.AllowedOutputs...),
		state:        "running",
		offloadOwned: opts.OffloadOwned,

		// 创建参数：登记表要用它们重建（见 residentpersist.go）。
		taskPrompt:        opts.TaskPrompt,
		capacity:          opts.Capacity,
		preferredProvider: opts.PreferredProvider,
	}

	// ★ 父为这个子**声明主体策略**（用户裁定 2026-10-30）。
	//
	// 位置必须在“子开始跑”之前：子一旦接到第一条输入就会执行工具，
	// 那个时刻策略必须已经在内核里 —— 否则存在一个“无约束执行”的窗口。
	//
	// 声明失败要**回滚整个创建**：一个“本该受限却无限”的执行体
	// 比创建失败危险得多（而创建失败是可重试的）。
	if err := m.A.DeclareChildPolicy(opts.ID, opts.AllowedTools, opts.DenyTools); err != nil {
		child.Stop()
		_ = os.RemoveAll(filepath.Dir(opts.TempPath))
		return ResidentInfo{}, fmt.Errorf("为驻留子 %s 声明安全策略失败（拒绝创建一个无约束的子执行体）: %w", opts.ID, err)
	}

	// ④ 子 → 父的主动消息（**L3 中断**，带子标识）：投进父的 inputch。
	parentInCh := m.residentInboundChannel(opts.ID)
	// 登记了入站 inputch 就必须有回滚路径：注册点与 residents 登记之间
	// 当前无可失败步骤，但一旦将来插入（或 child.Start 变更），早退会把这条
	// 登记留在共享表里——就是 child/<id> 残留那只 bug 的另一条入口。
	// defer 保证“只要没走到成功返回，就注销这条登记”。
	registered := false
	defer func() {
		if registered {
			return
		}
		if reg := m.A.IO().ChannelRegistry(); reg != nil {
			reg.Unregister(inboundChannelName(opts.ID))
		}
	}()
	// ★ 上行通路：父一次性把"怎么投给父"装进子的基座（用户裁定 2026-11-18）。
	//   子此后只调 CallParent —— 通道名与注入点都由父提供，子不拼、不猜。
	child.InstallCallParent(agent.BuildCallParent(m.A, opts.ID, parentInCh))
	m.mkResident(child, func(text string) {
		// 子主动汇报（CallParent 唯一上行出口）：父该尽快看到，L3。
		_ = child.CallParent(agent.CallLevelInteractive, agent.CallKindNotify, text, nil)
	}, func() { m.handleChildContextFull(rc) })

	if err := m.A.Children().Add(rc); err != nil {
		child.Stop()
		_ = os.RemoveAll(filepath.Dir(opts.TempPath))
		return ResidentInfo{}, fmt.Errorf("登记驻留子 %s 失败: %w", opts.ID, err)
	}
	// ★ 交出"刚造的那个"给宿主：registry.Instantiate 要用它跑 Initialize。
	// 这一步是**交接**（与 Children().Add 同层），不是"类型知道注册表怎么查"。
	if im := m.A.EnsureInstanceManager(); im != nil {
		im.MarkCreated(child)
	}

	// ★ **不在这里 Start**（2026-11-25）：内核在"面挂好"之后统一调
	// （`registry.StartInstance`）。construct 里 Start 会让运行期在 IO/
	// StageHost 还是 nil 时起来 —— 此前靠 `StartRuntime` 的 `sched == nil`
	// 兜底掩盖了，那不是正确性，是巧合。

	// ⑥ create 即开工：把任务提示词作为**第一条排队输入**投给子。
	//
	// 为什么必须在这里投：TaskPrompt 只进子的系统提示词（"你是谁、要做什么"），
	// 而**不会**让子跑起来 —— 实测现象是子启动后 rounds=0、永远待机
	// （日志 `[agent] r1 started, waiting for IO interrupts` 之后无事发生）。
	// 走排队输入（非中断）：创建是"安排工作"，不是"打断它正在做的事"。
	if strings.TrimSpace(opts.TaskPrompt) != "" {
		// ★ 第二个参数是**输出通道**，不是入站通道名。
		//
		// `InjectInputTo(source, outputChannel, ...)`：它落进事件的
		// `OutputChannel` 字段，而类型侧 `outputChannelOf(evt)` 用它决定
		// "这一轮的回复往哪发"。
		//
		// 这里曾经传 `parentInCh`（`child/<id>`）—— 那是**父的入站**通道名，
		// 不是输出设备。后果（实测）：每次流式增量都打
		// `[io] no output device "child/z9" ... dropped`，用户在界面上
		// 看不到驻留子说话，而子以为自己已经说了。
		child.IO().InjectInputTo(m.residentParentSource(), m.residentTaskOutputChannel(opts), "text",
			map[string]interface{}{"content": opts.TaskPrompt})
	}
	registered = true

	// 登记表持久化：创建成功后立刻落盘（否则重启后这个子不会被重建）。
	// 放在 registered 之后：那时所有可能失败的步骤都已过，落盘记录的是
	// "真建成了的子"，不是"建了一半的"。
	if !opts.OffloadOwned {
		m.PersistResidents()
	}
	return rc.info(), nil
}

// residentParentSource 是"父给子投递"的输入来源名（子的视角里能看出是谁发的）。
func (m *M) residentParentSource() string { return "parent/" + string(m.A.ID()) }

// residentInboundChannel 是"父接收某个子的消息"的 inputch 名（登记进登记表可见）。
//
// inboundChannelName 只拼名字，不产生副作用（注销路径要用它算出同一个名字，
// 不能再去调 residentInboundChannel——那会顺手把刚摘掉的登记又写回去）。
func inboundChannelName(childID string) string { return "child/" + childID }

func (m *M) residentInboundChannel(childID string) string {
	ch := inboundChannelName(childID)
	if reg := m.A.IO().ChannelRegistry(); reg != nil {
		// 归属父自己：它是父的入站 inputch。
		_ = reg.Register(agentIO.InputChannel{Name: ch, Plugin: "resident", Owner: string(m.A.ID())})
	}
	return ch
}

// DestroyResident 立刻销毁一个驻留子并从登记表移除（父的"销毁"动作；不收割）。
//
// 销毁是父**随时**可做的；父退出时由 StopResidents 对全部子执行。
func (m *M) DestroyResident(id string) error {
	return m.DestroyResidentWith(id, ChannelOrphan)
}

// DestroyResidentWith 按父的决定处置通道后销毁驻留子。
func (m *M) DestroyResidentWith(id string, disp ChannelDisposition) error {
	return m.DestroyResidentWithTarget(id, disp, "")
}

// DestroyResidentWithTarget 是 DestroyResidentWith 的完整版：额外给出
// **交接目标**（disp=ChannelHandover 时生效；空 = 父自己）。
//
// ★ 为什么需要单独一个入口：`DestroyResidentWith` 已有多个调用方（工具面、
// 回收、判据），给它们全加一个参数是纯噪声。而"划拨给别的子"是新能力，
// 只有显式要交接的调用点才需要它。
func (m *M) DestroyResidentWithTarget(id string, disp ChannelDisposition, handoverTarget string) error {
	e := m.A.Children().Remove(id)
	if e == nil {
		return fmt.Errorf("驻留子 %s 不存在", id)
	}
	rc, _ := e.(*residentChild)
	if rc != nil {
		m.teardownResidentWith(rc, disp, handoverTarget, false, nil)
		// 显式销毁 ⇒ 从登记表里去掉（否则重启会把它又建回来）。
		m.PersistResidents()
	}
	return nil
}

// TakeResidual 取走一个驻留子手头**尚未处理**的残余任务（不处置）。
//
// 调它**会**从子的队列里移除这些任务，所以父必须先看返回值再决定：
// 一旦取走，不再调 ApplyResidual 就等于把它们丢了。控制面工具走 ApplyResidual
// （取+处置一步完成）以避免这个误用。
func (m *M) TakeResidual(id string) ([]*agentIO.InputEvent, error) {
	rc := residentOf(m, id)
	if rc == nil || rc.agent == nil {
		return nil, fmt.Errorf("驻留子 %s 不存在", id)
	}
	return m.sched.TakeAllPendingEvents(), nil
}

// FinishResident 是**驻留子终结的唯一入口**：停止调度 → 回收残余任务 →
// 处置 → 收割记忆 → 销毁。
//
// ## 它修的是什么竞态（2026-10-18）
//
// 原实现把这件事拆成三步，由控制面工具依次调用：
//
//	ApplyResidual(id, policy)   // 取队列
//	ReclaimResident / DestroyResident(id)
//
// 于是取完到销毁之间有一个窗口：子仍在运行、继续收新消息、开始执行，
// 而它马上会被销毁 —— 那些任务的结果**永远无人认领**，且日志无痕迹
// （既不在"残余清单"里，也没跑到出结果）。用 ResponseCh 的同步调用方
// 更糟：Stop 的 drainPendingInterrupts 会给它补一个 "skipped"，
// 调用方看到"已跳过"，而任务其实执行过。
//
// ## 为什么是"停止调度"而不是"冻结队列"
//
// 我第一版加过 `scheduler.freeze`（让 nextRef 返回 nextNone）。那是错的：
//
//	· 它凭空多出一个状态位，而调度器**本来就有**停止语义 —— `ctx.Done()`；
//	· 真正的问题是 schedulerLoop 只在"队列空"时才 select ctx.Done，
//	  只要队列非空就永远不看它（见 schedulerLoop 里的修复）。
//	· 一个状态机的缺陷用另一个状态去掩盖，会让"到底谁负责停"变得含糊。
//
// 现在：cancel 关 ctx.Done ⇒ 调度循环在**下一次取任务前**就退出
// （不再取新任务），已在执行的那一条跑完后循环自然结束。
// 等它真正停下，再回收队列 —— 此刻队列是静止的，回收没有歧义。
func (m *M) FinishResident(id string, policy ResidualPolicy, onDestroy func(rc *residentChild) error) (n int, msg string, err error) {
	return m.FinishResidentWith(id, policy, ChannelOrphan, onDestroy)
}

// FinishResidentWith 是 FinishResident 的完整版：残余任务策略 + **通道处置**一次定。
//
// 为什么合成一个入口：两者有依赖关系 ——
//   - residual=stash 要求通道**继续存在**（暂存挂在通道上）；
//   - channel=release 会把通道还给池子（可能被别的 agent 接走）。
//
// 若拆成两次调用，中间窗口里通道可能已被别人接管，暂存任务就跑到了
// 一个跟它无关的 agent 手里。**一次定完，没有窗口。**
func (m *M) FinishResidentWith(id string, policy ResidualPolicy, disp ChannelDisposition, onDestroy func(rc *residentChild) error) (n int, msg string, err error) {
	return m.FinishResidentWithTarget(id, policy, disp, "", onDestroy)
}

// FinishResidentWithTarget 是 FinishResidentWith 的完整版：额外给出交接目标。
func (m *M) FinishResidentWithTarget(id string, policy ResidualPolicy, disp ChannelDisposition, handoverTarget string, onDestroy func(rc *residentChild) error) (n int, msg string, err error) {
	rc := residentOf(m, id)
	if rc == nil || rc.agent == nil {
		return 0, "", fmt.Errorf("驻留子 %s 不存在", id)
	}
	child := rc.agent

	// ① 停止调度：关 ctx.Done。调度循环在下次取任务前退出。
	child.Cancel()()
	// ② 等它真的停下（已在执行的那条会跑完，不再取新的）。
	m.sched.WaitSchedulerStopped(sched.StopTimeout, id)

	// ③ 此刻队列静止，回收残余任务没有歧义。
	events := m.sched.TakeAllPendingEvents()
	n, msg, err = m.applyResidualEvents(id, events, policy)
	if err != nil {
		return n, msg, err
	}

	// ④ 收割记忆（必须在 Destroy 之前 —— Destroy 会关掉 temp）。
	if onDestroy != nil {
		if herr := onDestroy(rc); herr != nil {
			return n, msg, herr
		}
	}
	// ⑤ 销毁（内部会 Stop 子、关 temp、按 disp 处置通道）。
	//
	// ⚠️ 必须用 DestroyResidentWith：走 DestroyResident 会丢掉 disp，
	// 通道被无条件释放 —— 而 stash 的暂存任务正挂在那条通道上。
	return n, msg, m.DestroyResidentWithTarget(id, disp, handoverTarget)
}

// ApplyResidual 按父给出的策略处置一个驻留子的残余任务。
//
// keep：逐条转回父自己的队列（保留原事件，含 ResponseCh 与来源/通道）；
// drop：逐条记录日志后丢弃（不可追溯的丢弃是不允许的）。
//
// 无论哪种，带 ResponseCh 的都要给终态，否则 cli/a2a 这类无超时同步调用方
// 会永久挂起（设计 §7 I5）。
func (m *M) ApplyResidual(id string, policy ResidualPolicy) (int, string, error) {
	// ⚠️ 本函数只取残余、**不终止子**，因此取完之后子仍在运行 ——
	// 控制面工具请用 FinishResident（停止调度 + 取 + 处置 + 销毁，一步完成），
	// 否则"取完到销毁之间"子还会收新任务，那些任务的结果随销毁丢失。
	//
	// 保留它是为了 TakeResidual 的配对语义（先取后自己决定），以及既有判据。
	rc := residentOf(m, id)
	if rc == nil || rc.agent == nil {
		return 0, "", fmt.Errorf("驻留子 %s 不存在", id)
	}
	events := m.sched.TakeAllPendingEvents()
	return m.applyResidualEvents(id, events, policy)
}

// applyResidualEvents 按策略处置已取走的残余任务（keep 转回父 / drop 留痕丢弃）。
func (m *M) applyResidualEvents(id string, events []*agentIO.InputEvent, policy ResidualPolicy) (int, string, error) {
	if len(events) == 0 {
		return 0, "无残余任务", nil
	}
	if policy == ResidualDrop {
		for _, evt := range events {
			// 丢弃必须留痕：异步通道（qq/wechat）没有 ResponseCh，
			// 不记日志的话“这条消息为什么没人回”永远查不出来。
			log.Printf("[resident] %s residual task discarded per parent decision: source=%s channel=%s request=%s content=%s",
				id, evt.Source, evt.OutputChannel, evt.RequestID, agent.TruncateStr(inputTextOf(evt), 80))
			m.A.EmitSkippedReply(evt, "residual_dropped_by_parent")
		}
		return len(events), fmt.Sprintf("已按父的决定丢弃 %d 条残余任务（逐条已记日志）", len(events)), nil
	}
	// stash：暂存回**每条任务来的那条通道**。
	//
	// ★ 必须逐条按其自身 OutputChannel 归位，不能整批塞进一个通道 ——
	// 一个驻留子可能同时收 qq 和 webui 的消息，混存会导致接回 qq 的人
	// 拿到本该给 webui 的活。
	if policy == ResidualStash {
		reg := m.A.IO().ChannelRegistry()
		if reg == nil {
			return 0, "", fmt.Errorf("无通道登记表，无法暂存残余任务")
		}
		stashed, failed := 0, 0
		for _, evt := range events {
			if evt == nil {
				continue
			}
			ch := evt.OutputChannel
			if ch == "" {
				ch = evt.Source // 无回程通道时用来源通道兜底
			}
			if err := reg.Stash(ch, []*agentIO.InputEvent{evt}); err != nil {
				// 通道已不存在（插件卸载等）：这条只能丢，但必须留痕，
				// 并给同步调用方终态 —— 否则 cli/a2a 会永久挂起。
				failed++
				log.Printf("[resident] %s residual task could NOT be stashed (channel %q gone): source=%s request=%s content=%s",
					id, ch, evt.Source, evt.RequestID, agent.TruncateStr(inputTextOf(evt), 80))
				m.A.EmitSkippedReply(evt, "residual_stash_channel_gone")
				continue
			}
			stashed++
			// 暂存也要留痕：不可追溯的"暂存"和丢弃没区别。
			log.Printf("[resident] %s residual task stashed to channel %q: source=%s request=%s content=%s",
				id, ch, evt.Source, evt.RequestID, agent.TruncateStr(inputTextOf(evt), 80))
		}
		msg := fmt.Sprintf("已把 %d 条残余任务暂存到各自通道（接回通道即释放）", stashed)
		if failed > 0 {
			msg += fmt.Sprintf("；%d 条因通道已不存在而丢弃（已记日志并给调用方终态）", failed)
		}
		return stashed + failed, msg, nil
	}
	// keep / takeover：转回父自己
	for _, evt := range events {
		if evt == nil {
			continue
		}
		m.sched.Enqueue(sched.NewInputTask(evt))
	}
	m.sched.SignalWake()
	return len(events), fmt.Sprintf("已把 %d 条残余任务转回主 agent 队列", len(events)), nil
}

// inputTextOf 取一条输入事件的正文（供日志描述残余任务用）。
func inputTextOf(evt *agentIO.InputEvent) string {
	if evt == nil || evt.Payload == nil {
		return ""
	}
	s, _ := evt.Payload["content"].(string)
	return s
}

func (m *M) teardownResident(rc *residentChild) {
	m.teardownResidentWith(rc, ChannelOrphan, "", false, nil)
}

// teardownResidentWith 是 teardownResident 的完整版：额外决定**通道去向**，
// 并在 takeover 时把释放出来的暂存任务转进父队列。
func (m *M) teardownResidentWith(rc *residentChild, disp ChannelDisposition, handoverTarget string, persistTemp bool, onDrained func([]*agentIO.InputEvent)) {
	rc.mu.Lock()
	rc.state = "stopped"
	rc.mu.Unlock()

	// ★ 撤销父为它声明的主体策略（与 teardown 同时，早于一切资源回收）。
	//
	// 不撤销的后果：主体表以**调用方给的名字**为键，回收后同名重建是常规
	// 操作 ⇒ 新一代用上一代父留的约束运行；且表随 create/destroy 单调增长。
	// 与 rtchild 留档串线（rtClosedTranscripts）同一形态。
	m.A.RevokeChildPolicy(rc.ID)

	rc.agent.Stop() // 停子的调度器（取消其运行中的任务）

	// ★ temp 目录的处置**分两种情况**（2026-11-17 改）。
	//
	// 原语义是「temp 与子同生共死，回收/销毁都丢弃」—— 那对**显式销毁**是对的
	// （父决定不要这个子了，它的记忆也该没），但对**进程停机**是灾难：
	// `Stop()` → `StopResidents()` → 这里 ⇒ 每次重启都把全部驻留子的
	// 图记忆和上下文删掉。而重启后会按 registry 重建同名子 —— 于是它
	// **记忆全空**地复活，用户看到的是"挂了一晚上的子醒来什么都不记得"。
	//
	// 现在的规则：
	//	persist=false（显式 destroy / reclaim）⇒ 删（原语义）
	//	persist=true （进程停机）              ⇒ 保留，供重启后重建复用
	if !persistTemp && rc.dir != "" && strings.Contains(rc.tempPath, rc.ID) {
		_ = os.RemoveAll(rc.dir)
	}
	// 处置划入的 inputch（按父的决定：悬空 / 显式交接）。
	if reg := m.A.IO().ChannelRegistry(); reg != nil {
		for _, ch := range rc.inputChs {
			switch disp {
			case ChannelHandover:
				// 显式交接：通道归属改为**目标 agent**，并交出暂存任务。
				//
				// 目标缺省是父（原 ChannelTakeover 的语义）；handoverTarget 非空时
				// 交给那个子 —— "划拨给其他子 agent" 就是这么表达的。
				target := handoverTarget
				if target == "" {
					target = string(m.A.ID())
				}
				drained, err := reg.AssignAndDrainStash(ch, target, 0)
				if err != nil {
					log.Printf("[resident] %s 交接通道 %s 给 %s 失败: %v", rc.ID, ch, target, err)
					continue
				}
				if len(drained) > 0 {
					log.Printf("[resident] %s 交接通道 %s 给 %s，释放暂存任务 %d 条",
						rc.ID, ch, target, len(drained))
					if target == string(m.A.ID()) {
						// 交给父 ⇒ 直接进父的调度队列（原 takeover 行为）。
						if onDrained != nil {
							onDrained(drained)
						} else {
							for _, evt := range drained {
								if evt != nil {
									m.sched.Enqueue(sched.NewInputTask(evt))
								}
							}
							m.sched.SignalWake()
						}
					} else {
						// 交给另一个子 ⇒ 交出来的任务必须投给那个子，
						// 否则它们会落在父队列里而父已经不持有这条通道了
						// （"接回了但活没人做"的同型静默缺陷）。
						if rcTarget, ok := m.residentEntryOf(target); ok && rcTarget.agent != nil {
							for _, evt := range drained {
								if evt != nil {
									rcTarget.agent.IO().DeliverRouted(evt)
								}
							}
							log.Printf("[resident] %s 交接的 %d 条任务已投给 %s", rc.ID, len(drained), target)
						} else {
							log.Printf("[resident] %s 交接目标 %s 不存在，%d 条暂存任务留在通道上",
								rc.ID, target, len(drained))
							_ = reg.Stash(ch, drained)
						}
					}
				}
			default: // ChannelOrphan（默认，悬空）
				// 悬空：Owner 清空 + Orphaned 标记 ⇒ 期间输入写进 stash，
				// 等父显式接回/划拨。**不**留给根 agent 兜底。
				if err := reg.Orphan(ch); err != nil {
					log.Printf("[resident] %s 悬空通道 %s 失败: %v", rc.ID, ch, err)
					continue
				}
				log.Printf("[resident] %s 通道 %s 已悬空（暂存 %d 条，等显式接回或划拨）",
					rc.ID, ch, reg.StashLen(ch))
			}
		}
		// 注销"父接收该子消息"的入站 inputch（child/<id>）。
		//
		// 它由 residentInboundChannel 在 create 时登记（Owner=父），销毁时必须
		// 一并摘掉：登记表是共享的、按 name 全局唯一，残留会随 create/destroy
		// 次数单调累积脏数据。实测：destroy 后 child/<id> 仍挂在根 agent 名下，
		// 而外部没有任何工具能单独注销 inputch，只能重启 homed 清。
		// 注意用纯函数算名字，不要再走 residentInboundChannel（会重新登记）。
		reg.Unregister(inboundChannelName(rc.ID))
	}
}

// StopResidents 销毁全部驻留子（父退出时必须；不留孤儿）。
// ★ 与 DestroyResident（单数）共用 teardownResident —— 两者都由骨架的
// Cleanup 驱动撤销策略/注销入站，本函数只补通道处置那一段（需要父的 sched）。
func (m *M) StopResidents() int {
	all := m.A.Children().ByKind(childpkg.KindResident)
	for _, e := range all {
		m.A.Children().Remove(e.Base().ID)
		if rc, ok := e.(*residentChild); ok {
			// ★ 进程停机 ⇒ 保留 temp（供重启后重建复用）。
			// 显式 Destroy/Reclaim 走另一条路径，那里才删。
			m.teardownResidentWith(rc, ChannelOrphan, "", true, nil)
		}
	}
	return len(all)
}

// Residents 返回登记表的快照（按 id 排序，便于断言与展示稳定）。
func (m *M) Residents() []ResidentInfo {
	entries := m.A.Children().ByKind(childpkg.KindResident)
	out := make([]ResidentInfo, 0, len(entries))
	for _, e := range entries {
		if rc, ok := e.(*residentChild); ok {
			out = append(out, rc.info())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ResidentTable 是父**查看**子的 inputch 处理表（pull，不打断子）。
//
// ★ 表是**那个实例自己的内部数据**（2026-11-26 修）。
//
// 记账发生在子的运行期（`driver.go` 的 `executeNewTask`），写在**子的载体**上。
// 本方法跑在 handle 所在的载体上（内核把 `InstanceManager` 交给
// `Descriptor.Admin`，resident 的 handle 用 `Of(host)` 取根载体），
// 所以 `m.st` 是**父侧**的表 —— 它永远是空的。
//
// 正确取法：经**内核实例表**定位该 id 对应的实例，再取它自己的载体。
// 数据没有离开 resident 类型的作用域（两边都是本包的 `M`），
// 父子之间也没有直连通道 —— 定位走的是 `m.A.Children()`（内核的实例表）。
func (m *M) ResidentTable(id string) ([]InputchRecord, error) {
	rc, ok := m.residentEntryOf(id)
	if !ok {
		return nil, fmt.Errorf("驻留子 %s 不存在", id)
	}
	sub := wrapOf(rc)
	if sub == nil {
		return nil, fmt.Errorf("驻留子 %s 的运行期未装配", id)
	}
	return sub.inputchTableSnapshot(), nil
}

// SendToResident 是父的"发送消息"：经输出通道寻址到该子的 inputch，
// 对子而言是 **L4 中断**（取消当前状态 + 插入新消息）。
func (m *M) SendToResident(id, text string) error {
	rc, ok := m.residentEntryOf(id)
	if !ok {
		return fmt.Errorf("驻留子 %s 不存在", id)
	}
	ch := "sub/" + id
	if len(rc.inputChs) > 0 {
		ch = rc.inputChs[0]
	}
	// 来源 = 父；子的 KernelSource 是父 ⇒ 在子的阶梯上这是合法的 L4。
	rc.agent.IO().InjectInterruptTextOpts(string(m.A.ID()), ch, text,
		agentIO.InjectOptions{Priority: "L4"})
	return nil
}

// CompressResident 是父的"压缩"（**保留语义**）：**压成摘要** + 清理处理表，子继续存在。
//
// ## 这里曾经是纯截断（2026-10-30 修正）
//
// 原实现直接 `TrimKeepRecent(20)` —— 丢掉除了最近 20 条之外的一切，
// 与设计文档明写的「**压成摘要**」（resident-subagent-design.md §9, R7）
// 不符。两个后果：
//
//	① "压缩"与"零成本丢数据"没有区别，被丢的语义不可恢复
//	② 压缩（保留）与回收（取消）在实现上退化成同一个动作的两种力度
//
// 现在的顺序（用户 2026-10-30 裁定 A+B）：
//
//	① PlanCompaction  取一份**只读计划**（不动上下文）
//	② 用**子自己的** provider 生成摘要（设计 R7："子的轻量内核自己压"）
//	③ ApplyCompaction 用摘要事件**替换**那段
//	④ 摘要失败 ⇒ **回退截断**（裁定 B），并留日志（不静默降级）
//
// 为什么先出计划再执行：摘要要调 LLM（可能几十秒），期间不能持锁、也不能
// 先把事件摘掉 —— 那样一旦失败/取消，上下文就被破坏了一半。
func (m *M) CompressResident(id string) (int, error) {
	rc, ok := m.residentEntryOf(id)
	if !ok {
		return 0, fmt.Errorf("驻留子 %s 不存在", id)
	}

	ctx := rc.ctx
	compacted := 0

	if ctx != nil {
		// ① 只读计划：此时上下文一个字都没动。
		plan := ctx.PlanCompaction(residentKeepRecent)
		if len(plan.Old) > 0 {
			// ② 用**子自己**的 provider 压（裁定 A）。
			summary, err := m.summarizeForCompaction(plan.PrefixText)
			if err != nil {
				// ③ 裁定 B：回退截断。必须留日志 —— 静默降级会让运维
				//    永远不知道"压缩退化成丢弃"发生了多少次。
				log.Printf("[resident] %s compaction summary failed (falling back to truncation — "+
					"%d events will be DISCARDED, not summarized): %v", id, len(plan.Old), err)
				compacted = ctx.TrimKeepRecent(residentKeepRecent)
			} else {
				// ④ 摘要替换那段（不是丢弃）。
				compacted = ctx.ApplyCompaction(plan, summary)
				log.Printf("[resident] %s compacted %d events into a %d-char summary (kept last %d)",
					id, compacted, len([]rune(summary)), residentKeepRecent)
			}
		}
	}

	m.clearInputchTable() // 处理表记的是被压掉那段窗口的逐轮处理 ⇒ 必须清
	// 清掉"只上报一次"的去重标记：压缩之后可再次上报。
	m.st.ctxFullMu.Lock()
	m.st.ctxFullSignaled = false
	m.st.ctxFullMu.Unlock()
	rc.mu.Lock()
	rc.state = "running"
	rc.mu.Unlock()
	return compacted, nil
}

// ReclaimResident 是父的"回收"（**取消语义**）：父读子的 temp → 选记录 → 合入 main
// → 丢弃 temp → **取消**该驻留子。
//
// keep 由父决定"哪些纳入记忆"；为 nil 时表示全部合入。
// ★ 已删 `HarvestResidentMemory`（2026-11-20）：它读子 agent 的 temp 图库
// 并合进主记忆。而现在**没有子 temp 库** —— 子 agent 的写入由
// `graph_commit` 直接落主库（按调用方打归属标记）。所以"收割"这件事
// 从结构上不存在了。

func (m *M) ReclaimResident(id string, keep func([]InputchRecord, []model.Triple) []model.Triple) (ResidentInfo, error) {
	rc, ok := m.residentEntryOf(id)
	if !ok {
		return ResidentInfo{}, fmt.Errorf("驻留子 %s 不存在", id)
	}
	info := rc.info()

	// 收割：读 temp 的全部活跃三元组（比通过图记录选择更直接）。
	//
	// 逻辑抽到 harvestResidentMemory，供 FinishResident 的 onDestroy 复用 ——
	// 那里也必须**在 Destroy 之前**收割（Destroy 会关掉 temp）。
	// ★ 不再有 temp 库可收割（见 HarvestResidentMemory 的删除说明）。
	promoted := 0
	var herr error
	if herr != nil {
		return info, herr
	}

	// 收割完成 ⇒ 取消该驻留子（回收是取消语义，≠ 压缩）。
	if err := m.DestroyResident(id); err != nil {
		return info, err
	}
	info.State = fmt.Sprintf("reclaimed(promoted=%d)", promoted)
	return info, nil
}

// handleChildContextFull 把子的 contextfull 当**内核级事件**上报给父自己：
// 父侧 L4 中断（带子标识）—— 只推信号，细节靠"查看"拉状态面。
func (m *M) handleChildContextFull(rc *residentChild) {
	rc.mu.Lock()
	rc.state = "contextfull"
	rc.mu.Unlock()
	m.raiseKernelInterruptAt("child/"+rc.ID, "kernel", residentContextFullText(rc.ID), sched.LevelCritical)
}

// checkContextFull 的实现已搬到 resident 自己的 OverflowHandler 里。
//
// ★ 基座不算水位：它只承接内核上报的 contextfull 异常，然后调类型注册的
// handler。水位怎么算、超了报给谁、去重标记放哪 —— 全是类型策略。

// ---- 子侧：inputch 处理表（子持有，父 pull） ----

// autoRecordInputch 是**系统自动写**兜底：本轮未主动写时，把该轮 inputch 的处理信息写入。
// 保证每一轮必有记录，父不会看到空洞。
func (m *M) AutoRecordInputch(f *ResidentFrame) {
	if f == nil {
		return
	}
	r := m.st
	if r == nil {
		return
	}
	r.tableMu.Lock()
	defer r.tableMu.Unlock()
	if r.pending != nil {
		rec := *r.pending
		r.pending = nil
		rec.InputCh = firstNonEmpty(rec.InputCh, r.currentInputch)
		r.table = append(r.table, rec)
		return
	}
	text := fmt.Sprintf("轮次完成：输入=%s", agent.TruncateStr(f.Input, 80))
	if f.Response != "" {
		text += "；产出=" + agent.TruncateStr(f.Response, 120)
	}
	r.table = append(r.table, InputchRecord{
		InputCh: r.currentInputch, At: time.Now(), Proactive: false, Text: text,
	})
}

func (m *M) inputchTableSnapshot() []InputchRecord {
	r := m.st
	if r == nil {
		return nil
	}
	r.tableMu.Lock()
	defer r.tableMu.Unlock()
	return append([]InputchRecord(nil), r.table...)
}

func (m *M) clearInputchTable() {
	r := m.st
	if r == nil {
		return
	}
	r.tableMu.Lock()
	defer r.tableMu.Unlock()
	r.table = nil
	r.pending = nil
}

// ---- 子侧：主动向父发消息 ----
//
// ★ 上行只有一条路：`CallParent`。曾经有个 `NotifyParentFrom` + `notify_parent`
//   工具，与它并存 —— 两条上行路径意味着"有的消息带 level/kind，有的不带"，
//   父侧无法统一处置。已删（用户反复裁定：父子只有两条协议）。

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// residentKeepRecent 是压缩时保留的最近事件条数。
const residentKeepRecent = 20

// contextFullRatio 是触发 contextfull 的占比：积累上下文超过窗口的这个比例就报。
// 取 0.9 而不是 1.0：留一点余量，让父 agent 在"下一次必须丢事件"之前就能决策
// （压缩 / 回收 / 销毁），而不是等已经丢了再报。
const contextFullRatio = 0.9

// defaultMaxContextTokens 是 provider 未报告窗口时的兜底（与 ComputeTokenBudget 一致）。
const defaultMaxContextTokens = 32768

func (rc *residentChild) info() ResidentInfo {
	rc.mu.Lock()
	state, full := rc.state, rc.state == "contextfull"
	rc.mu.Unlock()
	table := wrapOf(rc).inputchTableSnapshot()
	// 子自己的调度器积压（per-agent 负载展示用）。schedulerStatus 只读快照，
	// 每次状态轮询为每个子算一次，成本可忽略（驻留子数量是个位数）。
	sc := wrapOf(rc).SchedulerStatus()
	info := ResidentInfo{
		ID: rc.ID, State: state, InputChs: append([]string(nil), rc.inputChs...),
		AllowedOutputs: append([]string(nil), rc.allowed...),
		ContextFull:    full, CreatedAt: time.Unix(0, rc.CreatedAt), TableSize: len(table),
		SchedReady:   sc.ReadyQueueDepth,
		SchedPending: sc.PendingInterrupts,
		SchedStack:   sc.SuspendStack,
		SchedQueues:  sc.InterruptQueues,
		// Rounds = 子**已执行的轮次数**（调度器的执行计数，单调不减）。
		//
		// 此前这里根本没填这个字段 ⇒ 父看到的永远是 `轮次=0`，与"处理表已有 N 条"
		// 自相矛盾（现场：子明明处理了两轮，父读到 rounds=0，误判成"子没干活"）。
		// 注意它**不等于** len(table)：处理表记的是"当前上下文窗口内"的轮次，
		// 压缩会清空（§8.3），所以窗口内的条数会被重置，而轮次总数不会。
		Rounds: rc.agent.RoundsExecuted(),
		// OffloadOwned：让父分清"我建的子"与"内核临时拉的分诊助手"。
		// 父据此决定回收策略（临时的可以空闲时回收，正式的按需保留）。
		OffloadOwned: rc.offloadOwned,
	}
	if len(table) > 0 {
		info.Table = table
	}
	return info
}

// MarshalResidentInfo 便于工具输出（单工具多视图用 JSON 视图）。
func MarshalResidentInfo(info ResidentInfo) string {
	b, err := json.Marshal(info)
	if err != nil {
		return fmt.Sprintf("%+v", info)
	}
	return string(b)
}

// compactionSummaryPrompt 是压缩摘要的指令。
//
// 三条硬要求，每条都对应一种坏摘要：
//   - 保留事实（人名/数字/决定/未完成项）—— 否则摘要等于把知识蒸干了
//   - 不得编造 —— 模型很容易"补全"它以为该有的结论
//   - 输出纯文本，不要复述"以下是摘要"这类客套 —— 它会被原样插回上下文
const compactionSummaryPrompt = `把下面这段对话压缩成一段摘要，供你自己后续继续工作使用。

要求：
1. **保留全部事实**：人物、数字、时间、已作出的决定、结论、以及**尚未完成的事项**。
2. **不得编造**：原文没有的内容一律不要补充；不确定的写「未确认」。
3. 丢掉过程细节（逐条工具调用的原始输出、寒暄、重复确认），保留结果。
4. 直接输出摘要正文，不要写「以下是摘要」之类的开场白。
5. 控制在 300 字以内。

对话原文：
%s`

// summarizeForCompaction 用**本 agent 自己的 provider** 把一段原文压成摘要。
//
// ## 为什么用子自己的 provider（设计 R7 / 用户 2026-10-30 裁定 A）
//
// 「压缩由**子的轻量内核自己压**（它有自己的上下文与 LLM）」——
// resident-subagent-design.md:640。语义上压缩是**子对自身记忆的整理**，
// 它的视角、它认为重要的东西，与父不同。让父代劳会让子的记忆被父的兴趣点
// 重塑 —— 那不是压缩，是替换。
//
// ## 失败语义（用户裁定 B：回退截断）
//
// 返回错误时调用方**退回 TrimKeepRecent**（截断），并记日志。
// 不抛错让压缩失败：contextfull 时子已经顶着满上下文，压缩再失败就等于
// 把"能不能继续干活"赌在一次可能超时的 LLM 调用上。
func (m *M) summarizeForCompaction(text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("没有可压缩的内容")
	}
	if m == nil {
		return "", fmt.Errorf("本 agent 没有可用的 LLM provider")
	}

	req := &agentAPI.CompletionRequest{
		Messages: []agentAPI.Message{
			{Role: "user", Content: fmt.Sprintf(compactionSummaryPrompt, text)},
		},
		// 摘要不需要长输出；给足 300 字（中文约 600 token）+ 余量。
		MaxTokens:       1024,
		ToolChoice:      "none",
		DisableThinking: true,
	}
	// 用本 agent 的 ctx：父退出/子被销毁时压缩自然中止（不留下悬空调用）。
	// ★ 走内核 LLM 设施（单次、不重试）：压缩是"已经顶着满上下文"时的
	//   补救动作，重试只会让它更晚。
	resp, err := m.A.CompleteLLMNoRetry(m.A.Ctx(), req)
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(resp.Content)
	if out == "" {
		return "", fmt.Errorf("摘要器返回空内容")
	}
	return out, nil
}

// loadContext 从**内核传入的实例目录**恢复本实例的整窗上下文。
//
// ★ 驻留子的上下文归**子自己的载体**（不是父的）：它有自己的调度循环，
//
//	读写的是自己的 RelevanceContext。
//
// 目录里没有 context.json ⇒ 首次启动，正常。
func (rc *residentChild) loadContext(dir string) error {
	if rc == nil || rc.agent == nil || dir == "" {
		return nil
	}
	p := filepath.Join(dir, contextFileName)
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	m := Of(rc.agent)
	if m == nil {
		return nil
	}
	m.OpenContext()
	if m.ctx != nil {
		rc.ctx = m.ctx
	}
	return nil
}

// residentTaskOutputChannel 是"给这个驻留子投任务时，回复该走哪个输出通道"。
//
// 两级取法，都不留空：
//
//	① 父为它授权的第一个输出通道（`allowed_outputs`）
//	② 父自己的第一个输出通道（父怎么说话，子就怎么说话）
//
// ★ 返回空串是**不允许**的：`outputChannelOf(evt)` 在 OutputChannel 为空时
//
//	会回落到 `evt.Source`（`parent/main`），那是个来源名不是通道 ——
//	于是流式增量全被 io 丢弃（`no output device "parent/main"`）。
//	所以必须有兜底，且兜底要从真实的通道表里取。
func (m *M) residentTaskOutputChannel(opts ResidentOptions) string {
	for _, ch := range opts.AllowedOutputs {
		if ch = strings.TrimSpace(ch); ch != "" {
			return ch
		}
	}
	if m.A == nil || m.A.IO() == nil {
		return ""
	}
	for _, ch := range m.A.IO().ListChannels() {
		if ch.Type == agentIO.DeviceOutput || ch.Type == agentIO.DeviceIO {
			return ch.Name
		}
	}
	return ""
}
