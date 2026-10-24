import { hilog } from '@kit.PerformanceAnalysisKit';
import { ApiError } from './ApiClient';

/**
 * 面向用户的错误文案统一出口。
 *
 * 问题背景：之前各页面直接把 `e.message` 拼进 UI，于是屏幕上出现
 * "获取状态失败: Failed to connect to the server."、原始 JSON 报错体、
 * 甚至后端堆栈。这类文本对用户没有意义，还会泄露内网地址与实现细节。
 *
 * 约定：
 *  - UI 只显示 userMessage() 返回的短句（人话、可行动、不含技术细节）；
 *  - 技术细节（原始 message / HTTP 状态码 / 响应体）只写进 hilog，
 *    通过 `hdc shell hilog | grep HologramAgent` 排查，不进 UI。
 */

const DOMAIN: number = 0xA0A0;
const TAG: string = 'HologramAgent';

/** 网络类错误的统一提示：不暴露主机名、端口、curl 错误码。 */
const MSG_UNREACHABLE: string = '连接不上后端服务，请检查网络与服务地址';
const MSG_TIMEOUT: string = '后端响应超时，请稍后重试';
const MSG_AUTH: string = 'API Key 无效或已过期，请在设置里更新';
const MSG_FORBIDDEN: string = '没有访问权限，请检查 API Key 的权限范围';
const MSG_NOT_FOUND: string = '后端没有这个接口，可能版本不匹配';
const MSG_SERVER: string = '后端服务内部出错，请查看服务端日志';
const MSG_BAD_DATA: string = '后端返回的数据无法解析';
const MSG_TLS: string = '证书校验失败，请检查 HTTPS 配置';
const MSG_GENERIC: string = '操作失败，请稍后重试';
const MSG_NO_CONN: string = '尚未配置后端连接，请先在设置里添加';

/** 一眼判定是否"网络层根本没连上"，用于页面显示离线态而不是报错态。 */
export function isOffline(e: Object): boolean {
  const code: number = businessCode(e);
  if (code === 2300006 || code === 2300007 || code === 2300005 ||
      code === 2300052 || code === 2300056 || code === 2300028) {
    return true;
  }
  if (e instanceof ApiError) {
    return e.status < 0;
  }
  return false;
}

/**
 * 是否为超时。聊天场景里超时不算失败——请求已经到后端，
 * 只是回复还没生成完，UI 要显示"等待回复"而不是报错。
 */
export function isTimeout(e: Object): boolean {
  const code: number = businessCode(e);
  if (code === 2300028) {
    return true;
  }
  if (e instanceof ApiError && (e.status === 408 || e.status === 504)) {
    return true;
  }
  const raw: string = rawMessage(e);
  return raw.indexOf('timeout') >= 0 || raw.indexOf('超时') >= 0;
}

/**
 * 把任意异常翻译成一句用户能看懂、且不含技术细节的话。
 * 同时把原始信息写入 hilog（scene 用于定位是哪个调用点）。
 */
export function userMessage(scene: string, e: Object): string {
  logDetail(scene, e);

  if (e instanceof ApiError) {
    const st: number = e.status;
    if (st === 401) {
      return MSG_AUTH;
    }
    if (st === 403) {
      return MSG_FORBIDDEN;
    }
    if (st === 404) {
      return MSG_NOT_FOUND;
    }
    if (st === 408 || st === 504) {
      return MSG_TIMEOUT;
    }
    if (st >= 500) {
      return MSG_SERVER;
    }
    if (st < 0) {
      return MSG_NO_CONN;
    }
    if (st >= 400) {
      // 4xx 里后端通常给了业务原因，但不保证是人话，也可能带内部路径。
      // 只在明显短且不含技术噪音时透传，否则退回通用文案。
      return safeBackendReason(e.message);
    }
  }

  const code: number = businessCode(e);
  if (code === 2300028) {
    return MSG_TIMEOUT;
  }
  if (code === 2300005 || code === 2300006 || code === 2300007 ||
      code === 2300052 || code === 2300055 || code === 2300056) {
    return MSG_UNREACHABLE;
  }
  if (code === 2300058 || code === 2300059 || code === 2300060 || code === 2300077) {
    return MSG_TLS;
  }
  if (code === 2300001 || code === 2300003) {
    return '服务地址格式不正确，请在设置里检查';
  }
  if (code === 2300009 || code === 2300094) {
    return MSG_AUTH;
  }
  if (code === 2300997 || code === 2300998) {
    return '系统禁止访问该地址，请改用 HTTPS 或放开域名白名单';
  }

  if (e instanceof SyntaxError) {
    // JSON.parse 失败
    return MSG_BAD_DATA;
  }

  const raw: string = rawMessage(e);
  if (raw.indexOf('timeout') >= 0 || raw.indexOf('超时') >= 0) {
    return MSG_TIMEOUT;
  }
  return MSG_GENERIC;
}

/** 未配置连接时的统一文案，页面不要各写一份。 */
export function noConnectionMessage(): string {
  return MSG_NO_CONN;
}

/**
 * 4xx 的后端说明只在"看起来是给人看的"时才透传：
 * 短、无换行、不含路径/括号异常/HTML/JSON 花括号。
 */
function safeBackendReason(msg: string): string {
  const s: string = msg.trim();
  if (s.length === 0 || s.length > 60) {
    return MSG_GENERIC;
  }
  if (s.indexOf('\n') >= 0 || s.indexOf('{') >= 0 || s.indexOf('<') >= 0 ||
      s.indexOf('/') >= 0 || s.indexOf('0x') >= 0 || s.indexOf('Exception') >= 0 ||
      s.indexOf('Error:') >= 0 || s.indexOf('panic') >= 0) {
    return MSG_GENERIC;
  }
  return s;
}

/** 取 BusinessError.code（ArkTS 不允许 in / 索引访问，用可选字段读取）。 */
function businessCode(e: Object): number {
  const be = e as BusinessErrorLike;
  const c: number | undefined = be.code;
  return c !== undefined ? c : 0;
}

function rawMessage(e: Object): string {
  if (e instanceof Error) {
    return e.message;
  }
  return String(e);
}

function logDetail(scene: string, e: Object): void {
  const code: number = businessCode(e);
  let detail: string = rawMessage(e);
  if (e instanceof ApiError) {
    detail = 'HTTP ' + e.status.toString() + ' ' + detail + ' body=' + e.body;
  }
  hilog.error(DOMAIN, TAG, '%{public}s failed: code=%{public}d detail=%{private}s',
    scene, code, detail);
}

/** BusinessError 的最小结构（避免为了读 code 而 import 整个 kit）。 */
interface BusinessErrorLike {
  code?: number;
  message?: string;
}
