/**
 * 阶段轨迹（单例）：本轮对话在七个内核阶段里真实发生过什么。
 *
 * 由 ChatSse 在收到 `stage` 事件时喂入，运行态面板读取。
 * 为什么不放进 StatusStore：轨迹来自 SSE 流、与 /status、/kernel 的轮询无关，
 * 两者的生命周期和失败模式都不一样，混在一个数据源里会互相拖累
 * （SSE 断连不该让状态卡变空，状态轮询失败也不该清掉轨迹）。
 *
 * 七阶段归并成五格（与内核 sdk.Stage 的顺序一致）：
 * 一轮里工具调用会反复回到「行动后」，线性滑块本身就是错的表述，
 * 所以画成 输入 → 行动 ⇄(工具) → 输出 → 结束，工具那格带循环标记。
 */

/** 一个阶段组 */
export interface StageGroup {
  /** 0 输入 / 1 行动 / 2 工具 / 3 输出 / 4 结束 */
  group: number;
  label: string;
  en: string;
}

/** 轨迹里的一条事件 */
export interface StageEvent {
  group: number;
  /** 'stage' | 'tool' | 'output' */
  kind: string;
  label: string;
  short: string;
  count: number;
}

/** 阶段组定义，索引即 group */
export const STAGE_GROUPS: StageGroup[] = [
  { group: 0, label: '输入', en: 'in' },
  { group: 1, label: '行动', en: 'act' },
  { group: 2, label: '工具', en: 'tool' },
  { group: 3, label: '输出', en: 'out' },
  { group: 4, label: '结束', en: 'done' },
];

/** 当前阶段（SSE 驱动）。空串 = 空闲。 */
export const K_STAGE_PHASE: string = 'stagePhase';
/** 轨迹版本号：数组不进 AppStorage，靠它触发订阅组件重取快照 */
export const K_STAGE_REV: string = 'stageRev';

/** 阶段停留多久后回「空闲」——否则会留下一个永远停在 after_output 的假状态 */
const IDLE_AFTER_MS: number = 2500;
const MAX_ITEMS: number = 24;

/** rtShortTool 把 `qq_get_message` / `output_send__qq` 压成尾段短名 */export function shortTool(name: string): string {
  let n: string = name;
  const i: number = n.lastIndexOf('__');
  if (i >= 0) {
    n = n.substring(i + 2);
  }
  return n.length > 14 ? n.substring(0, 13) + '…' : n;
}

class StageTrail {
  private items: StageEvent[] = [];
  private timerId: number = -1;

  init(): void {
    AppStorage.setOrCreate<string>(K_STAGE_PHASE, '');
    AppStorage.setOrCreate<number>(K_STAGE_REV, 0);
  }

  /** 只给订阅组件读；调用方不要持有它 */
  snapshot(): StageEvent[] {
    return this.items;
  }

  currentPhase(): string {
    return AppStorage.get<string>(K_STAGE_PHASE) ?? '';
  }

  /** 按阶段分组取条目 */
  eventsOf(group: number): StageEvent[] {
    const out: StageEvent[] = [];
    for (let i = 0; i < this.items.length; i++) {
      const e: StageEvent = this.items[i];
      if (e.group === group) {
        out.push(e);
      }
    }
    return out;
  }

  /**
   * 收到一条 stage 事件。phase 取值与内核一致：
   * on_input / pre_action / post_action / before_toolcall / after_toolcall /
   * before_output / after_output。
   */
  onStage(phase: string, tool: string): void {
    if (phase.length === 0) {
      return;
    }
    if (phase === 'on_input') {
      // 新的一轮：清空上一轮的轨迹
      this.items = [];
      this.push(0, 'stage', '输入', '输入');
    } else if (phase === 'pre_action') {
      this.push(1, 'stage', '组装上下文并思考', '思考');
    } else if (phase === 'before_toolcall' && tool.length > 0) {
      // output_* 是输出通道工具，与普通工具用不同配色区分
      const kind: string = tool.indexOf('output_') === 0 ? 'output' : 'tool';
      this.push(2, kind, tool, shortTool(tool));
    } else if (phase === 'before_output') {
      this.push(3, 'stage', '生成回复', '生成');
    } else if (phase === 'after_output') {
      this.push(4, 'stage', '本轮完成', '完成');
    } else {
      // post_action / after_toolcall 不单独记：它们与相邻格重复，
      // 逐条记会把轨迹刷成噪音。
      AppStorage.set<string>(K_STAGE_PHASE, phase);
      this.armIdleTimer();
      return;
    }
    AppStorage.set<string>(K_STAGE_PHASE, phase);
    this.armIdleTimer();
    this.bump();
  }

  private push(group: number, kind: string, label: string, short: string): void {
    // 同一阶段重复出现的同一条（如同一工具连调 3 次）只累加计数，不刷屏
    const n: number = this.items.length;
    if (n > 0) {
      const last: StageEvent = this.items[n - 1];
      if (last.group === group && last.kind === kind && last.short === short) {
        last.count = last.count + 1;
        return;
      }
    }
    this.items.push({ group: group, kind: kind, label: label, short: short, count: 1 });
    if (this.items.length > MAX_ITEMS) {
      this.items.shift();
    }
  }

  private armIdleTimer(): void {
    if (this.timerId !== -1) {
      clearTimeout(this.timerId);
    }
    this.timerId = setTimeout(() => {
      this.timerId = -1;
      AppStorage.set<string>(K_STAGE_PHASE, '');
      this.bump();
    }, IDLE_AFTER_MS);
  }

  private bump(): void {
    const cur: number = AppStorage.get<number>(K_STAGE_REV) ?? 0;
    AppStorage.set<number>(K_STAGE_REV, cur + 1);
  }
}

export const stageTrail: StageTrail = new StageTrail();

/**
 * 内核的七个阶段归并到五个展示格。顺序与 sdk.Stage 一致，
 * 所以「当前阶段」直接看返回值是不是当前格。空闲返回 -1。
 */
export function phaseGroup(phase: string): number {
  if (phase === 'on_input') {
    return 0;
  }
  if (phase === 'pre_action' || phase === 'post_action') {
    return 1;
  }
  if (phase === 'before_toolcall' || phase === 'after_toolcall') {
    return 2;
  }
  if (phase === 'before_output') {
    return 3;
  }
  if (phase === 'after_output') {
    return 4;
  }
  return -1;
}
