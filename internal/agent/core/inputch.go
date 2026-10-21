package core

// inputch 总览：**单工具多视图**。
//
// inputch 是**最基本的输入路由单位**（由插件注册，一个插件可注册多个）。
// 父 agent 需要能看清两件事：
//  1. 有哪些 inputch 已注册（谁注册的）；
//  2. 它们是怎么划分的（各自划给了哪个 agent、容量多少）。
//
// 按用户要求做成**单工具多视图**（一个 `input_channels` 工具 + `view` 参数），
// 而不是一堆小工具 —— 视图切换比工具增殖更好用，也更省提示词预算。

import (
	"fmt"
	"sort"
	"strings"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

func (a *Agent) executeInputChannels(tc agentAPI.ToolCall) string {
	view, _ := tc.Arguments["view"].(string)
	view = strings.TrimSpace(view)
	if view == "" {
		view = "all"
	}
	name, _ := tc.Arguments["name"].(string)

	all := a.io.InputChannels()
	if len(all) == 0 {
		return "没有任何已注册的 inputch。"
	}

	switch view {
	case "all":
		return a.renderInputChannels(all, "全部已注册 inputch")
	case "mine":
		return a.renderInputChannels(a.channelRegistry().ListByOwner(string(a.id)),
			"划给本 agent（"+string(a.id)+"）的 inputch")
	case "unassigned":
		return a.renderInputChannels(a.channelRegistry().ListByOwner(""),
			"尚未划出的 inputch（可按需分配）")
	case "by_agent":
		return a.renderInputChannelsByAgent(all)
	case "detail":
		if name == "" {
			return "view=detail 需要 name 参数（inputch 名）"
		}
		ch, ok := a.io.LookupInputChannel(name)
		if !ok {
			return fmt.Sprintf("inputch %q 未注册", name)
		}
		return renderInputChannelDetail(ch)
	default:
		return fmt.Sprintf("未知 view=%q；可用：all | mine | unassigned | by_agent | detail", view)
	}
}

func (a *Agent) channelRegistry() *agentIO.ChannelRegistry { return a.io.ChannelRegistry() }

// renderInputChannels 渲染一组 inputch 的一行式概览。
func (a *Agent) renderInputChannels(list []agentIO.InputChannel, title string) string {
	if len(list) == 0 {
		return title + "：无"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s（%d 个）:", title, len(list))
	for _, ch := range list {
		fmt.Fprintf(&b, "\n  - %s%s%s", ch.Name, pluginSuffix(ch), policySuffix(ch))
		fmt.Fprintf(&b, "\n      归属: %s", ownerLabel(ch.Owner))
		if ch.Capacity > 0 {
			fmt.Fprintf(&b, " | 容量: %d", ch.Capacity)
		}
		if ch.Output != "" {
			fmt.Fprintf(&b, " | 默认回程: %s", ch.Output)
		}
	}
	return b.String()
}

// renderInputChannelsByAgent 按归属分组（"划分情况"总览）。
func (a *Agent) renderInputChannelsByAgent(all []agentIO.InputChannel) string {
	byOwner := map[string][]agentIO.InputChannel{}
	for _, ch := range all {
		byOwner[ch.Owner] = append(byOwner[ch.Owner], ch)
	}
	owners := make([]string, 0, len(byOwner))
	for o := range byOwner {
		owners = append(owners, o)
	}
	sort.Strings(owners)

	var b strings.Builder
	fmt.Fprintf(&b, "inputch 划分情况（共 %d 个）:", len(all))
	for _, o := range owners {
		names := make([]string, 0, len(byOwner[o]))
		for _, ch := range byOwner[o] {
			names = append(names, ch.Name)
		}
		sort.Strings(names)
		fmt.Fprintf(&b, "\n  - %s: %s", ownerLabel(o), strings.Join(names, ", "))
	}
	return b.String()
}

// renderInputChannelDetail 渲染单个 inputch 的全部字段。
func renderInputChannelDetail(ch agentIO.InputChannel) string {
	var b strings.Builder
	fmt.Fprintf(&b, "inputch: %s\n", ch.Name)
	fmt.Fprintf(&b, "  注册插件: %s\n", orDash(ch.Plugin))
	fmt.Fprintf(&b, "  归属 agent: %s\n", ownerLabel(ch.Owner))
	if ch.Capacity > 0 {
		fmt.Fprintf(&b, "  容量: %d\n", ch.Capacity)
	} else {
		fmt.Fprintf(&b, "  容量: 内核默认\n")
	}
	fmt.Fprintf(&b, "  默认回程输出通道: %s\n", orDash(ch.Output))
	fmt.Fprintf(&b, "  记忆策略: %s\n", policyLabel(ch))
	return b.String()
}

func pluginSuffix(ch agentIO.InputChannel) string {
	if ch.Plugin == "" {
		return ""
	}
	return "（插件 " + ch.Plugin + "）"
}

// policySuffix 用短标记提示策略（详见 view=detail）。
func policySuffix(ch agentIO.InputChannel) string {
	var m []string
	if ch.Def.NoMemory {
		m = append(m, "无记忆")
	}
	if ch.Def.Cleaner != nil {
		m = append(m, "清洗")
	}
	if ch.Def.ContextPolicy != "" && ch.Def.ContextPolicy != "none" {
		m = append(m, "裁剪:"+ch.Def.ContextPolicy)
	}
	if ch.Def.RecallPolicy != "" && ch.Def.RecallPolicy != "auto" {
		m = append(m, "召回:"+ch.Def.RecallPolicy)
	}
	if len(m) == 0 {
		return ""
	}
	return " [" + strings.Join(m, "/") + "]"
}

func policyLabel(ch agentIO.InputChannel) string {
	if s := policySuffix(ch); s != "" {
		return strings.Trim(s, " []")
	}
	return "默认（记入记忆、不裁剪）"
}

func ownerLabel(owner string) string {
	if owner == "" {
		return "未分配（根 agent/内核默认）"
	}
	return owner
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
