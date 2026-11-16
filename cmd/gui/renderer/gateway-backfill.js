// 设备桥网关**回写决策**（纯逻辑，无 IO/无全局）。
//
// ## 为什么单独一个模块
//
// 这段逻辑原本内联在 `renderer/app.js` 的 `loadDiscoveredGateway()` 里：
//
//	if (cur && !cur.gateway && state.discoveredGateway) {
//	  await window.homeagent.deviceBridge.set({ gateway: state.discoveredGateway });
//	}
//
// 它只有两行，但那两行是**安全契约**：
//
//	· 只在本地**未配置**时才写 —— 手填值优先（用户可能故意指向内网直连
//	  或灰度实例）。写错了就是「自动发现冲掉用户设置」。
//	· 只传 gateway —— 发现端点不返回设备令牌，传 token 会把已有的写坏。
//
// 内联在 147KB 的 app.js 里时，判据只能正则匹配「源码里有没有 cur && !cur.gateway」。
// 抽出成纯函数后，判据直接调它断言**决策结果**（返回 null 还是 {gateway}）。
//
// ## 接口
//
//	decideBackfill(current, discovered) → string | null
//	  current    : deviceBridge.get() 的当前配置（{gateway?, token?}）或 null
//	  discovered : 服务端 /device/gateway 发现的地址（可能为空串）
//	  返回       : 非空字符串 = 该回写的 gateway；null = 不回写
//
// 注意它**不构造 payload**：调用方只 set({ gateway })。把 payload 也放进这里
// 会让"只传 gateway"这条契约从一个字段变成一层间接 —— 判据反而更难钉住。
(function (root, factory) {
  if (typeof module === "object" && module.exports) {
    module.exports = factory();
  } else {
    root.GatewayBackfill = factory();
  }
})(typeof self !== "undefined" ? self : this, function () {
  function decideBackfill(current, discovered) {
    var gw = typeof discovered === "string" ? discovered.trim() : "";
    if (!gw) return null; // 没发现任何地址
    if (current && current.gateway) return null; // 本地已配置：手填优先
    return gw;
  }

  return { decideBackfill: decideBackfill };
});
