import { apiClient } from '../common/ApiClient';
import { userMessage, noConnectionMessage } from '../common/UserError';
import { connStore } from '../common/ConnStore';
import { registerNavStack, unregisterNavStack } from '../common/NavStackRegistry';
import { ConnectionConfig, AppSettings } from '../model/Model';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, WIDE_NAV_BAR_WIDTH, WIDE_MIN_CONTENT, K_HAS_CONN, K_SETTINGS_SUB } from '../common/Constants';
import { RADIUS_MD, ANIM_NORMAL, ANIM_ENTER } from '../common/Constants';
import { SubPageLayer, markSubPageOpen, subPageParam } from '../components/SubPage';
import { StatusDetailContent } from '../components/StatusCards';
import { SettingsHome } from '../components/SettingsHome';
import { ConnectionsPane } from '../components/ConnectionsPane';
import { AppearancePane } from '../components/AppearancePane';
import { BackendSectionsPane, SectionEntriesPane } from '../components/BackendSettingsPane';
import {
  SettingEntry, SettingsSection, SettingMetaRaw, ParsedSettings, SectionStats,
  parseSettingsPayload, buildSections, pickInitialSection, buildEntries, withEntryMarked,
  SUB_NONE, SUB_STATUS, SUB_CONNECTIONS, SUB_APPEARANCE, SUB_BACKEND, SUB_SECTION,
} from '../common/SettingsModel';
import { statusStore } from '../common/StatusStore';
import { stageTrail } from '../common/StageTrail';

interface SaveSettingBody {
  key: string;
  value: string;
}

@Component
export struct SettingsPage {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @StorageProp('navVisible') private navVisible: boolean = true;
  @StorageProp('currentTab') private currentTab: number = 0;
  /** 宽屏：左边一级界面（含底部导航栏），右边二级界面 */
  @StorageProp('isWideScreen') private isWide: boolean = false;
  /** 当前右栏展示的二级页面 id，用于宽屏下高亮左侧入口行 */
  @State activeSub: string = SUB_NONE;
  @State connections: ConnectionConfig[] = [];
  @State currentId: string = '';
  @State themeMode: string = 'system';
  @State lang: string = 'zh';
  @State bgImage: string = '';
  @State bgOpacity: number = 0.25;

  /** 二级页面导航栈：系统返回手势/三键返回直接作用于它 */
  private navStack: NavPathStack = new NavPathStack();
  /** 未连接时是否已自动弹过连接页——避免用户关掉后又被 onNavigationModeChange 弹回来 */
  private autoOpenedConn: boolean = false;
  /** 外部请求打开某个二级页（聊天空态的“去设置连接”用） */
  @StorageProp(K_SETTINGS_SUB) @Watch('onSubRequest') private subRequest: string = '';

  // ===== backend key/value editor state =====
  @State sections: SettingsSection[] = [];
  /** plugin.* 配置项总数与涉及的插件个数（只用于在设置里给出去向提示） */
  @State pluginKeyCount: number = 0;
  @State pluginConfigCount: number = 0;
  @State activeSection: string = 'core';
  @State entries: SettingEntry[] = [];
  @State loadingSettings: boolean = false;
  @State settingsError: string = '';
  @State savingCount: number = 0;
  private metaStore: Record<string, SettingMetaRaw> = {};
  private valuesStore: Record<string, string> = {};

  @State toastMsg: string = '';
  @State toastIsError: boolean = false;

  aboutToAppear(): void {
    // 登记导航栈：返回手势由 Index.onBackPress 按当前 Tab 精确派发过来
    registerNavStack(3, this.navStack, () => {
      this.activeSub = SUB_NONE;
    });
    const s: AppSettings = connStore.getSettings();
    this.themeMode = s.theme.length > 0 ? s.theme : 'system';
    this.lang = s.lang;
    this.bgImage = s.bgImage ?? '';
    this.bgOpacity = s.bgOpacity ?? 0.25;
    this.loadConnections();
    this.loadBackendSettings();
    // 状态 Tab 已删除，运行状态并入本页：顶部摘要卡 + 二级明细页
    statusStore.init();
    // 阶段轨迹由 SSE 喂入，与状态轮询是两条独立的数据源
    stageTrail.init();
    statusStore.refresh();
  }

