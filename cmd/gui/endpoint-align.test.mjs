// GUI ↔ WebUI 端点对齐门禁。
//
// ## 要判的是什么
//
// GUI（cmd/gui/renderer/app.js）与服务端 WebUI 插件（internal/plugins/webui）
// 是两个独立演进的客户端，它们共享同一套 REST 接口。这个共享关系**没有任何
// 强制手段**：服务端加端点不必通知 GUI，GUI 改端点也不必通知服务端。
//
// 真实漂移实例（2026-10-01 核实）：服务端 handler.go 里有 39 个路径，
// GUI 只用 28 个。差集里最刺眼的是 `/memory/graph/pulse` ——
// 服务端**专门为星图「跟随 agent 动」**造这个端点，handler 注释写了动机
// （全量图谱生产实例 408KB / 1151 节点，pulse 只有几 KB，差两个数量级），
// WebUI dashboard 接了它（dashboard.js 里 5 处），GUI 却只在初始化时拉一次
// 全量且完全不轮询 ⇒ GUI 星图停在打开那一刻的快照。
//
// 之前没人发现，是因为 protocol-align.test.mjs 是**真浏览器**判据
// （要 Electron + Xvfb + 真后端），CI 明确不跑。
//
// ## 为什么这个判据必须是「文本扫描」而不是运行时探测
//
// 两个已知端点集合（GUI 的 api("...") 与服务端 mux.HandleFunc）都是**源码文本**
// 里的东西，扫源码就能拿到完整集合，不需要起服务、不需要鉴权。
// 而运行时探测（真连一个后端）在这两个维度上不可用：
//   - 要真后端 + 有效凭据（本机实测 admin/admin 已失效）；
//   - 只能看到「已部署版本」的端点，看不到源码里的 —— 而漂移恰恰
//     发生在「源码已加、客户端还没接」的窗口里，那正是要抓的。
//
// 与既有判据（sse-backoff / retry-guard）同一路子：从**真实源码**提取，
// 不抄一份逻辑重写（抄的那份会和真实代码漂移，而漂移正是本判据要防的）。
//
// 运行：node endpoint-align.test.mjs

import { readFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, "..", "..");

const APP = join(here, "renderer", "app.js");
const HANDLER_GO = join(repoRoot, "internal", "plugins", "webui", "handler.go");

let failures = 0;
const check = (name, ok, detail) => {
  if (ok) {
    console.log(`  ✓ ${name}`);
  } else {
    failures++;
    console.log(`  ✗ ${name}${detail ? " — " + detail : ""}`);
  }
};

// ── 1) 读源码 ───────────────────────────────────────────────────────

if (!existsSync(HANDLER_GO)) {
  console.error(`找不到服务端路由表：${HANDLER_GO}`);
  process.exit(1);
}
const appSrc = readFileSync(APP, "utf8");
const handlerSrc = readFileSync(HANDLER_GO, "utf8");

// ── 2) 抽端点集合 ───────────────────────────────────────────────────

// 服务端：mux.HandleFunc("...", ...) 里的第一个参数。
// 只取 /api/v1 与 /v1 前缀的（/files/ /uploads/ /static/ / 是页面与静态资源，
// 不是 GUI 的 api() 目标；带上它们只会制造噪音）。
function serverRoutes() {
  const out = new Set();
  const re = /mux\.HandleFunc\(\s*"([^"]+)"/g;
  let m;
  while ((m = re.exec(handlerSrc)) !== null) {
    const p = m[1];
    if (p.startsWith("/api/v1/") || p.startsWith("/v1/")) out.add(p);
  }
  return out;
}

// GUI：api("...") 的第一个参数。带 query 串的按 '?' 截断
// （如 "/chat/history?limit=" → "/chat/history"）。
//
// ★ 正则必须允许字符串后面跟表达式（+ 拼接）：星图 pulse 写成
// api("/memory/graph/pulse?since=" + since)，只匹配到右引号为止会漏掉它 ——
// 而本判据的由来恰恰就是这个端点，用一个会漏掉它的正则去防它漏接是自相矛盾。
// (?<![\w.]) 前视是为了避开 cliMap() 里的字符串比较（path === "/status"），
// 那不是 api() 调用。
function guiCalls() {
  const out = new Set();
  const re = /(?<![\w.])api\(\s*"([^"]*)"/g;
  let m;
  while ((m = re.exec(appSrc)) !== null) {
    out.add(m[1].split("?")[0]);
  }
  return out;
}

const routes = serverRoutes();
const calls = guiCalls();

check(
  "能抽到服务端路由",
  routes.size >= 30,
  `只抽到 ${routes.size} 条 —— 正则可能已与 handler.go 漂移`,
);
check(
  "能抽到 GUI 调用",
  calls.size >= 20,
  `只抽到 ${calls.size} 条 —— 正则可能已与 app.js 漂移`,
);

// ── 3) GUI 调用的每个端点都必须在服务端存在 ──────────────────────────
//
// 这是**硬门禁**：GUI 调了一个服务端没有的端点 ⇒ 运行时必然 404，
// 属于真缺陷。反向（服务端有、GUI 没接）是能力差距，按需评估。

