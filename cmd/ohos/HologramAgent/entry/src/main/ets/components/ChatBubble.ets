/**
 * 单条聊天气泡（含头像、渠道名、思考卡、工具卡、附件卡、正文）。
 *
 * 从 pages/ChatPage.ets 抽出（原来是 Avatar / ChanAvatar / BubbleSlot /
 * MessageBubble / BubbleBody 五个 @Builder）。
 *
 * 传参约定：一切都在构造时快照进来。ForEach 的键（structSig）只在
 * 消息"结构"变化时改变（新增思考/工具/附件/来源、定稿），结构一变
 * 气泡就整条重建，因此结构类字段不需要二次更新；唯一会在键不变时
 * 持续变化的是正文 content，所以它单独用基本类型 @Prop 传（与
 * MarkdownView 的 @Prop content 走同一条响应式链路，流式渲染不变）。
 */

import { ChatMessage, ToolCallInfo, ChatAttachment } from '../model/Model';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, RADIUS_MD, RADIUS_PILL,
  ANIM_NORMAL } from '../common/Constants';
import { bubbleMaxWidth, chanColor, chanLabel, chanLetter } from '../common/ChatFormat';
import { AttachmentCard } from './Attachment';
import { MarkdownView } from './MarkdownView';
import { ChatReasoningCard, ChatToolCard } from './ChatToolCard';

@Component
export struct ChatBubble {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop msgId: number = 0;
  @Prop role: string = '';
  /** 正文：唯一会在 ForEach 键不变时持续变化的字段 */
  @Prop content: string = '';
  @Prop isStreaming: boolean = false;
  @Prop isFinal: boolean = false;
  @Prop reasoningContent: string = '';
  @Prop reasoningOpen: boolean = false;
  @Prop toolCalls: ToolCallInfo[] = [];
  @Prop attachment: ChatAttachment | undefined = undefined;
  @Prop source: string = '';
  /** 是否"别处来的"消息（渠道/设备） */
  @Prop channel: boolean = false;
  /** 是否自己发的（右对齐、"我"头像） */
  @Prop mine: boolean = false;
  /** 入场动画阶段 */
  @Prop fresh: boolean = false;
  onOpenAttachment?: (att: ChatAttachment) => void;
  onToggleReasoning?: (msgId: number) => void;
  onToggleTool?: (msgId: number, index: number) => void;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  /** 思考卡/工具卡是否是"面板"：气泡宽度要放宽，见 common/ChatFormat */
  private maxWidth(): string {
    const msg: ChatMessage = {
      id: this.msgId,
      role: this.role,
      content: this.content,
      toolCalls: this.toolCalls,
    };
    if (this.reasoningContent.length > 0) {
      msg.reasoningContent = this.reasoningContent;
    }
    return bubbleMaxWidth(msg);
  }

  @Builder
  Avatar() {
    Text(this.role === 'user' ? '我' : 'AI')
      .fontSize(11)
      .fontWeight(FontWeight.Bold)
      .fontColor(this.role === 'user' ? this.palette().msgUserText : this.palette().accent)
      .textAlign(TextAlign.Center)
      .width(28)
      .height(28)
      .borderRadius(RADIUS_PILL)
      .backgroundColor(this.role === 'user' ? this.palette().msgUserBg : this.palette().accentBg)
      .margin({ top: 2 })
  }

  @Builder
  ChanAvatar() {
    Text(chanLetter(this.source))
      .fontSize(12)
      .fontWeight(FontWeight.Bold)
      .fontColor(Color.White)
      .textAlign(TextAlign.Center)
      .width(28)
      .height(28)
      .borderRadius(RADIUS_PILL)
      .backgroundColor(chanColor(this.source))
      .margin({ top: 2 })
  }

  /**
   * 气泡占位槽 —— 分栏右侧被切掉的根因就在这里。
   *
   * 原来 BubbleBody 直接放进 Row，它的 constraintSize maxWidth 是百分比
   * （78% / 92%）。百分比是相对【父节点外框】解析的，而这个 Row 自带
   * 左右 8 的 padding、外层列表 Column 又有左右 14 的 padding，
   * 于是 92% 算出来的宽度里包含了这些 padding，再加上 28 的头像和 8 的
   * 间距，一行的总宽就超过了可用内容宽。窄屏因为整体够宽看不出来，
   * 分栏后左栏只有 420vp，溢出的十几 vp 直接被栏宽裁掉 —— 表现为
   * 消息右侧被切了一条（这与 MarkdownView 里 width('100%') 溢出 12vp
   * 被 clip 的问题是同一个成因）。
   *
   * 修法同 MarkdownView：用 layoutWeight(1) 拿"剩余空间"而不是百分比。
   * 槽自身无 padding，外框宽 == 内容宽 == 头像与间距之外的真实可用宽度，
   * 气泡的百分比再相对它解析，无论栏宽多少都不可能溢出。
   */
  @Builder
  BubbleSlot() {
    Column() {
      this.BubbleBody()
    }
    .layoutWeight(1)
    .alignItems(this.mine ? HorizontalAlign.End : HorizontalAlign.Start)
  }

