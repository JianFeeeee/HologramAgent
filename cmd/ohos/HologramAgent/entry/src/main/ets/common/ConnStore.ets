import { preferences } from '@kit.ArkData';
import { Context } from '@kit.AbilityKit';
import { deviceInfo } from '@kit.BasicServicesKit';
import { ConnectionConfig, AppSettings, emptySettings, defaultConnection, normalizeBaseUrl } from '../model/Model';

const PREF_NAME: string = 'homeagent_prefs';
const KEY_CONNECTIONS: string = 'connections_json';
const KEY_SETTINGS: string = 'settings_json';
const KEY_DEVICE_ID: string = 'device_id';
const KEY_DEVICE_NAME: string = 'device_name';
const KEY_DEVICE_AUTH: string = 'device_authorized';

export class ConnStore {
  private prefs: preferences.Preferences | null = null;
  private connections: ConnectionConfig[] = [];
  private settings: AppSettings = emptySettings();

  async init(context: Context): Promise<void> {
    this.prefs = await preferences.getPreferences(context, PREF_NAME);
    await this.load();
  }

  private async load(): Promise<void> {
    if (this.prefs === null) {
      return;
    }
    const connJson: string = await this.prefs.get(KEY_CONNECTIONS, '') as string;
    if (connJson.length > 0) {
      try {
        this.connections = JSON.parse(connJson) as ConnectionConfig[];
      } catch (e) {
        this.connections = [];
      }
      // 存量数据可能是旧版本存进去的裸域名或带尾斜杠的地址，读出来时一并规范化，
      // 否则老用户升级后仍然会拼出错误的请求地址。
      for (let i = 0; i < this.connections.length; i++) {
        const c: ConnectionConfig = this.connections[i];
        c.url = normalizeBaseUrl(c.url);
      }
    }
    const settingsJson: string = await this.prefs.get(KEY_SETTINGS, '') as string;
    if (settingsJson.length > 0) {
      try {
        this.settings = JSON.parse(settingsJson) as AppSettings;
      } catch (e) {
        this.settings = emptySettings();
      }
    }
  }

  // 不预置任何连接：地址与密钥属于用户私有配置，不能随源码分发。
  // 连接列表为空时各页面走"尚未配置后端连接"的统一提示，用户到设置页添加。

  getConnections(): ConnectionConfig[] {
    return this.connections;
  }

  getCurrentConnection(): ConnectionConfig | null {
    if (this.settings.currentConnId.length === 0) {
      if (this.connections.length > 0) {
        return this.connections[0];
      }
      return null;
    }
    for (let i = 0; i < this.connections.length; i++) {
      const c: ConnectionConfig = this.connections[i];
      if (c.id === this.settings.currentConnId) {
        return c;
      }
    }
    if (this.connections.length > 0) {
      return this.connections[0];
    }
    return null;
  }

  async addConnection(name: string, url: string, apiKey: string): Promise<ConnectionConfig> {
    const conn: ConnectionConfig = defaultConnection();
    conn.id = Date.now().toString(36) + Math.floor(Math.random() * 10000).toString(36);
    conn.name = name;
    conn.url = normalizeBaseUrl(url);
    conn.apiKey = apiKey;
    conn.type = 'webui';
    this.connections.push(conn);
    if (this.settings.currentConnId.length === 0) {
      this.settings.currentConnId = conn.id;
    }
    await this.save();
    return conn;
  }

  async updateConnection(id: string, name: string, url: string,
                         apiKey: string): Promise<void> {
    for (let i = 0; i < this.connections.length; i++) {
      const c: ConnectionConfig = this.connections[i];
      if (c.id === id) {
        c.name = name;
        c.url = normalizeBaseUrl(url);
        c.apiKey = apiKey;
        break;
      }
    }
    await this.save();
  }

  async deleteConnection(id: string): Promise<void> {
    const next: ConnectionConfig[] = [];
    for (let i = 0; i < this.connections.length; i++) {
      const c: ConnectionConfig = this.connections[i];
      if (c.id !== id) {
        next.push(c);
      }
    }
    this.connections = next;
    if (this.settings.currentConnId === id) {
      if (this.connections.length > 0) {
        this.settings.currentConnId = this.connections[0].id;
      } else {
        this.settings.currentConnId = '';
      }
    }
    await this.save();
  }

  async setCurrent(id: string): Promise<void> {
    this.settings.currentConnId = id;
    await this.save();
  }

  getSettings(): AppSettings {
    return this.settings;
  }

  async saveSettings(s: AppSettings): Promise<void> {
    this.settings = s;
    await this.save();
  }

  /** Persisted local device id (stable across restarts). */
  getDeviceId(): string {
    if (this.prefs === null) {
      return '';
    }
    return this.prefs.getSync(KEY_DEVICE_ID, '') as string;
  }

  saveDeviceId(id: string): void {
    if (this.prefs === null) {
      return;
    }
    this.prefs.putSync(KEY_DEVICE_ID, id);
    this.prefs.flush();
  }

  /**
   * 取本机设备 ID，没有就地生成并持久化。
   *
   * 聊天发送必须带 device_id：后端 handleChat 只有拿到它才会把来源编码成
   * webui/<device_id> 并往 stageCtx 注入"当前输入来自设备[...]"，
   * 否则 source 恒为 "webui"，agent 会以为消息是网页端发的。
   * 之前只有设备页在用时才生成 ID，聊天页拿到空串 → 身份丢失。
   */
  ensureDeviceId(): string {
    const cur: string = this.getDeviceId();
    if (cur.length > 0) {
      return cur;
    }
    const id: string = 'ohos-' + Date.now().toString(36);
    this.saveDeviceId(id);
    return id;
  }

  /** 设备显示名：优先用户自定义，否则按机型自动生成（"HUAWEI Mate 60 (HarmonyOS)"）。 */
  getDeviceName(): string {
    if (this.prefs !== null) {
      const saved: string = this.prefs.getSync(KEY_DEVICE_NAME, '') as string;
      if (saved.length > 0) {
        return saved;
      }
    }
    return ConnStore.autoDeviceName();
  }

  saveDeviceName(name: string): void {
    if (this.prefs === null) {
      return;
    }
    this.prefs.putSync(KEY_DEVICE_NAME, name);
    this.prefs.flush();
  }

  /** 由 deviceInfo 拼一个人看得懂的机器名；静态方法内不能用 this。 */
  private static autoDeviceName(): string {
    let name: string = '';
    try {
      const brand: string = deviceInfo.brand;
      const model: string = deviceInfo.productModel;
      if (model.length > 0) {
        name = brand.length > 0 && model.indexOf(brand) < 0 ? brand + ' ' + model : model;
      }
    } catch (e) {
      name = '';
    }
    if (name.length === 0) {
      name = 'HarmonyOS 设备';
    }
    return name;
  }

  /** Local authorization flag reported via hello; server stores nothing. */
  getDeviceAuth(): boolean {
    if (this.prefs === null) {
      return false;
    }
    return this.prefs.getSync(KEY_DEVICE_AUTH, false) as boolean;
  }

  saveDeviceAuth(on: boolean): void {
    if (this.prefs === null) {
      return;
    }
    this.prefs.putSync(KEY_DEVICE_AUTH, on);
    this.prefs.flush();
  }

  private async save(): Promise<void> {
    if (this.prefs === null) {
      return;
    }
    await this.prefs.put(KEY_CONNECTIONS, JSON.stringify(this.connections));
    await this.prefs.put(KEY_SETTINGS, JSON.stringify(this.settings));
    await this.prefs.flush();
  }
}

export const connStore: ConnStore = new ConnStore();
