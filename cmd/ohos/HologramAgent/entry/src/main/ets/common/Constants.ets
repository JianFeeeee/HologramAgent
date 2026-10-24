/**
 * Design tokens — mirror of gui/renderer/style.css `:root` (dark theme).
 * All values copied 1:1 from the web GUI to keep visual identity identical.
 *
 * Theme system:
 *  - ThemePalette holds every color token used by the app.
 *  - DARK_PALETTE mirrors style.css `:root`; LIGHT_PALETTE mirrors
 *    `[data-theme="light"][data-color="sakura"]`.
 *  - themeStore keeps a global AppStorage("themeIsDark") flag that every page
 *    reads through @StorageProp, so the whole UI re-renders on switch.
 */

// ===== network =====
export const DEFAULT_API_TIMEOUT: number = 120000;
export const SSE_RECONNECT_DELAY: number = 5000;
export const DEFAULT_WS_PORT: number = 9890;
/** 聊天历史首屏条数：只拉最新 N 条，向上滚动触顶再加载更早的 */
export const CHAT_PAGE_SIZE: number = 40;

// ===== AppStorage 跨页面信号键 =====
//
// 未连接后端时的“入口可达性”靠这三个键串起来：聊天空态按钮 → 切主 Tab →
// 设置页打开连接二级页。不用组件回调是因为按钮与目标分属不同的 Swiper 子页，
// 中间还隔着 Index，逐层传回调会把两个无关页面耦在一起。
/** 当前是否已配置并激活后端连接（空态/入口的响应式判断） */
export const K_HAS_CONN: string = 'hasConn';
/** 外部请求切换主 Tab（-1 = 无请求），由 Index 监听 */
export const K_REQUESTED_TAB: string = 'requestedTab';
/** 请求设置页打开某个二级页（空串 = 无请求），由 SettingsPage 监听 */
export const K_SETTINGS_SUB: string = 'settingsSubRequest';

// ===== sakura / frost palette (style.css :root) =====
export const COLOR_SAKURA_100: string = 'rgba(10, 89, 247, 0.1)';
export const COLOR_SAKURA_200: string = 'rgba(10, 89, 247, 0.16)';
export const COLOR_SAKURA_300: string = '#5B93F8';
export const COLOR_SAKURA_400: string = '#0A59F7';
export const COLOR_SAKURA_500: string = '#0A59F7';
export const COLOR_SAKURA_600: string = '#0A59F7';
export const COLOR_SAKURA_700: string = '#0846C2';
export const COLOR_FROST_300: string = '#4A90D9';
export const COLOR_FROST_400: string = '#3A78B5';
export const COLOR_FROST_500: string = '#2C5E8C';
export const COLOR_SUCCESS: string = '#30B260';
export const COLOR_WARNING: string = '#D99A2B';
export const COLOR_ERROR: string = '#E84026';
export const COLOR_INFO: string = '#3F6EF5';
export const COLOR_CYAN: string = '#2DD4BF';
export const COLOR_VIOLET: string = '#A78BFA';
export const COLOR_EMERALD: string = '#34D399';
export const COLOR_AMBER: string = '#FBBF24';
export const COLOR_BLUE: string = '#60A5FA';

// ===== 宽屏（平板 / 折叠展开 / 分屏）适配 =====
/**
 * 宽屏断点：窗口宽度 >= 600vp 视为宽屏。
 * 600vp = Navigation 分栏所需的 minNavBarWidth(240) + minContentWidth(360)，
 * 与系统 NavigationMode.Auto 的切换阈值保持一致，避免自判与系统行为脱节。
 */
export const WIDE_MIN_WIDTH: number = 600;
/** 宽屏下左侧一级界面栏宽度（vp）：左边一级界面，右边二级界面 */
export const WIDE_NAV_BAR_WIDTH: number = 420;
/** 宽屏下右侧二级界面的最小宽度（vp），不足时左栏被压缩 */
export const WIDE_MIN_CONTENT: number = 380;
/** 悬浮导航胶囊的最大宽度（vp）：宽屏下避免被拉成长条 */
export const NAV_PILL_MAX_WIDTH: number = 620;

// ===== 动效时长（ms）=====
/**
 * 统一动效节奏，避免每处各写一个魔数导致快慢不一。
 *
 * 取值依据：状态切换类交互（按下、展开、选中）控制在 150-250ms，
 * 超过 300ms 会显得"拖"；进出场大位移（导航栏收起、悬浮层）用 300-400ms
 * 才不突兀。曲线统一 EaseOut：起步快、收尾缓，手感上更"跟手"。
 */
