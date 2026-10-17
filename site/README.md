# 官网（静态站）

单文件、零构建、零依赖的落地页。与仓库 WebUI 的做法一致（原生 HTML/CSS/JS，无打包步骤）。

## 文件

```
site/
├── index.html          单文件站点（内联 CSS/JS）
└── assets/
    └── logo.svg        logo（来自 assets/branding/logo-h-he.svg）
```

资源是**复制**而非引用上级目录，因此 `site/` 可整体拷走独立部署。

## 本地预览

```bash
cd site && python3 -m http.server 8799
# 打开 http://127.0.0.1:8799/
```

## 部署

纯静态，任意 web 服务器指向 `site/` 即可：

```nginx
server {
    listen 80;
    server_name your.domain;
    root /path/to/TrueAgent/site;
    index index.html;
    location / { try_files $uri $uri/ =404; }
}
```

## 设计说明

- **配色取自品牌指南**：蓝 `#3b82f6` / 青 `#06b6d4` / 金 `#f59e0b`，
  正好对应三层记忆 Context / Document / Graph，因此三层记忆那节直接用了这三色做色条。
- **不放置立绘**。曾试过放看板娘，但原图是白底 + 蓝紫渐变外框的方形图，
  与页面几乎无法自然衔接（详见下文「为什么最终没有图」）。
- **淡入动画是渐进增强**：内容默认可见，仅当 JS 真的跑起来才加 `.js-reveal` 接管。
  早期版本把 `opacity:0` 写在默认样式里，结果 JS 未执行时整页永久空白
  （实测 30 个元素里 26 个停在不可见）——现在禁用 JS 也完整可读。
- 尊重 `prefers-reduced-motion`；架构图的 `<pre>` 在窄屏横向滚动，避免整页撑宽。

## 深浅双主题

跟随系统偏好；导航栏右上角的按钮可手动切换，选择存入 `localStorage`（键 `ha-theme`）。

实现要点：

- 在 `<head>` 里**首绘前**就定好 `data-theme`，避免先渲染深色再跳浅色的闪白。
- 语义色全部走 CSS 变量（`--bg` / `--text` / `--border` / `--code-bg` / `--hover` …），
  `:root[data-theme="light"]` 只覆盖取值，不重复选择器。
- 品牌三色（蓝/青/金）两主题共用，因为它对应三层记忆，换主题不该换语义。

## 动效层

| 效果 | 位置 |
|---|---|
| 四团极光漂移 + 细网格 | 背景（`position: fixed`, `z-index: -2`） |
| 粒子网络（近邻连线） | `#fx` canvas，密度按视口面积自适应，上限 72 |
| 三色滚动进度条 | 顶部 |
| 标题渐变流动 | `h1 .grad` |
| 分块上浮入场（错落延迟） | 各 section |
| 卡片聚光 + 3D 微倾 | `.card` / `.stat`（仅精确指针） |
| 色条自上而下「灌注」 | 三层记忆 |
| 数据流光带下行 + 节点脉冲 | 架构图 |
| 数字滚动到位 | 统计卡 |
| 文字下划线展开 | 分节标题 |

**三条硬约束**（都吃过亏）：

1. **内容默认可读**：初始隐藏只在 `.js-fx` 下生效，而 `.js-fx` 只有 JS 真跑起来才加。
   JS 被禁用或报错时，页面是完整可读的 —— 早期版本把 `opacity:0` 写在默认样式里，
   实测 30 个元素里 26 个永久不可见。
2. **尊重 `prefers-reduced-motion`**：此时不加 `.js-fx`、不启粒子、不画进度条，
   所有元素直接可见。
3. **装饰不得产生滚动条**：canvas 按 `documentElement.clientWidth` 定尺寸
   （用 `window.innerWidth` 会含滚动条宽度，实测多出 15px 撑出横向滚动）；
   `body` 再加 `overflow-x: clip` 兜底。

