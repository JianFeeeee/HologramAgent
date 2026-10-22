/**
 * 聊天流状态源（单例）。
 *
 * 从 pages/ChatPage.ets 抽出：消息数组、分页游标、"哪几条是新消息"、
 * SSE 连接与历史拉取都属于同一个状态机；页面只剩渲染与输入。
 *
 * 为什么数组不进 AppStorage：StatusStore 已经踩过一次 —— 数组同步语义不可靠。
 * 这里沿用同一套做法：数组留在 store 内部，标量走 AppStorage 广播，
 * 另加一个自增版本号 K_CHAT_REV 通知订阅组件"重取一次快照"。
 *
 * 订阅组件的接法（见 components/ChatStream.ets）：
 *   @StorageProp(K_CHAT_REV) @Watch('onRev') private rev: number = 0;
 *   onRev(): void { this.messages = chatStore.messages(); }
 * ForEach 拿到的仍然是"每次刷新一个新数组引用"，与拆分前
 * （this.messages = this.messages.slice()）的渲染语义完全一致。
 *
 * SSE 事件的翻译在 common/ChatSse.ets：本类实现它的 ChatStreamSink 接口，
 * 依赖方向只有"ChatStore → ChatSse"一条，不构成循环。
 */

import { ChatMessage, ToolCallInfo, ChatAttachment } from '../model/Model';
import { SseClient, SseEvent } from './SseClient';
import { connStore } from './ConnStore';
import { apiClient } from './ApiClient';
import { CHAT_PAGE_SIZE } from './Constants';
import { ParsedHistory, parseHistoryPayload } from './ChatHistory';
import { applyChatSse, ChatStreamSink } from './ChatSse';

// ===== AppStorage 键：页面/聊天流/输入区共用 =====
export const K_CHAT_REV: string = 'chatRev';
export const K_CHAT_SCROLL_REV: string = 'chatScrollRev';
export const K_CHAT_LOADING: string = 'chatBusy';
export const K_CHAT_STAGE: string = 'chatStageText';
export const K_CHAT_CONNECTED: string = 'chatSseUp';

const SSE_RECONNECT_MS: number = 5000;
/** 增量轮询间隔：与 WebUI 的 chatTicker 一致（3s）。 */
const CHAT_POLL_MS: number = 3000;

/**
 * 把服务端来的消息并进本地列表，**按 seq 对账**（与 WebUI 的
 * applyServerMessages 同口径）。
 *
 * 为什么不再按“正文内容”去重：那是本次调研确认的缺陷根因。同一个人把
 * 同一句话发两次，或本地乐观消息与服务端回显内容相同时，内容比对会把
 * 其中一条误判成重复而丢弃（“App 发出的消息不显示”就是这个表现）。
 * seq 是服务端分配的唯一序号，才是可靠的定位符。
 *
 * 规则：
 *   - 服务端消息带 seq：本地已有同 seq → 原地更新（工具卡/最终文本是
 *     原地改的，不产生新 seq，只靠 after 拿不到，必须靠尾部探测更新）；
 *     本地没有 → 追加。
 *   - 服务端消息无 seq（旧后端）：退化为「本地末尾同角色同内容则认领」。
 *   - 本地无 seq 的乐观 user 消息：服务端回显同一句时被认领（补上 seq），
 *     而不是重复出现——认领先匹配最后一条无 seq 的同类消息。
 *
 * tailOnly：只允许在末尾追加/更新，用于“尾部探测”（拉最新一条做原地更新），
 * 避免把历史中间的消息插进来造成顺序错乱。
 */
