//go:build cgo

package api

// codec_chunkfast_golden_test.go —— C 快速路径 vs 原 Go 实现的**逐值差分对照**。
//
// ============================ 这是接线的唯一验收 ============================
// 快速路径是**纯优化**：它与 chunkParseGo 必须在所有输入上等价。
// 而这条等价性不能靠「读代码觉得对」——本刀前面已经有 7 个「读三遍都认为对」
// 的 C 缺陷。故这里用**差分测试**：同一批输入，两条路径，逐字段比对。
//
// 输入来源三类：
//  ① 手工枚举的协议形态（含全部回退触发条件）
//  ② 真实负载形状（content / toolcall / usage 块）
//  ③ 随机 JSON（用 encoding/json 生成合法值再编码，覆盖嵌套与转义）
//
// 比对字段：整个 StreamChunk（Content / ReasoningContent / Done /
// FinishReason / ToolCalls / Usage）与 bool 返回值。

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// diffChunk 逐字段比对两个结果，不同则报告首个差异点。
func diffChunk(t *testing.T, in string, fastCK StreamChunk, fastOK bool, goCK StreamChunk, goOK bool) {
	t.Helper()
	if fastOK != goOK {
		t.Errorf("返回 bool 分歧 in=%q: fast=%v go=%v", in, fastOK, goOK)
		return
	}
	if !fastOK {
		return
	}
	if fastCK.Content != goCK.Content {
		t.Errorf("Content 分歧 in=%q:\n  fast=%q\n  go  =%q", in, fastCK.Content, goCK.Content)
	}
	if fastCK.ReasoningContent != goCK.ReasoningContent {
		t.Errorf("ReasoningContent 分歧 in=%q:\n  fast=%q\n  go  =%q",
			in, fastCK.ReasoningContent, goCK.ReasoningContent)
	}
	if fastCK.Done != goCK.Done || fastCK.FinishReason != goCK.FinishReason {
		t.Errorf("Done/FinishReason 分歧 in=%q: fast=(%v,%q) go=(%v,%q)",
			in, fastCK.Done, fastCK.FinishReason, goCK.Done, goCK.FinishReason)
	}
	if !reflect.DeepEqual(fastCK.Usage, goCK.Usage) {
		t.Errorf("Usage 分歧 in=%q:\n  fast=%+v\n  go  =%+v", in, fastCK.Usage, goCK.Usage)
	}
	if len(fastCK.ToolCalls) != len(goCK.ToolCalls) {
		t.Errorf("ToolCalls 数量分歧 in=%q: fast=%d go=%d",
			in, len(fastCK.ToolCalls), len(goCK.ToolCalls))
	} else {
		for i := range fastCK.ToolCalls {
			if !reflect.DeepEqual(fastCK.ToolCalls[i], goCK.ToolCalls[i]) {
				t.Errorf("ToolCalls[%d] 分歧 in=%q:\n  fast=%+v\n  go  =%+v",
					i, in, fastCK.ToolCalls[i], goCK.ToolCalls[i])
			}
		}
	}
}

func checkPair(t *testing.T, in string) {
	t.Helper()
	fastCK, fastOK, _ := chunkParseFast(in)
	if !fastCK.Done && !fastOK {
		// handled=false ⇒ 走 Go。这里要区分「回退」与「快速路径给出失败」：
	}
	// 真实入口（含回退）
	gotCK, gotOK := parseOpenAICompatibleStreamChunkFull(in)
	goCK, goOK := parseOpenAICompatibleStreamChunkFullGo(in)
	diffChunk(t, in, gotCK, gotOK, goCK, goOK)
}

// ---------------------------------------------------------------------
// 1. 手工协议形态（覆盖所有回退触发条件）
// ---------------------------------------------------------------------

