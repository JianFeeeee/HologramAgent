import { image } from '@kit.ImageKit';
import { util } from '@kit.ArkTS';
import { pasteboard } from '@kit.BasicServicesKit';
import { deviceInfo } from '@kit.BasicServicesKit';
import { textToSpeech } from '@kit.CoreSpeechKit';
import { componentSnapshot } from '@kit.ArkUI';
import { abilityAccessCtrl, common, PermissionRequestResult, Permissions } from '@kit.AbilityKit';
import { camera, cameraPicker } from '@kit.CameraKit';
import { fileIo, fileUri } from '@kit.CoreFileKit';
import { appVersion } from './AppVersion';

// ===== 能力结果 =====

/** 与 BridgeRouter 实际支持的本机命令保持一一对应。 */
export const LOCAL_DEVICE_CAPS: string[] = [
  'status',
  'deviceinfo',
  'screensee',
  'screensue',
  'clipboardsee',
  'clipboardsue',
  'speakeruse',
  'camerasue',
];

export interface CapResult {
  status: string; // 'ok' | 'error'
  output: string;
  error: string;
  // chunked 为 true 时表示结果**已由能力内部经二进制分块回传**（如录像），
  // DeviceBridge 不要再发 cmd_result；否则网关会把后续分块挂在一条已完成的
  // 请求上，或先用 cmd_result 结束、再来的 cmd_data_start 找不到归属。
  chunked?: boolean;
}

interface DeviceStatusPayload {
  device_id: string;
  status: string;
  hostname: string;
  platform: string;
  arch: string;
  uptime: number;
}

interface DeviceDetails {
  hostname: string;
  platform: string;
  arch: string;
  os_release: string;
  version: string;
  cpus: number;
  brand: string;
  manufacturer: string;
  model: string;
  series: string;
  sdk_api_version: number;
  security_patch: string;
  abi_list: string;
  device_type: string;
}

interface DeviceInfoPayload {
  device_id: string;
  name: string;
  kind: string;
  caps: string[];
  info: DeviceDetails;
}

const APP_STARTED_AT: number = Date.now();

function okResult(output: string): CapResult {
  const r: CapResult = { status: 'ok', output: output, error: '' };
  return r;
}

function errResult(errMsg: string): CapResult {
  const r: CapResult = { status: 'error', output: '', error: errMsg };
  return r;
}

/**
 * 二进制分块发送回调。由 BridgeRouter 注入（它持有 deviceBridge + reqId），
 * BridgeCaps 因此不必 import DeviceBridge —— 否则 DeviceBridge 为取 CapResult
 * 而 import BridgeCaps，两边成环。分层也更干净：能力实现不碰 socket。
 */
export type DataChunkSender = (kind: string, mime: string, bytes: Uint8Array) => void;

// chunkedResult：结果已由能力自己分块发出，不再回 cmd_result。
function chunkedResult(output: string): CapResult {
  const r: CapResult = { status: 'ok', output: output, error: '', chunked: true };
  return r;
}

// ===== screensee：截取本应用当前画面（前台时为整屏可见内容）=====

const SNAPSHOT_COMPONENT_ID: string = 'homeagent-root';

/** 根组件 id（Index 的根 Stack 设置同名 .id()），截屏时按此定位。 */
export function snapshotComponentId(): string {
  return SNAPSHOT_COMPONENT_ID;
}

async function captureScreenPixelMap(): Promise<image.PixelMap> {
  const pm: image.PixelMap = await componentSnapshot.get(SNAPSHOT_COMPONENT_ID);
  return pm;
}

/**
 * screensee 实现：截本应用画面，缩放到最大宽度 720px 后压成 jpeg base64 data URL。
 * 说明：鸿蒙三方应用无法无弹窗截取整个系统屏幕（CUSTOM_SCREEN_CAPTURE 为系统权限），
 * 此处回传应用自身前台画面；应用在前台运行时即为用户正在看到的界面。
 */
