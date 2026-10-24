/**
 * 聊天消息流：列表 + 顶栏遮罩 + 底部淡出遮罩 + 滚动/懒加载。
 *
 * 从 pages/ChatPage.ets 抽出（原来是 ChatBody 里除悬浮输入区之外的三层）。
 * 消息数组来自 common/ChatStore.ets：用版本号 K_CHAT_REV 订阅，
 * 版本变化时重取一次快照（数组引用每次都是新的，ForEach 的渲染语义
 * 与拆分前 this.messages = this.messages.slice() 完全一致）。
 */

import { ChatMessage } from '../model/Model';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, ANIM_FAST, K_HAS_CONN, K_REQUESTED_TAB, K_SETTINGS_SUB } from '../common/Constants';
import { SUB_CONNECTIONS } from '../common/SettingsModel';
import { navBar } from '../common/NavBarController';
import { ChatAttachment } from '../model/Model';
import { chatStore, K_CHAT_REV, K_CHAT_SCROLL_REV, K_CHAT_LOADING, K_CHAT_STAGE } from '../common/ChatStore';
import { isChannelMsg, isSelfMsg, structSig } from '../common/ChatFormat';
import { connStore } from '../common/ConnStore';
import { ChatBubble } from './ChatBubble';
import { PageTopBar } from './PageTopBar';

@Component
export struct ChatStream {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  /** 是否已配置后端连接（决定空态是引导连接还是引导开聊） */
  @StorageProp(K_HAS_CONN) private hasConn: boolean = false;
  @StorageProp(K_CHAT_LOADING) private loading: boolean = false;
  @StorageProp(K_CHAT_STAGE) private stage: string = '';
  /** 数组快照的订阅信号 */
  @StorageProp(K_CHAT_REV) @Watch('onChatRev') private rev: number = 0;
  /** "滚到底"请求信号 */
  @StorageProp(K_CHAT_SCROLL_REV) @Watch('onScrollReq') private scrollRev: number = 0;
  @State messages: ChatMessage[] = [];
  /** 列表底部留白：随输入区展开/加号菜单/待发送附件变化 */
  @Prop bottomPad: number = 210;
  onOpenAttachment?: (att: ChatAttachment) => void;

  private scroller: Scroller = new Scroller();
  private autoScrolling: boolean = false;
  /** 滚动世代号：scrollRev 每次变化自增，旧一轮的延迟滚动据此作废 */
  private scrollGen: number = 0;
  private navHidden: boolean = false;

  aboutToAppear(): void {
    this.messages = chatStore.messages();
    // 首帧如果已经有消息（历史加载先于本组件挂载完成），必须自己滚到底。
    //
    // 为何必须补这一下：@Watch 只在值**变化**时触发，不触发初始值。
    // ChatPage.aboutToAppear 里 loadHistory() 是异步的，若它在 ChatStream
    // 构造之前就完成了，requestScroll 递增的 chatScrollRev 就成了“挂载前
    // 已经发生的变化”——本组件的 onScrollReq 永远不会被调到，表现就是
    // “消息加载好了却停在顶部/中间，不滚到最新”。
    if (this.messages.length > 0) {
      this.scrollToBottom();
    }
  }

  private onChatRev(): void {
    this.messages = chatStore.messages();
  }

