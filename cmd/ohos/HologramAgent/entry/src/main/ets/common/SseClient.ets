import { http } from '@kit.NetworkKit';
import { ConnectionConfig } from '../model/Model';

export interface SseEvent {
  event: string;
  data: string;
  id: string;
}

export type SseHandler = (ev: SseEvent) => void;
export type SseCloseHandler = () => void;
export type SseOpenHandler = () => void;

function decodeUtf8(bytes: Uint8Array): string {
  let result: string = '';
  let i: number = 0;
  while (i < bytes.length) {
    const b: number = bytes[i];
    if (b < 0x80) {
      result += String.fromCharCode(b);
      i++;
    } else if (b < 0xC0) {
      i++;
    } else if (b < 0xE0) {
      if (i + 1 < bytes.length) {
        result += String.fromCharCode(((b & 0x1F) << 6) | (bytes[i + 1] & 0x3F));
        i += 2;
      } else {
        i++;
      }
    } else if (b < 0xF0) {
      if (i + 2 < bytes.length) {
        result += String.fromCharCode(
          ((b & 0x0F) << 12) | ((bytes[i + 1] & 0x3F) << 6) | (bytes[i + 2] & 0x3F),
        );
        i += 3;
      } else {
        i++;
      }
    } else {
      if (i + 3 < bytes.length) {
        const cp: number =
          ((b & 0x07) << 18) |
          ((bytes[i + 1] & 0x3F) << 12) |
          ((bytes[i + 2] & 0x3F) << 6) |
          (bytes[i + 3] & 0x3F);
        const adjusted: number = cp - 0x10000;
        result += String.fromCharCode(0xD800 + (adjusted >> 10));
        result += String.fromCharCode(0xDC00 + (adjusted & 0x3FF));
        i += 4;
      } else {
        i++;
      }
    }
  }
  return result;
}

export class SseClient {
  private httpRequest: http.HttpRequest | null = null;
  private buffer: string = '';
  private lastEventId: string = '';
  private closed: boolean = false;
  private opened: boolean = false;
  private onEvent: SseHandler | null = null;
  private onClose: SseCloseHandler | null = null;
  private onOpen: SseOpenHandler | null = null;

  setLastEventId(id: string): void {
    this.lastEventId = id;
  }

  getLastEventId(): string {
    return this.lastEventId;
  }

  async connect(conn: ConnectionConfig, path: string,
                onEvent: SseHandler, onClose: SseCloseHandler,
                onOpen: SseOpenHandler | null = null): Promise<void> {
    this.onEvent = onEvent;
    this.onClose = onClose;
    this.onOpen = onOpen;
    this.closed = false;
    this.opened = false;
    this.buffer = '';
    this.curEvent = '';
    this.curData = '';
    this.curId = '';

    const base: string = conn.url.replace(/\/+$/, '');
    const url: string = base + '/api/v1' + path;

    const headers: Record<string, string> = {
      'Accept': 'text/event-stream',
      'Cache-Control': 'no-store',
    };
    if (conn.apiKey.length > 0) {
      // 后端 validAPIKey 两种都认；有些反代只放行 Authorization，两个都带更稳
      headers['X-API-Key'] = conn.apiKey;
      headers['Authorization'] = 'Bearer ' + conn.apiKey;
    }
    if (this.lastEventId.length > 0) {
      headers['Last-Event-ID'] = this.lastEventId;
    }

    const req: http.HttpRequest = http.createHttp();
    this.httpRequest = req;

    req.on('headersReceive', (header: Object) => {
      const ct: string = this.getHeaderValue(header, 'content-type');
      if (ct.indexOf('text/event-stream') >= 0) {
        this.markOpened();
      } else {
        this.finish();
      }
    });

    req.on('dataReceive', (chunk: ArrayBuffer) => {
      if (this.closed) {
        return;
      }
      this.markOpened();
      const bytes: Uint8Array = new Uint8Array(chunk);
      const text: string = decodeUtf8(bytes);
      this.buffer += text;
      this.processBuffer();
    });

    req.on('dataEnd', () => {
      this.finish();
    });

    const options: http.HttpRequestOptions = {
      method: http.RequestMethod.GET,
      header: headers,
      expectDataType: http.HttpDataType.ARRAY_BUFFER,
      usingCache: false,
      readTimeout: 3600000,
      connectTimeout: 15000,
    };

    try {
      await req.requestInStream(url, options);
    } catch (e) {
      this.finish();
    }
  }

