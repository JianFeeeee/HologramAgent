package sdk

import pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"

// KernelControlAPI 是内核重启/关机通道（转发公开 SDK 的同名接口）。
//
// 内置插件（WebUI）与外部插件面对的是同一份契约 —— 只做类型别名，
// 不额外加方法：加方法会让"内置插件能调、外部插件调不到"成为既成事实。
type KernelControlAPI = pubsdk.KernelControlAPI
