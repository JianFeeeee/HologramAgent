const fs = require('fs');
const path = require('path');
const { execSync } = require('child_process');

// ---- Utility ----
// stdout 仅承载 JSON-RPC；插件/manager 的 console 输出一律走 stderr，避免污染协议流
const rawLog = console.log.bind(console);
console.log = (...args) => process.stderr.write(args.map(String).join(' ') + '\n');

function writeJSON(obj) {
  process.stdout.write(JSON.stringify(obj) + '\n');
}
function sendError(id, code, message) {
  writeJSON({ jsonrpc: '2.0', id, error: { code, message } });
}
function notify(method, params) {
  writeJSON({ jsonrpc: '2.0', method, params });
}
function readJSON(file) {
  try { return JSON.parse(fs.readFileSync(file, 'utf8')); } catch (e) { return null; }
}

// ---- Enhanced OC Plugin Detection ----

function resolvePackageEntry(dir, pkg) {
  const candidates = [];

  if (pkg) {
    if (pkg.main) candidates.push(path.resolve(dir, pkg.main));
    if (pkg.exports) {
      const exp = pkg.exports;
      if (typeof exp === 'string') candidates.push(path.resolve(dir, exp));
      if (exp['.']) {
        const dot = exp['.'];
        if (typeof dot === 'string') candidates.push(path.resolve(dir, dot));
        if (dot.require) candidates.push(path.resolve(dir, dot.require));
        if (dot.default) candidates.push(path.resolve(dir, dot.default));
      }
    }
  }

  if (!pkg || !pkg.main) {
    for (const name of ['index.js', 'main.js', 'src/index.js', 'lib/index.js']) {
      candidates.push(path.join(dir, name));
    }
  }

  for (const cp of candidates) {
    if (fs.existsSync(cp)) return cp;
  }
  return null;
}

function ensureOCManifest(dir, name) {
  const manifestPath = path.join(dir, 'openclaw.plugin.json');
  if (fs.existsSync(manifestPath)) return;

  const pkg = readJSON(path.join(dir, 'package.json'));
  const entry = resolvePackageEntry(dir, pkg);
  const relEntry = entry ? path.relative(dir, entry) : 'index.js';

  const manifest = {
    name: name,
    version: (pkg && pkg.version) || '1.0.0',
    entry: relEntry,
    description: (pkg && pkg.description) || 'OpenClaw plugin (auto-detected)'
  };
  fs.writeFileSync(manifestPath, JSON.stringify(manifest, null, 2));
  process.stderr.write(`[manager] created synthetic manifest: ${manifestPath}\n`);
}

function tryDetectOCPackage(pkgDir, pkgName) {
  const pkg = readJSON(path.join(pkgDir, 'package.json'));
  if (!pkg) return null;

  const entryPath = resolvePackageEntry(pkgDir, pkg);
  if (!entryPath) return null;

  try {
    delete require.cache[require.resolve(entryPath)];
    const mod = require(entryPath);
    const entry = mod.default || mod;
    if (entry && typeof entry === 'object' && typeof entry.register === 'function') {
      const name = pkgName || (pkg.openclaw ? (pkg.openclaw.name || pkg.name) : pkg.name) || path.basename(pkgDir);
      ensureOCManifest(pkgDir, name);
      process.stderr.write(`[manager] enhanced detection found OC plugin: ${name} (via require)\n`);
      return { dir: pkgDir, name };
    }
  } catch (e) {
    process.stderr.write(`[manager] require detect failed for ${pkgDir}: ${e.message}\n`);
  }

  return null;
}

// ---- Plugin registry ----
const loadedPlugins = {}; // name -> { entry, tools: [{name, execute, ...}] }
const allTools = [];      // flat list of all tools across all plugins
const allProviders = {};  // type -> { name, instance } across all plugins
const registeredChannels = {}; // name -> { pluginName, channelPlugin, output, send, type, runtime, deliverers, accounts, status }

// ---- OpenClaw channelRuntime mock ----
// 真实通道插件依赖 channelRuntime 完成：入站转发（dispatchReplyWithBufferedBlockDispatcher）、
// 出站回调（dispatcherOptions.deliver，插件自带的发送实现）、poll 轮询与 call 透传。
function makeChannelRuntime(chName, ch) {
  return {
    id: chName,
    // OC 通用通道轮询输入：Go 端经 channel/send 注入的消息放入 ch.pollQueue，
    // 插件每次 chatPolls/getPolls 取走（poll 语义：取走即消费，不重复投递）。
    chatPolls: async (opts) => {
      const accountId = (opts && opts.accountId) || 'default';
      const limit = (opts && opts.limit) || 20;
      const q = ch.pollQueues.get(accountId) || [];
      const msgs = q.splice(0, limit).map((m) => ({ ...m }));
      return { msgs };
    },
    getPolls: async (opts) => {
      const accountId = (opts && opts.accountId) || 'default';
      const limit = (opts && opts.limit) || 20;
      const q = ch.pollQueues.get(accountId) || [];
      const msgs = q.splice(0, limit).map((m) => ({ ...m }));
      return { msgs };
    },
    // 插件经 runtime 直接调用服务器 API：无目标服务器，转发 Go 端作日志/降级
    call: async (method, args) => {
      notify('channel_output', { channel: chName, type: 'call', method, args });
      return { ok: true };
    },
    reply: {
      // 入站消息 + 出站 deliver 绑定。ctx 来自真实插件（Body/From/SessionKey/AccountId...）
      dispatchReplyWithBufferedBlockDispatcher: async (opts) => {
        const ctx = (opts && opts.ctx) || {};
        const dopts = (opts && opts.dispatcherOptions) || {};
        const accountId = ctx.AccountId || ctx.accountId || 'default';
        ch.deliverers.set(accountId, {
          deliver: dopts.deliver,
          typingCallbacks: dopts.typingCallbacks || {},
        });
        notify('channel_input', {
          channel: chName,
          payload: {
            content: ctx.BodyForAgent || ctx.Body || ctx.RawBody || '',
            from: ctx.From || ctx.SenderId || '',
            sessionKey: ctx.SessionKey || '',
            accountId,
            messageSid: ctx.MessageSid || '',
            chatType: ctx.ChatType || 'direct',
            raw: ctx,
          },
        });
        const buffer = [];
        return {
          sendNow: async (items) => {
            for (const item of (items || [])) await deliverItem(ch, accountId, item);
          },
          addToBuffer: async (item) => { buffer.push(item); },
          sendBuffer: async () => {
            for (const item of buffer) await deliverItem(ch, accountId, item);
            buffer.length = 0;
          },
          closeBuffer: async () => {},
        };
      },
    },
  };
}