export async function capScreensee(): Promise<CapResult> {
  let full: image.PixelMap | null = null;
  let packer: image.ImagePacker | null = null;
  try {
    full = await captureScreenPixelMap();
    const info: image.ImageInfo = await full.getImageInfo();
    const maxW: number = 420;
    const maxH: number = 640;
    let scale: number = 1;
    if (info.size.width > maxW) {
      scale = maxW / info.size.width;
    }
    if (info.size.height * scale > maxH) {
      scale = maxH / info.size.height;
    }
    if (scale < 1) {
      await full.scale(scale, scale);
    }
    packer = image.createImagePacker();
    const opt: image.PackingOption = { format: 'image/jpeg', quality: 55 };
    const packed: ArrayBuffer = await packer.packing(full, opt);
    const helper: util.Base64Helper = new util.Base64Helper();
    const b64: string = helper.encodeToStringSync(new Uint8Array(packed));
    if (b64.length > 950000) {
      return errResult('当前画面数据过大，请稍后重试');
    }
    return okResult('data:image/jpeg;base64,' + b64);
  } catch (e) {
    return errResult('无法读取当前应用画面，请保持应用在前台后重试');
  } finally {
    if (packer !== null) {
      packer.release();
    }
    if (full !== null) {
      full.release();
    }
  }
}

// ===== camerasue：系统相机抓拍 =====
//
// 与桌面/CLI 端的实现路径不同：鸿蒙三方应用不能无界面地直接驱动摄像头
// （CameraKit 需要预览 surface + CAMERA 权限，且后台采集受限），能拿到
// “用户正在拍的这一张”的合规路径是系统相机选择器 cameraPicker —— 由系统
// 相机完成采集，本应用只取回结果文件。语义与桌面端一致：现在给 agent 拍一张。
//
// 结果落在应用沙箱（saveUri 指向 filesDir），不写系统媒体库，也就不需要
// READ_IMAGEVIDEO 这类受限权限。
const CAMERASUE_MAX_B64: number = 950000;

/** 录像回传上限：与网关 mediaDir 落盘模式配合，避免把设备内存/WS 打爆。 */
const CAMERASUE_MAX_VIDEO: number = 64 * 1024 * 1024;

/**
 * camerasue 实现。
 *
 * 参数语义与 homeagent-cmdrun 的说明一致：无参数 = 抓拍单张；
 * `<N秒>` = 录 N 秒视频。
 *
 * 视频为什么要走二进制分块：一段 10s 录像动辄数 MB，base64 后还要再膨胀
 * 1/3，既撑爆模型上下文也撑爆 WS 单帧。cameraPicker 本身支持 VIDEO
 * 模式（系统相机会直接进录像界面），取回文件后用 sendDataChunked 按
 * cmd_data_start/分块/cmd_data_end 回传——网关侧聚合后落盘成文件，agent 拿路径。
 * 这与 GUI/CLI 客户端的 camerasue 录像路径一致。
 */
export async function capCamerasue(context: common.UIAbilityContext,
                                  rawArgs: string,
                                  sendChunked: DataChunkSender | null): Promise<CapResult> {
  const raw: string = rawArgs.trim();
  let videoSeconds: number = 0;
  if (raw.length > 0) {
    const digits: RegExp = new RegExp('^\\d+$');
    if (!digits.test(raw)) {
      return errResult('camerasue 参数只接受纯数字秒数，如 camerasue 5');
    }
    videoSeconds = parseInt(raw, 10);
    if (videoSeconds <= 0 || videoSeconds > 300) {
      return errResult('录像时长需在 1~300 秒之间');
    }
  }
  const isVideo: boolean = videoSeconds > 0;

  const ext: string = isVideo ? '.mp4' : '.jpg';
  const filePath: string = context.filesDir + '/camerasue_' + Date.now().toString() + ext;
  try {
    // cameraPicker 要求 saveUri 指向的文件存在且可写，先建空文件占位
    const f: fileIo.File = fileIo.openSync(filePath,
      fileIo.OpenMode.READ_WRITE | fileIo.OpenMode.CREATE);
    fileIo.closeSync(f);
  } catch (e) {
    return errResult('无法创建相机输出文件');
  }

  try {
    const profile: cameraPicker.PickerProfile = {
      cameraPosition: camera.CameraPosition.CAMERA_POSITION_BACK,
      saveUri: fileUri.getUriFromPath(filePath),
    };
    if (isVideo) {
      profile.videoDuration = videoSeconds;
    }
    const mediaType: cameraPicker.PickerMediaType = isVideo
      ? cameraPicker.PickerMediaType.VIDEO
      : cameraPicker.PickerMediaType.PHOTO;
    const res: cameraPicker.PickerResult =
      await cameraPicker.pick(context, [mediaType], profile);
    if (res.resultCode !== 0 || res.resultUri.length === 0) {
      return errResult(isVideo ? '未获取到录像（可能被取消）' : '未获取到照片（可能被取消）');
    }
  } catch (e) {
    return errResult('相机不可用或未授权，请确认应用在前台并允许使用相机');
  }

  if (isVideo) {
    return readAndSendVideo(filePath, sendChunked);
  }
  return readPhotoAsBase64(filePath);
}

