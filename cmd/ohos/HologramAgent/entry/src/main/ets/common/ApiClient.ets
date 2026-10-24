import { http } from '@kit.NetworkKit';
import { ConnectionConfig } from '../model/Model';
import { DEFAULT_API_TIMEOUT } from './Constants';

export class ApiError extends Error {
  status: number;
  body: string;

  constructor(message: string, status: number, body: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.body = body;
  }
}

export interface ApiResponse {
  status: number;
  body: string;
}

/** 二进制响应：附件预览需要原始字节来解码成 PixelMap */
export interface ApiBinaryResponse {
  status: number;
  data: ArrayBuffer;
}

export class ApiClient {
  private conn: ConnectionConfig | null = null;

  setConnection(conn: ConnectionConfig): void {
    this.conn = conn;
  }

  clearConnection(): void {
    this.conn = null;
  }

  getConnection(): ConnectionConfig | null {
    return this.conn;
  }

  hasConnection(): boolean {
    return this.conn !== null && this.conn.url.length > 0;
  }

  private buildUrl(path: string): string {
    if (this.conn === null) {
      return path;
    }
    const base = this.conn.url.replace(/\/+$/, '');
    return base + '/api/v1' + path;
  }

  /**
   * 把附件的 url 字段解析成可直接请求的绝对地址。
   *
   * 后端给的是 `/files/<name>` 或 `/uploads/<name>`（注意：不带 /api/v1 前缀），
   * 远程附件则直接是 http(s) 绝对地址，原样返回。
   */
  absoluteUrl(url: string): string {
    if (url.startsWith('http://') || url.startsWith('https://')) {
      return url;
    }
    if (this.conn === null) {
      return url;
    }
    const base = this.conn.url.replace(/\/+$/, '');
    return url.startsWith('/') ? base + url : base + '/' + url;
  }

  /**
   * 读取附件字节。/files/ 与 /uploads/ 走 requireWeb，
   * 但后端对 API Key 客户端同等放行，所以带上同一套鉴权头即可。
   */
  async getBinary(absUrl: string, timeoutMs: number): Promise<ApiBinaryResponse> {
    const httpRequest = http.createHttp();
    try {
      const options: http.HttpRequestOptions = {
        method: http.RequestMethod.GET,
        header: this.buildHeaders(),
        expectDataType: http.HttpDataType.ARRAY_BUFFER,
        readTimeout: timeoutMs,
        connectTimeout: Math.min(timeoutMs, 15000),
      };
      const resp = await httpRequest.request(absUrl, options);
      const statusCode = resp.responseCode;
      if (statusCode >= 400) {
        throw new ApiError('下载失败', statusCode, '');
      }
      const buf: ArrayBuffer = resp.result as ArrayBuffer;
      return { status: statusCode, data: buf };
    } finally {
      httpRequest.destroy();
    }
  }

  /**
   * multipart/form-data 上传。
   *
   * 后端 POST /api/v1/chat/file 要求 file 字段为文件本体，
   * message / device_id / device_name / client_msg_id 为普通文本字段。
   * ArkTS 侧用 http 的 multiFormDataList：文件走 filePath，文本走 data。
   */
  async postMultipart(path: string, parts: http.MultiFormData[],
                      timeoutMs: number): Promise<ApiResponse> {
    if (this.conn === null) {
      throw new ApiError('未选择连接', -1, '');
    }
    const url = this.buildUrl(path);
    const httpRequest = http.createHttp();
    try {
      const header: Record<string, string> = this.buildHeaders();
      // multipart 的 boundary 由底层生成，必须让出 Content-Type 的控制权
      header['Content-Type'] = 'multipart/form-data';
      const options: http.HttpRequestOptions = {
        method: http.RequestMethod.POST,
        header: header,
        expectDataType: http.HttpDataType.STRING,
        multiFormDataList: parts,
        readTimeout: timeoutMs,
        connectTimeout: Math.min(timeoutMs, 15000),
      };
      const resp = await httpRequest.request(url, options);
      const statusCode = resp.responseCode;
      const body = typeof resp.result === 'string' ? resp.result : '';
      if (statusCode >= 400) {
        let errMsg = body;
        try {
          const parsed: Record<string, string> = JSON.parse(body) as Record<string, string>;
          if (parsed['error'] !== undefined) {
            errMsg = parsed['error'];
          }
        } catch (e) {
          // keep raw body
        }
        throw new ApiError(errMsg, statusCode, body);
      }
      return { status: statusCode, body: body };
    } finally {
      httpRequest.destroy();
    }
  }

  private buildHeaders(): Record<string, string> {
    const headers: Record<string, string> = {};
    headers['Content-Type'] = 'application/json';
    if (this.conn !== null && this.conn.apiKey.length > 0) {
      // 后端 validAPIKey 认 X-API-Key，也认 Authorization: Bearer；
      // query string 形式会被判 401，不要用。
      headers['X-API-Key'] = this.conn.apiKey;
      headers['Authorization'] = 'Bearer ' + this.conn.apiKey;
    }
    return headers;
  }

  async request(path: string, method: string, bodyStr: string,
                timeoutMs: number): Promise<ApiResponse> {
    if (this.conn === null) {
      throw new ApiError('未选择连接', -1, '');
    }
    const url = this.buildUrl(path);
    const httpRequest = http.createHttp();
    try {
      const options: http.HttpRequestOptions = {
        method: (method === 'POST' ? http.RequestMethod.POST :
                 method === 'PUT' ? http.RequestMethod.PUT :
                 method === 'DELETE' ? http.RequestMethod.DELETE :
                 http.RequestMethod.GET) as http.RequestMethod,
        header: this.buildHeaders(),
        expectDataType: http.HttpDataType.STRING,
        readTimeout: timeoutMs,
        connectTimeout: Math.min(timeoutMs, 15000),
      };
      if (bodyStr.length > 0) {
        options.extraData = bodyStr;
      }
      const resp = await httpRequest.request(url, options);
      const statusCode = resp.responseCode;
      const body = typeof resp.result === 'string' ? resp.result : '';
      if (statusCode >= 400) {
        let errMsg = body;
        try {
          const parsed: Record<string, string> = JSON.parse(body) as Record<string, string>;
          if (parsed['error'] !== undefined) {
            errMsg = parsed['error'];
          } else if (parsed['message'] !== undefined) {
            errMsg = parsed['message'];
          }
        } catch (e) {
          // keep raw body
        }
        throw new ApiError(errMsg, statusCode, body);
      }
      return { status: statusCode, body: body };
    } finally {
      httpRequest.destroy();
    }
  }

  async get(path: string): Promise<ApiResponse> {
    return this.request(path, 'GET', '', DEFAULT_API_TIMEOUT);
  }

  async getWithTimeout(path: string, timeoutMs: number): Promise<ApiResponse> {
    return this.request(path, 'GET', '', timeoutMs);
  }

  async post(path: string, bodyObj: object | null): Promise<ApiResponse> {
    const bodyStr = bodyObj === null ? '' : JSON.stringify(bodyObj);
    return this.request(path, 'POST', bodyStr, DEFAULT_API_TIMEOUT);
  }

  async postWithTimeout(path: string, bodyObj: object | null,
                        timeoutMs: number): Promise<ApiResponse> {
    const bodyStr = bodyObj === null ? '' : JSON.stringify(bodyObj);
    return this.request(path, 'POST', bodyStr, timeoutMs);
  }
}

export const apiClient: ApiClient = new ApiClient();
