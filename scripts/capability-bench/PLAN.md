# 跑分计划

> 本文原为根目录 `benchmark.md`，随仓库清理归位到此处
> （与同目录 `README.md`「能力标定台」是两份不同文档，勿混）。
>
> **本机路径已脱敏**：脚本里出现的 `/home/<user>`、`/var/tmp/...` 均为占位示例，
> 实际跑请按本机环境替换，或改用 `spawn-instance.sh` 的参数。

> 交接文档：供新会话执行。旧会话的测试**已全部停止**，本文件是从头开始的唯一依据。
> 新会话开始时：**先验证本文件每一条「前置事实」再执行**，不要凭记忆或信任旧会话输出。

## 0. 前置事实（新会话必须先逐条重验）

| # | 事实 | 验证命令 | 期望 |
| --- | --- | --- | --- |
| 1 | 跑分工具已就位 | `ls scripts/capability-bench/` | bench.py pi_bench.py compare.py memory_recall.py tasks.example.json tasks.zerobasis.json spawn-instance.sh README.md |
| 2 | 代码提交已完成 | `git log --oneline -8` | 见 §5 提交清单 |
| 3 | 工作区干净 | `git status --short` | 空 |
| 4 | 生产实例未受影响 | `ps -p $(pgrep -f 'homed -data ${HA_DATA}' \| head -1) -o pid,etime` | 存活 |
| 5 | 测试进程已清 | `pgrep -f 'memory_recall\|pi_bench\|bench.py'` | 空 |
| 6 | llmsproxy 可用 | `curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $(sqlite3 ${HA_DATA}/config.db \"SELECT value FROM config WHERE key='core.llm.api_key'\")" http://127.0.0.1:8081/v1/models` | 200 |
| 7 | pi 可用且版本 | `pi --version`（如 EADDRINUSE 用 `env -u PI_A2A_PORT -u PI_ACP_PORT`） | 0.85.1 |
| 8 | pi 的 llmsproxy provider | `python3 -c "import json;d=json.load(open('${PI_MODELS}'));print(d['providers']['llmsproxy']['baseUrl'])"` | <http://127.0.0.1:8081/v1> |

## 1. 已知的关键教训（不重犯）

1. **两侧 usage 同名不同义**（已归一，勿改回去）：
   - HologramAgent：`prompt_tokens` **含**缓存（OpenAI 口径）
   - pi：`input` 是**未命中侧**，`totalTokens = input + cacheRead + output`（Anthropic 口径，受控实验实证）
   - ⇒ 两侧各自过归一函数（pi_bench.py `_usage_of` / memory_recall.py `_norm_ha_usage`+`_norm_pi_usage`），汇总只认归一后的键
2. **缓存命中率恒 100% 的假绿已修**（`59c0689`）：上游只报命中侧时 `miss = prompt - read`。修复前 100.0% → 修复后 87.6%。
3. **cli.sock 按行读**：消息含换行 = 拆成多条独立消息（历史全乱，表现为 BrokenPipe）。memory_recall.py 已有 `assert "\n" not in text`。
4. **内核会 dedupe 完全相同的输入**（`skipped:true`）：多轮/重跑必须让每条文本不同。
5. **pi 必须隔离配置目录 + 端口**：`PI_CODING_AGENT_DIR=${PI_ISO}` + `PI_A2A_PORT`/`PI_ACP_PORT` 给空闲端口。隔离后无扩展 ⇒ `pi -p` 跑完自己退出。
6. **pi 的工具事件**：事件名 `tool_execution_start`、字段 `toolName`（写错只是静默为空）。
7. **★ 超窗校验是判据的前提，不是可选项**：旧 200k 跑分因实例窗口（HologramAgent 1M / pi 600k）大于灌入量 253k，实际测的是「窗口内召回」—— 跑 19 分钟、数字漂亮、完全无效。memory_recall.py 已加 `--expect-window` 校验（对无效配置直接拒跑）；新会话用它，并先实测两侧真实窗口。
8. **改 LLM 配置必须重启实例**：provider 启动时构建，`/settings set` 落库但不生效。
9. **不要用 `pkill -f`**：会匹配到自己这条 shell 命令行。用 pid 文件或 `pgrep` 精确 pid。
10. **面板懒渲染**：WebUI 的 `#chat-panel-chat` 只在 `switchTab('chat')` 后存在；浏览器验证先切页。
11. **本机 diff 是坏的**（PATH 首位对任何输入返回 0）：用 `/usr/bin/diff` 或 Python difflib。

