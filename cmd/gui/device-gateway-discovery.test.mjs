// 设备桥 gateway 自动发现链路的门禁。
//
// ## 要判的是什么
//
// 2026-10-05 实测故障：GUI 面板里能看到设备通道地址，但设备桥连不上。
// 日志只有一行 `ws cookie len=0` + 401，没有任何「地址来自发现」的痕迹。
//
// 根因不是配置错，是**发现结果与实际连接配置从不联通**：
//
//   · 渲染进程 app.js 的 loadDiscoveredGateway() 调 /device/gateway，
//     拿到的地址只写进 **state.discoveredGateway**（UI 展示用）；
//   · 主进程 startDeviceBridge() 读的是 **gui-prefs.deviceBridge.gateway**；
//   · 两边从未交换过 —— state 是渲染进程内存，prefs 是主进程文件。
//
// 于是服务端明明已经通过 /device/gateway 告诉了客户端正确地址
// （proxy.go:1078 生成的 url_portal），GUI 却仍在用用户几个月前
// 手填的旧地址，且**永远 401/ECONNREFUSED，且不重试**。
//
// ## 判据形态：文本扫描而非运行时
//
// 与 endpoint-align / device-align 同路子。自动回写的触发点在
// loadDiscoveredGateway() 内，由 refreshAll 的 15s 定时器驱动 ——
// 构造那个时序需要真Electron + 真后端，而 /device/gateway 还要求
// 会话 cookie（CI 里没有）。故从源码文本判「链路是否接通」。
//
// 运行：node device-gateway-discovery.test.mjs

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { createRequire } from "node:module";

const here = dirname(fileURLToPath(import.meta.url));
const APP = join(here, "renderer", "app.js");
const MAIN = join(here, "main.js");
const PRELOAD = join(here, "preload.js");

let failures = 0;
const check = (name, ok, detail) => {
  if (ok) {
    console.log(`  ✓ ${name}`);
  } else {
    failures++;
    console.log(`  ✗ ${name}${detail ? " — " + detail : ""}`);
  }
};

const appSrc = readFileSync(APP, "utf8");

// loadGatewayBackfill 加载 renderer/gateway-backfill.js（UMD，无 import）。
function loadGatewayBackfill() {
  try {
    return createRequire(import.meta.url)(join(here, "renderer", "gateway-backfill.js"));
  } catch (e) {
    return null;
  }
}
const mainSrc = readFileSync(MAIN, "utf8");
const preloadSrc = readFileSync(PRELOAD, "utf8");

// ── 1) 发现函数存在且会调服务端权威端点 ────────────────────────────

check(
  "存在 /device/gateway 发现调用",
  appSrc.includes("/device/gateway"),
  "找不到 —— 服务端发现端点没被调用",
);

// ── 2) 发现结果必须回写到主进程（本判据的核心）──────────────────────
//
// 这是本次修复的要点。若这段回写被删掉（重构时不慎），设备桥会退回
// 「只用用户手填地址」的行为，且没有任何报错——
// 这正是 2026-10-05 那次故障的形态。

