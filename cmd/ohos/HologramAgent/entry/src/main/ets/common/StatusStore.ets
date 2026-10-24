import { apiClient } from './ApiClient';
import { userMessage, noConnectionMessage } from './UserError';

/**
 * 运行状态数据源（单例）。
 *
 * 状态页已并入设置页：设置一级页顶部嵌一张摘要卡，明细走二级页。
 * 摘要卡与明细页是两个独立组件，但必须显示同一份数据、只请求一次，
 * 所以把请求与解析收拢到这里，标量通过 AppStorage 广播给两边。
 *
 * 字段口径与 WebGUI 的 概览/内核 两页一致：
 *  - GET /status  → status / version / startedAt / agents
 *  - GET /kernel  → agent_id / plugins / tools / llm / memory / documents /
 *                   text_memory / knowledge / runtime
 * 后端不提供 token 用量、配额、错误列表，所以这里也没有。
 */

/** 一行明细 */
export interface StatField {
  label: string;
  value: string;
}

/** 一组明细卡 */
export interface StatGroup {
  title: string;
  fields: StatField[];
}

// ===== 运行态快照（/runtime）=====
//
// 与 StatGroup 的分工：明细卡回答「内核有哪些东西、多少」，
// 运行态回答「现在在干什么」——四级中断队列积压多少、有几个驻留子。
// 两者数据源不同（/kernel vs /runtime），所以分开取、分开存。

/** 一条队列（四级中断之一，或排队队列） */
export interface RuntimeQueue {
  /** 4/3/2/1；0 表示排队队列（无级别） */
  lv: number;
  name: string;
  desc: string;
  depth: number;
  registered: number;
  preempted: number;
}

/** 一次刷新的运行态快照 */
export interface RuntimeSnapshot {
  /** 排队队列深度（无级别，纯 FIFO） */
  ready: number;
  pending: number;
  stack: number;
  maxStack: number;
  subagents: number;
  /** 格槽数（按全场最大深度缩放，至少 5） */
  slots: number;
  /** 五条队列：L4/L3/L2/L1 + 排队 */
  queues: RuntimeQueue[];
}

interface LevelDef {
  lv: number;
  name: string;
  desc: string;
}

/** 四级中断的定义（顺序即 L4→L1） */
const LEVEL_DEFS: LevelDef[] = [
  { lv: 4, name: 'L4', desc: '内核独占' },
  { lv: 3, name: 'L3', desc: '交互' },
  { lv: 2, name: 'L2', desc: '消息' },
  { lv: 1, name: 'L1', desc: '后台' },
];

/** JSON 数组里按下标取数：越界/类型不符都当 0 */
function numAt(arr: Object[] | undefined, i: number): number {
  if (arr === undefined || arr === null || i < 0 || i >= arr.length) {
    return 0;
  }
  const v: number = arr[i] as number;
  return isNaN(v) ? 0 : v;
}

/** 格槽数：按全场最大深度缩放，至少 5 格（0 时也要有可见形状）、最多 16 格 */
function clampSlots(maxQ: number): number {
  if (maxQ < 5) {
    return 5;
  }
  return maxQ > 16 ? 16 : maxQ;
}

// ===== AppStorage 键：摘要卡与明细页共用 =====
export const K_UP: string = 'statUp';
export const K_VERSION: string = 'statVersion';
export const K_STARTED: string = 'statStartedAt';
export const K_AGENTS: string = 'statAgents';
export const K_PLUGINS: string = 'statPlugins';
export const K_TOOLS: string = 'statTools';
export const K_ERR: string = 'statErr';
export const K_LOADING: string = 'statLoading';
export const K_REV: string = 'statRev';
/** 内核身份副行：内核名 · commit。版本号光有一个号码分不清是哪个内核、哪次构建。 */
export const K_BUILD: string = 'statBuildSub';

