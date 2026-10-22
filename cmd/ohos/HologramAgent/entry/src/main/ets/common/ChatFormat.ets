/**
 * 聊天页的纯格式化/判定逻辑（无 UI 依赖）。
 *
 * 从 pages/ChatPage.ets 抽出：这些函数只吃数据吐字符串/布尔，
 * 抽出来后气泡、工具卡、渠道头像三个组件可以共用同一份口径。
 */

import { ChatMessage, ToolCallInfo } from '../model/Model';

/**
 * 由文件名后缀推断 Content-Type。
 * 后端按 multipart 部件的 Content-Type 判定 image/file，
 * 给错会让图片被当成普通文件（缩略图就没了）。
 */
export function mimeOf(name: string, isImage: boolean): string {
  const i: number = name.lastIndexOf('.');
  const ext: string = i >= 0 ? name.substring(i + 1).toLowerCase() : '';
  if (ext === 'png') {
    return 'image/png';
  }
  if (ext === 'jpg' || ext === 'jpeg') {
    return 'image/jpeg';
  }
  if (ext === 'webp') {
    return 'image/webp';
  }
  if (ext === 'gif') {
    return 'image/gif';
  }
  if (ext === 'bmp') {
    return 'image/bmp';
  }
  if (ext === 'heic' || ext === 'heif') {
    return 'image/heic';
  }
  if (isImage) {
    return 'image/jpeg';
  }
  if (ext === 'pdf') {
    return 'application/pdf';
  }
  if (ext === 'txt' || ext === 'log' || ext === 'md') {
    return 'text/plain';
  }
  if (ext === 'json') {
    return 'application/json';
  }
  return 'application/octet-stream';
}

/** payload 字段可能是字符串、对象或数组，统一转成可展示文本。 */
export function stringifyField(raw: Object | undefined): string {
  if (raw === undefined || raw === null) {
    return '';
  }
  if (typeof raw === 'string') {
    return raw as string;
  }
  try {
    return JSON.stringify(raw);
  } catch (e) {
    return String(raw);
  }
}

/**
 * ForEach 键：消息结构变化即换键 → 旧气泡销毁重建 → @Builder 里的
 * if 分支重新求值。这是 ArkUI V1 渲染模型决定的：ForEach 对相同键
 * 只更新 @Prop/@Link 绑定，不重新执行 @Builder 体，所以
 * 「思考卡/工具卡/附件」这些用 if 包裹的条件分支在首次渲染后
 * 永远不会再次求值——气泡里的这些面板就永远不出现。
 *
 * 反过来，content_delta 不进 structSig：正文文本靠 MarkdownView
 * 的 @Prop content 响应式更新，不重建气泡 → 流式渲染平滑。
 * 实测 SSE 里 reasoning_delta 与 content_delta 不交错（思考阶段
 * 先于输出阶段），所以思考期间重建气泡不会打断正文流式动画。
 */
export function structSig(msg: ChatMessage): string {
  let s: string = msg.id.toString();
  const rc: string | undefined = msg.reasoningContent;
  s += '_r' + (rc !== undefined ? rc.length.toString() : '0');
  s += '_ro' + (msg.reasoningOpen === true ? '1' : '0');
  const tcs: ToolCallInfo[] | undefined = msg.toolCalls;
  if (tcs !== undefined) {
    s += '_t' + tcs.length.toString();
    for (let i = 0; i < tcs.length; i++) {
      const tc: ToolCallInfo = tcs[i];
      s += '_' + (tc.status ?? '');
      s += '_' + (tc.open === true ? 'o' : 'c');
      s += '_' + (tc.args !== undefined ? tc.args.length.toString() : '0');
      s += '_' + (tc.result !== undefined ? tc.result.length.toString() : '0');
      s += '_' + (tc.plugin ?? '');
    }
  } else {
    s += '_t0';
  }
  s += '_a' + (msg.attachment !== undefined ? '1' : '0');
  s += '_src' + (msg.source ?? '');
  s += '_f' + (msg.isFinal === true ? '1' : '0');
  s += '_s' + (msg.isStreaming === true ? '1' : '0');
  return s;
}