/** 读回录像并以二进制分块回传；网关聚合后落盘，agent 拿文件路径。 */
function readAndSendVideo(filePath: string, sendChunked: DataChunkSender | null): CapResult {
  let fd: number = -1;
  try {
    const stat: fileIo.Stat = fileIo.statSync(filePath);
    if (stat.size <= 0) {
      return errResult('录像文件为空，请重试');
    }
    if (stat.size > CAMERASUE_MAX_VIDEO) {
      return errResult('录像文件过大（超过 64MB），请缩短时长');
    }
    const buf: ArrayBuffer = new ArrayBuffer(stat.size);
    const rf: fileIo.File = fileIo.openSync(filePath, fileIo.OpenMode.READ_ONLY);
    fd = rf.fd;
    fileIo.readSync(fd, buf);
    fileIo.closeSync(rf);
    fd = -1;
    if (sendChunked === null) {
      return errResult('录像回传通道未就绪，请重试');
    }
    sendChunked('camera_video', 'video/mp4', new Uint8Array(buf));
    // 分块已代表本次请求的完整结果，DeviceBridge 不再回 cmd_result。
    return chunkedResult('录像已回传（' + stat.size.toString() + ' 字节）');
  } catch (e) {
    if (fd >= 0) {
      try { fileIo.closeSync(fd); } catch (ignore) {}
    }
    return errResult('录像读取失败，请重试');
  }
}

/** 照片仍走小体积 base64 内联（图片不大，不必分块）。 */
function readPhotoAsBase64(filePath: string): CapResult {
  let fd: number = -1;
  try {
    const stat: fileIo.Stat = fileIo.statSync(filePath);
    const buf: ArrayBuffer = new ArrayBuffer(stat.size);
    const rf: fileIo.File = fileIo.openSync(filePath, fileIo.OpenMode.READ_ONLY);
    fd = rf.fd;
    fileIo.readSync(fd, buf);
    fileIo.closeSync(rf);
    fd = -1;
    const helper: util.Base64Helper = new util.Base64Helper();
    const b64: string = helper.encodeToStringSync(new Uint8Array(buf));
    if (b64.length > CAMERASUE_MAX_B64) {
      return errResult('照片数据过大，请降低分辨率后重试');
    }
    return okResult('data:image/jpeg;base64,' + b64);
  } catch (e) {
    if (fd >= 0) {
      try { fileIo.closeSync(fd); } catch (ignore) {}
    }
    return errResult('照片读取失败，请重试');
  }
}

// ===== clipboardsee / clipboardsue =====

const CLIPBOARD_PERMISSIONS: Array<Permissions> = ['ohos.permission.READ_PASTEBOARD'];

/**
 * READ_PASTEBOARD 是 user_grant 权限：仅在 agent 真正请求 clipboardsee 时弹出系统授权，
 * 不在应用启动时抢先索权。已授权时系统会直接返回，不会重复打扰用户。
 */
async function ensureClipboardPermission(context: common.UIAbilityContext): Promise<boolean> {
  try {
    const atManager = abilityAccessCtrl.createAtManager();
    const result: PermissionRequestResult =
      await atManager.requestPermissionsFromUser(context, CLIPBOARD_PERMISSIONS);
    return result.authResults.length > 0 && result.authResults[0] === 0;
  } catch (e) {
    return false;
  }
}

