#!/usr/bin/env python3
"""CI 门禁自检：断言 ci.yml 的关键机制仍在。退出码 0 = 通过。

## 为什么用 YAML 解析，而不是 grep 文本

本次改动的注释里**刻意保留了旧写法的引述**（例如
「原先这里写死 ./internal/agent/core/ ./cmd/waiter/ 两个包」）。
于是 grep 会两头骗人：
  · 断言「旧写法不存在」→ 注释里那句引述让它**假红**
  · 断言「新机制存在」  → 注释里写一句同名机制就能让它**假绿**
yaml.safe_load 丢掉全部注释，只留可执行内容 ⇒ 两类误判同时消失。

## 这条判据防的是什么

CI 配置是「静默退化」的高发区：把动态枚举改回写死包列表，
流水线照样绿（只是罩得少了），没人会发现。所以必须有人盯着
「动态枚举还在」「go test 真的吃了枚举结果」这两件事。
"""
import sys

import yaml

PATH = ".github/workflows/ci.yml"
fails = []


def check(cond, msg):
    if not cond:
        fails.append(msg)


with open(PATH, encoding="utf-8") as f:
    doc = yaml.safe_load(f)

jobs = (doc or {}).get("jobs") or {}
check(bool(jobs), "YAML 解析后没有 jobs —— 文件结构坏了")

LEGACY_RACE_PKGS = "./internal/agent/core/ ./cmd/waiter/"


def code_lines(run):
    """只保留**可执行**行：剥掉 shell 注释。

    ★ 为什么必须这么干（本条判据自己栽过的坑）：
      YAML 解析只丢掉了 YAML 级注释，但 `run: |` 块里的 `# ...`
      是**字符串内容**，照样落在 run 文本里。
      于是 `"cmd/gui" in run` 会被一句说明性注释满足 ——
      真把 `grep -v '^.*/cmd/gui$'` 删掉，断言仍然绿。
      判据自己被注释骗过去，和被测代码骗判据是同一类错误。
    """
    out = []
    for line in run.splitlines():
        if line.lstrip().startswith("#"):
            continue
        out.append(line)
    return "\n".join(out)


def step_runs(job):
    out = []
    for st in (jobs.get(job) or {}).get("steps") or []:
        run = st.get("run")
        if run:
            out.append((st.get("name") or "", run))
    return out


# ── 1. race job：包列表必须动态枚举，并真的剔除 cmd/gui ──
race = step_runs("race")
enum = [(n, r) for n, r in race if "go list ./..." in r and "GITHUB_OUTPUT" in r]
check(len(enum) == 1, "race job 应恰有一步用 `go list ./...` 动态枚举包（实际 %d 步）" % len(enum))
if enum:
    # ★ 用 code_lines 剥掉 shell 注释后再断言，否则注释里的 "cmd/gui" 会顶包
    check("cmd/gui" in code_lines(enum[0][1]),
          "race 枚举步骤必须显式剔除 cmd/gui（否则 go test 报 no Go files 的假红）")

# ── 2. 真正的 go test -race 必须吃枚举结果，不许写死包列表 ──
#
# ★ 只认「全仓主步骤」：race job 里还有一步是**单包取证**（只跑 webui 那一条
#   已知竞争），它本来就不该吃全仓枚举。初版脚本没区分这两类，把取证步骤
#   也套上「必须吃枚举」的断言 ⇒ 误报红。判据自己误报同样是缺陷（会让人
#   习惯性忽略红），所以这里按「步骤是否消费 PKGS 变量」来分流。
main_race = [(n, r) for n, r in race
             if "go test -race" in r and "$PKGS" in r]
check(len(main_race) == 1,
      "race job 应恰有一步全仓 go test -race 消费 $PKGS（实际 %d 步）" % len(main_race))
for _n, r in main_race:
    check("steps.pkgs.outputs.pkgs" in code_lines(r),
          "全仓 go test -race 必须使用枚举结果 steps.pkgs.outputs.pkgs")
    check(LEGACY_RACE_PKGS not in code_lines(r),
          "全仓 go test -race 里出现了旧的写死包列表 " + LEGACY_RACE_PKGS)

# 取证步骤：允许单包跑，但必须写明修复单号（否则排除项成了无主的掩盖）
evid = [(n, r) for n, r in race if "go test -race" in r and "$PKGS" not in r]
for _n, r in evid:
    check("DATA RACE" in r,
          "取证步骤应显式检查 DATA RACE 报告数（否则取证没意义）")

# ── 3. skip 排除表：真跑一次坏输入，要求它必须失败 ──
#
# ★ 这里换过三版，每一版的失效方式都值得记：
#   v1 `any("没有写原因" in r ...)`        —— 只查提示文案；把 if 条件改 false 仍绿
#   v2 剥注释后查 `${reason// /}` + `exit 1` —— 两个 token 都在仍可能不成立；
#      实测 `if [ -z "${reason// /}" ] && false; then` 依然绿
#   v3 真跑、但拿真实 webui 那一行做坏输入 —— 该排除项修好被删掉时**误报红**
#   v4（本条）用**合成行**注入 TABLE 再真跑：
#      · 不依赖任何真实排除项 ⇒ 排除项增删不会引起误报
#      · 真跑 ⇒ `&& false` 这类「形似而无效」的改动会被抓出来
#   v5（ID:19）堵空表假绿：v4 的 `(.*?)` 非贪婪且允许空串 ⇒ 排除表被删到
#      空串时捕获体为空，下游只看 "$n" -eq 0 就静默走「无排除项」分支，
#      排除项漏删/误删永远不会被任何人发现。现在要求**空表必须显式声明**
#      （heredoc 里保留一行 `# none`），且下列三条各自有只违反它的坏输入做自测。
#
# 两条守卫必须**各自**用只违反它的输入来验，否则一条会顶另一条的包
# （第一版我把 reason 与 ticket 同时清空，ticket 守卫兜住了，reason 守卫死没死看不出来）。
import subprocess as _sp
import tempfile as _tf

