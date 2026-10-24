---
name: knowledge-base
description: 按分类树检索 HologramAgent 知识库。当需要查阅项目知识、架构约定、历史决策，或用户提到"知识库/knowledge/知识库有哪些/这个项目的约定是什么"时使用。支持树形导航、分类内检索、以图搜知识。
version: 1.1.0
author: HologramAgent
---

## Usage

先看树定位分类，再做定向检索。接口由 HologramAgent 的 `kbtree` 插件提供，
只读、需 token。详见下方"接入"。

HologramAgent 知识库按**分类树**组织（`tech/go/并发`、`life/sleep` …）。

### 何时用

- 用户问"这个项目/内核的某个约定是什么" → 先看树，找对分类再检索
- 用户提到"知识库"或某个看起来像分类名的词（如 `tech/go`）→ 查该子树
- 用户给了图片并问"知识库里有相关的吗" → 见"以图搜知识"

### 接入

服务默认监听 `127.0.0.1:9892`（配置项 `kbtree.listen_addr`），只读，
每次请求需带 token（配置项 `kbtree.token`；未配置则启动时随机生成）。

```bash
BASE=http://127.0.0.1:9892
AUTH="X-API-Key: $KB_TOKEN"     # 或 Authorization: Bearer <token> 或 ?token=
```

先摸清可用接口：

```bash
curl -s -H "$AUTH" "$BASE/"
```

### 本机封装脚本（推荐先用它）

`scripts/kb_tree.sh` 把上面四条命令封好了，省得手拼 URL 与鉴权头：

```bash
S=~/.claude/skills/knowledge-base/scripts/kb_tree.sh   # 按实际安装路径调整

$S                       # 整棵树
$S tree -c tech -d 1     # tech 子树，只看第一层
$S categories            # 分类列表
$S counts                # 各分类条目数
$S search -q goroutine -c tech
```

token 读取顺序：环境变量 `KB_TOKEN` → 同上��目录的 `../config.json`。
**不要**把 token 写在命令行上（会进 shell 历史与 `ps` 输出）。

### 核心工作流：先看树，再定向检索

**别一上来就全文搜索。** 知识库是分层的，先定位分类能显著提高命中率，
也能避免把范围外的弱匹配当答案。

#### 第 1 步 · 看有哪些分类

```bash
curl -s -H "$AUTH" "$BASE/categories"
# {"categories":["cook","life","tech","tech/go","tech/rust"]}

# 内容最多的分类（按条目数倒序）
curl -s -H "$AUTH" "$BASE/counts"
# {"counts":[{"category":"tech","count":3}, ...], "total":5}
```

#### 第 2 步 · 浏览树结构

```bash
# 整棵树，只要结构
curl -s -H "$AUTH" "$BASE/tree?items=0"

# 只要第一层 —— 分类多时用来做懒加载
curl -s -H "$AUTH" "$BASE/tree?depth=1&items=0"

# 某棵子树，带条目详情
curl -s -H "$AUTH" "$BASE/tree?category=tech/go"
```

节点里 `name` 是**本级段名**（`"go"`），`path` 是**完整路径**
（`"tech/go"`）。拼层级用 `name`，把 `path` 拿去请求子节点。
`item_count` 是本级条目数，`total_count` 是整棵子树。

#### 第 3 步 · 分类内检索

```bash
# 关键词 + 分类子树（推荐）
curl -s -H "$AUTH" "$BASE/search?q=goroutine&category=tech"

# 全库
curl -s -H "$AUTH" "$BASE/search?q=goroutine&limit=5"
```

`category` 是**前缀匹配**：`tech` 会命中 `tech/go`、`tech/rust` 下的条目；
传 `tech/go` 只命中它自己的子树。

### 以图搜知识（多模态）

若知识条目挂了图片，它在**多模态统一空间**里有向量，能被图片本身检索到。
前提是宿主已接入多模态向量 provider（否则只是记录了媒体，不参与召回）。

判断是否就绪：HologramAgent 自身的知识库接口会返回稠密路状态
（`dense.enabled` / `dense.ready` / `dense.total`）。若为未启用，
**不要承诺"能以图搜"**。

本服务只提供**按关键词检索**——把图片字节提交给嵌入服务计算向量不在此接口内。所以：
- 用户给了图 → 用图的**可见内容**（或你先读图得到的文字）当关键词检索
- 或用本机可用的读图工具先看图，再拿描述来检索

### 读结果

`/search` 每条结果：

| 字段 | 含义 |
|---|---|
| `name` | 知识名（**已含分类前缀**，如 `tech/go/并发`） |
| `content` | 正文全文 |

`/tree` 里的条目额外有 `preview`（前 120 字）、`size`、`updated_at`、
`media`（挂载的媒体 digest/mime/kind）。

### 易错点

- **`name` 已经含分类**。不要再拼 `category + "/" + name`，会得到
  `tech/go/tech/go/并发`。
- **检索会返回弱匹配。** 词法路会给所有条目打一个低分，靠排序把强命中顶到
  前面。**只看第一条**；第一条明显不相关就换个分类或关键词，别把第 2、3 条
  当答案。
- **分类不存在返回 404**，并在 `categories` 字段里附上现有分类 —— 用它自查
  拼写。
- **`items=0` 只是不要正文**，`total_count` 仍准确，可用于判断规模。
- **本服务只读**。写方法返回 405。要写知识请用 HologramAgent 主 agent 的
  `knowledge_create`（或 WebUI 界面），不要试图绕过它直接写这个接口。
- 知识名里不能有 `..`、空格（会被规范成 `_`）、点开头的段。

### 写入

本服务不提供写入。若你在 HologramAgent 主 agent 内部，写入用内核工具：

```
knowledge_create  name="tech/go/调度"  content="正文..."
```

`name` 用 `/` 表示分类层级（如 `tech/go/调度`）。写完它**立刻可检索**
（词法 IDF 是增量维护的，不必重启）。
