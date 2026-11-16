// 加载动画 + 系统通知的判据。
//
// ## 要判的是什么
//
// 1) 加载动画：旧实现是 border + rotate 的 spinner（.loading /
//    .loading-spinner / .live-spinner 三处），在消息气泡里刺眼，
//    且"持续旋转"语义不对 —— 旋转暗示在加载某个确定的东西，
//    而 agent 思考本身没有进度。改为 PiDeck 用的「等距三点 +
//    透明度递减 + 轻微上浮」。
//
// 2) 系统通知：**此前完全不存在**（主进程无 Notification、preload 无接口、
//    渲染层只有页面内 toast）。这是新增能力，容易被后续改动静默删掉。
//
// 运行：node loading-notify.test.mjs

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { createRequire } from "node:module";

const here = dirname(fileURLToPath(import.meta.url));
const css = readFileSync(join(here, "renderer", "style.css"), "utf8");
const app = readFileSync(join(here, "renderer", "app.js"), "utf8");
const main = readFileSync(join(here, "main.js"), "utf8");
const preload = readFileSync(join(here, "preload.js"), "utf8");

function loadNotifyOnce() {
  try {
    return createRequire(import.meta.url)(join(here, "renderer", "notify-once.js"));
  } catch (e) {
    return null;
  }
}

let failures = 0;
const check = (name, ok, detail) => {
  if (ok) console.log(`  ✓ ${name}`);
  else {
    failures++;
    console.log(`  ✗ ${name}${detail ? " — " + detail : ""}`);
  }
};

// ── 1) 加载动画：三点而非旋转 ────────────────────────────────────────
check("定义 haDots 关键帧", /@keyframes haDots/.test(css), "缺 @keyframes haDots");
check("定义 haDotFloat 关键帧", /@keyframes haDotFloat/.test(css));
check("定义 haSweep 关键帧", /@keyframes haSweep/.test(css));

// 三点的错峰 delay 是"追逐感"的来源，缺了就变成同步闪烁
check(
  "三点有错峰 delay（nth-child 2/3）",
  /\.ha-dots\s*>?\s*i:nth-child\(2\)/.test(css) ||
    /\.ha-dots > i:nth-child\(2\)/.test(css),
  "缺 nth-child delay ⇒ 三点同步闪，不像追逐",
);

// 旧类名必须仍然可用（还有调用点用它们）
check(
  ".loading 别名仍指向新动画",
  /\.loading,\s*\.loading-spinner\s*{[^}]*animation:\s*none/.test(css),
  "别名没解除旋转 ⇒ 旧调用点仍是转圈",
);
check(
  "live-spinner 已是 flex 三点容器",
  /\.msg-bubble \.live-spinner\s*{[^}]*display:\s*inline-flex/.test(css),
  "live-spinner 仍是 inline-block 圆环",
);

// 旧的旋转实现不应再被这三类使用
const usesOldSpin = /\.loading[^{]*\{[^}]*border-top-color/.test(css);
check("没有残留的 border 旋转 spinner", !usesOldSpin, "仍有 border-top-color 旋转实现");

// ── 2) 调用点都换成了三点结构 ──────────────────────────────────────
const dotMarkup = (app.match(/class="ha-dots[^"]*"><i><\/i><i><\/i><i><\/i>/g) || []).length;
check(
  "★ 面板/列表加载已换成三点结构",
  dotMarkup >= 4,
  `只找到 ${dotMarkup} 处 <i>x3 结构（预期 >=4：星图 2 + 列表 3 + 其他）`,
);
check(
  "★ 消息内 live-spinner 带三个点",
  /class="live-spinner"><i><\/i><i><\/i><i><\/i>/.test(app),
  "live-spinner 仍是空的（会看不到任何点）",
);
check(
  "旧的空 <div class=\"loading\"></div> 已清理",
  !/class="loading"><\/div>/.test(app),
  "仍有裸 <div class=loading>（无点）",
);

// ── 3) 系统通知：主进程 ────────────────────────────────────────────
check("main 引入 Notification", /new Notification\(/.test(main), "未调用原生 Notification");
check("有 notify:show IPC", /ipcMain\.handle\("notify:show"/.test(main));
check("有 notify:supported IPC", /ipcMain\.handle\("notify:supported"/.test(main));
check(
  "★ 前台聚焦时不打扰用户",
  /isFocused\(\)/.test(main) && /skipped:\s*"focused"/.test(main),
  "没有「窗口在前台就不弹」判定 ⇒ 正看着界面时会被弹窗打断",
);
check(
  "★ 点击通知会回传并唤起窗口",
  /notify:clicked/.test(main) && /mainWindow\.show\(\)/.test(main),
  "点击通知后没有 show()+回传，点了等于没点",
);
check(
  "正文超长截断（避免通知被撑爆）",
  /body\.length\s*>\s*\d+/.test(main),
  "未截断正文",
);

// ── 4) preload 桥 ──────────────────────────────────────────────────
check("preload 暴露 notify.show", /notify:\s*{[\s\S]{0,200}show:/.test(preload));
check("preload 暴露 notify.supported", /supported:\s*\(\)\s*=>\s*ipcRenderer\.invoke\("notify:supported"/.test(preload));
check("preload 暴露 notify.onClicked", /onClicked:/.test(preload));

// ── 5) 渲染层 ──────────────────────────────────────────────────────
// ── 去重决策：**调接口**断言 ───────────────────────────────────────
//
// 逻辑已抽到 `renderer/notify-once.js` 的 `decideNotify(prev, sig)`：
// 返回 {state, notify}。语义是「同一轮只弹一次」。
//
// 原判据只匹配 `function notifyTurnOnce(` 是否存在 —— 那不能证明去重有效
// （函数在、逻辑错，照样每段输出弹一次）。
{
  const N = loadNotifyOnce();
  check("能加载 notify-once.js", N !== null);
  if (N) {
    const d = N.decideNotify;
    const first = d(null, "turn-1");
    check("★ 首轮应当通知", first.notify === true, "首轮不通知 ⇒ 系统通知完全不工作");
    const again = d(first.state, "turn-1");
    check(
      "★ 同一轮第二次不通知",
      again.notify === false,
      "同轮重复通知 ⇒ 一轮输出几段就弹几次（实测被用户投诉的形态）",
    );
    const next = d(again.state, "turn-2");
    check(
      "★ 新一轮恢复通知",
      next.notify === true,
      "新一轮不通知 ⇒ 去重把后续所有轮次都吞了",
    );
    // 幂等：同一 state 连续判多次，只有第一次为真
    let st = d(null, "turn-3").state;
    let count = 0;
    for (let k = 0; k < 5; k++) {
      const r = d(st, "turn-3");
      if (r.notify) count++;
      st = r.state;
    }
    check("同一轮连判 5 次只通知 0 次（已通知过）", count === 0, `仍通知了 ${count} 次`);
  }
}

check("注册了通知点击跳转", /onNotifyClicked/.test(app) && /onClicked\(onNotifyClicked\)/.test(app));
check("启动时探测通知可用性", /notify\.supported\(\)/.test(app), "未探测 Notification.isSupported");

console.log("");
if (failures > 0) {
  console.log(`全部失败：${failures} 条`);
  process.exit(1);
}
console.log("全部通过");