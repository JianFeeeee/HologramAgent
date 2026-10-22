/**
 * 气泡内的两张"面板"卡：思考过程卡 + 工具调用卡。
 *
 * 从 pages/ChatPage.ets 抽出（原来分别是 ReasoningCard / ToolCard 两个 @Builder）。
 * 折叠状态与开关动作都交回调用方（状态在 chatStore 里，且展开/收起要在
 * animateTo 帧内完成 —— 那需要组件上下文）。
 */

import { ToolCallInfo } from '../model/Model';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, RADIUS_SM,
  ANIM_NORMAL } from '../common/Constants';
import { tcRunning, tcError, tcLeftColor, tcIcoColor, tcStateLabel, tcStateColor } from '../common/ChatFormat';

@Component
export struct ChatReasoningCard {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  /** 思考正文（已折叠时也带着，展开不再请求） */
  @Prop content: string = '';
  @Prop open: boolean = false;
  /** 流式中：显示转圈 + 扫光条 */
  @Prop sweeping: boolean = false;
  /** 入场动画阶段：扫光条起始偏移靠它切换 */
  @Prop fresh: boolean = false;
  onToggle?: () => void;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  build() {
    // 两层结构，原因见 ChatBubble 的布局说明：
    // 外层 Row 是"外观壳"（虚线边框 / 底色 / 圆角），不设百分比宽度，
    // 靠内层 layoutWeight(1) 把气泡内容框的剩余宽度吃满；
    // 内层 holder Column 自身无 padding，所以它的子节点写 width('100%')
    // 才有正确的解析基准，不会再溢出到气泡外被 clip 切掉。
    Row() {
      Column() {
        Row({ space: 7 }) {
          Image($r('app.media.ic_sparkle'))
            .width(12)
            .height(12)
            .fillColor(this.palette().accent)
          Text('思考过程')
            .fontSize(11)
            .fontWeight(FontWeight.Medium)
            .fontColor(this.palette().textPrimary)
          // 流式思考时给出明确进度指示，而不是一张看不出在动的折叠卡
          if (this.sweeping) {
            LoadingProgress()
              .width(11)
              .height(11)
              .color(this.palette().accent)
          }
          Blank()
          Text(this.content.length > 0 ? this.content.length.toString() + ' 字' : '')
            .fontSize(9.5)
            .fontColor(this.palette().textMuted)
          Image($r('app.media.ic_chevron_down'))
            .width(14)
            .height(14)
            .fillColor(this.palette().textMuted)
            .rotate({ angle: this.open ? 180 : 0 })
            .animation({ duration: 200, curve: Curve.EaseOut })
        }
        .width('100%')
        .padding({ left: 10, right: 10, top: 6, bottom: 6 })
        .onClick(() => {
          const cb: (() => void) | undefined = this.onToggle;
          if (cb !== undefined) {
            cb();
          }
        })

        if (this.open) {
          Text(this.content)
            .fontSize(11.5)
            .lineHeight(17)
            .fontColor(this.palette().textTertiary)
            .width('100%')
            .padding({ left: 10, right: 10, bottom: 8 })
            .maxLines(24)
            .textOverflow({ overflow: TextOverflow.Ellipsis })
            .wordBreak(WordBreak.BREAK_ALL)
            // 面板内容靠 if 挂载：用 transition 在展开/收起时淡入淡出
            .transition(TransitionEffect.OPACITY.combine(TransitionEffect.translate({ y: -6 })).animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut }))
        }

        // 流式思考中：扫光动画条（对齐 WebGUI reasoningSweep）
        if (this.sweeping) {
          Stack() {
            Row()
              .height(2)
              .borderRadius(2)
              .width('200%')
              .linearGradient({
                angle: 90,
                colors: [
                  ['rgba(255,255,255,0.01)', 0],
                  [this.palette().accent, 0.35],
                  ['rgba(255,255,255,0.01)', 0.5],
                  [this.palette().accent, 0.65],
                  ['rgba(255,255,255,0.01)', 1],
                ],
              })
              .opacity(0.7)
              .translate({ x: this.fresh ? '0%' : '-50%' })
              .animation({ duration: 1200, curve: Curve.Linear })
          }
          .width('100%')
          .clip(true)
          .height(2)
          .margin({ top: 6 })
        }
      }
      .layoutWeight(1)
      .alignItems(HorizontalAlign.Start)
    }
    .alignItems(VerticalAlign.Top)
    .margin({ bottom: 6 })
    .borderRadius(RADIUS_SM)
    .backgroundColor(this.palette().bgHover)
    .border({ width: 1, color: this.palette().kvBorder, style: BorderStyle.Dashed })
  }
}

