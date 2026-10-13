/**
 * SSE 事件 → 聊天流状态的翻译层（无 UI 依赖）。
 *
 * 从 pages/ChatPage.ets 抽出。这里只认「事件帧」与「往状态里写什么」，
 * 具体状态由 ChatStreamSink 提供 —— 这样本文件不必 import ChatStore，
 * 两边不会形成 ArkTS 里被拒绝的模块循环依赖。
 */

import { ChatMessage, ToolCallInfo, ChatAttachment } from '../model/Model';
import { attachmentFromChannelOutput } from './AttachmentMeta';
import { SseEvent } from './SseClient';
import { stringifyField } from './ChatFormat';
import { stageTrail } from './StageTrail';
import { hilog } from '@kit.PerformanceAnalysisKit';

/** 聊天流状态机对外暴露的最小写入面（由 common/ChatStore.ets 实现） */
export interface ChatStreamSink {
  allocId(): number;
  lastMessage(): ChatMessage | null;
  /** 取/建"未定稿的助手消息" */
  ensureAssistant(): ChatMessage;
  /** 取/建同名未完成工具卡 */
  ensureToolCall(name: string): ToolCallInfo;
  /** 追加一条新消息并播入场动画 */
  pushNew(msg: ChatMessage): void;
  setLoading(v: boolean): void;
  setStage(s: string): void;
  setSseActive(v: boolean): void;
  /** 防抖刷新（合并高频 delta） */
  refresh(): void;
  /** 请求滚到底 */
  requestScroll(): void;
  /** sync_required：补拉历史 */
  reloadHistory(): void;
}

