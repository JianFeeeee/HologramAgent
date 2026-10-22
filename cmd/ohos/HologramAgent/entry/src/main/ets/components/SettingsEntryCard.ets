/**
 * 单个配置项卡片。
 *
 * 从 pages/SettingsPage.ets 抽出：按 type 分发控件（bool/select/password/text/其他），
 * 值的保存与「未保存」标记交回页面（页面上持有 valuesStore 与 entries）。
 */

import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, RADIUS_MD, RADIUS_SM,
  ANIM_FAST, ANIM_NORMAL } from '../common/Constants';
import { SettingEntry } from '../common/SettingsModel';

@Component
export struct SettingsEntryCard {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop entry: SettingEntry;
  /** bool 开关 / select 选项：值已知，直接落库 */
  onSaveValue?: (key: string, value: string) => void;
  /** 输入类控件的保存：由页面取该 key 的最新编辑值再落库 */
  onSaveCurrent?: (key: string) => void;
  /** 输入框内容变化：只更新本地标记，不请求 */
  onEdit?: (key: string, value: string) => void;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  private saveValue(value: string): void {
    const cb: ((key: string, value: string) => void) | undefined = this.onSaveValue;
    if (cb !== undefined) {
      cb(this.entry.key, value);
    }
  }

  private saveCurrent(): void {
    const cb: ((key: string) => void) | undefined = this.onSaveCurrent;
    if (cb !== undefined) {
      cb(this.entry.key);
    }
  }

  private editValue(value: string): void {
    const cb: ((key: string, value: string) => void) | undefined = this.onEdit;
    if (cb !== undefined) {
      cb(this.entry.key, value);
    }
  }

  build() {
    Column() {
      Row() {
        Text(this.entry.displayName)
          .fontSize(13)
          .fontWeight(FontWeight.Medium)
          .fontColor(this.palette().textPrimary)
          .maxLines(2)
          .textOverflow({ overflow: TextOverflow.Ellipsis })
          .layoutWeight(1)
        Text(this.entry.dirty ? '未保存' : this.entry.type)
          .fontSize(10)
          .fontColor(this.entry.dirty ? '#D99A2B' : this.palette().textMuted)
          .padding({ left: 6, right: 6, top: 1, bottom: 1 })
          .borderRadius(RADIUS_SM)
          .backgroundColor(this.entry.dirty ? 'rgba(217, 154, 43, 0.16)' : this.palette().bgHover)
      }
      .width('100%')

      Text(this.entry.key)
        .fontSize(10)
        .fontColor(this.palette().textMuted)
        .maxLines(1)
        .textOverflow({ overflow: TextOverflow.Ellipsis })
        .margin({ top: 1 })

      if (this.entry.description.length > 0) {
        Text(this.entry.description)
          .fontSize(11)
          .fontColor(this.palette().textSecondary)
          .maxLines(3)
          .textOverflow({ overflow: TextOverflow.Ellipsis })
          .margin({ top: 3 })
      }

      // control row per type
      if (this.entry.type === 'bool') {
        Row() {
          Text(this.entry.value === 'true' ? 'true' : 'false')
            .fontSize(12)
            .fontColor(this.entry.value === 'true' ? '#17A964' : this.palette().textMuted)
            .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
          Blank()
          Toggle({ type: ToggleType.Switch, isOn: this.entry.value === 'true' })
            .selectedColor(this.palette().accent)
            .onChange((on: boolean) => {
              this.saveValue(on ? 'true' : 'false');
            })
        }
        .width('100%')
        .margin({ top: 8 })
      } else if (this.entry.type === 'select' && this.entry.options.length > 0) {
        Flex({
          direction: FlexDirection.Row,
          justifyContent: FlexAlign.Start,
          alignItems: ItemAlign.Center,
          wrap: FlexWrap.Wrap,
        }) {
          ForEach(this.entry.options, (opt: string) => {
            Button(opt)
              .height(26)
              .fontSize(11)
              .margin({ right: 6, bottom: 6 })
              .backgroundColor(this.entry.value === opt ? this.palette().accent : this.palette().bgHover)
              .fontColor(this.entry.value === opt ? Color.White : this.palette().textSecondary)
              // 选中项迁移：底色与字色一起过渡，避免整排选项同时硬切
              .animation({ duration: ANIM_FAST, curve: Curve.EaseOut })
              .onClick(() => {
                this.saveValue(opt);
              })
          }, (opt: string) => opt)
        }
        .width('100%')
        .margin({ top: 8 })
      } else if (this.entry.type === 'password') {
        Row() {
          TextInput({ text: this.entry.value })
            .height(36)
            .fontSize(13)
            .fontColor(this.palette().textPrimary)
            .placeholderColor(this.palette().textMuted)
            .backgroundColor(this.palette().bgInput)
            .borderRadius(RADIUS_SM)
            .border({ width: 1, color: this.palette().border })
            .type(InputType.Password)
            .layoutWeight(1)
            .onChange((v: string) => {
              this.editValue(v);
            })
          Button('保存')
            .height(30)
            .fontSize(12)
            .backgroundColor(this.entry.dirty ? '#D99A2B' : this.palette().accent)
            .fontColor(Color.White)
            .margin({ left: 8 })
            .onClick(() => {
              this.saveCurrent();
            })
        }
        .width('100%')
        .margin({ top: 8 })
      } else if (this.entry.type === 'text') {
        TextArea({ text: this.entry.value })
          .width('100%')
          .fontSize(13)
          .fontColor(this.palette().textPrimary)
          .backgroundColor(this.palette().bgInput)
          .borderRadius(RADIUS_SM)
          .border({ width: 1, color: this.palette().border })
          .constraintSize({ minHeight: 60, maxHeight: 200 })
          .margin({ top: 8 })
          .onChange((v: string) => {
            this.editValue(v);
          })
        Row() {
          Blank()
          Button('保存')
            .height(30)
            .fontSize(12)
            .backgroundColor(this.entry.dirty ? '#D99A2B' : this.palette().accent)
            .fontColor(Color.White)
            .onClick(() => {
              this.saveCurrent();
            })
        }
        .width('100%')
        .margin({ top: 6 })
      } else {
        // string / int / duration
        Row() {
          TextInput({ text: this.entry.value })
            .height(36)
            .fontSize(13)
            .fontColor(this.palette().textPrimary)
            .placeholderColor(this.palette().textMuted)
            .backgroundColor(this.palette().bgInput)
            .borderRadius(RADIUS_SM)
            .border({ width: 1, color: this.palette().border })
            .layoutWeight(1)
            .onChange((v: string) => {
              this.editValue(v);
            })
          Button('保存')
            .height(30)
            .fontSize(12)
            .backgroundColor(this.entry.dirty ? '#D99A2B' : this.palette().accent)
            .fontColor(Color.White)
            .margin({ left: 8 })
            .onClick(() => {
              this.saveCurrent();
            })
        }
        .width('100%')
        .margin({ top: 8 })
      }
    }
    .width('100%')
    .padding(14)
    .borderRadius(RADIUS_MD)
    .backgroundColor(this.palette().bgCard)
    .border({
      width: 1,
      color: this.entry.dirty ? '#D99A2B' : this.palette().glassBorder,
    })
    .margin({ bottom: 10 })
    .alignItems(HorizontalAlign.Start)
  }
}
