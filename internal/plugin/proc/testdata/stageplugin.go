//go:build ignore

// stageplugin 是完整形态的测试插件：注册工具/阶段/输出通道，
// stage 处理经共享内存读改写（模拟 sanitizer 的清洗行为）。
//
// 它手写 RPC 与共享段访问，不依赖公开 SDK——因为 SDK 侧的 proc 支持
// 属于 Part 3（hmapdev 工具链）的内容。这里只验证内核侧机制。
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strings"
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

	shm      []byte
	shmSize  int
	region   []byte // 完整统一区域 mmap
	arenaOff uint32
)

// ---- Exchange Arena 槽池（与内核 proc/arena.go 一致）----

const (
	arOffSlotCount = 8
	arOffSlotSize  = 12
	slotHeaderSize = 8
)

// arenaSlotSize 返回单个槽的总字节数（含 8 字节槽头）。
func arenaSlotSize() uint32 {
	return binary.LittleEndian.Uint32(region[arenaOff+arOffSlotSize:])
}

// ---- 调用帧（funccall 模型）辅助 ----

// frameInput 返回帧内的输入段（内核写入的参数/输入文本）。
func frameInput(frame SharedRef, inputLen uint32) []byte {
	if frame.IsZero() || int(inputLen) > len(frame.Slice(region)) {
		return nil
	}
	return frame.Slice(region)[:inputLen]
}

// frameOutput 尝试把 payload 写进帧的结果区（帧内 [inputLen, frame.Length)）。
// 放不下时返回错误，由调用方决定是否申请扩容块。
func frameOutput(frame SharedRef, inputLen uint32, payload []byte, jsonFlag bool) (SharedRef, error) {
	if frame.IsZero() {
		return SharedRef{}, fmt.Errorf("无调用帧")
	}
	area := frame.Slice(region)
	start := int(inputLen)
	if start > len(area) || len(payload) > len(area)-start {
		return SharedRef{}, fmt.Errorf("帧内空间不足（需 %d，剩 %d）", len(payload), len(area)-start)
	}
	copy(region[frame.Offset+uint32(start):], payload)
	ref := SharedRef{
		Offset:     frame.Offset + uint32(start),
		Length:     uint32(len(payload)),
		Generation: frame.Generation,
	}
	if jsonFlag {
		ref.Flags |= sharedRefFlagJSON
	}
	return ref, nil
}

// arenaPut 申请一块扩容块并写入 payload，引用上打 sharedRefFlagExpand
// 告知内核该块需单独归还（插件只申请，回收由内核做）。
func arenaPut(payload []byte, jsonFlag bool) (SharedRef, error) {
	ref, err := arenaAlloc(uint32(len(payload)))
	if err != nil {
		return SharedRef{}, err
	}
	if len(payload) > int(ref.Length) {
		arenaFree(ref)
		return SharedRef{}, fmt.Errorf("扩容块容量不足（需 %d，得 %d）", len(payload), ref.Length)
	}
	copy(region[ref.Offset:ref.Offset+uint32(len(payload))], payload)
	ref.Length = uint32(len(payload))
	ref.Flags |= sharedRefFlagExpand
	if jsonFlag {
		ref.Flags |= sharedRefFlagJSON
	}
	return ref, nil
}

// ---- 共享槽池的插件侧接口（内核 RPC，内部实现）----

// SharedRef 是内核下发的共享内存描述符。
//
// 这是**内部实现**：真实插件不直接触碰它，模板运行时在传输层自动使用。
// 本测试插件手写 RPC，所以需要显式声明。
type SharedRef struct {
	Offset     uint32 `json:"offset"`
	Length     uint32 `json:"length"`
	Generation uint32 `json:"generation"`
	Flags      uint32 `json:"flags"`
}

func (r SharedRef) IsZero() bool { return r.Offset == 0 && r.Length == 0 }

func (r SharedRef) Slice(data []byte) []byte {
	if r.IsZero() || int(r.Offset)+int(r.Length) > len(data) {
		return nil
	}
	return data[r.Offset : r.Offset+r.Length]
}

// SharedRef.Flags 语义位（须与内核 internal/plugin/proc/arena.go 一致）。
const (
	sharedRefFlagJSON   = 1 << 0 // 载荷是 JSON
	sharedRefFlagExpand = 1 << 1 // 引用指向插件申请的扩容块
)

