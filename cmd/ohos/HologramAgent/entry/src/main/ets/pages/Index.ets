import { ChatPage } from './ChatPage';
import { PluginsPage } from './PluginsPage';
import { DevicePage } from './DevicePage';
import { SettingsPage } from './SettingsPage';
import { connStore } from '../common/ConnStore';
import { apiClient } from '../common/ApiClient';
import { navBar } from '../common/NavBarController';
import { handleBackPress } from '../common/NavStackRegistry';
import { ConnectionConfig } from '../model/Model';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, WIDE_MIN_WIDTH, WIDE_NAV_BAR_WIDTH, K_HAS_CONN, K_REQUESTED_TAB, K_SETTINGS_SUB } from '../common/Constants';
import { ANIM_NORMAL, ANIM_SLOW } from '../common/Constants';
import { MotionBase } from '../components/MotionBase';
import { GradientBackground } from '../components/GradientBackground';
import { ScreensuePage } from '../components/ScreensuePage';
import { registerScreensueHandler } from '../common/BridgeRouter';
import { markForegroundBridgeUIReady } from '../common/DeviceBridgeSession';
import { snapshotComponentId } from '../common/BridgeCaps';
import { ScreensuePayload } from '../common/ScreensueHtml';
import { window, display } from '@kit.ArkUI';
import { common } from '@kit.AbilityKit';

interface TabItem {
  icon: Resource;
  label: string;
  pageId: string;
}

/**
 * 底部导航单项：矢量图标 + 文字。
 * active 必须 @Prop 才能在父组件 currentTab 变化时刷新高亮。
 */
@Component
export struct RailTab {
  icon: Resource = $r('app.media.ic_tab_chat');
  @Prop label: string = '';
  @Prop active: boolean = false;
  @Prop accent: string = '';
  @Prop muted: string = '';

  build() {
    // 底部导航按压反馈：交给 MotionBase 统一（flexWeight 由外层调用点的
    // .layoutWeight(1) 负责，这里内部只需撑满自己那一格）。
    MotionBase({ pressEnabled: true }) {
      Column({ space: 3 }) {
        Image(this.icon)
          .width(22)
          .height(22)
          .fillColor(this.active ? this.accent : this.muted)
          // 选中态的颜色迁移必须写在子节点上：父容器的 .animation()
          // 只驱动它自己的属性，不会下传给 Image / Text。
          .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
          .draggable(false)

        Text(this.label)
          .fontSize(10)
          // fontWeight 不可插值，切换时仍是跳变，只对颜色做过渡
          .fontWeight(this.active ? FontWeight.Medium : FontWeight.Normal)
          .fontColor(this.active ? this.accent : this.muted)
          .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
          .maxLines(1)
      }
      .justifyContent(FlexAlign.Center)
      .alignItems(HorizontalAlign.Center)
      .width('100%')
      .height(48)
    }
  }
}

@Entry
@Component
struct Index {
  @Watch('onTabChanged') @State currentTab: number = 0;
  @StorageProp('navVisible') private navVisible: boolean = true;
  /** 二级页面打开时锁住 Swiper 横滑，避免误切换主 Tab */
  @StorageProp('subPageOpen') private subPageOpen: boolean = false;
  /** 宽屏（>=600vp）：一级界面在左、二级界面在右，导航栏跟随左侧一级界面 */
  @StorageProp('isWideScreen') private isWide: boolean = false;
  @State screensueVisible: boolean = false;
  @State screensueText: string = '';
  @State screensueCountdown: number = 0;
  /** 状态栏避让高度（vp），从窗口 avoid area 动态获取；默认 44 兜底 */
  @State topInset: number = 44;
  /** 底部手势条高度（vp） */
  @State bottomGesture: number = 16;
  @StorageProp('themeIsDark') @Watch('onThemeChanged') private isDark: boolean = true;
  /** 外部请求切换主 Tab（未连接时聊天空态的“去设置连接”用） */
  @StorageProp(K_REQUESTED_TAB) @Watch('onRequestedTab') private requestedTab: number = -1;
  private swiper: SwiperController = new SwiperController();
  private screensueTimer: number = -1;
  private snapshotBuilder: CustomBuilder = (): void => { }; // 由 @Builder 传入的实际锚点