function reconcileServerMsgs(local: ChatMessage[], incoming: ChatMessage[],
                             tailOnly: boolean, alloc: () => number): ChatMessage[] {
  const out: ChatMessage[] = local.slice();
  for (let i = 0; i < incoming.length; i++) {
    const sm: ChatMessage = incoming[i];
    const sq: number | undefined = sm.seq;
    let found: number = -1;
    if (sq !== undefined) {
      // 从尾部往前找：新消息总在尾部，省掉全表扫描
      for (let j = out.length - 1; j >= 0 && j >= out.length - 12; j--) {
        if (out[j].seq === sq) {
          found = j;
          break;
        }
      }
    }
    if (found >= 0) {
      // 原地更新：保留本地 id（组件按 id 复用，不重建气泡），
      // 只覆盖服务端权威字段。
      const prev: ChatMessage = out[found];
      if (prev.content !== sm.content) {
        prev.content = sm.content;
      }
      if (sm.reasoningContent !== undefined && prev.reasoningContent !== sm.reasoningContent) {
        prev.reasoningContent = sm.reasoningContent;
      }
      if (sm.toolCalls !== undefined) {
        prev.toolCalls = sm.toolCalls;
      }
      if (sm.attachment !== undefined) {
        prev.attachment = sm.attachment;
      }
      if (sm.source !== undefined) {
        prev.source = sm.source;
      }
      prev.isFinal = true;
      prev.isStreaming = false;
      continue;
    }
    if (tailOnly) {
      // 尾部探测：只有比本地最后一条 seq 更大才有意义，否则忽略（它已在中间）
      let maxLocalSeq: number = 0;
      for (let j = 0; j < out.length; j++) {
        const ls: number | undefined = out[j].seq;
        if (ls !== undefined && ls > maxLocalSeq) {
          maxLocalSeq = ls;
        }
      }
      if (sq !== undefined && sq > maxLocalSeq) {
        sm.id = alloc();
        out.push(sm);
      }
      continue;
    }
    // 认领本地乐观消息：本地末尾尚未拿到 seq 的同类消息，视为它的回显。
    if (sq !== undefined) {
      let claimed: number = -1;
      for (let j = out.length - 1; j >= 0; j--) {
        const lm: ChatMessage = out[j];
        if (lm.seq !== undefined) {
          break;
        }
        if (lm.role === sm.role) {
          claimed = j;
          break;
        }
      }
      if (claimed >= 0) {
        const prev: ChatMessage = out[claimed];
        prev.seq = sq;
        prev.isFinal = true;
        prev.isStreaming = false;
        if (sm.source !== undefined) {
          prev.source = sm.source;
        }
        if (sm.attachment !== undefined) {
          prev.attachment = sm.attachment;
        }
        continue;
      }
    }
    sm.id = alloc();
    out.push(sm);
  }
  return out;
}

class ChatStore implements ChatStreamSink {
  private msgs: ChatMessage[] = [];
  private nextId: number = 1;
  private refreshTimer: number = -1;
  private reconnectTimer: number = -1;
  /** 分页历史：当前已加载消息在服务端全量中的起始下标 */
  private offset: number = 0;
  /** 是否还有更早历史可向上加载 */
  private hasEarlier: boolean = false;
  private loadingOlder: boolean = false;
  /** 正在为新消息播入场动画的 id */
  private newIds: number[] = [];
  // SSE 正在为当前轮次推送内容时置 true，阻止 POST 响应重复创建消息
  private sseActiveForTurn: boolean = false;
  /** 增量游标：本地已知的最大服务端 seq（对应 WebUI 的 state.chatLastSeq） */
  private lastSeq: number = 0;
  /** 增量轮询中进行中，避免重入 */
  private polling: boolean = false;
  private pollTimer: number = -1;
  private sse: SseClient = new SseClient();

  init(): void {
    AppStorage.setOrCreate<boolean>(K_CHAT_LOADING, false);
    AppStorage.setOrCreate<string>(K_CHAT_STAGE, '');
    AppStorage.setOrCreate<boolean>(K_CHAT_CONNECTED, false);
    AppStorage.setOrCreate<number>(K_CHAT_REV, 0);
    AppStorage.setOrCreate<number>(K_CHAT_SCROLL_REV, 0);
  }

  // ===================== 读取 =====================

  messages(): ChatMessage[] {
    return this.msgs;
  }

  findMessage(id: number): ChatMessage | undefined {
    for (let i = 0; i < this.msgs.length; i++) {
      if (this.msgs[i].id === id) {
        return this.msgs[i];
      }
    }
    return undefined;
  }

  lastMessage(): ChatMessage | null {
    if (this.msgs.length === 0) {
      return null;
    }
    return this.msgs[this.msgs.length - 1];
  }

  isFresh(id: number): boolean {
    return this.newIds.indexOf(id) >= 0;
  }

  hasMore(): boolean {
    return this.hasEarlier;
  }

  isFetchingOlder(): boolean {
    return this.loadingOlder;
  }

  isLoading(): boolean {
    return AppStorage.get<boolean>(K_CHAT_LOADING) ?? false;
  }

  setLoading(v: boolean): void {
    AppStorage.set<boolean>(K_CHAT_LOADING, v);
  }

  setStage(s: string): void {
    AppStorage.set<string>(K_CHAT_STAGE, s);
  }