  private getHeaderValue(header: Object, name: string): string {
    try {
      const rec: Record<string, string> = header as Record<string, string>;
      const lower: string = name.toLowerCase();
      const keys: string[] = Object.keys(rec);
      for (let i = 0; i < keys.length; i++) {
        if (keys[i].toLowerCase() === lower) {
          const val: string | undefined = rec[keys[i]];
          return val !== undefined ? val : '';
        }
      }
    } catch (e) {
      // ignore
    }
    return '';
  }

  /**
   * 帧解析状态必须【跨 chunk 保持】。
   *
   * 之前把 eventType/data/id 作为 processBuffer 的局部变量，
   * 而 TCP 分片完全可能切在帧内部的换行处（服务端 16ms 批量 flush 时
   * 一次写入几十帧，尾部被切开是常态）：
   *   chunk1 = "...event: content_delta\n"
   *   chunk2 = "data: {...}\n\n"
   * 于是 chunk1 解析出的 event 被丢掉，chunk2 只剩 data 而没有事件名，
   * 整帧被静默丢弃 —— 表现就是"工具调用和思考不显示、也不是流式"。
   */
  private curEvent: string = '';
  private curData: string = '';
  private curId: string = '';

  private processBuffer(): void {
    const lines: string[] = this.buffer.split('\n');
    this.buffer = lines.pop() ?? '';
    for (let i = 0; i < lines.length; i++) {
      // 兼容 CRLF：\r 会污染事件名与 JSON 尾部
      let line: string = lines[i];
      if (line.length > 0 && line.charAt(line.length - 1) === '\r') {
        line = line.substring(0, line.length - 1);
      }
      if (line.startsWith(':')) {
        // 注释行（心跳），忽略
        continue;
      }
      if (line.startsWith('id:')) {
        this.curId = line.substring(3).trim();
        if (this.curId.length > 0) {
          this.lastEventId = this.curId;
        }
      } else if (line.startsWith('event:')) {
        this.curEvent = line.substring(6).trim();
      } else if (line.startsWith('data:')) {
        // SSE 规范：data: 后的单个空格属于分隔符，其余原样保留；
        // 多行 data 用换行拼接。
        let chunk: string = line.substring(5);
        if (chunk.startsWith(' ')) {
          chunk = chunk.substring(1);
        }
        this.curData = this.curData.length > 0 ? this.curData + '\n' + chunk : chunk;
      } else if (line === '') {
        if (this.curEvent.length > 0 && this.curData.length > 0) {
          const ev: SseEvent = { event: this.curEvent, data: this.curData, id: this.curId };
          if (this.onEvent !== null) {
            this.onEvent(ev);
          }
        }
        this.curEvent = '';
        this.curData = '';
        this.curId = '';
      }
    }
  }

  close(): void {
    this.closed = true;
    this.opened = false;
    if (this.httpRequest !== null) {
      try {
        this.httpRequest.off('dataReceive');
        this.httpRequest.off('dataEnd');
        this.httpRequest.off('headersReceive');
        this.httpRequest.destroy();
      } catch (e) {
        // ignore
      }
      this.httpRequest = null;
    }
  }

  private markOpened(): void {
    if (this.opened || this.closed) {
      return;
    }
    this.opened = true;
    if (this.onOpen !== null) {
      this.onOpen();
    }
  }

  private finish(): void {
    if (this.closed) {
      return;
    }
    this.close();
    if (this.onClose !== null) {
      this.onClose();
    }
  }
}
