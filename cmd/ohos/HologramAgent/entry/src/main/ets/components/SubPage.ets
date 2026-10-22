import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, RADIUS_LG,
  ANIM_NORMAL } from '../common/Constants';
import { MotionBase } from './MotionBase';
import { handleNavOnScroll } from '../common/NavBarController';
import { PageTopBar, NavFloatOverlay, NavFloatRow, FloatIconButton } from './PageTopBar';
import { GradientBackground } from './GradientBackground';

/**
 * 二级页面开关标志（AppStorage）。
 * Index 读取它来锁住 Swiper 左右滑动——否则在二级页面里横滑会误切换主 Tab。
 * 由 Navigation.onNavBarStateChange 驱动，与真实导航栈严格同步。
 */
export const KEY_SUBPAGE_OPEN: string = 'subPageOpen';

export function markSubPageOpen(open: boolean): void {
  if (AppStorage.has(KEY_SUBPAGE_OPEN)) {
    AppStorage.set<boolean>(KEY_SUBPAGE_OPEN, open);
  } else {
    AppStorage.setOrCreate<boolean>(KEY_SUBPAGE_OPEN, open);
  }
}

/** pushPathByName 的参数载体：ArkTS 不允许把 string 断言成 object */
export interface SubPageParam {
  id: string;
}

export function subPageParam(id: string): SubPageParam {
  const p: SubPageParam = { id: id };
  return p;
}

/**
 * 二级页面内容层：放在 NavDestination 里使用。
 *
 * 为什么二级页面走 Navigation / NavDestination 而不是自己用 @State 切换：
 *  - NavDestination 由系统维护导航栈，侧滑返回、三键返回、
 *    以及无障碍返回都会自动 pop，不需要自己拦 onBackPress；
 *  - 转场动画由系统提供，与其他系统应用一致。
 *
 * 视觉约定与一级页面完全一致：
 *  - 自带同款渐变背景（NavDestination 会整屏盖住一级内容，
 *    不铺背景会透出下层列表形成重影）；
 *  - 顶栏只有标题，上边缘由不透明渐变到透明；
 *  - 返回按钮不放顶栏，而是作为独立悬浮组件放在底部悬浮区（拇指可达）。
 *
 * 注意：@BuilderParam 只能有一个（trailing lambda 限制），
 * 所以悬浮区的刷新按钮用 showRefresh + onRefresh 两个普通属性表达，
 * 而不是第二个 @BuilderParam。
 */
@Component
export struct SubPageLayer {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  /** 宽屏分栏时本层就是右侧栏，一级界面一直在左边可见，返回键无意义 */
  @StorageProp('isWideScreen') private isWide: boolean = false;
  @Prop title: string = '';
  /** 所属主 Tab 序号，供底部悬浮区的显隐动画使用 */
  @Prop tab: number = 0;
  /** 悬浮区是否附带刷新按钮 */
  @Prop showRefresh: boolean = false;
  onBack?: () => void;
  onRefresh?: () => void;
  @BuilderParam content: () => void;

