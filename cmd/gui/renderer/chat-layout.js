// 对话页（chat 视图）的**布局模板**：输入状态 → 返回 HTML 字符串。
//
// ## 为什么单独一个模块
//
// 它原本是 `renderer/app.js` 里的 `buildChatLayout()`：169 行、几乎全是
// HTML 字符串拼接，只依赖四样东西（kernel 快照 / chatStage / messages 长度 /
// 当前连接）和两个辅助（翻译 `__()`、转义 `escHtml()`）。
//
// 内联在 app.js 里的后果是**判据只能切源码文本**：
// `chat-panel-dom.test.mjs` 用正则数 `<div>` 与 `</div>` 的个数来判配平 ——
// 那是**读源码做断言**（§0 第 5 条禁止），而且极其脆弱：
// 注释里出现一个 `<div>` 就会误判，模板换成别的写法（如模板字符串）就假红。
//
// 抽成模块后判据能直接拿**模板输出**断言：
//
//	布局({...}) → 字符串 → 数标签配平 / 断言每个面板都在 / 断言 id 唯一
//
// 这才是「面向接口」：判据调函数拿到产出，不读实现。
//
// ## 依赖全部变成参数（p）
//
//	p.kernel        内核状态快照（用于记忆/知识计数）
//	p.chatStage     当前阶段徽标文本
//	p.messages      消息数组（只用 .length，判空态）
//	p.currentConn   当前连接（决定是否渲染「未配置后端」引导页）
//	p.t             翻译函数（zh/en）
//	p.esc           转义函数
//
// 这样它零全局依赖 —— 渲染进程与 node 判据都能直接调。
//
// ## 加载方式
//
// 渲染进程用 `<script src="chat-layout.js">`（无 import），
// node 判据用 `require`。UMD 同时满足两者（与 renderer/vendor/*.min.js 同形态）。
(function (root, factory) {
  if (typeof module === "object" && module.exports) {
    module.exports = factory();
  } else {
    root.ChatLayout = factory();
  }
})(typeof self !== "undefined" ? self : this, function () {
function buildChatLayout(p) {
  var k = (p && p.kernel) || {};
  var html = '<div class="chat-layout">';
  html +=
    '<div class="chat-tabs">' +
    '<span class="active" onclick="switchChatPanel(\'chat\',this)">' +
    p.t("对话", "Chat") +
    "</span>" +
    "<span onclick=\"switchChatPanel('starmap',this)\">" +
    p.t("星图", "Star Map") +
    "</span>" +
    "<span onclick=\"switchChatPanel('terminal',this)\">" +
    p.t("终端", "Terminal") +
    "</span>" +
    "<span onclick=\"switchChatPanel('cmd',this)\">" +
    p.t("运行中命令", "Running Commands") +
    "</span>" +
    "<span onclick=\"switchChatPanel('memory',this)\">" +
    p.t("记忆", "Memory") +
    "</span>" +
    "<span onclick=\"switchChatPanel('context',this)\">" +
    p.t("上下文", "Context") +
    "</span>" +
    "<span onclick=\"switchChatPanel('knowledge',this)\">" +
    p.t("知识", "Knowledge") +
    "</span>" +
    "</div>";
  if (!p.currentConn) {
    html +=
      '<div class="chat-panel active" id="chat-panel-chat"><div class="card setup-card">' +
      '<svg class="setup-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z"/></svg>' +
      "<h2>" +
      p.t("未配置后端", "No backend configured") +
      "</h2>" +
      "<p>" +
      p.t(
        "连接 HologramAgent 服务端后即可开始对话。请在设置中添加后端连接。",
        "Connect to a HologramAgent server to start chatting. Add a backend connection in Settings.",
      ) +
      "</p>" +
      '<button class="btn btn-primary" onclick="goSettingsConn()">' +
      p.t("前往设置添加后端", "Go to Settings to add backend") +
      "</button>" +
      "</div></div>";
    for (var p2 = 0; p2 < 6; p2++) {
      html +=
        '<div class="chat-panel" id="chat-panel-' +
        ["starmap", "terminal", "cmd", "memory", "context", "knowledge"][p2] +
        '"><div class="card"><p style="color:var(--text-muted)">' +
        p.t(
          "请先在设置中添加后端连接",
          "Add a backend connection in Settings first",
        ) +
        "</p></div></div>";
    }
    html += "</div>"; // 闭合 .chat-layout
    return html;
  }
  html +=
    '<div class="chat-panel active" id="chat-panel-chat"><div class="chat-main">';
  html +=
    '<div class="card"><h2>' +
    p.t("对话", "Chat") +
    ' <span id="chat-stage" class="badge" style="font-size:12px;font-weight:400;display:none">' +
    p.esc(p.chatStage || "") +
    '</span></h2><div class="chat-messages" id="chat-msgs">';
  if (p.messages.length === 0) {
    html +=
      '<div class="empty-state" style="flex:1;display:flex;align-items:center;justify-content:center"><p>' +
      p.t("开始与您的 HologramAgent 聊天吧", "Start chatting with your HologramAgent") +
      "</p></div>";
  }
  html +=
    "</div>" +
    '<div class="chat-input-row">' +
    '<input id="chat-input" placeholder="' +
    p.t("输入消息...", "Type a message...") +
    '" onkeydown="if(event.key==\'Enter\')sendChat()">' +
    '<button class="btn" onclick="interruptChat()" id="chat-stop-btn" style="display:none;background:#d1383d;color:#fff">' +
    p.t("停止", "Stop") +
    "</button>" +
    '<button class="btn btn-primary" onclick="sendChat()" id="chat-send-btn">' +
    p.t("发送", "Send") +
    "</button>" +
    "</div></div></div></div>";
  html +=
    '<div class="chat-panel" id="chat-panel-starmap"><div class="card"><h2>' +
    p.t("星图", "Star Map") +
    "</h2>" +
    '<div id="sm-container-chat" style="display:flex;align-items:center;justify-content:center;min-height:480px"><div class="ha-dots-panel"><span class="ha-dots ha-dots-lg"><i></i><i></i><i></i></span></div></div></div></div>';
  html +=
    '<div class="chat-panel" id="chat-panel-terminal"><div class="card"><h2>' +
    p.t("终端", "Terminal") +
    ' <span id="term-count-badge" class="badge badge-blue">0</span></h2>' +
    '<div id="term-list" style="max-height:60vh;overflow-y:auto;font-size:12px"></div></div></div>';
  html +=
    '<div class="chat-panel" id="chat-panel-cmd"><div class="card"><h2>' +
    p.t("运行中命令", "Running Commands") +
    ' <span id="cmd-count-badge" class="badge badge-blue">0</span></h2>' +
    '<div id="cmd-list" style="max-height:60vh;overflow-y:auto;font-size:12px"></div></div></div>';
  html +=
    '<div class="chat-panel" id="chat-panel-memory"><div class="card"><h2>' +
    p.t("记忆", "Memory") +
    "</h2>" +
    '<div class="kv-row"><span class="key">' +
    p.t("实体", "Entities") +
    '</span><span class="val">' +
    (k?.memory?.entity_count || "-") +
    "</span></div>" +
    '<div class="kv-row"><span class="key">' +
    p.t("关系", "Relations") +
    '</span><span class="val">' +
    (k?.memory?.relation_count || "-") +
    "</span></div>" +
    '<div style="margin-top:8px">' +
    '<input id="mem-query" placeholder="' +
    p.t("关键词查询", "Keyword query") +
    '">' +
    '<button class="btn btn-primary btn-sm" onclick="queryMemoryChat()">' +
    p.t("查询", "Query") +
    "</button>" +
    '</div><div id="mem-result-chat" style="margin-top:8px;max-height:180px;overflow:auto"></div>' +
    "</div></div>";
  html +=
    '<div class="chat-panel" id="chat-panel-context"><div class="card"><h2>' +
    p.t("上下文", "Context") +
    "</h2>" +
    '<div style="margin-top:8px">' +
    '<input id="ctx-query" placeholder="' +
    p.t("输入当前话题", "Enter current topic") +
    '">' +
    '<button class="btn btn-primary btn-sm" onclick="queryMemoryContext()">' +
    p.t("获取上下文", "Get Context") +
    "</button>" +
    '</div><div id="ctx-result" style="margin-top:8px;max-height:200px;overflow:auto"></div>' +
    "</div></div>";
  html +=
    '<div class="chat-panel" id="chat-panel-knowledge"><div class="card"><h2>' +
    p.t("知识", "Knowledge") +
    "</h2>" +
    '<div class="kv-row"><span class="key">' +
    p.t("项目", "Items") +
    '</span><span class="val">' +
    (k?.knowledge?.item_count || "-") +
    "</span></div>" +
    '<div style="margin-top:8px">' +
    '<input id="know-query" placeholder="' +
    p.t("搜索知识", "Search knowledge") +
    '">' +
    '<button class="btn btn-primary btn-sm" onclick="searchKnowledgeChat()">' +
    p.t("搜索", "Search") +
    "</button>" +
    '</div><div id="know-result-chat" style="margin-top:8px;max-height:180px;overflow:auto"></div>' +
    '<div style="margin-top:12px;border-top:1px solid var(--border-color);padding-top:8px">' +
    '<input id="know-name" placeholder="' +
    p.t("知识名称", "Knowledge name") +
    '" style="margin-bottom:4px">' +
    '<textarea id="know-content" placeholder="' +
    p.t("内容", "Content") +
    '" style="min-height:50px;margin-bottom:4px"></textarea>' +
    '<button class="btn btn-primary btn-sm" onclick="createKnowledgeChat()">' +
    p.t("创建", "Create") +
    "</button>" +
    "</div></div></div>";
  // ★ 闭合最外层 .chat-layout（2026-11-16 抽取时发现原实现漏了）。
  //
  // 原 buildChatLayout 里 `<div class="chat-layout">` 只有开标签、没有对应
  // 的 `</div>`。浏览器会"尽力修复"把它补在末尾，所以**肉眼看不出问题** ——
  // 这正是它一直没被发现的原因。但 `chat-panel-dom.test.mjs` 从
  // `chat-panel active` 处开始切片，把最外层那个 div 跳过了，于是判据是绿的。
  //
  // 抽成纯模板后一数深度就露出来：无连接/有连接两个分支最终深度都是 1。
  html += "</div>";
  return html;
}

  return { buildChatLayout: buildChatLayout };
});
