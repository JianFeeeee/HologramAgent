package webui

// 聊天受理超时的判据（2026-10-17 内测故障）。
//
// ## 故障现象
//
// 用户发一句话，界面回「已取消（用户停止）」。而实际日志显示
// agent **一直在正常干活**，并且跑完了：
//
//	20:58:36  POST /chat 开始
//	20:58:52  ★ 前端 15s ackTimer abort → 服务端 status=504 (15.89s)
//	20:59:29  ★ agent 正常完成，52552ms，22 个工具，返回 response
//
// 也就是说：回复被生成了，但 HTTP 连接已被前端自己掐断 ⇒ 送不回去，
// 用户看到的是"已取消（用户停止）"——一个**用户从没做过的操作**。
//
// ## 根因
//
// 前端 `sendChat()` 里把"受理确认"的等待写死成 15 秒，而 agent 的
// 正常任务动辄 45-55 秒（工具越多越久）。这个 POST 本来就是**触发式**的，
// 真正渲染靠 SSE，没必要在这里抢时间。
//
// ## 判据
//
//  1. 那个超时必须远大于实测的正常任务耗时（不能是 15s 这种量级）
//  2. 超时后的提示不能说成"失败/超时错误"，否则用户会重发造成双任务
//
// ★ 为什么用源码扫描而不是行为测试：这是前端 abort 时序，
//   没有可注入的时钟。扫描至少能防住"有人把 15s 改回来"。

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func dashboardJS(t *testing.T) string {
	t.Helper()
	// webui 包目录 → 同目录下 dashboard.js
	p := filepath.Join("dashboard.js")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 dashboard.js: %v", err)
	}
	return string(b)
}

// ★★★ 判据：受理超时必须 ≥ 5 分钟。
//
// 实测正常任务 45-55 秒；留 5 分钟以上余量，
// 保证"任务慢"不会被误判成"连接坏了"。
func TestChatAckTimeoutIsGenerous(t *testing.T) {
	src := dashboardJS(t)

	// 找 ackTimer 的 setTimeout 时长
	re := regexp.MustCompile(`var ackTimer = setTimeout\(\(\) => \{\s*ctrl\.abort\(\);\s*\}, (\d+)\)`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatal("找不到 ackTimer 的超时值 —— 判据需要更新（代码结构变了？）")
	}
	ms, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("超时值不是数字: %q", m[1])
	}
	const minAck = 300000 // 5 分钟
	if ms < minAck {
		t.Fatalf("★ 受理超时只有 %d ms —— 太小。\n"+
			"  实测 agent 正常任务就要 45-55 秒（22 个工具那次 52552ms）；\n"+
			"  超时太短会让前端 abort，而 agent 仍在正常干活，\n"+
			"  结果是用户看到「已取消（用户停止）」——他从没点过停止。", ms)
	}
}

// ★★★ 判据：超时提示不得把"还在处理"说成"失败"。
func TestChatAckTimeoutMessageDoesNotBlameFailure(t *testing.T) {
	src := dashboardJS(t)

	// 定位 ack 失败分支附近的 toast 文案
	i := strings.Index(src, "catch (ackErr)")
	if i < 0 {
		t.Fatal("找不到 ack 失败分支")
	}
	seg := src[i:]
	if end := strings.Index(seg, "} finally {"); end > 0 {
		seg = seg[:end]
	}

	// 不能出现"失败/错误/Error/failed"这类归因
	for _, bad := range []string{"请求失败", "Request failed", "请求超时", "Request timeout"} {
		if strings.Contains(seg, bad) {
			t.Fatalf("★ ack 超时提示用了 %q —— 会让用户以为出错了而重发，"+
				" 造成同一句话产生两条任务（实测 agent 其实还在正常跑）", bad)
		}
	}
	// 必须告诉用户"已发出、正在处理、不要重发"
	if !strings.Contains(seg, "正在处理") && !strings.Contains(seg, "is working") {
		t.Fatal("★ ack 超时提示没说清'AI 正在处理' —— 用户会以为消息丢了")
	}
	if !strings.Contains(seg, "不要重复发送") && !strings.Contains(seg, "do not resend") {
		t.Fatal("★ 没提醒不要重发 —— 重发会造成重复任务")
	}
	// 不能标成 error（第二个参数 true）
	if strings.Contains(seg, ",\n\t\t\t\ttrue,\n\t\t\t);") && !strings.Contains(seg, "false,") {
		t.Log("提示: 建议用非 error 样式展示（这不是错误）")
	}
}
