# 场景式记忆修复 plan

> **状态：大部分落地（2026-10-31 复核）。** 7 个步骤里 5 步已完成。
>
> | 步骤 | 状态 |
> |---|---|
> | 1 修 R1+R2（归一化、时段污染主键） | ✅ 已完成 |
> | 2 SDK 补 `ScenePolicy` 声明项 | ✅ 已完成（`sdk/plugin.go` 有 `ScenePolicyAuto` / `ScenePolicyNone`） |
> | 3 修 R3（图整理覆盖全库） | ✅ 已完成 |
> | 4 清理现网垃圾场景 | ❓ 未复核（属现场操作，不在代码里） |
> | 5 修 R5（证据桶别全表清） | ✅ 已完成 |
> | 6 修 R4 覆盖面（peer / 语义场景） | ✅ 已完成（`scene_emerge.go` 已区分 `peer:group_*` 与 `peer:user_*`） |
> | 7 验证与收口 | ❓ 未复核 |
>
> 剩余不确定项已索引到 [`future-plan.md`](./future-plan.md) §六。
> 未完成项已索引到 [`future-plan.md`](./future-plan.md) §六。

> 起点：2026-09-26 生产库实测（`${HA_DATA}/memory/graph.db`）。
> 现象：65 个场景中 6 组是同一场面的双胞胎键；主力场景 `auto:chan:qq+part:morning`
> strength=270、6 个 features、**0 条记忆**，日志里被"命中"179 次。
> 分支：`feature/scene-writeback`（从 main 拉出，工作树干净）。

---

## 根因（三条，逐条修）

### R1 建键不过归一化（病因）

`internal/memory/scene_emerge.go:329`

```go
base := "auto:" + sig.Label(2)     // Label 用 "+" 拼接，未归一化
```

而同一层的另外三条路**都**过了归一化：

| 位置 | 是否归一化 |
|---|---|
| `EnsureScene`（声明建键）`scene_emerge.go:555` | ✅ |
| `effectiveScenes`（写侧挂 ref）`graph.go:1397` | ✅ |
| `RecallByScene`（读侧召回）`scene.go:328` | ✅ |
| **`createSceneLocked`（涌现建键）`scene_emerge.go:329`** | ❌ |

`normalizeSceneSegment` 把 `+` 归一成 `_`，所以涌现键天生带 `+`、其余三方说 `_`。
`scenes.key` 虽有 `UNIQUE` 约束，但**两个不同字符串都合法**，拦不住。

实测 6 组双胞胎（`REPLACE(key,'+','_')` 后重名）：

```
auto:chan:qq+part:morning            ↔ auto:chan:qq_part:morning      (270/0 refs)
auto:chan:mc:event+topic:mc          ↔ auto:chan:mc:event_topic:mc
auto:chan:mc:system+topic:mc         ↔ auto:chan:mc:system_topic:mc
auto:chan:mc-resident+topic:mc       ↔ auto:chan:mc-resident_topic:mc
auto:chan:homeagent-mail-bridge+...  ↔ auto:chan:homeagent-mail-bridge_...
auto:chan:system+topic:任务           ↔ auto:chan:system_topic:任务
```

### R2 `part`（时段，权重 0.2）污染主键与门槛

`Label(2)` 取权重最高的 2 个特征。特征顺序是
`chan(1.0) → peer(1.0) → tool(0.8) → topic(0.4) → part(0.2)`，
只有当前 2 个强特征不足时 `part` 才会进键——于是出现
`auto:chan:qq+part:morning` 这种"时段成了场景身份"的名字。

**更糟的是 `recordSituationEvidenceLocked` 也用 `Label(2)` 做桶**（`scene_emerge.go:380`），
所以 `part` 不只影响名字，还决定 `minSceneEvidence=2` 这个门槛在哪个桶里计数。

实测证据（键名与实际指纹自相矛盾）：

```
场景键: auto:chan:qq+part:morning   strength=270
实际指纹: [chan:qq part:evening]    ← 19:15 命中，evening 却并入 morning
```

