import {
  ThemePalette, DARK_PALETTE, LIGHT_PALETTE,
  RADIUS_SM, RADIUS_MD, ANIM_FAST,
  COLOR_ACCENT, COLOR_EMERALD, COLOR_FROST_300,
} from '../common/Constants';
import { statusStore, RuntimeSnapshot, RuntimeQueue, K_REV } from '../common/StatusStore';
import {
  stageTrail, StageEvent, StageGroup, STAGE_GROUPS, phaseGroup,
  K_STAGE_PHASE, K_STAGE_REV,
} from '../common/StageTrail';

/** 四级中断 + 排队的配色：与 WebUI 总览同一套（级别色贯穿框头、槽位、描边） */
const LV_COLORS: string[] = ['#A3BE8C', '#4A90D9', '#0A59F7', '#FFA657', '#FF5C7A'];
/** 阶段管道里的「工具」格（循环格）：一轮内可能调几十次，只露最新一条 */
const TOOL_GROUP: number = 2;
/** 索引 = lv（0 排队 / 1 L1 / 2 L2 / 3 L3 / 4 L4） */
function lvColor(lv: number): string {
  if (lv < 0 || lv > 4) {
    return COLOR_ACCENT;
  }
  return LV_COLORS[lv];
}

/**
 * 运行态面板：阶段管道 + 中断队列。
 *
 * 与 WebUI / 桌面版同一套设计语言：**等大表框**。
 * 此前鸿蒙端完全没有运行态展示（「状态」Tab 已并入设置页 + 二级明细），
 * 这里补在二级明细页顶部。
 *
 * 两块数据来源不同：
 *  - 队列 / 计数：StatusStore 轮询 /runtime，靠 K_REV 通知；
 *  - 阶段 + 本轮轨迹：StageTrail 由 SSE 的 stage 事件喂入，靠 K_STAGE_REV 通知。
 */
@Component
export struct RuntimePanel {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  /** 宽屏（平板/折叠）5 框一行；手机 3+2。窄屏硬塞 5 框会把标签挤成省略号。 */
  @StorageProp('isWideScreen') private isWide: boolean = false;
  @StorageProp(K_REV) @Watch('onStatusRev') private statusRev: number = 0;
  @StorageProp(K_STAGE_REV) @Watch('onStageRev') private stageRev: number = 0;
  @StorageProp(K_STAGE_PHASE) private phase: string = '';
  @State private snap: RuntimeSnapshot | undefined = undefined;
  /** 格槽下标：ArkUI 的 ForEach 遍历的是数组，所以把「几格」摊成下标数组 */
  @State private slotIdx: number[] = [];
  @State private trail: StageEvent[] = [];

  aboutToAppear(): void {
    this.pull();
  }

  private onStatusRev(): void {
    this.pull();
  }

  private onStageRev(): void {
    this.trail = stageTrail.snapshot();
  }

