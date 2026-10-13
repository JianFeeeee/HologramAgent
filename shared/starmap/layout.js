// 星图力导向布局 —— WebUI 与 GUI 的**唯一事实源**。
//
// ===================================================================
// 这个文件为什么存在（别把它当成“又一份副本”）
// ===================================================================
//
// 下面这三个函数原本在 `internal/plugins/webui/dashboard.js` 与
// `cmd/gui/renderer/app.js` 里各有一份手抄。分叉的代价已经兑现过两次：
//
//   1. WebUI 把 O(n²) 布局换成网格近邻（生产库 3754 块：3.52 亿次配对 →
//      n·k 次），GUI 那份没跟着改 ⇒ 换客户端卡顿原样存在。
//   2. 把空闲帧跳过从 WebUI 抄进 GUI 时抄了 `starmapMorphSet`（WebUI 独有），
//      GUI 实际用 `state.starmapGrown` ⇒ 渲染循环每帧 ReferenceError、星图 0 帧。
//
// 所以这里放**唯一一份**实现，由 `tools/starmap-sync.mjs` 写进两个客户端文件
// 的 `BEGIN/END shared:starmap/layout` 标记块内，并由判据钉住“三处逐字节相同”。
//
// ===================================================================
// 算法
// ===================================================================
//
// 被替换掉的旧实现是 50 轮 `for i / for j=i+1` 全对全斥力：
// 生产库 3754 块 ⇒ 每轮 704 万次配对、50 轮 3.52 亿次。实测（CDP Profiler
// + Electron）单次建图阻塞主线程 1.8–10.9 秒。
//
// 关键观察：原斥力体里本来就有 `if (d < 25)` —— **只对近邻施力**。既然只关心
// 近邻，就没必要算全对全：把空间切成边长 == 斥力半径的格子，每个节点只与
// 「自身格 + 相邻 26 格」（3×3×3）内的点比较即可。
//
// ★ 为何格边长必须恰等于斥力半径：这样「距离 < 25 的点」必然落在相邻 26 格内，
//   **绝不遗漏** —— 这是敢跳过远处配对的唯一依据。格边长一旦小于斥力半径，
//   就会漏掉真实邻居，图会塌成一团。
//
// 复杂度 O(n·k)：均匀分布下每格约 1 点 ⇒ 每轮约 27n 次比较。
// 配合迭代数自适应与收敛早停，3754 节点实测 8365ms → 438ms。

// 自适应迭代数：小图跑满 50 轮（值得精细），大图退到 14 轮。
//
// n=3754 时约 15 轮。旧实现固定 50 轮，且每轮是 O(n²)。
// 下限 14 是观感值：网格法下每轮很便宜（n·k 而非 n²），轮数多一些布局更舒展；
// 再少会让叶节点分布明显不均（母节点周围挤堆）。
function starmapLayoutIterationsFor(n) {
	if (n <= 400) return 50;
	var it = Math.floor(60000 / n);
	if (it < 14) it = 14;
	if (it > 50) it = 50;
	return it;
}

// 早停阈值：取「节点平均间距的 0.2%」。
// 用相对量而不是绝对值，否则同一个阈值在不同规模的图上含义完全不同。
function starmapLayoutMaxMoveThreshold(n, maxR) {
	var perimeter = 2 * Math.PI * maxR;
	var gap = n > 0 ? perimeter / n : maxR;
	var th = gap * 0.002;
	var cap = maxR * 2 * 0.001;
	return th < cap ? th : cap;
}