/** 按压反馈、图标旋转等即时反馈 */
export const ANIM_FAST: number = 150;
/** 展开/折叠、选中态迁移等常规状态切换 */
export const ANIM_NORMAL: number = 220;
/** 卡片入场、列表项出现 */
export const ANIM_ENTER: number = 280;
/** 导航栏收起、悬浮层进出等大位移 */
export const ANIM_SLOW: number = 400;
/** 按压态缩放：轻微下沉即可，过度缩放显廉价 */
export const PRESS_SCALE: number = 0.97;

/** Every color token the app renders with. */
export interface ThemePalette {
  bgPrimary: string;
  bgSecondary: string;
  bgCard: string;
  bgInput: string;
  bgHover: string;
  glassBgStrong: string;
  glassBorder: string;
  border: string;
  kvBorder: string;
  btnGhostBorder: string;
  textPrimary: string;
  textSecondary: string;
  textMuted: string;
  textTertiary: string;
  accent: string;
  accentBg: string;
  preBg: string;
  preText: string;
  msgUserBg: string;
  msgUserText: string;
  msgAssistantBg: string;
  msgAssistantText: string;
  msgBubbleBg: string;
  msgBubbleText: string;
  msgBubbleBorder: string;
  msgUserBubbleBg: string;
  msgUserBubbleBorder: string;
  msgAssistantBubbleBg: string;
  msgAssistantBubbleBorder: string;
  toastBg: string;
  toastText: string;
  toastErrorBg: string;
  toastErrorText: string;
  toastWarnBg: string;
  toastWarnText: string;
  successSoftBg: string;
  errorSoftBg: string;
  frostSoftBg: string;
  navBarBg: string;
  navBarBorder: string;
  navBarGradientStart: string;
  navBarGradientEnd: string;
  shadow: string;
  gradA: string; // sakura top-right glow (start color)
  gradB: string; // frost mid-left glow
  gradC: string; // sakura bottom glow
  gradAEnd: string; // transparent end color for radial fade
  gradBEnd: string;
  gradCEnd: string;
}

/** Dark palette — HarmonyOS system-app style neutral dark. */
export const DARK_PALETTE: ThemePalette = {
  bgPrimary: '#000000',
  bgSecondary: 'rgba(28, 28, 30, 0.72)',
  bgCard: 'rgba(28, 28, 30, 0.6)',
  bgInput: 'rgba(44, 44, 46, 0.92)',
  bgHover: 'rgba(255, 255, 255, 0.08)',
  glassBgStrong: 'rgba(28, 28, 30, 0.82)',
  glassBorder: 'rgba(255, 255, 255, 0.1)',
  border: 'rgba(255, 255, 255, 0.1)',
  kvBorder: 'rgba(255, 255, 255, 0.08)',
  btnGhostBorder: 'rgba(255, 255, 255, 0.18)',
  textPrimary: '#FFFFFF',
  textSecondary: '#D1D1D6',
  textMuted: '#98989F',
  textTertiary: '#8E8E93',
  accent: '#0A59F7',
  accentBg: 'rgba(10, 89, 247, 0.18)',
  preBg: 'rgba(20, 20, 22, 0.9)',
  preText: '#D6E4FF',
  msgUserBg: 'rgba(10, 89, 247, 0.2)',
  msgUserText: '#9DC0FC',
  msgAssistantBg: 'rgba(120, 120, 128, 0.24)',
  msgAssistantText: '#E5E5EA',
  msgBubbleBg: '#1C1C1E',
  msgBubbleText: '#F2F2F7',
  msgBubbleBorder: 'rgba(255, 255, 255, 0.12)',
  msgUserBubbleBg: 'rgba(10, 89, 247, 0.18)',
  msgUserBubbleBorder: 'rgba(10, 89, 247, 0.25)',
  msgAssistantBubbleBg: 'rgba(58, 58, 60, 0.72)',
  msgAssistantBubbleBorder: 'rgba(255, 255, 255, 0.1)',
  toastBg: 'rgba(48, 178, 96, 0.18)',
  toastText: '#4CD47A',
  toastErrorBg: 'rgba(232, 64, 38, 0.2)',
  toastErrorText: '#FF6B4A',
  toastWarnBg: 'rgba(255, 159, 10, 0.16)',
  toastWarnText: '#FFCC66',
  successSoftBg: 'rgba(48, 178, 96, 0.16)',
  errorSoftBg: 'rgba(232, 64, 38, 0.18)',
  frostSoftBg: 'rgba(74, 144, 217, 0.14)',
  navBarBg: 'rgba(24, 24, 26, 0.45)',
  navBarBorder: 'rgba(255, 255, 255, 0.14)',
  navBarGradientStart: 'rgba(40, 40, 44, 0.35)',
  navBarGradientEnd: 'rgba(20, 20, 22, 0.5)',
  shadow: 'rgba(0, 0, 0, 0.5)',
  gradA: 'rgba(10, 89, 247, 0.1)',
  gradB: 'rgba(74, 144, 217, 0.07)',
  gradC: 'rgba(94, 92, 230, 0.06)',
  gradAEnd: 'rgba(10, 89, 247, 0)',
  gradBEnd: 'rgba(74, 144, 217, 0)',
  gradCEnd: 'rgba(94, 92, 230, 0)',
};