func TestChunkFast_ProtocolForms(t *testing.T) {
	cases := []string{
		// —— 真实负载三形态（应走快速路径）——
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"这是一段中文内容。"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"thinking..."},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"stablemem_recall","arguments":"{\"query\":\"x\"}"}}]},"finish_reason":null}]}`,
		`{"id":"c1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`,
		`{"id":"c1","usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`,
		`{"usage":{"prompt":1,"completion":2,"total":3}}`,
		`{"usage":{"prompt_tokens":1}}`,
		`{"usage":{"prompt_cache_hit_tokens":5,"prompt_tokens":1,"total_tokens":2}}`,
		`{"usage":{"prompt_tokens_details":{"cached_tokens":7},"prompt_tokens":1,"total_tokens":2}}`,
		`{}`,
		`{"choices":null}`,
		`{"usage":null}`,
		`{"choices":[{"delta":null}]}`,
		`{"choices":[{"finish_reason":null}]}`,
		`{"choices":[{"finish_reason":""}]}`,
		`{"choices":[{"finish_reason":"length"}]}`,
		// content 的各种界面
		`{"choices":[{"delta":{"content":""}}]}`,
		`{"choices":[{"delta":{"content":null}}]}`,
		`{"choices":[{"delta":{"content":123}}]}`,
		`{"choices":[{"delta":{"content":true}}]}`,
		`{"choices":[{"delta":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}]}`,
		`{"choices":[{"delta":{"content":[]}}]}`,
		`{"choices":[{"delta":{"content":["x",{"text":"y"}]}}]}`,
		`{"choices":[{"delta":{"content":[{"text":123},{"text":"ok"}]}}]}`,
		`{"choices":[{"delta":{"content":"a\"b\\c\nd"}}]}`,
		`{"choices":[{"delta":{"content":"你好😀"}}]}`,
		`{"choices":[{"delta":{"content":"\u4f60\u597d"}}]}`,

		// —— 必须回退 Go 的形态 ——
		// §5.2：对象 content 需 json.Marshal 重新编码（键排序 + HTML 转义）
		`{"choices":[{"delta":{"content":{"b":1,"a":2}}}]}`,
		`{"choices":[{"delta":{"content":{"k":"<a>&b"}}}]}`,
		`{"choices":[{"delta":{"content":{"nested":{"deep":[1,2]}}}}]}`,
		`{"choices":[{"delta":{"content":1e2}}]}`,
		`{"choices":[{"delta":{"content":1.0}}]}`,
		`{"choices":[{"delta":{"content":0.1}}]}`,
		`{"choices":[{"delta":{"content":123456789012345678}}]}`,
		// §5.1：重复键
		`{"choices":[{"delta":{"content":"a"}}],"choices":[{"delta":{"content":"b"}}]}`,
		`{"choices":[{"delta":{"content":"a"}}],"choices":[{"delta":{"reasoning_content":"r"}}]}`,
		`{"usage":{"prompt_tokens":1},"usage":{"completion_tokens":2}}`,
		`{"choices":[{"delta":{"content":{"x":1},"content":"s"}}]}`,
		// 尾部残留
		`{"a":1}{"b":2}`,
		`{"choices":[{"delta":{"content":"x"}}]} trailing`,
		// 类型不符（应两侧都 false）
		`{"choices":{}}`,
		`{"usage":{"prompt_tokens":"1"}}`,
		`{"usage":{"prompt_tokens":1.5}}`,
		`{"usage":{"prompt_cache_hit_tokens":"x","prompt_tokens":1}}`,
		`{"choices":[{"delta":{"reasoning_content":123}}]}`,
		`{"choices":[{"delta":{"content":"x"},"finish_reason":42}]}`,
		`{"choices":[{"delta":{"tool_calls":{}}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1.5,"function":{"name":"f"}}]}}]}`,
		// 键大小写
		`{"CHOICES":[{"DELTA":{"CONTENT":"ci"}}]}`,
		`{"choices":[{"delta":{"content":[{"TEXT":"up"}]}}]}`,
		`{"choices":[{"delta":{"content":[{"text":"low"}]}}]}`,
		`{"CHOICES":[{"DELTA":{"CONTENT":"a"}}],"choices":[{"DELTA":{"CONTENT":"b"}}]}`,
		// 畸形
		``, `{`, `null`, `[]`, `"str"`, `123`, `{"a":}`, `{"a":1,}`,
		`{'a':1}`, `{"a":1 `, `{"choices":[`, `{"choices":[{"delta":`,
	}
	for _, in := range cases {
		checkPair(t, in)
	}
}

// ---------------------------------------------------------------------
// 2. 真实负载形状（从实际网关抓的形态）
// ---------------------------------------------------------------------