class StatusStore {
  /** 明细分组：只有明细页读它，不进 AppStorage（数组同步语义太脆） */
  private groups: StatGroup[] = [];
  /** 运行态快照：同上，靠 K_REV 触发订阅组件重取 */
  private runtimeSnapshot: RuntimeSnapshot | undefined = undefined;

  init(): void {
    AppStorage.setOrCreate<boolean>(K_UP, false);
    AppStorage.setOrCreate<string>(K_VERSION, '-');
    AppStorage.setOrCreate<string>(K_STARTED, '');
    AppStorage.setOrCreate<number>(K_AGENTS, 0);
    AppStorage.setOrCreate<number>(K_PLUGINS, 0);
    AppStorage.setOrCreate<number>(K_TOOLS, 0);
    AppStorage.setOrCreate<string>(K_ERR, '');
    AppStorage.setOrCreate<boolean>(K_LOADING, false);
    AppStorage.setOrCreate<number>(K_REV, 0);
    AppStorage.setOrCreate<string>(K_BUILD, '');
  }

  getGroups(): StatGroup[] {
    return this.groups;
  }

  getRuntime(): RuntimeSnapshot | undefined {
    return this.runtimeSnapshot;
  }

  isUp(): boolean {
    return AppStorage.get<boolean>(K_UP) ?? false;
  }

  async refresh(): Promise<void> {
    if (!apiClient.hasConnection()) {
      this.fail(noConnectionMessage());
      return;
    }
    // setOrCreate 只负责首次建键，键已存在时不会覆盖旧值。
    // init() 已把所有键种好，刷新阶段必须用 set，否则摘要卡会永远停在
    // 版本 "-"、插件 0 的初始状态。
    AppStorage.set<boolean>(K_LOADING, true);
    AppStorage.set<string>(K_ERR, '');
    try {
      const resp = await apiClient.getWithTimeout('/status', 8000);
      const obj: Record<string, Object> = JSON.parse(resp.body) as Record<string, Object>;

      const version: string = obj['version'] as string ?? '未知';
      const status: string = obj['status'] as string ?? 'unknown';
      const uptimeStr: string = obj['uptime'] as string ?? '';
      const agents: number = obj['agents'] as number ?? 0;
      const startedAt: string = obj['startedAt'] as string ?? '';

      AppStorage.set<boolean>(K_UP, true);
      AppStorage.set<string>(K_VERSION, version);
      AppStorage.set<number>(K_AGENTS, agents);
      AppStorage.set<string>(K_STARTED, startedAt);

      const systemFields: StatField[] = [
        { label: '版本', value: version },
        { label: '运行状态', value: statusText(status) },
        { label: '运行时长', value: uptimeStr.length > 0 ? uptimeStr : '-' },
      ];
      if (startedAt.length > 0) {
        systemFields.push({ label: '启动时间', value: formatTime(startedAt) });
      }

      const groups: StatGroup[] = [
        { title: '系统概览', fields: systemFields },
      ];

      await this.collectKernel(groups);
      await this.collectRuntime();
      this.groups = groups;
      this.bump();
    } catch (e) {
      this.fail(userMessage('status.refresh', e));
    }
    AppStorage.set<boolean>(K_LOADING, false);
  }