/** Light palette — HarmonyOS system-app style neutral light. */
export const LIGHT_PALETTE: ThemePalette = {
  bgPrimary: '#F1F3F5',
  bgSecondary: 'rgba(255, 255, 255, 0.85)',
  bgCard: 'rgba(255, 255, 255, 0.95)',
  bgInput: 'rgba(118, 118, 128, 0.28)',
  bgHover: 'rgba(0, 0, 0, 0.05)',
  glassBgStrong: 'rgba(255, 255, 255, 0.92)',
  glassBorder: 'rgba(60, 60, 67, 0.12)',
  border: 'rgba(60, 60, 67, 0.12)',
  kvBorder: 'rgba(60, 60, 67, 0.1)',
  btnGhostBorder: 'rgba(60, 60, 67, 0.2)',
  textPrimary: '#191919',
  textSecondary: '#494949',
  textMuted: '#777779',
  textTertiary: '#8A8A8E',
  accent: '#0A59F7',
  accentBg: 'rgba(10, 89, 247, 0.1)',
  preBg: 'rgba(118, 118, 128, 0.1)',
  preText: '#3C3C43',
  msgUserBg: 'rgba(10, 89, 247, 0.12)',
  msgUserText: '#0A59F7',
  msgAssistantBg: '#FFFFFF',
  msgAssistantText: '#333333',
  msgBubbleBg: '#FFFFFF',
  msgBubbleText: '#191919',
  msgBubbleBorder: 'rgba(60, 60, 67, 0.12)',
  msgUserBubbleBg: 'rgba(10, 89, 247, 0.1)',
  msgUserBubbleBorder: 'rgba(10, 89, 247, 0.18)',
  msgAssistantBubbleBg: 'rgba(255, 255, 255, 0.88)',
  msgAssistantBubbleBorder: 'rgba(60, 60, 67, 0.1)',
  toastBg: 'rgba(48, 178, 96, 0.14)',
  toastText: '#157347',
  toastErrorBg: 'rgba(232, 64, 38, 0.12)',
  toastErrorText: '#C0361F',
  toastWarnBg: 'rgba(255, 159, 10, 0.14)',
  toastWarnText: '#8F5A00',
  successSoftBg: 'rgba(48, 178, 96, 0.12)',
  errorSoftBg: 'rgba(232, 64, 38, 0.1)',
  frostSoftBg: 'rgba(74, 144, 217, 0.12)',
  navBarBg: 'rgba(250, 250, 252, 0.55)',
  navBarBorder: 'rgba(60, 60, 67, 0.15)',
  navBarGradientStart: 'rgba(255, 255, 255, 0.35)',
  navBarGradientEnd: 'rgba(240, 240, 245, 0.5)',
  shadow: 'rgba(0, 0, 0, 0.1)',
  gradA: 'rgba(10, 89, 247, 0.06)',
  gradB: 'rgba(74, 144, 217, 0.05)',
  gradC: 'rgba(94, 92, 230, 0.04)',
  gradAEnd: 'rgba(10, 89, 247, 0)',
  gradBEnd: 'rgba(74, 144, 217, 0)',
  gradCEnd: 'rgba(94, 92, 230, 0)',
};

/**
 * Legacy single-theme constants kept for incremental migration.
 * New code should use tp() / ThemePalette instead.
 */
export const COLOR_BG_PRIMARY: string = '#000000';
export const COLOR_BG_SECONDARY: string = 'rgba(17, 24, 44, 0.72)';
export const COLOR_BG_CARD: string = 'rgba(17, 24, 44, 0.6)';
export const COLOR_BG_INPUT: string = 'rgba(13, 18, 34, 0.75)';
export const COLOR_BG_HOVER: string = 'rgba(255, 255, 255, 0.06)';
export const COLOR_GLASS_BG: string = 'rgba(13, 18, 34, 0.6)';
export const COLOR_GLASS_BG_STRONG: string = 'rgba(13, 18, 32, 0.82)';
export const COLOR_GLASS_BORDER: string = 'rgba(255, 255, 255, 0.08)';
export const COLOR_GLASS_HOVER: string = 'rgba(255, 255, 255, 0.05)';

