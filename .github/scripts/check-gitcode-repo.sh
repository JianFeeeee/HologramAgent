#!/usr/bin/env bash
# 前置检查 gitcode 镜像目标仓是否存在（**有界重试**）
#
# ── 为什么要重试 ──────────────────────────────────────────────
# 2026-10-21 的 main 流水线（dfc4e16）第一次跑这一步时，gitcode 没有给出
# 预期响应（网络抖动），一次 `ls-remote` 判失败 ⇒ sync job 红；重跑立刻全绿。
# 把「一次网络抖动」翻译成「镜像同步坏了」，是纯噪声。
#
# ── 为什么不能无脑重试 ────────────────────────────────────────
# 目标仓**确实不存在**时，重试只会把「去建仓」这个真问题拖成 3 次等待 +
# 更长的红，问题反而被掩盖。所以只对**网络层**失败重试：
#
#   网络层失败（DNS / 连接重置 / TLS / 超时 / 5xx / EOF）→ 重试，最多 3 次，退避 5s、10s
#   仓不存在（gitcode 实测：`could not be found` + 403）→ 立即失败，**不重试**
#   凭据问题（401/403 但不带「不存在」特征）→ 立即失败，**不重试**
#       （token 错了等 15 秒也不会变对，只会让日志更难读）
#
# ── 口径 ──────────────────────────────────────────────────────
# ci.yml 的 sync-gitcode-main 与 release.yml 的 sync-gitcode **共用本文件**，
# 不各写一份 —— 两份逐字一致的脚本迟早会漂。
#
# ── token 安全 ───────────────────────────────────────────────
# GC_URL 里含 token，**任何情况下都不 echo 它**；git 的报错可能把 URL 的
# userinfo 带出来，故所有输出都先过 redact()。
#
# 用法：
#   GC_TOKEN=<token> [GC_REPO=owner/name] bash .github/scripts/check-gitcode-repo.sh
# 退出码：0 = 目标仓可达；1 = 不可达（原因见日志末行「判定」）
#
# 环境变量（除 GC_TOKEN 外均为可选项，CI 只设 GC_TOKEN）：
#   GC_REPO           默认 JianFeeeee/HologramAgent
#   GC_HOST           默认 gitcode.com
#   GC_CHECK_ATTEMPTS 默认 3（探测总次数上限，含首次）
#   GC_CHECK_BACKOFF  默认 "5 10"（每次失败后的等待秒数，按序取用；
#                     仅供本地判据/调试加速，CI 不设置）
set -uo pipefail

GC_REPO="${GC_REPO:-JianFeeeee/HologramAgent}"
GC_HOST="${GC_HOST:-gitcode.com}"
MAX_ATTEMPTS="${GC_CHECK_ATTEMPTS:-3}"
read -r -a BACKOFF <<<"${GC_CHECK_BACKOFF:-5 10}"

# 凭据缺失时必须响亮失败：静默匿名探测会得到 403，被误读成「仓不存在/权限不足」，
# 而不是「根本没配 token」。
: "${GC_TOKEN:?GC_TOKEN 未设置 —— 前置检查不能匿名探测}"

# ★ 这一行含 token。永远不要 echo / printf 它，也不要把它拼进任何报错。
GC_URL="https://JianFeeeee:${GC_TOKEN}@${GC_HOST}"

TRAIL=()   # 每次尝试的一行摘要，耗尽时整段回放

# redact：把日志里的凭据抹掉。两道，**先字面量、后结构**：
#
#   1) 先按字面量遮 token —— 这是最可靠的一道（token 是我们自己的串，
#      不该出现的地方一个都不该漏）。必须先把 token 里的 sed BRE 元字符
#      转义，否则 token 含 `[`/`*`/`|` 时这条规则会静默失效。
#   2) 再按 URL 结构遮 https://<userinfo>@ —— 兜底 git 回显整个 URL 的情形。
#
# ⚠️ 为什么结构式的 password 段**不能排除 `/`**：真实 token 可以含 `/`
#   （`sekret-tok+ABC/123`）。第一版写成 `[^@/[:space:]]*`，于是
#   `https://JianFeeeee:sekret-tok+ABC/123@...` 匹配不上，只剩字面量那一道兜住。
#   实测发现（判据 C6 首次就是红的）—— 这类"看起来在脱敏"的规则必须
#   用含特殊字符的 token 验。
redact() {
  local s esc
  esc=$(printf '%s' "$GC_TOKEN" | sed -e 's/[][\.^$*|]/\\&/g')
  if [ -n "$esc" ]; then
    s=$(printf '%s\n' "$1" | sed "s|${esc}|***|g" 2>/dev/null || printf '%s\n' "$1")
  else
    s="$1"
  fi
  # 结构：先遮 user:pass@（pass 段允许含 /），再遮裸 user@。
  s=$(printf '%s\n' "$s" | sed -E 's#(https?://)[^/@[:space:]]+:[^@[:space:]]+@#\1***@#g')
  printf '%s\n' "$s" | sed -E 's#(https?://)[^/@[:space:]]+@#\1***@#g'
}

