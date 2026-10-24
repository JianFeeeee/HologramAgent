import { common } from '@kit.AbilityKit';
import { http } from '@kit.NetworkKit';
import { deviceBridge } from './DeviceBridge';
import { installCmdRouter, setBridgeAppContext } from './BridgeRouter';
import { LOCAL_DEVICE_CAPS, shutdownSpeakerUse } from './BridgeCaps';
import { connStore } from './ConnStore';
import { ConnectionConfig } from '../model/Model';

export { LOCAL_DEVICE_CAPS } from './BridgeCaps';

let bridgeStarting: boolean = false;
let foregroundActive: boolean = false;
let rootUIReady: boolean = false;
let bridgeGeneration: number = 0;
let stateTrackingReady: boolean = false;

function ensureBridgeStateTracking(): void {
  if (stateTrackingReady) {
    return;
  }
  stateTrackingReady = true;
  AppStorage.setOrCreate<boolean>('deviceBridgeConnected', false);
  deviceBridge.setStateListener((open: boolean) => {
    AppStorage.set<boolean>('deviceBridgeConnected', open);
  });
}

/**
 * 把门户 HTTP 地址转换为同源设备桥 WebSocket 地址（**回退路径**）。
 *
 * 网关改造为子域反代后位于 devices.<基域名>，而基域名与子域标签都是**服务端
 * 配置**，客户端拼不出来。正常路径是先调 discoverGateway() 问服务端；
 * 本函数只在「服务端没有发现端点」（老版本 HologramAgent）时兜底，
 * 保留旧部署的可用性。
 */
export function deviceGatewayUrl(base: string): string {
  let trimmed: string = base.trim();
  while (trimmed.length > 0 && trimmed.charAt(trimmed.length - 1) === '/') {
    trimmed = trimmed.substring(0, trimmed.length - 1);
  }
  let scheme: string = 'ws://';
  let rest: string = trimmed;
  if (trimmed.startsWith('https://')) {
    scheme = 'wss://';
    rest = trimmed.substring('https://'.length);
  } else if (trimmed.startsWith('http://')) {
    rest = trimmed.substring('http://'.length);
  } else if (trimmed.startsWith('wss://')) {
    scheme = 'wss://';
    rest = trimmed.substring('wss://'.length);
  } else if (trimmed.startsWith('ws://')) {
    rest = trimmed.substring('ws://'.length);
  }
  return scheme + rest + '/api/v1/device/ws';
}

/**
 * 应用进入前台后建立全局设备桥。它不再依赖用户先打开“设备”Tab，
 * 因而 screensue、clipboardsee 等前台能力从主页面加载后即可接收。
 */
/**
 * 向门户询问设备网关的**权威地址**。
 *
 * 为什么必须问而不是自己拼：网关现在挂在 devices.<基域名> 子域上，基域名
 * （webui.base_domain，默认 localhost）与子域标签（插件声明里可改）都在
 * 服务端，客户端无从得知。服务端作答是唯一不会漂移的做法。
 *
 * 失败/老版本（404）不报错，返回空串让调用方回退到 deviceGatewayUrl()——
 * 发现是增强而非必需。
 */
async function discoverGateway(portalUrl: string, apiKey: string): Promise<string> {
  let base: string = portalUrl.trim();
  if (base.length === 0) {
    return '';
  }
  // 用户配置里可能填的是完整网关地址：截到门户根再拼发现路径
  const cut: number = base.indexOf('/api/v1/');
  if (cut >= 0) {
    base = base.substring(0, cut);
  }
  while (base.length > 0 && base.charAt(base.length - 1) === '/') {
    base = base.substring(0, base.length - 1);
  }
  try {
    const r = await http.createHttp().request(base + '/api/v1/device/gateway', {
      method: http.RequestMethod.GET,
      header: { 'X-API-Key': apiKey } as Record<string, string>,
      connectTimeout: 5000,
      readTimeout: 5000,
    });
    if (r.responseCode !== 200) {
      return '';
    }
    const body: string = typeof r.result === 'string' ? r.result : '';
    const parsed: Record<string, Object> = JSON.parse(body) as Record<string, Object>;
    // 服务端明确报告不可用（没有声明设备网关反代）时不返回地址，
    // 让调用方回退，而不是拿着一个连不上的 URL 反复重连。
    if (parsed['available'] !== true) {
      return '';
    }
    // 优先**门户同源形态**（url_portal：同一 host、同一端口）。
    //
    // 原因：子域形态 devices.<基域名> 依赖 DNS 解析，而 *.localhost 只有
    // 浏览器内置该特例（RFC 6761）—— 应用内 HTTP/WS 客户端走系统解析器，
    // 通常解析不到。门户同源形态无任何 DNS 依赖，永远可解析。
    const portal: string = parsed['url_portal'] as string;
    if (portal !== undefined && portal !== null && portal.length > 0) {
      return portal;
    }
    const url: string = parsed['url'] as string;
    return url === undefined || url === null ? '' : url;
  } catch (e) {
    // 网络失败 / 老版本无此端点：静默回退
    return '';
  }
}

/** 解析设备桥最终使用的网关地址：优先服务端发现，回退同源推导。 */
async function resolveGatewayUrl(portalUrl: string, apiKey: string): Promise<string> {
  const discovered: string = await discoverGateway(portalUrl, apiKey);
  if (discovered.length > 0) {
    return discovered;
  }
  return deviceGatewayUrl(portalUrl);
}

export async function startForegroundBridge(context: common.UIAbilityContext): Promise<void> {
  foregroundActive = true;
  setBridgeAppContext(context);
  installCmdRouter();
  ensureBridgeStateTracking();
  if (!rootUIReady || deviceBridge.isConnected() || bridgeStarting) {
    return;
  }
  const cur: ConnectionConfig | null = connStore.getCurrentConnection();
  if (cur === null || cur.url.length === 0 || cur.apiKey.length === 0) {
    return;
  }
  bridgeStarting = true;
  bridgeGeneration = bridgeGeneration + 1;
  const generation: number = bridgeGeneration;
  const deviceId: string = connStore.ensureDeviceId();
  try {
    const gatewayUrl: string = await resolveGatewayUrl(cur.url, cur.apiKey);
    await deviceBridge.connect(
      gatewayUrl, cur.apiKey, deviceId,
      LOCAL_DEVICE_CAPS, 'ohos-phone', connStore.getDeviceAuth(), connStore.getDeviceName());
    if (!foregroundActive || generation !== bridgeGeneration) {
      deviceBridge.disconnect();
    }
  } catch (e) {
    // DeviceBridge 自己会安排重连；前台启动不弹技术错误打扰用户。
  }
  if (generation === bridgeGeneration) {
    bridgeStarting = false;
  }
}

/** 根页面挂载完成后才连接，避免首条 screensue 到达时展示层尚未注册。 */
export function markForegroundBridgeUIReady(context: common.UIAbilityContext): void {
  rootUIReady = true;
  startForegroundBridge(context);
}

/** 后台不接受需要前台 UI/剪贴板授权的命令。 */
export function stopForegroundBridge(): void {
  foregroundActive = false;
  bridgeGeneration = bridgeGeneration + 1;
  bridgeStarting = false;
  shutdownSpeakerUse();
  deviceBridge.disconnect();
}

/** 连接配置切换或修改后立即让设备桥使用新地址和 Token。 */
export async function restartForegroundBridge(context: common.UIAbilityContext): Promise<void> {
  bridgeGeneration = bridgeGeneration + 1;
  bridgeStarting = false;
  deviceBridge.disconnect();
  await startForegroundBridge(context);
}