所有 listener 走 `requestAnimationFrame` 节流；标签页隐藏时暂停粒子。

## 看板娘素材：从「没法用」到「能用了」

> 结论先写在这：**素材侧的问题已经解决**（现役 `mascot-xiaozhai.webp` 自带真 alpha，
> 1024×1024）。下面保留当年的失败记录，因为「为什么当时不行」这件事本身
> 仍是有用约束——它解释了为什么不能拿一张白底图硬塞进深色版面。

### 当年为什么不行

试过放看板娘立绘，失败，最后决定不放。

原图（旧版）是**不透明** WebP（无 alpha），白底 + 蓝紫渐变外框。
直接用 = 深色页面上一个刺眼的白色方块；抠图又不可行：

```bash
python3 -c "
from PIL import Image
im = Image.open('assets/branding/mascot-xiaozhai.webp')
print(im.mode)  # 旧版是 RGB —— 没有 alpha 通道
"
```

背景连通域分析显示**白底与角色的白裙子、白围裙同色**，flood-fill 会顺着轮廓
渗进去，让约 42% 的身体变成透明（发丝高光先死）。曾退而求其次做成
「圆角卡面 + 发光边框 + 底部渐隐」，视觉上能接受，但本质上是在方形图上盖装饰，
而非让图融入页面。

### 现在为什么能了

新版素材走的是另一条路：出图时背景就是**棋盘格提示**，再按「背格连通域 +
中性灰判据」抠成真 alpha，落盘为带 `ALPH` chunk 的 WebP。

```bash
python3 -c "
from PIL import Image
im = Image.open('assets/branding/mascot-xiaozhai.webp')
print(im.mode)  # RGBA
"
```

同一件事在代码里也有判据钉住：`internal/plugins/webui/mascot_asset_test.go`
直接读 WebP 容器头，断言 VP8X 的 Alpha 标志位与 `ALPH` chunk 同时存在——
防止日后有人把一张「看起来透明」的棋盘格 JPEG 换进来。

### 但版面仍未放图

素材不再是阻碍，**但这不等于 Hero 就该放图**。现版面是单栏文字，加图会改变
信息密度与首屏节奏，属于设计取舍而非素材问题——所以这一步没有动。
要放的话，素材已经就绪。

## 插件卡片

「开箱可用的插件」一节里，每个徽章都是**按钮**：

- 点一下展开该插件卡（名称 / 版本 / 用途），再点收起
- 可同时展开多个
- 末尾「展开全部 N 个」一次全开

数据来自各插件的 `plugin.json`，不是手写的：

```bash
cd ${HA_DATA}/plugins
for d in */; do [ -f "$d/plugin.json" ] && python3 -c "
import json;d=json.load(open('$d/plugin.json'))
print(d.get('name'), d.get('version'), d.get('description','')[:60])"; done
```

## 改内容前请核对事实

页面上的数字都是**实测值**。**注意「在跑插件 36」是加载总数**
（16 内置 + 20 外部），不是"外部插件数" —— 早期版本写成「36 外部插件」是错的，
已更正。改动前建议重新取一遍：

```bash
# 在跑插件（以日志里实际 loaded 的为准，比数目录可靠）
grep -h '\[plugin\] loaded:' $(ls -t ${HA_DATA}/log/homed_*.log | head -1) \
  | sed 's/.*loaded: //' | sort -u | wc -l

# 工具数（同上，日志里注册后的真实值）
grep -ohE '[0-9]+ tools' $(ls -t ${HA_DATA}/log/homed_*.log | head -1) | tail -1

# Go 代码行（排除第三方与鸿蒙工程）
find . -name '*.go' -not -path './.git/*' -not -path './.go/*' \
  -not -path './third_party/*' -not -path './cmd/ohos/*' | xargs cat | wc -l

# LLM 适配器（注意 server.lua 是网关脚本，不是厂商适配器，故为 9）
ls internal/lua/adapters/*.lua | xargs -n1 basename
```
