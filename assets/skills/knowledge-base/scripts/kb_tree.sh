#!/usr/bin/env bash
# kbtree 客户端：按分类树检索 HologramAgent 知识库。
#
# 由 knowledge-base skill 调用（agent 读 SKILL.md 后按需调本脚本）。
# 也可人工使用：
#   kb_tree.sh                          # 整棵树
#   kb_tree.sh -c tech -d 1              # tech 子树，只看一层
#   kb_tree.sh -q goroutine -c tech     # 在 tech 子树内检索
#   kb_tree.sh -l                       # 列分类
#
# token 读取顺序：环境变量 KB_TOKEN > <skill>/config/config.json（或 <skill>/config.json）> 报错。
# 故意不放命令行参数：token 会进 shell 历史与 ps 输出。
set -euo pipefail

BASE="${KB_BASE:-http://127.0.0.1:9892}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# 读 token
# 帮助：-h/--help 必须在解析命令之前判，否则会被当成未知命令
for a in "$@"; do
  case "$a" in
    -h|--help)
      echo "用法:"
      echo "  kb_tree.sh [tree|categories|counts|search] [选项]"
      echo "选项:"
      echo "  -q 关键词    检索词（给了 q 就走 search）"
      echo "  -c 分类      限定分类子树（前缀匹配），如 tech / tech/go"
      echo "  -d 层数      树展开层数，1=只看第一层（懒加载）"
      echo "  -i 0|1      是否带条目详情，默认 1"
      echo "  -l 条数      search 的返回条数，默认 10"
      echo "环境变量: KB_TOKEN（也可放同目录 config/config.json）"
      exit 0 ;;
  esac
done


if [[ -z "${KB_TOKEN:-}" ]]; then
  # 两种布局都试：<skill>/config/config.json（当前布局）与 <skill>/config.json。
  # 只写一种会因目录结构调整而静默失效——症状是「明明配了 token 却说没找到」。
  for CFG in "$HERE/../config/config.json" "$HERE/../config.json"; do
    if [[ -f "$CFG" ]]; then
      KB_TOKEN="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("token",""))' "$CFG" 2>/dev/null || true)"
      [[ -n "$KB_TOKEN" ]] && break
    fi
  done
fi
if [[ -z "${KB_TOKEN:-}" ]]; then
  cat >&2 <<'EOF'
kbtree: 未找到访问令牌。

取 token 的办法（其一）：
  1. 若 homed 启动日志里有「token 未配置，本次随机生成」，那行日志附近有 token；
  2. 若已配置过固定 token，从配置库读：
       sqlite3 <data_dir>/config.db "SELECT value FROM config_kbtree WHERE key='token'"
  3. 直接设置：export KB_TOKEN=<token>

token 未配置时服务会在启动时随机生成，进程重启即失效。
EOF
  exit 2
fi

auth=(-H "X-API-Key: ${KB_TOKEN}")

# 第一个参数若是选项（如 -q x），它就是选项而非子命令。
# 原先会把 "-q" 当命令名报「未知命令: -q」——而 `kb_tree.sh -q 并发` 是很自然的写法。
if [[ $# -gt 0 && "$1" == -* ]]; then
  cmd=""
else
  cmd="${1:-tree}"
  shift || true
fi
query=""; category=""; depth=""; items=""; limit=""
while getopts "q:c:d:i:l:h" opt 2>/dev/null; do
  case "$opt" in
    q) query="$OPTARG" ;;
    c) category="$OPTARG" ;;
    d) depth="$OPTARG" ;;
    i) items="$OPTARG" ;;
    l) limit="$OPTARG" ;;
    h) echo "用法: kb_tree.sh [tree|categories|counts|search] -q 关键词 -c 分类 -d 层数 -i 0|1 -l 条数"; exit 0 ;;
  esac
done

# 预检：端口通不通。不通就直说"服务未上线"，
# 而不是让用户看 curl: (7) Connection refused 后自行猜测。
HOSTPORT="${BASE#http://}"; HOSTPORT="${HOSTPORT%%/*}"
if ! (exec 3<>"/dev/tcp/${HOSTPORT%%:*}/${HOSTPORT##*:}") 2>/dev/null; then
  cat >&2 <<EOF
