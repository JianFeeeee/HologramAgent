/**
 * 设置一级页的页面骨架：滚动区 + 顶栏 + 悬浮区 + 轻提示。
 *
 * 从 pages/SettingsPage.ets 抽出：页面本身只剩数据流与导航表，
 * 这一层是纯布局 —— 所有计数/文案都由页面算好传进来。
 */

import { handleNavOnScroll } from '../common/NavBarController';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE } from '../common/Constants';
import { PageTopBar, NavFloatOverlay, NavFloatRow } from './PageTopBar';
import { SettingsRootEntries } from './SettingsRootEntries';
import { ToastBar } from './ToastBar';

@Component
export struct SettingsHome {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  /** 一级入口列表所需的计数/文案（见 SettingsRootEntries） */
  @Prop connCount: number = 0;
  @Prop connName: string = '';
  @Prop sectionCount: number = 0;
  @Prop coreKeys: number = 0;
  @Prop busy: boolean = false;
  @Prop errorText: string = '';
  @Prop themeLabel: string = '';
  @Prop activeSub: string = '';
  @Prop isWide: boolean = false;
  /** 保存进行中：悬浮区显示转圈 */
  @Prop savingCount: number = 0;
  @Prop toastMsg: string = '';
  @Prop toastIsError: boolean = false;
  onOpen?: (id: string) => void;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  build() {
    Stack({ alignContent: Alignment.Bottom }) {
      Column() {
        Scroll() {
          Column() {
            SettingsRootEntries({
              connCount: this.connCount,
              connName: this.connName,
              sectionCount: this.sectionCount,
              coreKeys: this.coreKeys,
              busy: this.busy,
              errorText: this.errorText,
              themeLabel: this.themeLabel,
              activeSub: this.activeSub,
              isWide: this.isWide,
              onOpen: (id: string) => {
                const cb: ((id: string) => void) | undefined = this.onOpen;
                if (cb !== undefined) {
                  cb(id);
                }
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

      PageTopBar({ title: '设置' })

      // 一级悬浮区：仅在保存进行中显示一个转圈；
      // 已保存/未保存的常驻徽标按用户要求去掉（保存本来就是即时的，不需要状态吊牌）
      NavFloatOverlay({ tab: 3 }) {
        NavFloatRow() {
          if (this.savingCount > 0) {
            LoadingProgress()
              .width(14)
              .height(14)
              .color(this.palette().accent)
          }
        }
      }

      ToastBar({ msg: this.toastMsg, isError: this.toastIsError })
    }
    .width('100%')
    .height('100%')
    .backgroundColor(Color.Transparent)
  }
}
