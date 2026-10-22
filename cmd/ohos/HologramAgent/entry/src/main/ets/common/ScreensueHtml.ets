/**
 * screensue 载荷解析与 HTML 渲染（无 UI 依赖）。
 *
 * 从 BridgeCaps.ets 抽出：加进 HTML 检测/编码后那个文件超过 520 行，
 * 超出工程「单文件 ≤400 行」的约定；而「screensue 内容怎么解析、怎么渲染」
 * 与「设备能力怎么实现」本就是两件事。
 */

import { util } from '@kit.ArkTS';

// ===== screensue 内容解析 =====
// 服务端协议: screensue [秒] <内容>；0=常驻。

export interface ScreensuePayload {
  duration: number; // 秒；0 表示常驻直到用户关闭
  content: string;
}

export function parseScreensue(rawArgs: string): ScreensuePayload {
  const p: ScreensuePayload = { duration: 5, content: '' };
  const leadingSpaces: RegExp = new RegExp('^\\s+');
  const firstSpace: RegExp = new RegExp('\\s');
  let rest: string = rawArgs.replace(leadingSpaces, '');
  const splitAt: number = rest.search(firstSpace);
  if (splitAt > 0) {
    const first: string = rest.substring(0, splitAt);
    const digits: RegExp = new RegExp('^\\d+$');
    if (digits.test(first)) {
      p.duration = Math.min(parseInt(first, 10), 86400);
      rest = rest.substring(splitAt).replace(leadingSpaces, '');
    }
  }
  p.content = rest;
  return p;
}

/**
 * 判断 agent 下发的 screensue 内容是不是 HTML。
 *
 * 服务端两侧协议都允许 HTML（localuse 的 local_screensue 在 Linux 用 browsh/w3m
 * 渲染 HTML；remotedevice 的工具说明写的就是「显示内容/HTML」）。
 *
 * ★ 判据必须容忍前导杂质：实测 agent 常把整段文档连引号一起传进来
 * （`'<html>…</html>'`），而"首个非空字符必须是 '<'"的旧判据直接判否、
 * 退回纯文本渲染，用户看到的就是满屏标签源码（截图取证）。
 *
 * 所以这里扫到第一个「像标签开头」的 '<'，不要求它在开头；但只有后面紧根
 * 字母或 '/' 时才认，避免把 "a < b" 这类文本里的比较符当标签。
 */
export function looksLikeHtml(content: string): boolean {
  return findHtmlStart(content) >= 0;
}

/** 找到第一个「像标签开头」的 '<'；没有则 -1。 */
function findHtmlStart(content: string): number {
  for (let i = 0; i < content.length; i++) {
    if (content.charAt(i) !== '<') {
      continue;
    }
    const next: string = i + 1 < content.length ? content.charAt(i + 1) : '';
    if (next === '/') {
      const after: string = i + 2 < content.length ? content.charAt(i + 2) : '';
      if (isAsciiLetter(after)) {
        return i;
      }
      continue;
    }
    if (isAsciiLetter(next)) {
      return i;
    }
  }
  return -1;
}

function isAsciiLetter(ch: string): boolean {
  if (ch.length === 0) {
    return false;
  }
  const c: number = ch.charCodeAt(0);
  return (c >= 65 && c <= 90) || (c >= 97 && c <= 122);
}

/**
 * 取出真正的 HTML 片段：剥掉 agent 误带的包裹引号，再从头截到第一个标签。
 *
 * 剥引号是必须的：不剥的话那个孤立的 `'` 会被 Web 当正文渲染出来
 * （截图上第一行就是它），而且它还会把后续判据带偏。返回 '' 表示不是 HTML。
 */
export function screensueHtmlDocument(content: string): string {
  let body: string = content.trim();
  // 反复剥成对的包裹引号（agent 把整段 HTML 当命令参数传时的常见形态）。
  while (body.length >= 2) {
    const first: string = body.charAt(0);
    const last: string = body.charAt(body.length - 1);
    if ((first === '\'' && last === '\'') || (first === '"' && last === '"')) {
      body = body.substring(1, body.length - 1).trim();
      continue;
    }
    break;
  }
  const idx: number = findHtmlStart(body);
  if (idx < 0) {
    return '';
  }
  if (idx > 0) {
    body = body.substring(idx);
  }
  return body;
}

