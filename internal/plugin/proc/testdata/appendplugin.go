//go:build ignore

// appendplugin 在 stage 中把自己的标记追加到 FinalText（读-改-写）。
//
// 用于跨进程 lost update 验证：多个此类插件并发处理同一 stage，
// 若全部标记都保留 ⇒ 无丢失；若少了 ⇒ 出现 lost update。
//
// 这是实验 8（5 进程 × 300 轮零丢失）在真实 RPC + 真实内核 RunStage
// 下的复刻——机制单测已过，这里验证集成后同样成立。
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
	tag string
)

func send(v interface{}) {
	b, _ := json.Marshal(v)
	writeMu.Lock()
	out.Write(b)
	out.WriteByte('\n')
	out.Flush()
	writeMu.Unlock()
}

func callKernel(method string, params interface{}) (json.RawMessage, bool) {
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
	r, ok := <-ch
	return r, ok
}

const (
	headerSize      = 64
	stageFieldCount = 18
	sliceSize       = 8
	flagCount       = 8

	offArenaBase = 8
	offArenaCap  = 12
	offArenaUsed = 16
	offCtxBase   = 20
	offSeq       = 24

	fFinalText = 5 // 与 proc/shmcodec.go 的 stageField 枚举顺序一致
)

func desc(field int) (uint32, uint32) {
	cb := binary.LittleEndian.Uint32(shm[offCtxBase:])
	o := cb + uint32(field*sliceSize)
	return binary.LittleEndian.Uint32(shm[o:]), binary.LittleEndian.Uint32(shm[o+4:])
}

func setDesc(field int, off, ln uint32) {
	cb := binary.LittleEndian.Uint32(shm[offCtxBase:])
	o := cb + uint32(field*sliceSize)
	binary.LittleEndian.PutUint32(shm[o:], off)
	binary.LittleEndian.PutUint32(shm[o+4:], ln)
}

func readFinalText() string {
	off, ln := desc(fFinalText)
	if off == 0 && ln == 0 {
		return ""
	}
	if ln == 0 {
		return ""
	}
	base := binary.LittleEndian.Uint32(shm[offArenaBase:])
	return string(shm[base+off : base+off+ln])
}

func writeFinalText(s string) error {
	used := binary.LittleEndian.Uint32(shm[offArenaUsed:])
	if used == 0 {
		used = 1
	}
	cap_ := binary.LittleEndian.Uint32(shm[offArenaCap:])
	if used+uint32(len(s)) > cap_ {
		return fmt.Errorf("arena 空间不足")
	}
	base := binary.LittleEndian.Uint32(shm[offArenaBase:])
	copy(shm[base+used:], []byte(s))
	binary.LittleEndian.PutUint32(shm[offArenaUsed:], used+uint32(len(s)))
	setDesc(fFinalText, used, uint32(len(s)))
	// 世代号自增
	v := binary.LittleEndian.Uint64(shm[offSeq:])
	binary.LittleEndian.PutUint64(shm[offSeq:], v+1)
	return nil
}

func main() {
	tag = os.Getenv("PLUGIN_TAG")
	if tag == "" {
		tag = "?"
	}

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
				"sdk_version": "1.0.0",
				"plugin_name": "append-" + tag, "pid": os.Getpid(),
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
				// 拿锁 → 读 → 追加自己的标记 → 写回 → 放锁
				callKernel("stage.lock", nil)
				cur := readFinalText()
				err := writeFinalText(cur + tag)
				callKernel("stage.unlock", nil)
				if err != nil {
					send(response{ID: id, Error: err.Error()})
					return
				}
				send(response{ID: id, Result: map[string]interface{}{"dirty_fields": 1}})
			}(req.ID)

		default:
			if req.ID != 0 {
				send(response{ID: req.ID})
			}
		}
	}
}