async function deliverItem(ch, accountId, item) {
  const d = ch.deliverers.get(accountId);
  if (!d || typeof d.deliver !== 'function') return;
  const payload = {
    text: (item && item.text) || '',
    mediaUrls: (item && (item.mediaUrls || (item.mediaUrl ? [item.mediaUrl] : []))) || [],
  };
  if (d.typingCallbacks && typeof d.typingCallbacks.onReplyStart === 'function') {
    try { await d.typingCallbacks.onReplyStart(payload); } catch {}
  }
  await d.deliver(payload);
  if (d.typingCallbacks && typeof d.typingCallbacks.onCleanup === 'function') {
    try { await d.typingCallbacks.onCleanup(payload); } catch {}
  }
}

// ---- OpenClaw gateway 生命周期桥 ----
// 通道插件的心跳/收消息/状态上报都挂在 gateway.startAccount 上；startAccount 会永久挂起
// （await new Promise(()=>{})），配合 OC health-monitor 判定账号存活，必须 fire-and-forget。
async function startChannels(name) {
  const ch = registeredChannels[name];
  if (!ch || !ch.channelPlugin) return;
  const gateway = ch.channelPlugin.gateway;
  if (!gateway || typeof gateway.startAccount !== 'function') return;

  ch.runtime = ch.runtime || makeChannelRuntime(name, ch);
  ch.accounts = ch.accounts || {};
  ch.status = ch.status || { running: false, connected: false };

  const chCfg = ch.channelPlugin.config || {};
  const passCfg = ch.pluginConfig || chCfg;
  let accountIds = [];
  try {
    if (typeof chCfg.listAccountIds === 'function') {
      // OC 规范签名: listAccountIds(cfg)
      const ids = chCfg.listAccountIds(passCfg);
      accountIds = (ids && ids.then ? await ids : (Array.isArray(ids) ? ids : []));
    }
  } catch (e) {
    process.stderr.write(`[manager] ${name}: listAccountIds failed: ${e.message}\n`);
  }
  if (!Array.isArray(accountIds) || accountIds.length === 0) accountIds = ['default'];

  for (const accountId of accountIds) {
    let account = null;
    try {
      if (typeof chCfg.resolveAccount === 'function') {
        // OC 规范签名: resolveAccount(cfg, accountId)
        account = chCfg.resolveAccount(passCfg, accountId);
        if (account && account.then) account = await account;
      }
    } catch (e) {
      process.stderr.write(`[manager] ${name}: resolveAccount(${accountId}) failed: ${e.message}\n`);
    }
    if (!account) account = { accountId };

    if (ch.accounts[accountId]) continue;
    ch.accounts[accountId] = { started: true };

    const getStatus = () => ({ ...ch.status, accountId });
    const setStatus = (patch) => {
      ch.status = { ...ch.status, ...(patch || {}) };
      notify('channel_status', { channel: name, status: { ...ch.status, accountId } });
    };

    const ctx = {
      account,
      cfg: ch.pluginConfig || chCfg,
      channelRuntime: ch.runtime,
      getStatus,
      setStatus,
    };

    gateway.startAccount(ctx).catch((err) => {
      process.stderr.write(`[manager] ${name}: startAccount(${accountId}) crashed: ${err.message}\n`);
      ch.status = { ...ch.status, running: false, connected: false, error: err.message };
      notify('channel_status', { channel: name, status: { ...ch.status, accountId } });
    });
  }
}

function stopChannels(name) {
  const ch = registeredChannels[name];
  if (!ch || !ch.channelPlugin || !ch.accounts) return;
  const gateway = ch.channelPlugin.gateway;
  if (!gateway || typeof gateway.stopAccount !== 'function') return;
  for (const accountId of Object.keys(ch.accounts)) {
    const account = { accountId };
    gateway.stopAccount({ account, channelRuntime: ch.runtime, cfg: ch.pluginConfig || {} }).catch((err) => {
      process.stderr.write(`[manager] ${name}: stopAccount(${accountId}) failed: ${err.message}\n`);
    });
  }
  ch.accounts = {};
}

function registerPluginTools(name, tools, api) {
  for (const t of tools) {
    if (t && t.name) {
      t._plugin = name;
      allTools.push(t);
      notify('register', { type: 'tool', data: { name: t.name, description: t.description, parameters: t.parameters, plugin: name } });
    }
  }
  loadedPlugins[name] = { tools, api };
}

