/**
 * 设备页的四个二级页面内容：本机设备 / 设备能力 / 设备通道 / 接入的设备。
 *
 * 从 pages/DevicePage.ets 抽出（原来是 LocalDeviceContent / CapsContent /
 * GatewayContent / OnlineDevicesContent 四个 @Builder + KvRow）。
 * 每个面板自带 SubPageLayer 外壳（标题、所属 Tab、返回、刷新），
 * 页面只保留路由分发 —— 与 SettingsPage 拆出的三个 Pane 同一套做法。
 */

import { DeviceInfo } from '../model/Model';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, ANIM_FAST, ANIM_NORMAL,
  ANIM_ENTER } from '../common/Constants';
import { LOCAL_DEVICE_CAPS } from '../common/DeviceBridgeSession';
import { MotionBase } from './MotionBase';
import { PlainCard, SubPageLayer } from './SubPage';

/** 通用 KV 行（面板之间共用） */
@Component
export struct DeviceKvRow {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop label: string = '';
  @Prop value: string = '';

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  build() {
    Row() {
      Text(this.label)
        .fontSize(13)
        .fontColor(this.palette().textSecondary)
      Blank()
      Text(this.value)
        .fontSize(13)
        .fontColor(this.palette().textPrimary)
        .maxLines(1)
        .textOverflow({ overflow: TextOverflow.Ellipsis })
    }
    .width('100%')
    .padding({ top: 8, bottom: 8 })
    .border({ width: { bottom: 1 }, color: this.palette().kvBorder })
  }
}

/** 二级：本机设备（基本信息 + 远程控制授权） */
@Component
export struct DeviceLocalPane {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop deviceId: string = '';
  @Prop authorized: boolean = false;
  onBack?: () => void;
  onToggleAuth?: (on: boolean) => void;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  build() {
    SubPageLayer({
      title: '本机设备',
      tab: 2,
      onBack: () => {
        const cb: (() => void) | undefined = this.onBack;
        if (cb !== undefined) {
          cb();
        }
      },
    }) {
      PlainCard({ caption: '基本信息' }) {
        DeviceKvRow({ label: '设备 ID', value: this.deviceId.length > 0 ? this.deviceId : '未注册' })
        DeviceKvRow({ label: '名称', value: 'HologramAgent OHOS' })
        DeviceKvRow({ label: '类型', value: 'phone' })
      }

      PlainCard({ caption: '权限控制' }) {
        Row() {
          Column({ space: 2 }) {
            Text('允许 agent 控制本机')
              .fontSize(14)
              .fontColor(this.palette().textPrimary)
            Text('授权后 agent 可调用下方能力；截屏仅捕获本应用画面，剪贴板读取需系统弹窗确认。')
              .fontSize(11)
              .fontColor(this.palette().textMuted)
              .margin({ top: 4 })
          }
          .layoutWeight(1)
          .alignItems(HorizontalAlign.Start)

          Toggle({ type: ToggleType.Switch, isOn: this.authorized })
            .selectedColor(this.palette().accent)
            .onChange((on: boolean) => {
              const cb: ((on: boolean) => void) | undefined = this.onToggleAuth;
              if (cb !== undefined) {
                cb(on);
              }
            })
        }
        .width('100%')
        .alignItems(VerticalAlign.Center)

        Row() {
          Circle({ width: 8, height: 8 })
            .fill(this.authorized ? '#17A964' : '#E84026')
            .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
            .margin({ right: 8 })
          Text(this.authorized ? '已授权 — agent 可远程调用能力' : '未授权 — agent 将拒绝远程命令')
            .fontSize(12)
            .fontColor(this.authorized ? '#17A964' : this.palette().textMuted)
            .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
        }
        .width('100%')
        .margin({ top: 12 })
        .padding({ left: 4 })
      }
    }
  }
}

/** 二级：设备能力（本机能被 agent 调用的能力清单） */
@Component
export struct DeviceCapsPane {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  onBack?: () => void;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  build() {
    SubPageLayer({
      title: '设备能力',
      tab: 2,
      onBack: () => {
        const cb: (() => void) | undefined = this.onBack;
        if (cb !== undefined) {
          cb();
        }
      },
    }) {
      PlainCard({ caption: '能力清单' }) {
        Text('agent 通过设备桥可调用的本机能力：')
          .fontSize(12)
          .fontColor(this.palette().textMuted)
          .margin({ bottom: 10 })

        Flex({ wrap: FlexWrap.Wrap }) {
          ForEach(LOCAL_DEVICE_CAPS, (cap: string) => {
            Text(cap)
              .fontSize(11)
              .fontColor(this.palette().accent)
              .padding({ left: 8, right: 8, top: 4, bottom: 4 })
              .borderRadius(999)
              .backgroundColor(this.palette().bgHover)
              .border({ width: 1, color: this.palette().glassBorder })
              .margin({ right: 6, bottom: 6 })
          }, (cap: string) => cap)
        }
        .width('100%')
      }
    }
  }
}