  aboutToDisappear(): void {
    unregisterNavStack(3);
  }

  private loadConnections(): void {
    this.connections = connStore.getConnections();
    const cur = connStore.getCurrentConnection();
    this.currentId = cur !== null ? cur.id : '';
    // 广播连接状态：聊天空态据此显示“去设置连接”
    AppStorage.setOrCreate<boolean>(K_HAS_CONN, apiClient.hasConnection());
    // 可能从聊天空态带着“打开连接页”的请求进来（本页尚未挂载时请求已写入）
    const pending: string = AppStorage.get<string>(K_SETTINGS_SUB) ?? '';
    if (pending.length > 0) {
      AppStorage.set<string>(K_SETTINGS_SUB, '');
      this.autoOpenedConn = true;
      setTimeout(() => {
        this.openSub(pending);
      }, 0);
    }
  }

  /**
   * 宽屏右栏默认该展示哪一页：没连上就把“后端连接”给出来。
   *
   * 为什么不能只靠一级入口行：入口行在列表里，用户很容易略过；
   * 而“未连接”恰恰是最需要直接看到连接表单的时刻。窄屏同理，
   * 在 onNavigationModeChange(Stack) 里会把连接页直接推到面前。
   */
  private initialSub(): string {
    return apiClient.hasConnection() ? SUB_STATUS : SUB_CONNECTIONS;
  }

  /** 外部请求打开二级页（“去设置连接”） */
  private onSubRequest(): void {
    const id: string = this.subRequest;
    if (id.length === 0) {
      return;
    }
    AppStorage.set<string>(K_SETTINGS_SUB, '');
    this.autoOpenedConn = true;
    this.openSub(id);
  }

