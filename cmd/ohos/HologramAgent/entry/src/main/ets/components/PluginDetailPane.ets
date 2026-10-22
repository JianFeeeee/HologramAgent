/**
 * 插件详情（二级页面内容）。
 *
 * 从 pages/PluginsPage.ets 抽出。
 * WebGUI 这里只有一个 JSON.stringify 的 <pre>，
 * 移植时改成结构化卡片：状态 / 清单字段 / 工具 / 配置 / 操作。
 */

import { PluginRow, PluginDetail } from '../model/Model';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, RADIUS_SM,
  ANIM_FAST, COLOR_ERROR } from '../common/Constants';
import { pluginDetailStatusLine, pluginStatusColor } from '../common/PluginStatus';
import { PlainCard } from './SubPage';
import { SettingsEditor } from './SettingsEditor';

@Component
export struct PluginDetailPane {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop detail: PluginDetail;
  /** 当前查看详情的插件名（可能还没拿到 detail.name） */
  @Prop activeName: string = '';
  @Prop busy: boolean = false;
  @Prop errorText: string = '';
  /** 一级列表里对应的那一行；取不到详情时用它兜底，也为操作按钮提供状态 */
  @Prop row: PluginRow | undefined = undefined;
  onToggle?: () => void;
  onRemove?: () => void;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  // ---- 取值助手：ArkTS 禁止非空断言，统一在这里做 null 收敛 ----

  private hasManifest(): boolean {
    return this.detail.author.length > 0 || this.detail.license.length > 0 ||
      this.detail.homepage.length > 0 || this.detail.repository.length > 0 ||
      this.detail.entry.length > 0 || this.detail.minVersion.length > 0;
  }

  private isBuiltin(): boolean {
    const r: PluginRow | undefined = this.row;
    return r !== undefined ? !r.external : false;
  }

  private isDisabled(): boolean {
    const r: PluginRow | undefined = this.row;
    return r !== undefined ? r.disabled : false;
  }

  private tools(): string[] {
    const r: PluginRow | undefined = this.row;
    if (r === undefined) {
      return [];
    }
    return r.tools ?? [];
  }

  private statusLine(): string {
    const r: PluginRow | undefined = this.row;
    return r !== undefined ? pluginDetailStatusLine(r, this.detail.deprecated) : '未加载';
  }

  private statusColor(): string {
    const r: PluginRow | undefined = this.row;
    return r !== undefined ? pluginStatusColor(r, this.palette().textMuted) : this.palette().textMuted;
  }

  /** 明细行：值为空时整行不渲染，避免详情页出现一排 "-" */
  @Builder
  KvRow(label: string, value: string) {
    if (value.length > 0) {
      Row() {
        Text(label)
          .fontSize(13)
          .fontColor(this.palette().textSecondary)
          .layoutWeight(1)
        Text(value)
          .fontSize(13)
          .fontColor(this.palette().textPrimary)
          .textAlign(TextAlign.End)
          .maxLines(2)
          .textOverflow({ overflow: TextOverflow.Ellipsis })
          .constraintSize({ maxWidth: 220 })
          .margin({ left: 16 })
      }
      .width('100%')
      .padding({ top: 8, bottom: 8 })
      .alignItems(VerticalAlign.Top)
    }
  }

  /** 详情页状态行：同样去掉徽标，一个状态点 + 一行纯文字 */
  @Builder
  Badges() {
    Row({ space: 6 }) {
      Circle({ width: 7, height: 7 })
        .fill(this.statusColor())
      Text(this.statusLine())
        .fontSize(12)
        .fontColor(this.palette().textSecondary)
        .layoutWeight(1)
        .maxLines(1)
        .textOverflow({ overflow: TextOverflow.Ellipsis })
    }
    .width('100%')
    .alignItems(VerticalAlign.Center)
  }

  /** 工具清单来自一级列表已合并的 kernel.tools（按 plugin 归属） */
  @Builder
  ToolsCard() {
    if (this.tools().length > 0) {
      PlainCard({ caption: '注册的工具 (' + this.tools().length.toString() + ')' }) {
        Flex({ wrap: FlexWrap.Wrap }) {
          ForEach(this.tools(), (tool: string) => {
            Text(tool)
              .fontSize(11)
              .fontColor('#4A90D9')
              .padding({ left: 8, right: 8, top: 3, bottom: 3 })
              .borderRadius(RADIUS_SM)
              .backgroundColor(this.palette().frostSoftBg)
              .margin({ right: 5, bottom: 5 })
          }, (tool: string) => tool)
        }
      }
    }
  }