function loadPlugin(pluginDir, name) {
  // Resolve entry
  const pkg = readJSON(path.join(pluginDir, 'package.json'));
  let entryPath = null;

  if (pkg && pkg.openclaw) {
    let raw = pkg.openclaw.runtimeExtensions || pkg.openclaw.extensions;
    if (typeof raw === 'string') raw = [raw];
    if (Array.isArray(raw)) {
      for (const ext of raw) {
        let ep = path.resolve(pluginDir, ext);
        if (ep.endsWith('.ts')) { const js = ep.replace(/\.ts$/, '.js'); if (fs.existsSync(js)) { entryPath = js; break; } }
        if (fs.existsSync(ep)) { entryPath = ep; break; }
      }
    }
  }

  if (!entryPath) {
    const manifest = readJSON(path.join(pluginDir, 'openclaw.plugin.json'));
    if (manifest) {
      const ep = manifest.entry || manifest.main || 'index.js';
      entryPath = path.join(pluginDir, ep);
    }
  }

  if (!entryPath) entryPath = path.join(pluginDir, 'index.js');
  if (!fs.existsSync(entryPath)) {
    process.stderr.write(`[manager] entry not found for ${name}: ${entryPath}\n`);
    return false;
  }

  let pluginEntry;
  try { pluginEntry = require(entryPath); } catch (e) {
    process.stderr.write(`[manager] load ${name}: ${e.message}\n`);
    return false;
  }

  const entry = pluginEntry.default || pluginEntry;
  if (typeof entry !== 'object' || typeof entry.register !== 'function') {
    process.stderr.write(`[manager] ${name}: entry must export {register(api)}\n`);
    return false;
  }

  const registeredTools = [];
  const api = {
    id: name,
    name,
    version: (pkg && pkg.version) || '1.0.0',
    description: (pkg && pkg.description) || '',
    source: pluginDir,
    rootDir: pluginDir,
    config: {},
    pluginConfig: {},
    registrationMode: 'full',
    logger: { debug: () => {}, info: () => {}, warn: () => {}, error: (...args) => process.stderr.write(`[${name}] ${args.join(' ')}\n`) },
    resolvePath: (p) => path.resolve(pluginDir, p),
    registerTool: (def, opts) => {
      if (typeof def === 'function') {
        const toolCtx = { id: name, cwd: pluginDir, env: process.env, allow: ['*'] };
        const result = def(toolCtx);
        const tools = Array.isArray(result) ? result : [result];
        for (const t of tools) { if (t && typeof t.execute === 'function') registeredTools.push(t); }
        return;
      }
      if (!def || !def.name) return;
      registeredTools.push({ name: def.name, label: def.label || def.name, description: def.description || '', parameters: def.parameters || { type: 'object', properties: {} }, execute: typeof def.execute === 'function' ? def.execute : undefined });
    },
    registerProvider: (p) => {
      if (p && p.id) allProviders['llm'] = { name: p.id, instance: p };
      notify('register', { type: 'provider', data: { name: p?.id || p?.name, plugin: name } });
    },
    registerChannel: (ch) => {
      let chName = ch.name;
      let chType = ch.type || 'text';
      const chPlugin = ch.plugin;

      // OpenClaw ChannelPlugin 格式: { plugin: { id, gateway: {startAccount, stopAccount}, config: {...}, ... } }
      if (chPlugin && typeof chPlugin === 'object') {
        chName = chName || chPlugin.id || chPlugin.meta?.id || name + '-channel';
        chType = chType || (chPlugin.capabilities?.media ? 'io' : 'text');
        registeredChannels[chName] = {
          pluginName: name, channelPlugin: chPlugin, type: chType,
          deliverers: new Map(), accounts: {}, status: {},
          pollQueues: new Map(),
        };
        // gateway 生命周期桥：fire-and-forget，绝不阻塞 registerChannel
        startChannels(chName).catch((e) =>
          process.stderr.write(`[manager] ${name}: startChannels failed: ${e.message}\n`));
      } else {
        // 简单格式: { name, type, output }
        registeredChannels[chName] = { pluginName: name, output: ch.output || ch.send, type: chType };
      }

      notify('register', { type: 'channel', data: { name: chName, type: chType, id: chPlugin?.id, plugin: name } });
    },
    submitInput: (msg) => {
      notify('channel_input', { channel: name, payload: msg });
    },
    registerHook: (hook) => notify('register', { type: 'hook', data: { name: hook.name, event: hook.event } }),
    registerHttpRoute: (route) => notify('register', { type: 'http_route', data: { path: route.path, method: route.method } }),
    registerCommand: (cmd) => notify('register', { type: 'command', data: { name: cmd.name, description: cmd.description } }),
    registerService: (svc) => notify('register', { type: 'service', data: { name: svc.name } }),
    registerImageGenerationProvider: (p) => {
      if (p) allProviders['image_generation'] = { name: p.name, instance: p };
      notify('register', { type: 'image_generation_provider', data: { name: p?.name, plugin: name } });
    },
    registerWebFetchProvider: (p) => {
      if (p) allProviders['web_fetch'] = { name: p.name, instance: p };
      notify('register', { type: 'web_fetch_provider', data: { name: p?.name, plugin: name } });
    },
    registerWebSearchProvider: (p) => {
      if (p) allProviders['web_search'] = { name: p.name, instance: p };
      notify('register', { type: 'web_search_provider', data: { name: p?.name, plugin: name } });
    },
    registerSpeechProvider: (p) => {
      if (p) allProviders['speech'] = { name: p.name, instance: p };
      notify('register', { type: 'speech_provider', data: { name: p?.name, plugin: name } });
    },
    registerRealtimeTranscriptionProvider: (p) => {
      if (p) allProviders['realtime_transcription'] = { name: p.name, instance: p };
      notify('register', { type: 'realtime_transcription_provider', data: { name: p?.name, plugin: name } });
    },
    registerRealtimeVoiceProvider: (p) => {
      if (p) allProviders['realtime_voice'] = { name: p.name, instance: p };
      notify('register', { type: 'realtime_voice_provider', data: { name: p?.name, plugin: name } });
    },
    registerMediaUnderstandingProvider: (p) => {
      if (p) allProviders['media_understanding'] = { name: p.name, instance: p };
      notify('register', { type: 'media_understanding_provider', data: { name: p?.name, plugin: name } });
    },
    registerMusicGenerationProvider: (p) => {
      if (p) allProviders['music_generation'] = { name: p.name, instance: p };
      notify('register', { type: 'music_generation_provider', data: { name: p?.name, plugin: name } });
    },
    registerVideoGenerationProvider: (p) => {
      if (p) allProviders['video_generation'] = { name: p.name, instance: p };
      notify('register', { type: 'video_generation_provider', data: { name: p?.name, plugin: name } });
    },
    registerEmbeddingProvider: (p) => {
      if (p) allProviders['embedding'] = { name: p.name, instance: p };
      notify('register', { type: 'embedding_provider', data: { name: p?.name, plugin: name } });
    },
    registerMemoryEmbeddingProvider: (p) => {
      if (p) allProviders['memory_embedding'] = { name: p.name, instance: p };
      notify('register', { type: 'memory_embedding_provider', data: { name: p?.name, plugin: name } });
    },
    start: (cb) => {},
    stop: (cb) => {},
  };

  entry.register(api);
  registerPluginTools(name, registeredTools, api);
  process.stderr.write(`[manager] loaded plugin: ${name} (${registeredTools.length} tools)\n`);
  return true;
}