  private palette(): ThemePalette {
    // 引用 this.isDark 建立响应式依赖：主题切换时整个组件树重渲染
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  // ===================== 二级页面导航 =====================

  /**
   * 打开二级页面。
   *
   * 窄屏（Stack 模式）：push 一层，整屏覆盖，系统侧滑返回可退。
   * 宽屏（Split 模式）：左栏一级列表常驻，右栏只应有一页，
   *   所以用 replace 换页而不是叠栈 —— 否则返回手势要一层层退回去。
   */
  private openSub(id: string): void {
    this.activeSub = id;
    if (this.isWide && this.navStack.size() > 0) {
      this.navStack.replacePathByName(id, subPageParam(id), false);
    } else {
      this.navStack.pushPathByName(id, subPageParam(id), true);
    }
  }

  /** 二级页面返回键：宽屏下左栏常驻可见，SubPageLayer 已隐藏返回键 */
  private closeSub(): void {
    this.navStack.pop();
    this.activeSub = SUB_NONE;
  }

  private currentConnName(): string {
    for (let i = 0; i < this.connections.length; i++) {
      if (this.connections[i].id === this.currentId) {
        return this.connections[i].name;
      }
    }
    return '未配置';
  }

  private themeLabel(): string {
    if (this.themeMode === 'dark') {
      return '深色';
    }
    if (this.themeMode === 'light') {
      return '浅色';
    }
    return '跟随系统';
  }

  /** 核心 + 其他（不含 plugin.*，那些在插件详情页里） */
  private coreKeyCount(): number {
    let n: number = 0;
    for (let i = 0; i < this.sections.length; i++) {
      n = n + this.sections[i].count;
    }
    return n;
  }

  // ===================== backend kv editor =====================

  private async loadBackendSettings(): Promise<void> {
    if (!apiClient.hasConnection()) {
      this.settingsError = noConnectionMessage();
      return;
    }
    this.loadingSettings = true;
    this.settingsError = '';
    try {
      const resp = await apiClient.getWithTimeout('/settings', 15000);
      const parsed: ParsedSettings = parseSettingsPayload(resp.body);
      this.metaStore = parsed.meta;
      this.valuesStore = parsed.values;
      const stats: SectionStats = buildSections(this.valuesStore, this.metaStore);
      this.sections = stats.sections;
      this.pluginKeyCount = stats.pluginKeyCount;
      this.pluginConfigCount = stats.pluginConfigCount;
      this.selectSection(pickInitialSection(this.sections));
    } catch (e) {
      this.settingsError = userMessage('settings.load', e);
    }
    this.loadingSettings = false;
  }

  private selectSection(secId: string): void {
    this.activeSection = secId;
    this.entries = buildEntries(this.valuesStore, this.metaStore, secId);
  }

  private findEntry(key: string): SettingEntry | undefined {
    for (let i = 0; i < this.entries.length; i++) {
      if (this.entries[i].key === key) {
        return this.entries[i];
      }
    }
    return undefined;
  }

  /** Persist one key via PUT /settings. bool is sent as "true"/"false" strings like the GUI. */
  private async saveSetting(key: string, newValue: string): Promise<void> {
    const entry: SettingEntry | undefined = this.findEntry(key);
    if (entry === undefined) {
      return;
    }
    let payload: string = newValue.trim();
    if (entry.type === 'bool') {
      payload = newValue === 'true' ? 'true' : 'false';
    }
    this.savingCount = this.savingCount + 1;
    try {
      const body: SaveSettingBody = { key: entry.key, value: payload };
      await apiClient.request('/settings', 'PUT', JSON.stringify(body), 10000);
      this.valuesStore[entry.key] = payload;
      this.markEntry(entry.key, payload, false);
      this.showToast('已保存: ' + entry.displayName, false);
    } catch (e) {
      this.showToast(userMessage('settings.save', e), true);
    }
    this.savingCount = this.savingCount - 1;
  }

  /** 已保存：读当前编辑值再落库（输入类控件的"保存"按钮） */
  private saveCurrentValue(key: string): void {
    this.saveSetting(key, this.currentValueOf(key));
  }

  private markEntry(key: string, value: string, dirty: boolean): void {
    this.entries = withEntryMarked(this.entries, key, value, dirty);
  }

  private updateEntryValue(key: string, value: string): void {
    const original: string = this.valuesStore[key] ?? '';
    this.markEntry(key, value, value !== original);
  }

  private currentValueOf(key: string): string {
    // read latest edited value from entries store (markEntry keeps copies)
    const e: SettingEntry | undefined = this.findEntry(key);
    return e !== undefined ? e.value : '';
  }

  private showToast(msg: string, isError: boolean): void {
    // 颜色标记必须在 animateTo 之外先定好，否则第一帧会用上一条 toast 的配色
    this.toastIsError = isError;
    this.getUIContext().animateTo({ duration: ANIM_NORMAL, curve: Curve.EaseOut }, () => {
      this.toastMsg = msg;
    });
    setTimeout(() => {
      this.getUIContext().animateTo({ duration: ANIM_NORMAL, curve: Curve.EaseOut }, () => {
        this.toastMsg = '';
      });
    }, 2500);
  }

  build() {
    // Navigation 提供真实导航栈：系统侧滑返回 / 三键返回 / 无障碍返回
    // 都会自动 pop 当前 NavDestination，无需自己拦 onBackPress。
    //
    // 宽屏（>=600vp）改用 Split 模式：navBar（这里的一级入口列表，含底部悬浮导航栏）
    // 常驻左栏，NavDestination（二级页面）渲染在右栏，两栏同时可见。
    Navigation(this.navStack) {
      SettingsHome({
        connCount: this.connections.length,
        connName: this.currentConnName(),
        sectionCount: this.sections.length,
        coreKeys: this.coreKeyCount(),
        busy: this.loadingSettings,
        errorText: this.settingsError,
        themeLabel: this.themeLabel(),
        activeSub: this.activeSub,
        isWide: this.isWide,
        savingCount: this.savingCount,
        toastMsg: this.toastMsg,
        toastIsError: this.toastIsError,
        onOpen: (id: string) => {
          this.openSub(id);
        },
      })
    }
    .navDestination(this.SubDestination)
    .mode(this.isWide ? NavigationMode.Split : NavigationMode.Stack)
    .navBarPosition(NavBarPosition.Start)
    .navBarWidth(WIDE_NAV_BAR_WIDTH)
    .minContentWidth(WIDE_MIN_CONTENT)
    .hideTitleBar(true)
    .hideToolBar(true)
    .hideBackButton(true)
    .width('100%')
    .height('100%')
    // Split 模式下 navBar 常驻可见，onNavBarStateChange 不再触发，
    // 所以在这里统一维护"横滑锁"：宽屏一律锁住（靠点击导航栏切换 Tab，
    // 避免在右栏操作时误触发主 Tab 切换）。
    .onNavBarStateChange((isVisible: boolean) => {
      markSubPageOpen(this.isWide || !isVisible);
    })
    // 模式初始化与切换的唯一钩子：Split 右栏不能空白，自动填入默认二级页面；
    // 退回 Stack 时清栈，否则会残留一个整屏覆盖的二级页面。
    .onNavigationModeChange((mode: NavigationMode) => {
      if (mode === NavigationMode.Split) {
        markSubPageOpen(true);
        if (this.navStack.size() === 0) {
          this.openSub(this.initialSub());
        }
      } else {
        this.navStack.clear(false);
        this.activeSub = SUB_NONE;
        markSubPageOpen(false);
        // 窄屏：未配置后端时直接推连接页，保证“设置里一定能找到连后端的入口”。
        if (!this.autoOpenedConn && !apiClient.hasConnection()) {
          this.autoOpenedConn = true;
          this.openSub(SUB_CONNECTIONS);
        }
      }
    })
  }

  /**
   * 二级页面路由表：name 由 pushPathByName 传入。
   * 各页面的外壳（SubPageLayer）与其内容一起放在对应组件里，
   * 这里只做 id → 组件的分发。
   */
  @Builder
  SubDestination(name: string, param: object) {
    NavDestination() {
      if (name === SUB_STATUS) {
        SubPageLayer({
          title: '运行状态',
          tab: 3,
          onBack: () => {
            this.closeSub();
          },
          showRefresh: true,
          onRefresh: () => {
            statusStore.refresh();
          },
        }) {
          StatusDetailContent()
        }
      } else if (name === SUB_CONNECTIONS) {
        ConnectionsPane({
          connections: $connections,
          currentId: $currentId,
          onBack: () => {
            this.closeSub();
          },
          onToast: (msg: string, isError: boolean) => {
            this.showToast(msg, isError);
          },
        })
      } else if (name === SUB_APPEARANCE) {
        AppearancePane({
          themeMode: $themeMode,
          bgImage: $bgImage,
          bgOpacity: $bgOpacity,
          onBack: () => {
            this.closeSub();
          },
          onToast: (msg: string, isError: boolean) => {
            this.showToast(msg, isError);
          },
        })
      } else if (name === SUB_BACKEND) {
        BackendSectionsPane({
          sections: this.sections,
          pluginKeyCount: this.pluginKeyCount,
          pluginConfigCount: this.pluginConfigCount,
          busy: this.loadingSettings,
          errorText: this.settingsError,
          activeSection: this.activeSection,
          highlightRows: this.isWide && this.activeSub === SUB_SECTION,
          onBack: () => {
            this.closeSub();
          },
          onRefresh: () => {
            this.loadBackendSettings();
          },
          onOpenSection: (id: string) => {
            this.selectSection(id);
            this.openSub(SUB_SECTION);
          },
        })
      } else if (name === SUB_SECTION) {
        SectionEntriesPane({
          sections: this.sections,
          activeSection: this.activeSection,
          entries: this.entries,
          busy: this.loadingSettings,
          onBack: () => {
            this.closeSub();
          },
          onRefresh: () => {
            this.loadBackendSettings();
          },
          onSaveValue: (key: string, value: string) => {
            this.saveSetting(key, value);
          },
          onSaveCurrent: (key: string) => {
            this.saveCurrentValue(key);
          },
          onEdit: (key: string, value: string) => {
            this.updateEntryValue(key, value);
          },
        })
      }
    }
    .hideTitleBar(true)
    .backgroundColor(Color.Transparent)
  }
}
