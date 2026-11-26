package resident

// resident_type.go —— **驻留子类型**的注册项（Descriptor）。
//
// ## ★ 归属说明（不要被文件位置误导）
//
// 这个文件是**驻留子类型模块的一部分**，只是暂时还在基座包里 ——
// 因为 `residentChild` / `SpawnResident` 等实现也还在基座包（拆包未完成）。
// 它们会**一起**搬到 `internal/agent/resident`，本文件一行都不必改语义。
//
// 所以这里出现"驻留子怎么创建、能做哪些动作"是**对的**：
// 那是类型知识，不是基座知识。基座只通过 registry 查表调用它。
//
// ## 注册的时机
//
// `init()` —— 与"类型先注册、只能经 Instantiate 产生实例"同一纪律
// （见 types.go）。内置插件经 EventBus 注册的路径走 registry.Register，
// 与这里**同一张表**。

import (
	"os"

	"fmt"
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"path/filepath"
	"strings"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

func init() {
	// 注册失败 = 计划错误（两个包声称同一类型）。装配期 panic 是刻意的：
	// 静默跳过会让"该类型不可用"变成运行期才发现的谜。
	if err := registry.Register(residentDescriptor()); err != nil {
		panic("agent/resident: " + err.Error())
	}
}

// residentDescriptor 返回驻留子的注册项。
func residentDescriptor() registry.Descriptor {
	return registry.Descriptor{
		Meta: registry.Meta{
			Type:         string(kindResident),
			Description:  "长期派驻的子 agent：有自己的输入队列与记忆，可承接持续任务；上下文整窗不裁，撞窗口时向父上报",
			NeedsInputCh: true,
			AgentCap:     agentIO.CapText,
		},
		Actions: []registry.Action{
			{
				Name:        "compress",
				Description: "压缩它的上下文（保留语义：压成摘要 + 清处理表），子继续存在",
				Args: map[string]interface{}{
					"id": map[string]interface{}{"type": "string", "description": "驻留子 id"},
				},
				Required: []string{"id"},
			},
			{
				Name:        "reclaim",
				Description: "回收：把它的记忆纳入你的主记忆，然后取消该子（与 destroy 的区别是它会先收割记忆）",
				Args: map[string]interface{}{
					"id": map[string]interface{}{"type": "string", "description": "驻留子 id"},
					"residual": map[string]interface{}{
						"type": "string", "enum": []string{"keep", "takeover", "stash", "drop"},
						"description": "残余任务处置（默认 keep=转回你的队列）",
					},
				},
				Required: []string{"id"},
			},
			{
				Name:        "table",
				Description: "查看它的 inputch 处理表（它处理到哪了、有没有卡住），不打断它",
				Args: map[string]interface{}{
					"id": map[string]interface{}{"type": "string", "description": "驻留子 id"},
				},
				Required: []string{"id"},
			},
		},
		// Initialize 由**类型自己**装配运行时所需的服务（用户裁定：基座不替
		// 任何类型决定它需要什么）。
		//
		// ★ 驻留子有队列 ⇒ 必须有调度器。原先只注册了 Start/Stop，
		//   于是子 Start 之后 `schedulerLoop` 撞到 nil 调度器（实测 panic：
		//   `(*scheduler).hasRoom(0x0?)`，被 recover 吞掉后子永远不处理输入）。
		Initialize: func(instance any) error {
			a, _ := instance.(*agent.Agent)
			if a == nil {
				return fmt.Errorf("resident: 实例不是 *agent.Agent（装配错误）")
			}
			installResidentHooks(a)
			// ★ 上下文实现在这里装配（**与主 agent 同一路口**）。
			//
			// 此前它只在 `RestoreContext` 里有条件地建：`context.json` **已存在**
			// 才 `OpenContext()`，否则不建。于是**新建**的驻留子 `m.ctx` 恒为 nil，
			// 第一轮处理输入就撞 `prepareInputTask` 的空指针（实测：创建成功、
			// state=running、rounds 永远是 0、panic 被 recover 吞掉）。
			//
			// 上下文是"跑起来就需要的东西"，不是"恢复时才需要的东西"：
			// 归属 Initialize（装配期），恢复只负责往里灌历史。
			Wrap(a).OpenContext()
			return nil
		},
		Construct: func(host registry.Host, arg map[string]interface{}) (string, error) {
			m := Of(host)
			if m == nil {
				return "", fmt.Errorf("resident: 宿主不是实例管理器")
			}
			return m.spawnResidentForAdmin(arg)
		},
		Admin: func(host registry.Host, action string, arg map[string]interface{}) (string, error) {
			m := Of(host)
			if m == nil {
				return "", fmt.Errorf("resident: 宿主不是实例管理器")
			}
			return m.residentAdmin(action, arg)
		},
		Help: func(host registry.Host) string {
			return residentHelpText()
		},
	}
}

// residentHelpText 是 `admin_child_help(type="resident")` 的正文。
//
// ★ 技术细节收敛在这里（与 outputch 的 `_help` 同一纪律）：主工具描述里
// 只留一句话，参数语义与注意事项全部放这里 + skill。
func residentHelpText() string {
	return strings.TrimSpace(`
【create 参数】
  id              必填。子 agent 的 id（同时是登记表键与跨 agent 寻址依据）
  task_prompt     在固定提示词之上注入的任务提示词（它"是谁、要做什么"）
  input_chs       划入它的 inputch（逗号分隔）。不划通道它就收不到任何输入
  allowed_outputs 授权给它的输出通道（逗号分隔；留空 = 完整授权，只能收窄）
  capacity        划入 inputch 的队列容量（0 = 内核默认）
  temp_path       temp 图记忆路径（留空 = <data>/residents/<id>/graph.db）
  preferred_provider 它优先使用的模型源（留空 = 继承你的）
  allowed_tools   **你为它声明**的工具白名单（支持 cmd_* 通配）。留空=不额外限制。只能收窄
  deny_tools      **你为它声明**的追加禁止（deny 优先于 allow）

【注意事项】
  · 创建即开工：task_prompt 会作为第一条排队输入投给它，不必再 send 一次
  · 它有自己的输入队列与调度器，跑自己的 loop —— 与你的执行是并行的
  · 它的上下文**整窗不裁**：撞窗口时会 CallParent 上报 contextfull 给你，
    由你决定 compress（保语义）还是 reclaim/destroy
  · 销毁时它手头未处理的消息（残余任务）由 residual 决定：keep 转回你的队列 /
    drop 明确丢弃。两者都会逐条记日志；异步通道（如 qq）被 drop 时用户收不到
    任何回复，请慎重
  · 通道处置 channels：orphan（默认，悬空、输入暂存等接回）/ handover（显式交接
    给 channel_target，缺省你自己）
`)
}

// kindResident 是驻留子的类型名（与 child.KindResident 同源）。
const kindResident = "resident"

// ── 类型实现：create / 独有动作 ──
//
// ★ 这两个函数是**驻留子类型**的实现（不是基座知识）。它们用 arg map 而不是
//   ToolCall，因为注册面的入参已经是规范化的 map（kernel 已拆好参数）。

// spawnResidentForAdmin 处理 admin_child(type=resident, action=create)。
func (m *M) spawnResidentForAdmin(arg map[string]interface{}) (string, error) {
	id := strings.TrimSpace(strOf(arg, "id"))
	if id == "" {
		return "", fmt.Errorf("resident: create 需要 id")
	}
	inputChs := splitComma(strOf(arg, "input_chs"))
	if len(inputChs) == 0 {
		return "", fmt.Errorf("resident: create 需要至少一个 input_chs（不划通道它收不到任何输入）")
	}
	// ★ 工作目录由**内核**分配并传入（用户裁定 2026-11-19）。
	//   类型只决定"我的图库文件在目录里叫什么"，不决定目录在哪。
	//   调用方给的 temp_path 只作兼容参数：它若与内核目录不一致，以内核为准。
	workDir := strings.TrimSpace(strOf(arg, registry.ArgWorkDir))
	if workDir == "" {
		return "", fmt.Errorf("resident: 内核未分配工作目录（装配错误）")
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return "", fmt.Errorf("resident: 创建工作目录 %s 失败: %w", workDir, err)
	}
	tempPath := filepath.Join(workDir, "graph.db")
	info, err := m.SpawnResident(ResidentOptions{
		ID:                id,
		WorkDir:           workDir,
		TaskPrompt:        strOf(arg, "task_prompt"),
		InputChs:          inputChs,
		AllowedOutputs:    splitComma(strOf(arg, "allowed_outputs")),
		Capacity:          intOf(arg, "capacity"),
		TempPath:          tempPath,
		PreferredProvider: strOf(arg, "preferred_provider"),
		AllowedTools:      splitComma(strOf(arg, "allowed_tools")),
		DenyTools:         splitComma(strOf(arg, "deny_tools")),
	})
	if err != nil {
		return "", err
	}
	// 记录创建参数（软重启与持久化共用）。
	//
	// ★ 用 EnsureInstanceManager（懒初始化）而不是 Instances（只读裸取）。
	//
	// 两者差一个 nil：`Instances()` 只返回"已装配的"那份。裸取 + nil 判断
	// 让"没记住"与"不需要记"长得一样 —— 实测症状是
	// `admin_child(type=resident, action=create)` 成功、子也能干活，
	// 但随后 `action=restart` 报「没有创建参数快照（无法重建）」。
	//
	// 记录创建参数是**创建动作的一部分**（软重启与持久化都靠它），
	// 不能因为管理器此刻还没被挂上就静默丢掉。
	if im := m.A.EnsureInstanceManager(); im != nil {
		im.Remember(string(kindResident), id, arg)
	}
	return "已创建驻留子: " + MarshalResidentInfo(info), nil
}

// residentAdmin 处理驻留子的动作（通用 destroy + 类型独有 compress/reclaim/table）。
//
// ★ `destroy` 落在这里而不是内核：残余怎么处置（keep/stash/drop）、
//
//	通道怎么交代 —— 全是**驻留子的语义**。内核只按 type 查表调用。
func (m *M) residentAdmin(action string, arg map[string]interface{}) (string, error) {
	id := strings.TrimSpace(strOf(arg, "id"))
	if id == "" {
		return "", fmt.Errorf("resident: 动作 %q 需要 id", action)
	}
	switch action {
	case "destroy":
		policy := ResidualKeep
		switch strings.ToLower(strOf(arg, "residual")) {
		case "drop":
			policy = ResidualDrop
		case "stash":
			policy = ResidualStash
		}
		n, msg, err := m.FinishResidentWithTarget(id, policy, ChannelOrphan, "", nil)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已销毁 %s（残余 %d 条：%s）", id, n, msg), nil
	case "compress":
		n, err := m.CompressResident(id)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已压缩 %s 的上下文（丢弃 %d 条旧事件，并清理其 inputch 处理表）；子继续存在", id, n), nil
	case "reclaim":
		policy := ResidualKeep
		switch strings.ToLower(strOf(arg, "residual")) {
		case "drop":
			policy = ResidualDrop
		case "stash":
			policy = ResidualStash
		}
		n, msg, err := m.FinishResidentWithTarget(id, policy, ChannelOrphan, "", nil)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已回收 %s（残余 %d 条：%s）", id, n, msg), nil
	case "table":
		table, err := m.ResidentTable(id)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "驻留子 %s 的 inputch 处理表（%d 条）:", id, len(table))
		for _, r := range table {
			kind := "系统写"
			if r.Proactive {
				kind = "主动写"
			}
			fmt.Fprintf(&b, "\n  - [%s][%s] %s", r.InputCh, kind, r.Text)
		}
		if len(table) == 0 {
			b.WriteString("\n  （尚无记录）")
		}
		return b.String(), nil
	}
	return "", fmt.Errorf("resident: 未知动作 %q", action)
}