  /** /kernel 可能不存在（旧后端），失败不影响 /status 已取到的部分 */
  private async collectKernel(groups: StatGroup[]): Promise<void> {
    try {
      const kResp = await apiClient.getWithTimeout('/kernel', 8000);
      const k: Record<string, Object> = JSON.parse(kResp.body) as Record<string, Object>;

      const agentId: string = k['agent_id'] as string ?? 'main';
      const startTime: string = k['start_time'] as string ?? '';
      const pluginsArr: Object[] | undefined = k['plugins'] as Object[];
      const toolsArr: Object[] | undefined = k['tools'] as Object[];
      const pluginCount: number = pluginsArr !== undefined ? pluginsArr.length : 0;
      const toolCount: number = toolsArr !== undefined ? toolsArr.length : 0;

      AppStorage.set<number>(K_PLUGINS, pluginCount);
      AppStorage.set<number>(K_TOOLS, toolCount);

      const kernelFields: StatField[] = [
        { label: 'Agent ID', value: agentId },
        { label: '已加载插件', value: pluginCount.toString() },
        { label: '工具数', value: toolCount.toString() },
      ];
      if (startTime.length > 0) {
        kernelFields.push({ label: '内核启动', value: formatTime(startTime) });
      }

      // 构建身份：版本 / commit / SDK 兼容都取自 /kernel 的 build（-ldflags 注入）。
      // 为何不能只用 /status 的 version：那里只有一个版本号，分不清是哪个内核、
      // 哪次构建；许可标识更是完全没有。
      const build: Record<string, Object> | undefined = k['build'] as Record<string, Object>;
      let lic: string = '';
      let licURL: string = '';
      let srcURL: string = '';
      if (build !== undefined && build !== null) {
        const bVer: string = build['version'] as string ?? '';
        const bCommit: string = build['commit'] as string ?? '';
        const bName: string = build['kernel_name'] as string ?? 'HologramAgent';
        const bSdk: string = build['sdk_compatible'] as string ?? '';
        const bTime: string = build['build_time'] as string ?? '';
        lic = build['license'] as string ?? '';
        licURL = build['license_url'] as string ?? '';
        srcURL = build['source_url'] as string ?? '';
        if (bVer.length > 0) {
          AppStorage.set<string>(K_VERSION, 'v' + bVer);
          kernelFields.push({ label: '内核版本', value: 'v' + bVer });
        }
        let sub: string = bName;
        if (bCommit.length > 0 && bCommit !== 'unknown') {
          sub = sub + ' · ' + bCommit.substring(0, 7);
        }
        AppStorage.set<string>(K_BUILD, sub);
        if (bCommit.length > 0) {
          kernelFields.push({ label: 'Commit', value: bCommit });
        }
        if (bSdk.length > 0) {
          kernelFields.push({ label: 'SDK 兼容', value: bSdk });
        }
        if (bTime.length > 0 && bTime !== 'unknown') {
          kernelFields.push({ label: '构建时间', value: formatTime(bTime) });
        }
      }
      groups.push({ title: '内核', fields: kernelFields });

      // 开源许可：只给一个源码链接、不写协议名，使用者看不出这受什么许可约束，
      // 也看不出网络服务场景下的 §13 义务。
      const legalFields: StatField[] = [];
      if (lic.length > 0) {
        legalFields.push({ label: '许可协议', value: lic });
      }
      if (licURL.length > 0) {
        legalFields.push({ label: '协议全文', value: licURL });
      }
      if (srcURL.length > 0) {
        legalFields.push({ label: '源码仓库', value: srcURL });
      }
      if (legalFields.length > 0) {
        if (lic.toUpperCase().indexOf('AGPL') >= 0) {
          legalFields.push({
            label: '网络条款',
            value: '把修改后的版本作为网络服务对外提供时，必须向使用者提供取得对应源码的途径（§13）',
          });
        }
        groups.push({ title: '开源许可', fields: legalFields });
      }

      // LLM：provider / 可用源 / 是否可用
      const llm: Record<string, Object> | undefined = k['llm'] as Record<string, Object>;
      if (llm !== undefined && llm !== null) {
        const provider: string = llm['provider'] as string ?? '';
        const sources: number = llm['sources'] as number ?? 0;
        const available: boolean = llm['available'] as boolean ?? false;
        groups.push({
          title: '模型',
          fields: [
            { label: 'Provider', value: provider.length > 0 ? provider : '未配置' },
            { label: '可用源', value: sources.toString() },
            { label: '状态', value: available ? '运行中' : '不可用' },
          ],
        });
      }

      // 记忆：图 / 文档 / 文本 / 知识库
      const memFields: StatField[] = [];
      const mem: Record<string, Object> | undefined = k['memory'] as Record<string, Object>;
      if (mem !== undefined && mem !== null) {
        const ok: boolean = mem['available'] as boolean ?? false;
        const ent: number = mem['entity_count'] as number ?? 0;
        const rel: number = mem['relation_count'] as number ?? 0;
        memFields.push({
          label: '图记忆',
          value: ok ? ent.toString() + ' 实体 · ' + rel.toString() + ' 关系' : '未初始化',
        });
      }
      const docs: Record<string, Object> | undefined = k['documents'] as Record<string, Object>;
      if (docs !== undefined && docs !== null) {
        const ok: boolean = docs['available'] as boolean ?? false;
        const n: number = docs['doc_count'] as number ?? 0;
        memFields.push({ label: '文档记忆', value: ok ? n.toString() + ' 文档' : '未初始化' });
      }
      const tm: Record<string, Object> | undefined = k['text_memory'] as Record<string, Object>;
      if (tm !== undefined && tm !== null) {
        const ok: boolean = tm['available'] as boolean ?? false;
        const n: number = tm['file_count'] as number ?? 0;
        memFields.push({ label: '文本记忆', value: ok ? n.toString() + ' 文件' : '未初始化' });
      }
      const kb: Record<string, Object> | undefined = k['knowledge'] as Record<string, Object>;
      if (kb !== undefined && kb !== null) {
        const ok: boolean = kb['available'] as boolean ?? false;
        const n: number = kb['item_count'] as number ?? 0;
        memFields.push({ label: '知识库', value: ok ? n.toString() + ' 项' : '未初始化' });
      }
      if (memFields.length > 0) {
        groups.push({ title: '记忆', fields: memFields });
      }

      // 运行时：goroutine / 内存 / Go 版本
      const rt: Record<string, Object> | undefined = k['runtime'] as Record<string, Object>;
      if (rt !== undefined && rt !== null) {
        const g: number = rt['goroutines'] as number ?? 0;
        const mb: number = rt['memory_mb'] as number ?? 0;
        const gov: string = rt['go_version'] as string ?? '';
        groups.push({
          title: '运行时',
          fields: [
            { label: 'Goroutines', value: g.toString() },
            { label: '内存占用', value: mb.toString() + ' MB' },
            { label: 'Go 版本', value: gov.length > 0 ? gov : '-' },
          ],
        });
      }

      // 工具清单：名称 → 归属插件
      if (toolsArr !== undefined && toolsArr.length > 0) {
        const toolFields: StatField[] = [];
        for (let i = 0; i < toolsArr.length; i++) {
          const t: Record<string, Object> = toolsArr[i] as Record<string, Object>;
          const name: string = t['name'] as string ?? '';
          const plugin: string = t['plugin'] as string ?? '';
          if (name.length > 0) {
            toolFields.push({ label: name, value: plugin });
          }
        }
        if (toolFields.length > 0) {
          groups.push({ title: '可用工具', fields: toolFields });
        }
      }
    } catch (e) {
      // /kernel 不可用时只保留 /status 的概览分组
    }
  }

