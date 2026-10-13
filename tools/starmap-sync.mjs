#!/usr/bin/env node
// 把 shared/starmap/*.js 同步进各客户端的内联脚本标记块。
//
// ## 为什么是「生成器」而不是「运行时加载」
//
// 两个客户端的内联脚本形态不同、加载机制也不同：
//   · WebUI：dashboard.js 由 Go 的 //go:embed 编进二进制，再被 init() 拼进 HTML
//   · GUI ：app.js 由 electron 用 file:// 直接 <script src> 加载
//
// 让 GUI 去服务端拉 dashboard.js 会把「星图能否初始化」绑到网络与登录态上
// （实测 GUI 是 file:// + 可能离线启动），而让 WebUI 内联一份 GUI 的代码则
// 要动 embed 与启动顺序。两者都比「编辑期把同一份源码写进两个文件的标记块」
// 风险大得多。
//
// 所以取生成器方案：**唯一事实源在 shared/，两份客户端文件里的相应区间由本
// 脚本重写**。判据（starmap-perf.test.mjs 一侧）钉住三处逐字节相同，
// 手改任一副本会被立刻拦下。
//
// 用法：
//   node tools/starmap-sync.mjs           # 同步
//   node tools/starmap-sync.mjs --check   # 只校验，漂移则退出 1

import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = join(dirname(fileURLToPath(import.meta.url)), "..");

// 共享块 → 宿主文件。块名与标记名构成契约。
const SHARED = "starmap/layout";
const MARK_BEGIN = `// ===== BEGIN shared:${SHARED} =====`;
const MARK_END = `// ===== END shared:${SHARED} =====`;

const HOSTS = [
  "internal/plugins/webui/dashboard.js",
  "cmd/gui/renderer/app.js",
];

const CHECK = process.argv.includes("--check");

const sharedPath = join(ROOT, "shared", SHARED + ".js");
const shared = readFileSync(sharedPath, "utf8").replace(/\r\n/g, "\n");

// 标记块内容 = 共享源 + 一行来源提示。提示放在块内（而不是块外），
// 这样「块内容逐字节相同」这条判据才能同时保证「两边都带着同一提示」。
const block =
  MARK_BEGIN +
  "\n" +
  shared.replace(/\n+$/, "") +
  "\n// 本块由 tools/starmap-sync.mjs 从 shared/" +
  SHARED +
  ".js 生成 —— 请改共享源，不要直接改这里。\n" +
  MARK_END;

let drift = 0;
let changed = 0;

for (const rel of HOSTS) {
  const p = join(ROOT, rel);
  const src = readFileSync(p, "utf8").replace(/\r\n/g, "\n");
  const b = src.indexOf(MARK_BEGIN);
  const e = src.indexOf(MARK_END);
  if (b < 0 || e < 0 || e < b) {
    console.error(`✗ ${rel}: 找不到共享标记块（${MARK_BEGIN} / ${MARK_END}）`);
    drift++;
    continue;
  }
  const cur = src.slice(b, e + MARK_END.length);
  if (cur === block) {
    console.log(`✓ ${rel}: 已与共享源一致`);
    continue;
  }
  drift++;
  if (CHECK) {
    console.error(`✗ ${rel}: 共享块与 shared/${SHARED}.js 不一致（漂移）`);
    continue;
  }
  writeFileSync(p, src.slice(0, b) + block + src.slice(e + MARK_END.length), "utf8");
  console.log(`~ ${rel}: 已刷新共享块`);
  changed++;
}

if (CHECK) {
  if (drift) {
    console.error(
      `\n共享块漂移 ${drift} 处。修法：node tools/starmap-sync.mjs\n` +
        `（若是改了算法，请只改 shared/${SHARED}.js，再跑同步。）`,
    );
    process.exit(1);
  }
  console.log("\n共享块一致");
  process.exit(0);
}
console.log(changed ? `\n已同步 ${changed} 个文件` : "\n无需改动");