## 2. 测试 A：零基础认知对比（质量，可多实例并行）

**目的**：同模型、同一份材料，只变 harness，测认知而非"翻自己的库"。
（旧版硬伤：knowledge-stats 让 pi 读 HologramAgent 自己的 graph.db ⇒ pi 97s/317k token 全是权限差异，不是能力差异。）

材料：`${BENCH_MATERIAL}/{incident-2026-08.md, config-notes.md}`（若不存在，按 git 历史里的任务集重建；材料必须含：512 连接池上限、4200 峰值、v2.30.4、87 订单、"600" **不在**材料里须自己算 200+400、ttl=0=永不过期、N+1、对账批处理）。

任务集：`scripts/capability-bench/tasks.zerobasis.json`（6 任务：检索/算术/推断/抗干扰/综合/多跳，全部带显式 check）。

执行：

```bash
# ① 起隔离实例（窗口两侧都配 200000）
BIN=${REPO}/build/homed \
  bash scripts/capability-bench/spawn-instance.sh a 18082 200000
# ② HologramAgent 侧
python3 scripts/capability-bench/bench.py --socket ${INSTANCE_A}/cli.sock \
  --api-key "$(cat "${INSTANCE_A}/apikey")"   # 由 spawn-instance.sh 写入 \
  --tasks scripts/capability-bench/tasks.zerobasis.json --out ${OUT_DIR}/ha --timeout 300
# ③ pi 侧（同一任务文件 ⇒ 提示词逐字相同）
python3 scripts/capability-bench/pi_bench.py \
  --tasks scripts/capability-bench/tasks.zerobasis.json --out ${OUT_DIR}/pi --timeout 300
# ④ 对比
python3 scripts/capability-bench/compare.py --a ${OUT_DIR}/ha --b ${OUT_DIR}/pi \
  --label-a HologramAgent --label-b pi --out ${OUT_DIR}
```

验收：两侧任务集一致（compare.py 会检查）；逐任务看通过与 token；失败原因必须留在 compare.md。

## 3. 测试 B：200k 超窗记忆召回（性能敏感 ⇒ 独占实例，串行跑）

**目的**：多轮对话超出窗口后，早期内容的召回率（HologramAgent 向量裁剪 vs pi 压缩）。

**★ v3 填充模型（`95224eb`，当前唯一有效版本）**

| 版本 | 填充 | 结论 |
| --- | --- | --- |
| v1 | 显式强调针 + 语义空转 | 两例 4/4 满分零区分度 |
| v2 | 端口同构干扰 + **废话流**（天气/树叶/墨盒） | **作废**：废话被压缩直接丢弃 ⇒ 偶发针成填充里唯一的价值内容 ⇒ pi 跑分虚高 |
| v3 | **高密度叙事**（order-gw 运维主线） | 当前版本 |

v3 主线：`上线准备 → 灰度事故 → 修复验证 → 版本发布 → 交接收尾`，
每轮 5 片段、每片段 2-4 条有信息增量的工作项（数值、因果、决策、变更）、
含跨轮引用。针全部嵌在价值信息流里：

- **偶发针** = 服务依赖清单条目（同构干扰 = 其它服务的**真实**端口，同样有意义，只是不是针）
- **覆盖针** = 值班安排改号（`4379 → 4324`，真实变更场景；反向哨兵验证不塌回）
- **多跳** = 交接流程本身（团队→门禁→申请表）

实测证据支持「废话填充虚高」：v3 第 1 轮 HA 模型就同时调用
`memory_recall / doc_query / config_list_plugins / cmd_run` 四个工具，
而 v2 废话填充下只调 `doc_commit` —— 高密度负载才考得出 harness 的工具选择与
循环管理能力。