// ---- Install npm package ----
function installNPMPackage(spec, skillsDir) {
  // Strip npm: prefix if present
  if (spec.startsWith('npm:')) spec = spec.slice(4);
  process.stderr.write(`[manager] installing: ${spec}\n`);
  const installDir = path.join(skillsDir, '.npm_install_' + Date.now());
  fs.mkdirSync(installDir, { recursive: true });

  try {
    execSync(`npm install ${spec} --no-save --prefix "${installDir}"`, {
      cwd: installDir, stdio: ['pipe', 'pipe', 'pipe'],
      timeout: 120000, env: { ...process.env, NODE_PATH: path.join(installDir, 'node_modules') }
    });
  } catch (e) {
    fs.rmSync(installDir, { recursive: true, force: true });
    return { error: e.stderr ? e.stderr.toString() : e.message };
  }

  const nm = path.join(installDir, 'node_modules');
  if (!fs.existsSync(nm)) {
    fs.rmSync(installDir, { recursive: true, force: true });
    return { error: 'node_modules not created' };
  }

  let foundPluginDir = null;
  let foundName = null;

  const entries = fs.readdirSync(nm);
  for (const entry of entries) {
    const dir = path.join(nm, entry);
    if (!fs.statSync(dir).isDirectory()) continue;

    if (entry.startsWith('@')) {
      const subs = fs.readdirSync(dir);
      for (const sub of subs) {
        const subDir = path.join(dir, sub);
        if (fs.existsSync(path.join(subDir, 'openclaw.plugin.json')) ||
            (fs.existsSync(path.join(subDir, 'package.json')) && readJSON(path.join(subDir, 'package.json'))?.openclaw)) {
          foundPluginDir = subDir;
          foundName = entry + '/' + sub;
        }
      }
      continue;
    }

    if (fs.existsSync(path.join(dir, 'openclaw.plugin.json')) ||
        (fs.existsSync(path.join(dir, 'package.json')) && readJSON(path.join(dir, 'package.json'))?.openclaw)) {
      foundPluginDir = dir;
      foundName = entry;
    }
  }

  if (!foundPluginDir) {
    process.stderr.write(`[manager] standard scan failed, trying enhanced detection...\n`);
    for (const entry of entries) {
      if (foundPluginDir) break;
      const dir = path.join(nm, entry);
      if (!fs.statSync(dir).isDirectory()) continue;

      if (entry.startsWith('@')) {
        for (const sub of fs.readdirSync(dir)) {
          if (foundPluginDir) break;
          const subDir = path.join(dir, sub);
          const detected = tryDetectOCPackage(subDir, entry + '/' + sub);
          if (detected) { foundPluginDir = detected.dir; foundName = detected.name; }
        }
      } else {
        const detected = tryDetectOCPackage(dir, entry);
        if (detected) { foundPluginDir = detected.dir; foundName = detected.name; }
      }
    }
  }

  if (!foundPluginDir) {
    fs.rmSync(installDir, { recursive: true, force: true });
    return { error: `no OC plugin found in installed package "${spec}"` };
  }

  // Copy to skills dir
  const targetDir = path.join(skillsDir, foundName);
  if (fs.existsSync(targetDir)) fs.rmSync(targetDir, { recursive: true, force: true });
  cpSync(foundPluginDir, targetDir);
  fs.rmSync(installDir, { recursive: true, force: true });

  return { name: foundName, dir: targetDir };
}

