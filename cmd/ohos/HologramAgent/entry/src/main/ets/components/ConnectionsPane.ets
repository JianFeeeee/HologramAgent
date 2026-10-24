/**
 * 「后端连接」二级页面。
 *
 * 从 pages/SettingsPage.ets 抽出：连接列表 UI、增删改表单与其状态、
 * 以及切换连接后必须做的连带动作（刷新 ApiClient / 重启前台桥）
 * 都属于这一个功能域，收在一个组件里。
 *
 * connections / currentId 用 @Link 与一级页面共享：一级页的入口行
 * 要显示"几个连接配置"和当前连接名，两边必须是同一份数据。
 */

import { apiClient } from '../common/ApiClient';
import { connStore } from '../common/ConnStore';
import { restartForegroundBridge } from '../common/DeviceBridgeSession';
import { ConnectionConfig } from '../model/Model';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, RADIUS_MD, RADIUS_SM, K_HAS_CONN } from '../common/Constants';
import { SubPageLayer, PlainCard } from './SubPage';
import { common } from '@kit.AbilityKit';

@Component
export struct ConnectionsPane {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Link connections: ConnectionConfig[];
  @Link currentId: string;
  onBack?: () => void;
  onToast?: (msg: string, isError: boolean) => void;
  @State showAddForm: boolean = false;
  @State addFormVisible: boolean = false;
  /**
   * 表单当前在编辑哪条连接：空串表示新建。
   *
   * 之前只有"添加"入口，ConnStore.updateConnection 写好了却没有任何调用者，
   * 于是地址填错的连接只能删掉重建（API Key 也得重敲）。同一套表单
   * 靠这个 id 区分保存走 add 还是 update。
   */
  @State editingId: string = '';
  @State editUrl: string = '';
  @State editApiKey: string = '';
  @State editName: string = '';

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  private toast(msg: string, isError: boolean): void {
    const cb: ((m: string, e: boolean) => void) | undefined = this.onToast;
    if (cb !== undefined) {
      cb(msg, isError);
    }
  }

  /** 连接变更后广播状态：聊天空态据此隐藏“去设置连接”入口。 */
  private syncConnFlag(): void {
    AppStorage.setOrCreate<boolean>(K_HAS_CONN, apiClient.hasConnection());
  }

  private currentConnName(): string {
    for (let i = 0; i < this.connections.length; i++) {
      if (this.connections[i].id === this.currentId) {
        return this.connections[i].name;
      }
    }
    return '未配置';
  }

  private selectConnection(id: string): void {
    connStore.setCurrent(id).then(() => {
      const cur: ConnectionConfig | null = connStore.getCurrentConnection();
      if (cur !== null) {
        apiClient.setConnection(cur);
      }
      this.syncConnFlag();
      this.connections = connStore.getConnections();
      restartForegroundBridge(getContext(this) as common.UIAbilityContext);
      this.toast('已切换连接', false);
    });
  }

  private addConnection(): void {
    const name: string = this.editName.trim();
    const url: string = this.editUrl.trim();
    const apiKey: string = this.editApiKey.trim();
    if (name.length === 0 || url.length === 0) {
      this.toast('名称和地址不能为空', true);
      return;
    }
    if (this.editingId.length > 0) {
      this.updateConnection(this.editingId, name, url, apiKey);
      return;
    }
    connStore.addConnection(name, url, apiKey).then(() => {
      this.closeConnForm();
      const cur = connStore.getCurrentConnection();
      if (cur !== null) {
        apiClient.setConnection(cur);
      }
      this.syncConnFlag();
      this.connections = connStore.getConnections();
      restartForegroundBridge(getContext(this) as common.UIAbilityContext);
      this.toast('连接已添加', false);
    });
  }

  /**
   * 保存对已有连接的修改。
   *
   * 修改当前生效的连接后必须重新 setConnection：ApiClient 持有的是
   * ConnectionConfig 的引用快照，不刷新的话后续请求还会打到旧地址。
   */
  private updateConnection(id: string, name: string, url: string, apiKey: string): void {
    connStore.updateConnection(id, name, url, apiKey).then(() => {
      this.closeConnForm();
      const cur = connStore.getCurrentConnection();
      if (cur !== null) {
        apiClient.setConnection(cur);
      }
      this.syncConnFlag();
      this.connections = connStore.getConnections();
      restartForegroundBridge(getContext(this) as common.UIAbilityContext);
      this.toast('连接已更新', false);
    });
  }

