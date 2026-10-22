import { apiClient } from '../common/ApiClient';
import { userMessage } from '../common/UserError';
import { handleNavOnScroll } from '../common/NavBarController';
import { registerNavStack, unregisterNavStack } from '../common/NavStackRegistry';
import { PluginRow, PluginDetail, emptyPluginDetail } from '../model/Model';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, WIDE_NAV_BAR_WIDTH, WIDE_MIN_CONTENT } from '../common/Constants';
import { ANIM_NORMAL, ANIM_ENTER } from '../common/Constants';
import { PageTopBar, NavFloatOverlay, NavFloatRow, FloatIconButton } from '../components/PageTopBar';
import { SubPageLayer, markSubPageOpen, subPageParam } from '../components/SubPage';
import { PluginDetailPane } from '../components/PluginDetailPane';
import { PluginListView } from '../components/PluginListView';
import { PluginInstallForm } from '../components/PluginsOverlays';
import { ToastBar } from '../components/ToastBar';
import { fetchPluginRows, fetchPluginDetail } from '../common/PluginApi';

/** 二级页面标识：插件详情 */
const SUB_NONE: string = '';
const SUB_DETAIL: string = 'detail';

interface InstallBody {
  url: string;
}

@Component
export struct PluginsPage {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @StorageProp('navVisible') private navVisible: boolean = true;
  @StorageProp('currentTab') private currentTab: number = 0;
  /** 宽屏：左边插件列表（含底部导航栏），右边插件详情 */
  @StorageProp('isWideScreen') private isWide: boolean = false;
  @State plugins: PluginRow[] = [];
  @State loading: boolean = false;
  @State showInstallForm: boolean = false;
  @State installUrl: string = '';
  @State toastMsg: string = '';
  @State toastIsError: boolean = false;
  /** 当前查看详情的插件名，用于宽屏下高亮左侧列表项 */
  @State activeName: string = '';
  /** 详情页数据（GET /plugins/{name}） */
  @State detail: PluginDetail = emptyPluginDetail();
  @State detailLoading: boolean = false;
  @State detailError: string = '';
  /** 二级页面导航栈：系统返回手势/三键返回直接作用于它 */
  private navStack: NavPathStack = new NavPathStack();

  aboutToAppear(): void {
    // 登记导航栈：返回手势由 Index.onBackPress 按当前 Tab 精确派发过来
    registerNavStack(1, this.navStack, () => {
      this.activeName = '';
    });
    this.loadPlugins();
  }

  aboutToDisappear(): void {
    unregisterNavStack(1);
  }

