package agentcli

import (
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/plugin"
)

// 编译期断言：agentcli 必须实现 plugin.SkillProvider，
// 否则内核的 InstallBuiltinSkill 会因类型断言失败而**静默跳过** ——
// 模型永远看不到这份文档，而工具照样存在。
//
// 这个断言本身就是判据：删掉 BuiltinSkills 方法 ⇒ 编译失败。
var _ plugin.SkillProvider = (*Plugin)(nil)

func TestSkillProviderContract(t *testing.T) {
	p := New("agentcli")
	skills := p.BuiltinSkills()
	if len(skills) == 0 {
		t.Fatal("BuiltinSkills 返回空 —— 模型永远看不到 agentcli 的使用说明")
	}
	body, ok := skills[builtinSkillName]
	if !ok {
		t.Fatalf("缺 %q 条目", builtinSkillName)
	}
	if len(body) < 500 {
		t.Fatalf("SKILL 正文过短（%d 字节），不足以讲清终端与一次性命令的边界", len(body))
	}
	// frontmatter 的 name 必须与 map 的键一致（内核按它建目录）。
	if !strings.Contains(body, "name: "+builtinSkillName) {
		t.Fatalf("frontmatter 的 name 必须等于 %q（否则 directory 名与索引对不上）", builtinSkillName)
	}
}

// TestSkillSeparatesTerminalFromOneShot 是本文件最重要的判据。
//
// ## 它守的是什么（真实生产事故）
//
// 用户报告：agent 把 terminal_create 当成一次性命令执行器用。日志实录：
//
//	terminal_create(command='cd /usr/lib/node_modules/@deepseek-ai && echo "..." && ls -d */ | head -60 && …')
//	→ read /dev/ptmx: input/output error
//	→ 再开 term_7、term_8、term_9 …
//
// 那份文档必须同时说清三件事，缺任何一条都会让模型退回旧行为：
//
//	① 一次性命令该用 cmd_run（并说明 cmd 插件要先 plugin_start）
//	② 终端的真正用途是"要持续输入的程序"
//	③ 复合命令也属于"一次性"，不是"所以要用终端"
//
// **特别是第 ③ 条**：`command` 这个参数名天然诱导模型往里面塞
// `cd && echo && grep`。文档必须正面抵消这个引力。
func TestSkillSeparatesTerminalFromOneShot(t *testing.T) {
	body := New("agentcli").BuiltinSkills()[builtinSkillName]

	musts := []struct {
		substr string
		why    string
	}{
		{"cmd_run", "必须点名一次性命令该用 cmd_run，否则模型只知道有终端"},
		{"plugin_start", "必须说明 cmd 插件是按需加载（auto_start=false），否则模型找不到 cmd_run 就用终端"},
		{"vim", "必须给出终端的正面用例（vim/ssh/top 这类要持续输入的程序）"},
		{"&&", "必须点明复合命令（cd && echo && ls）属于一次性，不是终端的理由"},
		{"跑完就结束", "必须给出可操作的判据，而不是只说「交互式/非交互式」这种抽象词"},
		{"/dev/ptmx", "必须解释那个真实报错的含义（命令跑完终端已退出），否则模型会以为终端坏了"},
	}
	for _, m := range musts {
		if !strings.Contains(body, m.substr) {
			t.Fatalf("SKILL 缺少 %q ——\n%s", m.substr, m.why)
		}
	}
}

// TestSkillIndexLineStatesBoundary 钉住索引行（模型每轮唯一能看到的一行）。
//
// 索引行是 `[Skills]` 段里的那条摘要。如果它只写"交互式终端"，
// 模型无从知道"一次性命令别用我"——而那正是要拦的行为。
func TestSkillIndexLineStatesBoundary(t *testing.T) {
	body := New("agentcli").BuiltinSkills()[builtinSkillName]

	// 取 frontmatter 的 description（这是内核 extractDescription 的
	// 主要来源之一；另一条路是正文首段，见下一个判据）。
	var desc string
	lines := strings.Split(body, "\n")
	inFM := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if i == 0 && t == "---" {
			inFM = true
			continue
		}
		if inFM {
			if t == "---" {
				break
			}
			if strings.HasPrefix(t, "description:") {
				desc = strings.TrimSpace(strings.TrimPrefix(t, "description:"))
			}
		}
	}
	if desc == "" {
		t.Fatal("frontmatter 缺 description —— 索引行会退化成正文首段")
	}
	if !strings.Contains(desc, "cmd_run") {
		t.Fatalf("索引行必须指出替代方案（cmd_run），实际：%q\n"+
			"只说「交互式终端」不够：模型看到那一行仍不知道一次性命令该用什么。", desc)
	}
	if !strings.Contains(desc, "一次性") {
		t.Fatalf("索引行必须点明边界（一次性命令别用终端），实际：%q", desc)
	}
}

// TestSkillBodyFirstParagraphStandalone 钉住正文首段能独立看懂。
//
// 内核的 extractDescription **跳过 frontmatter**，取正文第一个非空
// 非标题段落 —— 所以首段才是真正注入系统提示词的那一行。
func TestSkillBodyFirstParagraphStandalone(t *testing.T) {
	body := New("agentcli").BuiltinSkills()[builtinSkillName]

	lines := strings.Split(body, "\n")
	inFM, started := false, false
	first := ""
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if i == 0 && t == "---" {
			inFM = true
			continue
		}
		if inFM {
			if t == "---" {
				inFM, started = false, true
			}
			continue
		}
		if started && t != "" && !strings.HasPrefix(t, "#") {
			first = t
			break
		}
	}
	if first == "" {
		t.Fatal("正文首段为空 —— 索引行会缺失")
	}
	// 首段必须自带边界，不能只靠标题或后文。
	if !strings.Contains(first, "cmd_run") {
		t.Fatalf("正文首段必须提到 cmd_run（它才是注入提示词的那段），实际：%q", first)
	}
}
