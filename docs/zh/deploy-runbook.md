# HologramAgent 生产部署手册（homed / waiter）

> 适用范围：本机（`.60`）的 `homeagent.service`，以及两台设备桥宿主上的
> `waiter-remote.service`。
>
> 静态站 / nginx / 证书的运维见另册
> [`site-infra-runbook.md`](./site-infra-runbook.md) —— 本册**不涉及**。
>
> 记录日期：2026-09-27。本册所有数字均与现场核对过，不是模板值。

---

## 0. 一句话拓扑

```
                    ┌─────────────────── 本机 .60 ───────────────────┐
                    │ homeagent.service ← /usr/local/bin/homed      │
                    │   （必须 -tags=onnxruntime 构建）              │
  QQ 用户 ─NapCat─► │   :9890  remotedevice 网关                    │
   （在 106 上）    │   ${HA_DATA}/  51G 模型资产（部署不动）   │
                    └────────┬──────────────────────┬────────────────┘
                             │ ws 设备桥             │ ws 设备桥
                 ┌───────────▼──────────┐  ┌────────▼─────────┐
                 │ 192.168.2.106 fnnas  │  │ 192.168.2.30     │
                 │ /opt/waiter/waiter   │  │  mainnas         │
                 │ waiter-remote.service│  │ /opt/waiter/...  │
                 │ + NapCat(25570)      │  │                  │
                 └──────────────────────┘  └──────────────────┘
```

要点：

- **`waiter` 与 `homed` 是两个独立部署单元**，更新其一不影响其二。
- **GUI 不在服务端部署**（见下）。
- 设备桥授权（`device_authorized`）在 **waiter 侧**，网关地址也在
  waiter 的 `waiter.yaml` 里。
- NapCat（QQ 上游）在 **106** 上，`http://192.168.2.106:25570`。

---

### 0.1 ★★ 本机 `diff` 是坏的 —— 所有比对判据的硬前置

**结论：本机一律用 `/usr/bin/diff` 或 `cmp`，不要用裸 `diff`。**

PATH 首位是 `/opt/huawei/harmonyos/ohos-sdk/linux/toolchains/diff`，
它**对任何输入都返回 0 且无输出**。用两个必然不同的小文件验证：

```bash
printf 'A\n' > /tmp/d1; printf 'B\n' > /tmp/d2
diff -q /tmp/d1 /tmp/d2; echo $?              # ⇒ 0（错！应为 1）
/usr/bin/diff -q /tmp/d1 /tmp/d2; echo $?     # ⇒ 1（对）
cmp -s /tmp/d1 /tmp/d2; echo $?               # ⇒ 1（cmp 未被污染）
```

为什么这比“偶尔报错”危险得多：它让**部署判据变成假绿灯**。
2026-09-29 实测：`deploy/scripts/deploy-sdk-site.sh --check` 对着两份**确实不同**的
`index.html`（本地 `f8898cfc…` / 线上 `cc615cfa…`）报「✓ 逐字节一致」，
于是永远判定「无需部署」——站点改完再也不会被更新，而且看不出来。
已在 `547d28d` 修掉（钉绝对路径 + 启动自检）。

★ 判据自检的通用做法：

```bash
# 启动时用两个必然不同的输入验证判据真的能报差异；不通过就退出，
# 而不是继续拿一个坏判据去做决定。
if "$DIFF" -q <(printf 'A\n') <(printf 'B\n') >/dev/null 2>&1; then
  echo '判据自检失败：这个 diff 认为两份不同内容“无差异”' >&2; exit 1
fi
```

同类信号（任一出现就立刻怀疑判据本身）：

| 信号 | 含义 |
| --- | --- |
| `diff` 说无差异，但 `wc -c` / `stat -c %s` 说大小不同 | 判据坏了 |
| 部署脚本报「一致」但线上内容明显是旧的 | 判据坏了 |
| 测了两个**必然不同**的样本却报「相同」 | 判据坏了 |

远端（106 / 30）的 `diff` **是健康的**（`/usr/bin/diff`），不受影响；
但嵌在 `ssh "…"` 里的命令要分清楚是本地还是远端执行。

---

## 1. 部署 homed（本机）

### 1.1 硬前置：必须 onnxruntime 构建

