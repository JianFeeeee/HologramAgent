export interface ConnectionConfig {
  id: string;
  name: string;
  url: string;
  apiKey: string;
  type: string; // 'webui' | 'cli'
}

export interface ToolCallInfo {
  name: string;
  args: string;
  result?: string;
  plugin?: string;
  status?: string; // 'ok' | 'error' | 'denied' | 'running'
  open?: boolean;
}

export interface ChatMessage {
  id: number;
  role: string; // 'user' | 'assistant'
  content: string;
  reasoningContent?: string;
  reasoningOpen?: boolean;
  isStreaming?: boolean;
  isFinal?: boolean;
  toolCalls?: ToolCallInfo[];
  /** 消息来源通道：'webui' | 'channel' | 'webui/<device_id>' 等；用于区分设备/渠道消息 */
  source?: string;
  /**
   * 服务端单调递增序号（后端 ChatMsg.seq）。
   *
   * 它是与后端增量查询（/chat/history?after=<seq>）对账的唯一定位符：
   * 本地乐观消息没有 seq，服务端回显后靠 seq 认领并去重。
   * 没有它就只能拿“正文内容”去重，一旦同一句话发两次就会误删。
   */
  seq?: number;
  /** 图片/文件附件（后端 ChatMsg.attachment） */
  attachment?: ChatAttachment;
}

export interface HistoryMessage {
  role: string;
  content: string;
  source?: string;
  timestamp?: string;
  attachment?: ChatAttachment;
}

export interface AgentStatus {
  version?: string;
  uptime_ms?: number;
  agent?: string;
  provider?: string;
  memory_events?: number;
}

export interface DeviceInfo {
  deviceId: string;
  name: string;
  kind: string;
  online: boolean;
  authorized: boolean;
  caps: string[];
  hostname?: string;
  platform?: string;
}

export interface PluginInfo {
  name: string;
  nameZh?: string;
  nameEn?: string;
  version?: string;
  description?: string;
  author?: string;
  deprecated?: boolean;
  builtin: boolean;
  loaded: boolean;
  disabled: boolean;
  tools?: string[];
}

/**
 * One row in the merged plugin list:
 * kernel.plugins ∪ installed(/plugins) ∪ disabled(/plugins/disabled)，
 * 与 WebGUI renderPlugins 的合并口径一致。
 */
export interface PluginRow {
  name: string;
  /** /kernel.plugins 里出现且 loaded=true 才算已加载 */
  loaded: boolean;
  /** 出现在 /plugins/disabled 列表里 */
  disabled: boolean;
  /** 已安装的外部插件（/plugins 返回） */
  external: boolean;
  version?: string;
  description?: string;
  tools?: string[];
}

/**
 * GET /plugins/{name} 返回的插件清单明细。
 * 字段与后端 pluginmgr 的 pluginInfo 一一对应；
 * WebGUI 只把它 JSON.stringify 到 <pre> 里，这里改为结构化展示。
 */
export interface PluginDetail {
  name: string;
  version: string;
  description: string;
  author: string;
  license: string;
  homepage: string;
  repository: string;
  entry: string;
  minVersion: string;
  tags: string[];
  deprecated: boolean;
  files: string[];
}

export function emptyPluginDetail(): PluginDetail {
  const d: PluginDetail = {
    name: '',
    version: '',
    description: '',
    author: '',
    license: '',
    homepage: '',
    repository: '',
    entry: '',
    minVersion: '',
    tags: [],
    deprecated: false,
    files: [],
  };
  return d;
}

/**
 * 聊天消息附件。字段与后端 webui 的 Attachment 严格一致，只有四个：
 * type / url / size / name —— 后端不提供 mime、尺寸、本地路径。
 */
export interface ChatAttachment {
  /** 'image' | 'file' */
  type: string;
  /** /files/<name>、/uploads/<name> 或远程 http(s) 地址 */
  url: string;
  /** 字节数；远程 URL 为 0 */
  size: number;
  /** 展示用文件名 */
  name: string;
}

export interface AppSettings {
  lang: string; // 'zh' | 'en'
  theme: string; // 'dark' | 'light' | 'system'
  currentConnId: string;
  /** 自定义背景图路径（应用沙箱内文件路径），空串表示未设置 */
  bgImage: string;
  /** 背景图不透明度 0..1 */
  bgOpacity: number;
}

export function emptySettings(): AppSettings {
  const s: AppSettings = {
    lang: 'zh',
    theme: 'dark',
    currentConnId: '',
    bgImage: '',
    bgOpacity: 0.25,
  };
  return s;
}

/**
 * 规范化用户填写的后端地址。
 *
 * 用户习惯直接敲域名（homeagent.example.xyz）或粘贴带路径的地址。
 * 三件事必须在存库前做掉，否则请求会以各种方式失败：
 *
 * 1) 补协议，且默认 https。
 *    很多反代把 http 整站 302 到别的主机（例如门户站 www.xxx），
 *    重定向后的主机没有 API，客户端只会拿到 404 页面而不是 401/200，
 *    表现为"连接直接失败"且看不出原因。默认 https 可以绕过整类问题。
 *    只有明确写了 http:// 的内网地址才走明文。
 * 2) 去掉结尾斜杠，避免拼出 //api/v1。
 * 3) 去掉用户误粘的 /api/v1 后缀，避免拼成 /api/v1/api/v1。
 */
export function normalizeBaseUrl(raw: string): string {
  let u: string = raw.trim();
  if (u.length === 0) {
    return '';
  }
  const lower: string = u.toLowerCase();
  if (!lower.startsWith('http://') && !lower.startsWith('https://')) {
    u = 'https://' + u;
  }
  u = u.replace(/\/+$/, '');
  u = u.replace(/\/api\/v1$/, '');
  return u;
}

/**
 * 新建连接用的空白模板。
 * 这里不能写死任何地址或密钥：源码是公开的，硬编码等于把内网地址和访问凭据一起发布。
 * 地址与 API Key 由用户在「设置 - 连接」里填写，未配置时各页面统一提示（见 UserError.MSG_NO_CONN）。
 */
export function defaultConnection(): ConnectionConfig {
  const c: ConnectionConfig = {
    id: '',
    name: 'HologramAgent',
    url: '',
    apiKey: '',
    type: 'webui',
  };
  return c;
}
