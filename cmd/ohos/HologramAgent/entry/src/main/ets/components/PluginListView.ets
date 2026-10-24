/**
 * 插件一级列表（列表卡 + 三种占位态）。
 *
 * 从 pages/PluginsPage.ets 抽出：列表只负责"选谁"，
 * 描述/工具/启停全部下沉到详情页。
 */

import { PluginRow } from '../model/Model';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, RADIUS_LG, RADIUS_MD,
  ANIM_FAST, ANIM_ENTER } from '../common/Constants';
import { pluginRowSubtitle, pluginStatusColor } from '../common/PluginStatus';
import { noConnectionMessage } from '../common/UserError';
import { MotionBase } from './MotionBase';

/**
 * 单个插件行卡。
 *
 * 外层 Column 只为承载 transition：.transition() 不能直接挂在自定义组件
 * 调用点上（会生成 __Common__ 包装节点）。按压缩放由 MotionBase 统一提供，
 * 每行自带独立按压态，不再需要 pressedName 这种"哪一行被按"的手工记账。
 */
@Component
export struct PluginListCard {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop plugin: PluginRow;
  /** 是否当前选中（宽屏高亮左侧列表项）。单独用基本类型传，选中态变更才能触发更新 */
  @Prop active: boolean = false;
  onTap?: () => void;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  build() {
    Column() {
      MotionBase({ pressEnabled: true }) {
        Row() {
          Column({ space: 3 }) {
            Row({ space: 6 }) {
              Text(this.plugin.name)
                .fontSize(15)
                .fontWeight(this.active ? FontWeight.Medium : FontWeight.Normal)
                .fontColor(this.active ? this.palette().accent : this.palette().textPrimary)
                .maxLines(1)
                .textOverflow({ overflow: TextOverflow.Ellipsis })
                .animation({ duration: ANIM_FAST, curve: Curve.EaseOut })
              if (this.plugin.version !== undefined && this.plugin.version.length > 0) {
                Text('v' + this.plugin.version)
                  .fontSize(10)
                  .fontColor(this.palette().textMuted)
              }
            }
            // 徽标全部去掉（用户要求）：状态用一个 3vp 圆点表达，
            // 其余信息退化为一行灰字副标题 —— 列表只负责"选谁"，细节看详情页。
            Row({ space: 6 }) {
              Circle({ width: 6, height: 6 })
                .fill(pluginStatusColor(this.plugin, this.palette().textMuted))
              Text(pluginRowSubtitle(this.plugin))
                .fontSize(11)
                .fontColor(this.palette().textMuted)
                .maxLines(1)
                .textOverflow({ overflow: TextOverflow.Ellipsis })
                .layoutWeight(1)
            }
            .width('100%')
            .alignItems(VerticalAlign.Center)
          }
          .layoutWeight(1)
          .alignItems(HorizontalAlign.Start)

          Image($r('app.media.ic_chevron_right'))
            .width(15)
            .height(15)
            .fillColor(this.active ? this.palette().accent : this.palette().textMuted)
            .animation({ duration: ANIM_FAST, curve: Curve.EaseOut })
            .draggable(false)
        }
        .width('100%')
        .padding(14)
        .borderRadius(RADIUS_LG)
        .backgroundColor(this.active ? this.palette().accentBg : this.palette().bgCard)
        .border({
          width: 1,
          color: this.active ? this.palette().accent : this.palette().glassBorder,
        })
        // 选中态的底色/描边渐变：MotionBase 的 .animation() 到不了这里
        .animation({ duration: ANIM_FAST, curve: Curve.EaseOut })
        .alignItems(VerticalAlign.Center)
        .onClick(() => {
          const cb: (() => void) | undefined = this.onTap;
          if (cb !== undefined) {
            cb();
          }
        })
      }
    }
    .width('100%')
    .margin({ bottom: 10 })
    // ForEach key 含 loaded/disabled：启停会整行重挂载，靠 transition 变成交叉淡入
    .transition(TransitionEffect.OPACITY
      .combine(TransitionEffect.translate({ y: 12 }))
      .animation({ duration: ANIM_ENTER, curve: Curve.EaseOut }))
  }
}

/**
 * 一级列表整体：占位态 + 行卡 ForEach。
 * 取数留在页面里，这里只吃数据。
 */
@Component
export struct PluginListView {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop plugins: PluginRow[] = [];
  @Prop busy: boolean = false;
  @Prop activeName: string = '';
  @Prop hasConn: boolean = false;
  onSelect?: (name: string) => void;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  build() {
    Column() {
      // ── 加载中 / 未配置 / 空列表三种占位态 ──
      if (this.busy && this.plugins.length === 0) {
        LoadingProgress()
          .width(32)
          .height(32)
          .color(this.palette().accent)
          .margin({ top: 40 })
          .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
      }

      if (!this.hasConn) {
        Text(noConnectionMessage())
          .fontSize(13)
          .fontColor(this.palette().textMuted)
          .padding(20)
          .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
      }

      if (!this.busy && this.plugins.length === 0 && this.hasConn) {
        Text('暂无已加载插件')
          .fontSize(13)
          .fontColor(this.palette().textMuted)
          .padding(20)
          .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
      }

      ForEach(this.plugins, (plugin: PluginRow) => {
        PluginListCard({
          plugin: plugin,
          active: this.activeName === plugin.name,
          onTap: () => {
            const cb: ((name: string) => void) | undefined = this.onSelect;
            if (cb !== undefined) {
              cb(plugin.name);
            }
          },
        })
      }, (plugin: PluginRow) => plugin.name + (plugin.loaded ? 'L' : '') + (plugin.disabled ? 'D' : ''))
    }
    .width('100%')
  }
}
