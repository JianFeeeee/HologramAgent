//go:build ignore

// echoplugin 是测试用的最简子进程插件：实现握手 + 回显工具。
// 不 import 公开 SDK——只验证 proc 包的 RPC 机制本身。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
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
	in.Buffer(make([]byte, 0, 64*1024), 1024*1024)
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
				"sdk_version": "1.0.0",
				"plugin_name": "echo",
				"pid":         os.Getpid(),
			}})
		case "plugin.stop":
			send(response{ID: req.ID})
			out.Flush()
			os.Exit(0)
		case "tool.invoke":
			var p struct {
				Name string                 `json:"name"`
				Args map[string]interface{} `json:"args"`
			}
			json.Unmarshal(req.Params, &p)
			switch p.Name {
			case "fail_tool":
				send(response{ID: req.ID, Error: "故意失败：用于验证错误上报"})
			default:
				text, _ := p.Args["text"].(string)
				send(response{ID: req.ID, Result: map[string]interface{}{"result": text}})
			}
		case "event.deliver":
			// 通知：不回应答
		default:
			if req.ID != 0 {
				send(response{ID: req.ID, Error: fmt.Sprintf("未实现 %s", req.Method)})
			}
		}
	}
}
