// GUI 与 waiter 共同支持的 computeruse **动作清单**（唯一事实源）。
//
// ## 为什么单独一个模块
//
// 这些动作名原本散落在三处：
//   · cmd/gui/main.js 的 switch (action)
//   · cmd/waiter/device.go 的 execComputeruseLinux 的 switch
//   · computeruse.test.mjs 里硬编码的数组（判据自己的副本）
//
// 判据那个副本就是问题：它和实现是三份手抄，漂移了没人发现。
// 抽成数据文件后，判据拿**同一份清单**去断言两侧都实现了它。
//
// ## 这不是「改成数据驱动」
//
// 动作的**语义**各不相同（drag 要起止点、hotkey 要组合键、display 只读），
// 没法用一段通用代码实现。所以这不是把 switch 换成查表，而是把
// **「有哪些动作」这个事实**集中到一处 —— 实现仍各自按语义写。
//
// ## 字段
//
//	name        动作名（switch 里用的字符串）
//	desc        中文说明（判据输出用）
//	waiterOnly  某些动作只有一侧支持时的标记（如 display 无 waiter 实现）
//
// 判据用法：遍历清单，断言两侧源码里都有对应 case。
(function (root, factory) {
  if (typeof module === "object" && module.exports) {
    module.exports = factory();
  } else {
    root.ComputerUseActions = factory();
  }
})(typeof self !== "undefined" ? self : this, function () {
  // 基础动作（两侧都支持）
  var BASIC = [
    { name: "click", desc: "左键单击" },
    { name: "doubleclick", desc: "双击" },
    { name: "right", desc: "右键单击" },
    { name: "move", desc: "移动光标" },
    { name: "type", desc: "输入文本" },
    { name: "keypress", desc: "按键（可带修饰键）" },
    { name: "scroll", desc: "滚轮" },
  ];

  // 进阶动作（两侧都支持，除 display 外）
  var ADVANCED = [
    { name: "drag", desc: "拖拽" },
    { name: "hover", desc: "悬停（触发 tooltip）" },
    { name: "mousedown", desc: "按键按下（与 mouseup 配对做按住）" },
    { name: "mouseup", desc: "按键释放" },
    { name: "tripleclick", desc: "三击选中整行" },
    { name: "middleclick", desc: "中键点击" },
    { name: "hotkey", desc: "组合键" },
    { name: "wait", desc: "显式等待（等 UI/动画完成）" },
    { name: "display", desc: "显示器几何信息（多屏定位）", waiterOnly: false, guiOnly: true },
  ];

  return { basic: BASIC, advanced: ADVANCED, all: BASIC.concat(ADVANCED) };
});
