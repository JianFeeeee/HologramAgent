// 星图 GPU 资源生命周期判据（2026-10-05）。
//
// ## 要判的是什么
//
// 实测故障：聊天页长时间挂着之后 GUI 卡死。CDP 抓生产实例（2588 节点）：
//
//	Mesh/SphereGeometry    2588 个   geometry 数据合计 22.67MB
//	Sprite/BufferGeometry  1294 个   label 纹理，256×64 RGBA ≈ 64KB/个
//
// 进程常驻内存反复冲上 GB 级、被 GC 拉回后又再度暴涨，界面随之卡住。
//
// 两个叠加缺陷：
//
//  1. **几何体从不共享**。每个节点都 new THREE.SphereGeometry(r, 16, 12)
//     —— 因为半径各异，看起来「只能各自 new」。但 three.js 里大小应由
//     mesh.scale 表达，几何体应共享。2588 份独立几何体数据。
//
//  2. **重建时不 dispose**。buildChatStarmapGraph 只 starmapScene.remove(m)，
//     从不 dispose()。remove 只改场景树，geometry/material/texture 仍占着
//     WebGL 资源。星图每次全量重建泄漏一整轮，节点数还随记忆增长 ——
//     泄漏只增不减。全代码搜不到 dispose() 就是铁证。
//
// ## 为什么用文本扫描
//
// 真跑需要在 CDP 里连生产后端抓 renderer.info.memory.geometries，
// 依赖 driver/GL 与网络，CI 不可靠。判据钉「该有的共享与释放是否在位」。
//
// 运行：node starmap-gpu-lifecycle.test.mjs

import { readFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { createRequire } from "node:module";

const here = dirname(fileURLToPath(import.meta.url));
const APP = join(here, "renderer", "app.js");

function loadDisposePlan() {
  try {
    return createRequire(import.meta.url)(join(here, "renderer", "dispose-plan.js"));
  } catch (e) {
    return null;
  }
}

let failures = 0;
const check = (name, ok, detail) => {
  if (ok) {
    console.log(`  ✓ ${name}`);
  } else {
    failures++;
    console.log(`  ✗ ${name}${detail ? " — " + detail : ""}`);
  }
};

if (!existsSync(APP)) {
  console.error(`找不到 ${APP}`);
  process.exit(1);
}
const src = readFileSync(APP, "utf8");
// ── 释放决策：**调接口**断言 ────────────────────────────────────────
//
// 决策抽到 `renderer/dispose-plan.js` 的 `planDispose(root, shared)`，
// 返回待释放清单。原判据只能正则匹配「remove 旁边有没有 dispose」——
// 那证明不了三件真正要紧的事：去重、排除共享资源、纹理收齐。
{
  const D = loadDisposePlan();
  check("能加载 dispose-plan.js", D !== null);
  if (D) {
    const g1 = {}, g2 = {}, sharedGeo = {};
    const tex1 = { isTexture: true }, tex2 = { isTexture: true };
    const mat1 = { map: tex1 };
    const mat2 = { map: tex2, bumpMap: tex1 }; // 纹理被两个材质引用
    const tree = {
      traverse: (cb) => {
        cb({ geometry: g1, material: mat1 });
        cb({ geometry: g1, material: mat1 }); // 重复出现同一几何/材质
        cb({ geometry: sharedGeo, material: mat2 }); // 共享几何不得释放
        cb({ geometry: g2, material: [mat1, mat2] });
      },
    };
    const plan = D.planDispose(tree, sharedGeo);
    check(
      "★ 共享几何体不进释放清单",
      plan.geometries.indexOf(sharedGeo) < 0,
      "共享球被 dispose ⇒ 仍在渲染的 mesh 变黑/报错（全场景共用一份）",
    );
    check(
      "★ 同一几何体只释放一次",
      plan.geometries.filter((x) => x === g1).length === 1,
      "重复 dispose 同一几何体 ⇒ 第二次在已释放对象上调 dispose",
    );
    check(
      "★ 同一材质只释放一次",
      plan.materials.filter((x) => x === mat1).length === 1,
      "材质被多个 mesh 共用时只该释放一次",
    );
    check(
      "★ 纹理收齐且去重（含 material 的非 map 字段）",
      plan.textures.length === 2 &&
        plan.textures.indexOf(tex1) >= 0 &&
        plan.textures.indexOf(tex2) >= 0,
      "漏收 bumpMap 等字段上的纹理 ⇒ 只 remove 不 dispose 的泄漏原样存在",
    );
  }
}


// 从函数声明扫到列0 的 }（嵌套闭合都有缩进）。
function sliceFunction(name) {
  const start = src.indexOf(`function ${name}(`);
  if (start < 0) return null;
  const lines = src.slice(start).split("\n");
  const out = [lines[0]];
  for (let i = 1; i < lines.length; i++) {
    out.push(lines[i]);
    if (lines[i] === "}") return out.join("\n");
  }
  return out.join("\n");
}

// ── 1) 球体几何体必须共享 ───────────────────────────────────────────
//
// 判据要盯的是「循环体内不再 new SphereGeometry(...)」——
// 只看文件里有没有 SphereGeometry 会被starmapSphereGeometry 里的
// 那一次正当构造骗过去。

const build = sliceFunction("buildChatStarmapGraph");
check(
  "能抽到 buildChatStarmapGraph",
  !!build,
  "找不到该函数 —— 正则可能已与 app.js 漂移",
);

// 共享几何体入口：建一次、缓存、复用。
const shared = sliceFunction("starmapSphereGeometry");
check(
  "存在共享球体几何体入口",
  !!shared && /SphereGeometry/.test(shared) && /starmapUnitSphere/.test(shared),
  "没有共享几何体入口 —— 每节点一份 SphereGeometry，2588 节点即 2588 份",
);

if (build) {
  check(
    "节点循环用共享几何体",
    /starmapSphereGeometry\(\)/.test(build) &&
      !/new THREE\.SphereGeometry\(\s*rad/.test(build),
    "buildChatStarmapGraph 里仍在 new SphereGeometry(rad, ...) —— 未共享",
  );
  check(
    "光晕球也用共享几何体",
    !/new THREE\.SphereGeometry\(\s*gr/.test(build),
    "光晕球仍各自 new SphereGeometry(gr, ...) —— 1294 份未共享",
  );

  // 共享球的半径必须仍是 0.5：节点缩放基准 baseScale = rad / 0.5
  // 的分母就是它，改成 1 会让所有节点缩放翻倍。
  const r05 = shared && /SphereGeometry\(\s*0\.5\s*,/.test(shared);
  const baseScale05 = build && /baseScale\s*=\s*rad\s*\/\s*0\.5/.test(build);
  check(
    "共享球半径与 baseScale 基准一致（0.5）",
    r05 && baseScale05,
    r05
      ? "baseScale 公式不是 rad/0.5 —— 两边不自洽，节点大小会错"
      : "共享球半径不是 0.5 —— baseScale=rad/0.5 会让所有节点缩放翻倍",
  );

  // ── 2) 重建必须 dispose ──────────────────────────────────────────
  //
  // 只 remove 不 dispose 是泄漏的根。判据要求 remove 旁边有 dispose。
  //
  // ★ 先剥注释再匹配：函数上方的说明块里也写着
  //   `starmapNodeMeshes.forEach` 与 `disposeStarmapObject`（用于描述原
  //   实现的缺陷），裸正则会从注释开始匹配、在 200 字窗口内撞上别处的
  //   disposeStarmapObject 而误判为绿。
  const buildCode = build
    .split("\n")
    .filter((l) => !/^\s*(\/\/|\*|\/\*)/.test(l))
    .join("\n");

  //
  //
  // 窗口必须**小于**「节点 forEach 结束」到「边的 disposeStarmapObject(l)」
  // 之间的距离。实测距离 84 字符（变异后）：
  //   starmapNodeMeshes.forEach(function (m) {\n    starmapScene.remove(m);\n  });\n
  //   starmapEdgeLines.forEach(function (l) {\n    disposeStarmapObject(l);
  // 窗口开到 120/200 时，节点那处换成 remove 之后，**边**的 dispose 仍在
  // 窗口内 ⇒ 误判为绿（实测踩过两次）。
  // 取 40：足够跨过 forEach 的函数头，又够不到下一个 forEach。
  const NODE_WINDOW = 40;
  check(
    "重建时释放旧节点资源",
    new RegExp(
      `starmapNodeMeshes\\.forEach[\\s\\S]{0,${NODE_WINDOW}}?disposeStarmapObject\\s*\\(`,
    ).test(buildCode),
    "starmapNodeMeshes.forEach 里只有 remove —— GPU 资源不释放，每次重建泄漏一轮",
  );
  check(
    "重建时释放旧边资源",
    /starmapEdgeLines\.forEach[\s\S]{0,120}?disposeStarmapObject\s*\(/.test(buildCode),
    "starmapEdgeLines 同样需要 dispose",
  );
}

// ── 3) dispose 决策：**调接口**断言（不再读 disposeStarmapObject 源码）──
//
// 「递归到子节点 / 释放纹理 / 排除共享几何」这三条原是正则匹配源码，
// 现在由 planDispose 的行为判定（见文件顶部那组判据）。
// 这里只保留"入口存在"与"label 纹理有释放路径"两条结构性检查。
//
// ★ 判定标准：`planDispose` 收一棵带子节点的假树就能验这三件事，
//   而正则只能验"源码里出现过 traverse / isTexture / starmapUnitSphere"。
const disposeFn = sliceFunction("disposeStarmapObject");
check(
  "存在 disposeStarmapObject（执行副作用的入口）",
  !!disposeFn,
  "找不到释放函数",
);
check(
  "disposeStarmapObject 走 planDispose（决策与副作用分离）",
  disposeFn ? /DisposePlan\.planDispose\(/.test(disposeFn) : false,
  "释放入口没有走抽出的决策模块 —— 判据钉的行为与实际执行的可能漂移",
);

// ── 4) label 纹理数量应有上限感知的注释 ─────────────────────────────
//
// label 是 CanvasTexture，节点越多纹理越多；与节点同阶增长，
// 这次不单独限流（共享/释放已解决主要泄漏），但要留下判据位置，
// 避免将来有人再加节点类型时无人察觉。

check(
  "label 纹理有 dispose 覆盖",
  /new THREE\.CanvasTexture/.test(src) && !!disposeFn,
  "存在 CanvasTexture 但无释放路径",
);

// ── 汇总 ────────────────────────────────────────────────────────────

console.log("");
// 计数要排除注释：本文件在 starmapSphereGeometry 上方有一段注释
// 引用了 `new THREE.SphereGeometry(gr, 16, 12)` 来描述**原**实现，
// 裸grep 会把它算进去，显示 2 次而真实只有 1 次 —— 一个会误导人的摘要
// 比没有摘要更糟。
const codeOnly = src
  .split("\n")
  .filter((l) => !/^\s*(\/\/|\*|\/\*)/.test(l))
  .join("\n");
const nSphere = (codeOnly.match(/new THREE\.SphereGeometry/g) || []).length;
console.log(`  代码中 new SphereGeometry ${nSphere} 次（应为 1：共享入口那一次；注释里的引用已排除）`);
check(
  "代码里只有一处 SphereGeometry 构造",
  nSphere === 1,
  `代码中出现 ${nSphere} 处 —— 某处又各自 new 了几何体`,
);
console.log("");
if (failures > 0) {
  console.log(`失败 ${failures} 条`);
  process.exit(1);
}
console.log("全部通过");