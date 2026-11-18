package agent

// rt_type.go —— **实时通话子 agent（rtchild）类型**的注册项。
//
// ## ★ 归属说明
//
// 与 resident_type.go 同一处境：实现（rtChild / SpawnRTChild）还在基座包，
// 所以注册项也暂住这里；拆包时**一起**搬进 internal/agent/rtchild/，
// 本文件语义不变。
//
// ## 这个文件回答用户 2026-11-18 的质问
//
// > 「为什么还是有硬编码的 rt，现有的回调注册式接口还不够用吗？」
//
// 这里就是回答：rt 的类型元数据、它自己的三个管理动作（create/send/stop/
// transcript/children）全部**由 rt 自己声明**。基座只做查表调用，
// 不再有 `case tc.Name == "rt_children"` 之类的字面量。
//
// ## 与驻留子的差异（由 Meta 声明，不是基座 if）
//
//	驻留子  必须 inputch（可以多条）；上下文整窗；有任务队列与残余回灌
//	rt      必须 inputch + outputch **成对**；最新者胜单槽；无队列、无残余

import (
	"encoding/json"
	"fmt"
	"strings"

	childpkg "github.com/JianFeeeee/HologramAgent/internal/agent/agent/child"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

// kindRT 是 rtchild 的类型名（与 child.KindRT 同源）。
const kindRT = "rt"

func init() {
	if err := registry.Register(rtDescriptor()); err != nil {
		panic("agent/rt: " + err.Error())
	}
}

func rtDescriptor() registry.Descriptor {
	return registry.Descriptor{
		Meta: registry.Meta{
			Type: kindRT,
			Description: "实时通话子 agent：单槽覆盖式输入（最新者胜、永不排队），" +
				"必须成对给出 inputch + outputch；上下文按窗口裁，回收时交接原文给你按需拉取",
			NeedsInputCh:  true,
			NeedsOutputCh: true,
		},
		Actions: []registry.Action{
			{
				Name: "transcript",
				Description: "取回 rtchild 的通话上下文原文（回收后仍可取回留档）。" +
					"分段读取：一条通话可能几百条消息，不要整段塞进自己的上下文",
				Args: map[string]interface{}{
					"id":     map[string]interface{}{"type": "string", "description": "rtchild 的 id"},
					"offset": map[string]interface{}{"type": "number", "description": "从第几条开始（0 = 从头）"},
					"limit":  map[string]interface{}{"type": "number", "description": "最多返回多少条（默认 40）"},
					"format": map[string]interface{}{"type": "string", "enum": []string{"text", "raw"}, "description": "text=可读渲染（默认）/ raw=原始消息"},
				},
				Required: []string{"id"},
			},
			{
				Name:        "stop",
				Description: "回收 rtchild：先把全部上下文交给你（并推一条中断告知条数），再停掉它。原文用 transcript 动作按需拉取",
				Args: map[string]interface{}{
					"id": map[string]interface{}{"type": "string", "description": "rtchild 的 id"},
				},
				Required: []string{"id"},
			},
			{
				Name:        "stats",
				Description: "查看单个 rtchild 的规模（轮次/最新输入 seq/覆盖丢弃数/是否待处理）",
				Args: map[string]interface{}{
					"id": map[string]interface{}{"type": "string", "description": "rtchild 的 id"},
				},
				Required: []string{"id"},
			},
			{
				Name:        "rt_channels",
				Description: "列出当前可用的 rt 通道（哪些 inputch/outputch 带实时能力），创建前的必查",
			},
		},
		// rt 的运行时装配（用户裁定：类型自己决定需要什么）。
		//
		// ★ rt **不装调度器**：它不吃队列（单槽覆盖式输入，最新者胜）。
		//   它也不注册 Loop/RunStage —— rtLoop 是 rt 自己的驱动，
		//   不经基座的 RunTaskSteps（那个是同步状态机，对 rt 是反向的）。
		//
		// 但**上下文预算必须注册**：整窗型（只受硬上限约束），
		// 否则 ContextTokenBudget 会退回策略裁剪型的预算值，
		// 于是 rt 的通话历史会被"按相关度裁"——那是 rt 明确不要的。
		Initialize: func(instance any) error {
			a, _ := instance.(*Agent)
			if a == nil {
				return fmt.Errorf("rt: 实例不是 *Agent（装配错误）")
			}
			installRTHooks(a)
			return nil
		},
		Construct: func(host registry.Host, arg map[string]interface{}) (string, error) {
			a, ok := host.(*InstanceManager)
			if !ok || a == nil || a.root == nil {
				return "", fmt.Errorf("rt: 宿主不是实例管理器")
			}
			return a.root.spawnRTChildForAdmin(arg)
		},
		Admin: func(host registry.Host, action string, arg map[string]interface{}) (string, error) {
			a, ok := host.(*InstanceManager)
			if !ok || a == nil || a.root == nil {
				return "", fmt.Errorf("rt: 宿主不是实例管理器")
			}
			return a.root.rtAdmin(action, arg)
		},
		Help: func(host registry.Host) string { return rtHelpText() },
	}
}

func rtHelpText() string {
	return strings.TrimSpace(`
【create 参数】
  id              必填。rtchild 的 id
  inputch         **必填**。inputch(rt) 的名字 —— 采集侧往里灌实时事件
  outputch        **必填**。outputch(rt) 的设备名 —— 内核把 rt 文本写进它
  system_prompt   它的系统提示词（留空用内核的实时通话默认提示词）
  preferred_provider 它优先使用的模型源（通话场景应选首字延迟最低的）
  temp_path       temp 图记忆路径（留空 = 不建 temp，只读你的主库）
  allow_graph_write 是否允许它写图记忆（默认 false = 只读，避免通话噪声灌满图谱）
  max_tools       单轮最大工具批数（0 = 不限；rt 是常驻对话，跑满就结束是错的）
  allowed_tools   **你为它声明**的工具白名单（只能收窄）
  deny_tools      **你为它声明**的追加禁止（deny 优先于 allow）

【注意事项】
  · **必须成对**给 inputch + outputch，且两者都必须是 CapRT 通道：只给一半
    不是"简化"而是坏掉一半 —— 只给 output 它永远听不到用户（会自言自语后静默），
    只给 input 它的话无处可去（用户听到长时间静默）
  · 新版输入**覆盖**旧版（最新者胜、永不排队），与驻留子的队列语义不同
  · 它自己的上下文按窗口裁；回收时只推一条中断告知条数，正文用 transcript
    动作按需拉取 —— 是否保留、保留哪些由你决定
  · 它的 history 只在这一通电话内有效，通话结束就只剩留档；要长期记住的事
    应开 allow_graph_write 或由你在回收时自己保留
`)
}

// ── 类型实现（rt 侧）──

// spawnRTChildForAdmin 处理 admin_child(type=rt, action=create)。
func (a *Agent) spawnRTChildForAdmin(arg map[string]interface{}) (string, error) {
	id := strings.TrimSpace(strOf(arg, "id"))
	if id == "" {
		return "", fmt.Errorf("rt: create 需要 id")
	}
	input := strOf(arg, "inputch")
	output := strOf(arg, "outputch")
	if input == "" || output == "" {
		// 提示要**可操作**：直接列出当前登记表里哪些通道带 rt 能力。
		return "", fmt.Errorf("rt: create 必须成对给出 inputch 与 outputch（当前 inputch=%q outputch=%q）。\n%s",
			input, output, a.listRTChannels())
	}
	got, err := a.SpawnRTChild(RTChildOptions{
		ID:                id,
		Input:             input,
		Output:            output,
		SystemPrompt:      strOf(arg, "system_prompt"),
		PreferredProvider: strOf(arg, "preferred_provider"),
		MaxTools:          intOf(arg, "max_tools"),
		TempPath:          strOf(arg, "temp_path"),
		AllowGraphWrite:   boolOf(arg, "allow_graph_write"),
		AllowedTools:      splitComma(strOf(arg, "allowed_tools")),
		DenyTools:         splitComma(strOf(arg, "deny_tools")),
	})
	if err != nil {
		return "", err
	}
	if m := a.Instances(); m != nil {
		m.remember(kindRT, got, arg)
	}
	return "已创建实时子 agent（rtchild）: " + got, nil
}

// rtAdmin 处理 rt 的**类型独有**动作。
func (a *Agent) rtAdmin(action string, arg map[string]interface{}) (string, error) {
	switch action {
	case "rt_channels":
		return a.listRTChannels(), nil
	}
	id := strings.TrimSpace(strOf(arg, "id"))
	if id == "" {
		return "", fmt.Errorf("rt: 动作 %q 需要 id", action)
	}
	switch action {
	case "transcript":
		return a.rtTranscriptText(id, arg)
	case "stop":
		n, err := a.CloseRTChild(id)
		if err != nil {
			return "", err
		}
		if n == 0 {
			return fmt.Sprintf("已回收 rtchild %s（它没有历史，未推交接信号）", id), nil
		}
		return fmt.Sprintf("已回收 rtchild %s（全文 %d 条已存档，用 transcript 动作按需取回；是否保留由你决定）", id, n), nil
	case "stats":
		st, ok := a.RTChildStats(id)
		if !ok {
			return "", fmt.Errorf("rt: rtchild %q 不存在", id)
		}
		msg := fmt.Sprintf("%s 轮次=%d 最新输入seq=%d 覆盖丢弃=%d", st.ID, st.Rounds, st.LastSeq, st.Dropped)
		if st.Pending {
			msg += "  ⏳ 有待处理输入"
		}
		return msg, nil
	}
	return "", fmt.Errorf("rt: 未知动作 %q", action)
}

// rtTranscriptText 渲染 transcript 动作的输出（分段 + text/raw 两种格式）。
func (a *Agent) rtTranscriptText(id string, arg map[string]interface{}) (string, error) {
	msgs, ok := a.RTTranscript(id)
	if !ok {
		return "", fmt.Errorf("rt: rtchild %s 不存在，且没有已回收的留档", id)
	}
	offset := intOf(arg, "offset")
	limit := intOf(arg, "limit")
	if limit <= 0 {
		limit = rtTranscriptDefaultLimit
	}
	if offset >= len(msgs) {
		return "", fmt.Errorf("rt: rtchild %s 共 %d 条消息，offset=%d 越界", id, len(msgs), offset)
	}
	end := offset + limit
	if end > len(msgs) {
		end = len(msgs)
	}
	page := msgs[offset:end]
	if strings.EqualFold(strOf(arg, "format"), "raw") {
		b, err := json.MarshalIndent(page, "", "  ")
		if err != nil {
			return "", fmt.Errorf("rt: 序列化失败: %w", err)
		}
		return fmt.Sprintf("【rtchild %s 原文 %d-%d / 共 %d 条】\n%s", id, offset, end, len(msgs), string(b)), nil
	}
	return fmt.Sprintf("【rtchild %s 上下文 %d-%d / 共 %d 条】\n%s",
		id, offset, end, len(msgs), FormatRTTranscript(page)), nil
}

// boolOf 取布尔参数（arg map 版本）。
func boolOf(m map[string]interface{}, k string) bool {
	if m == nil {
		return false
	}
	b, _ := m[k].(bool)
	return b
}

// toAdminArg 把 rtchild 的**创建参数**转成 admin_child 的 arg 形态。
//
// ★ 与 residentChild.toAdminArg 同一纪律：软重启、进程重启恢复、持久化
//
//	落盘三条路都走同一个转换点，"三处参数不一致"这种漂移不可能发生。
func (rc *rtChild) toAdminArg() map[string]interface{} {
	if rc == nil {
		return nil
	}
	arg := map[string]interface{}{
		"id":       rc.ID,
		"inputch":  rc.input,
		"outputch": rc.output,
	}
	if rc.agent != nil {
		arg["system_prompt"] = rc.agent.systemPrompt
		arg["allow_graph_write"] = rc.light != nil
	}
	// 工具策略等其余创建参数用**创建时保存的原始 arg**：
	// rt 把 AllowedTools/DenyTools 直接交给了安全模块（DeclareChildPolicy），
	// 自己不再存副本；而"父的决定"必须可重建，所以原样留一份。
	for k, v := range rc.createArg {
		if k == "id" || k == "inputch" || k == "outputch" {
			continue
		}
		arg[k] = v
	}
	return arg
}

// PersistRTChildren 把当前 rtchild 的创建参数写进实例表。
//
// ★ 为什么 rt 也要持久化：右键一个"通话中"的 rt 以重启内核为代价丢掉
//
//	全部现场是错的；至少要让它在启动后**重新存在**（可被 transcript 复盘）。
//	但通话本身是瞬时的 —— 恢复出来的 rt 不会继续那通电话，这是刻意的：
//	恢复的是"它是什么"，不是"它当时在说什么"。
func (a *Agent) PersistRTChildren() {
	if a == nil || a.dataDir == "" {
		return
	}
	m := a.EnsureInstanceManager()
	if m == nil {
		return
	}
	seen := map[string]bool{}
	for _, e := range a.children.ByKind(childKindRT()) {
		rc, ok := e.(*rtChild)
		if !ok {
			continue
		}
		seen[rc.ID] = true
		m.remember(kindRT, rc.ID, rc.toAdminArg())
	}
	for _, r := range m.Records() {
		if r.Type != kindRT {
			continue
		}
		if !seen[r.ID] {
			m.forget(r.ID)
		}
	}
}

// childKindRT 返回 rt 的登记表类型标签（避免本文件直接 import child 包两次）。
func childKindRT() childpkg.Kind { return childpkg.KindRT }

// installRTHooks 注册**rt 自己**的上下文槽位。
//
// ## rt 与主/驻留的差别（这些都是 rt 的类型语义）
//
//	整窗上下文    通话历史不该被"按相关度裁"—— 通话是连贯的，
//	              裁掉中间那段会让模型失去上下文；只受模型窗口硬上限约束。
//	无本地超页处置  撞窗口时**上报父**（CallParent，contextfull 式交接），
//	              由父决定保留哪些（父用 transcript 动作按需取回）。
//	异步 loop     rtLoop 是 rt 自己的驱动（不经 RunTaskSteps）。
//
// 所以这里注册 ContextBudget（整窗）与 OverBudget（恒 false），
// Prune / EnforceOverflow 保持 nil（明确的策略：不做）。
func installRTHooks(a *Agent) {
	h := a.HooksValue()
	if h.ContextBudget == nil {
		h.ContextBudget = func(_ *Agent, b kit.TokenBudget) int { return b.MaxContext }
	}
	if h.OverBudget == nil {
		h.OverBudget = func(*Agent) bool { return false }
	}
	a.SetHooks(h)
}
