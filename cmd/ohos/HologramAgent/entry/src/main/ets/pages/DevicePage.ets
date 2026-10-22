import { deviceBridge } from '../common/DeviceBridge';
import { connStore } from '../common/ConnStore';
import { apiClient } from '../common/ApiClient';
import { handleNavOnScroll } from '../common/NavBarController';
import { registerNavStack, unregisterNavStack } from '../common/NavStackRegistry';
import { DeviceInfo } from '../model/Model';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, WIDE_NAV_BAR_WIDTH,
  WIDE_MIN_CONTENT, ANIM_NORMAL, ANIM_ENTER } from '../common/Constants';
import { deviceGatewayUrl } from '../common/DeviceBridgeSession';
import { DeviceRootEntries } from '../components/DeviceRootEntries';
import { DeviceLocalPane, DeviceCapsPane, DeviceGatewayPane, DeviceListPane } from '../components/DevicePanes';
import { PageTopBar, NavFloatOverlay, NavFloatRow, FloatIconButton } from '../components/PageTopBar';
import { markSubPageOpen, subPageParam } from '../components/SubPage';
import { parseOnlineDevices, resolveDeviceId, SUB_CAPS, SUB_GATEWAY, SUB_LIST,
  SUB_LOCAL, SUB_NONE } from '../common/DeviceModel';

/**
 * 设备页：只做"页面壳"。
 *
 * 拆分后的分工（拆分前这里是 560 行的单文件）：
 *  - 一级入口列表（本机/通道两组）        → components/DeviceRootEntries.ets
 *  - 四个二级页面内容（本机/能力/通道/列表）→ components/DevicePanes.ets
 *  - device_id 兜底与在线设备解析          → common/DeviceModel.ets（路由 id 也在那）
 * 本文件保留：导航栈与二级页分发、授权开关、toast、设备列表拉取与生命周期。
 */
@Component
export struct DevicePage {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @StorageProp('navVisible') private navVisible: boolean = true;
  @StorageProp('currentTab') private currentTab: number = 0;
  /** 宽屏：左边一级界面（含底部导航栏），右边二级界面 */
  @StorageProp('isWideScreen') private isWide: boolean = false;
  /** 当前右栏展示的二级页面 id，用于宽屏下高亮左侧入口行 */
  @State activeSub: string = SUB_NONE;
  @StorageProp('deviceBridgeConnected') private bridgeConnected: boolean = false;
  @State bridgeUrl: string = '';
  @State bridgeToken: string = '';
  @State deviceId: string = '';
  @State authorized: boolean = false;
  @State devices: DeviceInfo[] = [];
  @State loadingDevices: boolean = false;
  @State toastMsg: string = '';
  @State toastIsError: boolean = false;
  /** 二级页面导航栈：系统返回手势/三键返回直接作用于它 */
  private navStack: NavPathStack = new NavPathStack();

  aboutToAppear(): void {
    // id 判定顺序：桥里的 id 优先于持久化的 id，都没有才生成并落盘
    this.deviceId = resolveDeviceId(deviceBridge.getDeviceId());
    this.authorized = connStore.getDeviceAuth();
    // Gateway URL derives from current connection
    const cur = connStore.getCurrentConnection();
    if (cur !== null) {
      this.bridgeUrl = deviceGatewayUrl(cur.url);
      this.bridgeToken = cur.apiKey;
    }
    this.refreshDevices();
    // 登记导航栈：返回手势由 Index.onBackPress 按当前 Tab 精确派发过来
    registerNavStack(2, this.navStack, () => {
      this.activeSub = SUB_NONE;
    });
  }