// ── 小工具（arg map 版本）──

func strOf(m map[string]interface{}, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return strings.TrimSpace(s)
}

func intOf(m map[string]interface{}, k string) int {
	if m == nil {
		return 0
	}
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func splitComma(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// installResidentHooks 注册**驻留子自己**的七公共面槽位。
//
// ## 为什么这必须是驻留子的职责（用户 2026-11-18）
//
// 基座不假设任何 loop 样式、不假设任何裁剪策略。驻留子的语义是
//
//	整窗上下文   —— 传统上下文只受"模型能收多少"这个**硬上限**约束；
//	                不按相关度挑、不向 doc 记忆归档
//	contextfull  —— 撞窗口时**上报父**（CallParent/L4），由父决定压缩/回收/销毁；
//	                所以它注册 nil 的 Prune / EnforceOverflow —— 那是**明确的策略**
//	同步队列     —— 与主 agent 同一套标准 loop（标准状态机）
//
// ★ 阶段（stage）**不是槽位**了（I 批）：实现只有一份、调用点全在同包，
//
//	所以这里不再注册它。
func installResidentHooks(a *agent.Agent) {
	h := a.Hooks()
	mm := Wrap(a)

	// 槽位 5：上下文管理 —— 整窗型。
	if h.ContextBudget == nil {
		h.ContextBudget = func(_ *agent.Agent, b kit.TokenBudget) int { return b.MaxContext }
	}
	// ★ 驻留子的 OverflowHandler = **自己算水位 → 上报父**（不急裁）。
	//
	// 整窗型不做相关性裁剪，撞窗口时交给父决定（压缩 / 回收 / 销毁）——
	// 丢事件的决定权在父（用户裁定）。
	//
	// ★ 水位怎么算、超了报给谁、只报一次的去重标记放哪 —— **全在这里**。
	//   改造前这些在基座的 `CheckContextFull` 里（`m.st.onContextFull`
	//   闭包 + `ctxFullSignaled` 标记），那是基座替类型做策略。
	if h.OverflowHandler == nil {
		h.OverflowHandler = func(x *agent.Agent, _ any, req agent.OverflowRequest) (bool, int) {
			// 内核上报的 contextfull（Force）与本地水位两种来源都收。
			if req.Kind != agent.OverflowLocal {
				return false, 0
			}
			if x.ContextLen() == 0 {
				return false, 0
			}
			res := mm.st
			if res == nil {
				return false, 0
			}
			// 只报一次：滑无可滑是**持续状态**，每轮都报会把父的上下文也顶满。
			res.ctxFullMu.Lock()
			already := res.ctxFullSignaled
			res.ctxFullMu.Unlock()
			if already {
				return false, 0
			}
			// 本地水位：拿**未裁剪**的积累上下文估算（拼好的 f.Msgs 已被
			// 预算裁过，对 90% 阈值永远不成立 —— 那是「要被裁了」而不是
			// 「已经裁过」的判据）。
			if !req.Force && ratioOf(x) < contextFullRatio {
				return false, 0
			}
			res.ctxFullMu.Lock()
			res.ctxFullSignaled = true
			res.ctxFullMu.Unlock()
			if res.onContextFull != nil {
				res.onContextFull()
			}
			return false, 0
		}
	}

	// ★ 没有 Loop 槽位（docs §0.3）：帧推进是**本类型自己的实现**，
	//   由本包的循环（`driver.go` 的 SchedulerLoop → runInputTask）驱动。
	//   基座不认识帧，也就不可能有"驱动一帧"的槽。

	a.SetHooks(h)
}

// residentContextFullText 是"子上下文已满"这条内核级中断的正文。
//
// ★ 正文由**类型自己**决定（基座不认识"驻留子的 contextfull"是什么）：
//
//	父看到的是"该决定压缩 / 回收 / 销毁"，而不是一段实现细节。
func residentContextFullText(id string) string {
	return "驻留子 " + id + " 的上下文已满。请决定：压缩它的上下文" +
		"（admin_child action=compress）、回收它（action=reclaim），或销毁（action=destroy）。"
}

// ★ `residentLoop` 已删除（2026-11-26）。
//
// 它曾是"帧推进 + inputch 记账"的统一入口，但**没有任何调用方**——
// 驱动是 `SchedulerLoop` → `executeNewTask` → `RunInputTask`。
// 留着它的后果是记账静默消失：处理表恒 0 条，父永远读到「（尚无记录）」，
// 而子其实处理完了（context.json 的 resp 有值）。
//
// 现在记账落在**真正被调用的那条路径**上（`driver.go` 的 `executeNewTask`）。
// 判据因此不需要：没有第二个入口可以漏。
