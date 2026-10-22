// 全局 JS 错误捕获（透传到 gui.log 便于诊断）
window.onerror = (msg, src, line, col, err) => {
  try {
    if (window.homeagent && window.homeagent.log)
      window.homeagent.log(
        "JS-ERR " + msg + " @ " + src + ":" + line + ":" + col,
      );
  } catch (e) {}
};
window.onunhandledrejection = (e) => {
  try {
    if (window.homeagent && window.homeagent.log)
      window.homeagent.log(
        "JS-REJ " +
          (e && e.reason ? String(e.reason).slice(0, 200) : "unknown"),
      );
  } catch (e2) {}
};

// ===== State =====
const state = {
  status: {},
  kernel: null,
  settings: {},
  meta: {},
  pluginMeta: {},
  settingsPlugins: ["core"],
  disabledPlugins: [],
  currentView: "chat",
  selectedSection: "core",
  messages: [],
  chatLoading: false,
  chatStage: "",
  _turnWatchdog: null,
  healthResult: null,
  starmapInit: false,
  starmapLoading: false,
  starmapData: null,
  chatHistory: [],
  terminals: [],
  cmdHistory: [],
  termScreens: {},
  chatStick: true,
  pendingTools: [],
  eventSource: null,
  chatFinalIdx: -1,
  chatOffset: 0, // 分段历史：当前已加载消息在服务端全量中的起始下标
  chatTotal: 0, // 服务端历史总条数
  chatHasMore: false, // 是否还有更早历史可向上加载
  sseLastEventID: "", // 最近一次 SSE 事件 id，断线重连时随 Last-Event-ID 头回传
  lang: localStorage.getItem("ha-lang") || "zh",
  connections: [],
  currentConn: null,
  devices: [],
  displays: [],
  selfDeviceId: "",
  selfGateway: "",
  deviceBridgeToken: "", // 设备接入令牌明文（来自 device-bridge:get），用于 /device/* 的 requireToken
  _notifyState: null,      // 本轮通知去重状态 { turnSig, notified }
  _notifySupported: null, // 系统通知是否可用（null=未探测）
  // ---- 星图活动数据源（对齐服务端 /memory/graph/pulse + /runtime）----
  starmapPulses: [], // { mesh, until, kind } 活动脉冲队列
  starmapGrown: {}, // nodeId -> 生长动画截止时间戳（ms）
  starmapLastPulseAt: 0, // 最后一次活动时间（ms），驱动全局呼吸
  starmapPulseSince: 0, // 下次 pulse 的回看起点（unix 秒）
  starmapPulseTimer: null, // /runtime 3s 轮询 id
  starmapActivityTimer: null, // /memory/graph/pulse 10s 轮询 id
  _smPrevSched: null, // 上一拍 /runtime 调度器快照（做差值判定）
};

// ===== I18n =====
window._i18n = {
  navOverview: ["概览", "Overview"],
  navChat: ["对话", "Chat"],
  navPlugins: ["插件", "Plugins"],
  navSettings: ["设置", "Settings"],
  navAdapters: ["适配器", "Adapters"],
  navKernel: ["内核", "Kernel"],
  navLogout: ["退出登录", "Logout"],
  themeToggle: ["切换亮色/暗色模式", "Toggle theme"],
  clickManage: ["点击管理连接", "Click to manage connections"],
  secondsAgo: ["秒前", "s ago"],
  minutesAgo: ["分钟前", "min ago"],
  hoursAgo: ["小时前", "h ago"],
  noConnection: ["未连接", "Not connected"],
  agentAvatar: ["小宅", "Agent"],
  waitingAI: ["等待AI回复...", "Waiting for AI..."],
  noResponse: ["(无响应)", "(no response)"],
  error: ["错误: ", "Error: "],
  requestFailed: ["请求失败: ", "Request failed: "],
  send: ["发送", "Send"],
  queryFailed: ["查询失败: ", "Query failed: "],
  searchFailed: ["搜索失败: ", "Search failed: "],
  getFailed: ["获取失败: ", "Get failed: "],
  createFailed: ["创建失败", "Create failed"],
  createFailedWith: ["创建失败: ", "Create failed: "],
  nameContentEmpty: ["名称和内容不能为空", "Name and content cannot be empty"],
  knowledgeCreated: ["知识「", 'Knowledge "'],
  knowledgeCreatedEnd: ["」已创建", '" created'],
  noContext: ["无上下文", "No context"],
  noSessions: ["暂无终端会话", "No terminal sessions"],
  noHistory: ["暂无命令记录", "No command history"],
  running: ["运行中", "Running"],
  closed: ["已关闭", "Closed"],
  command: ["命令", "Command"],
  status: ["状态", "Status"],
  created: ["创建时间", "Created"],
  uptime: ["运行时长", "Uptime"],
  output: ["输出预览", "Output"],
  time: ["时间", "Time"],
  actions: ["操作", "Actions"],
  name: ["名称", "Name"],
  description: ["描述", "Description"],
  version: ["版本", "Version"],
  details: ["详情", "Details"],
  close: ["关闭", "Close"],
  install: ["安装", "Install"],
  installPlugin: ["安装插件", "Install Plugin"],
  packageUrl: [".hmap 包下载 URL", "Package URL"],
  uploadHmap: ["选择 .hmap 文件上传", "Upload .hmap file"],
  loadedPlugins: ["已加载插件", "Loaded Plugins"],
  noLoadedPlugins: ["暂无已加载插件", "No loaded plugins"],
  loaded: ["已加载", "Loaded"],
  builtin: ["内置", "Built-in"],
  unload: ["卸载", "Unload"],
  installedExternal: ["已安装外部插件", "Installed Plugins"],
  pluginDetails: ["插件详情", "Plugin Details"],
  registeredTools: ["已注册工具", "Registered Tools"],
  systemOps: ["系统操作", "System Operations"],
  reloadPlugins: ["重载插件", "Reload Plugins"],
};

function __(zh, en) {
  return state.lang === "en" ? en : zh;
}
function L() {
  return state.lang;
}

function toggleLang() {
  state.lang = state.lang === "zh" ? "en" : "zh";
  localStorage.setItem("ha-lang", state.lang);
  applyI18n();
  renderAll();
}

function applyI18n() {
  var lang = state.lang;
  var btn = document.getElementById("lang-btn");
  if (btn) btn.textContent = lang === "zh" ? "EN" : "中";
  document.querySelectorAll("[data-i18n]").forEach((el) => {
    var k = el.getAttribute("data-i18n");
    var m = window._i18n && window._i18n[k];
    if (m) el.textContent = lang === "en" ? m[1] : m[0];
  });
}

// ===== Theme =====
var ICON_SUN_GUI =
  '<svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2m0 16v2M4.9 4.9l1.4 1.4m11.4 11.4 1.4 1.4M2 12h2m16 0h2M4.9 19.1l1.4-1.4m11.4-11.4 1.4-1.4"/></svg>';
var ICON_MOON_GUI =
  '<svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"/></svg>';

function setTheme(name) {
  document.documentElement.setAttribute("data-theme", name);
  localStorage.setItem("ha-theme", name);
  var btn = document.getElementById("theme-btn");
  if (btn) btn.innerHTML = name === "light" ? ICON_SUN_GUI : ICON_MOON_GUI;
}

function toggleTheme() {
  var cur = document.documentElement.getAttribute("data-theme");
  setTheme(cur === "light" ? "dark" : "light");
}

// ===== Appearance: 主题色 / 背景图 =====
var PALETTES_GUI = {
  sakura: "#ff7fac",
  cyan: "#2dd4bf",
  violet: "#a78bfa",
  emerald: "#34d399",
  amber: "#fbbf24",
  blue: "#60a5fa",
};

function setColor(name) {
  document.documentElement.setAttribute("data-color", name);
  localStorage.setItem("ha-color", name);
  var pop = document.getElementById("palette-pop");
  if (!pop) return;
  var btns = pop.querySelectorAll("button.cdot");
  for (var i = 0; i < btns.length; i++) {
    btns[i].className =
      btns[i].getAttribute("data-c") === name ? "cdot on" : "cdot";
  }
}

