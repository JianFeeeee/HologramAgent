# 星图布局共享源（WebUI / GUI 共用）

## 一句话

这里只共享**布局数学**——3 个不碰 DOM、不碰全局状态的纯函数。
**不是**在合并 WebUI 与 GUI 的星图实现，两者本就不该合并。

## 共享范围（就这么大）

| 文件 | 内容 |
|---|---|
| `layout.js` | `starmapLayoutRelax`（力导向松弛）、`starmapLayoutIterationsFor`（迭代数自适应）、`starmapLayoutMaxMoveThreshold`（收敛早停阈值） |

判据实测：**153 行代码、3 个函数、零 `document`/`window`/`THREE`/`state.*` 引用**。
入参只有 `flat`（扁平坐标数组）、`nodes`、`edges`、`maxR`，出参是同一份坐标。
没有 UI、没有状态、没有渲染——纯数学。

## ★ 明确不共享的部分（别去"补完"它）

WebUI 与 GUI 是**两种不同的客户端**，不是同一份前端的两个皮肤：

| | WebUI | GUI |
|---|---|---|
| 形态 | 浏览器页面，Go `//go:embed` 进二进制 | Electron 桌面应用，`file://` 加载 |
| 视图体系 | 侧栏页签（概览/对话/星图/插件/设置/适配器/内核） | 自有面板 + 托盘 + 设备桥 |
| 星图承载 | 概览页门面 + 聊天面板 + 独立页签（三容器搬运） | 聊天内的星图子面板 |
| 节点标签 | `starmapDrawLabels` 2D 覆盖层 | 每节点 canvas sprite |
| 活跃集概念 | `starmapMorphSet`（mesh 集合） | `state.starmapGrown`（nodeId→时间戳） |
| 独有能力 | SSE `Last-Event-ID` 重放、`smAdaptGraph` 归一化、星图页签 | computer use、`disposeStarmapObject` 显式 GPU 释放、`starmapPanelActive` 面板判据 |

实测：WebUI 星图侧有 **54 个函数**，GUI 只有 **24 个**；两份文件 69 个同名函数里
**逐字节相同的为 0**（缩进风格都不同：GUI 2 空格 / WebUI tab）。
强行统一这些是**跨客户端 UI 重构**，风险远大于收益。

⇒ 只有「布局数学」这一层，两边的**语义本就该完全相同**、且**分叉的代价已被证明高昂**，
才值得抽取。其余部分请各自演化。

## 为什么必须共享这一层

分叉的代价不是理论上的，已经兑现过两次：

1. WebUI 把 O(n²) 全对全布局换成网格近邻（生产库 3754 块：3.52 亿次配对 → n·k 次），
   **GUI 那份没跟着改** ⇒ 换客户端卡顿原样存在（实测单次建图阻塞 1.8–9 秒）。
2. 把空闲帧跳过从 WebUI 抄进 GUI 时抄了 `starmapMorphSet`（**WebUI 独有**），
   GUI 实际用 `state.starmapGrown` ⇒ 渲染循环**每帧 ReferenceError、星图 0 帧**。
   JS 不校验未声明标识符，语法检查看不出来。

## 机制

```
shared/starmap/layout.js        ← 唯一事实源（改这里）
        │
        ├─ tools/starmap-sync.mjs ─→ internal/plugins/webui/dashboard.js  [BEGIN/END 标记块]
        └─────────────────────────→ cmd/gui/renderer/app.js               [BEGIN/END 标记块]
```

为什么用「编辑期生成」而不是「运行时加载」：GUI 用 `file://` 且可能离线启动，
让它去服务端拉 `dashboard.js` 会把「星图能否初始化」绑到网络与登录态上；
反过来让 WebUI 内联 GUI 的代码要动 `go:embed` 与启动顺序。
生成器两边风险都避开：**运行时零依赖，两侧各自仍是自包含文件**。

## 用法

    node tools/starmap-sync.mjs          # 同步
    node tools/starmap-sync.mjs --check  # 只校验，漂移则退出 1
    make check-starmap-shared            # 同上（已接入 make test）

★ 改布局算法**只改 `layout.js`**，然后跑 sync。
手改任一副本会被三处门禁同时拦下（生成器 `--check` + WebUI 的 Go 判据 + GUI 的 `.mjs` 判据）。
