# HologramAgent 鸿蒙客户端

HarmonyOS / OpenHarmony 原生客户端，用 ArkTS + ArkUI 实现（不是 WebView 套壳）。
功能与 WebUI 对齐：SSE 流式对话、工具调用卡片、思考过程折叠、附件上传预览、
设备桥、插件管理、设置编辑、宽屏双栏、深浅色主题。

## 工程结构

```
HologramAgent/
├── AppScope/                     应用级配置与图标
├── oh_modules/                   依赖（.gitignore 忽略，但**必须存在**，见下节）
├── entry/src/main/
│   ├── ets/
│   │   ├── common/               通信、状态与纯逻辑（无 UI）
│   │   │   ├── ApiClient.ets           REST 客户端（X-API-Key 鉴权、超时、二进制附件）
│   │   │   ├── SseClient.ets           SSE 长连接（Last-Event-ID 断线续传）
│   │   │   ├── ConnStore.ets           连接配置与设备身份持久化
│   │   │   ├── StatusStore.ets         运行状态缓存（单例 + AppStorage 广播）
│   │   │   ├── Constants.ets           主题色板、圆角、超时、分页大小
│   │   │   ├── UserError.ets           错误转人类可读文案
│   │   │   ├── NavBarController.ets / NavStackRegistry.ets  导航栏显隐与导航栈登记
│   │   │   ├── ChatStore.ets           聊天状态机（消息数组/分页/SSE/防抖刷新，单例）
│   │   │   ├── ChatSse.ets             SSE 事件 → 状态翻译（ChatStreamSink 接口）
│   │   │   ├── ChatSession.ets         发送/中断（POST /chat、/chat/file）
│   │   │   ├── ChatHistory.ets         历史载荷与 tool_calls 解析
│   │   │   ├── ChatFormat.ets          ForEach 键、工具卡状态/配色、渠道判定
│   │   │   ├── AttachmentMeta.ets      附件解析与格式化（纯函数）
│   │   │   ├── AttachmentImage.ets     附件字节获取与解码（沙箱/远端）
│   │   │   ├── DeviceBridge.ets        设备桥客户端（socket 生命周期与命令分发）
│   │   │   ├── BridgeProtocol.ets      设备桥协议消息与帧构造
│   │   │   ├── BridgeRouter.ets        桥请求路由
│   │   │   ├── BridgeCaps.ets          能力声明
│   │   │   ├── DeviceBridgeSession.ets 前台桥生命周期、网关地址推导
│   │   │   ├── DeviceModel.ets         设备页纯逻辑（device_id 兜底、在线设备解析）
│   │   │   ├── PluginApi.ets           插件列表/详情接口
│   │   │   ├── PluginStatus.ets        插件状态判定与配色
│   │   │   ├── SettingsModel.ets       设置载荷解析、分类归并、分页、路由 id
│   │   │   └── MarkdownParser.ets      Markdown 解析（块/行内/表格）
│   │   ├── components/           可复用组件
│   │   │   ├── MarkdownView.ets        流式 Markdown（增量渲染）
│   │   │   ├── StaticMarkdown.ets      静态 Markdown（历史消息，一次成型）
│   │   │   ├── Attachment.ets          附件卡 + 附件详情内容
│   │   │   ├── ChatStream.ets          消息列表 + 顶栏遮罩 + 底部淡出 + 触顶懒加载
│   │   │   ├── ChatBubble.ets          单条气泡（头像/渠道名/思考卡/工具卡/附件/正文）
│   │   │   ├── ChatToolCard.ets        思考过程卡 + 工具调用卡
│   │   │   ├── ChatComposer.ets        悬浮输入区（选图/选文件/上传/发送）
│   │   │   ├── ChatAttachBar.ets       加号菜单 + 待发送附件条
│   │   │   ├── SettingsHome.ets / SettingsRootEntries.ets / SettingsEntryCard.ets  设置一级页
│   │   │   ├── ConnectionsPane.ets / AppearancePane.ets / BackendSettingsPane.ets  设置二级页
│   │   │   ├── PluginListView.ets / PluginDetailPane.ets / PluginsOverlays.ets     插件页
│   │   │   ├── DeviceRootEntries.ets / DevicePanes.ets                            设备页
│   │   │   ├── SettingsEditor.ets      配置编辑器
│   │   │   ├── StatusCards.ets         状态卡片
│   │   │   ├── ToastBar.ets            统一提示条（插件页与设置页共用）
│   │   │   ├── PageTopBar.ets          顶栏 + 悬浮按钮
│   │   │   ├── SubPage.ets             二级页容器 / NavGroup / NavRow / PlainCard
│   │   │   ├── MotionBase.ets          统一按压反馈与入场动画
│   │   │   └── GradientBackground.ets
│   │   ├── model/Model.ets       共享类型定义
│   │   ├── pages/                页面（薄壳：导航 + 数据编排）
│   │   │   ├── Index.ets         Tab 容器（入口）
│   │   │   ├── ChatPage.ets      对话
│   │   │   ├── DevicePage.ets    设备
│   │   │   ├── PluginsPage.ets   插件
│   │   │   └── SettingsPage.ets  设置
│   │   └── entryability/EntryAbility.ets
│   ├── module.json5              权限、能力声明
│   └── resources/                字符串、颜色、图标、页面路由表
├── build-profile.json5.example   构建/签名配置模板（复制后填本机签名材料）
└── oh-package.json5              依赖
```

约定：**单个 `.ets` 不超过 400 行**，页面只做页面壳（导航栈 + 数据编排），
可复用结构进 `components/`，无 UI 的逻辑进 `common/`。

