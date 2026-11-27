package agent

// rtoutput.go —— **rt 输出通道**（`outputch(rt)`）归基座管理。
//
// ## 为什么在基座
//
// 用户裁定 2026-11-25：
//
//	「rt 有额外的 outputch(rt)，意思是这个 rt 通道会自动接收 agent"说"的话
//	  通过 rt 通道发出去，是由内核的 device 抽象管理的」
//
// 也就是说：agent 说出来的每一段增量，**自动**经它被划到的那个 rt 输出通道
// 发出。这是 device 层的能力，与"是哪个类型"无关 —— 类型不该知道
// 设备名、不该持有异步队列、不该自己去 Execute。
//
// ## 此前错在哪
//
// `rtchild` 自己持有 `pump *kit.OutputPump`、自己 `GetDevice(...)`、
// 自己 `dev.Execute(...)`。后果：
//
//	· 换一种类型（比如未来的"低延迟语音子"）要重写一遍这套异步壳
//	· 通道数量约束无处落地（一个 rtchild 最多一对 rt 通道，见下）
//	· 类型认识 `agentIO.Device` 的形状 —— 越过 device 抽象
//
// ## 一条 rt 输出通道，不是一个列表
//
// 一个 rt agent **最多被划一对** rt 通道（input + output）：
// 双通道会让"说出去的话"有两条落点，用户听到的与界面上看到的不一致。
// 因此这里是**单值字段**而不是 map/列表，重复划拨由内核拒绝。

import (
	"fmt"
	"strings"
	"sync"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
)

// rtOutputState 是本 agent 的 rt 输出通道状态（基座持有）。
//
// 用独立小结构 + 独立锁：它被 rtLoop（写）与 Dispose（拆）并发访问，
// 而 Agent 的字段锁（semanticMu）同时保护很多别的东西 —— 混用会让
// "推一段文本"卡住一次装配期写。
type rtOutputState struct {
	mu      sync.Mutex
	name    string          // 通道名（空 = 未划拨）
	pump    *kit.OutputPump // 异步壳（nil = 未建）
	bindErr error           // 划拨时的错误（诊断用）
}

// SetRTOutputChannel 把一个**输出通道**设为本 agent 的 rt 输出。
//
// 由内核在划拨 rt 通道时调用（`AssignRealtime`）。空名表示撤销。
func (a *Agent) SetRTOutputChannel(name string) error {
	if a == nil {
		return fmt.Errorf("rt 输出通道：agent 为空")
	}
	name = strings.TrimSpace(name)

	a.rtOut.mu.Lock()
	defer a.rtOut.mu.Unlock()

	if name == "" {
		// 撤销：拆掉异步壳（排空后退出）。
		if a.rtOut.pump != nil {
			a.rtOut.pump.Close()
			a.rtOut.pump = nil
		}
		a.rtOut.name = ""
		return nil
	}
	if a.rtOut.name != "" && a.rtOut.name != name {
		return fmt.Errorf("rt 输出通道已划为 %q，不能改成 %q "+
			"（一个 rt agent 最多一条 rt 输出通道：两条会让\"说出去的话\"有两个落点）",
			a.rtOut.name, name)
	}
	a.rtOut.name = name
	// 异步壳按需重建（每次**重新解析**设备 —— 插件崩溃重启后设备是新对象，
	// 缓存指针会一直指向死设备，症状是"插件重启后 rt 再也说不出话"）。
	if a.rtOut.pump == nil {
		a.rtOut.pump = kit.NewOutputPump(name, func() agentIO.Device {
			if a.IO() == nil {
				return nil
			}
			return a.IO().GetDevice(name)
		})
	}
	return nil
}

// RTOutputChannel 返回本 agent 被划到的 rt 输出通道名（"" = 没有）。
func (a *Agent) RTOutputChannel() string {
	if a == nil {
		return ""
	}
	a.rtOut.mu.Lock()
	defer a.rtOut.mu.Unlock()
	return a.rtOut.name
}

// EmitRTText 把一段增量交给本 agent 的 rt 输出通道（**异步**）。
//
// ★ 这是类型唯一的"说话"入口：它只给文本，不知道通道是谁、也不碰设备。
//
//	异步的理由（用户裁定 2026-11-04）：插件侧的延迟（TTS / 网络编码 /
//	字幕渲染）不该节流模型的 token 消费 —— "不攒整段"的收益会被吃掉。
//
// 返回 false 表示没有可用的 rt 输出通道（未划拨 / 已被撤销）。
// 调用方据此决定是否留痕（静默丢弃是"rt 说不出话"最难查的形态）。
func (a *Agent) EmitRTText(text string) (queued bool) {
	if a == nil || text == "" {
		return false
	}
	a.rtOut.mu.Lock()
	pump := a.rtOut.pump
	name := a.rtOut.name
	a.rtOut.mu.Unlock()
	if pump == nil || name == "" {
		return false
	}
	// ★ 锁外推：Push 在队列满时会阻塞，持锁会卡住撤销路径。
	pump.Push(text)
	return true
}

// CloseRTOutput 排空并关停本 agent 的 rt 输出通道（销毁时由内核调）。
func (a *Agent) CloseRTOutput() {
	if a == nil {
		return
	}
	a.rtOut.mu.Lock()
	pump := a.rtOut.pump
	a.rtOut.pump = nil
	name := a.rtOut.name
	a.rtOut.name = ""
	a.rtOut.mu.Unlock()
	if pump != nil {
		pump.Close()
	}
	_ = name
}
