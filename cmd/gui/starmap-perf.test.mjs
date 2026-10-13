// 星图性能判据 —— 钉住 2026-10-13 的两轮优化。
//
// ## 为什么需要这一组（真实现象，不是理论）
//
// `cmd/gui/renderer/app.js` 与 `internal/plugins/webui/dashboard.js` 的星图
// 代码是**两份手抄的副本**（当年 diff 为 0）。WebUI 先修了性能，GUI 因为
// 是独立文件**完全没跟着变** ⇒ 用户换 GUI 客户端时同样的卡顿原样存在。
//
// 更糟的是「照抄会抄错」：本轮把 WebUI 的空闲帧跳过抄进 GUI 时，直接抄了
// `starmapMorphSet` —— 那是 **WebUI 独有的变量**，GUI 用的是
// `state.starmapGrown`。结果是渲染循环**每帧 ReferenceError、星图 0 帧**
// （实测 frame 恒为 1）。这类错误在语法检查里看不出来（JS 不校验未声明
// 标识符），必须靠运行或静态判据抓。
//
// ## 判据取「结构性事实」而不是性能数字
//
// 跑真 Electron 量毫秒数需要后端 + 生产图数据，不适合做单测；而这几处
// 缺陷都有确定的结构特征（全对全双层循环 / 每节点一个光晕 mesh /
// 每边一个 Line / 无空闲跳过），源码静态可判且回归立刻红。
//
// 运行：node cmd/gui/starmap-perf.test.mjs

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const APP = join(here, "renderer", "app.js");
const src = readFileSync(APP, "utf8");

// 取一个顶层函数的函数体（到下一个行首 `}` 为止）。
function funcBody(name) {
  const marker = "function " + name + "(";
  const i = src.indexOf(marker);
  if (i < 0) return null;
  const rest = src.slice(i);
  const end = rest.indexOf("\n}\n");
  return end < 0 ? rest : rest.slice(0, end);
}

let failures = 0;
const check = (name, ok, detail) => {
  if (ok) console.log(`  ✓ ${name}`);
  else {
    failures++;
    console.log(`  ✗ ${name}${detail ? " — " + detail : ""}`);
  }
};

console.log("星图性能判据（GUI）：");