**★ 踩坑：锚点 frac 必须落在对应 phase 区间内**（生成器自证时抓到）
`narrative_block` 的 phase 由 `frac` 切分，而针锚点也用 `frac`。写
`i == int(total*0.50)` 想在 release 阶段插针，但 `0.50 < 0.61` 实际落在
verify 分支 ⇒ **永不触发**（表现为「覆盖新值找不到」，不报错）。同理
`0.63` 距边界 `0.61` 太近，浮点除法下可能仍在相邻区间。改法：把每个锚点
推到所属区间**内部**（0.52/0.66/0.70/0.76/0.85/0.92），并写落位自证。

**★ 先把两侧窗口真实配成一致（这是上次翻车点）**：

- HologramAgent：`core.llm.sources.deepseek.context_window = 200000`（spawn-instance.sh 第 3 参传 200000），起后 `sqlite3 ... "SELECT value FROM config WHERE key LIKE '%context_window%'"` 验证 = 200000
- pi：把隔离目录 `${PI_ISO}/models.json` 的 AUTO 模型 `contextWindow` 改成 200000，并用 python3 读回验证

执行（串行，先 A 后 pi；v3 填充实测 `overshoot` 需给 1.45 才能达到 1.23×）：

```bash
python3 scripts/capability-bench/memory_recall.py --harness homeagent \
  --socket ${INSTANCE_C}/cli.sock --api-key "$(cat ${INSTANCE_C}/apikey)" \
  --window 200000 --expect-window 200000 --overshoot 1.45 \
  --out ${OUT_DIR}/v3-ha
python3 scripts/capability-bench/memory_recall.py --harness pi \
  --window 200000 --expect-window 200000 --overshoot 1.45 \
  --out ${OUT_DIR}/v3-pi
```

验收：工具打印「✓ 超窗校验通过」（否则结果无效，删掉重跑）；报告 recall_rate 按针分层；两侧灌入量同级（v3 = 143 轮 / 245k / 1.23×）。

注意：143 轮/侧，高密度叙事下单轮 ~25-50s，单侧约 1.5-2 小时；用 bg_run 跑且**不要** `| tail`（会缓冲到看不见进度），输出落文件。实时轮数看实例 `run.log` 的 `grep -ac 'text from cli'`（脚本 stdout 重定向后块缓冲滞后）。

## 4. 测试 C：多实例并行吞吐（可选，性能）

spawn-instance.sh 可起多实例（18082/18083/…；pluginmgr 9876 / remotedevice 9890 / kbtree 9892 写死会 bind 失败属预期降级，不影响 cli.sock 标定）。质量类测试可并行；性能类必须独占。

## 5. 提交清单（已在本地 main，未推送）

```
9ca7e34 feat(bench): 能力标定台 —— 经 cli.sock 驱动实例，记录结果与账目
59c0689 fix(usage): 缓存规则收成单一实现 —— 修掉「命中率恒 100%」的结构性假绿
07e15dc feat(context): 上下文阈值接入配置系统 —— 消除 0.8 与 600000 合成的隐形断层
52c2aae feat(webui): 对话页展示缓存命中率与 token 用量
（更早：73a6359 54ba3ff 2f898a6 6d537ca afeba70 a8ed3a8 —— usage 全链路 + 估算器校准）
```

待提交：本文件（benchmark.md）+ capability-bench 新增的 pi_bench.py / compare.py / memory_recall.py / tasks.zerobasis.json / spawn-instance.sh / README 增补。

## 6. 推送与部署（人工确认后才做）

- 推送：`git push origin main`（8+ 提交在本地）
- 部署：`deploy-plan.sh check`（已预检过：onnxruntime ✓ libonnxruntime ✓）→ backup → deploy；deploy 前需人工确认
- 生产适配器：`${HA_DATA}/adapters/` 有 9/10 与 .bundled 清单不符（openai.lua 实测仅版本落后、无用户修改）；部署前逐个核对后清掉让内核重解包（backup 会带走）

