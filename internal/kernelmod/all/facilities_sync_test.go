package all

// 设施名「内核 ↔ 公开 SDK」一致性判据（2026-10-16）。
//
// ## 为什么需要它
//
// 设施名同时存在于**两处**，且必须逐字一致：
//
//	内核：internal/kernelmod/facilities.go   （模块 Provides 用）
//	SDK：third_party/homeagent-sdk/sdk/facilities.go （插件声明用）
//
// 它们**不能合并**：SDK 是独立发布的模块（外部插件依赖它），
// 而内核的 internal 规则又禁止内部包被外部 import。
//
// ⇒ 两处并存 ⇒ 有漂移风险。**改一处忘另一处**的后果是：
// 插件按 SDK 声明的名字，内核从未注册 ⇒ 插件**永不启动**，且（在本判据
// 与运行期校验出现前）无任何报错。
//
// 本判据把"两边一致"变成可执行的检查。
//
// ## 为什么必须从**源码**提取而不是手写清单
//
// 手写清单在"新增了一个设施名但只改了内核"时**也不会包含新名字**
// ⇒ 判据照样绿（守卫形同虚设）。本仓已有先例
// （TestCapability_AllMethodsClassified 初版就是这个毛病）。

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

// internalOnlyFacilities 是**内核内部**顺序标记（不是给插件的契约）。
//
// ★ 判据要排除它们：它们的消费者是内核模块，不该进公开 SDK。
// 若把它们也拿去比对，会得到"内核有但 SDK 没有"的**假红**（实测撞到：
// knowledge-migrated）。反向也要防：有人误把内部标记放进 SDK 当对外契约。
var internalOnlyFacilities = map[string]bool{
	"knowledge-migrated": true,
	// kernel.version 同理：它是 update 模块的启动时序事实（"版本自检与
	// 升级回滚判定已完成"），消费者是内核模块，不是插件契约。
	"kernel.version": true,
}

// extractFacilities 从 Go 源码里提取 `FacilityXxx = "值"` 形式的常量。
func extractFacilities(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	re := regexp.MustCompile(`(?m)^\s*(Fac[A-Za-z0-9_]*|Facility[A-Za-z0-9_]*)\s*=\s*"([^"]+)"`)
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = m[2]
	}
	if len(out) < 10 {
		t.Fatalf("只从 %s 提取到 %d 个设施常量 —— 正则或文件结构变了（判据会假绿）",
			path, len(out))
	}
	return out
}

// ★★★ 判据：**内核与 SDK 的设施名值集合逐字一致**。
func TestFacilityNamesMatchPublicSDK(t *testing.T) {
	kernel := extractFacilities(t, filepath.Join("..", "facilities.go"))
	sdk := extractFacilities(t, filepath.Join("..", "..", "..",
		"third_party", "homeagent-sdk", "sdk", "facilities.go"))

	kernelVals := map[string]string{} // 值 → 内核常量名
	for name, v := range kernel {
		if internalOnlyFacilities[v] {
			continue // 内核内部顺序标记，不是插件契约
		}
		kernelVals[v] = name
	}
	sdkVals := map[string]string{}
	for name, v := range sdk {
		sdkVals[v] = name
	}

	var missingInSDK, missingInKernel []string
	for v, name := range kernelVals {
		if _, ok := sdkVals[v]; !ok {
			missingInSDK = append(missingInSDK, name+"="+v)
		}
	}
	for v, name := range sdkVals {
		if _, ok := kernelVals[v]; !ok {
			missingInKernel = append(missingInKernel, name+"="+v)
		}
	}
	sort.Strings(missingInSDK)
	sort.Strings(missingInKernel)

	if len(missingInSDK) > 0 {
		t.Errorf("★ 内核有但**公开 SDK 没有**的设施名: %v\n"+
			"   后果：插件作者无法 import 该常量（只能照文档抄 ⇒ 易拼错），"+
			"且打包期校验会把它当'未知设施名'而拒绝", missingInSDK)
	}
	for v := range sdkVals {
		if internalOnlyFacilities[v] {
			t.Errorf("★ 公开 SDK 里出现了内核**内部顺序标记** %q —— "+
				"它的消费者是内核模块，不该成为插件契约（插件声明它没有意义，"+
				"且会误导作者以为可用）", v)
		}
	}
	if len(missingInKernel) > 0 {
		t.Errorf("★ 公开 SDK 有但**内核没有**的设施名: %v\n"+
			"   后果：插件按此声明 ⇒ 内核从未注册 ⇒ 插件**永不启动**（静默）",
			missingInKernel)
	}
}
