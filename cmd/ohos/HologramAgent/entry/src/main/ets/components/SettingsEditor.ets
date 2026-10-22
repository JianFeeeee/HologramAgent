import { apiClient } from '../common/ApiClient';
import { userMessage, noConnectionMessage } from '../common/UserError';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE } from '../common/Constants';
import { RADIUS_SM } from '../common/Constants';
import { COLOR_ERROR } from '../common/Constants';
import { ANIM_FAST, ANIM_NORMAL, ANIM_ENTER } from '../common/Constants';

/**
 * 可复用的后端配置编辑器（按 key 前缀取一段配置并就地编辑）。
 *
 * 抽出来的原因：配置项不应该全部堆在「设置 → 后端配置」一个平铺列表里。
 *  - core.* 各分类归到「核心」下一级；
 *  - plugin.<name>.* 属于插件本身，放进该插件的详情页 —— 就是这个组件的用处。
 *
 * 后端接口：
 *  - GET  /settings?prefix=<prefix>  → { settings: {k:v}, meta: {k:{...}} }
 *  - PUT  /settings                  → { key, value }（value 一律字符串）
 */

interface SettingMetaRaw {
  key: string;
  type: string;
  displayName: string;
  description: string;
  category: string;
  options: string[];
}

interface EditorEntry {
  key: string;
  displayName: string;
  description: string;
  type: string;
  options: string[];
  value: string;
  dirty: boolean;
}

interface SaveBody {
  key: string;
  value: string;
}

@Component
export struct SettingsEditor {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  /** key 前缀，例如 'plugin.ai_image.' 或 'core.llm.' */
  @Prop @Watch('onPrefixChanged') prefix: string = '';
  /** 空列表时的提示语 */
  @Prop emptyHint: string = '暂无可配置项';
  @State private entries: EditorEntry[] = [];
  @State private loading: boolean = false;
  @State private err: string = '';
  @State private toast: string = '';
  @State private toastErr: boolean = false;
  private original: Record<string, string> = {};

  aboutToAppear(): void {
    this.load();
  }

  /** 宽屏下同一个组件实例会被复用（切换插件只改 prefix），必须重新拉取 */
  private onPrefixChanged(): void {
    this.entries = [];
    this.load();
  }

  /**
   * 查询用前缀。
   *
   * 后端 GET /settings?prefix= 对 plugin.* 走的是另一条分支：
   * 它把 prefix 当作 "plugin." + 插件名 来切表，再自己拼 prefix + "." + key。
   * 所以传 'plugin.browser.'（带尾点）会被解析成插件名 "browser."，查不到表，
   * settings 返回空 —— 这就是插件详情页配置卡片空白的原因。
   * 查询必须去掉尾点，返回的 key 仍是 'plugin.browser.timeout' 这种全名，
   * 所以本地过滤/短标签依旧用带尾点的 this.prefix。
   */
  private queryPrefix(): string {
    const p: string = this.prefix;
    return p.endsWith('.') ? p.substring(0, p.length - 1) : p;
  }