普通 `go build` 只有 ~28MB，**缺 ONNX Runtime**，会让依存句法分析与多模态
向量化失效。生产二进制是 `-tags=onnxruntime`（约 87MB）。

`deploy/packaging/package-linux.sh:139` 会显式拒绝非 onnxruntime 构建。

### 1.2 ★ 构建参数必须与线上一致

```bash
CGO_ENABLED=1 CC=cc go build -tags onnxruntime -o /tmp/homed-ort-new \
  -ldflags "-X .../internal/meta.Version=<v> -X .../internal/meta.Commit=<c>" \
  ./cmd/homed
```

**不要顺手加 `-s -w`。** 加了会 strip 掉符号，产物从 ~86.8MB 掉到 ~78MB ——
体积差 8.8MB 会让人误判成"构建坏了"，而它只是被 strip 了。
若确实要 strip，须先确认线上也是同样参数，否则两次构建不可比。

验证：

```bash
go version -m /tmp/homed-ort-new | grep onnxruntime   # 必须有 build -tags=onnxruntime
ls -l /tmp/homed-ort-new                             # 与 /usr/local/bin/homed 同量级
```

### 1.3 部署

```bash
bash deploy/scripts/deploy-plan.sh check                             # 只读
NEW_BIN=/tmp/homed-ort-new bash deploy/scripts/deploy-plan.sh deploy # 需输入 yes
bash deploy/scripts/deploy-plan.sh rollback                          # 回滚
```

- `check`：服务状态、onnxruntime 标签、`libonnxruntime.so`、模型资产、适配器清单
- `deploy`：备份 → 替换二进制 → 重启 → 验证
- 备份落在 `/var/tmp/homed-backup-<时间戳>/`，含 `ROLLBACK.sh`、旧二进制、
  适配器、unit 文件
- 默认候选 `/tmp/homed-ort`，可用 `NEW_BIN=` 覆盖

### 1.4 部署后必须核对

```bash
systemctl is-active homeagent.service
journalctl -u homeagent.service --since "-3 min" | grep "multimodal space active"
journalctl -u homeagent.service --since "-3 min" | grep -c "registering tool: seq_"  # 应为 7
```

`multimodal space active: provider=chineseclip dim=512` 是 ONNX 链路真的
加载起来的标志 —— 缺它说明已降级，只是没报错。

### 1.4b ★ 判断线上跑的是哪次构建：看 `commit`，别看 `strings`

生产二进制里嵌着 `commit`（`internal/meta`），**这是唯一可靠的判据**：

```bash
K=$(sqlite3 ${HA_DATA}/config.db "select value from config_webui where key='api_key';")
curl -s -H "X-API-Key: $K" http://127.0.0.1:8080/api/v1/status | grep -oE '"commit":"[^"]*"'
# ⇒ "commit":"d084137"   这才是线上真实在跑的提交
```

⚠ **必须带 `X-API-Key`**：无认证时 `/api/v1/status` 返回**登录页 HTML**
（200 + `THEME_PLACEHOLDER`），`grep '"commit"'` 匹配不到任何东西 ——
看起来像"命令没输出"，实际是**认证缺失**。这与 GUI 客户端里
`api()` 专门检测 `THEME_PLACEHOLDER` 是同一件事。

**为什么不能用 `strings` 判**：webui 等插件的静态资源是
`//go:embed` **编译进二进制**的（`internal/plugins/webui/handler.go:27`），
其内容取决于**构建时**磁盘上的文件。

⇒ 已提交过的改动，即使还没部署，线上二进制的 `strings` 里**也可能**
已经出现新代码片段。

2026-09-28 实踩：我据此差点白部署一次（线上 commit 已是 `d084137`，
而我以为还是旧版）。同一天还发生过一次**字节数完全相同**的巧合
（86811464），更掩盖了这个问题。

⇒ 部署前的判据顺序：

1. `/api/v1/status` 的 `commit` —— 线上在跑什么
2. `git log <commit>..HEAD` —— 差哪些提交
3. 那些提交里**有无运行时改动**（`internal/`、`cmd/`）—— 只有它才需要部署
4. 文档 / 判据 / 部署脚本类的提交**不需要**部署

### 1.4c 内置插件 vs 独立二进制