/**
 * 把 screensue 内容编成可直接交给 Web 组件 `loadData` 的 base64。
 *
 * 为什么必须上 Web（不再用 RichText）：RichText 只认极小标签子集，
 * 对 <style>、CSS 动画、内联 SVG 一律不渲染 —— 实测 agent 推的是完整
 * HTML 文档（含 @keyframes 与 <svg>），RichText 下只能看到源码。用户明确要求引入 webview。
 *
 * 为什么用 base64 而不是明文 loadData：encoding 非 base64 时按 URL 规则转义，
 * 一个几 KB 的完整文档会撞上长度/转义问题；base64 是整篇加载的推荐方式，
 * 中文与引号、'#' 也不会被二次转义（自己手写 UTF-8 编码，见 base64Utf8）。
 *
 * 片段（非完整文档）补一层 shell：加 <meta viewport> 让窄屏排版正确，
 * 并注入主题前景色，避免深色主题下黑字不可见。返回 '' 表示不是 HTML（走纯文本渲染）。
 */
export function screensueWebData(content: string, dark: boolean): string {
  const fragment: string = screensueHtmlDocument(content);
  if (fragment.length === 0) {
    return '';
  }
  if (hasHtmlShell(fragment)) {
    // 已是完整文档：不再包壳，也不注入颜色（由页面自带样式决定）。
    return base64Utf8(fragment);
  }
  const fg: string = dark ? '#E8ECF4' : '#1B2430';
  const wrapped: string = '<!DOCTYPE html><html><head><meta charset="utf-8">'
    + '<meta name="viewport" content="width=device-width,initial-scale=1">'
    + '<style>html,body{margin:0;padding:0}'
    + 'body{padding:10px;color:' + fg + ';font-family:sans-serif;font-size:16px;'
    + 'line-height:1.6;word-break:break-word;-webkit-text-size-adjust:100%}'
    + 'img,svg,video{max-width:100%;height:auto}</style></head><body>'
    + fragment + '</body></html>';
  return base64Utf8(wrapped);
}

/** 内容是否已是完整 HTML 文档（有 <html> 或 <!DOCTYPE>），不必再包壳。 */
function hasHtmlShell(s: string): boolean {
  const head: string = s.substring(0, 400).toLowerCase();
  return head.indexOf('<html') >= 0 || head.indexOf('<!doctype') >= 0;
}

/**
 * UTF-8 字符串 → base64。
 *
 * 不能把 UTF-16 码元直接交给 Base64Helper：那样中文会变成乱码。
 * 这里手写 UTF-8 字节序列（按码点，含代理对合成）后再编码。
 */
export function base64Utf8(s: string): string {
  const bytes: number[] = [];
  for (let i = 0; i < s.length; i++) {
    let code: number = s.charCodeAt(i);
    // 代理对（emoji 等）合成成一个码点。
    if (code >= 0xD800 && code <= 0xDBFF && i + 1 < s.length) {
      const next: number = s.charCodeAt(i + 1);
      if (next >= 0xDC00 && next <= 0xDFFF) {
        code = ((code - 0xD800) << 10) + (next - 0xDC00) + 0x10000;
        i++;
      }
    }
    if (code < 0x80) {
      bytes.push(code);
    } else if (code < 0x800) {
      bytes.push(0xC0 | (code >> 6), 0x80 | (code & 0x3F));
    } else if (code < 0x10000) {
      bytes.push(0xE0 | (code >> 12), 0x80 | ((code >> 6) & 0x3F), 0x80 | (code & 0x3F));
    } else {
      bytes.push(0xF0 | (code >> 18), 0x80 | ((code >> 12) & 0x3F),
        0x80 | ((code >> 6) & 0x3F), 0x80 | (code & 0x3F));
    }
  }
  const helper: util.Base64Helper = new util.Base64Helper();
  return helper.encodeToStringSync(new Uint8Array(bytes));
}
