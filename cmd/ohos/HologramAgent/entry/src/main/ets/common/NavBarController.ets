/**
 * 导航栏显隐控制器（跨页面共享单例）。
 * 规则：任何页面滚动中隐藏底部导航，滚动停止（松手/fling 结束）后重新显示。
 */
type NavListener = (visible: boolean) => void;

export class NavBarController {
  private static instance: NavBarController | null = null;
  private visible: boolean = true;
  private listeners: NavListener[] = [];

  static shared(): NavBarController {
    if (NavBarController.instance === null) {
      NavBarController.instance = new NavBarController();
    }
    return NavBarController.instance;
  }

  isVisible(): boolean {
    return this.visible;
  }

  setVisible(v: boolean): void {
    if (this.visible !== v) {
      this.visible = v;
      AppStorage.setOrCreate<boolean>('navVisible', v);
      this.notify();
    }
  }

  addListener(l: NavListener): void {
    this.listeners.push(l);
  }

  private notify(): void {
    for (let i = 0; i < this.listeners.length; i++) {
      this.listeners[i](this.visible);
    }
  }
}

export const navBar: NavBarController = NavBarController.shared();

/**
 * 页面 Scroll.onDidScroll 的统一处理：
 * Scroll/Fling 状态（手指拖动或惯性滚动）隐藏导航栏，
 * Idle（松手或惯性结束）重新显示。
 */
export function handleNavOnScroll(state: ScrollState): void {
  if (state === ScrollState.Idle) {
    navBar.setVisible(true);
  } else {
    navBar.setVisible(false);
  }
}