check(
  "发现结果回写到主进程 deviceBridge.set",
  /discoveredGateway[\s\S]{0,600}?deviceBridge\s*\.\s*set\s*\(/.test(appSrc),
  "loadDiscoveredGateway 里没有 deviceBridge.set 回写 —— " +
    "state.discoveredGateway 只用于 UI，主进程仍在用旧的手填地址",
);

// ── 3) 回写决策：**调接口**断言，不读源码 ──────────────────────────
//
// 决策逻辑已抽到 `renderer/gateway-backfill.js` 的 `decideBackfill(current, discovered)`：
// 返回要回写的 gateway，或 null（不回写）。
//
// 两条契约直接调函数断言，不再正则匹配「源码里有没有 cur && !cur.gateway」：
//   · 本地已配置 ⇒ 必须 null（手填优先，用户可能故意指向内网/灰度）
//   · 无发现 ⇒ 必须 null；本地为空 ⇒ 返回值就是发现值
//
// 第三条（"只传 gateway 不传 token"）不在这里判：它由调用点的
// `set({ gateway: backfillGw })` 保证，而那是**单字段对象字面量** ——
// 判据可以断它，但更有意义的是断 decideBackfill 的返回值（见下）。
{
  const G = loadGatewayBackfill();
  check("能加载 gateway-backfill.js", G !== null);
  if (G) {
    const d = G.decideBackfill;
    check(
      "★ 本地已配置 gateway 时不回写（手填优先）",
      d({ gateway: "ws://user-set" }, "ws://discovered") === null,
      "已配置却回写 ⇒ 自动发现会冲掉用户手填的地址（内网直连/灰度实例被覆盖）",
    );
    check(
      "本地未配置时回写发现值",
      d(null, "ws://discovered") === "ws://discovered",
      "不返回发现值 ⇒ 设备桥仍用空地址，永远 401/ECONNREFUSED",
    );
    check(
      "本地 gateway 为空串时视作未配置",
      d({ gateway: "" }, "ws://discovered") === "ws://discovered",
      "空串被当成已配置 ⇒ 首次发现永远不生效",
    );
    check(
      "无发现值时一律不回写",
      d(null, "") === null && d({ gateway: "" }, "") === null,
      "空发现仍返回非空值 ⇒ 会写坏配置",
    );
  }
}

// ── 4) IPC 链路完整：preload 暴露 get/set，主进程处理 ──────────────
//
// 回写用的是 window.homeagent.deviceBridge.set(...)；contextBridge
// 若没暴露或主进程没注册，这次调用是 undefined TypeError（被 catch
// 吞掉，于是又回到「静默不生效」）。

check(
  "preload 暴露 deviceBridge.set",
  /deviceBridge[\s\S]{0,120}?set\s*:/.test(preloadSrc),
  "contextBridge 未暴露 set —— 渲染进程调不到",
);
check(
  "主进程注册 device-bridge:set",
  mainSrc.includes('ipcMain.handle("device-bridge:set"'),
  "主进程没有该 handler",
);
check(
  "主进程 device-bridge:get 返回明文 token",
  /device-bridge:get[\s\S]{0,900}?token:\s*db\.token/.test(mainSrc),
  "get 未返回 token 明文 —— 渲染侧拿不到设备令牌",
);

// ── 5) device-bridge:set 必须真正重启桥 ───────────────────────────
//
// 只存 prefs 不重启，配置不会立即生效（要等下次进程启动）。
// 这是同一类「存了但没生效」的静默失效。

// 不用大正则匹配整个 handler —— 返回对象里的嵌套 } 会让边界判断很脆。
// 改为取两个 handler 之间的**源码切片**，再在里面判 stop/start 是否存在。
const setStart = mainSrc.indexOf('ipcMain.handle("device-bridge:set"');
const setEnd = mainSrc.indexOf('ipcMain.handle("prefs:get"');
const setHandler =
  setStart >= 0 && setEnd > setStart
    ? mainSrc.slice(setStart, setEnd)
    : null;
check(
  "device-bridge:set 会重启设备桥",
  setHandler
    ? setHandler.includes("stopDeviceBridge") &&
      setHandler.includes("startDeviceBridge")
    : false,
  setHandler
    ? "handler 里没有 stop+start —— 新配置要等重启进程才生效"
    : "找不到 device-bridge:set handler",
);

// ── 6) 主进程连接必须带 token ─────────────────────────────────────
//
// 设备桥 WS 走 ?token=（registry.go ServeWS 只认 query/X-API-Key/
// Sec-WebSocket-Protocol 三种，**cookie 不参与** acceptBind）。
// 这是实测踩过的坑：曾误以为发 cookie 就能过，实际服务端仍 401。

const connectFn = mainSrc.match(
  /function connectDeviceWS[\s\S]*?\n}\n/,
);
check(
  "connectDeviceWS 把 token 放进 query",
  connectFn ? /token=/.test(connectFn[0]) : false,
  connectFn
    ? "query 里没带 token —— 服务端 acceptBind 会拒"
    : "找不到 connectDeviceWS",
);

// ★ keepalive 与重连的判据**不在本文件**，在同目录的
//   device-bridge-liveness.test.mjs —— 那份用 node:vm 把真实函数抽出来跑
//   「连接 → 断线 → 重连 → 再断线」，数真实的 ping 帧与重连次数。
//   本文件早期版本曾用文本判据盯keepalive（grep 0x89、找 setInterval 的
//   间隔），已删：它只能证明「代码里出现过这些字符」，证明不了「心跳真的
//   发出去了」「重连真的发生了」。两份盯同一件事时，弱的那份会在实现重构
//   时先红，掩盖真信号。

// ── 汇总 ────────────────────────────────────────────────────────────

console.log("");
if (failures > 0) {
  console.log(`失败 ${failures} 条`);
  process.exit(1);
}
console.log("全部通过");