## v4 跑分（2026-10-04，新内核 657ccd7）

### ★ 先说一个必须记的操作错误

跑之前先核对了内核版本：

```
build/homed      Oct  1 21:18   ← 3 天前的二进制
今天的 memory 层提交  19 个
```

**如果直接跑，测的就是三天前的内核。** 而今天改的
全部是 memory 层（融合召回 / 拒答判据 / 仲裁 / schema 退场）——
跑分会完全测不到今天的工作。

⇒ **跑分前必须核对 `vcs.revision` == `git rev-parse HEAD`。**
  这一条应该进 `spawn-instance.sh`，让它自动拒绝版本不符的实例。

（另外两次操作失误：`cp` 取快照在 WAL 模式下不一致；
`${VAR:0:8}` 在 `/bin/sh` 里报 Bad substitution。）

### 能力标定（tasks.zerobasis，6 任务）

| 指标 | 值 |
|---|---|
| 任务数 / 通过 | **6 / 6（100%）** |
| 总墙钟 | 159.17s |
| 工具调用 | 8 |
| 合计 token | 281971 |
| 缓存命中率 | 42.7% |
| 超时任务 | 0 |

分项：检索 3.85s / 算术 43.44s（算出 600）/ 推断 42.66s /
抗干扰 6.57s（答出「永不过期」）/ 综合 5.36s（命中 4200）/ 多跳 57.30s

### 与今天的记忆侧改动的关系

这一轮 6/6 **验证的是「今天没把主干跑坏」**，不是「今天的改动带来了提升」。

记忆召回质量另有专项（`memory_recall.py`，36 探针），
它的结论已经单独记录在 `production-recall-validation.md`：

| | 测试库 ha-c | 生产快照 1391 块 |
|---|---|---|
| 端到端 | 7/7 | 4/9 |
| abstention | — | 0/3（纯中文编造） |

★ **两个库的可拆率差 7.5 倍**（83% vs 11%），
  ha-c 的分数不能推广到生产。

## v4 跑分结果汇总（2026-10-04）

### 能力标定

```
tasks.zerobasis   6/6（100%）   墙钟 159.17s
token 281971      缓存命中 42.7%   超时 0
```

### 端到端记忆召回

```
仲裁前 6/7  →  仲裁后 7/7
  casual 3/3   overwrite 2/2   confusable 2/2
```

### ★ `memory_recall.py` 专项跑不完（不是慢，是必然失败）

36 轮 = 填充 26 + 探针 10。填充轮耗时实测：

```
[1/36]  30.9s
[2/36] 131.5s
[3/36] 392.0s        ← 递增比 2.98×
```

外推：**第 6 轮就超过单轮超时（900s）**，26 轮累计 12.9 小时。

★ 慢的原因是 `memory_commit` 在填充期间持续写库
  （entities 188 → 220），每轮检索的数据量与召回量都在涨。

⇒ **改走等价路径**：用探针判据（`TestProbe_端到端召回`）直接测，
  数据仍是真实内核跑过的迁移库，结果 7/7。

⇒ **这个专项需要重新设计**：26 轮逐条填充的做法在
  「写入会改变检索集」的系统上不成立。
  正确做法应是**批量预填**（一次写入全部填充内容），
  再跑探针轮 —— 否则填充本身就是被测对象的一部分。

### 本轮跑分的三个操作失误（已在 `421c1d7` 固化防护）

```
① 未核对内核版本   → 差点测三天前的二进制（已加自动拒绝）
② ${VAR:0:8}      → /bin/sh 报 Bad substitution
③ 改错二进制路径   → ${INSTANCE_C}/homed 不存在，实际是 build/homed
```

### 实战数据印证的三件事

```
exact 14 / text 24 / symbol 0   标签分布（symbol 是今天新增的）
拒答 2 次（第8批/第9批，精确串库里没有 ⇒ 判据正确触发）
overflow 32 / preempt 6 / 裁剪 3   L4 超页处理真实触发
entities 188 → 220            memory_commit 仍在写旧表
```

详见 `live-run-evidence-2026-10-04.md`。
