/**
 * 「外观」二级页面：主题三选一 + 自定义背景图。
 *
 * 从 pages/SettingsPage.ets 抽出。
 * themeMode / bgImage / bgOpacity 用 @Link 与一级页面共享（一级页的
 * 外观行要显示当前主题名，两边必须是同一份数据）。
 */

import { connStore } from '../common/ConnStore';
import { AppSettings, emptySettings } from '../model/Model';
import { applyThemeMode } from '../common/Constants';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, RADIUS_MD,
  ANIM_NORMAL } from '../common/Constants';
import { imageExt } from '../common/SettingsModel';
import { userMessage } from '../common/UserError';
import { SubPageLayer, PlainCard } from './SubPage';
import { MotionBase } from './MotionBase';
import { picker, fileIo } from '@kit.CoreFileKit';
import { common } from '@kit.AbilityKit';

@Component
export struct AppearancePane {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Link themeMode: string;
  @Link bgImage: string;
  @Link bgOpacity: number;
  onBack?: () => void;
  onToast?: (msg: string, isError: boolean) => void;
  @State pickingBg: boolean = false;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  private toast(msg: string, isError: boolean): void {
    const cb: ((m: string, e: boolean) => void) | undefined = this.onToast;
    if (cb !== undefined) {
      cb(msg, isError);
    }
  }

  private applyTheme(mode: string): void {
    this.themeMode = mode;
    this.persistSettings();
    // 立即翻转全局主题标志，整个 UI 随之切换
    applyThemeMode(mode);
  }

  private async pickBackgroundImage(): Promise<void> {
    if (this.pickingBg) {
      return;
    }
    this.pickingBg = true;
    try {
      const options = new picker.PhotoSelectOptions();
      options.MIMEType = picker.PhotoViewMIMETypes.IMAGE_TYPE;
      options.maxSelectNumber = 1;
      const photoPicker = new picker.PhotoViewPicker();
      const result = await photoPicker.select(options);
      if (result.photoUris.length === 0) {
        return;
      }
      const srcUri: string = result.photoUris[0];
      const ctx = getContext(this) as common.UIAbilityContext;
      // 文件名带时间戳：Image 组件按 src 字符串做内存缓存，
      // 每次都写同一个 bg_image 会让第二次换图看起来"没生效"。
      // 后缀必须保留：Image 组件按扩展名挑选解码器，无后缀的沙箱文件会解码失败，
      // 表现就是"设置了背景图但没生效"（onError 里能看到 decode 失败）。
      const destPath: string = ctx.filesDir + '/bg_' + Date.now().toString(36) + imageExt(srcUri);
      const srcFile = fileIo.openSync(srcUri, fileIo.OpenMode.READ_ONLY);
      const destFile = fileIo.openSync(destPath,
        fileIo.OpenMode.READ_WRITE | fileIo.OpenMode.CREATE | fileIo.OpenMode.TRUNC);
      fileIo.copyFileSync(srcFile.fd, destFile.fd);
      fileIo.closeSync(srcFile);
      fileIo.closeSync(destFile);
      this.removeOldBgFile();
      // Image 只认带协议头的沙箱 URI，裸路径会被当成资源名而静默失败
      this.bgImage = 'file://' + destPath;
      this.persistSettings();
      this.toast('背景图已设置', false);
    } catch (e) {
      this.toast(userMessage('settings.pickBg', e), true);
    }
    this.pickingBg = false;
  }

  /** 删除上一张背景图文件，避免沙箱里越攒越多 */
  private removeOldBgFile(): void {
    const old: string = this.bgImage;
    if (old.length === 0) {
      return;
    }
    const path: string = old.startsWith('file://') ? old.substring(7) : old;
    try {
      fileIo.unlinkSync(path);
    } catch (e) {
      // 文件可能已不存在，忽略
    }
  }

  private clearBackgroundImage(): void {
    this.removeOldBgFile();
    this.bgImage = '';
    this.persistSettings();
    this.toast('已清除背景图', false);
  }

  private onBgOpacityChange(value: number): void {
    this.bgOpacity = value / 100;
    this.persistSettings();
  }

  private persistSettings(): void {
    const s: AppSettings = emptySettings();
    const old: AppSettings = connStore.getSettings();
    s.lang = old.lang;
    s.theme = this.themeMode.length > 0 ? this.themeMode : (old.theme.length > 0 ? old.theme : 'system');
    s.currentConnId = old.currentConnId;
    s.bgImage = this.bgImage;
    s.bgOpacity = this.bgOpacity;
    connStore.saveSettings(s);
    AppStorage.set<string>('bgImage', this.bgImage);
    AppStorage.set<number>('bgOpacity', this.bgOpacity);
  }