async function applyBgImg(input) {
  var src = (input || "").trim();
  if (!src) {
    document.documentElement.style.setProperty("--bg-img", "none");
    localStorage.removeItem("ha-bg-img");
    localStorage.removeItem("ha-bg-final");
    return;
  }
  var finalSrc = src;
  if (window.homeagent && window.homeagent.cacheBg) {
    try {
      var r = await window.homeagent.cacheBg(src);
      if (r && r.ok && r.file) finalSrc = r.file;
      else if (r && r.error && !r.useOriginal)
        toast(__("背景图加载失败: ", "Bg load failed: ") + r.error, true);
    } catch (e) {
      toast(__("背景图加载失败: ", "Bg load failed: ") + e.message, true);
    }
  }
  document.documentElement.style.setProperty(
    "--bg-img",
    'url("' + finalSrc.replace(/"/g, '\\"') + '")',
  );
  if (/^data:/.test(src)) {
    localStorage.setItem("ha-bg-img", finalSrc);
  } else {
    localStorage.setItem("ha-bg-img", src);
  }
  localStorage.setItem("ha-bg-final", finalSrc);
}

function setBgImgVar(finalSrc) {
  document.documentElement.style.setProperty(
    "--bg-img",
    'url("' + (finalSrc || "").replace(/"/g, '\\"') + '")',
  );
}

function pickBgFile() {
  var fi = document.getElementById("bg-file-input");
  if (!fi) {
    fi = document.createElement("input");
    fi.type = "file";
    fi.id = "bg-file-input";
    fi.accept = "image/*";
    fi.style.display = "none";
    fi.onchange = () => {
      var f = fi.files && fi.files[0];
      if (!f) return;
      var rd = new FileReader();
      rd.onload = () => {
        applyBgImg(rd.result);
      };
      rd.readAsDataURL(f);
      fi.value = "";
    };
    document.body.appendChild(fi);
  }
  fi.click();
}

function applyBgBlur(n) {
  n = Math.max(0, Math.min(30, Number(n) || 0));
  document.documentElement.style.setProperty("--bg-blur", String(n));
  localStorage.setItem("ha-bg-blur", String(n));
  var v = document.getElementById("bg-blur-val");
  if (v) v.textContent = n + "px";
  var r = document.getElementById("bg-blur-range");
  if (r) r.value = String(n);
}

function toggleAppearance() {
  var pop = document.getElementById("palette-pop");
  if (!pop) return;
  var on = pop.classList.contains("on");
  if (!pop.querySelector("button.cdot")) {
    var cur = localStorage.getItem("ha-color") || "sakura";
    var img = localStorage.getItem("ha-bg-img") || "";
    var blur = localStorage.getItem("ha-bg-blur") || "0";
    var dots = "";
    Object.keys(PALETTES_GUI).forEach((k) => {
      dots +=
        '<button class="cdot" data-c="' +
        k +
        '" title="' +
        k +
        '" style="background:' +
        PALETTES_GUI[k] +
        '" onclick="setColor(\'' +
        k +
        "')\"></button>";
    });
    pop.innerHTML =
      "<h4>" +
      __("主题色", "Theme color") +
      "</h4><div>" +
      dots +
      "</div>" +
      '<h4 style="margin-top:8px">' +
      __("背景图片 URL", "Background image URL") +
      "</h4>" +
      '<input type="text" id="bg-img-input" placeholder="https://...jpg / png" value="' +
      escHtml(img) +
      '">' +
      '<div class="pp-row"><button class="btn btn-ghost btn-sm" onclick="applyBgImg(document.getElementById(\'bg-img-input\').value)">' +
      __("应用", "Apply") +
      "</button>" +
      '<button class="btn btn-ghost btn-sm" onclick="pickBgFile()">' +
      __("本地图片…", "Local image…") +
      "</button>" +
      '<button class="btn btn-ghost btn-sm" onclick="applyBgImg(\'\')">' +
      __("清除", "Clear") +
      "</button>" +
      '<span class="pp-val">' +
      __("模糊", "Blur") +
      ' <input type="range" id="bg-blur-range" min="0" max="30" value="' +
      blur +
      '" oninput="applyBgBlur(this.value)">' +
      '<span id="bg-blur-val">' +
      blur +
      "px</span></span></div>";
    setColor(cur);
    var rr = document.getElementById("bg-blur-range");
    if (rr) rr.value = blur;
    var vv = document.getElementById("bg-blur-val");
    if (vv) vv.textContent = blur + "px";
  }
  pop.classList.toggle("on", !on);
}

(() => {
  var saved = localStorage.getItem("ha-theme");
  setTheme(saved || "light");
  var savedColor = localStorage.getItem("ha-color");
  if (savedColor) setColor(savedColor);
  var finalSrc = localStorage.getItem("ha-bg-final");
  var img = localStorage.getItem("ha-bg-img");
  var blur = localStorage.getItem("ha-bg-blur");
  if (img) {
    if (finalSrc) setBgImgVar(finalSrc);
    else applyBgImg(img);
  }
  if (blur) applyBgBlur(blur);
})();

// ===== Utility =====
// 安全渲染 markdown：marked 转 HTML 后由 DOMPurify 剥离脚本/事件/危险标签。
//
// ★ 净化器不可用时**不降级**，直接退到纯文本。
//
// 旧写法是在净化器缺失时掉到手写正则（剥 <script>/on*=/javascript:）再返回。
// 那不是完备的 HTML sanitizer：它漏掉的东西包括 <iframe srcdoc>、
// SVG 内联事件、data: URI、CSS url()/expression 等一整类。
// 而 renderMd 渲染的是**模型输出与记忆文本**——两者都是不可信输入。
// 让不可信输入绕过净化，比不显示 markdown 危险得多。
//
// 库已本地化（renderer/vendor/purify.min.js），正常走不到这个分支；
// 万一打包漏了文件，看到纯文本 + 控制台一条 error，远好过静默开一个 XSS 口子。
function renderMd(text) {
  if (typeof text !== "string") text = String(text || "");
  var html;
  if (typeof marked !== "undefined") {
    try { html = marked.parse(text); }
    catch (e) { html = "<pre>" + escHtml(text) + "</pre>"; }
  } else {
    html = "<pre>" + escHtml(text) + "</pre>";
  }
  if (typeof DOMPurify === "undefined" || typeof DOMPurify.sanitize !== "function") {
    return "<pre>" + escHtml(text) + "</pre>";
  }
  try { return DOMPurify.sanitize(html, { USE_PROFILES: { html: true } }); }
  catch (e) { return "<pre>" + escHtml(text) + "</pre>"; }
}

function escHtml(s) {
  return String(s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

function timeAgo(t) {
  var s = Math.floor((Date.now() - new Date(t).getTime()) / 1000);
  if (s < 60) return s + __("秒前", "s ago");
  var m = Math.floor(s / 60);
  if (m < 60) return m + __("分钟前", "min ago");
  return Math.floor(m / 60) + __("小时前", "h ago");
}

// 生成客户端唯一消息 ID（服务端据此去重，避免断线/重试重放）
function clientMsgId() {
  return (
    "cli-" +
    Date.now().toString(36) +
    "-" +
    Math.random().toString(36).slice(2, 8)
  );
}
// 最近一次发送的消息 ID（防重复重发提示用）
var lastClientMsgId = null;

function toast(m, isError, warn) {
  var t = document.getElementById("toast");
  t.textContent = m;
  var cls = "toast";
  if (isError) cls += " error";
  else if (warn) cls += " warn";
  else cls += " success";
  t.className = cls;
  t.style.display = "block";
  t.style.animation = "none";
  void t.offsetWidth;
  t.style.animation = "";
  clearTimeout(t._hideTimer);
  t._hideTimer = setTimeout(() => {
    t.style.display = "none";
  }, 3000);
}

// confirmDialog 替换原生 confirm()：玻璃态弹窗 + Esc/Enter/遮罩关闭，返回 Promise<boolean>
function confirmDialog(action, isDanger) {
  var o = document.getElementById("confirm-overlay");
  if (!o) return Promise.resolve(false);
  o.innerHTML =
    '<div class="confirm-box" role="alertdialog" aria-modal="true" aria-labelledby="confirm-title">' +
    '<h3 id="confirm-title">' +
    __("确认操作", "Confirm action") +
    "</h3>" +
    "<p>" +
    escHtml(action) +
    "</p>" +
    '<div class="confirm-actions">' +
    '<button class="btn btn-ghost" data-confirm="no">' +
    __("取消", "Cancel") +
    "</button>" +
    '<button class="btn ' +
    (isDanger ? "btn-danger" : "btn-primary") +
    '" data-confirm="yes">' +
    __("确认", "Confirm") +
    "</button>" +
    "</div></div>";
  o.style.display = "flex";
  o.querySelector('[data-confirm="no"]').focus();
  // 复用持久遮罩层的单一委托监听（首次挂载），避免每次调用累积监听器
  if (!o._bound) {
    o._bound = true;
    o.addEventListener("click", (e) => {
      if (o.style.display !== "flex") return;
      var btn = e.target.closest("[data-confirm]");
      if (btn) {
        var ok = btn.getAttribute("data-confirm") === "yes";
        o._close(ok);
      } else if (e.target === o) {
        o._close(false);
      }
    });
    document.addEventListener("keydown", (ev) => {
      if (o.style.display !== "flex") return;
      if (ev.key === "Escape") o._close(false);
      else if (ev.key === "Enter") o._close(true);
    });
  }
  return new Promise((resolve) => {
    o._close = (ok) => {
      o.innerHTML = "";
      o.style.display = "none";
      o._close = null;
      resolve(ok);
    };
  });
}

// ===== API =====
// ===== 系统通知 =====
//
// 何时通知：agent **完成一轮输出**时。
// 不在流式过程中逐 token 通知 —— 那是噪音（一轮可能几十个 delta），
// 用户要的是"有结果了"，不是"正在打字"。
//
// 去重：同一轮（signature 相同）只通知一次。SSE 有 tool_call / stage /
// agent_output 多个事件都可能判定"一轮结束"，不去重会连弹好几次。
//
// 前台可见且聚焦时不通知：用户正看着界面，弹窗只会烦人。
// 静默启动（窗口 hidden）正好走"要通知"这条 —— 这也是静默后
// 唯一能让用户知道"agent 回了"的途径。
function shouldNotify() {
  if (!state._notifySupported) return false;
  return true;
}

async function notifyTurn(title, body, msgKey) {
  if (!shouldNotify()) return;
  try {
    await window.homeagent.notify.show({
      title: title,
      body: body,
      msgKey: msgKey || "",
      silent: false,
    });
  } catch (e) {}
}

// notifyTurnOnce 保证同一轮只通知一次。
function notifyTurnOnce(sig, title, body, msgKey) {
  if (!shouldNotify()) return;
  if (!state._notifyState || state._notifyState.turnSig !== sig) {
    state._notifyState = { turnSig: sig, notified: false };
  }
  if (state._notifyState.notified) return;
  state._notifyState.notified = true;
  notifyTurn(title, body, msgKey);
}

// onNotifyClicked 点系统通知后：切到 chat 视图并滚到那条消息。
function onNotifyClicked() {
  try {
    switchView("chat");
    var el = document.querySelector(".msg-assistant:last-of-type");
    if (el && el.scrollIntoView) el.scrollIntoView({ block: "end" });
  } catch (e) {}
}

async function cliRequest(line) {
  var conn = state.currentConn;
  if (!conn) throw new Error(__("未选择连接", "No connection selected"));
  if (!window.homeagent || !window.homeagent.cli)
    throw new Error("cli bridge unavailable");
  var resp = await window.homeagent.cli.request(
    conn.socketPath || conn.url,
    conn.apiKey,
    line,
  );
  if (resp && resp.error) throw new Error(resp.error);
  return resp;
}

// CLI 传输映射：将 REST 路径转换为 cli 内置命令或直接对话
function cliMap(path, o) {
  o = o || {};
  var m = o.method || "GET";
  if (m === "POST" && path.indexOf("/chat") !== -1) {
    var body = {};
    try {
      body = JSON.parse(o.body || "{}");
    } catch (e) {}
    return cliRequest(body.message || "");
  }
  if (path === "/status") return cliRequest("/status");
  if (path === "/kernel") return cliRequest("/kernel");
  if (path === "/settings") return cliRequest("/settings");
  if (path === "/chat/history") return Promise.resolve({ messages: [] });
  return Promise.reject(
    new Error(__("CLI 连接不支持此功能", "Not supported on CLI connection")),
  );
}

// HTTP API 错误对象：携带 status/statusText/body，便于调用方按 HTTP 语义分支处理。
function ApiError(message, status, statusText, body) {
  this.name = "ApiError";
  this.message = message;
  this.status = status;
  this.statusText = statusText || "";
  this.body = body || null;
  this.stack = (new Error()).stack;
}
ApiError.prototype = Object.create(Error.prototype);

// deviceTokenPaths 列出走**设备接入令牌**鉴权的端点。
//
// 为什么要单独判：服务端 /api/v1/device/ 命名空间下并存**两套鉴权**——
//   · /device/gateway   注册在 webui 插件，走 requireAPI，认 session cookie
//   · /device/online、/device/、/device/push
//                        注册在 remotedevice 插件，走 requireToken，
//                        **只认 X-API-Key 头或 ?token=，完全不认 cookie**
// 所以只带 cookie 调后三者 ⇒ 401（实测：cookie 与错 token 的响应体
// 都是纯文本 "unauthorized"，而 webui 鉴权是 JSON {"error":...}，
// 可据此判定命中的是 requireToken）。
//
// 三种能工作的客户端做法一致：
//   · WebUI 浏览器：cookie 进内核，reverseToUpstream 里注入
//     deviceGatewayToken（handler_device.go:118）
//   · waiter：客户端自己带 X-API-Key（gateway_discover.go:103）
//   · GUI：两者都没做 ⇒ 设备页永远空白（本次修复的内容）
function isDeviceTokenPath(p) {
  return (
    p === "/device/online" ||
    p === "/device" ||
    p === "/device/push" ||
    p.indexOf("/device/") === 0
  ) && p !== "/device/gateway";
}

// deviceApiKey 取设备接入令牌。
//
// 优先用连接自身的 apiKey（为该连接显式配的管理员凭据）；
// 退回本机设备桥的 token（state.deviceBridgeToken，来自
// device-bridge:get，桥与 waiter 同源）——那才是 requireToken 要的那份。
// 两者都没有时返回空：此时发出去的请求会被服务端 401，属预期，
// 调用方负责给用户可读提示（见 deviceAuthHint）。
function deviceApiKey() {
  if (state.currentConn && state.currentConn.apiKey)
    return state.currentConn.apiKey;
  return state.deviceBridgeToken || "";
}

// deviceAuthHint 在设备接口 401 时拼一句可读原因。
//
// 为什么不能静默：此前 401 被 catch 吞掉并置空数组，界面上与
// 「确实没有设备」长得一模一样，用户无从判断是配置问题还是真没设备。
function deviceAuthHint() {
  if (state.currentConn && state.currentConn.apiKey) return "";
  if (state.deviceBridgeToken) return "";
  return __(
    "设备接口需要设备接入令牌：请在「设置 → 设备」里配置设备桥网关与令牌，或给该连接填 API Key。",
    "Device endpoints require a device token: configure the device bridge gateway and token in Settings → Devices, or set an API Key on this connection.",
  );
}

async function api(p, o) {
  if (!state.currentConn)
    throw new Error(__("未选择连接", "No connection selected"));
  if (state.currentConn.type === "cli") {
    return cliMap(p, o);
  }
  var opts = o || {};
  var to = opts.timeout || 8000;
  var headers = { "Content-Type": "application/json", ...(opts.headers || {}) };
  if (state.currentConn.apiKey) headers["X-API-Key"] = state.currentConn.apiKey;
  // 设备接入面需要设备令牌而非管理员会话 cookie（见 isDeviceTokenPath 注释）。
  if (!headers["X-API-Key"] && isDeviceTokenPath(p)) {
    var devKey = deviceApiKey();
    if (devKey) headers["X-API-Key"] = devKey;
  }
  var ctl = new AbortController();
  var timer = setTimeout(() => {
    ctl.abort();
  }, to);
  var r;
  try {
    r = await fetch(state.currentConn.url + "/api/v1" + p, {
      ...opts,
      headers: headers,
      signal: ctl.signal,
    });
  } catch (e) {
    clearTimeout(timer);
    throw new Error(__("连接超时或失败", "Timeout or connection failed"));
  }
  clearTimeout(timer);
  if (r.status === 401) {
    // 认证失败：尝试自动重新登录一次（避免 cookie 过期后界面持续报错）
    if (window._haReloginLock) throw new ApiError(__("认证失败", "unauthorized"), 401, r.statusText, null);
    window._haReloginLock = true;
    try {
      await syncConnAuth();
      // ★ 原来固定 setTimeout(…, 800)：认证过期时**每个**请求都白等 0.8s，
      //   并发几个请求就叠加成明显的「卡」。syncConnAuth 本身是 await 的，
      //   它返回即代表凭据已就绪，无需再额外空等。
      //   留 30ms 让 setAuth 的 cookie 落盘，避免极端情况下仍用旧凭据。
      await new Promise((res2) => setTimeout(res2, 30));
    } catch (e2) {}
    // ★★ 锁必须在**递归返回之后**才释放。
    //
    // 原写法在递归前 `window._haReloginLock = false` ⇒ 每层递归进来看到的
    // 都是「没人在重登」⇒ 无限自我递归。真机实测（Electron + CDP）：
    // fetch 被调 13 次、重登 12 次才被上限截断。
    //
    // 而且这与「800ms → 30ms」那次优化直接相关：原来只是每 800ms 慢速空转，
    // 改完变成每 30ms 快速烧 CPU 并反复打服务端 —— **优化放大了这个 bug**。
    //
    // try/finally 保证：递归正常返回后锁才释放（下次真实 401 仍可重登），
    // 递归抛错时也一定释放（不会把后续所有请求都锁死成直接 401）。
    try {
      return await api(p, o);
    } finally {
      window._haReloginLock = false;
    }
  }
  if (r.status === 408 || r.status === 504) {
    // 网关超时：API 层直接抛错，调用方可选择提示用户重试或自动降级
    throw new ApiError(__("请求超时", "Request timeout"), r.status, r.statusText, null);
  }
  if (r.status === 429) {
    // 限流：抛错 + 建议调用方退避
    var retryAfter = r.headers.get("Retry-After");
    throw new ApiError(
      __("请求过于频繁，请稍后重试", "Rate limited, please retry later"),
      429,
      r.statusText,
      retryAfter ? { retryAfterSeconds: parseInt(retryAfter, 10) } : null,
    );
  }
  if (opts.raw) return r;
  var body = await r.text();
  // 网关会话过期：返回 200 但内容为登录页 HTML —— 自动重登后重试
  if (
    body.indexOf("THEME_PLACEHOLDER") !== -1 ||
    body.indexOf("统一门户登录") !== -1 ||
    (body.indexOf("<title>") !== -1 && body.indexOf("login") !== -1)
  ) {
    if (window._haReloginLock) throw new ApiError(__("认证失败", "unauthorized"), r.status, r.statusText, body);
    window._haReloginLock = true;
    try {
      await syncConnAuth();
      // ★ 原来固定 setTimeout(…, 800)：认证过期时**每个**请求都白等 0.8s，
      //   并发几个请求就叠加成明显的「卡」。syncConnAuth 本身是 await 的，
      //   它返回即代表凭据已就绪，无需再额外空等。
      //   留 30ms 让 setAuth 的 cookie 落盘，避免极端情况下仍用旧凭据。
      await new Promise((res2) => setTimeout(res2, 30));
    } catch (e2) {}
    // ★★ 同上：锁在递归返回后才释放（递归前清锁 = 无限重试）
    try {
      return await api(p, o);
    } finally {
      window._haReloginLock = false;
    }
  }
  // 非 2xx 状态码：解包 server error + 以 ApiError 抛出，调用方按 status 分支处理
  if (!r.ok) {
    var errMsg = body;
    try {
      var parsed = JSON.parse(body);
      if (parsed.error) errMsg = parsed.error;
      else if (parsed.message) errMsg = parsed.message;
    } catch (e) {}
    throw new ApiError(
      __("请求失败: ", "Request failed: ") + errMsg,
      r.status,
      r.statusText,
      body,
    );
  }
  var ct = r.headers.get("content-type") || "";
  if (ct.includes("json")) {
    try {
      return JSON.parse(body);
    } catch (e3) {
      return body;
    }
  }
  return body;
}

// 从服务端发现设备网关地址（自动链接的权威来源）。
//
// 失败不报错：老版本 HologramAgent 没有这个端点，回退到本地推导即可
// （见 renderDeviceChannel 里的 state.discoveredGateway || 旧口径）。
//
// 优先 url_portal（门户同源形态）：GUI 主进程连 WS 走系统解析器，
// devices.localhost 这类子域在系统解析器下通常解析不到 —— *.localhost
// 是浏览器内置特例（RFC 6761），不适用于普通进程。实测确认。
async function loadDiscoveredGateway() {
  if (!state.currentConn || state.currentConn.type !== "webui") {
    state.discoveredGateway = "";
    return;
  }
  // 已有值则跳过：网关地址在一次会话内基本不变，
  // 而 refreshAll 会被 15s 定时器反复触发。
  if (state.discoveredGateway) return;
  try {
    var d = await api("/device/gateway");
    state.discoveredGateway =
      (d && d.available && (d.url_portal || d.url)) || "";
    if (state.discoveredGateway) {
      console.log("[device-bridge] discovered gateway: " + state.discoveredGateway);
      // ★ 回写到主进程，让设备桥**真的**用上发现结果。
      //
      // 此前发现只停在state.discoveredGateway（UI 展示用），而主进程的
      // startDeviceBridge 读的是 gui-prefs.deviceBridge.gateway ——
      // 两者从不联通。症状：GUI 面板里能看到设备通道地址，但设备桥连不上，
      // 而日志只有一行 ws cookie len=0，没有任何“地址来自发现”的痕迹。
      //
      // 仅当本地未配置 gateway 时才写：手填的值优先（用户可能故意指向
      // 内网直连或灰度实例），发现只做**缺省补全**。
      if (window.homeagent && window.homeagent.deviceBridge) {
        try {
          var cur = await window.homeagent.deviceBridge.get();
          if (cur && !cur.gateway && state.discoveredGateway) {
            await window.homeagent.deviceBridge.set({ gateway: state.discoveredGateway });
            console.log(
              "[device-bridge] gateway auto-configured from discovery: " +
                state.discoveredGateway,
            );
          }
        } catch (e2) {
          console.log("[device-bridge] auto-config skipped: " + e2.message);
        }
      }
    }
  } catch (e) {
    state.discoveredGateway = "";
  }
}

// ===== Navigation =====
function switchView(n) {
  document.querySelectorAll(".view").forEach((e) => {
    e.classList.remove("active");
  });
  var el = document.getElementById("view-" + n);
  if (el) el.classList.add("active");
  document.querySelectorAll(".rail-btn").forEach((e) => {
    e.classList.remove("active");
  });
  var rb = document.getElementById("rail-" + n);
  if (rb) rb.classList.add("active");
  state.currentView = n;
  if (n === "chat") {
    state.chatStick = true;
    var scrollToBottom = () => {
      var msgsEl = document.getElementById("chat-msgs");
      if (!msgsEl) return;
      try {
        msgsEl.scrollTop = msgsEl.scrollHeight;
      } catch (e) {}
    };
    // 立即滚 + 渲染完成后/延迟再滚（DOM 重建后会重置滚动位置）
    scrollToBottom();
    setTimeout(scrollToBottom, 60);
    setTimeout(scrollToBottom, 300);
  }
  renderAll();
  if (n === "chat") {
    // renderAll（含异步 refreshAll）完成后确保仍在底部
    setTimeout(scrollToBottom, 600);
    setTimeout(scrollToBottom, 1500);
  }
}

// ===== Tab Render Dispatch =====
async function doRenderAll() {
  // 如果连接表单正在显示（用户正在编辑），跳过全量刷新，避免擦掉用户输入
  var connForm = document.getElementById("conn-form");
  if (connForm && connForm.style.display === "block") {
    // 仅刷新数据，不重建 DOM
    await refreshDataOnly();
    return;
  }
  await refreshAll();
}

function renderAll() {
  try {
    renderOverview();
  } catch (e) {
    console.error("renderOverview", e);
  }
  try {
    // 聊天视图未激活时不重建聊天 DOM（避免每次轮询/切换全量重建→卡死）
    if (!state.currentView || state.currentView === "chat") {
      renderChat();
      renderChatStarmap();
    }
  } catch (e) {
    console.error("renderChat", e);
  }
  try {
    renderPlugins();
  } catch (e) {
    console.error("renderPlugins", e);
  }
  try {
    renderKernel();
  } catch (e) {
    console.error("renderKernel", e);
  }
  try {
    renderOneSettings();
  } catch (e) {
    console.error("renderOneSettings", e);
  }
  try {
    renderAdapters();
  } catch (e) {
    console.error("renderAdapters", e);
  }
  try {
    renderDevices();
  } catch (e) {
    console.error("renderDevices", e);
  }
  applyI18n();
  applyCardTilt();
  refreshAll();
}

// ensureDeviceToken 拉取设备桥配置并缓存设备令牌。
//
// ★ 必须在任何 /device/* 请求**之前**调用：那些端点走服务端
// requireToken，只认 X-API-Key；令牌不在手上就必然 401。
// （此前令牌加载排在 /device/online 之后，顺序正好反了。）
async function ensureDeviceToken() {
  try {
    if (!window.homeagent || !window.homeagent.deviceBridge) return "";
    var dbinfo = await window.homeagent.deviceBridge.get();
    state.dbConfig = dbinfo || state.dbConfig;
    state.deviceBridgeToken = (dbinfo && dbinfo.token) || "";
    // 顺带按需发现网关地址：设备通道配置（renderDevices 的 webuiUrl）
    // 依赖 state.discoveredGateway，而它是服务端配置、客户端无从推导。
    // loadDiscoveredGateway 自带缓存，这里不会反复请求。
    try {
      await loadDiscoveredGateway();
    } catch (e) {}
    return state.deviceBridgeToken;
  } catch (e) {
    return "";
  }
}

async function refreshDataOnly() {
  // 仅刷新 state 数据，不重建 DOM（用于定时轮询时避免擦掉用户输入）
  try {
    var s = await api("/status");
    state.status = s;
    state.startedAt = s.startedAt ? new Date(s.startedAt).getTime() : null;
    updateConnIndicator();
  } catch (e) {}
  try {
    state.kernel = await api("/kernel");
  } catch (e) {}
  try {
    // 运行态快照：只给指标与队列/阶段展示用，不影响其他卡片。
    state.runtime = await api("/runtime");
  } catch (e) {}
  try {
    var s = await api("/settings");
    state.settings = s.settings || {};
    state.meta = s.meta || {};
    state.settingsPlugins = s.plugins || ["core"];
    state.pluginMeta = s.plugin_meta || {};
    state.disabledPlugins = s.disabled_plugins || [];
  } catch (e) {}
  try {
    state.installedPlugins = await api("/plugins");
  } catch (e) {}
  try {
    await loadTerminals();
  } catch (e) {}
  try {
    await loadCmdHistory();
  } catch (e) {}
  try {
    if (
      state.currentConn &&
      state.currentConn.type === "webui" &&
      state.currentConn.url
    ) {
      // 令牌必须先到位，否则 /device/online 必 401（见 ensureDeviceToken）。
      await ensureDeviceToken();
      var d = await api("/device/online");
      state.devices = (d && d.devices) || [];
      state.deviceAuthError = "";
    } else {
      state.devices = [];
    }
  } catch (e) {
    // 静默置空会把「没鉴权」显示成「没设备」，两者界面上无法区分。
    state.devices = [];
    state.deviceAuthError = e && e.status === 401 ? deviceAuthHint() : "";
  }
  try {
    try {
      if (window.homeagent && window.homeagent.displays) {
        state.displays = (await window.homeagent.displays.list()) || [];
      }
    } catch (e) {}
    try {
      if (window.homeagent && window.homeagent.audio) {
        state.audioDevices = (await window.homeagent.audio.list()) || [];
      }
    } catch (e) {}
    if (window.homeagent && window.homeagent.deviceBridge) {
      var dbinfo = await window.homeagent.deviceBridge.get();
      state.dbConfig = dbinfo || state.dbConfig;
      if (dbinfo && dbinfo.enabled) {
        if (dbinfo.deviceId) {
          state.selfDeviceId = dbinfo.deviceId;
          state.selfGateway = dbinfo.address || state.selfGateway;
        }
        if (
          state.devices.length === 0 &&
          dbinfo.gateway &&
          window.homeagent &&
          window.homeagent.device
        ) {
          try {
            var eb = await window.homeagent.device.identity();
            if (eb && eb.device_id) {
              state.selfDeviceId = eb.device_id;
              state.selfGateway = eb.address || dbinfo.gateway;
            }
          } catch (e2) {}
        }
      }
    }
  } catch (e) {}
}

async function refreshAll() {
  try {
    var s = await api("/status");
    state.status = s;
    state.startedAt = s.startedAt ? new Date(s.startedAt).getTime() : null;
    updateConnIndicator();
  } catch (e) {}
  try {
    state.kernel = await api("/kernel");
  } catch (e) {}
  try {
    // 运行态快照（调度器/驻留子/通道），供总览的运行态面板使用。
    state.runtime = await api("/runtime");
  } catch (e) {}
  try {
    var s = await api("/settings");
    state.settings = s.settings || {};
    state.meta = s.meta || {};
    state.settingsPlugins = s.plugins || ["core"];
    state.pluginMeta = s.plugin_meta || {};
    state.disabledPlugins = s.disabled_plugins || [];
  } catch (e) {}
  try {
    state.installedPlugins = await api("/plugins");
  } catch (e) {}
  try {
    await loadTerminals();
  } catch (e) {}
  try {
    await loadCmdHistory();
  } catch (e) {}
  try {
    // 设备列表：webui 连接时经 webui 反代 /api/v1/device/online 拉取（反代只挂 /api/v1/device/ 前缀）
    if (
      state.currentConn &&
      state.currentConn.type === "webui" &&
      state.currentConn.url
    ) {
      // 网关发现**不在**这里做：doRenderAll 每 15s 会走到 refreshAll，
      // 而网关地址是服务端配置（慢变量），15s 拉一次纯属浪费。
      // 它由 ensureDeviceToken 同批按需拉取（下面 deviceRefresh/切连接时），
      // 且有缓存：discoveredGateway 一旦拿到就不重复请求。
      await ensureDeviceToken();
      var d = await api("/device/online");
      state.devices = (d && d.devices) || [];
      state.deviceAuthError = "";
    } else {
      state.devices = [];
    }
  } catch (e) {
    state.devices = [];
    state.deviceAuthError = e && e.status === 401 ? deviceAuthHint() : "";
  }
  try {
    // 本机显示器列表（screensue 默认屏幕配置用）
    try {
      if (window.homeagent && window.homeagent.displays) {
        state.displays = (await window.homeagent.displays.list()) || [];
      }
    } catch (e) {}
    // 本机音频输出设备列表（speakeruse 声卡选择用）
    try {
      if (window.homeagent && window.homeagent.audio) {
        state.audioDevices = (await window.homeagent.audio.list()) || [];
      }
    } catch (e) {}
    // 本机设备桥身份：设备桥由 gui-prefs 驱动，独立于当前连接类型
    if (window.homeagent && window.homeagent.deviceBridge) {
      var dbinfo = await window.homeagent.deviceBridge.get();
      state.dbConfig = dbinfo || state.dbConfig;
      if (dbinfo && dbinfo.enabled) {
        if (dbinfo.deviceId) {
          state.selfDeviceId = dbinfo.deviceId;
          state.selfGateway = dbinfo.address || state.selfGateway;
        }
        // 本机授权状态以客户端本地为准（服务端自报值仅展示）
        if (typeof dbinfo.authorized === "boolean") {
          var found = false;
          for (var di = 0; di < state.devices.length; di++) {
            if (state.devices[di].device_id === dbinfo.deviceId) {
              state.devices[di].authorized = dbinfo.authorized;
              found = true;
              break;
            }
          }
          if (!found && dbinfo.authorized) {
            // 服务端列表未含本机（可能离线），仍展示本地状态
            state.devices.push({
              device_id: dbinfo.deviceId,
              name: "HologramAgent GUI",
              kind: "computer",
              authorized: dbinfo.authorized,
              online: false,
              caps: [],
            });
          }
        }
        // 若尚未有设备列表且已启用设备桥但非 device 连接，尝试经设备桥网关拉取
        if (
          state.devices.length === 0 &&
          dbinfo.gateway &&
          window.homeagent &&
          window.homeagent.device
        ) {
          try {
            var eb = await window.homeagent.device.identity();
            if (eb && eb.device_id) {
              state.selfDeviceId = eb.device_id;
              state.selfGateway = eb.address || dbinfo.gateway;
            }
          } catch (e2) {}
        }
      }
    }
  } catch (e) {}
  try {
    renderOverview();
  } catch (e) {
    console.error("renderOverview", e);
  }
  try {
    // 聊天视图未激活时不重建聊天 DOM（避免每次轮询/切换全量重建→卡死）
    if (!state.currentView || state.currentView === "chat") {
      renderChat();
      renderChatStarmap();
    }
  } catch (e) {
    console.error("renderChat", e);
  }
  try {
    renderPlugins();
  } catch (e) {
    console.error("renderPlugins", e);
  }
  try {
    renderKernel();
  } catch (e) {
    console.error("renderKernel", e);
  }
  try {
    renderOneSettings();
  } catch (e) {
    console.error("renderOneSettings", e);
  }
  try {
    renderAdapters();
  } catch (e) {
    console.error("renderAdapters", e);
  }
  try {
    renderDevices();
  } catch (e) {
    console.error("renderDevices", e);
  }
  // ★ 协议对齐补齐的 6 个端点（2026-09-28）。
  //
  //   GUI 原来只用 22 个端点，服务端有 47 个。缺的这些**都是只读诊断类**，
  //   接上它们的价值是"不用切到别的工具就能看到"。
  //
  // ★ 只加进 refreshAll，**不加** refreshDataOnly：后者每 15 秒一轮，
  //   6 个端点虽只 +3ms（实测），但没必要为诊断数据高频轮询。
  await refreshDiagData();
  try {
    renderOverview();
    renderPersona();
    renderProxy();
  } catch (e) {
    console.error("render diag panels", e);
  }
  applyI18n();
  applyCardTilt();
}

// refreshDiagData 拉取协议对齐补齐的诊断类端点。
//
// 刻意每个都独立 try/catch：某个端点挂了（如老版本内核没有该路由），
// 不该拖垮整轮刷新 —— 与既有 6 个 loader 的写法一致。
async function refreshDiagData() {
  try {
    state.agents = await api("/agents");
  } catch (e) {}
  try {
    state.network = await api("/network");
  } catch (e) {}
  try {
    state.tracker = await api("/tracker");
  } catch (e) {}
  try {
    state.runConfig = await api("/config");
  } catch (e) {}
  try {
    state.persona = await api("/persona");
  } catch (e) {}
  try {
    state.proxy = await api("/proxy");
  } catch (e) {}
  try {
    state.proxyServices = await api("/proxy/services");
  } catch (e) {}
}

// ===== 动效补齐：卡片 3D tilt + 光标光斑（事件委托，动态渲染后自动生效） =====
function applyCardTilt() {
  if (!window.matchMedia || window.matchMedia("(hover: none)").matches) return;
  if (!document.body._tiltApplied) {
    document.body._tiltApplied = true;
    document.addEventListener("mousemove", onCardTiltMove);
    document.addEventListener("mouseleave", (e) => {
      var card = e.target.closest && e.target.closest(".card.tilt");
      if (card) card.style.transform = "";
    });
  }
  // 给概览/插件/内核三类页面的卡片补上 tilt-glow 子元素并启用 tilt
  var holders = ["#view-overview", "#view-plugins", "#view-kernel"];
  holders.forEach((sel) => {
    var root = document.querySelector(sel);
    if (!root) return;
    root.querySelectorAll(".card").forEach((c) => {
      if (c.classList.contains("tilt")) return;
      if (!c.querySelector(".tilt-glow")) {
        var g = document.createElement("span");
        g.className = "tilt-glow";
        c.appendChild(g);
      }
      c.classList.add("tilt");
    });
  });
}

function onCardTiltMove(e) {
  var card = e.target.closest ? e.target.closest(".card.tilt") : null;
  if (!card) return;
  var r = card.getBoundingClientRect();
  if (r.width === 0 || r.height === 0) return;
  card.style.setProperty("--mx", e.clientX - r.left + "px");
  card.style.setProperty("--my", e.clientY - r.top + "px");
  var rx = ((e.clientY - r.top) / r.height - 0.5) * -4;
  var ry = ((e.clientX - r.left) / r.width - 0.5) * 4;
  card.style.transform =
    "perspective(1000px) rotateX(" +
    rx.toFixed(2) +
    "deg) rotateY(" +
    ry.toFixed(2) +
    "deg) translateY(-1px)";
}

function fmtUptime(ms) {
  var s = Math.floor(ms / 1000);
  if (s < 60) return s + "s";
  var m = Math.floor(s / 60);
  s = s % 60;
  if (m < 60) return m + "m " + s + "s";
  var h = Math.floor(m / 60);
  m = m % 60;
  return h + "h " + m + "m " + s + "s";
}

var uptimeTick = null;
var _chatSyncTick = null;
function startUptimeTicker() {
  if (uptimeTick) clearInterval(uptimeTick);
  uptimeTick = setInterval(() => {
    var el = document.querySelector("#uptime-val");
    if (el && state.startedAt) {
      var now = Date.now();
      el.textContent = fmtUptime(now - state.startedAt);
    } else if (!state.startedAt) {
      var el2 = document.querySelector("#uptime-val");
      if (el2) el2.textContent = "-";
    }
  }, 1000);
  // 消息同步轮询兜底：每30秒增量同步 chatHistory，补偿 SSE 断连窗口期
  // 丢失的事件（尤其是非 GUI 触发的跨渠道消息，如 CLI/QQ/设备桥输出）。
  // syncChatFromHistory 增量同步，不重建已有消息 DOM，无闪烁。
  if (_chatSyncTick) clearInterval(_chatSyncTick);
  _chatSyncTick = setInterval(() => {
    if (state.currentConn && state.currentConn.type !== "cli") {
      syncChatFromHistory().catch(() => {});
    }
  }, 30000);
}

// ===== Overview =====
function statCard(l, v) {
  return (
    '<div class="card stat-card"><div class="stat-value">' +
    v +
    '</div><div class="stat-label">' +
    l +
    "</div></div>"
  );
}

// ===== 运行态面板：阶段管道 + 中断队列 =====
//
// 与 WebUI 总览**同一套设计语言：等大表框**。此前桌面版总览只有四个数字卡，
// 既看不到「这一轮走到哪一步」，也看不到四级中断队列的积压。
// 数据来自 /api/v1/runtime（KernelStatus 的运行态子集）。
var RT_LEVELS = [
  { lv: 4, name: "L4", zh: "内核独占", en: "kernel only", cls: "rt-lv-4" },
  { lv: 3, name: "L3", zh: "交互", en: "interactive", cls: "rt-lv-3" },
  { lv: 2, name: "L2", zh: "消息", en: "message", cls: "rt-lv-2" },
  { lv: 1, name: "L1", zh: "后台", en: "background", cls: "rt-lv-1" },
];
// 七阶段归并成五格（与内核 sdk.Stage 的顺序一致）：
// 一轮里工具调用会反复回到「行动后」，线性滑块本身就是错的表述，
// 所以画成 输入 → 行动 ⇄(工具) → 输出 → 结束，工具那格带循环标记。
var RT_PIPE_GROUPS = [
  { zh: "输入", en: "in", ico: "in" },
  { zh: "行动", en: "act", ico: "act" },
  { zh: "工具", en: "tool", ico: "tool", loop: true },
  { zh: "输出", en: "out", ico: "out" },
  { zh: "结束", en: "done", ico: "done" },
];
// 图标一律内联 SVG（24x24 / currentColor），不用 emoji/符号字符充当图标。
var RT_ICO = {
  in: '<svg class="rt-ico" viewBox="0 0 24 24"><path d="M21 12H8"/><path d="M13 6l-6 6 6 6"/></svg>',
  act: '<svg class="rt-ico" viewBox="0 0 24 24"><circle cx="12" cy="12" r="3.2"/><path d="M12 2v3M12 19v3M2 12h3M19 12h3M5.5 5.5l2.1 2.1M16.4 16.4l2.1 2.1M18.5 5.5l-2.1 2.1M7.6 16.4l-2.1 2.1"/></svg>',
  tool: '<svg class="rt-ico" viewBox="0 0 24 24"><path d="M14.5 6.5a3.8 3.8 0 0 1 5 5L10 21l-5-5z"/><path d="M14.5 6.5 17.5 9.5"/></svg>',
  out: '<svg class="rt-ico" viewBox="0 0 24 24"><path d="M4 12h13"/><path d="M13 6l6 6-6 6"/></svg>',
  done: '<svg class="rt-ico" viewBox="0 0 24 24"><path d="M20 6 9 17l-5-5"/></svg>',
  loop: '<svg class="rt-ico" viewBox="0 0 24 24"><path d="M20.5 12a8.5 8.5 0 1 1-2.5-6"/><path d="M21 3.5V9h-5.5"/></svg>',
};

function rtPhaseGroup(phase) {
  switch (phase) {
    case "on_input":
      return 0;
    case "pre_action":
    case "post_action":
      return 1;
    case "before_toolcall":
    case "after_toolcall":
      return 2;
    case "before_output":
      return 3;
    case "after_output":
      return 4;
  }
  return -1;
}
function rtShortTool(name) {
  var n = String(name || "");
  var i = n.lastIndexOf("__");
  if (i >= 0) n = n.slice(i + 2);
  return n.length > 14 ? n.slice(0, 13) + "…" : n;
}
// rtSlots 画一组「车位」式格槽：槽位数量固定可见，被占用的点亮。
// 为什么不用进度条：队列为 0 时进度条宽度就是 0，整行只剩文字，看上去就是「这块空着」。
function rtSlots(depth, slots, cls) {
  var n = Math.max(5, Math.min(16, slots || 5));
  var d = depth || 0;
  var out = '<span class="rt-slots ' + (cls || "") + '">';
  for (var i = 0; i < n; i++) out += '<i class="' + (i < d ? "on" : "") + '"></i>';
  // 溢出计数必须留在 .rt-slots 内：格槽是 flex 行，多一个兄弟节点会被挤出去
  if (d > n) out += '<b class="rt-slots-more">+' + (d - n) + "</b>";
  return out + "</span>";
}
// rtTrailPush 把一条「本轮发生过的事」落到它实际发生的阶段列里；
// 同一阶段重复的同一条（如同一工具连调 3 次）只累加计数，不刷屏。
function rtTrailPush(g, kind, label, short) {
  if (!state.stageTrail) state.stageTrail = [];
  var arr = state.stageTrail;
  var last = arr.length ? arr[arr.length - 1] : null;
  if (last && last.g === g && last.kind === kind && last.short === short) {
    last.n = (last.n || 1) + 1;
    return;
  }
  arr.push({ g: g, kind: kind, label: label, short: short, n: 1 });
  if (arr.length > 24) arr.shift();
}

function renderRuntimePanel() {
  var title = __("运行态", "Runtime");
  var rt = state.runtime;
  // 工具格的滑入动画只在「新到一条工具调用」那一次播放：overview 是整块
  // innerHTML 重建，节点每次都是新的；若无条件带动画类，任何重渲染都会闪一下。
  var toolFlash = !!state.toolFlash;
  state.toolFlash = false;
  if (!rt) {
    return (
      '<div class="card"><h2>' + title + '</h2><p class="rt-empty">' +
      __("运行态数据不可用", "runtime unavailable") + "</p></div>"
    );
  }
  var sc = rt.scheduler || {};
  var q = sc.interrupt_queues || [0, 0, 0, 0, 0];
  var pending = sc.pending_interrupts || 0;
  var ready = sc.ready_queue_depth || 0;
  var stack = sc.suspend_stack || 0;
  var maxStack = sc.max_suspend_depth || 4;
  var residents = rt.residents || [];
  var byLv = sc.interrupts_by_level || [];
  var preLv = sc.preempts_by_level || [];
  var maxQ = Math.max(1, ready, q[1] || 0, q[2] || 0, q[3] || 0, q[4] || 0);
  // 至少 5 格：0 时也有可见形状
  var qSlots = Math.max(5, Math.min(16, maxQ));

  var html = '<div class="card"><h2>' + title + "</h2>";
  // 四个数字块（沿用本 app 的 statCard 风格）
  html +=
    '<div class="grid-4">' +
    statCard(__("排队", "Ready"), ready, "") +
    statCard(__("中断", "Pending"), pending, "") +
    statCard(__("栈", "Stack"), stack + "/" + maxStack, "") +
    statCard(__("子代理", "Subagents"), residents.length, "") +
    "</div>";

  // ---- 阶段管道：五个等大表框 ----
  var g = rtPhaseGroup(state.pipelinePhase || "");
  html +=
    '<div class="rt-section-title">' + __("阶段管道", "Stage pipeline") +
    (g < 0 ? "　" + __("（空闲）", "(idle)") : "") + "</div>";
  html += '<div class="rt-pipe-row' + (g < 0 ? " rt-pipe-idle" : "") + '">';
  html += RT_PIPE_GROUPS.map((s, i) => {
    var items = (state.stageTrail || []).filter((t) => (t.g | 0) === i);
    // 「工具」是循环格：一轮里可能调几十次工具/输出通道，全部追加会把这一格
    // 撑成长条，反而看不出「现在在调什么」。只留**最新一条**，右侧给本轮累计
    // 次数（与 WebUI 同一口径，见 internal/plugins/webui/dashboard.js）。
    var cls = "rt-pipe-events";
    var body;
    if (!items.length) {
      body = '<span class="rt-chip rt-chip-none">' + __("无", "none") + "</span>";
    } else if (s.loop) {
      cls += " rt-pipe-scroll";
      var total = 0;
      for (var k = 0; k < items.length; k++) total += items[k].n || 1;
      var latest = items[items.length - 1];
      var lkind = latest.kind || "stage";
      var lico = lkind === "output" ? RT_ICO.out : lkind === "tool" ? RT_ICO.tool : "";
      body =
        '<span class="rt-chip rt-chip-' + lkind + (toolFlash ? " rt-chip-enter" : "") +
        '" title="' + escHtml(latest.label) + '">' + lico +
        '<b class="rt-chip-t">' + escHtml(latest.short || latest.label) + "</b>" +
        (latest.n > 1 ? '<i class="rt-chip-n">x' + latest.n + "</i>" : "") +
        "</span>" +
        '<i class="rt-scroll-count" title="' +
        __("本轮工具调用累计次数", "tool calls this turn") + '">x' + total + "</i>";
    } else {
      body = items
        .map((t) => {
          var kind = t.kind || "stage";
          var ico =
            kind === "output" ? RT_ICO.out : kind === "tool" ? RT_ICO.tool : "";
          return (
            '<span class="rt-chip rt-chip-' + kind + '" title="' +
            escHtml(t.label) + '">' + ico +
            '<b class="rt-chip-t">' + escHtml(t.short || t.label) + "</b>" +
            (t.n > 1 ? '<i class="rt-chip-n">x' + t.n + "</i>" : "") +
            "</span>"
          );
        })
        .join("");
    }
    return (
      '<div class="rt-pipe-cell' + (i === g ? " active" : "") + '">' +
      '<div class="rt-pipe-head">' + RT_ICO[s.ico] +
      "<b>" + __(s.zh, s.en) + "</b>" +
      (s.loop
        ? '<em class="rt-loop" title="' +
          __("工具调用会回到行动后，可多次", "tool calls loop back; may repeat") +
          '">' + RT_ICO.loop + "</em>"
        : "") +
      '</div><div class="' + cls + '">' + body + "</div></div>"
    );
  }).join("");
  html += "</div>";

  // ---- 中断队列：五个等大表框（L4/L3/L2/L1 + 排队）----
  html += '<div class="rt-section-title">' + __("队列", "Queues") + "</div>";
  html += '<div class="rt-queues">';
  RT_LEVELS.forEach((L) => {
    var depth = q[L.lv] || 0;
    var reg = byLv[L.lv] || 0;
    var pre = preLv[L.lv] || 0;
    var desc = __(L.zh, L.en);
    html +=
      '<div class="rt-qcell ' + L.cls + (depth ? " rt-active" : "") +
      '" title="' + escHtml(desc) + '">' +
      '<div class="rt-qhead"><b>' + L.name + "</b><span>" + escHtml(desc) + "</span></div>" +
      '<div class="rt-qnum">' + depth + "</div>" +
      rtSlots(depth, qSlots, L.cls) +
      '<div class="rt-qmeta">' + reg + " " + __("登记", "reg") + " · " +
      pre + " " + __("抢占", "pre") + "</div></div>";
  });
  // 排队队列无级别：用虚线框与四级中断区分（另一**类别**，不是另一优先级）
  html +=
    '<div class="rt-qcell rt-qcell-queued rt-lv-q' + (ready ? " rt-active" : "") +
    '" title="' + __("排队（无级别，纯 FIFO）", "queued (no priority, FIFO)") + '">' +
    '<div class="rt-qhead"><b>' + __("排队", "queued") + "</b><span>FIFO</span></div>" +
    '<div class="rt-qnum">' + ready + "</div>" +
    rtSlots(ready, qSlots, "rt-lv-q") +
    '<div class="rt-qmeta">' + __("无级别", "no priority") + "</div></div>";
  html += "</div></div>";
  return html;
}

// 开源许可卡：协议标识 + 协议全文 + 源码仓库。
// AGPL-3.0 §13 的义务是「向网络使用者提供取得 Corresponding Source 的机会」——
// 只给一个仓库链接、不写协议名，使用者看不出这受什么许可约束。
function renderLegalCard() {
  var b = ((state.kernel || {}).build) || {};
  var src = b.source_url || "";
  var lic = b.license || "";
  var licURL = b.license_url || "";
  if (!lic && !src) return "";
  function row(key, val) {
    return (
      '<div class="kv-row"><span class="key">' + escHtml(key) +
      '</span><span class="val">' + val + "</span></div>"
    );
  }
  function a(href, text) {
    return (
      '<a href="' + escHtml(href) +
      '" target="_blank" rel="noopener noreferrer">' + escHtml(text) + "</a>"
    );
  }
  var rows = "";
  if (lic) rows += row(__("许可协议", "License"), licURL ? a(licURL, lic) : escHtml(lic));
  if (src) rows += row(__("源码仓库", "Source"), a(src, src));
  // 网络条款只在 AGPL 系的许可下才成立，所以按标识判断，不硬写协议名。
  var note =
    lic && lic.toUpperCase().indexOf("AGPL") >= 0
      ? '<p class="rt-empty">' +
        __(
          "网络服务条款（§13）：把修改后的版本作为网络服务对外提供时，必须向使用者提供取得对应源码的途径。",
          "Network clause (section 13): offering a modified version as a network service requires giving users a way to obtain the Corresponding Source.",
        ) +
        "</p>"
      : "";
  return '<div class="card"><h2>' + __("开源许可", "License") + "</h2>" + rows + note + "</div>";
}

function renderOverview() {
  var s = state.status || {};
  var k = state.kernel;
  var html =
    '<div class="grid-4">' +
    statCard(__("运行状态", "Status"), s.status || "unknown", "running") +
    statCard(
      __("运行时间", "Uptime"),
      '<span id="uptime-val">' +
        (state.startedAt ? fmtUptime(Date.now() - state.startedAt) : "-") +
        "</span>",
      "uptime",
    ) +
    statCard(__("插件", "Plugins"), (k?.plugins || []).length || 0, "plugin") +
    statCard(
      __("版本", "Version"),
      (() => {
        // 构建身份取自 /kernel 的 build（-ldflags 注入的真实版本/commit）。
        // 旧实现用的是 /status 的 version 加一个凭空写死的 "0.1.0" 兑底 ——
        // 拿不到数据时会向用户展示一个不存在的版本号。
        var b = (k && k.build) || {};
        var v = b.version || s.version || "";
        if (!v) return "-";
        var sha =
          b.commit && b.commit !== "unknown" ? String(b.commit).slice(0, 7) : "";
        return (
          "v" + escHtml(v) +
          '<div class="stat-sub">' + escHtml(b.kernel_name || "HologramAgent") +
          (sha ? " · " + escHtml(sha) : "") + "</div>"
        );
      })(),
      "version",
    ) +
    "</div>";
  if (k) {
    html +=
      '<div class="grid-2">' +
      '<div class="card"><h2>' +
      __("LLM 状态", "LLM Status") +
      "</h2>" +
      '<div class="kv-row"><span class="key">Provider</span><span class="val">' +
      (k.llm?.provider || __("未配置", "Not configured")) +
      "</span></div>" +
      '<div class="kv-row"><span class="key">' +
      __("可用源", "Sources") +
      '</span><span class="val">' +
      (k.llm?.sources || 0) +
      "</span></div>" +
      '<div class="kv-row"><span class="key">' +
      __("状态", "Status") +
      '</span><span class="val"><span class="status-dot ' +
      (k.llm?.available ? "dot-green" : "dot-red") +
      '"></span>' +
      (k.llm?.available
        ? __("运行中", "Running")
        : __("不可用", "Unavailable")) +
      "</span></div>" +
      "</div>" +
      '<div class="card"><h2>' +
      __("记忆状态", "Memory Status") +
      "</h2>" +
      '<div class="kv-row"><span class="key">' +
      __("图记忆", "Graph Memory") +
      '</span><span class="val"><span class="status-dot ' +
      (k.memory?.available ? "dot-green" : "dot-gray") +
      '"></span>' +
      (k.memory?.available
        ? k.memory.entity_count +
          __(" 实体, ", " entities, ") +
          k.memory.relation_count +
          __(" 关系", " relations")
        : __("未初始化", "Uninitialized")) +
      "</span></div>" +
      '<div class="kv-row"><span class="key">' +
      __("文档记忆", "Document Memory") +
      '</span><span class="val">' +
      (k.documents?.available
        ? k.documents.doc_count + __(" 文档", " docs")
        : __("未初始化", "Uninitialized")) +
      "</span></div>" +
      '<div class="kv-row"><span class="key">' +
      __("文本记忆", "Text Memory") +
      '</span><span class="val">' +
      (k.text_memory?.available
        ? k.text_memory.file_count + __(" 文件", " files")
        : __("未初始化", "Uninitialized")) +
      "</span></div>" +
      '<div class="kv-row"><span class="key">' +
      __("知识库", "Knowledge") +
      '</span><span class="val">' +
      (k.knowledge?.available
        ? k.knowledge.item_count + __(" 项", " items")
        : __("未初始化", "Uninitialized")) +
      "</span></div>" +
      "</div></div>";
  }
  html += renderRuntimePanel();
  html +=
    '<div class="card"><h2>' +
    __("运行时", "Runtime") +
    '</h2><div class="grid-3">' +
    statCard("Goroutines", k?.runtime?.goroutines || "-", "") +
    statCard(
      __("内存", "Memory"),
      k?.runtime?.memory_mb ? k.runtime.memory_mb + " MB" : "-",
      "",
    ) +
    statCard("Go " + __("版本", "Version"), k?.runtime?.go_version || "-", "") +
    "</div></div>";
  html += renderDiagPanel();
  html += renderLegalCard();
  document.getElementById("view-overview").innerHTML = html;
}

// renderDiagPanel 协议对齐补齐的诊断卡片（agents / network / tracker / config）。
//
// 全部走 `(x && x.y)` 的安全取值：任一端点没取到（老内核无该路由、
// 连接断开）都只显示 "-"，不抛错 —— 与既有卡片一致。
function renderDiagPanel() {
  var a = state.agents || {};
  var list = a.agents || [];
  var main0 = list[0] || {};
  var nw = state.network || {};
  var tk = state.tracker || {};
  var cfg = state.runConfig || {};
  var llmOk = main0.network && main0.network.llm_api_reachable;

  return (
    '<div class="card"><h2>' +
    __("诊断", "Diagnostics") +
    '</h2><div class="grid-4">' +
    statCard(
      __("Agent 健康", "Agent health"),
      main0.health === 1
        ? __("正常", "healthy")
        : main0.health === 0
          ? "-"
          : __("异常", "unhealthy"),
      main0.health === 1 ? "running" : "",
    ) +
    statCard(
      __("LLM 可达", "LLM reachable"),
      llmOk === true
        ? __("是", "yes")
        : llmOk === false
          ? __("否", "no")
          : "-",
      llmOk === false ? "denied" : "",
    ) +
    statCard(
      __("网络端点", "Endpoints"),
      Array.isArray(nw.endpoints) ? String(nw.endpoints.length) : "-",
      "",
    ) +
    statCard(
      __("文件变更集", "Changesets"),
      tk.changesets !== undefined ? String(tk.changesets) : "-",
      tk.has_changes ? "warn" : "",
    ) +
    "</div>" +
    '<div class="grid-2" style="margin-top:10px">' +
    '<div><b>' +
    __("数据目录", "Data dir") +
    "</b>: " +
    escHtml(cfg?.daemon?.data_dir || "-") +
    "</div>" +
    '<div><b>' +
    __("心跳间隔", "Heartbeat") +
    "</b>: " +
    (cfg?.daemon?.heartbeat_interval
      ? Math.round(cfg.daemon.heartbeat_interval / 1e6) + " ms"
      : "-") +
    "</div>" +
    "</div></div>"
  );
}

// renderPersona 人设面板：展示 /persona 返回的人格设定。
//
// 只读展示。当前先不做编辑 —— 编辑要处理保存、失败回滚、并发覆盖，
// 与"协议对齐"是两件事，混在一起容易做半。
function renderPersona() {
  var el = document.getElementById("view-persona");
  if (!el) return;
  var p = state.persona || {};
  var prompt = p.current_prompt || "";
  // ★ 实测结构是 {current_prompt, file_override, initialized}，
  //   不是键值对 —— 我第一版按 map 遍历，结果只会显示三个字段名。
  el.innerHTML =
    '<div class="card"><h2>' +
    __("人设", "Persona") +
    '</h2><div class="grid-3">' +
    statCard(
      __("已初始化", "Initialized"),
      p.initialized ? __("是", "yes") : __("否", "no"),
      p.initialized ? "running" : "",
    ) +
    statCard(
      __("文件覆盖", "File override"),
      p.file_override ? __("开", "on") : __("关", "off"),
      "",
    ) +
    statCard(__("提示词长度", "Prompt length"), String(prompt.length), "") +
    "</div>" +
    '<h2 style="margin-top:12px">' +
    __("当前提示词", "Current prompt") +
    '</h2><pre style="white-space:pre-wrap;word-break:break-word">' +
    escHtml(prompt || __("（空）", "(empty)")) +
    "</pre></div>";
}

// renderProxy 反代面板：展示 /proxy 的挂载与模式。
function renderProxy() {
  var el = document.getElementById("view-proxy");
  if (!el) return;
  var px = state.proxy || {};
  var svc = state.proxyServices || null;
  var head =
    '<div class="card"><h2>' +
    __("反代", "Reverse Proxy") +
    '</h2><div class="grid-4">' +
    statCard(__("基础域名", "Base domain"), px.base_domain || "-", "") +
    statCard(__("模式", "Mode"), px.mode || "-", "") +
    statCard(__("总数", "Total"), px.total !== undefined ? String(px.total) : "-", "") +
    statCard(__("手动", "Manual"), px.manual !== undefined ? String(px.manual) : "-", "") +
    "</div>";
  var body = "";
  var list = svc && Array.isArray(svc.services) ? svc.services : null;  // 实测：services 是数组
  if (list) {
    body =
      '<div class="card"><h2>' +
      __("服务", "Services") +
      "</h2><pre style=\"white-space:pre-wrap;word-break:break-word\">" +
      escHtml(
        list
          .map((s) => {
            // ★ 实测字段：{name, host, path, url, target, ok, auth, websocket}
            return (
              (s.ok === false ? "✗ " : "✓ ") +
              (s.name || "?") +
              " → " +
              (s.target || s.url || "?")
            );
          })
          .join("\n"),
      ) +
      "</pre></div>";
  }
  el.innerHTML = head + body;
}

// ===== Chat =====
var _chatLayoutBuilt = false;

function buildChatLayout() {
  var cont = document.getElementById("view-chat");
  var k = state.kernel || {};
  var html = '<div class="chat-layout">';
  html +=
    '<div class="chat-tabs">' +
    '<span class="active" onclick="switchChatPanel(\'chat\',this)">' +
    __("对话", "Chat") +
    "</span>" +
    "<span onclick=\"switchChatPanel('starmap',this)\">" +
    __("星图", "Star Map") +
    "</span>" +
    "<span onclick=\"switchChatPanel('terminal',this)\">" +
    __("终端", "Terminal") +
    "</span>" +
    "<span onclick=\"switchChatPanel('cmd',this)\">" +
    __("运行中命令", "Running Commands") +
    "</span>" +
    "<span onclick=\"switchChatPanel('memory',this)\">" +
    __("记忆", "Memory") +
    "</span>" +
    "<span onclick=\"switchChatPanel('context',this)\">" +
    __("上下文", "Context") +
    "</span>" +
    "<span onclick=\"switchChatPanel('knowledge',this)\">" +
    __("知识", "Knowledge") +
    "</span>" +
    "</div>";
  if (!state.currentConn) {
    html +=
      '<div class="chat-panel active" id="chat-panel-chat"><div class="card setup-card">' +
      '<svg class="setup-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z"/></svg>' +
      "<h2>" +
      __("未配置后端", "No backend configured") +
      "</h2>" +
      "<p>" +
      __(
        "连接 HologramAgent 服务端后即可开始对话。请在设置中添加后端连接。",
        "Connect to a HologramAgent server to start chatting. Add a backend connection in Settings.",
      ) +
      "</p>" +
      '<button class="btn btn-primary" onclick="goSettingsConn()">' +
      __("前往设置添加后端", "Go to Settings to add backend") +
      "</button>" +
      "</div></div>";
    for (var p2 = 0; p2 < 6; p2++) {
      html +=
        '<div class="chat-panel" id="chat-panel-' +
        ["starmap", "terminal", "cmd", "memory", "context", "knowledge"][p2] +
        '"><div class="card"><p style="color:var(--text-muted)">' +
        __(
          "请先在设置中添加后端连接",
          "Add a backend connection in Settings first",
        ) +
        "</p></div></div>";
    }
    cont.innerHTML = html;
    _chatLayoutBuilt = true;
    return;
  }
  html +=
    '<div class="chat-panel active" id="chat-panel-chat"><div class="chat-main">';
  html +=
    '<div class="card"><h2>' +
    __("对话", "Chat") +
    ' <span id="chat-stage" class="badge" style="font-size:12px;font-weight:400;display:none">' +
    escHtml(state.chatStage || "") +
    '</span></h2><div class="chat-messages" id="chat-msgs">';
  if (state.messages.length === 0) {
    html +=
      '<div class="empty-state" style="flex:1;display:flex;align-items:center;justify-content:center"><p>' +
      __("开始与您的 HologramAgent 聊天吧", "Start chatting with your HologramAgent") +
      "</p></div>";
  }
  html +=
    "</div>" +
    '<div class="chat-input-row">' +
    '<input id="chat-input" placeholder="' +
    __("输入消息...", "Type a message...") +
    '" onkeydown="if(event.key==\'Enter\')sendChat()">' +
    '<button class="btn" onclick="interruptChat()" id="chat-stop-btn" style="display:none;background:#d1383d;color:#fff">' +
    __("停止", "Stop") +
    "</button>" +
    '<button class="btn btn-primary" onclick="sendChat()" id="chat-send-btn">' +
    __("发送", "Send") +
    "</button>" +
    "</div></div></div></div>";
  html +=
    '<div class="chat-panel" id="chat-panel-starmap"><div class="card"><h2>' +
    __("星图", "Star Map") +
    "</h2>" +
    '<div id="sm-container-chat" style="display:flex;align-items:center;justify-content:center;min-height:480px"><div class="ha-dots-panel"><span class="ha-dots ha-dots-lg"><i></i><i></i><i></i></span></div></div></div></div>';
  html +=
    '<div class="chat-panel" id="chat-panel-terminal"><div class="card"><h2>' +
    __("终端", "Terminal") +
    ' <span id="term-count-badge" class="badge badge-blue">0</span></h2>' +
    '<div id="term-list" style="max-height:60vh;overflow-y:auto;font-size:12px"></div></div></div>';
  html +=
    '<div class="chat-panel" id="chat-panel-cmd"><div class="card"><h2>' +
    __("运行中命令", "Running Commands") +
    ' <span id="cmd-count-badge" class="badge badge-blue">0</span></h2>' +
    '<div id="cmd-list" style="max-height:60vh;overflow-y:auto;font-size:12px"></div></div></div>';
  html +=
    '<div class="chat-panel" id="chat-panel-memory"><div class="card"><h2>' +
    __("记忆", "Memory") +
    "</h2>" +
    '<div class="kv-row"><span class="key">' +
    __("实体", "Entities") +
    '</span><span class="val">' +
    (k?.memory?.entity_count || "-") +
    "</span></div>" +
    '<div class="kv-row"><span class="key">' +
    __("关系", "Relations") +
    '</span><span class="val">' +
    (k?.memory?.relation_count || "-") +
    "</span></div>" +
    '<div style="margin-top:8px">' +
    '<input id="mem-query" placeholder="' +
    __("关键词查询", "Keyword query") +
    '">' +
    '<button class="btn btn-primary btn-sm" onclick="queryMemoryChat()">' +
    __("查询", "Query") +
    "</button>" +
    '</div><div id="mem-result-chat" style="margin-top:8px;max-height:180px;overflow:auto"></div>' +
    "</div></div>";
  html +=
    '<div class="chat-panel" id="chat-panel-context"><div class="card"><h2>' +
    __("上下文", "Context") +
    "</h2>" +
    '<div style="margin-top:8px">' +
    '<input id="ctx-query" placeholder="' +
    __("输入当前话题", "Enter current topic") +
    '">' +
    '<button class="btn btn-primary btn-sm" onclick="queryMemoryContext()">' +
    __("获取上下文", "Get Context") +
    "</button>" +
    '</div><div id="ctx-result" style="margin-top:8px;max-height:200px;overflow:auto"></div>' +
    "</div></div>";
  html +=
    '<div class="chat-panel" id="chat-panel-knowledge"><div class="card"><h2>' +
    __("知识", "Knowledge") +
    "</h2>" +
    '<div class="kv-row"><span class="key">' +
    __("项目", "Items") +
    '</span><span class="val">' +
    (k?.knowledge?.item_count || "-") +
    "</span></div>" +
    '<div style="margin-top:8px">' +
    '<input id="know-query" placeholder="' +
    __("搜索知识", "Search knowledge") +
    '">' +
    '<button class="btn btn-primary btn-sm" onclick="searchKnowledgeChat()">' +
    __("搜索", "Search") +
    "</button>" +
    '</div><div id="know-result-chat" style="margin-top:8px;max-height:180px;overflow:auto"></div>' +
    '<div style="margin-top:12px;border-top:1px solid var(--border-color);padding-top:8px">' +
    '<input id="know-name" placeholder="' +
    __("知识名称", "Knowledge name") +
    '" style="margin-bottom:4px">' +
    '<textarea id="know-content" placeholder="' +
    __("内容", "Content") +
    '" style="min-height:50px;margin-bottom:4px"></textarea>' +
    '<button class="btn btn-primary btn-sm" onclick="createKnowledgeChat()">' +
    __("创建", "Create") +
    "</button>" +
    "</div></div></div>";
  cont.innerHTML = html;
  _chatLayoutBuilt = true;
}

var CHAN_COLORS = [
  "#e08a5f",
  "#5f9fe0",
  "#6bbf8f",
  "#c06bbf",
  "#d9a13b",
  "#5fb3bf",
  "#b06b6b",
  "#7f8ce0",
];

function chanColor(src) {
  var h = 0;
  for (var i = 0; i < src.length; i++) h = (h * 31 + src.charCodeAt(i)) >>> 0;
  return CHAN_COLORS[h % CHAN_COLORS.length];
}

function chanLetter(src) {
  var s = (src || "").trim();
  if (!s) return "C";
  var ch = s.charAt(0).toUpperCase();
  return /[A-Za-z0-9]/.test(ch) ? ch : "C";
}

// renderToolCard 渲染单个工具调用卡。
// 独立成函数是为了让分组逻辑能复用（组卡展开后要逐个渲染）。
function renderToolCard(tc, drip) {
  var argsStr =
    typeof tc.args === "object"
      ? JSON.stringify(tc.args, null, 1)
      : tc.args || "";
  var resultStr = tc.result
    ? typeof tc.result === "object"
      ? JSON.stringify(tc.result, null, 1)
      : String(tc.result)
    : "";
  var running = !resultStr && tc.status !== "denied";
  var error = tc.status === "error" || tc.status === "denied" || !!tc.error;
  var iconSvg = error
    ? '<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="12" r="9"/><path d="M5.6 5.6l12.8 12.8"/></svg>'
    : running
      ? '<span class="tc-spinner"></span>'
      : '<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.9 2.9-2.5-.6-.6-2.5z"/></svg>';
  var statusHtml =
    tc.status === "denied"
      ? '<span class="tc-state tc-deny">' + __("已拒绝", "Denied") + "</span>"
      : running
        ? '<span class="tc-state tc-run">' + __("调用中", "Running") + "</span>"
        : '<span class="tc-state tc-done">' + __("完成", "Done") + "</span>";
  var pluginHtml = tc.plugin
    ? '<span class="tc-plugin">' + escHtml(tc.plugin) + "</span>"
    : "";
  return (
    '<div class="tool-card' +
    (error ? " tc-error" : running ? " tc-running" : " tc-done") +
    (drip ? " tool-drip-in" : "") +
    '" data-tool="' +
    escHtml(tc.tool || tc.name || "") +
    '" onclick="toggleToolCall(this)">' +
    '<div class="tc-line"><span class="tc-ico">' +
    iconSvg +
    '</span><span class="tc-name">' +
    escHtml(tc.tool || tc.name || "") +
    "</span>" +
    pluginHtml +
    statusHtml +
    '<span class="tc-caret">▾</span></div>' +
    '<div class="tc-detail" style="display:none">' +
    (argsStr && argsStr !== "{}"
      ? '<div class="tc-args"><div class="tc-detail-label">' +
        __("参数", "Args") +
        "</div>" +
        escHtml(argsStr) +
        "</div>"
      : "") +
    (resultStr
      ? '<div class="tc-result"><div class="tc-detail-label">' +
        __("结果", "Result") +
        "</div>" +
        escHtml(resultStr) +
        "</div>"
      : "") +
    "</div></div>"
  );
}

// renderToolGroupOrSingle 按数量决定形态：1 个走单卡，多个折成组卡。
//
// ★ 分组策略参考 PiDeck 的 tool-group-card。原来一轮里每个调用各占一张卡，
// 调 5 个工具就是 5 张卡竖排，把真正的回复挤到很下面；而这些卡形态高度
// 相似（图标 + 工具名 + 状态），信息密度极低。折成组卡后「agent 干了什么」
// 一眼可见，而不被细节淹没。
function renderToolGroupOrSingle(tcs, newlyDone) {
  var cards = tcs.map(function (tc) {
    return renderToolCard(
      tc,
      newlyDone.indexOf(tc.tool || tc.name || "") !== -1,
    );
  });
  if (cards.length === 1) return cards[0];
  return renderToolGroup(tcs, cards);
}

// renderToolGroup 组卡：头部脉冲点 + 计数 + 状态摘要，展开后逐个单卡。
function renderToolGroup(tcs, cards) {
  var runningN = 0;
  var errN = 0;
  tcs.forEach(function (tc) {
    if (!tc.result && tc.status !== "denied") runningN++;
    if (tc.status === "error" || tc.status === "denied" || !!tc.error) errN++;
  });
  var tone =
    errN > 0 ? "tone-error" : runningN > 0 ? "tone-running" : "tone-done";
  var summary =
    errN > 0
      ? __("部分失败", "some failed")
      : runningN > 0
        ? __("进行中", "running")
        : __("完成", "done");
  return (
    '<div class="tool-group-card ' + tone + '">' +
    '<div class="tool-group-head" onclick="toggleToolGroup(this)">' +
    '<span class="tool-group-dot"></span>' +
    '<span class="tool-group-count">' + cards.length + "</span>" +
    '<span class="tool-group-label">' + __("个工具调用", "tool calls") + "</span>" +
    '<span class="tc-state">' + escHtml(summary) + "</span>" +
    '<span class="tc-caret"></span>' +
    "</div>" +
    '<div class="tool-group-body" style="display:none">' +
    cards.join("") +
    "</div></div>"
  );
}

// PiDeck 风格思考卡片：Brain 图标 + 折叠时单行预览（流式中扫光）+ 展开/收起
// PiDeck 风格思考卡片：Brain 图标 + 折叠单行预览（流式中扫光）+ 展开懒加载全文
function renderReasoningCard(text, isStreaming, idx) {
  var preview =
    typeof marked === "undefined"
      ? escHtml(text)
          .replace(/<[^>]+>/g, " ")
          .slice(0, 60)
      : text.replace(/[\s\n]+/g, " ").slice(0, 60);
  return (
    '<div class="reasoning-card' +
    (isStreaming ? " rc-streaming" : "") +
    '" data-idx="' +
    (idx | 0) +
    '">' +
    '<div class="reasoning-head" onclick="toggleReasoning(this)">' +
    '<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="rc-ico"><path d="M9 3a2 2 0 0 0-2 2v2a2 2 0 0 1-2 2H3a2 2 0 0 0-2 2v6a2 2 0 0 0 2 2h2a2 2 0 0 1 2 2v2a2 2 0 0 0 2 2h1a2 2 0 0 0 2-2v-2a2 2 0 0 1 2-2h2a2 2 0 0 0 2-2V7a2 2 0 0 0-2-2h-2a2 2 0 0 1-2-2V3a2 2 0 0 0-2-2H9zM12 8v4m0 4h.01"/></svg>' +
    '<span class="rc-title">' +
    (isStreaming ? __("思考中...", "Thinking...") : __("思考", "Thinking")) +
    "</span>" +
    '<span class="rc-chev">▾</span></div>' +
    '<div class="reasoning-body" style="display:' +
    (isStreaming ? "block" : "none") +
    '">' +
    (isStreaming
      ? '<div class="reasoning-preview">' +
        escHtml(preview) +
        '</div><div class="reasoning-sweep"></div>'
      : '<div class="reasoning-content"></div>') +
    "</div></div>"
  );
}

function renderChat() {
  if (!_chatLayoutBuilt) {
    buildChatLayout();
    renderChatStarmap();
    renderTerminals();
    renderCmdHistory();
  }
  var msgsEl = document.getElementById("chat-msgs");
  if (!msgsEl) return;
  if (!msgsEl._stickBound) {
    msgsEl._stickBound = true;
    msgsEl.addEventListener(
      "scroll",
      () => {
        state.chatStick =
          msgsEl.scrollHeight - msgsEl.scrollTop - msgsEl.clientHeight < 80;
        // 触顶（近顶部 60px）且服务端还有更早历史 → 向上懒加载下一页
        if (msgsEl.scrollTop < 60 && state.chatHasMore) {
          loadOlderChat();
        }
      },
      { passive: true },
    );
  }
  var msgs = state.messages;
  var sig =
    msgs
      .map((m) => {
        var c = m.content || "";
        return (
          (m.role || "") +
          ":" +
          c.length +
          ":" +
          c.slice(-40) +
          ":" +
          (m.tool_calls || [])
            .map((t) => (t.tool || t.name || "") + "/" + (t.status || ""))
            .join(",")
        );
      })
      .join("|") +
    "|L" +
    (state.chatLoading ? "1" : "0") +
    "|P" +
    (state.pendingTools || []).join(",");
  if (msgsEl._chatSig === sig && msgsEl.childElementCount > 0) {
    return;
  }
  msgsEl._chatSig = sig;
  var prevPending = msgsEl._lastPending || [];
  var newPending = (state.pendingTools || []).slice();
  var lastM = msgs.length ? msgs[msgs.length - 1] : null;
  // 同上：不依赖 chatLoading。旁观其它渠道的工具调用时，
  // 「运行中工具」列表此前恒为空，用户看不到 agent 正在做什么。
  if (
    (state.chatLoading ||
      (lastM && lastM._streaming && !lastM._final)) &&
    lastM &&
    lastM.role === "assistant"
  ) {
    (lastM.tool_calls || []).forEach((tc) => {
      if (!tc.result && tc.status !== "denied") {
        var nm = tc.tool || tc.name || "";
        if (newPending.indexOf(nm) === -1) newPending.push(nm);
      }
    });
  }
  var newlyDone = prevPending.filter((n) => newPending.indexOf(n) === -1);
  msgsEl._lastPending = newPending;
  // ★ 同 rerenderChatIfActive 的判据：看消息自身的 _streaming，
  // 不依赖 state.chatLoading。
  //
  // chatLoading 只在「用户从 GUI 自己发消息」（sendChat）时为 true。agent
  // 响应 memo/QQ/email 等其他渠道时，GUI 是通过 SSE 旁观流式帧，
  // chatLoading 恒 false ⇒ streamingLast 恒假 ⇒ 每个增量帧都走**全量**
  // innerHTML 重建。
  //
  // 实测（2026-10-01，本机 192.168.2.60）：旁观一次 tool_call + 回复，
  // 1.7 秒内 13 次全量重渲（消息数 45）；另一段流式更密的样本 2 秒 26 次。
  // 200 条消息时单次全量重建实测 235ms，这是「聊天卡顿」的直接来源。
  //
  // 所有 SSE 流式帧创建的 assistant 消息都带 _streaming:true，
  // 收尾时才置 _final —— 所以正确判据是「最后一条是尚未收尾的流式 assistant」。
  var streamingLast = !!(
    lastM &&
    lastM.role === "assistant" &&
    !lastM._final &&
    lastM._streaming
  );
  function pillHtml() {
    var s = "";
    newPending.forEach((nm) => {
      var anim = prevPending.indexOf(nm) === -1 ? " pill-in" : "";
      s +=
        '<span class="thinking-tool' +
        anim +
        '" data-tool="' +
        escHtml(nm) +
        '">' +
        '<svg viewBox="0 0 24 24" width="11" height="11" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.9 2.9-2.5-.6-.6-2.5z"/></svg>' +
        escHtml(nm) +
        "</span>";
    });
    return s;
  }
  var html = "";
  if (msgs.length === 0) {
    html =
      '<div class="empty-state" style="flex:1;display:flex;align-items:center;justify-content:center"><p>' +
      __("开始与您的 HologramAgent 聊天吧", "Start chatting with your HologramAgent") +
      "</p></div>";
  } else {
    msgs.forEach((m, i) => {
      var role = m.role || "user";
      var c = m.content || "";
      // ★ 稳定标识：role + 序号 + 内容长度 + 首尾片段。
      //   序号参与是为了区分「连续两条同 role 同长度」的消息；
      //   内容片段参与是为了让「同一条消息内容变了」能被识别出来。
      //   增量渲染靠它定位可复用的 DOM 节点（**不能靠下标** ——
      //   loadOlderChat 会 unshift 前插消息，下标整体位移）。
      var mkey = role + ":" + i + ":" + c.length + ":" + c.slice(0, 24) + ":" + c.slice(-24);
      if (role === "assistant") {
        if (typeof marked === "undefined") {
          c = "<pre>" + escHtml(c) + "</pre>";
        } else {
          c = renderMd(c);
        }
      } else if (role === "system") {
        c = escHtml(c);
      } else {
        c = escHtml(c);
      }
      var isChan = !!(m.source && m.source !== "webui");
      var rc = "";
      if (m.reasoning_content) {
        rc = renderReasoningCard(m.reasoning_content, isStreamingLast, i);
      }
      var tcs = "";
      if (m.tool_calls && m.tool_calls.length > 0) {
        tcs = renderToolGroupOrSingle(m.tool_calls, newlyDone);
      }
      if (false) {
        m.tool_calls.forEach((tc) => {
          var argsStr =
            typeof tc.args === "object"
              ? JSON.stringify(tc.args, null, 1)
              : tc.args || "";
          var resultStr = tc.result
            ? typeof tc.result === "object"
              ? JSON.stringify(tc.result, null, 1)
              : String(tc.result)
            : "";
          var running = !resultStr && tc.status !== "denied";
          var error =
            tc.status === "error" || tc.status === "denied" || !!tc.error;
          var drip =
            newlyDone.indexOf(tc.tool || tc.name || "") === -1
              ? ""
              : " tool-drip-in";
          var iconSvg = error
            ? '<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="12" r="9"/><path d="M5.6 5.6l12.8 12.8"/></svg>'
            : running
              ? '<span class="tc-spinner"></span>'
              : '<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.9 2.9-2.5-.6-.6-2.5z"/></svg>';
          var statusHtml =
            tc.status === "denied"
              ? '<span class="tc-state tc-deny">' +
                __("已拒绝", "Denied") +
                "</span>"
              : running
                ? '<span class="tc-state tc-run">' +
                  __("调用中", "Running") +
                  "</span>"
                : '<span class="tc-state tc-done">' +
                  __("完成", "Done") +
                  "</span>";
          var pluginHtml = tc.plugin
            ? '<span class="tc-plugin">' + escHtml(tc.plugin) + "</span>"
            : "";
          tcs +=
            '<div class="tool-card' +
            (error ? " tc-error" : running ? " tc-running" : " tc-done") +
            drip +
            '" data-tool="' +
            escHtml(tc.tool || tc.name || "") +
            '" onclick="toggleToolCall(this)">' +
            '<div class="tc-line"><span class="tc-ico">' +
            iconSvg +
            '</span><span class="tc-name">' +
            escHtml(tc.tool || tc.name || "") +
            "</span>" +
            pluginHtml +
            statusHtml +
            '<span class="tc-caret">▾</span></div>' +
            '<div class="tc-detail" style="display:none">' +
            (argsStr && argsStr !== "{}"
              ? '<div class="tc-args"><div class="tc-detail-label">' +
                __("参数", "Args") +
                "</div>" +
                escHtml(argsStr) +
                "</div>"
              : "") +
            (resultStr
              ? '<div class="tc-result"><div class="tc-detail-label">' +
                __("结果", "Result") +
                "</div>" +
                escHtml(resultStr) +
                "</div>"
              : "") +
            "</div></div>";
        });
      }
      var body = rc + tcs;
      var growCls = m._grow ? " grow-in" : "";
      if (m._grow) m._grow = false;
      var isStreamingLast = i === msgs.length - 1 && streamingLast;
      if (isStreamingLast) {
        var liveRow =
          '<span class="live-spinner"><i></i><i></i><i></i></span>' +
          (newPending.length
            ? '<span class="thinking-tools">' + pillHtml() + "</span>"
            : "");
        // ★ msg-running：整条气泡的呼吸光晕。参考 PiDeck 的
        // .turn-row--running:before —— 一层极淡的 accent 底色缓慢明暗
        // 交替。单看气泡里那几个点，视线要缩到一小块；光晕铺在整条消息
        // 上，余光就能感知「agent 正在回」。只加在最后一条且未收尾的
        // 流式消息上，已完成的历史消息不带。
        var runCls = " msg-running";
        if (c) {
          body +=
            '<div class="msg-bubble' +
            growCls +
            '">' +
            liveRow +
            '<div class="text">' +
            c +
            "</div></div>";
          c = "";
        } else {
          body += '<div class="msg-bubble">' + liveRow + "</div>";
        }
      } else if (c) {
        body +=
          '<div class="msg-bubble' +
          growCls +
          '"><div class="text">' +
          c +
          "</div></div>";
      }
      if (role === "system") {
        html +=
          '<div class="msg msg-system" data-msgkey="' + escHtml(mkey) + '"><div class="msg-bubble">' +
          (c || "") +
          "</div></div>";
      } else if (isChan) {
        html +=
          '<div class="msg msg-channel" data-msgkey="' + escHtml(mkey) + '">' +
          '<div class="msg-avatar chan-avatar" style="background:' +
          chanColor(m.source) +
          '">' +
          chanLetter(m.source) +
          "</div>" +
          '<div class="msg-content"><div class="msg-chan-name">' +
          escHtml(m.source) +
          "</div>" +
          body +
          "</div>" +
          "</div>";
      } else {
        var userAvatar =
          '<svg viewBox="0 0 24 24" style="width:16px;height:16px" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="8" r="4"/><path d="M4 20c0-4 4-6 8-6s8 2 8 6"/></svg>';
        var aiAvatar =
          '<img src="mascot.webp" style="width:28px;height:28px;border-radius:50%;object-fit:cover" alt="' +
          __("小宅", "Agent") +
          '">';
        html +=
          '<div class="msg msg-' +
          role +
          '" data-msgkey="' +
          escHtml(mkey) +
          '">' +
          '<div class="msg-avatar">' +
          (role === "user" ? userAvatar : aiAvatar) +
          "</div>" +
          '<div class="msg-content">' +
          body +
          "</div>" +
          "</div>";
      }
    });
  }
  if (state.chatLoading && !streamingLast) {
    var aiAvatar2 =
      '<img src="mascot.webp" style="width:28px;height:28px;border-radius:50%;object-fit:cover" alt="' +
      __("小宅", "Agent") +
      '">';
    html +=
      '<div class="msg msg-assistant" data-msgkey="__loading__"><div class="msg-avatar">' +
      aiAvatar2 +
      '</div><div class="msg-content"><div class="msg-bubble">' +
      '<span class="live-spinner"><i></i><i></i><i></i></span>' +
      (newPending.length
        ? '<span class="thinking-tools">' + pillHtml() + "</span>"
        : "") +
      "</div></div></div>";
  }
  // ★ 增量渲染：能复用就复用，别整棵重建（见 applyIncrementalChatRender）。
  applyIncrementalChatRender(msgsEl, html);
  if (state.chatStick !== false) {
    // ★ 必须用 scrollTop 立即到位，**不能**用 scrollTo({behavior:"smooth"})。
    //
    // 真机实测（Xvfb + Electron + CDP，200 条消息 / 3500 DOM 节点）：
    //   smooth：立即 scrollTop=0，300ms 后只到 6894，而上限是 22838
    //           ⇒ 既慢又**没到位**，用户看到的就是「打开不在最新消息、
    //             还要反复滑动」
    //   scrollTop=scrollHeight：立即 22838，一次到位
    //
    // 原因：上面刚做完 DOM 变更，smooth 动画的起点算的是**旧**布局；
    // 动画启动前布局又变了，于是滚到错误位置。重建后本就不该有动画
    // ——用户要的是「立刻看到最新消息」。
    msgsEl.scrollTop = msgsEl.scrollHeight;
  }
  updateChatBadge();
  if (window.homeagent && window.homeagent.log) {
    window.homeagent.log(
      "render: msgs=" +
        msgs.length +
        " sig=" +
        sig.slice(0, 60) +
        " last=" +
        (lastM
          ? lastM.role +
            "/C=" +
            String(lastM.content || "").length +
            "/T=" +
            (lastM.tool_calls || []).length
          : "none") +
        " roles=" +
        msgs
          .map(
            (m) =>
              m.role +
              (m.content ? "#" + String(m.content).length : "") +
              (m.source ? "@" + m.source : "") +
              (m.tool_calls && m.tool_calls.length
                ? "T" + m.tool_calls.length
                : ""),
          )
          .join(","),
    );
  }
}

function guardedRenderChat() {
  try {
    renderChat();
  } catch (e) {
    if (window.homeagent && window.homeagent.log)
      window.homeagent.log(
        "renderChat ERROR: " +
          e.message +
          " stack=" +
          (e.stack || "").split("\n").slice(0, 2).join(";"),
      );
    console.error("renderChat error", e);
  }
}

function updateChatBadge() {
  var badge = document.getElementById("chat-stage");
  if (!badge) return;
  badge.textContent = state.chatStage || "";
  badge.style.display = "none";
}

// applyIncrementalChatRender 按 data-msgkey 复用可用的 DOM 节点。
//
// 为什么需要：旧实现无条件整棵 innerHTML 重建 —— 200 条消息 = 3500 个 DOM
// 节点全部销毁重建，真机实测单次 235ms；流式追加时每个放行的 chunk 都走这条路
// （200 条时每 chunk 6.6ms）⇒ 聊得越久越卡，用户「卡得没有用的欲望」。
//
// 策略（从便宜到贵）：
//   1) 尾部追加 —— 新 key 序列是旧序列的前缀 + 新增（最常见：发消息/工具轮）
//   2) 头部前插 —— loadOlderChat 往上翻：新序列 = 新前缀 + 旧序列
//   3) 局部替换 —— 个别 key 变了（某条消息内容更新）
//   4) 兜底整棵重建 —— 序列既非前缀也非后缀（删除/去重/重排）
function applyIncrementalChatRender(msgsEl, html) {
  var want = [];
  var re = /data-msgkey="([^"]*)"/g;
  var m;
  while ((m = re.exec(html)) !== null) want.push(m[1]);

  var kids = Array.prototype.slice.call(msgsEl.children);
  var have = kids.map((n) => n.getAttribute("data-msgkey") || "");

  // 没带 key（老结构 / 空列表）⇒ 只能整棵重建
  if (want.length === 0 || have.length === 0) {
    msgsEl.innerHTML = html;
    return;
  }

  // 情况 2：头部前插。新序列 = 新前缀 + 旧序列
  if (want.length > have.length) {
    var off = want.length - have.length;
    var isPrepend = true;
    for (var p = 0; p < have.length; p++) {
      if (want[p + off] !== have[p]) { isPrepend = false; break; }
    }
    if (isPrepend) {
      var prevH = msgsEl.scrollHeight;
      var prevTop = msgsEl.scrollTop;
      for (var q = off - 1; q >= 0; q--) {
        var tn = parseChatNode(html, want[q]);
        if (tn) msgsEl.insertBefore(tn, msgsEl.firstChild);
      }
      // 保持滚动锚点：内容变高后把视口往下挪相同高度
      msgsEl.scrollTop = prevTop + (msgsEl.scrollHeight - prevH);
      return;
    }
  }

  // 情况 1：尾部追加。新序列是旧序列的前缀
  var isAppend = have.length <= want.length;
  for (var a = 0; a < have.length && isAppend; a++) {
    if (want[a] !== have[a]) isAppend = false;
  }
  if (isAppend) {
    var frag = document.createDocumentFragment();
    for (var b = have.length; b < want.length; b++) {
      var node = parseChatNode(html, want[b]);
      if (node) frag.appendChild(node);
    }
    msgsEl.appendChild(frag);
    return;
  }

  // 情况 3：局部替换
  for (var c = 0; c < want.length && c < kids.length; c++) {
    if (have[c] === want[c]) continue;
    var fresh = parseChatNode(html, want[c]);
    if (fresh && kids[c] && kids[c].parentNode === msgsEl) {
      msgsEl.replaceChild(fresh, kids[c]);
    }
  }
  // 尾部有删除 ⇒ 截断
  while (msgsEl.childElementCount > want.length) {
    msgsEl.removeChild(msgsEl.lastElementChild);
  }
  if (msgsEl.childElementCount === want.length) return;
  // 兜底
  msgsEl.innerHTML = html;
}

// parseChatNode 从整段 html 里切出 key 对应的那一个顶层消息节点。
//
// 用一个容器承载并逐个子节点比对 data-msgkey —— 不引 DOMParser 重新解析整棵
// html（那会抵消掉增量渲染省下的开销）。
function parseChatNode(html, key) {
  var holder = document.createElement("div");
  holder.innerHTML = html;
  for (var i = 0; i < holder.children.length; i++) {
    if ((holder.children[i].getAttribute("data-msgkey") || "") === key) {
      return holder.children[i];
    }
  }
  return null;
}

function rerenderChat() {
  guardedRenderChat();
  renderChatStarmap();
  renderTerminals();
  renderCmdHistory();
}

// toggleToolGroup 展开/收起工具调用组卡。
// 只切 body 的 display，不重建 DOM —— 展开时里面的单卡已是渲染好的
// HTML，重建会丢掉它们各自的折叠状态与"刚完成"的入场动画。
function toggleToolGroup(el) {
  var card = el.parentNode;
  if (!card) return;
  var body = card.querySelector(".tool-group-body");
  if (!body) return;
  var open = body.style.display !== "none";
  body.style.display = open ? "none" : "block";
  el.classList.toggle("open", !open);
}

function toggleToolCall(el) {
  var d = el.querySelector(".tc-detail");
  if (!d) return;
  var open = d.style.display !== "none";
  d.style.display = open ? "none" : "block";
  if (open) {
    el.classList.remove("open");
  } else {
    el.classList.add("open");
  }
}

function toggleReasoning(el) {
  var card = el.closest(".reasoning-card");
  if (!card) return;
  var body = card.querySelector(".reasoning-body");
  if (!body) return;
  var open = body.style.display !== "none";
  if (open) {
    body.style.display = "none";
    card.classList.remove("open");
    return;
  }
  // 展开时懒加载全文（流式中只有 preview，未 parse 全文）
  var content = card.querySelector(".reasoning-content");
  if (content && !content.childElementCount) {
    var idx = parseInt(card.getAttribute("data-idx"), 10) || 0;
    var text =
      (state.messages[idx] && state.messages[idx].reasoning_content) || "";
    content.innerHTML = renderMd(text);
  }
  body.style.display = "block";
  card.classList.add("open");
}

function renderChatStarmap() {
  var cont = document.getElementById("sm-container-chat");
  if (!cont) return;
  if (
    window._THREE_FAILED ||
    (!window.THREE && window._THREE_FAILED !== undefined)
  ) {
    cont.innerHTML =
      '<p style="color:var(--text-muted);padding:20px;text-align:center;font-size:13px">' +
      __(
        "3D 星图不可用（本地 three.js 缺失）",
        "Star map unavailable (local three.js missing)",
      ) +
      "</p>";
    state.starmapInit = true;
    state.starmapLoading = false;
    return;
  }
  if (!window.THREE) {
    cont.innerHTML =
      '<div class="ha-dots-panel"><span class="ha-dots ha-dots-lg"><i></i><i></i><i></i></span></div>';
    state.starmapInit = false;
    state.starmapLoading = false;
    return;
  }
  if (cont.querySelector("canvas")) {
    // 已有 canvas：只调尺寸。但若上轮 pulse 置了脏标志，
    // 必须在这里消费（否则新实体永远进不来）。
    if (starmapDirty) starmapRefreshFull();
    var rect = cont.getBoundingClientRect();
    if (starmapRen && rect.width > 0)
      starmapRen.setSize(rect.width, Math.max(rect.height, 250));
    return;
  }
  if (state.starmapInit) {
    if (starmapRen) {
      var rect = cont.getBoundingClientRect();
      if (rect.width > 0)
        starmapRen.setSize(rect.width, Math.max(rect.height, 250));
      cont.appendChild(starmapRen.domElement);
      starmapRen.domElement.style.display = "block";
    } else {
      // starmapRen was destroyed (e.g. re-render cycle), restart
      state.starmapInit = false;
      state.starmapLoading = false;
    }
    return;
  }
  if (state.starmapLoading) return;
  // ★★ 2026-10-13：星图面板不可见时不建图。
  //
  // 为何必须在这里拦：GUI 的**默认视图就是 chat**（state.currentView = "chat"），
  // 而 renderAll 在 chat 视图会调 renderChatStarmap()。此前该函数无可见性守卫 ⇒
  // 每次启动都为**看不见的**星图面板付全价：实测 943ms 建图 + 首帧 433ms
  // 纹理上传（1294 张）≈ 1.4 秒的白耗——用户第一眼看到的却是聊天页。
  //
  // 判据用 offsetParent：面板 display:none 或未挂载时为 null。
  // 等用户真点到「星图」子面板，switchChatPanel 会再调本函数。
  var visCheck = document.getElementById("sm-container-chat");
  if (!visCheck || !visCheck.offsetParent) {
    state.starmapLoading = false;
    state.starmapInit = false;
    return;
  }
  state.starmapLoading = true;
  loadChatStarmapData();
}

// starmapDirty 置位后需要重拉全量图，但 renderChatStarmap() 在已有 canvas
// 时会早退（只做 setSize），标志会一直没人消费。
//
// 所以这里直接驱动重建：拉新数据 → 就地重画（initChatStarmap 在 starmapRen
// 已存在时只重建 graph，不重建 renderer/camera/控制器）。
// 轻忘“清标志”而不是“清 canvas”：initChatStarmap 开头对已存在的
// starmapRen 是早退的，所以光清 canvas 不会触发重拉。
async function starmapRefreshFull() {
  if (state.starmapLoading) return;
  state.starmapLoading = true;
  starmapDirty = false;
  await loadChatStarmapData();
  if (starmapRen) {
    buildChatStarmapGraph();
    try {
      starmapRen.render(starmapScene, starmapCam);
    } catch (e) {}
  }
}

async function loadChatStarmapData() {
  try {
    var resp = await api("/memory/graph");
    if (
      !resp ||
      !resp.success ||
      !resp.data ||
      !resp.data.nodes ||
      resp.data.nodes.length === 0
    ) {
      document.getElementById("sm-container-chat").innerHTML =
        '<p style="color:var(--text-muted);padding:20px;text-align:center">' +
        __("暂无记忆数据", "No memory data") +
        "</p>";
      state.starmapInit = true;
      state.starmapLoading = false;
      return;
    }
    var d = resp.data;
    starmapNodes = d.nodes || [];
    starmapEdges = d.edges || [];
    state.starmapInit = true;
    state.starmapLoading = false;
    initChatStarmap();
  } catch (e) {
    document.getElementById("sm-container-chat").innerHTML =
      '<p style="color:var(--text-muted);padding:20px;text-align:center">' +
      __("加载失败", "Load failed") +
      "</p>";
    state.starmapInit = true;
    state.starmapLoading = false;
  }
}

// starmapStartActivity 启动两路活动轮询（幂等）。
//
// 两个 setInterval 的 id **必须分开记**：写进同一个字段会被后者覆盖，
// 于是 stopStarmapActivity 只能清掉一个，留下一个永远跑的僵尸定时器。
// （这是 WebUI dashboard.js 里踩过并注释下来的坑，这里跟着分开。）
function starmapStartActivity() {
  if (state.starmapPulseTimer) return;
  starmapPullActivity();
  starmapPullPulse();
  state.starmapPulseTimer = setInterval(starmapPullActivity, 3000);
  state.starmapActivityTimer = setInterval(starmapPullPulse, 10000);
}

// starmapStopActivity 停掉两路轮询（切连接 / 星图重建时用）。
function starmapStopActivity() {
  if (state.starmapPulseTimer) {
    clearInterval(state.starmapPulseTimer);
    state.starmapPulseTimer = null;
  }
  if (state.starmapActivityTimer) {
    clearInterval(state.starmapActivityTimer);
    state.starmapActivityTimer = null;
  }
}

// starmapPullActivity 拉 /runtime 快照，把调度器状态映射成图的整体节奏。
//
// ★ 关键：**只读已有的 state.runtime，不自己再发一轮请求**。
// refreshDataOnly() 已经每 15s 取过 /runtime；这里如果再拉，就是把
// 「秒级刷新的活动源」变成「凭空多出来的高频轮询」。
// /runtime 的设计意图正是这条注释（handler.go）：让前端秒级刷新而不必
// 反复拉 30KB 的 /kernel。
function starmapPullActivity() {
  if (!starmapScene) return;
  var s = state.runtime;
  if (!s || !s.scheduler) return;
  var prev = state._smPrevSched;
  if (prev) {
    var sum = function (a) {
      return (a || []).reduce(function (x, y) {
        return x + y;
      }, 0);
    };
    // 队列变深 ⇒ 图整体「绷紧」；中断/抢占上升 ⇒ 强脉冲。
    // 判定用差值而不是绝对值：绝对值在长会话里会一直触发，脉冲退化成常亮。
    if ((s.ready_queue_depth || 0) > (prev.ready_queue_depth || 0)) {
      starmapPulse("tool", null);
    }
    if (
      sum(s.interrupts_by_level) - sum(prev.interrupts_by_level) > 0 ||
      sum(s.preempts_by_level) - sum(prev.preempts_by_level) > 0
    ) {
      starmapPulse("output", null);
    }
  }
  state._smPrevSched = s.scheduler;
}

// starmapPullPulse 拉轻量活动端点，把新长出来的节点标记为「生长」。
function starmapPullPulse() {
  if (!starmapScene) return;
  var since = state.starmapPulseSince || 0;
  api("/memory/graph/pulse?since=" + since)
    .then(function (r) {
      if (!r || !r.success || !r.data) return;
      var now = Math.floor(Date.now() / 1000);
      // 30s 重叠窗口：宁可重复点亮几个，也不漏掉刚好卡在边界上的节点。
      state.starmapPulseSince = now - 30;
      var nodes = r.data.nodes || [];
      if (!nodes.length) return;
      var known = 0;
      nodes.forEach(function (n) {
        var m = starmapNodeMeshes.find(function (x) {
          return x.userData.nodeId === n.id;
        });
        // 全量图里没有（可能刚创建）⇒ 本拍忽略，等下次全量重拉。
        if (!m) return;
        known++;
        state.starmapGrown[m.userData.nodeId] = Date.now() + SM_PULSE_MS * 2;
        starmapPulse("grow", n.name);
      });
      // 有新实体却一个都没匹配上 ⇒ 全量图已经过期，标脏让下一拍重拉。
      // 阈值取 2：单双节点抖动（同一实体反复 mention）不该触发全量重拉。
      //
      // ★ 惰性重拉：**不能**在脉冲回调里直接重拉。pulse 是 10s 一次的，
      // 而 refreshDataOnly / SSE 也会调 renderChatStarmap；若在这里直接拉，
      // 就把「几 KB 的轻量活动源」变成「每 10s 拉一次 408KB 全量」——
      // 正好是 pulse 端点存在的理由。所以只置标志，由下一次
      // renderChatStarmap 消费。
      if (known === 0 && nodes.length > 2) starmapDirty = true;
    })
    .catch(function () {});
}

// starmapPulse 发出一次活动脉冲。kind: "tool" | "stage" | "output" | "grow"
function starmapPulse(kind, hint) {
  if (!starmapScene || !starmapNodeMeshes.length) return;
  var now = Date.now();
  state.starmapLastPulseAt = now;
  var targets = starmapPickPulseTargets(hint);
  if (!targets.length) return;
  var life = kind === "grow" ? SM_PULSE_MS * 1.6 : SM_PULSE_MS;
  targets.forEach(function (m) {
    // 去重：同一个节点已在队列里就只延长到期时间，不再 push 新项。
    // 否则「stage 事件 + tool_call 事件」会点亮两次同一个节点，
    // 队列里出现重复项，脉冲结束时要复位两次。
    var existing = null;
    for (var i = 0; i < state.starmapPulses.length; i++) {
      if (state.starmapPulses[i].mesh === m) {
        existing = state.starmapPulses[i];
        break;
      }
    }
    if (existing) {
      existing.until = now + life;
      existing.kind = kind;
    } else {
      state.starmapPulses.push({ mesh: m, until: now + life, kind: kind });
    }
  });
  // 队列上限：密集工具调用时脉冲无限堆积会占内存。
  if (state.starmapPulses.length > 260)
    state.starmapPulses = state.starmapPulses.slice(-260);
}

// starmapPickPulseTargets 选出该被点亮的节点。
//
// ★ 匹配必须用「词」而不是子串包含：hint="knowledge" 与实体名 "k"/"e"
// 互为子串，会让半个图谱（含「时」「会」这类单字实体）全部命中 ⇒
// 脉冲退化成「全图齐亮」，既看不出关联，又把队列瞬间打满。
// （WebUI 实测：旧写法一次 pulse 选中 250 个节点、队列顶到 260 上限。）
function starmapPickPulseTargets(hint) {
  var out = [];
  if (!starmapNodeMeshes.length) return out;
  if (hint) {
    var keys = starmapHintTokens(hint);
    if (keys.length) {
      for (var i = 0; i < starmapNodeMeshes.length && out.length < 26; i++) {
        var nd = starmapNodeMeshes[i].userData.nodeData || {};
        var nm = String(nd.name || "").toLowerCase();
        var ty = String(nd.type || "").toLowerCase();
        if (!nm) continue;
        for (var k = 0; k < keys.length; k++) {
          var key = keys[k];
          // 词边界命中：恰好等于该词，或以该词为词首（knowledge_base 命中 knowledge）。
          if (nm === key || nm.indexOf(key + "_") === 0 || ty === key) {
            out.push(starmapNodeMeshes[i]);
            break;
          }
        }
      }
    }
  }
  // 不足时按 mention_count 补齐：高权重节点本身就是最常被 agent 触碰的，
  // 用它们代表「整体活动」合理。
  if (out.length < 8) {
    var cand = starmapNodeMeshes
      .filter(function (m) {
        return out.indexOf(m) === -1;
      })
      .sort(function (a, b) {
        return (
          (b.userData.nodeData.mention_count || 0) -
          (a.userData.nodeData.mention_count || 0)
        );
      });
    for (var c = 0; c < cand.length && out.length < 8; c++) out.push(cand[c]);
  }
  return out;
}

// starmapHintTokens 把提示切成「词」（≥3 字符），供上面做词边界匹配。
function starmapHintTokens(hint) {
  var raw = String(hint)
    .toLowerCase()
    .split(/[^a-z0-9\u4e00-\u9fa5]+/);
  var out = [];
  for (var i = 0; i < raw.length; i++) {
    var t = raw[i];
    if (t.length >= 3 && out.indexOf(t) === -1) out.push(t);
  }
  return out;
}

// starmapTickActivity 在渲染循环里推进所有脉冲与余晖。
//
// 挂在已有的 starmapAnimate（~12fps）里，不额外起定时器 ——
// 12fps 对「发光冲高 + 缩放」这类动画足够，多起一个 rAF 只会和它抢帧。
function starmapTickActivity() {
  var now = Date.now();
  // 1) 活动脉冲：发光冲高 + 尺寸微扩
  if (state.starmapPulses.length) {
    var keep = [];
    for (var i = 0; i < state.starmapPulses.length; i++) {
      var p = state.starmapPulses[i];
      if (p.until <= now) {
        p.mesh.material.emissiveIntensity = p.mesh.userData.baseEmissive;
        p.mesh.scale.setScalar(p.mesh.userData.baseScale || 1);
        continue;
      }
      keep.push(p);
      var left = (p.until - now) / SM_PULSE_MS; // 1→0
      var k = 1 - left; // 0→1
      var wave = Math.sin(Math.min(1, k) * Math.PI);
      var amp = p.kind === "output" ? 1.8 : 1.2;
      p.mesh.material.emissiveIntensity = p.mesh.userData.baseEmissive + wave * amp;
      // hover 期间不覆写 scale（会打断 hover 的 1.2 倍高亮）
      if (p.mesh !== starmapHovered)
        p.mesh.scale.setScalar(p.mesh.userData.baseScale * (1 + wave * 0.28));
      // 生长：新节点从 0 弹到正常大小
      if (p.kind === "grow") {
        var g = Math.min(1, k * 1.4);
        p.mesh.scale.setScalar(p.mesh.userData.baseScale * (0.15 + 0.85 * g));
      }
    }
    state.starmapPulses = keep;
  }
  // 2) 「生长」余晖：脉冲结束后短暂保留一点亮
  if (state.starmapGrown) {
    for (var gid in state.starmapGrown) {
      if (state.starmapGrown[gid] <= now) {
        delete state.starmapGrown[gid];
        continue;
      }
      var gm = starmapNodeMeshes.find(function (x) {
        return String(x.userData.nodeId) === gid;
      });
      if (gm)
        gm.material.emissiveIntensity = Math.max(
          gm.material.emissiveIntensity,
          gm.userData.baseEmissive + 0.6,
        );
    }
  }
  // 3) 全局呼吸：距上次活动越近越亮，实现「agent 一忙图就活」。
  // 只抽样一部分节点，避免每帧改 1151 个材质。
  var idle = (now - (state.starmapLastPulseAt || 0)) / 4000;
  var breathe = Math.max(0, 1 - idle);
  if (breathe > 0.01 && starmapNodeMeshes.length) {
    var stride = 24;
    for (var b = 0; b < starmapNodeMeshes.length; b += stride) {
      var m2 = starmapNodeMeshes[b];
      if (m2.userData.baseEmissive === undefined) continue;
      if (m2 === starmapHovered) continue;
      m2.material.emissiveIntensity =
        m2.userData.baseEmissive + breathe * 0.25;
    }
  }
}

function getStarmapBg() {
  return 0x0a0a1a;
}

function initChatStarmap() {
  var cont = document.getElementById("sm-container-chat");
  if (!cont) return;
  var rect = cont.getBoundingClientRect();
  var w = Math.max(rect.width || 300, 100);
  var h = Math.max(rect.height || 250, 100);
  if (starmapRen) {
    starmapRen.setSize(w, h);
    cont.appendChild(starmapRen.domElement);
    starmapRen.domElement.style.display = "block";
    return;
  }
  starmapScene = new THREE.Scene();
  starmapScene.fog = new THREE.FogExp2(0x0a0a1a, 0.015);
  starmapCam = new THREE.PerspectiveCamera(60, w / h, 0.1, 2000);
  starmapCam.position.set(0, 20, 40);
  // 性能：关抗锯齿 + pixelRatio 1（装饰背景，视觉差异极小，GPU 负载显著下降）
  starmapRen = new THREE.WebGLRenderer({ antialias: false, alpha: true });
  starmapRen.setSize(w, h);
  starmapRen.setPixelRatio(1);
  starmapRen.setClearColor(0x0a0a1a, 1);
  cont.innerHTML = "";
  cont.appendChild(starmapRen.domElement);
  starmapCtrl = new THREE.OrbitControls(starmapCam, starmapRen.domElement);
  starmapCtrl.enableDamping = true;
  starmapCtrl.dampingFactor = 0.05;
  starmapCtrl.rotateSpeed = 0.5;
  starmapCtrl.zoomSpeed = 0.8;
  var al = new THREE.AmbientLight(0x444466, 0.6);
  starmapScene.add(al);
  var dl = new THREE.DirectionalLight(0xffffff, 0.8);
  dl.position.set(50, 100, 50);
  starmapScene.add(dl);
  createStarField();
  createNebula();
  buildChatStarmapGraph();
  starmapRen.domElement.addEventListener("mousemove", onStarmapMove);
  starmapRen.domElement.addEventListener("click", onStarmapClick);
  window.addEventListener("resize", onStarmapResize);
  // 活动数据源：/runtime 3s + /memory/graph/pulse 10s。
  //
  // 为什么要接：服务端**专门**为星图造了 /memory/graph/pulse 这个轻量活动
  // 端点（handler_memory.go 的注释写了动机：生产实例全量图谱 408KB /
  // 1151 节点 / 866 边，为了「知道哪些节点是新的」而每 N 秒拉一次全量，
  // 是把带宽和 JSON.parse 全花在重复数据上；pulse 只回 id+name+type+
  // mention_count+updated_at，几百字节 ~ 几 KB，差两个数量级）。
  //
  // 而 GUI 此前**只在初始化时拉一次**全量 /memory/graph 且完全没有轮询
  // ⇒ 星图停在打开那一刻的快照，agent 后面学的东西它永远看不到。
  // WebUI dashboard 接了这个端点，GUI 没接 —— 这是两端的一次真实漂移。
  //
  // 幂等：重复 initChatStarmap（切连接/重建）不会起第二轮定时器。
  starmapStartActivity();
  if (starmapRaf) cancelAnimationFrame(starmapRaf);
  // 首次加载强制渲染一帧（即使星图面板未激活，切换过去也有内容）
  try {
    if (starmapRen && starmapScene && starmapCam)
      starmapRen.render(starmapScene, starmapCam);
  } catch (e) {}
  starmapAnimate();
}
// ===== BEGIN shared:starmap/layout =====
// 星图力导向布局 —— WebUI 与 GUI 的**唯一事实源**。
//
// ===================================================================
// 这个文件为什么存在（别把它当成“又一份副本”）
// ===================================================================
//
// 下面这三个函数原本在 `internal/plugins/webui/dashboard.js` 与
// `cmd/gui/renderer/app.js` 里各有一份手抄。分叉的代价已经兑现过两次：
//
//   1. WebUI 把 O(n²) 布局换成网格近邻（生产库 3754 块：3.52 亿次配对 →
//      n·k 次），GUI 那份没跟着改 ⇒ 换客户端卡顿原样存在。
//   2. 把空闲帧跳过从 WebUI 抄进 GUI 时抄了 `starmapMorphSet`（WebUI 独有），
//      GUI 实际用 `state.starmapGrown` ⇒ 渲染循环每帧 ReferenceError、星图 0 帧。
//
// 所以这里放**唯一一份**实现，由 `tools/starmap-sync.mjs` 写进两个客户端文件
// 的 `BEGIN/END shared:starmap/layout` 标记块内，并由判据钉住“三处逐字节相同”。
//
// ===================================================================
// 算法
// ===================================================================
//
// 被替换掉的旧实现是 50 轮 `for i / for j=i+1` 全对全斥力：
// 生产库 3754 块 ⇒ 每轮 704 万次配对、50 轮 3.52 亿次。实测（CDP Profiler
// + Electron）单次建图阻塞主线程 1.8–10.9 秒。
//
// 关键观察：原斥力体里本来就有 `if (d < 25)` —— **只对近邻施力**。既然只关心
// 近邻，就没必要算全对全：把空间切成边长 == 斥力半径的格子，每个节点只与
// 「自身格 + 相邻 26 格」（3×3×3）内的点比较即可。
//
// ★ 为何格边长必须恰等于斥力半径：这样「距离 < 25 的点」必然落在相邻 26 格内，
//   **绝不遗漏** —— 这是敢跳过远处配对的唯一依据。格边长一旦小于斥力半径，
//   就会漏掉真实邻居，图会塌成一团。
//
// 复杂度 O(n·k)：均匀分布下每格约 1 点 ⇒ 每轮约 27n 次比较。
// 配合迭代数自适应与收敛早停，3754 节点实测 8365ms → 438ms。

// 自适应迭代数：小图跑满 50 轮（值得精细），大图退到 14 轮。
//
// n=3754 时约 15 轮。旧实现固定 50 轮，且每轮是 O(n²)。
// 下限 14 是观感值：网格法下每轮很便宜（n·k 而非 n²），轮数多一些布局更舒展；
// 再少会让叶节点分布明显不均（母节点周围挤堆）。
function starmapLayoutIterationsFor(n) {
	if (n <= 400) return 50;
	var it = Math.floor(60000 / n);
	if (it < 14) it = 14;
	if (it > 50) it = 50;
	return it;
}

// 早停阈值：取「节点平均间距的 0.2%」。
// 用相对量而不是绝对值，否则同一个阈值在不同规模的图上含义完全不同。
function starmapLayoutMaxMoveThreshold(n, maxR) {
	var perimeter = 2 * Math.PI * maxR;
	var gap = n > 0 ? perimeter / n : maxR;
	var th = gap * 0.002;
	var cap = maxR * 2 * 0.001;
	return th < cap ? th : cap;
}

// starmapLayoutRelax 就地松弛坐标。
//
// 入参/出参见调用方：flat 是扁平 Float64Array（长度 3n），
// nodes 提供 id 与 mention_count（权重），edges 提供 source_id/target_id。
// 用扁平数组而非对象数组，是为了让可行时能直接走结构化克隆交给 Worker。
function starmapLayoutRelax(flat, nodes, edges, maxR) {
	var n = nodes.length;
	if (n === 0) return flat;
	var CUTOFF = 25; // == 斥力半径，见文件头说明
	var BASE_R = 15;
	// 归一化权重（斥力/吸力都按它加权）
	var mn = new Float64Array(n);
	var maxMc = 1,
		minMc = 0;
	for (var a = 0; a < n; a++) {
		var m = (nodes[a] && nodes[a].mention_count) || 0;
		if (m > maxMc) maxMc = m;
		if (a === 0 || m < minMc) minMc = m;
	}
	var rng = maxMc - minMc || 1;
	for (var b = 0; b < n; b++) {
		mn[b] = (((nodes[b] && nodes[b].mention_count) || 0) - minMc) / rng;
	}
	// 边下标（id → 下标对）。下标化是为了内层循环不做字符串比较。
	var idx = {};
	for (var i0 = 0; i0 < n; i0++) idx[nodes[i0].id] = i0;
	var edgeIdx = [];
	for (var e0 = 0; e0 < edges.length; e0++) {
		var s0 = idx[edges[e0].source_id],
			t0 = idx[edges[e0].target_id];
		if (s0 === undefined || t0 === undefined || s0 === t0) continue;
		edgeIdx.push([s0, t0]);
	}
	var grid = new Map();
	function rebuildGrid() {
		grid.clear();
		for (var i = 0; i < n; i++) {
			var key =
				Math.floor(flat[i * 3] / CUTOFF) +
				"|" +
				Math.floor(flat[i * 3 + 1] / CUTOFF) +
				"|" +
				Math.floor(flat[i * 3 + 2] / CUTOFF);
			var bucket = grid.get(key);
			if (!bucket) grid.set(key, (bucket = []));
			bucket.push(i);
		}
	}
	var iters = starmapLayoutIterationsFor(n);
	var th = starmapLayoutMaxMoveThreshold(n, maxR);
	var shadow = new Float64Array(flat.length);
	shadow.set(flat);
	for (var it = 0; it < iters; it++) {
		rebuildGrid();
		// 斥力：只算 3×3×3 邻格内的近邻；每对只处理一次（j > i），
		// 与旧实现 `for j = i+1` 的语义一致，避免重复施力。
		for (var i1 = 0; i1 < n; i1++) {
			var cxi = Math.floor(flat[i1 * 3] / CUTOFF);
			var cyi = Math.floor(flat[i1 * 3 + 1] / CUTOFF);
			var czi = Math.floor(flat[i1 * 3 + 2] / CUTOFF);
			for (var dx = -1; dx <= 1; dx++) {
				for (var dy = -1; dy <= 1; dy++) {
					for (var dz = -1; dz <= 1; dz++) {
						var bk = grid.get(cxi + dx + "|" + (cyi + dy) + "|" + (czi + dz));
						if (!bk) continue;
						for (var bi = 0; bi < bk.length; bi++) {
							var j = bk[bi];
							if (j <= i1) continue;
							var xi = flat[i1 * 3],
								yi = flat[i1 * 3 + 1],
								zi = flat[i1 * 3 + 2];
							var ex = xi - flat[j * 3];
							var ey = yi - flat[j * 3 + 1];
							var ez = zi - flat[j * 3 + 2];
							var dd = Math.sqrt(ex * ex + ey * ey + ez * ez) + 0.1;
							if (dd >= CUTOFF) continue;
							var rf = 0.5 + (mn[i1] + mn[j]) * 0.5;
							var force = (0.06 * rf) / Math.max(dd, 0.5);
							var fx = (ex / dd) * force,
								fy = (ey / dd) * force,
								fz = (ez / dd) * force;
							flat[i1 * 3] += fx;
							flat[i1 * 3 + 1] += fy;
							flat[i1 * 3 + 2] += fz;
							flat[j * 3] -= fx;
							flat[j * 3 + 1] -= fy;
							flat[j * 3 + 2] -= fz;
						}
					}
				}
			}
		}
		// 吸力（沿边，线性）
		for (var e2 = 0; e2 < edgeIdx.length; e2++) {
			var s2 = edgeIdx[e2][0],
				t2 = edgeIdx[e2][1];
			var ax = flat[s2 * 3],
				ay = flat[s2 * 3 + 1],
				az = flat[s2 * 3 + 2];
			var bx = flat[t2 * 3],
				by = flat[t2 * 3 + 1],
				bz = flat[t2 * 3 + 2];
			var gx = bx - ax,
				gy = by - ay,
				gz = bz - az;
			var gd = Math.sqrt(gx * gx + gy * gy + gz * gz) + 0.1;
			if (gd <= 20) continue;
			var af = 0.04 * Math.max(0.3, 1.0 - (mn[s2] + mn[t2]) * 0.3);
			var ux = (gx / gd) * af,
				uy = (gy / gd) * af,
				uz = (gz / gd) * af;
			flat[s2 * 3] += ux;
			flat[s2 * 3 + 1] += uy;
			flat[s2 * 3 + 2] += uz;
			flat[t2 * 3] -= ux;
			flat[t2 * 3 + 1] -= uy;
			flat[t2 * 3 + 2] -= uz;
		}
		// 向心约束 + 早停位移统计（一次遍历同时做完，省一趟内存读）
		var maxA = maxR * 1.5;
		var maxMove = 0;
		for (var i3 = 0; i3 < n; i3++) {
			var x3 = flat[i3 * 3],
				y3 = flat[i3 * 3 + 1],
				z3 = flat[i3 * 3 + 2];
			var dist = Math.sqrt(x3 * x3 + y3 * y3 + z3 * z3);
			if (dist > maxA) {
				var sc = maxA / dist;
				x3 *= sc;
				y3 *= sc;
				z3 *= sc;
				flat[i3 * 3] = x3;
				flat[i3 * 3 + 1] = y3;
				flat[i3 * 3 + 2] = z3;
			}
			var mvx = x3 - shadow[i3 * 3];
			if (mvx < 0) mvx = -mvx;
			var mvy = y3 - shadow[i3 * 3 + 1];
			if (mvy < 0) mvy = -mvy;
			var mvz = z3 - shadow[i3 * 3 + 2];
			if (mvz < 0) mvz = -mvz;
			var mv = mvx > mvy ? mvx : mvy;
			if (mvz > mv) mv = mvz;
			if (mv > maxMove) maxMove = mv;
		}
		shadow.set(flat);
		if (maxMove < th) break; // 收敛：剩余轮次只是重复同一状态
	}
	return flat;
}
// 本块由 tools/starmap-sync.mjs 从 shared/starmap/layout.js 生成 —— 请改共享源，不要直接改这里。
// ===== END shared:starmap/layout =====


var starmapUnitSphere = null;
// starmapGlowMesh：全部节点的光晕合并成的一个 InstancedMesh，以及它的构建参数。
//
// 为何合并（2026-10-13）：原每节点一个光晕子 mesh（1294 个），使 draw call
// 达 3477/帧。光晕是纯静态装饰（glowSphere 全仓只有写入、从无读取）。
var starmapGlowMesh = null;
var starmapGlowItems = [];

// starmapBuildGlowInstances 把全部节点的光晕球合并成一个 InstancedMesh。
//
// ★ 半径换算：主球 mesh 是按 baseScale(=rad/0.5) 缩放的，而光晕现在脱离了
//   该缩放链，所以必须用**世界半径**直接调 scale：共享单位球半径 0.5 ⇒
//   scale = worldRadius / 0.5。原先子 mesh 的相对换算 (gr/parentScale)*2
//   在这里不适用（那是为挂在缩放过的父节点上算的）。
function starmapBuildGlowInstances(items, pos) {
  if (!items.length) return;
  var unit = starmapSphereGeometry();
  var gm = new THREE.MeshBasicMaterial({
    transparent: true,
    opacity: 0.16,
    side: THREE.BackSide,
    blending: THREE.AdditiveBlending,
  });
  var inst = new THREE.InstancedMesh(unit, gm, items.length);
  inst.instanceMatrix.setUsage(THREE.StaticDrawUsage);
  var dummy = new THREE.Object3D();
  var c = new THREE.Color();
  for (var i = 0; i < items.length; i++) {
    var it = items[i];
    var p = pos[it.id];
    if (!p) {
      dummy.position.set(0, 0, 0);
      dummy.scale.setScalar(0);
    } else {
      dummy.position.set(p.x, p.y, p.z);
      dummy.scale.setScalar(it.worldRadius / 0.5);
    }
    dummy.updateMatrix();
    inst.setMatrixAt(i, dummy.matrix);
    inst.setColorAt(i, c.setHex(it.color));
  }
  inst.instanceMatrix.needsUpdate = true;
  if (inst.instanceColor) inst.instanceColor.needsUpdate = true;
  inst.frustumCulled = false;
  starmapScene.add(inst);
  starmapGlowMesh = inst;
  starmapGlowItems = items;
}

function starmapSphereGeometry() {
  if (!starmapUnitSphere) starmapUnitSphere = new THREE.SphereGeometry(0.5, 16, 12);
  return starmapUnitSphere;
}

// 释放一个 three.js 子树占用的 GPU 资源（geometry / material / texture）。
//
// 递归处理子树：节点 mesh 下挂着 glow sphere（子 mesh）与 label sprite，
// 只 dispose 顶层会漏掉子节点的资源。
//
// 共享资源要防误 dispose：starmapUnitSphere 被全场景共用，
// dispose 掉它会让仍在渲染的 mesh 变黑/报错 —— 用 seen 集合兜底。
function disposeStarmapObject(root) {
  var seenGeo = new Set();
  var seenMat = new Set();
  var seenTex = new Set();
  if (starmapScene) starmapScene.remove(root);
  root.traverse(function (o) {
    var g = o.geometry;
    if (g && g.dispose && !seenGeo.has(g) && g !== starmapUnitSphere) {
      seenGeo.add(g);
      g.dispose();
    }
    var m = o.material;
    if (!m) return;
    var mats = Array.isArray(m) ? m : [m];
    mats.forEach(function (mm) {
      if (!mm || seenMat.has(mm)) return;
      seenMat.add(mm);
      // 纹理在 material.map / 上多通道贴图上。
      Object.keys(mm).forEach(function (k) {
        var v = mm[k];
        if (v && v.isTexture && !seenTex.has(v)) {
          seenTex.add(v);
          v.dispose();
        }
      });
      if (mm.dispose) mm.dispose();
    });
  });
  if (root.geometry && root.geometry.dispose &&
      root.geometry !== starmapUnitSphere && !seenGeo.has(root.geometry)) {
    root.geometry.dispose();
  }
  var rm = root.material;
  if (rm && !Array.isArray(rm) && !seenMat.has(rm)) {
    Object.keys(rm).forEach(function (k) {
      var v = rm[k];
      if (v && v.isTexture && !seenTex.has(v)) v.dispose();
    });
    if (rm.dispose) rm.dispose();
  }
}

function buildChatStarmapGraph() {
  // ★ 先释放上一轮的 GPU 资源，再重建（2026-10-05 修内存暴涨）。
  //
  // 原实现只 remove 不 dispose。remove 只改场景树，geometry/material/texture
  // 仍占着 WebGL 资源；节点数还会随记忆增长，于是泄漏只增不减。
  starmapNodeMeshes.forEach(function (m) {
    disposeStarmapObject(m);
  });
  starmapEdgeLines.forEach(function (l) {
    disposeStarmapObject(l);
  });
  starmapNodeMeshes = [];
  starmapEdgeLines = [];
  // ★ 光晕实例要一并释放（同一次重建里它会被重建）。
  if (starmapGlowMesh) {
    starmapScene.remove(starmapGlowMesh);
    if (starmapGlowMesh.material) starmapGlowMesh.material.dispose();
    // geometry 是共享单位球（starmapSphereGeometry），不能 dispose。
    starmapGlowMesh = null;
  }
  var glowItems = [];
  if (starmapNodes.length === 0) return;
  // Calculate node degrees for leaf node detection
  var nodeDegs = {};
  starmapNodes.forEach((n) => {
    nodeDegs[n.id] = 0;
  });
  starmapEdges.forEach((e) => {
    nodeDegs[e.source_id] = (nodeDegs[e.source_id] || 0) + 1;
    nodeDegs[e.target_id] = (nodeDegs[e.target_id] || 0) + 1;
  });
  var nodeMap = {};
  starmapNodes.forEach((n) => {
    nodeMap[n.id] = n;
  });
  var sorted = starmapNodes
    .slice()
    .sort((a, b) => (b.mention_count || 0) - (a.mention_count || 0));
  var mc = sorted.map((n) => n.mention_count || 0);
  var maxMc = Math.max(...mc, 1),
    minMc = Math.min(...mc, 0),
    rng = maxMc - minMc || 1;
  // Layout positions
  var pos = {};
  var baseR = 15,
    maxR = 80;
  var total = sorted.length;
  var acc = 0;
  sorted.forEach((n, i) => {
    var m = n.mention_count || 0,
      mn = rng > 0 ? (m - minMc) / rng : 0;
    var radius = baseR + mn * (maxR - baseR);
    var baseStep = (Math.PI * 2) / total;
    var extra = mn * baseStep * 2;
    var angle = acc + extra / 2;
    acc += baseStep + extra;
    pos[n.id] = {
      x: radius * Math.cos(angle),
      y: (Math.random() - 0.5) * (10 + mn * 20),
      z: radius * Math.sin(angle),
      mn: mn,
      rad: radius,
    };
  });
  // Leaf nodes (degree 1) reposition near parent
  sorted.forEach((n) => {
    var deg = nodeDegs[n.id] || 0;
    if (deg !== 1) return;
    var edge = starmapEdges.find(
      (e) => e.source_id === n.id || e.target_id === n.id,
    );
    if (!edge) return;
    var parentId = edge.source_id === n.id ? edge.target_id : edge.source_id;
    if (!pos[parentId]) return;
    var pp = pos[parentId];
    var m = n.mention_count || 0,
      mn = rng > 0 ? (m - minMc) / rng : 0;
    var off = 6 + mn * 8 + Math.random() * 4;
    var a2 = Math.random() * Math.PI * 2;
    pos[n.id] = {
      x: pp.x + off * Math.cos(a2),
      y: pp.y + (Math.random() - 0.5) * (4 + mn * 6),
      z: pp.z + off * Math.sin(a2),
      mn: mn,
      rad: off,
    };
  });
  // Force-directed simulation
  //
  // ★★ 2026-10-13：O(n²) → O(n·k) 网格法（与 WebUI dashboard.js 同一算法）。
  //
  // 旧实现在此内联跑 50 轮 `for i / for j=i+1` 全对全斥力。实测（Electron
  // + 真生产库 3754 块）：**单次建图阻塞主线程 1.8–9 秒**。而斥力体里本来
  // 就有 `if (d < 25)` —— 只对近邻施力。既然只关心近邻，全对全就是白算：
  // 把空间切成边长 == 斥力半径的格子，每点只比自身格 + 相邻 26 格。
  // 距离 < 25 的点必然落在相邻格内，**不遗漏**（这是跳过远处配对的唯一依据）。
  //
  // ★ 为何这里不像 WebUI 那样走 Worker：GUI 用 file:// 加载页面（main.js 的
  //   loadFile），外部 Worker 脚本与 Blob Worker 的可靠性都需要在打包环境
  //   里单独验证，不能在“性能修复”里顺带赌上星图能否初始化。网格法本身
  //   已把建图从秒级降到百毫秒级，先把它坐实。
  var flatLayout = new Float64Array(starmapNodes.length * 3);
  for (var li = 0; li < starmapNodes.length; li++) {
    var lp = pos[starmapNodes[li].id];
    flatLayout[li * 3] = lp.x;
    flatLayout[li * 3 + 1] = lp.y;
    flatLayout[li * 3 + 2] = lp.z;
  }
  starmapLayoutRelax(flatLayout, starmapNodes, starmapEdges, maxR);
  for (var lj = 0; lj < starmapNodes.length; lj++) {
    var lq = pos[starmapNodes[lj].id];
    if (!lq) continue;
    lq.x = flatLayout[lj * 3];
    lq.y = flatLayout[lj * 3 + 1];
    lq.z = flatLayout[lj * 3 + 2];
  }
  // Create nodes
  starmapNodes.forEach((n) => {
    var p = pos[n.id];
    if (!p) return;
    var mn = n.mention_count || 0,
      mnr = rng > 0 ? (mn - minMc) / rng : 0;
    var rad = 0.5 + mnr * 2.0;
    var col = smTypeColors[n.type] || 0xcccccc;
    var ei = 0.3 + mnr * 0.7;
    var g = starmapSphereGeometry();
    var mat = new THREE.MeshPhongMaterial({
      color: col,
      emissive: col,
      emissiveIntensity: ei,
      shininess: 30,
    });
    var mesh = new THREE.Mesh(g, mat);
    mesh.position.set(p.x, p.y, p.z);
    mesh.userData.nodeData = n;
    mesh.userData.nodeId = n.id;
    mesh.userData.baseEmissive = ei;
    // baseScale 必须记：脉冲结束时要把 scale 复原到「按 mention_count
    // 缩放后」的值，而不是 set(1,1,1)—— 那会把大节点缩成最小尺寸。
    mesh.userData.baseScale = rad / 0.5;
    // Glow sphere
    // Glow sphere —— 共享单位球，靠 scale 表达半径（详见 starmapSphereGeometry）。
    var gr = rad * 1.2 + mnr * 0.5;
    // mesh 已按 rad/baseScale 缩放过（baseScale = rad/0.5），故 glow 用
    // 相对比例：世界半径 gr = mesh 缩放 × 本地 scale。
    var parentScale = mesh.scale.x || 1;
    // ★★ 2026-10-13：光晕不再每节点一个子 mesh（与 WebUI 同改）。
    //
    // GUI 原先 1294 节点 = 1294 球 + 1294 光晕 + 1294 标签 + 966 条边
    // ⇒ renderer.info.render.calls 达 **3477/帧**，实测 render() 19–40ms
    // 且 1.8–2.5s 的建图里有 433ms 花在「第一次提交时上传 1294 张纹理」。
    //
    // 光晕是**纯静态装饰**（全仓 grep：glowSphere 只有写入、从无读取，
    // 任何动画都不碰它），故合并成单个 InstancedMesh：1294 → 1 draw call。
    glowItems.push({ id: n.id, worldRadius: gr, color: col });
    // Label sprite
    var canvas = document.createElement("canvas");
    canvas.width = 256;
    canvas.height = 64;
    var ctx = canvas.getContext("2d");
    ctx.clearRect(0, 0, 256, 64);
    ctx.font = "Bold 24px Courier New";
    ctx.textAlign = "center";
    ctx.textBaseline = "middle";
    ctx.shadowColor = "#aaccff";
    ctx.shadowBlur = 8;
    ctx.fillStyle = "#ffffff";
    ctx.fillText((n.name || n.id).substring(0, 12), 128, 32);
    var tex = new THREE.CanvasTexture(canvas);
    tex.needsUpdate = true;
    var spMat = new THREE.SpriteMaterial({
      map: tex,
      transparent: true,
      opacity: 0.9,
      depthTest: false,
      depthWrite: false,
      blending: THREE.AdditiveBlending,
    });
    var sprite = new THREE.Sprite(spMat);
    sprite.scale.set(8, 2, 1);
    sprite.position.y = rad + 2;
    mesh.add(sprite);
    starmapScene.add(mesh);
    starmapNodeMeshes.push(mesh);
  });
  // Create edges
  //
  // ★ 合并成按颜色分组的 LineSegments（与 WebUI 同改）：
  //   原来 966 条边 = 966 个独立 Line + 966 个 BufferGeometry，
  //   实测隐藏这 966 条就省下 ~8ms/帧（~40% 的 render 成本）。
  //   代价：单条边不再能单独点选 —— GUI 此前也没有点选边的交互。
  var edgeByColor = {};
  starmapEdges.forEach((e) => {
    var a = pos[e.source_id],
      b = pos[e.target_id];
    if (!a || !b) return;
    var col = smEdgeColors[e.relation_type] || smEdgeColors[e.type] || 0x444466;
    var k = String(col);
    if (!edgeByColor[k]) edgeByColor[k] = { col: col, pts: [] };
    edgeByColor[k].pts.push(a.x, a.y, a.z, b.x, b.y, b.z);
  });
  Object.keys(edgeByColor).forEach((k) => {
    var g = edgeByColor[k];
    if (!g.pts.length) return;
    var geo = new THREE.BufferGeometry();
    geo.setAttribute("position", new THREE.Float32BufferAttribute(g.pts, 3));
    var mat = new THREE.LineBasicMaterial({
      color: g.col,
      transparent: true,
      opacity: 0.4,
    });
    var seg = new THREE.LineSegments(geo, mat);
    starmapScene.add(seg);
    starmapEdgeLines.push(seg);
  });
  starmapBuildGlowInstances(glowItems, pos);
}

// 回合收尾：由 SSE 事件（agent_output final / reset 帧）或 watchdog 驱动。
// POST 结束 ≠ 回合结束：agent 可能还在生成（排队+长生成），提前复位
// chatLoading 会让后续 delta 走全量重建、停止按钮消失、用户误发重复消息。
function endChatTurn() {
  if (!state.chatLoading) return;
  state.chatLoading = false;
  state.chatStage = "";
  if (state._turnWatchdog) {
    clearTimeout(state._turnWatchdog);
    state._turnWatchdog = null;
  }
  var btn = document.getElementById("chat-send-btn");
  if (btn) {
    btn.disabled = false;
    btn.textContent = __("发送", "Send");
  }
  var sb = document.getElementById("chat-stop-btn");
  if (sb) sb.style.display = "none";
  rerenderChatIfActive();
}

// 回合看门狗：POST 已超时且 SSE 迟迟无终帧时兕底收尾（连接不稳/事件丢失），
// 提示用户回复可能已生成、可刷新查看历史。避免回合永久卡在 loading。
function armTurnWatchdog() {
  if (state._turnWatchdog) clearTimeout(state._turnWatchdog);
  state._turnWatchdog = setTimeout(() => {
    state._turnWatchdog = null;
    if (state.chatLoading) {
      endChatTurn();
      toast(
        __(
          "长时间未收到回复，连接可能不稳定；回复可能已生成，可刷新连接后查看",
          "No reply for a long time; the reply may have been generated, reconnect to check",
        ),
        true,
      );
    }
  }, 120000);
}

async function sendChat() {
  var inp = document.getElementById("chat-input");
  var btn = document.getElementById("chat-send-btn");
  var stopBtn = document.getElementById("chat-stop-btn");
  var text = inp.value.trim();
  if (!text || state.chatLoading) return;
  if (state.currentConn && state.currentConn.type === "cli") {
    toast(
      __(
        "设备网关连接不支持聊天",
        "Device gateway connection does not support chat",
      ),
      true,
    );
    return;
  }
  state.chatStick = true;
  state.chatFinalIdx = -1;
  state.messages.push({ role: "user", content: text });
  inp.value = "";
  rerenderChat();
  state.chatLoading = true;
  state.chatStage = __("等待AI回复...", "Waiting for AI...");
  btn.disabled = true;
  btn.textContent = "";
  if (stopBtn) stopBtn.style.display = ""; // 生成期间可停止
  rerenderChat();
  try {
    // webui 连接：同步 POST 等完整回复（服务端 X-Trigger-Only 也返回 response；SSE 公网不稳时靠同步兜底）
    var isWebui = !state.currentConn || state.currentConn.type !== "cli";
    var r = null;
    // 唯一消息 ID：服务端据此去重（断线重放/超时重试不再重复处理）
    var cid = clientMsgId();
    lastClientMsgId = cid;
    try {
      r = await api("/chat", {
        method: "POST",
        body: JSON.stringify({ message: text, client_msg_id: cid }),
        timeout: 120000,
      });
    } catch (e) {
      // 同步超时/失败：不阻塞 UI，等 SSE 兜底；若 SSE 也无响应则报错。
      // 明确提示"可能已发送"，避免用户在超时后重复点击导致服务端收到多条相同消息
      console.warn("[sendChat] trigger failed: " + e.message);
      toast(
        __(
          "请求超时（可能已发送，请稍候或在收到回复前勿重复发送）",
          "Request timeout (may have been sent; wait for reply before resending)",
        ),
        true,
      );
      r = null;
    }
    state.chatStage = __("AI 回复中...", "AI replying...");
    var last = state.messages[state.messages.length - 1];
    // 无论 webui/cli：同步 POST 拿到完整回复就直接填充（SSE 公网不稳靠此兜底）
    if (r && r.response) {
      if (last && last.role === "assistant" && last._streaming) {
        last.content = r.response || __("(无响应)", "(no response)");
        last._grow = true;
        if (!last.reasoning_content)
          last.reasoning_content = r.reasoning_content || "";
        last._final = true;
        delete last._streaming;
      } else {
        state.messages.push({
          role: "assistant",
          content: r.response || __("(无响应)", "(no response)"),
          reasoning_content: r.reasoning_content,
          tool_calls:
            last && last.role === "assistant" && last.tool_calls
              ? last.tool_calls
              : [],
          _final: true,
          _grow: true,
        });
      }
      state.chatFinalIdx = state.messages.length - 1;
    }
    rerenderChat();
  } catch (e) {
    state.messages.push({
      role: "assistant",
      content: __("错误: ", "Error: ") + e.message,
      _final: true,
    });
    rerenderChat();
    toast(__("请求失败: ", "Request failed: ") + e.message, true);
  } finally {
    if (r && r.response) {
      // 同步兜底已拿到完整回复：回合结束
      endChatTurn();
    } else {
      // 触发式受理（POST 超时/失败）：回合仍打开，等 SSE 流式渲染；
      // 由 agent_output final / reset 帧 / watchdog 收尾
      armTurnWatchdog();
    }
  }
}

// 停止生成 / 发送中断消息。核心拦截语义：有 LLM 在跑则取消当前请求并以
// [中断消息] 重启轮次；无则在跑则作为普通消息处理。
async function interruptChat() {
  try {
    await api("/chat/interrupt", {
      method: "POST",
      body: JSON.stringify({}),
    });
    toast(__("已发送中断信号", "Interrupt signal sent"));
  } catch (e) {
    toast(__("中断失败: ", "Interrupt failed: ") + e.message, true);
  }
}

async function queryMemoryChat() {
  var q = document.getElementById("mem-query")?.value;
  var r = document.getElementById("mem-result-chat");
  if (!r || !q) return;
  r.innerHTML = '<div class="ha-dots-panel"><span class="ha-dots"><i></i><i></i><i></i></span></div>';
  try {
    var data = await api("/memory?q=" + encodeURIComponent(q) + "&depth=2");
    r.innerHTML =
      '<pre style="font-size:13px">' +
      escHtml(JSON.stringify(data, null, 2)) +
      "</pre>";
  } catch (e) {
    r.innerHTML =
      '<p style="color:#fca5a5">' +
      __("查询失败: ", "Query failed: ") +
      escHtml(e.message) +
      "</p>";
  }
}

async function queryMemoryContext() {
  var q = document.getElementById("ctx-query")?.value;
  var r = document.getElementById("ctx-result");
  if (!r) return;
  r.innerHTML = '<div class="ha-dots-panel"><span class="ha-dots"><i></i><i></i><i></i></span></div>';
  try {
    var data = await api("/memory/context?q=" + encodeURIComponent(q || ""));
    var ctx = data?.context || __("无上下文", "No context");
    var summary = data?.summary || "";
    var entities = data?.entities || [];
    var tk = data?.token_estimate || 0;
    var html = '<div style="font-size:13px">';
    if (summary)
      html +=
        '<div class="kv-row"><span class="key">' +
        __("摘要", "Summary") +
        '</span><span class="val">' +
        escHtml(summary) +
        "</span></div>";
    html +=
      '<div class="kv-row"><span class="key">Token ' +
      __("预估", "Estimate") +
      '</span><span class="val">' +
      tk +
      "</span></div>";
    if (entities.length) {
      html +=
        '<div class="kv-row"><span class="key">' +
        __("实体", "Entities") +
        '</span><span class="val">' +
        entities.map((e) => escHtml(e.name || e.id || "")).join(", ") +
        "</span></div>";
    }
    html +=
      '<pre style="font-size:13px;margin-top:8px">' +
      escHtml(ctx) +
      "</pre></div>";
    r.innerHTML = html;
  } catch (e) {
    r.innerHTML =
      '<p style="color:#fca5a5">' +
      __("获取失败: ", "Get failed: ") +
      escHtml(e.message) +
      "</p>";
  }
}

async function searchKnowledgeChat() {
  var q = document.getElementById("know-query")?.value;
  var r = document.getElementById("know-result-chat");
  if (!r || !q) return;
  r.innerHTML = '<div class="ha-dots-panel"><span class="ha-dots"><i></i><i></i><i></i></span></div>';
  try {
    var data = await api("/knowledge?q=" + encodeURIComponent(q));
    r.innerHTML =
      '<pre style="font-size:13px">' +
      escHtml(JSON.stringify(data, null, 2)) +
      "</pre>";
  } catch (e) {
    r.innerHTML =
      '<p style="color:#fca5a5">' +
      __("搜索失败: ", "Search failed: ") +
      escHtml(e.message) +
      "</p>";
  }
}

async function createKnowledgeChat() {
  var name = document.getElementById("know-name")?.value;
  var content = document.getElementById("know-content")?.value;
  if (!name || !content) {
    toast(__("名称和内容不能为空", "Name and content cannot be empty"), true);
    return;
  }
  try {
    var r = await api("/knowledge", {
      method: "POST",
      body: JSON.stringify({ name: name, content: content }),
    });
    if (r.status || r.id) {
      toast(__("知识「", 'Knowledge "') + name + __("」已创建", '" created'));
      document.getElementById("know-name").value = "";
      document.getElementById("know-content").value = "";
    } else {
      toast(__("创建失败", "Create failed"), true);
    }
  } catch (e) {
    toast(__("创建失败: ", "Create failed: ") + e.message, true);
  }
}

function switchChatPanel(tab, el) {
  var panels = {
    chat: document.getElementById("chat-panel-chat"),
    starmap: document.getElementById("chat-panel-starmap"),
    terminal: document.getElementById("chat-panel-terminal"),
    cmd: document.getElementById("chat-panel-cmd"),
    memory: document.getElementById("chat-panel-memory"),
    context: document.getElementById("chat-panel-context"),
    knowledge: document.getElementById("chat-panel-knowledge"),
  };
  Object.keys(panels).forEach((k) => {
    var p = panels[k];
    if (p) p.classList.toggle("active", k === tab);
  });
  if (el) {
    var parent = el.parentElement;
    if (parent) {
      Array.from(parent.children).forEach((ch) => {
        ch.classList.remove("active");
      });
      el.classList.add("active");
    }
  }
  if (tab === "starmap") {
    renderChatStarmap();
    onStarmapResize();
    // 星图面板激活：若动画未在跑则启动
    if (!starmapRaf) {
      starmapLastFrame = 0;
      starmapAnimate();
    }
  }
  if (tab === "terminal") renderTerminals();
  if (tab === "cmd") renderCmdHistory();
  if (tab === "memory") queryMemoryChat();
  if (tab === "context") queryMemoryContext();
  if (tab === "knowledge") searchKnowledgeChat();
}

// 首屏分段加载条数：只拉最新 N 条，向上滚动触顶时再拉更早的。
var CHAT_PAGE_SIZE = 40;

async function loadChatHistory() {
  try {
    var data = await api("/chat/history?limit=" + CHAT_PAGE_SIZE);
    if (data && data.messages) {
      state.messages = data.messages;
      state.chatOffset = typeof data.offset === "number" ? data.offset : 0;
      state.chatTotal =
        typeof data.total === "number" ? data.total : data.messages.length;
      state.chatHasMore = !!data.has_more;
      if (window.homeagent && window.homeagent.log)
        window.homeagent.log(
          "history: loaded " +
            data.messages.length +
            "/" +
            state.chatTotal +
            (state.chatHasMore ? " (has more)" : ""),
        );
    } else if (window.homeagent && window.homeagent.log) {
      window.homeagent.log("history: no messages field");
    }
  } catch (e) {
    if (window.homeagent && window.homeagent.log)
      window.homeagent.log("history: error " + e.message);
  }
}

// loadOlderChat 向上翻页：拉 offset 之前的一页，前置到 messages 头部。
// 保持滚动位置补偿，避免视口跳动。
var _loadingOlder = false;
async function loadOlderChat() {
  if (_loadingOlder || !state.chatHasMore) return;
  _loadingOlder = true;
  var msgsEl = document.getElementById("chat-msgs");
  var prevH = msgsEl ? msgsEl.scrollHeight : 0;
  var prevTop = msgsEl ? msgsEl.scrollTop : 0;
  try {
    var before = state.chatOffset || 0;
    if (before <= 0) {
      state.chatHasMore = false;
      return;
    }
    var data = await api(
      "/chat/history?limit=" + CHAT_PAGE_SIZE + "&before=" + before,
    );
    if (data && data.messages && data.messages.length) {
      state.messages = data.messages.concat(state.messages);
      state.chatOffset =
        typeof data.offset === "number" ? data.offset : 0;
      state.chatHasMore = !!data.has_more;
      state.chatStick = false;
      rerenderChatIfActive();
      if (msgsEl) {
        msgsEl.scrollTop = prevTop + (msgsEl.scrollHeight - prevH);
      }
      if (window.homeagent && window.homeagent.log)
        window.homeagent.log(
          "history: older " + data.messages.length + " (offset=" + state.chatOffset + ")",
        );
    } else {
      state.chatHasMore = false;
    }
  } catch (e) {
  } finally {
    _loadingOlder = false;
  }
}

// syncChatFromHistory 增量同步：对比服务端历史，仅追加新消息 DOM 节点，
// 不重建已有消息 → 无闪烁。用于 SSE 断连恢复期间的轮询兜底（跨渠道消息补偿）。
function syncChatFromHistory() {
  return api("/chat/history?limit=" + CHAT_PAGE_SIZE)
    .then((data) => {
      if (!data || !data.messages || data.messages.length === 0) return;
      var serverMsgs = data.messages;
      var localMsgs = state.messages;
      // 首次加载（空列表）→ 全量赋值
      if (localMsgs.length === 0) {
        state.messages = serverMsgs;
        state.chatOffset = typeof data.offset === "number" ? data.offset : 0;
        state.chatHasMore = !!data.has_more;
        rerenderChatIfActive();
        return;
      }
      // 分段拉取只回传最新页，本地可能已向上翻页加载更多，
      // 因此不能用长度比较，改用末尾内容比对 + 重叠区对齐。
      var lastLocal = localMsgs[localMsgs.length - 1];
      var lastServer = serverMsgs[serverMsgs.length - 1];
      var localContent = lastLocal.content || lastLocal.Content || "";
      var serverContent = lastServer.content || lastServer.Content || "";
      // 末尾一致 → 无新增
      if (localContent === serverContent) return;
      // 寻找重叠点（本地末尾 k 条在服务端页中的位置）
      var overlap = -1;
      var maxK = Math.min(3, serverMsgs.length - 1, localMsgs.length - 1);
      for (var k = maxK; k >= 1; k--) {
        var sMsg = serverMsgs[serverMsgs.length - 1 - k];
        var lMsg = localMsgs[localMsgs.length - 1 - k];
        if (
          lMsg &&
          sMsg &&
          (lMsg.content || "") === (sMsg.content || "") &&
          lMsg.role === sMsg.role
        ) {
          overlap = k;
          break;
        }
      }
      if (overlap >= 0) {
        var newMsgs = serverMsgs.slice(serverMsgs.length - overlap);
        if (newMsgs.length === 0) return;
        Array.prototype.push.apply(state.messages, newMsgs);
        rerenderChatIfActive();
      } else {
        // 无重叠点（本地领先过多/已失同步）→ 安全退化为全量刷新最新页
        state.messages = serverMsgs;
        state.chatOffset = typeof data.offset === "number" ? data.offset : 0;
        state.chatHasMore = !!data.has_more;
        rerenderChatIfActive();
      }
    })
    .catch(() => {});
}

async function loadTerminals() {
  try {
    var data = await api("/terminals");
    if (data && data.terminals) state.terminals = data.terminals;
  } catch (e) {}
}

async function loadCmdHistory() {
  try {
    var data = await api("/cmd/history");
    if (data && data.history) state.cmdHistory = data.history;
  } catch (e) {}
}

function appendTermBuf(el, text) {
  if (!text) return;
  el.textContent += text;
  if (el.textContent.length > 262144) {
    el.textContent = el.textContent.slice(el.textContent.length - 262144);
  }
  el.scrollTop = el.scrollHeight;
}

function renderTerminals() {
  var r = document.getElementById("term-list");
  var cnt = document.getElementById("term-count-badge");
  if (!r) return;
  var list = state.terminals || [];
  if (cnt) cnt.textContent = list.length;
  if (list.length === 0) {
    r.innerHTML =
      '<p style="color:var(--text-muted);padding:8px;text-align:center;font-size:13px">' +
      __("暂无终端会话", "No terminal sessions") +
      "</p>";
    return;
  }
  var html = "";
  list.forEach((t, i) => {
    var detailId = "term-detail-" + i;
    var scr = (state.termScreens && state.termScreens[t.id]) || null;
    var running = scr ? scr.running : !!t.running;
    var fullOut = scr ? scr.output : t.output || "";
    if (fullOut) {
      fullOut = escHtml(fullOut);
    } else {
      fullOut =
        '<span style="color:#5c6672">' +
        __("[终端暂无输出]", "[No terminal output]") +
        "</span>";
    }
    html +=
      '<div style="border:1px solid var(--border-color);border-radius:6px;margin-bottom:4px;font-size:13px">';
    html +=
      '<div style="display:flex;align-items:center;gap:6px;padding:6px 8px;cursor:pointer;background:var(--bg-hover)" onclick="var d=document.getElementById(\'' +
      detailId +
      "');d.style.display=d.style.display==='none'?'block':'none'\">";
    html +=
      '<span style="font-family:monospace;font-size:12px;flex:1">' +
      escHtml(t.id || "-") +
      "</span>";
    html +=
      '<span style="flex:1;color:var(--text-muted);overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' +
      escHtml(t.command || "") +
      "</span>";
    html +=
      '<span class="badge ' +
      (running ? "badge-green" : "badge-red") +
      '">' +
      (running ? __("运行中", "Running") : __("已关闭", "Closed")) +
      "</span>";
    html +=
      '<span style="color:var(--text-muted);font-size:12px">' +
      escHtml(t.created_at || "") +
      "</span>";
    html += "</div>";
    html +=
      '<div id="' +
      detailId +
      '" style="display:none;padding:8px;border-top:1px solid var(--border-color);background:var(--bg-input)">';
    html += '<div class="term-screen">';
    html +=
      '<div class="term-head"><span class="term-dot' +
      (running ? "" : " stopped") +
      '" id="term-dot-' +
      escHtml(t.id) +
      '"></span><span style="font-weight:600">' +
      escHtml(t.id) +
      "</span><span>" +
      escHtml(t.command || "") +
      '</span><span style="flex:1"></span><span>' +
      escHtml(t.uptime || "") +
      "</span></div>";
    html +=
      '<pre class="term-buf" id="term-buf-' +
      escHtml(t.id) +
      '">' +
      fullOut +
      "</pre>";
    html += "</div></div></div>";
  });
  r.innerHTML = html;
}

function renderCmdHistory() {
  var r = document.getElementById("cmd-list");
  var cnt = document.getElementById("cmd-count-badge");
  if (!r) return;
  var running = (state.terminals || []).filter((t) => t.running);
  if (cnt) cnt.textContent = running.length;
  if (running.length === 0) {
    r.innerHTML =
      '<p style="color:var(--text-muted);padding:8px;text-align:center;font-size:13px">' +
      __("暂无运行中的命令", "No running commands") +
      "</p>";
    return;
  }
  var html =
    '<table style="font-size:12px"><tr><th>' +
    __("命令", "Command") +
    "</th><th>" +
    __("状态", "Status") +
    "</th><th>" +
    __("运行时长", "Uptime") +
    "</th></tr>";
  running.forEach((t) => {
    var scr = (state.termScreens && state.termScreens[t.id]) || null;
    var out = scr ? scr.output : t.output || "";
    html +=
      "<tr>" +
      '<td style="font-family:monospace;max-width:200px;overflow:hidden;text-overflow:ellipsis">' +
      escHtml(t.command || t.id || "") +
      "</td>" +
      '<td><span class="badge badge-green">' +
      __("运行中", "Running") +
      "</span></td>" +
      '<td style="color:var(--text-muted);white-space:nowrap">' +
      escHtml(t.uptime || "-") +
      "</td>" +
      "</tr>";
    if (out) {
      html +=
        '<tr><td colspan="3" style="padding:0"><pre style="margin:0;padding:4px 8px;max-height:120px;overflow:auto;background:var(--bg-input);border-radius:4px;font-size:12px;color:var(--text-secondary)">' +
        escHtml(out.substring(0, 2000)) +
        "</pre></td></tr>";
    }
  });
  html += "</table>";
  r.innerHTML = html;
}

// ===== Plugins =====
function renderPlugins() {
  var k = state.kernel;
  var plugins = k?.plugins || [];
  var tools = k?.tools || [];
  var installed = state.installedPlugins || [];
  var html =
    '<div class="card"><h2>' +
    __("安装插件", "Install Plugin") +
    "</h2>" +
    '<div style="display:flex;gap:8px;margin-bottom:8px">' +
    '<input id="plugin-url" placeholder=".hmap ' +
    __("包下载 URL", "Package URL") +
    '" style="flex:1" onkeydown="if(event.key==\'Enter\')installPlugin()">' +
    '<button class="btn btn-primary" onclick="installPlugin()">' +
    __("安装", "Install") +
    "</button></div>" +
    '<div><input type="file" id="plugin-file" accept=".hmap" style="display:inline;width:auto" onchange="installPluginFile(this.files[0])">' +
    '<label for="plugin-file" class="btn btn-ghost" style="cursor:pointer">' +
    __("选择 .hmap 文件上传", "Upload .hmap file") +
    "</label></div></div>";
  var installedNames = (state.installedPlugins || []).map((p) => p.name);
  var disabledNames = {};
  (state.disabledPlugins || []).forEach((d) => {
    disabledNames[d.name] = d;
  });
  html +=
    '<div class="card"><h2>' +
    __("已加载插件", "Loaded Plugins") +
    " (" +
    plugins.length +
    ")</h2>";
  if (plugins.length === 0 && (state.disabledPlugins || []).length === 0) {
    html +=
      '<div class="empty-state"><p>' +
      __("暂无已加载插件", "No loaded plugins") +
      "</p></div>";
  } else {
    html +=
      "<table><tr><th>" +
      __("名称", "Name") +
      "</th><th>" +
      __("状态", "Status") +
      "</th><th>" +
      __("操作", "Actions") +
      "</th></tr>";
    var allNames = {};
    plugins.forEach((p) => {
      allNames[p.name] = true;
    });
    (state.disabledPlugins || []).forEach((d) => {
      allNames[d.name] = true;
    });
    Object.keys(allNames)
      .sort()
      .forEach((name) => {
        var loaded = plugins.some((p) => p.name === name);
        var isDisabled = !!disabledNames[name];
        var isExternal = installedNames.indexOf(name) >= 0;
        var statusHtml =
          loaded && !isDisabled
            ? '<span class="badge badge-green">' +
              __("已加载", "Loaded") +
              "</span>"
            : loaded && isDisabled
              ? '<span class="badge badge-yellow">' +
                __("运行中(禁用待生效)", "Running (disable pending)") +
                "</span>"
              : isDisabled
                ? '<span class="badge badge-red">' +
                  __("已禁用", "Disabled") +
                  "</span>"
                : '<span class="badge">' +
                  __("未加载", "Not Loaded") +
                  "</span>";
        var actionsHtml = "";
        if (loaded && !isDisabled) {
          actionsHtml +=
            '<button class="btn btn-sm btn-warning" onclick="disablePlugin(\'' +
            escHtml(name) +
            '\')" style="margin-right:4px">' +
            __("禁用", "Disable") +
            "</button>";
        }
        if (isDisabled) {
          actionsHtml +=
            '<button class="btn btn-sm btn-primary" onclick="enablePlugin(\'' +
            escHtml(name) +
            '\')" style="margin-right:4px">' +
            __("启用", "Enable") +
            "</button>";
        }
        if (loaded && isExternal) {
          actionsHtml +=
            '<button class="btn btn-sm btn-danger" onclick="removePlugin(\'' +
            escHtml(name) +
            "')\">" +
            __("卸载", "Unload") +
            "</button>";
        } else if (loaded) {
          actionsHtml +=
            '<span class="badge badge-blue">' +
            __("内置", "Built-in") +
            "</span>";
        }
        html +=
          "<tr><td>" +
          escHtml(name) +
          "</td>" +
          "<td>" +
          statusHtml +
          "</td>" +
          "<td>" +
          actionsHtml +
          "</td></tr>";
      });
    html += "</table>";
  }
  html += "</div>";
  if (installed.length > 0) {
    html +=
      '<div class="card"><h2>' +
      __("已安装外部插件", "Installed Plugins") +
      " (" +
      installed.length +
      ")</h2>" +
      "<table><tr><th>" +
      __("名称", "Name") +
      "</th><th>" +
      __("版本", "Version") +
      "</th><th>" +
      __("描述", "Description") +
      "</th><th>" +
      __("操作", "Actions") +
      "</th></tr>";
    installed.forEach((p) => {
      html +=
        "<tr><td>" +
        escHtml(p.name) +
        "</td>" +
        "<td>" +
        escHtml(p.version || "-") +
        "</td>" +
        "<td>" +
        escHtml((p.description || "").substring(0, 50)) +
        "</td>" +
        '<td><button class="btn btn-sm btn-ghost" onclick="showPluginInfo(\'' +
        escHtml(p.name) +
        "')\">" +
        __("详情", "Details") +
        "</button> " +
        '<button class="btn btn-sm btn-danger" onclick="removePlugin(\'' +
        escHtml(p.name) +
        "')\">" +
        __("卸载", "Unload") +
        "</button></td></tr>";
    });
    html += "</table></div>";
  }
  if (state.pluginInfo) {
    html +=
      '<div class="card"><h2>' +
      __("插件详情", "Plugin Details") +
      ": " +
      escHtml(state.pluginInfo.name) +
      "</h2>" +
      "<pre>" +
      escHtml(JSON.stringify(state.pluginInfo, null, 2)) +
      "</pre>" +
      '<button class="btn btn-ghost" onclick="closePluginInfo()">' +
      __("关闭", "Close") +
      "</button></div>";
  }
  if (tools.length > 0) {
    html +=
      '<div class="card"><h2>' +
      __("已注册工具", "Registered Tools") +
      " (" +
      tools.length +
      ")</h2>" +
      '<div style="display:flex;flex-wrap:wrap;gap:4px">';
    tools.forEach((t) => {
      html +=
        '<span class="tool-badge" title="' +
        escHtml(t.description || "") +
        '">' +
        escHtml(t.name) +
        "</span>";
    });
    html += "</div></div>";
  }
  html +=
    '<div class="card"><h2>' +
    __("系统操作", "System Operations") +
    "</h2>" +
    '<button class="btn btn-primary" onclick="reloadPlugins()" style="margin-right:8px">' +
    __("重载插件", "Reload Plugins") +
    "</button>" +
    '<button class="btn btn-ghost" onclick="runHealthcheck()" style="margin-right:8px">' +
    __("健康检查", "Health Check") +
    "</button></div>";
  html +=
    '<div class="card"><h2>' +
    __("健康检查", "Health Check") +
    '</h2><div id="health-panel">';
  if (state.healthResult) {
    html += renderHealthResult(state.healthResult);
  } else {
    html +=
      '<p style="color:var(--text-muted);font-size:13px">' +
      __("点击上方按钮运行", "Click the button above to run") +
      "</p>";
  }
  html += "</div></div>";
  document.getElementById("view-plugins").innerHTML = html;
}

async function loadInstalledPlugins() {
  try {
    state.installedPlugins = await api("/plugins");
  } catch (e) {
    state.installedPlugins = [];
  }
}

async function installPlugin() {
  var inp = document.getElementById("plugin-url");
  var url = inp?.value.trim();
  if (!url) {
    toast(__("请输入插件包 URL", "Please enter plugin URL"), true);
    return;
  }
  try {
    var r = await api("/plugins", {
      method: "POST",
      body: JSON.stringify({ url: url }),
    });
    toast(
      __("安装结果: ", "Install result: ") + (r.status || JSON.stringify(r)),
    );
    if (r.action === "reload_required")
      toast(
        __(
          "已安装，请点击「重载插件」加载",
          'Installed, click "Reload Plugins" to load',
        ),
        false,
      );
    loadInstalledPlugins();
    renderPlugins();
  } catch (e) {
    toast(__("安装失败: ", "Install failed: ") + e.message, true);
  }
}

async function installPluginFile(file) {
  if (!file) return;
  try {
    var r = await fetch("/api/v1/plugins", {
      method: "POST",
      body: file,
      headers: { "Content-Type": "application/octet-stream" },
    });
    var data = await r.json();
    toast(
      __("上传安装: ", "Upload install: ") +
        (data.status || JSON.stringify(data)),
    );
    if (data.action === "reload_required")
      toast(
        __(
          "已安装，请点击「重载插件」加载",
          'Installed, click "Reload Plugins" to load',
        ),
        false,
      );
    loadInstalledPlugins();
    renderPlugins();
  } catch (e) {
    toast(__("上传失败: ", "Upload failed: ") + e.message, true);
  }
}

async function showPluginInfo(name) {
  try {
    state.pluginInfo = await api("/plugins/" + encodeURIComponent(name));
    renderPlugins();
  } catch (e) {
    toast(__("获取详情失败: ", "Get details failed: ") + e.message, true);
  }
}

function closePluginInfo() {
  state.pluginInfo = null;
  renderPlugins();
}

async function removePlugin(name) {
  if (
    !(await confirmDialog(
      __("确定卸载插件", "Are you sure to unload plugin") +
        "「" +
        name +
        "」？",
      true,
    ))
  )
    return;
  try {
    var r = await api("/plugins/" + encodeURIComponent(name), {
      method: "DELETE",
    });
    toast(__("已卸载: ", "Unloaded: ") + (r.status || r.name));
    if (r.action === "reload_required")
      toast(
        __(
          "已卸载，请点击「重载插件」生效",
          'Unloaded, click "Reload Plugins" to apply',
        ),
        false,
      );
    loadInstalledPlugins();
    renderPlugins();
  } catch (e) {
    toast(__("卸载失败: ", "Unload failed: ") + e.message, true);
  }
}

async function disablePlugin(name) {
  if (name === "webui") {
    var r = await confirmDialog(
      __(
        "禁用 WebUI 后将无法通过 URL:端口访问此管理面板，若要重新启用需要通过 CLI 命令 /plugin enable webui 恢复。\n\n确定要禁用吗？",
        "Disabling WebUI will make this management panel inaccessible via URL:port. To re-enable, use CLI command /plugin enable webui.\n\nAre you sure?",
      ),
      true,
    );
    if (!r) return;
  }
  try {
    await api("/plugins/" + encodeURIComponent(name) + "/disable", {
      method: "POST",
    });
    toast(__("已禁用: ", "Disabled: ") + name);
    state.kernel = await api("/kernel");
    var s = await api("/settings");
    state.disabledPlugins = s.disabled_plugins || [];
    renderPlugins();
  } catch (e) {
    toast(__("禁用失败: ", "Disable failed: ") + e.message, true);
  }
}

async function enablePlugin(name) {
  try {
    await api("/plugins/" + encodeURIComponent(name) + "/enable", {
      method: "POST",
    });
    toast(__("已启用: ", "Enabled: ") + name);
    state.kernel = await api("/kernel");
    var s = await api("/settings");
    state.disabledPlugins = s.disabled_plugins || [];
    renderPlugins();
  } catch (e) {
    toast(__("启用失败: ", "Enable failed: ") + e.message, true);
  }
}

async function reloadPlugins() {
  try {
    var r = await api("/plugins/reload", { method: "POST" });
    toast(__("插件已重载", "Plugins reloaded"));
    state.kernel = await api("/kernel");
    renderPlugins();
  } catch (e) {
    toast(__("重载失败: ", "Reload failed: ") + e.message, true);
  }
}

async function runHealthcheck() {
  var panel = document.getElementById("health-panel");
  if (!panel) return;
  panel.innerHTML =
    '<div class="loading" style="margin:12px auto"></div><p style="text-align:center;color:var(--text-muted)">' +
    __("运行中...", "Running...") +
    "</p>";
  try {
    var r = await api("/kernel");
    var tools = r?.tools || [];
    var healthTool = tools.find((t) => t.name === "healthcheck");
    if (!healthTool) {
      panel.innerHTML =
        '<p style="color:var(--text-secondary)">' +
        __("healthcheck 工具未注册", "healthcheck tool not registered") +
        "</p>";
      return;
    }
    panel.innerHTML =
      '<p style="color:var(--text-secondary)">' +
      __(
        "通过 Agent 对话触发 healthcheck...",
        "Triggering healthcheck via Agent...",
      ) +
      "</p>";
    var chatR = await api("/chat", {
      method: "POST",
      body: JSON.stringify({
        message: __(
          "请运行 healthcheck 工具进行全面健康检查并报告结果",
          "Please run the healthcheck tool for a full system check and report the results",
        ),
      }),
    });
    panel.innerHTML =
      "<pre>" + escHtml(JSON.stringify(chatR, null, 2)) + "</pre>";
  } catch (e) {
    panel.innerHTML =
      '<p style="color:#fca5a5">' +
      __("错误: ", "Error: ") +
      escHtml(e.message) +
      "</p>";
    toast(__("健康检查失败: ", "Health check failed: ") + e.message, true);
  }
}

function renderHealthResult(r) {
  if (!r || !r.checks)
    return (
      '<p style="color:var(--text-secondary)">' +
      __("暂无健康检查数据", "No health check data") +
      "</p>"
    );
  var checks = r.checks || [];
  var passed = checks.filter((c) => c.pass).length;
  var failed = checks.filter((c) => !c.pass).length;
  var html =
    '<div style="margin-bottom:12px;display:flex;gap:16px;align-items:center">' +
    '<span class="badge badge-green">' +
    __("通过: ", "Pass: ") +
    passed +
    "</span>" +
    '<span class="badge ' +
    (failed > 0 ? "badge-red" : "badge-green") +
    '">' +
    __("失败: ", "Fail: ") +
    failed +
    "</span>" +
    '<span class="badge badge-blue">' +
    __("总计: ", "Total: ") +
    checks.length +
    "</span></div>";
  checks.forEach((c) => {
    var passClass = c.pass ? "check-pass" : "check-fail";
    if (c.status === "skip") passClass = "check-skip";
    html +=
      '<div class="health-item">' +
      '<span class="check-name">' +
      escHtml(c.name) +
      "</span>" +
      '<span class="check-status ' +
      passClass +
      '">' +
      (c.status || "unknown") +
      "</span>" +
      '<span style="color:var(--text-muted);font-size:13px">' +
      escHtml(c.detail || "") +
      "</span></div>";
  });
  return html;
}

// ===== Kernel =====
function renderKernel() {
  var k = state.kernel;
  if (!k) {
    document.getElementById("view-kernel").innerHTML =
      '<div class="card"><p style="color:var(--text-muted)">' +
      __("内核未响应", "Kernel not responding") +
      "</p></div>";
    return;
  }
  var html =
    '<div class="card"><h2>' +
    __("运行时", "Runtime") +
    '</h2><div class="grid-3">' +
    statCard("Goroutines", k?.runtime?.goroutines || "-", "") +
    statCard(
      __("内存", "Memory"),
      k?.runtime?.memory_mb ? k.runtime.memory_mb + " MB" : "-",
      "",
    ) +
    statCard("Go " + __("版本", "Version"), k?.runtime?.go_version || "-", "") +
    "</div></div>";
  html +=
    '<div class="card"><h2>LLM</h2>' +
    '<div class="kv-row"><span class="key">Provider</span><span class="val">' +
    (k.llm?.provider || __("未配置", "Not configured")) +
    "</span></div>" +
    '<div class="kv-row"><span class="key">' +
    __("可用源", "Sources") +
    '</span><span class="val">' +
    (k.llm?.sources || 0) +
    "</span></div>" +
    '<div class="kv-row"><span class="key">' +
    __("状态", "Status") +
    '</span><span class="val"><span class="status-dot ' +
    (k.llm?.available ? "dot-green" : "dot-red") +
    '"></span>' +
    (k.llm?.available ? __("运行中", "Running") : __("不可用", "Unavailable")) +
    "</span></div></div>";
  html +=
    '<div class="card"><h2>' +
    __("记忆", "Memory") +
    "</h2>" +
    '<div class="kv-row"><span class="key">' +
    __("图记忆", "Graph Memory") +
    '</span><span class="val">' +
    (k.memory?.available
      ? k.memory.entity_count +
        __(" 实体, ", " entities, ") +
        k.memory.relation_count +
        __(" 关系", " relations")
      : __("未初始化", "Uninitialized")) +
    "</span></div>" +
    '<div class="kv-row"><span class="key">' +
    __("文档记忆", "Document Memory") +
    '</span><span class="val">' +
    (k.documents?.available
      ? k.documents.doc_count + __(" 文档", " docs")
      : __("未初始化", "Uninitialized")) +
    "</span></div>" +
    '<div class="kv-row"><span class="key">' +
    __("文本记忆", "Text Memory") +
    '</span><span class="val">' +
    (k.text_memory?.available
      ? k.text_memory.file_count + __(" 文件", " files")
      : __("未初始化", "Uninitialized")) +
    "</span></div>" +
    '<div class="kv-row"><span class="key">' +
    __("知识库", "Knowledge") +
    '</span><span class="val">' +
    (k.knowledge?.available
      ? k.knowledge.item_count + __(" 项", " items")
      : __("未初始化", "Uninitialized")) +
    "</span></div></div>";
  html +=
    '<div class="card"><h2>' +
    __("插件", "Plugins") +
    " (" +
    (k.plugins?.length || 0) +
    ")</h2>";
  if (k.plugins?.length) {
    html += '<div style="display:flex;flex-wrap:wrap;gap:4px">';
    k.plugins.forEach((p) => {
      html += '<span class="badge badge-blue">' + escHtml(p.name) + "</span>";
    });
    html += "</div>";
  } else {
    html += '<p style="color:var(--text-muted)">' + __("无", "None") + "</p>";
  }
  html += "</div>";
  document.getElementById("view-kernel").innerHTML = html;
}

// ===== Star Map =====
var starmapScene = null,
  starmapCam = null,
  starmapRen = null,
  starmapCtrl = null;
var starmapNodes = [],
  starmapEdges = [];
var starmapNodeMeshes = [],
  starmapEdgeLines = [],
  starmapStarField = null;
var starmapHovered = null,
  starmapSelected = null,
  starmapAutoView = true;
var starmapRaf = null;
var smTypeColors = {
  person: 0x4488ff,
  task: 0xff8844,
  ai: 0xaa44ff,
  concept: 0x44ff88,
  object: 0xff4444,
};
var smEdgeColors = {
  喜欢: 0xff6b6b,
  学习: 0x4ecdc4,
  属于: 0x45b7d1,
  相关: 0x96ceb4,
  使用: 0xfeca57,
  创建: 0xff9ff3,
};

function createStarField() {
  var c = 1200;
  var p = new Float32Array(c * 3),
    cl = new Float32Array(c * 3),
    s = new Float32Array(c);
  for (var i = 0; i < c; i++) {
    var i3 = i * 3;
    var r = 400 + Math.random() * 600,
      th = Math.random() * Math.PI * 2,
      ph = Math.acos(2 * Math.random() - 1);
    p[i3] = r * Math.sin(ph) * Math.cos(th);
    p[i3 + 1] = r * Math.sin(ph) * Math.sin(th);
    p[i3 + 2] = r * Math.cos(ph);
    if (Math.random() < 0.7) {
      cl[i3] = 0.8 + Math.random() * 0.2;
      cl[i3 + 1] = 0.8 + Math.random() * 0.2;
      cl[i3 + 2] = 1;
    } else {
      cl[i3] = 1;
      cl[i3 + 1] = 0.9 + Math.random() * 0.1;
      cl[i3 + 2] = 0.8 + Math.random() * 0.2;
    }
    s[i] = 0.5 + Math.random() * 2;
  }
  var g = new THREE.BufferGeometry();
  g.setAttribute("position", new THREE.BufferAttribute(p, 3));
  g.setAttribute("color", new THREE.BufferAttribute(cl, 3));
  g.setAttribute("size", new THREE.BufferAttribute(s, 1));
  var m = new THREE.PointsMaterial({
    size: 1.5,
    vertexColors: true,
    transparent: true,
    opacity: 0.8,
    sizeAttenuation: true,
  });
  starmapStarField = new THREE.Points(g, m);
  starmapScene.add(starmapStarField);
}

function onStarmapMove(e) {
  if (!starmapRen || !starmapCam) return;
  var rect = starmapRen.domElement.getBoundingClientRect();
  var mouse = new THREE.Vector2(
    ((e.clientX - rect.left) / rect.width) * 2 - 1,
    -((e.clientY - rect.top) / rect.height) * 2 + 1,
  );
  var rc = new THREE.Raycaster();
  rc.setFromCamera(mouse, starmapCam);
  var hits = rc.intersectObjects(starmapNodeMeshes);
  var infoEl = document.getElementById("starmap-info");
  if (hits.length > 0) {
    var n = hits[0].object;
    if (starmapHovered !== n) {
      // 复位用 baseScale，不能用 set(1,1,1)：节点是按 mention_count
      // 缩放过的（userData.baseScale），置 1 会把大节点缩成最小尺寸；
      // 同时脉冲也靠 baseScale 复原，两边必须用同一个基准。
      if (starmapHovered)
        starmapHovered.scale.setScalar(starmapHovered.userData.baseScale || 1);
      starmapHovered = n;
      n.scale.setScalar(n.userData.baseScale * 1.2);
      var nd = n.userData.nodeData;
      if (infoEl) {
        var e1 = document.getElementById("sm-info-name");
        if (e1) e1.textContent = nd.name || "";
        var e2 = document.getElementById("sm-info-type");
        if (e2) e2.textContent = nd.type || "";
        var e3 = document.getElementById("sm-info-mentions");
        if (e3) e3.textContent = (nd.mention_count || 0) + "";
        var lk = starmapEdges.filter(
          (e) => e.source_id === nd.id || e.target_id === nd.id,
        ).length;
        var e4 = document.getElementById("sm-info-links");
        if (e4) e4.textContent = lk + "";
        infoEl.style.display = "block";
      }
    }
  } else {
    if (starmapHovered) {
      starmapHovered.scale.setScalar(starmapHovered.userData.baseScale || 1);
      starmapHovered = null;
    }
    if (!starmapSelected && infoEl) infoEl.style.display = "none";
  }
}

function onStarmapClick(e) {
  if (!starmapRen || !starmapCam) return;
  var rect = starmapRen.domElement.getBoundingClientRect();
  var mouse = new THREE.Vector2(
    ((e.clientX - rect.left) / rect.width) * 2 - 1,
    -((e.clientY - rect.top) / rect.height) * 2 + 1,
  );
  var rc = new THREE.Raycaster();
  rc.setFromCamera(mouse, starmapCam);
  var hits = rc.intersectObjects(starmapNodeMeshes);
  if (hits.length > 0) {
    var n = hits[0].object;
    starmapSelected = starmapSelected === n ? null : n;
    if (starmapAutoView && starmapSelected)
      flyStarmapTo(starmapSelected.userData.nodeId, 500);
    onStarmapMove(e);
  } else {
    starmapSelected = null;
  }
}

function flyStarmapTo(nodeId, dur) {
  if (!starmapAutoView) return;
  var m = starmapNodeMeshes.find((x) => x.userData.nodeId === nodeId);
  if (!m) return;
  var tp = m.position.clone(),
    sp = starmapCam.position.clone(),
    st = starmapCtrl.target.clone();
  var dist = tp.length() + 25,
    ep = new THREE.Vector3(tp.x, tp.y + dist * 0.4, tp.z + dist * 0.8);
  var t0 = Date.now();
  (function lerp() {
    var t = Math.min((Date.now() - t0) / dur, 1),
      e = 1 - (1 - t) ** 3;
    starmapCam.position.lerpVectors(sp, ep, e);
    starmapCtrl.target.lerpVectors(st, tp, e);
    if (t < 1) requestAnimationFrame(lerp);
  })();
}

function onStarmapResize() {
  if (!starmapRen || !starmapCam) return;
  var cont = starmapRen.domElement.parentElement;
  if (!cont) return;
  var rect = cont.getBoundingClientRect();
  var w = rect.width || 800,
    h = Math.max(rect.height || 250, 100);
  if (w > 0 && h > 0) {
    starmapCam.aspect = w / h;
    starmapCam.updateProjectionMatrix();
    starmapRen.setSize(w, h);
  }
}

function toggleStarmapAuto() {
  starmapAutoView = !starmapAutoView;
  var b = document.getElementById("sm-auto-btn");
  if (b) b.className = starmapAutoView ? "on" : "";
}

function resetStarmapCamera() {
  if (!starmapCam || !starmapCtrl || !starmapNodeMeshes) return;
  var maxD = 0;
  starmapNodeMeshes.forEach((m) => {
    var d = m.position.length();
    if (d > maxD) maxD = d;
  });
  if (maxD < 1) maxD = 30;
  var td = Math.min(Math.max(maxD + 20, 30), 150);
  var sp = starmapCam.position.clone(),
    ep = new THREE.Vector3(td * 0.9, td * 0.6, td * 0.9);
  var st = starmapCtrl.target.clone(),
    t0 = Date.now();
  (function lerp() {
    var t = Math.min((Date.now() - t0) / 400, 1),
      e = 1 - (1 - t) ** 3;
    starmapCam.position.lerpVectors(sp, ep, e);
    starmapCtrl.target.lerpVectors(st, new THREE.Vector3(0, 0, 0), e);
    if (t < 1) requestAnimationFrame(lerp);
  })();
}

var starmapLastFrame = 0;
// 空闲帧跳过与星空时间驱动的状态（2026-10-13，与 WebUI 同改）。
//
// ★ 为何需要：GUI 每帧要提交 1809 draw call（1294 标签精灵 + 节点球），
//   图形静止时按 12fps 重绘是纯浪费。配合“星空按 dt 推进”，空闲时可降频
//   而不让星空跳变。
var starmapLastRenderKey = null;
var starmapLastRenderAt = 0;
var starmapLastFrameTs = 0;
var SM_IDLE_RENDER_MS = 5000;
var SM_PULSE_MS = 1400; // 单个脉冲的生命期
var starmapDirty = false; // 全量图过期标记（pulse 检出未知新实体时置位）
function starmapPanelActive() {
  // 仅当 chat 视图且"星图"子面板激活时才运行动画
  if (!state.currentView || state.currentView !== "chat") return false;
  var panel = document.getElementById("chat-panel-starmap");
  return panel && panel.classList.contains("active");
}
function starmapAnimate() {
  if (!starmapPanelActive()) {
    starmapRaf = null;
    return;
  }
  starmapRaf = requestAnimationFrame(starmapAnimate);
  // 帧率限制 ~12fps：装饰背景无需 60fps（GPU 高占用主因）
  var now = Date.now();
  if (now - starmapLastFrame < 83) return;
  starmapLastFrame = now;
  if (starmapCtrl) starmapCtrl.update();
  // 星空按**真实流逝时间**旋转（而非每帧固定 0.0001）。
  //
  // ★ 为何改成时间驱动：以前每帧加固定角度，所以为了“让星空动”就必须
  //   按帧重绘；而每次重绘（1809 draw call + 1294 精灵）成本显著。
  //   改成按 dt 推进后，星空角度与重绘频率解耦，于是空闲时可以降频。
  var dtSec = starmapLastFrameTs ? (now - starmapLastFrameTs) / 1000 : 0;
  starmapLastFrameTs = now;
  // 0.0001 弧度/帧 @12fps ≈ 0.0012 弧度/秒（等值换算，保持原观感速度）
  if (starmapStarField && dtSec > 0 && dtSec < 1) {
    starmapStarField.rotation.y += dtSec * 0.0012;
  }
  // 推进活动脉冲（发光/缩放/呼吸），放在渲染前，
  // 这样本帧看到的就是本帧推进后的材质状态。
  // hover 冲突由 starmapTickActivity 内部逐节点跳过处理，
  // **不**在这里整段跳过 —— 那样会在 hover 期间冻结全部脉冲，
  // hover 一放开就看到一堆积压的脉冲同时弹。
  starmapTickActivity();
  // ★★ 2026-10-13：空闲帧跳过绘制（与 WebUI 同改）。
  //
  // 为何要做：GUI 的 draw call 已从 3477 降到 1809，但**每帧仍是实成本**
  // （1294 个标签精灵 + 节点球）。图形完全静止时按 12fps 重绘是纯浪费。
  //
  // 判据：相机姿态 + 活动集。任何交互（拖动/悬停/点击）或脉冲都会改变
  // camKey ⇒ 立即重绘，不受此间隔限制，手感不受影响。
  //
  // ★ 必须先判 starmapCam/starmapCtrl：switchChatPanel 里 renderChatStarmap()
  //   是**异步**拉数据建图的（loadChatStarmapData），但它紧接着就同步调了
  //   starmapAnimate()。那一瞬间相机还没建出来（starmapCam === null），
  //   直接读 .position 会抛 TypeError —— 实测报在 switchChatPanel 调用栈上。
  if (!starmapRen || !starmapCam || !starmapCtrl) return;
  // ★ 活跃集概念两仓不同，不能相抄：GUI 用 state.starmapGrown（nodeId → 截止时间戳），
  //   WebUI 用 starmapMorphSet。此前直接从 WebUI 抄了 starmapMorphSet，
  //   而 GUI 没有这个变量 ⇒ 每帧 ReferenceError、星图 0 帧。
  var grownCount = 0;
  for (var _gk in state.starmapGrown) grownCount++;
  var camKey =
    starmapCam.position.x.toFixed(3) +
    "," +
    starmapCam.position.y.toFixed(3) +
    "," +
    starmapCam.position.z.toFixed(3) +
    "," +
    starmapCtrl.target.x.toFixed(3) +
    "," +
    starmapCtrl.target.y.toFixed(3) +
    "," +
    starmapCtrl.target.z.toFixed(3) +
    "," +
    state.starmapPulses.length +
    "," +
    grownCount +
    "," +
    (starmapHovered ? starmapHovered.userData.nodeId : "") +
    "," +
    (starmapSelected ? starmapSelected.userData.nodeId : "");
  var changed = camKey !== starmapLastRenderKey;
  if (!changed && now - starmapLastRenderAt < SM_IDLE_RENDER_MS) return;
  starmapLastRenderKey = camKey;
  starmapLastRenderAt = now;
  if (starmapRen && starmapScene && starmapCam)
    starmapRen.render(starmapScene, starmapCam);
}

function createNebula() {
  var nc = 500;
  var p = new Float32Array(nc * 3),
    cl = new Float32Array(nc * 3);
  for (var i = 0; i < nc; i++) {
    var i3 = i * 3;
    p[i3] = (Math.random() - 0.5) * 800;
    p[i3 + 1] = (Math.random() - 0.5) * 800;
    p[i3 + 2] = (Math.random() - 0.5) * 800;
    var ch = Math.random();
    if (ch < 0.33) {
      cl[i3] = 0.5 + Math.random() * 0.3;
      cl[i3 + 1] = 0.2 + Math.random() * 0.2;
      cl[i3 + 2] = 0.7 + Math.random() * 0.3;
    } else if (ch < 0.66) {
      cl[i3] = 0.2 + Math.random() * 0.2;
      cl[i3 + 1] = 0.3 + Math.random() * 0.3;
      cl[i3 + 2] = 0.8 + Math.random() * 0.2;
    } else {
      cl[i3] = 0.7 + Math.random() * 0.3;
      cl[i3 + 1] = 0.2 + Math.random() * 0.2;
      cl[i3 + 2] = 0.5 + Math.random() * 0.3;
    }
  }
  var g = new THREE.BufferGeometry();
  g.setAttribute("position", new THREE.BufferAttribute(p, 3));
  g.setAttribute("color", new THREE.BufferAttribute(cl, 3));
  var m = new THREE.PointsMaterial({
    size: 8,
    vertexColors: true,
    transparent: true,
    opacity: 0.15,
    sizeAttenuation: true,
    blending: THREE.AdditiveBlending,
  });
  var np = new THREE.Points(g, m);
  starmapScene.add(np);
}

// ===== Settings =====
function pluginDisplayName(p) {
  if (p === "core") return __("核心", "Core");
  var name = p.replace("plugin.", "");
  var meta = state.pluginMeta && state.pluginMeta[name];
  if (meta)
    return state.lang === "en" ? meta.name_en || name : meta.name_zh || name;
  return name;
}

function renderSettingsTabs() {
  var el = document.getElementById("settings-tabs");
  if (!el) return;
  el.innerHTML = "";
  state.settingsPlugins.forEach((p) => {
    var s = document.createElement("span");
    s.textContent = pluginDisplayName(p);
    if (p === state.selectedSection) s.className = "active";
    s.onclick = () => {
      state.selectedSection = p;
      renderOneSettings();
    };
    el.appendChild(s);
  });
}

function renderOneSettings() {
  var prefix = state.selectedSection + ".";
  var allKeys = Object.keys(state.settings || {});
  var filtered = allKeys.filter(
    (k) => k === prefix.slice(0, -1) || k.startsWith(prefix),
  );
  filtered.sort();
  var hideTopLlms = [
    "core.llm.base_url",
    "core.llm.model",
    "core.llm.api_key",
    "core.llm.adapter",
    "core.llm.adapter_path",
    "core.llm.thinking_enabled",
  ];
  var sourceKeys = filtered.filter((k) => k.startsWith("core.llm.sources."));
  var sourceMap = {};
  sourceKeys.forEach((k) => {
    var parts = k.split(".");
    var srcName = parts[3];
    if (!sourceMap[srcName]) sourceMap[srcName] = {};
    sourceMap[srcName][k] = true;
  });
  var mcpServerKeys = filtered.filter(
    (k) => k.startsWith("plugin.mcp.servers.") && k.split(".").length >= 5,
  );
  var mcpServerMap = {};
  mcpServerKeys.forEach((k) => {
    var parts = k.split(".");
    var srvName = parts[3];
    if (!mcpServerMap[srvName]) mcpServerMap[srvName] = {};
    mcpServerMap[srvName][k] = true;
  });
  var regularKeys = filtered.filter(
    (k) =>
      !k.startsWith("core.llm.sources.") &&
      hideTopLlms.indexOf(k) === -1 &&
      !k.startsWith("plugin.mcp.servers.") &&
      k !== "plugin.mcp.servers",
  );
  var html =
    '<div class="card"><h2>' +
    __("后端连接", "Backend Connections") +
    "</h2>" +
    '<div id="conn-manager"></div></div>' +
    '<div id="gui-prefs"></div>' +
    '<div class="settings-tabs" id="settings-tabs"></div>' +
    '<div class="settings-content">';
  if (
    regularKeys.length === 0 &&
    Object.keys(sourceMap).length === 0 &&
    Object.keys(mcpServerMap).length === 0 &&
    state.selectedSection !== "plugin.mcp"
  ) {
    html +=
      '<div class="card"><h2>' +
      escHtml(state.selectedSection) +
      '</h2><p style="color:var(--text-muted)">' +
      __("暂无设置项", "No settings") +
      "</p></div>";
  } else {
    regularKeys.forEach((k) => {
      var v = state.settings[k];
      var sv = typeof v === "object" ? JSON.stringify(v) : String(v);
      var m = state.meta?.[k];
      var shortName = k.split(".").pop().replace(/_/g, " ");
      var label = m?.display_name || shortName;
      var desc = m?.description || "";
      var typ = m?.type || "string";
      var ph = m?.placeholder || "";
      var opts = m?.options || [];
      var inpId = "inp-" + k.replace(/\./g, "_");
      var inp = "";
      if (typ === "bool") {
        var chk = sv === "true" ? "checked" : "";
        inp =
          '<label style="display:flex;align-items:center;gap:8px;cursor:pointer"><input type="checkbox" id="' +
          inpId +
          '" ' +
          chk +
          " onchange=\"markDirty('" +
          k +
          '\')" style="width:auto;margin:0"> ' +
          label +
          "</label>";
      } else if (typ === "select") {
        var selOpts = "";
        opts.forEach((o) => {
          selOpts +=
            '<option value="' +
            o +
            '"' +
            (sv === o ? " selected" : "") +
            ">" +
            o +
            "</option>";
        });
        inp =
          "<label>" +
          label +
          '</label><select id="' +
          inpId +
          '" onchange="markDirty(\'' +
          k +
          "')\">" +
          selOpts +
          "</select>";
      } else if (typ === "text") {
        inp =
          "<label>" +
          label +
          '</label><textarea id="' +
          inpId +
          '" placeholder="' +
          escHtml(ph) +
          '" onchange="markDirty(\'' +
          k +
          "')\">" +
          escHtml(sv) +
          "</textarea>";
      } else {
        inp =
          "<label>" +
          label +
          '</label><input type="text" id="' +
          inpId +
          '" value="' +
          escHtml(sv) +
          '" placeholder="' +
          escHtml(ph) +
          '" onchange="markDirty(\'' +
          k +
          "')\">";
      }
      var extra = "";
      if (m?.extra) {
        m.extra.forEach((f) => {
          var fk = (k ? k + "." : "") + f.key;
          var fv = state.settings?.[fk];
          var fph = f.placeholder || __("输入", "Enter ") + f.label;
          extra +=
            '<div class="form-row" style="margin-left:16px;margin-top:4px"><label>' +
            f.label +
            "</label>";
          if (f.type === "select") {
            var fopts = "";
            if (f.options)
              f.options.forEach((o) => {
                fopts +=
                  '<option value="' +
                  o +
                  '"' +
                  (fv === o ? "selected" : "") +
                  ">" +
                  o +
                  "</option>";
              });
            extra +=
              "<select onchange=\"saveSetting('" +
              fk +
              "',this.value)\">" +
              fopts +
              "</select>";
          } else {
            extra +=
              '<input type="text" value="' +
              escHtml(fv || "") +
              '" placeholder="' +
              escHtml(fph) +
              '" onchange="markDirty(\'' +
              fk +
              '\')" style="margin-bottom:0">';
          }
          extra += "</div>";
        });
      }
      var descHtml = desc
        ? '<p style="font-size:13px;color:var(--text-muted);margin:-6px 0 10px">' +
          escHtml(desc) +
          "</p>"
        : "";
      html +=
        '<div class="card"><div class="settings-key">' +
        escHtml(k) +
        "</div>" +
        inp +
        descHtml +
        extra +
        '<button class="btn btn-sm btn-ghost" style="border-color:var(--save-btn-border);margin-top:4px" onclick="saveSetting(\'' +
        k +
        "')\">" +
        __("保存", "Save") +
        "</button></div>";
    });
    // LLM Sources
    Object.keys(sourceMap)
      .sort()
      .forEach((src) => {
        var baseKey = "core.llm.sources." + src;
        var srcData =
          state.settings?.[baseKey + ".adapter"] ||
          state.settings?.[baseKey + ".base_url"] ||
          "";
        var fields = [
          { key: "adapter", label: __("适配器", "Adapter"), type: "text" },
          { key: "base_url", label: "Base URL", type: "text" },
          { key: "model", label: __("模型", "Model"), type: "text" },
          { key: "api_key", label: "API Key", type: "text" },
          {
            key: "thinking_enabled",
            label: __("思考模式", "Thinking Mode"),
            type: "select",
            options: ["true", "false"],
          },
          {
            key: "adapter_path",
            label: __("适配器路径", "Adapter Path"),
            type: "text",
          },
        ];
        var headerLabel = mL10n(src, "LLM Source: " + src);
        html += '<div class="card"><h2>' + escHtml(headerLabel) + "</h2>";
        fields.forEach((f) => {
          var fk = baseKey + "." + f.key;
          var fv = state.settings?.[fk] || "";
          var flabel = f.label;
          var fieldId = "inp-" + fk.replace(/\./g, "_");
          if (f.type === "select") {
            var fopts = "";
            f.options.forEach((o) => {
              fopts +=
                '<option value="' +
                o +
                '"' +
                (fv === o ? "selected" : "") +
                ">" +
                o +
                "</option>";
            });
            html +=
              "<label>" +
              flabel +
              '</label><select id="' +
              fieldId +
              '" onchange="markDirty(\'' +
              fk +
              '\')" style="margin-bottom:4px">' +
              fopts +
              "</select>";
          } else {
            html +=
              "<label>" +
              flabel +
              '</label><input type="text" id="' +
              fieldId +
              '" value="' +
              escHtml(fv) +
              '" placeholder="' +
              (f.key === "api_key"
                ? __("输入 API Key", "Enter API Key")
                : __("输入", "Enter ") + flabel) +
              '" onchange="markDirty(\'' +
              fk +
              '\')" style="margin-bottom:4px">';
          }
        });
        html +=
          '<div style="display:flex;gap:8px;margin-top:8px">' +
          '<button class="btn btn-sm btn-ghost" style="border-color:var(--save-btn-border)" onclick="saveSetting(\'' +
          baseKey +
          ".adapter');saveSetting('" +
          baseKey +
          ".base_url');saveSetting('" +
          baseKey +
          ".model');saveSetting('" +
          baseKey +
          ".api_key');var inp=document.getElementById('" +
          ("inp-" + baseKey + ".thinking_enabled").replace(/\./g, "_") +
          "');if(inp)saveSetting('" +
          baseKey +
          ".thinking_enabled');toast('" +
          __("源", "Source") +
          " \\'" +
          src +
          "\\' " +
          __("已保存", "saved") +
          "')\">" +
          __("保存", "Save") +
          "</button>" +
          '<button class="btn btn-sm btn-danger" onclick="deleteSource(\'' +
          src +
          "')\">" +
          __("删除", "Delete") +
          "</button></div></div>";
      });
    if (
      Object.keys(sourceMap).length > 0 ||
      state.selectedSection === "core.llm"
    ) {
      html +=
        '<button class="btn btn-ghost btn-sm" onclick="showAddSourceDialog()" style="margin-bottom:16px">+ ' +
        __("添加 LLM 源", "Add LLM Source") +
        "</button>";
    }
    // MCP Servers
    if (
      state.selectedSection === "plugin.mcp" ||
      Object.keys(mcpServerMap).length > 0
    ) {
      html +=
        '<div class="card"><h2>' +
        __("MCP 服务器", "MCP Servers") +
        '</h2><p style="font-size:13px;color:var(--text-muted);margin-bottom:8px">' +
        __(
          "配置 Model Context Protocol 服务端连接",
          "Configure Model Context Protocol server connections",
        ) +
        "</p></div>";
      Object.keys(mcpServerMap)
        .sort()
        .forEach((srv) => {
          var baseKey = "plugin.mcp.servers." + srv;
          var fields = [
            { key: "command", label: __("启动命令", "Command"), type: "text" },
            { key: "url", label: "SSE URL", type: "text" },
            {
              key: "args",
              label: __("参数(JSON数组)", "Args (JSON array)"),
              type: "text",
            },
            {
              key: "env",
              label: __("环境变量(JSON数组)", "Env (JSON array)"),
              type: "text",
            },
          ];
          html += '<div class="card"><h2>' + escHtml(srv) + "</h2>";
          fields.forEach((f) => {
            var fk = baseKey + "." + f.key;
            var fv = state.settings?.[fk] || "";
            var fieldId = "inp-" + fk.replace(/\./g, "_");
            html +=
              "<label>" +
              f.label +
              '</label><input type="text" id="' +
              fieldId +
              '" value="' +
              escHtml(fv) +
              '" placeholder="' +
              __("输入", "Enter ") +
              f.label +
              '" onchange="markDirty(\'' +
              fk +
              '\')" style="margin-bottom:4px">';
          });
          html +=
            '<div style="display:flex;gap:8px;margin-top:8px">' +
            '<button class="btn btn-sm btn-ghost" style="border-color:var(--save-btn-border)" onclick="saveSetting(\'' +
            baseKey +
            ".command');saveSetting('" +
            baseKey +
            ".url');saveSetting('" +
            baseKey +
            ".args');saveSetting('" +
            baseKey +
            ".env');toast('MCP \\'" +
            srv +
            "\\' " +
            __("已保存", "saved") +
            "')\">" +
            __("保存", "Save") +
            "</button>" +
            '<button class="btn btn-sm btn-danger" onclick="deleteMCPServer(\'' +
            srv +
            "')\">" +
            __("删除", "Delete") +
            "</button></div></div>";
        });
      html +=
        '<button class="btn btn-ghost btn-sm" onclick="addMCPSource()" style="margin-bottom:16px">+ ' +
        __("添加 MCP 服务器", "Add MCP Server") +
        "</button>";
    }
  }
  html += "</div></div>";
  document.getElementById("view-settings").innerHTML = html;
  renderGuiPrefs();
  renderSettingsTabs();
  renderConnSection();
  loadGuiPrefs();
}

function markDirty(k) {
  var inp = document.getElementById("inp-" + k.replace(/\./g, "_"));
  if (inp) inp.style.borderColor = "var(--save-btn-border)";
}

async function saveSetting(k) {
  var inp = document.getElementById("inp-" + k.replace(/\./g, "_"));
  if (!inp) return;
  var val;
  var m = state.meta?.[k];
  if (m?.type === "bool") {
    val = inp.checked ? "true" : "false";
  } else if (m?.type === "select") {
    val = inp.value;
  } else {
    var raw = inp.value;
    try {
      val = JSON.parse(raw);
    } catch (e) {
      val = raw;
    }
  }
  try {
    var r = await api("/settings", {
      method: "PUT",
      body: JSON.stringify({ key: k, value: val }),
    });
    if (r.status === "ok") {
      inp.style.borderColor = "";
      state.settings[k] = val;
      toast(__("已保存: ", "Saved: ") + k);
    } else {
      toast(__("保存失败: ", "Save failed: ") + (r.error || "unknown"), true);
    }
  } catch (e) {
    toast(__("保存失败: ", "Save failed: ") + e.message, true);
  }
}

function renderConfigDisabled() {
  document.getElementById("view-settings").innerHTML =
    '<div class="card"><h2>' +
    __("设置", "Settings") +
    '</h2><p style="color:var(--text-muted)">' +
    __("设置面板已加载", "Settings panel loaded") +
    "</p></div>";
  renderOneSettings();
}

function mL10n(key, fallback) {
  var meta = state.meta?.[key];
  if (meta?.display_name) return meta.display_name;
  return fallback || key;
}

function showAddSourceDialog() {
  var name = prompt(
    __(
      "输入新 LLM 源名称（如 openai、anthropic）:",
      "Enter new LLM source name (e.g. openai, anthropic):",
    ),
  );
  if (!name || !name.trim()) return;
  name = name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9_]/g, "_");
  if (!name) {
    toast(__("名称无效", "Invalid name"), true);
    return;
  }
  var keys = [
    "base_url",
    "model",
    "api_key",
    "adapter",
    "adapter_path",
    "thinking_enabled",
  ];
  var values = {
    base_url: "https://api." + name + ".com",
    model: "",
    api_key: "",
    adapter: name,
    adapter_path: "",
    thinking_enabled: "false",
  };
  var promises = keys.map((f) =>
    api("/settings", {
      method: "PUT",
      body: JSON.stringify({
        key: "core.llm.sources." + name + "." + f,
        value: values[f],
      }),
    }),
  );
  Promise.all(promises)
    .then(() => {
      toast(
        __("源", "Source") +
          ' "' +
          name +
          '" ' +
          __("已创建，请配置各项参数", "created, please configure parameters"),
      );
      renderAll();
    })
    .catch((e) => {
      toast(__("创建失败: ", "Create failed: ") + e.message, true);
    });
}

