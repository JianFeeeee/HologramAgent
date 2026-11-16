// 对话页布局的判据 —— **调接口**，不读源码文本。
//
// ## 被测对象
//
// `renderer/chat-layout.js` 的 `buildChatLayout(params)`：输入状态 →
// 返回 HTML 字符串。判据直接 require 它、拿输出断言。
//
// ## 原判据的问题（2026-11-16 改）
//
// 它从 `app.js` 源码里切出 HTML 片段，用正则把跨行字符串拼接还原成一段
// 连续 HTML，再数 `<div>` 与 `</div>` 的个数。三重脆弱：
//
//   ① 切片起点写死 `chat-panel active` ⇒ **最外层 `<div class="chat-layout">`
//      被跳过**，于是那个 div 漏闭合它看不见（实测确实漏了，浏览器"尽力修复"了）；
//   ② 注释里出现一个 `<div>` 就误判；
//   ③ 模板换成模板字符串（反引号）就假红。
//
// ## 为什么标签配平是真契约
//
// 不配平 ⇒ 浏览器尽力修复 ⇒ 后面的面板被塞进前一个内部 ⇒
// 父级 display:none ⇒ getBoundingClientRect() 恒 0x0 ⇒ 面板看起来空白。
// 而源码里完全看不出来（字符串拼接 + 跨行）。
//
// 运行：node cmd/gui/chat-panel-dom.test.mjs

import { createRequire } from "node:module";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const require_ = createRequire(import.meta.url);
const ChatLayout = require_(join(here, "renderer", "chat-layout.js"));

let failures = 0;
const check = (name, ok, detail) => {
  if (ok) console.log(`  ✓ ${name}`);
  else {
    failures++;
    console.log(`  ✗ ${name}${detail ? " — " + detail : ""}`);
  }
};

// 两种输入形态都要判：未配置后端（引导页）与已配置（完整布局）。
const CASES = [
  ["无连接", { kernel: {}, chatStage: "", messages: [], currentConn: null }],
  [
    "有连接",
    {
      kernel: { memory: { entity_count: 3 }, knowledge: { item_count: 5 } },
      chatStage: "工具调用中",
      messages: [],
      currentConn: { id: "c1", type: "webui" },
    },
  ],
];

const PANELS = [
  "chat",
  "starmap",
  "terminal",
  "cmd",
  "memory",
  "context",
  "knowledge",
];

// 数 div 深度：最终必须回到 0（配平），且中途不得为负（没有多余的闭合）。
function divDepth(html) {
  const re = /<div\b|<\/div>/g;
  let depth = 0;
  let minDepth = 0;
  let m;
  while ((m = re.exec(html))) {
    depth += m[0] === "<div" ? 1 : -1;
    if (depth < minDepth) minDepth = depth;
  }
  return { depth, minDepth };
}

for (const [label, params] of CASES) {
  const html = ChatLayout.buildChatLayout({
    kernel: params.kernel,
    chatStage: params.chatStage,
    messages: params.messages,
    currentConn: params.currentConn,
    t: (zh) => zh,
    esc: (s) => String(s),
  });

  const { depth, minDepth } = divDepth(html);
  check(
    `${label}：div 标签配平`,
    depth === 0,
    `最终深度 ${depth} —— 少 ${depth} 个 </div> 会让后面所有面板被塞进前一个内部（表现为空白）`,
  );
  check(
    `${label}：没有多余的 </div>`,
    minDepth >= 0,
    `中途深度降到 ${minDepth} —— 多了闭合标签`,
  );

  // 每个面板都必须出现且 id 唯一
  for (const name of PANELS) {
    const id = 'id="chat-panel-' + name + '"';
    const count = html.split(id).length - 1;
    check(
      `${label}：面板 ${name} 出现且唯一`,
      count === 1,
      count === 0 ? "缺失" : `出现 ${count} 次（id 重复会让 switchChatPanel 抓错节点）`,
    );
  }

  // 当前激活的面板必须恰好一个（CSS 靠 .active 控制可见）
  const activeCount = (html.match(/chat-panel[^>]*\bactive\b/g) || []).length;
  check(
    `${label}：恰好一个面板是 active`,
    activeCount === 1,
    `有 ${activeCount} 个 active —— 多则同时显示，零则全不可见`,
  );
}

console.log(
  failures === 0 ? "\n全部通过" : `\n${failures} 项未通过`,
);
process.exit(failures === 0 ? 0 : 1);
