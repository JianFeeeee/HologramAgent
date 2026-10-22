#!/usr/bin/env node
const path = require('path');
const fs = require('fs');
const { fork, execSync } = require('child_process');

const binDir = __dirname;
const simDir = path.resolve(binDir, '..');

function readJSON(file) {
  try { return JSON.parse(fs.readFileSync(file, 'utf8')); } catch { return null; }
}

// ---- Resolve skills directory ----
function resolveSkillsDir() {
  const skillsdirPath = path.join(simDir, '.skillsdir');
  if (fs.existsSync(skillsdirPath)) {
    return fs.readFileSync(skillsdirPath, 'utf8').trim();
  }
  const envDir = process.env.OPENCLAW_SKILLS_DIR;
  if (envDir) return path.resolve(envDir);
  const defaultDir = path.resolve(simDir, '..', 'skills');
  if (fs.existsSync(defaultDir)) return defaultDir;
  return null;
}

const skillsDir = resolveSkillsDir();

// ---- Detect system openclaw conflict ----
if (process.env.OPENCLAW_CLI !== '1') {
  const PATH = process.env.PATH || '';
  const pathEntries = PATH.split(path.delimiter);
  for (const entry of pathEntries) {
    if (entry === binDir) continue;
    const otherPath = path.join(entry, 'openclaw');
    try {
      if (fs.statSync(otherPath).isFile() && fs.realpathSync(otherPath) !== __filename) {
        process.stderr.write(`[openclaw] warning: found another openclaw at ${otherPath}\n`);
        process.stderr.write(`[openclaw] using: ${__filename}\n`);
      }
    } catch {}
  }
}

// ---- Settings helpers ----
function getSettings() {
  return readJSON(path.join(simDir, '.settings.json')) || {};
}

function writePendingSetting(key, value) {
  const pendingPath = path.join(simDir, '.settings-pending.json');
  let pending = readJSON(pendingPath) || {};
  pending[key] = value;
  fs.writeFileSync(pendingPath, JSON.stringify(pending, null, 2));
}

// ---- Permission check for plugin commands ----
function requireSkillsDir() {
  if (!skillsDir) {
    process.stderr.write('Error: cannot determine skills directory. Start the manager first or set OPENCLAW_SKILLS_DIR.\n');
    process.exit(1);
  }
  if (!fs.existsSync(skillsDir)) {
    fs.mkdirSync(skillsDir, { recursive: true });
  }
}

// ---- Main ----
const cmd = process.argv[2];

if (!cmd || cmd === 'help' || cmd === '--help') {
  console.log(`HologramAgent OpenClaw Adapter 1.0.0 (openclaw-compatible)
Usage: openclaw <command> [args]

Commands:
  plugin:install <spec>    Install a plugin (npm:xxx, clawhub:xxx, or path)
  plugin:uninstall <name>  Uninstall a plugin
  plugin:list              List installed plugins
  auth:login [url]         Show QR code for login
  auth:status              Check auth status
  config:get <key>         Get config value
  config:set <key> <val>   Set config value
  config:list              List all config
  doctor                   Run diagnostics
  update                   Check for updates
  env                      Show runtime info
  --version                Show version
  help                     Show this help`);
  process.exit(0);
}

if (cmd === '--version' || cmd === 'version') {
  console.log('HologramAgent OpenClaw Adapter 1.0.0 (openclaw-compatible)');
  process.exit(0);
}

if (cmd === 'env') {
  requireSkillsDir();
  const info = {
    homeAgent: true,
    openclawVersion: 'compatible',
    platform: process.platform,
    nodeVersion: process.version,
    skillsDir: skillsDir,
    simulatorDir: simDir,
    openclawCli: __filename,
  };
  console.log(JSON.stringify(info, null, 2));
  process.exit(0);
}

if (cmd === 'doctor') {
  console.log('OpenClaw Adapter diagnostics:');
  console.log(`  Simulator dir: ${simDir}`);
  console.log(`  Skills dir: ${skillsDir || '(not set)'}`);
  console.log(`  Node: ${process.version}`);
  console.log(`  Platform: ${process.platform}`);
  if (skillsDir) {
    const entries = fs.readdirSync(skillsDir).filter(e => !e.startsWith('.'));
    console.log(`  Installed plugins: ${entries.length}`);
  }
  console.log('  Status: OK');
  process.exit(0);
}

if (cmd === 'update') {
  console.log('Update check: HologramAgent OpenClaw Adapter is bundled with HologramAgent.');
  console.log('To update, update HologramAgent itself.');
  process.exit(0);
}

if (cmd === 'config:get') {
  const key = process.argv[3];
  if (!key) { process.stderr.write('Usage: openclaw config:get <key>\n'); process.exit(1); }
  const settings = getSettings();
  if (key in settings) {
    console.log(settings[key]);
  } else {
    process.stderr.write(`(not set: ${key})\n`);
    process.exit(1);
  }
  process.exit(0);
}

if (cmd === 'config:set') {
  const key = process.argv[3];
  const value = process.argv[4];
  if (!key || value === undefined) { process.stderr.write('Usage: openclaw config:set <key> <value>\n'); process.exit(1); }
  writePendingSetting(key, value);
  console.log(`Set: ${key}=${value} (will apply on next sync)`);
  process.exit(0);
}

if (cmd === 'config:list') {
  const settings = getSettings();
  const keys = Object.keys(settings);
  if (keys.length === 0) {
    console.log('(no settings)');
  } else {
    for (const k of keys) {
      console.log(`  ${k}=${settings[k]}`);
    }
  }
  process.exit(0);
}

if (cmd === 'auth:login' || cmd === 'auth:qrcode') {
  const url = process.argv[3] || 'openclaw://auth';
  try {
    const qrcode = require('qrcode');
    qrcode.generate(url, { small: true }, (qr) => process.stdout.write(qr + '\n'));
  } catch {
    console.log('QR not available (install qrcode package). URL:');
  }
  console.log(`  ${url}`);
  process.exit(0);
}

if (cmd === 'auth:status') {
  console.log('Auth status: running in HologramAgent mode (not applicable)');
  process.exit(0);
}

// ---- Plugin lifecycle commands - needs manager.js fork ----
if (cmd === 'plugin:install' || cmd === 'plugin:uninstall' || cmd === 'plugin:list') {
  requireSkillsDir();
  const managerPath = path.join(simDir, 'manager.js');
  if (!fs.existsSync(managerPath)) {
    process.stderr.write('Error: manager.js not found at ' + managerPath + '\n');
    process.exit(1);
  }
  const proc = fork(managerPath, [skillsDir, ...process.argv.slice(2)], {
    stdio: 'inherit',
    env: { ...process.env, OPENCLAW_CLI: '1' },
  });
  proc.on('exit', (code) => {
    // After plugin:install, notify Go side via .reload-request
    if (cmd === 'plugin:install' && code === 0) {
      const spec = process.argv[3] || '';
      const installLog = path.join(simDir, '.install-result');
      if (fs.existsSync(installLog)) {
        const result = readJSON(installLog);
        if (result && result.name) {
          fs.writeFileSync(path.join(simDir, '.reload-request'), result.name);
          console.log(`[openclaw] queued reload for plugin: ${result.name}`);
        }
        fs.rmSync(installLog, { force: true });
      }
    }
    process.exit(code);
  });
  return;
}

process.stderr.write(`Unknown command: ${cmd}\n`);
process.stderr.write("Run 'openclaw help' for usage.\n");
process.exit(1);
