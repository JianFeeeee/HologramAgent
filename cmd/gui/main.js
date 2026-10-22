const { app, BrowserWindow, ipcMain, Menu, screen, desktopCapturer, clipboard } = require("electron");
let screensueWin = null; // screensue 展示窗口（独立于主窗口，显示在配置的屏幕）
// 设备桥直连远程网关：绕过系统代理（本机 clash 代理会导致 wss 被雷池 403）
try {
  app.commandLine.appendSwitch("no-proxy-server");
} catch (e) {}
const path = require("path");
const fs = require("fs");
const { spawn } = require("child_process");
const http = require("http");
const crypto = require("crypto");
const { pathToFileURL } = require("url");
const agentCursor = require("./agent-cursor");
const agentInject = require("./agent-inject");

const CONNECTIONS_FILE = path.join(app.getPath("userData"), "connections.json");
const LOG_FILE = path.join(app.getPath("userData"), "gui.log");
function log(msg) {
  try {
    fs.appendFileSync(LOG_FILE, new Date().toISOString() + " " + msg + "\n");
  } catch (e) {}
}
const _consoleLog = console.log,
  _consoleErr = console.error;
console.log = function () {
  log(Array.prototype.slice.call(arguments).join(" "));
  _consoleLog.apply(null, arguments);
};
console.error = function () {
  log("ERR " + Array.prototype.slice.call(arguments).join(" "));
  _consoleErr.apply(null, arguments);
};

let homedProcess = null;
let mainWindow;
let authRule = null;

function installAuthRule() {
  const { session } = require("electron");
  session.defaultSession.webRequest.onBeforeSendHeaders((details, callback) => {
    const h = Object.assign({}, details.requestHeaders);
    // 按目标 host 校验作用域：网关会把请求 302 到 login.* 域，若按 url 前缀匹配，
    // 登录域自身请求会被错误附加本站 cookie。
    const hu = (() => {
      try {
        return new URL(details.url);
      } catch (e) {
        return null;
      }
    })();
    if (authRule && hu && authRule.hosts && authRule.hosts.has(hu.hostname)) {
      if (authRule.headers) {
        Object.keys(authRule.headers).forEach((k) => {
          h[k] = authRule.headers[k];
        });
      }
      // Electron 会自动附带 jar 中 Cookie（含外部网关 Set-Cookie 的 sl-session）。
      // 这里再显式合并持久化 cookie 与 sl-session 兜底，避免网关再 302。
      // 注意去重：多个 sl-session 叠加会让 portal 网关解析冲突 → 401
      const extra = [authRule.cookie, authRule.slSession].filter(Boolean);
      if (extra.length) {
        const existing = h["Cookie"] || "";
        const merged = [existing].concat(extra).filter(Boolean).join("; ");
        // 按 cookie 名去重：保留第一个，后续同名丢弃（网关只认首值）
        const seen = {};
        const dedup = merged
          .split("; ")
          .map((s) => s.trim())
          .filter((s) => {
            const eq = s.indexOf("=");
            const name = eq > 0 ? s.slice(0, eq) : s;
            if (seen[name]) return false;
            seen[name] = true;
            return true;
          })
          .join("; ");
        h["Cookie"] = dedup;
      }
    }
    callback({ requestHeaders: h });
  });
  session.defaultSession.webRequest.onHeadersReceived((details, callback) => {
    const h = Object.assign({}, details.responseHeaders || {});
    const inScope = (() => {
      if (details.method === "OPTIONS") return true;
      if (!authRule || !authRule.urlHost) return false;
      try {
        return new URL(details.url).hostname === authRule.urlHost;
      } catch (e) {
        return false;
      }
    })();
    if (inScope) {
      h["Access-Control-Allow-Origin"] = ["*"];
      h["Access-Control-Allow-Methods"] = ["GET,POST,PUT,DELETE,OPTIONS"];
      h["Access-Control-Allow-Headers"] = [
        "Content-Type, X-API-Key, Authorization, Cookie",
      ];
      h["Access-Control-Max-Age"] = ["86400"];
    }
    if (details.method === "OPTIONS") {
      callback({ responseHeaders: h, statusLine: "HTTP/1.1 200 OK" });
      return;
    }
    callback({ responseHeaders: h });
  });
  session.defaultSession.webRequest.onCompleted((details) => {
    if (!authRule || !authRule.urlHost) return;
    try {
      if (new URL(details.url).hostname !== authRule.urlHost) return;
    } catch (e) {
      return;
    }
    if (details.url.indexOf("/api/") !== -1) {
      log(
        "[req] " +
          details.method +
          " " +
          details.statusCode +
          " " +
          details.url.slice(0, 120),
      );
    }
  });
  session.defaultSession.webRequest.onErrorOccurred((details) => {
    if (!authRule || !authRule.urlHost) return;
    try {
      if (new URL(details.url).hostname !== authRule.urlHost) return;
    } catch (e) {
      return;
    }
    log(
      "[req-err] " +
        details.method +
        " " +
        details.error +
        " " +
        details.url.slice(0, 120),
    );
  });
}

// 组装 authRule（url + 持久化 cookie + 显式 sl-session），并缓存 hosts 集合。
// 供 installAuthRule 按目标 host 做作用域匹配：jsdom 前缀匹配有误伤，host 级最稳。
async function applyAuthRule(url, cookie, headers) {
  if (!url) {
    authRule = null;
    return;
  }
  let hosts;
  let urlHost = "";
  try {
    const h = new URL(url).hostname;
    urlHost = h;
    hosts = new Set([h]);
  } catch (e) {
    hosts = new Set();
  }
  authRule = {
    url,
    urlHost,
    hosts,
    cookie: cookie || "",
    headers: headers || {},
  };
  // 显式的 sl-session 从 cookie jar 里取（网关 Set-Cookie 且 HttpOnly）
  authRule.slSession = await adoptSlSession(url);
}

// 从 Electron cookie jar 中取出目标 host 的 sl-session（若存在）。
async function adoptSlSession(url) {
  try {
    const { session } = require("electron");
    const hostname = new URL(url).hostname;
    const all = await session.defaultSession.cookies.get({});
    const hit = (all || []).find(
      (c) =>
        c.name === "sl-session" &&
        (c.domain || "").replace(/^\./, "") === hostname,
    );
    return hit ? "sl-session=" + hit.value : "";
  } catch (e) {
    return "";
  }
}

// 带 Electron cookie jar 的 fetch（跟随外部网关 302 → login 域 → 回跳，最多 6 跳），
// 使 POST /api/v1/login 真正触达内层 HologramAgent 并取回 homeagent_session。
async function sessionFetch(initUrl, opts) {
  const { session } = require("electron");
  let url = initUrl;
  let resp;
  for (let i = 0; i < 6; i++) {
    resp = await session.defaultSession.fetch(url, opts);
    if (
      resp.status >= 300 &&
      resp.status < 400 &&
      resp.headers.get("location")
    ) {
      url = new URL(resp.headers.get("location"), url).toString();
      continue;
    }
    return resp;
  }
  return resp;
}

function loadConnections() {
  try {
    if (fs.existsSync(CONNECTIONS_FILE)) {
      const raw = fs
        .readFileSync(CONNECTIONS_FILE, "utf-8")
        .replace(/^\uFEFF/, "");
      // ★ 空/纯空白 = 「还没有配置」，不是「配置损坏」。
      //
      // 原实现在这里直接 JSON.parse，空文件会抛错并掉进下面的 catch，
      // 而 catch 会**把文件改写成空配置**。也就是说：只要读到一次空/半截
      // 内容（写入竞争、上次异常退出的残留、另一实例正在改写），
      // 用户的真实连接列表就被永久覆盖成空 —— 且没有二次确认。
      //
      // 实测（沙箱复现原函数）：空文件 -> 返回空配置，
      // **并把文件写成 {"connections":[],"currentId":null}**。
      if (raw.trim() === "") {
        console.log("connections.json is empty; treating as uninitialized");
        return { connections: [], currentId: null };
      }
      const data = JSON.parse(raw);
      normalizeConnections(data);
      return data;
    }
  } catch (e) {
    console.error("Failed to load connections:", e);
    // 配置损坏：备份后重建，避免应用一直处于"无连接"状态
    try {
      // 带时间戳：原实现只写一个固定的 .bak，反复损坏会把上一份
      // 真实配置覆盖掉，于是「唯一的退路」也丢了。
      const stamp = new Date().toISOString().replace(/[:.]/g, "-");
      const backup = CONNECTIONS_FILE + ".corrupt-" + stamp + ".bak";
      fs.copyFileSync(CONNECTIONS_FILE, backup);
      fs.writeFileSync(
        CONNECTIONS_FILE,
        '{"connections":[],"currentId":null}',
        "utf-8",
      );
      console.error("Backed up corrupt connections to", backup);
    } catch (e2) {
      console.error("Failed to recover connections file:", e2);
    }
  }
  // Fallback: check app resource dir (installer writes fallback copy there)
  try {
    const fallback = path.join(__dirname, "connections.json");
    if (fs.existsSync(fallback)) {
      const data = JSON.parse(fs.readFileSync(fallback, "utf-8"));
      normalizeConnections(data);
      saveConnections(data);
      console.log("Imported connections from app resource dir");
      return data;
    }
  } catch (e) {
    console.error("Fallback connections load failed:", e);
  }
  return { connections: [], currentId: null };
}

// 兼容旧数据：缺失的 type 默认为 webui（HTTP）
function normalizeConnections(data) {
  if (!data || !Array.isArray(data.connections)) return;
  data.connections.forEach((c) => {
    if (!c.type) c.type = "webui";
    if (c.type !== "cli" && c.type !== "webui") c.type = "webui";
  });
}

function saveConnections(data) {
  // ★ 原子写：先写同目录临时文件，再 rename 覆盖。
  //
  // 为什么必须：原实现直接 writeFileSync 截断重写，落地过程中存在
  // 「已截断、内容未写完」的窗口。若此刻另一个实例（或本实例另一次
  // loadConnections）读到那份半截内容，JSON.parse 抛错就走损坏分支 ——
  // 于是「一次并发读」变成「配置被清空」。rename 在同一文件系统内
  // 是原子的：读者只会看到改写前或改写后的完整文件。
  const tmp = CONNECTIONS_FILE + ".tmp-" + process.pid;
  try {
    fs.writeFileSync(tmp, JSON.stringify(data, null, 2), "utf-8");
    fs.renameSync(tmp, CONNECTIONS_FILE);
  } catch (e) {
    console.error("Failed to save connections:", e);
    try {
      if (fs.existsSync(tmp)) fs.unlinkSync(tmp);
    } catch (_) {}
  }
}

function findHomed() {
  if (process.platform !== "win32") return null;
  const exeDir = path.dirname(app.getPath("exe"));
  const p = path.resolve(exeDir, "..", "homed.exe");
  return fs.existsSync(p) ? p : null;
}

