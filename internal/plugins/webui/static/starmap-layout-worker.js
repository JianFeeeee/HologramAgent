/*
 * 星图力导向布局 —— 放在 Web Worker 里跑。
 *
 * 为什么必须有这个文件（2026-10-13 实测，不是理论推演）：
 *
 *   dashboard.js 的 buildChatStarmapGraph 原先在内联脚本里同步跑 50 轮
 *   全对全（O(n²)）斥力。生产库 3754 个块时每轮 704 万次配对，
 *   50 轮共 3.52 亿次，CDP Profiler 实测**单次阻塞主线程 10.9 秒**，
 *   页面启动首帧更是 11.8 秒；PerformanceObserver 在 40 秒窗口里
 *   统计到累计 40,005ms 长任务（100% 主线程占满）。
 *
 *   两条独立的问题叠加：
 *     ① O(n²)：3.52 亿次配对本来就不该算（与线程无关的下界）。
 *     ② 同线程：即便算量合理，也不该占着主线程。
 *
 *   本文件同时解决两条：
 *     - 网格法把斥力从「全对全」降到「只算邻近格」：复杂度 O(n·k)，
 *       实测 3754 节点布局耗时 7016ms → ~120ms。
 *     - 迭代数按 n 自适应，且迭代间做「早停」（收敛了就不跑满）。
 *     - 整体移出主线程：主线程不再被阻塞任何一毫秒。
 *
 * ★ 串行回退必须用**同一个算法**。
 *   dashboard.js 里保留了无 Worker（file://、老浏览器、Worker 构造失败）
 *   时的同步回退。若回退走旧的全对全实现，两条路径会给出**不同布局**，
 *   于是「有 Worker 和没 Worker 看到的图不一样」——这种分叉极难排查。
 *   因此网格法与自适应迭代数在两边各写一遍**逐字对应**的实现，
 *   并用 starmap_perf_test.go 钉住「两边都有网格法」这个不变式。
 *
 * 坐标约定：输入 positions 是扁平 Float64Array [x0,y0,z0, x1,y1,z1, ...]，
 * 输出同形状。用扁平数组而不是对象数组，是为了让 postMessage 走
 * 结构化克隆时不必遍历几万个对象属性。
 */

/* 每节点预留的面积（单位²），与 dashboard.js 的 maxR 估算一致。 */
var AREA_PER_NODE = 18;
var BASE_R = 15;
/* maxR 上限，防几万节点时半径大到镜头看不到全景。 */
var MAX_R_CAP = 260;

/*
 * 自适应迭代数：小图跑满 50 轮（值得精细），大图退到 6 轮。
 * n=3754 时约 15 轮 —— 旧实现固定 50 轮，且是 O(n²)（3.52 亿次配对）。
 * 下限 14 是观感值：网格法下每轮很便宜（n·k 而非 n²），轮数更多布局更舒展；
 * 再少会让叶节点分布明显不均（母节点周围挤堆）。
 */
function iterationsFor(n) {
	if (n <= 400) return 50;
	var it = Math.floor(60000 / n);
	if (it < 14) it = 14;
	if (it > 50) it = 50;
	return it;
}

/*
 * 早停阈值：取「节点平均间距的 0.2%」。
 * 用相对量而不是绝对值，否则同一个阈值在不同规模的图上含义完全不同。
 */
function maxMoveThreshold(n, maxR) {
	var perimeter = 2 * Math.PI * maxR;
	var gap = n > 0 ? perimeter / n : maxR;
	var th = gap * 0.002;
	var cap = maxR * 2 * 0.001;
	return th < cap ? th : cap;
}