这不是 bug 触发，是加权 Jaccard 正常工作：共享 `chan:qq`(1.0)、
并集含 `part`(0.2×2)，相似度 1.0/1.4 = 0.714 > `joinSceneThreshold` 0.5。

### R3 图整理心跳漏扫 `scenes`

`detectEntityMerge` 唯一遍历入口 `Recall(nil,nil,1,"")`，其全量路径只查
`entities`(`graph.go:609`) + `relations`(`graph.go:631`)，`scenes` 不在其中。
所以 R1/R2 造成的双胞胎从 9-15 起无人发现，23 号空转到 strength=270。

**R3 是"没被发现"的原因，R1/R2 是病因。** 顺序不能反。

### R4 场景身份只有「输入通道」一个可靠维度

现网 65 个场景键的维度分布（实测）：

```
auto:chan（涌现·通道主导）  36
chan:  （输入通道）        26
tool:  （工具）             3
peer 主导                  0
topic 主导                 0
```

两个独立原因叠加：

1. **采集侧恒空**：`situationFeaturesFor` 从 `evt.Payload` 找
   `peer/peer_id/group_id/user_id/chat_id`，而全仓**没有任何插件在 InjectInput
   时填这些键**（`group_id` 只出现在 `output.go:145` 的发送侧帮助文本里）。
   ⇒ 日志中 `peer` 特征出现次数为 **0**。
2. **排序上被挤掉**：`chan` 与 `peer` 权重同为 1.0，而 `NewSituation` 按权重
   降序**稳定**排序，`chan` 先 append 就永远在前 ⇒ 即使采集到 peer，
   `Label(2)` 也轮不到它。

后果：「跟谁对话」这个本该最强的身份信号（权重与 chan 并列）**根本进不了
场景身份**。同一件事在 QQ 和 Telegram 上会落进不同场景而无法共享。

**R4 与 R1/R2 是不同层面的问题**：R1/R2 让键构造正确，R4 决定键**能表达
什么**。R1 修完后 `auto:chan:qq` 会取代 `auto:chan:qq+part:morning`，
但它依然只认通道——所以 R4 不修，修复效果只到「正确的单一维度」。

### R5 `situation_evidence` 全表清空

`createSceneLocked` 新场景一成立就 `DELETE FROM situation_evidence`
（不只删本指纹的足迹）。多场景并发轮次下，A 场景的建立会连带清掉 B 尚未
攒够 `minSceneEvidence=2` 的证据 ⇒ 门槛判定被别的场景的建立随机打断。

### R6 通道侧没有「是否参与场面识别」的声明项（SDK 缺口）

`ChannelDef` 的记忆相关声明已有三件套，语义各管一轴：

```
NoMemory       进不进记忆计算
ContextPolicy  裁不裁上下文（破坏性，默认关）
RecallPolicy   召不召回记忆（只读，默认开）
```

**唯独没有「这条通道是否参与场面识别」。** 现状是**无条件参与**：
`situationFeaturesFor` 里只要 `evt.Source != ""` 就塞一个 `chan` 特征，
没有可关的开关 ⇒ 现网 `chan:system` / `chan:kernel` / `chan:timer` 这类
**纯内部信噪通道也在参与场面聚类**。

穷举确认不是查漏：编译使用的就是 `third_party/homeagent-sdk`（go.mod replace），
`plugin.go` 中 `scene` 出现 0 次，SDK 自身 git 历史 `-S'Scene' -- sdk/` 为空。

已有但未被使用的另一个口子：`Payload["scene"]`（`memorypass.go:37`）允许插件
在单次注入时声明场景键（string/[]string/[]interface{} 三形态，来自 `d98bf51`）。
现网 **0 个插件使用**，24 个 declared 场景全是 `ChannelScene(evt.Source)` 派生。

### R7 渠道本身可以覆盖多个场景

`payload["scene"]` 传 `chan:qq/peer:group_1` 这类**层级键**时，
`RecallByScene` 的 `(key = ? OR key LIKE ? || '/%')`（`scene.go:351`）
支持前缀召回——这层能力已存在，但因 R6 无人使用而闲置。

---

## 步骤