  build() {
    Stack({ alignContent: Alignment.Bottom }) {
      // 二级页面自带背景：Stack 模式下整屏覆盖，必须自己铺底；
      // 宽屏分栏时右栏也需要同款背景，与左栏视觉连续。
      GradientBackground()

      Scroll() {
        Column() {
          this.content()
        }
        .width('100%')
        // 宽屏右栏底部没有主导航胶囊，只保留悬浮键的空间
        .padding({ left: 16, right: 16, top: 76, bottom: this.isWide ? 108 : 174 })
        .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
        .alignItems(HorizontalAlign.Start)
      }
      .width('100%')
      .height('100%')
      .scrollBar(BarState.Off)
      .align(Alignment.Top)
      .onDidScroll((xOffset: number, yOffset: number, state: ScrollState) => {
        handleNavOnScroll(state);
      })

      PageTopBar({ title: this.title })

      // 底部悬浮区：返回键（+ 可选刷新键）。
      // 宽屏分栏时左栏常驻可见，一级页面自己的悬浮刷新键也还在屏幕上，
      // 这里再挂一个就成了"两个重加载按钮"——所以宽屏下本层不出刷新键，
      // 由一级页面那一个统一负责（它的回调会连带刷新右栏内容）。
      NavFloatOverlay({ tab: this.tab }) {
        NavFloatRow() {
          if (!this.isWide) {
            // 自定义组件不能直接挂 .transition()（会生成 __Common__ 包装节点），
            // 所以各自套一层无 padding 的 Column，由它承载出入场动画，
            // 让悬浮键与 400ms 平滑滑出的悬浮层不再"一帧硬切"。
            Column() {
              SubPageBackButton({
                onTap: () => {
                  const cb = this.onBack;
                  if (cb !== undefined) {
                    cb();
                  }
                },
              })
            }
            .transition(TransitionEffect.OPACITY.combine(TransitionEffect.scale({ x: 0.8, y: 0.8 })).animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut }))
            if (this.showRefresh) {
              Column() {
                FloatIconButton({
                  icon: $r('app.media.ic_refresh'),
                  onTap: () => {
                    const cb = this.onRefresh;
                    if (cb !== undefined) {
                      cb();
                    }
                  },
                })
              }
              .transition(TransitionEffect.OPACITY.combine(TransitionEffect.scale({ x: 0.8, y: 0.8 })).animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut }))
            }
          }
        }
      }
    }
    .width('100%')
    .height('100%')
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }
}

/**
 * 一级页面里的入口行：图标 + 标题 + 副标题 + 右侧摘要值 + 右尖角。
 * 点击进入二级页面。
 */
@Component
export struct NavRow {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop icon: Resource | undefined = undefined;
  @Prop title: string = '';
  @Prop subtitle: string = '';
  @Prop value: string = '';
  @Prop tint: string = '';
  @Prop showDivider: boolean = true;
  /** 宽屏分栏时右栏正显示本行对应的二级页面 —— 高亮当前项 */
  @Prop selected: boolean = false;
  onTap?: () => void;

  build() {
    // 通用按压动画收进 MotionBase（父组件）：scale 回弹统一。
    // 选中态底色仍在这里管：它是 NavRow 特有外观，不归父组件。
    MotionBase({ pressEnabled: true }) {
      Row() {
        if (this.icon !== undefined) {
          Row() {
            Image(this.icon)
              .width(17)
              .height(17)
              .fillColor(this.iconColor())
              .draggable(false)
          }
          .width(32)
          .height(32)
          .borderRadius(10)
          .backgroundColor(this.iconBg())
          .justifyContent(FlexAlign.Center)
          .margin({ right: 12 })
        }

        Column({ space: 2 }) {
          Text(this.title)
            .fontSize(15)
            .fontColor(this.selected ? this.palette().accent : this.palette().textPrimary)
            // 父行 MotionBase 的 .animation() 到不了子节点，选中态字色自己缓动
            .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
            .fontWeight(this.selected ? FontWeight.Medium : FontWeight.Normal)
            .maxLines(1)
            .textOverflow({ overflow: TextOverflow.Ellipsis })
          if (this.subtitle.length > 0) {
            Text(this.subtitle)
              .fontSize(11)
              .fontColor(this.palette().textMuted)
              .maxLines(1)
              .textOverflow({ overflow: TextOverflow.Ellipsis })
          }
        }
        .layoutWeight(1)
        .alignItems(HorizontalAlign.Start)

        if (this.value.length > 0) {
          Text(this.value)
            .fontSize(12)
            .fontColor(this.palette().textSecondary)
            .maxLines(1)
            .textOverflow({ overflow: TextOverflow.Ellipsis })
            .constraintSize({ maxWidth: 130 })
            .margin({ right: 8 })
        }

        Image($r('app.media.ic_chevron_right'))
          .width(15)
          .height(15)
          .fillColor(this.selected ? this.palette().accent : this.palette().textMuted)
          .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
          .draggable(false)
      }
      .width('100%')
      .padding({ left: 14, right: 14, top: 12, bottom: 12 })
      .alignItems(VerticalAlign.Center)
      // 选中态底色：写在 .border 之前，避免分割线宽度被纳入动画。
      // MotionBase 的 .animation() 只作用在包装 Column 上，到不了这里，
      // 选中高亮的底色迁移要本行自己声明。
      .backgroundColor(this.selected ? this.palette().accentBg : Color.Transparent)
      .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
      .border({
        width: { bottom: this.showDivider ? 1 : 0 },
        color: this.palette().kvBorder,
      })
      .onClick(() => {
        const cb = this.onTap;
        if (cb !== undefined) {
          cb();
        }
      })
    }
  }