async function deleteSource(name) {
  if (
    !(await confirmDialog(
      __("确认删除源", "Are you sure to delete source") + ' "' + name + '"?',
      true,
    ))
  )
    return;
  var base = "core.llm.sources." + name;
  var fields = [
    "adapter",
    "base_url",
    "model",
    "api_key",
    "thinking_enabled",
    "adapter_path",
  ];
  try {
    for (var f of fields) {
      await api("/settings", {
        method: "PUT",
        body: JSON.stringify({ key: base + "." + f, value: null }),
      });
    }
    toast(__("源", "Source") + ' "' + name + '" ' + __("已删除", "deleted"));
    renderAll();
  } catch (e) {
    toast(__("删除失败: ", "Delete failed: ") + e.message, true);
  }
}

function addMCPSource() {
  var name = prompt(__("输入新 MCP 服务器名称:", "Enter new MCP server name:"));
  if (!name || !name.trim()) return;
  name = name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9_]/g, "_");
  if (!name) {
    toast(__("名称无效", "Invalid name"), true);
    return;
  }
  var fields = ["command", "url", "args", "env"];
  var values = { command: "", url: "", args: "[]", env: "[]" };
  var promises = fields.map((f) =>
    api("/settings", {
      method: "PUT",
      body: JSON.stringify({
        key: "plugin.mcp.servers." + name + "." + f,
        value: values[f],
      }),
    }),
  );
  Promise.all(promises)
    .then(() => {
      toast(
        "MCP " +
          __("服务器", "server") +
          ' "' +
          name +
          '" ' +
          __("已创建", "created"),
      );
      renderAll();
    })
    .catch((e) => {
      toast(__("创建失败: ", "Create failed: ") + e.message, true);
    });
}