# 空表显式标记：与 ci.yml 里的判据同一串（改一处必须同时改另一处）。
_EMPTY_MARK = "# none"

_tbl = None
for _n, _r in race:
    if "计算 -race 排除表并打印" in (_n or ""):
        _tbl = _r
if _tbl is None:
    fails.append("race job 找不到「计算 -race 排除表并打印」步骤 —— 无法自测排除表守卫")
elif "${{" in _tbl:
    fails.append("排除表步骤含 GitHub 表达式，本脚本无法本地执行它（需改用别的验证方式）")
else:
    import re as _re
    _m = _re.search(r"read -r -d '' TABLE <<'EOF'[^\n]*\n(.*?)\nEOF\n", _tbl, _re.DOTALL)
    if _m is None:
        fails.append("排除表步骤里找不到 TABLE heredoc —— 无法注入合成坏行")
    else:
        def _run_body(body):
            fd, tmp_sh = _tf.mkstemp(suffix=".sh")
            with __import__("os").fdopen(fd, "w", encoding="utf-8") as f:
                f.write(body)
            out = _tf.mktemp()
            env = dict(__import__("os").environ)
            env["GITHUB_OUTPUT"] = out
            try:
                return _sp.run(["bash", tmp_sh], capture_output=True, text=True, env=env)
            finally:
                __import__("os").unlink(tmp_sh)
                if __import__("os").path.exists(out):
                    __import__("os").unlink(out)

        def _inject(line):
            inner = _m.group(1)
            new_inner = (line + "\n" + inner) if inner.strip() else line
            return _tbl[:_m.start(1)] + new_inner + _tbl[_m.end(1):]

        def _replace_table(new_inner):
            """把 heredoc 的捕获体整段替换成 new_inner（用来造各种空表形态）。"""
            return _tbl[:_m.start(1)] + new_inner + _tbl[_m.end(1):]

        # 正向：原样必须通过
        if _run_body(_tbl).returncode != 0:
            fails.append("排除表步骤用当前 ci.yml 实跑不通过（正向基线坏）")

        # ── 正向：合法空表（显式 `# none` 标记）必须通过 ──
        # ★ 这是"排除表可以合法为空"的必要性证明：最后一条 pkg 行被删掉后，
        #   表本来就该空，不能一空就红。
        if _run_body(_replace_table(_EMPTY_MARK)).returncode != 0:
            fails.append("合法的空表（显式 `# none` 标记）竟被拒 —— 空表本应被允许，"
                         "守卫不能把「排除项删干净」与「漏写标记」混为一谈")

        # ── 反向：空体没有任何标记 ⇒ 必须红（这就是 ID:19 的假绿入口）──
        #
        # ★ 自测自身的可被改坏性（ID:19 变异 M8/M8d 抓出来的缺口）：
        #   初版把四条反向样本放在一个 for 循环里。把容器改成 `[] and [...]`
        #   或 `[] or [...]` 后循环空转、一条都不跑，而**没人发现**
        #   —— 实测那份变异下本脚本 exit=0、零失败（自测静默死掉）。
        #   补一道"旁路重算 _ran"也不够：M8d 证明旁路能正常数出 4/4，
        #   而真正做判断的那段已被空转（两份逻辑脱钩了）。
        #
        # ⇒ 结论：**不用可被改坏的容器驱动断言**。把四条显式展开，
        #   样本集只当"取值源"用（下标直取），不存在"改坏 for 容器"这个面。
        #   这样只剩两种可能：样本被删（len 断言报）或某条真的没咬住（逐条报）。
        _EMPTY_BAD_CASES = [
            ("完全空体", ""),
            ("只有空行", "\n"),
            ("只有空行+空白", "\n   \n"),
            ("非显式标记的注释", "# 无排除项"),
        ]
        if len(_EMPTY_BAD_CASES) != 4:
            fails.append("空表反向样本数变了（期望 4，实际 %d）—— 除新增样本，否则说明样本被删"
                         % len(_EMPTY_BAD_CASES))

        _rejected_checked = [0]  # 用 list 当可变计数（闭包内可写）

        def _must_reject(idx):
            """断言第 idx 个空表形态**必须**被拒。"""
            _rejected_checked[0] += 1
            if idx >= len(_EMPTY_BAD_CASES):
                # 样本被删：不抛 IndexError，给可读报错（崩溃不如可读断言好）。
                fails.append("空表反向样本集缺第 %d 条（只剩 %d 条）—— 样本被删或容器被改坏"
                             % (idx, len(_EMPTY_BAD_CASES)))
                return
            _n, _i = _EMPTY_BAD_CASES[idx]
            if _run_body(_replace_table(_i)).returncode == 0:
                fails.append(
                    "空表形态「%s」竟通过 ⇒ 空表显式声明守卫已失效"
                    "（表被删空与有意清空无法区分，排除项漏删会静默变绿）" % _n)

        _must_reject(0)  # 完全空体
        _must_reject(1)  # 只有空行
        _must_reject(2)  # 只有空行+空白
        _must_reject(3)  # 非显式标记的注释

        # ★ 四次调用本身也必须被核对（ID:19 变异 M8d）：
        #   把上面四行 `_must_reject(...)` 整段删掉/注掉时，前面所有断言代码都还在，
        #   但**一次都不会跑** —— 自测静默死掉、门禁照绿（实测 exit=0）。
        #   所以把调用次数当成一份合同：删一行也红，不会默默少检。
        if _rejected_checked[0] != 4:
            fails.append("空表反向自测只调用了 %d/4 次 —— 有检查被删/跳过（自测静默失效）"
                         % _rejected_checked[0])

        _probe = "test|internal/probe/synthetic|TestSyntheticProbe"
        # 只打 reason：reason 空、ticket 有效
        if _run_body(_inject(_probe + "||PROBE-TICKET")).returncode == 0:
            fails.append("合成行「reason 空 / ticket 有效」竟通过 ⇒ reason 守卫已失效"
                         "（形似而无效的改动，例如 if 条件加 && false）")
        # 只打 ticket：reason 有效、ticket 空
        if _run_body(_inject(_probe + "|合成探针原因|")).returncode == 0:
            fails.append("合成行「reason 有效 / ticket 空」竟通过 ⇒ ticket 守卫已失效")

