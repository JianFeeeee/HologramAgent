//go:build ignore

// badprotoplugin 上报不同主版本的 SDK，验证内核显式拒绝而非半兼容运行。
//
// 主版本跃迁是破坏性变更的定义（如 1.0.0 从 C ABI 改子进程），
// 所以它是兼容性的正确分界——不需要另一个自定的协议号来复述。
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
		if req.Method == "handshake" {
			send(response{ID: req.ID, Result: map[string]interface{}{
				"sdk_version": "99.0.0", // 主版本不同，故意不匹配
				"plugin_name": "badproto",
				"pid":         os.Getpid(),
			}})
			continue
		}
		if req.ID != 0 {
			send(response{ID: req.ID})
		}
	}
}