async function deleteMCPServer(name) {
  if (
    !(await confirmDialog(
      __("确认删除 MCP 服务器", "Are you sure to delete MCP server") +
        ' "' +
        name +
        '"?',
      true,
    ))
  )
    return;
  var fields = ["command", "url", "args", "env"];
  try {
    for (var f of fields) {
      await api("/settings", {
        method: "PUT",
        body: JSON.stringify({
          key: "plugin.mcp.servers." + name + "." + f,
          value: null,
        }),
      });
    }
    toast(
      "MCP " +
        __("服务器", "server") +
        ' "' +
        name +
        '" ' +
        __("已删除", "deleted"),
    );
    renderAll();
  } catch (e) {
    toast(__("删除失败: ", "Delete failed: ") + e.message, true);
  }
}

// ===== Adapters =====
async function renderAdapters() {
  var html = "";
  try {
    var r = await api("/adapters");
    var adapters = r.adapters || [];
    window._adapters = adapters;
    html +=
      '<div class="card"><h2>' +
      __("已加载的适配器", "Loaded Adapters") +
      " (" +
      adapters.length +
      ")</h2>";
    if (adapters.length === 0) {
      html +=
        '<p style="color:var(--text-muted)">' +
        __("暂无适配器", "No adapters") +
        "</p>";
    } else {
      html +=
        "<table><tr><th>" +
        __("名称", "Name") +
        "</th><th>" +
        __("版本", "Version") +
        "</th><th>" +
        __("操作", "Actions") +
        "</th></tr>";
      adapters.forEach((a) => {
        html +=
          "<tr><td>" +
          escHtml(a.name) +
          "</td><td>" +
          escHtml(a.version || "-") +
          "</td>" +
          '<td><button class="btn btn-danger btn-sm" onclick="deleteAdapter(\'' +
          escHtml(a.name) +
          "')\">" +
          __("删除", "Delete") +
          "</button></td></tr>";
      });
      html += "</table>";
    }
    html += "</div>";
    html +=
      '<div class="card"><h2>' +
      __("上传新适配器", "Upload New Adapter") +
      "</h2>" +
      "<label>" +
      __("适配器名称（不带 .lua）", "Adapter name (without .lua)") +
      "</label>" +
      '<input id="adapter-name" placeholder="' +
      __("如 openai", "e.g. openai") +
      '">' +
      "<label>" +
      __("Lua 脚本代码", "Lua Script Code") +
      "</label>" +
      '<textarea id="adapter-code" rows="12" placeholder="-- ' +
      __("返回一个适配器表", "return an adapter table") +
      '\nreturn {\n  name = &quot;openai&quot;,\n  version = &quot;1.0&quot;,\n  transform_request = function(raw) ... end,\n  transform_response = function(raw) ... end,\n}"></textarea>' +
      '<button class="btn btn-primary" onclick="uploadAdapter()">' +
      __("上传", "Upload") +
      "</button></div>";
  } catch (e) {
    html +=
      '<div class="card"><p style="color:var(--text-muted)">' +
      __("加载适配器失败: ", "Failed to load adapters: ") +
      escHtml(e.message) +
      "</p></div>";
  }
  document.getElementById("view-adapters").innerHTML = html;
}

