package pluginmgr

// 暴露给 agent 的两个更新工具的判据。
//
// ## 为什么需要它
//
// kernel_update / plugin_update 是**模型唯一能触达的更新入口**：
// 它们一旦被删掉、改名、或参数对不上，agent 就再也升不了级 ——
// 而源码里 grep 得到工具名**不能证明**它注册成功（本仓教训：
// E-3 那次"源码里有、运行时没有"靠静态字符串查不出来）。
//
// 所以这里走**真实 registerTools**（捕获点 SDKConfig.RegTool），
// 断言"注册结果"而不是"源码文本"。
//
// ## 判据重点：参数与描述必须让 agent 选对
//
// agent 只能靠 Description/Parameters 判断怎么调用。若描述不说明
// "两种来源二选一"，模型会漏传 pkg/release，然后被工具拒绝一次 ——
// 多烧一轮往返。

import (
	"strings"
	"testing"
)

// ★★★ 判据：两个更新工具都注册给了 agent。
func TestUpdateToolsAreRegisteredToAgent(t *testing.T) {
	defs := collectToolDefs(t)
	for _, want := range []string{"kernel_update", "plugin_update"} {
		def, ok := defs[want]
		if !ok {
			t.Fatalf("★ 缺少工具 %s —— agent 无法升级%s", want,
				map[string]string{"kernel_update": "内核", "plugin_update": "插件"}[want])
		}
		// 两个工具都会改磁盘/运行态 ⇒ 必须串行，避免与 install/remove 撞车。
		if !def.Serial {
			t.Errorf("★ %s 未标记 Serial：更新会替换文件并重启，与安装/卸载并发会撞车", want)
		}
		if strings.TrimSpace(def.Description) == "" {
			t.Errorf("★ %s 缺 Description —— 模型无法判断何时该用", want)
		}
	}
}

// ★★★ 判据：kernel_update 必须同时支持"本机包"与"Release 自动更新"两种来源。
//
// 这是用户明确要求的能力：只支持其中一种，另一种场景就没有入口。
func TestKernelUpdateToolExposesBothSources(t *testing.T) {
	def := collectToolDefs(t)["kernel_update"]
	props, _ := def.Parameters["properties"].(map[string]interface{})
	if props == nil {
		t.Fatalf("★ kernel_update 没有 properties，模型无法传参")
	}

	apply, _ := props["apply"].(map[string]interface{})
	if apply == nil {
		t.Fatal("★ kernel_update 缺 apply 参数（两阶段：默认只检查）")
	}

	// 来源一：本机包
	pkg, _ := props["pkg"].(map[string]interface{})
	if pkg == nil {
		t.Error("★ kernel_update 缺 pkg 参数 —— 无法用本机安装包升级")
	}

	// 来源二：Release 自动更新
	release, _ := props["release"].(map[string]interface{})
	if release == nil {
		t.Error("★ kernel_update 缺 release 参数 —— 无法从 GitHub Release 自动更新")
	}

	// 描述必须说清"二选一"，否则模型不知道两者关系（会都传或都不传）。
	d := def.Description
	if !strings.Contains(d, "pkg") || !strings.Contains(d, "Release") {
		t.Errorf("★ 描述应同时说明 pkg 与 Release 两种来源，实际: %s", d)
	}
}

// ★★★ 判据：plugin_update 必须有显式范围（不许默认全量升级）。
//
// 默认全升的后果：用户只想升一个插件，结果所有插件被重编译重装，
// 配置/依赖被动变更 —— 一次不该发生的批量变更。
func TestPluginUpdateToolRequiresExplicitScope(t *testing.T) {
	def := collectToolDefs(t)["plugin_update"]
	props, _ := def.Parameters["properties"].(map[string]interface{})
	if props == nil {
		t.Fatal("★ plugin_update 没有 properties")
	}
	plugins, _ := props["plugins"].(map[string]interface{})
	if plugins == nil {
		t.Fatal("★ plugin_update 缺 plugins 参数 —— 无法表达升级范围")
	}
	if !strings.Contains(def.Description, "all") {
		t.Errorf("★ 描述应说明范围取 all 或名单，实际: %s", def.Description)
	}
}
