# 能力标定台（capability-bench）

经 `cli.sock` 驱动一个 HologramAgent 实例跑任务集，记录**结果 + 账目**，产出
`report.md` / `results.json`。

存在的理由：标定「能力」需要一个**可复现的驱动 + 记账 + 判据**，
而不是手工敲几句看回复。三件事各对应一个曾经缺失的环节：

| 环节 | 做法 | 依赖 |
|---|---|---|
| 驱动 | 连 `cli.sock`，发任务，收帧到终态 | cli 插件（无头唯一入口）|
| 记账 | `/kernel` 取累计用量，做**任务前后差分** | `/kernel` 的 `usage` 段（2026-09-30 补）|
| 判定 | 任务自带的 `check` | 无 check 一律判失败（防空绿）|

## 用法

```bash
# 不连实例，先看会跑什么
python3 bench.py --data ${HA_DATA} \
    --tasks tasks.example.json --out /var/tmp/bench --dry-run

# 真跑
HOMEAGENT_CLI_KEY=<cli 或 webui 的 api_key> \
python3 bench.py --data ${HA_DATA} \
    --tasks tasks.example.json --out /var/tmp/bench

# 只跑其中几个 / 放大超时（长时任务）
python3 bench.py --socket /path/cli.sock --api-key KEY \
    --tasks tasks.example.json --only long-soak --timeout 7200 --out /var/tmp/bench
```

退出码：`0` 全过，`1` 有失败，`2` 参数/任务集/实例问题，`3` 驱动层失败。

## 计费口径（重要）

**用 `/kernel` 差分，不用单次回包的 usage。** 一个任务常触发多轮 LLM 调用
（工具回环），单次回包只反映最后一段。两者都记录在 `results.json` 里便于对照。

命中率的分母只算**报过缓存的调用**（`cache_read + cache_miss`）。
一次都没报过时报告写「—」而不是 0% —— 把「没数据」画成 0% 会让人去优化
一个本来就没开的功能。

## 任务集格式

```jsonc
{
  "tasks": [
    {
      "id": "read-single-file",
      "dimension": "readonly",          // 维度，便于分维度看退化
      "prompt": "…",
      "timeout_s": 180,                 // 可选，默认取 --timeout
      "check": { "kind": "output_contains", "value": "1.4" }
    }
  ]
}
```

支持的 `check.kind`：

| kind | 字段 | 判据 |
| --- | --- | --- |
| `output_contains` | `value` | 回复含该子串 |
| `output_regex` | `value` | 回复匹配该正则 |
| `file_exists` | `value` | 该路径存在 |
| `file_contains` | `value` / `needle` | 该文件含 needle |
| `tool_used` | `value` | 过程帧里用过该工具 |
| `completed` | — | 没超时、没报错（用于「跑得完」类长时任务） |

**期望值必须实测核对过再写进去**（例如 `Version=1.4.0` 来自
`internal/meta/meta.go:33`）。写错的期望值会让标定结果整体失去意义 ——
它会把「模型答对了」判成失败。

## 踩过的坑（写任务集/起实例前先看）

1. **提示词不能重复。** 内核把完全相同的输入判为 `duplicate` 直接跳过
   （`skipped:true`），第二次根本没跑 LLM。任务集里每条都不一样，若同一
   任务要重复跑，得在提示词里加变化（时间戳/序号）。
2. **改 LLM 配置必须重启实例。** provider 在启动时构建；经 `/settings set`
   改 `core.llm.base_url` 会落库但**不生效**（实测仍打原地址，报 401）。
3. **隔离实例要在固定插件端口上撞车。** `-webui` 能换，但
   `pluginmgr(9876)` / `remotedevice(9890)` / `kbtree(9892)` 是写死的，
   同机第二个实例会 bind 失败（那几个插件降级，内核其余部分照常）。
   这对经 cli.sock 标定无影响，但别以为实例「完全隔离」。