async function uploadAdapter() {
  var name = document.getElementById("adapter-name")?.value;
  var code = document.getElementById("adapter-code")?.value;
  if (!name || !code) {
    toast(__("名称和代码不能为空", "Name and code cannot be empty"), true);
    return;
  }
  try {
    var r = await api("/adapters", {
      method: "POST",
      body: JSON.stringify({ name: name, code: code }),
    });
    if (r.status === "loaded") {
      toast(
        __("适配器", "Adapter") + ' "' + name + '" ' + __("已加载", "loaded"),
      );
      renderAdapters();
    } else {
      toast(__("上传失败: ", "Upload failed: ") + (r.error || "unknown"), true);
    }
  } catch (e) {
    toast(__("上传失败: ", "Upload failed: ") + e.message, true);
  }
}

async function deleteAdapter(name) {
  if (
    !(await confirmDialog(
      __("确定删除适配器", "Are you sure to delete adapter") +
        ' "' +
        name +
        '"？',
      true,
    ))
  )
    return;
  try {
    var r = await api("/adapters/" + encodeURIComponent(name), {
      method: "DELETE",
    });
    if (r.status === "deleted") {
      toast(
        __("适配器", "Adapter") + ' "' + name + '" ' + __("已删除", "deleted"),
      );
      renderAdapters();
    } else {
      toast(__("删除失败", "Delete failed"), true);
    }
  } catch (e) {
    toast(__("删除失败: ", "Delete failed: ") + e.message, true);
  }
}

