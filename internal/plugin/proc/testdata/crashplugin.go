//go:build ignore

// crashplugin 在收到 boom 工具调用时 panic，用于验证崩溃隔离：
// 子进程死亡不应带崩 homed，且内核须能感知退出（供 recordCrash 使用）。
package main

import (
	"bufio"
	"encoding/json"
	"os"
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
				"sdk_version": "1.0.0", "plugin_name": "crash", "pid": os.Getpid(),
			}})
		case "tool.invoke":
			// 模拟插件 bug：直接 panic，进程带非零码退出
			panic("插件内部 panic：用于验证崩溃隔离")
		default:
			if req.ID != 0 {
				send(response{ID: req.ID})
			}
		}
	}
}