  private onScrollReq(): void {
    this.scrollToBottom();
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  /**
   * 底部淡出遮罩的两个端色：背景底色的全不透明 / 全透明版本。
   * bgPrimary 是 6 位十六进制，这里手拼 8 位 ARGB —— 与 PageTopBar
   * 顶部淡出用的是同一手法，保证上下两端的融入观感一致。
   */
  private opaqueBottomBg(): string {
    return '#FF' + this.palette().bgPrimary.substring(1);
  }

  private transparentBottomBg(): string {
    return '#00' + this.palette().bgPrimary.substring(1);
  }

  private scrollToBottom(): void {
    this.autoScrolling = true;
    // 多次重试：内容高度是消息数组更新后**若干帧内**才逐步确定的，
    // 长历史 / Markdown / 思考卡 / 工具卡布局都慢。旧实现只重试到 260ms，
    // 长历史下那一次仍落在“当时”的底部（用户看到的是加载完停在中间）。
    // 用递增间隔重试到 ~1s，让后几帧的布局增长也跟得上。
    //
    // scrollRev 变化时旧一轮的定时器不能继续干预新滚动，用世代号作废。
    const gen: number = ++this.scrollGen;
    const delays: number[] = [50, 120, 220, 360, 550, 800];
    for (let i = 0; i < delays.length; i++) {
      setTimeout(() => {
        if (gen !== this.scrollGen) {
          return;
        }
        this.scroller.scrollEdge(Edge.Bottom);
      }, delays[i]);
    }
    setTimeout(() => {
      if (gen !== this.scrollGen) {
        return;
      }
      this.autoScrolling = false;
      this.navHidden = false;
      navBar.setVisible(true);
    }, 900);
  }

  /**
   * 滚动回调：只设置普通标志位，仅在状态翻转时通知 navBar，
   * 不在回调里做任何耗时操作。navBar.setVisible 内部已去重，
   * 而布局（padding）不再依赖 navVisible，故翻转只触发 GPU 变换，
   * 不会引起布局回流——这是滑动流畅的关键。
   */
  private handleScrollDirection(yOffset: number, state: ScrollState): void {
    if (this.autoScrolling) {
      return;
    }
    // 触顶（近顶部 60vp）且服务端还有更早历史 → 向上懒加载下一页
    if (yOffset < 60 && chatStore.hasMore()) {
      chatStore.loadOlder();
    }
    if (state === ScrollState.Idle) {
      if (this.navHidden) {
        this.navHidden = false;
        navBar.setVisible(true);
      }
    } else {
      // Scroll / Fling：向下/惯性滚动时隐藏导航与输入栏
      if (!this.navHidden) {
        this.navHidden = true;
        navBar.setVisible(false);
      }
    }
  }

  build() {
    Stack({ alignContent: Alignment.Bottom }) {
      // 层1：消息列表（铺满全屏，内容从顶栏遮罩下方穿过时逐渐淡出）
      Column() {
        Scroll(this.scroller) {
          Column() {
            ForEach(this.messages, (msg: ChatMessage, idx: number) => {
              ChatBubble({
                msgId: msg.id,
                role: msg.role,
                content: msg.content,
                isStreaming: msg.isStreaming === true,
                isFinal: msg.isFinal === true,
                reasoningContent: msg.reasoningContent ?? '',
                reasoningOpen: msg.reasoningOpen === true,
                toolCalls: msg.toolCalls ?? [],
                attachment: msg.attachment,
                source: msg.source ?? '',
                channel: isChannelMsg(msg.source ?? '', connStore.ensureDeviceId()),
                mine: isSelfMsg(msg.role, isChannelMsg(msg.source ?? '', connStore.ensureDeviceId())),
                fresh: chatStore.isFresh(msg.id),
                onOpenAttachment: (att: ChatAttachment) => {
                  const cb: ((a: ChatAttachment) => void) | undefined = this.onOpenAttachment;
                  if (cb !== undefined) {
                    cb(att);
                  }
                },
                onToggleReasoning: (id: number) => {
                  // 在 animateTo 里翻转：展开/收起时 chevron 走已有 .animation，
                  // 面板节点在 animateTo 帧内获得默认过渡，不会再硬切。
                  this.getUIContext().animateTo({ duration: 220, curve: Curve.EaseOut }, () => {
                    chatStore.setReasoningOpen(id, !chatStore.reasoningOpen(id));
                    chatStore.forceRefresh();
                  });
                },
                onToggleTool: (id: number, index: number) => {
                  this.getUIContext().animateTo({ duration: 220, curve: Curve.EaseOut }, () => {
                    chatStore.setToolOpen(id, index, !chatStore.toolOpen(id, index));
                    chatStore.forceRefresh();
                  });
                },
              })
            }, (msg: ChatMessage, idx: number) => structSig(msg))

            if (this.loading) {
              Row({ space: 8 }) {
                LoadingProgress()
                  .width(16)
                  .height(16)
                  .color(this.palette().accent)
                Text(this.stage.length > 0 ? this.stage : '处理中...')
                  .fontSize(12)
                  .fontColor(this.palette().textMuted)
              }
              .width('100%')
              .justifyContent(FlexAlign.Start)
              .padding({ left: 52, top: 6, bottom: 6 })
              // if 控制的节点无法用 .animation() 做进出场（那只驱动自身属性的增量更新）；
              // 用 transition 才能让"处理中"这条在出现和消失时都淡入淡出。
              .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
            }
          }
          .width('100%')
          .padding({
            left: 14,
            right: 14,
            top: 76,
            bottom: this.bottomPad,
          })
        }
        .width('100%')
        .height('100%')
        .scrollBar(BarState.Off)
        .edgeEffect(EdgeEffect.Spring)
        .align(Alignment.Top)
        .onDidScroll((xOffset: number, yOffset: number, state: ScrollState) => {
          this.handleScrollDirection(yOffset, state);
        })
      }
      .width('100%')
      .height('100%')

      // 层1.05：空态 —— 未连接后端时给出明确的“去设置连接”入口。
      //
      // 为什么必须有：全新安装时聊天页只有一条空列表 + 输入框，用户看不到
      // 任何连后端的入口（入口在设置页的二级页里，很容易找不到）。
      if (this.messages.length === 0 && !this.loading) {
        Column({ space: 10 }) {
          Image($r('app.media.ic_link'))
            .width(34)
            .height(34)
            .fillColor(this.palette().textMuted)
            .draggable(false)
          Text(this.hasConn ? '开始新的对话' : '尚未连接后端服务')
            .fontSize(15)
            .fontWeight(FontWeight.Medium)
            .fontColor(this.palette().textPrimary)
          Text(this.hasConn
            ? '在下方输入框发送第一条消息'
            : '请先在“后端连接”里填写服务地址与 API Key')
            .fontSize(12)
            .fontColor(this.palette().textMuted)
            .textAlign(TextAlign.Center)
          if (!this.hasConn) {
            Button('去设置连接')
              .height(34)
              .fontSize(13)
              .backgroundColor(this.palette().accent)
              .fontColor(Color.White)
              .margin({ top: 4 })
              .onClick(() => {
                // 跨页信号：切到设置 Tab，并让设置页直接打开连接二级页
                AppStorage.setOrCreate<string>(K_SETTINGS_SUB, SUB_CONNECTIONS);
                AppStorage.setOrCreate<number>(K_REQUESTED_TAB, 3);
              })
          }
        }
        .width('100%')
        .height('100%')
        .padding({ left: 44, right: 44 })
        .justifyContent(FlexAlign.Center)
        .alignItems(HorizontalAlign.Center)
        // 自身不吃触摸（空白处仍可滑列表），但子节点（按钮）正常响应
        .hitTestBehavior(HitTestMode.Transparent)
      }

      // 层1.5：顶栏遮罩（自身撑满并顶部对齐，全链路 hitTest None，触摸完全穿透）
      PageTopBar({ title: '聊天' })

      // 层1.6：底部淡出遮罩 —— 滚动内容接近悬浮输入区/底部导航时逐渐隐入背景，
      // 而不是在玻璃后面清晰可见（PageTopBar 顶部淡出手法的镜像，方向相反）。
      Column()
        .width('100%')
        .height(170)
        .linearGradient({
          direction: GradientDirection.Bottom,
          colors: [
            [this.transparentBottomBg(), 0.0],
            [this.opaqueBottomBg(), 0.6],
            [this.opaqueBottomBg(), 1.0],
          ],
        })
        .hitTestBehavior(HitTestMode.None)
    }
    .width('100%')
    .height('100%')
  }
}
