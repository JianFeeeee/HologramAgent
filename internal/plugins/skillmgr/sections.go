package skillmgr

// SKILL.md 的章节切分（2026-11-04）。
//
// ## 为什么需要
//
// skill_info 此前**整份返回** SKILL.md。生产实测最坏的一例：
//
//	commondev-discipline  v2.3.0  982 行 / 31925 字节 ≈ 10600 token
//
// 模型为了查一次「怎么开 PR」（§14，约 30 行）要把 hmap 插件开发纪律
// （§32–§41，320 行）和红牌（11 行）一起读进上下文 —— 三块互不相关
// 的内容被强绑在一份文档里。一次误读就是一万 token。
//
// 拆 skill（一个插件声明多个）能解决"跨文档绑定"，但解决不了
// "单文档内部按需读"。本文件补的就是后者。
//
// ## 为什么不能按 `#` 硬切
//
// SKILL.md 正文里有大量 **shell 注释**，形如：
//
//	```bash
//	# 期望 200；401/403 = 凭据无效
//	curl -s ...
//	```
//
// 这些 `#` 在代码块内，不是标题。硬切会把一条 curl 示例拆成两半，
// 模型拿到的是一段无法执行的碎片 —— 比不给还糟。
//
// 所以切分必须**感知围栏代码块**（``` 与 ~~~，允许信息串如 ```bash）。
//
// ## 为什么用行号区间而不是复制字符串
//
// 返回的是 [start, end] 行区间，由调用方从原文切片。这样：
//   - 章节内容的唯一事实来源仍是文件本身（不会出现两份漂移）
//   - 目录（TOC）与正文用同一套边界计算，不会对不上

import (
	"fmt"
	"strings"
)

// skillSection 是 SKILL.md 里的一个章节。
type skillSection struct {
	Level int    // 标题的 # 个数（1 = 顶章，2 = ##，3 = ###）
	Title string // 标题行去掉 # 与首尾空白后的文本
	Start int    // 标题行在原文里的行号（0-based，含）
	End   int    // 本章节最后一行之后的行号（0-based，不含）
}

// parseSections 按层级切分 SKILL.md 正文。
//
// 只切**围栏代码块之外**的 ATX 标题（`#` 开头）。frontmatter 里的
// 内容不参与切分（它是 YAML，`#` 是注释，不是标题）。
//
// 返回的区间覆盖整个文档：第一个标题之前的正文归入一个合成章节
// （Title 为空），这样"全文 = 所有章节拼接"成立，调用方不会漏内容。
func parseSections(content string) []skillSection {
	lines := strings.Split(content, "\n")

	// frontmatter 区间：首个 --- 到闭合 --- 之间不参与切分。
	fmEnd := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				fmEnd = i + 1
				break
			}
		}
	}

	// 找出所有真标题的行号。
	type head struct {
		line  int
		level int
		title string
	}
	var heads []head
	fence := "" // 当前所处围栏标记（"```" 或 "~~~"），空 = 不在代码块内
	for i := fmEnd; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// 围栏开/合：只在行首（允许缩进）出现时生效。
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			marker := trimmed[:3]
			if fence == "" {
				fence = marker
			} else if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue // 代码块内的 # 是注释/内容，不是标题
		}

		if lv, title, ok := atxHeading(line); ok {
			heads = append(heads, head{i, lv, title})
		}
	}

	if len(heads) == 0 {
		// 没有标题：整份文档作为一个匿名章节。
		return []skillSection{{Level: 0, Title: "", Start: 0, End: len(lines)}}
	}

	var out []skillSection
	// 第一个标题之前的内容（含 frontmatter）作为匿名前导章节。
	if heads[0].line > 0 {
		out = append(out, skillSection{Level: 0, Title: "", Start: 0, End: heads[0].line})
	}
	for i, h := range heads {
		end := len(lines)
		if i+1 < len(heads) {
			end = heads[i+1].line
		}
		out = append(out, skillSection{
			Level: h.level,
			Title: h.title,
			Start: h.line,
			End:   end,
		})
	}
	return out
}