  private palette(): ThemePalette {
    // 引用 this.isDark 建立响应式依赖：主题切换时整个组件树重渲染
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  /** 取数口径见 common/PluginApi.ets；这里只负责把结果落到 @State。 */
  private async loadPlugins(): Promise<void> {
    if (!apiClient.hasConnection()) {
      return;
    }
    this.loading = true;
    try {
      const rows: PluginRow[] = await fetchPluginRows();
      // 列表整体重建（ForEach key 含 loaded/disabled，启停会整行重挂载）：
      // 放进 animateTo 让新旧行走 transition 交叉淡入，而不是硬切一帧。
      this.getUIContext().animateTo({ duration: ANIM_ENTER, curve: Curve.EaseOut }, () => {
        this.plugins = rows;
      });
    } catch (e) {
      this.showToast(userMessage('plugins.load', e), true);
    }
    this.getUIContext().animateTo({ duration: ANIM_ENTER, curve: Curve.EaseOut }, () => {
      this.loading = false;
    });
  }

  private async togglePlugin(plugin: PluginRow): Promise<void> {
    const name: string = plugin.name;
    const action: string = plugin.disabled ? 'enable' : 'disable';
    try {
      await apiClient.post('/plugins/' + name + '/' + action, null);
      this.showToast('已' + (plugin.disabled ? '启用' : '禁用') + '插件: ' + name, false);
      this.loadPlugins();
    } catch (e) {
      this.showToast(userMessage('plugins.toggle', e), true);
    }
  }

  private async removePlugin(plugin: PluginRow): Promise<void> {
    const name: string = plugin.name;
    try {
      await apiClient.request('/plugins/' + name, 'DELETE', '', 8000);
      this.showToast('已卸载插件: ' + name, false);
      this.loadPlugins();
    } catch (e) {
      this.showToast(userMessage('plugins.remove', e), true);
    }
  }

  private async installPlugin(): Promise<void> {
    const url: string = this.installUrl.trim();
    if (url.length === 0) {
      return;
    }
    try {
      const bodyObj: InstallBody = { url: url };
      // 后端没有 /plugins/install 这个路由：安装就是 POST /plugins，body 带 url。
      await apiClient.post('/plugins', bodyObj);
      this.showToast('安装请求已发送', false);
      this.installUrl = '';
      this.getUIContext().animateTo({ duration: ANIM_ENTER, curve: Curve.EaseOut }, () => {
        this.showInstallForm = false;
      });
      this.loadPlugins();
    } catch (e) {
      this.showToast(userMessage('plugins.install', e), true);
    }
  }

  // ===================== 二级页面：插件详情 =====================

  /**
   * 打开插件详情。
   *
   * 窄屏（Stack）：push 一层整屏覆盖，系统侧滑返回可退。
   * 宽屏（Split）：左栏列表常驻，右栏换页而不叠栈。
   */
  private openDetail(name: string): void {
    this.activeName = name;
    this.loadDetail(name);
    if (this.isWide && this.navStack.size() > 0) {
      this.navStack.replacePathByName(SUB_DETAIL, subPageParam(SUB_DETAIL), false);
    } else {
      this.navStack.pushPathByName(SUB_DETAIL, subPageParam(SUB_DETAIL), true);
    }
  }

  private closeDetail(): void {
    this.navStack.pop();
    this.activeName = '';
  }

  /**
   * 详情数据落地。
   * 内置插件不在 /plugins 里，取不到详情时退回用列表已有的信息。
   */
  private async loadDetail(name: string): Promise<void> {
    this.detailLoading = true;
    this.detailError = '';
    try {
      const d: PluginDetail = await fetchPluginDetail(name);
      // 详情字段一次性落地：条件卡片在 animateTo 帧内挂载，V1 给它们默认透明度过渡
      this.getUIContext().animateTo({ duration: ANIM_ENTER, curve: Curve.EaseOut }, () => {
        this.detail = d;
      });
    } catch (e) {
      // 内置插件没有清单，属于预期情况，不当成错误刷红
      const row: PluginRow | null = this.findRow(name);
      const d: PluginDetail = emptyPluginDetail();
      d.name = name;
      if (row !== null) {
        d.version = row.version ?? '';
        d.description = row.description ?? '';
      }
      const errText: string = (row !== null && row.external) ? userMessage('plugins.detail', e) : '';
      this.getUIContext().animateTo({ duration: ANIM_ENTER, curve: Curve.EaseOut }, () => {
        this.detail = d;
        this.detailError = errText;
      });
    }
    this.getUIContext().animateTo({ duration: ANIM_ENTER, curve: Curve.EaseOut }, () => {
      this.detailLoading = false;
    });
  }

  private findRow(name: string): PluginRow | null {
    for (let i = 0; i < this.plugins.length; i++) {
      if (this.plugins[i].name === name) {
        return this.plugins[i];
      }
    }
    return null;
  }

  /** 详情页正在展示的那一行（供二级页面里的操作按钮使用） */
  private activeRow(): PluginRow | null {
    return this.findRow(this.activeName);
  }

  private async reloadPlugins(): Promise<void> {
    try {
      await apiClient.post('/plugins/reload', null);
      this.showToast('插件已重载', false);
      this.loadPlugins();
    } catch (e) {
      this.showToast(userMessage('plugins.reload', e), true);
    }
  }

  private showToast(msg: string, isError: boolean): void {
    // 颜色标志必须在 animateTo 闭包之外先落地，否则第一帧配色是上一次的
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
    // 宽屏（>=600vp）走 Split：navBar（左栏 = 插件列表 + 底部导航栏）常驻，
    // NavDestination（右栏 = 插件详情）与之并列；窄屏则是整屏覆盖的二级页面。
    Navigation(this.navStack) {
      Stack({ alignContent: Alignment.Bottom }) {
        Column() {
          Scroll() {
            Column() {
              PluginListView({
                plugins: this.plugins,
                busy: this.loading,
                activeName: this.activeName,
                hasConn: apiClient.hasConnection(),
                onSelect: (name: string) => {
                  this.openDetail(name);
                },
              })
            }
            .width('100%')
            .padding({ left: 16, right: 16, top: 76, bottom: 174 })
          }
          .width('100%')
          .height('100%')
          .scrollBar(BarState.Off)
          .align(Alignment.Top)
          .onDidScroll((xOffset: number, yOffset: number, state: ScrollState) => {
            handleNavOnScroll(state);
          })
        }
        .width('100%')
        .height('100%')

        // 顶栏遮罩（自身撑满并顶部对齐，全链路 hitTest None，触摸完全穿透到滚动区）
        PageTopBar({ title: '插件' })

        // 悬浮操作区：安装（悬浮主按钮）+ 重载。
        // 安装表单以悬浮卡形式浮在按钮上方，不再占用列表顶部一行。
        NavFloatOverlay({ tab: 1 }) {
          if (this.showInstallForm) {
            PluginInstallForm({
              url: this.installUrl,
              onUrlChange: (v: string) => {
                this.installUrl = v;
              },
              onInstall: () => {
                this.installPlugin();
              },
            })
          }
          NavFloatRow() {
            FloatIconButton({
              icon: $r('app.media.ic_reload'),
              onTap: () => {
                this.reloadPlugins();
              },
            })
            Button() {
              Image($r('app.media.ic_plus'))
                .width(20)
                .height(20)
                .fillColor(Color.White)
                .rotate({ angle: this.showInstallForm ? 45 : 0 })
                .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
            }
              .width(50)
              .height(50)
              .type(ButtonType.Circle)
              .backgroundColor(this.palette().accent)
              .shadow({ radius: 24, color: this.palette().shadow, offsetY: 8 })
              .onClick(() => {
                this.getUIContext().animateTo({ duration: ANIM_ENTER, curve: Curve.EaseOut }, () => {
                  this.showInstallForm = !this.showInstallForm;
                });
              })
          }
        }

        ToastBar({ msg: this.toastMsg, isError: this.toastIsError, bordered: true })
      }
      .width('100%')
      .height('100%')
      .backgroundColor(Color.Transparent)
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
    // Split 模式下 navBar 常驻，此回调不再触发；宽屏一律锁住主 Tab 横滑
    .onNavBarStateChange((isVisible: boolean) => {
      markSubPageOpen(this.isWide || !isVisible);
    })
    // 模式初始化与切换：Split 右栏不能空白，自动选中第一个插件；
    // 退回 Stack 时清栈，否则会残留一个整屏覆盖的详情页。
    .onNavigationModeChange((mode: NavigationMode) => {
      if (mode === NavigationMode.Split) {
        markSubPageOpen(true);
        if (this.navStack.size() === 0 && this.plugins.length > 0) {
          this.openDetail(this.plugins[0].name);
        }
      } else {
        this.navStack.clear(false);
        this.activeName = '';
        markSubPageOpen(false);
      }
    })
  }

  /** 二级页面路由表：插件详情 */
  @Builder
  SubDestination(name: string, param: object) {
    NavDestination() {
      if (name === SUB_DETAIL) {
        SubPageLayer({
          title: this.activeName.length > 0 ? this.activeName : '插件详情',
          tab: 1,
          onBack: () => {
            this.closeDetail();
          },
          showRefresh: true,
          onRefresh: () => {
            this.loadDetail(this.activeName);
          },
        }) {
          PluginDetailPane({
            detail: this.detail,
            activeName: this.activeName,
            busy: this.detailLoading,
            errorText: this.detailError,
            row: this.activeRow() ?? undefined,
            onToggle: () => {
              const r: PluginRow | null = this.activeRow();
              if (r !== null) {
                this.togglePlugin(r);
              }
            },
            onRemove: () => {
              const r: PluginRow | null = this.activeRow();
              if (r !== null) {
                this.removePlugin(r);
              }
            },
          })
        }
      }
    }
    .hideTitleBar(true)
    .backgroundColor(Color.Transparent)
  }
}
