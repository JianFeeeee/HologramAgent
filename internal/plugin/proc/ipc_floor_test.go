package proc

// ipc_floor_test.go —— 进程间通信的**成本地板**（纯测量，判定「优化 IPC」是否值得）。
//
// ============================ 为什么需要这个文件 ============================
// 本轮的目标是回答：「工具调用往返 30µs 里，那 ~20µs 非编解码部分花在哪、
// 能否优化」。而回答这类问题必须先建立**地板**：任何跨进程方案都有一个
// 由 OS 调度决定的下界，低于它是不可能达到的。
//
// 于是做了三层对照（同一台机、同一次会话）：
//
//	① OS 调度地板   `cat` 子进程管道 echo（无协议、无 JSON、无分配）
//	② 裸 RPC        最简 method、无载荷、无共享帧
//	③ 纯编解码      共享段 write+read+compact（纯内存，不跨进程）
//
// ★ 判据：若 ① 已经接近 ②，则 RPC 层的开销主要是**OS 调度**而非协议/JSON；
//   那么「优化 IPC」的空间就只剩下 ②−① 那一小段，而不是整个 ②。
//   没有地板数，任何「还能再快 X%」的说法都是空话。

import (
	"os/exec"
	"testing"
)

// BenchmarkIPC_OSFloorRawPipeEcho 建立跨进程往返的**绝对地板**：
// 一个 `cat` 子进程，父进程写一行、读回一行。不含任何协议解析。
//
// 这个是「无论怎么优化协议都不可能低于」的量级 —— 它只包含
// 两次进程唤醒（父→子、子→父）+ 两次管道读写。
func BenchmarkIPC_OSFloorRawPipeEcho(b *testing.B) {
	cmd := exec.Command("cat")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		b.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		b.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		b.Fatalf("Start: %v", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}()

	msg := []byte("{\"id\":1,\"method\":\"x\"}\n")
	buf := make([]byte, 4096)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := stdin.Write(msg); err != nil {
			b.Fatalf("write: %v", err)
		}
		if _, err := stdout.Read(buf); err != nil {
			b.Fatalf("read: %v", err)
		}
	}
}

// BenchmarkIPC_MinimalRPC 最简 RPC 往返：无载荷、不经共享帧。
//
// ★ 选 output.invoke 而不是 tool.invoke：前者不要求插件注册任何东西，
//
//	插件会回一个「未实现」错误 —— 但**往返已经完成**。
//	本基准测的是**传输成本**，因此应答内容无关紧要
//	（且 Call 的错误已被忽略，不会中断计时）。
func BenchmarkIPC_MinimalRPC(b *testing.B) {
	bin := buildBenchPlugin(b, "echoplugin.go")
	p, err := Spawn("echo", bin, Options{Handler: noopHandler})
	if err != nil {
		b.Fatalf("Spawn: %v", err)
	}
	defer p.Kill()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = p.Call(MethodOutputInvoke, nil)
	}
}