export function applyChatSse(ev: SseEvent, sink: ChatStreamSink): void {
  try {
    // 服务端 data 字段是完整 sdk.Event：{type, source, payload, timestamp}
    // 业务字段全部在 payload 之下，历史实现直接读顶层导致流式/思考/工具调用全部失效。
    const frame: Record<string, Object> = JSON.parse(ev.data) as Record<string, Object>;
    const inner: Object | undefined = frame['payload'];
    const payload: Record<string, Object> =
      inner !== undefined && inner !== null ? inner as Record<string, Object> : frame;
    const frameType: string = frame['type'] as string ?? '';
    const type: string = ev.event.length > 0 ? ev.event : frameType;
    // 诊断只记事件类型（内容可能含隐私，不落盘）
    hilog.debug(0x0000, 'HomeAgent', 'sse %{public}s', type);

    if (type === 'agent_output') {
      const content: string = payload['content'] as string ?? '';
      // channel_output 携带图片/文件：url/size/output_type 三个字段在 payload 顶层，
      // 它是一条独立的附件消息，不能合并进上一条文本气泡。
      const kind: string = payload['kind'] as string ?? '';
      if (kind === 'channel_output') {
        const att: ChatAttachment | undefined = attachmentFromChannelOutput(
          payload['output_type'] as string ?? '',
          payload['url'] as string ?? '',
          payload['size'] as number ?? 0);
        if (att !== undefined) {
          const amsg: ChatMessage = {
            id: sink.allocId(),
            role: 'assistant',
            content: content,
            isFinal: true,
            source: 'channel',
            attachment: att,
          };
          sink.pushNew(amsg);
          sink.setLoading(false);
          sink.setStage('');
          sink.setSseActive(false);
          sink.refresh();
          sink.requestScroll();
          return;
        }
      }
      const last: ChatMessage | null = sink.lastMessage();
      if (last !== null && last.role === 'assistant' && !last.isFinal) {
        last.content = content;
        last.isFinal = true;
        last.isStreaming = false;
      } else if (last !== null && last.role === 'assistant' && last.isFinal) {
        // POST 已经创建了最终消息，仅合并内容（如果有增量）
        if (content.length > last.content.length) {
          last.content = content;
        }
      } else {
        const msg: ChatMessage = {
          id: sink.allocId(),
          role: 'assistant',
          content: content,
          isFinal: true,
        };
        sink.pushNew(msg);
      }
      sink.setLoading(false);
      sink.setStage('');
      sink.setSseActive(false);
      sink.refresh();
      sink.requestScroll();
    } else if (type === 'reasoning') {
      const rc: string = payload['content'] as string ?? '';
      if (rc.length > 0) {
        sink.setStage('AI 思考中...');
        // 聚合 reasoning 可能先于任何 delta 到达（非流式后端就只有这一条），
        // 此时还没有"未完成的助手消息"，必须新建一条，否则思考内容直接丢失。
        const last: ChatMessage = sink.ensureAssistant();
        last.reasoningContent = rc;
        sink.refresh();
        sink.requestScroll();
      }
    } else if (type === 'sync_required') {
      // 断线重连时服务端要求补拉历史（ring 里没有可重放的聚合事件）
      sink.reloadHistory();
    } else if (type === 'system') {
      // 内核级状态事件（目前唯一发布者是 plugin_crashed）。
      //
      // ★ 为何必须补上（2026-10-13 协议审计）：该事件一直在发
      //   （internal/plugin/dynamic_proc.go:175，注释还写着「webui/诊断插件
      //   可订阅」），但鸿蒙端从未处理 —— 插件崩溃对手机用户不可见。
      if (payload['event'] === 'plugin_crashed') {
        const nm: string = payload['plugin'] as string ?? '?';
        // 用 stage 条告知（鸿蒙端 sink 没有 toast 能力；stage 是已有的轻量提示位）。
        // 不用 setLoading(false)：崩溃不影响本轮对话本身。
        sink.setStage('插件 ' + nm + ' 异常退出，内核正在重启它');
        sink.refresh();
      }
    } else if (type === 'error') {
      // ★ 这里原本是 'agent_error' —— 那是一个**不存在的类型**（2026-10-13 审计）。
      //   events 包与 SDK 都没有这个常量，全仓无任何发布者，生产日志 0 次。
      //   而真正的失败帧叫 'error'（见 webui/handler_chat.go 与 cli/plugin.go
      //   都发 {type:"error", ...}），于是「本轮处理失败」提示根本没机会显示。
      //   改用真实类型名。
      sink.setLoading(false);
      sink.setStage('本轮处理失败，请重试');
      sink.refresh();
    } else if (type === 'content_delta') {
      const delta: string = payload['content'] as string ?? '';
      if (delta.length > 0) {
        sink.setSseActive(true);
        const last: ChatMessage = sink.ensureAssistant();
        last.content += delta;
        sink.refresh();
        sink.requestScroll();
      }
    } else if (type === 'reasoning_delta') {
      const delta: string = payload['content'] as string ?? '';
      if (delta.length > 0) {
        sink.setSseActive(true);
        sink.setStage('AI 思考中...');
        const last: ChatMessage = sink.ensureAssistant();
        if (last.reasoningContent === undefined) {
          last.reasoningContent = '';
        }
        last.reasoningContent += delta;
        sink.refresh();
      }
    } else if (type === 'tool_call') {
      const toolName: string = payload['tool'] as string ?? '';
      const toolStatus: string = payload['status'] as string ?? '';
      const toolPlugin: string = payload['plugin'] as string ?? '';
      if (toolName.length > 0) {
        sink.setStage('工具调用: ' + toolName);
        const target: ToolCallInfo = sink.ensureToolCall(toolName);
        if (toolPlugin.length > 0) {
          target.plugin = toolPlugin;
        }
        const argsText: string = stringifyField(payload['args']);
        if (argsText.length > 0) {
          target.args = argsText;
        }
        if (toolStatus.length > 0) {
          // 后端只在工具执行【结束】时发 tool_call（status=ok/denied/interrupted），
          // 所以拿到 status 就意味着这次调用已收尾，result 一并落卡。
          target.status = toolStatus;
          target.result = stringifyField(payload['result']);
        } else {
          target.status = 'running';
        }
        sink.refresh();
        sink.requestScroll();
      }
    } else if (type === 'stage') {
      const phase: string = payload['phase'] as string ?? '';
      const channel: string = payload['channel'] as string ?? '';
      const stageTool: string = payload['tool'] as string ?? '';
      if (channel !== '_consolidation_') {
        // 运行态面板的阶段管道靠这条轨迹活着：先喂轨迹，再管聊天侧的角标。
        // 两者是独立消费者，轨迹不依赖任何聊天状态。
        stageTrail.onStage(phase, stageTool);
        if (phase === 'pre_action') {
          sink.setStage('AI 思考中...');
        } else if (phase === 'before_toolcall') {
          sink.setStage('工具调用: ' + stageTool);
          // 关键：tool_call 事件只在执行【结束】后才发，所以"调用中"这一态
          // 必须由 before_toolcall 建卡，否则用户永远看不到工具正在跑。
          if (stageTool.length > 0) {
            const tc: ToolCallInfo = sink.ensureToolCall(stageTool);
            if (tc.status === undefined) {
              tc.status = 'running';
            }
          }
        } else if (phase === 'before_output') {
          sink.setStage('生成回复中...');
        }
        sink.refresh();
      }
    }
  } catch (e) {
    // ignore parse errors
  }
}