  /**
   * 取运行态快照（/runtime）。失败不影响已经取到的明细分组：
   * 旧后端可能没有这个端点，那时面板显示「运行态数据不可用」即可。
   */
  private async collectRuntime(): Promise<void> {
    try {
      const resp = await apiClient.getWithTimeout('/runtime', 8000);
      const r: Record<string, Object> = JSON.parse(resp.body) as Record<string, Object>;

      const sc: Record<string, Object> | undefined = r['scheduler'] as Record<string, Object>;
      const residents: Object[] | undefined = r['residents'] as Object[];

      let ready: number = 0;
      let pending: number = 0;
      let stack: number = 0;
      let maxStack: number = 4;
      let qArr: Object[] | undefined = undefined;
      let regArr: Object[] | undefined = undefined;
      let preArr: Object[] | undefined = undefined;
      if (sc !== undefined && sc !== null) {
        ready = sc['ready_queue_depth'] as number ?? 0;
        pending = sc['pending_interrupts'] as number ?? 0;
        stack = sc['suspend_stack'] as number ?? 0;
        maxStack = sc['max_suspend_depth'] as number ?? 4;
        qArr = sc['interrupt_queues'] as Object[];
        regArr = sc['interrupts_by_level'] as Object[];
        preArr = sc['preempts_by_level'] as Object[];
      }

      // 四级语义引用内核的定义（internal/agent/core/scheduler.go），
      // 前端只负责把它们画出来，不自己起名字。
      const queues: RuntimeQueue[] = [];
      let maxQ: number = 1;
      for (let i = 0; i < LEVEL_DEFS.length; i++) {
        const d: LevelDef = LEVEL_DEFS[i];
        const depth: number = numAt(qArr, d.lv);
        if (depth > maxQ) {
          maxQ = depth;
        }
        queues.push({
          lv: d.lv,
          name: d.name,
          desc: d.desc,
          depth: depth,
          registered: numAt(regArr, d.lv),
          preempted: numAt(preArr, d.lv),
        });
      }
      if (ready > maxQ) {
        maxQ = ready;
      }
      // 第五条：排队队列。它不是优先级，而是另一**类别**（排队 vs 中断），
      // 所以 lv 用 0 标记「无级别」。
      queues.push({
        lv: 0,
        name: '排队',
        desc: 'FIFO',
        depth: ready,
        registered: 0,
        preempted: 0,
      });

      const subagents: number = residents !== undefined && residents !== null
        ? residents.length : 0;

      this.runtimeSnapshot = {
        ready: ready,
        pending: pending,
        stack: stack,
        maxStack: maxStack,
        subagents: subagents,
        slots: clampSlots(maxQ),
        queues: queues,
      };
    } catch (e) {
      // /runtime 不可用（旧后端）：保留上一次快照，不清空
    }
  }

