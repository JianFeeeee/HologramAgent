import { connStore } from '../common/ConnStore';
import { registerNavStack, unregisterNavStack } from '../common/NavStackRegistry';
import { ChatAttachment } from '../model/Model';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, WIDE_NAV_BAR_WIDTH, WIDE_MIN_CONTENT } from '../common/Constants';
import { ChatStream } from '../components/ChatStream';
import { ChatComposer } from '../components/ChatComposer';
import { SubPageLayer, markSubPageOpen, subPageParam } from '../components/SubPage';
import { AttachmentDetailContent } from '../components/Attachment';
import { chatStore } from '../common/ChatStore';

/** 二级页面标识：附件（图片/文件）详情 */
const SUB_NONE: string = '';
const SUB_ATTACHMENT: string = 'attachment';

/**
 * 聊天页：只做"页面壳"。
 *
 * 拆分后的分工（拆分前这里是 1962 行的单文件）：
 *  - 消息流状态机（SSE / 历史 / 新消息动画）→ common/ChatStore.ets
 *  - SSE 事件翻译                          → common/ChatSse.ets
 *  - 发送与中断                            → common/ChatSession.ets
 *  - 消息列表 + 滚动                       → components/ChatStream.ets
 *  - 气泡（含思考卡/工具卡）                → components/ChatBubble.ets
 *  - 悬浮输入区 + 附件选择/上传             → components/ChatComposer.ets
 * 本文件保留：导航栈与附件详情二级页、以及"列表底部留白"这个
 * 输入区与列表之间的耦合点（所以三个相关标志位用 @Link 与输入区共享）。
 */
@Component
export struct ChatPage {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  /** 宽屏：左边聊天流（含底部导航栏），右边附件详情 */
  @StorageProp('isWideScreen') private isWide: boolean = false;
  /** 当前在右栏/二级页展示的附件；未打开时为 undefined */
  @State activeAtt: ChatAttachment | undefined = undefined;
  @State activeSub: string = SUB_NONE;
  /** 以下三项影响列表底部留白，与输入区共享（输入区负责改） */
  @State inputMultiLine: boolean = false;
  @State attachMenuOpen: boolean = false;
  @State pendingName: string = '';
  private navStack: NavPathStack = new NavPathStack();

  aboutToAppear(): void {
    // 把本页导航栈登记给 Index：返回手势/三键返回按当前 Tab 精确派发到这里
    registerNavStack(0, this.navStack, () => {
      this.activeSub = SUB_NONE;
      this.activeAtt = undefined;
    });
    chatStore.init();
    const cur = connStore.getCurrentConnection();
    if (cur !== null) {
      chatStore.loadHistory();
      chatStore.connect();
    }
  }

  aboutToDisappear(): void {
    unregisterNavStack(0);
    chatStore.disconnect();
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  /**
   * 消息列表底部留白：悬浮输入区（导航栏上方 94 + 输入行 44 ≈ 138）
   * 之上再留 ~72 的安全间距，保证最后一条气泡的思考卡/工具卡/附件
   * 不被遮挡；加号菜单与待发送附件预览展开时再加高。
   */
  private listBottomPad(): number {
    let pad: number = 210;
    if (this.inputMultiLine) {
      pad += 60;
    }
    if (this.attachMenuOpen) {
      pad += 70;
    }
    if (this.pendingName.length > 0) {
      pad += 46;
    }
    return pad;
  }

  // ===================== 二级页面：附件详情 =====================

  /**
   * 打开附件详情。
   * WebGUI 点图片是新开浏览器标签、点文件是直接下载；
   * 移动端没有"标签页"，改成一个二级页面：大图预览 + 元信息 + 保存。
   */
  private openAttachment(att: ChatAttachment): void {
    this.activeAtt = att;
    this.activeSub = SUB_ATTACHMENT;
    if (this.isWide && this.navStack.size() > 0) {
      this.navStack.replacePathByName(SUB_ATTACHMENT, subPageParam(SUB_ATTACHMENT), false);
    } else {
      this.navStack.pushPathByName(SUB_ATTACHMENT, subPageParam(SUB_ATTACHMENT), true);
    }
  }

  private closeAttachment(): void {
    this.navStack.pop();
    this.activeSub = SUB_NONE;
    this.activeAtt = undefined;
  }

  build() {
    // 宽屏：左栏聊天流（含底部导航与输入区），右栏附件详情。
    // 窄屏：附件详情整屏覆盖，系统返回手势直接作用于 navStack。
    Navigation(this.navStack) {
      this.ChatBody()
    }
    .navDestination(this.SubDestination)
    .mode(this.isWide ? NavigationMode.Split : NavigationMode.Stack)
    .navBarPosition(NavBarPosition.Start)
    .navBarWidth(WIDE_NAV_BAR_WIDTH)
    .minContentWidth(WIDE_MIN_CONTENT)
    .hideTitleBar(true)
    .hideToolBar(true)
    .hideBackButton(true)
    .width('100%')
    .height('100%')
    .onNavBarStateChange((isVisible: boolean) => {
      markSubPageOpen(this.isWide || !isVisible);
    })
    .onNavigationModeChange((mode: NavigationMode) => {
      if (mode === NavigationMode.Split) {
        markSubPageOpen(true);
        // 右栏不能空白：有附件就展示最近一个，没有则由 SubDestination 显示空态
        if (this.navStack.size() === 0) {
          const a: ChatAttachment | undefined = chatStore.latestAttachment();
          this.activeAtt = a;
          this.activeSub = SUB_ATTACHMENT;
          this.navStack.pushPathByName(SUB_ATTACHMENT, subPageParam(SUB_ATTACHMENT), false);
        }
      } else {
        this.navStack.clear(false);
        this.activeSub = SUB_NONE;
        markSubPageOpen(false);
      }
    })
  }

  /** 附件详情二级页面 */
  @Builder
  SubDestination(name: string, param: object) {
    NavDestination() {
      if (name === SUB_ATTACHMENT) {
        SubPageLayer({
          title: '附件详情',
          tab: 0,
          onBack: () => {
            this.closeAttachment();
          },
        }) {
          this.AttachmentPane()
        }
      }
    }
    .hideTitleBar(true)
    .backgroundColor(Color.Transparent)
  }

  @Builder
  AttachmentPane() {
    if (this.activeAtt !== undefined) {
      AttachmentDetailContent({ att: this.activeAtt })
    } else {
      // 宽屏右栏空态：聊天里还没有任何图片/文件
      Column({ space: 8 }) {
        Image($r('app.media.ic_file'))
          .width(38)
          .height(38)
          .fillColor(this.palette().textMuted)
          .draggable(false)
        Text('选择聊天中的图片或文件查看详情')
          .fontSize(13)
          .fontColor(this.palette().textMuted)
      }
      .width('100%')
      .padding({ top: 60 })
      .alignItems(HorizontalAlign.Center)
    }
  }

  @Builder
  ChatBody() {
    Stack({ alignContent: Alignment.Bottom }) {
      // 层1~1.6：消息流 + 顶栏遮罩 + 底部淡出遮罩
      ChatStream({
        bottomPad: this.listBottomPad(),
        onOpenAttachment: (att: ChatAttachment) => {
          this.openAttachment(att);
        },
      })

      // 层2：悬浮输入区（导航栏上方，与导航栏左右平齐，各组件独立不共框）
      ChatComposer({
        inputMultiLine: $inputMultiLine,
        attachMenuOpen: $attachMenuOpen,
        pendingName: $pendingName,
      })
    }
    .width('100%')
    .height('100%')
  }
}