# ── 4. go job：测试归拢门 + 包依赖门必须都在 ──
#
# 2026-11-05：整机启动图判据（bootgraph_test.go）等白盒测试已全部删除，
# 原先「必须单列一步跑 kernelmod/all」的断言随之作废 —— 盯着一个不再
# 存在的步骤就是永远红的空转。改为盯两道新的构建期静态门。
gojob = step_runs("go")
allgo = "\n".join(r for _n, r in gojob)
check("源码目录不得有 _test.go" in allgo or
      "find internal cmd pkg providers deploy -name '*_test.go'" in allgo,
      "go job 缺少「测试归拢」硬门（源码目录不得有 _test.go）")
check("go list -deps" in allgo,
      "go job 缺少「包依赖门」（go list -deps 属工具链，不写在 go test 里）")

# ── 5. cross job：cmd 清单必须实测枚举，不许写死 ──
cross = step_runs("cross")
pure = [r for _n, r in cross if "CGO_ENABLED=0 go build" in r and "GITHUB_OUTPUT" in r]
check(bool(pure), "cross job 缺少「枚举可纯交叉编译的 cmd」步骤")
builds = [(n, r) for n, r in cross if "go build -trimpath" in r]
check(bool(builds), "cross job 里没有真正的 go build 步骤")
for _n, r in builds:
    check("steps.pure.outputs.cmds" in r,
          "交叉构建必须吃枚举结果 steps.pure.outputs.cmds，不许写死 cmd 列表")

# ── 4.5 SDK 判据步骤：go test 的包参数必须**真的含测试** ──
#
# 真实事故（2026-11-16）：ci.yml 走 `go test ./sdk/ ./meta/`，而 SDK 仓在
# 3a38ef5 把测试搬到 `sdk/test/`。于是该步骤打印 "[no test files]" 却
# **退出码 0** —— CI 每轮报「SDK 判据通过」，33 个 RUN 一个都没跑。
#
# 判法：抽出 SDK 判据步骤里所有 `./xxx` 形式的包参数，逐个检查该目录下
# 真有 *_test.go。写了个空目录就红。
import glob
import os

sdk_runs = []
for job in jobs.values():
    for st in (job or {}).get("steps") or []:
        name = st.get("name") or ""
        run = st.get("run") or ""
        if "SDK" in name or "hologramagentsdk" in run:
            if "go test" in code_lines(run):
                sdk_runs.append(run)

check(bool(sdk_runs), "找不到跑 go test 的 SDK 判据步骤（ci.yml 结构变了？）")
_sdkroot = "third_party/hologramagentsdk"
for run in sdk_runs:
    targets = [t for t in code_lines(run).split()
               if t.startswith("./") and "/meta" not in t and t != "./"]
    check(bool(targets), "SDK 判据的 go test 里没抽出包参数")
    for t in targets:
        d = os.path.join(_sdkroot, t.lstrip("./").rstrip("/"))
        pat = glob.glob(os.path.join(d, "*_test.go"))
        check(bool(pat),
              "SDK 判据指向 %s，但那里没有 *_test.go —— 步骤会打印 "
              "[no test files] 而退出码 0（判据静默不跑）" % t)

# ── 4.6 protocol method 常量 vs 能力登记表：跨文件对账（2026-11-16 新增）──
#
# ## 为什么必须在 CI 里对（不能只靠 Go 判据）
#
# `allows()` 对**未登记**的 method 返回 `(true, CapCore)` —— 按基础能力放行。
# 于是「新增一个 method 常量却忘了在 methodCapability 里登记」的结果不是报错，
# 而是**权限梯度静默少一格**：那个 method 变成谁都能调。
#
# Go 侧判据做不到这件事：它只能比对自己那份**手抄的 method 列表**，
# 新增常量不会自动进去（实测：加一个 MethodNewForgotten，判据全绿）。
#
# 所以对账放在 CI：两侧都是源码文本，扫一遍就能看见漂移。
# 与 endpoint-align 同型 —— 运行时探测反而看不到「源码已加、登记还没加」那个窗口。
#
# ## 排除项（内核→插件方向，不经过 allows，因此不需要登记）
#
# 它们由内核主动调用插件（`p.proc.Call(...)`），从不进 corehandler.Handle。
# 这不是遗漏，是方向不同 —— 列在这里让决策可见。
_KERNEL_TO_PLUGIN = {
    "MethodCleanerInvoke", "MethodOutputInvoke", "MethodPluginInit",
    "MethodPluginStart", "MethodPluginStop", "MethodToolInvoke", "MethodStageInvoke",
}
import re as _re

