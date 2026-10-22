/**
 * 设备页的纯逻辑：本机 device_id 兜底与在线设备列表解析。
 *
 * 从 pages/DevicePage.ets 抽出（非 UI，可被其它页面/桥复用）。
 */

import { DeviceInfo } from '../model/Model';
import { connStore } from './ConnStore';

// ===== 二级页面路由 id（页面与一级入口列表共用）=====
export const SUB_NONE: string = '';
export const SUB_LOCAL: string = 'local';
export const SUB_CAPS: string = 'caps';
export const SUB_GATEWAY: string = 'gateway';
export const SUB_LIST: string = 'list';

/**
 * 本机 device_id：桥里已有就用桥的，其次读持久化，都没有则生成一个并落盘。
 * 生成后必须持久化，否则每次冷启动换 id，网关侧会累积成一堆幽灵设备。
 *
 * 判定顺序与原 DevicePage.aboutToAppear 一致：桥的 id 优先于持久化的 id。
 */
export function resolveDeviceId(bridgeId: string): string {
  let id: string = bridgeId;
  if (id.length === 0) {
    id = connStore.getDeviceId();
  }
  if (id.length === 0) {
    id = 'ohos-' + Date.now().toString(36);
    try {
      connStore.saveDeviceId(id);
    } catch (e) {
      // ignore persist failure
    }
  }
  return id;
}

/**
 * 解析 /device/online 响应体。
 *
 * apiClient 已自动前置 /api/v1，调用方只写其后的部分
 * （否则会拼成 /api/v1/api/v1/device/online 并 404）。
 */
export function parseOnlineDevices(parsed: Record<string, Object>): DeviceInfo[] {
  const devs: Object = parsed['devices'];
  const list: DeviceInfo[] = [];
  if (devs === undefined || devs === null) {
    return list;
  }
  const arr: Object[] = devs as Object[];
  for (let i = 0; i < arr.length; i++) {
    const d: Record<string, Object> = arr[i] as Record<string, Object>;
    const capsArr: Object = d['caps'];
    const caps: string[] = [];
    if (capsArr !== undefined && capsArr !== null) {
      const cArr: Object[] = capsArr as Object[];
      for (let j = 0; j < cArr.length; j++) {
        caps.push(cArr[j] as string);
      }
    }
    const info: DeviceInfo = {
      deviceId: d['device_id'] as string ?? '',
      name: d['name'] as string ?? '',
      kind: d['kind'] as string ?? '',
      online: true,
      authorized: d['authorized'] as boolean ?? false,
      caps: caps,
    };
    list.push(info);
  }
  return list;
}
