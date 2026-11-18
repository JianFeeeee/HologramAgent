package agent

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
	"fmt"
	"path/filepath"
	"strings"

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
		Construct: func(host registry.Host, arg map[string]interface{}) (string, error) {
			a, ok := host.(*InstanceManager)
			if !ok || a == nil || a.root == nil {
				return "", fmt.Errorf("resident: 宿主不是实例管理器")
			}
			return a.root.spawnResidentForAdmin(arg)
		},
		Admin: func(host registry.Host, action string, arg map[string]interface{}) (string, error) {
			a, ok := host.(*InstanceManager)
			if !ok || a == nil || a.root == nil {
				return "", fmt.Errorf("resident: 宿主不是实例管理器")
			}
			return a.root.residentAdmin(action, arg)
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
func (a *Agent) spawnResidentForAdmin(arg map[string]interface{}) (string, error) {
	id := strings.TrimSpace(strOf(arg, "id"))
	if id == "" {
		return "", fmt.Errorf("resident: create 需要 id")
	}
	inputChs := splitComma(strOf(arg, "input_chs"))
	if len(inputChs) == 0 {
		return "", fmt.Errorf("resident: create 需要至少一个 input_chs（不划通道它收不到任何输入）")
	}
	tempPath := strings.TrimSpace(strOf(arg, "temp_path"))
	if tempPath == "" {
		anchor := a.dataDir
		if anchor == "" && a.GraphDB() != nil && a.GraphDB().Path() != "" {
			anchor = filepath.Dir(filepath.Dir(a.GraphDB().Path()))
		}
		if anchor == "" {
			return "", fmt.Errorf("resident: create 需要 data_dir 或显式 temp_path")
		}
		tempPath = filepath.Join(anchor, "residents", id, "graph.db")
	}
	info, err := a.SpawnResident(ResidentOptions{
		ID:                id,
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
	if m := a.Instances(); m != nil {
		m.remember(string(kindResident), id, arg)
	}
	return "已创建驻留子: " + MarshalResidentInfo(info), nil
}

// residentAdmin 处理驻留子的**类型独有**动作（compress / reclaim / table）。
func (a *Agent) residentAdmin(action string, arg map[string]interface{}) (string, error) {
	id := strings.TrimSpace(strOf(arg, "id"))
	if id == "" {
		return "", fmt.Errorf("resident: 动作 %q 需要 id", action)
	}
	switch action {
	case "compress":
		n, err := a.CompressResident(id)
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
		n, msg, err := a.FinishResidentWithTarget(id, policy, ChannelOrphan, "", nil)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已回收 %s（残余 %d 条：%s）", id, n, msg), nil
	case "table":
		table, err := a.ResidentTable(id)
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
