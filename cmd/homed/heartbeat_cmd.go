package main

// 心跳里的**控制指令**：worker → guard 的唯一意图通道。
//
// ## 为什么走心跳，而不是另开一条通道
//
// guard 与 worker 之间已经有一条现成的、每 5s 一次的单向通报（心跳文件）。
// 用户点"重启/关机"本质就是"我要你在下一次心跳时知道这件事"——
// 开第二条通道（unix socket / 信号 / 临时文件）只会多一套要防重、防丢、
// 防竞态的机制，而这条已经存在且有 mtime 存活语义。
//
// ## 格式演进（必须向后兼容）
//
// 原格式每行 `t=<unix>\n`，**只用来刷 mtime**，内容从没有人读。
// 现在追加一行指令：
//
//	t=<unix>
//	cmd=restart reason=<理由>
//
// ★ 兼容性：老 guard 只 stat mtime、不读内容 ⇒ 追加行完全无害。
//   新 guard 读不到 cmd 行 ⇒ 行为与改动前逐字一致。
//
// ## ★ 指令为何必须**一次性消费**
//
// 如果 guard 读到 cmd 就执行、却不清理，那么下一次心跳（或下一次重启后的
// 第一个心跳）仍看得到同一条 cmd ⇒ 无限重启。
// ⇒ 消费方（guard）读完立即**原子地**把 cmd 行抹掉。
// 用 O_TRUNC 重写整个文件（只保留 t= 行）——文件只有一个写者（worker 的
// ticker）和一个读者（guard），且两者不会同时写（worker 退出后才轮到 guard 读）。
//
// ## 为什么关机要 guard 来做，而不是 worker 直接调 systemctl
//
// worker 是被守护方。它自己调 `systemctl stop` 等于在父进程眼皮底下自杀，
// 且 systemd 可能把它判成"异常退出"而先计入一次失败。
// 更根本的是：**只有 guard 知道这次退出该不该重拉**。让决策集中在一个地方。

import (
	"bufio"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 心跳指令取值。
const (
	hbCmdRestart  = "restart"
	hbCmdShutdown = "shutdown"
)

// hbCommand 是从心跳文件里读出的指令。
type hbCommand struct {
	Cmd    string // "" | "restart" | "shutdown"
	Reason string
	// OK 表示确实读到了合法指令（Cmd 非空）。
	OK bool
}

// parseHeartbeatCmd 解析心跳文件内容，取出指令。
//
// 容错：任何畸形行都忽略（心跳文件是"尽力而为"的通道，不该因为
// 一个坏行就让 guard 崩掉）。
func parseHeartbeatCmd(content string) hbCommand {
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "cmd=") {
			continue
		}
		rest := strings.TrimPrefix(line, "cmd=")
		var cmd, reason string
		if i := strings.Index(rest, " reason="); i >= 0 {
			cmd = strings.TrimSpace(rest[:i])
			reason = strings.TrimSpace(rest[i+len(" reason="):])
		} else {
			cmd = strings.TrimSpace(rest)
		}
		switch cmd {
		case hbCmdRestart, hbCmdShutdown:
			return hbCommand{Cmd: cmd, Reason: reason, OK: true}
		}
	}
	return hbCommand{}
}

// consumeHeartbeatCmd 读出指令并**立即清除**它（一次性语义）。
//
// 清除方式：重写文件为只含当前时间戳的心跳行，这样 mtime 仍是新的
// （不会让 guard 误判 worker 卡死），而 cmd 行消失。
func consumeHeartbeatCmd(path string) hbCommand {
	// ★ 先看大小再决定读不读（2026-10-17 修正）。
	//
	// 修复前：无条件 ReadFile(path) —— 生产上那个文件长到 14.6MB，
	// 每次 worker 退出都要整读一遍（实测 27ms，纯属浪费）。
	//
	// 滚动写入修好后文件只有十几到几十字节，但这里仍保留尺寸守卫：
	// 老部署上那个巨型文件还在，第一次读完就会被清除，不该为此再读几次。
	const maxRead = 8 << 10 // 8KB 足够容纳 t= 行 + cmd= 行
	fi, err := os.Stat(path)
	if err != nil {
		return hbCommand{}
	}
	if fi.Size() > maxRead {
		// 巨型历史文件：指令若有，一定在**末尾**（worker 最后几次心跳）。
		// 只读尾部 8KB，避免整读。
		return consumeTail(path, maxRead)
	}
	return consumeFrom(path)
}

// consumeTail 只读文件尾部（老部署的巨型心跳文件）。
func consumeTail(path string, n int64) hbCommand {
	f, err := os.Open(path)
	if err != nil {
		return hbCommand{}
	}
	defer f.Close()
	fi, _ := f.Stat()
	off := fi.Size() - n
	if off < 0 {
		off = 0
	}
	buf := make([]byte, n)
	read, _ := f.ReadAt(buf, off)
	c := parseHeartbeatCmd(string(buf[:read]))
	if c.OK {
		clearHeartbeatCmd(path)
	}
	return c
}

