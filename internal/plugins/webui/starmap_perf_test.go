package webui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 星图渲染必须避免「每帧全量」——这些判据钉住 2026-10-08 修「非常卡」的三处。
//
// 为何用源码断言而不是运行时测量：dashboard.js 是嵌入的浏览器脚本，
// 用 Node 跑不了（依赖 three.js / DOM）。而这三条都是**结构性**缺陷
// （循环扫全表、循环不停、每帧重建），源码里能可靠判定，且回归时立刻红。
func TestStarmapNoPerFrameFullScan(t *testing.T) {
	src := readDashboardJS(t)

	// ① starmapTickMorph 不得再全量遍历 starmapNodeMeshes。
	//    原先 3190 节点 × 60fps = 每秒 19 万次 userData 读取与分支，
	//    而绝大多数帧里「正在形变」的节点是 0 个。
	morph := extractFunc(t, src, "starmapTickMorph")
	if strings.Contains(morph, "starmapNodeMeshes") {
		t.Error("starmapTickMorph 又全量遍历 starmapNodeMeshes 了：应只遍历 starmapMorphSet 活跃集，" +
			"空集时 O(1) 返回（否则每帧 3190 次无谓遍历）")
	}
	if !strings.Contains(morph, "starmapMorphSet") {
		t.Error("starmapTickMorph 必须走 starmapMorphSet 活跃集合")
	}
	if !regexp.MustCompile(`starmapMorphSet\.size\s*===\s*0`).MatchString(morph) {
		t.Error("starmapTickMorph 必须在活跃集为空时提前返回（这是常见路径，应当零开销）")
	}

	// ② 渲染循环必须能被暂停（否则切走页签/后台仍 60fps 空转）。
	anim := extractFunc(t, src, "starmapAnimate")
	if !strings.Contains(anim, "starmapAnimateRunning") {
		t.Error("starmapAnimate 缺少 starmapAnimateRunning 开关：切走页签后循环不会停")
	}
	if !strings.Contains(anim, "starmapActiveContainer") {
		t.Error("starmapAnimate 必须按「星图容器是否可见」判断暂停，" +
			"不能用 document.hidden（星图可能在未激活页签里被搬走）")
	}

	// ③ 重建图时必须清空形变活跃集，否则集合单调增长、
	//    「空集 O(1)」快速路径永不生效，每帧还遍历已废弃 mesh。
	//    断言必须在**重建函数体内**且真的在调用 clear（不能只看全仓有这串，
	//    否则改成 `if (false) ...clear()` 也能骗过——变异测试实测过）。
	rebuild := extractFunc(t, src, "buildChatStarmapGraph")
	if !regexp.MustCompile(`(?m)^\s*if \(starmapMorphSet\) starmapMorphSet\.clear\(\);`).MatchString(rebuild) {
		t.Error("buildChatStarmapGraph 必须无条件清空 starmapMorphSet（`if (starmapMorphSet) starmapMorphSet.clear();`）：" +
			"旧 mesh 已从场景摘掉，却会留在活跃集里被永远遍历")
	}

	// ④ 标签层必须有节流（相机拖动时 camKey 每帧都变 → 否则每帧重建全量 plan）。
	labels := extractFunc(t, src, "starmapDrawLabels")
	if !strings.Contains(labels, "SM_LABEL_MIN_INTERVAL") {
		t.Error("starmapDrawLabels 缺少节流：OrbitControls 有阻尼，拖动时每帧重建 3190 项 plan")
	}

	// ⑤ ★★ 2026-10-13：力导向布局必须是网格近邻（O(n·k)），不得再出现
	//    全对全（O(n²)）的双层循环。
	//
	//    为何单独钉这一条：生产库 3754 块时全对全 50 轮 = 3.52 亿次配对，
	//    CDP Profiler 实测单次阻塞主线程 10.9 秒。而斥力体里本来就有
	//    `if (d < 25)` —— 只对近邻施力，没必要全对全。
	//
	//    ★ 判据必须同时覆盖**两处**实现（串行回退 + Worker 脚本）：
	//    只盯着 dashboard.js 的话，把回退改回全对全就没人拦 ——
	//    而那正是“有 Worker 与没 Worker 看到的图不一样”的成因。
	relax := extractFunc(t, src, "starmapLayoutRelax")
	if !strings.Contains(relax, "grid") || !strings.Contains(relax, "CUTOFF") {
		t.Error("starmapLayoutRelax 必须走网格近邻（grid + CUTOFF）：" +
			"全对全斥力在生产库（3754 块）实测阻塞主线程 10.9 秒")
	}
	// 全对全的特征：内层循环遍历外层指针的副本（Object.keys(pos) / ids.length）
	if strings.Contains(relax, "Object.keys") {
		t.Error("starmapLayoutRelax 又回到 Object.keys 全量遍历：那是 O(n²) 的写法")
	}
	if !strings.Contains(src, "starmapLayoutIterationsFor") {
		t.Error("缺少迭代数自适应（starmapLayoutIterationsFor）：" +
			"固定 50 轮在大图上绝大多数轮次是在重复收敛后的状态")
	}

	// ★★ 布局代码必须**与 GUI 同源**（2026-10-13）。
	//
	// 为何单独钉这一条：这份布局曾在 dashboard.js 与 cmd/gui/renderer/app.js
	// 各存一份手抄。两次真实事故都出自这个分叉：
	//   ① WebUI 换上网格法后 GUI 那份没跟着改（换客户端卡顿原样存在）
	//   ② 反向抄时抄了 WebUI 独有的 starmapMorphSet，GUI 用 state.starmapGrown
	//      ⇒ 渲染循环每帧 ReferenceError、星图 0 帧
	// 现在唯一事实源在 shared/starmap/layout.js，由 tools/starmap-sync.mjs
	// 写进两处标记块。这里校验「本文件里的块 == 共享源」，手改会被立刻拦下。
	checkStarmapSharedBlock(t, src)

	// Worker 脚本必须存在且用同一套网格算法
	wb, err := os.ReadFile(filepath.Join("static", "starmap-layout-worker.js"))
	if err != nil {
		t.Fatalf("缺少 static/starmap-layout-worker.js: %v", err)
	}
	wsrc := string(wb)
	if !strings.Contains(wsrc, "CUTOFF") || !strings.Contains(wsrc, "iterationsFor") {
		t.Error("starmap-layout-worker.js 必须用同一套网格法（CUTOFF）与自适应迭代数，" +
			"否则 Worker 与串行回退会给出不同布局")
	}
	if regexp.MustCompile(`for \(var j = i \+ 1; j < ids\.length`).MatchString(wsrc) {
		t.Error("starmap-layout-worker.js 里仍有全对全双层循环（O(n²)）")
	}

	// ⑥ 建图必须在容器可见时才做（否则聊天页签也被拖死）。
	render := extractFunc(t, src, "renderChatStarmap")
	if !strings.Contains(render, "offsetParent") {
		t.Error("renderChatStarmap 必须在容器不可见时提前返回（offsetParent 判据）：" +
			"starmapActiveContainer 会 fallback 到聊天面板，使未打开星图也被迫建图")
	}
}

