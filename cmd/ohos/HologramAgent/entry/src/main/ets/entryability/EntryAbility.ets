import { UIAbility, AbilityConstant, Configuration, ConfigurationConstant, Want } from '@kit.AbilityKit';
import { window } from '@kit.ArkUI';
import { BusinessError } from '@kit.BasicServicesKit';
import { connStore } from '../common/ConnStore';
import { apiClient } from '../common/ApiClient';
import { themeIsDark, seedTheme, seedSystemIsDark, resolveIsDark, applyThemeMode } from '../common/Constants';
import { startForegroundBridge, stopForegroundBridge } from '../common/DeviceBridgeSession';

/** Read the persisted theme mode ('system'|'dark'|'light'), defaulting to 'system'. */
function storedThemeMode(): string {
  try {
    const s = connStore.getSettings();
    if (typeof s.theme === 'string' && s.theme.length > 0) {
      return s.theme;
    }
  } catch (e) {
    // store not ready yet
  }
  return 'system';
}

export default class EntryAbility extends UIAbility {
  onCreate(want: Want, launchParam: AbilityConstant.LaunchParam): void {
    console.info('[HologramAgent] ability onCreate');
    const sysDark: boolean =
      this.context.config?.colorMode === ConfigurationConstant.ColorMode.COLOR_MODE_DARK;
    // Seed the OS color state first, then resolve the persisted mode.
    // connStore may not be initialized here (mode falls back to 'system');
    // onWindowStageCreate re-applies the real stored mode after init.
    seedSystemIsDark(sysDark);
    seedTheme(resolveIsDark(storedThemeMode(), sysDark));
  }

  onDestroy(): void {
    stopForegroundBridge();
    console.info('[HologramAgent] ability onDestroy');
  }

  /** System dark/light switch — re-resolve when mode is 'system'. */
  onConfigurationUpdate(newConfig: Configuration): void {
    try {
      const sysDark: boolean = newConfig.colorMode === ConfigurationConstant.ColorMode.COLOR_MODE_DARK;
      seedSystemIsDark(sysDark);
      const mode: string = storedThemeMode();
      if (mode === 'system') {
        AppStorage.set('themeIsDark', resolveIsDark('system', sysDark));
        this.applySystemBar();
      }
    } catch (e) {
      // ignore
    }
  }

  private applySystemBar(): void {
    // 状态栏与窗口底色跟随主题，消除浅色模式下的暗色割裂
    const dark: boolean = themeIsDark();
    const bg: string = dark ? '#000000' : '#F1F3F5';
    const fg: string = dark ? '#FFFFFF' : '#191919';
    try {
      window.getLastWindow(this.context).then((win: window.Window) => {
        win.setWindowSystemBarProperties({
          statusBarColor: bg,
          statusBarContentColor: fg,
          navigationBarColor: bg,
        });
        win.setWindowBackgroundColor(bg);
      }).catch(() => {
        // ignore
      });
    } catch (e) {
      // ignore
    }
  }

  onWindowStageCreate(windowStage: window.WindowStage): void {
    const startUI = (): void => {
      windowStage.loadContent('pages/Index', () => {
        console.info('[HologramAgent] main page loaded');
      });
    };

    // 沉浸式窗口 + 主题化系统栏，消除真机顶部/底部白边
    windowStage.getMainWindow((err: BusinessError, win: window.Window) => {
      if (err.code !== 0) {
        connStore.init(this.context).then(async () => {
          const cur = connStore.getCurrentConnection();
          if (cur !== null) {
            apiClient.setConnection(cur);
          }
          this.reapplyStoredTheme();
          startForegroundBridge(this.context);
          startUI();
        }).catch(() => {
          startUI();
        });
        return;
      }
      try {
        // 全屏沉浸：内容延伸到状态栏和导航栏区域
        win.setWindowLayoutFullScreen(true);
      } catch (e) {
        console.warn('[HologramAgent] setWindowLayoutFullScreen failed: ' + (e as Error).message);
      }
      connStore.init(this.context).then(async () => {
        const cur = connStore.getCurrentConnection();
        if (cur !== null) {
          apiClient.setConnection(cur);
        }
        console.info('[HologramAgent] connStore initialized');
        // init 之后持久化的主题模式才可读，这里按存量设置重新解析并刷新系统栏
        this.reapplyStoredTheme();
        this.applySystemBar();
        startForegroundBridge(this.context);
        startUI();
      }).catch((e: Error) => {
        console.error('[HologramAgent] connStore init failed: ' + e.message);
        startUI();
      });
    });
  }

  /** After connStore.init, re-resolve themeIsDark from the persisted mode. */
  private reapplyStoredTheme(): void {
    try {
      applyThemeMode(storedThemeMode());
    } catch (e) {
      // ignore
    }
  }

  onWindowStageDestroy(): void {
    console.info('[HologramAgent] ability onWindowStageDestroy');
  }

  onForeground(): void {
    startForegroundBridge(this.context);
    console.info('[HologramAgent] ability onForeground');
  }

  onBackground(): void {
    stopForegroundBridge();
    console.info('[HologramAgent] ability onBackground');
  }
}