  // 「状态」Tab 已删除：运行状态并入设置页（顶部摘要卡 + 二级明细页），
  // 底部导航只保留四项，胶囊内每项更宽、点按更好命中。
  private tabs: TabItem[] = [
    { icon: $r('app.media.ic_tab_chat'), label: '聊天', pageId: 'chat' },
    { icon: $r('app.media.ic_tab_plugins'), label: '插件', pageId: 'plugins' },
    { icon: $r('app.media.ic_tab_devices'), label: '设备', pageId: 'devices' },
    { icon: $r('app.media.ic_tab_settings'), label: '设置', pageId: 'settings' },
  ];

  aboutToAppear(): void {
    const cur: ConnectionConfig | null = connStore.getCurrentConnection();
    if (cur !== null) {
      apiClient.setConnection(cur);
    }
    // 后端连接状态广播：聊天空态根据它决定是否显示“去设置连接”。
    // Index.aboutToAppear 在 EntryAbility 等 connStore.init 之后才跑，
    // 所以此处读到的连接状态就是真实的启动态。
    AppStorage.setOrCreate<boolean>(K_HAS_CONN, apiClient.hasConnection());
    AppStorage.setOrCreate<number>(K_REQUESTED_TAB, -1);
    AppStorage.setOrCreate<string>(K_SETTINGS_SUB, '');
    // 种子化自定义背景图状态到 AppStorage，GradientBackground 响应读取
    const st = connStore.getSettings();
    AppStorage.setOrCreate<string>('bgImage', st.bgImage ?? '');
    AppStorage.setOrCreate<number>('bgOpacity', st.bgOpacity ?? 0.25);
    AppStorage.setOrCreate<boolean>('navVisible', true);
    AppStorage.setOrCreate<boolean>('subPageOpen', false);
    AppStorage.setOrCreate<boolean>('isWideScreen', false);
    // 启动时按当前主题刷新一次系统栏（EntryAbility 已设过，这里兜底对齐）
    this.syncSystemBar();
    // 动态读取状态栏/导航栏避让区，实现真正的沉浸式布局（替换硬编码 top:44）
    this.resolveSafeArea();
    // 根 UI 只负责 screensue 呈现；命令路由由前台全局设备桥安装。
    registerScreensueHandler((payload: ScreensuePayload) => {
      this.showScreensue(payload);
    });
    markForegroundBridgeUIReady(getContext(this) as common.UIAbilityContext);
  }

  /** agent 下发的 screensue 内容展示（窄屏整页、宽屏右栏；0=常驻）。 */
  private showScreensue(payload: ScreensuePayload): void {
    this.screensueText = payload.content;
    this.screensueCountdown = payload.duration;
    this.screensueVisible = true;
    if (this.screensueTimer >= 0) {
      clearInterval(this.screensueTimer);
      this.screensueTimer = -1;
    }
    if (payload.duration > 0) {
      this.screensueTimer = setInterval(() => {
        if (this.screensueCountdown <= 1) {
          clearInterval(this.screensueTimer);
          this.screensueTimer = -1;
          this.screensueVisible = false;
        } else {
          this.screensueCountdown = this.screensueCountdown - 1;
        }
      }, 1000);
    }
  }

  private closeScreensue(): void {
    if (this.screensueTimer >= 0) {
      clearInterval(this.screensueTimer);
      this.screensueTimer = -1;
    }
    this.screensueVisible = false;
  }

  /** 主题翻转时同步系统栏前景/背景色，避免浅色页面配黑状态栏。 */
  private onThemeChanged(): void {
    this.syncSystemBar();
  }

