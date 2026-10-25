package seq

// seq 的随包 SKILL 文档。
//
// ## 插件这边只做一件事：交出文档正文
//
// 写盘、幂等、事件通知、停用时清理——全部由内核统一处理
// （internal/plugin/builtin_skill.go，挂载在 loadOne / StopPlugin / StopAll）。
//
// 第一版我让插件自己写盘 + 发事件，那是错的：
//   · 每个内置插件都要重复实现一遍「释放/清理/幂等」
//   · 更要命的是**禁用插件后 skill 还留着** —— 模型看得到 skill，
//     点进去工具却不存在，只会拿到一连串"工具不存在"且无从知道原因
//
// 正确语义：skill 是插件能力的一部分，生命周期必须与插件同生共死。
//
// ## 为什么要有 skill（而不只靠 ToolDef.Description）
//
// skill 会被 skillmgr 汇总成索引注入系统提示词（buildSystemPrompt 的 skillIndex 段）：
// 模型**每轮**都能看到"有这么个能力"的一行摘要。只写在 ToolDef.Description 里，
// 模型不主动翻工具列表就看不到——这正是此前 seq 几乎不被使用的原因
// （与 doc_query 的历史教训同构：内核从没解释过 L1/L2/L3，agent 就只能靠猜）。

const builtinSkillName = "seq-tools"

// BuiltinSkills 实现 plugin.SkillProvider —— 内核据此让 SKILL 随插件启停出入。
func (p *Plugin) BuiltinSkills() map[string]string {
	return map[string]string{
		builtinSkillName: seqSkillMarkdown,
	}
}

// seqSkillMarkdown 是 SKILL 正文。
//
// frontmatter 的 name 必须与 map 的键（也就是目录名）一致。
//
// ⚠️ 注意 description 的取值：内核的 extractDescription **跳过 frontmatter**，
// 取正文第一个非空非标题段落。所以正文首段必须能独立看懂——
// 它才是注入系统提示词的那一行。
const seqSkillMarkdown = `---
name: seq-tools
description: 把反复出现的同类工具操作编成序列存起来，以后一次执行。
---

把反复出现的同类工具操作编成「序列」存起来，以后一次执行：组内并行、组间串行，可带条件判断与变量槽引用。

## 什么时候用

**看到下面任一情形，先想一下要不要用序列：**

- 你要连续做几件**顺序已知**的事（例：拉日志 → 过滤 → 汇总 → 发通知）。
- 同样一套操作要在**多个对象**上重复（例：对 5 个仓库各跑一次同样的三步检查）。
- 某套流程**以前做过**，这次还要再做一遍 —— 先用 ` + "`seq_list`" + ` 看有没有现成的，直接 ` + "`seq_run`" + `。
- 一轮里要调的工具很多、参数很长 —— 编成序列可以写成文件（` + "`seq_create`" + ` 的 file 参数），避免参数被长度上限截断。

**不适合**：只有一两个工具、或后一步依赖前一步的**实际返回值**（那种要分多轮）。

## 核心概念

一条序列由若干 **group（组）** 组成：

- **组内并行** —— 同一个 group 的 tools 同时执行
- **组间串行** —— 按 groups 数组顺序，一组跑完再下一组
- 每组有自己的签名：` + "`in`" + `（入参）/ ` + "`out`" + `（出参槽名）
- 组可以被 ` + "`seq_call`" + ` 按名单独调用

理解这两条决定了你怎么分组：**能并行的放同一组，要前一步结果的放后一组**。

## 工具

| 工具 | 用途 |
|---|---|
| ` + "`seq_help`" + ` | **写序列前先查这个** —— tools 字段的分隔符、in/out 写法、as 与 out 的关系都在这里 |
| ` + "`seq_create`" + ` | 创建/更新序列。groups 与 file 二选一 |
| ` + "`seq_list`" + ` | 列出全部序列及各组签名 |
| ` + "`seq_run`" + ` | 执行整条序列（按组顺序，每组先判 when） |
| ` + "`seq_call`" + ` | 按名调用单个 group。target 写组名，或 #序列名 跨序列调用 |
| ` + "`seq_when_call`" + ` | 条件按名调用：when 为真才执行，否则整次跳过（不产出槽） |
| ` + "`seq_delete`" + ` | 删除序列 |

## 正确用法

1. **第一次编序列前先 ` + "`seq_help`" + `** —— 不要凭猜写 tools 字段，它的格式有坑
   （每个 tool 后都要有 ` + "`;`" + `，含最后一个；in 必须是对象，无入参写 ` + "`{}`" + `）。
2. 用 ` + "`seq_list`" + ` 看有没有现成的复用，别重复造。
3. 长序列用 ` + "`file`" + ` 参数从文件加载，比在参数里写一大坨 JSON 稳。
4. 执行后看返回的**变量槽快照**，后续步骤用 ` + "`$槽名`" + ` 引用。

## 常见错误

- 把「后一步要用前一步返回值」的流程硬塞进一个 group —— 组内并行，拿不到彼此的槽。
  跨组可以（组间串行），组内不行。
- 不查 ` + "`seq_help`" + ` 就写 tools，分隔符或 ` + "`as`" + `/` + "`out`" + ` 对不上，静态校验会直接报错。
`
