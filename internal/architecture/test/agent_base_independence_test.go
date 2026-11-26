package test

// agent_base_independence_test.go —— **基座对类型包零依赖**（第一判据）。
//
// 用户裁定：删掉 `internal/agent/{mainagent,resident,rtchild,worker}`，
// 基座（`internal/agent/agent` 及其核心子包）仍能编译，并能实例化一个
// "只有 inputch/outputch、按契约产 output"的裸 agent。
//
// 判据用 `go list -deps`（Go 自己算的依赖，最权威），不读源码猜。

import (
	"os/exec"
	"strings"
	"testing"
)

// basePackages 是**基座**（内核空间最上层 + syscall）。
var basePackages = []string{
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent",
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent/child",
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry",
	"github.com/JianFeeeee/HologramAgent/internal/agent/api",
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit",
	"github.com/JianFeeeee/HologramAgent/internal/agent/io",
	"github.com/JianFeeeee/HologramAgent/internal/agent/agentreg",
}

// typePackages 是**用户空间**的四个内置类型。基座一个都不许 import。
var typePackages = []string{
	"internal/agent/mainagent",
	"internal/agent/resident",
	"internal/agent/rtchild",
	"internal/agent/worker",
}

// moduleRoot 返回本模块根目录（`go list -m -f {{.Dir}}`）。
func moduleRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatalf("go list -m: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestBaseDoesNotDependOnAnyAgentType：基座的传递依赖里没有任何类型包。
func TestBaseDoesNotDependOnAnyAgentType(t *testing.T) {
	for _, pkg := range basePackages {
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		for _, dep := range strings.Split(string(out), "\n") {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				continue
			}
			for _, ty := range typePackages {
				if strings.HasSuffix(dep, ty) {
					t.Fatalf("★ 基座 %s 依赖了类型包 %s —— 删掉那个包基座就编译不过", pkg, dep)
				}
			}
		}
	}
}

// TestOnlyAssemblyPointImportsTheTypes：类型包**只**由装配点引入。
//
// 这正是"加一种新类型时基座与既有类型一行不改"的结构条件：
// 类型经自己的 `init()` 登记进唯一那张表，装配点只做一次空白 import。
func TestOnlyAssemblyPointImportsTheTypes(t *testing.T) {
	// ★ 必须从**模块根**列包：`go test` 的 cwd 是包目录，
	//   `./...` 在那里只会列出本测试包（实测漏掉了整仓依赖）。
	root := moduleRoot(t)
	cmd := exec.Command("go", "list", "-f", "{{.ImportPath}}: {{join .Imports \" \"}}", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list ./...: %v", err)
	}
	allowed := map[string]bool{}
	for _, p := range []string{
		"github.com/JianFeeeee/HologramAgent/internal/agent/mainagent",
		"github.com/JianFeeeee/HologramAgent/internal/agent/resident",
		"github.com/JianFeeeee/HologramAgent/internal/agent/rtchild",
		"github.com/JianFeeeee/HologramAgent/internal/agent/worker",
	} {
		allowed[p] = true
	}
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(line, ": ", 2)
		if len(parts) != 2 {
			continue
		}
		importer := parts[0]
		// 类型包互相之间、测试、以及**装配点**是合法的。
		//
		// ⚠️ `kernelmod/agenttype` 目前仍在允许列表里，但它**是债务不是许可**：
		//    它借 `mainagent.SchedulerLoop` 驱动插件类型。R-3 完成后它要
		//    有自己的驱动，这行豁免随之删除（判据见 todo.md R-3）。
		if strings.Contains(importer, "/internal/agent/") ||
			strings.HasSuffix(importer, "/test") ||
			importer == "github.com/JianFeeeee/HologramAgent/internal/kernelmod/agent" ||
			importer == "github.com/JianFeeeee/HologramAgent/internal/kernelmod/agenttype" {
			continue
		}
		for _, imp := range strings.Fields(parts[1]) {
			if allowed[imp] {
				t.Fatalf("★ %s 引入了类型包 %s —— 只有装配点可以 (加类型时要改的不是它)",
					importer, imp)
			}
		}
	}
}