`internal/plugins/<name>/` 是**内置**插件，编译进 homed。
`${HA_DATA}/plugins/<name>/plugin.bin` 是**独立**插件，要单独构建部署。

判定方法（2026-09-28 核实）：

```bash
for p in webui qq cmd seq; do
  printf "%-8s " $p
  ls ${HA_DATA}/plugins/$p/plugin.bin >/dev/null 2>&1 \
    && echo "独立二进制（需单独部署）" || echo "内置（随 homed 部署）"
done
# 2026-09-28 实测：webui/cmd/seq 内置，qq 独立
```

⚠ 目录**存在不等于**有独立二进制 —— `plugins/webui/` 目录存在但**0 个文件**，
走的是内置。改内置插件只需重编 homed。

### 1.5 ★ 适配器升级的保护语义

`${HA_DATA}/adapters/.bundled` 记录**上次随包带出的版本**哈希：

| 盘上版本 | 判定 | 行为 |
| --- | --- | --- |
| 无 `.bundled`（首次升级） | 未知 | **只补缺失文件，不动已有文件** |
| 盘上 == 旧内嵌 | 未被改过 | **自动覆盖**为新版本 |
| 盘上 != 旧内嵌 | **用户改过** | **保留用户版本** |

**2026-09-27 21:50 实测**：生产 `openai.lua` 是 8月26日手工补过 `stream_index`
的版本（盘上 `1a649be2…` ≠ 旧内嵌 `6374c596…`），被**正确判定为用户修改并保留**。

验证方式（部署前后各跑一次，应完全一致）：

```bash
md5sum ${HA_DATA}/adapters/*.lua | md5sum
```

### 1.6 两次部署的真实记录（2026-09-27）

| | 19:45 首次 | 21:50 修复后 |
| --- | --- | --- |
| 二进制 | 86,496,624 → 86,784,400 | 86,784,400 → 86,811,464 |
| onnxruntime | ✓ | ✓ |
| `seq_*` 工具 | 0 → **7** | 7 |
| 适配器 | 无 `.bundled` ⇒ 一律不动 | 有清单 ⇒ 保护用户修改 |
| 备份 | `homed-backup-20260927-194554` | `homed-backup-20260927-215026` |
| `has empty arguments` 误报 | 24 次（仍在增长） | **0 次** |

首次部署前生产二进制构建于**当天 06:36**，而 `seq` 插件引入于更晚的提交 ⇒
旧实例的 `strings /usr/local/bin/homed | grep -c internal/plugins/seq` 为 **0**。
它在 QQ 上如实回答"没有编排工具"**不是说谎**，是确实没有。

> 这类"实例自述与代码状态不一致"，**先查二进制构建时间与内含符号**，
> 别急着怀疑提示词或模型。

---

## 2. 部署 waiter（设备桥，106 / 30）

### 2.1 现状

两台配置相同（同一 `device_gateway`、同一 `device_token`、`device_authorized: true`），
差异只在登录方式：106 走 `admin@` + `sudo`，30 走 `root@`。

### 2.2 更新

```bash
bash deploy/scripts/deploy-waiter.sh check                # 两台一起，只读
bash deploy/scripts/deploy-waiter.sh deploy 192.168.2.106 # 需输入 yes
bash deploy/scripts/deploy-waiter.sh deploy 192.168.2.30  # 上一台验证通过后再做
bash deploy/scripts/deploy-waiter.sh rollback <ip>
```

**逐台更新，不要并行** —— 两台都连同一网关，同时重启会同时断链。

`deploy` 会：备份 `/opt/waiter/waiter` → 替换 → 重启服务 → 验证
（服务 active、进程时长、**配置 md5 未变**）。

### 2.3 部署后确认设备已注册

```bash
journalctl -u homeagent.service --since "-2 min" | grep "device waiter-"
# 期望：device waiter-fnnas online / device waiter-mainnas online
```

**2026-09-27 顺手解决的一个悬案**：106 此前一直没有 `online` 日志而 30 正常，
两台配置与 token 完全相同 ⇒ 差异只可能在旧 waiter 二进制。8月27日那版落在
"未 bind 时收到 ping 会关连接"的缺陷窗口里，更新后即正常。

### 2.4 命令白名单（`device_cmd_allowlist`）