_proto = open("internal/plugin/proc/protocol.go", encoding="utf-8").read()
_cap = open("internal/plugin/proc/capability.go", encoding="utf-8").read()

# protocol.go 里所有 method 常量名（`MethodXxx = "..."`）
_proto_methods = set(_re.findall(r'^\t?(Method[A-Za-z0-9]+)\s*=', _proto, _re.M))
# capability.go 的 methodCapability 表里登记过的（`MethodXxx: Cap...`）
_cap_registered = set(_re.findall(r'^\t?(Method[A-Za-z0-9]+)\s*:\s*Cap', _cap, _re.M))

check(len(_proto_methods) > 50,
      "protocol.go 只抽到 %d 个 method 常量 —— 正则与文件结构漂移了" % len(_proto_methods))
check(len(_cap_registered) > 50,
      "capability.go 只抽到 %d 条登记 —— 正则与文件结构漂移了" % len(_cap_registered))

_forgotten = sorted(_proto_methods - _cap_registered - _KERNEL_TO_PLUGIN)
check(not _forgotten,
      "以下 protocol method 常量**未登记能力组**（allows() 会按 CapCore 放行，"
      "权限梯度静默少一格）：%s" % ", ".join(_forgotten))

_stale = sorted(_cap_registered - _proto_methods)
check(not _stale,
      "capability.go 登记了 protocol.go 里不存在的 method：%s" % ", ".join(_stale))

# ── 4.7 SDK 与主仓的双份副本：必须逐字节一致（2026-11-16 新增）──
#
# SDK 仓独立发布，但主仓有一份**镜像副本**供 Lua 插件在内核内使用：
#
#   internal/lua/sdk/sdk.lua                    （内核侧，Lua 插件 require 它）
#   third_party/hologramagentsdk/sdk/lua/sdk.lua （SDK 仓，对外发布）
#
# 此刻 md5 相同（2be4cb73…），但**没有任何东西守着它们继续相同**。
# 改一边忘另一边 ⇒ Lua 插件跑一套 API、文档说另一套，且两边都"能跑"。
#
# 这类漂移没法用 Go 判据抓（两份都在各自 module 之外），只能在 CI 对账。
import hashlib
import os

_DUALS = [
    ("internal/lua/sdk/sdk.lua", "third_party/hologramagentsdk/sdk/lua/sdk.lua"),
]
for a, b in _DUALS:
    if not (os.path.exists(a) and os.path.exists(b)):
        fails.append("双份副本缺文件：%s / %s" % (a, b))
        continue
    ha = hashlib.sha256(open(a, "rb").read()).hexdigest()
    hb = hashlib.sha256(open(b, "rb").read()).hexdigest()
    check(ha == hb,
          "%s 与 %s 内容不一致（sha256 %s… vs %s…）—— "
          "改一边忘另一边会让 Lua 插件与文档跑两套 API" %
          (a, b, ha[:12], hb[:12]))

# ── 4.8 设施名：内核 vs 公开 SDK 必须逐字一致（2026-11-16 新增）──
#
# `internal/kernelmod/facilities.go` 的注释写着「★ 判据
# TestFacilityNamesMatchPublicSDK 守这个一致性」，而**那个判据不存在**
# （全仓 grep = 0）。SDK 侧同样写着。两边都靠人眼比对。
#
# 而设施名是 plugin.json 的 `requires_facilities` 契约：拼错就是
#'  "插件永远等不到就绪"（运行期静默挂起，极难排查）。
#
# 两侧都是 Go 常量字符串，扫一遍对账即可。
import re as _re2

# ★ 设施名的**唯一声明点**已搬到 internal/kernelapi（纯接口叶子包）：
# 常量与能力接口必须同居一个叶子，消费方才可能"只 import 接口"。
# internal/kernelmod/facilities.go 保留同名常量但改为转发（对外 SDK 契约不变）。
# ⇒ 对账必须扫**声明点**，否则会抽到 0 个（本门禁 2026-11-17 由此红过一次）。
_kfac = open("internal/kernelapi/kernelapi.go", encoding="utf-8").read()
_sfac = open("third_party/hologramagentsdk/sdk/facilities.go", encoding="utf-8").read()

# 内核侧：Fac<Name> = "<value>"（只取「插件可依赖」那一段之前的部分）
_kernel_names = set(_re2.findall(r'^\t(Fac[A-Za-z0-9]+)\s*=\s*"([^"]+)"', _kfac, _re2.M))
# SDK 侧：Facility<Name> = "<value>"
_sdk_names = set(_re2.findall(r'^\t(Facility[A-Za-z0-9]+)\s*=\s*"([^"]+)"', _sfac, _re2.M))

check(len(_kernel_names) > 10, "kernelmod/facilities.go 只抽到 %d 个设施" % len(_kernel_names))
check(len(_sdk_names) > 10, "sdk/facilities.go 只抽到 %d 个设施" % len(_sdk_names))