### 步骤 1：修 R1 + R2（源头，不碰存量）✅ 已完成

- [x] `Label` 只取权重 ≥ `labelFeatureWeight`(0.5) 的主导特征，结果过 `NormalizeSceneKey`
- [x] `createSceneLocked` 的 `base` 再做一次防御性归一化；label 为空时拒建无名场景
- [x] 冲突后缀 `#N` → `.N`（`'#'` 会被归一化成 `'_'`，是第四处双胞胎来源）
- [x] 判据：`scene_key_test.go` 6 例，先红后绿（4 红 1 绿 → 全绿）
- 提交：`1fa9ef6`

### 步骤 2：SDK 补 `ScenePolicy` 声明项（公开接口，可动）

按 `ContextPolicy` / `RecallPolicy` 的既有风格补齐（同一文件、同一形状）：

- [ ] 常量：`ScenePolicyAuto = "auto"` / `ScenePolicyNone = "none"`
- [ ] 校验：`ValidScenePolicy(policy string) bool`（空串等价默认）
- [ ] `ChannelDef.ScenePolicy string` + json tag `scene_policy,omitempty`
- [ ] `InjectOptions.ScenePolicy string`（单次注入可覆盖通道默认）
- [ ] 内核接线：
      - `internal/agent/io/channel.go:349-357` 的 payload 搬运加一条 `scene_policy`
      - `situationFeaturesFor` 读到 `none` 时**不产任何特征**（连 `part` 也不产——
        一个不参与场面识别的通道不该留下时段噪声）
      - 声明路 `sceneKeysFor` 同样受 `none` 约束
- [ ] 判据：`none` 通道连续 5 次交互，`scenes` 表行数不变
- [x] **存量标注：不做**（用户裁定 2026-09-26：「所有都默认开启，因为多写无影响，
      少写会缺场景」）。实测支持：8 个 0-refs 通道合计 70 strength、0 条记忆，
      召回返回空；且 declared 场景**不进**相似度空间
      （`loadEmergentScenesLocked` 只取 `origin='emergent'`），
      故多写对聚类零影响。声明项作为「插件将来确实需要时」的闸门保留。

### 步骤 3：修 R3（让图整理覆盖全库）✅ 已完成

- [x] `GraphDB.DedupeScenes()`：归一化后同名场景合成一个
- [x] 接到 `mergeLoop` 尾部（不塞进实体的 O(n²) 双重循环）
- [x] 判据 8 例（先红后绿）
- 生产库副本实测：65 → 57，合并 8 组，`auto:chan:qq_part:morning`
  的 refs/rel/ent 一条没丢，strength 1 → 272
- 提交：`758ec11`

- [ ] 给 `mergeLoop` 加**独立的场景去重路径**，不塞进实体那个 O(n²) 双重循环
      （理由：实体 1 万行 × bigram + LLM 裁决，实测 5000 万次配对/轮；
        场景表小且**已有现成的 `situationSimilarity` 加权 Jaccard**，语义更准）
- [ ] 键归一化后相同 ⇒ 合并 refs/features/strength，**不经 LLM**（键相同已证明同一场面）
- [ ] 判据：构造两个 `+`/`_` 孪生键，跑一次 mergeLoop 后期望合成一个

### 步骤 4：清理现网垃圾场景，让它重新生成

用户明确要求：**直接清理，重新生成**（不做保守迁移）。

> ⚠️ **硬约束：必须先部署含 R1 修复的二进制**。反过来的话，重新涌现出来的
> 还是带 `+`/`#` 的旧键，清一场白清。

#### 部署前基线（2026-09-26 20:1x 实测）

```
PID 92115 / 启动于 Sat 19:12:43
子进程插件 25 个
近 24h 异常日志（Fatal/panic/DATA RACE）: 0
场景数 65
`homed --version` 不存在（flag 未定义）⇒ 纪律清单那条用 status 接口或 ps 核对
```

#### 执行顺序