  /** 通过窗口 avoid area 计算状态栏与底部手势区高度（vp），失败时保留默认值。 */
  private resolveSafeArea(): void {
    try {
      const ctx = getContext(this) as common.UIAbilityContext;
      window.getLastWindow(ctx).then((win: window.Window) => {
        const prop = win.getWindowProperties();
        const isLayoutFull = prop.isLayoutFullScreen === true;
        if (!isLayoutFull) {
          return; // 非全屏模式下系统自动避让
        }
        const density = display.getDefaultDisplaySync().densityPixels > 0
          ? display.getDefaultDisplaySync().densityPixels : 3;
        const topArea = win.getWindowAvoidArea(window.AvoidAreaType.TYPE_SYSTEM);
        const bottomArea = win.getWindowAvoidArea(window.AvoidAreaType.TYPE_NAVIGATION_INDICATOR);
        if (topArea.topRect.height > 0) {
          this.topInset = Math.ceil(topArea.topRect.height / density);
        }
        // 底部手势条：有值时取其高度的一半作为悬浮导航的下边距基准，最小 8 最大 24
        if (bottomArea.bottomRect.height > 0) {
          const h: number = Math.ceil(bottomArea.bottomRect.height / density);
          this.bottomGesture = Math.min(24, Math.max(8, h));
        } else {
          this.bottomGesture = 16;
        }
      }).catch(() => {
        // ignore, keep defaults
      });
    } catch (e) {
      // ignore
    }
    AppStorage.set<number>('currentTab', 0);
  }

  private onTabChanged(): void {
    AppStorage.set<number>('currentTab', this.currentTab);
  }

  /**
   * 响应外部切 Tab 请求（聊天空态的“去设置连接”）。
   *
   * 为什么不能直接改 AppStorage 的 currentTab：Index 的 currentTab 是
   * @State，Swiper.index() 只认它；外部写 AppStorage 不会驱动 Swiper。
   * 所以用独立请求键 + @Watch 把请求转成自己的状态变更。
   */
  private onRequestedTab(): void {
    const t: number = this.requestedTab;
    AppStorage.set<number>(K_REQUESTED_TAB, -1);
    if (t < 0 || t >= this.tabs.length) {
      return;
    }
    this.currentTab = t;
    navBar.setVisible(true);
  }

  private syncSystemBar(): void {
    const dark: boolean = this.isDark;
    const bg: string = dark ? '#000000' : '#F1F3F5';
    const fg: string = dark ? '#FFFFFF' : '#191919';
    try {
      window.getLastWindow(getContext(this) as common.UIAbilityContext).then((win: window.Window) => {
        win.setWindowSystemBarProperties({
          statusBarColor: bg,
          statusBarContentColor: fg,
          navigationBarColor: bg,
        });
        win.setWindowBackgroundColor(bg);
      }).catch(() => {
        // ignore
      });
    } catch (e) {
      // ignore
    }
  }

