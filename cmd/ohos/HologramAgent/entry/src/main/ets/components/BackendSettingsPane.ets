/**
 * 「核心配置」相关的两个二级页面：分类列表 + 某个分类的配置项。
 *
 * 从 pages/SettingsPage.ets 抽出。
 * 取数/分区/落库都留在页面（它同时要显示"几个分类 · 几项"和错误态），
 * 这里只负责渲染与把用户动作转成回调。
 */

import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE } from '../common/Constants';
import { SettingsSection, SettingEntry, activeSectionTitle } from '../common/SettingsModel';
import { SubPageLayer, NavGroup, NavRow, PlainCard } from './SubPage';
import { SettingsEntryCard } from './SettingsEntryCard';

@Component
export struct BackendSectionsPane {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop sections: SettingsSection[] = [];
  @Prop pluginKeyCount: number = 0;
  @Prop pluginConfigCount: number = 0;
  @Prop busy: boolean = false;
  @Prop errorText: string = '';
  @Prop activeSection: string = '';
  /** 宽屏右栏正显示分区明细时高亮左侧对应行 */
  @Prop highlightRows: boolean = false;
  onBack?: () => void;
  onRefresh?: () => void;
  onOpenSection?: (id: string) => void;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  build() {
    SubPageLayer({
      title: '核心配置',
      tab: 3,
      onBack: () => {
        const cb: (() => void) | undefined = this.onBack;
        if (cb !== undefined) {
          cb();
        }
      },
      showRefresh: true,
      onRefresh: () => {
        const cb: (() => void) | undefined = this.onRefresh;
        if (cb !== undefined) {
          cb();
        }
      },
    }) {
      Column() {
        if (this.errorText.length > 0) {
          PlainCard({ caption: '状态' }) {
            Text(this.errorText)
              .fontSize(12)
              .fontColor('#E84026')
          }
        }

        NavGroup({ caption: '核心' }) {
          ForEach(this.sections, (sec: SettingsSection, idx: number) => {
            NavRow({
              icon: $r('app.media.ic_tune'),
              title: sec.title,
              subtitle: sec.count.toString() + ' 项配置',
              showDivider: idx < this.sections.length - 1,
              selected: this.highlightRows && this.activeSection === sec.id,
              onTap: () => {
                const cb: ((id: string) => void) | undefined = this.onOpenSection;
                if (cb !== undefined) {
                  cb(sec.id);
                }
              },
            })
          }, (sec: SettingsSection) => sec.id + sec.count.toString())
        }

        // 插件配置不在这里编辑：入口在插件页的详情里，这里只指路，避免两处重复入口
        if (this.pluginKeyCount > 0) {
          Text('插件的 ' + this.pluginKeyCount.toString() + ' 项配置（' +
            this.pluginConfigCount.toString() + ' 个插件）在「插件 → 选择插件 → 插件配置」中修改。')
            .fontSize(12)
            .fontColor(this.palette().textMuted)
            .width('100%')
            .padding({ left: 4, right: 4 })
        }

        if (this.sections.length === 0 && !this.busy) {
          Text('未获取到配置分类。检查后端连接后点击刷新。')
            .fontSize(12)
            .fontColor(this.palette().textMuted)
            .padding({ left: 4 })
        }
      }
      .width('100%')
      .alignItems(HorizontalAlign.Start)
    }
  }
}

@Component
export struct SectionEntriesPane {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop sections: SettingsSection[] = [];
  @Prop activeSection: string = '';
  @Prop entries: SettingEntry[] = [];
  @Prop busy: boolean = false;
  onBack?: () => void;
  onRefresh?: () => void;
  onSaveValue?: (key: string, value: string) => void;
  onSaveCurrent?: (key: string) => void;
  onEdit?: (key: string, value: string) => void;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  build() {
    SubPageLayer({
      title: activeSectionTitle(this.sections, this.activeSection),
      tab: 3,
      onBack: () => {
        const cb: (() => void) | undefined = this.onBack;
        if (cb !== undefined) {
          cb();
        }
      },
      showRefresh: true,
      onRefresh: () => {
        const cb: (() => void) | undefined = this.onRefresh;
        if (cb !== undefined) {
          cb();
        }
      },
    }) {
      Column() {
        ForEach(this.entries, (entry: SettingEntry) => {
          SettingsEntryCard({
            entry: entry,
            onSaveValue: (key: string, value: string) => {
              const cb: ((k: string, v: string) => void) | undefined = this.onSaveValue;
              if (cb !== undefined) {
                cb(key, value);
              }
            },
            onSaveCurrent: (key: string) => {
              const cb: ((k: string) => void) | undefined = this.onSaveCurrent;
              if (cb !== undefined) {
                cb(key);
              }
            },
            onEdit: (key: string, value: string) => {
              const cb: ((k: string, v: string) => void) | undefined = this.onEdit;
              if (cb !== undefined) {
                cb(key, value);
              }
            },
          })
        }, (entry: SettingEntry) => entry.key + '|' + entry.value + '|' + (entry.dirty ? 'd' : 'c'))

        if (!this.busy && this.entries.length === 0) {
          Text('该分区暂无配置项')
            .fontSize(12)
            .fontColor(this.palette().textMuted)
            .padding(16)
        }
      }
      .width('100%')
      .alignItems(HorizontalAlign.Start)
    }
  }
}
