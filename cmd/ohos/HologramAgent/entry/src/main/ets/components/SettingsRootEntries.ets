/**
 * 设置一级页的入口列表。
 *
 * 从 pages/SettingsPage.ets 抽出：纯展示 + 跳转回调，
 * 所有计数/文案由页面算好传进来（页面才是这些状态的持有者）。
 */

import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE } from '../common/Constants';
import { SUB_STATUS, SUB_CONNECTIONS, SUB_BACKEND, SUB_SECTION, SUB_APPEARANCE } from '../common/SettingsModel';
import { StatusSummaryCard } from './StatusCards';
import { NavGroup, NavRow } from './SubPage';

@Component
export struct SettingsRootEntries {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  /** 连接配置条数 */
  @Prop connCount: number = 0;
  /** 当前生效连接的展示名 */
  @Prop connName: string = '';
  /** 后端配置：分类数 / 核心项数 / 是否加载中 / 错误文案 */
  @Prop sectionCount: number = 0;
  @Prop coreKeys: number = 0;
  @Prop busy: boolean = false;
  @Prop errorText: string = '';
  /** 当前主题的中文名（一级行右侧摘要值） */
  @Prop themeLabel: string = '';
  /** 宽屏分栏时用来高亮右栏对应的入口行 */
  @Prop activeSub: string = '';
  @Prop isWide: boolean = false;
  onOpen?: (id: string) => void;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  private open(id: string): void {
    const cb: ((id: string) => void) | undefined = this.onOpen;
    if (cb !== undefined) {
      cb(id);
    }
  }

  build() {
    Column() {
      // 运行状态摘要（原「状态」Tab）：整卡可点，进入明细二级页
      StatusSummaryCard({
        onTap: () => {
          this.open(SUB_STATUS);
        },
      })

      NavGroup({ caption: '连接' }) {
        NavRow({
          icon: $r('app.media.ic_link'),
          title: '后端连接',
          subtitle: this.connCount.toString() + ' 个连接配置',
          value: this.connName,
          selected: this.isWide && this.activeSub === SUB_CONNECTIONS,
          onTap: () => {
            this.open(SUB_CONNECTIONS);
          },
        })
        NavRow({
          icon: $r('app.media.ic_tune'),
          title: '核心配置',
          subtitle: this.sectionCount.toString() + ' 个分类 · ' + this.coreKeys.toString() + ' 项',
          value: this.busy ? '加载中' : (this.errorText.length > 0 ? '不可用' : ''),
          showDivider: false,
          selected: this.isWide && (this.activeSub === SUB_BACKEND || this.activeSub === SUB_SECTION),
          onTap: () => {
            this.open(SUB_BACKEND);
          },
        })
      }

      NavGroup({ caption: '个性化' }) {
        NavRow({
          icon: $r('app.media.ic_theme'),
          title: '外观',
          subtitle: '主题与背景图',
          value: this.themeLabel,
          showDivider: false,
          selected: this.isWide && this.activeSub === SUB_APPEARANCE,
          onTap: () => {
            this.open(SUB_APPEARANCE);
          },
        })
      }

      if (this.errorText.length > 0) {
        Text(this.errorText)
          .fontSize(12)
          .fontColor('#E84026')
          .padding({ left: 4, right: 4 })
      }
    }
    .width('100%')
    .alignItems(HorizontalAlign.Start)
  }
}