const missing = [...calls].filter((p) => {
  if (routes.has(p)) return false;
  // 前缀通配：服务端有 "/api/v1/adapters/" 而 GUI 调 "/adapters"
  // → api() 会拼成 /api/v1/adapters，靠 Go 1.22 ServeMux 最长前缀匹配命中。
  const withPrefix = "/api/v1" + p;
  if (routes.has(withPrefix)) return false;
  for (const r of routes) {
    if (r.endsWith("/") && withPrefix.startsWith(r)) return false;
  }
  return true;
});

check(
  "GUI 调用的端点服务端都存在",
  missing.length === 0,
  missing.length
    ? "服务端无此路由（运行时必然 404）：\n       - " + missing.join("\n       - ")
    : "",
);

// ── 4) 关键能力端点必须被 GUI 用上 ──────────────────────────────────
//
// 只判「服务端有 + GUI 必须有」的一小撮**能力面**端点，不是全部差集。
// 理由：差集里的管理面端点（/login /logout /tracker/ /knowledge/tree/
// /memory/text ...）该不该接取决于产品决策，逐条人工判；把这几类硬编码
// 进判据会让人为了让门禁变绿而随手接端点。
//
// 这里只钉**服务端已明确为其设计、且 GUI 已经在用同类能力**的端点：
// 星图活动（pulse）是本判据的由来——服务端专门造它、WebUI 用了、GUI 没接。
// 若将来 GUI 确实不再需要星图，请连同下面这条判据一起删，并在注释里
// 写明理由；不要静默留着一条红的门禁。

const mustUse = [
  {
    path: "/api/v1/memory/graph/pulse",
    why: "星图活动源：服务端为「不重复拉 408KB 全量」专门造的轻量端点",
  },
];

const unused = mustUse.filter((m) => {
  // mustUse 写的是服务端全路径（/api/v1/…），GUI 侧 api() 传的是
  // 去掉 /api/v1 前缀的短路径（api() 内部会拼）。两边都比一次。
  const short = m.path.replace(/^\/api\/v1/, "");
  return !calls.has(m.path) && !calls.has(short);
}).map((m) => m.why);
check(
  "能力面端点未被 GUI 漏接",
  unused.length === 0,
  unused.length ? unused.join("；") : "",
);

// ── 5) 页面不得再引用公网 CDN ───────────────────────────────────────
//
// 与服务端 starmap_vendor_test.go 同一判据（那里钉 WebUI，这里钉 GUI）。
// HologramAgent 支持离线/内网部署，而前端有 4 个硬依赖在公网上时，
// 断网/出口受限环境下星图必坏、且用户无从修复。

const htmlSrc = readFileSync(join(here, "renderer", "index.html"), "utf8");
const cdnRe = /(?:src|href)\s*=\s*["'](https?:\/\/[^"']+)["']/g;
const cdnUrls = [...htmlSrc.matchAll(cdnRe)].map((m) => m[1]);
check(
  "index.html 不引用外部 CDN",
  cdnUrls.length === 0,
  cdnUrls.length ? "仍引用：" + cdnUrls.join(", ") : "",
);

// 本地 vendor 文件必须真的存在（embed/打包漏文件 ⇒ 静默失效）。
const vendorFiles = [
  "three.min.js",
  "OrbitControls.js",
  "marked.min.js",
  "purify.min.js",
];
const missingVendor = vendorFiles.filter(
  (f) => !existsSync(join(here, "renderer", "vendor", f)),
);
check(
  "vendor 第三方库文件都在",
  missingVendor.length === 0,
  missingVendor.length ? "缺少：" + missingVendor.join(", ") : "",
);

// 两端 vendor 版本必须一致（否则星图/markdown 行为会在两端分叉）。
const serverStatic = join(repoRoot, "internal", "plugins", "webui", "static");
const drift = vendorFiles.filter((f) => {
  const a = join(here, "renderer", "vendor", f);
  const b = join(serverStatic, f);
  if (!existsSync(b)) return true;
  return readFileSync(a, "utf8") !== readFileSync(b, "utf8");
});
check(
  "GUI 与 WebUI 的 vendor 版本一致",
  drift.length === 0,
  drift.length
    ? "内容与 internal/plugins/webui/static 不一致：" + drift.join(", ")
    : "",
);

// ── 汇总 ────────────────────────────────────────────────────────────

console.log("");
console.log(`  服务端路由 ${routes.size} 条 / GUI 调用 ${calls.size} 条`);
if (missing.length === 0) {
  console.log("  服务端有、GUI 未使用的端点（差集，供人工评估，非门禁）：");
  const notUsed = [...routes].filter((r) => {
    const short = r.replace(/^\/api\/v1\//, "/").replace(/^\/v1\//, "/");
    return !calls.has(r) && !calls.has(short);
  });
  notUsed.slice(0, 14).forEach((p) => console.log(`    · ${p}`));
  console.log(`    （共 ${notUsed.length} 条未使用）`);
}
console.log("");

if (failures > 0) {
  console.log(`全部失败：${failures} 条`);
  process.exit(1);
}
console.log("全部通过");