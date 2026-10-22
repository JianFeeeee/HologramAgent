import { webSocket } from '@kit.NetworkKit';
import { hilog } from '@kit.PerformanceAnalysisKit';
import { CapResult } from './BridgeCaps';
import { BridgeCmdHandler, bridgeHelloFrame, bridgeBindFrame, bridgeResultFrame,
  bridgeDataStartFrame, bridgeDataEndFrame, bridgeEventFrame, bridgeStatusFrame,
  bridgeChunkSlices } from './BridgeProtocol';

export class DeviceBridgeClient {
  private ws: webSocket.WebSocket = webSocket.createWebSocket();
  private url: string = '';
  private token: string = '';
  private deviceId: string = '';
  private name: string = 'HologramAgent OHOS';
  private kind: string = 'phone';
  private caps: string[] = [];
  private hostname: string = 'ohos';
  private connected: boolean = false;
  private bound: boolean = false;
  private manualClose: boolean = false;
  private reconnectTimer: number = -1;
  private connectionGeneration: number = 0;
  private cmdHandler: BridgeCmdHandler | null = null;
  private onStateChange: ((open: boolean) => void) | null = null;

  isConnected(): boolean {
    return this.connected && this.bound;
  }

  getDeviceId(): string {
    return this.deviceId;
  }

  setCmdHandler(handler: BridgeCmdHandler): void {
    this.cmdHandler = handler;
  }

  setStateListener(listener: (open: boolean) => void): void {
    this.onStateChange = listener;
  }

  async connect(url: string, token: string, deviceId: string,
                caps: string[], hostname: string,
                authorized: boolean, name: string): Promise<void> {
    this.url = url;
    this.token = token;
    this.deviceId = deviceId;
    this.caps = caps;
    this.hostname = hostname;
    this.name = name;
    this.manualClose = false;
    await this.openAndRegister(authorized);
  }

  private async openAndRegister(authorized: boolean): Promise<void> {
    try {
      this.ws.off('open');
      this.ws.off('message');
      this.ws.off('close');
      this.ws.off('error');
      this.ws.close().catch(() => {
        // ignore stale socket close failure
      });
    } catch (e) {
      // ignore stale socket cleanup failure
    }
    this.connectionGeneration = this.connectionGeneration + 1;
    const generation: number = this.connectionGeneration;
    const socket: webSocket.WebSocket = webSocket.createWebSocket();
    this.ws = socket;
    this.connected = false;
    this.bound = false;
    this.bindWsEvents(socket, authorized, generation);
    const opts: webSocket.WebSocketRequestOptions = {
      header: this.authHeader(),
    };
    try {
      await socket.connect(this.url, opts);
    } catch (e) {
      if (generation === this.connectionGeneration && !this.manualClose) {
        this.connected = false;
        this.bound = false;
        this.notifyState(false);
        this.scheduleReconnect();
      }
    }
  }

  /** WebUI 用 API key 验证外层连接，并由反代向设备网关注入其内部 token。 */
  private authHeader(): Record<string, string> {
    const h: Record<string, string> = {};
    if (this.token.length > 0) {
      h['X-API-Key'] = this.token;
      h['Authorization'] = 'Bearer ' + this.token;
    }
    return h;
  }

  private bindWsEvents(socket: webSocket.WebSocket, authorized: boolean, generation: number): void {
    socket.on('open', (err: Error, value: Object) => {
      if (generation !== this.connectionGeneration || this.manualClose) {
        socket.close().catch(() => {
          // ignore stale socket close failure
        });
        return;
      }
      this.connected = true;
      this.bound = false;
      this.sendHello(authorized);
      this.sendBind();
    });
    socket.on('message', (err: Error, value: string | ArrayBuffer) => {
      if (generation === this.connectionGeneration && typeof value === 'string') {
        this.handleTextFrame(value);
      }
    });
    socket.on('close', (err: Error, value: webSocket.CloseResult) => {
      this.handleSocketEnd(generation);
    });
    socket.on('error', (err: Error) => {
      this.handleSocketEnd(generation);
    });
  }

  private handleSocketEnd(generation: number): void {
    if (generation !== this.connectionGeneration) {
      return;
    }
    this.connected = false;
    this.bound = false;
    this.notifyState(false);
    this.scheduleReconnect();
  }

  private notifyState(open: boolean): void {
    if (this.onStateChange !== null) {
      this.onStateChange(open);
    }
  }

