import { deviceBridge } from './DeviceBridge';
import {
  CapResult,
  DataChunkSender,
  capScreensee,
  capCamerasue,
  capClipboardSee,
  capClipboardsue,
  capSpeakerUse,
  capDeviceInfo,
  capStatus,
} from './BridgeCaps';
import { parseScreensue, ScreensuePayload } from './ScreensueHtml';
import { connStore } from './ConnStore';
import { common } from '@kit.AbilityKit';

// screensue 展示回调由根 UI 注册：窄屏整页，宽屏右侧内容栏。
export type ScreensueHandler = (payload: ScreensuePayload) => void;

let screensueHandler: ScreensueHandler | null = null;
let appContext: common.UIAbilityContext | null = null;

export function registerScreensueHandler(handler: ScreensueHandler): void {
  screensueHandler = handler;
}

export function setBridgeAppContext(ctx: common.UIAbilityContext): void {
  appContext = ctx;
}

/** 解析裸能力名或过渡期 homeagent-* 命令；参数正文不裁剪，避免改变推送内容。 */
function splitCapability(command: string): string[] {
  let start: number = 0;
  while (start < command.length && isCommandSpace(command.charAt(start))) {
    start = start + 1;
  }
  let cmd: string = command.substring(start);
  if (cmd.startsWith('homeagent-')) {
    cmd = cmd.substring('homeagent-'.length);
  }
  let idx: number = -1;
  for (let i: number = 0; i < cmd.length; i++) {
    if (isCommandSpace(cmd.charAt(i))) {
      idx = i;
      break;
    }
  }
  if (idx < 0) {
    return [cmd];
  }
  const out: string[] = [cmd.substring(0, idx), cmd.substring(idx + 1)];
  return out;
}

function isCommandSpace(ch: string): boolean {
  return ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r';
}

function hasArgs(args: string): boolean {
  return args.trim().length > 0;
}

async function executeCommand(reqId: string, command: string): Promise<CapResult> {
  const parts: string[] = splitCapability(command);
  const name: string = parts[0];
  const args: string = parts.length > 1 ? parts[1] : '';

  if (name === 'screensee') {
    if (hasArgs(args)) {
      return errRes('screensee 不接受额外参数');
    }
    return capScreensee();
  }
  if (name === 'screensue') {
    if (!hasArgs(args)) {
      return errRes('screensue 需要展示内容');
    }
    const payload: ScreensuePayload = parseScreensue(args);
    if (payload.content.length === 0) {
      return errRes('screensue 需要展示内容');
    }
    if (screensueHandler !== null) {
      screensueHandler(payload);
      return okRes('内容已显示');
    }
    return errRes('展示界面尚未就绪，请保持应用在前台后重试');
  }
  if (name === 'camerasue') {
    if (appContext === null) {
      return errRes('相机能力尚未就绪，请保持应用在前台后重试');
    }
    // 录像走二进制分块：把「往本请求回传字节」的能力注入能力实现，
    // 避免 BridgeCaps 反向 import DeviceBridge 形成循环依赖。
    const sender: DataChunkSender = (kind: string, mime: string, bytes: Uint8Array) => {
      deviceBridge.sendDataChunked(reqId, kind, mime, bytes);
    };
    return capCamerasue(appContext, args, sender);
  }
  if (name === 'clipboardsee') {
    if (hasArgs(args)) {
      return errRes('clipboardsee 不接受额外参数');
    }
    if (appContext === null) {
      return errRes('应用界面尚未就绪，请保持应用在前台后重试');
    }
    return capClipboardSee(appContext);
  }
  if (name === 'clipboardsue') {
    if (!hasArgs(args)) {
      return errRes('clipboardsue 需要写入文字');
    }
    return capClipboardsue(args);
  }
  if (name === 'speakeruse') {
    if (!hasArgs(args)) {
      return errRes('speakeruse 需要朗读文字');
    }
    return capSpeakerUse(args);
  }
  if (name === 'status') {
    if (hasArgs(args)) {
      return errRes('status 不接受额外参数');
    }
    return capStatus(connStore.getDeviceId());
  }
  if (name === 'deviceinfo') {
    if (hasArgs(args)) {
      return errRes('deviceinfo 不接受额外参数');
    }
    return capDeviceInfo(connStore.getDeviceId(), connStore.getDeviceName());
  }
  return errRes('不支持的本机能力：' + name);
}

function okRes(output: string): CapResult {
  const r: CapResult = { status: 'ok', output: output, error: '' };
  return r;
}

function errRes(errMsg: string): CapResult {
  const r: CapResult = { status: 'error', output: '', error: errMsg };
  return r;
}

/** 安装命令处理器到 bridge 单例。 */
export function installCmdRouter(): void {
  deviceBridge.setCmdHandler(executeCommand);
}