@Component
export struct ChatToolCard {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop tc: ToolCallInfo;
  /** 在所属消息 toolCalls 里的下标：开关动作要交回调用方按 (msgId, index) 定位 */
  @Prop index: number = 0;
  onToggle?: (index: number) => void;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  build() {
    // 同 ChatReasoningCard：外层 Row 只做外观，内层 layoutWeight(1) 取真实内容宽
    Row() {
      Column() {
        Row({ space: 6 }) {
          if (tcError(this.tc)) {
            Image($r('app.media.ic_error'))
              .width(13).height(13)
              .fillColor(tcIcoColor(this.tc, this.palette().accent))
          } else if (tcRunning(this.tc)) {
            LoadingProgress()
              .width(12).height(12)
              .color(tcIcoColor(this.tc, this.palette().accent))
          } else {
            Image($r('app.media.ic_check'))
              .width(13).height(13)
              .fillColor(tcIcoColor(this.tc, this.palette().accent))
          }
          Text(this.tc.name)
            .fontSize(11)
            .fontWeight(FontWeight.Medium)
            .fontColor(this.palette().textPrimary)
            .maxLines(1)
            .textOverflow({ overflow: TextOverflow.Ellipsis })
            // 名字可长可短，必须让它占剩余宽度并可收缩，
            // 否则右侧状态文字会被挤出气泡（78% 宽度 + clip 直接切掉）
            .layoutWeight(1)
          if (this.tc.plugin !== undefined && this.tc.plugin.length > 0) {
            Text(this.tc.plugin)
              .fontSize(9.5)
              .fontColor(this.palette().textMuted)
              .maxLines(1)
              .flexShrink(0)
          }
          // 状态徽标去掉，只留一行小字（用户要求"去掉所有状态徽标"）
          Text(tcStateLabel(this.tc))
            .fontSize(9.5)
            .fontWeight(FontWeight.Medium)
            .fontColor(tcStateColor(this.tc))
            .flexShrink(0)
          Image($r('app.media.ic_chevron_down'))
            .width(13).height(13)
            .fillColor(this.palette().textMuted)
            .flexShrink(0)
            .rotate({ angle: this.tc.open === true ? 180 : 0 })
            .animation({ duration: 150, curve: Curve.EaseOut })
        }
        .width('100%')
        .alignItems(VerticalAlign.Center)
        .onClick(() => {
          const cb: ((index: number) => void) | undefined = this.onToggle;
          if (cb !== undefined) {
            cb(this.index);
          }
        })

        if (this.tc.open === true) {
          Column() {
            if (this.tc.args.length > 0 && this.tc.args !== '{}') {
              Text('参数')
                .fontSize(9.5).fontWeight(FontWeight.Medium)
                .fontColor(this.palette().textMuted)
                .margin({ top: 6, bottom: 2 })
              Text(this.tc.args)
                .fontSize(11)
                .fontColor(this.palette().preText)
                .backgroundColor(this.palette().preBg)
                .borderRadius(4)
                .padding({ left: 7, right: 7, top: 5, bottom: 5 })
                .width('100%')
                .textAlign(TextAlign.Start)
                .wordBreak(WordBreak.BREAK_ALL)
            }
            if (this.tc.result !== undefined && this.tc.result.length > 0) {
              Text('结果')
                .fontSize(9.5).fontWeight(FontWeight.Medium)
                .fontColor(this.palette().textMuted)
                .margin({ top: 6, bottom: 2 })
              Text(this.tc.result)
                .fontSize(11)
                .fontColor(this.palette().preText)
                .backgroundColor(this.palette().preBg)
                .borderRadius(4)
                .padding({ left: 7, right: 7, top: 5, bottom: 5 })
                .width('100%')
                .textAlign(TextAlign.Start)
                .wordBreak(WordBreak.BREAK_ALL)
                .maxLines(8)
                .textOverflow({ overflow: TextOverflow.Ellipsis })
            }
          }
          .width('100%')
          .alignItems(HorizontalAlign.Start)
          // 展开内容整体用 if 挂载：transition 让参数/结果随 chevron 一起淡入
          .transition(TransitionEffect.OPACITY.combine(TransitionEffect.translate({ y: -6 })).animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut }))
        }
      }
      .layoutWeight(1)
      .alignItems(HorizontalAlign.Start)
    }
    .alignItems(VerticalAlign.Top)
    .padding({ left: 10, right: 10, top: 7, bottom: 7 })
    .margin({ bottom: 4 })
    .borderRadius(RADIUS_SM)
    .backgroundColor(this.palette().bgHover)
    .border({
      width: { left: 3, top: 1, right: 1, bottom: 1 },
      color: {
        left: tcLeftColor(this.tc, this.palette().accent),
        top: this.palette().kvBorder,
        right: this.palette().kvBorder,
        bottom: this.palette().kvBorder,
      },
    })
  }
}
