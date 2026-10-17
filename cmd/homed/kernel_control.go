package main

// 内核控制通道的实现：把 WebUI 的"重启/关机"请求转成心跳指令。
//
// ## 完整链路（与心跳复用，不新开通道）
//
//	WebUI 点按钮
//	  → POST /api/v1/kernel/{restart,shutdown}
//	  → sdk.KernelControlAPI.RequestKernel*（本文件实现，只投递不等待）
//	  → requestHeartbeatCmd()：把指令放进 worker 内存的单槽队列
//	  → 下一次心跳（≤5s）把 `cmd=` 行写进 <data>/heartbeat
//	  → ① 重启：worker 走正常清理 → exit 42 → guard 重拉（既有语义，零改动）
//	    ② 关机：guard 在 workerCleanExit 分支读到 cmd=shutdown
//	            → systemctl stop → 不再拉起
//
// ## ★ 为什么不用"worker 自己调 systemctl stop"
//
// worker 是被守护方。它自己停服务等于在父进程眼皮底下自杀，而且
// systemd 可能先把这次退出记成一次失败。更根本的是：
// **只有 guard 知道这次退出该不该重拉** —— 决策该集中在一个地方。
//
// ## 重启为什么几乎零改动
//
// `exitRestartRequested=42` 与 guard 的 `workerRestartRequested` 分支早已存在
// 且生产验证过（热更新就走这条路）。我们只是给"用户点按钮"也接到同一个出口。

import (
	"fmt"
)

// kernelControlImpl 实现 sdk.KernelControlAPI。
type kernelControlImpl struct {
	// restartCh 是内核主循环的重启通道（既有机制，supervisor 也用）。
	restartCh chan<- struct{}
}

// RequestKernelRestart 请求热重启。
//
// 走**既有**的 restartCh（与 supervisor.Restart 同一条路）：
// worker 清理 → exit 42 → guard 用当前二进制重拉，不计失败轮次。
func (k *kernelControlImpl) RequestKernelRestart(reason string) error {
	if k.restartCh == nil {
		return fmt.Errorf("内核重启通道未装配")
	}
	select {
	case k.restartCh <- struct{}{}:
		return nil
	default:
		// 已有待处理的重启请求 ⇒ 目标一致，视为成功（幂等）
		return nil
	}
}

// RequestKernelShutdown 请求关机。
//
// ★ 与重启的区别：关机**不能**复用 restartCh（那会被 guard 重拉）。
// 必须让 guard 在 worker 退出后知道"这次别再起了" ⇒ 写心跳指令。
//
// 这里只投递；真正的写入在 worker 的下一次心跳（≤5s），
// 因此本方法立即返回 —— 调用方（HTTP handler）需要的是"请求已受理"。
func (k *kernelControlImpl) RequestKernelShutdown(reason string) error {
	requestHeartbeatCmd(hbCmdShutdown, reason)
	return nil
}
