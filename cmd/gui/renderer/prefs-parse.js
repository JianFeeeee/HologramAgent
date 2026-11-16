// GUI 偏好文件（gui-prefs.json）的**解析与归一化**（纯函数，无 fs/无全局）。
//
// ## 为什么单独一个模块
//
// 这段逻辑原本是 `main.js` 里 `loadGuiPrefs()` 的一部分：读文件 → 去 BOM →
// JSON.parse → 逐字段合并默认值。判据只能正则匹配
// `readFileSync(GUI_PREFS_FILE[\s\S]{0,80}replace(/^\uFEFF/` —— 读源码断言。
//
// 抽出来后判据直接喂字符串断言**归一化结果**：带 BOM 的能解析、
// 缺字段的拿到默认值、deviceBridge 子字段逐个补齐。
//
// ## 为什么去 BOM 是硬要求（真实故障）
//
// Windows 上 PowerShell `Set-Content -Encoding UTF8`、记事本等工具改过这个
// 文件，会在开头写入 UTF-8 BOM（EF BB BF）。JSON.parse 遇到它直接抛错 →
// 调用方 catch → 返回**默认 prefs** ⇒ silentStart / exitToTray / deviceBridge
// 全部被重置，而界面上开关"像是保存了但不起作用"。
//
// 实测踩过：判据脚本用 Set-Content 改 silentStart，文件带 BOM 后静默启动
// 不生效。而 loadConnections 早就有去 BOM、loadGuiPrefs 没有 —— 两个读取点
// 不一致，是实打实的疏漏。
//
// ## 接口
//
//	parsePrefs(text) → prefs 对象（永远返回完整结构，不会抛）
//	  · 空/坏 JSON/BOM → 默认值
//	  · 缺字段 → 逐个用默认值补
//
// 调用方（main.js）负责读文件；本模块只管"文本 → 结构"。
(function (root, factory) {
  if (typeof module === "object" && module.exports) {
    module.exports = factory();
  } else {
    root.PrefsParse = factory();
  }
})(typeof self !== "undefined" ? self : this, function () {
  // 默认偏好（唯一一处定义；原实现有两个 return 分支各写一遍，容易漂移）
  function defaultPrefs() {
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

  function parsePrefs(text) {
    if (typeof text !== "string" || text === "") return defaultPrefs();
    var d;
    try {
      d = JSON.parse(text.replace(/^\uFEFF/, "")); // ★ 先去 BOM
    } catch (e) {
      return defaultPrefs();
    }
    if (!d || typeof d !== "object") return defaultPrefs();

    var db = d.deviceBridge || {};
    return {
      autoLaunch: !!d.autoLaunch,
      silentStart: !!d.silentStart,
      exitToTray: d.exitToTray === undefined ? true : !!d.exitToTray,
      deviceBridge: {
        enabled: !!db.enabled,
        authorized: !!db.authorized,
        gateway: db.gateway || "",
        token: db.token || "",
        screensueDisplay: db.screensueDisplay || "0",
        screensueDuration:
          db.screensueDuration === undefined ||
          db.screensueDuration === null ||
          db.screensueDuration === ""
            ? "5"
            : String(db.screensueDuration),
        exec: db.exec || {},
        authSchedule: db.authSchedule || {},
        audio: db.audio || {},
      },
    };
  }

  return { parsePrefs: parsePrefs, defaultPrefs: defaultPrefs };
});
