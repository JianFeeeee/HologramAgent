package core

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
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// InputchRecord 是 inputch 处理表的一条记录（子持有，父 pull）。
type InputchRecord struct {
	InputCh   string    `json:"inputch"`
	At        time.Time `json:"at"`
	Proactive bool      `json:"proactive"` // true = 子主动写入；false = 系统自动写
	Text      string    `json:"text"`
}

// ResidentOptions 是创建一个驻留子的参数（父的"创建"动作）。
type ResidentOptions struct {
	// ID 是子 agent 的 id（同时是登记表的键、跨 agent 寻址的依据）。
	ID string
	// TaskPrompt 是在固定提示词之上注入的**任务提示词**。
	TaskPrompt string
	// InputChs 是**划入**给这个子的 inputch（单位 = inputch；可来自同一插件的多个）。
	InputChs []string
	// AllowedOutputs 是授权给它的输出通道集合（nil/空 = 完整授权）。
	AllowedOutputs []string
	// Capacity 是划入 inputch 的队列容量（0 = 内核默认）。
	Capacity int
	// TempPath 是它 temp 图记忆的存储路径（必填；与子同生共死）。
	TempPath string
	// PreferredProvider 是**这个驻留子**优先使用的模型源（空 = 继承父的）。
	//
	// 为什么由父在创建时传入、而不是让子自己读配置：子没有配置中心句柄
	// （轻量内核刻意不接），而"哪个源给哪类 agent 用"是**部署策略**，
	// 本来就该在创建点决定。
	PreferredProvider string
	// OffloadOwned 标记这是**内核为转投而拉起**的驻留子（见 offload.go）。
	//
	// 为什么要区分：自动转投会复用、也会按上限限制自己拉起的子，
	// 但**绝不能**把人（父的模型）建的子算进去或拿去复用 ——
	// 那会把人工安排的工作负载搬到一个本来在做别的事的 agent 上。
	OffloadOwned bool
	// AllowedTools 是**父为这个子声明**的工具白名单（空 = 不作额外限制）。
	//
	// ## 为什么由父在创建时给（用户裁定 2026-10-30）
	//
	// 子 agent 没有配置中心句柄（轻量内核刻意不接），也**没有资格自己决定权限**
	// —— 权限只能来自委派者。这与 PreferredProvider 同一道理：
	// “用哪个源/给多少权限”是**委派策略**，本来就该在创建点决定。
	//
	// 内核持有这份声明（`security.Runtime` 的主体表），子崩了/被替换也不会丢：
	// 否则“挂掉子就能绕过”就是现成的提权口子。
	AllowedTools []string
	// DenyTools 是**父为这个子声明**的追加禁止（父自己能用、但不给这个子用）。
	//
	// 与 AllowedTools 同时给时：两者**同时生效**（先 deny 后 allow，deny 优先）。
	// 典型用法：不给白名单（沿用父的），只禁掉几个高危工具。
	DenyTools []string
}

// ResidentInfo 是父对某个驻留子的可查询状态（登记表条目 + 状态面摘要）。
type ResidentInfo struct {
	ID             string    `json:"id"`
	State          string    `json:"state"`
	InputChs       []string  `json:"inputchs"`
	AllowedOutputs []string  `json:"allowed_outputs"`
	Rounds         int       `json:"rounds"`
	ContextFull    bool      `json:"context_full"`
	CreatedAt      time.Time `json:"created_at"`
	TableSize      int       `json:"table_size"`
	// OffloadOwned 标记这是内核为承接积压而拉起的**临时分诊助手**（见 offload.go）。
	OffloadOwned bool            `json:"offload_owned,omitempty"`
	Table        []InputchRecord `json:"table,omitempty"`

	// Sched* 是这个驻留子**自己的**输入调度器积压摘要（排队 / 待处理中断 /
	// 中断栈 / 四级中断队列）。
	//
	// 为什么必须单列：每个驻留子是独立 agent，跑自己的事件循环与调度器
	// （见 agent.go 的 newScheduler）。KernelStatus.Scheduler 只是**根**那份，
	// 拿它代表全部子会让界面把「某个子堵死」显示成「一切正常」。
	SchedReady   int    `json:"sched_ready"`
	SchedPending int    `json:"sched_pending"`
	SchedStack   int    `json:"sched_stack"`
	SchedQueues  [5]int `json:"sched_queues"`
}

type residentChild struct {
	id        string
	agent     *Agent
	light     *memory.LightMemory
	mainRO    *memory.GraphDB
	tempPath  string
	dir       string
	inputChs  []string
	allowed   []string
	createdAt time.Time
	// offloadOwned 标记这是内核为转投拉起的子（见 offload.go）。
	offloadOwned bool

	mu    sync.Mutex
	state string // running | contextfull | stopped
}