// starmapLayoutRelax 就地松弛坐标。
//
// 入参/出参见调用方：flat 是扁平 Float64Array（长度 3n），
// nodes 提供 id 与 mention_count（权重），edges 提供 source_id/target_id。
// 用扁平数组而非对象数组，是为了让可行时能直接走结构化克隆交给 Worker。
function starmapLayoutRelax(flat, nodes, edges, maxR) {
	var n = nodes.length;
	if (n === 0) return flat;
	var CUTOFF = 25; // == 斥力半径，见文件头说明
	var BASE_R = 15;
	// 归一化权重（斥力/吸力都按它加权）
	var mn = new Float64Array(n);
	var maxMc = 1,
		minMc = 0;
	for (var a = 0; a < n; a++) {
		var m = (nodes[a] && nodes[a].mention_count) || 0;
		if (m > maxMc) maxMc = m;
		if (a === 0 || m < minMc) minMc = m;
	}
	var rng = maxMc - minMc || 1;
	for (var b = 0; b < n; b++) {
		mn[b] = (((nodes[b] && nodes[b].mention_count) || 0) - minMc) / rng;
	}
	// 边下标（id → 下标对）。下标化是为了内层循环不做字符串比较。
	var idx = {};
	for (var i0 = 0; i0 < n; i0++) idx[nodes[i0].id] = i0;
	var edgeIdx = [];
	for (var e0 = 0; e0 < edges.length; e0++) {
		var s0 = idx[edges[e0].source_id],
			t0 = idx[edges[e0].target_id];
		if (s0 === undefined || t0 === undefined || s0 === t0) continue;
		edgeIdx.push([s0, t0]);
	}
	var grid = new Map();
	function rebuildGrid() {
		grid.clear();
		for (var i = 0; i < n; i++) {
			var key =
				Math.floor(flat[i * 3] / CUTOFF) +
				"|" +
				Math.floor(flat[i * 3 + 1] / CUTOFF) +
				"|" +
				Math.floor(flat[i * 3 + 2] / CUTOFF);
			var bucket = grid.get(key);
			if (!bucket) grid.set(key, (bucket = []));
			bucket.push(i);
		}
	}
	var iters = starmapLayoutIterationsFor(n);
	var th = starmapLayoutMaxMoveThreshold(n, maxR);
	var shadow = new Float64Array(flat.length);
	shadow.set(flat);
	for (var it = 0; it < iters; it++) {
		rebuildGrid();
		// 斥力：只算 3×3×3 邻格内的近邻；每对只处理一次（j > i），
		// 与旧实现 `for j = i+1` 的语义一致，避免重复施力。
		for (var i1 = 0; i1 < n; i1++) {
			var cxi = Math.floor(flat[i1 * 3] / CUTOFF);
			var cyi = Math.floor(flat[i1 * 3 + 1] / CUTOFF);
			var czi = Math.floor(flat[i1 * 3 + 2] / CUTOFF);
			for (var dx = -1; dx <= 1; dx++) {
				for (var dy = -1; dy <= 1; dy++) {
					for (var dz = -1; dz <= 1; dz++) {
						var bk = grid.get(cxi + dx + "|" + (cyi + dy) + "|" + (czi + dz));
						if (!bk) continue;
						for (var bi = 0; bi < bk.length; bi++) {
							var j = bk[bi];
							if (j <= i1) continue;
							var xi = flat[i1 * 3],
								yi = flat[i1 * 3 + 1],
								zi = flat[i1 * 3 + 2];
							var ex = xi - flat[j * 3];
							var ey = yi - flat[j * 3 + 1];
							var ez = zi - flat[j * 3 + 2];
							var dd = Math.sqrt(ex * ex + ey * ey + ez * ez) + 0.1;
							if (dd >= CUTOFF) continue;
							var rf = 0.5 + (mn[i1] + mn[j]) * 0.5;
							var force = (0.06 * rf) / Math.max(dd, 0.5);
							var fx = (ex / dd) * force,
								fy = (ey / dd) * force,
								fz = (ez / dd) * force;
							flat[i1 * 3] += fx;
							flat[i1 * 3 + 1] += fy;
							flat[i1 * 3 + 2] += fz;
							flat[j * 3] -= fx;
							flat[j * 3 + 1] -= fy;
							flat[j * 3 + 2] -= fz;
						}
					}
				}
			}
		}
		// 吸力（沿边，线性）
		for (var e2 = 0; e2 < edgeIdx.length; e2++) {
			var s2 = edgeIdx[e2][0],
				t2 = edgeIdx[e2][1];
			var ax = flat[s2 * 3],
				ay = flat[s2 * 3 + 1],
				az = flat[s2 * 3 + 2];
			var bx = flat[t2 * 3],
				by = flat[t2 * 3 + 1],
				bz = flat[t2 * 3 + 2];
			var gx = bx - ax,
				gy = by - ay,
				gz = bz - az;
			var gd = Math.sqrt(gx * gx + gy * gy + gz * gz) + 0.1;
			if (gd <= 20) continue;
			var af = 0.04 * Math.max(0.3, 1.0 - (mn[s2] + mn[t2]) * 0.3);
			var ux = (gx / gd) * af,
				uy = (gy / gd) * af,
				uz = (gz / gd) * af;
			flat[s2 * 3] += ux;
			flat[s2 * 3 + 1] += uy;
			flat[s2 * 3 + 2] += uz;
			flat[t2 * 3] -= ux;
			flat[t2 * 3 + 1] -= uy;
			flat[t2 * 3 + 2] -= uz;
		}
		// 向心约束 + 早停位移统计（一次遍历同时做完，省一趟内存读）
		var maxA = maxR * 1.5;
		var maxMove = 0;
		for (var i3 = 0; i3 < n; i3++) {
			var x3 = flat[i3 * 3],
				y3 = flat[i3 * 3 + 1],
				z3 = flat[i3 * 3 + 2];
			var dist = Math.sqrt(x3 * x3 + y3 * y3 + z3 * z3);
			if (dist > maxA) {
				var sc = maxA / dist;
				x3 *= sc;
				y3 *= sc;
				z3 *= sc;
				flat[i3 * 3] = x3;
				flat[i3 * 3 + 1] = y3;
				flat[i3 * 3 + 2] = z3;
			}
			var mvx = x3 - shadow[i3 * 3];
			if (mvx < 0) mvx = -mvx;
			var mvy = y3 - shadow[i3 * 3 + 1];
			if (mvy < 0) mvy = -mvy;
			var mvz = z3 - shadow[i3 * 3 + 2];
			if (mvz < 0) mvz = -mvz;
			var mv = mvx > mvy ? mvx : mvy;
			if (mvz > mv) mv = mvz;
			if (mv > maxMove) maxMove = mv;
		}
		shadow.set(flat);
		if (maxMove < th) break; // 收敛：剩余轮次只是重复同一状态
	}
	return flat;
}