/** 二级：设备通道（网关地址 / Token / 连接状态） */
@Component
export struct DeviceGatewayPane {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop bridgeUrl: string = '';
  @Prop bridgeToken: string = '';
  @Prop bridgeConnected: boolean = false;
  onBack?: () => void;
  onRefresh?: () => void;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  build() {
    SubPageLayer({
      title: '设备通道',
      tab: 2,
      onBack: () => {
        const cb: (() => void) | undefined = this.onBack;
        if (cb !== undefined) {
          cb();
        }
      },
    }) {
      PlainCard({ caption: '连接信息' }) {
        DeviceKvRow({ label: '网关地址', value: this.bridgeUrl.length > 0 ? this.bridgeUrl : '-' })
        DeviceKvRow({ label: 'Token', value: this.bridgeToken.length > 0 ? '已从连接继承' : '未配置' })
        DeviceKvRow({ label: '状态', value: this.bridgeConnected ? '已连接' : '未连接' })
      }

      PlainCard({ caption: '操作' }) {
        Row() {
          Blank()
          MotionBase({ pressEnabled: true, fillWidth: false }) {
            Button('刷新设备')
              .height(34)
              .fontSize(12)
              .backgroundColor(Color.Transparent)
              .border({ width: 1, color: this.palette().btnGhostBorder })
              .fontColor(this.palette().textSecondary)
              .onClick(() => {
                const cb: (() => void) | undefined = this.onRefresh;
                if (cb !== undefined) {
                  cb();
                }
              })
          }
        }
        .width('100%')

        Text('设备通道由应用前台生命周期统一管理；切换连接配置后会自动使用新地址和 Token。')
          .fontSize(11)
          .fontColor(this.palette().textMuted)
          .margin({ top: 10 })
      }
    }
  }
}

/** 二级：接入的设备（网关侧在线设备列表） */
@Component
export struct DeviceListPane {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop devices: DeviceInfo[] = [];
  onBack?: () => void;
  onRefresh?: () => void;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  build() {
    SubPageLayer({
      title: '接入的设备',
      tab: 2,
      showRefresh: true,
      onBack: () => {
        const cb: (() => void) | undefined = this.onBack;
        if (cb !== undefined) {
          cb();
        }
      },
      onRefresh: () => {
        const cb: (() => void) | undefined = this.onRefresh;
        if (cb !== undefined) {
          cb();
        }
      },
    }) {
      DeviceOnlineList({ devices: this.devices })
    }
  }
}

/** 在线设备列表本体（抽出来只是为了让 DeviceListPane 的 build 更短） */
@Component
struct DeviceOnlineList {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop devices: DeviceInfo[] = [];

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  build() {
    Column() {
      if (this.devices.length === 0) {
        Text('暂无其他设备。电脑 GUI 或 CLI 连接同一网关后会出现在这里。')
          .fontSize(12)
          .fontColor(this.palette().textMuted)
          .padding({ left: 4 })
          .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
      }

      ForEach(this.devices, (dev: DeviceInfo) => {
        // PlainCard 是自定义组件，transition 不能直接挂在它上面（会生成 __Common__ 包装），
        // 所以用一个无 padding、满宽的 Column 承载入场动画，布局不受影响。
        Column() {
          PlainCard({ caption: '' }) {
            Row() {
              Circle({ width: 8, height: 8 })
                .fill(dev.online ? '#17A964' : '#77809A')
                .margin({ right: 10 })
              Column() {
                Text(dev.name.length > 0 ? dev.name : dev.deviceId)
                  .fontSize(14)
                  .fontColor(this.palette().textPrimary)
                Text(dev.kind + (dev.authorized ? ' · 已授权' : ' · 未授权'))
                  .fontSize(11)
                  .fontColor(dev.authorized ? '#17A964' : this.palette().textMuted)
              }
              .layoutWeight(1)
              .alignItems(HorizontalAlign.Start)

              Text(dev.caps.length.toString() + ' 能力')
                .fontSize(10)
                .fontColor(this.palette().textMuted)
            }
            .width('100%')
          }
        }
        .width('100%')
        .transition(TransitionEffect.OPACITY
          .combine(TransitionEffect.translate({ y: 12 }))
          .animation({ duration: ANIM_ENTER, curve: Curve.EaseOut }))
      }, (dev: DeviceInfo) => dev.deviceId + dev.online.toString())
    }
    .width('100%')
    .alignItems(HorizontalAlign.Start)
  }
}