# 比对**字符串值**（常量名两侧不同是设计：FacMemory vs FacilityMemory）
_kvals = {v for _n, v in _kernel_names}
_svals = {v for _n, v in _sdk_names}
# 内核内部顺序标记（不是给插件的契约，SDK 侧不该有）—— 它们在
# 「内核内部顺序标记」那段注释之后，值形如 "migration"/"knowledge-migration" 等。
# 用白名单排除：只要求「SDK 有的，内核必须有」+「内核对外那组，SDK 必须有」。
_missing_in_kernel = sorted(_svals - _kvals)
check(not _missing_in_kernel,
      "公开 SDK 声明了内核不存在的设施：%s —— 插件照它声明就会永远等不到就绪"
      % ", ".join(_missing_in_kernel))

# ── 4.9 README 事实漂移：Go 版本 / 内置插件数（2026-11-16 新增）──
#
# `README.md` 曾写「28 个内置插件、89 个模型可调工具」与「Go 1.25+」。
# 实测：`internal/plugins/all.go` 的 import 是 **12** 个，`go.mod` 是 **1.27.1**。
#
# 这类数字没人守着就会持续漂 —— 而 README 是外部读者的第一入口，
# 写错等于对外承诺了不存在的东西。
#
# ## 只钉"能机械核算的两项"
#
# 工具数**故意不钉**：它随用户装了哪些外部插件而变（生产实测 209，空装是 0），
# 钉死必然很快过期。README 现在改成了「不钉具体工具数」的写法。
import re as _re3

_gomod = open("go.mod", encoding="utf-8").read()
_m = _re3.search(r'^go\s+([0-9]+)\.([0-9]+)', _gomod, _re3.M)
check(_m is not None, "go.mod 里找不到 go 指令")
_gover = "%s.%s" % (_m.group(1), _m.group(2))

_allgo = open("internal/plugins/all.go", encoding="utf-8").read()
_builtin_imports = _re3.findall(
    r'^\t_ "github\.com/JianFeeeee/HologramAgent/internal/plugins/([a-z0-9_]+)"',
    _allgo, _re3.M)
_nbuiltin = len(_builtin_imports)

for _readme in ("README.md", "README_EN.md"):
    _txt = open(_readme, encoding="utf-8").read()
    # Go 版本：形如 "Go 1.25+" / "Go 1.27+"；只认主版本+次版本
    _vers = _re3.findall(r'Go\s+([0-9]+\.[0-9]+)\+', _txt)
    check(_vers, "%s 里没找到「Go <版本>+」的表述 —— 若是有意删掉，同步删本条门禁" % _readme)
    for _v in _vers:
        check(_v == _gover,
              "%s 写「Go %s+」，而 go.mod 是 **%s** —— 对外承诺了错的版本要求"
              % (_readme, _v, _gover))

# 内置插件数：README 里形如 "**12 个内置插件**"
_txt_zh = open("README.md", encoding="utf-8").read()
_nums = _re3.findall(r'\*\*([0-9]+) 个内置插件\*\*', _txt_zh)
check(_nums, "README.md 里没找到「**N 个内置插件**」—— 若是有意改写法，同步删本条门禁")
for _n in _nums:
    check(int(_n) == _nbuiltin,
          "README.md 写「%s 个内置插件」，而 internal/plugins/all.go 的 import 是 **%d** 个"
          "（%s）" % (_n, _nbuiltin, ", ".join(sorted(_builtin_imports))))

# ── 4.10 注释不得引用被 gitignore 的文件（2026-11-16 新增）──
#
# 实测发现 35 处 Go 注释写着「见 plan.md 十一」「plan.md §D4」「plan.md 十一.三 B」，
# 而：
#   · `.gitignore:101` 有 `/plan.md` ⇒ 文件不在仓库里，读者打不开
#   · 现存 plan.md（本机）是「rtchild 标准化实施计划」，grep '十一' = **0**
#   · `git log --all -- plan.md` 的历史版本同样是 0 ⇒ 那些章节**从未入库过**
#
# 于是这 35 处指向的是"不存在、也从未存在过的章节"。本次已全部改写为自足说明。
#
# 这条门禁守的是同一个坑不再出现：注释里引用一个 gitignore 掉的文件，
# 等于把读者引到死路，而 CI 不会因此变红。
import os as _os4
import re as _re4

_gi = open(".gitignore", encoding="utf-8").read()
_ignored_root_files = set()
for _line in _gi.splitlines():
    _line = _line.strip()
    if not _line or _line.startswith("#"):
        continue
    # 形如 "/plan.md" 或 "plan.md" —— 只收根目录下的 .md
    _m = _re4.match(r'^/?(\S+\.md)$', _line)
    if _m:
        _ignored_root_files.add(_os4.path.basename(_m.group(1)))

check(_ignored_root_files, ".gitignore 里一个根目录 .md 都没抽到 —— 正则与文件结构漂移了")