// consumeFrom 读小文件（正常路径）。
func consumeFrom(path string) hbCommand {
	b, err := os.ReadFile(path)
	if err != nil {
		return hbCommand{}
	}
	c := parseHeartbeatCmd(string(b))
	if c.OK {
		clearHeartbeatCmd(path)
	}
	return c
}

// clearHeartbeatCmd 把心跳文件重写为只含当前时间戳的一行。
//
// 原子（写临时文件再 rename）：避免 guard 读到半截内容。
// 同时把巨型历史文件**一次性收敛**成十几字节 —— 老部署升级后第一次
// 消费指令就顺带清理掉了。
func clearHeartbeatCmd(path string) {
	tmp := path + ".cmdtmp"
	now := []byte("t=" + strconv.FormatInt(time.Now().Unix(), 10) + "\n")
	if err := os.WriteFile(tmp, now, 0644); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
	}
}

// ---- worker 侧的待发指令队列 ----

// pendingCmd 是"用户点了按钮，但 guard 还没消费"的指令。
//
// 用 mutex + 单槽（不是队列）：重启/关机都是**目标态**，连点两下不该
// 排队两次。后来的请求覆盖前者即可（都指向"要重启"这件事）。
var (
	pendingCmdMu sync.Mutex
	pendingCmd   hbCommand
	// pendingWritten 标记这条指令是否**已经写进过心跳文件**。
	//
	// ★ 为什么需要它（否则指令永远清不掉）：
	//   guard 与 worker 是**两个进程**。guard 消费后把文件重写成纯 t= 行，
	//   但 worker 内存里的 pendingCmd 还是满的 ⇒ 下一次 tick 又写回去
	//   ⇒ guard 又读到 ⇒ **无限重启/关机**。
	//   真正的"已消费"信号只能从**文件**上读：写过了、而文件里的 cmd 行没了
	//   ⇒ 说明被 guard 吃了 ⇒ 清本地 pending。
	pendingWritten bool
)

// requestHeartbeatCmd 由一个外部调用（WebUI 经内核接口）投递指令。
//
// 立即返回：真正的写入发生在下一次心跳（≤5s）。
func requestHeartbeatCmd(cmd, reason string) {
	pendingCmdMu.Lock()
	pendingCmd = hbCommand{Cmd: cmd, Reason: reason, OK: true}
	pendingWritten = false
	pendingCmdMu.Unlock()
}

// nextHeartbeatWrite 决定这一次心跳该写什么内容。
//
// 返回 (content, writeFull)：
//   - content 非空 ⇒ 用**整文件重写**（保留 t= 行 + cmd= 行）
//   - content 为空 ⇒ 走原来的 append 语义（只刷 mtime）
//
// 消费检测也在这里：若指令写过、而文件里已没有 cmd 行，说明 guard 已消费，
// 清掉本地 pending，本次回到 append。
func nextHeartbeatWrite(hbPath string, now int64) string {
	pendingCmdMu.Lock()
	defer pendingCmdMu.Unlock()

	if pendingCmd.Cmd == "" {
		return ""
	}
	if pendingWritten {
		// 检查 guard 是否已消费（guard 消费 = 把文件重写成纯 t= 行）
		if b, err := os.ReadFile(hbPath); err == nil && !strings.Contains(string(b), "cmd=") {
			log.Printf("[homed] control command %s consumed by guard, clearing local pending state", pendingCmd.Cmd)
			pendingCmd = hbCommand{}
			pendingWritten = false
			return ""
		}
	}
	pendingWritten = true
	return "t=" + strconv.FormatInt(now, 10) + "\ncmd=" + pendingCmd.Cmd + " reason=" + pendingCmd.Reason + "\n"
}

// stopOwnUnit 让 systemd 停下来（不再自动拉起）。
//
// ★ 为什么必须显式 stop，不能只是 return：
//
//	生产 unit 是 `Restart=always`。guard 干净退出后 systemd 会立刻拉起整个
//	unit —— 用户点了"关机"，几秒后服务又活了，看起来像按钮没生效。
//
// 用 `systemctl stop <unit>`：
//
//	· 不依赖 unit 里配 RestartPreventExitStatus（那种做法要求 drop-in 与
//	  主 unit 一起交付，部署时容易漏）
//	· 走正常停机序列（SIGTERM → TimeoutStopSec），与 guard 自己收尾不冲突
//
// unit 名为空（非 systemd 环境 / 探测失败）时只记日志：
// 那种情况下 guard 退出后进程就真的没了，无需额外动作。
func stopOwnUnit(unit string) {
	if unit == "" {
		log.Printf("[guard] non-systemd environment (unit unknown): guard can just exit, systemd will not restart it")
		return
	}
	log.Printf("[guard] running systemctl stop %s", unit)
	// 同步执行：guard 此时已经没有别的事要做，且必须在**自己退出之前**
	// 让 systemd 收到停止请求。异步会与 return 竞争 ——
	// guard 退了、systemctl 还没跑，systemd 可能已经判定 unit 需要重启。
	cmd := exec.Command("systemctl", "stop", unit)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Printf("[guard] systemctl stop %s failed: %v (guard still exits; "+
			"if the unit is Restart=always it may be started again)", unit, err)
	}
}
