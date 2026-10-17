//go:build cgo

package api

// codec_streamchunk_c.go —— SSE 分块解析的 C 化「结构导航」层（Go 侧绑定）
//
// ============================ 为什么是「导航」而不是「全量编解码」 ============================
// 接线前实测出两条 wire 语义（docs/zh/c-core/sse-codec-c.md §5），它们让
// 「整条 parseOpenAICompatibleStreamChunkFull 全 C 化」不成立：
//
//	§5.1 重复键是**字段级合并**：`{"choices":[{content:a}],"choices":[{reasoning:r}]}`
//	     → content="a" **且** reasoning="r"。json.Unmarshal 的 object() 收尾时做
//	     `v.SetIndex(i, subv.v)`，而 subv 拿到的是**已存在元素的指针**，
//	     所以第二次是叠加而非替换。正确实现要维护「本次哪些字段出现过」的表。
//	§5.2 stringifyContent 的 default 分支 = `json.Marshal(interface{})`，
//	     即**重新序列化**：`{"b":1,"a":2}` → `{"a":2,"b":1}`（键排序）、
//	     `1e2` → `100`、`<` → `\u003c`、大 int 先舍入成 float64。
//	     逐值一致要求复刻 Ryu 最短浮点 + map 键排序 + HTML 转义 + int 舍入。
//
// 而这两条**只在取值阶段**才需要。故本层只做**结构导航**：
//
//	C：把 JSON 定位到「哪个值在哪里」——零分配、零解码，并直接给出两个热分支的结果
//	Go：把「已定位的原始字节」按既有类型 unmarshal，成串逻辑完全不变
//
// ⇒ 类型检查的等价性靠「用**相同的 Go 类型** unmarshal **相同形状的子树**」保证，
// 而不靠 C 重新实现一遍类型规则。这是本设计同时拿到速度与正确性的关键。
//
// 代价如实记录：命中字段仍要一次小 Unmarshal（原来是对整块做）。收益是免除
// json.Unmarshal 对整块的**反射建树**——那正是每块 12~21 allocs 的主因。
//
// ★ 键匹配**大小写敏感**（与 ha_json_scan.h 的 ha_json_key_eq 相反，两者用途不同）
//   `content` 是 map[string]interface{}，取 `m["text"]` 走 map key 语义
//   ⇒ 大小写敏感。实测 `{"TEXT":"up"}` 取不到 `text`。
//   struct 字段（choices/delta/usage）是大小写**不**敏感 —— 那一跳交给
//   encoding/json，天然正确。
//
// ★ C 实现放在 csrc/ha_sse.c 而**不是**本文件的 cgo 前言里：
//   前言里的 C 代码会逃出全部 C 门禁（告警 / ASan+UBSan / arm64 交叉 / 模糊测试），
//   而这里恰恰是本刀最容易出错的位置。这是结构性决定，不是形式主义。

/*
#cgo CFLAGS: -std=c99
#include <stdlib.h>
#include "ha_sse.h"

// C 结构体一律不跨越语言边界（cgo 禁止「Go 指针指向的 Go 指针」，
// 实测会 panic），故所有 span 传递都拆成 (指针, 长度) 标量。
static int go_obj_find(const char *p, size_t n, const char *key, int keylen,
                       char **vp, size_t *vlen, int *dup) {
    ha_span obj, out;
    obj.p = p; obj.len = n;
    int rc = ha_sse_obj_find(&obj, key, (size_t)keylen, &out, dup);
    if (rc == 1) { *vp = (char *)out.p; *vlen = out.len; }
    return rc;
}

static int go_arr_first(const char *p, size_t n, char **vp, size_t *vlen) {
    ha_span arr, out;
    arr.p = p; arr.len = n;
    int rc = ha_sse_arr_first(&arr, &out);
    if (rc == 1) { *vp = (char *)out.p; *vlen = out.len; }
    return rc;
}

static int go_stringify(const char *p, size_t n, char *out, size_t cap,
                        size_t *outlen) {
    ha_span val;
    val.p = p; val.len = n;
    return ha_sse_stringify(&val, out, cap, outlen);
}

static int go_arg_string(const char *p, size_t n, char *out, size_t cap,
                         size_t *outlen) {
    ha_span val;
    val.p = p; val.len = n;
    return ha_sse_arg_string(&val, out, cap, outlen);
}

static int go_obj_find_ci(const char *p, size_t n, const char *key, int keylen,
                          char **vp, size_t *vlen, int *dup) {
    ha_span obj, out;
    obj.p = p; obj.len = n;
    int rc = ha_sse_obj_find_ci(&obj, key, (size_t)keylen, &out, dup);
    if (rc == 1) { *vp = (char *)out.p; *vlen = out.len; }
    return rc;
}

static int go_root_object(const char *p, size_t n) {
    ha_span doc;
    doc.p = p; doc.len = n;
    return ha_sse_root_object(&doc);
}

// 把数组全部元素写进 out（Go 侧预分配的 span 数组）。
// 返回元素数；超出 cap 时返回 -1（调用方据此判定「需要更大的缓冲」⇒ 回退）。
static int go_arr_all(const char *p, size_t n, ha_span *out, int cap) {
    ha_json_scan sc;
    int count = 0;
    ha_json_scan_init(&sc, p, n);
    (void)ha_json_scan_ws(&sc);
    if (ha_json_scan_eof(&sc) || sc.s[sc.i] != '[') { return -1; }
    sc.i++;
    for (;;) {
        (void)ha_json_scan_ws(&sc);
        if (ha_json_scan_eof(&sc) || sc.s[sc.i] == ']') { break; }
        if (count >= cap) { return -1; }
        size_t start = sc.i;
        if (!ha_json_skip(&sc)) { return -1; }
        out[count].p = p + start;
        out[count].len = sc.i - start;
        count++;
        (void)ha_json_scan_ws(&sc);
        if (ha_json_scan_eof(&sc)) { return -1; }
        if (sc.s[sc.i] == ',') { sc.i++; continue; }
        if (sc.s[sc.i] == ']') { break; }
        return -1;
    }
    return count;
}

static int go_chunk_locate(const char *p, size_t n, ha_chunk_out *out,
                           char *sbuf, size_t scap, size_t *sused) {
    return ha_sse_chunk_locate(p, n, out, sbuf, scap, sused);
}

static int go_sse_abi(void) { return ha_sse_abi_version(); }
*/
import "C"

