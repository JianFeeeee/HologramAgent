import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE } from '../common/Constants';
import { hilog } from '@kit.PerformanceAnalysisKit';
import { image } from '@kit.ImageKit';
import { fileIo } from '@kit.CoreFileKit';
import { BusinessError } from '@kit.BasicServicesKit';

/**
 * 背景层：底色 + 三层径向渐变光斑（右上蓝 / 左中青 / 底部深），
 * 对应 WebGUI 的 grad-a / grad-b / grad-c。
 *
 * 独立成文件的原因：Navigation 的 NavDestination 在栈模式下会整屏盖住
 * 一级内容，必须自带同款背景（否则透出下层列表形成重影）。
 * 如果这个组件留在 Index.ets 里，二级页面 import 它会与
 * Index -> SettingsPage -> Index 形成循环依赖。
 */
@Component
export struct GradientBackground {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @StorageProp('bgImage') @Watch('onBgChanged') private bgImage: string = '';
  @StorageProp('bgOpacity') private bgOpacity: number = 0.25;
  /**
   * 背景图解码后的位图。
   *
   * 为什么不把路径字符串直接交给 Image：
   * 把沙箱路径交给 Image 要同时满足"带 file:// 协议头"和"带可识别的图片后缀"
   * 两个隐含前提，任何一条不满足都只是静默不显示 —— 前两轮"背景不生效"就是
   * 卡在这里，而且没有任何可观测的失败点。
   * 这里改成自己用 ImageSource 解码（与聊天图片附件同一条已验证通路），
   * 成功与失败都能落 hilog，Image 只负责画一张现成的 PixelMap。
   */
  @State private bgPixel: image.PixelMap | undefined = undefined;

  aboutToAppear(): void {
    this.decodeBg();
  }

  private onBgChanged(): void {
    this.decodeBg();
  }

  /** 解码沙箱/网络背景图为 PixelMap；失败时清空，只留渐变底。 */
  private async decodeBg(): Promise<void> {
    const src: string = this.bgImage;
    if (src.length === 0) {
      this.bgPixel = undefined;
      return;
    }
    // 只处理本地沙箱文件；http(s) 背景图不是本应用的场景（设置页只给图库选择）
    const path: string = src.startsWith('file://') ? src.substring(7) : src;
    if (!path.startsWith('/')) {
      hilog.error(0x0000, 'HologramAgent', 'bg: unsupported source');
      this.bgPixel = undefined;
      return;
    }
    try {
      const f = fileIo.openSync(path, fileIo.OpenMode.READ_ONLY);
      const srcObj: image.ImageSource = image.createImageSource(f.fd);
      const pm: image.PixelMap = await srcObj.createPixelMap();
      await srcObj.release();
      fileIo.closeSync(f);
      this.bgPixel = pm;
      hilog.info(0x0000, 'HologramAgent', 'bg decoded ok');
    } catch (e) {
      // 失败必须留痕：否则表现就是"设置了但没生效"，无从定位
      const err = e as BusinessError;
      hilog.error(0x0000, 'HologramAgent', 'bg decode failed code=%{public}d', err.code as number);
      this.bgPixel = undefined;
    }
  }

  build() {
    Stack() {
      // 底色
      Column()
        .width('100%')
        .height('100%')
        .backgroundColor(this.palette().bgPrimary)

      // grad-a: 右上角蓝色光斑
      Column()
        .width('100%')
        .height('100%')
        .radialGradient({
          center: ['88%', '-4%'],
          radius: 520,
          colors: [[this.palette().gradA, 0.0], [this.palette().gradAEnd, 1.0]],
        })

      // grad-b: 左中部霜冻青光斑
      Column()
        .width('100%')
        .height('100%')
        .radialGradient({
          center: ['-6%', '38%'],
          radius: 470,
          colors: [[this.palette().gradB, 0.0], [this.palette().gradBEnd, 1.0]],
        })

      // grad-c: 底部深色光斑
      Column()
        .width('100%')
        .height('100%')
        .radialGradient({
          center: ['50%', '110%'],
          radius: 560,
          colors: [[this.palette().gradC, 0.0], [this.palette().gradCEnd, 1.0]],
        })

      // 自定义背景图（可选）：叠在渐变光斑之上、页面内容之下
      if (this.bgPixel !== undefined) {
        Image(this.bgPixel)
          .width('100%')
          .height('100%')
          .objectFit(ImageFit.Cover)
          .opacity(this.bgOpacity)
          .draggable(false)
      }
    }
    .width('100%')
    .height('100%')
  }

  private palette(): ThemePalette {
    // 引用 this.isDark 建立响应式依赖：主题切换时整个组件树重渲染
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }
}