  setConnected(v: boolean): void {
    AppStorage.set<boolean>(K_CHAT_CONNECTED, v);
  }

  sseActive(): boolean {
    return this.sseActiveForTurn;
  }

  setSseActive(v: boolean): void {
    this.sseActiveForTurn = v;
  }

  /** 聊天流里最后一个带附件的消息：宽屏进入 Split 时用它填充右栏 */
  latestAttachment(): ChatAttachment | undefined {
    for (let i = this.msgs.length - 1; i >= 0; i--) {
      const a: ChatAttachment | undefined = this.msgs[i].attachment;
      if (a !== undefined) {
        return a;
      }
    }
    return undefined;
  }

  // ===================== 列表变更 =====================

  allocId(): number {
    return this.nextId++;
  }

  /** 追加一条消息并播入场动画 */
  pushNew(msg: ChatMessage): void {
    this.msgs.push(msg);
    this.markNew(msg.id);
  }

  /** 标记新消息，触发入场动画 */
  markNew(msgId: number): void {
    const arr: number[] = this.newIds.slice();
    arr.push(msgId);
    this.newIds = arr;
    // 这里不切片也不广播：与拆分前一致，入场动画的开场交给紧随其后的
    // refresh()（50ms 防抖后换新数组引用）那一次一起触发。
    setTimeout(() => {
      const idx: number = this.newIds.indexOf(msgId);
      if (idx >= 0) {
        const updated: number[] = this.newIds.slice();
        updated.splice(idx, 1);
        this.newIds = updated;
        this.forceRefresh();
      }
    }, 250);
  }

  /**
   * 取当前助手消息里名为 name 的未完成工具卡，没有就建一张。
   * 顺带保证一定存在一条"未定稿的助手消息"来挂这些卡。
   */
  ensureToolCall(name: string): ToolCallInfo {
    const last: ChatMessage = this.ensureAssistant();
    if (last.toolCalls === undefined) {
      last.toolCalls = [];
    }
    for (let i = 0; i < last.toolCalls.length; i++) {
      const t: ToolCallInfo = last.toolCalls[i];
      // 只复用"仍在执行"的同名卡：同一轮里同名工具被多次调用时，
      // 已完成的那张不能被后来的调用覆盖。
      const st: string | undefined = t.status;
      if (t.name === name && (st === undefined || st.length === 0 || st === 'running')) {
        return t;
      }
    }
    const created: ToolCallInfo = { name: name, args: '' };
    last.toolCalls.push(created);
    return created;
  }

  /**
   * 保证存在一条"未定稿的助手消息"，返回它。
   * 聚合 reasoning 可能先于任何 delta 到达（非流式后端就只有这一条），
   * 此时还没有"未完成的助手消息"，必须新建一条，否则内容直接丢失。
   */
  ensureAssistant(): ChatMessage {
    const last: ChatMessage | null = this.lastMessage();
    if (last !== null && last.role === 'assistant' && last.isFinal !== true) {
      return last;
    }
    const msg: ChatMessage = {
      id: this.allocId(),
      role: 'assistant',
      content: '',
      isStreaming: true,
      isFinal: false,
    };
    this.pushNew(msg);
    return msg;
  }

  /** 流式/高频变更的通知信号：让 ChatStream 滚到底 */
  requestScroll(): void {
    const cur: number = AppStorage.get<number>(K_CHAT_SCROLL_REV) ?? 0;
    AppStorage.set<number>(K_CHAT_SCROLL_REV, cur + 1);
  }

  /** 防抖刷新：合并高频 SSE delta，最多 ~20fps */
  refresh(): void {
    if (this.refreshTimer >= 0) {
      return;
    }
    this.refreshTimer = setTimeout(() => {
      this.refreshTimer = -1;
      this.msgs = this.msgs.slice();
      this.bump();
    }, 50);
  }

  /** 强制立即刷新（用于状态切换等需要即时响应的场景） */
  forceRefresh(): void {
    if (this.refreshTimer >= 0) {
      clearTimeout(this.refreshTimer);
      this.refreshTimer = -1;
    }
    this.msgs = this.msgs.slice();
    this.bump();
  }

  /** 明细数组不进 AppStorage，用一个自增版本号触发订阅组件重取 */
  bump(): void {
    const cur: number = AppStorage.get<number>(K_CHAT_REV) ?? 0;
    AppStorage.set<number>(K_CHAT_REV, cur + 1);
  }

