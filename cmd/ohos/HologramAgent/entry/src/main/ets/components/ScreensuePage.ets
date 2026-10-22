import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, ANIM_NORMAL } from '../common/Constants';
import { GradientBackground } from './GradientBackground';
import { PageTopBar } from './PageTopBar';
import { MotionBase } from './MotionBase';
import { looksLikeHtml, screensueWebData } from '../common/ScreensueHtml';
import { webview } from '@kit.ArkWeb';

/**
 * agent 主动推送的前台内容页。
 *
 * 调用方负责决定页面宽度：窄屏占满窗口，宽屏只占右侧内容栏，
 * 从而让左侧一级页面和主导航保持可见、可操作。
 *
 * 内容可能是纯文本，也可能是 HTML（服务端两侧协议都允许，见 ScreensueHtml）。
 *
 * HTML 走 **Web 组件**（用户明确要求）：agent 推的常是完整文档 —— 带 <style>
 * CSS 动画、内联 <svg>、radial-gradient 背景。RichText 只认极小标签子集，
 * 对这些一律不渲染，实测只能看到满屏源码。
 *
 * 安全：内容来自 agent（第三方），所以显式关掉 JS 与本地文件访问 ——
 * 注意 **javaScriptAccess 默认是 true**，不显式关掉等于让远端内容在客户端执行脚本。
 * 纯文本仍走 Text（无需开销，也不该把文本塞进 Web）。
 */
@Component
export struct ScreensuePage {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop pushedText: string = '';
  @Prop countdown: number = 0;
  onClose: () => void = () => {
  };

  /** 内容是不是 HTML（决定走 Web 还是 Text）。 */
  private htmlMode(): boolean {
    return looksLikeHtml(this.pushedText);
  }

  /** HTML 的 base64 载荷（空串表示不是 HTML）。 */
  private webData(): string {
    return screensueWebData(this.pushedText, this.isDark);
  }

  build() {
    Stack({ alignContent: Alignment.Bottom }) {
      GradientBackground()

      Scroll() {
        Column({ space: 14 }) {
          Row({ space: 8 }) {
            Circle({ width: 8, height: 8 })
              .fill(this.palette().accent)
            Text('agent 推送')
              .fontSize(12)
              .fontWeight(FontWeight.Medium)
              .fontColor(this.palette().textSecondary)
            Blank()
            if (this.countdown > 0) {
              Text(this.countdown.toString() + 's')
                .fontSize(12)
                .fontColor(this.palette().textMuted)
            } else {
              Text('常驻')
                .fontSize(12)
                .fontColor(this.palette().textMuted)
            }
          }
          .width('100%')

          Column() {
            if (this.htmlMode()) {
              // HTML：整篇交给 Web 渲染（base64 loadData，见 screensueWebData）。
              // 高度固定 420vp：Web 不参与父级自适应测量，给 height('100%')
              // 会在 Scroll 里塌成 0。内容区本身可滚。
              ScreenWebView({ data: this.webData(), isDark: this.isDark })
                .width('100%')
                .height(420)
            } else {
              Text(this.pushedText)
                .fontSize(16)
                .lineHeight(25)
                .fontColor(this.palette().textPrimary)
                .width('100%')
                .textAlign(TextAlign.Start)
                .copyOption(CopyOptions.LocalDevice)
            }
          }
          .width('100%')
          .padding(18)
          .borderRadius(18)
          .backgroundColor(this.palette().bgCard)
          .border({ width: 1, color: this.palette().glassBorder })
          .alignItems(HorizontalAlign.Start)
        }
        .width('100%')
        .padding({ left: 18, right: 18, top: 82, bottom: 96 })
        .alignItems(HorizontalAlign.Start)
      }
      .width('100%')
      .height('100%')
      .scrollBar(BarState.Auto)
      .align(Alignment.Top)

      PageTopBar({ title: '推送内容' })

      Row() {
        MotionBase({ pressEnabled: true, fillWidth: false }) {
          Button('关闭')
            .height(42)
            .padding({ left: 22, right: 22 })
            .fontSize(14)
            .fontWeight(FontWeight.Medium)
            .fontColor(Color.White)
            .backgroundColor(this.palette().accent)
            .borderRadius(21)
            .onClick(() => {
              this.onClose();
            })
        }
      }
      .width('100%')
      .padding({ left: 18, right: 18, bottom: 22 })
      .justifyContent(FlexAlign.End)
      .transition(TransitionEffect.OPACITY
        .combine(TransitionEffect.translate({ y: 18 }))
        .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut }))
    }
    .width('100%')
    .height('100%')
    .backgroundColor(this.palette().bgPrimary)
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }
}

/**
 * 承载 screensue HTML 的 Web 视图（独立组件，便于按内容变化重建控制器）。
 *
 * 为什么单开一个组件而不是直接在 ScreensuePage 里放 Web：
 * `WebviewController` 与 Web 组件是一对一绑定的，必须等组件挂载（onControllerAttached）
 * 才能真正 loadData；把它隔离在这里，ScreensuePage 只管布局与倒计时。
 */
@Component
struct ScreenWebView {
  // @Watch 挂在这里是必须的：ScreensuePage 在 `if (screensueVisible)` 里常驻，
  // 第二次 screensue 只会改这个 @Prop 而不会重建组件，而 onControllerAttached
  // 只在挂载时触发一次 —— 不 watch 就会一直显示上一条推送的内容。
  // 实测：连推两条不同 HTML，倒计时变了、Web 里还是旧画面。
  @Prop @Watch('onDataChanged') data: string = '';
  @Prop isDark: boolean = true;
  private controller: webview.WebviewController = new webview.WebviewController();
  /** 控制器是否已与 Web 组件关联（过早 loadData 会抛 17100001）。 */
  private attached: boolean = false;

  /** data 变化时重新加载（组件不重建，必须显式刷新）。 */
  onDataChanged(): void {
    if (this.attached) {
      this.load();
    }
  }

  build() {
    Web({ src: '', controller: this.controller })
      // ★ 内容来自 agent（第三方）：显式关闭脚本与本地文件访问。
      // javaScriptAccess 的默认值是 true，不写这一行等于放任远端内容执行脚本。
      .javaScriptAccess(false)
      .fileAccess(false)
      .domStorageAccess(false)
      .onlineImageAccess(false)
      .imageAccess(true)      // 保留内联/数据 URI 图片（不联网）
      .zoomAccess(false)      // 禁手势缩放，避免与外层滚动打架
      .horizontalScrollBarAccess(false)
      .verticalScrollBarAccess(false)
      .darkMode(WebDarkMode.Off)
      .backgroundColor(Color.Transparent)
      // 控制器挂载完才 loadData：过早调用会抛 17100001（控制器未与组件关联）。
      .onControllerAttached(() => {
        // 挂载完成才允许 loadData；此前的变更由 onDataChanged 记着，这里补一次。
        this.attached = true;
        this.load();
      })
      .width('100%')
      .height('100%')
  }

  /** 以 base64 整篇加载（空串直接跳过，避免 Web 显示错误页）。 */
  private load(): void {
    if (this.data.length === 0) {
      return;
    }
    try {
      this.controller.loadData(this.data, 'text/html', 'base64');
    } catch (e) {
      // 加载失败不该把整页带崩：保持空白，用户仍能看到顶栏与关闭按钮。
    }
  }
}