import "unsafe"

// ---------------------------------------------------------------------
// span 表示
// ---------------------------------------------------------------------

// strSpan 是 JSON 里一段字节，指向**原缓冲**（零拷贝）。
type strSpan struct {
	p *C.char
	n C.size_t
}

func (s strSpan) valid() bool { return s.p != nil && s.n > 0 }

// bytes 把 span 变成 Go 字节切片（此处才产生一次拷贝）。
//
// ★ 用途：把「已定位的原始子树」交给 json.Unmarshal —— 用同一 Go 类型
//
//	unmarshal 同一形状，是本层保证「类型检查语义与原实现一致」的手段。
func (s strSpan) bytes() []byte {
	if s.p == nil || s.n == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(s.p)), int(s.n))
}

// str 把 span 变成 Go 字符串（此处才产生一次拷贝）。
func (s strSpan) str() string {
	if s.p == nil || s.n == 0 {
		return ""
	}
	return string(unsafe.Slice((*byte)(unsafe.Pointer(s.p)), int(s.n)))
}

// firstByte 只看首字节，用于区分值类型。
func (s strSpan) firstByte() byte {
	if s.p == nil || s.n == 0 {
		return 0
	}
	return *(*byte)(unsafe.Pointer(s.p))
}

// ---------------------------------------------------------------------
// 定位
// ---------------------------------------------------------------------

// ---------------------------------------------------------------------
// 取值（C 可判定的热分支）
// ---------------------------------------------------------------------

// decBuf 是解码/反转义用的可写缓冲。
//
// ★ 尺寸必须按输入长度定：C 侧要求 cap >= len*3+4（最坏每字节一个 U+FFFD），
//
//	不足时它会返回 0 让调用方回退 Go（宁可慢也不截断）。
//	每次调用 1 次分配（原来整块 Unmarshal 是 12~21 次）—— 这是主要的节省点。
func decBuf(n int) []byte { return make([]byte, n*3+8) }

// stringifyC 对应 Go stringifyContent 的**C 可判定分支**
// （字符串值 / 文本数组），返回 (结果, handled)。
// handled=false ⇒ 值类型需要 json.Marshal 重新编码（§5.2），调用方须回退 Go。
func stringifyC(val strSpan) (string, bool) {
	if !val.valid() {
		// 缺失 / 空 ⇒ Go 侧 stringifyContent(nil) 也是 ""
		return "", true
	}
	buf := decBuf(int(val.n))
	var outLen C.size_t
	if C.go_stringify(val.p, val.n, cstrb(buf), C.size_t(len(buf)), &outLen) != 1 {
		return "", false
	}
	return string(buf[:int(outLen)]), true
}

// ---------------------------------------------------------------------
// 顶层 helper：大小写不敏感（struct 字段语义）与根对象校验
// ---------------------------------------------------------------------

// C_size 把 Go int 转成 C.size_t（零拷贝 span 的长度）。
func C_size(n int) C.size_t { return C.size_t(n) }

// rootSpan 构造指向 data 的 span（零拷贝）。
func rootSpan(data string) strSpan { return strSpan{cstrp(data), C_size(len(data))} }