  cancelRefresh(): void {
    if (this.refreshTimer >= 0) {
      clearTimeout(this.refreshTimer);
      this.refreshTimer = -1;
    }
  }

  // ===================== 折叠开关 =====================

  reasoningOpen(msgId: number): boolean {
    const m: ChatMessage | undefined = this.findMessage(msgId);
    return m !== undefined && m.reasoningOpen === true;
  }

  setReasoningOpen(msgId: number, open: boolean): void {
    const m: ChatMessage | undefined = this.findMessage(msgId);
    if (m !== undefined) {
      m.reasoningOpen = open;
    }
  }

  toolOpen(msgId: number, index: number): boolean {
    const m: ChatMessage | undefined = this.findMessage(msgId);
    if (m === undefined || m.toolCalls === undefined || index >= m.toolCalls.length) {
      return false;
    }
    return m.toolCalls[index].open === true;
  }

  setToolOpen(msgId: number, index: number, open: boolean): void {
    const m: ChatMessage | undefined = this.findMessage(msgId);
    if (m !== undefined && m.toolCalls !== undefined && index < m.toolCalls.length) {
      m.toolCalls[index].open = open;
    }
  }

  // ===================== 历史 =====================

  private parseHistory(obj: Record<string, Object>): ParsedHistory {
    return parseHistoryPayload(obj, () => this.allocId());
  }

  async loadHistory(): Promise<void> {
    try {
      // 分段懒加载：首屏只拉最新 CHAT_PAGE_SIZE 条，向上滚动触顶再拉更早的。
      const resp = await apiClient.getWithTimeout('/chat/history?limit=' + CHAT_PAGE_SIZE, 8000);
      const obj: Record<string, Object> = JSON.parse(resp.body) as Record<string, Object>;
      const parsed: ParsedHistory = this.parseHistory(obj);
      // 首屏允许列表本来就是空的（全部加载失败/新会话）：这里不做早退，
      // 否则游标 lastSeq 永远建不起来，增量轮询也就起不来。
      if (this.msgs.length === 0) {
        this.msgs = parsed.msgs;
      } else {
        // 与本地未回显的消息按 seq 对账，而不是整表替换。
        //
        // 为什么：sync_required 触发的 reloadHistory 会与刚发出的 POST 竞争；
        // 若历史快照里还没有这条 user 消息，整表替换会让它凭空消失
        // （“客户端侧发出的消息不显示”）。
        this.msgs = reconcileServerMsgs(this.msgs, parsed.msgs, false,
          () => this.allocId());
      }
      this.offset = parsed.offset;
      this.hasEarlier = parsed.hasMore;
      // 增量游标：首屏全量后据 last_seq 初始化，后续只拿增量。
      if (parsed.lastSeq > this.lastSeq) {
        this.lastSeq = parsed.lastSeq;
      }
      this.forceRefresh();
      this.requestScroll();
      this.startPolling();
    } catch (e) {
      // ignore history load failure
    }
  }

  /**
   * 增量轮询：只拉 seq 更大的消息，再补一次尾部探测。
   *
   * 这是 jianf 说的「接口调用方式改变」——后端 /chat/history 早已提供
   * after=<seq> 游标（commit 9711177），WebUI 前端据此 3s 轮询增量并 patch
   * 视图。鸿蒙端一直只做首屏全量加载，于是**其他端/其他渠道发来的消息
   * 永远进不来**（页面不会加载新的聊天信息）。
   *
   * 尾部探测不可省：工具调用与最终文本是**原地改写**已有 seq 的记录，
   * 不会产生新 seq，单靠 after 拿不到这些更新。
   */
  async pollIncremental(): Promise<void> {
    if (this.polling) {
      return;
    }
    this.polling = true;
    try {
      if (this.lastSeq <= 0) {
        // 游标还没建立（首屏没跑或失败）：退回全量，交给 loadHistory 建游标。
        this.polling = false;
        await this.loadHistory();
        return;
      }
      const resp = await apiClient.getWithTimeout(
        '/chat/history?after=' + this.lastSeq, 8000);
      const obj: Record<string, Object> = JSON.parse(resp.body) as Record<string, Object>;
      const parsed: ParsedHistory = this.parseHistory(obj);
      let changed: boolean = false;
      if (parsed.msgs.length > 0) {
        this.msgs = reconcileServerMsgs(this.msgs, parsed.msgs, false,
          () => this.allocId());
        changed = true;
      }
      if (parsed.lastSeq > this.lastSeq) {
        this.lastSeq = parsed.lastSeq;
      }
      // 尾部探测：拿最新一条做原地更新（工具卡/最终文本）。
      try {
        const tailResp = await apiClient.getWithTimeout('/chat/history?limit=1', 8000);
        const tailObj: Record<string, Object> = JSON.parse(tailResp.body) as Record<string, Object>;
        const tail: ParsedHistory = this.parseHistory(tailObj);
        if (tail.msgs.length > 0) {
          const before: number = this.msgs.length;
          this.msgs = reconcileServerMsgs(this.msgs, tail.msgs, true,
            () => this.allocId());
          if (this.msgs.length !== before) {
            changed = true;
          }
        }
      } catch (e) {
        // 尾部探测失败不影响增量结果
      }
      if (changed) {
        this.forceRefresh();
      }
    } catch (e) {
      // 轮询失败静默：下一拍会重试（SSE 仍在负责流式渲染）
    } finally {
      this.polling = false;
    }
  }

