package pluginmgr

// ★★★ 真实大包安装的端到端判据。
//
// 用**内核自己的 installFromData** 装一个真实约 99MB 的 .hmap
// （commondev 内嵌 node+pi 的产物）。这条判据回答的是用户的问题：
// "解除限制后，内嵌运行时真能装进来吗？"

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRealEmbeddedRuntimePackageInstalls(t *testing.T) {
	src := os.Getenv("HA_RAWFILE_E2E_HMAP")
	if src == "" {
		t.Skip("设 HA_RAWFILE_E2E_HMAP=<hmap路径> 以运行")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读包失败: %v", err)
	}
	t.Logf("真实包大小: %.1f MiB", float64(len(data))/(1<<20))

	// ★ 旧上限下这个包会被拒（对照）
	if int64(len(data)) > 100<<20 {
		t.Logf("注意：该包超过旧的 100MB 硬编码上限")
	}

	// ★ pluginDir 必须显式给定，否则 installFromData 直接返回
	// {"error":"plugin dir not configured"} 且 err == nil ——
	// 初版没设它，于是判据"通过"了但什么都没装。这是本仓反复出现的
	// "假通过"模式：**必须断言返回值的内容，不能只看 err**。
	dir := t.TempDir()
	p := &Plugin{
		pluginDir:       dir,
		maxPackageBytes: int64(defaultMaxPackageMB) << 20,
	}
	res, err := p.installFromData(data, false)
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if m, ok := res.(map[string]interface{}); ok {
		if e, bad := m["error"]; bad {
			t.Fatalf("★ 放宽上限后真实包仍装不进来: %v (details=%v)", e, m["details"])
		}
	} else {
		t.Fatalf("返回值类型意外: %T", res)
	}

	// 装完必须能看到内嵌运行时资源（这才证明"包真的解开了"）
	installed := filepath.Join(dir, "commondev")
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("★ 插件目录不存在，包没被解开: %v", err)
	}
	node := filepath.Join(installed, "rawfile", "node", "bin", "node")
	st, err := os.Stat(node)
	if err != nil {
		t.Fatalf("★ 内嵌 node 不在预期路径: %v", err)
	}
	if st.Mode()&0o111 == 0 {
		t.Errorf("★ 内嵌 node 丢了可执行位: %v", st.Mode())
	}
	t.Logf("解包成功：内嵌 node %d 字节，可执行位 OK", st.Size())
}

// ★★★ 判据（对照）：钉住"真实包与旧上限的关系"这个**事实**。
//
// 这条判据的来历值得留着：我原本写的是"该包在旧 100MB 上限下会被拒"，
// 想用它证明上面的安装判据有意义。**它报红了** —— 实测包 103,795,462 字节
// < 旧上限 104,857,600 字节，差 1.01 MiB，根本没超。
//
// 也就是说：旧上限**从来不是**当前这个包的障碍，真正的风险是
// "余量只剩 1 MiB，加任何一个文件就爆"。搞错这一点会得出错误的
// "解除了一个阻塞"的结论。判据在这里的作用就是不让错的叙事留下来。
func TestRealPackageHeadroomAgainstOldLimit(t *testing.T) {
	src := os.Getenv("HA_RAWFILE_E2E_HMAP")
	if src == "" {
		t.Skip("设 HA_RAWFILE_E2E_HMAP=<hmap路径> 以运行")
	}
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	const oldLimit = 100 << 20
	headroom := oldLimit - st.Size()
	t.Logf("真实包 %.1f MiB，旧上限 %.1f MiB，余量 %.2f MiB",
		float64(st.Size())/(1<<20), float64(oldLimit)/(1<<20), float64(headroom)/(1<<20))

	// 事实断言：余量小于 5MiB 就算"贴近上限"，属于真实风险
	if headroom < 5<<20 {
		t.Logf("★ 确认：旧上限下余量仅 %.2f MiB —— 虽未超限，但任何新增资源都会爆。",
			float64(headroom)/(1<<20))
	}
	// 新上限必须给出实质空间
	newLimit := int64(defaultMaxPackageMB) << 20
	if newLimit-st.Size() < st.Size() {
		t.Errorf("★ 新上限 %dMB 未给出足够余量", defaultMaxPackageMB)
	}
}