function cpSync(src, dst) {
  fs.mkdirSync(dst, { recursive: true });
  for (const entry of fs.readdirSync(src)) {
    const s = path.join(src, entry);
    const d = path.join(dst, entry);
    if (fs.statSync(s).isDirectory()) {
      cpSync(s, d);
    } else {
      fs.copyFileSync(s, d);
    }
  }
}

// ---- OpenClaw CLI compatibility ----

function showQR(text) {
  try {
    const qrcode = require('qrcode');
    qrcode.generate(text, { small: true }, (qr) => process.stdout.write(qr + '\n'));
  } catch {
    process.stdout.write(`QR: ${text}\n`);
    process.stdout.write('(install qrcode package for QR display: npm install qrcode)\n');
  }
}

async function runCLI(skillsDir, cliArgs) {
  const cmd = cliArgs[0] || '';

  switch (cmd) {
    case 'plugin:install': {
      const spec = cliArgs[1];
      if (!spec) throw new Error('Usage: openclaw plugin:install <npm:package|clawhub:name|path>');
      const result = installNPMPackage(spec, skillsDir);
      if (result.error) throw new Error(result.error);
      console.log(`Installed: ${result.name}`);
      // Write install result for CLI wrapper to pick up
      const simDir = path.dirname(process.argv[1]);
      fs.writeFileSync(path.join(simDir, '.install-result'), JSON.stringify({ name: result.name }));
      break;
    }

    case 'plugin:uninstall': {
      const name = cliArgs[1];
      if (!name) throw new Error('Usage: openclaw plugin:uninstall <name>');
      const targetDir = path.join(skillsDir, name);
      if (!fs.existsSync(targetDir)) throw new Error(`Plugin not found: ${name}`);
      fs.rmSync(targetDir, { recursive: true, force: true });
      console.log(`Uninstalled: ${name}`);
      break;
    }

    case 'plugin:list': {
      if (!fs.existsSync(skillsDir)) { console.log('(no plugins)'); break; }
      let count = 0;
      for (const entry of fs.readdirSync(skillsDir)) {
        if (entry.startsWith('.')) continue;
        const pluginDir = path.join(skillsDir, entry);
        if (!fs.statSync(pluginDir).isDirectory()) continue;
        const pkg = readJSON(path.join(pluginDir, 'package.json'));
        const manifest = readJSON(path.join(pluginDir, 'openclaw.plugin.json'));
        if (pkg || manifest) {
          const version = pkg?.version || manifest?.version || '?';
          const desc = pkg?.description || manifest?.description || '';
          console.log(`  ${entry} v${version}${desc ? ' — ' + desc : ''}`);
          count++;
        }
      }
      if (count === 0) console.log('(no OpenClaw plugins)');
      break;
    }

    case 'auth:login':
    case 'auth:qrcode': {
      const url = cliArgs[1] || 'openclaw://auth';
      console.log('Scan the QR code to log in:');
      showQR(url);
      console.log('\nOr open this URL:');
      console.log(`  ${url}`);
      break;
    }

    case 'auth:status': {
      console.log('Auth status: not implemented (running in HologramAgent mode)');
      break;
    }

    case 'config:get': {
      const key = cliArgs[1];
      if (!key) throw new Error('Usage: openclaw config:get <key>');
      // TODO: read from HologramAgent config system when bridged
      console.log(`(not available in CLI mode: ${key})`);
      break;
    }

    case 'config:set': {
      const key = cliArgs[1];
      const value = cliArgs[2];
      if (!key || value === undefined) throw new Error('Usage: openclaw config:set <key> <value>');
      // TODO: write to HologramAgent config system when bridged
      console.log(`(not available in CLI mode: ${key}=${value})`);
      break;
    }

    case 'config:list':
      console.log('(not available in CLI mode)');
      break;

    case 'env': {
      const info = {
        homeAgent: true,
        openclawVersion: 'compatible',
        platform: process.platform,
        nodeVersion: process.version,
        skillsDir,
      };
      console.log(JSON.stringify(info, null, 2));
      break;
    }

    case '--version':
    case 'version':
      console.log('HologramAgent OpenClaw Adapter 1.0.0 (openclaw-compatible)');
      break;

    case 'help':
    case '--help':
      console.log(`Usage: openclaw <command> [args]

Commands:
  plugin:install <spec>    Install a plugin (npm:xxx, clawhub:xxx, or path)
  plugin:uninstall <name>  Uninstall a plugin
  plugin:list              List installed plugins
  auth:login [url]         Show QR code for login/binding
  auth:status              Check authentication status
  config:get <key>         Get config value
  config:set <key> <val>   Set config value
  config:list              List all config
  env                      Show runtime environment info
  --version                Show version
  help                     Show this help`);
      break;

    default:
      throw new Error(`Unknown command: ${cmd}\nRun 'openclaw help' for usage.`);
  }
}

// ---- Main ----
const args = process.argv.slice(2);
if (args.length < 1) {
  process.stderr.write('[manager] usage: node main.js <skills-dir> [openclaw-command...]\n');
  process.exit(1);
}

const skillsDir = path.resolve(args[0]);

// CLI mode: if additional args provided, run as openclaw CLI command and exit
if (args.length > 1) {
  const cliArgs = args.slice(1);
  runCLI(skillsDir, cliArgs).then(() => process.exit(0)).catch(e => {
    process.stderr.write(`Error: ${e.message}\n`);
    process.exit(1);
  });
  return;
}

