//go:build ignore

// readonlyplugin 是只读 stage 插件（模拟 weather 的 AfterToolcall）：
// 读取共享段但不写回任何字段。
//
// **这是 lost update 修复的关键验证对象**：C ABI 副本模型下，
// 它会把自己收到的旧快照无条件回传，覆盖 sanitizer 的清洗结果
// （§8.6 实测现网 1.6~4.3% 被覆盖）。共享内存模型下它零写入。
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"syscall"
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
	pendMu  sync.Mutex
	pending = map[uint64]chan json.RawMessage{}

	shm []byte
)

func send(v interface{}) {
	b, _ := json.Marshal(v)
	writeMu.Lock()
	out.Write(b)
	out.WriteByte('\n')
	out.Flush()
	writeMu.Unlock()
}

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

const (
	offArenaBase = 8
	offCtxBase   = 20
	sliceSize    = 8
	fToolResults = 10
)

func readToolResults() []byte {
	base := binary.LittleEndian.Uint32(shm[offArenaBase:])
	cb := binary.LittleEndian.Uint32(shm[offCtxBase:])
	o := cb + uint32(fToolResults*sliceSize)
	off := binary.LittleEndian.Uint32(shm[o:])
	ln := binary.LittleEndian.Uint32(shm[o+4:])
	if off == 0 && ln == 0 {
		return nil
	}
	return shm[base+off : base+off+ln]
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
		if json.Unmarshal(line, &probe) != nil {
			continue
		}
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
			var hp struct {
				ShmSize int `json:"shm_size"`
			}
			json.Unmarshal(req.Params, &hp)
			if hp.ShmSize > 0 {
				m, err := syscall.Mmap(3, 0, hp.ShmSize,
					syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					send(response{ID: req.ID, Error: fmt.Sprintf("mmap: %v", err)})
					continue
				}
				// 统一区域：前 64B 是 SuperBlock，StageContext 段在其后
				ctxOff := binary.LittleEndian.Uint32(m[20:])
				ctxSize := binary.LittleEndian.Uint32(m[24:])
				shm = m[ctxOff : ctxOff+ctxSize]
			}
			send(response{ID: req.ID, Result: map[string]interface{}{
				"sdk_version": "1.0.0", "plugin_name": "readonly", "pid": os.Getpid(),
			}})

		case "plugin.init":
			send(response{ID: req.ID})

		case "plugin.start":
			go func(id uint64) {
				callKernel("stage.register", map[string]interface{}{
					"stage": "after_toolcall",
					"scope": "global",
				})
				send(response{ID: id})
			}(req.ID)

		case "plugin.stop":
			send(response{ID: req.ID})
			out.Flush()
			os.Exit(0)

		case "stage.invoke":
			go func(id uint64) {
				if shm == nil {
					send(response{ID: id, Error: "共享段未挂载"})
					return
				}
				callKernel("stage.lock", nil)
				// 只读：读了但一个字节都不写回
				_ = readToolResults()
				callKernel("stage.unlock", nil)
				send(response{ID: id, Result: map[string]interface{}{"dirty_fields": 0}})
			}(req.ID)

		default:
			if req.ID != 0 {
				send(response{ID: req.ID})
			}
		}
	}
}
