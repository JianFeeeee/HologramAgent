// 系统通知的**去重决策**（纯逻辑，无 IO/无全局）。
//
// ## 为什么单独一个模块
//
// 这段逻辑原本内联在 `renderer/app.js` 的 `notifyTurnOnce()` 里：
//
//	if (!state._notifyState || state._notifyState.turnSig !== sig) {
//	  state._notifyState = { turnSig: sig, notified: false };
//	}
//	if (state._notifyState.notified) return;
//	state._notifyState.notified = true;
//	notifyTurn(title, body, msgKey);
//
// 语义是「**同一轮只通知一次**」：一轮对话会产生多个 assistant 消息，
// 若每条都弹通知，用户会被连续轰炸（实测形态：模型输出 5 段，弹 5 次）。
//
// 内联在 147KB 的 app.js 里时，判据只能正则匹配「源码里有没有
// _notifyState.turnSig !== sig」—— 读源码断言，换实现就假红。
//
// ## 接口
//
//	decideNotify(prev, sig) → { state, notify: bool }
//	  prev : 上一次的 state（{turnSig, notified} 或 null/ undefined）
//	  sig  : 本轮的标识（同一轮相同）
//	  返回 : 新 state + 本次是否应当弹通知
//
// 纯函数：不改入参、不碰全局。调用方负责把返回的 state 写回。
(function (root, factory) {
  if (typeof module === "object" && module.exports) {
    module.exports = factory();
  } else {
    root.NotifyOnce = factory();
  }
})(typeof self !== "undefined" ? self : this, function () {
  function decideNotify(prev, sig) {
    // 新一轮（sig 变化）⇒ 重置去重状态
    if (!prev || prev.turnSig !== sig) {
      return { state: { turnSig: sig, notified: true }, notify: true };
    }
    // 同一轮且已通知过 ⇒ 不再弹
    if (prev.notified) return { state: prev, notify: false };
    return { state: { turnSig: sig, notified: true }, notify: true };
  }
  return { decideNotify: decideNotify };
});
