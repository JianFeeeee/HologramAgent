// 静默启动的判据。
//
// ## 要判的是什么
//
// prefs.silentStart 曾是**死设置**：设置页有开关、gui-prefs.json 存了它、
// 文案写着「启动时不显示主窗口，驻留托盘后台运行」，但主进程从来没读过它
// —— whenReady 无条件 createWindow()，于是必然弹窗。
//
// 实测（本机，prefs.silentStart=true）：窗口标题 HologramAgent 照样出现。
//
// 本判据钉住三件事：
//   1. whenReady 真的读 silentStart 并据此决定 show
//   2. 静默时窗口**仍被创建**（不是"不建窗"）—— 否则渲染进程不启动，
//      SSE/设备桥/聊天全废，且 showMainWindow() 只做 show() 不会创建，
//      托盘菜单点了没反应，进程变成唤不起的僵尸
//   3. loadGuiPrefs 去掉 UTF-8 BOM —— 否则 JSON.parse 抛错 → 返回默认
//      prefs → 所有偏好静默失效（Windows 工具改过文件就会踩到）
//
// 运行：node silent-start.test.mjs

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, "main.js"), "utf8");

let failures = 0;
const check = (name, ok, detail) => {
  if (ok) console.log(`  ✓ ${name}`);
  else {
    failures++;
    console.log(`  ✗ ${name}${detail ? " — " + detail : ""}`);
  }
};

// ── 1) whenReady 必须消费 silentStart ─────────────────────────────────
const readyIdx = src.indexOf("app.whenReady().then");
check("能定位 whenReady", readyIdx > 0);
const readyBlock = src.slice(readyIdx, src.indexOf("app.on(\"before-quit\"", readyIdx));

check(
  "whenReady 读取 prefs.silentStart",
  /loadGuiPrefs\(\)[\s\S]{0,40}silentStart|\.silentStart/.test(readyBlock),
  "whenReady 里没有读 silentStart ⇒ 开关是死的",
);
check(
  "whenReady 按 silent 决定是否显示",
  /createWindow\(\s*!silent\s*\)/.test(readyBlock),
  "没有 createWindow(!silent) ⇒ 无条件弹窗",
);

// ── 2) 静默必须「创建但隐藏」，不是不创建 ─────────────────────────────
const createIdx = src.indexOf("function createWindow(");
check("能定位 createWindow", createIdx > 0);
const createBlock = src.slice(createIdx, src.indexOf("\nfunction ", createIdx + 10));

check(
  "createWindow 支持 show 参数",
  /function createWindow\(\s*show\s*=\s*true\s*\)/.test(createBlock),
  "签名不是 createWindow(show = true)",
);
check(
  "BrowserWindow 初始 show:false（避免闪一下再隐藏）",
  /show:\s*false/.test(createBlock),
  "没用 show:false ⇒ 先显示再隐藏会闪",
);
check(
  "非静默时经 ready-to-show 显示",
  // ★ 2026-10-06：原来写死 `{0,120}` 字符窗口，被新增的**注释**挤爆而假红。
  //
  // 实情：main.js 在 ready-to-show 与 show() 之间夹了 558 字符 —— 其中
  // 大部分是解释「为什么用 on 而非 once」的注释（7bd569ec 白屏修复）。
  // 实现是对的，判据的字节窗口太小。
  //
  // ⇒ 判据该断的是**结构关系**（ready-to-show 处理器的回调里有 show()），
  //   不是两者相隔多少字符 —— 后者会随注释/排版无意义地红。
  //   去掉行注释后再量，且把窗口放到足够容纳该回调体。
  (() => {
    const noLineComments = createBlock.replace(/\/\/[^\n]*/g, "");
    return /ready-to-show[\s\S]{0,400}show\(\)/.test(noLineComments);
  })(),
  "没有 ready-to-show → show()，非静默启动可能白屏闪现",
);
check(
  "★ 静默路径仍然创建窗口（渲染进程要启动）",
  /createWindow\(\s*!silent\s*\)/.test(readyBlock) &&
    /function createWindow\(\s*show\s*=\s*true\s*\)/.test(createBlock),
  "静默时若不建窗口，SSE/设备桥/聊天全废",
);

// ── 3) showMainWindow 必须能在窗口缺失时重建 ──────────────────────────
const showIdx = src.indexOf("function showMainWindow(");
check("能定位 showMainWindow", showIdx > 0);
const showBlock = src.slice(showIdx, src.indexOf("\nfunction ", showIdx + 10));

check(
  "★ showMainWindow 在窗口不存在时会重建",
  /createWindow\(\s*true\s*\)/.test(showBlock),
  "只调 show() 不创建 ⇒ 托盘菜单点了没反应，进程成僵尸（activate 事件仅 macOS 触发）",
);
check(
  "showMainWindow 对隐藏窗口调 show()",
  /isVisible\(\)[\s\S]{0,60}show\(\)/.test(showBlock),
  "未处理 isVisible() ⇒ 静默创建的窗口可能无法被唤起",
);

// ── 4) loadGuiPrefs 必须去 BOM ────────────────────────────────────────
const prefsIdx = src.indexOf("function loadGuiPrefs(");
check("能定位 loadGuiPrefs", prefsIdx > 0);
const prefsBlock = src.slice(prefsIdx, src.indexOf("\nfunction ", prefsIdx + 10));

check(
  "★ loadGuiPrefs 读取时去 UTF-8 BOM",
  /readFileSync\(GUI_PREFS_FILE[\s\S]{0,80}replace\(\s*\/\^\\uFEFF\//.test(prefsBlock),
  "未去 BOM ⇒ Windows 工具(PowerShell Set-Content -Encoding UTF8/记事本)改过文件后，" +
    "JSON.parse 抛错 → 返回默认 prefs → 所有偏好静默失效",
);

// ── 5) loadConnections 的 BOM 处理不能被回退 ─────────────────────────
const connIdx = src.indexOf("function loadConnections(");
const connBlock = src.slice(connIdx, src.indexOf("\nfunction ", connIdx + 10));
check(
  "loadConnections 仍去 BOM（既有行为不回退）",
  /replace\(\s*\/\^\\uFEFF\//.test(connBlock),
  "loadConnections 的 BOM 处理被移除了",
);

console.log("");
if (failures > 0) {
  console.log(`全部失败：${failures} 条`);
  process.exit(1);
}
console.log("全部通过");