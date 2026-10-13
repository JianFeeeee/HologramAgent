//go:build ignore

// oldminorplugin 上报同主版本但更早的中版本 SDK，验证内核**不会**无谓拒绝。
//
// 规约（docs/git-branching.md §七.1）：1.x 内的中版本跃迁全部是纯追加 ——
// 新增方法由插件调用、内核实现，不调就不受影响。因此未重编的存量插件
// （如用 SDK 1.0.0/1.1.0 编的）必须能在新内核上直接建链。
//
// 若判据误用「全等比较」而非「主版本比较」，这条会失败 —— 正是它的价值。
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
				"sdk_version": "1.0.0", // 同主版本，更早的中版本
				"plugin_name": "oldminor",
				"pid":         os.Getpid(),
			}})
			continue
		}
		if req.ID != 0 {
			send(response{ID: req.ID})
		}
	}
}