  /** 打开表单：id 为空是新建，非空是编辑并回填原值（API Key 一并带出，避免用户重敲）。 */
  private openConnForm(conn: ConnectionConfig | null): void {
    this.showAddForm = true;
    this.addFormVisible = false;
    if (conn === null) {
      this.editingId = '';
      this.editName = '';
      this.editUrl = '';
      this.editApiKey = '';
    } else {
      this.editingId = conn.id;
      this.editName = conn.name;
      this.editUrl = conn.url;
      this.editApiKey = conn.apiKey;
    }
    setTimeout(() => {
      this.addFormVisible = true;
    }, 30);
  }

  private closeConnForm(): void {
    this.showAddForm = false;
    this.addFormVisible = false;
    this.editingId = '';
    this.editName = '';
    this.editUrl = '';
    this.editApiKey = '';
  }

  private deleteConnection(id: string): void {
    connStore.deleteConnection(id).then(() => {
      this.connections = connStore.getConnections();
      const cur: ConnectionConfig | null = connStore.getCurrentConnection();
      if (cur !== null) {
        apiClient.setConnection(cur);
      } else {
        apiClient.clearConnection();
      }
      this.syncConnFlag();
      restartForegroundBridge(getContext(this) as common.UIAbilityContext);
      this.toast('连接已删除', false);
    });
  }

