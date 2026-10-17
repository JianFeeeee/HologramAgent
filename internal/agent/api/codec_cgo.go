//go:build cgo

package api

// codec_cgo.go —— 编解码层的 C 实现绑定（CGO_ENABLED=1 时参与编译）。
//
// ============================ 架构：包内符号链接 ============================
// C 源是 `ha_codec.c` / `ha_codec.h`，它们是**指向 csrc/ 的符号链接**
// （`ln -s ../../../csrc/src/ha_codec.c`）：
//
//	internal/agent/api/ha_codec.c -> ../../../csrc/src/ha_codec.c
//	internal/agent/api/ha_codec.h -> ../../../csrc/include/ha_codec.h
//
// 权威源只有一份（csrc/），Go 侧看到的是包目录内的链接。
//
// ============================ 为什么不用另外两种做法 ============================
//
// **不能链接预构建静态库**（`LDFLAGS: .../csrc/build/libha_codec.a`）：
//   - .a 是构建产物、不入库（.gitignore 的 build/ 命中 csrc/build/），
//     而发布脚本原先并不产出它 ⇒「不入库 + 不生成」两头空，链接必然失败
//   - 交叉编译 linux/arm64（homed 的真实发布目标）时，宿主 x86-64 的 .a
//     被链进目标产物，报 `file in wrong format`
//
// **不能用 `#include "../../../csrc/src/ha_codec.c"`（包外相对包含）**：
//   ★ Go 构建缓存**不跟踪包外被 #include 的 C 文件**。实测：包外源把返回值
//   7 改成 8，`go test` 依然通过（缓存命中、静默沿用旧代码）；同样改动落在
//   包内文件时立即判红。这对「逐步推进 C 化」是致命的——改 C 源码却不生效
//   且无任何报错。
//   （包内 shim `#include` 包外源同样漏跟踪，已实测排除。）
//
// 包内符号链接同时满足两点：文件在包目录内 ⇒ 缓存按内容正确跟踪；
// 只有一份权威源 ⇒ 无副本漂移，也不需要「同步 C 源」的 make 目标。
//
// ============================ 零拷贝：不 CString、不 strlen ============================
// ★ 这是**被实测教训倒逼出来的**（见 docs/zh/c-core/llm-orchestration-c.md §7.1）：
//
// cgo 边界的固有成本实测约 **32 ns**（零拷贝传指针 + 空函数体）。
// 而初版每次调用都做 `C.CString`（malloc + 整串拷贝）+ C 侧 `strlen`（再扫一遍），
// 单这一项就约 **75 ns**，加上 C 侧 `lower_dup` 的 malloc 与逐字节扫描，
// 使 ModelContextWindow 实测达到 **175 ns** —— 即 **82% 是自找的开销**，
// 而非 cgo 的固有代价。初版由此得出「C 比 Go 慢」的结论是**错的**。
//
// 现在：Go 侧用 `unsafe.StringData` 把 string 的底层字节**直接**交给 C
// （传指针 + 长度），C 侧不 malloc、不 strlen、不要求 NUL 结尾。
// 截断则只回**字节长度**（结果必然是输入前缀），Go 侧 `s[:n]` 完成切片，
// 全程零分配零拷贝。
//
// 边界与安全：
//   - 不把 Go 指针交给 C 长期持有（C 侧不保存任何指针，纯函数）
//   - 空串在 Go 侧短路，不把可能的 nil 指针传下去
//   - cgo 规则允许传「不含 Go 指针的内存」的指针，string 底层字节满足
//
// ============================ 为什么不需要额外 build tag ============================
// 与 onnxruntime（internal/nlp/onnx.go，需运行期 libonnxruntime.so）不同：
// ha_codec 是**零依赖纯 C99 源码内联编译**，不需要任何外部库或工具链前提。
// 而 homed 本就强制 cgo（mattn/go-sqlite3 + gojieba），故 C 路径自然生效。
// 因此只用 `cgo` 约束（**没有 `!cgo` 回退**：CGO_ENABLED=0 下本包构建失败，
// 这是有意的响亮失败，理由见上），也不引入 hacodec tag。
//
// 语义必须与 codec_pure.go 逐值等价，由 codec_golden_test.go 钉死。

/*
#cgo CFLAGS: -std=c99
#include <stdlib.h>
#include "ha_codec.h"

// 下面这个常量就是 C 侧宏展开后的值，经由 cgo 暴露给 Go。
//
// ★ 声明成 C 函数（而非 const）才能从 Go 侧读到值：
//   cgo 生成的 `*_Cvar_*` 变量对 Go 而言**不是常量**（实测报
//   "is not constant"），所以 Go 侧拿它做不了编译期断言，
//   只能在测试期当普通变量比对。编译期的保证由下面那条 C 断言提供。
int ha_abi_version_macro(void) { return HA_CODEC_ABI_VERSION; }

// C 侧自检：宏合成式与主/次版本必须自洽。
// 这条断言在**编译 C 时**就生效，而不是等 Go 侧测试跑到。
_Static_assert(HA_CODEC_ABI_MAJOR * 1000 + HA_CODEC_ABI_MINOR == HA_CODEC_ABI_VERSION,
               "ha_abi.h: HA_CODEC_ABI_VERSION 合成式与主/次版本不一致");
*/
import "C"

