/**
 * 设备桥协议：消息结构、帧构造与分块切片。
 *
 * 与 homed 的 remotedevice 插件对齐（internal/plugins/remotedevice）。
 * 从 common/DeviceBridge.ets 抽出：这里只有"协议形状"和"帧怎么拼"，
 * 没有任何 socket、状态机与重连逻辑 —— 那些留在 DeviceBridgeClient 里。
 *
 * 注意：DeviceBridgeClient 的对外方法名与语义不因本文件而改变，
 * 各 send* 方法仍是"拼帧 + 发出去"两步，只是第一步搬到了这里。
 */

import { CapResult } from './BridgeCaps';
import { appVersion } from './AppVersion';

// ===== 协议消息（与 remotedevice 插件对齐）=====

export interface HelloDeviceInfo {
  hostname: string;
  platform: string;
  arch: string;
  os_release: string;
  version: string;
  cpus: number;
}

export interface HelloDevice {
  device_id: string;
  name: string;
  kind: string;
  authorized: boolean;
  caps: string[];
  info: HelloDeviceInfo;
}

export interface HelloMessage {
  op: string;
  device: HelloDevice;
}

export interface BindMessage {
  op: string;
  device_id: string;
  token: string;
}

export interface CmdReply {
  op: string; // 'cmd_result'
  req_id: string;
  status: string;
  output: string;
  error: string;
}

export interface DataStartMessage {
  op: string;
  req_id: string;
  kind: string;
  mime: string;
  total: number;
  chunk_size: number;
}

export interface DataEndMessage {
  op: string;
  req_id: string;
  status: string;
  total?: number;
  error?: string;
}

export const CHUNK_SIZE: number = 8192;

// ===== 命令处理器回调 =====
// 返回 CapResult；二进制大结果通过 dataHandler 分块回传。
export type BridgeCmdHandler = (reqId: string, command: string) => Promise<CapResult>;

// ===== 帧构造 =====

export function bridgeHelloFrame(deviceId: string, name: string, kind: string,
                                 caps: string[], hostname: string,
                                 authorized: boolean): string {
  const info: HelloDeviceInfo = {
    hostname: hostname,
    platform: 'OpenHarmony',
    arch: '',
    os_release: '',
    version: appVersion(),
    cpus: 0,
  };
  const device: HelloDevice = {
    device_id: deviceId,
    name: name,
    kind: kind,
    authorized: authorized,
    caps: caps,
    info: info,
  };
  const hello: HelloMessage = { op: 'hello', device: device };
  return JSON.stringify(hello);
}

export function bridgeBindFrame(deviceId: string, token: string): string {
  const bind: BindMessage = {
    op: 'bind',
    device_id: deviceId,
    token: token,
  };
  return JSON.stringify(bind);
}

export function bridgeResultFrame(reqId: string, status: string,
                                  output: string, errMsg: string): string {
  const result: CmdReply = {
    op: 'cmd_result',
    req_id: reqId,
    status: status,
    output: output,
    error: errMsg,
  };
  return JSON.stringify(result);
}

export function bridgeDataStartFrame(reqId: string, kind: string, mime: string,
                                     total: number): string {
  const startMsg: DataStartMessage = {
    op: 'cmd_data_start',
    req_id: reqId,
    kind: kind,
    mime: mime,
    total: total,
    chunk_size: CHUNK_SIZE,
  };
  return JSON.stringify(startMsg);
}

export function bridgeDataEndFrame(reqId: string): string {
  const endMsg: DataEndMessage = {
    op: 'cmd_data_end',
    req_id: reqId,
    status: 'ok',
  };
  return JSON.stringify(endMsg);
}

export function bridgeEventFrame(deviceId: string, eventType: string, detail: string): string {
  const payload: Record<string, string> = { 'detail': detail };
  const msg: Record<string, Object> = {
    'op': 'event',
    'device_id': deviceId,
    'type': eventType,
    'payload': payload,
  };
  return JSON.stringify(msg);
}

export function bridgeStatusFrame(deviceId: string, status: string): string {
  const msg: Record<string, Object> = {
    'op': 'status',
    'device_id': deviceId,
    'status': status,
  };
  return JSON.stringify(msg);
}

/** 按 CHUNK_SIZE 切二进制；切片顺序即发送顺序。 */
export function bridgeChunkSlices(bytes: Uint8Array): Uint8Array[] {
  const out: Uint8Array[] = [];
  for (let off: number = 0; off < bytes.byteLength; off += CHUNK_SIZE) {
    const end: number = Math.min(off + CHUNK_SIZE, bytes.byteLength);
    out.push(bytes.slice(off, end));
  }
  return out;
}
