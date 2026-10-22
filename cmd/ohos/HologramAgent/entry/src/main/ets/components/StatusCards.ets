import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, RADIUS_LG, RADIUS_SM,
  ANIM_FAST, ANIM_NORMAL, ANIM_ENTER } from '../common/Constants';
import { COLOR_ACCENT, COLOR_SUCCESS, COLOR_CYAN, COLOR_ERROR } from '../common/Constants';
import { MotionBase } from './MotionBase';
import { RuntimePanel } from './RuntimePanel';
import {
  statusStore, StatGroup, StatField, compactDuration,
  K_UP, K_VERSION, K_STARTED, K_AGENTS, K_PLUGINS, K_TOOLS, K_ERR, K_LOADING, K_REV, K_BUILD,
} from '../common/StatusStore';

/**
 * 运行状态摘要卡：嵌在设置一级页顶部（原「状态」Tab 已删除）。
 *
 * 只放一眼可读的东西：环形仪表 + 运行时长 + 三项 KPI + 版本，
 * 明细（内核 / 模型 / 记忆 / 运行时 / 工具清单）走二级页面。
 * 点击整卡进入明细，所以自身不放任何按钮，避免嵌套点击。
 */
@Component
export struct StatusSummaryCard {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @StorageProp(K_UP) private up: boolean = false;
  @StorageProp(K_VERSION) private version: string = '-';
  /** 内核名 · commit。与版本号分开一行：版本号本身没有内核身份。 */
  @StorageProp(K_BUILD) private buildSub: string = '';
  @StorageProp(K_STARTED) private startedAt: string = '';
  @StorageProp(K_AGENTS) private agents: number = 0;
  @StorageProp(K_PLUGINS) private plugins: number = 0;
  @StorageProp(K_TOOLS) private tools: number = 0;
  @StorageProp(K_ERR) private err: string = '';
  @StorageProp(K_LOADING) private loading: boolean = false;
  /** 秒级刷新的运行时长文本：环心不能用 startedAt 直接算，否则不会自动跳秒 */
  @State private uptime: string = '-';
  private timerId: number = -1;
  onTap?: () => void;

  aboutToAppear(): void {
    this.uptime = compactDuration(this.startedAt);
    this.timerId = setInterval(() => {
      this.uptime = compactDuration(this.startedAt);
    }, 1000);
  }

  aboutToDisappear(): void {
    if (this.timerId >= 0) {
      clearInterval(this.timerId);
      this.timerId = -1;
    }
  }

  build() {
    // 整卡按压反馈统一收进 MotionBase（父组件）：这里是进明细页的唯一入口，
    // 父组件负责 scale 回弹；导航语义仍由本卡自己的 onClick（点击释放）负责，
    // 避免"按下即跳转"的误触手感。
    MotionBase({ pressEnabled: true }) {
    Column() {
      if (this.err.length > 0) {
        // 连接失败：只显示一句人话，技术细节在 hilog 里
        Row() {
          Image($r('app.media.ic_error'))
            .width(16)
            .height(16)
            .fillColor(COLOR_ERROR)
            .draggable(false)
          Text(this.err)
            .fontSize(13)
            .fontColor(COLOR_ERROR)
            .layoutWeight(1)
            .margin({ left: 10 })
        }
        .width('100%')
        .alignItems(VerticalAlign.Top)
        .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_ENTER, curve: Curve.EaseOut }))
      } else if (this.loading && !this.up) {
        Row() {
          LoadingProgress()
            .width(26)
            .height(26)
            .color(this.palette().accent)
        }
        .width('100%')
        .justifyContent(FlexAlign.Center)
        .padding({ top: 20, bottom: 20 })
        .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
      } else {
        // 状态主行：一枚状态图标（运行=脉搏，离线=断开）+ 状态词 + 运行时长。
        // 之前那个环形进度条表达的是"100% / 0%"，而在线与否是布尔量，
        // 用百分比环表示只会让人以为有什么在加载；架构是单 agent 单 session，
        // "Agent 数" 恒为 1，也没有信息量，一并去掉。
        Row({ space: 12 }) {
          Stack({ alignContent: Alignment.Center }) {
            Image(this.up ? $r('app.media.ic_pulse') : $r('app.media.ic_offline'))
              .width(24)
              .height(24)
              .fillColor(this.up ? COLOR_SUCCESS : this.palette().textMuted)
              .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
              .draggable(false)
          }
          .width(48)
          .height(48)
          .borderRadius(RADIUS_SM)
          .backgroundColor(this.up ? 'rgba(23, 169, 100, 0.16)' : this.palette().bgHover)
          .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })

          Column({ space: 3 }) {
            Text(this.up ? '运行中' : '离线')
              .fontSize(17)
              .fontWeight(FontWeight.Bold)
              .fontColor(this.up ? COLOR_SUCCESS : this.palette().textMuted)
              .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
            Text(this.up ? '已运行 ' + this.uptime : '未连接到后端服务')
              .fontSize(12)
              .fontColor(this.palette().textSecondary)
              .maxLines(1)
              .textOverflow({ overflow: TextOverflow.Ellipsis })
          }
          .alignItems(HorizontalAlign.Start)
          .layoutWeight(1)
        }
        .width('100%')
        .alignItems(VerticalAlign.Center)
        .transition(TransitionEffect.OPACITY.combine(TransitionEffect.translate({ y: 12 })).animation({ duration: ANIM_ENTER, curve: Curve.EaseOut }))

        // 能力计数：图标 + 数字，只保留真正会变的两项（插件 / 工具）。
        //
        // 必须是**子组件**（@Prop 单向下发）而不是本组件里的 @Builder：
        // ArkUI 的 @Builder 按值传参是“快照”语义，父组件重渲染时
        // 不会用新值重跑 builder —— 实测：K_PLUGINS 已经是 35，
        // 但 @Builder 里画的还是首次的 0，永远显示 '-'。
        Row({ space: 10 }) {
          KpiTile({ icon: $r('app.media.ic_plug'), label: '插件', value: this.plugins, tint: COLOR_ACCENT })
          KpiTile({ icon: $r('app.media.ic_tool'), label: '工具', value: this.tools, tint: COLOR_CYAN })
        }
        .width('100%')
        .margin({ top: 14 })

        // 版本 + 「查看明细」提示，与 KPI 之间用分割线断开
        Row() {
          Text('版本')
            .fontSize(12)
            .fontColor(this.palette().textMuted)
          Column() {
            Text(this.version)
              .fontSize(12)
              .fontWeight(FontWeight.Medium)
              .fontColor(this.palette().textSecondary)
              .maxLines(1)
              .textOverflow({ overflow: TextOverflow.Ellipsis })
            // 内核名 + commit：光有版本号会分不清是哪个内核、哪次构建
            Text(this.buildSub)
              .fontSize(10)
              .fontColor(this.palette().textMuted)
              .maxLines(1)
              .textOverflow({ overflow: TextOverflow.Ellipsis })
              .visibility(this.buildSub.length > 0 ? Visibility.Visible : Visibility.None)
          }
          .alignItems(HorizontalAlign.Start)
          .layoutWeight(1)
          .margin({ left: 10 })
          Text('明细')
            .fontSize(12)
            .fontColor(this.palette().accent)
          Image($r('app.media.ic_chevron_right'))
            .width(14)
            .height(14)
            .fillColor(this.palette().accent)
            .draggable(false)
        }
        .width('100%')
        .alignItems(VerticalAlign.Center)
        .margin({ top: 14 })
        .padding({ top: 11 })
        .border({ width: { top: 1 }, color: this.palette().kvBorder })
      }
    }
    .width('100%')
    .padding(18)
    .borderRadius(RADIUS_LG)
    .backgroundColor(this.palette().bgCard)
    .border({ width: 1, color: this.palette().glassBorder })
    .alignItems(HorizontalAlign.Start)
    .margin({ bottom: 18 })
    .onClick(() => {
      const cb = this.onTap;
      if (cb !== undefined) {
        cb();
      }
    })
    }
  }

  @Builder
  kpiTile(icon: Resource, label: string, value: number, color: string) {
    Row({ space: 8 }) {
      Image(icon)
        .width(16)
        .height(16)
        .fillColor(color)
        .draggable(false)
      Column({ space: 1 }) {
        Text(value > 0 ? value.toString() : '-')
          .fontSize(17)
          .fontWeight(FontWeight.Bold)
          .fontColor(this.palette().textPrimary)
        Text(label)
          .fontSize(11)
          .fontColor(this.palette().textMuted)
      }
      .alignItems(HorizontalAlign.Start)
    }
    .layoutWeight(1)
    .padding({ left: 12, right: 12, top: 10, bottom: 10 })
    .borderRadius(RADIUS_SM)
    .backgroundColor(this.palette().bgHover)
    .alignItems(VerticalAlign.Center)
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }
}

/**
 * 能力计数小卡（插件 / 工具）。
 *
 * 单独成组件而非 @Builder：@Builder 的按值参数不会随父组件重渲染而刷新，
 * 数值会永远停在首次渲染的 0。@Prop 是单向下发，父组件因 @StorageProp
 * 变化重渲染时，子组件拿到新值并重绘。
 */
@Component
struct KpiTile {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop icon: Resource = $r('app.media.ic_plug');
  @Prop label: string = '';
  @Prop value: number = 0;
  @Prop tint: string = '';

  build() {
    Row({ space: 8 }) {
      Image(this.icon)
        .width(16)
        .height(16)
        .fillColor(this.tint)
        .draggable(false)
      Column({ space: 1 }) {
        Text(this.value > 0 ? this.value.toString() : '-')
          .fontSize(17)
          .fontWeight(FontWeight.Bold)
          .fontColor(this.palette().textPrimary)
        Text(this.label)
          .fontSize(11)
          .fontColor(this.palette().textMuted)
      }
      .alignItems(HorizontalAlign.Start)
    }
    .layoutWeight(1)
    .padding({ left: 12, right: 12, top: 10, bottom: 10 })
    .borderRadius(RADIUS_SM)
    .backgroundColor(this.palette().bgHover)
    .alignItems(VerticalAlign.Center)
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }
}

