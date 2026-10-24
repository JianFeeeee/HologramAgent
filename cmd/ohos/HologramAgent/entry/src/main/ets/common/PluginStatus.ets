/**
 * 插件状态展示口径（纯函数，无 UI 依赖）。
 *
 * 从 pages/PluginsPage.ets 抽出：列表行与详情页共用同一套状态判定/文案/配色，
 * 抽成模块后两处不会再各写一份。
 *
 * 状态口径与 WebGUI 一致：已加载绿 / 禁用待生效黄 / 已禁用红 / 未加载灰。
 */

import { PluginRow } from '../model/Model';

/** 'loaded' | 'pending' | 'disabled' | 'notloaded' */
export function pluginStatusOf(plugin: PluginRow): string {
  if (plugin.loaded && !plugin.disabled) {
    return 'loaded'; // 已加载
  }
  if (plugin.loaded && plugin.disabled) {
    return 'pending'; // 运行中(禁用待生效)
  }
  if (plugin.disabled) {
    return 'disabled'; // 已禁用
  }
  return 'notloaded'; // 未加载
}

export function pluginStatusText(plugin: PluginRow): string {
  const s: string = pluginStatusOf(plugin);
  if (s === 'loaded') {
    return '已加载';
  }
  if (s === 'pending') {
    return '待生效';
  }
  if (s === 'disabled') {
    return '已禁用';
  }
  return '未加载';
}

/** 状态点颜色；未加载态用主题里的弱化色（mutedColor 由调用方从 palette 取）。 */
export function pluginStatusColor(plugin: PluginRow, mutedColor: string): string {
  const s: string = pluginStatusOf(plugin);
  if (s === 'loaded') {
    return '#17A964';
  }
  if (s === 'pending') {
    return '#D99A2B';
  }
  if (s === 'disabled') {
    return '#E84026';
  }
  return mutedColor;
}

/** 列表行副标题：状态 + 内置/外部 + 工具数，一行灰字，不用徽标 */
export function pluginRowSubtitle(plugin: PluginRow): string {
  const parts: string[] = [];
  parts.push(pluginStatusText(plugin));
  parts.push(plugin.external ? '外部' : '内置');
  if (plugin.tools !== undefined && plugin.tools.length > 0) {
    parts.push(plugin.tools.length.toString() + ' 工具');
  }
  return parts.join(' · ');
}

/** 详情页状态行：状态 + 内置/外部 + 已废弃 + 工具数（顺序与原实现一致） */
export function pluginDetailStatusLine(row: PluginRow, deprecated: boolean): string {
  const parts: string[] = [];
  parts.push(pluginStatusText(row));
  parts.push(row.external ? '外部' : '内置');
  if (deprecated) {
    parts.push('已废弃');
  }
  if (row.tools !== undefined && row.tools.length > 0) {
    parts.push(row.tools.length.toString() + ' 个工具');
  }
  return parts.join(' · ');
}