4. **`cli.sock` 的认证 key 回落到 webui 的 `api_key`**（`config_webui.api_key`）；
   `config_cli.api_key` 为空时用后者。认证失败会直接断开连接。
5. **不要用 `pkill -f`** 清理 mock/实例 —— 它会匹配到自己这条 shell 命令行，
   把当前 shell 一起杀掉（本项目已踩过）。用 PID 文件。

## 本地验证过什么（2026-09-30）

用 `build/homed` + `scripts/kernel-stress/usage_mock_llm.py` 起隔离实例
（`-webui 127.0.0.1:18080`），实测确认账目链路端到端贯通：

```text
mock 上报        prompt=300 completion=30 cache_read=200 cache_miss=100
回包 usage       prompt_tokens=300 completion_tokens=30 total_tokens=330
                 cache_read_tokens=200 cache_miss_tokens=100 cache_reported=true
/kernel 累计     calls=1 prompt=300 total=330 cache_read=200 cache_miss=100
                 cache_hit_rate=0.667   (= 200/(200+100))
```

标定台本身跑 3 个任务：差分 330 token/任务、命中率 66.7%、PASS/FAIL 判定正常、
报告与 JSON 落盘正常。

## 其他三个工具

| 文件 | 作用 | 关键点 |
| --- | --- | --- |
| `pi_bench.py` | **pi 基线**（同任务、同模型） | `pi -p --mode json`；必须隔离配置目录与端口 |
| `compare.py` | 两侧并排对比 | 校验任务集一致；命中率无数据写「—」 |
| `memory_recall.py` | 多轮**超出窗口后**的记忆召回 | 埋针 + 填充 + 提问；单行消息（协议按行读） |

### pi 基线的三个坑（都实测踩过）

1. **必须隔离配置目录**：`PI_CODING_AGENT_DIR` 指向独立目录，否则子 pi 会
   读写你的会话状态。
2. **必须隔离端口**：本机会话守护进程占着 `PI_A2A_PORT=14010`，另一个 pi 占默认
   `12010` ⇒ 子进程继承环境后直接 `EADDRINUSE` 崩。
   顺带的好事：隔离配置里没有扩展 ⇒ 不起 A2A/ACP 服务 ⇒ `pi -p` **跑完自己退出**
   （有扩展时它跑完不退，必须自己 kill）。
3. **工具名是 `toolName`、事件名是 `tool_execution_start`**：
   写错不会报错，只是工具名恒为空（`tool_calls=30` 而 `tools=[]`）。

### ★ 同名不同义：两侧 usage 口径相反

| | 输入总量 | 未命中侧 |
| --- | --- | --- |
| HologramAgent（OpenAI 口径） | `prompt_tokens` **含**缓存 | `cache_miss_tokens` |
| pi（Anthropic 口径） | `input + cacheRead` | `input` |

实测实证（pi）：`totalTokens == input + cacheRead + output`。
不归一就直接比，会给 pi 系统性低估，且命中率会算成 `read/read` = **恒 100%**。
两侧都过各自的归一函数，汇总只认归一后的键。

### 记忆召回测试怎么用

```bash
# 小窗口先验仪器（快）
python3 memory_recall.py --harness homeagent --socket <cli.sock> --api-key KEY \
    --window 20000 --overshoot 1.3 --needles 3 --out /var/tmp/mem/probe

# 正式：200k
python3 memory_recall.py --harness pi --window 200000 --overshoot 1.25 \
    --needles 4 --out /var/tmp/mem/pi-200k
```

**两侧必须把窗口配成同一个值**，否则比的不是策略而是配置：

- HologramAgent：`core.llm.sources.<name>.context_window`
- pi：隔离目录 `models.json` 里的 `ctx`

⚠️ 消息**不能含换行**：`cli.sock` 按行读，一条多行消息会被拆成几十条独立消息
（历史全乱，表现为 BrokenPipe）。脚本内有 `assert "\n" not in text` 兜底。
