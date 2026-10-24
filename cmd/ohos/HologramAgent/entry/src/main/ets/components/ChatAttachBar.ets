/**
 * 输入区上方的两个悬浮条：加号菜单（图片/文件）与待发送附件预览。
 *
 * 从 components/ChatComposer.ets 拆出 —— 两者都是"输入区上方的独立浮层"，
 * 数据与动作全部由输入区传入。
 */

import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, RADIUS_MD, RADIUS_PILL,
  ANIM_NORMAL } from '../common/Constants';
import { MotionBase } from './MotionBase';
import { formatBytes } from '../common/AttachmentMeta';

/** 加号菜单：两枚独立的玻璃胶囊，和输入区其他组件同一套视觉语言 */
@Component
export struct ChatAttachMenu {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  onPickImage?: () => void;
  onPickFile?: () => void;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  @Builder
  AttachOption(icon: Resource, label: string, tap: () => void) {
    // 按压反馈交给 MotionBase：每个实例自带独立按压态，
    // 修掉了原先两枚胶囊共用一个 @State、按一枚两枚同时缩放的问题。
    MotionBase({ pressEnabled: true, fillWidth: false }) {
      Row({ space: 6 }) {
        Image(icon)
          .width(15)
          .height(15)
          .fillColor(this.palette().textSecondary)
          .draggable(false)
        Text(label)
          .fontSize(12)
          .fontColor(this.palette().textPrimary)
      }
      .padding({ left: 12, right: 14, top: 8, bottom: 8 })
      .backgroundColor(this.palette().navBarBg)
      .borderRadius(RADIUS_PILL)
      .border({ width: 1, color: this.palette().navBarBorder })
      .shadow({ radius: 20, color: this.palette().shadow, offsetY: 6 })
      .onClick(tap)
    }
  }

  build() {
    // 外层撑满并左对齐：菜单要出现在加号正上方，而不是跟着悬浮区右对齐
    Row() {
      Row({ space: 8 }) {
        this.AttachOption($r('app.media.ic_image'), '图片', () => {
          const cb: (() => void) | undefined = this.onPickImage;
          if (cb !== undefined) {
            cb();
          }
        })
        this.AttachOption($r('app.media.ic_file'), '文件', () => {
          const cb: (() => void) | undefined = this.onPickFile;
          if (cb !== undefined) {
            cb();
          }
        })
      }
    }
    .width('100%')
    .justifyContent(FlexAlign.Start)
    .margin({ bottom: 8 })
    .hitTestBehavior(HitTestMode.Transparent)
    // 加号菜单由 if 控制，进出场只能靠 transition；配合 toggle 处的
    // animateTo，展开时两枚胶囊从加号上方浮起而不是硬闪出来。
    .transition(TransitionEffect.OPACITY.combine(TransitionEffect.translate({ y: 12 })).animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut }))
  }
}

/** 待发送附件预览条：缩略信息 + 一个移除按钮 */
@Component
export struct ChatPendingChip {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop name: string = '';
  @Prop byteSize: number = 0;
  @Prop isImage: boolean = false;
  @Prop uploading: boolean = false;
  onRemove?: () => void;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  build() {
    // 按压反馈交给 MotionBase（全宽预览条）
    MotionBase({ pressEnabled: true }) {
      Row({ space: 8 }) {
        Image(this.isImage ? $r('app.media.ic_image') : $r('app.media.ic_file'))
          .width(15)
          .height(15)
          .fillColor(this.palette().accent)
          .draggable(false)
        Column({ space: 1 }) {
          Text(this.name)
            .fontSize(12)
            .fontColor(this.palette().textPrimary)
            .maxLines(1)
            .textOverflow({ overflow: TextOverflow.Ellipsis })
          Text(this.uploading ? '上传中...' : formatBytes(this.byteSize))
            .fontSize(10)
            .fontColor(this.palette().textMuted)
        }
        .alignItems(HorizontalAlign.Start)
        .layoutWeight(1)
        if (this.uploading) {
          LoadingProgress()
            .width(14)
            .height(14)
            .color(this.palette().accent)
        } else {
          Image($r('app.media.ic_close'))
            .width(13)
            .height(13)
            .fillColor(this.palette().textMuted)
            .draggable(false)
            .onClick(() => {
              const cb: (() => void) | undefined = this.onRemove;
              if (cb !== undefined) {
                cb();
              }
            })
        }
      }
      .width('100%')
      .padding({ left: 12, right: 12, top: 8, bottom: 8 })
      .margin({ bottom: 8 })
      .backgroundColor(this.palette().navBarBg)
      .borderRadius(RADIUS_MD)
      .border({ width: 1, color: this.palette().navBarBorder })
      .shadow({ radius: 20, color: this.palette().shadow, offsetY: 6 })
    }
  }
}
