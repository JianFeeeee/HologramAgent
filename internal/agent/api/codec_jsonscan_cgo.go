//go:build cgo

package api

// codec_jsonscan_cgo.go — ha_json_scan（C）的 cgo 桥接。
//
// ============================ 为什么桥接在非测试文件里 ============================
// Go **不允许在 _test.go 里用 cgo**（实测：use of cgo in test ... not supported）。
// 而 C 侧静态链接函数没有对应的 Go 声明就没法调用 ⇒ 桥接必须落在这里，
// 由 codec_jsongolden_test.go（纯 Go 测试）来验证其语义。
//
// 与 codec_cgo.go 同理：本包是 cgo-only（编解码层已完全 C 化），
// 所以这些桥接函数在 CGO_ENABLED=0 下不存在，而那正是**有意的响亮失败**。

/*
#cgo CFLAGS: -std=c99
#include <stdlib.h>
#include "ha_json_scan.h"

// cgo 编不了 C 宏，这里用一个小 helper 把 C 侧结果取出来。
// span 指向 Go 传进来的原缓冲（零拷贝），Go 侧用 unsafe 读回。
static ha_span go_scan_members(ha_json_members *m, ha_span *key) {
    ha_span val;
    if (!ha_json_members_next(m, key, &val)) {
        ha_span none;
        none.p = NULL;
        none.len = 0;
        return none;
    }
    return val;
}

static int go_members_complete(const ha_json_members *m) {
    return ha_json_members_complete(m);
}

// 严格判定：整串**恰好**是一个 JSON 值（尾部只允许空白）。
//
// ★ 全部逻辑留在 C 侧，故意不让 Go 把 ha_json_scan 结构体传进来：
//   cgo 规则禁止「Go 指针指向的 Go 指针」。把 C 结构体声明成 Go 变量
//   递给 C 时，若该变量因逃逸分析被堆分配，运行时无法证明它不含
//   Go 指针 ⇒ 直接 panic
//   （实测报 cgo argument has Go pointer to unpinned Go pointer）。
//   正确做法是「只传裸指针 + 长度给 C，让 C 自己持有游标」——
//   这也与库本身「零分配、调用方栈上持有」的设计一致。
static int go_skip_strict(const char *s, size_t n) {
    ha_json_scan sc;
    ha_json_scan_init(&sc, s, n);
    if (!ha_json_skip(&sc)) {
        return 0;
    }
    (void)ha_json_scan_ws(&sc);
    return ha_json_scan_eof(&sc);
}

static int go_skip(const char *s, size_t n) {
    ha_json_scan sc;
    ha_json_scan_init(&sc, s, n);
    return ha_json_skip(&sc);
}

static int go_scan_string(const char *s, size_t n, size_t *out_len) {
    ha_json_scan sc;
    ha_json_scan_init(&sc, s, n);
    ha_span raw;
    if (!ha_json_scan_string(&sc, &raw)) {
        return 0;
    }
    *out_len = raw.len;
    return 1;
}

static int go_decode(const char *p, size_t n, char *out, size_t cap, size_t *outlen) {
    ha_span raw;
    raw.p = p;
    raw.len = n;
    size_t k = ha_json_decode_string_into(raw, out, cap);
    if (k == (size_t)-1) {
        return 0;
    }
    *outlen = k;
    return 1;
}

static int go_get_int(const char *p, size_t n, long long *out) {
    ha_span raw;
    raw.p = p;
    raw.len = n;
    return ha_json_get_int(raw, out);
}

static int go_abi(void) { return ha_json_scan_abi_version(); }
*/
import "C"

import "unsafe"

// 供测试调用的 C 侧薄封装（C 的类型无法直接出现在测试文件里）

func cjsSkip(s string) bool {
	p, n := cstr2(s)
	return C.go_skip(p, n) == 1
}

func cjsMembersInit(m *C.ha_json_members, s string) bool {
	p, n := cstr2(s)
	return C.ha_json_members_init(m, p, n) == 1
}

