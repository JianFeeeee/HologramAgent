/**
 * 安装表单悬浮卡。
 *
 * 从 pages/PluginsPage.ets 抽出（浮在内容之上的独立图层，与列表/详情无关）。
 * 轻提示条与设置页共用，见 components/ToastBar.ets。
 */

import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, RADIUS_MD, RADIUS_SM,
  ANIM_ENTER } from '../common/Constants';

/** 安装表单：悬浮在安装按钮上方的一张玻璃卡（点悬浮按钮开合） */
@Component
export struct PluginInstallForm {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop url: string = '';
  onUrlChange?: (v: string) => void;
  onInstall?: () => void;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  build() {
    Row() {
      TextInput({ placeholder: '.hmap 包下载 URL', text: this.url })
        .layoutWeight(1)
        .height(36)
        .fontSize(14)
        .fontColor(this.palette().textPrimary)
        .placeholderColor(this.palette().textMuted)
        .backgroundColor(this.palette().bgInput)
        .borderRadius(RADIUS_SM)
        .border({ width: 1, color: this.palette().border })
        .onChange((v: string) => {
          const cb: ((v: string) => void) | undefined = this.onUrlChange;
          if (cb !== undefined) {
            cb(v);
          }
        })
      Button('安装')
        .height(36)
        .fontSize(12)
        .backgroundColor(this.palette().accent)
        .fontColor('#FFFFFF')
        .margin({ left: 6 })
        .onClick(() => {
          const cb: (() => void) | undefined = this.onInstall;
          if (cb !== undefined) {
            cb();
          }
        })
    }
    .width('100%')
    .padding(10)
    .margin({ bottom: 10 })
    .backgroundColor(this.palette().navBarBg)
    .borderRadius(RADIUS_MD)
    .border({ width: 1, color: this.palette().navBarBorder })
    .shadow({ radius: 20, color: this.palette().shadow, offsetY: 6 })
    .alignItems(VerticalAlign.Center)
    .transition(TransitionEffect.OPACITY
      .combine(TransitionEffect.translate({ y: 12 }))
      .animation({ duration: ANIM_ENTER, curve: Curve.EaseOut }))
  }
}