export const COLOR_TEXT_PRIMARY: string = '#FFFFFF';
export const COLOR_TEXT_SECONDARY: string = 'rgba(255, 255, 255, 0.6)';
export const COLOR_TEXT_MUTED: string = 'rgba(255, 255, 255, 0.4)';
export const COLOR_TEXT_TERTIARY: string = 'rgba(255, 255, 255, 0.45)';

export const COLOR_ACCENT: string = '#0A59F7';
export const COLOR_ACCENT_BG: string = 'rgba(10, 89, 247, 0.14)';
export const COLOR_BORDER: string = 'rgba(255, 255, 255, 0.09)';
export const COLOR_KV_BORDER: string = 'rgba(255, 255, 255, 0.07)';
export const COLOR_BTN_GHOST_BORDER: string = 'rgba(255, 255, 255, 0.14)';
export const COLOR_SAVE_BTN_BORDER: string = '#D99A2B';

export const COLOR_TOAST_BG: string = 'rgba(23, 169, 100, 0.16)';
export const COLOR_TOAST_TEXT: string = '#6EE7A8';
export const COLOR_TOAST_ERROR_BG: string = 'rgba(232, 64, 38, 0.18)';
export const COLOR_TOAST_ERROR_TEXT: string = '#F0865B';
export const COLOR_TOAST_WARN_BG: string = 'rgba(217, 154, 43, 0.16)';
export const COLOR_TOAST_WARN_TEXT: string = '#FCD9A0';

// ===== radius (style.css --radius-*) =====
export const RADIUS_SM: number = 6;
export const RADIUS_MD: number = 10;
export const RADIUS_LG: number = 14;
export const RADIUS_PILL: number = 999;

// ===== misc states =====
export const COLOR_CONN_OFFLINE: string = '#77809A';
export const COLOR_DOT_GRAY: string = '#475569';

// =====================================================================
// Global reactive theme store.
//
// AppStorage keys:
//   "themeIsDark"   boolean — current resolved dark/light state
//   "themeMode"     string  — 'system' | 'dark' | 'light'
//
// Pages read via @StorageProp("themeIsDark") and pick colors from
// tp() so the whole tree re-renders when the mode flips.
// EntryAbility seeds both on launch and updates "themeIsDark" on
// onConfigurationUpdate (system dark-mode change).
// =====================================================================

const KEY_THEME_IS_DARK: string = 'themeIsDark';
const KEY_SYSTEM_IS_DARK: string = 'systemIsDark';
const KEY_THEME_MODE: string = 'themeMode';

export function seedTheme(isDark: boolean): void {
  if (!AppStorage.has(KEY_THEME_IS_DARK)) {
    AppStorage.setOrCreate(KEY_THEME_IS_DARK, isDark);
  } else {
    AppStorage.set(KEY_THEME_IS_DARK, isDark);
  }
}

/**
 * Record the OS dark/light state. UI never binds to this directly, but the
 * 'system' mode resolves against it, so EntryAbility updates it on config
 * change and then flips themeIsDark when mode === 'system'.
 */
export function seedSystemIsDark(isDark: boolean): void {
  if (!AppStorage.has(KEY_SYSTEM_IS_DARK)) {
    AppStorage.setOrCreate(KEY_SYSTEM_IS_DARK, isDark);
  } else {
    AppStorage.set(KEY_SYSTEM_IS_DARK, isDark);
  }
}

/**
 * Resolve a stored theme mode ('system'|'dark'|'light') against the current
 * system color mode. 'system' falls back to the systemIsDark flag.
 */
export function resolveIsDark(mode: string, sysDark: boolean): boolean {
  if (mode === 'dark') {
    return true;
  }
  if (mode === 'light') {
    return false;
  }
  return sysDark;
}

/**
 * Apply a stored theme mode: persist the mode token and immediately flip the
 * reactive themeIsDark flag so the whole UI re-renders.
 */
export function applyThemeMode(mode: string): void {
  const sysDark: boolean = AppStorage.get<boolean>(KEY_SYSTEM_IS_DARK) ?? true;
  AppStorage.set(KEY_THEME_MODE, mode);
  seedTheme(resolveIsDark(mode, sysDark));
}

/** Current resolved palette for @Builder / build() usage. */
export function tp(): ThemePalette {
  const dark: boolean = AppStorage.get<boolean>(KEY_THEME_IS_DARK) ?? true;
  return dark ? DARK_PALETTE : LIGHT_PALETTE;
}

/** Raw read for non-UI code. */
export function themeIsDark(): boolean {
  return AppStorage.get<boolean>(KEY_THEME_IS_DARK) ?? true;
}