// cjsMembersStep 推进一次迭代，只把**键**交回 Go。
//
// ★ 为什么只返回键：cgo 规则禁止把「Go 指针指向的 Go 指针」传给 C
//
//	（cgo argument has Go pointer to unpinned Go pointer）——若把 key 与
//	value 两个 span 都交回 Go，再在同一个调用里传回 C，就会构成
//	「Go 切片 → Go 指针 → Go 指针」的未固定链，运行时直接 panic。
//	所以每次跨语言只搬运**一个**字符串，其余信息留到下一次调用。
//
// ★ 值 span 只在需要时**在 C 侧**用（见 cjsWalkMembers）。
func cjsMembersStep(m *C.ha_json_members) (key string, ok bool) {
	var ck C.ha_span
	v := C.go_scan_members(m, &ck)
	if v.p == nil {
		return "", false
	}
	return unsafeString(ck.p, int(ck.len)), true
}

func cjsMembersComplete(m *C.ha_json_members) bool {
	return C.go_members_complete(m) == 1
}

func cjsScanString(s string) (string, bool) {
	p, n := cstr2(s)
	var outLen C.size_t
	if C.go_scan_string(p, n, &outLen) == 0 {
		return "", false
	}
	// 去掉两端引号
	if n < 2 {
		return "", false
	}
	return string(s[1 : int(n)-1]), true
}

func cjsDecode(raw string) (string, bool) {
	// 上界：每字节最坏变一个 3 字节 U+FFFD
	buf := make([]byte, len(raw)*3+16)
	var outLen C.size_t
	p := cstrp(raw)
	ok := C.go_decode(p, C.size_t(len(raw)), cstrb(buf), C.size_t(len(buf)), &outLen) == 1
	if !ok {
		return "", false
	}
	return string(buf[:int(outLen)]), true
}

func cjsGetInt(s string) (int64, bool) {
	p, n := cstr2(s)
	var v C.longlong
	if C.go_get_int(p, n, &v) != 1 {
		return 0, false
	}
	return int64(v), true
}

func cjsABIVersion() int { return int(C.go_abi()) }

// unsafeString 把 C 返回的 span（指向 Go 原缓冲）读成 Go string。
func unsafeString(p *C.char, n int) string {
	if p == nil || n < 0 {
		return ""
	}
	bytes := (*[1 << 30]byte)(unsafe.Pointer(p))[:n:n]
	return string(bytes)
}

// cjsWalkMembers 遍历一个对象字符串，返回 (init 成功, 成员数, 是否正常结束)。
//
// 存在的原因：Go 测试文件**不能引用 C 类型**（没有 cgo），
// 而 ha_json_members 必须在 Go 栈上持有（零分配，见头文件设计约束）。
// 故由本文件在内部持有并把结果压成三个 Go 值。
func cjsWalkMembers(s string) (inited bool, count int, complete bool) {
	var m C.ha_json_members
	if !cjsMembersInit(&m, s) {
		return false, 0, false
	}
	for {
		_, ok := cjsMembersStep(&m)
		if !ok {
			break
		}
		count++
		if count > 100000 {
			break // 死循环保护
		}
	}
	return true, count, cjsMembersComplete(&m)
}

// cjsSkipStrict 复刻 Go json.Unmarshal 的严格性：整个输入必须是**恰好一个**
// JSON 值，尾部除空白外不得有残留。
//
// ★ 为什么测试不能只调 ha_json_skip：skip 的语义是「跳过这里的一个值」，
//
//	它成功返回并不能证明「整串就是这一个值」。实测 `{"a":1}{"b":2}`
//	在 skip 下成功，而 json.Valid=false —— 这正是两者职责的差别。
//	内核协议层要的是严格语义，故这里显式做尾部校验。
func cjsSkipStrict(s string) bool {
	p, n := cstr2(s)
	return C.go_skip_strict(p, n) == 1
}
