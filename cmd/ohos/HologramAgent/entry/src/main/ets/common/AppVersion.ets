import { bundleManager } from '@kit.AbilityKit';

/**
 * 应用版本号：从 bundle 元数据读取，而不是在 .ets 里再抄一份。
 *
 * AppScope/app.json5 是版本的唯一来源（由 deploy/scripts/sync-client-versions.sh
 * 与内核 internal/meta.Version 对齐）。在代码里再写一个字面量就是第二份真相，
 * 实测已经漂过：app.json5 写 1.1.1、设备桥上又是一份 1.1.1，而内核早已 1.4.0。
 * 设备桥上报 / deviceinfo 回显的真实安装包版本，应当来自同一个来源。
 */
export function appVersion(): string {
  try {
    const info: bundleManager.BundleInfo =
      bundleManager.getBundleInfoForSelfSync(bundleManager.BundleFlag.GET_BUNDLE_INFO_DEFAULT);
    return info.versionName;
  } catch (e) {
    return '';
  }
}