// ── ① 力导向布局必须是网格近邻（O(n·k)），不得再是全对全（O(n²)）──
//
// 旧实现 `for (it<50) { for (i) { for (j=i+1) } }`：生产库 3754 块
// = 3.52 亿次配对，Electron 实测单次建图阻塞 1.8–9 秒。
{
  const relax = funcBody("starmapLayoutRelax");
  check("存在 starmapLayoutRelax（网格近邻实现）", relax !== null);
  if (relax) {
    check(
      "starmapLayoutRelax 走空间网格（grid + CUTOFF）",
      /\bgrid\b/.test(relax) && /\bCUTOFF\b/.test(relax),
      "缺网格：全对全斥力在生产库实测阻塞数秒",
    );
    check(
      "starmapLayoutRelax 不再有 Object.keys(pos) 全量遍历",
      !/Object\.keys/.test(relax),
      "Object.keys 全量遍历是 O(n²) 写法的特征",
    );
  }
  check(
    "存在迭代数自适应 starmapLayoutIterationsFor",
    src.includes("function starmapLayoutIterationsFor("),
    "固定 50 轮在大图上绝大多数轮次是在重复收敛后的状态",
  );
  // 全对全的字符特征：`for (var it = 0; it < 50` 配 `for (var j = i + 1`
  check(
    "已无「50 轮 × 全对全」双层循环",
    !/for \(var it = 0; it < 50; it\+\+\)[\s\S]{0,900}?for \(var j = i \+ 1/.test(src),
    "这正是被替换掉的 O(n²) 布局",
  );
}

// ── ② 光晕必须合并成 InstancedMesh（每节点一个 mesh = 1294 draw call）──
{
  check(
    "存在 starmapBuildGlowInstances（光晕合并）",
    src.includes("function starmapBuildGlowInstances("),
  );
  const build = funcBody("buildChatStarmapGraph");
  check("buildChatStarmapGraph 调用了 starmapBuildGlowInstances", !!build && build.includes("starmapBuildGlowInstances("));
  check(
    "光晕不再逐个 add 到 mesh 上",
    !/mesh\.add\(gs\)/.test(src),
    "每节点一个光晕子 mesh 会让 draw call 翻倍",
  );
  check(
    "光晕 InstancedMesh 用的是共享单位球",
    /new THREE\.InstancedMesh\(/.test(src) && /starmapSphereGeometry\(\)/.test(src),
    "实例化必须复用共享几何，否则几何体数量没省下来",
  );
}

// ── ③ 边必须按颜色合并成 LineSegments（每边一个 Line = 966 draw call）──
{
  const build = funcBody("buildChatStarmapGraph");
  check(
    "边走 LineSegments 合并",
    !!build && build.includes("new THREE.LineSegments("),
    "每边一个 THREE.Line 是 966 次 draw call；合并后为关系类型数",
  );
  check(
    "边不再逐条 new THREE.Line(",
    !/new THREE\.Line\(geo, mat\)/.test(src),
    "逐条 Line 是被替换掉的写法",
  );
}

// ── ④ 空闲帧跳过 + 星空时间驱动 ──
{
  const anim = funcBody("starmapAnimate");
  check("starmapAnimate 存在", anim !== null);
  if (anim) {
    check(
      "空闲帧跳过（SM_IDLE_RENDER_MS）",
      anim.includes("SM_IDLE_RENDER_MS"),
      "图形静止时按 12fps 重绘 1800+ draw call 是纯浪费",
    );
    check(
      "camKey 变化时立即重绘（交互不受节流影响）",
      /camKey !== starmapLastRenderKey/.test(anim),
      "若无论如何都受间隔限制，拖动会变粘",
    );
    check(
      "星空按真实 dt 推进（而非每帧固定角度）",
      /dtSec/.test(anim) && !/rotation\.y \+= 0\.0001;/.test(anim),
      "每帧固定角度 ⇒ 必须按帧重绘，无法降频",
    );
    // ★★ 相机守卫：switchChatPanel 里 renderChatStarmap() 是异步建图，
    //    但紧接着就同步调 starmapAnimate()，那一瞬间 starmapCam 仍是 null。
    check(
      "camKey 之前有 starmapCam/starmapCtrl 守卫",
      /if \(!starmapRen \|\| !starmapCam \|\| !starmapCtrl\) return;/.test(anim),
      "无守卫时读 starmapCam.position 会抛 TypeError（实测报在 switchChatPanel 栈上）",
    );
  }
  // ★★ 跨仓抄错的那一处：GUI 没有 starmapMorphSet。
  check(
    "不引用 WebUI 独有的 starmapMorphSet",
    !/starmapMorphSet\s*[?)]/.test(src),
    "GUI 用 state.starmapGrown；抄 WebUI 的 starmapMorphSet 会让每帧 ReferenceError（星图 0 帧，实测）",
  );
  check(
    "空闲跳过用的是 GUI 自己的活跃集 state.starmapGrown",
    /grownCount/.test(src) || /state\.starmapGrown/.test(src),
  );
  check("SM_IDLE_RENDER_MS 已定义", /var SM_IDLE_RENDER_MS\s*=/.test(src));
}

// ── ⑤ 布局代码必须与 WebUI 同源（shared/starmap/layout.js）──
//
// 为何单独钉这一条：这份布局曾在 GUI 与 WebUI 各存一份手抄，两次真实事故
// 都出自这个分叉（WebUI 换网格法 GUI 没跟上；反向抄时抄了 WebUI 独有的
// starmapMorphSet ⇒ GUI 每帧 ReferenceError、星图 0 帧）。
// 现在唯一事实源在 shared/starmap/layout.js，由 tools/starmap-sync.mjs
// 写进本文件的标记块。这里校验「本文件里的块 == 共享源」。
{
  const BEGIN = "// ===== BEGIN shared:starmap/layout =====";
  const END = "// ===== END shared:starmap/layout =====";
  const b = src.indexOf(BEGIN);
  const e = src.indexOf(END);
  check("app.js 含共享块标记", b >= 0 && e > b);
  if (b >= 0 && e > b) {
    const block = src.slice(b, e + END.length);
    const shared = readFileSync(
      join(here, "..", "..", "shared", "starmap", "layout.js"),
      "utf8",
    ).replace(/\r\n/g, "\n");
    check(
      "共享块与 shared/starmap/layout.js 逐字节一致（无漂移）",
      block.includes(shared.trim()),
      "修法：node tools/starmap-sync.mjs（改算法请只改共享源）",
    );
    check(
      "共享块内带「由生成器产出」提示",
      /生成/.test(block) || /generated/.test(block),
      "避免后人当普通代码手改",
    );
  }
}

// ── ⑥ 首屏不得为隐藏的星图面板建图 ──
//
// GUI 默认视图是 chat，而 renderAll 在 chat 视图会调 renderChatStarmap()。
// 无守卫时每次启动都为「看不见的」星图面板付 943ms 建图 + 433ms 纹理上传。
{
  const rcs = funcBody("renderChatStarmap");
  check("renderChatStarmap 存在", rcs !== null);
  if (rcs) {
    // ★★ 必须先剔除注释再判！
    //    本判据第一版写的是 `body.includes("offsetParent")`，而**注释里
    //    恰好写了这个词**（解释为何要加守卫），于是把真正的守卫代码删掉、
    //    只留注释，判据仍然通过 —— 变异测试当场拆穿过这个假绿。
    //    教训：凡「某标识符是否出现在函数体」类判据，都要先去注释，
    //    否则注释会变成判据的挡箭牌。
    const code = rcs
      .replace(/\/\*[\s\S]*?\*\//g, "")
      .replace(/(^|[^:])\/\/[^\n]*/g, "$1");
    check(
      "renderChatStarmap 有可见性守卫（代码中真读 offsetParent，不只写在注释里）",
      /\.offsetParent/.test(code),
      "否则首屏（chat 视图）就会为隐藏的星图面板建图，白付 1.4 秒",
    );
    check(
      "可见性守卫在 loadChatStarmapData 之前（真能拦住）",
      code.indexOf(".offsetParent") < code.indexOf("loadChatStarmapData()"),
      "守卫写在拉数据之后等于没拦",
    );
  }
}

console.log(
  failures === 0 ? "\n全部通过" : `\n${failures} 项未通过`,
);
process.exit(failures === 0 ? 0 : 1);
