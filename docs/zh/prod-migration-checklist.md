# 生产迁移执行清单

**日期**：2026-10-04
**分支**：`feat/context-overflow-l4`（未 push）
**状态**：HologramAgent 已停机（inactive + disabled）

---

## 0. 前置条件（必须全部满足）

| 项 | 要求 | 实测 |
|---|---|---|
| 服务停机 | `homeagent.service` inactive | ✅ inactive + disabled |
| WAL 已 checkpoint | `PRAGMA wal_checkpoint(TRUNCATE)` 返回 0\|0\|0 | ⬜ 执行前重查 |
| 快照已存在 | 一致副本（非 `cp`，用 `.backup`） | ✅ `/var/tmp/ha-prod-migrate/before.db` |
| 快照基线 | 1294/980/66/98/97 | ✅ 已核对 |
| 副本验证 | 结构 7 项 + 召回 7/9 | ✅ 已通过 |
| 代码提交 | 43 包全绿 | ✅ |
| 磁盘空间 | 迁移后约 18MB（副本实测） | ⬜ 执行前确认 |

★ **副本用的就是生产快照**，所以"在副本验证"等价于"在生产数据形态上验证"。

---

## 1. 执行步骤

```bash
# ① 停机确认（已停，此处防呆）
systemctl is-active  homeagent.service     # 期望: inactive
systemctl is-enabled homeagent.service     # 期望: disabled

# ② WAL checkpoint
sqlite3 ${HA_DATA}/memory/graph.db "PRAGMA wal_checkpoint(TRUNCATE)"
#    期望输出: 0|0|0

# ③ 生成新快照（.backup，不是 cp）
TS=$(date +%Y%m%d-%H%M%S)
sqlite3 ${HA_DATA}/memory/graph.db ".backup '/var/tmp/ha-prod-migrate/prod-$TS.db'"
#    核对副本基线与源库一致
sqlite3 /var/tmp/ha-prod-migrate/prod-$TS.db \
  "SELECT COUNT(*) FROM entities; SELECT COUNT(*) FROM relations; SELECT COUNT(*) FROM sentences;"

# ④ 先报告（不写库）
go run -tags onnxruntime ./cmd/homed-graph-migrate \
  -db ${HA_DATA}/memory/graph.db \
  -embed-provider chineseclip \
  -model-dir /var/tmp/ha-c/models/chinese-clip-vit-b16-onnx
#    ★ 核对：实体 1294，关系 966，块 98，块边 97
#    ★ 若数字对不上 → 停止，不要 -apply

# ⑤ 执行迁移（约 200s）
go run -tags onnxruntime ./cmd/homed-graph-migrate \
  -db ${HA_DATA}/memory/graph.db -apply \
  -embed-provider chineseclip \
  -model-dir /var/tmp/ha-c/models/chinese-clip-vit-b16-onnx

# ⑥ 验证
bash scripts/verify-migration.sh ${HA_DATA}/memory/graph.db
#    期望：✅ 结构验证全部通过

# ⑦ 召回探针
PROD_SNAPSHOT=${HA_DATA}/memory/graph.db \
  go test -count=1 -tags onnxruntime ./internal/memory/ \
    -run 'TestProbe_生产规模召回' -v
#    期望：casual 3/4, confusable 1/1, abstention 3/3, 合计 7/9
```

★ 全程**串行**。迁移涉及 1294 个块的 embedding，
并行会导致资源争用与长时间卡死（此前实测过 600 秒卡死）。

★ **旧表不删**。迁移只写块与边，`entities/relations/sentences`
保持原样（验证脚本第 1 项逐表核对）。

---

## 2. 期望产出（副本实测值）

```
句子 +66，块 +1294，边 +980，scene_refs +718
块 2752（带向量 1391），边 2371
悬空端点 0
向量维度唯一 512，fingerprint 唯一
confidence 为 0 的关系边 0 条
deleted 状态关系边 14 条（对应旧表 14 条）
中心向量已重建（各向异性 detected，双边中心化启用）
耗时约 200s
```

---

## 3. 回滚

### 回滚条件

迁移后出现以下任一情况 ⇒ 立即回滚：

- 验证脚本报 `❌`
- 召回探针合计低于 4/9（迁移前基线）
- 启动后 `memory_recall` 报「检索失败」
- 健康检查异常

### 回滚方式

★ **旧表未被改动**，所以回滚很简单：

```bash
# ① 恢复代码到迁移前的提交
git checkout 75d4609      # 或 d4eda47 / 2fe4ca7 等任一已验证提交

# ② 恢复数据库（若需要）
TS=<迁移时的快照时间戳>
cp /var/tmp/ha-prod-migrate/prod-$TS.db ${HA_DATA}/memory/graph.db
#   ★ 生产库必须用 .backup 恢复，不要直接 cp 源库

# ③ 重建二进制并重启（需你确认后才启服务）
go build -tags onnxruntime -o build/homed ./cmd/homed
systemctl enable homeagent.service && systemctl start homeagent.service
```

### 回滚安全性

| 保证 | 依据 |
|---|---|
| 旧表数据完好 | 迁移只 INSERT 块/边，验证脚本第 1 项核对 |
| 迁移单事务 | 任一步失败整体回滚 |
| 迁移自动快照 | 命令自身会生成 `.bak-<时间戳>` |
| 代码可回退 | 全部改动已提交，未 push |

---

## 4. 迁移后仍存在的问题（不阻塞迁移，但要记录）

| 问题 | 状态 | 补法 |
|---|---|---|
| `coexist` 泛指提问召不回端口号 | 0/1 | BFS 接入 `RecallBlocksFused` |
| 纯中文编造查询不拒答 | 契约外 | 需符号切分改进或换判定信号 |
| 旧表仍在增长 | 待停双写 | 读方已全切，可摘 `commit()` 里的 INSERT |
| `TestResidualKeepReturnsTasksToParent` | 既有 flake | 单独复验，勿误判 |

---

## 5. 与旧快照的关系

```
/var/tmp/ha-prod-migrate/before.db   本轮开始时的生产基线（1294/980/66/98/97）
/var/tmp/ha-prod-migrate/probe.db    副本，已完整迁移并验证
/var/tmp/ha-prod-migrate/base-copy.db  迁移前基线探针用
```

★ `before.db` 与 `probe.db` 是**同一份数据的迁移前后对照**，
两者都在，可随时重跑任何验证。