import "unsafe"

// codecABIVersionExpected 是 C 侧 ha_abi.h 里 HA_CODEC_ABI_VERSION 的 Go 副本。
//
// ★ 为什么要手工拄一份而不是让 cgo 直接读宏：
//
//	cgo 顶部的 C 代码在 cgo 阶段被**预处理并丢弃**，其中的宏在 Go 侧不可见；
//	能看到的只有 cgo 生成的文件。用 cgo 的 `const` 桥接（C.ha_codec_abi_version）只能在
//	**运行期**问到版本，编译期拿不到，无法把「两侧版本不一致」变成构建失败。
//	而「注释里说冻结」不是机制。这份 Go 常量 + codec_abiversion_test.go
//	把版本漂移变成**测试期断言**，真正对得上才跑得起来。
//
// 改动规则（与 ha_abi.h 一致）：
//   - C 侧新增函数/枚举值（纯追加）→ 同步把这里 +1，并改 abi_test 的期望
//   - 改签名/删函数/改结构体布局 → MAJOR+1，**所有调用方必须同步重编**
const (
	codecABIMajorExpected = 1
	codecABIMinorExpected = 0
)

// codecABIVersionMacroValue 查询 C 侧 HA_CODEC_ABI_VERSION 宏展开后的值。
//
// ★ 它的存在是为了堵一个盲区：TestABIVersionMatches 比的是
//
//	「Go 常量 vs C 函数返回值」。若有人同时把 Go 常量和 C 函数
//	一起改掉（而忘了改 ha_abi.h 的宏），那条测试照样通过 ——
//	**两边一起错成一样**是它的盲区。这个值直接取自 C 宏，
//	由 codec_abimacro_test.go 拿来交叉核对。
//
// ★ 为何是函数而非 Go 常量：cgo 生成的 `*_Cvar_*` 不是 Go 常量
//
//	（实测 "is not constant"），无法在编译期参与断言。
//	编译期的保证在 C 侧（codec_cgo.go 里的 _Static_assert）。
func codecABIVersionMacroValue() int { return int(C.ha_abi_version_macro()) }

// codecABIVersion 查询 C 侧自称的 ABI 版本（major*1000 + minor）。
func codecABIVersion() int { return int(C.ha_codec_abi_version()) }

// cstr2 与 cstr 同义（返回 Go 的 string 版本），供 cgo 桥接层使用。
// 名字不同是为了与测试文件里的辅助函数区分，避免包内重名。
func cstr2(s string) (*C.char, C.size_t) { return cstr(s) }

// cstrb 取字节切片的首地址（供 C 侧写入目标缓冲）。
func cstrb(b []byte) *C.char {
	if len(b) == 0 {
		return nil
	}
	return (*C.char)(unsafe.Pointer(&b[0]))
}

// cstrp 返回 Go string 的底层字节首地址（不做空串短路，供
// 「长度已知、可能为空」的取值场景使用）。
func cstrp(s string) *C.char {
	if len(s) == 0 {
		return nil
	}
	return (*C.char)(unsafe.Pointer(unsafe.StringData(s)))
}

// cstr 返回 s 的底层字节首地址与长度，供 C 侧零拷贝读取。
//
// 空串返回 (nil, 0)：调用方不应把 nil 传给会解引用的 C 函数。
func cstr(s string) (*C.char, C.size_t) {
	if len(s) == 0 {
		return nil, 0
	}
	return (*C.char)(unsafe.Pointer(unsafe.StringData(s))), C.size_t(len(s))
}

// modelContextWindowC 经 C 实现推断上下文窗口。
func modelContextWindowC(model string) int {
	p, n := cstr(model)
	return int(C.ha_codec_model_context_window(p, n))
}

// estimateTokensC 经 C 实现估算 token 数。
//
// ★ 不做按长度分派：**完全 C 化**——compute 一律走 C，纯 Go 实现不再是
// 生产路径（只作为黄金对照的规格基准）。
//
// 代价（如实记录，勿用「C 更快」一句话盖过）：cgo 边界固有成本实测约 30ns，
// 故对「极短串」（如 2 字节的 "qq"）本函数约 30ns，而直调纯 Go 仅约 3ns
// ——即极短输入上 C 路径约慢一个数量级，但绝对值是**纳秒级**
// （30ns = 0.00003ms，单次请求尺度可忽略）。
// 换来的是：单一实现、无静默分派分叉、C 侧对畸形 UTF-8 的严格校验恒生效。
func estimateTokensC(text string) int {
	p, n := cstr(text)
	return int(C.ha_codec_estimate_tokens(p, n))
}

// truncateByTokensC 经 C 实现按 token 截断。
//
// C 侧只返回「应保留的字节数」——截断结果必然是输入的前缀，
// 故这里直接切片，无需缓冲区、无需 malloc、无需把结果拷回来。
func truncateByTokensC(s string, maxTokens int) string {
	if maxTokens <= 0 || s == "" {
		return ""
	}
	p, n := cstr(s)
	keep := C.ha_codec_truncate_by_tokens(p, n, C.int(maxTokens))
	if uint64(keep) >= uint64(len(s)) {
		return s
	}
	return s[:int(keep)]
}