kbtree: 连不上 $BASE（端口未监听）。

最常见原因——**服务还没上线**。kbtree 是 HologramAgent 的内置插件，
需要二进制里含它才会启动。若 homed 是在合入该插件之前构建/部署的，
就一直没有这个服务。上线后验证：

  systemctl show homeagent -p MainPID --value
  strings /usr/local/bin/homed | grep -c kbtree     # 0 = 二进制里没有

本机上线步骤（会重启守护进程）：
  sqlite3 <data_dir>/config.db ".backup '<data_dir>/config.db.bak-\$(date +%Y%m%d-%H%M%S)'"
  install -m 0755 <新二进制> /usr/local/bin/homed    # 勿用 cp：会写坏运行中进程的映像
  systemctl restart homeagent
  journalctl -u homeagent -f | grep kbtree
EOF
  exit 7
fi

enc() { python3 -c 'import sys,urllib.parse;print(urllib.parse.quote(sys.argv[1]))' "$1"; }

# kb_call 发请求并把错误翻译成人话。
# 不用 curl -f：它只吐 "curl: (22) 404"，把服务端给的有用信息（404 会附现有分类）
# 全丢了 —— 而那恰恰是排查时最需要的。
kb_call() {
  local url="$1" raw code
  raw="$(curl -sS -w $'\n%{http_code}' "${auth[@]}" "$url" 2>&1)" || {
    echo "kbtree: 请求失败: ${raw}" >&2; exit 8
  }
  code="$(printf '%s' "$raw" | tail -n1)"
  body="$(printf '%s' "$raw" | sed '$d')"
  case "$code" in
    2*) printf '%s' "$body" | python3 -m json.tool; return 0 ;;
    401) echo "kbtree: 令牌无效（401）。检查 config/config.json 或 KB_TOKEN 是否与 config_kbtree 表一致。" >&2; exit 3 ;;
    404) echo "kbtree: 分类不存在（404）。服务端返回的现有分类：" >&2
         printf '%s' "$body" | KB_BODY="$body" python3 -c '
import json, os, sys
try:
    d = json.loads(os.environ.get("KB_BODY",""))
    print("  " + ", ".join(d.get("categories", [])), file=sys.stderr)
except Exception:
    print("  (无法解析响应体)", file=sys.stderr)
'
         exit 4 ;;
    *)  echo "kbtree: HTTP $code" >&2; printf '%s\n' "$body" >&2; exit 5 ;;
  esac
}

# 命令在解析选项**之后**定：没显式给子命令时，
# 有 -q 走 search、没有则走 tree。
if [[ -z "$cmd" ]]; then
  if [[ -n "$query" ]]; then cmd="search"; else cmd="tree"; fi
fi
case "$cmd" in
  tree|categories|counts|search) ;;
  *) echo "未知命令: $cmd" >&2; exit 2 ;;
esac

if [[ "$cmd" == "categories" ]]; then
  kb_call "$BASE/categories"
  exit 0
fi
if [[ "$cmd" == "counts" ]]; then
  kb_call "$BASE/counts"
  exit 0
fi
if [[ "$cmd" == "search" || -n "$query" ]]; then
  [[ -n "$query" ]] || { echo "search 需要 -q 关键词" >&2; exit 2; }
  url="$BASE/search?q=$(enc "$query")"
  [[ -n "$category" ]] && url="$url&category=$(enc "$category")"
  [[ -n "$limit" ]] && url="$url&limit=$limit"
  kb_call "$url"
  exit 0
fi

# 默认：树
url="$BASE/tree"
[[ -n "$category" ]] && url="$url?category=$(enc "$category")"
q=""
[[ -n "$depth" ]] && q="depth=$depth"
[[ -n "$items" ]] && q="${q:+$q&}items=$items"
[[ -n "$q" ]] && url="$url?$q"
kb_call "$url"
