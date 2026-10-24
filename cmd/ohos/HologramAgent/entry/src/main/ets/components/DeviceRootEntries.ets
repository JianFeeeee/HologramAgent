/**
 * 设备页一级入口列表（本机 / 通道两组）。
 *
 * 从 pages/DevicePage.ets 抽出（原来是 RootEntries 一个 @Builder）。
 * 宽屏高亮由组件自己读 AppStorage 的 isWideScreen 决定，
 * 页面只负责给 activeSub 与四个打开动作。
 */

import { LOCAL_DEVICE_CAPS } from '../common/DeviceBridgeSession';
import { SUB_CAPS, SUB_GATEWAY, SUB_LIST, SUB_LOCAL } from '../common/DeviceModel';
import { NavGroup, NavRow } from './SubPage';

@Component
export struct DeviceRootEntries {
  /** 宽屏：左边一级界面（含底部导航栏），右边二级界面 */
  @StorageProp('isWideScreen') private isWide: boolean = false;
  /** 当前右栏展示的二级页面 id，用于宽屏下高亮左侧入口行 */
  @Prop activeSub: string = '';
  @Prop deviceId: string = '';
  @Prop bridgeConnected: boolean = false;
  @Prop bridgeUrl: string = '';
  @Prop loadingDevices: boolean = false;
  @Prop deviceCount: number = 0;
  onOpen?: (id: string) => void;

  private capsCount(): number {
    return LOCAL_DEVICE_CAPS.length;
  }

  private open(id: string): void {
    const cb: ((id: string) => void) | undefined = this.onOpen;
    if (cb !== undefined) {
      cb(id);
    }
  }

  build() {
    // 一级入口列表整体作为一个容器根节点：@Component 的 build() 只允许一个根，
    // 页面侧仍是 `.padding(...)` 的 Column，逐项布局与拆分前一致。
    Column() {
      NavGroup({ caption: '本机' }) {
        NavRow({
          icon: $r('app.media.ic_phone'),
          title: '本机设备',
          subtitle: this.deviceId.length > 0 ? this.deviceId : '未注册',
          value: this.bridgeConnected ? '在线' : '离线',
          selected: this.isWide && this.activeSub === SUB_LOCAL,
          onTap: () => {
            this.open(SUB_LOCAL);
          },
        })
        NavRow({
          icon: $r('app.media.ic_bolt'),
          title: '设备能力',
          subtitle: this.capsCount().toString() + ' 项能力',
          value: '',
          showDivider: false,
          selected: this.isWide && this.activeSub === SUB_CAPS,
          onTap: () => {
            this.open(SUB_CAPS);
          },
        })
      }

      NavGroup({ caption: '通道' }) {
        NavRow({
          icon: $r('app.media.ic_gateway'),
          title: '设备通道',
          subtitle: this.bridgeUrl.length > 0 ? '网关已配置' : '未配置',
          value: this.bridgeConnected ? '已连接' : '未连接',
          selected: this.isWide && this.activeSub === SUB_GATEWAY,
          onTap: () => {
            this.open(SUB_GATEWAY);
          },
        })
        NavRow({
          icon: $r('app.media.ic_devices_multi'),
          title: '接入的设备',
          subtitle: this.loadingDevices ? '加载中...' : '当前在线',
          value: this.deviceCount.toString() + ' 台',
          showDivider: false,
          selected: this.isWide && this.activeSub === SUB_LIST,
          onTap: () => {
            this.open(SUB_LIST);
          },
        })
      }
    }
    .width('100%')
    .alignItems(HorizontalAlign.Start)
  }
}