这条约定有一个边界，别用反了：

> **不到 400 行的文件不要为了拆分而拆分。** `@Component` 的 `build()` 只允许一个根节点，
> 把原来多节点的 `@Builder` 改成组件时会多出一层 `Column` 包裹 —— 布局等价是**推理**出来的、
> 不是看出来的，每拆一次都要付一次"未上机验证"的账。所以拆分只用来解决真实的可读性/维护性
> 问题（超长文件、职责混杂），而不是凑行数。`pages/Index.ets` 目前 396 行就属于"不动"的一类：
> 没越线，余量本身也是有用的缓冲；等它真越线了再拆，并且优先看是不是又长出了大 `@Builder`。

## 编译

需要 DevEco Studio 或 [command-line-tools](https://developer.huawei.com/consumer/cn/deveco-studio/)。
本工程用 `compatibleSdkVersion 6.1.1(24)` / `compileSdkVersion 26.0.0`。

0. **前置条件：`oh_modules/` 必须存在**（`ohpm install` 的产物）。

   它被 `.gitignore` 忽略，所以干净 clone 后没有；而 hvigor **不会**自动补齐它 ——
   实测把 `oh_modules/` 移走后构建不会触发 `ohpm install`，而是直接报一堆
   `arkts-no-untyped-obj-literals`（依赖类型声明缺失），且不会重建该目录。
   所以：clone 后先 `ohpm install`，之后别把这个目录当垃圾清掉。
   `entry/build/`、`.hvigor/` 是纯构建产物，可以随时删除（冷构建 ~8s）。

1. **准备签名配置**（`build-profile.json5` 含密码明文，未入库）：

   ```bash
   cd cmd/ohos/HologramAgent
   cp build-profile.json5.example build-profile.json5
   ```

   把 `REPLACE_WITH_YOUR_*` 换成本机 DevEco 生成的调试签名材料，
   默认在 `~/.ohos/config/` 下（`.cer` / `.p7b` / `.p12` 三件套 + 两个密码）。
   用 DevEco Studio 打开工程会自动生成，命令行可参考 `deveco-cli` 生成签名材料。

2. **构建 HAP**：

   ```bash
   cd cmd/ohos/HologramAgent
   # ⚠️ 不要用仓库里的 ./hvigorw：它是符号链接，启动脚本按 $(dirname $0) 定位，
   #    会报 File not found: <repo>/cmd/ohos/hvigor/bin/hvigorw。
   #    一律用 command-line-tools 里的绝对路径（本机为 /opt/huawei/command-line-tools/bin/hvigorw）：
   /opt/huawei/command-line-tools/bin/hvigorw \
     assembleHap --mode module -p product=default --no-daemon
   ```

   产物在 `entry/build/default/outputs/default/entry-default-signed.hap`。

3. **安装到设备**（`entry/build/` 是纯构建产物、不入库，所以**必须先跑完第 2 步**，
   否则下面这个路径不存在）：

   ```bash
   hdc install entry/build/default/outputs/default/entry-default-signed.hap
   ```

   路径里的目录名随构建模式而变：默认是 `default/`，若用 `-p product=<名字>` 则是该产品名。
   拿不准就先 `find entry/build -name '*.hap'` 找一下。同目录还有 `entry-default-unsigned.hap`，
   `hdc install` 要用带 `-signed` 的那个。

## 连接 homed

首次启动在「设置」里填：

- **服务地址**：`http://<homed 主机>:8080`（WebUI 插件监听端口）
- **API Key**：homed 的 `plugin.webui.api_key`

客户端所有请求走 `<服务地址>/api/v1/*`，带 `X-API-Key` 头。
附件路径 `/files/` `/uploads/` 不带 `/api/v1` 前缀，同样携带鉴权头。

设备桥需要 homed 启用 `remotedevice` 插件（默认 9890），
在「设备」页填 ws token 后本机能力即可被 agent 调用。

## 注意事项

- **聊天历史分页**：首屏只拉最新 `CHAT_PAGE_SIZE`（40）条，向上滚动触顶自动加载更早的。
  服务端 `/chat/history` 支持 `limit` / `before` 游标；工具调用详情与思考内容完整下发不裁剪。
- **修改主题色**：改 `common/Constants.ets` 的 `DARK_PALETTE` / `LIGHT_PALETTE`，全局生效。
- **新增页面**：同时在 `resources/base/profile/main_pages.json` 注册，且只有入口页带 `@Entry`。
- 项目代码部分由 AI 辅助生成，改动请自行评估。

## 改动后的运行时验证清单

构建通过只能证明编译期没问题；ArkUI 的状态绑定、过渡动画与手势行为
必须上设备/模拟器点一遍。UI 相关改动（尤其拆分、状态搬家）请至少走完：

- [ ] 发一条消息，确认流式输出、滚动到底、"AI 思考中/工具调用"状态条正常
- [ ] 点开思考过程卡与工具调用卡，确认能展开/收起且有过渡动画
- [ ] 传一张图片与一个文件，确认预览条、上传进度、发送后附件卡正常
- [ ] 进设置的四个二级页（状态/连接/外观/后端），确认进出场与保存生效
- [ ] 进插件列表与插件详情，确认状态色、开关与卸载正常
- [ ] 进出设备页四个二级页，确认授权开关与在线设备列表正常
- [ ] 宽屏（>=600vp）下确认左右分栏、返回手势与返回键行为

模拟器在无图形/无提权环境里可能起不来（需要写 `~/.Huawei` 等宿主目录），
此时请在真机或有权限的机器上补这轮验证，并在提交信息里注明"未做运行时验证"。
