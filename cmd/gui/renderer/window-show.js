// 「从托盘/菜单唤起主窗口」的**决策**（纯逻辑，无 Electron API）。
//
// ## 为什么单独一个模块
//
// 这段逻辑原本内联在 `main.js` 的 `showMainWindow()` 里：
//
//	if (mainWindow && !mainWindow.isDestroyed()) {
//	  if (wc.isCrashed()) { wc.reload(); }   // 崩溃了先重载
//	  if (!mainWindow.isVisible()) mainWindow.show();
//	  mainWindow.focus();
//	  return;
//	}
//	createWindow(true);                       // 窗口没了就重建
//
// 判据只能正则匹配「源码里有没有 if (wc.isCrashed())」—— 读源码断言。
//
// ## 为什么要这个决策（真实故障，2026-10-05）
//
// 长时间后台后 renderer 可能已被回收：窗口句柄还在、`isVisible()` **仍为 true**，
// 但内容是空的。此时只判 isVisible() 会跳过 show()，从托盘打开就是一片白，
// **而且永远不会自愈**（不会再有别的路径去 reload）。
//
// 所以必须先判 `isCrashed()`：崩了要 reload 才能出内容。
// 反过来 `isLoading()` 不能 reload —— 还在加载时重载会把正在进行的加载
// 推倒重来，白等一轮。
//
// ## 接口
//
//	decideShow(state) → { action, reload, show, focus }
//	  state: { exists, destroyed, crashed, loading, visible }
//	  action: "create" | "reload-show" | "show" | "focus-only"
//
// 纯函数：只读入参、不碰 Electron。
(function (root, factory) {
  if (typeof module === "object" && module.exports) {
    module.exports = factory();
  } else {
    root.WindowShow = factory();
  }
})(typeof self !== "undefined" ? self : this, function () {
  function decideShow(s) {
    s = s || {};
    // 窗口不存在 / 已销毁 ⇒ 重建（否则托盘菜单点了没反应，进程成僵尸）
    if (!s.exists || s.destroyed) {
      return { action: "create", reload: false, show: false, focus: false };
    }
    // renderer 已死 ⇒ 必须先 reload，否则 show 出来是白屏且不会自愈。
    // ★ loading 时**不** reload：会把正在进行的加载推倒重来。
    var reload = !!s.crashed && !s.loading;
    if (reload) {
      return { action: "reload-show", reload: true, show: true, focus: true };
    }
    if (!s.visible) {
      return { action: "show", reload: false, show: true, focus: true };
    }
    return { action: "focus-only", reload: false, show: false, focus: true };
  }

  return { decideShow: decideShow };
});