// arenaAlloc 向内核申请一块共享内存，内核返回偏移与大小。
func arenaAlloc(size uint32) (SharedRef, error) {
	raw := callKernel("arena.alloc", map[string]interface{}{"size": size})
	var r struct {
		Ref SharedRef `json:"ref"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return SharedRef{}, err
	}
	if r.Ref.IsZero() {
		return SharedRef{}, fmt.Errorf("内核返回空引用")
	}
	return r.Ref, nil
}

// arenaFree 通知内核回收先前申请的共享内存。
func arenaFree(ref SharedRef) {
	callKernel("arena.free", map[string]interface{}{"ref": ref})
}

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

// ---- 共享段访问（与内核 proc 包的布局一致）----

const (
	headerSize = 64
	// 与 proc 包保持一致：18 个字段 × 8 字节 + 8 字节标志位
	stageFieldCount = 18
	sliceSize       = 8
	flagCount       = 8
	ctxSize         = stageFieldCount*sliceSize + flagCount

	offArenaBase = 8
	offArenaCap  = 12
	offArenaUsed = 16
	offCtxBase   = 20
	offSeq       = 24

	// 字段索引（与 proc/shmcodec.go 的 stageField 枚举顺序一致）
	fToolResults = 10
)

func arenaBase() uint32 { return binary.LittleEndian.Uint32(shm[offArenaBase:]) }
func arenaCap() uint32  { return binary.LittleEndian.Uint32(shm[offArenaCap:]) }
func ctxBase() uint32   { return binary.LittleEndian.Uint32(shm[offCtxBase:]) }

func descOffset(field int) uint32 { return ctxBase() + uint32(field*sliceSize) }

func getDesc(field int) (off, ln uint32) {
	o := descOffset(field)
	return binary.LittleEndian.Uint32(shm[o:]), binary.LittleEndian.Uint32(shm[o+4:])
}

func setDesc(field int, off, ln uint32) {
	o := descOffset(field)
	binary.LittleEndian.PutUint32(shm[o:], off)
	binary.LittleEndian.PutUint32(shm[o+4:], ln)
}

func readField(field int) []byte {
	off, ln := getDesc(field)
	if off == 0 && ln == 0 {
		return nil
	}
	if ln == 0 {
		return []byte{}
	}
	base := arenaBase()
	return shm[base+off : base+off+ln]
}

func writeField(field int, data []byte) error {
	used := binary.LittleEndian.Uint32(shm[offArenaUsed:])
	if used == 0 {
		used = 1
	}
	end := used + uint32(len(data))
	if end > arenaCap() {
		return fmt.Errorf("arena 空间不足")
	}
	base := arenaBase()
	copy(shm[base+used:], data)
	binary.LittleEndian.PutUint32(shm[offArenaUsed:], end)
	setDesc(field, used, uint32(len(data)))
	return nil
}

func bumpSeq() {
	v := binary.LittleEndian.Uint64(shm[offSeq:])
	binary.LittleEndian.PutUint64(shm[offSeq:], v+1)
}

type toolResult struct {
	CallID  string      `json:"call_id"`
	Name    string      `json:"name"`
	Plugin  string      `json:"plugin,omitempty"`
	Success bool        `json:"success"`
	Result  interface{} `json:"result"`
}

// handleStage 模拟 sanitizer：拿锁 → 读 ToolResults → 剥 ANSI → 只写脏字段 → 放锁
func handleStage() (int, error) {
	callKernel("stage.lock", nil)
	defer callKernel("stage.unlock", nil)

	raw := readField(fToolResults)
	if len(raw) == 0 {
		return 0, nil
	}
	var results []toolResult
	if err := json.Unmarshal(raw, &results); err != nil {
		return 0, err
	}

	before := string(raw)
	for i := range results {
		if s, ok := results[i].Result.(string); ok {
			results[i].Result = strings.NewReplacer("\x1b[31m", "", "\x1b[0m", "").Replace(s)
		}
	}
	after, _ := json.Marshal(results)

	// 只有真变了才写回 —— 这是消除 lost update 的核心
	if string(after) == before {
		return 0, nil
	}
	if err := writeField(fToolResults, after); err != nil {
		return 0, err
	}
	bumpSeq()
	return 1, nil
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
			var hp struct {
				ShmSize int `json:"shm_size"`
			}
			json.Unmarshal(req.Params, &hp)
			shmSize = hp.ShmSize
			if shmSize > 0 {
				// fd 3 = 内核传入的统一共享内存区域
				m, err := syscall.Mmap(3, 0, shmSize,
					syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					send(response{ID: req.ID, Error: fmt.Sprintf("mmap 共享段失败: %v", err)})
					continue
				}
				// 统一区域：前 64B 是 SuperBlock，StageContext 段在其后
				ctxOff := binary.LittleEndian.Uint32(m[20:])
				ctxSize := binary.LittleEndian.Uint32(m[24:])
				region = m
				arenaOff = binary.LittleEndian.Uint32(m[36:])
				shm = m[ctxOff : ctxOff+ctxSize]
			}
			send(response{ID: req.ID, Result: map[string]interface{}{
				"sdk_version": "1.0.0", "plugin_name": "stage", "pid": os.Getpid(),
			}})

		case "plugin.init":
			send(response{ID: req.ID})

		case "plugin.start":
			go func(id uint64) {
				callKernel("lifecycle.autoRestart", map[string]interface{}{"enabled": true})
				callKernel("tool.register", map[string]interface{}{
					"name":        "demo_upper",
					"def":         map[string]interface{}{"name": "demo_upper", "description": "转大写"},
					"has_cleaner": true,
				})
				callKernel("tool.register", map[string]interface{}{
					"name":        "demo_inject",
					"def":         map[string]interface{}{"name": "demo_inject", "description": "共享内存注入"},
					"has_cleaner": false,
				})
				callKernel("tool.register", map[string]interface{}{
					"name":        "demo_big",
					"def":         map[string]interface{}{"name": "demo_big", "description": "报告参数量及结果回传路径"},
					"has_cleaner": false,
				})
				callKernel("stage.register", map[string]interface{}{
					"stage": "after_toolcall",
					"scope": "global",
				})
				callKernel("input.register", map[string]interface{}{
					"name": "demo_in", "def": map[string]interface{}{}, "has_cleaner": true,
				})
				callKernel("output.register", map[string]interface{}{
					"name": "demo_ch", "caps": 1, "desc": "测试通道",
					"def": map[string]interface{}{}, "has_cleaner": true,
				})
				send(response{ID: id})
			}(req.ID)

		case "plugin.stop":
			send(response{ID: req.ID})
			out.Flush()
			os.Exit(0)

		case "tool.invoke":
			// 必须在独立 goroutine 里处理：demo_inject 会在处理过程中反向
			// 调用内核（arena.alloc / io.injectText / arena.free），而内核的
			// 应答只能由主读循环接收。若同步处理就会自锁死——这也是真实
			// 模板对每个内核请求都 `go handleKernelRequest` 的原因。
			go func(id uint64, raw json.RawMessage) {
				var p struct {
					Name    string                 `json:"name"`
					Args    map[string]interface{} `json:"args"`
					Frame   SharedRef              `json:"frame"`
					ArgsLen uint32                 `json:"args_len"`
				}
				json.Unmarshal(raw, &p)

				// 参数：内核标定帧的前段。只有直连 RPC 的调用方（无帧）才走
				// 内联 Args——生产路径永远走帧。
				args := p.Args
				argsFrom := "inline"
				if !p.Frame.IsZero() {
					argsFrom = "shared"
					if blob := frameInput(p.Frame, p.ArgsLen); len(blob) > 0 {
						var decoded map[string]interface{}
						if err := json.Unmarshal(blob, &decoded); err != nil {
							send(response{ID: id, Error: "解析共享参数: " + err.Error()})
							return
						}
						args = decoded
					}
				}
				text, _ := args["text"].(string)

				// demo_inject 走插件侧共享内存路径：
				// 申请 → 写入 → 随业务 RPC 回传 → 归还。
				if p.Name == "demo_inject" {
					ref, err := arenaAlloc(uint32(len(text)))
					if err != nil {
						send(response{ID: id, Error: err.Error()})
						return
					}
					copy(region[ref.Offset:ref.Offset+uint32(len(text))], text)
					ref.Length = uint32(len(text))
					// 注入标志位随参数一起过 RPC：内核据此决定这次注入是否
					// 记入记忆、是否据此裁剪上下文、用哪个已注册 cleaner 清洗。
					callKernel("io.injectText", map[string]interface{}{
						"source": "plugin", "channel": "demo", "text_ref": ref,
						"no_memory":      true,
						"context_policy": "prune",
						"cleaner_name":   "demo_cleaner",
					})
					arenaFree(ref)

					blob, _ := json.Marshal("injected")
					outRef, err := frameOutput(p.Frame, p.ArgsLen, blob, true)
					if err != nil {
						outRef, err = arenaPut(blob, true)
						if err != nil {
							send(response{ID: id, Error: "结果扩容失败: " + err.Error()})
							return
						}
					}
					send(response{ID: id, Result: map[string]interface{}{"result_ref": outRef}})
					return
				}

				var out interface{}
				if p.Name == "demo_big" {
					// 把参数来路编进结果，让测试能验证真的走了共享内存而非只看返回值。
					out = argsFrom + ":" + strings.ToUpper(text)
				} else {
					out = strings.ToUpper(text)
				}

				blob, err := json.Marshal(out)
				if err != nil {
					send(response{ID: id, Error: "序列化结果失败: " + err.Error()})
					return
				}
				// 结果优先写进内核标定的帧；放不下才申请扩容块。
				ref, err := frameOutput(p.Frame, p.ArgsLen, blob, true)
				if err != nil {
					ref, err = arenaPut(blob, true)
					if err != nil {
						send(response{ID: id, Error: "结果扩容失败: " + err.Error()})
						return
					}
				}
				send(response{ID: id, Result: map[string]interface{}{"result_ref": ref}})
			}(req.ID, req.Params)

		case "cleaner.invoke":
			var p struct {
				Scope    string    `json:"scope"`
				Name     string    `json:"name"`
				Frame    SharedRef `json:"frame"`
				InputLen uint32    `json:"input_len"`
			}
			json.Unmarshal(req.Params, &p)
			valid := (p.Scope == "tool" && p.Name == "demo_upper") ||
				(p.Scope == "input" && p.Name == "demo_in") ||
				(p.Scope == "output" && p.Name == "demo_ch")
			if !valid {
				send(response{ID: req.ID, Error: "未注册 Cleaner"})
				continue
			}

			input := string(frameInput(p.Frame, p.InputLen))
			output := p.Scope + "-cleaned:" + input

			// 结果优先写进内核标定的帧；放不下才申请扩容块。
			ref, err := frameOutput(p.Frame, p.InputLen, []byte(output), false)
			if err != nil {
				ref, err = arenaPut([]byte(output), false)
				if err != nil {
					send(response{ID: req.ID, Error: "结果扩容失败: " + err.Error()})
					continue
				}
			}
			send(response{ID: req.ID, Result: map[string]interface{}{"text_ref": ref}})

		case "stage.invoke":
			go func(id uint64) {
				if shm == nil {
					send(response{ID: id, Error: "共享段未挂载"})
					return
				}
				n, err := handleStage()
				if err != nil {
					send(response{ID: id, Error: err.Error()})
					return
				}
				send(response{ID: id, Result: map[string]interface{}{"dirty_fields": n}})
			}(req.ID)

		case "output.invoke":
			// §13.6：payload 走共享内存调用帧，与 tool.invoke 同一模型。
			// 必须在独立 goroutine 里处理：插件可能反向调用内核。
			go func(id uint64, raw json.RawMessage) {
				var p struct {
					Channel string                 `json:"channel"`
					Args    map[string]interface{} `json:"args"`
					Frame   SharedRef              `json:"frame"`
					ArgsLen uint32                 `json:"args_len"`
				}
				json.Unmarshal(raw, &p)

				args := p.Args
				if !p.Frame.IsZero() {
					if blob := frameInput(p.Frame, p.ArgsLen); len(blob) > 0 {
						var decoded map[string]interface{}
						if err := json.Unmarshal(blob, &decoded); err != nil {
							send(response{ID: id, Error: "解析输出参数: " + err.Error()})
							return
						}
						args = decoded
					}
				}
				payload, _ := args["payload"].(string)
				if payload == "fail" {
					// 模拟现网 qq 插件的真实失败：meta 缺 user_id
					send(response{ID: id, Error: "meta 中缺少 user_id 字段"})
					return
				}
				// 标量结果直接在应答字段返回（output 应答很小，无需走共享内存）。
				// payload_len 让测试能验证共享帧把完整 payload 带到了插件侧。
				send(response{ID: id, Result: map[string]interface{}{
					"status": "sent", "payload_len": len(payload),
				}})
			}(req.ID, req.Params)

		default:
			if req.ID != 0 {
				send(response{ID: req.ID})
			}
		}
	}
}