  build() {
    SubPageLayer({
      title: '后端连接',
      tab: 3,
      onBack: () => {
        const cb: (() => void) | undefined = this.onBack;
        if (cb !== undefined) {
          cb();
        }
      },
    }) {
      Column() {
        PlainCard({ caption: '当前连接' }) {
          Row() {
            Column({ space: 3 }) {
              Text(this.currentConnName())
                .fontSize(15)
                .fontColor(this.palette().textPrimary)
              Text(this.currentId.length > 0 ? '已激活，用于所有请求' : '尚未选择连接')
                .fontSize(11)
                .fontColor(this.palette().textMuted)
            }
            .layoutWeight(1)
            .alignItems(HorizontalAlign.Start)

            Button('+ 添加')
              .height(30)
              .fontSize(12)
              .backgroundColor(this.palette().accent)
              .fontColor(Color.White)
              .onClick(() => {
                this.openConnForm(null);
              })
          }
          .width('100%')
          .alignItems(VerticalAlign.Center)

          if (this.showAddForm) {
            Column() {
              Text(this.editingId.length > 0 ? '编辑连接' : '新建连接')
                .fontSize(12)
                .fontColor(this.palette().textSecondary)
                .margin({ bottom: 10 })
              TextInput({ placeholder: '名称 (如 HologramAgent)', text: this.editName })
                .height(36).fontSize(13).fontColor(this.palette().textPrimary)
                .placeholderColor(this.palette().textMuted).backgroundColor(this.palette().bgInput)
                .borderRadius(RADIUS_SM).border({ width: 1, color: this.palette().border })
                .margin({ bottom: 10 })
                .onChange((v: string) => {
                  this.editName = v;
                })
              TextInput({ placeholder: '地址 (域名或 http://192.168.1.100:8080)', text: this.editUrl })
                .height(36).fontSize(13).fontColor(this.palette().textPrimary)
                .placeholderColor(this.palette().textMuted).backgroundColor(this.palette().bgInput)
                .borderRadius(RADIUS_SM).border({ width: 1, color: this.palette().border })
                .margin({ bottom: 10 })
                .onChange((v: string) => {
                  this.editUrl = v;
                })
              TextInput({ placeholder: 'API Key (可选)', text: this.editApiKey })
                .height(36).fontSize(13).fontColor(this.palette().textPrimary)
                .placeholderColor(this.palette().textMuted).backgroundColor(this.palette().bgInput)
                .borderRadius(RADIUS_SM).border({ width: 1, color: this.palette().border })
                .type(InputType.Password).margin({ bottom: 12 })
                .onChange((v: string) => {
                  this.editApiKey = v;
                })

              Row() {
                Button('取消')
                  .height(30)
                  .fontSize(12)
                  .backgroundColor(Color.Transparent)
                  .border({ width: 1, color: this.palette().btnGhostBorder })
                  .fontColor(this.palette().textSecondary)
                  .onClick(() => {
                    this.closeConnForm();
                  })
                Blank()
                Button('保存')
                  .height(30)
                  .fontSize(12)
                  .backgroundColor(this.palette().accent)
                  .fontColor(Color.White)
                  .onClick(() => {
                    this.addConnection();
                  })
              }
              .width('100%')
            }
            .width('100%')
            .padding(12)
            .borderRadius(RADIUS_MD)
            .backgroundColor(this.palette().bgHover)
            .border({ width: 1, color: this.palette().kvBorder })
            .margin({ top: 12 })
            .alignItems(HorizontalAlign.Start)
            .opacity(this.addFormVisible ? 1 : 0)
            .translate({ y: this.addFormVisible ? 0 : 12 })
            .animation({ duration: 220, curve: Curve.EaseOut })
          }
        }

        PlainCard({ caption: '全部连接' }) {
          if (this.connections.length === 0) {
            Text('暂无连接。点击上方“添加”配置后端地址。')
              .fontSize(12)
              .fontColor(this.palette().textMuted)
          }
          ForEach(this.connections, (conn: ConnectionConfig) => {
            Row() {
              Circle({ width: 8, height: 8 })
                .fill(conn.id === this.currentId ? this.palette().accent : '#77809A')
                .margin({ right: 10 })
              Column() {
                Text(conn.name)
                  .fontSize(13)
                  .fontColor(this.palette().textPrimary)
                  .maxLines(1)
                  .textOverflow({ overflow: TextOverflow.Ellipsis })
                Text(conn.url)
                  .fontSize(11)
                  .fontColor(this.palette().textMuted)
                  .maxLines(1)
                  .textOverflow({ overflow: TextOverflow.Ellipsis })
              }
              .layoutWeight(1)
              .alignItems(HorizontalAlign.Start)

              if (conn.id !== this.currentId) {
                Button('切换')
                  .height(26)
                  .fontSize(11)
                  .backgroundColor(Color.Transparent)
                  .border({ width: 1, color: this.palette().btnGhostBorder })
                  .fontColor(this.palette().textSecondary)
                  .margin({ right: 6 })
                  .onClick(() => {
                    this.selectConnection(conn.id);
                  })
              } else {
                Text('使用中')
                  .fontSize(11)
                  .fontColor(this.palette().accent)
                  .padding({ left: 8, right: 8, top: 2, bottom: 2 })
                  .borderRadius(RADIUS_SM)
                  .backgroundColor(this.palette().accentBg)
                  .margin({ right: 6 })
              }
              Button('编辑')
                .height(26)
                .fontSize(11)
                .backgroundColor(Color.Transparent)
                .border({ width: 1, color: this.palette().btnGhostBorder })
                .fontColor(this.palette().textSecondary)
                .margin({ right: 6 })
                .onClick(() => {
                  this.openConnForm(conn);
                })
              Button('删除')
                .height(26)
                .fontSize(11)
                .backgroundColor(Color.Transparent)
                .border({ width: 1, color: 'rgba(232, 64, 38, 0.45)' })
                .fontColor('#E84026')
                .onClick(() => {
                  this.deleteConnection(conn.id);
                })
            }
            .width('100%')
            .padding(10)
            .borderRadius(RADIUS_SM)
            .backgroundColor(this.palette().bgHover)
            .border({
              width: { left: 3 },
              color: conn.id === this.currentId ? this.palette().accent : Color.Transparent,
            })
            .margin({ bottom: 6 })
            // 键里带上 name/url：ForEach 对相同键只更新绑定、不重跑 @Builder 体，
            // 只用 id 做键时改完地址这一行还显示旧值。行内没有 TextInput，
            // 因此把可变字段放进键不会有"编辑时焦点被销毁"的副作用。
          }, (conn: ConnectionConfig) => conn.id + '|' + conn.name + '|' + conn.url)
        }
      }
      .width('100%')
      .alignItems(HorizontalAlign.Start)
    }
  }
}