// ===== Init =====

(async () => {
  var data = await window.homeagent.connections.list();
  state.connections = data.connections || [];
  if (data.currentId)
    state.currentConn =
      state.connections.find((c) => c.id === data.currentId) || null;
  if (state.currentConn) {
    await syncConnAuth();
    connectSSE();
    await loadChatHistory();
    doRenderAll();
    startUptimeTicker();
    setInterval(doRenderAll, 15000);
    // 系统通知：探测可用性 + 接点击跳转（放在连上之后，避免无连接时空转）
    if (window.homeagent && window.homeagent.notify) {
      try {
        const sup = await window.homeagent.notify.supported();
        state._notifySupported = !!(sup && sup.supported);
      } catch (e) {
        state._notifySupported = false;
      }
      window.homeagent.notify.onClicked(onNotifyClicked);
    }
  } else {
    renderAll();
    updateConnIndicator();
  }
})();

// ===== Connection Management =====
function updateConnIndicator() {
  var el = document.getElementById("conn-name-display");
  var dot = document.getElementById("conn-dot");
  var rdot = document.getElementById("rail-conn-dot");
  if (state.currentConn) {
    el.textContent = state.currentConn.name;
    var cls =
      state.status.status === "running" ? "dot-green pulse" : "dot-yellow";
    dot.className = "status-dot " + cls;
    if (rdot)
      rdot.className =
        "conn-dot " +
        (state.status.status === "running" ? "dot-green" : "dot-yellow");
  } else {
    el.textContent = __("未连接", "Not connected");
    dot.className = "status-dot dot-gray";
    if (rdot) rdot.className = "conn-dot";
  }
}