  private pull(): void {
    const s: RuntimeSnapshot | undefined = statusStore.getRuntime();
    this.snap = s;
    if (s !== undefined) {
      const idx: number[] = [];
      for (let i = 0; i < s.slots; i++) {
        idx.push(i);
      }
      this.slotIdx = idx;
    } else {
      this.slotIdx = [];
    }
    this.trail = stageTrail.snapshot();
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  /** 该阶段本轮发生的事件 */
  private eventsOf(group: number): StageEvent[] {
    const out: StageEvent[] = [];
    for (let i = 0; i < this.trail.length; i++) {
      const e: StageEvent = this.trail[i];
      if (e.group === group) {
        out.push(e);
      }
    }
    return out;
  }

  /** 该阶段本轮事件的**最后一条**（最新）。没有则 undefined。 */
  private latestEvent(group: number): StageEvent | undefined {
    let out: StageEvent | undefined = undefined;
    for (let i = 0; i < this.trail.length; i++) {
      const e: StageEvent = this.trail[i];
      if (e.group === group) {
        out = e;
      }
    }
    return out;
  }

  /** 该阶段本轮事件条数合计（同一工具连调会累加到 count 上） */
  private totalCount(group: number): number {
    let n: number = 0;
    for (let i = 0; i < this.trail.length; i++) {
      const e: StageEvent = this.trail[i];
      if (e.group === group) {
        n += e.count;
      }
    }
    return n;
  }

  @Builder
  tile(label: string, value: string, warn: boolean) {
    Column({ space: 2 }) {
      Text(value)
        .fontSize(20)
        .fontWeight(FontWeight.Bold)
        .fontColor(warn ? this.palette().accent : this.palette().textPrimary)
        .maxLines(1)
      Text(label)
        .fontSize(11)
        .fontColor(this.palette().textMuted)
        .maxLines(1)
    }
    .layoutWeight(1)
    .alignItems(HorizontalAlign.Start)
    .padding({ left: 10, right: 10, top: 9, bottom: 9 })
    .borderRadius(RADIUS_SM)
    .backgroundColor(this.palette().bgHover)
  }

  /** 阶段管道的一格 */
  @Builder
  stageCell(g: StageGroup, active: boolean) {
    Column({ space: 6 }) {
      Row({ space: 5 }) {
        // 用一个几何圆点而非图标：本项目没有为「阶段」准备的图形资源，
        // 而猜 sys.media.* 名称会直接编译不过；也不允许用 emoji 充当图标。
        // 不用 Circle().fill()：那是 SDK 26 起的 API，本工程兼容版本是 6.1.1(24)。
        Row()
          .width(7)
          .height(7)
          .borderRadius(4)
          .backgroundColor(active ? this.palette().accent : this.palette().textMuted)
        Text(g.label)
          .fontSize(11)
          .fontWeight(FontWeight.Bold)
          .fontColor(active ? this.palette().accent : this.palette().textSecondary)
          .maxLines(1)
          .textOverflow({ overflow: TextOverflow.Ellipsis })
      }
      .width('100%')

      // 本阶段本轮发生的事件；没有就给一个弱化的「无」，让框不空着。
      //
      // 「工具」格是循环格：一轮里可能调几十次工具/输出通道。把每一次都追加成
      // 一行，这格会被撑成长条，反而看不出「现在在调什么」。所以它只保留**最新
      // 一条**，右侧给本轮累计次数（与 WebUI/桌面版同一口径）。
      Column({ space: 3 }) {
        if (g.group === TOOL_GROUP) {
          if (this.eventsOf(g.group).length > 0) {
            Row({ space: 3 }) {
              Text(this.latestEvent(g.group)?.short ?? '')
                .fontSize(9)
                .fontColor(this.latestEvent(g.group)?.kind === 'output' ? this.palette().accent
                  : (this.latestEvent(g.group)?.kind === 'tool' ? COLOR_FROST_300 : this.palette().textSecondary))
                .maxLines(1)
                .textOverflow({ overflow: TextOverflow.Ellipsis })
                .layoutWeight(1)
              Text('x' + this.totalCount(g.group).toString())
                .fontSize(9)
                .fontColor(this.palette().textMuted)
            }
            .width('100%')
          } else {
            Text('无')
              .fontSize(9)
              .fontColor(this.palette().textMuted)
              .opacity(0.5)
          }
        } else {
          ForEach(this.eventsOf(g.group), (e: StageEvent) => {
            Row({ space: 3 }) {
              Text(e.short)
                .fontSize(9)
                .fontColor(e.kind === 'output' ? this.palette().accent
                  : (e.kind === 'tool' ? COLOR_FROST_300 : this.palette().textSecondary))
                .maxLines(1)
                .textOverflow({ overflow: TextOverflow.Ellipsis })
              if (e.count > 1) {
                Text('x' + e.count.toString())
                  .fontSize(9)
                  .fontColor(this.palette().textMuted)
              }
            }
            .width('100%')
          }, (e: StageEvent, i: number) => i.toString() + ':' + e.short)

          if (this.eventsOf(g.group).length === 0) {
            Text('无')
              .fontSize(9)
              .fontColor(this.palette().textMuted)
              .opacity(0.5)
          }
        }
      }
      .width('100%')
      .alignItems(HorizontalAlign.Start)
    }
    .layoutWeight(1)
    .alignItems(HorizontalAlign.Start)
    .padding({ left: 8, right: 8, top: 9, bottom: 9 })
    .borderRadius(RADIUS_SM)
    .backgroundColor(active ? this.palette().accentBg : this.palette().bgHover)
    .border({ width: 1, color: active ? this.palette().accent : this.palette().glassBorder })
    .animation({ duration: ANIM_FAST, curve: Curve.EaseOut })
  }

  /** 中断队列的一格 */
  @Builder
  queueCell(q: RuntimeQueue) {
    Column({ space: 5 }) {
      Row({ space: 4 }) {
        Text(q.name)
          .fontSize(13)
          .fontWeight(FontWeight.Bold)
          .fontColor(lvColor(q.lv))
          .maxLines(1)
        Text(q.desc)
          .fontSize(9)
          .fontColor(this.palette().textMuted)
          .maxLines(1)
          .textOverflow({ overflow: TextOverflow.Ellipsis })
      }
      .width('100%')
      .alignItems(VerticalAlign.Bottom)

      Text(q.depth.toString())
        .fontSize(20)
        .fontWeight(FontWeight.Bold)
        .fontColor(this.palette().textPrimary)
        .maxLines(1)

      // 格槽：固定可见的「车位」。用进度条时深度为 0 宽度就是 0，
      // 整格只剩文字，看上去就是「这块空着」。
      Row({ space: 2 }) {
        ForEach(this.slotIdx, (i: number) => {
          Row()
            .layoutWeight(1)
            .height(11)
            .borderRadius(2)
            .backgroundColor(i < q.depth ? lvColor(q.lv) : this.palette().bgHover)
        }, (i: number) => q.lv.toString() + '-' + i.toString())
      }
      .width('100%')

      if (q.lv > 0) {
        Text(q.registered.toString() + ' 登记 · ' + q.preempted.toString() + ' 抢占')
          .fontSize(9)
          .fontColor(this.palette().textMuted)
          .maxLines(1)
      } else {
        Text('无级别')
          .fontSize(9)
          .fontColor(this.palette().textMuted)
          .maxLines(1)
      }
    }
    .layoutWeight(1)
    .alignItems(HorizontalAlign.Start)
    .padding({ left: 8, right: 8, top: 9, bottom: 9 })
    .borderRadius(RADIUS_SM)
    .backgroundColor(this.palette().bgHover)
    .border({
      width: 1,
      color: q.depth > 0 ? lvColor(q.lv) : this.palette().glassBorder,
      style: q.lv === 0 ? BorderStyle.Dashed : BorderStyle.Solid,
    })
    .animation({ duration: ANIM_FAST, curve: Curve.EaseOut })
  }

  build() {
    Column() {
      Text('运行态')
        .fontSize(15)
        .fontWeight(FontWeight.Medium)
        .fontColor(this.palette().textPrimary)
        .margin({ bottom: 8 })

      if (this.snap === undefined) {
        Text('运行态数据不可用')
          .fontSize(12)
          .fontColor(this.palette().textMuted)
          .padding({ top: 4, bottom: 4 })
      } else {
        // 四个数字块
        Row({ space: 8 }) {
          this.tile('排队', this.snap.ready.toString(), this.snap.ready > 0)
          this.tile('中断', this.snap.pending.toString(), this.snap.pending > 0)
          this.tile('栈', this.snap.stack.toString() + '/' + this.snap.maxStack.toString(),
            this.snap.stack > 0)
          this.tile('子代理', this.snap.subagents.toString(), false)
        }
        .width('100%')

        // 阶段管道：等大表框，事件落在所属阶段那一格
        Text('阶段管道')
          .fontSize(12)
          .fontWeight(FontWeight.Medium)
          .fontColor(this.palette().textSecondary)
          .margin({ top: 14, bottom: 6 })
        if (this.isWide) {
          Row({ space: 6 }) {
            ForEach(STAGE_GROUPS, (g: StageGroup) => {
              this.stageCell(g, phaseGroup(this.phase) === g.group)
            }, (g: StageGroup) => 'w' + g.group.toString())
          }
          .width('100%')
          .alignItems(VerticalAlign.Top)
        } else {
          Row({ space: 6 }) {
            ForEach(STAGE_GROUPS.slice(0, 3), (g: StageGroup) => {
              this.stageCell(g, phaseGroup(this.phase) === g.group)
            }, (g: StageGroup) => 'n0' + g.group.toString())
          }
          .width('100%')
          .alignItems(VerticalAlign.Top)
          Row({ space: 6 }) {
            ForEach(STAGE_GROUPS.slice(3), (g: StageGroup) => {
              this.stageCell(g, phaseGroup(this.phase) === g.group)
            }, (g: StageGroup) => 'n1' + g.group.toString())
            // 占位：第二行只有 2 格，补一格位置让框宽与第一行对齐
            Row().layoutWeight(1)
          }
          .width('100%')
          .alignItems(VerticalAlign.Top)
          .margin({ top: 6 })
        }

        // 中断队列：五个等大表框（L4/L3/L2/L1 + 排队）
        Text('队列')
          .fontSize(12)
          .fontWeight(FontWeight.Medium)
          .fontColor(this.palette().textSecondary)
          .margin({ top: 14, bottom: 6 })
        if (this.isWide) {
          Row({ space: 6 }) {
            ForEach(this.snap.queues, (q: RuntimeQueue) => {
              this.queueCell(q)
            }, (q: RuntimeQueue) => 'wq' + q.lv.toString())
          }
          .width('100%')
          .alignItems(VerticalAlign.Top)
        } else {
          Row({ space: 6 }) {
            ForEach(this.snap.queues.slice(0, 3), (q: RuntimeQueue) => {
              this.queueCell(q)
            }, (q: RuntimeQueue) => 'nq0' + q.lv.toString())
          }
          .width('100%')
          .alignItems(VerticalAlign.Top)
          Row({ space: 6 }) {
            ForEach(this.snap.queues.slice(3), (q: RuntimeQueue) => {
              this.queueCell(q)
            }, (q: RuntimeQueue) => 'nq1' + q.lv.toString())
            Row().layoutWeight(1)
          }
          .width('100%')
          .alignItems(VerticalAlign.Top)
          .margin({ top: 6 })
        }
      }
    }
    .width('100%')
    .alignItems(HorizontalAlign.Start)
    .padding(16)
    .margin({ bottom: 14 })
    .borderRadius(RADIUS_MD)
    .backgroundColor(this.palette().bgCard)
    .border({ width: 1, color: this.palette().glassBorder })
  }
}