// Server mode: persist skills dir reference for CLI wrapper
try {
  const simDir = path.dirname(process.argv[1]);
  fs.writeFileSync(path.join(simDir, '.skillsdir'), skillsDir);
} catch (e) {
  process.stderr.write(`[manager] warning: could not write .skillsdir: ${e.message}\n`);
}

// Add our bin directory to PATH so subprocesses can find 'openclaw' CLI
try {
  const binDir = path.join(path.dirname(process.argv[1]), 'bin');
  if (fs.existsSync(binDir)) {
    const PATH = process.env.PATH || '';
    if (!PATH.includes(binDir)) {
      process.env.PATH = binDir + path.delimiter + PATH;
    }
  }
} catch (e) {
  process.stderr.write(`[manager] warning: could not update PATH: ${e.message}\n`);
}

// Load existing plugins on startup
process.stderr.write(`[manager] scanning: ${skillsDir}\n`);
if (fs.existsSync(skillsDir)) {
  for (const entry of fs.readdirSync(skillsDir)) {
    if (entry.startsWith('.')) continue;
    const pluginDir = path.join(skillsDir, entry);
    if (!fs.statSync(pluginDir).isDirectory()) continue;
    if (fs.existsSync(path.join(pluginDir, 'main.js')) || fs.existsSync(path.join(pluginDir, 'main.py'))) {
      continue;
    }
    if (fs.existsSync(path.join(pluginDir, 'openclaw.plugin.json')) ||
        (fs.existsSync(path.join(pluginDir, 'package.json')) && readJSON(path.join(pluginDir, 'package.json'))?.openclaw)) {
      loadPlugin(pluginDir, entry);
    } else {
      const detected = tryDetectOCPackage(pluginDir, entry);
      if (detected) {
        loadPlugin(detected.dir, detected.name);
      }
    }
  }
}

// ---- JSON-RPC ----
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin, output: process.stdout, terminal: false });