function goSettingsConn() {
  switchView("settings");
  renderConnSection();
}

function openConnManager() {
  renderConnSection();
  switchView("settings");
}

// ===== 客户端偏好（开机自启/静默启动/退出进托盘） =====
var guiPrefs = { autoLaunch: false, silentStart: false, exitToTray: true };
async function loadGuiPrefs() {
  try {
    if (window.homeagent && window.homeagent.prefs) {
      guiPrefs = (await window.homeagent.prefs.get()) || guiPrefs;
    }
  } catch (e) {}
}
async function saveGuiPrefs(key, val) {
  var next = Object.assign({}, guiPrefs, { [key]: val });
  try {
    if (window.homeagent && window.homeagent.prefs) {
      guiPrefs = (await window.homeagent.prefs.set(next)) || next;
    } else {
      guiPrefs = next;
    }
  } catch (e) {
    toast(__("保存偏好失败: ", "Save prefs failed: ") + e.message, true);
    return;
  }
  toast(__("已保存", "Saved"));
  renderGuiPrefs();
}
function renderGuiPrefs() {
  var el = document.getElementById("gui-prefs");
  if (!el) return;
  function sw(key, label, desc, onchange) {
    return (
      '<div class="pref-row" style="margin:8px 0;display:flex;justify-content:space-between;align-items:center">' +
      "<div><div>" +
      label +
      "</div>" +
      (desc
        ? '<div style="font-size:13px;color:var(--text-muted)">' +
          desc +
          "</div>"
        : "") +
      "</div>" +
      '<label class="switch"><input type="checkbox" ' +
      (guiPrefs[key] ? "checked" : "") +
      ' onchange="' +
      onchange +
      '"><span></span></label></div>'
    );
  }
  el.innerHTML =
    '<div class="card" id="gui-prefs-card"><h2>' +
    __("客户端偏好", "Client Preferences") +
    "</h2>" +
    sw(
      "autoLaunch",
      __("开机自启", "Auto launch on start"),
      __("登录后自动启动 HologramAgent（静默）", "Start silently at login"),
      "saveGuiPrefs('autoLaunch', this.checked)",
    ) +
    sw(
      "silentStart",
      __("静默启动", "Silent start"),
      __("启动时不显示主窗口，驻留托盘后台运行", "Start hidden, keep in tray"),
      "saveGuiPrefs('silentStart', this.checked)",
    ) +
    sw(
      "exitToTray",
      __("退出进托盘", "Exit to tray"),
      __("关闭窗口时驻留托盘而不是退出", "Closing window keeps app in tray"),
      "saveGuiPrefs('exitToTray', this.checked)",
    ) +
    "</div>";
}
function renderConnSection() {
  var cont = document.getElementById("conn-manager");
  if (!cont) return;
  cont.innerHTML = "";
  if (state.connections.length === 0) {
    cont.innerHTML +=
      '<div class="card"><p style="color:var(--text-muted)">' +
      __(
        "暂无后端连接，添加一个以开始使用",
        "No backend connections yet. Add one to get started.",
      ) +
      "</p></div>";
  } else {
    state.connections.forEach((c) => {
      var div = document.createElement("div");
      div.className =
        "conn-item " +
        (state.currentConn && state.currentConn.id === c.id ? "active" : "");
      div.innerHTML =
        '<span class="status-dot ' +
        (state.currentConn && state.currentConn.id === c.id
          ? "dot-green"
          : "dot-gray") +
        '"></span>' +
        '<div class="conn-info"><div class="conn-name">' +
        escHtml(c.name) +
        (c.gateway
          ? ' <span class="gw-badge">' + __("总网关", "Gateway") + "</span>"
          : "") +
        '</div><div class="conn-url">' +
        escHtml(c.url) +
        "</div></div>" +
        '<div class="conn-actions">' +
        '<button class="btn btn-ghost btn-sm" onclick="selectConnection(\'' +
        c.id +
        "')\">" +
        __("连接", "Connect") +
        "</button> " +
        '<button class="btn btn-ghost btn-sm" onclick="editConnection(\'' +
        c.id +
        "', event)\">" +
        __("编辑", "Edit") +
        "</button> " +
        '<button class="btn btn-danger btn-sm" onclick="deleteConnection(\'' +
        c.id +
        "', event)\">" +
        __("删除", "Delete") +
        "</button></div>";
      cont.appendChild(div);
    });
  }
  var form = document.createElement("div");
  form.className = "conn-form";
  form.id = "conn-form";
  form.style.display = "none";
  form.innerHTML =
    '<h3 id="conn-form-title">' +
    __("添加连接", "Add Connection") +
    "</h3>" +
    "<label>" +
    __("名称", "Name") +
    '</label><input id="conn-name" placeholder="My HologramAgent">' +
    "<label>" +
    __("连接类型", "Type") +
    '</label><select id="conn-type" onchange="toggleConnType()">' +
    '<option value="webui">WebUI (HTTP)</option>' +
    '<option value="cli">CLI (unix socket)</option></select>' +
    '<div id="conn-addr-webui"><label>' +
    __("地址", "URL") +
    '</label><input id="conn-url" placeholder="http://localhost:18080"></div>' +
    '<div id="conn-addr-cli" style="display:none"><label>' +
    __("Socket 路径", "Socket Path") +
    '</label><input id="conn-sock" placeholder="C:\\path\\to\\cli.sock"></div>' +
    '<div id="conn-auth-webui">' +
    "<label>" +
    __("WebUI 账号", "WebUI Username") +
    ' <span style="color:var(--text-muted);font-weight:400">(' +
    __("自动登录第二层网关", "auto-login 2nd gateway") +
    ")</span></label>" +
    '<input id="conn-user" placeholder="admin">' +
    "<label>" +
    __("密码", "Password") +
    '</label><input id="conn-pass" type="password" placeholder="••••">' +
    '<div style="display:flex;gap:6px;align-items:center;padding:4px 0 8px;color:var(--text-secondary);font-size:12px">' +
    '<input type="checkbox" id="conn-gw" onchange="toggleGwFields()" style="width:auto;margin:0">' +
    '<label for="conn-gw" style="margin:0;font-size:12px">' +
    __("经过总网关（可选）", "Via gateway (optional)") +
    "</label>" +
    "</div>" +
    '<div id="conn-gw-fields" style="display:none">' +
    "<label>" +
    __("总网关 Cookie", "Gateway Cookie") +
    ' <span style="color:var(--text-muted);font-weight:400">(' +
    __("登录窗口自动抓取", "auto-captured by login window") +
    ")</span></label>" +
    '<textarea id="conn-cookie" rows="2" placeholder="sl-session=...; gateway_session=..." style="min-height:40px"></textarea>' +
    '<div style="display:flex;gap:6px;align-items:center;margin-bottom:10px">' +
    '<button class="btn btn-ghost btn-sm" onclick="openLoginWindow()">' +
    __(
      "打开登录窗口（自动抓取 Cookie）",
      "Open login window (auto-grab cookies)",
    ) +
    "</button>" +
    "</div>" +
    "</div>" +
    "<label>" +
    __("额外请求头 JSON", "Extra Headers JSON") +
    ' <span style="color:var(--text-muted);font-weight:400">(' +
    __("可选", "optional") +
    ")</span></label>" +
    '<input id="conn-headers" placeholder=\'{"X-Api-Key":"..."}\'>' +
    "</div>" +
    "<label>" +
    __("API 密钥", "API Key") +
    ' <span style="color:var(--text-muted);font-weight:400">(' +
    __("可选", "optional") +
    ")</span></label>" +
    '<input id="conn-key" type="password" placeholder="sk-...">' +
    '<div class="conn-form-actions">' +
    '<button class="btn btn-ghost" onclick="cancelConnForm()">' +
    __("取消", "Cancel") +
    "</button>" +
    '<button class="btn btn-primary" onclick="saveConnForm()" id="conn-save-btn">' +
    __("保存", "Save") +
    "</button></div>";
  cont.appendChild(form);
  var addBtn = document.createElement("button");
  addBtn.className = "btn btn-primary";
  addBtn.id = "conn-add-btn";
  addBtn.textContent = "+ " + __("添加连接", "Add Connection");
  addBtn.style.marginTop = "8px";
  addBtn.onclick = showConnForm;
  cont.appendChild(addBtn);
}

function toggleConnType() {
  var t = document.getElementById("conn-type").value;
  document.getElementById("conn-addr-webui").style.display =
    t === "cli" ? "none" : "block";
  document.getElementById("conn-addr-cli").style.display =
    t === "cli" ? "block" : "none";
  document.getElementById("conn-auth-webui").style.display =
    t === "cli" ? "none" : "block";
  toggleGwFields();
}

function toggleGwFields() {
  var gw = document.getElementById("conn-gw");
  var fields = document.getElementById("conn-gw-fields");
  if (gw && fields) fields.style.display = gw.checked ? "block" : "none";
}

async function openLoginWindow(useForm) {
  if (useForm === undefined) useForm = true;
  var url, us, ps;
  if (useForm) {
    url = document.getElementById("conn-url").value.trim().replace(/\/+$/, "");
    us = document.getElementById("conn-user").value.trim();
    ps = document.getElementById("conn-pass").value;
  } else {
    url = arguments[1];
    us = arguments[2] || "";
    ps = arguments[3] || "";
  }
  if (!url) {
    toast(__("请先填写地址", "Set URL first"), true);
    return;
  }
  var handled = false;
  window.homeagent.webui.onLoginResult((d) => {
    if (handled) return;
    handled = true;
    if (window.homeagent && window.homeagent.log)
      window.homeagent.log(
        "r: login-result ok=" + (d && d.ok) + " count=" + (d && d.count),
      );
    if (_loginWaitRes) {
      var r = _loginWaitRes;
      _loginWaitRes = null;
      r(d);
      return;
    }
    if (!d || !d.ok) {
      toast(
        __("未取得 Cookie: ", "No cookies: ") + ((d && d.error) || "unknown"),
        true,
      );
      return;
    }
    var form = document.getElementById("conn-form");
    var editing = form && form.style.display === "block";
    if (editing) {
      document.getElementById("conn-cookie").value = d.cookie || "";
      toast(
        __("已取得 ", "Got ") +
          (d.count || 0) +
          __(" 个 Cookie，点保存生效", " cookies, click Save to apply"),
      );
      return;
    }
    if (
      state.currentConn &&
      d.url.replace(/\/+$/, "") === state.currentConn.url
    ) {
      window.homeagent.connections
        .update(state.currentConn.id, { cookie: d.cookie || "" })
        .then((data) => {
          state.connections = data.connections;
          state.currentConn =
            data.connections.find((c) => c.id === data.currentId) ||
            state.currentConn;
          updateConnIndicator();
          return syncConnAuth();
        })
        .then(() => {
          toast(
            __(
              "总网关 Cookie 已自动生效",
              "Gateway cookie applied automatically",
            ),
          );
          if (state.messages.length === 0)
            loadChatHistory()
              .then(() => {
                rerenderChat();
              })
              .catch(() => {});
          return null;
        })
        .catch((e) => {
          toast(
            __("应用 Cookie 失败: ", "Apply cookie failed: ") + e.message,
            true,
          );
        });
    } else {
      toast(
        __(
          "已获得 Cookie（请切换到对应连接后保存）",
          "Cookies acquired (switch to the matching connection to save)",
        ),
        false,
      );
    }
  });
  var r = await window.homeagent.webui.openLogin(url, us, ps);
  if (!r || !r.ok)
    toast(
      __("无法打开登录窗口: ", "Cannot open login window: ") +
        ((r && r.error) || ""),
      true,
    );
}

function showConnForm() {
  editingConnId = null;
  document.getElementById("conn-form-title").textContent = __(
    "添加连接",
    "Add Connection",
  );
  document.getElementById("conn-name").value = "";
  document.getElementById("conn-url").value = "http://localhost:18080";
  document.getElementById("conn-sock").value = "";
  document.getElementById("conn-key").value = "";
  document.getElementById("conn-user").value = "";
  document.getElementById("conn-pass").value = "";
  document.getElementById("conn-cookie").value = "";
  document.getElementById("conn-headers").value = "";
  document.getElementById("conn-gw").checked = false;
  toggleGwFields();
  document.getElementById("conn-type").value = "webui";
  toggleConnType();
  document.getElementById("conn-form").style.display = "block";
  document.getElementById("conn-add-btn").style.display = "none";
}

function editConnection(id, e) {
  if (e) e.stopPropagation();
  var c = state.connections.find((x) => x.id === id);
  if (!c) return;
  editingConnId = id;
  document.getElementById("conn-form-title").textContent = __(
    "编辑连接",
    "Edit Connection",
  );
  document.getElementById("conn-name").value = c.name;
  document.getElementById("conn-url").value = c.url || "http://localhost:18080";
  document.getElementById("conn-sock").value = c.socketPath || "";
  document.getElementById("conn-key").value = c.apiKey;
  document.getElementById("conn-user").value = c.username || "";
  document.getElementById("conn-pass").value = c.password || "";
  document.getElementById("conn-cookie").value = c.cookie || "";
  document.getElementById("conn-headers").value = c.headers || "";
  document.getElementById("conn-gw").checked = !!(c.gateway || c.cookie);
  toggleGwFields();
  document.getElementById("conn-type").value =
    c.type === "cli" ? "cli" : "webui";
  toggleConnType();
  document.getElementById("conn-form").style.display = "block";
  document.getElementById("conn-add-btn").style.display = "none";
}

function cancelConnForm() {
  document.getElementById("conn-form").style.display = "none";
  document.getElementById("conn-add-btn").style.display = "block";
}

async function selectConnection(id) {
  if (state.eventSource) {
    state.eventSource.close();
    state.eventSource = null;
  }
  var data = await window.homeagent.connections.setCurrent(id);
  state.currentConn = data.connections.find((c) => c.id === id) || null;
  state.connections = data.connections;
  state.messages = [];
  updateConnIndicator();
  await syncConnAuth();
  connectSSE();
  await loadChatHistory();
  doRenderAll();
  startUptimeTicker();
  switchView("chat");
  renderConnSection();
}

async function deleteConnection(id, e) {
  if (e) e.stopPropagation();
  if (
    !(await confirmDialog(
      __("确定删除此连接？", "Delete this connection?"),
      true,
    ))
  )
    return;
  var wasCurrent = state.currentConn && state.currentConn.id === id;
  var data = await window.homeagent.connections.delete(id);
  state.connections = data.connections;
  state.currentConn = data.currentId
    ? state.connections.find((c) => c.id === data.currentId)
    : null;
  if (wasCurrent && state.eventSource) {
    state.eventSource.close();
    state.eventSource = null;
  }
  if (state.currentConn) {
    updateConnIndicator();
    doRenderAll();
    syncConnAuth();
    connectSSE();
  } else {
    updateConnIndicator();
    if (window.homeagent.webui)
      await window.homeagent.webui.setAuth("", "", "", "", "");
  }
  renderConnSection();
}

var editingConnId = null;
var _loginWaitRes = null;

function waitLogin() {
  return new Promise((res) => {
    _loginWaitRes = res;
  });
}

async function syncConnAuth() {
  var c = state.currentConn;
  if (!c || c.type !== "webui" || !c.url) {
    if (window.homeagent.webui)
      await window.homeagent.webui.setAuth("", "", "", "", "");
    return true;
  }
  var headers = {};
  if (c.headers) {
    try {
      headers = JSON.parse(c.headers) || {};
    } catch (e) {}
  }
  var r = await window.homeagent.webui.setAuth(
    c.url,
    c.cookie || "",
    headers,
    c.username || "",
    c.password || "",
  );
  if (r && r.ok === false) {
    toast(
      __(
        "自动登录 WebUI 失败（已忽略，继续使用现有 Cookie）: ",
        "WebUI auto-login failed (ignored): ",
      ) + r.error,
      true,
    );
    if (c.gateway && !c.cookie) {
      setTimeout(() => {
        openLoginWindow(false, c.url, c.username || "", c.password || "");
      }, 900);
    }
    return false;
  }
  if (c.gateway && c.cookie && r && r.ok !== false) {
    toast(__("总网关 Cookie 已生效", "Gateway cookie active"));
  }
  return true;
}

async function saveConnForm() {
  if (window.homeagent && window.homeagent.log)
    window.homeagent.log("save: start");
  var name = document.getElementById("conn-name").value.trim();
  var ctype = document.getElementById("conn-type").value;
  var url = document
    .getElementById("conn-url")
    .value.trim()
    .replace(/\/+$/, "");
  var sock = document.getElementById("conn-sock").value.trim();
  var apiKey = document.getElementById("conn-key").value.trim();
  var username = document.getElementById("conn-user").value.trim();
  var password = document.getElementById("conn-pass").value;
  var gwEnabled = !!(
    document.getElementById("conn-gw") &&
    document.getElementById("conn-gw").checked
  );
  var cookie = gwEnabled
    ? document.getElementById("conn-cookie").value.trim()
    : "";
  var headersRaw = document.getElementById("conn-headers").value.trim();
  var headers = "";
  if (headersRaw) {
    try {
      JSON.parse(headersRaw);
      headers = headersRaw;
    } catch (e) {
      toast(
        __("额外请求头不是合法 JSON", "Extra headers not valid JSON"),
        true,
      );
      return;
    }
  }
  if (ctype === "cli") {
    if (!name || !sock) {
      toast(
        __("名称和 Socket 路径不能为空", "Name and Socket Path required"),
        true,
      );
      return;
    }
  } else if (!name || !url) {
    toast(__("名称和地址不能为空", "Name and URL required"), true);
    return;
  }
  var testBtn = document.querySelector("#conn-form .btn-primary");
  testBtn.textContent = __("测试中...", "Testing...");
  testBtn.disabled = true;
  try {
    if (ctype === "cli") {
      if (!window.homeagent.cli) throw new Error("cli bridge unavailable");
      var testR = await window.homeagent.cli.request(sock, apiKey, "/status");
      if (testR.error || testR.type === "error") {
        toast(
          __("CLI 连接测试失败: ", "CLI test failed: ") +
            (testR.error || testR.type),
          true,
        );
        testBtn.textContent = __("保存", "Save");
        testBtn.disabled = false;
        return;
      }
    } else {
      if (window.homeagent && window.homeagent.webui) {
        if (gwEnabled && !cookie) {
          testBtn.textContent = __(
            "请在登录窗口完成网关登录…",
            "Complete gateway login…",
          );
          testBtn.disabled = true;
          openLoginWindow(false, url, username, password);
          var lg = await waitLogin();
          if (!lg || !lg.ok) {
            toast(
              __(
                "网关登录未完成，已取消保存",
                "Gateway login incomplete, save cancelled",
              ) + (lg && lg.error ? ": " + lg.error : ""),
              true,
            );
            testBtn.textContent = __("保存", "Save");
            testBtn.disabled = false;
            return;
          }
          cookie = lg.cookie || "";
        }
        var tHeaders = {};
        if (headersRaw) {
          try {
            tHeaders = JSON.parse(headersRaw);
          } catch (e) {}
        }
        if (window.homeagent.log)
          window.homeagent.log(
            "save: setAuth url=" +
              url +
              " gw=" +
              gwEnabled +
              " cookieLen=" +
              cookie.length,
          );
        try {
          await window.homeagent.webui.setAuth(
            url,
            cookie,
            tHeaders,
            username,
            password,
          );
        } catch (e) {
          toast(__("应用认证失败: ", "Apply auth failed: ") + e.message, true);
        }
      }
      if (window.homeagent.log)
        window.homeagent.log("save: testing " + url + "/api/v1/status");
      var testR;
      try {
        testR = await fetch(url + "/api/v1/status", {
          headers: apiKey ? { "X-API-Key": apiKey } : {},
        });
      } catch (e) {
        if (window.homeagent.log)
          window.homeagent.log("save: fetch error: " + e.message);
        toast(
          __("无法连接到 ", "Cannot connect to ") + url + ": " + e.message,
          true,
        );
        testBtn.textContent = __("保存", "Save");
        testBtn.disabled = false;
        return;
      }
      if (window.homeagent.log)
        window.homeagent.log("save: status=" + testR.status);
      if (!testR.ok) {
        toast(
          __("连接测试失败: HTTP ", "Connection test failed: HTTP ") +
            testR.status +
            "（" +
            (await testR.text()).slice(0, 120) +
            "）",
          true,
        );
        testBtn.textContent = __("保存", "Save");
        testBtn.disabled = false;
        return;
      }
    }
  } catch (e) {
    toast(
      __("无法连接到 ", "Cannot connect to ") +
        (ctype === "cli" ? sock : url) +
        ": " +
        e.message,
      true,
    );
    testBtn.textContent = __("保存", "Save");
    testBtn.disabled = false;
    return;
  }
  testBtn.textContent = __("保存", "Save");
  testBtn.disabled = false;
  var connData =
    ctype === "cli"
      ? { name: name, type: "cli", socketPath: sock, url: "", apiKey: apiKey }
      : {
          name: name,
          type: "webui",
          url: url,
          apiKey: apiKey,
          username: username,
          password: password,
          cookie: cookie,
          headers: headers,
          gateway: gwEnabled,
        };
  var data;
  if (editingConnId) {
    data = await window.homeagent.connections.update(editingConnId, connData);
  } else {
    data = await window.homeagent.connections.add(connData);
  }
  state.connections = data.connections;
  var cur = data.connections.find((c) => c.id === data.currentId);
  var switched =
    !!cur && (!state.currentConn || state.currentConn.id !== cur.id);
  if (cur) {
    state.currentConn = cur;
    await syncConnAuth();
    if (switched) {
      if (state.eventSource) {
        state.eventSource.close();
        state.eventSource = null;
      }
      // 切连接时停掉星图轮询：旧后端的图谱/调度器数据对上新后端无意义，
      // 而定时器闭包里的 api() 会一直打旧地址。
      starmapStopActivity();
      starmapDirty = false;
      // 切连接后网关地址要重新发现（缓存属于旧后端）
      state.discoveredGateway = "";
      state.starmapPulses = [];
      state.starmapGrown = {};
      state._smPrevSched = null;
      state.messages = [];
      updateConnIndicator();
      connectSSE();
      await loadChatHistory();
      doRenderAll();
      startUptimeTicker();
      switchView("chat");
    } else {
      updateConnIndicator();
      doRenderAll();
    }
  }
  cancelConnForm();
  renderConnSection();
}

document.addEventListener("keydown", (e) => {
  if (
    e.key === "Escape" &&
    document.getElementById("conn-form").style.display === "block"
  )
    cancelConnForm();
});

// ===== SSE (fetch-based) =====
// 用 fetch + ReadableStream 而不是 EventSource：需要自定义请求头
// （X-API-Key / Last-Event-ID），EventSource 不支持。
//
// ★ 必须用 function 声明而不是 `connectSSE = () => {}`：
// 后者是隐式全局赋值，依赖非严格模式。一旦给 app.js 加 "use strict"
// （或改成 ES module），就在这一行 ReferenceError，而它位于文件靠后
// 位置 —— 报错点离调用点很远，难查。
function connectSSE() {
  if (state.eventSource) {
    state.eventSource.close();
    state.eventSource = null;
  }
  if (!state.currentConn) return;
  // CLI/device 连接无 SSE 通道，聊天走同步
  if (state.currentConn.type === "cli") return;
  connectFetchSSE(state.currentConn.url + "/api/v1/chat/events");
};

