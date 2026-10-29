#!/usr/bin/env bash
# 段页式 e2e：从生产数据取隔离快照并跑判据。
#
# ## 为什么要取快照而不是直接跑生产库
#
# 判据会**写**（迁移补列、记录 last_recalled_at、删除块）。
# 直接对生产库跑等于把测试变成了数据损伤。
#
# ## 为什么用 sqlite3 .backup 而不是 cp
#
# 生产库在 WAL 模式下运行：graph.db 里只有已 checkpoint 的部分，
# 最近的写入还在 graph.db-wal 里。直接 cp 会拿到**缺尾部**的库
# （表现为「少了最近几十个块」或 integrity 失败）。
# .backup 走 SQLite 的备份 API，得到一致性快照。
#
# ## 用法
#
#   ./scripts/e2e_prod_snapshot.sh              # 取快照 + 跑判据
#   ./scripts/e2e_prod_snapshot.sh --keep       # 保留快照目录便于排查
#
# 快照落在 /tmp/_work/segpaging-e2e/（不污染仓库，也不进 $HOME）。

set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DATA_DIR="${HOLOGRAM_DATA_DIR:-/home/hologramagent}"
WORK="${SEGPAGING_E2E_DIR:-/tmp/_work/segpaging-e2e}"
KEEP=0
[[ "${1:-}" == "--keep" ]] && KEEP=1

echo "==> 数据目录: $DATA_DIR"
echo "==> 快照目录: $WORK"

if [[ ! -f "$DATA_DIR/memory/graph.db" ]]; then
  echo "!! 找不到 $DATA_DIR/memory/graph.db" >&2
  exit 1
fi

rm -rf "$WORK"
mkdir -p "$WORK"

# ① L3：一致性快照（只读打开 + .backup，不碰 WAL）
echo "==> 备份 graph.db（WAL 安全）..."
sqlite3 "file:$DATA_DIR/memory/graph.db?mode=ro" ".backup '$WORK/graph-prod.db'"

# 校验：块数必须可读且非零
BLOCKS=$(sqlite3 "$WORK/graph-prod.db" "SELECT COUNT(*) FROM memory_blocks;")
echo "    块数 = $BLOCKS"
if [[ "$BLOCKS" -eq 0 ]]; then
  echo "!! 快照里没有块，中止" >&2
  exit 1
fi
INTEG=$(sqlite3 "$WORK/graph-prod.db" "PRAGMA integrity_check;")
echo "    integrity = $INTEG"
[[ "$INTEG" == "ok" ]] || { echo "!! 快照完整性失败" >&2; exit 1; }

# ② L1：context.json
if [[ -f "$DATA_DIR/memory/context.json" ]]; then
  cp "$DATA_DIR/memory/context.json" "$WORK/context-prod.json"
  echo "==> context.json 已复制"
fi

# ③ L2：documents
if [[ -d "$DATA_DIR/memory/documents" ]]; then
  cp -r "$DATA_DIR/memory/documents" "$WORK/documents-prod"
  N=$(find "$WORK/documents-prod" -name '*.json' | wc -l)
  echo "==> documents 已复制（$N 篇）"
fi

echo
echo "==> 跑生产数据判据..."
cd "$REPO_DIR"
HA_PROD_DB="$WORK/graph-prod.db" \
HA_PROD_DOCS="$WORK/documents-prod" \
  go test -count=1 ./internal/memory/ ./internal/memory/coldcontext/ -run 'TestProd'

echo
echo "==> 完成"
if [[ "$KEEP" -eq 0 ]]; then
  echo "    快照保留在 $WORK（用 --keep 可抑制此提示；手动删除即可）"
fi
