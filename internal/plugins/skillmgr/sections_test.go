package skillmgr

// 判据：SKILL.md 章节切分与按需读取。
//
// ## 守的是什么
//
// skill_info 此前**整份返回** SKILL.md。生产实测最坏一例：
//
//	commondev-discipline v2.3.0 → 983 行 / 53810 字节 / ≈10600 token
//
// 模型为查「怎么开 PR」（§14，21 行）要把 hmap 插件开发纪律（320 行）
// 和红牌一起读进上下文。一次误读一万 token，且挤掉了真正要用的历史。
//
// ## 这一组判据最容易写废的地方
//
// ① 用一份**人造的短文档**测切分 —— 那种文档里没有 shell 注释、
//    没有嵌套层级、没有围栏代码块，测过了也不代表能切真实文件。
//    所以 TestSectionsRealDocument 用的就是生产那份文件。
//
// ② 只断言"切出了 N 个章节" —— 数量对不代表边界对。所以主判据是
//    **所有区间拼接 == 原文**，它同时能抓漏行、重行、错位。
//
// ③ 不测退化路径 —— 没有标题的文档、只有 frontmatter 的文档，
//    在真实技能里都存在，必须各自钉住。

import (
	"os"
	"strings"
	"testing"
)

// realCommondev 返回生产上那份真实的巨无霸 SKILL.md。
//
// 路径不存在时 skip（这条判据依赖本地样例文件；在干净检出 / CI
// 上它就该跳过，而不是把"文件不在"误报成"切分坏了"）。
func realCommondev(t *testing.T) string {
	t.Helper()
	for _, p := range []string{
		"/home/program/plugbundle/plugins/commondev/skill/commondev-discipline/SKILL.md",
		"/home/hologramagent/plugins/commondev/skill/commondev-discipline/SKILL.md",
	} {
		if b, err := readFileString(p); err == nil && len(b) > 0 {
			return b
		}
	}
	t.Skip("本地没有 commondev-discipline 样例文件")
	return ""
}

// TestSectionsRebuildEqualsOriginal 是**主判据**：
// 所有章节区间按序拼接必须逐字节等于原文。
//
// 它能同时抓三类缺陷：漏行（少了内容）、重行（重复内容）、错位（边界偏）。
// 变异：把 end 的取值从 heads[i+1].line 改成 heads[i+1].line-1 ⇒ 每个
// 章节末尾丢一行 ⇒ 红。
func TestSectionsRebuildEqualsOriginal(t *testing.T) {
	for name, content := range map[string]string{
		"real-commondev": realCommondev(t),
		"nested":         "# Top\n\ntext\n\n## Sub A\n\nbody A\n\n### Deep\n\ndeep body\n\n## Sub B\n\nbody B\n",
		"no-heading":     "just text\nno heading at all\n",
		"only-fm":        "---\nname: x\n---\n",
		"empty":          "",
	} {
		if content == "" {
			continue
		}
		lines := strings.Split(content, "\n")
		secs := parseSections(content)
		var rebuilt []string
		for _, s := range secs {
			if s.Start < 0 || s.End > len(lines) || s.Start > s.End {
				t.Fatalf("[%s] 非法区间 [%d,%d)，共 %d 行", name, s.Start, s.End, len(lines))
			}
			rebuilt = append(rebuilt, lines[s.Start:s.End]...)
		}
		if got := strings.Join(rebuilt, "\n"); got != content {
			t.Fatalf("[%s] ★ 区间拼接与原文不一致（原文 %d 行，拼回 %d 行）——\n"+
				"切分漏行 / 重行 / 错位。\n前 200 字符差异：由测试下方打印",
				name, len(lines), len(rebuilt))
		}
	}
}