  private iconColor(): string {
    return this.tint.length > 0 ? this.tint : this.palette().accent;
  }

  private iconBg(): string {
    return this.tint.length > 0 ? this.palette().bgHover : this.palette().accentBg;
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }
}

/**
 * 分组卡：一个标题 + 一组 NavRow，圆角裁剪让行分割线不越出卡片。
 */
@Component
export struct NavGroup {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop caption: string = '';
  @BuilderParam content: () => void;

  build() {
    Column() {
      if (this.caption.length > 0) {
        Text(this.caption)
          .fontSize(12)
          .fontColor(this.palette().textMuted)
          .margin({ left: 4, bottom: 8 })
      }
      Column() {
        this.content()
      }
      .width('100%')
      .borderRadius(RADIUS_LG)
      .clip(true)
      .backgroundColor(this.palette().bgCard)
      .border({ width: 1, color: this.palette().glassBorder })
    }
    .width('100%')
    .alignItems(HorizontalAlign.Start)
    .margin({ bottom: 18 })
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }
}

/**
 * 通用内容卡（二级页面里承载表单/明细的容器）。
 */
@Component
export struct PlainCard {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop caption: string = '';
  @BuilderParam content: () => void;

  build() {
    Column() {
      if (this.caption.length > 0) {
        Text(this.caption)
          .fontSize(12)
          .fontColor(this.palette().textMuted)
          .margin({ left: 4, bottom: 8 })
      }
      Column() {
        this.content()
      }
      .width('100%')
      .padding(16)
      .borderRadius(RADIUS_LG)
      .backgroundColor(this.palette().bgCard)
      .border({ width: 1, color: this.palette().glassBorder })
      .alignItems(HorizontalAlign.Start)
    }
    .width('100%')
    .alignItems(HorizontalAlign.Start)
    .margin({ bottom: 18 })
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }
}

/**
 * 二级页面的返回按钮：与悬浮图标按钮同款玻璃，放在底部悬浮区最左侧。
 * 系统侧滑返回同样可用，这个按钮只是给单手拇指多一条路径。
 */
@Component
export struct SubPageBackButton {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  onTap?: () => void;

  build() {
    // 玻璃底自带的按压变暗在 45% 透明度上几乎看不见，缩放反馈由 MotionBase 统一提供。
    // fillWidth: false —— 悬浮区按内容宽度排列，不能撑满整行。
    MotionBase({ pressEnabled: true, fillWidth: false }) {
      Button() {
        Row({ space: 4 }) {
          Image($r('app.media.ic_arrow_back'))
            .width(17)
            .height(17)
            .fillColor(this.palette().textSecondary)
            .draggable(false)
        }
      }
      .width(42)
      .height(42)
      .type(ButtonType.Circle)
      .backgroundColor(this.palette().navBarBg)
      .border({
        width: { left: 1, top: 1, right: 1, bottom: 1 },
        color: this.palette().navBarBorder,
      })
      .shadow({ radius: 24, color: this.palette().shadow, offsetY: 8 })
      .onClick(() => {
        const cb = this.onTap;
        if (cb !== undefined) {
          cb();
        }
      })
    }
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }
}