waiter 的命令白名单原本是**源码里硬编码的正则**（18 个命令），`waiter.yaml` 里
**没有任何键能改它** ⇒ `find`/`grep`/`sed`/`sort`/`tr` 这些排查问题最常用的
**只读**命令一律被拒，报错：

```
device_ctl_cmdrun  device_id:waiter-fnnas  error: command not in whitelist
```

现改为 `waiter.yaml` 可配置（2026-09-27 随 `7193446` 部署）：

```yaml
device_cmd_allowlist:
  - ls
  - find
  - grep
  - sed
```

- **替换**默认集而非追加 —— 避免"以为加了 find、结果还留着 `python3 -c` 任意执行"
- 留空 ⇒ 用内置默认集（**绝不能变成"全放行"**，那等于静默拆掉闸门）
- 生效验证：重启后启动日志应打印
  `device cmd allowlist: 22 条（来自 waiter.yaml）`

**当前生产配置**：22 条，只读为主（`ls pwd cat du df free ps ip uname uptime
date hostname find grep sed sort tr wc head tail stat file`）。

> **已知局限**：白名单**只匹配命令名、不看参数** ⇒ `find -delete`、
> `find -exec rm {} ;`、`sed -i`、`sort -o` 仍能放行。
> 这是"命令名清单"，**不是"只读保证"**。参数级拦截是后续项。
> ⇒ 文档与对话里都**不要**把它称作"只读白名单"，那会让人以为写操作被挡住了。

追加到 `waiter.yaml` 的幂等做法（已用于两台）：

```bash
Y=/opt/waiter/waiter.yaml
cp -a "$Y" "$Y.bak-$(date +%Y%m%d-%H%M%S)"
grep -q '^device_cmd_allowlist:' "$Y" || cat >> "$Y" <<'CFG'
device_cmd_allowlist:
  - ls
  - find
  - grep
CFG
systemctl restart waiter-remote.service
```

---

## 3. 故障排查

### 3.1 "消息发过去没反应"

按这个顺序查，**别跳步**：

```bash
# 1) 消息到了吗
journalctl -u homeagent.service --since "-5 min" | grep -E "interrupt from|webhook recv"
#    群聊里没 @bot 会打 not @bot —— 那不是 bug，是设计

# 2) 任务执行了吗（tools=[] 说明模型没发 tool_call）
journalctl -u homeagent.service --since "-5 min" | grep "→ response"

# 3) LLM 通吗
journalctl -u homeagent.service --since "-10 min" | grep -i unreachable

# 4) 代理层活着吗
curl -s --max-time 8 http://127.0.0.1:8081/v1/models -H "Authorization: Bearer <key>"
#    data:[] 是正常的（该网关不列模型），能返回即说明进程活着
```

**"群聊 not @bot"** 与 **"私聊没回"** 是两件事：前者是插件按规则丢弃，
内核压根没收到任务，自然没有"卡死"。

### 3.2 设备命令被拒

```bash
journalctl -u homeagent.service --since "-10 min" | grep "not in whitelist"
```

先确认命令**第一段在不在** `waiter.yaml` 的 `device_cmd_allowlist` 里
（`netstat`、`systemctl` 不在当前的 22 条内，被拒是**正确行为**）。

### 3.3 适配器流式 tool_call 异常

```bash
journalctl -u homeagent.service --since "-10 min" | grep -E "finish_reason=length|unmarshal unified"
```

`非法 JSON 帧` 出现在**启动握手期**属正常（插件启动时的非 JSON 帧），
只在**运行期持续出现**才是问题。

### 3.4 有告警但不确定是不是缺陷

先看**是不是内核的兜底在工作**。例：`重复申请 stage 锁` 是 SDK 模板在每个
stage handler 入口自动加锁 + 锁不可重入所致，**插件侧问题**；内核的强制释放
是有意设计（避免后续插件死锁），不要当内核缺陷去修。详见
[`toolcall-parallel-execution-plan.md`](./toolcall-parallel-execution-plan.md) 末节。

---

## 4. 已知未修

