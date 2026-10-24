/**
 * MotionBase —— 全局通用动效的"父组件"。
 *
 * 目标：把各个组件重复实现的按压反馈收拢到这唯一一个父组件里，
 * 全局统一手感（时长 token + EaseOut 曲线 + 形变幅度）。
 * 公共组件（NavRow / StatusSummaryCard / AttachmentCard / 插件卡片 …）
 * 在自己的 build 根节点接入 MotionBase，通用按压动效即随父组件生效，
 * 子组件无需再各自维护 @State pressed + scale + animation + onTouch 四件套。
 *
 * 特有动画（多态扩展）：留在各组件内部。例如聊天输入框向上弹出
 * （ChatPage 的 inputMultiLine 上移 + Curve.Friction 展开）、加号菜单浮起、
 * 折叠面板展开、toast 进出场——这些是"某个组件专属"的动效，
 * 不应塞进父组件，由各组件按需声明。
 *
 * ArkUI V1 的 @Component struct 不能继承另一位 struct 的 build，
 * 这里用「组合」表达继承：调用组件把内容经唯一内容插槽传给 MotionBase，
 * MotionBase 在包装节点上统一挂通用动效修饰器。
 *
 * 通用动效契约：
 *  - 按压反馈：pressEnabled 时整容器 scale → pressScale，快速回弹。
 *    V1 自带按压态在玻璃/透明底上几乎不可见，这里统一补足；
 *    scale 只作用于容器自身，不侵入子节点外观。
 *  - onPress 只是按压瞬时的轻量钩子（如提前高亮），
 *    真正的导航/提交语义仍由调用组件的 onClick（点击释放）负责，
 *    二者不耦合，避免"按下即触发"的手感偏差。
 *
 * 用法：
 *   MotionBase({ pressEnabled: true }) {
 *     // 实际内容；外层 onClick 由调用组件自己绑在内容上
 *   }
 *
 * 注意：包装节点默认 width('100%')。像聊天气泡里的附件卡那样需要
 * 「内容自适应宽度」的组件传 fillWidth: false，否则会被撑满整行。
 */
import { ANIM_FAST, PRESS_SCALE } from '../common/Constants';

@Component
export struct MotionBase {
  /** 内容插槽：调用组件把实际内容传进来 */
  @BuilderParam content: () => void;
  /** 是否启用按压反馈（默认关，纯展示容器传 false 零开销） */
  @Prop pressEnabled: boolean = false;
  /** 按下的瞬间回调（可选）：只做轻量即时反馈，不要在这里做导航/提交 */
  onPress?: () => void;
  /** 按压缩放幅度，默认全局 PRESS_SCALE */
  @Prop pressScale: number = PRESS_SCALE;
  /** 按压时整体透明度（1 = 不启用透明度反馈） */
  @Prop pressOpacity: number = 1;
  /** 包装节点是否撑满父宽；false = 由内容决定宽度（气泡内的卡片必须传 false） */
  @Prop fillWidth: boolean = true;
  /** 在 Row/Column 中参与剩余空间分配的权重；0 = 不参与（等分排列的按钮传 1） */
  @Prop flexWeight: number = 0;

  /** 按压态：只对容器自身 scale 生效 */
  @State private pressed: boolean = false;

  build() {
    Column() {
      this.content()
    }
    .width(this.fillWidth && this.flexWeight <= 0 ? '100%' : undefined)
    .layoutWeight(this.flexWeight)
    .alignItems(HorizontalAlign.Start)
    // ===== 通用动效（父组件统一声明，子组件继承） =====
    // 按压回弹：pressed 驱动 scale 增量，.animation 在下方覆盖该增量属性
    .scale({
      x: this.pressed ? this.pressScale : 1,
      y: this.pressed ? this.pressScale : 1,
    })
    .opacity(this.pressed ? this.pressOpacity : 1)
    .animation({ duration: ANIM_FAST, curve: Curve.EaseOut })
    .onTouch((e: TouchEvent) => {
      if (!this.pressEnabled) {
        return;
      }
      if (e.type === TouchType.Down) {
        this.pressed = true;
        const cb = this.onPress;
        if (cb !== undefined) {
          cb();
        }
      } else if (e.type === TouchType.Up || e.type === TouchType.Cancel) {
        this.pressed = false;
      }
    })
  }
}