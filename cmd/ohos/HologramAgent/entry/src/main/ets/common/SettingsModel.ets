/**
 * 设置数据模型与纯解析逻辑（无 UI 依赖）。
 *
 * 从 pages/SettingsPage.ets 抽出：/settings 的响应解析、key 分区归类、
 * 分页取项都是纯函数，页面只负责把结果落到 @State。
 */

/** One settings key card rendered in the editor list. */
export interface SettingEntry {
  key: string;
  displayName: string;
  description: string;
  type: string; // bool | int | duration | select | password | text | string
  options: string[];
  value: string; // raw string value as stored by backend
  dirty: boolean;
}

export interface SettingsSection {
  id: string;
  title: string;
  count: number;
}

export interface SettingMetaRaw {
  key: string;
  type: string;
  displayName: string;
  description: string;
  category: string;
  options: string[];
}

export const SETTINGS_PAGE_SIZE: number = 40;

/**
 * 二级页面标识。
 * 一级入口列表（components/SettingsRootEntries.ets）与页面路由表分开成文件后，
 * 这些 id 必须只有一个来源 —— 否则改一处就会"点了没反应"。
 */
export const SUB_NONE: string = '';
export const SUB_STATUS: string = 'status';
export const SUB_CONNECTIONS: string = 'connections';
export const SUB_APPEARANCE: string = 'appearance';
export const SUB_BACKEND: string = 'backend';
export const SUB_SECTION: string = 'section';

/** /settings 响应解析结果 */
export interface ParsedSettings {
  meta: Record<string, SettingMetaRaw>;
  values: Record<string, string>;
}

/** 分区统计结果 */
export interface SectionStats {
  sections: SettingsSection[];
  /** plugin.* 配置项总数（编辑入口在插件详情页，这里只用于提示去向） */
  pluginKeyCount: number;
  /** 涉及的插件个数 */
  pluginConfigCount: number;
}

/**
 * 解析 GET /settings 响应：meta 定义 + 当前值。
 * 值统一转成字符串（后端可能给 bool/number/嵌套对象）。
 */
export function parseSettingsPayload(body: string): ParsedSettings {
  const obj: Record<string, Object> = JSON.parse(body) as Record<string, Object>;
  const metaStore: Record<string, SettingMetaRaw> = {};
  const valuesStore: Record<string, string> = {};

  const rawMeta: Object | undefined = obj['meta'];
  if (rawMeta !== undefined && rawMeta !== null) {
    const mObj: Record<string, Object> = rawMeta as Record<string, Object>;
    for (const mk of Object.keys(mObj)) {
      const item: Record<string, Object> = mObj[mk] as Record<string, Object>;
      const optsArr: Object | undefined = item['options'];
      const opts: string[] = [];
      if (optsArr !== undefined && optsArr !== null) {
        const oa: Object[] = optsArr as Object[];
        for (let i = 0; i < oa.length; i++) {
          opts.push(oa[i] as string);
        }
      }
      const meta: SettingMetaRaw = {
        key: item['key'] as string ?? mk,
        type: item['type'] as string ?? 'string',
        displayName: item['display_name'] as string ?? '',
        description: item['description'] as string ?? '',
        category: item['category'] as string ?? '',
        options: opts,
      };
      metaStore[mk] = meta;
    }
  }

  const rawVals: Object | undefined = obj['settings'];
  if (rawVals !== undefined && rawVals !== null) {
    const vObj: Record<string, Object> = rawVals as Record<string, Object>;
    for (const vk of Object.keys(vObj)) {
      if (vk.length === 0) {
        continue;
      }
      const val: Object = vObj[vk];
      let strVal: string;
      if (typeof val === 'string') {
        strVal = val as string;
      } else if (typeof val === 'boolean' || typeof val === 'number') {
        strVal = String(val);
      } else {
        strVal = JSON.stringify(val);
      }
      valuesStore[vk] = strVal;
    }
  }

  const parsed: ParsedSettings = { meta: metaStore, values: valuesStore };
  return parsed;
}

export function metaCategory(metaStore: Record<string, SettingMetaRaw>, key: string): string {
  const m: SettingMetaRaw | undefined = metaStore[key];
  return m !== undefined && m.category.length > 0 ? m.category : '';
}

export function categoryTitle(cat: string): string {
  const map: Record<string, string> = {
    'agent': '智能体',
    'daemon': '守护进程',
    'llm': '大模型',
    'sources': '数据源',
    'input': '输入',
    'paths': '路径',
    'resources': '资源',
    'defaults': '默认值',
    'snapshot': '快照',
    'rollback': '回滚',
  };
  const t: string | undefined = map[cat];
  return t !== undefined ? t : cat;
}

/**
 * 把 key 归到分区：
 *  - core.*   → 'core/<category>'，展示在「核心」二级页下
 *  - plugin.* → 'plugin/<name>'，仅用于计数；实际编辑在插件详情页里，
 *               不在这里列出（否则同一批 key 会有两个入口）
 *  - 其余     → 'other'
 */