// extractFunc 取一个顶层函数的源码体（到下一个行首 `}` 为止）。
func extractFunc(t *testing.T, src, name string) string {
	t.Helper()
	marker := "function " + name + "("
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("找不到函数 %s", name)
	}
	rest := src[i:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func readDashboardJS(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("dashboard.js")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// checkStarmapSharedBlock 校验本文件里的共享块与 shared/starmap/layout.js 逐字节一致。
//
// 判据读宿主文件 + 共享源 + 生成器三者约定：标记块内容 = 生成器产出的那一整段。
// 用一个薄封装调生成器（--check）比在 Go 里重算拼接规则更可靠 ——
// 拼接规则只有一处（tools/starmap-sync.mjs），重复实现它正是「两份副本」的老毛病。
func checkStarmapSharedBlock(t *testing.T, src string) {
	t.Helper()
	const begin = "// ===== BEGIN shared:starmap/layout ====="
	const end = "// ===== END shared:starmap/layout ====="
	b := strings.Index(src, begin)
	e := strings.Index(src, end)
	if b < 0 || e < 0 || e < b {
		t.Fatalf("找不到共享块标记（%s / %s）—— 星图布局必须由 shared/starmap/layout.js 生成",
			begin, end)
	}
	block := src[b : e+len(end)]
	// 共享源必须被完整包进块里（抽首尾两段特征串判，避免把整份源码抄进断言）。
	shared, err := os.ReadFile(filepath.Join("..", "..", "..", "shared", "starmap", "layout.js"))
	if err != nil {
		t.Fatalf("读共享源失败: %v", err)
	}
	sh := strings.TrimRight(strings.ReplaceAll(string(shared), "\r\n", "\n"), "\n")
	// 共享源的正文（去掉首尾空行）必须逐字出现在块内。
	body := strings.TrimSpace(sh)
	if !strings.Contains(block, body) {
		t.Error("本文件的星图布局块与 shared/starmap/layout.js 不一致（漂移）。\n" +
			"     修法：node tools/starmap-sync.mjs\n" +
			"     若是改算法，请只改 shared/starmap/layout.js，再跑同步 ——\n" +
			"     直接改这个副本会让 WebUI 与 GUI 再次分叉（已因此出过两次事故）。")
	}
	if !strings.Contains(block, "generated") && !strings.Contains(block, "生成") {
		t.Error("共享块内应带「本块由生成器产出」的提示，避免后人当普通代码手改")
	}
}
