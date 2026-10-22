import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE } from '../common/Constants';
import { ANIM_FAST, ANIM_SLOW } from '../common/Constants';
import { MotionBase } from './MotionBase';

/**
 * 顶栏高度（vp）。页面 padding-top 应略大于此值，避免内容被标题压住，
 * 同时不要留过多空白。
 */
export const TOP_BAR_HEIGHT: number = 68;

/**
 * 页面顶栏遮罩：仅标题文字，无按钮无状态。
 * 浮在滚动区之上（页面用 PageTopBarLayer 置顶），背景为 不透明 -> 透明 的线性渐变，
 * 滚动内容从其下方穿过时逐渐淡出，形成"逐渐加深"的柔化边界，而不是硬截断。
 *
 * 关键：自定义组件被外部施加 .position() 时，ArkUI 会生成一个 __Common__ 包裹节点，
 * 该节点默认铺满父约束并使用默认命中测试 —— 会吞掉整页触摸事件。
 * 因此必须在【调用点】同时显式给出尺寸与 hitTestBehavior(None)，
 * 统一封装在 PageTopBarLayer 里，页面不要再手写 .position()。
 */
@Component
export struct PageTopBar {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop title: string = '';

  build() {
    // 外层撑满整页并顶部对齐，替代调用点的 .position()：
    // 自定义组件一旦在调用点被施加 .position()，ArkUI 会生成铺满父约束的
    // __Common__ 包裹节点，该节点使用默认命中测试，会吞掉整页的滚动与点击。
    // 这里改为自身撑满 + 全链路 HitTestMode.None，触摸完全穿透到下层滚动区。
    Column() {
      Column() {
        Text(this.title)
          .fontSize(20)
          .fontWeight(FontWeight.Bold)
          .fontColor(this.palette().textPrimary)
          .margin({ left: 16, top: 10 })
          .hitTestBehavior(HitTestMode.None)
      }
      .width('100%')
      .height(TOP_BAR_HEIGHT)
      .alignItems(HorizontalAlign.Start)
      .linearGradient({
        direction: GradientDirection.Bottom,
        colors: [
          [this.opaqueBg(), 0.0],
          [this.opaqueBg(), 0.45],
          [this.transparentBg(), 1.0],
        ],
      })
      .hitTestBehavior(HitTestMode.None)
    }
    .width('100%')
    .height('100%')
    .justifyContent(FlexAlign.Start)
    .hitTestBehavior(HitTestMode.None)
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  private opaqueBg(): string {
    const bg: string = this.palette().bgPrimary;
    return '#FF' + bg.substring(1);
  }

  private transparentBg(): string {
    const bg: string = this.palette().bgPrimary;
    return '#00' + bg.substring(1);
  }
}

/**
 * 独立气态玻璃节点：单个悬浮组件，与底部导航同款玻璃（半透明底 + 高光渐变）。
 * 尺寸自适应内容（宽度随内容，高度/圆角参数化），视觉上独立、不与其他组件共框。
 * 用于承载状态胶囊、图标按钮等单个悬浮元素。
 */
@Component
export struct GlassShell {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop nodeHeight: number = 32;
  @Prop nodeRadius: number = 16;
  @BuilderParam content: () => void;

  build() {
    Row() {
      this.content()
    }
    .height(this.nodeHeight)
    .padding({ left: 8, right: 8 })
    .alignItems(VerticalAlign.Center)
    .backgroundColor(this.palette().navBarBg)
    .borderRadius(this.nodeRadius)
    .border({
      width: { left: 1, top: 1, right: 1, bottom: 1 },
      color: this.palette().navBarBorder,
    })
    .shadow({
      radius: 16,
      color: this.palette().shadow,
      offsetY: 5,
    })
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }
}

/**
 * 悬浮区圆形图标按钮的直径（vp）。徽标不使用它——徽标是竖向窄条。
 */
export const FLOAT_NODE_SIZE: number = 42;

/**
 * 悬浮图标按钮：与底部导航同一套玻璃语言的圆形按钮。
 * 统一 42vp 直径、同款玻璃底/描边/投影，图标 18vp。
 */
@Component
export struct FloatIconButton {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop icon: Resource | undefined = undefined;
  @Prop accent: boolean = false;
  onTap?: () => void;

  build() {
    // 按压缩放交给 MotionBase（父组件统一手感）；
    // fillWidth: false —— 悬浮区靠右对齐，包装节点必须按内容 42vp 收窄。
    MotionBase({ pressEnabled: true, fillWidth: false }) {
      Button() {
        Image(this.icon)
          .width(18)
          .height(18)
          .fillColor(this.accent ? this.palette().accent : this.palette().textSecondary)
          .animation({ duration: ANIM_FAST, curve: Curve.EaseOut })
      }
      .width(FLOAT_NODE_SIZE)
      .height(FLOAT_NODE_SIZE)
      .type(ButtonType.Circle)
      .backgroundColor(this.palette().navBarBg)
      .border({
        width: { left: 1, top: 1, right: 1, bottom: 1 },
        color: this.palette().navBarBorder,
      })
      .shadow({
        radius: 24,
        color: this.palette().shadow,
        offsetY: 8,
      })
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

/**
 * 悬浮区的"一行"：宽度自适应内容，内部组件从右向左排列（右边缘与导航栏平齐）。
 * 放在 NavFloatOverlay 内部使用；声明顺序越靠前越贴近导航栏，后声明的行叠在其上方。
 */
@Component
export struct NavFloatRow {
  @BuilderParam content: () => void;

  build() {
    Row({ space: 8 }) {
      this.content()
    }
    .alignItems(VerticalAlign.Center)
  }
}

/**
 * 底部悬浮容器：透明、无任何背景/边框（视觉上不包裹任何东西，组件彼此独立），
 * 仅负责 右侧对齐（与导航栏同宽 88%）、自下而上分行堆叠 与 随滚动渐隐/滑出动画。
 *
 * 布局约定：直接子节点是"一行"（通常是自适应宽度的 Row）。
 * 行按【从上到下】声明：先声明的行在上方，最后声明的行贴住导航栏，
 * 即"一行放不下时向上面再开一行"（把新行写在前面）。
 *
 * 关键：这里必须用 Column 而不是 Flex —— Flex 在 ArkUI 中默认铺满父约束，
 * 会形成一个覆盖整页的默认命中测试节点，吞掉页面滚动与点击；
 * Column 高度自适应内容。再加 HitTestMode.Transparent，
 * 让每行右侧之外的空白区域触摸穿透到下层滚动区。
 */
@Component
export struct NavFloatOverlay {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @StorageProp('navVisible') private navVisible: boolean = true;
  @StorageProp('currentTab') private currentTab: number = 0;
  @Prop tab: number = 0;
  @Prop alignEnd: boolean = true;
  @BuilderParam content: () => void;

  build() {
    Column({ space: 8 }) {
      this.content()
    }
    .width('88%')
    .alignItems(this.alignEnd ? HorizontalAlign.End : HorizontalAlign.Start)
    .hitTestBehavior(HitTestMode.Transparent)
    .margin({ bottom: 94 })
    .translate({ y: (!this.navVisible || this.currentTab !== this.tab) ? 140 : 0 })
    .opacity((!this.navVisible || this.currentTab !== this.tab) ? 0 : 1)
    .animation({ duration: ANIM_SLOW, curve: Curve.EaseOut })
  }
}