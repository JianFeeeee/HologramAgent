# 门禁脚本需要 bash 的 PIPESTATUS（/bin/sh=dash 不支持）。
# 没有它就无法区分"编译器失败"与"head 成功" —— 实测会让门禁假通过。
SHELL := /bin/bash

.PHONY: test-gui all build build-plain build-cli build-gui clean install test run build-static build-linux-arm64 lint fmt sync-client-versions check-client-versions check-starmap-shared check-client-protocol csrc csrc-test csrc-lint csrc-abi csrc-headers csrc-sanitize csrc-cross csrc-fuzz check-csrc check-csrc-full
# HOMED_TAGS 默认带 onnxruntime：发行版**默认启用**本地向量空间（与
# deploy/packaging/build.sh 保持一致）。
#
# 曾经这里是空 tags，实测的后果（2026-09-15 热部署）：`make build` 产出的
# homed 只有 33MB，而 onnxruntime 版是 84MB；启动日志里
# 「multimodal space active: provider=chineseclip」整行消失，少加载一个插件，
# 静态词向量也退化成 fallback——而打包脚本会直接**拒收**这种二进制
# （package-linux.sh 检查 `-tags=.*onnxruntime`）。即「本地随手 make build」
# 与「发行构建」不是同一个东西，部署时无从察觉。
# 需要极简构建时显式 HOMED_TAGS= 关掉。
#
# 注意：内核 C 编解码层（internal/agent/api/ha_codec.c）**不靠 tag 开关**，
# 而是由 cgo 本身决定（`//go:build cgo` / `!cgo`）。原因：它是零依赖纯 C99
# 源码内联编译，不需要任何外部库/工具链前提；而 homed 本就强制 cgo
# （sqlite3 + gojieba），所以 C 路径自然生效，无需额外开关。
# 对比 onnxruntime：那个需要运行期的 libonnxruntime.so，所以必须显式 tag。
HOMED_TAGS ?= onnxruntime
TAG_ARGS = $(if $(HOMED_TAGS),-tags $(HOMED_TAGS),)

BINARY=homed
CLI_BINARY=waiter
GUI_BINARY=hologram-gui
GO=go
GOCACHE=/tmp/gocache
export GOPATH=/tmp/gopath
BUILD_DIR=build
# GRAPH_FILE 是启动依赖图的制品名（必须与 kernelboot.GraphFileName 一致）。
#
# ★ 图的来源是**各模块自己的 Requires()/Provides()**（不再有手工 JSON 清单）：
# 生成器 import internal/kernelmod/all（各模块 init 自注册）⇒ 算分层 ⇒ 落图。
GRAPH_FILE=depgraph.bin
PROJECT_ROOT := $(CURDIR)
# 版本号来源（见 docs/git-branching.md §2.1）：
#
#   发布线（release/vX.Y.x）：用该线的 tag 描述，产出 v1.3.12 这类正式号。
#   main（开发线）：**不**用 git describe —— main 上可达的最新 tag 永远属于
#   某条已发布的旧 patch 线（实测：main 可达 tag 是 v1.3.2，而 v1.3.12 打在
#   release/v1.3.x 上、不在 main 的祖先路径里），于是 main 构建会自称
#   "v1.3.2-192-gxxxxxx" —— 版本号看着像在 1.3.2 补丁线上，实际是下一个
#   中版本的开发态，属于会误导人的路牌。
#
# 所以 main 显式产出 <meta.Version>dev：1.4.0dev。
# meta.Version 由源码声明（internal/meta/meta.go），随发布推进而更新。
# 构建号（距上次 tag 的提交数 + 短 SHA）仍然保留，便于定位具体提交。
VERSION ?= $(shell git describe --tags --dirty 2>/dev/null)
# 开发线（main）强制走 dev 号：以 meta.Version 源码声明为准，加 dev 后缀。
# 不能用「describe 为空」来判断 —— main 上 describe 非空（可达 v1.3.2），
# 但那个号属于已发布的旧 patch 线，对 main 没有版本语义。
# 判定用「是否在发布线分支」而不是「是否等于 main」：detached HEAD（CI 常见）
# 时分支名是 HEAD，若只判 main 就会退回 describe，产出 v1.3.2-192 这种
# 属于旧 patch 线的误导号（实测踩到）。发布线之外的任何状态都走 dev 号。
CURRENT_BRANCH := $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null)
ifneq ($(filter release/v%,$(CURRENT_BRANCH)),)
VERSION := $(VERSION)
else
META_VERSION := $(shell grep -oE 'Version = "[0-9.]+"' internal/meta/meta.go | head -1 | grep -oE '[0-9.]+')
VERSION := $(META_VERSION)dev$(if $(filter --dirty,$(shell git status --porcelain)),+dirty)
endif
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS = -X github.com/JianFeeeee/HologramAgent/internal/meta.Version=$(VERSION) -X github.com/JianFeeeee/HologramAgent/internal/meta.Commit=$(COMMIT) -X github.com/JianFeeeee/HologramAgent/internal/meta.BuildTime=$(BUILD_TIME)