  @Builder
  ActionsCard() {
    if (this.activeName.length > 0) {
      PlainCard({ caption: '操作' }) {
        Row() {
          Button(this.isDisabled() ? '启用' : '禁用')
            .height(34)
            .fontSize(12)
            .backgroundColor(Color.Transparent)
            .border({
              width: 1,
              color: this.isDisabled()
                ? this.palette().btnGhostBorder : 'rgba(217, 154, 43, 0.5)',
            })
            .fontColor(this.isDisabled()
              ? this.palette().textSecondary : '#D99A2B')
            .animation({ duration: ANIM_FAST, curve: Curve.EaseOut })
            .onClick(() => {
              const cb: (() => void) | undefined = this.onToggle;
              if (cb !== undefined) {
                cb();
              }
            })
          Blank()
          if (!this.isBuiltin()) {
            Button('卸载')
              .height(34)
              .fontSize(12)
              .backgroundColor(Color.Transparent)
              .border({ width: 1, color: 'rgba(232, 64, 38, 0.45)' })
              .fontColor(COLOR_ERROR)
              .onClick(() => {
                const cb: (() => void) | undefined = this.onRemove;
                if (cb !== undefined) {
                  cb();
                }
              })
              .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
          }
        }
        .width('100%')
      }
    }
  }

  build() {
    // 单一根节点：与原来作为 @Builder 直接铺在 SubPageLayer 的 Column 里同构，
    // 保持 alignItems Start，避免文本被默认居中对齐。
    Column() {
    if (this.busy) {
      Row() {
        LoadingProgress()
          .width(26)
          .height(26)
          .color(this.palette().accent)
      }
      .width('100%')
      .justifyContent(FlexAlign.Center)
      .padding({ top: 30, bottom: 30 })
      .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
    }

    if (this.errorText.length > 0) {
      Text(this.errorText)
        .fontSize(12)
        .fontColor(COLOR_ERROR)
        .padding({ left: 4, bottom: 12 })
        .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
    }

    // 概览卡：名称、版本、状态徽标、描述
    PlainCard({ caption: '概览' }) {
      Row({ space: 8 }) {
        Text(this.detail.name.length > 0 ? this.detail.name : this.activeName)
          .fontSize(17)
          .fontWeight(FontWeight.Bold)
          .fontColor(this.palette().textPrimary)
          .layoutWeight(1)
          .maxLines(1)
          .textOverflow({ overflow: TextOverflow.Ellipsis })
        if (this.detail.version.length > 0) {
          Text('v' + this.detail.version)
            .fontSize(12)
            .fontColor(this.palette().textSecondary)
        }
      }
      .width('100%')
      .margin({ bottom: 10 })

      this.Badges()

      if (this.detail.description.length > 0) {
        Text(this.detail.description)
          .fontSize(13)
          .fontColor(this.palette().textSecondary)
          .width('100%')
          .margin({ top: 10 })
      }
    }

    // 清单卡：只有真拿到字段才出卡，否则会留一张空壳（内置插件没有清单文件）
    if (this.hasManifest()) {
      PlainCard({ caption: '清单' }) {
        this.KvRow('作者', this.detail.author)
        this.KvRow('许可证', this.detail.license)
        this.KvRow('主页', this.detail.homepage)
        this.KvRow('仓库', this.detail.repository)
        this.KvRow('入口', this.detail.entry)
        this.KvRow('最低内核版本', this.detail.minVersion)
      }
    }

    if (this.detail.tags.length > 0) {
      PlainCard({ caption: '标签' }) {
        Flex({ wrap: FlexWrap.Wrap }) {
          ForEach(this.detail.tags, (t: string) => {
            Text(t)
              .fontSize(10)
              .fontColor('#4A90D9')
              .padding({ left: 8, right: 8, top: 3, bottom: 3 })
              .borderRadius(RADIUS_SM)
              .backgroundColor(this.palette().frostSoftBg)
              .margin({ right: 5, bottom: 5 })
          }, (t: string) => t)
        }
      }
    }

    this.ToolsCard()

    // 插件配置：plugin.<name>.* 从后端 /settings?prefix= 取，就地编辑。
    // 这些 key 属于插件本身，之前被平铺在「设置 → 后端配置」里，
    // 现在归位到插件详情页 —— 「插件的设计页面就是插件的详情页」。
    if (this.activeName.length > 0) {
      PlainCard({ caption: '插件配置' }) {
        SettingsEditor({
          prefix: 'plugin.' + this.activeName + '.',
          emptyHint: '该插件没有暴露可配置项',
        })
      }
    }

    if (this.detail.files.length > 0) {
      PlainCard({ caption: '文件 (' + this.detail.files.length.toString() + ')' }) {
        ForEach(this.detail.files, (f: string) => {
          Text(f)
            .fontSize(12)
            .fontColor(this.palette().textSecondary)
            .width('100%')
            .maxLines(1)
            .textOverflow({ overflow: TextOverflow.Ellipsis })
            .margin({ bottom: 4 })
        }, (f: string) => f)
      }
    }

    this.ActionsCard()
    }
    .width('100%')
    .alignItems(HorizontalAlign.Start)
  }
}
