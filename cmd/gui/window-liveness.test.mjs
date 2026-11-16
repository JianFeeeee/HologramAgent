// 窗口/renderer 存活性的行为判据（2026-10-05）。
//
// ## 要判的是什么
//
// 实测故障：GUI 在后台挂很久（数小时）后，从托盘打开是**白屏**，
// 且不会自愈 —— 重启进程才恢复。
//
// 原因链（三个洞叠加）：
//
//  1. **没有任何崩溃监听**。全代码搜不到 render-process-gone /
//     unresponsive / child-process-gone。renderer 被 Chromium 回收
//     （GPU context lost，或渲染进程被判hung 杀掉）后，没有任何代码
//     感知这件事。
//
//  2. **窗口「可见」不等于「有内容」**。renderer 挂掉后窗口句柄还在、
//     isVisible() 仍返回 true —— 而 showMainWindow() 原来只判
//     `if (!isVisible()) show()`，于是 show() 被跳过，从托盘打开就是白的。
//
//  3. **ready-to-show 用了 once**。once 只等第一次首帧；窗口被系统回收后
//     重建、或 GPU 重启后重新 load，那些路径都需要再show 一次，
//     once 让它们静默失败。
//
// 仓库历史上有过「白屏修复(惰性Tray)」提交，但那只修了**首次**显示
// 时机，没管**长期后台后**。本判据盯后者。
//
// ## 为什么用文本扫描而非真跑
//
// 复现需要「挂后台数小时 + renderer 被回收」，构造那个时序要真Electron
// 且依赖驱动行为，CI 里不可靠。故从源码判「该有的监听与判据是否在位」——
// 与 endpoint-align / device-bridge-liveness 同路子。真正需要证明
// 「重载后能出内容」时，靠 device-bridge-liveness 那类跑真实函数的判据。
//
// 运行：node window-liveness.test.mjs

import { readFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { createRequire } from "node:module";

const here = dirname(fileURLToPath(import.meta.url));
const MAIN = join(here, "main.js");

function loadWindowShow() {
  try {
    return createRequire(import.meta.url)(join(here, "renderer", "window-show.js"));
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

if (!existsSync(MAIN)) {
  console.error(`找不到 main.js：${MAIN}`);
  process.exit(1);
}
const src = readFileSync(MAIN, "utf8");

// ── 1) renderer 崩溃监听必须存在 ───────────────────────────────────
//
// 没有它 ⇒ renderer 挂掉全程无感知，窗口留在白屏态。

check(
  "监听 render-process-gone",
  /webContents\.on\(\s*["']render-process-gone["']/.test(src),
  "没有 renderer 崩溃监听 —— 渲染进程挂掉时窗口会永久白屏",
);

const goneHandler = src.match(
  /webContents\.on\(\s*["']render-process-gone["'][\s\S]{0,900}?\n {2}\}\);/,
);
check(
  "renderer 崩溃后有恢复动作",
  goneHandler ? /reload\(\)/.test(goneHandler[0]) : false,
  goneHandler
    ? "崩溃分支里没有 reload —— 监听到了也不自愈"
    : "找不到 render-process-gone 处理块",
);

// OOM 时必须**不**reload：重建只会立刻再崩一次，反复重启会把机器拖死。
check(
  "OOM 崩溃不自动重载",
  goneHandler ? /oom/.test(goneHandler[0]) : false,
  "未区分 reason==='oom' —— 内存不足时反复 reload 会打爆机器",
);

// ── 2) GPU 崩溃监听 ────────────────────────────────────────────────
//
// 无边框窗口（frame:false）在 GPU 进程重启后，常见「停在上一帧不动」
// 或整片白 —— 与 renderer 崩溃是不同现象，但表现重合。

check(
  "监听 child-process-gone 且覆盖 GPU",
  /app\.on\(\s*["']child-process-gone["']/.test(src) &&
    /type\s*!==\s*["']GPU["']\s*\)?\s*return/.test(src),
  "没有 GPU 崩溃恢复 —— 无边框窗口在 GPU 重启后可能停在空白帧",
);

// ── 3) 显示路径必须查 renderer 健康 ───────────────────────────────
//
// 这是「白屏」被直接看见的地方：只判 isVisible() 会跳过 show()。

// showMainWindow 的源码切片。
//
// ★ 不用 /function showMainWindow\(\)[\s\S]*?\n}\n/ —— 函数体内有多处
//   嵌套闭合（try/catch/if），非贪婪匹配会在中途的 `\n}\n` 就停，
//   切片里根本不含函数尾部那几行。实测：删掉 isCrashed 判定后，
//   「showMainWindow 检查 isCrashed」仍显示绿 —— 切片里还有别的
//   isCrashed 残留，而真正的判定已在切片外。
//   改为：从函数声明行扫到**列 0** 的 `}`（顶层函数结束）。
function sliceFunction(name) {
  const start = src.indexOf(`function ${name}(`);
  if (start < 0) return null;
  const lines = src.slice(start).split("\n");
  const out = [lines[0]];
  for (let i = 1; i < lines.length; i++) {
    out.push(lines[i]);
    // 列 0 的右花括号 = 顶层函数体结束（嵌套闭合都有缩进）。
    if (lines[i] === "}") return out.join("\n");
  }
  return out.join("\n");
}

// ── 3) 「唤起窗口」的决策：**调接口**断言 ─────────────────────────
//
// 决策已抽到 `renderer/window-show.js` 的 `decideShow(state)`。
// 原判据正则匹配 showMainWindow 切片里有没有 `if (wc.isCrashed())` 与
// 紧跟的 `reload()` —— 那只能证明"源码里出现过这两个词"。
//
// 现在喂状态、断言动作：
{
  const W = loadWindowShow();
  check("能加载 window-show.js", W !== null);
  if (W) {
    const d = W.decideShow;

    const crashed = d({ exists: true, crashed: true, visible: true });
    check(
      "★ renderer 崩溃时必须先 reload 再 show",
      crashed.reload === true && crashed.show === true,
      "崩溃了只 show 不 reload ⇒ 窗口句柄还在、isVisible() 仍为 true，" +
        "但内容是空的 ⇒ 托盘打开就是白屏且**永远不会自愈**（2026-10-05 实测故障）",
    );

    const loading = d({ exists: true, crashed: true, loading: true, visible: true });
    check(
      "★ 加载中不 reload（会把正在进行的加载推倒重来）",
      loading.reload === false,
      "isLoading 时 reload ⇒ 白等一轮，用户看到的是长时间空白",
    );

    const gone = d({ exists: false });
    check(
      "★ 窗口不存在时重建",
      gone.action === "create",
      "只 show 不创建 ⇒ 托盘菜单点了没反应，进程成唤不起来的僵尸",
    );

    const hidden = d({ exists: true, visible: false });
    check(
      "隐藏窗口要 show",
      hidden.show === true,
      "静默创建的窗口可能无法被托盘唤起",
    );

    const visible = d({ exists: true, visible: true });
    check(
      "已可见时只 focus（不重复 show）",
      visible.show === false && visible.focus === true,
      "重复 show 会触发不必要的重绘/焦点抖动",
    );
  }
}

// ── 4) ready-to-show 不能是 once ──────────────────────────────────
//
// once 只等第一次首帧；窗口重建 / GPU 重启后的再显示路径全靠它。

check(
  "ready-to-show 用 on 而非 once",
  /mainWindow\.on\(\s*["']ready-to-show["']/.test(src) &&
    !/mainWindow\.once\(\s*["']ready-to-show["']/.test(src),
  "仍是 once —— 窗口重建 / GPU 重启后再显示会静默失败",
);

// ── 5) 所有「显示窗口」的入口都应走 showMainWindow ──────────────────
//
// 通知点击、托盘菜单若自己调 show()，就绕过了上面的健康检查。

const directShows = [
  ...src.matchAll(/mainWindow\.show\(\)/g),
].length;
const showMainCalls = [...src.matchAll(/showMainWindow\(\)/g)].length;
check(
  "无绕过健康检查的直接 show",
  directShows <= 2, // createWindow 的 ready-to-show + showMainWindow 内部各一处
  `发现 ${directShows} 处 mainWindow.show()；除 createWindow 与 showMainWindow 自身外，` +
    "其余入口（如通知点击）应改走 showMainWindow()，否则会开出白窗口",
);
check(
  "通知点击走 showMainWindow",
  /n\.on\(["']click["'][\s\S]{0,300}?showMainWindow\(\)/.test(src),
  "通知点击仍直接 show() —— 绕过 renderer 健康检查",
);

// ── 汇总 ────────────────────────────────────────────────────────────

console.log("");
console.log(`  show() 调用 ${directShows} 处 / showMainWindow() 调用 ${showMainCalls} 处`);
console.log("");
if (failures > 0) {
  console.log(`失败 ${failures} 条`);
  process.exit(1);
}
console.log("全部通过");