  @Builder
  BubbleBody() {
    Column() {
      // 渠道来源名（对齐 GUI 的 msg-chan-name）：只有别处来的消息才显示
      if (this.channel) {
        Text(chanLabel(this.source))
          .fontSize(10)
          .fontWeight(FontWeight.Medium)
          .fontColor(this.palette().textMuted)
          .margin({ bottom: 4 })
      }

      // Reasoning card (assistant only)
      if (this.role === 'assistant' && this.reasoningContent.length > 0) {
        ChatReasoningCard({
          content: this.reasoningContent,
          open: this.reasoningOpen,
          sweeping: this.isFinal !== true && this.isStreaming === true,
          fresh: this.fresh,
          onToggle: () => {
            const cb: ((id: number) => void) | undefined = this.onToggleReasoning;
            if (cb !== undefined) {
              cb(this.msgId);
            }
          },
        })
      }

      // 附件卡（图片缩略图 / 文件条），点击进入附件详情二级页
      if (this.attachment !== undefined) {
        AttachmentCard({
          att: this.attachment,
          mine: this.role === 'user',
          onTap: () => {
            const a: ChatAttachment | undefined = this.attachment;
            const cb: ((att: ChatAttachment) => void) | undefined = this.onOpenAttachment;
            if (a !== undefined && cb !== undefined) {
              cb(a);
            }
          },
        })
      }

      // Content bubble — 对齐 WebGUI bubbleGrow + textFadeIn
      if (this.content.length > 0) {
        if (this.role === 'assistant') {
          MarkdownView({
            content: this.content,
            isStreaming: this.isStreaming === true,
            isDark: this.isDark,
          })
        } else {
          Text(this.content)
            .fontSize(15)
            .lineHeight(24)
            .fontColor(this.palette().msgBubbleText)
            .textAlign(TextAlign.Start)
            .wordBreak(WordBreak.BREAK_ALL)
            .constraintSize({ maxWidth: '100%' })
            .margin({ top: this.attachment !== undefined ? 8 : 0 })
        }
      }

      // Tool cards
      if (this.toolCalls.length > 0) {
        Column() {
          ForEach(this.toolCalls, (tc: ToolCallInfo, tci: number) => {
            ChatToolCard({
              tc: tc,
              index: tci,
              onToggle: (index: number) => {
                const cb: ((id: number, i: number) => void) | undefined = this.onToggleTool;
                if (cb !== undefined) {
                  cb(this.msgId, index);
                }
              },
            })
          }, (tc: ToolCallInfo, tci: number) => tci.toString() + tc.name)
        }
        // 不写 width('100%')：百分比会按气泡外框解析而溢出 12vp 被 clip。
        // 让它自适应，最大宽约束由气泡内容框向下传递，ChatToolCard 内部用 layoutWeight 取满。
        .alignItems(HorizontalAlign.Start)
        .margin({ top: this.content.length > 0 ? 6 : 0 })
      }
    }
    .constraintSize({ maxWidth: this.maxWidth() })
    .clip(true)
    .padding({ left: 12, right: 12, top: 9, bottom: 9 })
    .backgroundColor(this.role === 'user' ? this.palette().msgUserBubbleBg : this.palette().msgAssistantBubbleBg)
    .borderRadius({
      topLeft: RADIUS_MD,
      topRight: RADIUS_MD,
      bottomLeft: this.role === 'assistant' ? 4 : RADIUS_MD,
      bottomRight: this.role === 'user' ? 4 : RADIUS_MD,
    })
    .border({ width: 1, color: this.role === 'user' ? this.palette().msgUserBubbleBorder : this.palette().msgAssistantBubbleBorder })
    .shadow({ radius: 8, color: this.palette().shadow, offsetY: 2 })
    .alignItems(HorizontalAlign.Start)
    // bubbleGrow: 气泡入场缩放效果
    .scale({
      x: this.fresh ? 0.95 : 1,
      y: this.fresh ? 0.95 : 1,
    })
    .animation({ duration: 200, curve: Curve.EaseOut })
  }

  build() {
    Row({ space: 8 }) {
      if (this.channel) {
        this.ChanAvatar()
        this.BubbleSlot()
      } else if (this.role === 'user') {
        this.BubbleSlot()
        this.Avatar()
      } else {
        this.Avatar()
        this.BubbleSlot()
      }
    }
    .width('100%')
    .alignItems(VerticalAlign.Top)
    // 槽已经用 layoutWeight 吃掉了剩余宽度，这里的对齐实际不再参与分配，
    // 保留是为了兜底：若某处布局退化成非加权分配，方向也仍然正确。
    .justifyContent(this.mine ? FlexAlign.End : FlexAlign.Start)
    .padding({ left: 8, right: 8, top: 3, bottom: 3 })
    // 入场动画：对齐 WebGUI viewIn (opacity 0 -> 1, translateY 6 -> 0)
    .opacity(this.fresh ? 0 : 1)
    .translate({ y: this.fresh ? 8 : 0 })
    .animation({ duration: 180, curve: Curve.EaseOut })
  }
}
