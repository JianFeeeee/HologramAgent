#ifndef HA_CODEC_H
#define HA_CODEC_H

/*
 * ha_codec — HomeAgent 内核编解码层（C 实现）
 *
 * ============================ 接口冻结声明 ============================
 * 本头文件是对外契约。函数签名、语义、返回值一经发布即为冻结接口，
 * 修改必须走大版本流程（与 third_party/hologramagentsdk 同一冻结标准）。
 *
 * 设计约束（见 docs/zh/c-core/llm-orchestration-c.md §四）：
 *   1. 只吃 const char* + **显式长度**，出数值/字节偏移 —— 不回调 Go、
 *      不传 Go 指针、不要求 NUL 结尾
 *   2. **不 malloc**：不需要出参缓冲区，需要「结果」时返回字节偏移/长度，
 *      由调用方在自己的缓冲上切片（零拷贝）
 *   3. 无状态、纯函数、线程安全（不写全局可变状态）
 *
 * 当前覆盖：L1 协议编解码层中的纯计算部分（第一个最小切片）。
 *
 * ============================ 为什么签名带长度 ============================
 * 初版签名用 `const char*` 隐含「NUL 结尾」，于是每次调用都要：
 *   Go `C.CString` 分配+拷贝一遍 → C `strlen` 再扫一遍。
 * 实测这部分开销占单次调用的 80% 以上（cgo 边界本身仅 ~30ns，
 * 而初版 ModelContextWindow 实测 175ns）。
 * 改为「指针 + 长度」后，Go 侧用 unsafe.StringData 直接传底层数组，
 * 零分配零拷贝。这是设计约束第 1 条的字面要求。
 */

#include <stddef.h>

#include "ha_abi.h"

#ifdef __cplusplus
extern "C" {
#endif

/* ==================== ABI 自述（供 Go 侧与日志核对） ==================== */

/* 返回 HA_CODEC_ABI_VERSION（major*1000 + minor）。
 *
 * 存在的意义：Go 侧不该靠 `#include` 宏做版本断言（cgo 头文件里的宏在
 * 预处理后不可见），而要**运行期/测试期**能问 C 侧「你自称什么版本」。
 * 由 codec_cgo.go 绑定、codec_abiversion_test.go 与 C 侧宏三方比对。 */
int ha_codec_abi_version(void);

/* ==================== 模型上下文窗口推断 ==================== */

/* 无法从模型名推断时的哨兵值（与 Go 侧一致）。
 *
 * 为什么返回哨兵而不是直接给兜底值：调用方需要区分「真推断出了」与
 * 「推断不出、只能兜底」——后者要打一行日志（窗口被低估必须可见），
 * 并提示部署方用 per-source context_window 显式声明。
 * 若 C 侧直接返回兜底值，调用方就永远分不清这两种情况。 */
#define HA_CODEC_CONTEXT_WINDOW_UNKNOWN (-1)

/* 由模型名推断最大上下文窗口（token 数）；推断不出返回
 * HA_CODEC_CONTEXT_WINDOW_UNKNOWN。
 *
 * model 为 UTF-8 字节序列，**不需要 NUL 结尾**；model_len 是字节数。
 * model 为 NULL 或 model_len 为 0 时返回 UNKNOWN。
 *
 * 匹配大小写不敏感（仅对 ASCII 字母做折叠；非 ASCII 字节按原样比较，
 * 与 Go 侧对模型名的实际输入一致）。
 *
 * 语义必须与 Go 侧 modelContextWindowPure 逐值一致（黄金对照测试钉死）。 */
int ha_codec_model_context_window(const char *model, size_t model_len);

/* ==================== token 估算与截断 ==================== */

/* 粗略估算 token 数。
 *
 * 规则（与 Go 侧 EstimateTokens 一致）：保守取 max(1, runeCount * 2)。
 * 按 UTF-8 **字符数**（rune）计，不是字节数。
 * text 为 NULL 或 text_len 为 0 返回 0。
 *
 * 非法 UTF-8 序列按 Go 的 utf8 解码语义处理（每字节一个 rune），
 * 保证与 Go 侧逐值一致。 */
int ha_codec_estimate_tokens(const char *text, size_t text_len);

/* 按 token 预算计算「应保留的字节数」。
 *
 * ★ 返回的是**字节数**而非字符串：截断结果必然是输入的前缀，
 *   调用方直接在自己的缓冲上切片即可（零拷贝、无出参缓冲区、无 malloc）。
 *
 * 语义与 Go 侧 TruncateByTokens 一致：从开头保留 maxTokens/2 个 rune；
 * 未超预算时返回 text_len（即整串）。
 * max_tokens <= 0 或 text 为 NULL/text_len 为 0 时返回 0。
 *
 * 返回值保证 <= text_len。 */
size_t ha_codec_truncate_by_tokens(const char *text, size_t text_len,
                                   int max_tokens);

#ifdef __cplusplus
}
#endif

#endif /* HA_CODEC_H */
