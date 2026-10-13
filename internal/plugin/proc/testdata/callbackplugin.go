//go:build ignore

// callbackplugin 验证插件 → 内核的反向调用（51 个 core.* method 的机制）。
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
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

var (
	out     = bufio.NewWriter(os.Stdout)
	writeMu sync.Mutex
	nextID  uint64
	pending = map[uint64]chan json.RawMessage{}
	pendMu  sync.Mutex
)

func send(v interface{}) {
	b, _ := json.Marshal(v)
	writeMu.Lock()
	out.Write(b)
	out.WriteByte('\n')
	out.Flush()
	writeMu.Unlock()
}

// callKernel 反向调用内核并等待应答。
func callKernel(method string, params interface{}) json.RawMessage {
	pendMu.Lock()
	nextID++
	id := nextID
	ch := make(chan json.RawMessage, 1)
	pending[id] = ch
	pendMu.Unlock()

	var raw json.RawMessage
	if params != nil {
		b, _ := json.Marshal(params)
		raw = b
	}
	send(request{ID: id, Method: method, Params: raw})
	return <-ch
}

func main() {
	in := bufio.NewScanner(bufio.NewReader(os.Stdin))
	in.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for in.Scan() {
		line := make([]byte, len(in.Bytes()))
		copy(line, in.Bytes())

		var probe struct {
			ID     uint64 `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}

		// 内核对我们反向调用的应答
		if probe.Method == "" {
			var resp struct {
				ID     uint64          `json:"id"`
				Result json.RawMessage `json:"result"`
			}
			json.Unmarshal(line, &resp)
			pendMu.Lock()
			ch, ok := pending[resp.ID]
			delete(pending, resp.ID)
			pendMu.Unlock()
			if ok {
				ch <- resp.Result
			}
			continue
		}

		var req request
		json.Unmarshal(line, &req)
		switch req.Method {
		case "handshake":
			send(response{ID: req.ID, Result: map[string]interface{}{
				"sdk_version": "1.0.0",
				"plugin_name": "cb",
				"pid":         os.Getpid(),
			}})
		case "plugin.start":
			// 在独立 goroutine 里回调，避免阻塞读循环
			go func(id uint64) {
				callKernel("tool.register", map[string]interface{}{"name": "cb_tool"})
				callKernel("settings.get", map[string]interface{}{"key": "some_key"})
				send(response{ID: id})
			}(req.ID)
		case "plugin.stop":
			send(response{ID: req.ID})
			out.Flush()
			os.Exit(0)
		default:
			if req.ID != 0 {
				send(response{ID: req.ID})
			}
		}
	}
}
