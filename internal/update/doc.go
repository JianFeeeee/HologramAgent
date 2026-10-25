// Package update 是 HologramAgent 的**更新子系统**。
//
// ## 职责边界
//
// 只负责"发现新版 + 拿到产物"，不负责"装上之后怎么跑"：
//
//	kernel.go   内核升级：解包 → 校验 → 备份 → 原子替换 → 重启 → 版本复核 → 回滚
//	release.go  GitHub Release 查询（内核新版发现）
//	plugin.go   插件升级：查仓库 → 拉源码 → hmapdev 现场编译 → 产物 .hmap
//	semver.go   版本号解析与比较（子系统内唯一一份）
//
// ## 三端共享（架构约束，用户明确要求）
//
//	agent 工具（plugin_update / kernel_update）  ┐
//	WebAPI（/plugins/update、/kernel/update）    ├─ 本包
//	CLI（homed-upgrade）                         ┘
//
// 三份实现必然漂移，症状是"agent 说没有新版、WebUI 却升了级"这类自相矛盾的
// 状态。所以入口层只做参数解析与结果呈现，全部逻辑收敛在此。
//
// ## 为什么内核与插件在同一个包里
//
// 它们共享三样东西，分开写就要各自复制一份：
//
//  1. **版本比较** —— 合并前 kernel 侧有 ParseSemver/CmpSemver/versionNewer/
//     trimTagV，plugin 侧有 trimV/isNewer/semverNums，共 6 个函数做同一件事，
//     且行为不一致（kernel 侧会把 "1.4" 判不可比，plugin 侧的正则却漏过去）。
//  2. **两阶段交互**（检查 → 确认 → 执行）—— 同一套用户口径。
//  3. **失败要响亮** —— 都不静默降级：查不到就说查不到，编译失败给日志尾部，
//     替换失败自动回滚。
//
// 分家的代价是这三样各复制一遍，然后慢慢不一致。
//
// ## 与既有升级通道的关系
//
// 不新造安装逻辑：
//   · 插件产物交给 pluginmgr.installFromPath（保留配置、原地覆盖）
//   · 内核重启走 sdk.KernelControlAPI（guard 重拉，不经 systemd）
// 本包只管到"产物就绪"为止。
package update