// findKeyCI 按**大小写不敏感**定位（Go struct 字段语义）。
func findKeyCI(obj strSpan, name string) (strSpan, bool, bool, bool) {
	return findKeyGeneric(obj, name, true)
}

// findKeyCS 按**大小写敏感**定位（Go map key 语义）。
func findKeyCS(obj strSpan, name string) (strSpan, bool, bool, bool) {
	return findKeyGeneric(obj, name, false)
}

func findKeyGeneric(obj strSpan, name string, ci bool) (strSpan, bool, bool, bool) {
	if !obj.valid() {
		return strSpan{}, false, false, false
	}
	keyp, keyn := cstr(name)
	var vp *C.char
	var vlen C.size_t
	var dup C.int
	var rc C.int
	if ci {
		rc = C.go_obj_find_ci(obj.p, obj.n, keyp, C.int(keyn), &vp, &vlen, &dup)
	} else {
		rc = C.go_obj_find(obj.p, obj.n, keyp, C.int(keyn), &vp, &vlen, &dup)
	}
	switch rc {
	case 1:
		return strSpan{vp, vlen}, true, dup == 1, false
	case 0:
		return strSpan{}, false, dup == 1, false
	default:
		return strSpan{}, false, false, true
	}
}

// sseRootObject 校验「恰好一个良构对象」（含尾部残留检查）。
func sseRootObject(doc strSpan) bool {
	if !doc.valid() {
		return false
	}
	return C.go_root_object(doc.p, doc.n) == 1
}

// ---------------------------------------------------------------------
// 批量定位（第三刀的重做：一次 cgo 调用代替 5+ 次）
// ---------------------------------------------------------------------

// chunkLocateResult 是 C 侧 ha_chunk_out 的 Go 视图。
type chunkLocateResult struct {
	status int

	// 原始 span（用于交回 encoding/json 的那些字段）
	usageSpan     strSpan
	usageKind     int
	toolCallsSpan strSpan
	toolCallsKind int

	// C 已解码的字符串（sbuf 的副本）
	content   string
	reasoning string
	finish    string

	// 标志
	choicesPresent bool
	choicesKind    int
	choicesCount   int
	choice0Span    strSpan
	hasDelta       bool
	deltaKind      int
	contentKind    int
	reasoningKind  int
	finishKind     int
}

// 槽位/类型常量（与 ha_sse.h 保持一致；改动必须同步 ABI 版本）
const (
	slotDelta        = 0
	slotContent      = 1
	slotReasoning    = 2
	slotToolCalls    = 3
	slotFinishReason = 4
	slotUsage        = 5
	slotCount        = 6

	kindAbsent = 0
	kindNull   = 1
	kindString = 2
	kindObject = 3
	kindArray  = 4
	kindOther  = 5

	chunkOK       = 0
	chunkFallback = -1
	chunkTypeFail = -2
)

// locateChunkBatch 一次调用完成整块定位。
func locateChunkBatch(data string) chunkLocateResult {
	var out chunkLocateResult
	if len(data) == 0 {
		out.status = chunkFallback
		return out
	}
	p, n := cstr(data)

	var co C.ha_chunk_out
	// ★ 单块缓冲：整块解码输出（content+reasoning+finish）都写这一块。
	//   尺寸按输入上界（每字节最坏 3 字节 U+FFFD）——1 次分配，
	//   替代原来「每个字段一次 decBuf」的多次分配。
	sbuf := make([]byte, len(data)*3+16)
	var used C.size_t

	st := C.go_chunk_locate(p, n, &co, cstrb(sbuf), C.size_t(len(sbuf)), &used)
	out.status = int(st)
	if st != C.int(chunkOK) {
		return out
	}

	out.usageKind = int(co.slot[slotUsage].kind)
	out.usageSpan = strSpan{co.slot[slotUsage].span.p, co.slot[slotUsage].span.len}
	out.toolCallsKind = int(co.slot[slotToolCalls].kind)
	out.toolCallsSpan = strSpan{co.slot[slotToolCalls].span.p, co.slot[slotToolCalls].span.len}
	out.contentKind = int(co.slot[slotContent].kind)
	out.reasoningKind = int(co.slot[slotReasoning].kind)
	out.finishKind = int(co.slot[slotFinishReason].kind)
	out.hasDelta = int(co.slot[slotDelta].kind) == kindObject
	out.deltaKind = int(co.slot[slotDelta].kind)
	out.choicesPresent = co.has_choices == 1
	out.choicesKind = int(co.choices_kind)
	out.choicesCount = int(co.choices_count)

	s := sbuf[:int(used)]
	out.content = string(s[co.content_off : co.content_off+co.content_len])
	out.reasoning = string(s[co.reasoning_off : co.reasoning_off+co.reasoning_len])
	out.finish = string(s[co.finish_off : co.finish_off+co.finish_len])
	return out
}