export async function capClipboardSee(context: common.UIAbilityContext): Promise<CapResult> {
  const granted: boolean = await ensureClipboardPermission(context);
  if (!granted) {
    return errResult('剪贴板读取权限未授予，请在系统设置中允许后重试');
  }
  try {
    const clip: pasteboard.SystemPasteboard = pasteboard.getSystemPasteboard();
    const has: boolean = await clip.hasData();
    if (!has) {
      return okResult('');
    }
    const data: pasteboard.PasteData = await clip.getData();
    const txt: string = data.getPrimaryText();
    return okResult(txt ?? '');
  } catch (e) {
    return errResult('剪贴板读取失败，请确认应用在前台并已获得系统授权');
  }
}

export async function capClipboardsue(text: string): Promise<CapResult> {
  try {
    const clip: pasteboard.SystemPasteboard = pasteboard.getSystemPasteboard();
    const data: pasteboard.PasteData = pasteboard.createData(pasteboard.MIMETYPE_TEXT_PLAIN, text);
    await clip.setPasteData(data);
    return okResult('clipboard written');
  } catch (e) {
    return errResult('剪贴板写入失败，请保持应用在前台后重试');
  }
}

// ===== speakeruse：TTS 朗读 =====

class TtsSession {
  private engine: textToSpeech.TextToSpeechEngine | null = null;

  async speak(text: string): Promise<CapResult> {
    if (text.length > 4000) {
      return errResult('朗读内容过长，请缩短到 4000 字以内');
    }
    try {
      if (this.engine === null) {
        const params: textToSpeech.CreateEngineParams = {
          language: 'zh-CN',
          person: 0,
          online: 1,
        };
        const eng: textToSpeech.TextToSpeechEngine = await textToSpeech.createEngine(params);
        this.engine = eng;
      }
      const sp: textToSpeech.SpeakParams = {
        requestId: 'spk-' + Date.now().toString(),
      };
      this.engine.speak(text, sp);
      return okResult('speaking');
    } catch (e) {
      return errResult('语音服务暂时不可用，请稍后重试');
    }
  }

  shutdown(): void {
    if (this.engine !== null) {
      try {
        this.engine.shutdown();
      } catch (e) {
        // ignore
      }
      this.engine = null;
    }
  }
}

const ttsSession: TtsSession = new TtsSession();

export async function capSpeakerUse(text: string): Promise<CapResult> {
  return ttsSession.speak(text);
}

export function shutdownSpeakerUse(): void {
  ttsSession.shutdown();
}

// ===== status / deviceinfo =====

export function capStatus(deviceId: string): CapResult {
  const payload: DeviceStatusPayload = {
    device_id: deviceId,
    status: 'online',
    hostname: 'ohos-phone',
    platform: 'OpenHarmony',
    arch: deviceInfo.abiList,
    uptime: Math.floor((Date.now() - APP_STARTED_AT) / 1000),
  };
  return okResult(JSON.stringify(payload));
}

export function capDeviceInfo(deviceId: string, deviceName: string): CapResult {
  const details: DeviceDetails = {
    hostname: 'ohos-phone',
    platform: 'OpenHarmony',
    arch: deviceInfo.abiList,
    os_release: deviceInfo.osFullName,
    version: appVersion(),
    cpus: 0,
    brand: deviceInfo.brand,
    manufacturer: deviceInfo.manufacture,
    model: deviceInfo.productModel,
    series: deviceInfo.productSeries,
    sdk_api_version: deviceInfo.sdkApiVersion,
    security_patch: deviceInfo.securityPatchTag,
    abi_list: deviceInfo.abiList,
    device_type: deviceInfo.deviceType,
  };
  const payload: DeviceInfoPayload = {
    device_id: deviceId,
    name: deviceName,
    kind: 'ohos-phone',
    caps: LOCAL_DEVICE_CAPS,
    info: details,
  };
  return okResult(JSON.stringify(payload));
}
