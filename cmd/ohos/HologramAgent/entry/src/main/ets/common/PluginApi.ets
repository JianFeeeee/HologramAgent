/**
 * 插件数据获取（无 UI 依赖）。
 *
 * 从 pages/PluginsPage.ets 抽出：三处接口的取数、合并口径与字段解析
 * 都放在这里，页面只负责把结果落到 @State。
 *
 * 数据源对齐 WebGUI renderPlugins：
 *  - GET /kernel           → plugins[{name,loaded}]（含全部内置插件）+ tools（按 plugin 归属）
 *  - GET /plugins          → 已安装外部插件元数据（version/description 等）
 *  - GET /plugins/disabled → {disabled:[{name,...}]}
 * 三方按名称合并去重排序。
 */

import { apiClient } from './ApiClient';
import { PluginRow, PluginDetail, emptyPluginDetail } from '../model/Model';

/** 拉取并合并插件列表；失败时抛错，由调用方转成人话提示。 */
export async function fetchPluginRows(): Promise<PluginRow[]> {
  // ---- kernel: loaded plugins + tool ownership ----
  const loadedMap: Map<string, boolean> = new Map<string, boolean>();
  const toolsByPlugin: Map<string, string[]> = new Map<string, string[]>();
  const kResp = await apiClient.getWithTimeout('/kernel', 12000);
  const kernelObj: Record<string, Object> = JSON.parse(kResp.body) as Record<string, Object>;
  const kpRaw: Object | undefined = kernelObj['plugins'];
  if (kpRaw !== undefined && kpRaw !== null) {
    const kpArr: Object[] = kpRaw as Object[];
    for (let i = 0; i < kpArr.length; i++) {
      const item: Record<string, Object> = kpArr[i] as Record<string, Object>;
      const n: string = item['name'] as string ?? '';
      if (n.length === 0) {
        continue;
      }
      loadedMap.set(n, item['loaded'] as boolean ?? true);
    }
  }
  const tRaw: Object | undefined = kernelObj['tools'];
  if (tRaw !== undefined && tRaw !== null) {
    const tArr: Object[] = tRaw as Object[];
    for (let i = 0; i < tArr.length; i++) {
      const item: Record<string, Object> = tArr[i] as Record<string, Object>;
      const tn: string = item['name'] as string ?? '';
      const owner: string = item['plugin'] as string ?? '';
      if (tn.length === 0 || owner.length === 0) {
        continue;
      }
      let list: string[] | undefined = toolsByPlugin.get(owner);
      if (list === undefined) {
        list = [];
        toolsByPlugin.set(owner, list);
      }
      // 每插件最多展示 8 个工具名，避免卡片过长
      if (list.length < 8) {
        list.push(tn);
      }
    }
  }

  // ---- installed external plugins metadata ----
  const externalMeta: Map<string, Record<string, Object>> = new Map<string, Record<string, Object>>();
  try {
    const pResp = await apiClient.getWithTimeout('/plugins', 10000);
    const bodyTrim = pResp.body.trim();
    let arr: Object[] = [];
    if (bodyTrim.length > 0 && bodyTrim.charAt(0) === '[') {
      arr = JSON.parse(pResp.body) as Object[];
    } else {
      const obj: Record<string, Object> = JSON.parse(pResp.body) as Record<string, Object>;
      const rawList: Object = obj['plugins'] ?? obj['data'];
      if (rawList !== undefined && rawList !== null) {
        arr = rawList as Object[];
      }
    }
    for (let i = 0; i < arr.length; i++) {
      const item: Record<string, Object> = arr[i] as Record<string, Object>;
      const n: string = item['name'] as string ?? '';
      if (n.length > 0) {
        externalMeta.set(n, item);
      }
    }
  } catch (e) {
    // 外部列表失败不阻塞内置展示
  }

  // ---- disabled list ----
  const disabledNames: Set<string> = new Set<string>();
  try {
    const dResp = await apiClient.getWithTimeout('/plugins/disabled', 8000);
    const dObj: Record<string, Object> = JSON.parse(dResp.body) as Record<string, Object>;
    const dArr: Object | undefined = dObj['disabled'];
    if (dArr !== undefined && dArr !== null) {
      const items: Object[] = dArr as Object[];
      for (let di = 0; di < items.length; di++) {
        const dItem: Record<string, Object> = items[di] as Record<string, Object>;
        const dn: string = dItem['name'] as string ?? '';
        if (dn.length > 0) {
          disabledNames.add(dn);
        }
      }
    }
  } catch (e) {
    // disabled endpoint may not exist; ignore
  }

  // ---- merge: allNames sorted（与 GUI 一致）----
  const allNames: Set<string> = new Set<string>();
  loadedMap.forEach((v: boolean, k: string) => {
    allNames.add(k);
  });
  externalMeta.forEach((v: Record<string, Object>, k: string) => {
    allNames.add(k);
  });
  disabledNames.forEach((n: string) => {
    allNames.add(n);
  });
  const names: string[] = Array.from(allNames);
  names.sort();

  const rows: PluginRow[] = [];
  for (let i = 0; i < names.length; i++) {
    const name: string = names[i];
    const meta: Record<string, Object> | undefined = externalMeta.get(name);
    const tools: string[] | undefined = toolsByPlugin.get(name);
    const row: PluginRow = {
      name: name,
      loaded: loadedMap.get(name) ?? false,
      disabled: disabledNames.has(name),
      external: externalMeta.has(name),
      version: meta !== undefined ? meta['version'] as string ?? '' : '',
      description: meta !== undefined ? meta['description'] as string ?? '' : '',
      tools: tools,
    };
    rows.push(row);
  }
  return rows;
}

function strArray(raw: Object | undefined): string[] {
  const out: string[] = [];
  if (raw === undefined || raw === null) {
    return out;
  }
  const arr: Object[] = raw as Object[];
  for (let i = 0; i < arr.length; i++) {
    const s: string = arr[i] as string ?? '';
    if (s.length > 0) {
      out.push(s);
    }
  }
  return out;
}

/**
 * GET /plugins/{name} —— 后端返回插件清单字段。
 * WebGUI 只是把它 JSON.stringify 进 <pre>，这里逐字段结构化展示。
 * 内置插件不在 /plugins 里，取不到详情时由调用方退回列表已有信息。
 */
export async function fetchPluginDetail(name: string): Promise<PluginDetail> {
  const resp = await apiClient.getWithTimeout('/plugins/' + name, 10000);
  const o: Record<string, Object> = JSON.parse(resp.body) as Record<string, Object>;
  const d: PluginDetail = emptyPluginDetail();
  d.name = o['name'] as string ?? name;
  d.version = o['version'] as string ?? '';
  d.description = o['description'] as string ?? '';
  d.author = o['author'] as string ?? '';
  d.license = o['license'] as string ?? '';
  d.homepage = o['homepage'] as string ?? '';
  d.repository = o['repository'] as string ?? '';
  d.entry = o['entry'] as string ?? '';
  d.minVersion = o['min_version'] as string ?? '';
  d.deprecated = o['deprecated'] as boolean ?? false;
  d.tags = strArray(o['tags']);
  d.files = strArray(o['files']);
  return d;
}