export function buildSections(
  valuesStore: Record<string, string>, metaStore: Record<string, SettingMetaRaw>): SectionStats {
  const ids: string[] = [];
  const counts: Record<string, number> = {};
  const titles: Record<string, string> = {};
  let pluginKeys: number = 0;
  const plugNames: string[] = [];
  for (const key of Object.keys(valuesStore)) {
    if (key.startsWith('plugin.')) {
      // 插件配置不在这里列：它属于插件本身，入口在「插件 → 详情 → 插件配置」。
      // 这里只统计，用于提示有多少项在那边。
      pluginKeys = pluginKeys + 1;
      const rest: string = key.substring('plugin.'.length);
      const dot: number = rest.indexOf('.');
      const plugName: string = dot > 0 ? rest.substring(0, dot) : rest;
      if (plugName.length > 0 && plugNames.indexOf(plugName) < 0) {
        plugNames.push(plugName);
      }
      continue;
    }
    let secId: string;
    if (key.startsWith('core.')) {
      const cat: string = metaCategory(metaStore, key);
      secId = cat.length > 0 ? 'core/' + cat : 'core/misc';
      if (titles[secId] === undefined) {
        titles[secId] = cat.length > 0 ? categoryTitle(cat) : '未分类';
      }
    } else {
      secId = 'other';
      if (titles[secId] === undefined) {
        titles[secId] = '其他';
      }
    }
    if (counts[secId] === undefined) {
      counts[secId] = 0;
      ids.push(secId);
    }
    counts[secId] = counts[secId] + 1;
  }
  ids.sort((a: string, b: string): number => a.localeCompare(b));
  const secs: SettingsSection[] = [];
  for (const id of ids) {
    secs.push({ id: id, title: titles[id] ?? id, count: counts[id] ?? 0 });
  }
  const stats: SectionStats = {
    sections: secs,
    pluginKeyCount: pluginKeys,
    pluginConfigCount: plugNames.length,
  };
  return stats;
}

export function pickInitialSection(sections: SettingsSection[]): string {
  for (let i = 0; i < sections.length; i++) {
    if (sections[i].id === 'core/agent') {
      return 'core/agent';
    }
  }
  return sections.length > 0 ? sections[0].id : 'core';
}

export function keyInSection(
  metaStore: Record<string, SettingMetaRaw>, key: string, secId: string): boolean {
  if (secId === 'other') {
    return !key.startsWith('core.') && !key.startsWith('plugin.');
  }
  if (secId.startsWith('core/')) {
    if (!key.startsWith('core.')) {
      return false;
    }
    const cat: string = secId.substring('core/'.length);
    return cat === 'misc'
      ? metaCategory(metaStore, key).length === 0
      : metaCategory(metaStore, key) === cat;
  }
  if (secId.startsWith('plugin/')) {
    const p: string = secId.substring('plugin/'.length);
    return key.startsWith('plugin.' + p + '.');
  }
  return false;
}

export function activeSectionTitle(sections: SettingsSection[], secId: string): string {
  for (let i = 0; i < sections.length; i++) {
    if (sections[i].id === secId) {
      return sections[i].title;
    }
  }
  return '配置项';
}

/** 某个分区的配置项（按 key 字典序，最多 PAGE_SIZE*4 项） */
export function buildEntries(
  valuesStore: Record<string, string>,
  metaStore: Record<string, SettingMetaRaw>,
  secId: string): SettingEntry[] {
  const entries: SettingEntry[] = [];
  const keys: string[] = Object.keys(valuesStore).filter((k: string): boolean => {
    return keyInSection(metaStore, k, secId);
  });
  keys.sort((a: string, b: string): number => a.localeCompare(b));
  const limit: number = Math.min(keys.length, SETTINGS_PAGE_SIZE * 4);
  for (let i = 0; i < limit; i++) {
    const key: string = keys[i];
    const meta: SettingMetaRaw | undefined = metaStore[key];
    const entry: SettingEntry = {
      key: key,
      displayName: meta !== undefined && meta.displayName.length > 0 ? meta.displayName : key,
      description: meta !== undefined ? meta.description : '',
      type: meta !== undefined ? meta.type : 'string',
      options: meta !== undefined ? meta.options : [],
      value: valuesStore[key] ?? '',
      dirty: false,
    };
    entries.push(entry);
  }
  return entries;
}

/** 覆盖某个 entry 的 value/dirty，返回新数组（保持 @State 数组替换语义） */
export function withEntryMarked(
  entries: SettingEntry[], key: string, value: string, dirty: boolean): SettingEntry[] {
  const next: SettingEntry[] = [];
  for (let i = 0; i < entries.length; i++) {
    const e: SettingEntry = entries[i];
    if (e.key === key) {
      const copy: SettingEntry = {
        key: e.key,
        displayName: e.displayName,
        description: e.description,
        type: e.type,
        options: e.options,
        value: value,
        dirty: dirty,
      };
      next.push(copy);
    } else {
      next.push(e);
    }
  }
  return next;
}

/**
 * 从 picker 返回的 URI 里取图片后缀（带点）。
 * Image 组件依赖后缀选择解码器；沙箱里存成无后缀文件会静默解码失败，
 * 用户看到的就是"背景图设置了却不生效"。取不到后缀时兜底 .jpg。
 */
export function imageExt(uri: string): string {
  let s: string = uri;
  const q: number = s.indexOf('?');
  if (q >= 0) {
    s = s.substring(0, q);
  }
  const dot: number = s.lastIndexOf('.');
  const slash: number = s.lastIndexOf('/');
  if (dot > slash && dot < s.length - 1) {
    const ext: string = s.substring(dot).toLowerCase();
    if (ext.length <= 5) {
      return ext;
    }
  }
  return '.jpg';
}