  aboutToDisappear(): void {
    unregisterNavStack(2);
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  /**
   * 打开二级页面。
   *
   * 窄屏（Stack 模式）：push 一层，整屏覆盖，系统侧滑返回可退。
   * 宽屏（Split 模式）：左栏一级列表常驻，右栏只应有一页，
   *   所以用 replace 换页而不是叠栈 —— 否则返回手势要一层层退回去。
   */
  private openSub(id: string): void {
    this.getUIContext().animateTo({ duration: ANIM_NORMAL, curve: Curve.EaseOut }, () => {
      this.activeSub = id;
    });
    if (this.isWide && this.navStack.size() > 0) {
      this.navStack.replacePathByName(id, subPageParam(id), false);
    } else {
      this.navStack.pushPathByName(id, subPageParam(id), true);
    }
  }

  /** 二级页面返回键：宽屏下左栏常驻可见，SubPageLayer 已隐藏返回键 */
  private closeSub(): void {
    this.navStack.pop();
    this.getUIContext().animateTo({ duration: ANIM_NORMAL, curve: Curve.EaseOut }, () => {
      this.activeSub = SUB_NONE;
    });
  }

  /** Toggle local authorization flag; hello 同步到网关。 */
  private toggleAuthorized(on: boolean): void {
    this.authorized = on;
    try {
      connStore.saveDeviceAuth(on);
    } catch (e) {
      // ignore persist failure
    }
    deviceBridge.updateAuthorized(on);
    this.showToast(on ? '已授权，agent 可下发能力命令' : '已取消授权', false);
  }

  private showToast(msg: string, isError: boolean): void {
    // 颜色标记必须在动画闭包外先落定，否则第一帧用的还是上一条 toast 的配色
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

  /** 拉取网关在线设备列表（经 webui 反代）。 */
  private async refreshDevices(): Promise<void> {
    this.loadingDevices = true;
    try {
      // apiClient 已自动前置 /api/v1，这里只写其后的部分，
      // 否则会拼成 /api/v1/api/v1/device/online 并 404。
      const resp = await apiClient.get('/device/online');
      if (resp.status >= 200 && resp.status < 300) {
        const parsed: Record<string, Object> = JSON.parse(resp.body) as Record<string, Object>;
        const list: DeviceInfo[] = parseOnlineDevices(parsed);
        this.getUIContext().animateTo({ duration: ANIM_ENTER, curve: Curve.EaseOut }, () => {
          this.devices = list;
        });
      } else {
        this.devices = [];
      }
    } catch (e) {
      // 网络失败保持静默，不打扰用户
    }
    this.loadingDevices = false;
  }

  build() {
    // Navigation 提供真实导航栈：系统侧滑返回 / 三键返回都会自动 pop
    Navigation(this.navStack) {
      Stack({ alignContent: Alignment.Bottom }) {
        Column() {
          Scroll() {
            Column() {
              DeviceRootEntries({
                activeSub: this.activeSub,
                deviceId: this.deviceId,
                bridgeConnected: this.bridgeConnected,
                bridgeUrl: this.bridgeUrl,
                loadingDevices: this.loadingDevices,
                deviceCount: this.devices.length,
                onOpen: (id: string) => {
                  this.openSub(id);
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

        PageTopBar({ title: '设备' })

        // 一级悬浮区：只留刷新（网关状态徽标按用户要求去掉，
        // 状态已经在页面内的网关卡片里如实展示）
        NavFloatOverlay({ tab: 2 }) {
          NavFloatRow() {
            FloatIconButton({
              icon: $r('app.media.ic_refresh'),
              onTap: () => {
                this.refreshDevices();
              },
            })
          }
        }

        this.Toast()
      }
      .width('100%')
      .height('100%')
      .backgroundColor(Color.Transparent)
    }
    .navDestination(this.SubDestination)
    // 宽屏（>=600vp）用 Split：navBar（一级列表 + 底部悬浮导航栏）常驻左栏，
    // NavDestination（二级页面）渲染在右栏，两栏同时可见。
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
    // 模式初始化与切换：Split 右栏不能空白，自动填入默认二级页面；
    // 退回 Stack 时清栈，否则会残留一个整屏覆盖的二级页面。
    .onNavigationModeChange((mode: NavigationMode) => {
      if (mode === NavigationMode.Split) {
        markSubPageOpen(true);
        if (this.navStack.size() === 0) {
          this.openSub(SUB_LOCAL);
        }
      } else {
        this.navStack.clear(false);
        this.getUIContext().animateTo({ duration: ANIM_NORMAL, curve: Curve.EaseOut }, () => {
          this.activeSub = SUB_NONE;
        });
        markSubPageOpen(false);
      }
    })
  }

  /**
   * 二级页面路由表。
   * 每个面板自带 SubPageLayer 外壳（标题/所属 Tab/返回/刷新），这里只做分发。
   */
  @Builder
  SubDestination(name: string, param: object) {
    NavDestination() {
      if (name === SUB_LOCAL) {
        DeviceLocalPane({
          deviceId: this.deviceId,
          authorized: this.authorized,
          onBack: () => {
            this.closeSub();
          },
          onToggleAuth: (on: boolean) => {
            this.toggleAuthorized(on);
          },
        })
      } else if (name === SUB_CAPS) {
        DeviceCapsPane({
          onBack: () => {
            this.closeSub();
          },
        })
      } else if (name === SUB_GATEWAY) {
        DeviceGatewayPane({
          bridgeUrl: this.bridgeUrl,
          bridgeToken: this.bridgeToken,
          bridgeConnected: this.bridgeConnected,
          onBack: () => {
            this.closeSub();
          },
          onRefresh: () => {
            this.refreshDevices();
          },
        })
      } else if (name === SUB_LIST) {
        DeviceListPane({
          devices: this.devices,
          onBack: () => {
            this.closeSub();
          },
          onRefresh: () => {
            this.refreshDevices();
          },
        })
      }
    }
    .hideTitleBar(true)
    .backgroundColor(Color.Transparent)
  }

  @Builder
  Toast() {
    if (this.toastMsg.length > 0) {
      Row() {
        Text(this.toastMsg)
          .fontSize(13)
          .fontColor(this.toastIsError ? this.palette().toastErrorText : this.palette().toastText)
          .padding({ left: 20, right: 20, top: 10, bottom: 10 })
          .borderRadius(RADIUS_MD)
          .backgroundColor(this.toastIsError ? this.palette().toastErrorBg : this.palette().toastBg)
      }
      .width('100%')
      .justifyContent(FlexAlign.End)
      .padding({ right: 20 })
      .margin({ bottom: 166 })
      .transition(TransitionEffect.OPACITY
        .combine(TransitionEffect.translate({ y: 12 }))
        .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut }))
    }
  }
}

const RADIUS_MD: number = 10;
