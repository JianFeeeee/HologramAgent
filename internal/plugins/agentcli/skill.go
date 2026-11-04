package agentcli

// agentcli 的随包 SKILL 文档。
//
// ## 插件这边只做一件事：交出文档正文
//
// 写盘、幂等、事件通知、停用时清理——全部由内核统一处理
// （internal/plugin/builtin_skill.go，挂载在 loadOne / StopPlugin / StopAll）。
//
// 这与 seq 的做法一致，理由见 seq/skill.go 的注释：skill 是插件能力的
// 一部分，生命周期必须与插件同生共死。禁用插件后 skill 还留着，模型会
// 看得到说明、点进去工具却不存在。
//
// ## 为什么 agentcli 必须有 skill（真实生产事故）
//
// 用户报告：agent 把 terminal_create 当成一次性命令执行器用。
//
// 日志实录：
//
//	terminal_create(command='cd /usr/lib/node_modules/@deepseek-ai && echo "=== 已安装的 dsh 包 ===" && ls -d */ | head -60 && …')
//	→ read /dev/ptmx: input/output error
//	→ 再开 term_7、term_8、term_9 …
//
// 成因有三层：
//
//	① cmd 插件 auto_start=false ⇒ 工具表里没有 cmd_run，需先 plugin_start cmd
//	② terminal_create 的 command 参数描述写着"要执行的命令"，语义上像"跑一条命令"
//	③ 没有一份文档说清"一次性命令该用 cmd_run、终端是给要持续输入的程序用的"
//
// ①③ 已分别修掉（cmd-tools skill + 本文件）。② 是设计上的引力：
// 参数名叫 command、描述说"要执行的命令"，模型自然会塞复合命令进去。
// 文档要正面抵消这个引力。
//
// ## description 的取值规则
//
// 内核的 extractDescription **跳过 frontmatter**，取正文第一个非空非标题
// 段落。所以正文首段必须能独立看懂——它才是注入系统提示词的那一行。

const builtinSkillName = "agentcli-terminal"

// BuiltinSkills 实现 plugin.SkillProvider —— 内核据此让 SKILL 随插件启停出入。
func (p *Plugin) BuiltinSkills() map[string]string {
	return map[string]string{
		builtinSkillName: agentcliSkillMarkdown,
	}
}

// agentcliSkillMarkdown 是 SKILL 正文。
//
// frontmatter 的 name 必须与 map 的键（也就是目录名）一致。
const agentcliSkillMarkdown = `---
name: agentcli-terminal
description: 交互式终端（vim/ssh/top/长驻进程）。一次性的 shell 命令不要用终端，用 cmd_run。
version: 1.0.0
---

终端给**要持续输入**的程序用（vim、ssh、top、REPL、要按键确认的安装器）；一次性的 shell 命令用 **cmd_run**（cmd 插件，需先 plugin_start cmd）。
把 ` + "`cd && echo && ls && grep`" + ` 这类复合命令塞进 terminal_create，命令跑完终端就退出，
接下来 terminal_read 只会撞 ` + "`read /dev/ptmx: input/output error`" + `。

## 一句话判据

**跑完就结束 → cmd_run。要一直活着、要看屏幕、要按键 → terminal。**

| 你要做的事 | 用哪个 |
|---|---|
| ` + "`ls`" + ` / ` + "`git status`" + ` / ` + "`go build`" + ` / 一次性脚本 | **cmd_run** |
| ` + "`cd a && echo x && ls`" + ` 这种复合命令 | **cmd_run**（它就是 bash，管道/&&/重定向全支持） |
| 装包时要按 y 确认 | terminal |
| ` + "`vim`" + ` / ` + "`nano`" + ` / ` + "`top`" + ` / ` + "`htop`" + ` | terminal |
| ` + "`ssh`" + ` 进另一台机器再操作 | terminal |
| ` + "`python`" + ` / ` + "`node`" + ` 开 REPL 逐条试 | terminal |
| 长驻服务（` + "`npm run dev`" + `）要看持续输出 | terminal（配 notify） |

判据的关键不是"命令长不长"，而是**命令结束后你还需不需要这个会话**。
不需要就是一次性，用 cmd_run。

## cmd_run 不存在时

` + "`cmd`" + ` 插件默认不随内核启动（` + "`auto_start: false`" + `），所以工具表里
可能没有 ` + "`cmd_run`" + `。这是**按需加载**，不是没有这个能力：

` + "```" + `
plugin_start  name=cmd
` + "```" + `

之后 ` + "`cmd_run`" + ` 才出现在工具表里。用完可以 ` + "`plugin_stop name=cmd`" + `。

**不要因为找不到 cmd_run 就退而用 terminal_create 跑一次性命令** —— 那正是
本 skill 要拦住的行为。

## 终端的基本用法

| 工具 | 用途 |
|---|---|
| ` + "`terminal_create`" + ` | 建会话，返回 ` + "`terminal_id`" + `。` + "`command`" + ` 是**要跑的程序**（如 ` + "`vim /tmp/a.txt`" + `），不是一串命令 |
| ` + "`terminal_write`" + ` | 送输入。普通文本用 ` + "`input`" + `；特殊键用 ` + "`key`" + `（enter/tab/escape/ctrl_c/up/down/f1…） |
| ` + "`terminal_read`" + ` | 读输出。` + "`mode=new`" + `（默认）只给"上次读之后的新内容"；` + "`mode=now`" + ` 给整屏当前画面 |
| ` + "`terminal_watch`" + ` | 设提醒规则，避免长任务的输出造成通知风暴 |
| ` + "`terminal_close`" + ` | 关会话，释放资源 |

## 两个必须知道的坑

**① 先读再写。** 开完终端先 ` + "`terminal_read`" + ` 看屏幕当前是什么，
再决定发什么。盲写经常打在还没准备好的界面上（比如 vim 还在启动、
或者 shell 提示符还没出来）。

**② 终端默认 5 分钟自动关闭。** 长任务要显式给 ` + "`timeout`" + `
（如 ` + "`30m`" + `）。默认**不主动推送**，输出靠主动 ` + "`terminal_read`" + `；
要推送就显式声明 ` + "`notify`" + `（` + "`exit`" + ` / ` + "`interval=30s`" + ` / ` + "`buffer=8192`" + `）。

## 退出交互程序

vim / less 之类先发 ` + "`key=escape`" + ` 回到命令模式，再发 ` + "`input=\":q!\"`" + ` 加
` + "`key=enter`" + `。ctrl_c 对 ` + "`top`" + ` 有效，对 vim 要先 escape。

## 常见误用（都是真的发生过）

- ❌ 用 terminal_create 跑 ` + "`ls -la`" + ` —— 一条 cmd_run 就够了
- ❌ 每条命令开一个新终端，然后不复用 —— 终端是**会话**，不是命令管道
- ❌ 拿 terminal_read 的报错 ` + "`read /dev/ptmx: input/output error`" + ` 当成"终端不能用"
  —— 那通常是**命令已经跑完、终端已退出**，说明一开始就该用 cmd_run
- ❌ 用 terminal 做文件读写 —— 用 files 插件的 ` + "`files_read`" + `/` + "`files_write`" + `
`