# 只扫源码注释里的「见 X.md」/「X.md 第/§」。跳过 third_party 的文档引用
# （那些指向 docs/zh/ 下真实存在的文件，单独由 docs 构建门守）。
_src_roots = ["internal", "cmd", "pkg", "providers"]
_bad = []
for _root in _src_roots:
    for _dir, _dirs, _files in _os4.walk(_root):
        for _f in _files:
            if not _f.endswith(".go"):
                continue
            _path = _os4.path.join(_dir, _f)
            try:
                _txt = open(_path, encoding="utf-8").read()
            except Exception:
                continue
            for _ln, _content in enumerate(_txt.splitlines(), 1):
                _c = _content.strip()
                if not (_c.startswith("//") or _c.startswith("*")):
                    continue
                for _ref in _ignored_root_files:
                    # 只认"像文档引用"的形态：文件名 + 后续是 §/第/句末
                    if _ref in _c and _re4.search(
                            _re4.escape(_ref) + r'(\s|$|\u3001|\uff09|\))', _c):
                        _bad.append("%s:%d 引用了被 gitignore 的 %s" % (_path, _ln, _ref))
                        break

check(not _bad,
      "注释引用了不随仓库分发的文档（读者打不开）：\n    " +
      "\n    ".join(sorted(set(_bad))[:10]))

# ── 4.11 测试归拢的**另一半**：测试目录必须在 <module>/test/ 且是外部包 ──
#
# ci.yml 的「测试归拢」步骤只查了「源码目录不得有 _test.go」。它守的是
# 反方向的一半 —— 文件搬出去了就算过，而**搬到哪儿、用哪个包名**没守。
#
# 于是这两种退化都能过门禁：
#
#	internal/foo/bar_test.go       → 被第一条挡住（对）
#	internal/foo/test_test/x_test.go → 第一条放过（它在 /test/ 路径下），
#	                                  但那个包名不是 test，也不是 foo 的直接子目录
#	internal/foo/test/x_test.go 写 package foo → 第一条放过，
#	                                  但它成了**白盒**测试（能读私有符号）
#
# 「白盒测试必须整目录一起搬」这条纪律的判据只有 import 关系**看不出来**，
# 只有包名能看出来。所以在这里机械地核一遍。
import os as _os5
import re as _re5

_bad_pkg = []
_bad_dir = []
for _root in ("internal", "cmd", "pkg", "providers"):
    for _dir, _dirs, _files in _os5.walk(_root):
        # node_modules 里的第三方包自带 test 目录，与本仓纪律无关。
        _dirs[:] = [d for d in _dirs if d != "node_modules"]
        for _f in _files:
            if not _f.endswith("_test.go"):
                continue
            _path = _os5.path.join(_dir, _f)
            try:
                _first = open(_path, encoding="utf-8").readline().strip()
            except Exception:
                continue
            if _first != "package test":
                _bad_pkg.append("%s -> %s" % (_path, _first or "(空)"))

# 测试目录必须**恰好是某个模块的直接子目录 `test/`**，且不能再往下嵌套。
#
# 判据（两条都机械可核）：
#   ① `<module>/test/` 里必须有 `_test.go`（空目录是搬走时漏了收尾）
#   ② 一个模块的测试不得散进 `test/` 的子目录（那样 `go test ./<module>/test/`
#      覆盖不到，而 CI 的 `go test ./...` 会跑到、看起来一切正常）
#
# ⚠️ 第二版才修对：第一版只查了「目录名是 test 且父目录名也是 test」，
# 而 `internal/log/test/sub/` 的父目录名是 `test`、它自己是 `sub`
# ⇒ 两个条件都不命中，变异实测**放过了**。现在改成扫 test/ 的**全部子孙**。
for _root in ("internal", "cmd", "pkg", "providers"):
    for _dir, _dirs, _files in _os5.walk(_root):
        _dirs[:] = [d for d in _dirs if d != "node_modules"]
        if _os5.path.basename(_dir) != "test":
            continue
        # ⚠️ 不查"空 test 目录"：git 不跟踪空目录，CI checkout 里根本不存在，
        # 那种形态**测不到**（本地造出来是假红）。真要守住搬走后留空壳，
        # 得在本地 `go vet ./...` 或独立脚本里做，不属于 CI 门禁。
        # 扫这个 test/ 下面的每条子路径
        for _sub, _subdirs, _subfiles in _os5.walk(_dir):
            if _sub == _dir:
                continue
            if _subdirs:
                _subdirs[:] = [d for d in _subdirs if d != "node_modules"]
            if any(_x.endswith("_test.go") for _x in _subfiles):
                _bad_dir.append(
                    "%s（测试散进 test/ 的子目录 —— 该模块的 `go test ./test/` "
                    "只跑顶层，这一层会被漏掉）" % _sub)

check(not _bad_pkg,
      "以下测试文件不是外部测试包 `package test`（白盒测试能读私有符号，"
      "违反「从模块导出接口验证」）:\n    " + "\n    ".join(_bad_pkg[:10]))
check(not _bad_dir,
      "测试目录结构不合规（必须恰好是 <module>/test/）:\n    " +
      "\n    ".join(_bad_dir[:10]))

# ── 4.12 死代码：导出的**包级函数**零引用（2026-11-16 新增）──
#
# ## 为什么只查「导出的包级函数」
#
# 全仓扫「零引用」的尝试有两个高误报来源，都实测过：
#
#   ① 导出的**方法**：接口方法通过接口值调用时，文本搜索看不到具体接收者
#      （`s.bus.AllReady` 是 `kstate.SyncState.AllReady`，grep 命中不了）。
#   ② cgo / build tag 文件：`deadcode` 工具扫全仓报了 **297 条**，其中
#      `codec_cgo.go` 里 20 多条 C 侧回调在 Go 侧「不可达」是**正常的**。
#      直接拿它当硬门禁会一上来就 297 条假红。
#
# 所以门禁只查**最高置信度的子集**：
#
#   · 顶层 `func Name(...)`（不是方法 —— 避开接口实现）
#   · 去掉注释后，全仓（含测试）零引用
#   · build tag 文件**不作为候选定义**，但**算进引用语料**
#     （实测第一版把这类文件整份跳过 ⇒ 报了 12 条假红：cmd/homed/guard.go
#      是 `//go:build linux`，它对 recovery.LoadResult 有真调用）
#
# ## 白名单每项必须写理由
#
# 否则这条门禁会退化成"谁嫌它烦就把谁拉白"。
import os as _os7
import re as _re7