  private async load(): Promise<void> {
    if (!apiClient.hasConnection()) {
      this.err = noConnectionMessage();
      return;
    }
    if (this.prefix.length === 0) {
      this.entries = [];
      return;
    }
    this.loading = true;
    this.err = '';
    try {
      const resp = await apiClient.getWithTimeout('/settings?prefix=' + this.queryPrefix(), 15000);
      const obj: Record<string, Object> = JSON.parse(resp.body) as Record<string, Object>;

      const metaStore: Record<string, SettingMetaRaw> = {};
      const rawMeta: Object | undefined = obj['meta'];
      if (rawMeta !== undefined && rawMeta !== null) {
        const mObj: Record<string, Object> = rawMeta as Record<string, Object>;
        for (const mk of Object.keys(mObj)) {
          const item: Record<string, Object> = mObj[mk] as Record<string, Object>;
          const opts: string[] = [];
          const optsRaw: Object | undefined = item['options'];
          if (optsRaw !== undefined && optsRaw !== null) {
            const oa: Object[] = optsRaw as Object[];
            for (let i = 0; i < oa.length; i++) {
              const s: string = oa[i] as string ?? '';
              if (s.length > 0) {
                opts.push(s);
              }
            }
          }
          const entryMeta: SettingMetaRaw = {
            key: item['key'] as string ?? mk,
            type: item['type'] as string ?? 'string',
            displayName: item['display_name'] as string ?? '',
            description: item['description'] as string ?? '',
            category: item['category'] as string ?? '',
            options: opts,
          };
          // 两个索引都建：
          //  - mk 是 map 的键，插件分支里后端会把它拼成 'plugin.<name>.' + 完整 key，
          //    于是变成 'plugin.ai_image.plugin.ai_image.api_key' 这种双前缀，对不上；
          //  - entryMeta.key 是定义自带的真实全名（'plugin.ai_image.api_key'），才是能匹配的那个。
          // 只用 mk 索引就会让插件配置全部退化成"无显示名、类型按 string"。
          metaStore[mk] = entryMeta;
          if (entryMeta.key.length > 0) {
            metaStore[entryMeta.key] = entryMeta;
          }
        }
      }

      const list: EditorEntry[] = [];
      this.original = {};
      const rawVals: Object | undefined = obj['settings'];
      if (rawVals !== undefined && rawVals !== null) {
        const vObj: Record<string, Object> = rawVals as Record<string, Object>;
        const keys: string[] = Object.keys(vObj).filter((k: string): boolean => {
          return k.startsWith(this.prefix);
        });
        keys.sort((a: string, b: string): number => a.localeCompare(b));
        for (let i = 0; i < keys.length; i++) {
          const k: string = keys[i];
          const raw: Object = vObj[k];
          let sv: string;
          if (typeof raw === 'string') {
            sv = raw as string;
          } else if (typeof raw === 'boolean' || typeof raw === 'number') {
            sv = String(raw);
          } else {
            sv = JSON.stringify(raw);
          }
          this.original[k] = sv;
          const meta: SettingMetaRaw | undefined = metaStore[k];
          list.push({
            key: k,
            displayName: meta !== undefined && meta.displayName.length > 0
              ? meta.displayName : shortLabel(k, this.prefix),
            description: meta !== undefined ? meta.description : '',
            type: meta !== undefined ? meta.type : 'string',
            options: meta !== undefined ? meta.options : [],
            value: sv,
            dirty: false,
          });
        }
      }
      this.entries = list;
    } catch (e) {
      this.err = userMessage('settings.load', e);
    }
    this.loading = false;
  }

  private markEntry(key: string, value: string, dirty: boolean): void {
    const next: EditorEntry[] = [];
    for (let i = 0; i < this.entries.length; i++) {
      const e: EditorEntry = this.entries[i];
      if (e.key === key) {
        next.push({
          key: e.key,
          displayName: e.displayName,
          description: e.description,
          type: e.type,
          options: e.options,
          value: value,
          dirty: dirty,
        });
      } else {
        next.push(e);
      }
    }
    this.entries = next;
  }

  private onEdit(key: string, value: string): void {
    const orig: string = this.original[key] ?? '';
    this.markEntry(key, value, value !== orig);
  }

  private latestValue(key: string): string {
    for (let i = 0; i < this.entries.length; i++) {
      if (this.entries[i].key === key) {
        return this.entries[i].value;
      }
    }
    return '';
  }

  private async save(entry: EditorEntry, newValue: string): Promise<void> {
    let payload: string = newValue.trim();
    if (entry.type === 'bool') {
      payload = newValue === 'true' ? 'true' : 'false';
    }
    try {
      const body: SaveBody = { key: entry.key, value: payload };
      await apiClient.request('/settings', 'PUT', JSON.stringify(body), 10000);
      this.original[entry.key] = payload;
      this.markEntry(entry.key, payload, false);
      this.showToast('已保存', false);
    } catch (e) {
      this.showToast(userMessage('settings.save', e), true);
    }
  }

  private showToast(msg: string, isErr: boolean): void {
    this.toast = msg;
    this.toastErr = isErr;
    setTimeout(() => {
      this.toast = '';
    }, 2000);
  }