async function connectFetchSSE(url) {
  try {
    var headers = {};
    if (state.currentConn && state.currentConn.apiKey)
      headers["X-API-Key"] = state.currentConn.apiKey;
    // 断线重连时回传 Last-Event-ID，让服务端重放遗漏事件
    if (state.sseLastEventID)
      headers["Last-Event-ID"] = state.sseLastEventID;
    var resp = await fetch(url, { headers: headers, cache: "no-store" });
    if (!resp.ok || !resp.body) {
      setTimeout(() => {
        connectSSE();
      }, 5000);
      return;
    }
    // ★ 建连成功 ⇒ 退避计数清零。
    //
    // 为什么要在这里清：_sseRetryAttempts 只在下面 catch（**建立**连接失败）
    // 里自增，而 pump() 中途断流后的重连**只读它算延迟**。不清零的话，
    // 只要历史上累计过 5 次，之后每次断连都固定等 32s —— 哪怕这次刚成功
    // 连上、说明服务端和网络都好好的。
    //
    // 这正是「消息流不稳 / 看着卡」的一个共因：滞后、卡顿、闪断不是四个
    // 独立问题，而是同一条退避链在空等。
    state._sseRetryAttempts = 0;
    var reader = resp.body.getReader();
    var decoder = new TextDecoder();
    var buffer = "";
    var reconnectTimer = null;
    state.eventSource = {
      close: () => {
        reader.cancel();
        if (reconnectTimer) clearTimeout(reconnectTimer);
      },
    };
    function processLines() {
      var lines = buffer.split("\n");
      buffer = lines.pop() || "";
      var eventType = "",
        data = "",
        id = "";
      for (var i = 0; i < lines.length; i++) {
        var line = lines[i];
        if (line.startsWith("id: ")) {
          id = line.slice(4).trim();
          if (id) state.sseLastEventID = id;
        } else if (line.startsWith("event: ")) eventType = line.slice(7).trim();
        else if (line.startsWith("data: ")) data = line.slice(6).trim();
        else if (line === "" && eventType && data) {
          handleSSEEvent(eventType, data);
          eventType = "";
          data = "";
        }
      }
    }
    function handleSSEEvent(type, raw) {
      try {
        var ev = JSON.parse(raw);
        var p = ev.payload || {};
        if (type === "agent_output") {
          state.chatStage = __("AI 回复中...", "AI replying...");
          if (p.kind === "channel_output") {
            var cm = {
              role: "assistant",
              content: p.content || "",
              source: p.channel || "",
              _final: true,
              _grow: true,
            };
            if (
              state.chatFinalIdx >= 0 &&
              state.chatFinalIdx < state.messages.length
            ) {
              state.messages.splice(state.chatFinalIdx, 0, cm);
              state.chatFinalIdx++;
            } else {
              state.messages.push(cm);
            }
            rerenderChatIfActive();
            return;
          }
          var last =
            state.messages.length > 0
              ? state.messages[state.messages.length - 1]
              : null;
          if (last && last.role === "assistant" && !last._final) {
            // 聚合最终响应：覆盖 delta 累积的中间内容（以聚合为准，含 stage 插件改写后的文本），置 final 结束本轮流式。
            last._grow = true;
            last.content = p.content || "";
            last._final = true;
            rerenderChatIfActive();
            // 星图：回复产出也是一次活动（agent 说完了 → 图亮一下）。
            // 用文本前若干字符当 hint，让点亮落到**本轮相关**的实体上，
            // 而不是每次都亮同一批高权重节点。
            starmapPulse("output", String(p.content || "").slice(0, 60));
            // 系统通知：一轮输出收尾时通知一次（不在流式过程中逐 token 弹）。
            var _sig = (p.content || "").length + ":" +
              String(p.content || "").slice(0, 40);
            notifyTurnOnce(
              _sig,
              "HologramAgent",
              String(p.content || "").slice(0, 160) || __("（空回复）", "(empty reply)"),
            );
            endChatTurn();
            return;
          }
          if (
            last &&
            last.role === "assistant" &&
            last._final &&
            !last.source &&
            last.content === (p.content || "")
          ) {
            // 去重：同一轮的重复帧（如 SSE 重连回放）内容相同则忽略，仅收尾回合
            endChatTurn();
            return;
          }
          state.messages.push({
            role: "assistant",
            content: p.content || "",
            _streaming: true,
            _grow: true,
            _final: true,
          });
          rerenderChatIfActive();
          endChatTurn();
        } else if (type === "reasoning") {
          if (p.content) {
            state.chatStage = __("AI 思考中...", "AI thinking...");
            var last =
              state.messages.length > 0
                ? state.messages[state.messages.length - 1]
                : null;
            if (!last || last.role !== "assistant" || last._final) {
              state.messages.push({
                role: "assistant",
                content: "",
                reasoning_content: "",
                tool_calls: [],
                _streaming: true,
              });
              last = state.messages[state.messages.length - 1];
            }
            last.reasoning_content = p.content;
            rerenderChatIfActive();
          }
        } else if (type === "reasoning_delta") {
          // token 级思考流式增量：逐块追加到当前思考内容；reset 帧表示轮次作废
          if (p.channel === "_consolidation_") return;
          if (p.reset) {
            var lm = state.messages.length
              ? state.messages[state.messages.length - 1]
              : null;
            if (lm && lm.role === "assistant" && !lm._final) {
              lm._final = true;
              rerenderChatIfActive();
            }
            armTurnWatchdog();
            return;
          }
          if (!p.content) return;
          state.chatStage = __("AI 思考中...", "AI thinking...");
          var last =
            state.messages.length > 0
              ? state.messages[state.messages.length - 1]
              : null;
          if (!last || last.role !== "assistant" || last._final) {
            state.messages.push({
              role: "assistant",
              content: "",
              reasoning_content: "",
              tool_calls: [],
              _streaming: true,
            });
            last = state.messages[state.messages.length - 1];
          }
          last.reasoning_content =
            (last.reasoning_content || "") + p.content;
          rerenderChatIfActive();
        } else if (type === "content_delta") {
          // token 级回复流式增量：逐块追加到当前回复内容；reset 帧表示轮次作废（中断）
          if (p.channel === "_consolidation_") return;
          if (p.reset) {
            var lm = state.messages.length
              ? state.messages[state.messages.length - 1]
              : null;
            if (lm && lm.role === "assistant" && !lm._final) {
              lm._final = true;
              rerenderChatIfActive();
            }
            armTurnWatchdog();
            return;
          }
          if (!p.content) return;
          state.chatStage = __("AI 回复中...", "AI replying...");
          var last =
            state.messages.length > 0
              ? state.messages[state.messages.length - 1]
              : null;
          if (!last || last.role !== "assistant" || last._final) {
            state.messages.push({
              role: "assistant",
              content: "",
              tool_calls: [],
              _streaming: true,
              _grow: true,
            });
            last = state.messages[state.messages.length - 1];
          }
          last.content += p.content;
          rerenderChatIfActive();
        } else if (type === "tool_call") {
          if (!p.tool) return;
          var last =
            state.messages.length > 0
              ? state.messages[state.messages.length - 1]
              : null;
          if (!last || last.role !== "assistant" || last._final) {
            state.messages.push({
              role: "assistant",
              content: "",
              tool_calls: [],
              _streaming: true,
            });
            last = state.messages[state.messages.length - 1];
          }
          if (!last.tool_calls) last.tool_calls = [];
          last.tool_calls.push({
            tool: p.tool,
            name: p.tool,
            args: p.args || {},
            result: p.result || "",
            status: p.status || "ok",
            plugin: p.plugin || "",
          });
          // 星图：工具名就是最好的 hint（调 knowledge_* 就亮知识节点），
          // 这是「图在跟 agent 动」最直接的体现。
          starmapPulse("tool", p.tool || "");
          var pidx = (state.pendingTools || []).indexOf(p.tool);
          if (pidx !== -1) state.pendingTools.splice(pidx, 1);
          state.chatStage = __("工具调用: ", "Tool: ") + (p.tool || "");
          rerenderChatIfActive();
        } else if (type === "terminal_output") {
          if (!p.terminal_id) return;
          var tid = p.terminal_id;
          if (!state.termScreens) state.termScreens = {};
          var scr =
            state.termScreens[tid] ||
            (state.termScreens[tid] = { output: "", running: true });
          if (p.output) scr.output += p.output;
          if (typeof p.running === "boolean") scr.running = p.running;
          var bufel = document.getElementById("term-buf-" + tid);
          if (bufel) {
            appendTermBuf(bufel, p.output || "");
            var dot = document.getElementById("term-dot-" + tid);
            if (dot)
              dot.className = "term-dot" + (scr.running ? "" : " stopped");
          }
        } else if (type === "stage") {
          var phase = p.phase || "";
          var tool = p.tool || "";
          if (p.channel !== "_consolidation_") {
            // 阶段轨迹：本轮真实发生过什么，按阶段落到运行态面板的对应框里。
            // 与 WebUI 同一套 g（阶段组）编号，见 rtPhaseGroup。
            if (phase === "on_input") {
              state.stageTrail = [];
              rtTrailPush(0, "stage", __("输入", "input"), __("输入", "input"));
            } else if (phase === "pre_action") {
              rtTrailPush(
                1,
                "stage",
                __("组装上下文并思考", "assemble context and think"),
                __("思考", "think"),
              );
              state.chatStage = __("AI 思考中...", "AI thinking...");
            } else if (phase === "before_toolcall") {
              if (tool)
                rtTrailPush(
                  2,
                  tool.indexOf("output_") === 0 ? "output" : "tool",
                  tool,
                  rtShortTool(tool),
                );
              if (tool) state.toolFlash = true;
              state.chatStage = __("工具调用: ", "Tool: ") + (tool || "");
              // 星图：阶段事件与 tool_call 事件可能都到（两者由不同路径发布），
              // 所以 starmapPulse 自身按 (mesh,until) 粗粒度去重：同节点
              // 刷新到期时间而非叠加队列项。
              if (tool) starmapPulse("tool", tool);
              if (tool && (state.pendingTools || []).indexOf(tool) === -1) {
                if (!state.pendingTools) state.pendingTools = [];
                state.pendingTools.push(tool);
                rerenderChatIfActive();
              }
            } else if (phase === "before_output") {
              rtTrailPush(3, "stage", __("生成回复", "generate reply"), __("生成", "gen"));
              state.chatStage = __("生成回复中...", "Generating response...");
            } else if (phase === "after_output") {
              rtTrailPush(4, "stage", __("本轮完成", "turn complete"), __("完成", "done"));
            }
            state.pipelinePhase = phase;
            // 阶段停留一会儿就回空闲，避免留下一个永远停在 after_output 的假状态。
            if (state.pipelineTimer) clearTimeout(state.pipelineTimer);
            state.pipelineTimer = setTimeout(() => {
              state.pipelinePhase = "";
              if (state.currentView === "overview") renderOverview();
            }, 2500);
            if (state.currentView === "overview") renderOverview();
          }
          var badge = document.getElementById("chat-stage");
          if (badge) {
            badge.textContent = state.chatStage || "";
            badge.style.display = "none";
          }
        } else if (type === "sync_required") {
          // Server 因 Last-Event-ID 不在 ring（delta ID / 已到 tip）无法重放，
          // 通知前端增量补拉历史——避免空等后续聚合事件导致「消息同步不及时」。
          syncChatFromHistory();
        } else if (type === "system") {
          // 内核级状态事件（目前唯一发布者是 plugin_crashed）。
          //
          // ★ 为何必须补上（2026-10-13 协议审计）：该事件一直在发
          //   （internal/plugin/dynamic_proc.go:175），而 GUI 的事件分发里
          //   没有任何分支接它 —— 插件崩溃对桌面端用户完全不可见。
          //
          // toast(m, isError, warn)：GUI 是三参数形态，warn 走黄色样式。
          if (p.event === "plugin_crashed") {
            var nm = p.plugin || "?";
            toast(
              __("插件 " + nm + " 异常退出，内核正在重启它", "Plugin " + nm + " crashed; kernel is restarting it"),
              false,
              true,
            );
          }
        }
      } catch (err) {}
    }
    async function pump() {
      while (true) {
        try {
          var result = await reader.read();
          if (result.done) break;
          buffer += decoder.decode(result.value, { stream: true });
          processLines();
        } catch (e) {
          break;
        }
      }
      // 断连后先增量同步历史（补偿断连窗口期丢失的事件），再重连
      syncChatFromHistory().catch(() => {});
      // ★ 此处也清零：本次连接曾成功建立（上面已清），断流是运行期事件，
      //   不该把「建连失败」的累计次数带进下一次退避。
      state._sseRetryAttempts = 0;
      reconnectTimer = setTimeout(() => {
        connectSSE();
      }, Math.min(1000 * 2 ** Math.min((state._sseRetryAttempts || 0), 5), 32000));
    }
    pump();
  } catch (e) {
    var attempts = (state._sseRetryAttempts || 0) + 1;
    state._sseRetryAttempts = attempts;
    setTimeout(() => {
      connectSSE();
    }, Math.min(1000 * 2 ** Math.min(attempts - 1, 5), 60000));
  }
}

function rerenderChatIfActive() {
  var tab = document.getElementById("view-chat");
  if (!tab || !tab.classList.contains("active")) return;
  // 流式增量路径：防抖合并 + 只更新最后一条消息的正文/思考节点，避免全量重建
  var msgs = state.messages;
  var last = msgs.length ? msgs[msgs.length - 1] : null;
  // ★ 判据必须看**消息自身的 _streaming**，不能依赖 state.chatLoading。
  //
  // 真实事故（2026-10-01 实测，本机 192.168.2.60）：agent 响应非 GUI 渠道
  // （memo / QQ / email）时，GUI 只是通过 SSE 旁观流式帧。而 chatLoading
  // 只在**用户从 GUI 自己发消息**时置 true（sendChat），旁观路径下它恒为
  // false ⇒ streamingLast 恒假 ⇒ 每个 content_delta 帧都掉进「非流式」
  // 分支走**全量** renderChat()。
  //
  // 实测代价：2 秒内 26 次全量重渲（content C=1→C=66，每个增量帧一次），
  // 间隔 10~30ms；消息越多越慢（200 条时单次全量重建实测 235ms）。
  //
  // 而所有 SSE 流式帧创建的 assistant 消息都带 _streaming:true
  // （content_delta / reasoning_delta / tool_call / agent_output 五个 push 点），
  // 收尾时才置 _final。所以正确判据是「最后一条是尚未收尾的流式 assistant」。
  var streamingLast =
    !!last && last.role === "assistant" && !last._final && last._streaming;
  if (streamingLast) {
    if (state._streamTimer) clearTimeout(state._streamTimer);
    state._streamTimer = setTimeout(() => {
      state._streamTimer = null;
      renderChatStreamChunk();
    }, 90);
    return;
  }
  // 非流式（完成/工具/历史变化）：全量渲染
  if (state._streamTimer) {
    clearTimeout(state._streamTimer);
    state._streamTimer = null;
  }
  renderChat();
  renderChatStarmap();
  renderTerminals();
  renderCmdHistory();
}

// 流式增量渲染：仅更新最后一条 assistant 消息的正文（渐进，节流 parse）与思考预览
function renderChatStreamChunk() {
  var msgsEl = document.getElementById("chat-msgs");
  var msgs = state.messages;
  var last = msgs.length ? msgs[msgs.length - 1] : null;
  if (!msgsEl || !last) return;
  var el = msgsEl.lastElementChild;
  if (!el) {
    renderChat();
    return;
  }
  // 更新正文文本（节流 parse：内容变化 >200 字符或时间 >300ms 才 parse）
  var textEl = el.querySelector(".msg-bubble .text");
  var c = last.content || "";
  if (textEl) {
    var now = Date.now();
    var lastParse = el.__lastParse || 0;
    var lastLen = el.__lastLen || 0;
    if (c.length - lastLen > 200 || now - lastParse > 300) {
      textEl.innerHTML = renderMd(c);
      el.__lastParse = now;
      el.__lastLen = c.length;
    } else {
      // 小增量：纯文本渐进，避免反复 parse
      var tail = c.slice(lastLen);
      if (tail) {
        var tn = document.createTextNode(tail);
        textEl.appendChild(tn);
      }
      el.__lastLen = c.length;
    }
    if (state.chatStick !== false) {
      try {
        msgsEl.scrollTop = msgsEl.scrollHeight;
      } catch (e) {}
    }
    return;
  }
  // 思考预览更新（流式中折叠，只刷 preview + sweep）
  var rc = el.querySelector(".reasoning-card.rc-streaming .reasoning-preview");
  if (rc && last.reasoning_content) {
    var prev = last.reasoning_content.replace(/[\s\n]+/g, " ").slice(0, 60);
    rc.textContent = prev;
    return;
  }
  // 兜底：结构变化则全量
  renderChat();
}

// ===== Devices (remotedevice gateway) =====
function renderDevices() {
  var el = document.getElementById("view-devices");
  if (!el) return;
  var conn = state.currentConn;
  var devs = state.devices || [];
  // 设备列表鉴权失败的显式提示。
  //
  // 为什么必须显式：/device/online 走服务端 requireToken（只认
  // X-API-Key），而普通面板用管理员 cookie。没配令牌时服务端返回 401，
  // 旧实现把它 catch 掉置空数组 ⇒ 界面与「确实没有设备」完全一样，
  // 用户只能看到空列表，无法判断是配置问题还是真没设备。
  var authErrHtml = state.deviceAuthError
    ? '<div class="card" style="border-left:3px solid #d1383d">' +
      '<h2>' +
      __("设备列表不可用", "Device list unavailable") +
      "</h2><p style=\"color:var(--text-muted)\">" +
      escHtml(state.deviceAuthError) +
      "</p></div>"
    : "";
  var selfDev = null;
  if (state.selfDeviceId) {
    for (var si = 0; si < devs.length; si++) {
      if (devs[si].device_id === state.selfDeviceId) {
        selfDev = devs[si];
        break;
      }
    }
  }
  // 本机 GUI 设备卡片：始终显示（不依赖当前连接类型），
  // 设备桥由 gui-prefs 的 deviceBridge 驱动，独立于连接。
  var selfHtml =
    '<div class="card" style="border-left:3px solid #4f8cff"><h2>' +
    __("本机 GUI 设备", "Local GUI Device") +
    "</h2>";
  if (state.selfDeviceId) {
    var sOnline = selfDev ? selfDev.online : false;
    var sAuth = selfDev ? selfDev.authorized : false;
    selfHtml +=
      '<div class="kv-row"><span class="key">' +
      __("设备 ID", "Device ID") +
      '</span><span class="val">' +
      escHtml(state.selfDeviceId) +
      "</span></div>" +
      '<div class="kv-row"><span class="key">' +
      __("网关", "Gateway") +
      '</span><span class="val">' +
      escHtml(state.selfGateway || "-") +
      "</span></div>" +
      '<div class="kv-row"><span class="key">' +
      __("状态", "Status") +
      "</span><span>" +
      (sOnline
        ? '<span class="dot-green"></span>' + __("在线", "Online")
        : '<span class="dot-gray"></span>' + __("离线", "Offline")) +
      "</span></div>" +
      '<div class="kv-row"><span class="key">' +
      __("授权", "Authorized") +
      '</span><span class="val" style="display:flex;align-items:center;gap:8px">' +
      '<label class="switch"><input type="checkbox" ' +
      (sAuth ? "checked" : "") +
      " onchange=\"deviceToggleAuth('" +
      state.selfDeviceId +
      "',this.checked)\"><span></span></label>" +
      (sAuth
        ? '<span class="dot-green"></span>' + __("已授权", "Yes")
        : '<span class="dot-red"></span>' + __("未授权", "No")) +
      "</span></div>";
  } else {
    selfHtml +=
      '<p style="color:var(--text-muted)">' +
      __(
        "未接入设备网关。请在设置中配置「设备网关 (remotedevice)」连接后重启。",
        "Not connected. Configure a Remote Device Gateway connection in Settings then restart.",
      ) +
      "</p>";
  }
  // 设备通道配置（独立于连接类型：devicced 是 GUI 组件，默认走 webui 反代端口）
  var dbc = state.dbConfig || {};
  // 网关地址优先用**服务端发现的权威值**（state.discoveredGateway），
  // 其次才是用户手填 / 本地推导。
  //
  // 为什么不能继续用「门户 URL 同 host 拼 /api/v1/device/ws」：
  // 网关改造为子域反代后位于 devices.<基域名>，而**基域名与子域标签都是
  // 服务端配置**，客户端无从得知。硬拼的结果是连到门户自己的路由上。
  // 服务端 /api/v1/device/gateway 是唯一不会漂移的来源。
  var webuiUrl = state.discoveredGateway || "";
  if (
    !webuiUrl &&
    state.currentConn &&
    state.currentConn.type === "webui" &&
    state.currentConn.url
  ) {
    // 回退：老部署（无发现端点）仍按旧口径推导，保持向后兼容。
    webuiUrl = state.currentConn.url.replace(/\/+$/, "") + "/api/v1/device/ws";
  }
  var curGateway = dbc.gateway || webuiUrl || "";
  var dbExec = dbc.exec || {};
  var dbAuthSchedule = dbc.authSchedule || {};
  var dispIdx = dbc.screensueDisplay || "0";
  var dispOpts = (state.displays || [])
    .map(
      (d) =>
        '<option value="' +
        d.index +
        '"' +
        (String(d.index) === String(dispIdx) ? " selected" : "") +
        ">" +
        escHtml(d.name) +
        (d.size ? " (" + escHtml(d.size) + ")" : "") +
        (d.primary ? " 主" : "") +
        "</option>",
    )
    .join("");
  var audioDev = dbc.audio && dbc.audio.device ? dbc.audio.device : "default";
  var audioOpts = (state.audioDevices || [])
    .map(
      (a) =>
        '<option value="' +
        escHtml(a.name) +
        '"' +
        (a.name === audioDev ? " selected" : "") +
        ">" +
        escHtml(a.name) +
        (a.desc ? " — " + escHtml(String(a.desc).slice(0, 32)) : "") +
        "</option>",
    )
    .join("");
  selfHtml +=
    '<div class="kv-row"><span class="key">' +
    __("设备通道", "Device Channel") +
    '</span><span class="val" style="flex-direction:column;align-items:stretch;gap:4px">' +
    '<div style="display:flex;gap:6px;flex-wrap:wrap">' +
    '<span style="font-size:13px;color:var(--text-muted)">' +
    (dbc.connected
      ? __("设备桥已连接", "Bridge connected")
      : __("设备桥未连接", "Bridge not connected")) +
    (dbc.deviceId ? " · " + escHtml(dbc.deviceId) : "") +
    "</span></div>" +
    '<input id="dev-bridge-gw" value="' +
    escHtml(curGateway) +
    '" style="width:100%;font-size:13px;padding:4px 6px;border-radius:4px;border:1px solid var(--border-color);background:var(--bg-input);color:var(--text-primary)">' +
    '<input id="dev-bridge-token" value="' +
    escHtml(dbc.tokenSet ? "" : "") +
    '" placeholder="' +
    __("ws_token（留空保留已存）", "ws_token (empty keeps stored)") +
    '" type="password" style="width:100%;font-size:13px;padding:4px 6px;border-radius:4px;border:1px solid var(--border-color);background:var(--bg-input);color:var(--text-primary)">' +
    '<div style="display:flex;gap:6px;align-items:center;flex-wrap:wrap">' +
    "<label style='font-size:13px'>" +
    __("screensue 屏幕", "screensue display") +
    '</label><select id="dev-bridge-display" style="font-size:13px;padding:3px 6px;border-radius:4px;border:1px solid var(--border-color);background:var(--bg-input);color:var(--text-primary)">' +
    (dispOpts || '<option value="0">默认</option>') +
    "</select>" +
    "<label style='font-size:13px;margin-left:8px'>" +
    __("默认时长(秒)", "default duration(s)") +
    '</label><input id="dev-bridge-duration" value="' +
    escHtml(dbc.screensueDuration === undefined || dbc.screensueDuration === null ? "5" : String(dbc.screensueDuration)) +
    '" placeholder="5，0=常驻" title="' +
    __("screensue 默认显示秒数；0=永不超时常驻。命令带数字可临时覆盖", "screensue default seconds; 0=persistent. Command number overrides") +
    '" style="width:70px;font-size:13px;padding:3px 6px;border-radius:4px;border:1px solid var(--border-color);background:var(--bg-input);color:var(--text-primary)">' +
    "</div>" +
    '<div style="display:flex;gap:6px;align-items:center;flex-wrap:wrap">' +
    "<label style='font-size:13px'>" +
    __("speakeruse 声卡", "speakeruse audio out") +
    '</label><select id="dev-bridge-audio" style="font-size:13px;padding:3px 6px;border-radius:4px;border:1px solid var(--border-color);background:var(--bg-input);color:var(--text-primary);min-width:160px">' +
    (audioOpts || '<option value="default">default</option>') +
    "</select>" +
    "</div>" +
    '<div style="display:flex;gap:6px;align-items:center;flex-wrap:wrap">' +
    "<label style='font-size:13px'>" +
    __("cmdrun 目录", "cmdrun cwd") +
    '</label><input id="dev-bridge-cwd" value="' +
    escHtml(dbExec.cwd || "") +
    '" placeholder="' +
    __("留空=用户主目录", "empty=home dir") +
    '" style="flex:1;min-width:120px;font-size:13px;padding:3px 6px;border-radius:4px;border:1px solid var(--border-color);background:var(--bg-input);color:var(--text-primary)">' +
    "</div>" +
    '<div style="display:flex;gap:6px;align-items:center;flex-wrap:wrap">' +
    "<label style='font-size:13px'>" +
    __("沙箱", "Sandbox") +
    '</label><select id="dev-bridge-sandbox" style="font-size:13px;padding:3px 6px;border-radius:4px;border:1px solid var(--border-color);background:var(--bg-input);color:var(--text-primary)">' +
    '<option value="off"' +
    ((dbExec.sandbox || "off") === "off" ? " selected" : "") +
    ">" +
    __("不限", "off") +
    "</option>" +
    '<option value="home"' +
    (dbExec.sandbox === "home" ? " selected" : "") +
    ">" +
    __("主目录", "home") +
    "</option>" +
    '<option value="box"' +
    (dbExec.sandbox === "box" ? " selected" : "") +
    ">" +
    __("指定目录", "box") +
    "</option></select>" +
    (dbExec.sandbox === "box"
      ? '<input id="dev-bridge-boxdir" value="' +
        escHtml(dbExec.boxDir || "") +
        '" placeholder="沙箱目录" style="flex:1;min-width:120px;font-size:13px;padding:3px 6px;border-radius:4px;border:1px solid var(--border-color);background:var(--bg-input);color:var(--text-primary)">'
      : '<input id="dev-bridge-boxdir" style="display:none">') +
    "</div>" +
    // 定时撤销/恢复授权（睡眠期间自动撤销，醒来自动恢复）
    '<div style="display:flex;gap:6px;align-items:center;flex-wrap:wrap">' +
    "<label style='font-size:13px'>" +
    __("定时撤销授权", "Scheduled revoke") +
    '</label><label class="switch" style="margin-right:4px"><input type="checkbox" id="dev-auth-sched-enabled" ' +
    (dbAuthSchedule && dbAuthSchedule.enabled ? "checked" : "") +
    "><span></span></label>" +
    __("撤销", "Revoke") +
    ' <input type="time" id="dev-auth-revoke" value="' +
    escHtml((dbAuthSchedule && dbAuthSchedule.revokeTime) || "") +
    '" style="font-size:13px;padding:2px 6px;border-radius:4px;border:1px solid var(--border-color);background:var(--bg-input);color:var(--text-primary)">' +
    __("恢复", "Restore") +
    ' <input type="time" id="dev-auth-restore" value="' +
    escHtml((dbAuthSchedule && dbAuthSchedule.restoreTime) || "") +
    '" style="font-size:13px;padding:2px 6px;border-radius:4px;border:1px solid var(--border-color);background:var(--bg-input);color:var(--text-primary)">' +
    "</div>" +
    '<button class="btn btn-ghost btn-sm" onclick="saveBridgeChannel()">' +
    __("保存并应用", "Save & Apply") +
    "</button></div></div>";

  selfHtml += "</div>";
  var html =
    selfHtml +
    '<div class="card"><h2>' +
    __("设备网关", "Device Gateway") +
    " (" +
    devs.length +
    ")" +
    "</h2>" +
    '<div style="margin-bottom:8px"><button class="btn btn-ghost btn-sm" onclick="deviceRefresh()">' +
    __("刷新", "Refresh") +
    "</button></div>";
  if (devs.length === 0) {
    html +=
      '<p style="color:var(--text-muted)">' +
      __(
        "暂无设备接入。设备通过 WebSocket 连接到设备网关（默认经 HologramAgent 反代到 devices.<基域名>，或直连 127.0.0.1:9890/api/v1/device/ws），携带 token 后 hello 登记、bind 授权。",
        "No devices yet. Devices connect via WebSocket (proxied by HologramAgent at devices.<base-domain>, or directly 127.0.0.1:9890/api/v1/device/ws), hello to register, bind to authorize.",
      ) +
      "</p>";
  } else {
    html +=
      "<table><tr><th>" +
      __("设备", "Device") +
      "</th><th>" +
      __("种类", "Kind") +
      "</th><th>" +
      __("状态", "Status") +
      "</th><th>" +
      __("授权", "Authorized") +
      "</th><th>" +
      __("能力", "Caps") +
      "</th><th>" +
      __("操作", "Actions") +
      "</th></tr>";
    devs.forEach((d) => {
      var online = d.online
        ? '<span class="dot-green"></span>' + __("在线", "Online")
        : '<span class="dot-gray"></span>' + __("离线", "Offline");
      var auth = d.authorized
        ? '<span class="dot-green"></span>' + __("已授权", "Yes")
        : '<span class="dot-red"></span>' + __("未授权", "No");
      var caps = (d.caps || []).join(", ") || "-";
      html +=
        "<tr><td><b>" +
        escHtml(d.name || d.device_id) +
        '</b><br><span style="font-size:13px;color:var(--text-muted)">' +
        escHtml(d.device_id) +
        "</span></td><td>" +
        escHtml(d.kind || "-") +
        "</td><td>" +
        online +
        "</td><td>" +
        auth +
        "</td><td>" +
        escHtml(caps) +
        "</td><td>";
      // 客户端鉴权：只有本机设备可切换授权开关；其它设备的授权由其自身控制
      if (d.device_id === state.selfDeviceId) {
        html +=
          '<label class="switch"><input type="checkbox"' +
          (d.authorized ? " checked" : "") +
          " onchange=\"deviceToggleAuth('" +
          d.device_id +
          "',this.checked)\"><span></span></label>";
      } else {
        html += '<span style="color:var(--text-muted);font-size:12px">' +
          __("由该设备自行控制", "Controlled by device itself") + "</span>";
      }
      html += "</td></tr>";
    });
    html += "</table>";
  }
  html += "</div>";
  el.innerHTML = authErrHtml + html;
}

// 保存设备通道配置（网关 + token + 启用），调主进程 deviceBridge:set
async function saveBridgeChannel() {
  try {
    if (!window.homeagent || !window.homeagent.deviceBridge) {
      toast(__("设备桥不可用", "Device bridge unavailable"), true);
      return;
    }
    var gw = (document.getElementById("dev-bridge-gw").value || "").trim();
    var tok = (document.getElementById("dev-bridge-token").value || "").trim();
    if (!gw) {
      toast(__("请填设备通道地址", "Set device channel URL first"), true);
      return;
    }
    var cfg = { enabled: true, gateway: gw };
    if (tok) cfg.token = tok;
    // 能力配置：screensue 屏幕/时长 / cmdrun 目录 / 沙箱
    var disp = document.getElementById("dev-bridge-display");
    if (disp) cfg.screensueDisplay = disp.value || "0";
    var dur = document.getElementById("dev-bridge-duration");
    if (dur) {
      var dv = parseInt(dur.value, 10);
      cfg.screensueDuration = Number.isFinite(dv) && dv >= 0 ? String(dv) : "5";
    }
    var audioSel = document.getElementById("dev-bridge-audio");
    if (audioSel) cfg.audio = { device: audioSel.value || "default" };
    var cwd = document.getElementById("dev-bridge-cwd");
    var sandbox = document.getElementById("dev-bridge-sandbox");
    var boxdir = document.getElementById("dev-bridge-boxdir");
    var ex = {};
    if (cwd) ex.cwd = cwd.value.trim();
    if (sandbox) ex.sandbox = sandbox.value || "off";
    if (boxdir) ex.boxDir = boxdir.value.trim();
    cfg.exec = ex;
    // 定时撤销/恢复授权
    var schedEnabled = document.getElementById("dev-auth-sched-enabled");
    var schedRevoke = document.getElementById("dev-auth-revoke");
    var schedRestore = document.getElementById("dev-auth-restore");
    cfg.authSchedule = {
      enabled: !!(schedEnabled && schedEnabled.checked),
      revokeTime: schedRevoke ? schedRevoke.value || "" : "",
      restoreTime: schedRestore ? schedRestore.value || "" : "",
    };
    var r = await window.homeagent.deviceBridge.set(cfg);
    state.dbConfig = r || state.dbConfig;
    toast(__("设备通道已保存并应用", "Device channel saved & applied"));
    refreshAll();
    renderDevices();
  } catch (e) {
    toast(__("保存失败: ", "Save failed: ") + e.message, true);
  }
}

async function deviceRefresh() {
  try {
    // 手动刷新是显式动作：破掉网关地址缓存，确保拿到服务端当前值
    state.discoveredGateway = "";
    await loadDiscoveredGateway();
    await ensureDeviceToken();
    var d = await api("/device/online");
    state.devices = d.devices || [];
    renderDevices();
  } catch (e) {
    toast(__("设备列表刷新失败: ", "Refresh failed: ") + e.message, true);
  }
}

async function deviceToggleAuth(deviceID, auth) {
  try {
    // 客户端鉴权：授权状态存在设备本地（gui-prefs），不经服务端，agent 无法篡改
    if (window.homeagent && window.homeagent.deviceBridge) {
      await window.homeagent.deviceBridge.setAuthorized(auth);
    }
    toast(
      __("本机授权已更新", "Local authorization updated") + " (" +
        (auth ? __("已授权", "Yes") : __("未授权", "No")) +
        ")",
    );
    deviceRefresh();
  } catch (e) {
    toast(__("授权失败: ", "Auth failed: ") + e.message, true);
  }
}

function deviceSendCmd(deviceID) {
  var cmd = prompt(__("输入要执行的命令", "Enter command to run"));
  if (!cmd) return;
  api("/device/push", {
    method: "POST",
    body: JSON.stringify({
      device_id: deviceID,
      payload: { op: "cmd", command: cmd },
    }),
  })
    .then(() => {
      toast(__("命令已下发", "Command sent"));
      deviceRefresh();
    })
    .catch((e) => {
      toast(__("下发失败: ", "Send failed: ") + e.message, true);
    });
}