func TestChunkFast_Realistic(t *testing.T) {
	cases := []string{
		`{"id":"chatcmpl-abc","object":"chat.completion.chunk","created":1727000000,"model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"这是一段来自真实流式响应的中文内容，用于测量解析开销。"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-abc","object":"chat.completion.chunk","created":1727000000,"model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9a","type":"function","function":{"name":"stablemem_recall","arguments":"{\"query\":\"用户偏好\",\"limit\":20}"}}]},"finish_reason":null}]}`,
		`{"id":"chatcmpl-abc","object":"chat.completion.chunk","created":1727000000,"model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":""},"finish_reason":null}],"usage":{"prompt_tokens":3821,"completion_tokens":117,"total_tokens":3938,"prompt_cache_hit_tokens":3584,"prompt_cache_miss_tokens":237}}`,
		// 流式续传：name 不重发但 function.arguments 继续
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":null}]}`,
		// 扁平形态（顶层 name/arguments）
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"name":"f","arguments":{"a":1}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"f","arguments":null}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"f","arguments":123}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"f","arguments":[1,2]}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1"}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":null},"finish_reason":null}]}`,
		// reasoning 与 content 同时出现
		`{"choices":[{"delta":{"reasoning_content":"r","content":"c"},"finish_reason":null}]}`,
		// 多个 choices（只读 [0]）
		`{"choices":[{"delta":{"content":"first"},"finish_reason":"stop"},{"delta":{"content":"second"}}]}`,
		// 未知字段（应忽略）
		`{"choices":[{"delta":{"content":"x"},"unknown":{"deep":[1,2]}}],"zzz":1}`,
		`{"choices":[{"delta":{"content":"x"},"logprobs":{"tokens":["a"]}}],"system_fingerprint":"fp_1"}`,
	}
	for _, in := range cases {
		checkPair(t, in)
	}
}

// ---------------------------------------------------------------------
// 3. 随机 JSON（合法值 → 编码 → 解析），差分
// ---------------------------------------------------------------------

func randJSONValue(rng *rand.Rand, depth int) interface{} {
	if depth <= 0 {
		switch rng.Intn(6) {
		case 0:
			return nil
		case 1:
			return rng.Intn(1000)
		case 2:
			return rng.Float64() * 100
		case 3:
			return rng.Intn(2) == 0
		default:
			return randomString(rng)
		}
	}
	switch rng.Intn(8) {
	case 0:
		return map[string]interface{}{"a": randJSONValue(rng, depth-1)}
	case 1:
		return []interface{}{randJSONValue(rng, depth-1)}
	case 2:
		return map[string]interface{}{
			"prompt_tokens":           rng.Intn(9999),
			"total_tokens":            rng.Intn(9999),
			"completion":              rng.Intn(999),
			"prompt_cache_hit_tokens": rng.Intn(10),
		}
	default:
		return randJSONValue(rng, 0)
	}
}

func randomString(rng *rand.Rand) string {
	alphabet := []rune("abc中文😀\"\\\n\t<>äöü")
	n := rng.Intn(12)
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteRune(alphabet[rng.Intn(len(alphabet))])
	}
	return sb.String()
}

func TestChunkFast_RandomJSON(t *testing.T) {
	rng := rand.New(rand.NewSource(20260926))
	for iter := 0; iter < 30000; iter++ {
		// 构造一个「像 SSE chunk」的随机对象
		obj := map[string]interface{}{}
		switch rng.Intn(4) {
		case 0:
			obj["choices"] = []interface{}{map[string]interface{}{
				"index":         rng.Intn(3),
				"delta":         map[string]interface{}{"content": randJSONValue(rng, 2)},
				"finish_reason": []interface{}{nil, "", "stop", "length"}[rng.Intn(4)],
			}}
		case 1:
			obj["choices"] = []interface{}{map[string]interface{}{
				"delta": map[string]interface{}{
					"reasoning_content": randomString(rng),
					"content":           randomString(rng),
				},
			}}
		case 2:
			obj["usage"] = map[string]interface{}{
				"prompt_tokens":     rng.Intn(1000),
				"completion_tokens": rng.Intn(100),
				"total_tokens":      rng.Intn(1000),
			}
		default:
			obj["choices"] = []interface{}{map[string]interface{}{
				"delta": map[string]interface{}{
					"tool_calls": []interface{}{map[string]interface{}{
						"index": rng.Intn(3),
						"id":    randomString(rng),
						"type":  "function",
						"function": map[string]interface{}{
							"name":      randomString(rng),
							"arguments": randJSONValue(rng, 1),
						},
					}},
				},
			}}
		}
		b, err := json.Marshal(obj)
		if err != nil {
			continue
		}
		checkPair(t, string(b))
	}
}

// ---------------------------------------------------------------------
// 4. 随机字节（畸形输入）——两侧都必须拒绝、且不得 panic
// ---------------------------------------------------------------------

func TestChunkFast_RandomBytes(t *testing.T) {
	rng := rand.New(rand.NewSource(777))
	alphabet := []byte(`{}[]",:0123456789tfnul \` + "\n\t\xff\x80")
	for iter := 0; iter < 30000; iter++ {
		n := rng.Intn(60)
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		checkPair(t, string(b))
	}
}

// ---------------------------------------------------------------------
// 5. 快速路径**确实被用到**（否则「优化」是假的）
// ---------------------------------------------------------------------

func TestChunkFast_ActuallyHandlesRealistic(t *testing.T) {
	realistic := []string{
		`{"id":"c","choices":[{"index":0,"delta":{"content":"中文内容"},"finish_reason":null}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"i","type":"function","function":{"name":"n","arguments":"{}"}}]}}]}`,
		`{"id":"c","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
	}
	for _, in := range realistic {
		_, handled, decided := chunkParseFast(in)
		if !handled || !decided {
			t.Errorf("真实负载未走快速路径（优化失效）: %s", in)
		}
	}
	_ = fmt.Sprint()
}