  @Builder
  ThemeOption(label: string, mode: string) {
    // 按压缩放由 MotionBase 统一；flexWeight: 1 让三枚选项在 Row 里继续等分。
    MotionBase({ pressEnabled: true, flexWeight: 1 }) {
      Column() {
        Text(label)
          .fontSize(12)
          .fontColor(this.themeMode === mode ? Color.White : this.palette().textSecondary)
          // 子节点的颜色迁移要自己声明：父容器的 .animation() 不下传
          .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
      }
      .justifyContent(FlexAlign.Center)
      .width('100%')
      .height(34)
      .borderRadius(RADIUS_MD)
      .backgroundColor(this.themeMode === mode ? this.palette().accent : this.palette().bgHover)
      // 三选一的选中态迁移：底色与描边一起过渡。写在 .border 之后、
      // 覆盖它上面的所有状态驱动属性。
      .border({
        width: 1,
        color: this.themeMode === mode ? this.palette().accent : this.palette().btnGhostBorder,
      })
      .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
      .onClick(() => {
        this.applyTheme(mode);
      })
    }
  }

  build() {
    SubPageLayer({
      title: '外观',
      tab: 3,
      onBack: () => {
        const cb: (() => void) | undefined = this.onBack;
        if (cb !== undefined) {
          cb();
        }
      },
    }) {
      Column() {
        PlainCard({ caption: '主题' }) {
          Row({ space: 8 }) {
            this.ThemeOption('跟随系统', 'system')
            this.ThemeOption('浅色', 'light')
            this.ThemeOption('深色', 'dark')
          }
          .width('100%')

          Text(this.themeMode === 'system'
            ? '当前跟随系统，系统切换深浅色时自动跟随'
            : (this.themeMode === 'dark' ? '当前强制深色主题' : '当前强制浅色主题'))
            .fontSize(11)
            .fontColor(this.palette().textMuted)
            .margin({ top: 10 })
        }

        PlainCard({ caption: '背景图' }) {
          Row() {
            Column() {
              Text('自定义背景图')
                .fontSize(14)
                .fontColor(this.palette().textPrimary)
              Text(this.bgImage.length > 0 ? '已设置背景图' : '未设置背景图')
                .fontSize(11)
                .fontColor(this.palette().textMuted)
                .margin({ top: 2 })
            }
            .alignItems(HorizontalAlign.Start)
            .layoutWeight(1)

            if (this.bgImage.length > 0) {
              Button('更换')
                .height(28)
                .fontSize(12)
                .backgroundColor(Color.Transparent)
                .border({ width: 1, color: this.palette().btnGhostBorder })
                .fontColor(this.palette().textSecondary)
                .margin({ right: 6 })
                .onClick(() => {
                  this.pickBackgroundImage();
                })
              Button('清除')
                .height(28)
                .fontSize(12)
                .backgroundColor(Color.Transparent)
                .border({ width: 1, color: 'rgba(232, 64, 38, 0.45)' })
                .fontColor('#E84026')
                .onClick(() => {
                  this.clearBackgroundImage();
                })
            } else {
              Button(this.pickingBg ? '选择中...' : '选择图片')
                .height(28)
                .fontSize(12)
                .backgroundColor(this.palette().accent)
                .fontColor(Color.White)
                .onClick(() => {
                  this.pickBackgroundImage();
                })
            }
          }
          .width('100%')

          if (this.bgImage.length > 0) {
            Row() {
              Text('透明度')
                .fontSize(11)
                .fontColor(this.palette().textMuted)
              Slider({
                value: Math.round(this.bgOpacity * 100),
                min: 5,
                max: 60,
                step: 1,
              })
                .layoutWeight(1)
                .selectedColor(this.palette().accent)
                .trackColor(this.palette().bgHover)
                .margin({ left: 8, right: 8 })
                .onChange((v: number, mode: SliderChangeMode) => {
                  if (mode === SliderChangeMode.Moving || mode === SliderChangeMode.Click) {
                    this.onBgOpacityChange(v);
                  }
                })
              Text(Math.round(this.bgOpacity * 100).toString() + '%')
                .fontSize(11)
                .fontColor(this.palette().textSecondary)
                .width(32)
            }
            .width('100%')
            .alignItems(VerticalAlign.Center)
            .margin({ top: 12 })
          }
        }
      }
      .width('100%')
      .alignItems(HorizontalAlign.Start)
    }
  }
}
