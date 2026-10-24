#!/usr/bin/env bash
# HologramAgent 部署验证脚本：检查 healthcheck 残留 / 记忆去重 / archived 残留 / 嵌入规格
# 用法: verify_deploy.sh [data_dir]  默认 /home/newqqagent
set -u
DATA_DIR="${1:-/home/newqqagent}"
PASS=0; FAIL=0; SKIP=0
ok()   { echo "[PASS] $1"; PASS=$((PASS+1)); }
bad()  { echo "[FAIL] $1"; FAIL=$((FAIL+1)); }
skip() { echo "[SKIP] $1"; SKIP=$((SKIP+1)); }

echo "== HologramAgent 部署验证 =="
echo "数据目录: $DATA_DIR"
echo "时间:     $(date '+%Y-%m-%d %H:%M:%S')"
echo "模式:     只读检测，不修改/删除任何数据"
echo

# ---- 0. 数据目录可用性 ----
if [ ! -d "$DATA_DIR" ]; then
    echo "[FATAL] 数据目录不存在: $DATA_DIR"
    exit 2
fi

# ---- 1. healthcheck 残留检查 ----
echo "--- 1. healthcheck 残留 ---"
hc_files=$(find "$DATA_DIR/knowledge" "$DATA_DIR/memory" -maxdepth 2 \
    \( -name '*_hc_*' -o -name 'gotest' -o -name 'luatest' \) 2>/dev/null)
if [ -z "$hc_files" ]; then
    ok "无 _hc_*/gotest/luatest 残留"
else
    echo "     发现残留:"
    echo "$hc_files" | sed 's/^/       /'
    bad "存在 healthcheck 残留（上列，需人工确认清理）"
fi

# ---- 2. relations 去重检查 ----
echo "--- 2. relations 去重 ---"
GRAPH_DB="$DATA_DIR/memory/graph.db"
if [ -f "$GRAPH_DB" ] && command -v sqlite3 >/dev/null 2>&1; then
    dup=$(sqlite3 "$GRAPH_DB" "SELECT count(*) - count(DISTINCT source_id||'|'||target_id||'|'||relation_type||'|'||COALESCE(session_id,'')) FROM relations;" 2>/dev/null)
    total=$(sqlite3 "$GRAPH_DB" "SELECT count(*) FROM relations;" 2>/dev/null)
    if [ -n "$dup" ] && [ "$dup" -le 0 ] 2>/dev/null; then
        ok "relations 去重良好 (共 ${total:-0} 条, 重复 ${dup:-0})"
    else
        bad "relations 存在重复: 共 ${total:-?} 条, 重复 ${dup:-?}"
    fi
    # UNIQUE 索引存在性：查 relations 表 DDL 是否含 UNIQUE 约束
    # （注意：UNIQUE 约束创建的 sqlite_autoindex_* 索引其 sql 字段为 NULL，
    #   用 LIKE 查索引 sql 会漏报——须查表 DDL）
    ddl=$(sqlite3 "$GRAPH_DB" "SELECT sql FROM sqlite_master WHERE type='table' AND name='relations';" 2>/dev/null)
    if echo "$ddl" | grep -q 'UNIQUE'; then
        # 进一步确认唯一约束覆盖的列
        auto_idx=$(sqlite3 "$GRAPH_DB" "SELECT name FROM sqlite_master WHERE type='index' AND name LIKE 'sqlite_autoindex_relations%' AND tbl_name='relations';" 2>/dev/null | head -1)
        if [ -n "$auto_idx" ]; then
            cols=$(sqlite3 "$GRAPH_DB" "SELECT group_concat(name,',') FROM pragma_index_info('$auto_idx');" 2>/dev/null)
            echo "     UNIQUE 自动索引: $auto_idx, 覆盖列: ${cols:-?}"
        fi
        ok "relations 复合 UNIQUE 约束存在"
    else
        bad "relations 复合 UNIQUE 约束缺失（迁移未生效？）"
    fi
else
    skip "graph.db 不存在或 sqlite3 不可用 ($GRAPH_DB)"
fi

# ---- 3. archived 模板垃圾检查 ----
echo "--- 3. archived 残留 ---"
if [ -f "$GRAPH_DB" ] && command -v sqlite3 >/dev/null 2>&1; then
    # 兜底检测：relation_type 直接为 context_archived，或 target_id 含 context_archived
    arch=$(sqlite3 "$GRAPH_DB" "SELECT count(*) FROM relations WHERE relation_type='context_archived' OR relation_type='来源' AND target_id LIKE '%context_archived%';" 2>/dev/null)
    if [ -n "$arch" ] && [ "$arch" -eq 0 ] 2>/dev/null; then
        ok "无 context_archived 模板垃圾"
    else
        bad "context_archived 模板垃圾: ${arch:-?} 条"
    fi
else
    skip "graph.db 不存在或 sqlite3 不可用"
fi

# ---- 4. 嵌入规格检查 ----
echo "--- 4. 嵌入规格 ---"
CONFIG_DB="$DATA_DIR/config.db"
if [ -f "$CONFIG_DB" ] && command -v sqlite3 >/dev/null 2>&1; then
    emb=$(sqlite3 "$CONFIG_DB" "SELECT value FROM config WHERE key='core.agent.embedding_model_path';" 2>/dev/null)
    echo "     embedding_model_path = ${emb:-（未配置）}"
    if [ -n "$emb" ]; then
        # 模型路径含 #topN 则裁剪生效；否则全量加载（300 维 ≈ 1.5G/模型）
        if [[ "$emb" == *"#top"* ]]; then
            ok "嵌入模型已启用 #topN 裁剪（内存预算受控）"
        else
            # 逗号分隔的模型个数估算内存
            n=$(echo "$emb" | awk -F',' '{print NF}')
            est=$(( n * 1500 ))
            echo "     全量加载 ${n} 个模型，估算常驻 ≈ ${est}MB（未裁剪）"
            bad "嵌入模型未启用 #topN 裁剪（内存预算偏高）"
        fi
    else
        skip "embedding_model_path 未配置（可能是默认值）"
    fi
else
    skip "config.db 不存在或 sqlite3 不可用 ($CONFIG_DB)"
fi

# ---- 汇总 ----
echo
echo "== 汇总: PASS=$PASS FAIL=$FAIL SKIP=$SKIP =="
[ "$FAIL" -eq 0 ] && exit 0 || exit 1