  private fail(msg: string): void {
    AppStorage.set<string>(K_ERR, msg);
    AppStorage.set<boolean>(K_UP, false);
    AppStorage.set<boolean>(K_LOADING, false);
    this.groups = [];
    this.runtimeSnapshot = undefined;
    this.bump();
  }

  /** 明细数组不进 AppStorage，用一个自增版本号触发订阅组件重取 */
  private bump(): void {
    const cur: number = AppStorage.get<number>(K_REV) ?? 0;
    AppStorage.set<number>(K_REV, cur + 1);
  }
}

export const statusStore: StatusStore = new StatusStore();

export function statusText(s: string): string {
  if (s === 'running') {
    return '运行中';
  }
  if (s === 'stopped') {
    return '已停止';
  }
  if (s === 'starting') {
    return '启动中';
  }
  return s;
}

export function formatTime(iso: string): string {
  const t: number = new Date(iso).getTime();
  if (isNaN(t) || t <= 0) {
    return iso;
  }
  const d = new Date(t);
  return d.getFullYear().toString() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) +
    ' ' + pad2(d.getHours()) + ':' + pad2(d.getMinutes());
}

/**
 * 紧凑运行时长：环心里只有 96vp 单行，必须用 12h48m 这种记法，
 * 「10时 49分 16秒」会折行。
 */
export function compactDuration(startedAt: string): string {
  if (startedAt.length === 0) {
    return '-';
  }
  const t: number = new Date(startedAt).getTime();
  const now: number = Date.now();
  if (isNaN(t) || t <= 0 || now <= t) {
    return '-';
  }
  const sec: number = Math.floor((now - t) / 1000);
  const days: number = Math.floor(sec / 86400);
  const hours: number = Math.floor((sec % 86400) / 3600);
  const mins: number = Math.floor((sec % 3600) / 60);
  if (days > 0) {
    return days.toString() + 'd ' + hours.toString() + 'h';
  }
  if (hours > 0) {
    return hours.toString() + 'h ' + mins.toString() + 'm';
  }
  return mins.toString() + 'm ' + (sec % 60).toString() + 's';
}

function pad2(n: number): string {
  return n < 10 ? '0' + n.toString() : n.toString();
}