// TestSectionsShellCommentIsNotHeading 钉住围栏代码块的感知。
//
// 真实 SKILL.md 的 bash 示例里有 `# 期望 200；401/403 = 凭据无效`。
// 那不是标题。硬按 # 切会把一条 curl 示例拆成碎片 —— 模型拿到的是
// 无法执行的半截命令，比不给还糟。
//
// 变异：删掉 parseSections 里的 fence 判断 ⇒ 本例红。
func TestSectionsShellCommentIsNotHeading(t *testing.T) {
	content := strings.Join([]string{
		"# Real Title",
		"",
		"```bash",
		"# 期望 200；401/403 = 凭据无效",
		"curl -s https://example/api",
		"```",
		"",
		"## Next",
		"",
		"body",
	}, "\n")

	secs := parseSections(content)
	for _, s := range secs {
		if strings.Contains(s.Title, "期望") {
			t.Fatalf("★ 代码块内的 shell 注释被当成标题: %q（行 %d）", s.Title, s.Start+1)
		}
	}
	var titles []string
	for _, s := range secs {
		if s.Title != "" {
			titles = append(titles, s.Title)
		}
	}
	if len(titles) != 2 || titles[0] != "Real Title" || titles[1] != "Next" {
		t.Fatalf("★ 章节标题错误: %v（期望 [Real Title, Next]）", titles)
	}
}

// TestResolveSectionReturnsWholeSubtree 钉住"点名顶层章节要给整块"。
//
// 模型问 `## Setup` 时，它要的是 §11–§16 全部（Setup 这一部分），
// 不是只有标题那一行。
//
// 变异：把 `<= root.Level` 改成 `< root.Level` ⇒ 遇到第一个同级标题
// 不停止 ⇒ 返回整份文档 ⇒ 本例红。
func TestResolveSectionReturnsWholeSubtree(t *testing.T) {
	content := strings.Join([]string{
		"# Doc",
		"",
		"## Setup",
		"",
		"### 11. alpha",
		"",
		"### 12. beta",
		"",
		"## Usage",
		"",
		"### 21. gamma",
	}, "\n")
	secs := parseSections(content)

	start, end, idx, ok := resolveSection(content, secs, "Setup")
	if !ok {
		t.Fatal("Setup 未匹配")
	}
	body := strings.Join(strings.Split(content, "\n")[start:end], "\n")
	if !strings.Contains(body, "11. alpha") || !strings.Contains(body, "12. beta") {
		t.Fatalf("★ Setup 没返回它的子节（§11/§12）：\n%s", body)
	}
	if strings.Contains(body, "21. gamma") {
		t.Fatalf("★ Setup 越界吃进了 Usage 的内容：\n%s", body)
	}
	if secs[idx].Title != "Setup" {
		t.Fatalf("命中了错误的章节 %q", secs[idx].Title)
	}

	// 点名子节时只返回该子节。
	start, end, _, ok = resolveSection(content, secs, "12. beta")
	if !ok {
		t.Fatal("12. beta 未匹配")
	}
	body = strings.Join(strings.Split(content, "\n")[start:end], "\n")
	if !strings.Contains(body, "12. beta") || strings.Contains(body, "11. alpha") {
		t.Fatalf("★ 点子节却返回了别的子节：\n%s", body)
	}
}

// TestResolveSectionToleratesHashPrefix 钉住容错：
// 模型常把整行标题（含 ##）当参数传进来。
func TestResolveSectionToleratesHashPrefix(t *testing.T) {
	content := "## Setup: git 操作\n\nbody\n\n## Usage\n\nother\n"
	secs := parseSections(content)
	for _, q := range []string{"Setup", "## Setup", "## Setup: git 操作", "setup"} {
		if _, _, _, ok := resolveSection(content, secs, q); !ok {
			t.Fatalf("★ 查询 %q 未匹配（应容错）", q)
		}
	}
}

// TestRenderTOCListsLevels 钉住目录只列到指定层级 + 带行数。
//
// 行数不是装饰：模型据此判断"读这一节值不值"，这是它在
// "一次吞一万 token" 和 "先读目录" 之间做选择的唯一依据。
func TestRenderTOCListsLevels(t *testing.T) {
	content := "## A\n\nx\n\n### A1\n\ny\n\n#### deep\n\nz\n\n## B\n\nw\n"
	toc := renderTOC(parseSections(content), 2)
	if !strings.Contains(toc, "A") || !strings.Contains(toc, "B") {
		t.Fatalf("★ 目录漏了顶层章节:\n%s", toc)
	}
	if strings.Contains(toc, "deep") {
		t.Fatalf("★ 目录列出了超过 maxLevel 的章节:\n%s", toc)
	}
	if !strings.Contains(toc, "行)") {
		t.Fatalf("★ 目录没有标行数（模型无法判断读取成本）:\n%s", toc)
	}
}

// readFileString 读文件为字符串（避免在测试里到处 os.ReadFile）。
func readFileString(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}