  private palette(): ThemePalette {
    // 引用 this.isDark 建立响应式依赖：主题切换时整个组件树重渲染
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  /** 宽屏判定：窗口宽度 >= 600vp（与 Navigation Auto 模式的分栏阈值一致） */
  private updateWideScreen(w: number): void {
    const wide: boolean = w >= WIDE_MIN_WIDTH;
    if (wide !== this.isWide) {
      AppStorage.set<boolean>('isWideScreen', wide);
    }
  }

  /**
   * 统一的返回处理：把返回事件精确派发给【当前 Tab】的那个 Navigation 栈。
   *
   * 四个页面各自持有 Navigation（宽屏需要"左一级右二级"），它们在 Swiper 里
   * 同时存活，系统返回落到哪个栈并不确定 —— 这就是"有的页面无法使用返回手势"
   * 的根因。这里按 currentTab 显式选栈，行为对所有页面一致。
   */
  onBackPress(): boolean {
    if (this.screensueVisible) {
      this.closeScreensue();
      return true;
    }
    return handleBackPress(this.currentTab, this.isWide);
  }

  build() {
    // alignContent Bottom：悬浮导航停靠底部，而不是被 Stack 居中。
    // 导航胶囊用左右 6% padding 保持"宽度占 88% 居中"的原样视觉；
    // 宽屏下胶囊自然被限制在左侧一级界面栏内，不会横跨二级界面。
    Stack({ alignContent: Alignment.Bottom }) {
      // 渐变氛围背景
      GradientBackground()

      // 页面内容：Swiper 支持左右滑动切换；$$ 双向同步索引
      Swiper(this.swiper) {
        ChatPage()
        PluginsPage()
        DevicePage()
        SettingsPage()
      }
      .index(this.currentTab)
      .indicator(false)
      .loop(false)
      .duration(300)
      .curve(Curve.FastOutSlowIn)
      .vertical(false)
      .cachedCount(2)
      .disableSwipe(this.subPageOpen)
      .onChange((idx: number) => {
        this.currentTab = idx;
        navBar.setVisible(true);
      })
      .width('100%')
      .height('100%')
      // top 让出状态栏；bottom：所有页面统一为0，导航栏浮在内容之上
      .padding({
        top: this.topInset,
        bottom: 0
      })

      // 悬浮气态玻璃底部导航（三层：半透明底 -> 高光渐变 -> 内容）
      // 外层 Row 负责水平定位：窄屏居中；宽屏靠左，只落在一级界面栏范围内。
      Row() {
        Stack() {
          // 层1：玻璃底 —— 半透明背景（无 backdropBlur 以避免矩形模糊伪影）
          Column()
            .width('100%')
            .height('100%')
            .backgroundColor(this.palette().navBarBg)

          // 层2：玻璃高光渐变，模拟光从上方照射的质感
          Row()
            .width('100%')
            .height('100%')
            .linearGradient({
              direction: GradientDirection.Top,
              colors: [
                [this.palette().navBarGradientStart, 0.0],
                [this.palette().navBarGradientEnd, 1.0],
              ],
            })

          // 层3：导航项
          Row({ space: 2 }) {
            ForEach(this.tabs, (tab: TabItem, index: number) => {
              RailTab({
                icon: tab.icon,
                label: tab.label,
                active: this.currentTab === index,
                accent: this.palette().accent,
                muted: this.palette().textMuted,
              })
                .layoutWeight(1)
                .onClick(() => {
                  this.currentTab = index;
                  navBar.setVisible(true);
                })
            }, (item: TabItem, index: number) => item.pageId + index.toString())
          }
          .width('100%')
          .height(62)
          .padding({ left: 14, right: 14 })
          .alignItems(VerticalAlign.Center)
        }
        // 宽屏：胶囊宽度锁在左侧一级界面栏内（左右各留 24vp 余量）
        .width(this.isWide ? WIDE_NAV_BAR_WIDTH - 48 : '88%')
        .height(62)
        .borderRadius(28)
        .clip(true)
        .border({
          width: { left: 1, top: 1, right: 1, bottom: 1 },
            color: this.palette().navBarBorder,
        })
        .shadow({
          radius: 24,
          color: this.palette().shadow,
          offsetY: 8,
        })
      }
      .width('100%')
      .height(62)
      .padding({ left: this.isWide ? 24 : 0 })
      .justifyContent(this.isWide ? FlexAlign.Start : FlexAlign.Center)
      .margin({ bottom: this.bottomGesture + 6 })
      // 所有页面滑动时隐藏导航栏
      .translate({ y: !this.navVisible ? 130 : 0 })
      .opacity(!this.navVisible ? 0 : 1)
      .animation({ duration: ANIM_SLOW, curve: Curve.EaseOut })
      // 导航栏本身不吃触摸空白区，避免遮住下层内容点击
      .hitTestBehavior(HitTestMode.Transparent)

      // screensue 是前台内容页：窄屏覆盖整页；宽屏仅覆盖右侧内容栏，
      // 左侧一级页面与主导航保持可见、可操作。
      if (this.screensueVisible) {
        if (this.isWide) {
          Row() {
            Column()
              .width(WIDE_NAV_BAR_WIDTH)
              .height('100%')
              .hitTestBehavior(HitTestMode.None)

            ScreensuePage({
              pushedText: this.screensueText,
              countdown: this.screensueCountdown,
              onClose: () => {
                this.closeScreensue();
              },
            })
              .layoutWeight(1)
              .height('100%')
              .padding({ top: this.topInset })
          }
          .width('100%')
          .height('100%')
          .hitTestBehavior(HitTestMode.Transparent)
        } else {
          ScreensuePage({
            pushedText: this.screensueText,
            countdown: this.screensueCountdown,
            onClose: () => {
              this.closeScreensue();
            },
          })
            .width('100%')
            .height('100%')
            .padding({ top: this.topInset })
        }
      }
    }
    .width('100%')
    .height('100%')
    .id(snapshotComponentId())
    // 实时跟随窗口宽度：平板旋转、折叠展开、分屏、自由窗口拖拽都会回调，
    // 比启动时查一次 display 更可靠（不会漏掉运行中的尺寸变化）。
    .onAreaChange((oldValue: Area, newValue: Area) => {
      this.updateWideScreen(newValue.width as number);
    })
  }
}
