# HologramAgent GUI 设计说明与外部借鉴

本文件记录 GUI（`cmd/gui`）在设计过程中**明确借鉴的外部实现**，以及本仓自行做出的设计决策。

目的是让后来人能判断：某段 UI 行为是本地决定，还是有外部出处。这对追溯许可、
理解设计动机、以及避免无意中"重新发明"已有方案都有用。

---

## 借鉴来源

### PiDeck（`C:\Users\21989\AppData\Local\Programs\PiDeck`）

**PiDeck 是本机安装的一个第三方 Electron 应用**（其前端依赖
`@deepseek-ai/dsh-web-frontend`）。HologramAgent GUI 在交互设计上参考了它的以下
做法。参考方式为**阅读其打包产物 `app.asar` 中的 CSS 实现**，理解其设计意图后
在 HologramAgent 的样式体系中独立重写 —— 未复制其代码，也未使用其资源文件。

| 借鉴项 | PiDeck 的实现 | HologramAgent 的落地 | 动机 |
|---|---|---|---|
| **加载 / 思考动画** | `@keyframes _dsh-state-dot-chase`（三点透明度递减 `1 → .6 → .35 → .15`）与 `tool-activity-dot`（上浮 + 缩放） | `renderer/style.css` 的 `haDots` / `haDotFloat` | 旧实现是 `border + rotate` 的经典 spinner。旋转语义上暗示"在加载某个确定的东西"，而 agent 思考本身没有进度可转；且在 15px 的消息气泡里转圈糊成一团 |
| **运行态呼吸光晕** | `.turn-row--running:before` —— 一层几乎看不见的 accent 底色（`color-mix(... 3%)`）以 `2.2s` 周期做 `opacity: 0 → 1 → 0` 呼吸 | `.msg-bubble.msg-running::before` | 单看气泡里的几个点，视线要缩到一小块；光晕铺在整条消息上，余光就能感知"agent 正在回" |
| **工具调用分组卡** | `.tool-group-card` + `.tool-group-card-dot`（`toolGroupDotPulse`：缩放 + 透明度脉动）与 `tone-running/tone-error` 三态 | `renderToolGroup()` / `renderToolGroupOrSingle()` + `tool-group-card` 样式 | 原来一轮里每个工具调用各占一张卡，调 5 个工具就是 5 张卡竖排，把真正的回复挤到很下面；而这些卡形态高度相似（图标+工具名+状态），信息密度极低 |
| **组合键处理** | ——（PiDeck 为 Web 应用，不涉及系统级输入注入） | —— | —— |

**关于分组卡的规则差异**：PiDeck 的分组策略我们理解为"折叠成组"，
HologramAgent 额外加了一条规则 —— **只有 1 个工具时保持单卡**（组卡没意义，
反而多一层点击）。这是本地决策，不是 PiDeck 的做法。

---

## 本仓自行做出的设计（未借鉴）

以下几项与 PiDeck 无关，是为 HologramAgent 自身场景做的决定：

- **SSE 取代 EventSource**：需要自定义 `X-API-Key` 与 `Last-Event-ID` 请求头，
  浏览器原生 `EventSource` 不支持。
- **增量聊天渲染**（`applyIncrementalChatRender`）：按 `data-msgkey` 复用 DOM
  节点。性能问题由本仓实测定位（旧实现 200 条消息单次全量重建 235ms）。
- **设备令牌注入**（`isDeviceTokenPath` / `deviceApiKey`）：HologramAgent 服务端
  `/api/v1/device/` 命名空间下并存两套鉴权（`requireAPI` 认 cookie、
  `requireToken` 只认 `X-API-Key`），需要客户端主动带令牌。
- **静默启动的"创建后隐藏"**：不创建窗口会导致渲染进程不启动（SSE、设备桥
  全部失效），且托盘菜单唤不起窗口。PiDeck 为纯前端应用，无此问题。
- **原子写 connections.json**：`rename` 原子性用于消除写入竞争导致的配置清空。

---

## 其他外部依赖

GUI 的第三方库已全部本地化到 `renderer/vendor/`，与
`internal/plugins/webui/static/` 保持字节一致（由 `package-config.test.mjs` 钉住）：

| 库 | 版本 | 许可 | 用途 |
|---|---|---|---|
| three.js | r128 | MIT | 3D 星图 |
| OrbitControls.js | r128 examples | MIT | 星图视角控制 |
| marked | 4.3.0 | MIT | Markdown 渲染 |
| DOMPurify | 3.2.4 | Apache-2.0 / MPL-2.0 | HTML 净化（**安全必需**，不可降级） |

运行时依赖 `koffi`（MIT，FFI）用于调用 `user32.dll` 实现键鼠注入。
