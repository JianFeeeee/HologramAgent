/**
 * 附件的字节获取与解码（网络 / 沙箱 I/O）。
 *
 * 字节走 GET <base>/files/<name> 或 /uploads/<name>（注意不带 /api/v1 前缀）。
 * 这两条路由在后端是 requireWeb，但对 API Key 客户端同等放行，
 * 所以带上和普通接口一样的鉴权头即可，无需 web 登录态。
 *
 * 从 components/Attachment.ets 抽出：缩略图与详情大图都走同一条解码路径。
 */

import { image } from '@kit.ImageKit';
import { fileIo } from '@kit.CoreFileKit';
import { apiClient } from './ApiClient';

/** 下载并解码成 PixelMap；任何一步失败都返回 undefined（调用方显示占位）。 */
export async function loadPixelMap(url: string): Promise<image.PixelMap | undefined> {
  try {
    // 本地待上传的图片：直接读沙箱文件，不走网络
    if (url.startsWith('file://')) {
      const path: string = url.substring(7);
      const f = fileIo.openSync(path, fileIo.OpenMode.READ_ONLY);
      const localSrc: image.ImageSource = image.createImageSource(f.fd);
      const localPm: image.PixelMap = await localSrc.createPixelMap();
      await localSrc.release();
      fileIo.closeSync(f);
      return localPm;
    }
    const abs: string = apiClient.absoluteUrl(url);
    const resp = await apiClient.getBinary(abs, 15000);
    const src: image.ImageSource = image.createImageSource(resp.data);
    const pm: image.PixelMap = await src.createPixelMap();
    await src.release();
    return pm;
  } catch (e) {
    return undefined;
  }
}
