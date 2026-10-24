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
    _m = _re.search(r"read -r -d '' TABLE <<'EOF'[^\n]*\n(.*?)\nEOF\n", _tbl, _re.S)
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
        for _bad_name, _bad_inner in [
            ("完全空体", ""),
            ("只有空行", "\n"),
            ("只有空行+空白", "\n   \n"),
            ("非显式标记的注释", "# 无排除项"),
        ]:
            if _run_body(_replace_table(_bad_inner)).returncode == 0:
                fails.append(
                    "空表形态「%s」竟通过 ⇒ 空表显式声明守卫已失效"
                    "（表被删空与有意清空无法区分，排除项漏删会静默变绿）" % _bad_name)

        _probe = "test|internal/probe/synthetic|TestSyntheticProbe"
        # 只打 reason：reason 空、ticket 有效
        if _run_body(_inject(_probe + "||PROBE-TICKET")).returncode == 0:
            fails.append("合成行「reason 空 / ticket 有效」竟通过 ⇒ reason 守卫已失效"
                         "（形似而无效的改动，例如 if 条件加 && false）")
        # 只打 ticket：reason 有效、ticket 空
        if _run_body(_inject(_probe + "|合成探针原因|")).returncode == 0:
            fails.append("合成行「reason 有效 / ticket 空」竟通过 ⇒ ticket 守卫已失效")

# ── 4. go job：整机启动图判据必须单列一步跑 kernelmod/all ──
#
# ★ 目标包随 ID:13 从 internal/kernelboot/modules（死包，已删）迁到
#   internal/kernelmod/all（真实聚合入口）。本断言跟着改，否则它盯的
#   是一个**已不存在的包** —— 那会变成"永远红"或（更糟）"永远绿"的空转。
GO_BOOT_PKG = "./internal/kernelmod/all/"
gojob = step_runs("go")
boot = [r for _n, r in gojob if GO_BOOT_PKG in r and "go test" in r]
check(bool(boot), "go job 缺少「整机启动图判据」步骤（go test " + GO_BOOT_PKG + "）")
for r in boot:
    check("-count=1" in r, "整机启动图判据步骤必须带 -count=1（不许吃 go test 缓存）")

# ── 5. cross job：cmd 清单必须实测枚举，不许写死 ──
cross = step_runs("cross")
pure = [r for _n, r in cross if "CGO_ENABLED=0 go build" in r and "GITHUB_OUTPUT" in r]
check(bool(pure), "cross job 缺少「枚举可纯交叉编译的 cmd」步骤")
builds = [(n, r) for n, r in cross if "go build -trimpath" in r]
check(bool(builds), "cross job 里没有真正的 go build 步骤")
for _n, r in builds:
    check("steps.pure.outputs.cmds" in r,
          "交叉构建必须吃枚举结果 steps.pure.outputs.cmds，不许写死 cmd 列表")

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

print("CI 门禁自检通过：动态枚举 / 排除表校验 / 启动图判据步骤 / 交叉枚举 / 全部 run 步骤语法")