# summarize：把多行 git 输出压成一行摘要（日志里一次尝试一行，便于对照）。
summarize() {
  printf '%s\n' "$1" \
    | tr '\n' ' ' \
    | sed -E 's/[[:space:]]+/ /g; s/^ //; s/ $//' \
    | cut -c1-200
}

# classify：把脱敏后的报错分成三类。**顺序重要**。
# gitcode 的「仓不存在」响应里**同时**有 could not be found 和 403：
#   remote: <CH.00905403> The project you were looking for could not be found.
#   fatal: ... The requested URL returned error: 403
# 若先判 403，就会被错误地归成「凭据问题」—— 真问题被换了个标签，
# 于是要么白等一轮重试、要么提示人去查 token，都指向错误的方向。
classify() {
  case "$1" in
    *"could not be found"*|*"does not exist"*|*"does not appear to be a git repository"*|*"not found"*|*"Not Found"*|*"404"*)
      printf 'notfound\n'
      ;;
    *"403"*|*"401"*|*"Forbidden"*|*"Unauthorized"*|*"Authentication failed"*|*"Permission denied"*)
      printf 'auth\n'
      ;;
    *)
      printf 'network\n'
      ;;
  esac
}

# report_trail：按尝试序号回放本轮所有尝试的摘要。
report_trail() {
  if [ "${#TRAIL[@]}" -gt 0 ]; then
    echo "    每次尝试的错误摘要："
    local s
    for s in "${TRAIL[@]}"; do
      echo "      ${s}"
    done
  fi
}

# fail：统一失败出口。**每次失败都要打印「判定」，让人一眼分清
# 「仓不存在」/「凭据问题」/「网络问题」** —— 三者要采取的动作完全不同。
fail() {
  local verdict="$1" attempts="$2" err="$3"
  case "$verdict" in
    notfound)
      echo "  ✗ gitcode 目标仓不存在：${GC_HOST}/${GC_REPO}（第 ${attempts} 次探测，未重试）"
      echo "    请先在该账号下创建镜像仓（只读镜像），再重跑本 job："
      echo "    https://${GC_HOST}/${GC_REPO}"
      echo "    判定：仓不存在 —— 不重试（重试只会把「去建仓」拖成十几秒空等）"
      ;;
    auth)
      echo "  ✗ 无法访问 gitcode 目标仓 ${GC_HOST}/${GC_REPO}：凭据问题（第 ${attempts} 次探测，未重试）"
      echo "    若仓已建，请确认 GITCODE_TOKEN 属于该仓的推送账号："
      echo "    https://${GC_HOST}/${GC_REPO}"
      echo "    判定：凭据问题（401/403，不带 not-found 特征）—— 不重试（等十几秒它也不会变对）"
      report_trail
      ;;
    *)
      echo "  ✗ 连续 ${attempts} 次探测 gitcode 目标仓失败：${GC_HOST}/${GC_REPO}"
      echo "    判定：网络问题（非「仓不存在」—— 那种情况会立即失败、不重试）"
      echo "    可能的原因：gitcode 侧抖动 / DNS / TLS 握手被掐 / CI 出口被限流"
      report_trail
      echo "    最后一次的完整（已脱敏）输出："
      printf '%s\n' "$err" | sed 's/^/      /'
      ;;
  esac
  # 显式失败 —— 不静默跳过，也不 echo 一句就当成功。
  exit 1
}

attempt=1

while :; do
  # 用 ls-remote 探测：与后面那次 push 走同一个 URL、同一套凭据，
  # 比另调一次 API 更贴近真实失败面。
  if out=$(GIT_TERMINAL_PROMPT=0 git ls-remote --heads \
             "${GC_URL}/${GC_REPO}.git" 2>&1); then
    if [ "$attempt" -eq 1 ]; then
      echo "  目标仓可访问：${GC_HOST}/${GC_REPO}"
    else
      echo "  目标仓可访问：${GC_HOST}/${GC_REPO}（重试后第 ${attempt} 次探测成功）"
    fi
    exit 0
  fi

  err=$(redact "$out")
  digest=$(summarize "$err")
  verdict=$(classify "$err")
  TRAIL+=("#${attempt} [${verdict}] ${digest}")

  echo "  第 ${attempt}/${MAX_ATTEMPTS} 次探测失败（${verdict}）：${digest}"

  if [ "$verdict" != "network" ]; then
    fail "$verdict" "$attempt" "$err"
  fi

  if [ "$attempt" -ge "$MAX_ATTEMPTS" ]; then
    fail network "$attempt" "$err"
  fi

  # 退避：按 BACKOFF 顺序取，越界则沿用最后一个值。
  idx=$((attempt - 1))
  if [ "$idx" -ge "${#BACKOFF[@]}" ]; then
    idx=$((${#BACKOFF[@]} - 1))
  fi
  sleep_secs="${BACKOFF[$idx]}"

  # 打到 stderr：让人在 Actions 日志里一眼看出「发生过重试」，
  # 而不是以为只试了一次就红了。
  echo "  ⏳ 判为网络层失败，${sleep_secs} 秒后重试：${attempt}/${MAX_ATTEMPTS} → $((attempt + 1))/${MAX_ATTEMPTS}" >&2
  sleep "$sleep_secs"
  attempt=$((attempt + 1))
done
