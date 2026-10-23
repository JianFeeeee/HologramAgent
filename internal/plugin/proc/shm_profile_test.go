package proc

// shm_profile_test.go —— 共享内存数据面的**成本分解**基准（纯测量，不改实现）。
//
// ============================ 为什么要这个文件 ============================
// 「共享内存该用 C 实现」是个直觉，本文件负责把它变成数据。
//
// 端到端（跨进程）工具调用往返实测 35.9µs（inline/small），
// 而 BenchmarkSegmentWriteAllReadInto（纯编解码）3.1µs —— 差 ~9%。
// 但那 3.1µs **不是同质的**：里面混着三类成本，只有分开测才知道
// 哪一类是 C 的甜区、哪一类根本不该 C 化：
//
//	① 段内字节搬运（copy / string(b)）      —— C 的甜区（memcpy）
//	② **json.Marshal / Unmarshal**          —— 反射，C 无优势（且难保证逐值一致）
//	③ 描述符/游标记账（小字段多、极频繁）   —— 固定开销，非内存带宽
//
// 判据：若②占大头，则 C 化整条编解码**不划算**（跨语言重建 JSON 语义
// 的成本远高于省下的 memcpy）—— 这正是 ha_json_scan 那三刀学到的事。

import (
	"encoding/json"
	"strings"
	"testing"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// profileCtx 构造一组「接近真实」的 StageContext。
func profileCtx(toolResults, ctxMsgs int) *pubsdk.StageContext {
	sc := &pubsdk.StageContext{
		Phase:      pubsdk.StageAfterToolcall,
		RawMessage: strings.Repeat("用户输入的一段话。", 8),
		UserID:     "u1",
		LLMText:    strings.Repeat("模型输出的文本内容。", 16),
		FinalText:  strings.Repeat("最终给用户的回答。", 4),
	}
	for i := 0; i < toolResults; i++ {
		sc.ToolResults = append(sc.ToolResults, pubsdk.ToolResult{
			CallID: "call_" + strings.Repeat("x", 8),
			Name:   "tool_name_" + string(rune('a'+i%26)),
			Result: strings.Repeat("工具返回的结果内容。", 6),
		})
	}
	for i := 0; i < ctxMsgs; i++ {
		sc.ContextMsgs = append(sc.ContextMsgs, map[string]interface{}{
			"role":    "assistant",
			"content": strings.Repeat("历史消息内容。", 6),
		})
	}
	return sc
}

// ---------------------------------------------------------------------------
// ① 整体：write + read + compact（对齐现有 BenchmarkSegmentWriteAllReadInto）
// ---------------------------------------------------------------------------

func BenchmarkShm_Whole(b *testing.B) {
	for _, n := range []int{0, 2, 8} {
		sc := profileCtx(n, n)
		host, err := NewHost()
		if err != nil {
			b.Fatalf("NewHost: %v", err)
		}
		seg := host.Segment()
		b.Run(sizeName(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := seg.WriteAll(sc); err != nil {
					b.Fatal(err)
				}
				if err := seg.ReadInto(sc); err != nil {
					b.Fatal(err)
				}
				seg.Compact()
			}
		})
		host.Close()
	}
}

func sizeName(n int) string {
	switch n {
	case 0:
		return "empty"
	case 2:
		return "small"
	default:
		return "large"
	}
}

// ---------------------------------------------------------------------------
// ② 只测 JSON 编解码（②类成本：Marshal + Unmarshal）
// ---------------------------------------------------------------------------

func BenchmarkShm_JsonOnly(b *testing.B) {
	for _, n := range []int{0, 2, 8} {
		sc := profileCtx(n, n)
		host, err := NewHost()
		if err != nil {
			b.Fatalf("NewHost: %v", err)
		}
		seg := host.Segment()
		// 先落段，得到真实的 JSON 字节
		if err := seg.WriteAll(sc); err != nil {
			b.Fatal(err)
		}
		var blobs [][]byte
		for _, f := range []stageField{fToolCalls, fToolResults, fContextMsgs} {
			bl, err := seg.read(seg.getDesc(f))
			if err != nil {
				b.Fatal(err)
			}
			if len(bl) > 0 {
				cp := make([]byte, len(bl))
				copy(cp, bl)
				blobs = append(blobs, cp)
			}
		}
		var tcs []pubsdk.ToolCall
		var trs []pubsdk.ToolResult
		var cms []map[string]interface{}
		b.Run(sizeName(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				for j, bl := range blobs {
					switch j % 3 {
					case 0:
						_ = json.Unmarshal(bl, &tcs)
					case 1:
						_ = json.Unmarshal(bl, &trs)
					default:
						_ = json.Unmarshal(bl, &cms)
					}
				}
			}
		})
		// Marshal 侧
		b.Run(sizeName(n)+"/marshal", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = json.Marshal(tcs)
				_, _ = json.Marshal(trs)
				_, _ = json.Marshal(cms)
			}
		})
		host.Close()
	}
}

// ---------------------------------------------------------------------------
// ③ 只测段内字节搬运（①类成本：copy / string(b)）—— C 的甜区
// ---------------------------------------------------------------------------

func BenchmarkShm_ByteCopyOnly(b *testing.B) {
	host, err := NewHost()
	if err != nil {
		b.Fatalf("NewHost: %v", err)
	}
	defer host.Close()
	seg := host.Segment()
	seg.Compact()

	sizes := []int{0, 64, 1024, 16384, 131072}
	for _, sz := range sizes {
		src := make([]byte, sz)
		for i := range src {
			src[i] = byte('a' + i%26)
		}
		b.Run(sizeName2(sz), func(b *testing.B) {
			b.SetBytes(int64(sz))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				off, err := seg.alloc(sz)
				if err != nil {
					seg.Compact()
					off, err = seg.alloc(sz)
					if err != nil {
						b.Fatal(err)
					}
				}
				base := seg.arenaBase()
				copy(seg.data[base+off:base+off+uint32(sz)], src)
			}
		})
	}
}

func sizeName2(n int) string {
	switch {
	case n == 0:
		return "0B"
	case n < 1024:
		return itoa(n) + "B"
	default:
		return itoa(n/1024) + "KB"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// ---------------------------------------------------------------------------
// ④ 只测描述符记账（③类成本：18 个 Slice 描述符的 get/set）
// ---------------------------------------------------------------------------

func BenchmarkShm_DescOnly(b *testing.B) {
	host, err := NewHost()
	if err != nil {
		b.Fatalf("NewHost: %v", err)
	}
	defer host.Close()
	seg := host.Segment()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for f := stageField(0); f < stageFieldCount; f++ {
			sl := seg.getDesc(f)
			seg.setDesc(f, sl)
		}
	}
}

// ---------------------------------------------------------------------------
// ⑤ write / read 分离，并给出「C 化三类成本各自的天花板」
// ---------------------------------------------------------------------------

func BenchmarkShm_Split(b *testing.B) {
	for _, n := range []int{0, 2, 8} {
		sc := profileCtx(n, n)
		host, err := NewHost()
		if err != nil {
			b.Fatalf("NewHost: %v", err)
		}
		seg := host.Segment()

		b.Run(sizeName(n)+"/write", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := seg.WriteAll(sc); err != nil {
					b.Fatal(err)
				}
				seg.Compact()
			}
		})
		if err := seg.WriteAll(sc); err != nil {
			b.Fatal(err)
		}
		b.Run(sizeName(n)+"/read", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := seg.ReadInto(sc); err != nil {
					b.Fatal(err)
				}
			}
		})
		host.Close()
	}
}
