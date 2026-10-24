/**
 * 发送 / 中断（无 UI 依赖）。
 *
 * 从 pages/ChatPage.ets 抽出：POST /chat 与 POST /chat/file 的请求体、
 * 兜底消息合并、超时口径都收在这里，输入区组件只负责把文本/附件递进来。
 */

import { ChatMessage, ChatAttachment } from '../model/Model';
import { apiClient } from './ApiClient';
import { connStore } from './ConnStore';
import { userMessage, isTimeout } from './UserError';
import { chatStore } from './ChatStore';
import { parseAttachment } from './AttachmentMeta';
import { http } from '@kit.NetworkKit';

interface SendChatBody {
  message: string;
  client_msg_id: string;
  /** 非空时后端编码 source = "webui/<device_id>"，agent 可见来源设备 */
  device_id?: string;
  device_name?: string;
}

/** POST /chat/interrupt 的请求体。 */
interface InterruptBody {
  /** true = 停止（立即结束当前推理 + 短路已排队消息）；false/省略 = 普通中断。 */
  stop: boolean;
  /** 可选：中断时附带给模型的一句话；停止时为 undefined。 */
  message?: string;
}

/**
 * 纯文本发送：POST /chat。
 * 带附件的情况走 sendChatFile（后端收下附件后自己写会话并触发 agent）。
 */
export async function sendChatText(text: string): Promise<void> {
  const trimmed: string = text.trim();
  if (chatStore.isLoading()) {
    return;
  }
  const cur = connStore.getCurrentConnection();
  if (cur === null) {
    return;
  }
  if (trimmed.length === 0) {
    return;
  }
  // 重置 SSE 标记
  chatStore.setSseActive(false);

  const userMsg: ChatMessage = { id: chatStore.allocId(), role: 'user', content: trimmed };
  chatStore.pushNew(userMsg);
  chatStore.setLoading(true);
  chatStore.setStage('等待 AI 回复...');
  chatStore.forceRefresh();
  chatStore.requestScroll();

  const bodyObj: SendChatBody = {
    message: trimmed,
    client_msg_id: Date.now().toString(36),
    // 必须带设备身份：后端没有 device_id 就把来源编码成 webui，
    // agent 会以为消息来自网页端。device_id 非空时后端编码
    // source = "webui/<device_id>" 并注入设备上下文。
    device_id: connStore.ensureDeviceId(),
    device_name: connStore.getDeviceName(),
  };

  // 如果 SSE 已连接，POST 作为触发器（响应由 SSE 推送渲染）；
  // 仅在 SSE 未推送内容时才用 POST 响应兜底创建消息。
  try {
    const resp = await apiClient.postWithTimeout('/chat', bodyObj, 120000);

    // SSE 已经处理了响应，跳过 POST 消息创建
    if (chatStore.sseActive()) {
      chatStore.setLoading(false);
      chatStore.setStage('');
      chatStore.forceRefresh();
      chatStore.requestScroll();
      return;
    }

    const parsed: Record<string, string> = JSON.parse(resp.body) as Record<string, string>;
    const respText: string = parsed['response'] ?? '(无响应)';
    const reasoning: string = parsed['reasoning_content'] ?? '';
    const last: ChatMessage | null = chatStore.lastMessage();
    if (last !== null && last.role === 'assistant' && !last.isFinal) {
      last.content = respText;
      last.isFinal = true;
      last.isStreaming = false;
      if (reasoning.length > 0 && last.reasoningContent === undefined) {
        last.reasoningContent = reasoning;
      }
    } else if (last !== null && last.role === 'assistant' && last.isFinal) {
      // 已有最终消息，合并（不应发生，但防御性处理）
      if (respText.length > last.content.length) {
        last.content = respText;
      }
    } else {
      const msg: ChatMessage = {
        id: chatStore.allocId(),
        role: 'assistant',
        content: respText,
        isFinal: true,
      };
      if (reasoning.length > 0) {
        msg.reasoningContent = reasoning;
      }
      chatStore.pushNew(msg);
    }
    chatStore.setLoading(false);
    chatStore.setStage('');
    chatStore.forceRefresh();
    chatStore.requestScroll();
  } catch (e) {
    // 超时通常意味着后端仍在生成，不算失败；其余一律显示人话，
    // 原始错误只进 hilog（之前把 e.message 拼进 chatStage 会把
    // "Failed to connect to the server."、内网地址直接摆到聊天流里）。
    if (isTimeout(e)) {
      chatStore.setStage('请求已发送，等待回复...');
    } else {
      chatStore.setStage(userMessage('chat.send', e));
    }
    chatStore.forceRefresh();
    chatStore.requestScroll();
  }
}

