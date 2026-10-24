/**
 * 右下角轻提示条（设置页与插件页共用）。
 *
 * 两处原本各写一份，只有"描边"这一处不同：设置页无描边，插件页有。
 * 用 bordered 表达这个差异（width 0 的边框不占位、不可见）。
 *
 * 配色由调用方决定：颜色标志必须在调用方落 animateTo 之前先写好，
 * 否则第一帧会用上一条 toast 的配色。
 */

import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, RADIUS_MD, ANIM_NORMAL } from '../common/Constants';

@Component
export struct ToastBar {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop msg: string = '';
  @Prop isError: boolean = false;
  /** 是否带一圈语义色描边（插件页用，设置页不用） */
  @Prop bordered: boolean = false;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  build() {
    if (this.msg.length > 0) {
      Row() {
        Text(this.msg)
          .fontSize(13)
          .fontColor(this.isError ? this.palette().toastErrorText : this.palette().toastText)
          .padding({ left: 20, right: 20, top: 10, bottom: 10 })
          .borderRadius(RADIUS_MD)
          .backgroundColor(this.isError ? this.palette().toastErrorBg : this.palette().toastBg)
          .border({
            width: this.bordered ? 1 : 0,
            color: this.isError ? 'rgba(232, 64, 38, 0.3)' : 'rgba(23, 169, 100, 0.3)',
          })
      }
      .width('100%')
      .justifyContent(FlexAlign.End)
      .padding({ right: 20 })
      .margin({ bottom: 166 })
      .transition(TransitionEffect.OPACITY
        .combine(TransitionEffect.translate({ y: 12 }))
        .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut }))
    }
  }
}