_DEAD_WHITELIST = {
    # 平台专用（Windows build tag，Linux CI 上根本不参与编译）
    "ConPtyEnv":     "ptywin/conpty.go 有 //go:build windows —— Linux CI 看不到它的调用方",
    "ConPtyWorkDir": "同上",
    # 公开 SDK 的对外 API（文档站收录，第三方插件可能调用）
    "ValidScenePolicy": "third_party/hologramagentsdk/sdk —— 公开 SDK 面，仓库外有调用方",
    # 给 CI 门禁自己的诊断出口：本仓 CI 脚本要求它们存在
    "CapabilityOf":         "proc/capability.go —— check-ci-gates.py 对账用的诊断出口",
    "MethodNames":          "同上",
    "WithheldCapabilities": "同上",
    # 判据的输入生成器 / 配对的可调用形态
    "BuildCentroid": "memory/centroid.go —— 判据用它造输入",
    "BlockSubject":  "memory/distill/blocks.go —— block 文本格式的解析 API（唯一可调形态）",
    "FuncModule":    "kernelboot/funcmodule.go —— 判据用它造装配输入",
    "BaseResult":    "devicebridge/client/cmdrouter.go —— 判据用它构造期望值",
    "NewTestCodec":  "config/secret.go —— 名字即用途：判据构造 codec",
    "NewHost":       "plugin/proc/host.go —— NewHostWithArenaSize 的便捷形态（生产走带容量那个）；判据用它作唯一入口，避免复制容量常量",
    "ApplyDeltaFile": "update/delta.go —— MakeDeltaFile 的读取侧配对（前者活在 MakeDeltaForRelease → cmd/homed-upgrade）。生产还原用内存版 ApplyDelta",
    "NewCodec":       "security/codec.go —— 生产走 NewCodecWithOldKeys（支持轮换），NewCodec 是它的单密钥形态",
}

_dead_prod = {}
_dead_test = {}
_dead_buildtag = set()
for _root in ("internal", "cmd", "pkg", "providers", "third_party/hologramagentsdk/sdk"):
    for _dir, _dirs, _files in _os7.walk(_root):
        # ⚠️ 不能排除 "test" 目录 —— 测试就在 <module>/test/ 里，
        # 排除它会让 _dead_test 永远是空的，「只有测试引用」的符号会全被判成零引用。
        _dirs[:] = [d for d in _dirs if d not in ("node_modules", "testdata")]
        for _f in _files:
            if not _f.endswith(".go"):
                continue
            _path = _os7.path.join(_dir, _f)
            try:
                _raw = open(_path, encoding="utf-8").read()
            except Exception:
                continue
            _has_tag = "go:build" in "\n".join(_raw.splitlines()[:3])
            _txt = _re7.sub(r"/\*.*?\*/", " ", _raw, flags=_re7.S)
            _txt = _re7.sub(r"//[^\n]*", " ", _txt)
            if _f.endswith("_test.go"):
                _dead_test[_path] = _txt
            else:
                _dead_prod[_path] = _txt
                if _has_tag:
                    _dead_buildtag.add(_path)

_dead_candidates = []
for _path, _txt in _dead_prod.items():
    if _path in _dead_buildtag:
        continue  # 平台专用文件里的导出函数不作候选
    for _m in _re7.finditer(r"^func ([A-Z][A-Za-z0-9_]*)\s*[\[(]", _txt, _re7.M):
        _dead_candidates.append((_path, _m.group(1)))

_dead = []
for _path, _name in _dead_candidates:
    if _name in _DEAD_WHITELIST:
        continue
    _pat = _re7.compile(r"\b" + _re7.escape(_name) + r"\b")
    _prod_refs = sum(len(_pat.findall(t)) for t in _dead_prod.values())
    _test_refs = sum(len(_pat.findall(t)) for t in _dead_test.values())
    # 定义处自身算 1 次命中。
    # ⚠️ **只有测试引用也算死**：§0 第 5 条明令「不得为测试导出符号」。
    # 本仓已因此栽过一次（agent.SavePersonality 生产零调用、4 处判据在用）。
    if _prod_refs <= 1:
        _dead.append("%s: %s%s" % (
            _path, _name,
            "" if _test_refs == 0 else "（仅测试引用 %d 处 —— 不得为测试导出符号）" % _test_refs))

check(not _dead,
      "以下导出的包级函数**全仓零引用**（含测试）—— 死代码应当删掉，"
      "而不是留着一个无人调用的导出面。若确需保留（平台专用/公开 API/判据输入），"
      "把理由写进 check-ci-gates.py 的 _DEAD_WHITELIST：\n    " + "\n    ".join(sorted(_dead)[:12]))