  build() {
    Column() {
      if (this.loading && this.entries.length === 0) {
        Row() {
          LoadingProgress()
            .width(22)
            .height(22)
            .color(this.palette().accent)
        }
        .width('100%')
        .justifyContent(FlexAlign.Center)
        .padding({ top: 14, bottom: 14 })
        .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
      }

      if (this.err.length > 0) {
        Text(this.err)
          .fontSize(12)
          .fontColor(COLOR_ERROR)
          .width('100%')
          .transition(TransitionEffect.OPACITY.combine(TransitionEffect.translate({ y: -12 }))
            .animation({ duration: ANIM_ENTER, curve: Curve.EaseOut }))
      }

      if (!this.loading && this.err.length === 0 && this.entries.length === 0) {
        Text(this.emptyHint)
          .fontSize(12)
          .fontColor(this.palette().textMuted)
          .width('100%')
          .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
      }

      ForEach(this.entries, (entry: EditorEntry) => {
        this.EntryRow(entry)
      }, (entry: EditorEntry) => entry.key + '|' + entry.value + '|' + (entry.dirty ? 'd' : 'c'))

      if (this.toast.length > 0) {
        Text(this.toast)
          .fontSize(11)
          .fontColor(this.toastErr ? COLOR_ERROR : this.palette().accent)
          .margin({ top: 4 })
          .transition(TransitionEffect.OPACITY.combine(TransitionEffect.translate({ y: -12 }))
            .animation({ duration: ANIM_ENTER, curve: Curve.EaseOut }))
      }
    }
    .width('100%')
    .alignItems(HorizontalAlign.Start)
  }

  @Builder
  EntryRow(entry: EditorEntry) {
    Column() {
      Text(entry.displayName)
        .fontSize(13)
        .fontWeight(FontWeight.Medium)
        .fontColor(this.palette().textPrimary)
        .maxLines(2)
        .textOverflow({ overflow: TextOverflow.Ellipsis })
      Text(entry.key)
        .fontSize(10)
        .fontColor(this.palette().textMuted)
        .maxLines(1)
        .textOverflow({ overflow: TextOverflow.Ellipsis })
      if (entry.description.length > 0) {
        Text(entry.description)
          .fontSize(11)
          .fontColor(this.palette().textSecondary)
          .maxLines(3)
          .textOverflow({ overflow: TextOverflow.Ellipsis })
          .margin({ top: 3 })
      }

      if (entry.type === 'bool') {
        Row() {
          Text(entry.value === 'true' ? '已开启' : '已关闭')
            .fontSize(12)
            .fontColor(entry.value === 'true' ? this.palette().accent : this.palette().textMuted)
            .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
          Blank()
          Toggle({ type: ToggleType.Switch, isOn: entry.value === 'true' })
            .selectedColor(this.palette().accent)
            .onChange((on: boolean) => {
              this.save(entry, on ? 'true' : 'false');
            })
        }
        .width('100%')
        .margin({ top: 6 })
      } else if (entry.type === 'select' && entry.options.length > 0) {
        Flex({
          direction: FlexDirection.Row,
          justifyContent: FlexAlign.Start,
          alignItems: ItemAlign.Center,
          wrap: FlexWrap.Wrap,
        }) {
          ForEach(entry.options, (opt: string) => {
            Button(opt)
              .height(26)
              .fontSize(11)
              .margin({ right: 6, bottom: 6 })
              .backgroundColor(entry.value === opt ? this.palette().accent : this.palette().bgHover)
              .fontColor(entry.value === opt ? Color.White : this.palette().textSecondary)
              .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
              .onClick(() => {
                this.save(entry, opt);
              })
          }, (opt: string) => opt)
        }
        .width('100%')
        .margin({ top: 6 })
      } else {
        Row() {
          TextInput({ text: entry.value })
            .height(36)
            .fontSize(13)
            .fontColor(this.palette().textPrimary)
            .placeholderColor(this.palette().textMuted)
            .backgroundColor(this.palette().bgInput)
            .borderRadius(RADIUS_SM)
            .border({ width: 1, color: this.palette().border })
            .type(entry.type === 'password' ? InputType.Password : InputType.Normal)
            .layoutWeight(1)
            .onChange((v: string) => {
              this.onEdit(entry.key, v);
            })
          Button('保存')
            .height(30)
            .fontSize(12)
            .backgroundColor(entry.dirty ? '#D99A2B' : this.palette().accent)
            .fontColor(Color.White)
            .animation({ duration: ANIM_FAST, curve: Curve.EaseOut })
            .margin({ left: 8 })
            .onClick(() => {
              this.save(entry, this.latestValue(entry.key));
            })
        }
        .width('100%')
        .margin({ top: 6 })
      }
    }
    .width('100%')
    .padding({ top: 10, bottom: 10 })
    .border({ width: { bottom: 1 }, color: this.palette().kvBorder })
    .alignItems(HorizontalAlign.Start)
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }
}

/** 去掉前缀后的短标签：plugin.ai_image.model → model */
function shortLabel(key: string, prefix: string): string {
  const s: string = key.startsWith(prefix) ? key.substring(prefix.length) : key;
  return s.length > 0 ? s : key;
}