| 项 | 性质 | 说明 |
| --- | --- | --- |
| 白名单不看参数 | 功能限制 | `find -delete`/`sed -i` 能逃，用户已知悉并选择先下发 |
| `重复申请 stage 锁` | 插件缺陷 | 需改 SDK 模板并重编 9月14日的 `plugins/qq/plugin.bin` |
| 群聊必须 @ 才触发 | 设计 | 放宽会在群里引发 unwanted 触发 |

---

## 5. 部署站点（SDK 文档站 / introduce）

`deploy/scripts/deploy-sdk-site.sh` 覆盖两个静态站，**与 homed / waiter 是独立部署单元**。

| 站 | 本地源 | 线上位置 | 构建 |
| --- | --- | --- | --- |
| SDK 文档站 | `third_party/homeagent-sdk/site_build/` | `/vol1/docker/navi-data/sites/sdk` | 需 `tools/apidoc/build.sh`（§5.3 坑①） |
| introduce | `site/`（排除 `README.md`） | `/vol1/docker/navi-data/sites/introduce` | 零构建，源即产物 |

两站部署在**同一台** 106 的同一 `sites/` 目录下。

introduce 是**零构建**的 —— `site/` 里就是成品，脚本部署时**故意不带
`README.md`**（那是仓库说明文档，不是站点资源）。

### 5.1 用法

```bash
bash deploy/scripts/deploy-sdk-site.sh --check                        # 只核对差异，不动线上
bash deploy/scripts/deploy-sdk-site.sh                               # 构建 + 部署 + 验证
bash deploy/scripts/deploy-sdk-site.sh --rollback .sdk-bak-<时间戳>  # 回滚
```

`--check` 逐字节比对本地与线上，两个站都一致时可直接跳过部署。

### 5.2 链路（实测确认）

```
本机 192.168.2.60
  └─ nginx stream 按 ssl_preread SNI 把 443 透传 → 192.168.2.106:3080
       └─ navi 容器内的 nginx（**不是** portal-nginx，那个已 Exited 两周）
            └─ server_name sdk.homeagent.jianfgit.xyz → root /app/data/sites/sdk
                 └─ 宿主对应 /vol1/docker/navi-data/sites/sdk
```

登录 106 只能用 `admin@` + `sudo -n`，`root@` 会被拒。

### 5.3 ★ 两个坑

**① 构建必须走 `tools/apidoc/build.sh`，不能裸跑 `mkdocs build`**

裸跑会丢掉整个 `api/*.md` 和 `llms.txt` —— 而 `llms.txt` 正是**给 agent 直读的入口**。
正确产物 **106 个文件**，裸跑只有 **78 个**。

**② 打包不能走设备网关**

106 的 waiter 白名单（`device_cmd_allowlist`）现有 **22 条**：

```
ls pwd cat du df free ps ip uname uptime date hostname
find grep sed sort tr wc head tail stat file
```

里面**没有 `tar`**（有 `sed` 也不能打包）⇒ `device_ctl_cmdrun` 打不了包，
必须走 SSH 直连。

> 早先这里记的是「只放行 `ls`/`stat`/`find`/`cat`」，那是 waiter 白名单
> 硬编码 18 条时的状况。2026-09-27 部署 `7193446` 后扩到 22 条 ——
> 结论（打不了包）不变但**理由已变**，别照旧文字理解。

### 5.4 新增文档后要记得更新 nav

`mkdocs.yml` 的 nav 没登记的页面，mkdocs 会明确警告
`not included in the nav configuration` —— 等于**文档写完了但在站点里不可达**。

改完 SDK 文档的完整流程：

```bash
cd third_party/homeagent-sdk
# 1) 在 docs/guide/ 写文档
# 2) 在 mkdocs.yml 的 nav 里登记          ← 漏了这步站点里找不到
# 3) 重新生成（含 docs/api/* 与 llms.txt 同步）
tools/apidoc/build.sh
# 4) 部署并验证
bash deploy/scripts/deploy-sdk-site.sh
```

验证线上是否真的可访问（**别只看本地产物**）：

```bash
curl -s -o /dev/null -m 10 -w '%{http_code}\n' -k https://sdk.homeagent.jianfgit.xyz/guide/<新文档>/
```

### 5.5 回滚点

每次部署留 `.sdk-bak-<时间戳>`（同 `sites/` 下）。`--rollback` 会先把当前站
`cp -a` 成 `.sdk-failed-<时间戳>` 再恢复，失败版本不丢。