all: build build-cli

# csrc：C 源码的**独立**产物（静态库 + 契约测试），供 C 侧复用（鸿蒙/嵌入式/C SDK）。
#
# ⚠️ Go 构建**不依赖**它：internal/agent/api/ha_codec.{c,h} 是指向 csrc/ 的
# **符号链接**，cgo 直接编这份源码，而不是链接预构建的 .a。
#
# 为什么是「包内符号链接」而不是别的（历史教训 + 实测，勿回退）：
#   1. 不能链静态库：.a 是构建产物、不入库，而发布脚本原先不产出它
#      ⇒「不入库 + 不生成」两头空（实测 cannot find csrc/build/libha_codec.a）；
#      且交叉编译 linux/arm64 时宿主 x86-64 的 .a 被链进目标产物，
#      报 `file in wrong format`。
#   2. 不能用 `#include "../../../csrc/src/ha_codec.c"`（包外相对包含）：
#      ★ Go 构建缓存**不跟踪包外被 #include 的 C 文件**，改了 C 源码但缓存命中时
#        会静默沿用旧代码（实测：变异 C 源码后 go test 仍报 ok）。
#        这对「逐步推进 C 化」是致命的——改动无效却无人察觉。
#   3. 包内符号链接：文件在包目录内 ⇒ 缓存按内容哈希正确跟踪
#      （实测：改 csrc/ 源文件后 go test 立即判红）；
#      同时只有一份权威源（csrc/），无副本漂移、无需同步目标。
#
# csrc 目标本身只服务「C 侧独立使用 + ctest」，不参与 Go 构建链路。
CSRC_DIR=csrc
CSRC_BUILD=$(CSRC_DIR)/build
CSRC_LIB=$(CSRC_BUILD)/libha_codec.a