// atxHeading 判断一行是否是 ATX 标题（# 到 ######，后接空格或行尾）。
//
// 与 CommonMark 对齐：`#foo` 不是标题（缺空格）；`#` 单独成行是空标题。
// 这条规则正是把 shell 注释 `#期望 200`（无空格）与真标题区分开的依据 ——
// 不过代码块内的行已经在 parseSections 里被排除了，这里是第二道闸。
func atxHeading(line string) (level int, title string, ok bool) {
	i := 0
	for i < len(line) && line[i] == ' ' {
		i++ // 允许至多 3 个前导空格（这里放宽，不影响正确性）
	}
	if i >= len(line) || line[i] != '#' {
		return 0, "", false
	}
	j := i
	for j < len(line) && line[j] == '#' {
		j++
	}
	level = j - i
	if level > 6 {
		return 0, "", false
	}
	// 标题标记后必须是空格或行尾
	if j < len(line) && line[j] != ' ' && line[j] != '\t' {
		return 0, "", false
	}
	return level, strings.TrimSpace(line[j:]), true
}

// renderTOC 把章节列表渲染成目录（供 skill_info 不传 section 时返回）。
//
// 只列 Level <= maxLevel 的章节，避免把 #### 这类细节全铺出来。
// 顶层标题带行数，让模型能判断"读它值不值" —— 这是它在
// "一次读一万 token" 和 "先读目录再决定" 之间做选择的唯一依据。
func renderTOC(sections []skillSection, maxLevel int) string {
	var b strings.Builder
	for _, s := range sections {
		if s.Title == "" {
			continue
		}
		if s.Level == 0 || (maxLevel > 0 && s.Level > maxLevel) {
			continue
		}
		indent := strings.Repeat("  ", s.Level-1)
		fmt.Fprintf(&b, "%s%s%s  (%d 行)\n", indent, strings.Repeat("#", s.Level), s.Title, s.End-s.Start)
	}
	return b.String()
}

// 长文档判定与目录预览行的取值。
//
// 依据：生产上 26 个随包 skill 的行数分布，绝大多数在 20–250 行之间，
// 其中 <=200 行的占了大部分。200 行约 2000 token，一次读完仍算合理；
// 超过它才值得加一层目录（实测 commondev-discipline 982 行 ≈ 10600 token，
// 是唯一明显"必须按需读"的一份）。
const (
	shortSkillLines = 200 // <= 此行数直接返回全文
	tocHeadLines    = 30  // 长文档先给开头多少行
)

// resolveSection 在章节列表里按标题匹配，返回正文行区间。
//
// 匹配顺序（越靠前越优先，保证 "Setup" 不会误中 "Setup 细节"）：
//
//	① 标题完全相同（忽略大小写与首尾空白）
//	② 标题包含查询（子串，忽略大小写）
//	③ 查询包含标题（模型可能把 `## Setup: ...` 整行连同 # 一起传进来）
//
// 命中多个时取**层级最浅、位置最靠前**的那个 —— 顶层章节包含它的子节，
// 返回整块更符合"我要看 Setup 这部分"的意图。
func resolveSection(raw string, sections []skillSection, want string) (start, end, idx int, ok bool) {
	q := strings.ToLower(strings.TrimSpace(want))
	// 允许模型把 # 一起传进来
	q = strings.TrimLeft(q, "#")
	q = strings.TrimSpace(q)
	if q == "" {
		return 0, 0, 0, false
	}

	best := -1
	for i, s := range sections {
		title := strings.ToLower(s.Title)
		if title == "" {
			continue
		}
		if title != q && !strings.Contains(title, q) && !strings.Contains(q, title) {
			continue
		}
		if best < 0 {
			best = i
			continue
		}
		// 相同命中质量下：层级浅优先，其次位置靠前。
		if s.Level < sections[best].Level {
			best = i
		}
	}
	if best < 0 {
		return 0, 0, 0, false
	}

	lines := strings.Split(raw, "\n")
	root := sections[best]

	// ★ 关键：返回**整棵子树**，而不只是标题直到下一个同级标题。
	//
	// 例：`## Setup` 下面有 §11–§16 六个 `###` 子节。若只切到下一个
	// 同级标题，遇到 `## Usage` 才停 —— 那没问题；但如果模型点名的是
	// `### 14. 常见操作`，下一个标题就是 `### 15.`，只返回 §14 本身，
	// 这是对的。两种情况都是"到下一个同级或更浅的标题为止"。
	end = len(lines)
	for j := best + 1; j < len(sections); j++ {
		if sections[j].Level != 0 && sections[j].Level <= root.Level {
			end = sections[j].Start
			break
		}
	}
	start = root.Start
	if start >= len(lines) {
		return 0, 0, 0, false
	}
	if end > len(lines) {
		end = len(lines)
	}
	return start, end, best, true
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
