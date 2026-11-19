package agenttypes

// childvalues.go —— 子 agent 的**值类型**（基座与类型包共用）。
//
// ★ 为什么住在叶子层：基座（`internal/agent/agent`）与类型包（resident /
//   rtchild）都要用它们。若定义在类型包里，基座就得 import 类型包 ——
//   反向依赖，成环。值类型不带任何行为，放这里零风险。

import "time"

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

// ★ 2026-11-17 收敛：原先有 ChannelRelease / ChannelSuspend / ChannelTakeover 三个值，
// 而**前两个在机制上完全等价**（都是 `reg.Assign(ch, "", 0)`，唯一差别是一行日志）。
//
// 用户指出：「释放和挂起语义重复了」。核实属实，而且注释里声称的差别也不成立：
// 「不参与自动分配」没有对应字段，「输入被暂存」只在残余任务处置时发生、
// 不是投递路径。根因是登记表**没有"悬空"这个真实状态**（现已补上 Orphaned）。
//
// 现在只有两种真正的行为：
const (
	// ChannelOrphan 是**默认**：通道悬空 —— Owner 清空 + 打 Orphaned 标记，
	// 期间到达的输入写进 stash，等父显式接回或划给别的子时一并交出。
	//
	// 作为**零值**保持向后兼容（已有调用方不传就等于它）。
	//
	// ⚠️ 与"未分配"（Owner=="" 且未 Orphan）不同：未分配的通道输入会由根 agent
	// 兜底处理；悬空的输入**不归任何人**，既不丢也不让别人偷偷处理。
	ChannelOrphan ChannelDisposition = ""
	// ChannelHandover 显式交接：通道归属改为**目标 agent**（父或另一个子），
	// 并交出该通道上的暂存任务（立即转进目标 agent 的队列）。
	//
	// 目标由 ResidentOptions/控制面给出；缺省目标 = 父（原 ChannelTakeover 的语义）。
	ChannelHandover ChannelDisposition = "handover"
)