rl.on('line', async (line) => {
  let req;
  try { req = JSON.parse(line); } catch { sendError(null, -32700, 'Parse error'); return; }

  const id = req.id;
  const method = req.method;

  if (method === 'ping') {
    writeJSON({ jsonrpc: '2.0', id, result: { status: 'ok', plugins: Object.keys(loadedPlugins).length } });
    return;
  }

  // 向通道注入输入：Go 端外部输入（webui 会话/其他插件）→ pollQueue → 插件 chatPolls 轮询取走
  if (method === 'channel/send') {
    const chName = req.params?.channel;
    const payload = req.params?.payload || {};
    const accountId = (req.params && req.params.accountId) || payload.accountId || 'default';
    const ch = registeredChannels[chName];
    if (!ch) { sendError(id, -32601, `channel not found: ${chName}`); return; }
    ch.pollQueues.set(accountId, ch.pollQueues.get(accountId) || []);
    ch.pollQueues.get(accountId).push({
      sessionKey: payload.sessionKey || `${chName}:${accountId}:${payload.from || 'poll'}`,
      from: payload.from || '',
      text: payload.content || payload.text || '',
      type: payload.type || 'text',
      timestamp: Date.now(),
    });
    writeJSON({ jsonrpc: '2.0', id, result: { status: 'queued', channel: chName, accountId } });
    return;
  }

  // 停止通道账号（stopAccount：停止心跳/轮询）。channel 可省略（停该插件全部通道）
  if (method === 'channel/stop') {
    const chName = req.params?.channel;
    const accountId = req.params?.accountId;
    if (chName) {
      const ch = registeredChannels[chName];
      if (!ch) { sendError(id, -32601, `channel not found: ${chName}`); return; }
      if (accountId) {
        const gateway = ch.channelPlugin && ch.channelPlugin.gateway;
        if (gateway && typeof gateway.stopAccount === 'function') {
          gateway.stopAccount({ account: { accountId }, channelRuntime: ch.runtime, cfg: ch.pluginConfig || {} }).catch((e) =>
            process.stderr.write(`[manager] ${chName}: stopAccount(${accountId}) failed: ${e.message}\n`));
        }
        if (ch.accounts) delete ch.accounts[accountId];
      } else {
        stopChannels(chName);
      }
      writeJSON({ jsonrpc: '2.0', id, result: { status: 'stopped', channel: chName, accountId: accountId || 'all' } });
    } else {
      for (const name of Object.keys(registeredChannels)) stopChannels(name);
      writeJSON({ jsonrpc: '2.0', id, result: { status: 'stopped', channel: 'all' } });
    }
    return;
  }

  // 启动通道账号（gateway.startAccount fire-and-forget）
  if (method === 'channel/start') {
    const chName = req.params?.channel;
    const ch = registeredChannels[chName];
    if (!ch) { sendError(id, -32601, `channel not found: ${chName}`); return; }
    ch.accounts = {};
    startChannels(chName).catch((e) => process.stderr.write(`[manager] ${chName}: startChannels failed: ${e.message}\n`));
    writeJSON({ jsonrpc: '2.0', id, result: { status: 'started', channel: chName } });
    return;
  }

  if (method === 'plugins/list') {
    const list = Object.entries(loadedPlugins).map(([name, p]) => ({
      name,
      tools: p.tools.map(t => ({ name: t.name, description: t.description })),
    }));
    writeJSON({ jsonrpc: '2.0', id, result: { plugins: list } });
    return;
  }

  if (method === 'plugins/channels') {
    const list = Object.entries(registeredChannels).map(([name, ch]) => ({
      name,
      plugin: ch.pluginName,
      type: ch.type || 'text',
      status: ch.status || {},
      accounts: ch.accounts ? Object.keys(ch.accounts) : [],
    }));
    writeJSON({ jsonrpc: '2.0', id, result: { channels: list } });
    return;
  }

  if (method === 'plugins/install') {
    const pkg = req.params?.package;
    if (!pkg) { sendError(id, -32602, 'package required'); return; }

    const result = installNPMPackage(pkg, skillsDir);
    if (result.error) {
      sendError(id, -32603, result.error);
      return;
    }

    // Load the newly installed plugin
    const ok = loadPlugin(result.dir, result.name);
    if (!ok) {
      sendError(id, -32603, `failed to load installed plugin: ${result.name}`);
      return;
    }

    writeJSON({ jsonrpc: '2.0', id, result: { name: result.name, tools: loadedPlugins[result.name].tools.map(t => t.name) } });
    return;
  }

  if (method === 'plugins/uninstall') {
    const name = req.params?.name;
    if (!name) { sendError(id, -32602, 'name required'); return; }

    if (!loadedPlugins[name]) { sendError(id, -32601, `plugin not found: ${name}`); return; }

    // 先优雅停靠通道账号（stopAccount 停止心跳/轮询），再卸载
    for (const chName of Object.keys(registeredChannels)) {
      if (registeredChannels[chName].pluginName === name) {
        stopChannels(chName);
      }
    }

    // Remove tools
    const idxs = [];
    for (let i = allTools.length - 1; i >= 0; i--) {
      if (allTools[i]._plugin === name) allTools.splice(i, 1);
    }
    delete loadedPlugins[name];

    // Remove directory
    const pluginDir = path.join(skillsDir, name);
    if (fs.existsSync(pluginDir)) fs.rmSync(pluginDir, { recursive: true, force: true });

    writeJSON({ jsonrpc: '2.0', id, result: { status: 'uninstalled', name } });
    return;
  }

  if (method === 'plugins/detect') {
    const pluginDir = req.params?.dir;
    const name = req.params?.name || (pluginDir ? path.basename(pluginDir) : '');
    if (!pluginDir) { sendError(id, -32602, 'dir required'); return; }

    const resolvedDir = path.resolve(pluginDir);
    if (!fs.existsSync(resolvedDir)) {
      sendError(id, -32601, `directory not found: ${resolvedDir}`);
      return;
    }

    if (loadedPlugins[name]) {
      writeJSON({ jsonrpc: '2.0', id, result: { name, tools: loadedPlugins[name].tools.map(t => t.name), type: 'loaded' } });
      return;
    }

    // Install npm dependencies if package.json exists with deps
    const pkgPath = path.join(resolvedDir, 'package.json');
    if (fs.existsSync(pkgPath)) {
      const pkg = readJSON(pkgPath);
      if (pkg) {
        const hasDeps = (pkg.dependencies && Object.keys(pkg.dependencies).length > 0) ||
                        (pkg.devDependencies && Object.keys(pkg.devDependencies).length > 0);
        if (hasDeps) {
          try {
            execSync(`npm install --no-save --prefix "${resolvedDir}"`, {
              cwd: resolvedDir, stdio: ['pipe', 'pipe', 'pipe'],
              timeout: 120000, env: { ...process.env, NODE_PATH: path.join(resolvedDir, 'node_modules') }
            });
            process.stderr.write(`[manager] installed dependencies for ${name}\n`);
          } catch (e) {
            process.stderr.write(`[manager] npm install failed for ${name}: ${e.message}\n`);
          }
        }
      }
    }

    let detected = null;
    if (fs.existsSync(path.join(resolvedDir, 'openclaw.plugin.json')) ||
        (fs.existsSync(path.join(resolvedDir, 'package.json')) && readJSON(path.join(resolvedDir, 'package.json'))?.openclaw)) {
      detected = { dir: resolvedDir, name };
    } else {
      detected = tryDetectOCPackage(resolvedDir, name);
    }

    if (!detected) {
      sendError(id, -32601, `no OC plugin detected in: ${resolvedDir}`);
      return;
    }

    const ok = loadPlugin(detected.dir, detected.name);
    if (!ok) {
      sendError(id, -32603, `failed to load detected plugin: ${detected.name}`);
      return;
    }

    writeJSON({ jsonrpc: '2.0', id, result: { name: detected.name, tools: loadedPlugins[detected.name].tools.map(t => t.name), type: detected === Object(detected) && detected.dir === resolvedDir ? 'detected' : 'standard' } });
    return;
  }

  if (method === 'plugins/load') {
    const dir = req.params?.dir;
    const pluginName = req.params?.name || (dir ? path.basename(dir) : '');
    if (!dir) { sendError(id, -32602, 'dir required'); return; }
    const resolvedDir = path.resolve(dir);
    if (!fs.existsSync(resolvedDir)) { sendError(id, -32601, `directory not found: ${resolvedDir}`); return; }

    if (loadedPlugins[pluginName]) {
      writeJSON({ jsonrpc: '2.0', id, result: { name: pluginName, tools: loadedPlugins[pluginName].tools.map(t => t.name), type: 'already_loaded' } });
      return;
    }

    const ok = loadPlugin(resolvedDir, pluginName);
    if (!ok) { sendError(id, -32603, `failed to load plugin: ${pluginName}`); return; }

    writeJSON({ jsonrpc: '2.0', id, result: { name: pluginName, tools: loadedPlugins[pluginName].tools.map(t => t.name), type: 'loaded' } });
    return;
  }

  if (method === 'tools/list') {
    const tools = allTools.map(t => ({
      name: t.name,
      description: t.description || '',
      inputSchema: t.parameters || { type: 'object', properties: {} },
    }));
    writeJSON({ jsonrpc: '2.0', id, result: { tools } });
    return;
  }

  if (method === 'tools/call') {
    const params = req.params || {};
    const toolName = params.name;
    const args = params.arguments || {};

    // 通道输出路由：toolName 匹配已注册通道名时，调通道的输出 handler
    const ch = registeredChannels[toolName];
    if (ch) {
      try {
        const channelPlugin = ch.channelPlugin;
        let metaObj = {};
        try { metaObj = typeof args.meta === 'string' ? JSON.parse(args.meta) : (args.meta || {}); } catch {}
        const accountId = metaObj.accountId || 'default';

        if (channelPlugin && channelPlugin.outbound) {
          // 旧格式 outbound 直发
          const to = metaObj.user_id || metaObj.to || metaObj.group_id || '';
          const ctx = { to, text: args.payload || '', mediaUrl: metaObj.mediaUrl || '', cfg: {}, accountId };
          let result;
          if (ctx.mediaUrl && channelPlugin.outbound.sendMedia) {
            result = await channelPlugin.outbound.sendMedia(ctx);
          } else if (channelPlugin.outbound.sendText) {
            result = await channelPlugin.outbound.sendText(ctx);
          } else {
            throw new Error(`channel ${toolName} has no sendText/sendMedia handler`);
          }
          writeJSON({ jsonrpc: '2.0', id, result: { status: 'sent', result } });
        } else if (typeof ch.output === 'function') {
          const result = await ch.output(args.payload, args.type, args.meta);
          writeJSON({ jsonrpc: '2.0', id, result: { status: 'sent', result } });
        } else if (typeof ch.send === 'function') {
          const result = await ch.send(args.payload, args.meta);
          writeJSON({ jsonrpc: '2.0', id, result: { status: 'sent', result } });
        } else if (ch.deliverers && ch.deliverers.has(accountId)) {
          // OpenClaw ChannelPlugin 事件式发送：回复交给插件在入站时挂载的 deliver（插件自带发送实现）
          await deliverItem(ch, accountId, {
            text: args.payload || '',
            mediaUrls: metaObj.mediaUrl ? [metaObj.mediaUrl] : [],
          });
          writeJSON({ jsonrpc: '2.0', id, result: { status: 'sent', via: 'channelRuntime.deliver' } });
        } else if (ch.runtime) {
          // 通道已启动但尚未收到入站消息（deliver 未建立）：降级为 channel_output 事件
          notify('channel_output', {
            channel: toolName, type: 'message',
            text: args.payload || '', mediaUrl: metaObj.mediaUrl || '', to: metaObj.user_id || '',
          });
          writeJSON({ jsonrpc: '2.0', id, result: { status: 'queued', via: 'channel_output' } });
        } else {
          sendError(id, -32601, `channel ${toolName} has no output handler`);
        }
      } catch (e) { sendError(id, -32603, e.message); }
      return;
    }

    const tool = allTools.find(t => t.name === toolName);
    if (!tool) { sendError(id, -32601, `Tool not found: ${toolName}`); return; }
    if (typeof tool.execute !== 'function') { sendError(id, -32603, `Tool ${toolName} has no execute`); return; }

    try {
      const result = await tool.execute('mgr-call-1', args, undefined, undefined);
      if (result && typeof result === 'object' && Array.isArray(result.content)) {
        writeJSON({ jsonrpc: '2.0', id, result });
      } else {
        const text = typeof result === 'string' ? result : JSON.stringify(result);
        writeJSON({ jsonrpc: '2.0', id, result: { content: [{ type: 'text', text }] } });
      }
    } catch (e) { sendError(id, -32603, e.message); }
    return;
  }

  if (method === 'provider/call') {
    const { type, action, args } = req.params || {};
    if (!type) { sendError(id, -32602, 'type required'); return; }

    const provider = allProviders[type];
    if (!provider) { sendError(id, -32601, `Provider not found: ${type}`); return; }

    const methodName = action || 'execute';
    if (typeof provider.instance[methodName] !== 'function') {
      sendError(id, -32603, `Provider ${type} has no method ${methodName}`);
      return;
    }

    try {
      const result = await provider.instance[methodName](args);
      if (result && typeof result === 'object' && Array.isArray(result.content)) {
        writeJSON({ jsonrpc: '2.0', id, result });
      } else {
        const text = typeof result === 'string' ? result : JSON.stringify(result);
        writeJSON({ jsonrpc: '2.0', id, result: { content: [{ type: 'text', text }] } });
      }
    } catch (e) { sendError(id, -32603, e.message); }
    return;
  }

  sendError(id, -32601, `Method not found: ${method}`);
});

// ---- 优雅停靠：进程退出前逐个 stopAccount（插件停止心跳/轮询） ----
let shuttingDown = false;
async function shutdown() {
  if (shuttingDown) return;
  shuttingDown = true;
  for (const name of Object.keys(registeredChannels)) stopChannels(name);
  setTimeout(() => process.exit(0), 2000);
}
process.on('SIGTERM', shutdown);
process.on('SIGINT', shutdown);