  private scheduleReconnect(): void {
    if (this.manualClose || this.reconnectTimer >= 0) {
      return;
    }
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = -1;
      if (this.manualClose || this.url.length === 0) {
        return;
      }
      this.openAndRegister(this.lastAuthorized);
    }, 5000);
  }

  private lastAuthorized: boolean = false;

  private cancelReconnect(): void {
    if (this.reconnectTimer >= 0) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = -1;
    }
  }

  /** 更新本地授权状态并在已绑定连接上同步到服务端。 */
  updateAuthorized(authorized: boolean): void {
    this.lastAuthorized = authorized;
    if (this.connected && this.bound) {
      this.sendHello(authorized);
    }
  }

  disconnect(): void {
    this.manualClose = true;
    this.connectionGeneration = this.connectionGeneration + 1;
    this.cancelReconnect();
    this.connected = false;
    this.bound = false;
    try {
      this.ws.off('open');
      this.ws.off('message');
      this.ws.off('close');
      this.ws.off('error');
      this.ws.close().catch(() => {
        // ignore
      });
    } catch (e) {
      // ignore
    }
    this.notifyState(false);
  }

  private sendHello(authorized: boolean): void {
    this.lastAuthorized = authorized;
    this.send(bridgeHelloFrame(this.deviceId, this.name, this.kind, this.caps,
      this.hostname, authorized));
  }

  private sendBind(): void {
    this.send(bridgeBindFrame(this.deviceId, this.token));
  }

  // ===== 命令处理 =====

  private handleTextFrame(text: string): void {
    let obj: Record<string, Object>;
    try {
      obj = JSON.parse(text) as Record<string, Object>;
    } catch (e) {
      return;
    }
    const op: string = obj['op'] as string ?? '';
    if (op === 'bind_ack') {
      const accepted: boolean = obj['ok'] === true;
      if (accepted && this.connected && !this.manualClose) {
        this.bound = true;
        this.cancelReconnect();
        this.notifyState(true);
      } else {
        this.bound = false;
        this.notifyState(false);
        try {
          this.ws.close().catch(() => {
            // ignore bind rejection close failure
          });
        } catch (e) {
          this.scheduleReconnect();
        }
      }
      return;
    }
    if (op !== 'cmd') {
      // ★★ 2026-10-13 协议审计：这里原本是静默 return，而服务端**会**发
      //   op=push / cmd_speech_start / cmd_speech_end / hello_ack。
      //   本机（鸿蒙端）只做「被控设备」，不做输出通道，所以收到这些 op
      //   属预期——但**静默丢弃**会让「服务端以为发出去了、本端却毫无痕迹」，
      //   排查时只能对时序猜。至少留一行日志，让“收到了但不支持”与
      //   “根本没收到”可区分。
      //
      //   为何不报错回去：这些 op 不要求回执（服务端 pushToDevice 不等
      //   device→server 的应答），回一个错误帧反而会被当成未知帧处理。
      if (op.length > 0) {
        hilog.debug(0x0000, 'HologramAgent', 'device bridge: 忽略不支持的 op %{public}s', op);
      }
      return;
    }
    if (!this.bound) {
      return;
    }
    const reqId: string = obj['req_id'] as string ?? '';
    const command: string = obj['command'] as string ?? '';
    if (reqId.length === 0 || command.length === 0) {
      return;
    }
    if (!this.lastAuthorized) {
      this.sendResult(reqId, 'error', '', '设备未授权：请在设备页开启远程控制授权');
      return;
    }
    this.dispatchCommand(reqId, command);
  }

  private dispatchCommand(reqId: string, command: string): void {
    if (this.cmdHandler === null) {
      this.sendResult(reqId, 'error', '', '本机能力尚未就绪，请保持应用在前台后重试');
      return;
    }
    const handler: BridgeCmdHandler = this.cmdHandler;
    handler(reqId, command).then((res: CapResult) => {
      // res.chunked 时结果已由能力自己用二进制分块发完（如录像）：
      // 此时再发 cmd_result 会让网关把一条已完成请求与后续分块错配。
      if (res.chunked === true) {
        return;
      }
      this.sendResult(reqId, res.status, res.output, res.error);
    }).catch((e: Object) => {
      this.sendResult(reqId, 'error', '', '本机能力执行失败，请稍后重试');
    });
  }

  sendResult(reqId: string, status: string, output: string, errMsg: string): void {
    this.send(bridgeResultFrame(reqId, status, output, errMsg));
  }

  // ===== 二进制分块回传（协议与 GUI 客户端一致）=====

  sendDataChunked(reqId: string, kind: string, mime: string, bytes: Uint8Array): void {
    this.send(bridgeDataStartFrame(reqId, kind, mime, bytes.byteLength));
    const chunks: Uint8Array[] = bridgeChunkSlices(bytes);
    for (let i = 0; i < chunks.length; i++) {
      const ab: ArrayBuffer = chunks[i].buffer as ArrayBuffer;
      try {
        this.ws.send(ab).catch(() => {
          // ignore per-chunk failure; end frame reports error below
        });
      } catch (e) {
        break;
      }
    }
    this.send(bridgeDataEndFrame(reqId));
  }

  sendEvent(eventType: string, detail: string): void {
    this.send(bridgeEventFrame(this.deviceId, eventType, detail));
  }

  sendStatus(status: string): void {
    this.send(bridgeStatusFrame(this.deviceId, status));
  }

  send(text: string): void {
    if (!this.connected) {
      return;
    }
    this.ws.send(text).catch(() => {
      // ignore
    });
  }
}

export const deviceBridge: DeviceBridgeClient = new DeviceBridgeClient();