/**
 * 运行状态明细：设置页「运行状态」二级页面的内容。
 *
 * 服务负载三条容量条 + 分组明细卡（系统概览 / 内核 / 模型 / 记忆 / 运行时 / 工具）。
 * 分组数组不放 AppStorage（数组同步语义不可靠），改为订阅版本号 K_REV，
 * 版本变化时从 statusStore 重取一次快照。
 */
@Component
export struct StatusDetailContent {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @StorageProp(K_UP) private up: boolean = false;
  @StorageProp(K_AGENTS) private agents: number = 0;
  @StorageProp(K_PLUGINS) private plugins: number = 0;
  @StorageProp(K_TOOLS) private tools: number = 0;
  @StorageProp(K_ERR) private err: string = '';
  @StorageProp(K_LOADING) private loading: boolean = false;
  @StorageProp(K_REV) @Watch('onRevChanged') private rev: number = 0;
  @State groups: StatGroup[] = [];

  aboutToAppear(): void {
    this.groups = statusStore.getGroups();
  }

  private onRevChanged(): void {
    this.groups = statusStore.getGroups();
  }

  build() {
    Column() {
      if (this.err.length > 0) {
        Row() {
          Image($r('app.media.ic_error'))
            .width(16)
            .height(16)
            .fillColor(COLOR_ERROR)
            .draggable(false)
          Text(this.err)
            .fontSize(13)
            .fontColor(COLOR_ERROR)
            .layoutWeight(1)
            .margin({ left: 10 })
        }
        .width('100%')
        .padding(16)
        .borderRadius(RADIUS_LG)
        .backgroundColor(this.palette().bgCard)
        .border({ width: 1, color: this.palette().glassBorder })
        .alignItems(VerticalAlign.Top)
        .margin({ bottom: 14 })
        .transition(TransitionEffect.OPACITY.combine(TransitionEffect.translate({ y: 12 })).animation({ duration: ANIM_ENTER, curve: Curve.EaseOut }))
      }

      if (this.loading && this.groups.length === 0) {
        Row() {
          LoadingProgress()
            .width(28)
            .height(28)
            .color(this.palette().accent)
        }
        .width('100%')
        .justifyContent(FlexAlign.Center)
        .padding({ top: 40, bottom: 40 })
        .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
      }

      // 这里原本有一张"服务负载"卡，用两条容量条画插件/工具数量。
      // 后端并不存在"插件上限/工具上限"这种容量概念，分母是两者取大值，
      // 于是 31 个插件在 203 个工具旁边只剩一条短线 —— 读数没有意义。
      // 数量本身已在摘要卡上以图标+数字直观呈现，这里不再重复。

      // 运行态面板（阶段管道 + 中断队列）放在明细页最前：
      // 明细卡回答「内核有哪些东西、多少」，运行态回答「现在在干什么」，
      // 后者是进这个页面最先想看的。
      RuntimePanel()

      ForEach(this.groups, (g: StatGroup) => {
        Column() {
          Text(g.title)
            .fontSize(15)
            .fontWeight(FontWeight.Medium)
            .fontColor(this.palette().textPrimary)
            .margin({ bottom: 6 })
          ForEach(g.fields, (f: StatField, idx: number) => {
            Row() {
              Text(f.label)
                .fontSize(13)
                .fontColor(this.palette().textSecondary)
                .layoutWeight(1)
                .maxLines(1)
                .textOverflow({ overflow: TextOverflow.Ellipsis })
              Text(f.value.length > 0 ? f.value : '-')
                .fontSize(13)
                .fontColor(this.palette().textPrimary)
                .textAlign(TextAlign.End)
                .maxLines(1)
                .textOverflow({ overflow: TextOverflow.Ellipsis })
                .constraintSize({ maxWidth: 200 })
                .margin({ left: 16 })
            }
            .width('100%')
            .padding({ top: 9, bottom: 9 })
            .border({
              width: { bottom: idx < g.fields.length - 1 ? 1 : 0 },
              color: this.palette().kvBorder,
            })
          }, (f: StatField) => g.title + f.label)
        }
        .width('100%')
        .padding(18)
        .borderRadius(RADIUS_LG)
        .backgroundColor(this.palette().bgCard)
        .border({ width: 1, color: this.palette().glassBorder })
        .alignItems(HorizontalAlign.Start)
        .margin({ bottom: 14 })
        .transition(TransitionEffect.OPACITY.combine(TransitionEffect.translate({ y: 12 })).animation({ duration: ANIM_ENTER, curve: Curve.EaseOut }))
      }, (g: StatGroup) => g.title)
    }
    .width('100%')
    .alignItems(HorizontalAlign.Start)
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }
}