---

## 6. 部署单元清单（哪些在服务端、哪些不在）

排查"改了什么没生效"时，先确认改动落在**哪个单元** —— 服务端只有前三个。

| 单元 | 入口 | 部署位置 | 服务端部署 |
| --- | --- | --- | --- |
| homed（内核） | `cmd/homed` | 本机 `/usr/local/bin/homed` | ✅ |
| waiter / waitercli | `cmd/waiter` | 106、30 的 `/opt/waiter/waiter` | ✅ |
| 站点 | `site/`、`site_build/` | 106 的 `sites/` | ✅ |
| **GUI** | `cmd/gui` | **不在服务端** | ❌ |

### 6.1 GUI 是客户端，不在服务端

`cmd/gui` 是 **Electron 桌面应用**（`main.js` / `icon-tray.png` /
`devicebridge_dll.js`），随客户端分发，**不在服务端部署**：
生产既无 `*.service` 也无对应进程。

> 核查时的坑：`ps -ef | grep -icE "[e]lectron|cmd/gui"` 会把**它自己的
> grep 命令行**算进去而返回非 0（本机实测返回 2，实际 0）。
> 确认进程是否存在要**看列出的内容**，别只看计数。

⇒ **不要**在服务端找 GUI 的产物或配置；排查 GUI 问题时，
要看**用户机器上运行的客户端**，而不是 `homeagent.service` 的日志。

它与 waiter 的关系是**两端**：GUI 用 `devicebridge_dll.js` 走设备桥协议
连接服务端 waiter，`a781f8e`/`ff69127` 修的正是 GUI 侧 bind 结果判 ok
与登记状态暴露。

### 6.2 ★ 本机 `/usr/local/bin/waiter` 与 106/30 不同步

本机也留了一份 waiter，但**不是** 106/30 那条设备桥：

| 位置 | 版本 | 状态 |
| --- | --- | --- |
| 106 / 30 | `1.4.0`（`55906b8`） | ✅ 最新 |
| 本机 `/usr/local/bin/waiter` | `v1.3.2-153-gff69127`（2026-09-25 构建） | ⚠️ **落后 main** |

本机这份**早于** main 在 2026-09-26 合入的那批修复（设备反复掉线、
bind 判 ok、服务端发现自动链接），因此**缺**它们。

不过它**无进程、无服务**（只有 `~/.config/homeagent/waiter.yaml`），
所以不影响生产设备桥。若要更新，用与 106/30 同样的构建：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -ldflags "-X .../internal/meta.Version=1.4.0 -X .../internal/meta.Commit=$(git rev-parse --short HEAD)" \
  -o /tmp/waiter-new ./cmd/waiter
