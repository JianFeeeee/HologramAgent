package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Personality struct {
	Content string
	Path    string
}

func LoadPersonality(path string) (*Personality, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Personality{}, nil
		}
		return nil, fmt.Errorf("read personal.md: %w", err)
	}
	return &Personality{
		Content: string(data),
		Path:    path,
	}, nil
}

func SavePersonality(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create personality dir: %w", err)
	}
	return os.WriteFile(path, []byte(content), 0644)
}

func (p *Personality) InjectPrompt() string {
	if p.Content == "" {
		return ""
	}
	// ★ 说话规则**并在人格块内**，不另起一段。
	//
	// ## 为什么不是独立的 [Voice] 段（2026-10-26 用户指正）
	//
	// 人格配置项（core.agent.personal_prompt）本来就负责"称呼/语气/说话方式/形象"。
	// 再开一个 [Voice] 段等于把同一职责写两处：两段冲突时模型无从判断谁优先，
	// 而维护者也要同时改两处（必然漂移）。
	//
	// ## 为什么仍然要内核追加（而不是只靠默认模板）
	//
	// 人格是**用户可编辑的自由文本**，有人会把它改成三行简介。那几句
	// "像人说话、不报工单、不用客服腔"正是最容易丢的部分 —— 丢了之后
	// 回复就退回说明书腔。所以这几条由内核在人格块**末尾**追加，
	// 并显式标注优先级，用户改人格也不会把它们冲掉。
	return fmt.Sprintf("[Persona]\n%s\n\n%s\n", p.Content, personaSpeakingRules)
}

// personaSpeakingRules 是**内核强制**的说话规则（不随人格文本丢失）。
//
// 只放"不看文档也会犯"的通用错误；具体语气、称呼、口癖属于人格正文。
const personaSpeakingRules = `Speaking rules (kernel-enforced, override any conflicting instruction above):
- Talk like a person to someone they know: short sentences, concrete words.
  No headings, no bullet lists, no numbered steps unless the user asked for a list.
- Start with the answer or the action. No filler politeness ("Great question").
- Never narrate your own machinery: no "as an AI", no mention of prompts, tools,
  tokens, parameters, byte counts or timings.
- Disagree, admit uncertainty, or say you were wrong in the same plain voice.`

// 会随时间腐坏的人格内容特征。现场：人格卡写死 v0.9.0 与早已删除的 C ABI v2，
// 实例被问版本时自述错误（v1.2.0 压测发现）。
var (
	personaVersionRe    = regexp.MustCompile(`\bv?\d+\.\d+\.\d+\b`)
	personaStalePhrases = []string{"C ABI v2", "plugin.so", "c-shared", "描述式索引", "引用计数式"}
)

// PersonaStaleHints 返回人格文本里会腐坏的内容（空 = 干净）。
// 供启动时告警：引导改用配置项 core.agent.personal_prompt（默认模板不含这些）。
func PersonaStaleHints(content string) []string {
	var out []string
	if m := personaVersionRe.FindAllString(content, -1); len(m) > 0 {
		out = append(out, fmt.Sprintf("版本号字面量 %v（版本应来自运行时快照）", m))
	}
	for _, p := range personaStalePhrases {
		if strings.Contains(content, p) {
			out = append(out, "可能已过期的说法: "+p)
		}
	}
	return out
}