// mainGraph 返回父自己的完整图记忆库（驻留子的受限句柄由它派生）。
func (a *Agent) mainGraph() *memory.GraphDB { return a.memory }

// SpawnResident 创建一个驻留子（父的"创建"动作）。
func (a *Agent) SpawnResident(opts ResidentOptions) (ResidentInfo, error) {
	if strings.TrimSpace(opts.ID) == "" {
		return ResidentInfo{}, fmt.Errorf("驻留子必须有 id")
	}
	if strings.TrimSpace(opts.TempPath) == "" {
		return ResidentInfo{}, fmt.Errorf("驻留子必须给出 temp 图记忆路径")
	}
	main := a.mainGraph()
	if main == nil {
		return ResidentInfo{}, fmt.Errorf("父没有图记忆，无法为驻留子提供主库只读视图")
	}

	a.residentMu.Lock()
	if a.residents == nil {
		a.residents = map[string]*residentChild{}
	}
	if _, dup := a.residents[opts.ID]; dup {
		a.residentMu.Unlock()
		return ResidentInfo{}, fmt.Errorf("驻留子 %s 已存在", opts.ID)
	}
	a.residentMu.Unlock()

	// ① 轻量内核的记忆装配：主库**受限句柄**（结构上写不进）+ 自己的 temp 实例。
	// temp 目录由内核创建（调用方只给路径）——与子同生共死，销毁时整目录丢弃。
	if dir := filepath.Dir(opts.TempPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return ResidentInfo{}, fmt.Errorf("创建 temp 目录: %w", err)
		}
	}
	mainRO, err := memory.OpenGraphDBReadOnly(main.Path())
	if err != nil {
		return ResidentInfo{}, fmt.Errorf("打开主库受限句柄: %w", err)
	}
	light, err := memory.NewLightMemory(mainRO, opts.TempPath, true)
	if err != nil {
		_ = mainRO.Close()
		return ResidentInfo{}, err
	}

	// ② 划入 inputch（登记表里记归属；一个插件的多个 inputch 可分别划给不同子）。
	if reg := a.io.ChannelRegistry(); reg != nil {
		for _, ch := range opts.InputChs {
			if err := reg.Assign(ch, opts.ID, opts.Capacity); err != nil {
				_ = light.Close()
				_ = mainRO.Close()
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
	if reg := a.io.ChannelRegistry(); reg != nil {
		childIO.SetChannelRegistry(reg)
	}
	childIO.SetParentIO(a.io)

	parentID := string(a.id)
	child := New(AgentConfig{
		ID: types.AgentID(opts.ID),
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
		SystemPrompt:    a.systemPrompt,
		Provider:        a.provider,
		ProviderManager: a.providerManager,
		// 驻留子可以有自己的优先源；留空则继承父的（父再留空 ⇒ 总默认源）。
		PreferredProvider: firstNonEmpty(opts.PreferredProvider, a.preferredProvider),
		IO:                childIO,
		StageHost:         a.stageHost,
		LightMemory:       light, // 轻量内核：只有图记忆共同面
		AllowedOutputs:    opts.AllowedOutputs,
		KernelSource:      parentID, // 子的 L4 只属于父
		ParentID:          parentID,
		TaskPrompt:        opts.TaskPrompt,
		// ★ 安全运行时必须继承（2026-10-30 补）。
		//
		// 驻留子是长期挂着、无人盯的执行体：它自己跑调度器、接自己的输入、
		// 长时间不被人工审阅。它是本仓**最需要闸门**的一类 agent，
		// 而原先它是唯一**完全没有闸门**的（子从 New 起 a.security 就是 nil）。
		Security: a.security,
	})

	rc := &residentChild{
		id: opts.ID, agent: child, light: light, mainRO: mainRO,
		tempPath: opts.TempPath, dir: filepath.Dir(opts.TempPath),
		inputChs:  append([]string(nil), opts.InputChs...),
		allowed:   append([]string(nil), opts.AllowedOutputs...),
		createdAt: time.Now(), state: "running",
		offloadOwned: opts.OffloadOwned,
	}

	// ★ 父为这个子**声明主体策略**（用户裁定 2026-10-30）。
	//
	// 位置必须在“子开始跑”之前：子一旦接到第一条输入就会执行工具，
	// 那个时刻策略必须已经在内核里 —— 否则存在一个“无约束执行”的窗口。
	//
	// 声明失败要**回滚整个创建**：一个“本该受限却无限”的执行体
	// 比创建失败危险得多（而创建失败是可重试的）。
	if err := a.declareChildPolicy(opts.ID, opts.AllowedTools, opts.DenyTools); err != nil {
		child.Stop()
		if light != nil {
			_ = light.Close()
		}
		_ = mainRO.Close()
		_ = os.RemoveAll(filepath.Dir(opts.TempPath))
		return ResidentInfo{}, fmt.Errorf("为驻留子 %s 声明安全策略失败（拒绝创建一个无约束的子执行体）: %w", opts.ID, err)
	}

	// ④ 子 → 父的主动消息（**L3 中断**，带子标识）：投进父的 inputch。
	parentInCh := a.residentInboundChannel(opts.ID)
	// 登记了入站 inputch 就必须有回滚路径：注册点与 residents 登记之间
	// 当前无可失败步骤，但一旦将来插入（或 child.Start 变更），早退会把这条
	// 登记留在共享表里——就是 child/<id> 残留那只 bug 的另一条入口。
	// defer 保证“只要没走到成功返回，就注销这条登记”。
	registered := false
	defer func() {
		if registered {
			return
		}
		if reg := a.io.ChannelRegistry(); reg != nil {
			reg.Unregister(inboundChannelName(opts.ID))
		}
	}()
	child.notifyParent = func(text string) {
		a.io.InjectInterruptTextOpts(opts.ID, parentInCh, text,
			agentIO.InjectOptions{Priority: "L3"})
	}
	// ⑤ 子的 contextfull → 父侧的**内核级事件**（L4，带子标识）。
	child.onContextFull = func() { a.handleChildContextFull(rc) }

	a.residentMu.Lock()
	a.residents[opts.ID] = rc
	a.residentMu.Unlock()

	child.Start()

	// ⑥ create 即开工：把任务提示词作为**第一条排队输入**投给子。
	//
	// 为什么必须在这里投：TaskPrompt 只进子的系统提示词（"你是谁、要做什么"），
	// 而**不会**让子跑起来 —— 实测现象是子启动后 rounds=0、永远待机
	// （日志 `[agent] r1 started, waiting for IO interrupts` 之后无事发生）。
	// 走排队输入（非中断）：创建是"安排工作"，不是"打断它正在做的事"。
	if strings.TrimSpace(opts.TaskPrompt) != "" {
		child.io.InjectInputTo(a.residentParentSource(), parentInCh, "text",
			map[string]interface{}{"content": opts.TaskPrompt})
	}
	registered = true
	return rc.info(), nil
}

// residentParentSource 是"父给子投递"的输入来源名（子的视角里能看出是谁发的）。
func (a *Agent) residentParentSource() string { return "parent/" + string(a.id) }

// residentInboundChannel 是"父接收某个子的消息"的 inputch 名（登记进登记表可见）。
//
// inboundChannelName 只拼名字，不产生副作用（注销路径要用它算出同一个名字，
// 不能再去调 residentInboundChannel——那会顺手把刚摘掉的登记又写回去）。
func inboundChannelName(childID string) string { return "child/" + childID }

func (a *Agent) residentInboundChannel(childID string) string {
	ch := inboundChannelName(childID)
	if reg := a.io.ChannelRegistry(); reg != nil {
		// 归属父自己：它是父的入站 inputch。
		_ = reg.Register(agentIO.InputChannel{Name: ch, Plugin: "resident", Owner: string(a.id)})
	}
	return ch
}

// DestroyResident 立刻销毁一个驻留子并从登记表移除（父的"销毁"动作；不收割）。
//
// 销毁是父**随时**可做的；父退出时由 StopResidents 对全部子执行。
func (a *Agent) DestroyResident(id string) error {
	return a.DestroyResidentWith(id, ChannelRelease)
}

// DestroyResidentWith 按父的决定处置通道后销毁驻留子。
func (a *Agent) DestroyResidentWith(id string, disp ChannelDisposition) error {
	a.residentMu.Lock()
	rc, ok := a.residents[id]
	if ok {
		delete(a.residents, id)
	}
	a.residentMu.Unlock()
	if !ok {
		return fmt.Errorf("驻留子 %s 不存在", id)
	}
	a.teardownResidentWith(rc, disp, nil)
	return nil
}

// ResidualPolicy 是父对**残余任务**的显式决定。
//
// 为什么需要它（用户 2026-09-19 明确要求）：回收/销毁驻留子时，
// 它手头可能还有**尚未处理的消息**。这些消息的处置不能由内核悄悄决定：
//   - 直接丢弃 → 用户消息无声消失（异步通道更是零反馈：qq 无 ResponseCh，
//     用户不知道发生了什么，系统里也没任何痕迹）；
//   - 无条件转回父 → 父本来就很忙，把一堆活重新塞回去可能反而加剧积压。
//
// 因此与其它控制面动作一致：**原语在内核，决定在父的模型**（设计 §7）。
// 内核负责把残余任务**列清楚**（来源、通道、内容），父选 keep（转回自己）/ drop。
//
// 无论选哪个，内核都会**逐条留日志**：丢弃必须可追溯。
// 默认（不传 policy）取 keep：宁可多做一件，不可默默丢一条。
type ResidualPolicy string

const (
	// ResidualKeep 把残余任务转回父自己的队列，父稍后处理。
	ResidualKeep ResidualPolicy = "keep"
	// ResidualDrop 明确丢弃残余任务（父已看过清单并确认）。
	ResidualDrop ResidualPolicy = "drop"
	// ResidualStash 把残余任务**暂存到其原通道**，等通道被接回时再放出。
	//
	// ## 与 keep 的区别（用户 2026-10-30 提出）
	//
	// keep 是"转回父自己的队列"—— 任务立刻变成父的活。
	// stash 是"放回它来的那条通道"—— 任务不与任何 agent 绑定，
	// 谁接回那条通道，谁就拿到它们。
	//
	// 三个动作正好对应三种真实情况：
	//
	// | 选择 | 适用 |
	// |---|---|
	// | keep（接手） | 父有空、或这批活必须连续处理 |
	// | stash（暂存到通道） | 父现在忙，但活不能丢；下一个接手该通道的 agent 来做 |
	// | drop（丢弃） | 这批活已过期/已由别处完成 |
	//
	// ★ stash 的前提是"通道能挂起"：留空 Owner 的通道如果没人接，
	// 任务就静静躺着。所以 stash 必须**逐条留日志**（和 drop 同等待遇），
	// 且 ResidentInfo 里要能查到暂存条数 —— 否则它就是"更好的丢"。
	ResidualStash ResidualPolicy = "stash"
	// ResidualTakeover 是 keep 的别名，语义是"父**接手**这些活"。
	//
	// 单独给一个名字是因为用户要求里"接手"是一个独立选项，而 keep
	// 听起来像"保持原状"。两者实现完全一致（转回父队列），
	// 别名让调用方的意图在代码里可读。
	ResidualTakeover ResidualPolicy = "takeover"
)

// TakeResidual 取走一个驻留子手头**尚未处理**的残余任务（不处置）。
//
// 调它**会**从子的队列里移除这些任务，所以父必须先看返回值再决定：
// 一旦取走，不再调 ApplyResidual 就等于把它们丢了。控制面工具走 ApplyResidual
// （取+处置一步完成）以避免这个误用。
func (a *Agent) TakeResidual(id string) ([]*agentIO.InputEvent, error) {
	a.residentMu.Lock()
	rc := a.residents[id]
	a.residentMu.Unlock()
	if rc == nil || rc.agent == nil {
		return nil, fmt.Errorf("驻留子 %s 不存在", id)
	}
	return rc.agent.sched.takeAllPendingEvents(), nil
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
func (a *Agent) FinishResident(id string, policy ResidualPolicy, onDestroy func(rc *residentChild) error) (n int, msg string, err error) {
	return a.FinishResidentWith(id, policy, ChannelRelease, onDestroy)
}

// FinishResidentWith 是 FinishResident 的完整版：残余任务策略 + **通道处置**一次定。
//
// 为什么合成一个入口：两者有依赖关系 ——
//   - residual=stash 要求通道**继续存在**（暂存挂在通道上）；
//   - channel=release 会把通道还给池子（可能被别的 agent 接走）。
//
// 若拆成两次调用，中间窗口里通道可能已被别人接管，暂存任务就跑到了
// 一个跟它无关的 agent 手里。**一次定完，没有窗口。**
func (a *Agent) FinishResidentWith(id string, policy ResidualPolicy, disp ChannelDisposition, onDestroy func(rc *residentChild) error) (n int, msg string, err error) {
	a.residentMu.Lock()
	rc := a.residents[id]
	a.residentMu.Unlock()
	if rc == nil || rc.agent == nil {
		return 0, "", fmt.Errorf("驻留子 %s 不存在", id)
	}
	child := rc.agent

	// ① 停止调度：关 ctx.Done。调度循环在下次取任务前退出。
	child.cancel()
	// ② 等它真的停下（已在执行的那条会跑完，不再取新的）。
	child.sched.waitSchedulerStopped(schedStopTimeout, id)

	// ③ 此刻队列静止，回收残余任务没有歧义。
	events := child.sched.takeAllPendingEvents()
	n, msg, err = a.applyResidualEvents(id, events, policy)
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
	return n, msg, a.DestroyResidentWith(id, disp)
}

// ApplyResidual 按父给出的策略处置一个驻留子的残余任务。
//
// keep：逐条转回父自己的队列（保留原事件，含 ResponseCh 与来源/通道）；
// drop：逐条记录日志后丢弃（不可追溯的丢弃是不允许的）。
//
// 无论哪种，带 ResponseCh 的都要给终态，否则 cli/a2a 这类无超时同步调用方
// 会永久挂起（设计 §7 I5）。
func (a *Agent) ApplyResidual(id string, policy ResidualPolicy) (int, string, error) {
	// ⚠️ 本函数只取残余、**不终止子**，因此取完之后子仍在运行 ——
	// 控制面工具请用 FinishResident（停止调度 + 取 + 处置 + 销毁，一步完成），
	// 否则"取完到销毁之间"子还会收新任务，那些任务的结果随销毁丢失。
	//
	// 保留它是为了 TakeResidual 的配对语义（先取后自己决定），以及既有判据。
	a.residentMu.Lock()
	rc := a.residents[id]
	a.residentMu.Unlock()
	if rc == nil || rc.agent == nil {
		return 0, "", fmt.Errorf("驻留子 %s 不存在", id)
	}
	events := rc.agent.sched.takeAllPendingEvents()
	return a.applyResidualEvents(id, events, policy)
}

// applyResidualEvents 按策略处置已取走的残余任务（keep 转回父 / drop 留痕丢弃）。
func (a *Agent) applyResidualEvents(id string, events []*agentIO.InputEvent, policy ResidualPolicy) (int, string, error) {
	if len(events) == 0 {
		return 0, "无残余任务", nil
	}
	if policy == ResidualDrop {
		for _, evt := range events {
			// 丢弃必须留痕：异步通道（qq/wechat）没有 ResponseCh，
			// 不记日志的话“这条消息为什么没人回”永远查不出来。
			log.Printf("[resident] %s residual task discarded per parent decision: source=%s channel=%s request=%s content=%s",
				id, evt.Source, evt.OutputChannel, evt.RequestID, truncateStr(inputTextOf(evt), 80))
			a.emitSkippedReply(evt, "residual_dropped_by_parent")
		}
		return len(events), fmt.Sprintf("已按父的决定丢弃 %d 条残余任务（逐条已记日志）", len(events)), nil
	}
	// stash：暂存回**每条任务来的那条通道**。
	//
	// ★ 必须逐条按其自身 OutputChannel 归位，不能整批塞进一个通道 ——
	// 一个驻留子可能同时收 qq 和 webui 的消息，混存会导致接回 qq 的人
	// 拿到本该给 webui 的活。
	if policy == ResidualStash {
		reg := a.io.ChannelRegistry()
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
					id, ch, evt.Source, evt.RequestID, truncateStr(inputTextOf(evt), 80))
				a.emitSkippedReply(evt, "residual_stash_channel_gone")
				continue
			}
			stashed++
			// 暂存也要留痕：不可追溯的"暂存"和丢弃没区别。
			log.Printf("[resident] %s residual task stashed to channel %q: source=%s request=%s content=%s",
				id, ch, evt.Source, evt.RequestID, truncateStr(inputTextOf(evt), 80))
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
		a.sched.enqueue(newInputTask(evt))
	}
	a.sched.signalWake()
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

// teardownResident 停内核、放通道、丢 temp（销毁与回收共用）。
// ChannelDisposition 是父对**驻留子持有的通道**的显式决定。
//
// 用户 2026-10-30 要求：「原本驻留子持有的通道也应该支持**挂起或接回**主 agent」。
//
// 之前销毁驻留子时内核**无条件**把通道归到"未分配"（`Assign(ch, "", 0)`）：
// 通道确实回到了池子，但**谁也不知道该由谁来接**。于是有两种坏结局：
//   - 通道一直没人分配 ⇒ 投进来的消息无人处理，且无任何提示
//   - 刚好被别的 agent 抢到 ⇒ 那个 agent 莫名其妙开始收它没准备的流量
//
// 所以和残余任务的处置一样：**原语在内核，决定在父的模型**。
type ChannelDisposition string

const (
	// ChannelRelease 是原有行为：归属清空回到"未分配"，可被任意 agent 再分配。
	// 作为**零值**保持向后兼容（已有调用方不传就等于它）。
	ChannelRelease ChannelDisposition = ""
	// ChannelSuspend 挂起：归属清空但**标记为挂起**，不参与自动分配。
	//
	// 与 release 的差别是"会不会被别人悄悄接走"：挂起的通道等父显式接回，
	// 期间的输入被暂存（不是丢弃），接回时一并交出。
	ChannelSuspend ChannelDisposition = "suspend"
	// ChannelTakeover 接回主 agent：通道归属改为父，**并释放暂存任务**，
	// 那些任务立即转进父的队列。
	ChannelTakeover ChannelDisposition = "takeover"
)

func (a *Agent) teardownResident(rc *residentChild) {
	a.teardownResidentWith(rc, ChannelRelease, nil)
}

// teardownResidentWith 是 teardownResident 的完整版：额外决定**通道去向**，
// 并在 takeover 时把释放出来的暂存任务转进父队列。
func (a *Agent) teardownResidentWith(rc *residentChild, disp ChannelDisposition, onDrained func([]*agentIO.InputEvent)) {
	rc.mu.Lock()
	rc.state = "stopped"
	rc.mu.Unlock()

	// ★ 撤销父为它声明的主体策略（与 teardown 同时，早于一切资源回收）。
	//
	// 不撤销的后果：主体表以**调用方给的名字**为键，回收后同名重建是常规
	// 操作 ⇒ 新一代用上一代父留的约束运行；且表随 create/destroy 单调增长。
	// 与 rtchild 留档串线（rtClosedTranscripts）同一形态。
	a.revokeChildPolicy(rc.id)

	rc.agent.Stop() // 停子的调度器（取消其运行中的任务）
	if rc.light != nil {
		_ = rc.light.Close() // 关掉 temp 实例
	}
	if rc.mainRO != nil {
		_ = rc.mainRO.Close()
	}
	// temp 与子同生共死：连同目录一起丢弃（回收/销毁都不保留）。
	if rc.dir != "" && strings.Contains(rc.tempPath, rc.id) {
		_ = os.RemoveAll(rc.dir)
	}
	// 处置划入的 inputch（按父的决定：释放 / 挂起 / 接回）。
	if reg := a.io.ChannelRegistry(); reg != nil {
		for _, ch := range rc.inputChs {
			switch disp {
			case ChannelTakeover:
				// 接回父：通道归父，并把该通道上的暂存任务交出。
				drained, err := reg.AssignAndDrainStash(ch, string(a.id), 0)
				if err != nil {
					log.Printf("[resident] %s 接回通道 %s 失败: %v", rc.id, ch, err)
					continue
				}
				if len(drained) > 0 {
					log.Printf("[resident] %s 接回通道 %s，释放暂存任务 %d 条", rc.id, ch, len(drained))
					if onDrained != nil {
						onDrained(drained)
					} else {
						for _, evt := range drained {
							if evt != nil {
								a.sched.enqueue(newInputTask(evt))
							}
						}
						a.sched.signalWake()
					}
				}
			case ChannelSuspend:
				// 挂起：归属清空但**不交还池子**。实现上仍记为 ""，
				// 差别在于本通道上的暂存任务保留（接回时才释放），
				// 且日志明确写出"已挂起"，让运维能查到它没有在跑。
				_ = reg.Assign(ch, "", 0)
				log.Printf("[resident] %s 通道 %s 已挂起（暂存 %d 条，等接回时释放）",
					rc.id, ch, reg.StashLen(ch))
			default: // ChannelRelease
				_ = reg.Assign(ch, "", 0)
			}
		}
		// 注销"父接收该子消息"的入站 inputch（child/<id>）。
		//
		// 它由 residentInboundChannel 在 create 时登记（Owner=父），销毁时必须
		// 一并摘掉：登记表是共享的、按 name 全局唯一，残留会随 create/destroy
		// 次数单调累积脏数据。实测：destroy 后 child/<id> 仍挂在根 agent 名下，
		// 而外部没有任何工具能单独注销 inputch，只能重启 homed 清。
		// 注意用纯函数算名字，不要再走 residentInboundChannel（会重新登记）。
		reg.Unregister(inboundChannelName(rc.id))
	}
}

// StopResidents 销毁全部驻留子（父退出时必须；不留孤儿）。
func (a *Agent) StopResidents() int {
	a.residentMu.Lock()
	all := make([]*residentChild, 0, len(a.residents))
	for _, rc := range a.residents {
		all = append(all, rc)
	}
	a.residents = map[string]*residentChild{}
	a.residentMu.Unlock()
	for _, rc := range all {
		a.teardownResident(rc)
	}
	return len(all)
}

// Residents 返回登记表的快照（按 id 排序，便于断言与展示稳定）。
func (a *Agent) Residents() []ResidentInfo {
	a.residentMu.Lock()
	all := make([]*residentChild, 0, len(a.residents))
	for _, rc := range a.residents {
		all = append(all, rc)
	}
	a.residentMu.Unlock()
	out := make([]ResidentInfo, 0, len(all))
	for _, rc := range all {
		out = append(out, rc.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ResidentTable 是父**查看**子的 inputch 处理表（pull，不打断子）。
func (a *Agent) ResidentTable(id string) ([]InputchRecord, error) {
	a.residentMu.Lock()
	rc, ok := a.residents[id]
	a.residentMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("驻留子 %s 不存在", id)
	}
	return rc.agent.inputchTableSnapshot(), nil
}

// SendToResident 是父的"发送消息"：经输出通道寻址到该子的 inputch，
// 对子而言是 **L4 中断**（取消当前状态 + 插入新消息）。
func (a *Agent) SendToResident(id, text string) error {
	a.residentMu.Lock()
	rc, ok := a.residents[id]
	a.residentMu.Unlock()
	if !ok {
		return fmt.Errorf("驻留子 %s 不存在", id)
	}
	ch := "sub/" + id
	if len(rc.inputChs) > 0 {
		ch = rc.inputChs[0]
	}
	// 来源 = 父；子的 KernelSource 是父 ⇒ 在子的阶梯上这是合法的 L4。
	rc.agent.io.InjectInterruptTextOpts(string(a.id), ch, text,
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
func (a *Agent) CompressResident(id string) (int, error) {
	a.residentMu.Lock()
	rc, ok := a.residents[id]
	a.residentMu.Unlock()
	if !ok {
		return 0, fmt.Errorf("驻留子 %s 不存在", id)
	}

	ctx := rc.agent.context
	compacted := 0

	if ctx != nil {
		// ① 只读计划：此时上下文一个字都没动。
		plan := ctx.PlanCompaction(residentKeepRecent)
		if len(plan.Old) > 0 {
			// ② 用**子自己**的 provider 压（裁定 A）。
			summary, err := rc.agent.summarizeForCompaction(plan.PrefixText)
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

	rc.agent.clearInputchTable() // 处理表记的是被压掉那段窗口的逐轮处理 ⇒ 必须清
	rc.agent.resetContextFull()
	rc.mu.Lock()
	rc.state = "running"
	rc.mu.Unlock()
	return compacted, nil
}

// ReclaimResident 是父的"回收"（**取消语义**）：父读子的 temp → 选记录 → 合入 main
// → 丢弃 temp → **取消**该驻留子。
//
// keep 由父决定"哪些纳入记忆"；为 nil 时表示全部合入。
// harvestResidentMemory 把子 temp 里的活跃三元组收割进主记忆，返回合入条数。
//
// 必须在 Destroy **之前**调用：Destroy 会 Close 掉 temp。
func (a *Agent) harvestResidentMemory(rc *residentChild, keep ...func([]InputchRecord, []memory.Triple) []memory.Triple) (int, error) {
	if rc == nil || rc.light == nil || rc.light.Temp() == nil || a.mainGraph() == nil {
		return 0, nil
	}
	exported, err := rc.light.Temp().ExportTriples(0)
	if err != nil {
		return 0, fmt.Errorf("读取子 temp 失败: %w", err)
	}
	selected := exported
	if len(keep) > 0 && keep[0] != nil {
		selected = keep[0](rc.info().Table, exported)
	}
	if len(selected) == 0 {
		return 0, nil
	}
	if _, _, err := a.mainGraph().Commit(selected, "reclaim/"+rc.id, 0); err != nil {
		return 0, fmt.Errorf("合入主记忆失败: %w", err)
	}
	return len(selected), nil
}

func (a *Agent) ReclaimResident(id string, keep func([]InputchRecord, []memory.Triple) []memory.Triple) (ResidentInfo, error) {
	a.residentMu.Lock()
	rc, ok := a.residents[id]
	a.residentMu.Unlock()
	if !ok {
		return ResidentInfo{}, fmt.Errorf("驻留子 %s 不存在", id)
	}
	info := rc.info()

	// 收割：读 temp 的全部活跃三元组（比通过图记录选择更直接）。
	//
	// 逻辑抽到 harvestResidentMemory，供 FinishResident 的 onDestroy 复用 ——
	// 那里也必须**在 Destroy 之前**收割（Destroy 会关掉 temp）。
	promoted, herr := a.harvestResidentMemory(rc, keep)
	if herr != nil {
		return info, herr
	}

	// 收割完成 ⇒ 取消该驻留子（回收是取消语义，≠ 压缩）。
	if err := a.DestroyResident(id); err != nil {
		return info, err
	}
	info.State = fmt.Sprintf("reclaimed(promoted=%d)", promoted)
	return info, nil
}

// handleChildContextFull 把子的 contextfull 当**内核级事件**上报给父自己：
// 父侧 L4 中断（带子标识）—— 只推信号，细节靠"查看"拉状态面。
func (a *Agent) handleChildContextFull(rc *residentChild) {
	rc.mu.Lock()
	rc.state = "contextfull"
	rc.mu.Unlock()
	a.raiseVector(VectorChildContextFull, vectorArgs{ChildID: rc.id})
}

// checkContextFull 判断本 agent 的**积累上下文是否已经装不下窗口**，是则触发一次 contextfull。
//
// ❗判据为什么不能写成"估算拼好的 f.Msgs"：`buildMessages` 拿到的 `budget.ContextTokens`
// 本身就是按 `targetUsage = 0.8 × 窗口` 算出来的，时间线**在拼进消息之前就被预算裁过**了。
// 于是 `f.Msgs` 的规模结构上封顶在 ~80% 窗口 —— 对 90% 阈值而言是**永远不成立**的判据
// （写测试时我用一个比系统提示词还小的窗口才勉强越过线，那等于什么都没测）。
//
// 正确的事是"**要被裁了**"：拿**未裁剪**的积累上下文（`a.context` 的全部事件）估算，
// 它超过窗口阈值就说明下一轮必须丢事件 ⇒ 这就是 contextfull。
//
// 只对**驻留子**生效（只有它们设了 onContextFull）。
func (a *Agent) checkContextFull() {
	if a.onContextFull == nil {
		return
	}
	// 读标记要加锁：它可能被**父的 goroutine**（CompressResident）并发清零。
	a.ctxFullMu.Lock()
	signaled := a.ctxFullSignaled
	a.ctxFullMu.Unlock()
	if signaled {
		return
	}
	if a.context == nil {
		return
	}
	// ★ 判据统一走 overflowRatioNow（与容量保护同一个量）：
	//
	// 原实现自己手写了一遍 `Input+Response` 的累加，与 overflow.accumulatedTokens
	// 的差别不只是"重复"——后者用 eventTokens 计入 **ToolResults**，而手写版
	// 漏了它。工具回灌正是 v4/T10 实测的 prompt 主力（17~40 万 token 几乎全部
	// 来自工具输出），漏算等于把"已经装不下"误判成"还宽松"，
	// 驻留子因此永远报不到 contextfull、父也就永远不知道该回收它。
	if a.overflowRatioNow() < contextFullRatio {
		return
	}
	a.ctxFullMu.Lock()
	a.ctxFullSignaled = true
	a.ctxFullMu.Unlock()
	a.onContextFull()
}

// resetContextFull 清零上报标记（压缩后允许再次上报）。
//
// 加锁理由见 Agent.ctxFullMu：本函数**通常由父的 goroutine 调用**
// （CompressResident），而子的调度器 goroutine 正在读同一个字段。
func (a *Agent) resetContextFull() {
	a.ctxFullMu.Lock()
	a.ctxFullSignaled = false
	a.ctxFullMu.Unlock()
}

// ---- 子侧：inputch 处理表（子持有，父 pull） ----

// recordInputchNote 是子**主动写入**本轮 inputch 的处理信息（工具 inputch_note）。
func (a *Agent) recordInputchNote(text string) {
	a.tableMu.Lock()
	defer a.tableMu.Unlock()
	a.inputchPending = &InputchRecord{
		InputCh: a.currentInputch, At: time.Now(), Proactive: true, Text: text,
	}
}

// autoRecordInputch 是**系统自动写**兜底：本轮未主动写时，把该轮 inputch 的处理信息写入。
// 保证每一轮必有记录，父不会看到空洞。
func (a *Agent) autoRecordInputch(f *TaskFrame) {
	if f == nil {
		return
	}
	a.tableMu.Lock()
	defer a.tableMu.Unlock()
	if a.inputchPending != nil {
		rec := *a.inputchPending
		a.inputchPending = nil
		rec.InputCh = firstNonEmpty(rec.InputCh, a.currentInputch)
		a.inputchTable = append(a.inputchTable, rec)
		return
	}
	text := fmt.Sprintf("轮次完成：输入=%s", truncateStr(f.Input, 80))
	if f.Response != "" {
		text += "；产出=" + truncateStr(f.Response, 120)
	}
	a.inputchTable = append(a.inputchTable, InputchRecord{
		InputCh: a.currentInputch, At: time.Now(), Proactive: false, Text: text,
	})
}

func (a *Agent) inputchTableSnapshot() []InputchRecord {
	a.tableMu.Lock()
	defer a.tableMu.Unlock()
	return append([]InputchRecord(nil), a.inputchTable...)
}

func (a *Agent) clearInputchTable() {
	a.tableMu.Lock()
	defer a.tableMu.Unlock()
	a.inputchTable = nil
	a.inputchPending = nil
}

// ---- 子侧：主动向父发消息（L3） ----

func (a *Agent) notifyParentFrom(text string) string {
	if a.notifyParent == nil {
		return "本 agent 没有上级，无法发送消息"
	}
	if strings.TrimSpace(text) == "" {
		return "消息内容不能为空"
	}
	a.notifyParent(text)
	return "已发送给主 agent"
}

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
	table := rc.agent.inputchTableSnapshot()
	// 子自己的调度器积压（per-agent 负载展示用）。schedulerStatus 只读快照，
	// 每次状态轮询为每个子算一次，成本可忽略（驻留子数量是个位数）。
	sc := rc.agent.schedulerStatus()
	info := ResidentInfo{
		ID: rc.id, State: state, InputChs: append([]string(nil), rc.inputChs...),
		AllowedOutputs: append([]string(nil), rc.allowed...),
		ContextFull:    full, CreatedAt: rc.createdAt, TableSize: len(table),
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
		Rounds: rc.agent.roundsExecuted(),
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
func (a *Agent) summarizeForCompaction(text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("没有可压缩的内容")
	}
	if a == nil || a.provider == nil {
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
	resp, err := a.provider.Chat(a.ctx, req)
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(resp.Content)
	if out == "" {
		return "", fmt.Errorf("摘要器返回空内容")
	}
	return out, nil
}
