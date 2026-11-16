// 协议对齐判据：在真 Electron 里验证补齐的端点真的取到数据、
// 且三个面板渲染出**可见内容**（不是空壳）。
//
// ## 背景
//
// 2026-09-28 用户要求对齐 WebAPI 协议。核实发现 GUI 只用 22 个端点，
// 服务端有 47 个。补齐的是**只读诊断类**：agents / network / tracker /
// config / persona / proxy(+services)。
//
// ★ 刻意**不接** /login 与 /logout：那是 cookie 会话认证流程，
//   而 GUI 走 `X-API-Key` 头（见 api()）。接了反而是错的对齐。
//
// ★ 只加进 refreshAll、**不加** refreshDataOnly：后者每 15 秒一轮，
//   诊断数据不必高频轮询。
//
// ## 为什么必须真浏览器
//
// 判据要验的是「面板里真的有内容」，依赖 DOM 渲染 —— node 里测不了。
//
// 运行：node cmd/gui/protocol-align.test.mjs

import { spawn, spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const PORT = Number(process.env.GUI_CDP_PORT || 9370);

// apiKey 从哪来：环境变量优先，否则读生产 config.db。
//
// ★ 默认**不能**用假 key：webui 对错误凭据返回 200 + 登录页 HTML
//   （looks_like_login_page 会识别），于是所有取数都失败 ⇒ 判据全红，
//   看起来像「代码坏了」，实际只是认证缺失。踩过一次。
//
// 数据目录**不写死**：生产在 /home/hologramagent，历史机器在 /home/newqqagent，
// 开发机可能别处。硬编码单一路径的后果 = 换台机器判据直接红，看起来像代码坏了。
//
// 查找顺序：GUI_API_KEY 环境变量 → GUI_DATA_DIR → 常见生产路径 → 报错。
function resolveApiKey() {
  if (process.env.GUI_API_KEY) return process.env.GUI_API_KEY;
  const candidates = [];
  if (process.env.GUI_DATA_DIR) {
    candidates.push(join(process.env.GUI_DATA_DIR, "config.db"));
  }
  candidates.push(
    "/home/hologramagent/config.db",
    "/home/newqqagent/config.db",
    "/root/.config/hologramagent/config.db",
  );
  for (const db of candidates) {
    if (!existsSync(db)) continue;
    try {
      const out = spawnSync("sqlite3", [db,
        "select value from config_webui where key='api_key';"],
        { encoding: "utf8" });
      const key = (out.stdout || "").trim();
      if (key) return key;
    } catch {}
  }
  return "";
}

const API_KEY = resolveApiKey();
if (!API_KEY) {
  console.log("  ✗ 拿不到 webui api_key：设 GUI_API_KEY / GUI_DATA_DIR，或确认已知路径下有 config.db");
  process.exit(1);
}

let failures = 0;
const check = (n, ok, d) => {
  if (ok) console.log(`  ✓ ${n}`);
  else {
    failures++;
    console.log(`  ✗ ${n}${d ? " — " + d : ""}`);
  }
};

async function targets() {
  try {
    return await (await fetch(`http://127.0.0.1:${PORT}/json`)).json();
  } catch {
    return [];
  }
}

async function ensure() {
  const t = await targets();
  if (t.some((x) => x.type === "page" && x.url.includes("index.html"))) return;
  const bin = join(here, "node_modules/.bin/electron");
  if (!existsSync(bin)) throw new Error("electron 未安装，先 cd cmd/gui && npm i");
  // ★ setsid：detached 只脱离进程组，脚本退出仍会带走刚起的 GUI
  //   （症状：[tray] READY 打了，判据却报「找不到 GUI 页面」）。
  spawn(
    "setsid",
    ["xvfb-run", "-a", "--server-args=-screen 0 1280x900x24", bin, "--no-sandbox",
     `--remote-debugging-port=${PORT}`, "."],
    { cwd: here, detached: true, stdio: "ignore" },
  ).unref();
  for (let i = 0; i < 40; i++) {
    await new Promise((r) => setTimeout(r, 1000));
    const t2 = await targets();
    if (t2.some((x) => x.type === "page" && x.url.includes("index.html"))) return;
  }
  throw new Error(`GUI 启动超时（${PORT}）`);
}

async function ev(expr) {
  const tabs = await targets();
  const page = tabs.find((t) => t.type === "page" && t.url.includes("index.html"));
  if (!page) throw new Error("找不到 GUI 页面");
  const sock = new WebSocket(page.webSocketDebuggerUrl);
  await new Promise((r) => (sock.onopen = r));
  const id = Math.floor(Math.random() * 1e6);
  sock.send(JSON.stringify({
    id, method: "Runtime.evaluate",
    params: { expression: expr, awaitPromise: true, returnByValue: true },
  }));
  // ★ JSON.parse 必须包 try：CDP 也会发非 JSON 帧，裸 parse 抛在回调里
  //   既冒泡不到 await 也等不到 resolve ⇒ 整个判据挂死。
  const res = await new Promise((r) => {
    sock.onmessage = (e) => {
      let m;
      try {
        m = JSON.parse(e.data);
      } catch {
        return;
      }
      if (m && m.id === id) r(m);
    };
  });
  sock.close();
  const v = res.result?.result?.value;
  if (v === undefined) throw new Error(res.result?.exceptionDetails?.text || "求值失败");
  return v;
}

await ensure();

const r = await ev(`(async () => {
  const K = ${JSON.stringify(API_KEY)};
  state.connections = [{id:'p',name:'p',type:'webui',url:'http://127.0.0.1:8080',apiKey:K}];
  state.currentConn = state.connections[0];
  // ★ 走应用**自己的**入口 refreshAll()，不直接调 refreshDiagData()。
  //   早先直接调 refreshDiagData ⇒ 判据绕过了应用里的调用点 ⇒
  //   把「await refreshDiagData()」注释掉，判据仍全绿（实测踩过）。
  //   变异测试的意义就在于抓这种「判据没测到真路径」。
  await refreshAll();
  renderOverview(); renderPersona(); renderProxy();
  const txt = (id) => { const e = document.getElementById(id); return e ? e.innerText : ''; };
  return {
    slots: {
      agents: !!(state.agents && Array.isArray(state.agents.agents)),
      network: !!(state.network && Array.isArray(state.network.endpoints)),
      tracker: !!(state.tracker && typeof state.tracker.changesets === 'number'),
      runConfig: !!(state.runConfig && !!state.runConfig.daemon),
      persona: !!(state.persona && state.persona.current_prompt),
      proxy: !!(state.proxy && typeof state.proxy.total === 'number'),
      proxyServices: !!(state.proxyServices && Array.isArray(state.proxyServices.services)),
    },
    overviewHasDiag: txt('view-overview').includes('诊断') || txt('view-overview').includes('Diagnostic'),
    personaText: txt('view-persona').slice(0, 120),
    proxyText: txt('view-proxy').replace(/\\n+/g, ' | ').slice(0, 200),
  };
})()`);

for (const [k, v] of Object.entries(r.slots)) {
  check(`端点取到数据：${k}`, v, "state 槽为空 ⇒ 该端点没接上或没取到");
}
check("总览出现诊断卡片", r.overviewHasDiag);
check(
  "人设面板显示提示词",
  r.personaText.includes("你是") || r.personaText.length > 40,
  `实际文本：${r.personaText.slice(0, 60)}`,
);
check("反代面板列出服务", r.proxyText.includes("→"), `实际文本：${r.proxyText.slice(0, 80)}`);

console.log(failures === 0 ? "\n全部通过" : `\n${failures} 项未通过`);
process.exit(failures === 0 ? 0 : 1);