  /** 起 3s 增量轮询（与 WebUI 的 chatTicker 同节奏）。重复调用无副作用。 */
  startPolling(): void {
    if (this.pollTimer >= 0) {
      return;
    }
    this.pollTimer = setInterval(() => {
      this.pollIncremental();
    }, CHAT_POLL_MS);
  }

  stopPolling(): void {
    if (this.pollTimer >= 0) {
      clearInterval(this.pollTimer);
      this.pollTimer = -1;
    }
  }

  /**
   * 向上翻页：拉 offset 之前的更早一页，前置到 messages 头部并保持滚动位置。
   * 触顶（yOffset 接近 0）且有更早历史时由 onDidScroll 触发。
   */
  async loadOlder(): Promise<void> {
    if (this.loadingOlder || !this.hasEarlier) {
      return;
    }
    this.loadingOlder = true;
    try {
      const before: number = this.offset;
      if (before <= 0) {
        this.hasEarlier = false;
        return;
      }
      const resp = await apiClient.getWithTimeout(
        '/chat/history?limit=' + CHAT_PAGE_SIZE + '&before=' + before, 8000);
      const obj: Record<string, Object> = JSON.parse(resp.body) as Record<string, Object>;
      const parsed: ParsedHistory = this.parseHistory(obj);
      if (parsed.msgs.length === 0) {
        this.hasEarlier = false;
        return;
      }
      // 前置插入新页（更早的在前），追加到当前列表头部；id 用新分配的避免与新消息撞号
      this.msgs = parsed.msgs.concat(this.msgs);
      this.offset = parsed.offset;
      this.hasEarlier = parsed.hasMore;
      this.bump();
    } catch (e) {
      // 失败保留 hasEarlier，允许下次滚动重试
    } finally {
      this.loadingOlder = false;
    }
  }

  /** sync_required：断线重连后服务端要求补拉历史 */
  reloadHistory(): void {
    this.loadHistory();
  }

  // ===================== SSE 连接 =====================

  connect(): void {
    const cur = connStore.getCurrentConnection();
    if (cur === null) {
      return;
    }
    // 增量轮询与 SSE 同时拉起：SSE 负责 token 级流式观感，
    // 轮询负责「界面最终状态」——两者是两条腿，缺一不可
    // （轮询没接是“其他端/其他渠道的新消息永远不出现”的直接原因）。
    // 放在这里而不是只放在 loadHistory 末尾：首屏加载失败时也要能自愈。
    this.startPolling();
    this.sse.close();
    this.sse.connect(cur, '/chat/events',
      (ev: SseEvent) => {
        applyChatSse(ev, this);
      },
      () => {
        this.setConnected(false);
        this.scheduleReconnect();
      },
      () => {
        this.setConnected(true);
      }).catch(() => {
      this.setConnected(false);
      this.scheduleReconnect();
    });
  }

  scheduleReconnect(): void {
    if (this.reconnectTimer >= 0) {
      return;
    }
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = -1;
      this.connect();
    }, SSE_RECONNECT_MS);
  }

  cancelReconnect(): void {
    if (this.reconnectTimer >= 0) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = -1;
    }
  }

  /** 页面消失：断线、停表，避免后台空转 */
  disconnect(): void {
    this.cancelReconnect();
    this.stopPolling();
    this.sse.close();
    this.cancelRefresh();
  }
}

export const chatStore: ChatStore = new ChatStore();