/**
 * 带附件发送：POST /chat/file（multipart），字段与 WebGUI 一致。
 * 后端收下后自身会把用户消息与附件写进会话并触发 agent，
 * 回复照常从 SSE 过来，所以这里不再走 /chat。
 */
export async function sendChatFile(text: string, path: string, name: string,
                                   size: number, isImage: boolean, mime: string): Promise<void> {
  if (connStore.getCurrentConnection() === null) {
    return;
  }
  const att: ChatAttachment = {
    type: isImage ? 'image' : 'file',
    // 本地待上传：先用沙箱路径预览，上传成功后替换成服务端 URL
    url: 'file://' + path,
    size: size,
    name: name,
  };
  const userMsg: ChatMessage = { id: chatStore.allocId(), role: 'user', content: text };
  userMsg.attachment = att;
  chatStore.pushNew(userMsg);
  chatStore.setLoading(true);
  chatStore.setStage('正在上传附件...');
  chatStore.setSseActive(false);
  chatStore.forceRefresh();
  chatStore.requestScroll();

  const parts: http.MultiFormData[] = [
    { name: 'file', contentType: mime, remoteFileName: name, filePath: path },
    { name: 'message', contentType: 'text/plain', data: text },
    { name: 'client_msg_id', contentType: 'text/plain', data: Date.now().toString(36) },
    { name: 'device_id', contentType: 'text/plain', data: connStore.ensureDeviceId() },
    { name: 'device_name', contentType: 'text/plain', data: connStore.getDeviceName() },
  ];

  try {
    const resp = await apiClient.postMultipart('/chat/file', parts, 180000);
    const obj: Record<string, Object> = JSON.parse(resp.body) as Record<string, Object>;
    const uploaded: ChatAttachment | undefined = parseAttachment(obj['file']);
    if (uploaded !== undefined) {
      userMsg.attachment = uploaded;
    }
    chatStore.setStage('等待 AI 回复...');
  } catch (e) {
    chatStore.setStage(userMessage('chat.upload', e));
    chatStore.setLoading(false);
  }
  chatStore.forceRefresh();
  chatStore.requestScroll();
}

/**
 * 停止当前生成（停止按钮）。
 *
 * 发送 **`stop: true`**，与「带一句话的中断」区分开：
 *   - stop:true（无 message）= ①立即结束当前 LLM 推理（不重试）；
 *     ②对停止那一刻已排队的消息，后端在 pre-action 逐个短路。
 *   - message 非空 = 普通中断，模型看到被打断的上下文 + 新输入。
 *
 * 为什么必须带 stop：此前这里 POST 的是 null（空 body），后端把空内容当成
 * “无事发生”直接丢掉了——接口回 200 但生成继续跑到自然结束，也就是“按了没反应”。
 * 带中文字段比空 body 多不了几个字节，就把语义说清楚了。
 */
export async function interruptChat(): Promise<void> {
  if (connStore.getCurrentConnection() === null) {
    return;
  }
  try {
    const body: InterruptBody = { stop: true };
    await apiClient.post('/chat/interrupt', body);
  } catch (e) {
    // 停止是“减少工作”的指令，失败不需打断用户；但状态必须复位，
    // 否则按钮会一直停在“停止”态，用户以为没生效。
  }
  // 即时反馈：不等 SSE 的终态事件，先把本地忙态清掉。
  // 若后端稍后真的推来终态，SSE 处理器会再刷一次（幂等）。
  chatStore.setLoading(false);
  chatStore.setStage('已停止');
  chatStore.forceRefresh();
}