```

### 6.3 一次误判的记录：别只看"不在分支上"

2026-09-27 排查时，我用 `git merge-base --is-ancestor ff69127 origin/main`
判定"这批提交没进 main"，并据此推断"存在一条未合入、只靠 reflog 撑着的
5 提交线"。**该结论是错的。**

真实情况：这批改动在 2026-09-26 以**新 hash** 重做并进入 main：

| 旧（9-25 那条线） | 新（main 上） | 内容 diff |
| --- | --- | --- |
| `1acbd39` 通用反向代理 | `1e58af7` | 0 行 |
| `3d30482` 反代两处故障 | `5d278cf` | 0 行 |
| `078517e` 设备桥自动链接 | `7f5bf16` | 0 行 |
| `aaafaac` 修复设备反复掉线 | `94c74b2` | 0 行 |
| `ff69127` GUI bind 判 ok | `a781f8e` | 0 行 |

逐字节一致，无需合入。之所以误判，是因为只查了**旧 hash 的祖先关系**，
没查**提交标题/内容**是否已有等价副本。

⇒ **判定"某改动是否已合入"要按内容查，不能只按 hash 查。**
   同一改动可能被重做为新 hash；此时 `merge-base` 必然说不包含，
   而 `git merge-tree` 报的 13 个"冲突"正是同源改动做两遍的必然结果。

另注：`aaafaac`/`94c74b2`（修复设备反复掉线/静默失联：ping 路径断连 +
bind 结果无人处理）正是 106 此前长期无 `online` 日志的成因，
2026-09-26 已进 main，今天部署的 `1.4.0` 包含它。

---

## 7. WebAPI 压测（webui + kbtree + pluginmgr + remotedevice）

脚本：`scripts/kernel-stress/webui-bench.py`，打的是**生产实例**。

```bash
K=$(sqlite3 ${HA_DATA}/config.db "select value from config_webui where key='api_key';")
python3 scripts/kernel-stress/webui-bench.py --key "$K" --probe          # 先探测
python3 scripts/kernel-stress/webui-bench.py --key "$K" --scale 3 --json /tmp/w.json
```

### ★ 8080 上有 4 个插件注册路由，但监听端口不止 8080

第一版我只压了 webui，**漏了 pluginmgr 与 kbtree 的根路由**。
`ss -ltnp` 实测：

| 端口 | 插件 | 路由 | 认证 |
| --- | --- | --- | --- |
| `127.0.0.1:8080` | webui | `/api/v1/*` | `config_webui.api_key` |
| | remotedevice | `/api/v1/device/*` | `config_remotedevice.ws_token` |
| | kbtree 子路径 | `/api/v1/knowledge/tree/{categories,counts}` | `config_webui.api_key` |
| `127.0.0.1:9876` | **pluginmgr** | `/plugins`（**无** `/api/v1`） | **无需认证** |
| `127.0.0.1:9892` | **kbtree** | `/categories` `/counts` `/search`（**无**前缀） | `config_kbtree.token` |
| `127.0.0.1:9890` | remotedevice | 设备 WS 网关（非 REST） | 不压 |

⇒ 「打 `/api/v1/*`」这个假设只对 8080 上的 webui 成立。
`:8080/plugins` 实测 **404**。

### ★ 三条判据

1. **必须先 `--probe`**：webui 无认证时返回 **200 + 登录页 HTML**
   （含 `THEME_PLACEHOLDER`）。**状态码是 200** ⇒ 只看 `http_code`
   会把登录页当健康响应。
2. **只压只读端点**：`/chat`（真调 LLM）、`/chat/interrupt`（中断在跑
   的任务）、`/settings/*` `/plugins/*`（改配置）、`/login` `/logout`、
   `/device/push`（向真实设备下发）、`/device/ws`（长连接）—— 全部排除。
   `/api/v1/device/online` 收进来之前逐行读过实现：仅 `GET` +
   `registry.OnlineList()`，纯读。
3. **配置库只读打开**（`mode=ro`）—— 压测脚本绝不能碰生产库写路径。

### 实测（2026-09-28，24 端点 × 3 档）

| scale | 请求 | 吞吐 | p50 | p95 | p99 | max | 成功 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 480 | 953 req/s | 4.0ms | 23.0ms | 34.7ms | 40.6ms | **480/480** |
| 2 | 960 | 1310 req/s | 7.9ms | 32.4ms | 48.4ms | 72.3ms | **960/960** |
| 3 | 1440 | 1422 req/s | 12.0ms | 42.7ms | 60.5ms | 88.7ms | **1440/1440** |

（24 端点 × 20/40/60 轮 = 480/960/1440）

**2880 请求零失败**；压测期间服务端 `active`、0 个 5xx。

**p99 随并发单调上升**（34.7 → 48.4 → 60.5ms），符合排队预期。
上一轮曾出现 scale=2 的 p99 反常地高于 scale=3，当时机器上另一个 agent 的
chromium 占 766% CPU ⇒ **环境噪声**，已明确不作为性能特征。

### 踩过的三个坑（都在脚本注释里）

1. **端点表漏 `/api/v1` 前缀** ⇒ 18 个端点全 404，而同一时刻 curl 是 200。
   不 probe 直接压 ⇒ 结论会是「webui 全挂」。
2. **前缀补了两次** ⇒ `remotedevice` 拼成 `/api/v1/api/v1/device/online`
   ⇒ 落到 webui 兜底路由、返回 **200 + 登录页**（probe 的
   `looks_like_login_page()` 抓到的）。反向的「表里含前缀 + 代码仍补」
   又让 webui 组全 404 ⇒ 最终统一成**表里写完整路径、代码不补**。
3. **`contextlib.suppress(sqlite3.connect)`** —— `connect` 是函数不是
   异常类，`suppress` 会抛 `TypeError`。改回显式 `try/except sqlite3.Error`。

---

## 8. grandchild 测试：不稳定的是**测试设计**，不是生产代码

`internal/plugin/proc` 的 `TestKillReturnsEvenWhenGrandchildSurvives`
曾在 `go test ./...`（600s 超时）与 `make test`（20.4s FAIL）里失败，
但**单独跑 0.24s 通过**、连跑 3 次全绿 ⇒ 「单跑绿、合跑红」。

2026-09-28 定位，结论是**三个测试设计缺陷**，生产代码（`process.go`）没问题。

### 缺陷 1：名字说 Survives，实际测的是「被杀」

| 测试 | spawn 的源 | 孙进程 | `kill(-pgid)` 能杀吗 |
| --- | --- | --- | --- |
| `…EvenWhenGrandchildSurvives` | `grandchildPluginSource` | `sleep 400`，**不设** Setpgid，留在进程组内 | **能** |
| `…WhenGrandchildEscapesProcessGroup` | `escapingGrandchildSource` | `sleep 401` + `Setsid: true` | **不能** |

`grandchildPluginSource` 自己的注释写着「孙进程**不**设 Setpgid：它要留在
插件的进程组里」⇒ 第一个测试里孙进程**不会 Survive**。
⇒ 容易让人误以为「脱组场景已被覆盖」，而它其实覆盖的是另一个场景。

**已改名** `…WhenGrandchildDiesWithProcessGroup`，名字与实现一致。

### 缺陷 2：判据数的是**全系统**进程

`countShimGrandchildren()` / `countEscapingGrandchildren()` 扫 `/proc` 找
`"sleep 400"` / `"sleep 401"` 字符串，**不区分父子关系** ⇒ 同机任何命中同样
cmdline 的进程/容器都会串味。

原注释记过一次前车之鉴（「我第一版就踩了：明明单跑通过，合跑却红」），
但当时只加了 base 快照，**没解决全局匹配这个根因** —— base 也救不了
「别的测试中途拉起 sleep 400」。

**已修**：新增 `procPPid()`，两个计数器都限定 `PPid` 属于本测试的插件。
顺带补上 `e.Name()` 的 `Atoi` 校验（原来会把 `/proc/self`、`/proc/net`
这类非数字目录也去读 cmdline）。

### 缺陷 3：defer 清理「拿不到 pid 就整个跳过」

```go
if pid := pluginPid(p); pid > 0 { syscall.Kill(-pid, SIGKILL) }
```

pid 取不到时**静默跳过** ⇒ 残留 `sleep 400` 污染后续测试 ⇒ 变成下一个测试的
假失败。

**已修**：新增 `cleanupSleepMarkers(marker)`，按唯一 cmdline 标记兜底清理，
即使 `pluginPid` 返回 0 也执行。

### ★ 我被推翻的一个假设（记下来免得重犯）

我一度认定根因是 `waitLoop` 里 `p.cmd.Wait()` **先阻塞**、拆管道在**之后**
（`process.go:359-367`）：Go 的 `exec` 里 `Wait()` 会等 copy goroutine 结束，
而那些要等所有管道写端关闭 —— 孙进程持有着，死锁。

**实测推翻了它**：把拆管道提到 `Wait` 之前，那个测试**5 次全 FAIL**
（改前只是偶发）。说明真正的根因是上面三个测试设计问题，
而「Wait 阻塞」是**被孙进程持管道放大**的效应，不是缺陷本身。

⇒ 改了生产代码不但没修好，还把偶发变成必现。**先证明因果再动手。**

### 判据

`internal/plugin/proc/grandchild_design_test.go`，4 条。写它时踩了个坑：
它是**查源码文本**的，我一改源文件（改名/加 ppid 限定/加兜底清理），
锚点就全过期 ⇒ 三条判据一起红。

⇒ 判据自己被重构打断时，要改的是**判据的锚点**（认新旧两种形态），
不是回退修复。最后把判据①从「解函数体比对 spawn 参数」简化为
「只问名字是否还说 Survives」—— 少耦合一层，少失效一处。

变异测试（三个都抓到）：改名回 Survives / 抽掉 ppid 限定 / 去掉兜底清理。