# ============================ C 编译告警门禁 ============================
#
# 为什么要「零告警」而不是「有告警就看看」：
# 1. 本仓 C 代码量还小（ha_codec.c 约 340 行），任何告警都值得当场修；
#    门禁零成本维持（本地实测 4 个编译器×标准组合全 0 告警）。
# 2. C 侧没有 Go 那套 vet 等价物，告警是**唯一的**静态信号。
#    若是「先攒着」，C 侧会慢慢退化成一堆没人看的噪声，然后没人看。
# 3. -Wconversion 特意包含在内：C→Go 经 cgo 时隐式窄化（如 size_t→int）
#    是真实事故来源（长度字段截断），而它在默认档下是静默的。
#
# -Wpedantic 尤其重要：它抓出「用了 C11 特性但 CFLAGS 写 -std=c99」这类
# 跨工具链不一致（本轮就当场抓到 _Static_assert 一例，见 ha_abi.h）。
CSRC_STD ?= c99
CSRC_WARN_FLAGS = -Wall -Wextra -Wpedantic -Wshadow -Wconversion
CSRC_CFLAGS = -std=$(CSRC_STD) $(CSRC_WARN_FLAGS) -I$(CSRC_DIR)/include
CSRC_SRCS = $(wildcard $(CSRC_DIR)/src/*.c)
CSRC_HDRS = $(wildcard $(CSRC_DIR)/include/*.h)

# 可选的第二编译器：只有一份编译器通过 ≠ C 写法可移植
# （GCC 扩展在 clang 下报错、或反之，都是真实的发布事故）。
CSRC_CC2 ?= clang

csrc: $(CSRC_LIB)

$(CSRC_LIB): $(wildcard $(CSRC_DIR)/src/*.c) $(wildcard $(CSRC_DIR)/include/*.h) $(CSRC_DIR)/CMakeLists.txt
	@cmake -S $(CSRC_DIR) -B $(CSRC_BUILD) -DBUILD_TESTS=ON >/dev/null
	@cmake --build $(CSRC_BUILD) -j >/dev/null
	@echo "Built: $(CSRC_LIB)（C 侧复用，不参与 Go 构建）"

# csrc-test：C 侧契约测试（黄金对照的另一半，见 docs/zh/c-core/llm-orchestration-c.md §五）
csrc-test: csrc
	@cd $(CSRC_BUILD) && ctest --output-on-failure

# csrc-lint：C 侧告警门禁（主编译器 + 第二编译器交叉，零告警）
#
# 用 \`-Werror\` 而不是只看输出：只有「告警即失败」才是门禁，
# 否则它只是打印给人看，而人会累。
# cmd/gui 的判据入口。
#
# 为什么单列：cmd/gui 是**纯 Electron 目录**（0 个 .go、无 go.mod），
# `go test ./...` 会跳过它（实测 43 包全绿、0 处提及）。
# 也就是说：不挂到这里，GUI 的判据**没有任何标准工具链会跑**。
# `go test ./cmd/gui` 报 "no Go files [setup failed]" 属预期，不是回归。
test-gui:
	@command -v node >/dev/null || { echo "SKIP: 无 node，GUI 判据未跑"; exit 0; }
	@cd cmd/gui && npm test --silent

.PHONY: csrc-lint
csrc-lint:
	@echo "== C 告警门禁（$(CSRC_STD)，$(CSRC_WARN_FLAGS)）=="
	@for cc in $(CC) $(CSRC_CC2); do \
		command -v $$cc >/dev/null 2>&1 || { echo "  [SKIP] $$cc 不存在"; continue; }; \
		out=$$($$cc $(CSRC_CFLAGS) -Werror -fsyntax-only $(CSRC_SRCS) 2>&1); \
		if [ -n "$$out" ]; then \
			echo "  [FAIL] $$cc 有告警："; echo "$$out" | head -20; exit 1; \
		else \
			echo "  $$cc: 0 告警 ✓"; \
		fi; \
	done

# csrc-abi：C 侧 ABI 版本自洽性（编译期断言已在 ha_abi.h 内，这里做运行期核对）
.PHONY: csrc-abi
csrc-abi:
	@echo "== C ABI 版本自述 =="
	@printf '#include <stdio.h>\n#include "ha_codec.h"\nint main(void){printf("%%d\\n", ha_codec_abi_version());return 0;}\n' > $(CSRC_BUILD)/abi_probe.c 2>/dev/null || mkdir -p $(CSRC_BUILD) && printf '#include <stdio.h>\n#include "ha_codec.h"\nint main(void){printf("%%d\\n", ha_codec_abi_version());return 0;}\n' > $(CSRC_BUILD)/abi_probe.c
	@$(CC) $(CSRC_CFLAGS) $(CSRC_BUILD)/abi_probe.c -o $(CSRC_BUILD)/abi_probe $(CSRC_SRCS) 2>/dev/null
	@v=$$($(CSRC_BUILD)/abi_probe); \
	if [ "$$v" -ge 1000 ] && [ "$$v" -le 99999 ]; then \
		echo "  ha_codec ABI_VERSION = $$v (major=$$((v/1000)) minor=$$((v%1000))): OK"; \
	else \
		echo "  [FAIL] ABI 版本荒谬：$$v"; exit 1; \
	fi

# csrc-sanitize：ASan + UBSan 跑 C 契约测试
#
# 目的：内存错误与未定义行为在 C 侧默认是**静默的**（不崩、结果看起来对），
# 而内核 L1 路径零 malloc 的设计依赖「没有越界写」这一前提。
# C 侧没有 Go 的 -race 等价物，sanitizer 就是这里的关等物。
# 若本机无 libasan/libubsan（交叉工具链常见），明确 SKIP 而非静默跳过。
.PHONY: csrc-sanitize
# ★ 每个测试文件**各自**链接成独立二进制：契约测试每个都带 main，
#   合在一起会「multiple definition of main」——而报错被 2>/dev/null
#   吞掉后会被误报成「本机无 sanitizer」，是个假的 SKIP。
#   故这里逐个构建、逐个跑，任何一个失败都判红。
csrc-sanitize:
	@echo "== C 侧 ASan+UBSan =="
	@tmp=$$(mktemp -d); \
	built=0; \
	for t in $(CSRC_DIR)/test/test_*.c; do \
		case "$$t" in *fuzz*) continue ;; esac; \
		base=$$(basename $$t .c); \
		if ! $(CC) $(CSRC_CFLAGS) -fsanitize=address,undefined -fno-omit-frame-pointer \
			-o $$tmp/$$base $(CSRC_SRCS) $$t 2>$$tmp/build.log; then \
			if grep -qi 'sanitize\|asan\|ubsan' $$tmp/build.log; then \
				echo "  [SKIP] 本机无 ASan/UBSan 运行库，已跳过"; rm -rf $$tmp; exit 0; \
			fi; \
			echo "  [FAIL] 构建失败 ($$base)：" ; head -10 $$tmp/build.log; rm -rf $$tmp; exit 1; \
		fi; \
		built=1; \
		if ASAN_OPTIONS=detect_leaks=1 UBSAN_OPTIONS=print_stacktrace=1:halt_on_error=1 \
			$$tmp/$$base > $$tmp/$$base.out 2>&1; then \
			echo "  ASan+UBSan $$base: PASS"; \
		else \
			echo "  [FAIL] sanitizer 报告 ($$base)："; head -30 $$tmp/$$base.out; rm -rf $$tmp; exit 1; \
		fi; \
	done; \
	rm -rf $$tmp; \
	if [ "$$built" = "0" ]; then echo "  [FAIL] 没找到任何契约测试"; exit 1; fi

# csrc-headers：头文件自包含性（每个 .h 都能单独编过）
#
# 为什么需要：ha_codec.h 头写了「本头文件是对外契约，签名冻结」，
# 而**头文件能不能自己编过**是另一件事。若头里用到了自己没包含的东西
# （比如用了 int32_t 却没 <stdint.h>），后果是：
#   - 在某个翻译单元里恰好被别的头预先包含了 → 静默编过
#   - 在别处（鸿蒙/嵌入式/C SDK 直接包含它）→ 报一堆无关的错
# 本轮就靠它抓出 ha_abi.h 的静态断言垫片缺 <assert 类依赖> 类问题。
# 判据：每个头单独编 -fsyntax-only 必须为 0 告警 0 错。
.PHONY: csrc-headers
csrc-headers:
	@echo "== 头文件自包含性 =="
	@ok=1; \
	for h in $(CSRC_HDRS); do \
		base=$$(basename $$h); \
		inc=$$(dirname $$h); \
		out=$$(echo "$$cc" | tr -d '-'; ); \
		for cc in $(CC) $(CSRC_CC2); do \
			command -v $$cc >/dev/null 2>&1 || continue; \
			printf '#include "%s"\nint main(void){return 0;}\n' "$$base" > $(CSRC_BUILD)/hdr_probe.c; \
			res=$$($$cc -std=$(CSRC_STD) $(CSRC_WARN_FLAGS) -I$$inc -I$(CSRC_DIR)/include -Werror \
				-fsyntax-only $(CSRC_BUILD)/hdr_probe.c 2>&1); \
			if [ -n "$$res" ]; then \
				echo "  [FAIL] $$base 单独包含时失败（$$cc）："; echo "$$res" | head -10; ok=0; \
			fi; \
		done; \
	done; \
	if [ "$$ok" = "1" ]; then echo "  $(words $(CSRC_HDRS)) 个头文件：自包含 OK ✓"; else exit 1; fi

# csrc-fuzz：libFuzzer 跑不变式 + 内存安全（需 clang，无则明确 SKIP）
#
# 这是 C 侧唯一能「持续」而非「等下一次手写用例」的检验。
# ha_codec 的等价契约（与 Go 的 utf8.DecodeRuneInString 一致）在正常输入下
# 永远测不到，只有随机字节能覆盖截断序列/过长编码/代理对/超 U+10FFFF。
# 门禁不能假装通过：无 clang 或无 libFuzzer 时显式 SKIP 并说明。
.PHONY: csrc-fuzz
csrc-fuzz:
	@echo "== C 侧 libFuzzer（clang）=="
	@if ! command -v $(CSRC_CC2) >/dev/null 2>&1; then \
		echo "  [SKIP] $(CSRC_CC2) 不存在，无法跑 libFuzzer"; exit 0; \
	fi; \
	tmp=$$(mktemp -d); \
	if ! $(CSRC_CC2) $(CSRC_CFLAGS) -fsanitize=fuzzer,address,undefined -fno-omit-frame-pointer \
		-o $$tmp/fz $(CSRC_SRCS) $(CSRC_DIR)/test/test_fuzz_ha_codec.c 2>/dev/null; then \
		echo "  [SKIP] 无 libFuzzer 运行库（需要 clang 自带），已跳过"; rm -rf $$tmp; exit 0; \
	fi; \
	SECS=$${FUZZ_SECS:-20}; \
	if $$tmp/fz -max_total_time=$$SECS -rss_limit_mb=4096 > $$tmp/fz.log 2>&1; then \
		runs=$$(grep -oE 'Done [0-9]+ runs' $$tmp/fz.log | tail -1); \
		echo "  libFuzzer: PASS（$${runs:-完成}，$${SECS}s）"; rm -rf $$tmp; \
	else \
		echo "  [FAIL] 模糊测试崩溃："; tail -30 $$tmp/fz.log; rm -rf $$tmp; exit 1; \
	fi

# csrc-cross：交叉编译 C 侧（arm64 是 homed 的真实发布目标之一）
#
# 为什么要单独门禁：Go 侧的 `go build` 不等于 C 代码在该架构上能编。
# C 侧的架构相关问题（endianness 假设、指针宽度、size_t vs int 宽度、
# -fsanitize 不可用）只有真的用目标编译器编一遍才会暴露。
# 与 deploy/packaging/build.sh 的 arm64 目标共用同一套 CC 变量。
#
# ## 为什么首选 clang --target，而不是 aarch64-linux-gnu-gcc（2026-10-22）
#
# 这道门禁只做 `-fsyntax-only`（不链接、不运行），而 clang 自带交叉前端：
# `clang --target=aarch64-linux-gnu` 会真的切到 aarch64 目标（`__aarch64__`
# 宏生效、指针宽度按该架构计算），足以暴露本门禁要抓的那类问题
# （endianness 假设、指针/size_t 宽度、不支持的 C 扩展）。
#
# 而装 `gcc-aarch64-linux-gnu` 需要从源拉 10MB+ 的 cpp-13 等依赖 ——
# 实测在 CI runner 上会卡住超时（见 .github/workflows/ci.yml 的加固说明）。
# 既然只需要语法层验证，就不该为它引入一个会卡死的下载依赖。
#
# 优先级：CC_ARM64（显式指定） > clang --target > aarch64-linux-gnu-gcc > SKIP
#
# ★ 两个假通过陷阱（实测踩到，勿删）：
#   1. 显式 CC_ARM64 指向不存在的命令时，`cmd | head` 的退出码是 head 的（恒 0），
#      command-not-found(127) 被吞 ⇒ 门禁显示"通过"。所以循环里改用 PIPESTATUS[0]，
#      且对显式指定的编译器先做 command -v 校验。
#   2. 上述 PIPESTATUS 需要 bash；Make 默认 /bin/sh=dash 不支持（见文件顶 SHELL）。
.PHONY: csrc-cross
csrc-cross:
	@echo "== C 侧交叉编译（linux/arm64）=="
	@ok=1; \
	if [ -n "$${CC_ARM64:-}" ]; then \
		if ! command -v "$$CC_ARM64" >/dev/null 2>&1; then \
			echo "  [FAIL] CC_ARM64=$$CC_ARM64 不存在"; exit 1; \
		fi; \
		CC="$$CC_ARM64"; MODE="explicit"; \
	elif command -v clang >/dev/null 2>&1; then \
		CC="clang --target=aarch64-linux-gnu"; MODE="clang-target"; \
	elif command -v aarch64-linux-gnu-gcc >/dev/null 2>&1; then \
		CC="aarch64-linux-gnu-gcc"; MODE="cross-gcc"; \
	else \
		echo "  [SKIP] 无 clang、无 aarch64-linux-gnu-gcc（未装任何交叉前端）"; exit 0; \
	fi; \
	\
	if [ "$$MODE" = "clang-target" ]; then \
		printf 'int main(void){return 0;}\n' > /tmp/_archchk.c; \
		if ! clang --target=aarch64-linux-gnu -fsyntax-only /tmp/_archchk.c 2>/dev/null; then \
			echo "  [SKIP] clang 不支持 --target=aarch64-linux-gnu（版本过旧）"; exit 0; \
		fi; \
	fi; \
	\
	for src in $(CSRC_SRCS); do \
		$$CC $(CSRC_CFLAGS) -Werror -fsyntax-only $$src 2>&1 | head -20; \
		rc=$${PIPESTATUS[0]}; \
		if [ "$$rc" != "0" ]; then ok=0; fi; \
	done; \
	if [ "$$ok" = "1" ]; then \
		echo "  $$CC [$$MODE]: 0 告警、编译通过 ✓（$(words $(CSRC_SRCS)) 个源文件）"; \
	else \
		echo "  [FAIL] arm64 交叉编译失败"; exit 1; \
	fi

# check-csrc：C 侧全部门禁的聚合入口（接进 make test 与 CI）
.PHONY: check-csrc
check-csrc: csrc-lint csrc-abi csrc-headers csrc-sanitize csrc-cross
	@echo "== C 基础设施门禁：全部通过 =="

# check-csrc-full：在 check-csrc 基础上加模糊测试（耗时，故分开）
.PHONY: check-csrc-full
check-csrc-full: check-csrc csrc-fuzz
	@echo "== C 基础设施门禁（含模糊测试）：全部通过 =="

build:
	@mkdir -p $(BUILD_DIR)
	@# ★ 编译期定序：先由生成器（import 模块登记表）算出启动顺序图，
	@#   取 digest，再构建 homed 并把 digest 注入二进制。
	@#
	@# 为何必须"先出图后构建"（设计 §2.5.2）：启动时 homed 比对
	@# 「磁盘图的 digest」与「内嵌 digest」，不一致 ⇒ **拒绝启动**。
	@# 两边必须来自**同一次构建**。
	@#
	@# 为何要这个保护：它能拦住最危险的一类故障 —— 图文件自身完好
	@# （自校验通过），但描述的是**旧代码**（改了 Requires 没重出图）
	@# ⇒ 按旧图调度 ⇒ 顺序错（正是要消灭的"静默顺序错"）。
	@#
	@# ⚠ 本段必须写成**一条 shell 命令**（\ 续行）：
	@#   Make 的每行 recipe 是独立 shell，普通变量跨行不保留。
	@#
	@# ★★ ldflags 的引号必须让 $${GRAPH_HASH} **能展开**（2026-10-17 实测缺陷）：
	@#   原写法把整串包在**单引号**里 ⇒ shell 里单引号不做变量展开
	@#   ⇒ 注入进二进制的是**字面量** `$$GRAPH_HASH` ⇒ 内核启动时比对
	@#   「磁盘图 digest」vs「内嵌的 $$GRAPH_HASH」必然不等 ⇒ **拒绝启动**。
	@#   而 `make build` 日志全绿（图生成了、hash 也"注入"了），
	@#   只有真跑内核才暴露。修法：用**双引号**包 ldflags。
	@#
	@# ⚠ 上面这条注释**不能挪到 recipe 中间**：注释行会打断 \ 续行链，
	@#   使后半段成为**独立的 shell**（变量丢失 ⇒ hash 为空）。
	@GRAPH_HASH=""; \
	CGO_ENABLED=1 $(GO) build $(TAG_ARGS) -o $(BUILD_DIR)/homed-depgraph ./cmd/homed-depgraph/ && \
	GRAPH_HASH=$$($(BUILD_DIR)/homed-depgraph -digest) && \
	$(BUILD_DIR)/homed-depgraph -print && \
	$(BUILD_DIR)/homed-depgraph -out $(BUILD_DIR)/$(GRAPH_FILE) && \
	echo "已注入启动依赖图 hash: $$GRAPH_HASH" ; \
	CGO_ENABLED=1 $(GO) build $(TAG_ARGS) -trimpath -installsuffix dynlink -ldflags "$(LDFLAGS) -X github.com/JianFeeeee/HologramAgent/internal/kernelboot.EmbeddedGraphHash=$$GRAPH_HASH" -o $(BUILD_DIR)/$(BINARY) ./cmd/homed/
	@echo "Built: $(BUILD_DIR)/$(BINARY) ($(VERSION), tags='$(HOMED_TAGS)')"
	@go version -m $(BUILD_DIR)/$(BINARY) | grep -q 'onnxruntime' \
		|| echo "WARN: 本次构建不含 onnxruntime，本地向量空间不可用（HOMED_TAGS= 显式关掉时才符合预期）"
	@go version -m $(BUILD_DIR)/$(BINARY) | grep -q 'CGO_ENABLED=1' \
		|| echo "WARN: 本次构建未启用 cgo，编解码走纯 Go 回退（不应发生）"

build-cli:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GO) build -installsuffix dynlink -ldflags '$(LDFLAGS)' -o $(BUILD_DIR)/$(CLI_BINARY) ./cmd/waiter/
	@echo "Built: $(BUILD_DIR)/$(CLI_BINARY) ($(VERSION))"

build-gui:
	@cd cmd/gui && npm install --production && npx electron-packager . $(GUI_BINARY) --out=../../$(BUILD_DIR) --overwrite --no-sandbox --icon=icon.ico
	@echo "Built: $(BUILD_DIR)/$(GUI_BINARY)"

build-static:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=1 $(GO) build -tags netgo -installsuffix dynlink -ldflags '-extldflags "-static" $(LDFLAGS)' -o $(BUILD_DIR)/$(BINARY)-static ./cmd/homed/
	@echo "Built (static): $(BUILD_DIR)/$(BINARY)-static"

# build-linux-arm64：交叉编译 homed（真实发布目标之一）。
#
# 两处必须显式给定，否则必然失败（都不是 C 化引入的，但都长期缺覆盖）：
#   1. CC/CXX 交叉工具链。缺 CXX 时 cgo 回退到宿主 g++，而宿主编译器不认
#      aarch64 汇编，报 `gcc_arm64.S: no such instruction: 'stp x29,x30,[sp,'`。
#      deploy/packaging/build.sh:49 一直是对的，此处此前漏了。
#   2. .syso 隔离。cmd/{homed,waiter}/*.syso 是 Windows COFF 资源对象，
#      Go 会把同目录 .syso **无条件**链进任何目标；交叉到非 Windows 平台报
#      `file format not recognized`。build.sh 有 hide_syso_for_target，此处同样漏了。
CC_ARM64 ?= aarch64-linux-gnu-gcc
CXX_ARM64 ?= aarch64-linux-gnu-g++

build-linux-arm64:
	@mkdir -p $(BUILD_DIR)
	@for f in cmd/homed/*.syso cmd/waiter/*.syso; do \
		[ -f "$$f" ] || continue; \
		mv "$$f" "$$f.hidden"; \
	done; \
	trap 'for f in cmd/homed/*.syso.hidden cmd/waiter/*.syso.hidden; do \
		[ -f "$$f" ] || continue; mv "$$f" "$${f%.hidden}"; done' EXIT; \
	GOOS=linux GOARCH=arm64 CGO_ENABLED=1 CC=$(CC_ARM64) CXX=$(CXX_ARM64) \
		$(GO) build $(TAG_ARGS) -installsuffix dynlink -ldflags '$(LDFLAGS)' \
		-o $(BUILD_DIR)/$(BINARY)-arm64 ./cmd/homed/; \
	for f in cmd/homed/*.syso.hidden cmd/waiter/*.syso.hidden; do \
		[ -f "$$f" ] || continue; mv "$$f" "$${f%.hidden}"; done; \
	trap - EXIT
	@echo "Built (arm64): $(BUILD_DIR)/$(BINARY)-arm64 ($$(file $(BUILD_DIR)/$(BINARY)-arm64 | sed 's/.*: //'))"

clean:
	rm -rf $(BUILD_DIR) $(BINARY) $(CSRC_BUILD)

install: build
	-systemctl stop hologramagent 2>/dev/null
	cp $(BUILD_DIR)/$(BINARY) /usr/local/bin/$(BINARY)
	mkdir -p /etc/hologram /var/lib/hologram
	cp deploy/hologramagent.service /etc/systemd/system/hologramagent.service
	systemctl daemon-reload
	@echo "Installed. Run: systemctl enable --now hologramagent"

# ⚠ internal/plugin/proc 的 grandchild 系列测试（fork 真进程 + syscall.Kill
#   杀进程组）**本身不稳定**，与本次改动无关：
#     · 单独跑同一命令：ok 11.5s / FAIL 交替出现（实测至少各一次）
#     · 全量并发跑：曾 600s 超时，也曾 90s 就 FAIL
#     · 失败形态固定是 TestKillReturnsEvenWhenGrandchildSurvives，
#       伴随日志 `[proc] audit 退出: signal: killed`
#   症状像 fork/kill 的进程组语义在容器/并发下不稳（孙进程 setsid 脱组后
#   杀不掉 ⇒ Wait 挂死），但**尚未定位到根因**，所以这里只记录现象、
#   不下结论。要稳定门禁就单独跑并重试：
#       go test ./internal/plugin/proc/ -count=1
#   ⚠ 别把"单跑通过"当结论——同一命令会交替通过/失败。
# ★ test-gui 必须**一定被执行**，但不能被前序失败短路，也不能吞掉自己的失败。
#
#   问题：`go test ./...` 一旦 FAIL，make 立即中止 ⇒ 挂在它后面的目标
#   都不会跑。实测 internal/plugin/proc 偶发 FAIL 时，make test 日志里
#   **找不到 test-gui 的任何输出** —— 门禁形同虚设。
#   但直接用 `-@$(MAKE) test-gui` 又会**吞掉** GUI 判据自己的失败码，
#   变成给假绿灯。
#
#   做法：先无条件跑并把结果存进变量，最后统一决定退出码。
#   这样既保证它一定跑，也保留它自己的失败。
test:
	@rc=0; $(GO) test ./... || rc=$$?; \
	 $(MAKE) test-gui || rc=$$?; \
	 $(MAKE) csrc-test || rc=$$?; \
	 $(MAKE) check-csrc || rc=$$?; \
	 $(MAKE) check-codec-cgo-only || rc=$$?; \
	 $(MAKE) check-starmap-shared || rc=$$?; \
	 $(MAKE) check-client-protocol || rc=$$?; \
	 exit $$rc

# check-codec-cgo-only：钉死「编解码层完全 C 化」这一决定。
#
# 两条断言，缺一不可：
#   ① CGO_ENABLED=1 下测试全绿（含黄金对照：C 与纯 Go 参考实现逐值相等）
#   ② CGO_ENABLED=0 下**构建必须失败**
#
# 为什么②要断言「失败」而不是「也能编过」：内核已完全 C 化，C 是唯一实现。
# 若有人在 CGO_ENABLED=0 下让整包静默编过（例如加回一个纯 Go 回退），
# 就会同时存在两份语义可能分叉的实现 —— 而 C 侧对畸形 UTF-8 的解码边界
# 一旦与 Go 分叉，只表现为 rune 计数偏差（进而 token 预算与截断点偏移），
# **不会立刻暴露**。所以这里把「不许有第二条路」变成可执行的断言。
#
# 注：这不影响任何现有构建 —— waiter/initconfig/memgc 均不依赖本包
# （go list -deps 实测）；homed 本就强制 cgo。
.PHONY: check-codec-cgo-only
check-codec-cgo-only:
	@echo "== 编解码层：完全 C 化检查 =="
	@CGO_ENABLED=1 $(GO) test -count=1 ./internal/agent/api/ \
		&& echo "  ① cgo 下测试全绿（含黄金对照）: OK"
	@if CGO_ENABLED=0 $(GO) build ./internal/agent/api/ 2>/dev/null; then \
		echo "  [FAIL] CGO_ENABLED=0 下本包竟然构建成功——"; \
		echo "         编解码层已完全 C 化，不该存在第二条实现路径。"; \
		echo "         若是有意引入回退，请同时更新本检查与 codec_cgo.go 的说明。"; \
		exit 1; \
	else \
		echo "  ② CGO_ENABLED=0 下响亮失败（防静默回退）: OK"; \
	fi

run: build
	./$(BUILD_DIR)/$(BINARY) -data /tmp/hologram

fmt:
	$(GO) fmt ./...

lint:
	$(GO) vet ./...

# 客户端版本与内核版本同步（唯一事实源 internal/meta.Version）。
# GUI/鸿蒙各有自版本字段，手工改必漂——用脚本拉齐，check 版给门禁用。
sync-client-versions:
	@bash deploy/scripts/sync-client-versions.sh

check-client-versions:
	@bash deploy/scripts/sync-client-versions.sh --check

# check-starmap-shared：钉住 WebUI 与 GUI 的星图布局**同源**。
#
# 为何需要：两份客户端内联脚本里的布局代码曾是手抄副本，已经因此出过两次事故：
#   ① WebUI 修好 O(n²)→网格法，GUI 那份没跟着改（换客户端卡顿原样存在）
#   ② 抄空闲帧跳过时抄了 WebUI 独有的 starmapMorphSet，GUI 实际用
#      state.starmapGrown ⇒ 渲染循环每帧 ReferenceError、星图 0 帧
# 现在唯一事实源在 shared/starmap/，由 tools/starmap-sync.mjs 写进两处标记块。
# 本目标校验「两处标记块 == 共享源」；手改任一副本会立刻红。
sync-starmap:
	@node tools/starmap-sync.mjs

check-starmap-shared:
	@node tools/starmap-sync.mjs --check

# check-client-protocol：客户端↔核心的协议一致性门禁。
#
# 为何单列一个显式目标（虽然这些用例在 go test ./... 里也会跑）：
# 它们是**跨客户端契约**判据，改协议时最该被单独、反复地跑。2026-10-13 的
# 协议审计查出四处静默不一致（幽灵类型 agent_error、system 发了没人收、
# CLI 缺 stage、设备桥静默丢弃 push），全部靠这类判据才能防复发。
check-client-protocol:
	@$(GO) test -count=1 -run 'TestSSESubTypesAreRealEventTypes|TestEveryPublishedEventHasAnOutwardPath|TestClientsRecognizeServerEventTypes|TestNoPhantomFrameTypes|TestStageFrameFieldsMatchServer|TestDeviceBridgeOpCoverage|TestSseWhitelistParsesToExpectedSet' ./internal/plugins/webui/

# lint-full：在 vet 之外跑 golangci-lint（阈值见 .golangci.yml，起步 warn-only）。
# 未安装时给出可执行的安装提示与跳过原因，而不是静默成功。
.PHONY: lint-full
lint-full:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint 未安装，跳过（阈值见 .golangci.yml）"; \
		echo "  go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; \
	fi