# ── 4.13 插件不得直接 import 内核策略包（2026-11-16 新增）──
#
# ## 这条守的是一条**架构边界**，不是风格
#
# 插件的职责是"提供事实 + 调 SDK"，策略判定必须在内核（security 模块）。
# 一旦某插件自己 import 内核策略包，它就能：
#
#	· 自己判沙箱（被约束者决定约束）—— 本仓实测栽过：QQ 的 beforeToolcall
#	  自己判身份/白名单/硬私有，结果拒绝语义散落、插件崩了策略就没了
#	· 绕过 SDK 的声明面直接读写策略对象
#
# 而且**外部插件根本 import 不到**（独立 module / 独立进程）—— 所以这类
# import 只在内置插件里出现，会造成"内置版能做到、外部版做不到"的语义分裂，
# 正是 `files` 插件上一代 `archiveBeforeWrite` 的形态。
#
# 2026-11-16 实测发现：`internal/plugins/files` 直接 import `internal/system`
# 调 `ArchiveBeforeWrite` —— 而生产跑的是外部 plugin.bin，仓里那份是
# 「已迁出但源码未删」的残留。处置：留档收归内核（ToolDef.PathArgs +
# agent/core/toolguard.go），插件改为只声明参数名。
_policy_pkgs = ("internal/security", "internal/system")
_bad_plugin_imports = []
for _root in ("internal/plugins",):
    for _dir, _dirs, _files in _os5.walk(_root):
        _dirs[:] = [x for x in _dirs if x != "test"]
        for _f in _files:
            if not _f.endswith(".go") or _f.endswith("_test.go"):
                continue
            _path = _os5.path.join(_dir, _f)
            try:
                _src = open(_path, encoding="utf-8").read()
            except Exception:
                continue
            for _pkg in _policy_pkgs:
                if '"github.com/JianFeeeee/HologramAgent/%s"' % _pkg in _src:
                    _bad_plugin_imports.append("%s → %s" % (_path, _pkg))

check(not _bad_plugin_imports,
      "以下插件直接 import 了内核策略包（策略必须在内核、插件只调 SDK）：\n    " +
      "\n    ".join(sorted(_bad_plugin_imports)[:10]))

# ── 5.5 code_lines 自测：这是「注释顶包」的唯一防线，必须自己也被验 ──
#
# ★ 为什么需要这段自测（M7 的教训）：
#   原先把「code_lines 不再剥注释」当成一条变异，但它**单独注入时不会红** ——
#   因为 ci.yml 完好时，`grep -v cmd/gui` 真实存在，剥不剥注释都能匹配上。
#   那是**空变异**（行为没变，不可能红），拿它当判据缺口是误判。
#   正确做法不是造更狠的变异，而是让 code_lines 的行为**自己可被验证**：
#   喂一个"cmd/gui 只出现在注释里"的样本，要求剥离后断言为假。
#   这样把 code_lines 改成不剥注释，本条立刻失败 —— 单文件、真行为变化。
_probe = "#!/bin/bash\n# cmd/gui 只是注释里的说明\necho hi\n"
if "cmd/gui" in code_lines(_probe):
    fails.append("code_lines 没剥掉 shell 注释 —— cmd/gui 断言会被注释顶包（假绿）")
_keep = "#!/bin/bash\nALL=$(go list ./... | grep -v '^.*/cmd/gui$')\n"
if "cmd/gui" not in code_lines(_keep):
    fails.append("code_lines 把可执行行也删了 —— 断言会假红")

# ── 6. pkg 级排除（整包不进 -race）：必须带修复单号 + 兜底断言 ──
#
# 整包排除是有代价的：该包所有用例都不再受竞争检测保护。
# 因此要求：① 排除表每项必须有修复单号；② 剔除后包数有下限兜底，
# 不许"排除逻辑写坏了导致静默少测"；③ 断言排除项确实从待测列表消失。
allrace = "\n".join(r for _n, r in race)
if "KERNEL-REGISTRY-SETLOCK" in allrace or "pkg" in allrace:
    check("修复单" in allrace or "修复单号" in allrace,
          "pkg 级排除必须写明修复单号（否则成了无主的静默掩盖）")
check("-lt 50" in allrace or "lt 50" in allrace,
      "剔除后必须有包数下限兜底（防止排除逻辑写坏导致静默少测）")
check("仍出现在待测列表里" in allrace or "剔除逻辑失效" in allrace,
      "必须断言被排除的包确实从列表消失（防止写了排除但没生效）")

# ── 7. 每个 job 的每个 run 步骤：bash 语法必须过 ──
import os
import re
import subprocess
import tempfile

for jn, job in jobs.items():
    for st in job.get("steps") or []:
        run = st.get("run")
        if not run:
            continue
        body = re.sub(r"\$\{\{[^}]*\}\}", "PLACEHOLDER", run)
        fd, p = tempfile.mkstemp(suffix=".sh")
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                f.write(body)
            r = subprocess.run(["bash", "-n", p], capture_output=True, text=True)
            if r.returncode != 0:
                first = (r.stderr or "").strip().splitlines()
                fails.append("shell 语法错误 @ %s / %s: %s"
                             % (jn, st.get("name"), first[0] if first else "?"))
        finally:
            os.unlink(p)

if fails:
    print("CI 门禁自检失败：")
    for f in fails:
        print("  x " + f)
    sys.exit(1)

print("CI 门禁自检通过：动态枚举 / 排除表校验 / 测试归拢门 / 包依赖门 / 交叉枚举 / 全部 run 步骤语法")