- [ ] 1. `make build`（默认 `HOMED_TAGS=onnxruntime`，cgo；校验 onnx/cgo 标记）
- [ ] 2. 备份：`sqlite3 graph.db ".backup 'graph.db.bak-<ts>'"`（**禁用 cp**，WAL 不一致）
- [ ] 3. `install -m 0755 build/homed /usr/local/bin/homed`（**禁用 cp**，会写坏运行中进程映像）
- [ ] 4. `systemctl restart homeagent`
- [ ] 5. 后置验证：进程起来 / 子进程插件数恢复 / 异常日志为 0 / 真实对话跑通
- [ ] 6. 清理：删 `origin='emergent'` 全部 + 关联 features/refs + `situation_evidence`
- [ ] 7. 等 ≥2 次同类交互让场景重新涌现
- [ ] 8. 复验：新键不含 `+`/`#`、有 features **且**有 refs、strength 合理

- [ ] `sqlite3 .backup` 备份（**禁用 cp**，WAL 模式会拷出不一致快照）
- [ ] 删 `origin='emergent'` 的全部场景 + 其 `scene_features`/`scene_refs`
- [ ] 同步清 `situation_evidence`
- [ ] 重启 homed，等 ≥2 次同类交互让场景重新涌现
- [ ] 复验：新场景键**不含 `+`/`#`**、有 features **且**有 refs

> 清理**不影响记忆本体**：868 条 active 关系与 1179 个实体都在
> `relations`/`entities` 表，与 `scenes` 无外键依赖。
> 兜底不冷场：`chan:qq` 声明场景（strength=281、108 条关系）全程保留，
> 涌现重建期间它继续承担 QQ 场景召回。

### 步骤 5：修 R5（证据桶别全表清）✅ 已完成

- [x] `DELETE FROM situation_evidence` → `... WHERE label = ?`（桶键 = `sig.Label(2)`，
      **不带 `auto:` 前缀**——这点修的时候差点栽）
- [x] 判据 3 例：判据实测「6 个通道各来 3 次只长出 2 个场景」
- 提交：`758ec11`

### 步骤 6：修 R4 的「覆盖面」部分（让 peer / 语义场景进得来）

前置：先只读调查各插件 InjectInput 时手上有什么，产出结论表再动。
`payload["scene"]` 的口子已存在（R6），插件声明比内核猜 peer 更直接。

- [ ] 调查：qq / mail-bridge / a2a / acp / webui / cli … 各自可声明什么
- [ ] 排序侧：`chan` 与 `peer` 权重同为 1.0 而 `chan` 恒在前（稳定排序），
      即使采集到 peer 也进不了 `Label(2)` ⇒ 需要 peer 优先或提权
- [ ] 判据：构造「同一 chan、不同 peer」的两轮，期望落进**不同**场景

### 步骤 7：验证与收口

- [ ] `go build ./...` + 全仓 `go test ./...`
- [ ] `go test ./internal/memory/... ./internal/agent/core/...` 全绿
- [ ] SDK 接口冻结检查：`git diff main -- third_party/homeagent-sdk/sdk/` 的变化
      **已获用户授权**（开发阶段），但需在提交信息里写明「纯追加、omitempty、
      老插件行为不变」
- [ ] 现网观察：场景命中后能查到 refs（非 0）
- [ ] 现网观察：场景键前缀分布不再 100% 锚在 chan（R4 未做则保持挂账）
- [ ] `git_release_check.sh` 无新增红项（SDK 冻结项变化属预期）

## 不做的事（防反复挂账）

- **不**把 `scenes` 塞进 `memory_merge` 工具：该工具语义是"实体删除 + 关系重定向"，
  场景合并需要"特征并集 + refs 重定向 + strength 相加"，是另一套操作，硬塞会让工具语义变危险。
- **不**改 `validGraphNodeKind`：它只管 `memory_block_edges` 端点校验，与 `scenes` 无关。
- **不**给实体那个 O(n²) 循环做优化：属独立问题（已实测：1 万实体→~224GB 瞬时分配/轮），
  混进本次修复会让 diff 失焦。单独开条目。
- **不**在 R6 里动 `ValidContextPolicy` / `ValidRecallPolicy` 的既有语义：
  新增项是纯追加，不借机改旧行为。