// ★ 探测服务端候选地址。
//
// 旧实现硬编码 http://localhost:8080/，在「服务端跑在 WSL/另一台机器」
// 的常见部署下必然探不通，进而误判服务未启动、去拉起 GUI 旁边并不存在的
// homed.exe，最后打印 "homed failed to start within timeout"。
// 实测证据：localhost:8080 返回 000，而 connections.json 里配置的
// 192.168.2.60:8080 返回 302 —— 服务一直好好地在那儿。
//
// 现在按「已配置的连接 → localhost」顺序探，命中任一即视为在线。
function serverProbeTargets() {
  const targets = [];
  const seen = new Set();
  const push = (u) => {
    if (!u) return;
    const s = String(u).replace(/\/+$/, "");
    if (!/^https?:\/\//.test(s)) return;
    if (seen.has(s)) return;
    seen.add(s);
    targets.push(s);
  };
  try {
    const data = loadConnections();
    // loadConnections 返回 {connections:[...], currentId}，
    // 不是数组（早期按数组写的判断在此处不成立）。
    const list = data && Array.isArray(data.connections) ? data.connections : [];
    // 当前连接优先，其余也一并探测（用户可能切过连接）
    const curId = data ? data.currentId : null;
    const ordered = list
      .slice()
      .sort((a, b) => (a && b && a.id === curId ? -1 : 0));
    for (const c of ordered) if (c && c.url) push(c.url);
  } catch (e) {}
  push("http://localhost:8080");
  return targets;
}

function probeOne(target, timeoutMs) {
  return new Promise((resolve) => {
    let done = false;
    const finish = (ok, via) => {
      if (done) return;
      done = true;
      resolve({ ok, via });
    };
    let req;
    try {
      req = http.get(target + "/", () => finish(true, target));
    } catch (e) {
      return finish(false, target);
    }
    req.on("error", () => finish(false, target));
    req.setTimeout(timeoutMs || 2000, () => {
      try {
        req.destroy();
      } catch (e) {}
      finish(false, target);
    });
  });
}

// 任一候选端点可达即认为服务在线。
async function isServerRunning() {
  const targets = serverProbeTargets();
  for (const t of targets) {
    const r = await probeOne(t, 1500);
    if (r.ok) return true;
  }
  return false;
}

async function waitForServer(maxWait = 8000) {
  const start = Date.now();
  for (;;) {
    if (await isServerRunning()) return true;
    if (Date.now() - start > maxWait) return false;
    await new Promise((r) => setTimeout(r, 300));
  }
}

function startHomed() {
  const homedBin = findHomed();
  if (!homedBin) {
    console.log("homed.exe not found near GUI, skipping auto-launch");
    return;
  }
  const dataDir = path.resolve(path.dirname(homedBin), "data");
  console.log("Starting homed:", homedBin, "-data", dataDir);
  homedProcess = spawn(homedBin, ["-data", dataDir], {
    stdio: "ignore",
    detached: false,
    windowsHide: true,
  });
  homedProcess.on("error", (err) => {
    console.error("homed start failed:", err.message);
    homedProcess = null;
  });
  homedProcess.on("exit", (code) => {
    console.log("homed exited with code", code);
    homedProcess = null;
  });
}

function stopHomed() {
  if (homedProcess) {
    homedProcess.kill();
    homedProcess = null;
  }
}

// createWindow 创建主窗口。
//
// ★ show=false = **静默启动**：窗口照常创建（渲染进程随之启动，SSE、
// 设备桥、聊天全部可用），只是不显示。
//
// 为什么不是「不创建窗口」：不创建 ⇒ 渲染进程不启动 ⇒ app.js 整个不执行
// ⇒ SSE 通道与设备桥轮询全废，GUI 退化成纯托盘图标；而且
// showMainWindow() 只做 show()、不会创建窗口，app.on("activate") 又只在
// macOS 触发 —— Windows 上托盘菜单点「显示主界面」将**毫无反应**，
// 进程变成无法唤起的僵尸。
//
// 为什么不用 show:false 再 show()：那会先显示一帧再隐藏，肉眼可见闪一下。
// 这里用 ready-to-show + 条件 show，避免闪烁。
function createWindow(show = true) {
  const menu = Menu.buildFromTemplate([]);
  Menu.setApplicationMenu(menu);

  mainWindow = new BrowserWindow({
    width: 1280,
    height: 860,
    minWidth: 900,
    minHeight: 600,
    title: "HologramAgent",
    frame: false,
    show: false, // 一律先不显示，由 ready-to-show 决定
    icon: path.join(__dirname, "icon.ico"),
    webPreferences: {
      preload: path.join(__dirname, "preload.js"),
      contextIsolation: true,
      nodeIntegration: false,
    },
  });

  // 静默时不显示；非静默时在首帧就绪后显示，避免白屏闪现。
  //
  // ★ 用 on 而不是 once（2026-10-05 修白屏）：once 只等一次，首帧之后
  //   监听就没了。而窗口可能被系统回收后重建、或GPU 重启后重新 load，
  //   那时都需要再show 一次 —— once 的写法让那些路径静默失败。
  mainWindow.on("ready-to-show", () => {
    if (!show) return;
    try {
      if (mainWindow && !mainWindow.isDestroyed() && !mainWindow.isVisible()) {
        mainWindow.show();
      }
    } catch (e) {}
  });

  mainWindow.loadFile(path.join(__dirname, "renderer", "index.html"));

  // ★ renderer 崩溃自愈（2026-10-05 修「长时间挂后台后打开白屏」）。
  //
  // 现象：GUI 在后台挂很久（数小时）后，从托盘打开是**白屏**，且不会自愈。
  //
  // 原因链：
  //   长时间后台 → Chromium 回收 renderer 的 GPU context（或渲染进程
  //   本身被判定 hung 杀掉）→ 窗口对象仍在、isVisible() 仍返回 true，
  //   但内容是空的 → showMainWindow() 的 `if (!isVisible()) show()` 被跳过
  //   → 托盘点一下，窗口出现但是白的，永远不会好。
  //
  // 之前全代码没有任何 render-process-gone / unresponsive 监听，所以这条
  // 路径完全无感知。仓库历史上有过「白屏修复(惰性Tray)」提交，但那只修了
  // **首次**显示时机，没管**长期后台后**。
  mainWindow.webContents.on("render-process-gone", (e, details) => {
    console.error(
      "[window] renderer gone: " + (details && details.reason) +
        " (exitCode=" + (details && details.exitCode) + ")",
    );
    // OOM 之外的都重建；OOM（reason==="oom"）重建只会立刻再崩一次，
    // 只记录日志避免反复重启把机器拖死。
    if (details && details.reason === "oom") return;
    try {
      if (mainWindow && !mainWindow.isDestroyed()) {
        mainWindow.reload();
      }
    } catch (err) {
      console.error("[window] reload after crash failed: " + err.message);
    }
  });

  // renderer 活着但无响应（主线程被长任务卡死）：此时页面还在，
  // 只是不响应。给一次刷新机会，不自动刷（误判会让正在用的用户丢状态）。
  mainWindow.webContents.on("unresponsive", () => {
    console.error("[window] renderer unresponsive");
  });
  mainWindow.webContents.on("responsive", () => {
    console.log("[window] renderer responsive again");
  });

  if (process.argv.includes("--dev")) {
    mainWindow.webContents.openDevTools();
  }

  // 退出进托盘：拦截窗口关闭事件（exitToTray 且托盘可用时 hide 而非 close）
  mainWindow.on("close", (e) => {
    try {
      const _p = loadGuiPrefs();
      if (_p && _p.exitToTray && tray) {
        e.preventDefault();
        mainWindow.hide();
        return;
      }
    } catch (err) {}
  });
  mainWindow.on("closed", () => {
    mainWindow = null;
  });
}

ipcMain.handle("window:minimize", (e) => {
  BrowserWindow.fromWebContents(e.sender)?.minimize();
});
ipcMain.handle("window:toggleMaximize", (e) => {
  const win = BrowserWindow.fromWebContents(e.sender);
  if (!win) return;
  if (win.isMaximized()) win.unmaximize();
  else win.maximize();
});
ipcMain.handle("window:close", (e) => {
  const win = BrowserWindow.fromWebContents(e.sender);
  if (!win) return;
  // 退出进托盘：偏好开启且托盘存在时隐藏而非关闭
  try {
    const _p = loadGuiPrefs();
    console.log(
      "[window:close] exitToTray=" +
        !!(_p && _p.exitToTray) +
        " tray=" +
        !!tray,
    );
    if (_p && _p.exitToTray && tray) {
      win.hide();
      return true;
    }
  } catch (err) {}
  win.close();
});

ipcMain.handle("connections:list", () => {
  return loadConnections();
});

ipcMain.handle("connections:add", (_, conn) => {
  const data = loadConnections();
  const id = Date.now().toString(36) + Math.random().toString(36).slice(2, 6);
  data.connections.push({
    id,
    name: conn.name || "",
    url: conn.url || "",
    apiKey: conn.apiKey || "",
    type: conn.type === "cli" ? "cli" : "webui",
    socketPath: conn.socketPath || "",
    // 网关认证字段在 add 时必须一并持久化，否则新增连接后 cookie/网关标记丢失
    username: conn.username || "",
    password: conn.password || "",
    cookie: conn.cookie || "",
    headers: conn.headers || "",
    gateway: !!conn.gateway,
  });
  if (!data.currentId) data.currentId = id;
  saveConnections(data);
  try {
    rebuildTrayMenu();
  } catch (e) {}
  return data;
});

ipcMain.handle("connections:update", (_, { id, updates }) => {
  const data = loadConnections();
  const idx = data.connections.findIndex((c) => c.id === id);
  if (idx !== -1) {
    data.connections[idx] = { ...data.connections[idx], ...updates };
    saveConnections(data);
  }
  try {
    rebuildTrayMenu();
  } catch (e) {}
  return data;
});

ipcMain.handle("connections:delete", (_, id) => {
  const data = loadConnections();
  data.connections = data.connections.filter((c) => c.id !== id);
  if (data.currentId === id) {
    data.currentId =
      data.connections.length > 0 ? data.connections[0].id : null;
  }
  saveConnections(data);
  try {
    rebuildTrayMenu();
  } catch (e) {}
  return data;
});

function downloadTo(src, dest) {
  return new Promise((resolve, reject) => {
    const mod = src.startsWith("https:") ? require("https") : http;
    const req = mod.get(
      src,
      {
        headers: {
          "User-Agent":
            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
        },
        rejectUnauthorized: false,
      },
      (res) => {
        if (
          res.statusCode >= 300 &&
          res.statusCode < 400 &&
          res.headers.location
        ) {
          res.resume();
          downloadTo(new URL(res.headers.location, src).toString(), dest).then(
            resolve,
            reject,
          );
          return;
        }
        if (res.statusCode !== 200) {
          res.resume();
          reject(new Error("HTTP " + res.statusCode));
          return;
        }
        const out = fs.createWriteStream(dest);
        res.pipe(out);
        out.on("finish", () => out.close(resolve));
        out.on("error", reject);
        res.on("error", reject);
      },
    );
    req.on("error", reject);
    req.setTimeout(60000, () => req.destroy(new Error("timeout")));
  });
}

ipcMain.handle("bg:cache", async (_, { src }) => {
  if (!src || typeof src !== "string") return { ok: false, error: "no src" };
  const dir = path.join(app.getPath("userData"), "bg-cache");
  try {
    fs.mkdirSync(dir, { recursive: true });
    const hash = crypto
      .createHash("sha1")
      .update(src)
      .digest("hex")
      .slice(0, 24);
    let dest = null;
    if (/^https?:\/\//i.test(src)) {
      dest = path.join(dir, hash);
      if (!fs.existsSync(dest)) {
        try {
          await downloadTo(src, dest);
        } catch (e) {
          return {
            ok: false,
            error: "下载失败: " + e.message,
            useOriginal: true,
          };
        }
      }
    } else if (/^file:\/\//i.test(src)) {
      dest = new URL(src).pathname.replace(/^\/([A-Za-z]:)/, "$1");
      if (!fs.existsSync(dest))
        return { ok: false, error: "文件不存在: " + src };
    } else if (/^data:image\//i.test(src)) {
      dest = path.join(dir, hash + ".png");
      if (!fs.existsSync(dest))
        fs.writeFileSync(dest, Buffer.from(src.split(",")[1] || "", "base64"));
    } else {
      const p = path.resolve(src);
      if (!fs.existsSync(p)) return { ok: false, error: "路径不存在: " + src };
      dest = path.join(dir, hash + (path.extname(p) || ""));
      if (!fs.existsSync(dest)) fs.copyFileSync(p, dest);
    }
    return { ok: true, file: pathToFileURL(dest).href };
  } catch (e) {
    return { ok: false, error: e.message };
  }
});

ipcMain.handle("connections:setCurrent", (_, id) => {
  const data = loadConnections();
  if (data.connections.some((c) => c.id === id)) {
    data.currentId = id;
    saveConnections(data);
  }
  try {
    rebuildTrayMenu();
  } catch (e) {}
  return data;
});

async function doWebuiLogin(baseUrl, username, password, extraCookie) {
  const u = baseUrl + "/api/v1/login";
  const headers = {
    "Content-Type": "application/json",
    Accept: "application/json",
  };
  if (extraCookie) headers["Cookie"] = extraCookie;
  const resp = await sessionFetch(u, {
    method: "POST",
    headers,
    body: JSON.stringify({ username, password }),
  });
  // 1) Set-Cookie 直读（重定向未被 session.fetch 自动吞掉时）
  const sc = resp.headers.get("set-cookie") || "";
  let m = /homeagent_session=([^;]+)/.exec(sc);
  // 2) 重定向被自动跟随时 Set-Cookie 可能在中间 302 上：从 cookie jar 兜底
  if (!m) {
    try {
      const { session } = require("electron");
      const hostname = new URL(u).hostname;
      const all = await session.defaultSession.cookies.get({});
      const hit = (all || []).find(
        (c) =>
          c.name === "homeagent_session" &&
          (c.domain || "").replace(/^\./, "").toLowerCase() ===
            hostname.toLowerCase(),
      );
      if (hit) m = [null, hit.value];
    } catch (e) {}
  }
  if (!m) {
    const bodyTxt = await resp.text();
    throw new Error(
      "login ok but no homeagent_session cookie (HTTP " +
        resp.status +
        " " +
        bodyTxt.slice(0, 150) +
        ")",
    );
  }
  return m[1];
}

ipcMain.on("log:r", (_e, m) => {
  log("[r] " + m);
});
ipcMain.handle("log:r", (_e, m) => {
  log("[r] " + m);
  return true;
});

ipcMain.handle(
  "webui:setAuth",
  async (_, { url, cookie, headers, username, password }) => {
    const resp = { ok: true, error: "" };
    let extra = cookie || "";
    try {
      if (username && url) {
        try {
          const tok = await doWebuiLogin(url, username, password, extra);
          extra = extra
            ? extra + "; homeagent_session=" + tok
            : "homeagent_session=" + tok;
        } catch (e) {
          if (extra) {
            log(
              "[setAuth] auto-login skipped (gateway cookie mode): " +
                e.message.slice(0, 120),
            );
          } else {
            throw e;
          }
        }
      } else if (!cookie) {
        await applyAuthRule("", "", {});
        return resp;
      }
    } catch (e) {
      await applyAuthRule(url, extra || "", headers || {});
      resp.ok = false;
      resp.error = e.message;
      log(
        "[setAuth] fail url=" + (url || "") + " err=" + e.message.slice(0, 150),
      );
      return resp;
    }
    await applyAuthRule(url, extra, headers || {});
    log(
      "[setAuth] ok url=" +
        (url || "") +
        " cookieLen=" +
        extra.length +
        " hasTok=" +
        (extra.indexOf("homeagent_session") !== -1),
    );
    return resp;
  },
);

ipcMain.handle("webui:openLogin", (_, { url, username, password }) => {
  if (!url) return { ok: false, error: "no url" };
  const loginWin = new BrowserWindow({
    width: 980,
    height: 740,
    parent: mainWindow,
    modal: true,
    autoHideMenuBar: true,
    title: "HologramAgent - 网关联机登录",
    webPreferences: { contextIsolation: true, nodeIntegration: false },
  });
  const u = encodeURIComponent(username || "");
  const p = encodeURIComponent(password || "");
  let autoFills = 0;
  const tryAutofill = () => {
    if (autoFills > 8) return;
    autoFills++;
    loginWin.webContents
      .executeJavaScript(
        '(function(){var pw=document.querySelector("input[type=password]");' +
          'if(!pw)return false;var f=pw.closest("form")||pw.form;if(!f)return false;' +
          'var ins=f.querySelectorAll("input");var filled=false;' +
          'for(var i=0;i<ins.length;i++){var el=ins[i];var t=(el.type||"").toLowerCase();' +
          'if(t==="password"){if(!el.value)el.value=decodeURIComponent("' +
          p +
          '");}' +
          'else if(t!=="hidden"&&t!=="checkbox"&&t!=="submit"&&t!=="button"){' +
          'if(!el.value)el.value=decodeURIComponent("' +
          u +
          '");}}' +
          'if(f.querySelector("button")){}' +
          'var sb=f.querySelector("input[type=submit],button[type=submit]");' +
          "if(sb&&sb.disabled)return false;" +
          "if(f.requestSubmit)f.requestSubmit();else f.submit();" +
          "return true;})()",
      )
      .then((r) => {
        if (r) log("[openLogin] autofilled gateway login form");
      })
      .catch((e) => log("[openLogin] autofill error: " + e.message));
  };
  loginWin.webContents.on("did-finish-load", () => {
    if (autoFills < 3) tryAutofill();
  });
  loginWin.webContents.on("did-navigate", (_e, u) => {
    log("[openLogin] nav: " + u);
    if (autoFills < 6) setTimeout(tryAutofill, 600);
  });
  loginWin.webContents.on(
    "did-fail-load",
    (_e, code, desc, isMain, failedUrl) => {
      if (isMain)
        log("[openLogin] fail-load " + code + " " + desc + " @ " + failedUrl);
    },
  );
  loginWin
    .loadURL(url)
    .catch((e) => console.error("login window load error:", e.message));
  let grabbed = null;
  loginWin.on("close", async () => {
    if (grabbed) return;
    try {
      const all = await require("electron").session.defaultSession.cookies.get(
        {},
      );
      const host = new URL(url).hostname;
      const keep = (all || []).filter((c) => {
        const d = (c.domain || "").replace(/^\./, "");
        const onHost = host === d || host.endsWith("." + d);
        const onSibling = /(^|\.)jianfgit\.xyz$/i.test(d);
        return onHost || onSibling || /^sl-/.test(c.name || "");
      });
      const list = keep.map((c) => c.name + "=" + c.value);
      // 目标 host 的 sl-session 一并带上（网关会话是 host 级，登录域抓取未必覆盖）
      const sl = await adoptSlSession(url);
      if (sl && list.indexOf(sl) === -1) list.push(sl);
      log(
        "[openLogin] close-grab: host=" +
          host +
          " kept=" +
          keep.length +
          " names=" +
          keep.map((c) => c.name + "@" + (c.domain || "")).join(","),
      );
      grabbed = {
        ok: true,
        cookie: list.join("; "),
        count: keep.length + (sl ? 1 : 0),
      };
    } catch (e) {
      log("[openLogin] close-grab error: " + e.message);
      grabbed = { ok: false, error: e.message };
    }
  });
  loginWin.on("closed", () => {
    if (mainWindow && !mainWindow.isDestroyed() && grabbed) {
      mainWindow.webContents.send("webui:login-result", { ...grabbed, url });
    }
  });
  return { ok: true };
});

// CLI 传输：通过 homed 的 unix socket（逐行 JSON 协议）发起请求。
// 认证行：/auth <apiKey>（若配置了密钥）。返回 JSON 响应行。
ipcMain.handle("cli:request", (_, { socketPath, apiKey, line }) => {
  return new Promise((resolve) => {
    const net = require("net");
    let client;
    try {
      client = net.createConnection({ path: socketPath });
    } catch (e) {
      return resolve({ error: "create connection: " + e.message });
    }
    const timeout = setTimeout(() => {
      try {
        client.destroy();
      } catch (_) {}
      resolve({ error: "timeout waiting for cli response" });
    }, 30000);

    let buf = "";
    const onData = (chunk) => {
      buf += chunk.toString("utf8");
      const idx = buf.indexOf("\n");
      if (idx === -1) return;
      const lineOut = buf.slice(0, idx);
      clearTimeout(timeout);
      try {
        client.destroy();
      } catch (_) {}
      try {
        resolve(JSON.parse(lineOut));
      } catch (e) {
        resolve({ error: "bad response: " + lineOut });
      }
    };
    const onError = (err) => {
      clearTimeout(timeout);
      try {
        client.destroy();
      } catch (_) {}
      resolve({ error: err.message });
    };

    client.on("error", onError);
    client.on("data", onData);
    client.on("connect", () => {
      let next = line;
      if (apiKey) next = "/auth " + apiKey + "\n" + next;
      client.write(next + "\n");
    });
  });
});

// ============ Device Bridge: GUI 作为设备接入 remotedevice 网关 ============
// 复用 net（raw TCP）+ 手写 WS 帧；收到 {op:"cmd"} 用 spawn 在本机执行并回 cmd_result。
const devNet = require("net");
const devTls = require("tls");
const devOs = require("os");

let deviceBridge = null; // 当前活动设备桥
let deviceBridgeId = ""; // 设备 meta device_id（hello 后可用于 cmd_result）
let deviceBridgeAddr = ""; // 设备桥网关地址

// 设备桥的**登记**状态（与"连接已建立"是两件事）。
// 连接成功但 bind 被拒时设备是失联的，只看 connected 会给出假阳性。
let deviceBridgeBound = false;
let deviceBridgeBindError = "";
// 设备桥心跳与重连的状态（心跳 + 线性退避重连）。
let devicePingTimer = null;
let deviceReconnectTimer = null;
let deviceReconnectAttempt = 0;
// 拉起意图：用户主动停用后置 false，作为 scheduleDeviceReconnect 的闸。
let deviceWanted = false;
// 当前生效的连接配置，重连时复用（连接断开后局部变量就没了）。
//
// 缓存而非每次读 gui-prefs：重连是定时器里的异步路径，重读偏好会引入
// 「读到半写文件」与「用户刚改完就按新值连」两种意外；且重连必须用
// **当初启动时那份**配置，否则退避语义就乱了。
let deviceBridgeCfg = null;
// 音频/媒体接收聚合缓冲（服务端分块推送二进制→聚合→播放）
let speechAccum = null;

// 读取设备音频输出配置（gui-prefs.deviceBridge.audio）
function getAudioConfig() {
  try {
    const prefs = loadGuiPrefs();
    const db = prefs.deviceBridge || {};
    const au = db.audio || {};
    return { device: au.device || "default" };
  } catch (e) {
    return { device: "default" };
  }
}

// 枚举本机可用音频输出设备（aplay -L 解析：设备名顶格、描述缩进）
function listAudioDevices() {
  const porcp = require("child_process");
  const out = [];
  const SKIP_PREFIXES = ["lavrate", "samplerate", "speex", "jack", "oss",
    "upmix", "vdownmix", "speexrate"];
  try {
    const r = porcp.spawnSync("aplay", ["-L"], { encoding: "utf8" });
    const lines = ((r.stdout || "") + "\n").split("\n");
    for (let i = 0; i < lines.length; i++) {
      const ln = lines[i];
      if (!ln || ln[0] === " " || ln[0] === "\t") continue; // 跳过缩进的描述行
      const name = ln.trim();
      if (!name || /^(Default PCM|List of|Plug PCM)/.test(name)) continue;
      if (SKIP_PREFIXES.some((s) => name === s)) continue; // 插件类，非实体设备
      // 只保留可实际输出的设备类型；纯 hw: 裸设备不支持自动通道转换
      // （mono→stereo）易失败，改用 plughw:（带转换）。pipewire/pulse/sysdefault 亦可。
      const useful = name === "default" || name === "pipewire" ||
        name === "pulse" || name === "sysdefault" ||
        /^plughw:/.test(name) || /^sysdefault:/.test(name);
      if (!useful) continue;
      let desc = "";
      // 下一行可能是描述（缩进）
      if (lines[i + 1] && (lines[i + 1][0] === " " || lines[i + 1][0] === "\t"))
        desc = lines[i + 1].trim();
      out.push({ name, desc });
    }
  } catch (e) {}
  if (!out.find((d) => d.name === "default"))
    out.unshift({ name: "default", desc: "系统默认设备" });
  return out;
}

// 播放设备收到的音频（由 cmd_speech_end 触发，二进制已聚合）
function playDeviceAudio(audioBuf, mime, reqId) {
  try {
    const os = require("os");
    const path = require("path");
    const fs = require("fs");
    const porcp = require("child_process");
    const au = getAudioConfig();
    const ext =
      (mime || "").indexOf("mp3") === -1
        ? (mime || "").indexOf("ogg") === -1
          ? "wav"
          : "ogg"
        : "mp3";
    const tmp = path.join(os.tmpdir(), "ha_speech_" + Date.now() + "." + ext);
    fs.writeFileSync(tmp, audioBuf);
    const platform = process.platform;
    let cmd = null;
    let args = [];
    if (platform === "linux") {
      if (porcp.spawnSync("which", ["aplay"]).status === 0) {
        cmd = "aplay";
        // 支持用户选择的输出设备（默认 default）
        args = au.device && au.device !== "default" ? ["-D", au.device, tmp] : [tmp];
      } else if (porcp.spawnSync("which", ["paplay"]).status === 0) {
        cmd = "paplay";
        args = [tmp];
      } else if (porcp.spawnSync("which", ["ffplay"]).status === 0) {
        cmd = "ffplay";
        args = ["-nodisp", "-autoexit", "-loglevel", "quiet", tmp];
      }
    } else if (platform === "darwin") {
      cmd = "afplay";
      args = [tmp];
    } else if (platform === "win32") {
      cmd = "powershell";
      args = [
        "-Command",
        "(New-Object Media.SoundPlayer '" + tmp + "').PlaySync()",
      ];
    }
    if (cmd) {
      porcp.execFile(cmd, args, { timeout: 60000 }, () => {
        try {
          fs.unlinkSync(tmp);
        } catch (e2) {}
      });
      // 回执：媒体收到并开始播放
      sendCmdResult(
        reqId || "",
        baseResult(
          reqId || "",
          "ok",
          "audio played: " + audioBuf.length + " bytes via " + (au.device || "default"),
          "",
        ),
      );
    } else {
      // 无播放器，至少落盘供手动查看，回执带文件路径
      sendCmdResult(
        reqId || "",
        baseResult(
          reqId || "",
          "ok",
          "audio saved: " + tmp + " (" + audioBuf.length + " bytes)",
          "",
        ),
      );
    }
  } catch (e) {
    console.log("[device-bridge] play audio error: " + e.message);
  }
}

// 建立到 remotedevice WS 网关连接，返回 {send(obj), close()}，消息经 onMsg 回调。
function connectDeviceWS(url, token, onMsg) {
  // URL 支持完整端点（含路径/端口/TLS），不硬编码 host/path
  let u;
  try {
    u = new URL(url || "ws://127.0.0.1:9890/api/v1/device/ws");
  } catch (e) {
    return Promise.reject(new Error("invalid device gateway url: " + url));
  }
  const isTLS = u.protocol === "wss:" || u.protocol === "https:";
  const host = u.hostname;
  // https/wss 默认 443，ws/http 默认 9890（remotedevice 默认端口）
  const defaultPort = isTLS ? 443 : 9890;
  const port = u.port ? parseInt(u.port, 10) : defaultPort;
  const basePath = u.pathname || "/api/v1/device/ws";
  const sep = u.search ? "&" : "?";
  const path =
    basePath +
    (u.search || "") +
    sep +
    "token=" +
    encodeURIComponent(token || "");
  return new Promise((resolve, reject) => {
    const sock = isTLS
      ? devTls.connect({ host, port, rejectUnauthorized: false })
      : devNet.createConnection({ host, port });
    sock.on("error", (e) => reject(e));
    sock.on("connect", () => {
      const key = crypto.randomBytes(16).toString("base64");
      // 远程 wss 走 webui 反代需要门户会话 cookie
      // 优先 authRule（连接认证注入）；启动早期 authRule 未就绪时读 connections.json
      let cookieHdr = "";
      try {
        let ck = "";
        // 优先 connections.json 的持久 cookie（完整登录验证过，浏览器快照可能不完整）
        try {
          const conns = loadConnections();
          const curId = conns.currentId;
          const cur =
            conns.connections.find((c) => c.id === curId) ||
            conns.connections[0];
          if (cur && cur.cookie) ck = cur.cookie;
        } catch (e2) {}
        if (!ck && authRule && authRule.cookie) {
          ck = authRule.cookie;
        }
        if (ck) cookieHdr = "Cookie: " + ck + "\r\n";
        console.log("[device-bridge] ws cookie len=" + (ck || "").length);
      } catch (e) {}
      sock.write(
        "GET " +
          path +
          " HTTP/1.1\r\n" +
          "Host: " +
          host +
          ":" +
          port +
          "\r\n" +
          "Upgrade: websocket\r\nConnection: Upgrade\r\n" +
          "Sec-WebSocket-Key: " +
          key +
          "\r\nSec-WebSocket-Version: 13\r\n" +
          cookieHdr +
          "\r\n",
      );
    });
    let buf = Buffer.alloc(0);
    let upgraded = false;
    let opened = false;
    sock.on("data", (chunk) => {
      buf = Buffer.concat([buf, chunk]);
      if (!upgraded) {
        const idx = buf.indexOf("\r\n\r\n");
        if (idx === -1) return;
        const head = buf.slice(0, idx).toString("utf8");
        buf = buf.slice(idx + 4);
        upgraded = true;
        if (!head.includes("101")) {
          // 保留剩余字节（响应体），记录完整响应便于排错
          const body = buf.toString("utf8").slice(0, 800);
          sock.destroy();
          return reject(
            new Error(
              "WS upgrade failed: " +
                head.split("\r\n")[0] +
                " | BODY=" +
                body +
                " | REQ-PATH=" +
                path,
            ),
          );
        }
        opened = true;
        // 心跳不在这里起：统一由 connectDeviceBridgeOnce 在拿到 ws.__sock
        // 后调 startDevicePing。放这里的话心跳会跟着「每次 connectDeviceWS
        // 调用」起，而不是跟着「一条实际连上的桥」起——重连路径上会漏。
        resolve({
          send: (obj) => sendDeviceFrame(sock, JSON.stringify(obj)),
          __sock: sock, // 暴露底层 socket 供二进制分块发送
          close: () => sock.destroy(),
        });
      }
      while (buf.length >= 2) {
        const b0 = buf[0];
        const opcode = b0 & 0x0f;
        const b1 = buf[1];
        let len = b1 & 0x7f;
        let off = 2;
        if (len === 126) {
          if (buf.length < 4) break;
          len = buf.readUInt16BE(2);
          off = 4;
        } else if (len === 127) {
          if (buf.length < 10) break;
          len = Number(buf.readBigUInt64BE(2));
          off = 10;
        }
        if (buf.length < off + len) break;
        const payload = buf.slice(off, off + len);
        buf = buf.slice(off + len);
        if (opcode === 0x1) {
          try {
            const obj = JSON.parse(payload.toString("utf8"));
            if (obj && obj.op === "cmd_speech_start") {
              // 收到音频开始：初始化聚合缓冲
              try {
                if (speechAccum) speechAccum = null;
              } catch (e2) {}
              speechAccum = {
                reqId: obj.req_id || "",
                kind: obj.kind || "speech",
                mime: obj.mime || "audio/wav",
                total: obj.total || 0,
                chunks: [],
                got: 0,
              };
              continue;
            }
            if (obj && obj.op === "cmd_speech_end") {
              // 音频结束：合并块并播放
              const reqId = obj.req_id || "";
              if (speechAccum) {
                try {
                  const audio = Buffer.concat(speechAccum.chunks);
                  playDeviceAudio(audio, speechAccum.mime, reqId);
                } catch (e2) {}
                speechAccum = null;
              }
              continue;
            }
            onMsg(obj);
          } catch (e) {}
        } else if (opcode === 0x2) {
          // 二进制帧：接收音频数据块（若处于聚合状态）
          if (speechAccum) {
            speechAccum.chunks.push(payload);
            speechAccum.got += payload.length;
          }
        } else if (opcode === 0x8) {
          sock.destroy();
          return;
        }
      }
    });
    sock.on("error", (e) => {
      if (!opened) reject(e);
    });
    sock.on("close", () => {
      // 断开时停掉心跳，否则定时器会一直持有已死的 socket。
      stopDevicePing();
      deviceBridge = null;
      // 已建立过的连接被对端/中间链路关掉 ⇒ 排程重连。
      // scheduleDeviceReconnect 内部有 deviceWanted 闸，用户主动停用
      // 时不会把桥拉起来。
      if (opened) scheduleDeviceReconnect();
    });
  });
}

// ============ 设备桥心跳 + 断线重连 ============
//
// ★ 这里修的是 2026-10-05 实测到的两个洞，合起来的效果是
//   「一次失联，终身失联」：
//
// 1. **不发 ping**。waiter 侧有 pingLoop（internal/devicebridge/client/
//    bridge.go，每 30s 一个 0x9 帧），GUI 侧全文件一次 ping 都没发过。
//    而 remotedevice 服务端**没有设备侧读超时**（registry.go 里没有
//    SetReadDeadline）—— 不发心跳就没人能发现设备已死，网关会把一台
//    半死的设备长期显示为 online。
//
//    实测症状：bind 能成功（bind_ok）、设备进了 devices 表（面板可见），
//    但几秒后 push 就报
//        {"error":"device gui-JianF not online"}
//    —— conns 表已被 defer 里的 markOffline 清空，而 devices 表还在。
//    「可见但不可下发」就是这个割裂的表象。
//
// 2. **不重连**。原来 startDeviceBridge 只有启动/配置变更两个调用点，
//    失败就 console.error 结束。GUI 是最容易被休眠/切网/服务端重启打断的
//    形态（合盖一次、WiFi 抖一下、服务端重启），设备桥就再也回不来，
//    直到用户重启 GUI 或手动改一次配置。
//
// 判据：cmd/gui/device-bridge-liveness.test.mjs（用 node:vm 抽真实函数
// 在沙箱里跑「连接→ 断线 → 重连 → 再断线」，数真实的 ping 帧与重连次数）。

// 与 waiter 的 pingLoop 对齐（bridge.go:636 每 30s 一个 0x9 帧）。
const devicePingIntervalMs = 30000;
// 重连退避：线性增长、封顶 60s。首个等待 = base。
const deviceReconnectBaseMs = 2000;
const deviceReconnectMaxMs = 60000;

// 发送 WS 控制帧（客户端帧必须带掩码位，RFC6455 §5.3）。
//
// 通用签名而非写死 ping：控制帧有 ping(0x9)/pong(0xA)/close(0x8) 三种，
// 写死一种就换不来另外两种。payload 为 null 表示空载荷。
function sendDeviceControlFrame(sock, opcode, payload) {
  const body = payload ? Buffer.from(payload) : Buffer.alloc(0);
  const mask = crypto.randomBytes(4);
  const masked = Buffer.alloc(body.length);
  for (let i = 0; i < body.length; i++) masked[i] = body[i] ^ mask[i % 4];
  const len = masked.length;
  let hdr;
  if (len < 126) {
    hdr = Buffer.from([0x80 | opcode, 0x80 | len]);
  } else if (len < 65536) {
    hdr = Buffer.alloc(4);
    hdr[0] = 0x80 | opcode;
    hdr[1] = 0x80 | 126;
    hdr.writeUInt16BE(len, 2);
  } else {
    hdr = Buffer.alloc(10);
    hdr[0] = 0x80 | opcode;
    hdr[1] = 0x80 | 127;
    hdr.writeBigUInt64BE(BigInt(len), 2);
  }
  sock.write(Buffer.concat([hdr, mask, masked]));
}

// 启动心跳（先清旧的，避免重复 interval 叠加）。
function startDevicePing(sock) {
  if (devicePingTimer) clearInterval(devicePingTimer);
  devicePingTimer = setInterval(() => {
    try {
      if (!sock || sock.destroyed) return;
      sendDeviceControlFrame(sock, 0x9, null);
    } catch (e) {
      /* 心跳失败不致命：close 事件会触发重连 */
    }
  }, devicePingIntervalMs);
}

function stopDevicePing() {
  if (devicePingTimer) {
    clearInterval(devicePingTimer);
    devicePingTimer = null;
  }
}

// 排程一次重连。退避随失败次数线性增长并封顶。
//
// ★ deviceWanted 是闸：用户主动 stopDeviceBridge（或改配置）之后
//   再排程，就会把已停用的桥偷偷拉起来。GUI 是常驻托盘程序，
//   这个「拉起来」没人看得见，表现为「我明明关了怎么又在连」。
function scheduleDeviceReconnect() {
  if (!deviceWanted) return;
  if (deviceReconnectTimer) return; // 已有排程，不叠加
  // 先递增后算：首次排程（含连接失败那一回）拿到的就是 base，
  // 之后 2×base、3×base… 线性增长。用 max(1, attempt) 会在
  // attempt=0 与 1 时都算出 base，出现「退避头两次一样」的假象。
  deviceReconnectAttempt++;
  const delay = Math.min(
    deviceReconnectBaseMs * deviceReconnectAttempt,
    deviceReconnectMaxMs,
  );
  deviceReconnectTimer = setTimeout(async () => {
    deviceReconnectTimer = null;
    if (!deviceWanted) return;
    const cfg = deviceBridgeCfg;
    // 配置被清空（用户改了偏好）⇒ 无处可连，停在这里等下一次 start。
    if (!cfg) {
      deviceReconnectAttempt = 0;
      return;
    }
    try {
      await connectDeviceBridgeOnce(cfg);
      // 连上了：退避归零。下次若再断，重新从 base 开始 ——
      // 否则「昨天断十次、今天又断一次」要等 10×base 才有第一次重试。
      deviceReconnectAttempt = 0;
    } catch (e) {
      console.error("[device-bridge] reconnect failed: " + e.message);
      scheduleDeviceReconnect();
    }
  }, delay);
}

// startDeviceBridge 拆出的「只做一次连接」部分：重连走这里。
async function connectDeviceBridgeOnce(cfg) {
  const url = cfg.url || cfg.gateway || "";
  const token = cfg.apiKey || cfg.token || "";
  deviceBridgeAddr = url;
  deviceBridgeBound = false;
  // doConnectDeviceBridge 内部会建 ws、起心跳、并存进 deviceBridge。
  // 不用它的返回值：那层把结果写全局而非return，两边约定不一致时
  // 「读返回值」就得到 undefined，随后 ws.send 直接TypeError。
  await doConnectDeviceBridge(cfg);
  const ws = deviceBridge;
  if (!ws) {
    throw new Error("connect produced no bridge");
  }
  // host/cpus/mem 的推导包在try 里：它们在 ws.send 的实参里，而 send 之前
  // 抛错会被 startDeviceBridge 的 catch 吞成「连接失败」—— 实际连接是好的，
  // 表现为「反复重连但永远连不上」。拿不到主机信息不该阻断设备登记。
  let host = "";
  let cpus = 0;
  let totalMem = 0;
  let appVersion = "";
  let platform = "";
  let arch = "";
  let nodeVersion = "";
  let electronVersion = "";
  try {
    host = devOs.hostname() || "";
    cpus = devOs.cpus ? devOs.cpus().length : 0;
    totalMem = devOs.totalmem ? devOs.totalmem() : 0;
  } catch (e) {
    // 忽略：device_id 退化为 gui-local。
  }
  try {
    appVersion = app && app.getVersion ? app.getVersion() : "";
  } catch (e) {}
  // process 单独包：electron 主进程有，但判据的 vm 沙箱只注入了 Buffer/Math/
  // crypto —— 这里抛错会让 ws.send 之前就中断，被上层 catch 吞成
  // 「连接失败」，而实际连接是好的（表现为无限重连）。
  try {
    platform = process.platform || "";
    arch = process.arch || "";
    nodeVersion =
      (process.versions && process.versions.node) || "";
    electronVersion =
      (process.versions && process.versions.electron) || "";
  } catch (e) {}
  if (!deviceBridgeId) {
    deviceBridgeId =
      "gui-" + (host || "local").replace(/[^a-zA-Z0-9_-]/g, "_");
  }
  let authorized = false;
  try {
    authorized = !!(
      guiPrefs &&
      guiPrefs.deviceBridge &&
      guiPrefs.deviceBridge.authorized
    );
  } catch (e) {}
  ws.send({
    op: "hello",
    device: {
      device_id: deviceBridgeId,
      name: "HologramAgent GUI",
      kind: "computer",
      authorized: authorized,
      caps: [
        "status",
        "cmdrun",
        "deviceinfo",
        "cmdresult",
        "computeruse",
        "screensee",
        "clipboardsee",
        "clipboardsue",
        "speakeruse",
        "camerasue",
        "screensue",
        "omniparse",
      ],
      info: {
        hostname: host,
        platform: platform,
        arch: arch,
        os_release: "",
        node_version: nodeVersion,
        electron_version: electronVersion,
        version: appVersion,
        cpus: cpus,
        total_mem_bytes: totalMem,
      },
    },
  });
  ws.send({ op: "bind", device_id: deviceBridgeId, token });
  console.log("[device-bridge] connected as " + deviceBridgeId + " @ " + url);
}

// 真实连接：建 WS、起心跳、登记到全局。
//
// 「连上」与「开始对它发心跳」是同一件事的两半，放一函数里，重连路径
// 才不会漏掉心跳。
async function doConnectDeviceBridge(cfg) {
  const ws = await connectDeviceWS(
    cfg.url || cfg.gateway || "",
    cfg.apiKey || cfg.token || "",
    onDeviceMsg,
  );
  startDevicePing(ws.__sock);
  deviceBridge = ws;
  return ws;
}

// 发送 WS text 帧（客户端加掩码）
function sendDeviceFrame(sock, text) {
  const payload = Buffer.from(text, "utf8");
  const mask = crypto.randomBytes(4);
  const masked = Buffer.from(payload);
  for (let i = 0; i < masked.length; i++) masked[i] ^= mask[i % 4];
  const len = masked.length;
  let hdr;
  if (len < 126) {
    hdr = Buffer.from([0x81, 0x80 | len]);
  } else if (len < 65536) {
    hdr = Buffer.alloc(4);
    hdr[0] = 0x81;
    hdr[1] = 0x80 | 126;
    hdr.writeUInt16BE(len, 2);
  } else {
    hdr = Buffer.alloc(10);
    hdr[0] = 0x81;
    hdr[1] = 0x80 | 127;
    hdr.writeBigUInt64BE(BigInt(len), 2);
  }
  sock.write(Buffer.concat([hdr, mask, masked]));
}

// 发送 WS 二进制帧（0x2）——用于大体积数据（如录像）分块回传
function sendDeviceBinaryFrame(sock, buf) {
  const mask = crypto.randomBytes(4);
  const masked = Buffer.from(buf);
  for (let i = 0; i < masked.length; i++) masked[i] ^= mask[i % 4];
  const len = masked.length;
  let hdr;
  if (len < 126) {
    hdr = Buffer.from([0x82, 0x80 | len]);
  } else if (len < 65536) {
    hdr = Buffer.alloc(4);
    hdr[0] = 0x82;
    hdr[1] = 0x80 | 126;
    hdr.writeUInt16BE(len, 2);
  } else {
    hdr = Buffer.alloc(10);
    hdr[0] = 0x82;
    hdr[1] = 0x80 | 127;
    hdr.writeBigUInt64BE(BigInt(len), 2);
  }
  sock.write(Buffer.concat([hdr, mask, masked]));
}
// 通过设备通道以二进制分块回传大体积数据（如录像）。
// 协议（与服务端协商）：
//   cmd_data_start {op, req_id, kind, total, chunk_size, mime}  —— 文本帧
//   <N 个二进制帧 0x2>                                          —— video bytes
//   cmd_data_end   {op, req_id, status:ok|error, error?}        —— 文本帧
// 服务端按 req_id 聚合二进制块 → 存入 cmdresult，供 device_ctl_cmdresult 取回。
function sendDeviceDataChunked(reqId, kind, mime, buf) {
  if (!deviceBridge || !deviceBridge.send) {
    sendCmdResult(
      reqId,
      baseResult(reqId, "error", "", "device bridge not connected"),
    );
    return;
  }
  // 分块大小 8KB
  const CHUNK = 8192;
  // 取底层 socket 直接发二进制帧（deviceBridge.send 是文本封装）
  const total = buf.length;
  // 控制帧走文本协议（deviceBridge.send 封装）
  try {
    // 用 send 发文本帧控制头（start）
    deviceBridge.send({
      op: "cmd_data_start",
      req_id: reqId,
      kind: kind,
      mime: mime || "application/octet-stream",
      total: total,
      chunk_size: CHUNK,
    });
  } catch (e) {}
  // 二进制块：需要底层 socket。这里用包装的 raw send
  // sendDeviceRaw 通过 deviceBridge 的隐藏引用发二进制
  const rawSock = deviceBridge.__sock;
  if (rawSock && !rawSock.destroyed) {
    for (let off = 0; off < total; off += CHUNK) {
      sendDeviceBinaryFrame(rawSock, buf.slice(off, off + CHUNK));
    }
  } else {
    // 无 raw socket 时回退文本 base64（服务端老协议兜底）
    sendCmdResult(
      reqId,
      baseResult(
        reqId,
        "ok",
        "data:" + mime + ";base64," + buf.toString("base64"),
        "",
      ),
    );
    return;
  }
  try {
    deviceBridge.send({
      op: "cmd_data_end",
      req_id: reqId,
      status: "ok",
      total: total,
    });
  } catch (e) {}
}

// 处理网关 WS 消息：hello_ack/bind_ack/cmd 等。命令类型由服务端 cmd_type 指定（shell/homeagent）。
function onDeviceMsg(msg) {
  if (!msg || typeof msg !== "object") return;
  console.log(
    "[device-bridge] recv op=" +
      (msg.op || "") +
      " cmd_type=" +
      (msg.cmd_type || "") +
      " cmd=" +
      String(msg.command || msg.cmd || "").slice(0, 60),
  );
  const op = msg.op || "";
  if (op === "cmd") {
    // 客户端鉴权：未授权时拒绝执行（授权状态存本地 gui-prefs，服务端无法篡改）
    if (!guiPrefs?.deviceBridge?.authorized) {
      console.log("[device-bridge] cmd rejected (unauthorized): req=" + (msg.req_id || ""));
      sendCmdResult({
        op: "cmd_result",
        req_id: msg.req_id || msg.id || "",
        device_id: deviceBridgeId,
        status: "error",
        error: "设备未授权：请在设备 GUI 设置页开启远程控制授权",
      });
      return;
    }
    const command = msg.command || msg.cmd || "";
    const reqId = msg.req_id || msg.id || "";
    const cmdType = msg.cmd_type || "shell";
    if (!command) return;
    // 记录远控活动并刷新托盘菜单
    trayLastCmd = { cmd: command, at: Date.now(), result: "执行中…" };
    trayCmdCount++;
    try {
      rebuildTrayMenu();
    } catch (e) {}
    try {
      if (cmdType === "homeagent" || command.indexOf("homeagent-") === 0) {
        executeHomeagentCmd(command.replace(/^homeagent-/, ""), reqId);
      } else {
        executeShellCmd(command, reqId);
      }
    } catch (e) {
      console.log("[device-bridge] exec error: " + e.message);
    }
  } else if (op === "bind_ack") {
    // ★ bind 结果必须判 ok —— 与鸿蒙端（DeviceBridge.ets:209）对齐。
    //
    // 原实现与 Go 客户端同病：只打一行日志、不看 ok。服务端 bind 被拒时回
    // {"ok":false,"error":"bind rejected"} 并**关闭连接**，GUI 却既不报错也
    // 不重连，设备静默失联 —— TCP/WS 通但从未登记进网关，命令永远下发不到。
    //
    // 另：成功时服务端**不含 device 字段**，原日志靠 `msg.device || deviceBridgeId`
    // 兜底才显示得像成功，掩盖了「没有真的读 ok」这件事。
    const accepted = msg.ok === true;
    deviceBridgeBound = accepted;
    if (accepted) {
      console.log("[device-bridge] bind_ok device=" + deviceBridgeId);
    } else {
      deviceBridgeBindError = String(msg.error || "bind rejected");
      console.error(
        "[device-bridge] bind rejected: " +
          deviceBridgeBindError +
          "（设备令牌不匹配或设备未授权）",
      );
    }
    try {
      rebuildTrayMenu();
    } catch (e) {}
  } else if (op === "hello_ack") {
    console.log(
      "[device-bridge] hello_ack device=" + (msg.device || deviceBridgeId) + " online=" + msg.online,
    );
    try {
      rebuildTrayMenu();
    } catch (e) {}
  }
}

// 统一发送命令结果（回执 + 托盘更新）。兼容两种调用：sendCmdResult(resp) 或 sendCmdResult(reqId, resp)
function sendCmdResult(a, b) {
  const resp = b || a;
  console.log(
    "[device-bridge] send result op=cmd_result status=" +
      (resp.status || "") +
      " req=" +
      (resp.req_id || ""),
  );
  if (deviceBridge && deviceBridge.send) {
    try {
      deviceBridge.send(resp);
    } catch (e) {}
  }
  if (trayLastCmd) {
    trayLastCmd.result =
      resp.status === "ok"
        ? String(resp.output || "").slice(0, 40)
        : "错误: " + String(resp.error || "");
    try {
      rebuildTrayMenu();
    } catch (e) {}
  }
}

// 安全设置自绘光标的「忙碌」呼吸态（agentCursor 可能未就绪）
function agentCursorBusySafe(dispIdx, busy) {
  try {
    if (agentCursor && typeof agentCursor.agentCursorBusy === "function") {
      agentCursor.agentCursorBusy(busy, dispIdx);
    }
  } catch (e) {}
}

function baseResult(reqId, status, output, error) {
  return {
    op: "cmd_result",
    req_id: reqId,
    device_id: deviceBridgeId,
    status: status,
    output: output || "",
    error: error || "",
  };
}

// 读取设备命令执行配置（gui-prefs.deviceBridge.exec）
function getExecConfig() {
  try {
    const prefs = loadGuiPrefs();
    const db = prefs.deviceBridge || {};
    const ex = db.exec || {};
    return {
      cwd: ex.cwd || "",
      sandbox: ex.sandbox || "off", // off | home | box
      boxDir: ex.boxDir || "",
      timeout: parseInt(ex.timeout || "15", 10) || 15,
      maxBuffer: (parseInt(ex.maxBuffer || "8192", 10) || 8192) * 1024,
    };
  } catch (e) {
    return {
      cwd: "",
      sandbox: "off",
      boxDir: "",
      timeout: 15,
      maxBuffer: 8192 * 1024,
    };
  }
}

// shell 命令：按 exec 配置（cwd/沙箱/超时）执行
function executeShellCmd(command, reqId) {
  console.log(
    "[device-bridge] execShell cmd=" +
      String(command).slice(0, 60) +
      " req=" +
      reqId,
  );
  const cp = require("child_process");
  const ex = getExecConfig();
  if (!argsSafe(command)) {
    sendCmdResult(
      reqId,
      baseResult(reqId, "denied", "", "command not allowed"),
    );
    return;
  }
  // 沙箱策略：home=锁定用户主目录; box=锁定指定目录; off=cwd 或默认
  const os = require("os");
  let cwd = ex.cwd || os.homedir() || process.cwd();
  if (ex.sandbox === "home") cwd = os.homedir() || cwd;
  else if (ex.sandbox === "box" && ex.boxDir) cwd = ex.boxDir;
  else if (ex.sandbox === "box" && !ex.boxDir) {
    sendCmdResult(
      reqId,
      baseResult(reqId, "denied", "", "sandbox=box 需配置 boxDir"),
    );
    return;
  }
  cp.exec(
    command,
    {
      cwd: cwd,
      timeout: ex.timeout * 1000,
      maxBuffer: ex.maxBuffer,
      env: process.env,
    },
    (err, stdout, stderr) => {
      sendCmdResult(
        reqId,
        baseResult(
          reqId,
          err ? "error" : "ok",
          (stdout || "") + (stderr || ""),
          err ? err.message : "",
        ),
      );
    },
  );
}

// homeagent 内置能力分发（与服务端 device_ctl_cmdrun 的 homeagent-* 对齐）
function executeHomeagentCmd(capability, reqId) {
  const name = String(capability || "")
    .trim()
    .split(/[ >\n]/)[0];
  switch (name) {
    case "camerasue": {
      // 摄像头：camerasue=抓拍单张；camerasue <秒>=录制 N 秒视频
      // 平台分支：Windows=dshow(设备名自动探测)，macOS=avfoundation，Linux=v4l2
      const argStr = String(capability || "")
        .replace(/^camerasue/, "")
        .trim();
      const durMatch = /^\d+$/.test(argStr) ? parseInt(argStr, 10) : 0;
      const isVideo = durMatch > 0;
      const cp = require("child_process");
      const os = require("os");
      const path = require("path");
      const fs = require("fs");
      // 探测平台可用的 ffmpeg 输入参数（缓存结果避免重复探测）
      let camInput = null;
      function resolveCameraInput(cb) {
        if (camInput) return cb(camInput);
        const plat = process.platform;
        if (plat === "win32") {
          // dshow：先枚举设备名取第一个视频设备
          cp.execFile(
            "ffmpeg",
            ["-hide_banner", "-list_devices", "true", "-f", "dshow", "-i", "video= dummy"],
            { timeout: 8000 },
            (err, _so, se) => {
              const out = String(se || "");
              const m = out.match(/"([^"]+)"\s*\((?:video|默认)|"([^"]+)"[\s\S]{0,200}?\(video/)
                || out.match(/"([^"]+)"[^\n]*\(video/i);
              const name = m ? (m[1] || m[2]) : null;
              if (name) {
                camInput = { pre: ["-f", "dshow", "-i", "video=" + name] };
              } else {
                camInput = { pre: ["-f", "dshow", "-i", "video=USB Camera"] }; // 常见默认名兑底
              }
              cb(camInput);
            },
          );
          return;
        }
        if (plat === "darwin") {
          camInput = { pre: ["-f", "avfoundation", "-i", "0:0"] }; // 默认摄像头
          return cb(camInput);
        }
        camInput = { pre: ["-f", "v4l2", "-i", "/dev/video0"] }; // Linux
        return cb(camInput);
      }
      resolveCameraInput((cam) => {
      if (isVideo) {
        // 录像：ffmpeg 录 N 秒 mp4 到临时文件
        const outFile = path.join(os.tmpdir(), "ha_cam_" + Date.now() + ".mp4");
        const args = [
          ...cam.pre,
          "-t",
          String(durMatch),
          "-pix_fmt",
          "yuv420p",
        ];
        if (process.platform !== "win32") {
          args.push("-c:v", "libx264"); // Windows dshow→mp4 由扩展名驱动原生编码器
        }
        args.push("-f", "mp4", outFile);
        cp.execFile(
          "ffmpeg",
          args,
          { timeout: (durMatch + 15) * 1000, maxBuffer: 64 * 1024 * 1024 },
          (err) => {
            if (err || !fs.existsSync(outFile)) {
              sendCmdResult(
                reqId,
                baseResult(
                  reqId,
                  "error",
                  "",
                  "camera record failed: " + (err ? err.message : "no file"),
                ),
              );
              return;
            }
            try {
              const data = fs.readFileSync(outFile);
              fs.unlinkSync(outFile);
              // 录像以二进制分块经设备通道回传（协议 cmd_data_start/二进制帧/cmd_data_end）
              sendDeviceDataChunked(reqId, "camera_video", "video/mp4", data);
            } catch (e2) {
              sendCmdResult(
                reqId,
                baseResult(
                  reqId,
                  "error",
                  "",
                  "camera record read failed: " + e2.message,
                ),
              );
            }
          },
        );
        return;
      }
      // 抓拍单张 jpeg
        const args = [
          ...cam.pre,
          "-frames:v",
          "1",
          "-f",
          "image2pipe",
          "-vcodec",
          "mjpeg",
          "pipe:1",
        ];
      cp.execFile(
        "ffmpeg",
        args,
        { timeout: 10000, maxBuffer: 8 * 1024 * 1024 },
        (err, stdout) => {
          if (err || !stdout) {
            sendCmdResult(
              reqId,
              baseResult(
                reqId,
                "error",
                "",
                "camera capture failed: " + (err ? err.message : "no data"),
              ),
            );
            return;
          }
          const b64 = Buffer.from(stdout).toString("base64");
          sendCmdResult(
            reqId,
            baseResult(reqId, "ok", "data:image/jpeg;base64," + b64, ""),
          );
        },
      );
      }); // resolveCameraInput 回调闭合
      return;
    }
    case "screensue": {
      // 在配置的目标屏幕上拉起独立窗口显示内容（支持文字 / HTML）
      // 默认 5 秒自动关闭；命令可带 [秒] 指定时长，0 = 永不超时（常驻，用户手动关闭）
      var duration = 5;
      var raw = String(capability || "")
        .replace(/^screensue/, "")
        .trim();
      // 配置: gui-prefs.deviceBridge.screensueDisplay / screensueDuration
      var dispIdx = 0;
      try {
        const prefs = loadGuiPrefs();
        const db = prefs.deviceBridge || {};
        dispIdx = parseInt(db.screensueDisplay || "0", 10) || 0;
        const cfgDur = db.screensueDuration;
        if (cfgDur === undefined || cfgDur === null || cfgDur === "") {
          duration = 5; // 未配置时默认 5 秒
        } else {
          duration = parseInt(cfgDur, 10);
          if (!Number.isFinite(duration) || duration < 0) duration = 5;
        }
      } catch (e) {}
      // 参数支持 "screensue [时长秒] 内容"：首个纯数字 token 作为时长（0=永不超时）
      const tokens = raw.split(/\s+/);
      if (tokens.length > 1 && /^\d+$/.test(tokens[0])) {
        duration = parseInt(tokens[0], 10);
        raw = tokens.slice(1).join(" ");
      }
      const display =
        (screen.getAllDisplays() || [])[dispIdx] || screen.getAllDisplays()[0];
      const area = display
        ? display.workArea
        : { x: 0, y: 0, width: 800, height: 480 };
      const w = Math.min(parseInt(area.width || 800, 10) - 40, 900);
      const h = Math.min(parseInt(area.height || 600, 10) - 40, 560);
      const html = /<\/?[a-z][\s\S]*>/i.test(raw)
        ? raw
        : "<div style='font-family:sans-serif;display:flex;flex-direction:column;justify-content:center;align-items:center;height:100%;padding:24px;box-sizing:border-box'><h1 style='margin:0 0 16px;color:#ff7fac'>HologramAgent</h1><p style='font-size:16px;line-height:1.6;white-space:pre-wrap;word-break:break-all'>" +
          String(raw || "HologramAgent 远程屏幕提示").replace(/</g, "&lt;") +
          "</p></div>";
      try {
        if (screensueWin && !screensueWin.isDestroyed()) screensueWin.destroy();
        screensueWin = new BrowserWindow({
          x: area.x + 20,
          y: area.y + 20,
          width: w,
          height: h,
          alwaysOnTop: true,
          frame: false,
          resizable: true,
          title: "HologramAgent · screensue",
          webPreferences: { nodeIntegration: false, contextIsolation: true },
        });
        screensueWin.setAlwaysOnTop(true, "screen-saver");
        screensueWin.loadURL(
          "data:text/html;charset=utf-8," +
            encodeURIComponent(
              "<html><head><meta charset='utf-8'><style>body{margin:0;background:#0b1020;color:#eef1f8}</style></head><body>" +
                html +
                "</body></html>",
            ),
        );
        const closeHint =
          "<div style='position:fixed;top:8px;right:12px;font-size:12px;color:#77809a;background:rgba(20,26,44,.7);padding:2px 10px;border-radius:10px;cursor:pointer' onclick='window.close()'>× 关闭</div>";
        screensueWin.webContents.on("did-finish-load", () => {
          try {
            screensueWin.webContents.executeJavaScript(
              "document.body.insertAdjacentHTML('beforeend', '" +
                closeHint.replace(/'/g, "\\'") +
                "');",
            );
          } catch (e2) {}
        });
        // 显示时长：duration>0 时定时自动关闭
        if (duration > 0) {
          setTimeout(() => {
            try {
              if (screensueWin && !screensueWin.isDestroyed()) {
                screensueWin.destroy();
                screensueWin = null;
              }
            } catch (e3) {}
          }, duration * 1000);
        }
        sendCmdResult(
          reqId,
          baseResult(
            reqId,
            "ok",
            "screensue shown on display " +
              dispIdx +
              (duration > 0
                ? " for " + duration + "s"
                : " (persistent until closed)") +
              ": " +
              raw.slice(0, 80),
            "",
          ),
        );
      } catch (e) {
        sendCmdResult(
          reqId,
          baseResult(reqId, "error", "", "screensue failed: " + e.message),
        );
      }
      return;
    }
    case "screensee": {
      // screensee：截取指定屏幕并返回 jpeg base64 data URL（供 agent 视觉模型看屏）
      // 屏幕索引沿用 gui-prefs.deviceBridge.screensueDisplay，与 screensue 同源配置。
      try {
        const prefs = loadGuiPrefs();
        const db = prefs.deviceBridge || {};
        let dispIdx = parseInt(db.screensueDisplay || "0", 10) || 0;
        desktopCapturer
          .getSources({
            types: ["screen"],
            thumbnailSize: { width: 1920, height: 1080 },
            fetchWindowIcons: false,
          })
          .then((sources) => {
            if (!sources || sources.length === 0) {
              sendCmdResult(
                reqId,
                baseResult(reqId, "error", "", "screensee: no screen source"),
              );
              return;
            }
            // 按配置索引选屏，越界回退到主屏(0)
            if (dispIdx >= sources.length) dispIdx = 0;
            const src = sources[dispIdx];
            const thumb = src.thumbnail;
            if (!thumb || thumb.isEmpty()) {
              sendCmdResult(
                reqId,
                baseResult(
                  reqId,
                  "error",
                  "",
                  "screensee: empty thumbnail for screen " + dispIdx,
                ),
              );
              return;
            }
            const jpeg = thumb.toJPEG(80); // 质量 80，平衡清晰度与传输体积
            const b64 = Buffer.from(jpeg).toString("base64");
            console.log(
              "[device-bridge] screensee screen=" +
                dispIdx +
                " bytes=" +
                jpeg.length,
            );
            sendCmdResult(
              reqId,
              baseResult(
                reqId,
                "ok",
                "data:image/jpeg;base64," + b64,
                "",
              ),
            );
          })
          .catch((e) => {
            sendCmdResult(
              reqId,
              baseResult(reqId, "error", "", "screensee failed: " + e.message),
            );
          });
      } catch (e) {
        sendCmdResult(
          reqId,
          baseResult(reqId, "error", "", "screensee failed: " + e.message),
        );
      }
      return;
    }
    case "speakeruse": {
      // 音频播报：speakeruse <文字> 用语音朗读（TTS）。
      // 依赖系统 TTS；Linux 用 espeak/festival，macOS 用 say，Windows 用 PowerShell SAPI。
      const text = String(capability || "")
        .replace(/^speakeruse/, "")
        .trim();
      const cp = require("child_process");
      const platform = process.platform;
      let cmd = null;
      let args = [];
      if (platform === "linux") {
        // 优先尝试 espeak，其次 festival
        if (cp.spawnSync("which", ["espeak"]).status === 0) {
          cmd = "espeak";
          args = [text];
        } else if (cp.spawnSync("which", ["festival"]).status === 0) {
          cmd = "bash";
          args = [
            "-c",
            "echo '" +
              String(text).replace(/'/g, "'\\''") +
              "' | festival --tts",
          ];
        } else {
          sendCmdResult(
            reqId,
            baseResult(
              reqId,
              "error",
              "",
              "speakeruse: no TTS engine (espeak/festival)",
            ),
          );
          return;
        }
      } else if (platform === "darwin") {
        cmd = "say";
        args = [text];
      } else if (platform === "win32") {
        cmd = "powershell";
        args = [
          "-Command",
          "(New-Object -ComObject SAPI.SpVoice).Speak('" +
            String(text).replace(/'/g, "''") +
            "')",
        ];
      } else {
        sendCmdResult(
          reqId,
          baseResult(reqId, "error", "", "speakeruse: unsupported platform"),
        );
        return;
      }
      if (!text) {
        sendCmdResult(
          reqId,
          baseResult(reqId, "error", "", "speakeruse: empty text"),
        );
        return;
      }
      cp.execFile(
        cmd,
        args,
        { timeout: 30000, maxBuffer: 1024 * 1024 },
        (err) => {
          sendCmdResult(
            reqId,
            baseResult(
              reqId,
              err ? "error" : "ok",
              err ? "" : "spoken: " + text.slice(0, 60),
              err ? err.message : "",
            ),
          );
        },
      );
      return;
    }
    case "computeruse": {
      // computeruse：agent 控制鼠标/键盘在目标屏幕操作（点击/移动/滚动/按键/输入）
      // 参数：homeagent-computeruse {"x":100,"y":200,"action":"click","button":"left"}
      //   action: click/doubleclick/rightclick/move/scroll/type/keypress
      //   x/y: 相对屏幕左上角(px)；scroll 用 dx/dy; type 用 text
      const cp = require("child_process");
      const platform = process.platform;
      const raw = String(capability || "")
        .replace(/^computeruse/, "")
        .trim();
      // 解析 JSON 参数
      let params = {};
      const jsonM = raw.match(/\{[\s\S]*\}/);
      if (jsonM) {
        try {
          params = JSON.parse(jsonM[0]);
        } catch (e) {
          // 兼容非标准 JSON: {x:500,y:300} → 补双引号
          try {
            const fixed = jsonM[0]
              .replace(/([{,]\s*)([a-zA-Z_][a-zA-Z0-9_]*)\s*:/g, '$1"$2":')  // 给 key 加引号
              .replace(/:\s*'([^']*)'/g, ':"$1"');  // 单引号值转双引号
            params = JSON.parse(fixed);
          } catch (e2) {
            sendCmdResult(
              reqId,
              baseResult(
                reqId,
                "error",
                "",
                "computeruse: invalid JSON params: " + e.message,
              ),
            );
            return;
          }
        }
      } else {
        // 支持 "x y action" 简写
        const t = raw.split(/\s+/).filter(Boolean);
        params.x = parseFloat(t[0]);
        params.y = parseFloat(t[1]);
        params.action = t[2] || "click";
      }
      const action = params.action || "click";

      // ★ 注入模式：sendinput（默认）/ overlay / real
      //
      //   sendinput  SendInput 注入到系统输入队列。等价真实硬件事件，
      //              Chromium/Electron/游戏等自绘界面都能接收 —— 可靠。
      //              代价：会移动用户的真实光标，故操作前先等用户停手。
      //              参考 Pal-AI-Lab/Coopanion 的 cortico-world-cua。
      //   overlay    PostMessage 直投窗口句柄。用户鼠标纹丝不动，但自绘
      //              界面常忽略合成消息 ⇒ 只能操作原生 Win32 程序。
      //   real       同上但用旧代码路径（保留以兼容）。
      //
      // 默认 sendinput：用户要求「可靠 + 用户正在操作则暂缓」。
      let cursorMode = "sendinput";
      try {
        const _p = loadGuiPrefs();
        const _ac = (_p && _p.deviceBridge && _p.deviceBridge.agentCursor) || {};
        if (_ac.mode === "overlay" || _ac.mode === "real") cursorMode = _ac.mode;
      } catch (e) {}
      const injOk = agentInject.available();
      const useOverlay = cursorMode === "overlay" && injOk;
      const useSendInput = cursorMode === "sendinput" && injOk;
      // 用户活动暂缓：等用户停手再注入（毫秒）。0 = 不等待。
      let deferMs = 3000;
      try {
        const _p2 = loadGuiPrefs();
        const _ac2 = (_p2 && _p2.deviceBridge && _p2.deviceBridge.agentCursor) || {};
        if (typeof _ac2.deferMs === "number") deferMs = Math.max(0, Math.min(10000, _ac2.deferMs));
      } catch (e) {}

      // 把内部参数提到 action 分支之前共用
      const cuNumber = (v, dflt) => {
        const n = parseFloat(v);
        return Number.isFinite(n) ? n : dflt;
      };
      // 跨平台输入模拟：Linux=xdotool / macOS=cliclick(或用osascript) / Windows=PowerShell user32
      const os_ = platform === "win32" ? "win32" : platform === "darwin" ? "darwin" : "linux";
      let tool = null; // {cmd, args, shell}
      if (os_ === "linux") {
        if (cp.spawnSync("which", ["xdotool"]).status === 0) tool = { cmd: "xdotool", shell: false };
      } else if (os_ === "darwin") {
        if (cp.spawnSync("which", ["cliclick"]).status === 0) tool = { cmd: "cliclick", shell: false };
      } // win32 用 PowerShell 内联，下面单独处理
      if (os_ === "linux" && !tool) {
        sendCmdResult(reqId, baseResult(reqId, "error", "", "computeruse: xdotool not installed"));
        return;
      }
      if (os_ === "darwin" && !tool) {
        sendCmdResult(reqId, baseResult(reqId, "error", "", "computeruse: cliclick not installed"));
        return;
      }
      // 目标屏幕左上角原点偏移：沿用 screensueDisplay；坐标相对该屏幕
      let dispIdx = 0;
      try {
        const prefs = loadGuiPrefs();
        dispIdx = parseInt((prefs.deviceBridge || {}).screensueDisplay || "0", 10) || 0;
      } catch (e) {}
      const displays = screen.getAllDisplays() || [];
      if (dispIdx >= displays.length) dispIdx = 0;
      const disp = displays[dispIdx];
      const ox = disp ? (disp.bounds.x || 0) : 0;
      const oy = disp ? (disp.bounds.y || 0) : 0;

      // ★ 坐标空间：Win32 坐标与截图一样，都是**物理像素**，不要换算。
      //
      // 实测（Electron 主进程内，cmd/gui/probe2）：
      //   screen API (DIP)      : 1260 x 840   scaleFactor=2
      //   GetSystemMetrics      : 2520 x 1680  ← Win32 用的是物理像素
      //   SetCursorPos(1260,840) -> GetCursorPos 读回 (1260,840)
      //                            而 Electron 读回 DIP (630,420)
      //   比值 = 2.000
      //
      // 原因是 Electron 主进程默认 per-monitor DPI-aware：这种感知下
      // Win32 坐标 API 不做虚拟化，直接就是物理像素。
      //
      // ⚠ 本仓曾在这一段加过 `/ scaleFactor` 换算，理由是「SetCursorPos
      //   期望 DIP」——那个前提是错的（那是 DPI-*unaware* 进程的行为）。
      //   加了之后反而把原本正确的点击改坏：150% 缩放屏上点 (600,400)
      //   会被送到 (400,267)，越靠右下偏得越远。现已改回直接使用。
      //
      // 保留 physical 开关：多数截图工具给物理像素（默认），若调用方
      // 明确声明坐标已是 DIP（physical:false），再乘回 scaleFactor。
      const scale = disp && disp.scaleFactor ? disp.scaleFactor : 1;
      const rawX = parseFloat(params.x) || 0;
      const rawY = parseFloat(params.y) || 0;
      const physical = params.physical !== false;
      const absX = Math.round(ox + (physical ? rawX : rawX * scale));
      const absY = Math.round(oy + (physical ? rawY : rawY * scale));

      // === 模式 sendinput：SendInput 注入 + 用户在操作则暂缓 ===
      //
      // SendInput 会动真实光标，因此注入前先等用户停手（deferMs 上限）。
      // 等不到就跳过本次操作并明确报错 —— 宁可失败，也不打断用户。
      if (os_ === "win32" && useSendInput) {
        try {
          const busy = () => agentCursorBusySafe(dispIdx, true);
          const idle = () => agentCursorBusySafe(dispIdx, false);
          const fin = (ok, msg) => {
            idle();
            sendCmdResult(reqId, baseResult(reqId, ok ? "ok" : "error", msg, ok ? "" : msg));
          };
          const fail = (m) => fin(false, m);

          // 等待用户停手
          //
          // 注意：executeHomeagentCmd 是**同步函数**，不能 await。
          // 故改为「同步查询 + 轮询回调」：先看用户是否在操作；若是，
          // 定时轮询到空闲或超时再继续；全程不阻塞主进程。
          const startDefer = (cb) => {
            if (deferMs <= 0 || !agentInject.userIsActive(1200)) return cb(true);
            const before = agentInject.idleMs();
            const t0 = Date.now();
            const tick = () => {
              if (!agentInject.userIsActive(800)) return cb(true);
              if (Date.now() - t0 >= deferMs) {
                idle();
                sendCmdResult(
                  reqId,
                  baseResult(
                    reqId,
                    "error",
                    "",
                    "用户正在操作（已等待 " + deferMs + "ms，开始时空闲仅 " + before +
                      "ms）。为避免抢走光标已跳过本次操作。可在设置里调整 agentCursor.deferMs。",
                  ),
                );
                return cb(false);
              }
              setTimeout(tick, 120);
            };
            tick();
          };

          const runAction = () => {
          const btnName = params.button === "right" ? "right" : params.button === "middle" ? "middle" : "left";
          const toX = () => Math.round(ox + (physical ? (parseFloat(params.tox || params.tx) || 0) : (parseFloat(params.tox || params.tx) || 0) * scale));
          const toY = () => Math.round(oy + (physical ? (parseFloat(params.toy || params.ty) || 0) : (parseFloat(params.toy || params.ty) || 0) * scale));

          // 自绘光标同步到目标位置（视觉指示 agent 在做什么）
          const needsXY = ["click", "doubleclick", "rightclick", "middleclick", "tripleclick",
            "mousedown", "mouseup", "move", "hover", "drag", "scroll"].includes(action);
          if (needsXY) agentCursor.agentCursorMove(absX, absY, dispIdx);
          busy();

          switch (action) {
            case "move":
            case "hover": {
              const r = agentInject.si.move(absX, absY);
              return fin(r.ok, "sendinput " + action + " @ (" + absX + "," + absY + ")");
            }
            case "click":
            case "rightclick":
            case "middleclick":
            case "doubleclick":
            case "tripleclick":
            case "mousedown":
            case "mouseup": {
              const btn = action === "rightclick" ? "right" : action === "middleclick" ? "middle" : btnName;
              const act = action === "rightclick" || action === "middleclick" ? "click" : action;
              const r = agentInject.si.button(absX, absY, act, btn);
              if (act === "click" || act === "doubleclick" || act === "tripleclick") {
                agentCursor.agentCursorClick(dispIdx);
              }
              return fin(r.ok, "sendinput " + action + " @ (" + absX + "," + absY + ")");
            }
            case "drag": {
              const steps = Math.max(1, Math.min(60, parseInt(params.steps || 12, 10) || 12));
              const r = agentInject.si.drag(absX, absY, toX(), toY(), btnName, steps);
              agentCursor.agentCursorMove(toX(), toY(), dispIdx);
              agentCursor.agentCursorClick(dispIdx);
              return fin(r.ok, "sendinput drag (" + absX + "," + absY + ") -> (" + toX() + "," + toY() + ")");
            }
            case "scroll": {
              const dy = cuNumber(params.dy !== undefined ? params.dy : params.y, 0);
              const dx = cuNumber(params.dx !== undefined ? params.dx : params.x, 0);
              // GUI 侧约定：dy 正=向上。SendInput 的 WHEEL 正值即向上，直接传。
              const r = agentInject.si.scroll(dy, dx);
              return fin(r.ok, "sendinput scroll dx=" + dx + " dy=" + dy);
            }
            case "keypress":
            case "hotkey":
            case "combo": {
              const keys = String(params.keys || params.key || params.text || "");
              const r = agentInject.si.key(keys);
              return fin(r.ok, "sendinput keypress " + keys);
            }
            case "type": {
              const r = agentInject.si.type(String(params.text || ""));
              return fin(r.ok, "sendinput typed " + r.chars + " chars");
            }
            case "wait":
            case "sleep": {
              const ms = Math.max(0, Math.min(10000, parseInt(params.ms || params.duration || 500, 10) || 500));
              idle();
              setTimeout(() => sendCmdResult(reqId, baseResult(reqId, "ok", "sendinput waited " + ms + "ms", "")), ms);
              return;
            }
            case "display": {
              const info = displays.map((d, i) => ({
                index: i, bounds: d.bounds, scaleFactor: d.scaleFactor,
                primary: d.id === screen.getPrimaryDisplay().id,
              }));
              const v = agentInject.virtualScreen();
              idle();
              sendCmdResult(reqId, baseResult(reqId, "ok",
                "displays: " + JSON.stringify(info) +
                " | virtualScreen: " + JSON.stringify(v) +
                " | inputMode: sendinput | coordinateSpace: physical-pixel", ""));
              return;
            }
            default:
              return fail("unknown action " + action);
          }
          };
          startDefer((proceed) => { if (proceed) runAction(); });
          // ★ 必须在这里 return。
          //
          // runAction 里的 `return fin(...)` 只退出 runAction，不会退出
          // executeHomeagentCmd。少了这句，执行完 sendinput 后会继续往下
          // fall through 到旧的 real 鼠标路径 —— 同一条命令被彻底执行两遍
          // （实测：8 条命令回了 16 个 cmd_result，且真光标被额外抢一次）。
          return;
        } catch (e) {
          try { agentCursorBusySafe(dispIdx, false); } catch (e2) {}
          console.error("[sendinput] failed: " + e.message);
          sendCmdResult(reqId, baseResult(reqId, "error", "", "sendinput: " + e.message));
          return;
        }
      }

      // === 模式 A：后台注入（PostMessage 到目标窗口），不动用户真实鼠标 ===
      if (os_ === "win32" && useOverlay) {
        const btnName = params.button === "right" ? "right" : params.button === "middle" ? "middle" : "left";
        const beginBusy = () => agentCursor.agentCursorBusy(true, dispIdx);
        const endBusy = () => agentCursor.agentCursorBusy(false, dispIdx);
        const done = (ok, msg) => {
          endBusy();
          sendCmdResult(reqId, baseResult(reqId, ok ? "ok" : "error", msg, ok ? "" : msg));
        };
        const fail = (msg) => done(false, "agent-overlay: " + msg);
        // 位置类 action 之外（type/keypress 等）不需要先移动光标
        const needsXY = ["click", "doubleclick", "rightclick", "middleclick", "tripleclick",
          "mousedown", "mouseup", "move", "hover", "drag", "scroll"].includes(action);
        try {
          if (needsXY) {
            const h = agentInject.windowAtPoint(absX, absY);
            if (!h) {
              return fail(
                "该坐标下没有窗口（WindowFromPoint 返回空）。" +
                "后台注入只能作用于可见窗口，请确认 screensueDisplay 与截图用的屏一致。",
              );
            }
            // 自绘光标移到目标位置（不动真实鼠标）
            agentCursor.agentCursorMove(absX, absY, dispIdx);
          }
          beginBusy();
          switch (action) {
            case "move":
            case "hover": {
              const r = agentInject.mouse(absX, absY, "move", { button: btnName });
              agentCursor.setLastHwnd(r.hwnd);
              return done(r.ok, "agent-overlay " + action + " @ (" + absX + "," + absY + ") hwnd=" + agentInject.hwndStr(r.hwnd));
            }
            case "click": {
              const r = agentInject.mouse(absX, absY, "click", { button: btnName });
              agentCursor.setLastHwnd(r.hwnd);
              agentCursor.agentCursorClick(dispIdx);
              return done(r.ok, "agent-overlay click @ (" + absX + "," + absY + ") hwnd=" + agentInject.hwndStr(r.hwnd));
            }
            case "rightclick": {
              const r = agentInject.mouse(absX, absY, "click", { button: "right" });
              agentCursor.setLastHwnd(r.hwnd);
              agentCursor.agentCursorClick(dispIdx);
              return done(r.ok, "agent-overlay rightclick @ (" + absX + "," + absY + ") hwnd=" + agentInject.hwndStr(r.hwnd));
            }
            case "middleclick": {
              const r = agentInject.mouse(absX, absY, "click", { button: "middle" });
              agentCursor.setLastHwnd(r.hwnd);
              agentCursor.agentCursorClick(dispIdx);
              return done(r.ok, "agent-overlay middleclick @ (" + absX + "," + absY + ") hwnd=" + agentInject.hwndStr(r.hwnd));
            }
            case "doubleclick": {
              const r = agentInject.mouse(absX, absY, "doubleclick", { button: btnName });
              agentCursor.agentCursorClick(dispIdx);
              return done(r.ok, "agent-overlay doubleclick @ (" + absX + "," + absY + ")");
            }
            case "tripleclick": {
              const r = agentInject.tripleClick(absX, absY);
              agentCursor.setLastHwnd(r.hwnd);
              agentCursor.agentCursorClick(dispIdx);
              return done(r.ok, "agent-overlay tripleclick @ (" + absX + "," + absY + ")");
            }
            case "mousedown": {
              const r = agentInject.mouse(absX, absY, "mousedown", { button: btnName });
              return done(r.ok, "agent-overlay mousedown(" + btnName + ")");
            }
            case "mouseup": {
              const r = agentInject.mouse(absX, absY, "mouseup", { button: btnName });
              return done(r.ok, "agent-overlay mouseup(" + btnName + ")");
            }
            case "drag": {
              const toX = Math.round(ox + (physical ? (parseFloat(params.tox || params.tx) || 0) : (parseFloat(params.tox || params.tx) || 0) * scale));
              const toY = Math.round(oy + (physical ? (parseFloat(params.toy || params.ty) || 0) : (parseFloat(params.toy || params.ty) || 0) * scale));
              agentCursor.agentCursorMove(absX, absY, dispIdx);
              const steps = Math.max(1, Math.min(60, parseInt(params.steps || 12, 10) || 12));
              // 拖拽过程中让自绘光标跟着走，视觉上与真实拖拽一致
              const r = agentInject.drag(absX, absY, toX, toY, { button: btnName, steps });
              agentCursor.setLastHwnd(r.hwnd);
              agentCursor.agentCursorMove(toX, toY, dispIdx);
              agentCursor.agentCursorClick(dispIdx);
              return done(r.ok, "agent-overlay drag (" + absX + "," + absY + ") -> (" + toX + "," + toY + ")");
            }
            case "scroll": {
              const dy = cuNumber(params.dy !== undefined ? params.dy : params.y, 0);
              const dx = cuNumber(params.dx !== undefined ? params.dx : params.x, 0);
              const r = agentInject.scroll(absX, absY, dy, dx);
              return done(r.ok, "agent-overlay scroll dx=" + dx + " dy=" + dy);
            }
            case "keypress":
            case "hotkey":
            case "combo": {
              const keys = String(params.keys || params.key || params.text || "");
              // 无坐标语义：发给「当前光标所在窗口」，即上次 agent 操作过的窗口
              const h = agentInject.windowAtPoint(absX, absY) || agentCursor.lastHwnd;
              if (!h) {
                return fail("keypress 需要先有点击类操作确定目标窗口（当前无法确定目标窗口）");
              }
              const r = agentInject.keyTap(h, keys);
              return done(r.ok, "agent-overlay keypress " + keys + " -> hwnd=" + agentInject.hwndStr(h));
            }
            case "type": {
              const t = String(params.text || "");
              const h = agentInject.windowAtPoint(absX, absY) || agentCursor.lastHwnd;
              if (!h) return fail("type 需要先有点击类操作确定目标窗口");
              const r = agentInject.typeText(h, t);
              return done(r.ok, "agent-overlay typed " + r.chars + " chars -> hwnd=" + agentInject.hwndStr(h));
            }
            case "wait":
            case "sleep": {
              const ms = Math.max(0, Math.min(10000, parseInt(params.ms || params.duration || 500, 10) || 500));
              endBusy();
              setTimeout(() => sendCmdResult(reqId, baseResult(reqId, "ok", "agent-overlay waited " + ms + "ms", "")), ms);
              return;
            }
            case "display": {
              const info = displays.map((d, i) => ({
                index: i, bounds: d.bounds, scaleFactor: d.scaleFactor,
                primary: d.id === screen.getPrimaryDisplay().id,
              }));
              endBusy();
              sendCmdResult(reqId, baseResult(reqId, "ok",
                "displays: " + JSON.stringify(info) +
                " | coordinateSpace: physical-pixel (Win32 与截图像素一致, 无需换算)", ""));
              return;
            }
            default:
              return fail("unknown action " + action);
          }
        } catch (e) {
          endBusy();
          console.error("[agent-overlay] failed: " + e.message);
          sendCmdResult(reqId, baseResult(reqId, "error", "", "agent-overlay: " + e.message));
          return;
        }
      }

      // === Windows: 用 koffi 直接调用 user32.dll，不依赖 PowerShell C# 编译 ===
      if (os_ === "win32") {
        try {
          const koffi = require("koffi");
          const user32 = koffi.load("user32.dll");
          const SetCursorPos = user32.func("bool SetCursorPos(int x, int y)");
          const mouse_event = user32.func("void mouse_event(uint dwFlags, uint dx, uint dy, uint dwData, uint dwExtraInfo)");

          const BTN = { left: [0x0002, 0x0004], middle: [0x0020, 0x0040], right: [0x0008, 0x0010] };
          const btn = params.button === "right" ? "right" : params.button === "middle" ? "middle" : "left";
          const btnDown = BTN[btn][0];
          const btnUp = BTN[btn][1];
          // MOUSEEVENTF_WHEEL=0x0800, HWHEEL=0x0100；WHEEL_DELTA=120
          const WHEEL = 0x0800;
          const HWHEEL = 0x0100;
          // 修饰键虚拟键码（VK_*）
          const VK = {
            ctrl: 0x11, control: 0x11, alt: 0x12, shift: 0x10, win: 0x5b, meta: 0x5b, super: 0x5b,
            enter: 0x0d, return: 0x0d, tab: 0x09, esc: 0x1b, escape: 0x1b,
            space: 0x20, backspace: 0x08, delete: 0x2e, del: 0x2e,
            up: 0x26, down: 0x28, left: 0x25, right: 0x27,
            home: 0x24, end: 0x23, pageup: 0x21, pagedown: 0x22,
          };
          // 修饰键的 keybd_event 标志（EXTENDEDKEY 表示右侧/小键盘扩展键）
          const VK_EXTENDED = new Set([0x21,0x22,0x23,0x24,0x25,0x26,0x27,0x28,0x2e,0x5b,0x5c,0x6f,0x74]);

          // keybd_event 全局声明（拿得到 user32 后用）
          //
          // ★ 类型名必须是 uint8：koffi 不认 Win32 头文件里的 `byte`，
          //   实测 `void keybd_event(byte bVk, ...)` 抛
          //   "Unknown or invalid type name 'byte'"，而 uint8 可正常解析。
          //   这个 bug 曾让组合键（hotkey/keypress）整体报
          //   "keybd_event unavailable" —— 单键 click 不受影响，所以很容易漏掉。
          let keybd_event = null;
          try {
            keybd_event = user32.func(
              "void keybd_event(uint8 bVk, uint8 bScan, uint32 dwFlags, uintptr_t dwExtraInfo)",
            );
          } catch (e) {
            console.error("keybd_event signature failed: " + e.message);
          }

          // tapVk 按键一次：支持 "ctrl+c"、"ctrl+shift+t"、"alt+f4"
          const tapVk = (spec) => {
            if (!keybd_event) return "keybd_event unavailable";
            const parts = String(spec || "").split("+").map((x) => x.trim().toLowerCase()).filter(Boolean);
            if (parts.length === 0) return "empty key";
            const last = parts[parts.length - 1];
            const mods = parts.slice(0, -1);
            const modCodes = mods.map((m) => VK[m]).filter((c) => c !== undefined);
            if (modCodes.length !== mods.length) return "unknown modifier in " + spec;
            let keyCode = VK[last];
            if (keyCode === undefined) {
              // 单字符：字母/数字 → ASCII 大写
              if (last.length === 1) keyCode = last.toUpperCase().charCodeAt(0);
              else return "unknown key: " + last;
            }
            for (const c of modCodes) keybd_event(c, 0, 0, 0);
            keybd_event(keyCode, 0, VK_EXTENDED.has(keyCode) ? 1 : 0, 0);
            keybd_event(keyCode, 0, (VK_EXTENDED.has(keyCode) ? 1 : 0) | 0x0002, 0);
            for (const c of modCodes.slice().reverse()) keybd_event(c, 0, 0x0002, 0);
            return null;
          };

          // 滚动：dy 纵向、dx 横向，正数向上
          const doScroll = () => {
            const dy = Number(params.dy !== undefined ? params.dy : params.y) || 0;
            const dx = Number(params.dx !== undefined ? params.dx : params.x) || 0;
            if (dx) mouse_event(HWHEEL, 0, 0, -Math.round(dx * 120), 0);
            if (dy) mouse_event(WHEEL, 0, 0, Math.round(dy * 120), 0);
          };

          // 先移动鼠标到目标位置
          SetCursorPos(absX, absY);

          switch (action) {
            case "move":
              sendCmdResult(reqId, baseResult(reqId, "ok", "computeruse move @ (" + absX + "," + absY + ")", ""));
              break;
            case "click":
              mouse_event(btnDown, 0, 0, 0, 0);
              mouse_event(btnUp, 0, 0, 0, 0);
              sendCmdResult(reqId, baseResult(reqId, "ok", "computeruse click @ (" + absX + "," + absY + ")", ""));
              break;
            case "doubleclick":
              mouse_event(btnDown, 0, 0, 0, 0);
              mouse_event(btnUp, 0, 0, 0, 0);
              setTimeout(() => {
                mouse_event(btnDown, 0, 0, 0, 0);
                mouse_event(btnUp, 0, 0, 0, 0);
                sendCmdResult(reqId, baseResult(reqId, "ok", "computeruse doubleclick @ (" + absX + "," + absY + ")", ""));
              }, 50);
              break;
            case "rightclick":
              mouse_event(0x0008, 0, 0, 0, 0);
              mouse_event(0x0010, 0, 0, 0, 0);
              sendCmdResult(reqId, baseResult(reqId, "ok", "computeruse rightclick @ (" + absX + "," + absY + ")", ""));
              break;
            case "scroll": {
              // 旧实现固定向上滚 120*dy，且不支持横向。
              // 现在按参数给方向/量级，并支持 dx（横向滚动）。
              doScroll();
              sendCmdResult(
                reqId,
                baseResult(
                  reqId,
                  "ok",
                  "computeruse scroll dx=" + (params.dx || 0) + " dy=" + (params.dy || params.y || 0) +
                    " @ (" + absX + "," + absY + ")",
                  "",
                ),
              );
              break;
            }
            // === 以下为补齐的高级操作 ===
            case "mousedown":
              // 按下不释放：用于与 mouseup 配对做"按住拖拽"
              mouse_event(btnDown, 0, 0, 0, 0);
              sendCmdResult(reqId, baseResult(reqId, "ok", "computeruse mousedown(" + btn + ")", ""));
              break;
            case "mouseup":
              mouse_event(btnUp, 0, 0, 0, 0);
              sendCmdResult(reqId, baseResult(reqId, "ok", "computeruse mouseup(" + btn + ")", ""));
              break;
            case "drag": {
              // 一步拖拽：从 (x,y) 按下 → 移动到 (tox,toy) → 释放
              const toX = Math.round(
                ox + (physical ? (parseFloat(params.tox || params.tx) || 0) : (parseFloat(params.tox || params.tx) || 0) * scale),
              );
              const toY = Math.round(
                oy + (physical ? (parseFloat(params.toy || params.ty) || 0) : (parseFloat(params.toy || params.ty) || 0) * scale),
              );
              SetCursorPos(absX, absY);
              mouse_event(btnDown, 0, 0, 0, 0);
              // 分步移动：部分应用（浏览器 canvas、拖拽排序）需要中间 move 事件
              const steps = Math.max(1, Math.min(40, parseInt(params.steps || 12, 10) || 12));
              for (let i = 1; i <= steps; i++) {
                const t = i / steps;
                SetCursorPos(
                  Math.round(absX + (toX - absX) * t),
                  Math.round(absY + (toY - absY) * t),
                );
              }
              mouse_event(btnUp, 0, 0, 0, 0);
              sendCmdResult(
                reqId,
                baseResult(reqId, "ok", "computeruse drag (" + absX + "," + absY + ") -> (" + toX + "," + toY + ")", ""),
              );
              break;
            }
            case "hover":
              // 悬停：只移动不点击（触发 tooltip / hover 菜单）
              SetCursorPos(absX, absY);
              sendCmdResult(reqId, baseResult(reqId, "ok", "computeruse hover @ (" + absX + "," + absY + ")", ""));
              break;
            case "keypress": {
              // 支持 "ctrl+c" 这类组合键；旧实现只发单键
              const err2 = tapVk(params.key || params.keys || params.text);
              if (err2) {
                sendCmdResult(reqId, baseResult(reqId, "error", "", "computeruse keypress: " + err2));
              } else {
                sendCmdResult(reqId, baseResult(reqId, "ok", "computeruse keypress " + (params.key || params.keys || params.text), ""));
              }
              break;
            }
            case "hotkey":
            case "combo": {
              const keys = String(params.keys || params.key || "");
              const err2 = tapVk(keys);
              if (err2) {
                sendCmdResult(reqId, baseResult(reqId, "error", "", "computeruse hotkey: " + err2));
              } else {
                sendCmdResult(reqId, baseResult(reqId, "ok", "computeruse hotkey " + keys, ""));
              }
              break;
            }
            case "middleclick":
              mouse_event(BTN.middle[0], 0, 0, 0, 0);
              mouse_event(BTN.middle[1], 0, 0, 0, 0);
              sendCmdResult(reqId, baseResult(reqId, "ok", "computeruse middleclick @ (" + absX + "," + absY + ")", ""));
              break;
            case "tripleclick":
              // 三击：三次快速左键，用于选中整行
              for (let i = 0; i < 3; i++) {
                mouse_event(BTN.left[0], 0, 0, 0, 0);
                mouse_event(BTN.left[1], 0, 0, 0, 0);
              }
              sendCmdResult(reqId, baseResult(reqId, "ok", "computeruse tripleclick @ (" + absX + "," + absY + ")", ""));
              break;
            case "type": {
              // 用 SendKeys 注入文本（koffi 只能发按键，文本需走 SendInput）
              const t = String(params.text || "");
              if (!t) {
                sendCmdResult(reqId, baseResult(reqId, "error", "", "computeruse type: empty text"));
                break;
              }
              // 逐字符发送：SendKeys 语法里 + ^ % ~ { } 都是特殊字符
              const SPECIAL = { "+": "{+}", "^": "{^}", "%": "{%}", "~": "{~}", "{": "{{}", "}": "{}}", "(": "{(}", ")": "{)}", "[": "{[}", "]": "{]}" };
              let expr = "";
              for (const ch of t) expr += SPECIAL[ch] !== undefined ? SPECIAL[ch] : ch;
              const ps =
                "Add-Type -AssemblyName System.Windows.Forms;" +
                "[System.Windows.Forms.SendKeys]::SendWait('" +
                expr.replace(/'/g, "''") +
                "')";
              cp.exec("powershell", ["-NoProfile", "-Command", ps], { timeout: 15000 }, (err) => {
                if (err) {
                  sendCmdResult(reqId, baseResult(reqId, "error", "", "computeruse type failed: " + err.message));
                } else {
                  sendCmdResult(reqId, baseResult(reqId, "ok", "computeruse typed " + t.length + " chars", ""));
                }
              });
              break;
            }
            case "wait":
            case "sleep": {
              // 显式等待：UI 更新/动画未完成时先等一下再下一步
              const ms = Math.max(0, Math.min(10000, parseInt(params.ms || params.duration || 500, 10) || 500));
              setTimeout(() => {
                sendCmdResult(reqId, baseResult(reqId, "ok", "computeruse waited " + ms + "ms", ""));
              }, ms);
              break;
            }
            case "display": {
              // 返回各显示器信息：模型需要知道有几个屏、逻辑尺寸与缩放
              const info = displays.map((d, i) => ({
                index: i,
                bounds: d.bounds,
                scaleFactor: d.scaleFactor,
                primary: d.id === screen.getPrimaryDisplay().id,
              }));
              sendCmdResult(reqId, baseResult(reqId, "ok", "displays: " + JSON.stringify(info), ""));
              break;
            }
            default:
              sendCmdResult(reqId, baseResult(reqId, "error", "", "computeruse: unknown action " + action));
          }
        } catch (e) {
          sendCmdResult(reqId, baseResult(reqId, "error", "", "computeruse: " + e.message));
        }
        return;
      }
      // Linux / macOS 通过工具 argv
      const L = (a) => {
        if (os_ === "darwin") {
          // cliclick 参数: c:x,y / m:v1,v2 / w:+,-
          const pos = absX + "," + absY;
          switch (a[0]) {
            case "move": return ["m:" + pos];
            case "click": return ["c:" + pos];
            case "doubleclick": return ["dc:" + pos];
            case "rightclick": return ["c:" + pos];
            case "scroll": return ["w:" + (params.dy > 0 ? "+" : "-")];
            case "hover": return ["m:" + pos];
            case "middleclick": return ["dc:" + pos];
            // cliclick 原生支持 dd（按下拖拽）/ du（释放）/ dm（拖到）
            case "mousedown": return ["dd:" + pos];
            case "mouseup": return ["du:" + pos];
            case "drag": return ["dm:" + (absX) + "," + (absY) + "," + (ox + (parseFloat(params.tox) || 0) / scale) + "," + (oy + (parseFloat(params.toy) || 0) / scale)];
            default: return a;
          }
        }
        return a;
      };
      switch (action) {
        case "move":
          run(L(["mousemove", String(absX), String(absY)]));
          return;
        case "click": {
          const btn = params.button === "right" ? 3 : params.button === "middle" ? 2 : 1;
          run(L(["mousemove", String(absX), String(absY), "click", String(btn)]) );
          return;
        }
        case "doubleclick":
          run(L(["mousemove", String(absX), String(absY), "click", "--repeat", "2", "--delay", "50", "1"]));
          return;
        case "rightclick":
          run(L(["mousemove", String(absX), String(absY), "click", "3"]));
          return;
        case "scroll":
          run(L(["mousemove", String(absX), String(absY), "scroll", "--button", "5", String(Math.round(params.dy || params.y || 0))]));
          return;
        case "keypress":
        case "hotkey":
        case "combo": {
          // xdotool/ cliclick 的 key 参数本身就接受 "ctrl+c" 这类组合键，
          // 但旧代码把整个字符串原样透传，模型传 "Ctrl+C"（大写/带引号）就失效。
          // 这里做一次归一化。
          let spec = String(params.key || params.keys || params.text || "").trim();
          spec = spec
            .replace(/\+/g, " plus ")
            .replace(/ctrl\s*\+\s*ctrl\s*\+/gi, "ctrl+")
            .split(/\s*\+\s*/)
            .filter(Boolean)
            .map((x) => x.toLowerCase().trim())
            .join("+");
          if (!spec) {
            sendCmdResult(reqId, baseResult(reqId, "error", "", "computeruse keypress: empty key"));
            return;
          }
          run(L(["key", spec]));
          return;
        }
        case "mousedown":
          run(L([os_ === "darwin" ? "mousedown" : "mousedown", String(absX), String(absY)]));
          return;
        case "mouseup":
          run(L([os_ === "darwin" ? "mouseup" : "mouseup", String(absX), String(absY)]));
          return;
        case "drag": {
          const toX = Math.round(ox + (parseFloat(params.tox) || 0) / scale);
          const toY = Math.round(oy + (parseFloat(params.toy) || 0) / scale);
          const steps = Math.max(1, Math.min(40, parseInt(params.steps || 12, 10) || 12));
          run(L(["mousemove", String(absX), String(absY), "mousedown", "1"]));
          for (let i = 1; i <= steps; i++) {
            const tt = i / steps;
            run(
              L([
                "mousemove",
                String(Math.round(absX + (toX - absX) * tt)),
                String(Math.round(absY + (toY - absY) * tt)),
              ]),
            );
          }
          run(L(["mouseup", "1"]));
          return;
        }
        case "hover":
          run(L(["mousemove", String(absX), String(absY)]));
          return;
        case "middleclick":
          run(L(["mousemove", String(absX), String(absY), "click", "2"]));
          return;
        case "tripleclick":
          run(
            L([
              "mousemove", String(absX), String(absY),
              "click", "--repeat", "3", "--delay", "60", "1",
            ]),
          );
          return;
        case "wait":
        case "sleep": {
          const ms = Math.max(0, Math.min(10000, parseInt(params.ms || params.duration || 500, 10) || 500));
          setTimeout(() => {
            sendCmdResult(reqId, baseResult(reqId, "ok", "computeruse waited " + ms + "ms", ""));
          }, ms);
          return;
        }
        case "display": {
          const info = displays.map((d, i) => ({
            index: i,
            bounds: d.bounds,
            scaleFactor: d.scaleFactor,
            primary: d.id === screen.getPrimaryDisplay().id,
          }));
          sendCmdResult(reqId, baseResult(reqId, "ok", "displays: " + JSON.stringify(info), ""));
          return;
        }
        case "type": {
          // macOS cliclick 无 type，用 osascript
          if (os_ === "darwin") {
            const t = String(params.text || "");
            cp.execFile(
              "osascript",
              ["-e", 'tell application "System Events" to keystroke ' + JSON.stringify(t)],
              { timeout: 15000, maxBuffer: 1024 * 1024 },
              (err) => {
                sendCmdResult(
                  reqId,
                  baseResult(reqId, err ? "error" : "ok", err ? "" : "typed: " + t.slice(0, 40), err ? err.message : ""),
                );
              },
            );
            return;
          }
          const t = String(params.text || "");
          const shellSafe = "'" + String(t).replace(/'/g, "'\\''") + "'";
          cp.execFile(
            "bash",
            ["-c", "xdotool type " + shellSafe],
            { timeout: 15000, maxBuffer: 1024 * 1024 },
            (err) => {
              sendCmdResult(
                reqId,
                baseResult(reqId, err ? "error" : "ok", err ? "" : "typed: " + t.slice(0, 40), err ? err.message : ""),
              );
            },
          );
          return;
        }
        default:
          sendCmdResult(reqId, baseResult(reqId, "error", "", "computeruse: unknown action " + action));
          return;
      }
    }
    case "clipboardsee": {
      // 读取设备剪切板当前文字内容（仅文本）。隐私提示：可能含密码，仅用户明确要求时读。
      // 协议: homeagent-clipboardsee
      try {
        const txt = clipboard.readText();
        const has = !!(txt && txt.length);
        sendCmdResult(
          reqId,
          Object.assign(baseResult(reqId, "ok", has ? String(txt) : "", ""), {
            content: has ? "ok" : "empty",
          }),
        );
      } catch (e) {
        sendCmdResult(reqId, baseResult(reqId, "error", "", "clipboardsee failed: " + e.message));
      }
      return;
    }
    case "omniparse": {
      // 解析当前屏幕 UI 元素，返回结构化 JSON 供 agent 分析
      // 使用 PowerShell Get-Process + .NET 获取窗口信息
      try {
        const cp = require("child_process");
        const psScript = `
$wins = @()
$procs = [System.Diagnostics.Process]::GetProcesses()
foreach ($p in $procs) {
    if ($p.MainWindowHandle -ne 0 -and $p.MainWindowTitle) {
        $wins += @{
            pid = $p.Id
            name = $p.ProcessName
            title = $p.MainWindowTitle.Trim()
            hwnd = $p.MainWindowHandle.ToString("x")
        }
    }
}
if ($wins.Count -gt 50) { $wins = $wins[0..49] }
return ($wins | ConvertTo-Json -Compress)
`;
        const psFile = require("path").join(require("os").tmpdir(), "ha_omniparse_" + Date.now() + ".ps1");
        require("fs").writeFileSync(psFile, psScript, "utf8");
        cp.execFile("powershell", ["-NoProfile", "-ExecutionPolicy", "Bypass", "-File", psFile],
          { timeout: 15000, maxBuffer: 1024 * 1024 },
          (err, stdout) => {
            try { require("fs").unlinkSync(psFile); } catch (e) {}
            if (err) { sendCmdResult(reqId, baseResult(reqId, "error", "", "omniparse: " + err.message)); return; }
            const out = (stdout || "").trim();
            if (!out) { sendCmdResult(reqId, baseResult(reqId, "error", "", "omniparse: no output")); return; }
            sendCmdResult(reqId, baseResult(reqId, "ok", out, ""));
          });
      } catch (e) {
        sendCmdResult(reqId, baseResult(reqId, "error", "", "omniparse: " + e.message));
      }
      return;
    }
    case "clipboardsue": {
      // 写入文字到设备剪切板（用户可直接 Ctrl+V 粘贴）。
      // 协议: homeagent-clipboardsue <文字>
      const txt = String(capability || "")
        .replace(/^clipboardsue/, "")
        .trim();
      try {
        clipboard.writeText(txt);
        sendCmdResult(
          reqId,
          baseResult(
            reqId,
            "ok",
            "written " + Buffer.byteLength(txt) + " bytes: " + txt.slice(0, 60),
            "",
          ),
        );
      } catch (e) {
        sendCmdResult(reqId, baseResult(reqId, "error", "", "clipboardsue failed: " + e.message));
      }
      return;
    }
    default:
      sendCmdResult(
        reqId,
        baseResult(
          reqId,
          "error",
          "",
          "unsupported homeagent capability: " + name,
        ),
      );
  }
}

// 简单安全校验：拒绝明显危险命令
function argsSafe(cmd) {
  if (!cmd) return false;
  const cmdStr = String(cmd).toLowerCase();
  const bad = [
    "rm -rf",
    "mkfs",
    "dd if=",
    "shutdown",
    "reboot",
    "curl ",
    "wget ",
    ":(){",
    "eval ",
    "su ",
    "sudo ",
  ];
  for (const b of bad) {
    if (cmdStr.indexOf(b) !== -1) return false;
  }
  return true;
}

// 启动设备桥：url/token 来自 gui-prefs.deviceBridge（独立于连接类型）
//   url 可为 ws(s)://完整端点（含路径），token 为网关 ws_token。
async function startDeviceBridge(cfg) {
  if (!cfg) return;
  deviceBridgeBound = false;
  deviceBridgeBindError = "";
  const url = cfg.url || cfg.gateway || "";
  const token = cfg.apiKey || cfg.token || "";
  if (!url || !token) {
    console.error("[device-bridge] missing url/token, skipped");
    return;
  }
  deviceBridgeAddr = url;
  // 记住配置：重连时要用（连接断开后局部变量就没了）。
  deviceBridgeCfg = { url, token };
  // 拉起意图。scheduleDeviceReconnect 以此为闸 —— 用户主动停用后
  // 不得再排程重连。
  deviceWanted = true;
  deviceReconnectAttempt = 0;
  if (deviceReconnectTimer) {
    clearTimeout(deviceReconnectTimer);
    deviceReconnectTimer = null;
  }
  try {
    await connectDeviceBridgeOnce(deviceBridgeCfg);
    // 首次就连上：退避归零。
    deviceReconnectAttempt = 0;
  } catch (e) {
    console.error("[device-bridge] connect failed: " + e.message);
    // ★ 连接失败也必须排程重连 —— 这条路上**没有** socket close 事件
    //   （socket 从未 upgraded），若只把重连挂在 close 上，首次失败就
    //   永远不会自愈。
    scheduleDeviceReconnect();
  }
}

function stopDeviceBridge() {
  // 先断意图，再拆资源：顺序反了会让 close 事件里的排程再把桥拉起来。
  deviceWanted = false;
  stopDevicePing();
  if (deviceReconnectTimer) {
    clearTimeout(deviceReconnectTimer);
    deviceReconnectTimer = null;
  }
  deviceReconnectAttempt = 0;
  if (deviceBridge) {
    try {
      deviceBridge.close();
    } catch (e) {}
    deviceBridge = null;
  }
}

// ============ 定时授权调度（睡眠期间自动撤销，恢复时自动恢复） ============
let schedAuthTimer = null;
let schedAuthState = null; // "revoked" | "restored" 防抖

// 读授权调度配置 prefs.deviceBridge.authSchedule = {revokeTime:"23:00", restoreTime:"07:00", enabled}
function getAuthSchedule() {
  try {
    const prefs = loadGuiPrefs();
    const db = prefs.deviceBridge || {};
    const s = db.authSchedule || {};
    return {
      enabled: !!s.enabled,
      revokeTime: s.revokeTime || "",
      restoreTime: s.restoreTime || "",
    };
  } catch (e) {
    return { enabled: false, revokeTime: "", restoreTime: "" };
  }
}

// 当前 HH:mm
function nowHHMM() {
  const d = new Date();
  return (
    String(d.getHours()).padStart(2, "0") +
    ":" +
    String(d.getMinutes()).padStart(2, "0")
  );
}

// 比较 HH:mm；返回 true 表示 a <= b
function timeLeq(a, b) {
  return a <= b;
}

// 执行授权开关（客户端本地鉴权：写 gui-prefs.deviceBridge.authorized，
// 并重新 hello 同步自报状态到服务端展示。服务端不存储授权，agent 无法篡改。）
async function setDeviceAuthorized(authorized) {
  try {
    const prefs = loadGuiPrefs();
    prefs.deviceBridge = prefs.deviceBridge || {};
    prefs.deviceBridge.authorized = !!authorized;
    saveGuiPrefs(prefs);
    console.log("[auth] set local authorized=" + !!authorized);
    // 若设备桥已连接，重新 hello 同步状态
    if (deviceBridge && deviceBridge.send && deviceBridgeId) {
      deviceBridge.send({
        op: "hello",
        device: {
          device_id: deviceBridgeId,
          name: "HologramAgent GUI",
          kind: "computer",
          authorized: !!authorized,
          caps: [
            "status", "cmdrun", "deviceinfo", "cmdresult",
            "computeruse", "screensee", "clipboardsee", "clipboardsue",
            "speakeruse", "camerasue", "screensue", "omniparse",
          ],
        },
      });
    }
  } catch (e) {
    console.error("[auth] set authorized failed: " + e.message);
  }
}

// 调度器：每分钟检查，到点执行撤销/恢复
function startScheduledAuth() {
  if (schedAuthTimer) clearInterval(schedAuthTimer);
  const check = async () => {
    try {
      const s = getAuthSchedule();
      if (!s.enabled || !s.revokeTime) return;
      if (!deviceBridgeId) return;
      const now = nowHHMM();
      // 支持跨天：revoke 23:00 restore 07:00
      let shouldRevoke = false;
      if (s.revokeTime && s.restoreTime) {
        if (s.revokeTime <= s.restoreTime) {
          // 同日: revoke <= now <= restore 撤销
          shouldRevoke =
            timeLeq(s.revokeTime, now) && timeLeq(now, s.restoreTime);
        } else {
          // 跨天: now >= revoke 或 now <= restore 撤销
          shouldRevoke =
            timeLeq(s.revokeTime, now) || timeLeq(now, s.restoreTime);
        }
      } else if (s.revokeTime) {
        shouldRevoke = timeLeq(s.revokeTime, now);
      }
      if (shouldRevoke && schedAuthState !== "revoked") {
        schedAuthState = "revoked";
        await setDeviceAuthorized(false);
      } else if (!shouldRevoke && schedAuthState === "revoked") {
        schedAuthState = "restored";
        await setDeviceAuthorized(true);
      } else if (!shouldRevoke && schedAuthState === null) {
        schedAuthState = "restored";
      }
    } catch (e) {}
  };
  check();
  schedAuthTimer = setInterval(check, 60000);
}

// ============ 系统托盘（惰性 + 安全降级） ============
let tray = null;
// 托盘菜单动态数据
let trayLastCmd = null; // 最近一次 device cmd: {cmd, at, result}
let trayCmdCount = 0; // 历史 cmd 总次数
// 托盘菜单构建缓存 + 防抖（避免设备桥消息风暴时反复 setContextMenu 导致弹出卡顿）
const trayConnCache = { connLabel: "", connUrl: "" };
let trayRebuildTimer = null;
let trayMenuBuilt = false;
function buildTrayMenuTemplate() {
  const electron = require("electron");
  const TMenu = electron.Menu;
  const tpl = [];
  tpl.push({ label: "HologramAgent", enabled: true });
  tpl.push({ type: "separator" });
  // 远程连接状态（缓存避免每次读文件）
  const connLabel = trayConnCache.connLabel || "未连接";
  const connUrl = trayConnCache.connUrl || "";
  tpl.push({ label: "后端: " + connLabel, enabled: true });
  if (connUrl) tpl.push({ label: connUrl, enabled: true });
  let online = false;
  try {
    if (authRule && authRule.urlHost) online = true;
  } catch (e) {}
  tpl.push({
    label: online ? "[已连接]" : "[未连接]",
    enabled: true,
  });
  tpl.push({ type: "separator" });
  // 设备桥状态
  if (deviceBridge) {
    tpl.push({ label: "设备桥: [已连接]", enabled: true });
    if (deviceBridgeId)
      tpl.push({ label: "设备ID: " + deviceBridgeId, enabled: true });
    if (deviceBridgeAddr)
      tpl.push({ label: "网关: " + deviceBridgeAddr, enabled: true });
    if (trayLastCmd) {
      tpl.push({ label: "上次远控: " + trayLastCmd.cmd, enabled: true });
      tpl.push({
        label:
          "结果: " +
          (trayLastCmd.result || "…").slice(0, 60) +
          "（" +
          (trayCmdCount || 0) +
          "次总数）",
        enabled: true,
      });
    } else {
      tpl.push({ label: "未收到远控命令", enabled: true });
    }
  } else {
    tpl.push({ label: "设备桥: [未连接]", enabled: true });
  }
  tpl.push({ type: "separator" });
  tpl.push({ label: "显示主界面", click: () => showMainWindow() });
  tpl.push({
    label: "退出",
    click: () => {
      app.isQuitting = true;
      app.quit();
    },
  });
  return TMenu.buildFromTemplate(tpl);
}
function applyTrayMenu() {
  if (!tray) return;
  try {
    tray.setContextMenu(buildTrayMenuTemplate());
    trayMenuBuilt = true;
  } catch (e) {}
}
// 防抖重建：高频触发时合并，200ms 内只实际 setContextMenu 一次
function rebuildTrayMenu() {
  if (!tray) return;
  // 先刷新连接缓存
  try {
    const conns = loadConnections();
    const cur =
      conns.connections.find((c) => c.id === conns.currentId) ||
      conns.connections[0];
    if (cur) {
      trayConnCache.connLabel = cur.name || "未命名";
      trayConnCache.connUrl = cur.url || cur.socketPath || "";
    } else {
      trayConnCache.connLabel = "未连接";
      trayConnCache.connUrl = "";
    }
  } catch (e) {}
  // 首建立即（保证右键立刻有菜单），后续防抖
  if (!trayMenuBuilt) {
    applyTrayMenu();
    return;
  }
  if (trayRebuildTimer) clearTimeout(trayRebuildTimer);
  trayRebuildTimer = setTimeout(applyTrayMenu, 200);
}
function initTray() {
  if (tray) return;
  try {
    const electron = require("electron");
    const Tray = electron.Tray;
    const nImg = electron.nativeImage;
    let img = null;
    const cands = [
      path.join(__dirname, "icon-tray@2x.png"),
      path.join(__dirname, "icon-tray.png"),
      path.join(__dirname, "icon.ico"),
    ];
    for (const c of cands) {
      try {
        const m = nImg.createFromPath(c);
        if (m && !m.isEmpty()) {
          img = m;
          break;
        }
      } catch (e) {}
    }
    if (!img || img.isEmpty()) {
      try {
        img = nImg.createFromPath(path.join(__dirname, "icon-tray.png"));
      } catch (e) {}
    }
    if (!img || img.isEmpty()) {
      img = nImg.createEmpty();
    }
    tray = new Tray(img);
    tray.setToolTip("HologramAgent - 个人智能管家");
    rebuildTrayMenu();
    tray.on("double-click", () => showMainWindow());
    console.log("[tray] READY: " + (tray ? "tray-created" : "null"));
  } catch (e) {
    console.error("[tray] init failed (safe ignore): " + e.message);
    try {
      if (tray) {
        tray.destroy();
        tray = null;
      }
    } catch (e2) {}
  }
}
function showMainWindow() {
  try {
    if (mainWindow && !mainWindow.isDestroyed()) {
      // ★ 健康检查（2026-10-05 修白屏）：仅靠 isVisible() 不够。
      //
      // 长时间后台后 renderer 可能已被回收 —— 窗口句柄还在、isVisible()
      // 仍为 true，但内容是空的。此时原逻辑会跳过 show()，从托盘打开
      // 就是一片白，而且永远不会自愈。
      //
      // 两个判据：
      //   isCrashed() —— 渲染进程已死，必须重载才能出内容；
      //   isLoading() —— 还在加载，等ready-to-show 就行，不重载。
      const wc = mainWindow.webContents;
      if (wc.isCrashed()) {
        console.log("[window] renderer crashed, reloading before show");
        try {
          wc.reload();
        } catch (e) {}
      }
      // 窗口是静默创建时已处于「未显示」态，这里正常显示即可。
      // 用 showInactive + focus 两步：showInactive 不抢焦点地显示，
      // focus 再把焦点给过来（纯 show 在部分 Win 版本上会导致
      // 无边框窗口不获得焦点，用户还得再点一下）。
      if (!mainWindow.isVisible()) mainWindow.show();
      try {
        mainWindow.focus();
      } catch (e) {}
      return;
    }
  } catch (e) {
    /* 落到下面重建 */
  }
  // 兜底：窗口不存在（异常退出后残留状态、或未来改成不建窗）时重建，
  // 否则托盘菜单点了没反应，进程就成了唤不起来的僵尸。
  try {
    createWindow(true);
  } catch (e) {
    console.error("showMainWindow: recreate failed: " + e.message);
  }
}
function destroyTray() {
  try {
    if (tray) {
      tray.destroy();
      tray = null;
    }
  } catch (e) {}
}

app.whenReady().then(async () => {
  installAuthRule();
  // 清理历史遗留的重复开机自启项（产物改名会累积指向同一 exe 的旧项）。
  // 不依赖用户去设置里手动开关：否则旧项会一直留着、开机启动两次。
  try {
    cleanupStaleLoginItems();
  } catch (e) {}
  const running = await isServerRunning();
  if (running) {
    // 已探测到可用端点：服务端在别处正常运行（远程/WSL/另一台机器）。
    // 这类部署下 GUI 不该、也拉不起 homed —— 此前仍会去 startHomed()
    // 再等 8s 超时，刷出误导性的 "homed failed to start within timeout"。
    console.log("server reachable, skip homed autostart");
  } else if (!findHomed()) {
    // 没有本地 homed 可执行文件 ⇒ 本机根本不具备托管服务端的能力，
    // 此时干等超时毫无意义，直接说明情况即可。
    console.log("no local homed binary and no reachable endpoint; GUI will use configured connection");
  } else {
    startHomed();
    const started = await waitForServer();
    if (started) {
      console.log("homed started successfully");
    } else {
      console.error("homed failed to start within timeout");
    }
  }
  // 设备桥：由 GUI 偏好（deviceBridge 开关+网关+token）驱动，独立于连接
  try {
    const _prefs = loadGuiPrefs();
    const _db = _prefs.deviceBridge || {};
    if (_db.enabled && _db.gateway && _db.token) {
      startDeviceBridge({ url: _db.gateway, apiKey: _db.token });
    } else if (_db.enabled) {
      console.error(
        "[device-bridge] enabled but missing gateway/token, skipped",
      );
    }
  } catch (e) {
    console.error("device bridge init: " + e.message);
  }
  try {
    initTray();
  } catch (e) {
    console.error("[tray] whenReady call: " + e.message);
  }
  // 定时撤销/恢复授权：按 prefs.deviceBridge.authSchedule {revokeTime, restoreTime} 每分钟检查
  try {
    startScheduledAuth();
  } catch (e) {
    console.error("[auth-schedule] start failed: " + e.message);
  }

  // ★ 静默启动：prefs.silentStart 为真时不显示主窗口，驻留托盘。
  //
  // 此前这个开关是**死的**：设置页有它、gui-prefs.json 存了它、文案写着
  // 「启动时不显示主窗口，驻留托盘后台运行」，但主进程从来没读过它 ——
  // whenReady 无条件 createWindow()，于是必然弹窗。
  //
  // 实测（本机，prefs.silentStart=true）：窗口标题 HologramAgent 照样出现。
  //
  // 与 applyAutoLaunch 里的 openAsHidden 是两回事：那个是告诉 OS「开机自启
  // 时如何启动」（仅 Windows/macOS 生效），这里是本进程自己决定要不要显示，
  // 对手动启动同样生效。
  let silent = false;
  try {
    silent = !!loadGuiPrefs().silentStart;
  } catch (e) {
    console.error("[startup] read silentStart failed: " + e.message);
  }
  createWindow(!silent);
  if (silent) {
    console.log("[startup] silentStart=on: window created hidden (tray only)");
  }
});

app.on("before-quit", () => {
  stopHomed();
  try {
    stopDeviceBridge();
  } catch (e) {}
  try {
    destroyTray();
  } catch (e) {}
});

app.on("window-all-closed", () => {
  // 退出进托盘：依偏好决定（安全降级：无托盘时退出）
  let _exitTray = false;
  try {
    const _p = loadGuiPrefs();
    _exitTray = !!(_p && _p.exitToTray);
  } catch (e) {}
  if (_exitTray && tray) {
    try {
      if (mainWindow && !mainWindow.isDestroyed()) mainWindow.hide();
    } catch (e) {}
  } else {
    app.quit();
  }
});

app.on("activate", () => {
  if (mainWindow === null) {
    createWindow();
  }
});

// GPU 进程崩溃/重启后，无边框窗口（frame:false）在部分驱动上会停在
// 上一帧不再重绘 —— 表现同样是「窗口在、内容不动或全白」。
// `child-process-gone` 比 `render-process-gone` 覆盖面更广（含 GPU），
// 这里只在 GPU 挂掉时重载渲染进程（不重建窗口，避免丢窗口位置/尺寸）。
app.on("child-process-gone", (e, details) => {
  const type = details && details.type;
  const reason = details && details.reason;
  if (type !== "GPU") return;
  console.error("[window] GPU process gone: reason=" + reason);
  if (reason === "oom" || reason === "launch-failed") return;
  try {
    if (mainWindow && !mainWindow.isDestroyed()) {
      mainWindow.webContents.reload();
    }
  } catch (err) {
    console.error("[window] reload after GPU crash failed: " + err.message);
  }
});
// ============ 系统通知（Electron 原生）============
//
// 之前**完全没有**这个能力：主进程没引入 Notification、preload 没暴露接口、
// 渲染层只有页面内的 toast（用户不盯着窗口就看不到）。
// 这套是补齐：渲染层判断"该不该通知"，主进程负责弹系统通知。
//
// 为什么点击要回传会话：点了通知却不知道该看哪句话，等于没通知。
// 点击时通过 webContents 回到渲染进程，由它切到 chat 视图并滚到该条。
let notifSeq = 0;

ipcMain.handle("notify:show", (_, payload) => {
  try {
    const p = payload || {};
    const title = String(p.title || "HologramAgent");
    const body = String(p.body || "");
    if (!body) return { ok: false, error: "empty body" };
    if (!Notification.isSupported()) return { ok: false, error: "unsupported" };

    // 已有窗口在前台且可见时不再打扰：用户正看着界面。
    // 静默启动场景下 mainWindow 存在但 hidden，正好走"要通知"这条路。
    if (mainWindow && !mainWindow.isDestroyed() && mainWindow.isVisible() && mainWindow.isFocused()) {
      return { ok: false, skipped: "focused" };
    }

    const id = ++notifSeq;
    const n = new Notification({
      title: title,
      body: body.length > 180 ? body.slice(0, 180) + "\u2026" : body,
      silent: !!p.silent,
      urgency: p.urgent ? "critical" : "normal",
    });
    n.on("click", () => {
      // 走 showMainWindow 而非直接 show：它带renderer 健康检查，
      // 直接 show 在 renderer 已被回收时会开出一个白窗口。
      showMainWindow();
      try {
        mainWindow.webContents.send("notify:clicked", { id: id, msgKey: p.msgKey || "" });
      } catch (e) {}
      n.close();
    });
    n.show();
    return { ok: true, id: id };
  } catch (e) {
    return { ok: false, error: e.message };
  }
});

ipcMain.handle("notify:supported", () => {
  try {
    return { supported: Notification.isSupported() };
  } catch (e) {
    return { supported: false };
  }
});

// IPC：prefs（renderer 设置页需要）
// IPC：本机设备身份（renderer 设备页需要）
ipcMain.handle("device:identity", () =>
  deviceBridge
    ? {
        device_id: deviceBridgeId,
        active: true,
        address: deviceBridgeAddr || "",
      }
    : { device_id: "", active: false, address: "" },
);
const GUI_PREFS_FILE = path.join(app.getPath("userData"), "gui-prefs.json");

function loadGuiPrefs() {
  try {
    if (fs.existsSync(GUI_PREFS_FILE)) {
      // ★ 去 BOM 后再 parse。
      //
      // Windows 上用 PowerShell Set-Content -Encoding UTF8、记事本等工具改过
      // 这个文件，会在开头写入 UTF-8 BOM（EF BB BF）。JSON.parse 遇到它直接
      // 抛错 → 走 catch → 返回**默认 prefs**（silentStart/exitToTray/
      // deviceBridge 全部被重置）→ 用户界面上的开关像是"保存了但不起作用"。
      //
      // 本次实测踩到：判据脚本用 Set-Content 改 silentStart，文件带上 BOM 后
      // 静默启动不生效，而 loadConnections 早就有去 BOM 处理、这里没有 ——
      // 两个读取点不一致，属实打实的疏漏。
      const d = JSON.parse(
        fs.readFileSync(GUI_PREFS_FILE, "utf-8").replace(/^\uFEFF/, ""),
      );
      const db = d.deviceBridge || {};
      return {
        autoLaunch: !!d.autoLaunch,
        silentStart: !!d.silentStart,
        exitToTray: d.exitToTray === undefined ? true : !!d.exitToTray,
        deviceBridge: {
          enabled: !!db.enabled,
          authorized: !!db.authorized, // 客户端本地授权（用户在设备上手动开启，服务端不存储）
          gateway: db.gateway || "",
          token: db.token || "",
          screensueDisplay: db.screensueDisplay || "0",
          screensueDuration: db.screensueDuration === undefined || db.screensueDuration === null || db.screensueDuration === "" ? "5" : String(db.screensueDuration),
          exec: db.exec || {},
          authSchedule: db.authSchedule || {},
          audio: db.audio || {},
        },
      };
    }
  } catch (e) {}
  return {
    autoLaunch: false,
    silentStart: false,
    exitToTray: true,
    deviceBridge: {
      enabled: true,
      authorized: false, // 默认不授权，用户手动开启
      gateway: "",
      token: "",
      screensueDisplay: "0",
      screensueDuration: "5",
      exec: {},
      authSchedule: {},
      audio: {},
    },
  };
}

function saveGuiPrefs(p) {
  try {
    fs.writeFileSync(GUI_PREFS_FILE, JSON.stringify(p, null, 2), "utf-8");
  } catch (e) {}
  return p;
}

// Electron LoginItem 写入的注册表项名是「electron.app.<exe 名>」。
// 产物改名过（HologramAgent.exe → homeagent-gui.exe）就会多出一条旧项，
// 且 setLoginItemSettings 不会清理它们 ⇒ 开机启动两次同一个 exe。
// 实测本机 Run 键里确实同时存在：
//   electron.app.HologramAgent     = "E:\program\homeagent\...\homeagent-gui.exe"
//   electron.app.homeagent-gui =  E:\program\homeagent\...\homeagent-gui.exe
// 两条指向同一 exe，所以表面看是两个自启动，实际是同一份的重复。
//
// 修法：每次写入前，删掉所有指向当前 exe 的 electron.app.* 旧项，
// 只保留当前名字对应的那一条。
function cleanupStaleLoginItems() {
  if (process.platform !== "win32") return 0;
  let removed = 0;
  try {
    const { execFileSync } = require("child_process");
    const runKey = "HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run";
    const execPath = String(process.execPath || "").toLowerCase();
    // 列出 Run 键下的所有值名
    const out = execFileSync(
      "reg",
      ["query", runKey],
      { encoding: "utf8", timeout: 5000, windowsHide: true },
    );
    const names = [];
    for (const line of String(out).split(/\r?\n/)) {
      const m = line.match(/^\s{4}([A-Za-z0-9_.\-\\]+)\s+REG_/);
      if (m) names.push(m[1]);
    }
    for (const name of names) {
      if (!/^electron\.app\./i.test(name)) continue;
      // 只删指向**当前 exe** 的；别的 Electron 应用不能误伤
      let val = "";
      try {
        const q = execFileSync("reg", ["query", runKey, "/v", name], {
          encoding: "utf8",
          timeout: 5000,
          windowsHide: true,
        });
        val = String(q);
      } catch (e) {
        continue;
      }
      const sameExe = val.toLowerCase().includes(execPath);
      if (!sameExe) continue;
      // 保留当前 exe 名对应的那一条（Electron 期望的名字）
      const currentName = "electron.app." + path.basename(process.execPath, ".exe");
      if (name === currentName) continue;
      try {
        execFileSync("reg", ["delete", runKey, "/v", name, "/f"], {
          timeout: 5000,
          windowsHide: true,
        });
        removed++;
        console.log("[autolaunch] removed stale login item: " + name);
      } catch (e) {
        console.error("[autolaunch] failed to remove " + name + ": " + e.message);
      }
    }
  } catch (e) {
    console.error("cleanupStaleLoginItems failed: " + e.message);
  }
  return removed;
}

// 应用开机自启设置（Electron LoginItem）
function applyAutoLaunch(enabled) {
  try {
    if (enabled) cleanupStaleLoginItems();
    app.setLoginItemSettings({
      openAtLogin: enabled,
      openAsHidden: true, // 开机自启时静默（Windows/macOS 支持）
      path: process.execPath,
    });
    return true;
  } catch (e) {
    console.error("setLoginItemSettings failed: " + e.message);
    return false;
  }
}

// 全局偏好缓存（供 createWindow 静默判断使用）
let guiPrefs = loadGuiPrefs();

// IPC：本机显示屏幕列表（用于 screensue 默认屏幕配置）
ipcMain.handle("displays:list", () => {
  try {
    const ds = screen.getAllDisplays() || [];
    return ds.map((d, i) => {
      const lbl = (d.label || "").trim();
      return {
        index: i,
        name: lbl || "显示器" + (i + 1),
        id: d.id,
        size: d.bounds ? d.bounds.width + "x" + d.bounds.height : "",
        primary:
          d.id ===
          (screen.getPrimaryDisplay ? screen.getPrimaryDisplay().id : -1),
      };
    });
  } catch (e) {
    return [];
  }
});

// IPC：本机音频输出设备列表（供设备通道选择声卡）
ipcMain.handle("audio:list", () => {
  try {
    return listAudioDevices();
  } catch (e) {
    return [];
  }
});

// IPC：本机设备桥状态（启用+网关+token+连接状态）
ipcMain.handle("device-bridge:get", () => {
  const p = loadGuiPrefs();
  const db = p.deviceBridge || {};
  return {
    enabled: !!db.enabled,
    authorized: !!db.authorized, // 客户端本地授权状态
    gateway: db.gateway || "",
    tokenSet: !!(db.token || ""),
    // 设备接入面（/device/online 等）走服务端 requireToken，只认 X-API-Key。
    // 渲染进程要用同一份令牌去拉设备列表，而此前只给了 tokenSet（布尔），
    // 于是渲染侧永远拿不到明文 ⇒ 设备页恒 401（显示为空列表）。
    // 明文本来就存在本机 gui-prefs.json，不新增任何密钥存储面；
    // 且只经 contextBridge 送到渲染进程，不落日志、不进 URL。
    token: db.token || "",
    connected: !!deviceBridge,
    deviceId: deviceBridgeId,
    address: deviceBridgeAddr,
    screensueDisplay: db.screensueDisplay || "0",
    exec: db.exec || {},
    authSchedule: db.authSchedule || {},
    audio: db.audio || {},
  };
});

// IPC：设置本机授权状态（客户端鉴权：仅写本地 prefs + 重发 hello，不经服务端）
ipcMain.handle("device-bridge:setAuthorized", (_, auth) => {
  setDeviceAuthorized(!!auth);
  return { authorized: !!auth };
});

// IPC：配置本机设备桥（开关+网关+token），保存并动态启停
ipcMain.handle("device-bridge:set", (_, cfg) => {
  const cur = loadGuiPrefs();
  const db = Object.assign({}, cur.deviceBridge || {}, cfg || {});
  const next = Object.assign({}, cur, { deviceBridge: db });
  guiPrefs = saveGuiPrefs(next);
  // 动态应用：停止现有桥，按新配置启动
  try {
    stopDeviceBridge();
  } catch (e) {}
  if (db.enabled && db.gateway && db.token) {
    try {
      startDeviceBridge({ url: db.gateway, apiKey: db.token });
    } catch (e) {
      console.error("[device-bridge] start failed: " + e.message);
    }
  }
  return {
    enabled: !!db.enabled,
    gateway: db.gateway || "",
    connected: !!deviceBridge,
    deviceId: deviceBridgeId,
    address: deviceBridgeAddr,
    screensueDisplay: db.screensueDisplay || "0",
    exec: db.exec || {},
    authSchedule: db.authSchedule || {},
    audio: db.audio || {},
  };
});

// IPC：读取偏好
ipcMain.handle("prefs:get", () => {
  return loadGuiPrefs();
});
// IPC：写入并应用偏好
ipcMain.handle("prefs:set", (_, p) => {
  const cur = loadGuiPrefs();
  const next = Object.assign({}, cur, p || {});
  if (typeof next.autoLaunch === "boolean") {
    next.autoLaunch = applyAutoLaunch(next.autoLaunch)
      ? next.autoLaunch
      : false;
  }
  guiPrefs = saveGuiPrefs(next);
  return guiPrefs;
});
