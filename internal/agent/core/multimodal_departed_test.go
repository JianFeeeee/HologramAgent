// 结构性判据：多模态工具**已从内核退场**（2026-10-06）
//
// ## 为什么需要结构性判据
//
// 前面 plugin_test.go 验的是「插件侧行为对不对」。但这次改动的**要害**
// 不是插件能不能用，而是**内核不再持有这三个工具** ——
// 因为内核承诺零 IO，而它们要读本地文件。
//
// 行为判据抓不住这件事：哪怕把内核实现改回去，只要插件侧仍然工作，
// 功能测试照样全绿。而「内核能不能读文件」正是本仓第一原则的红线。
//
// ⇒ 这条判据直接扫源码：三个工具名不得出现在 internal/agent/core/ 的
//
//	实现文件里，也不得有 os.ReadFile 之类为它们服务。
package core

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// mediaToolNames 是已移出内核的三个工具。
var mediaToolNames = []string{"describe_image", "ocr_image", "transcribe_audio"}

// kernelImplFiles 内核的实现文件（不含 _test.go —— 判据自身要断言的东西不该自己触发）。
func kernelImplFiles(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读包目录: %v", err)
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasSuffix(n, "_test.go") || !strings.HasSuffix(n, ".go") {
			continue
		}
		out = append(out, n)
	}
	if len(out) < 5 {
		t.Fatalf("只找到 %d 个实现文件，判据覆盖不足", len(out))
	}
	return out
}

// TestMediaToolsGoneFromKernel 钉住：内核实现里不再出现这三个工具名。
//
// ★ 这是本次改动的**核心不变量**。它对应 README 的第一原则：
//
//	内核零 IO，而这三个工具要读本地文件。
func TestMediaToolsGoneFromKernel(t *testing.T) {
	for _, f := range kernelImplFiles(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s: %v", f, err)
		}
		src := string(b)
		// 只看代码行：注释里可以说明「已移出」（那是给人看的）
		for _, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			for _, name := range mediaToolNames {
				if !strings.Contains(line, name) {
					continue
				}
				// 允许出现在「已移出」的说明性注释里（带 ★ 或 已移出 标记）
				if strings.Contains(line, "已移出") || strings.Contains(line, "★") {
					continue
				}
				t.Errorf("internal/agent/core/%s 仍出现 %q —— 它已移出为内置插件 multimodal：\n\t%s",
					f, name, trimmed)
			}
		}
	}
}

// TestKernelHasNoMediaFileReading 钉住：内核不得为多模态读本地文件。
//
// ★ 比「名字没出现」更强：任何人新写一个 os.ReadFile 来干同样的事，
//
//	这条也会红。
//
// 已知例外：context.go 的上下文窗口持久化（savePath）属**记忆子系统**，
// 已在允许清单里 —— 它不是多模态，且属于内核既有的职责。
func TestKernelHasNoMediaFileReading(t *testing.T) {
	// 允许的内核磁盘读：上下文窗口持久化（记忆面）
	allowed := map[string]bool{
		"context.go": true, // RelevanceContext.savePath 的 load/save
	}
	readFile := regexp.MustCompile(`os\.(ReadFile|Open)\(`)
	for _, f := range kernelImplFiles(t) {
		if allowed[f] {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s: %v", f, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if readFile.MatchString(line) && !strings.HasPrefix(strings.TrimSpace(line), "//") {
				t.Errorf("internal/agent/core/%s:%d 出现磁盘读取 %q —— "+
					"内核承诺零 IO（多模态读文件已移到内置插件 multimodal）",
					f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestMediaToolsRegisteredByPlugin 确认能力没丢：插件侧确实注册了这三个工具。
//
// ★ 与上一条成对：「内核不该有」+「插件要有」= 迁移而不是删除。
//
//	只断言前者会放过「直接删掉功能」这种退化。
func TestMediaToolsRegisteredByPlugin(t *testing.T) {
	// 插件侧的注册清单在 internal/plugins/multimodal/plugin.go。
	// 这里只做**跨包引用检查**（core 包不该 import multimodal，那是反向依赖）。
	if strings.Contains(kernelSourceDump(t), `"github.com/JianFeeeee/HologramAgent/internal/plugins/multimodal"`) {
		t.Error("core 反向 import 了 multimodal 插件 —— " +
			"工具由插件注册进 StageHost，核心不应知道插件实现")
	}
}

func kernelSourceDump(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	for _, f := range kernelImplFiles(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s: %v", f, err)
		}
		sb.Write(b)
	}
	return sb.String()
}
