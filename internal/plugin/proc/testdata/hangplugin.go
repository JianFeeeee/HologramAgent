//go:build ignore

// hangplugin 收到工具调用后永久阻塞，用于验证：
//  1. 调用方能凭 context 超时返回（不被拖死）
//  2. Kill 能真正回收资源（对比 cgo 超时后 OS 线程永久泄漏，§9.3）
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"time"
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
				"sdk_version": "1.0.0", "plugin_name": "hang", "pid": os.Getpid(),
			}})
		case "tool.invoke":
			// 永久卡住，永不回应答
			time.Sleep(10 * time.Minute)
		default:
			if req.ID != 0 {
				send(response{ID: req.ID})
			}
		}
	}
}
