/**
 * 附件的解析与格式化（纯函数，无 UI、无平台 I/O）。
 *
 * 后端 Attachment 只有四个字段：type / url / size / name
 * （internal/plugins/webui/handler.go），没有 mime、没有像素尺寸、没有本地路径。
 * 所以详情页里的"尺寸/格式"必须由客户端自己解码得出，不能假装后端给了。
 *
 * 从 components/Attachment.ets 抽出：附件卡与附件详情都要用这几个函数，
 * 放在 common 里两边共用，也不必让 UI 文件承担这段纯逻辑。
 */

import { ChatAttachment } from '../model/Model';

/** 从后端 JSON 里解析 attachment 字段；缺字段或类型不对则返回 undefined。 */
export function parseAttachment(raw: Object | undefined): ChatAttachment | undefined {
  if (raw === undefined || raw === null) {
    return undefined;
  }
  const o: Record<string, Object> = raw as Record<string, Object>;
  const url: string = o['url'] as string ?? '';
  if (url.length === 0) {
    return undefined;
  }
  const t: string = o['type'] as string ?? 'file';
  const a: ChatAttachment = {
    type: t === 'image' ? 'image' : 'file',
    url: url,
    size: o['size'] as number ?? 0,
    name: o['name'] as string ?? fileNameOf(url),
  };
  return a;
}

/** 由 SSE channel_output 事件构造附件（字段名与 history 不同）。 */
export function attachmentFromChannelOutput(
  outputType: string, url: string, size: number): ChatAttachment | undefined {
  if (url.length === 0) {
    return undefined;
  }
  if (outputType !== 'image' && outputType !== 'file') {
    return undefined;
  }
  const a: ChatAttachment = {
    type: outputType,
    url: url,
    size: size,
    name: fileNameOf(url),
  };
  return a;
}

/** 取 URL 最后一段作为展示文件名，与后端 handler.go 的取名方式一致。 */
export function fileNameOf(url: string): string {
  let s: string = url;
  const q: number = s.indexOf('?');
  if (q >= 0) {
    s = s.substring(0, q);
  }
  const i: number = s.lastIndexOf('/');
  const name: string = i >= 0 ? s.substring(i + 1) : s;
  return name.length > 0 ? name : '附件';
}

/** 人类可读字节数，口径对齐后端 formatBytes（KB 以上保留一位小数）。 */
export function formatBytes(n: number): string {
  if (n <= 0) {
    return '';
  }
  if (n < 1024) {
    return n.toString() + ' B';
  }
  const kb: number = n / 1024;
  if (kb < 1024) {
    return oneDecimal(kb) + ' KB';
  }
  const mb: number = kb / 1024;
  if (mb < 1024) {
    return oneDecimal(mb) + ' MB';
  }
  return oneDecimal(mb / 1024) + ' GB';
}

function oneDecimal(v: number): string {
  return (Math.round(v * 10) / 10).toString();
}

/** 由文件名后缀猜测类型标签。后端不返回 mime，只能这样标注。 */
export function extLabel(name: string): string {
  const i: number = name.lastIndexOf('.');
  if (i < 0 || i === name.length - 1) {
    return '未知类型';
  }
  return name.substring(i + 1).toUpperCase();
}

/** 去掉路径分隔符，避免附件名把文件写到 filesDir 之外。 */
export function sanitize(name: string): string {
  let s: string = name.replace(/[\/\\:*?"<>|]/g, '_');
  if (s.length === 0) {
    s = 'attachment';
  }
  return s;
}