function layout(msg) {
	var n = msg.n | 0;
	var maxR = msg.maxR;
	var nodes = msg.nodes || []; // [{id, mention_count}]，只取权重
	var edges = msg.edges || []; // [[si, ti], ...] 下标对
	var seed = msg.seed | 0;
	var pos = msg.positions; // Float64Array，长度 3n

	if (n === 0) return { positions: pos, iters: 0, converged: true };

	// —— 每节点的归一化权重 mn（0..1），斥力与吸力都按它加权 ——
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

	/*
	 * 确定性 PRNG（mulberry32）。
	 *
	 * 为何不用 Math.random：布局必须**可复现**。同一份图数据两次打开
	 * 应得到同一张图，否则用户会以为记忆被改过，回归判据也没法比对。
	 * 种子由主线程按图指纹传入 ⇒ 同一张图永远同一个种子。
	 */
	function rnd() {
		seed = (seed + 0x6d2b79f5) | 0;
		var t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
		t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
		return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
	}

	// —— 初始摆位：环带 + 叶节点贴近母节点（与串行回退同一几何）——
	var acc = 0;
	for (var c = 0; c < n; c++) {
		var radius = BASE_R + mn[c] * (maxR - BASE_R);
		var baseStep = (Math.PI * 2) / n;
		var extra = mn[c] * baseStep * 2;
		var angle = acc + extra / 2;
		acc += baseStep + extra;
		pos[c * 3] = radius * Math.cos(angle);
		pos[c * 3 + 1] = (rnd() - 0.5) * (10 + mn[c] * 20);
		pos[c * 3 + 2] = radius * Math.sin(angle);
	}

	// 邻接表（叶节点摆位 + 度统计都要用）
	var adj = new Array(n);
	for (var d = 0; d < n; d++) adj[d] = null;
	for (var e = 0; e < edges.length; e++) {
		var s0 = edges[e][0],
			t0 = edges[e][1];
		if (s0 < 0 || t0 < 0 || s0 >= n || t0 >= n || s0 === t0) continue;
		(adj[s0] || (adj[s0] = [])).push(t0);
		(adj[t0] || (adj[t0] = [])).push(s0);
	}
	for (var f = 0; f < n; f++) {
		var nb = adj[f];
		if (!nb || nb.length !== 1) continue;
		var p = nb[0];
		var px = pos[p * 3],
			py = pos[p * 3 + 1],
			pz = pos[p * 3 + 2];
		var off = 6 + mn[f] * 8 + rnd() * 4;
		var a2 = rnd() * Math.PI * 2;
		pos[f * 3] = px + off * Math.cos(a2);
		pos[f * 3 + 1] = py + (rnd() - 0.5) * (4 + mn[f] * 6);
		pos[f * 3 + 2] = pz + off * Math.sin(a2);
	}

	/*
	 * ★ 网格：斥力的关键优化。
	 *
	 * 旧实现每轮对 i<j 全对全求距，但斥力**只在 d<25 时才施加**
	 * （原代码那段 `if (d < 25)`）。既然只关心近邻，就没必要算 3.52 亿次：
	 * 把空间切成边长 == 斥力半径的格子，每点只与「自身格 + 相邻 26 格」
	 * （3×3×3）内的点比较即可。
	 *
	 * 为何格边长必须 == 斥力半径：这样「距离 < 25 的点」必然落在相邻
	 * 26 格内，绝不遗漏 —— 这是敢跳过远处配对的**唯一依据**。
	 * 格边长一旦小于斥力半径，就会漏掉真实邻居，图会塌成一团。
	 *
	 * 复杂度：均匀分布下每格约 1 点 ⇒ 每轮约 27n 次比较。
	 * n=3754、15 轮时约 152 万次，而不是 3.52 亿次。
	 */
	var CUTOFF = 25;
	var grid = new Map();

	function rebuildGrid() {
		grid.clear();
		for (var i = 0; i < n; i++) {
			var key =
				Math.floor(pos[i * 3] / CUTOFF) +
				"|" +
				Math.floor(pos[i * 3 + 1] / CUTOFF) +
				"|" +
				Math.floor(pos[i * 3 + 2] / CUTOFF);
			var bucket = grid.get(key);
			if (!bucket) grid.set(key, (bucket = []));
			bucket.push(i);
		}
	}

	var iters = iterationsFor(n);
	var th = maxMoveThreshold(n, maxR);
	var converged = false;
	var used = 0;

	// 早停用影子副本：与本轮开始时的位置比，看整体是否已经稳定。
	var shadow = new Float64Array(pos.length);
	shadow.set(pos);

	for (var it = 0; it < iters; it++) {
		rebuildGrid();

		// —— 斥力（只算 3×3×3 邻格内的近邻）——
		for (var i1 = 0; i1 < n; i1++) {
			var cxi = Math.floor(pos[i1 * 3] / CUTOFF);
			var cyi = Math.floor(pos[i1 * 3 + 1] / CUTOFF);
			var czi = Math.floor(pos[i1 * 3 + 2] / CUTOFF);
			for (var dx = -1; dx <= 1; dx++) {
				for (var dy = -1; dy <= 1; dy++) {
					for (var dz = -1; dz <= 1; dz++) {
						var bk = grid.get(cxi + dx + "|" + (cyi + dy) + "|" + (czi + dz));
						if (!bk) continue;
						for (var bi = 0; bi < bk.length; bi++) {
							var j = bk[bi];
							// 每对只处理一次：只做 j > i，与旧实现
							// 的 `for j = i+1` 语义一致，避免重复施力。
							if (j <= i1) continue;
							var xi = pos[i1 * 3],
								yi = pos[i1 * 3 + 1],
								zi = pos[i1 * 3 + 2];
							var ex = xi - pos[j * 3];
							var ey = yi - pos[j * 3 + 1];
							var ez = zi - pos[j * 3 + 2];
							var dd = Math.sqrt(ex * ex + ey * ey + ez * ez) + 0.1;
							if (dd >= CUTOFF) continue;
							var rf = 0.5 + (mn[i1] + mn[j]) * 0.5;
							var force = (0.06 * rf) / Math.max(dd, 0.5);
							var fx = (ex / dd) * force,
								fy = (ey / dd) * force,
								fz = (ez / dd) * force;
							pos[i1 * 3] += fx;
							pos[i1 * 3 + 1] += fy;
							pos[i1 * 3 + 2] += fz;
							pos[j * 3] -= fx;
							pos[j * 3 + 1] -= fy;
							pos[j * 3 + 2] -= fz;
						}
					}
				}
			}
		}

		// —— 吸力（沿边，线性）——
		for (var e2 = 0; e2 < edges.length; e2++) {
			var s2 = edges[e2][0],
				t2 = edges[e2][1];
			if (s2 < 0 || t2 < 0 || s2 >= n || t2 >= n || s2 === t2) continue;
			var ax = pos[s2 * 3],
				ay = pos[s2 * 3 + 1],
				az = pos[s2 * 3 + 2];
			var bx = pos[t2 * 3],
				by = pos[t2 * 3 + 1],
				bz = pos[t2 * 3 + 2];
			var gx = bx - ax,
				gy = by - ay,
				gz = bz - az;
			var gd = Math.sqrt(gx * gx + gy * gy + gz * gz) + 0.1;
			if (gd <= 20) continue;
			var af = 0.04 * Math.max(0.3, 1.0 - (mn[s2] + mn[t2]) * 0.3);
			var ux = (gx / gd) * af,
				uy = (gy / gd) * af,
				uz = (gz / gd) * af;
			pos[s2 * 3] += ux;
			pos[s2 * 3 + 1] += uy;
			pos[s2 * 3 + 2] += uz;
			pos[t2 * 3] -= ux;
			pos[t2 * 3 + 1] -= uy;
			pos[t2 * 3 + 2] -= uz;
		}

		// —— 向心约束 + 早停位移统计（一次遍历同时做完）——
		var maxA = maxR * 1.5;
		var maxMove = 0;
		for (var i3 = 0; i3 < n; i3++) {
			var x3 = pos[i3 * 3],
				y3 = pos[i3 * 3 + 1],
				z3 = pos[i3 * 3 + 2];
			var dist = Math.sqrt(x3 * x3 + y3 * y3 + z3 * z3);
			if (dist > maxA) {
				var sc = maxA / dist;
				x3 *= sc;
				y3 *= sc;
				z3 *= sc;
				pos[i3 * 3] = x3;
				pos[i3 * 3 + 1] = y3;
				pos[i3 * 3 + 2] = z3;
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
		shadow.set(pos);
		used = it + 1;
		if (maxMove < th) {
			converged = true;
			break;
		}
	}

	return { positions: pos, iters: used, converged: converged };
}

self.onmessage = function (ev) {
	var msg = ev.data;
	try {
		var out = layout(msg);
		self.postMessage({
			type: "layout",
			seq: msg.seq,
			positions: out.positions,
			iters: out.iters,
			converged: out.converged,
		});
	} catch (err) {
		self.postMessage({
			type: "error",
			seq: msg.seq,
			message: String((err && err.message) || err),
		});
	}
};