/** 折叠时也要能看出思考在增长：显示字数 */
export function reasoningLenLabel(msg: ChatMessage): string {
  const rc: string | undefined = msg.reasoningContent;
  if (rc === undefined || rc.length === 0) {
    return '';
  }
  return rc.length.toString() + ' 字';
}

/**
 * 是否仍在执行。
 * 判据是 status 而不是 result：后端 status=ok 的工具也可能返回空串，
 * 用 result 判断会让这类调用永远显示"调用中"。
 */
export function tcRunning(tc: ToolCallInfo): boolean {
  const s: string | undefined = tc.status;
  return s === undefined || s.length === 0 || s === 'running';
}

export function tcError(tc: ToolCallInfo): boolean {
  return tc.status === 'denied' || tc.status === 'error';
}

/** 工具卡左侧色条（accent 由调用方从 palette 取） */
export function tcLeftColor(tc: ToolCallInfo, accent: string): string {
  if (tcError(tc)) {
    return '#DB3694';
  }
  if (tcRunning(tc)) {
    return accent;
  }
  return 'rgba(23, 169, 100, 0.8)';
}

/** 工具卡状态图标颜色 */
export function tcIcoColor(tc: ToolCallInfo, accent: string): string {
  if (tcError(tc)) {
    return '#DB3694';
  }
  if (tcRunning(tc)) {
    return accent;
  }
  return 'rgba(23, 169, 100, 0.9)';
}

export function tcStateLabel(tc: ToolCallInfo): string {
  if (tc.status === 'denied') {
    return '已拒绝';
  }
  if (tcRunning(tc)) {
    return '调用中';
  }
  return '完成';
}

export function tcStateColor(tc: ToolCallInfo): string {
  if (tc.status === 'denied') {
    return '#FF9EC6';
  }
  if (tcRunning(tc)) {
    return '#A3B8FF';
  }
  return '#6EE7A8';
}

/**
 * 气泡最大宽度（相对 BubbleSlot 的宽度，即扣掉头像与间距后的真实可用宽）。
 * 纯文本 78% 好看；但工具卡/思考卡是"面板"，78% 会把里面的状态文字和
 * 参数/结果压成一团（还会被 clip 切掉），所以带卡片时放宽到 92%。
 */
export function bubbleMaxWidth(msg: ChatMessage): string {
  const hasPanels: boolean =
    (msg.toolCalls !== undefined && msg.toolCalls.length > 0) ||
    (msg.reasoningContent !== undefined && msg.reasoningContent.length > 0);
  return hasPanels ? '92%' : '78%';
}

/**
 * 是否"别处来的"消息。对齐 GUI 的 source !== 'webui' 判定，但多减一项：
 * 本机自己发的消息在后端会被写成 webui/<device_id>，那仍然是"我发的"，
 * 不能当成渠道消息挂上别人的头像。
 */
export function isChannelMsg(source: string, deviceId: string): boolean {
  if (source.length === 0 || source === 'webui') {
    return false;
  }
  return source !== 'webui/' + deviceId;
}

/** 自己发的消息（右对齐、"我"头像）：渠道消息即使 role=user 也不算 */
export function isSelfMsg(role: string, channel: boolean): boolean {
  return role === 'user' && !channel;
}

/** 渠道名展示：webui/<id> 只显示 <id>，其余原样。 */
export function chanLabel(src: string): string {
  if (src.startsWith('webui/')) {
    return src.substring(6);
  }
  return src;
}

/** 渠道首字母（大写），用作头像文字。 */
export function chanLetter(src: string): string {
  const label: string = chanLabel(src);
  if (label.length === 0) {
    return '?';
  }
  return label.substring(0, 1).toUpperCase();
}

/** 由渠道名散列出稳定色，避免每次渲染换色。 */
export function chanColor(src: string): string {
  const label: string = chanLabel(src);
  let h: number = 0;
  for (let i = 0; i < label.length; i++) {
    h = (h * 31 + label.charCodeAt(i)) % 360;
  }
  return 'hsl(' + h.toString() + ', 52%, 46%)';
}
