//go:build !race

package proc

// raceEnabled 报告当前测试二进制是否用 -race 构建。
//
// 用途：流式发布的耗时判据在 race instrumentation 下常数因子会显著放大
// （每条内存访问都过影子内存检查），"1→8 订阅者的耗时比值"因此会抖动。
// 判据需要知道自己在什么环境下跑，才能既不误报、又不放弃拦截 O(N) 广播。
const raceEnabled = false
