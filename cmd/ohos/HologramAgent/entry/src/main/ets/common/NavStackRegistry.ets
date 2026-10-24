/**
 * 各主 Tab 页面的二级导航栈登记处。
 *
 * 为什么需要它：四个一级页面各自持有一个 Navigation（宽屏要"左一级右二级"，
 * 所以栈必须是页面局部的，不能提到 Index 里去）。但四个 Navigation 同时
 * 挂在 Swiper 里都是活的，系统返回事件落到哪一个并不确定 —— 表现就是
 * "有的页面返回手势能用、有的不能"。
 *
 * 解决办法：页面把自己的栈按 Tab 序号登记进来，@Entry 页在 onBackPress 里
 * 按当前 Tab 精确地 pop 对应的栈。这样返回手势/三键返回/无障碍返回
 * 在每个页面上的行为都是确定的。
 */

interface StackEntry {
  stack: NavPathStack;
  /** pop 之后页面要同步自己的选中态（宽屏高亮、activeXxx 等） */
  onPopped: () => void;
}

const registry: Map<number, StackEntry> = new Map<number, StackEntry>();

/** 页面 aboutToAppear 时登记；同一 Tab 重复登记以最后一次为准。 */
export function registerNavStack(tab: number, stack: NavPathStack, onPopped: () => void): void {
  registry.set(tab, { stack: stack, onPopped: onPopped });
}

export function unregisterNavStack(tab: number): void {
  registry.delete(tab);
}

/**
 * 返回键/返回手势的统一处理。
 * 返回 true 表示已消费（弹出了一层二级页面），false 表示交回系统（退出应用）。
 *
 * 宽屏 Split 模式下右栏常驻，返回不该把它清空，所以那时直接不消费。
 */
export function handleBackPress(tab: number, isWide: boolean): boolean {
  if (isWide) {
    return false;
  }
  const e: StackEntry | undefined = registry.get(tab);
  if (e === undefined) {
    return false;
  }
  if (e.stack.size() <= 0) {
    return false;
  }
  e.stack.pop();
  e.onPopped();
  return true;
}
