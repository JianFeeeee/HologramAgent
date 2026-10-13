//go:build ignore

// forkplugin 在启动时 fork 一个存活时间比自己长的子进程（继承同一个 stdout），
// 然后在收到 die 工具调用时让自己退出。
//
// 用途：复现「EOF 不等于进程死亡」这一缺陷。
// 插件本体死后，孙子进程仍持有 stdout 写端，父进程（homed）的 readLoop
// 永远读不到 EOF——若内核只靠 EOF 判定死亡，就会完全感知不到插件已死：
// 工具调用一直超时、崩溃回调不触发、自动重启永不发生。
// 生产上 browser 拉 chromium、editdoc 拉 python 都是这个形状。
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
)

type request struct {
	ID     uint64          `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type response struct {
	ID     uint64      `json:"id"`
	Result interface{} `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"`
}

func main() {
	// 孙子进程**只**继承 stdout（本测试的要点），不给 stderr：
	// 插件的 stderr 直通到 go test 的捕获管道，孙子抿着它不放会让
	// go test 在测试全部通过后仍等 60s I/O。
	//
	// sleep 给 3s：只需在插件本体退出的那一瞬间它还持有写端即可（实际 <200ms），
	// 不必拖得更久而拖慢测试。
	child := exec.Command("sleep", "3")
	child.Stdout = os.Stdout
	_ = child.Start()

	in := bufio.NewScanner(bufio.NewReader(os.Stdin))
	out := bufio.NewWriter(os.Stdout)
	send := func(v interface{}) {
		b, _ := json.Marshal(v)
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}

	for in.Scan() {
		var req request
		if err := json.Unmarshal(in.Bytes(), &req); err != nil {
			continue
		}
		switch req.Method {
		case "handshake":
			send(response{ID: req.ID, Result: map[string]interface{}{
				"sdk_version": "1.0.0", "plugin_name": "fork", "pid": os.Getpid(),
			}})
		case "tool.invoke":
			// 不回应答，直接退出：模拟插件突然死亡（崩溃/被 kill）。
			os.Exit(7)
		default:
			if req.ID != 0 {
				send(response{ID: req.ID})
			}
		}
	}
}
