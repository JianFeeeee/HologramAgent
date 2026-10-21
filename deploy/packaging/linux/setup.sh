#!/usr/bin/env bash
# HologramAgent 首次初始化脚本
# 在安装后执行，生成凭据并初始化数据库
set -e

DATA_DIR="${HOLOGRAM_DATA:-/var/lib/hologram}"
CRED_FILE="${DATA_DIR}/credentials.txt"
CONFIG_DB="${DATA_DIR}/config.db"
WAITER_CONF="${DATA_DIR}/waiter.yaml"
INITCONFIG_BIN="/usr/bin/initconfig"
BUNDLED_MODEL_DIR="/usr/lib/hologram/models/chinese-clip-vit-b16-onnx"
MODEL_LINK="${DATA_DIR}/models/chinese-clip-vit-b16-onnx"

# 模型随 server/full 包安装到只读的 /usr/lib；配置默认仍指向 dataDir/models。
# 用符号链接把两者接起来，既不复制 754MB，也保持 dataDir 可迁移语义。
# 用户已有自定义目录时绝不覆盖；升级时既有链接自然指向新版包内容。
if [ -d "$BUNDLED_MODEL_DIR" ]; then
  mkdir -p "${DATA_DIR}/models"
  if [ ! -e "$MODEL_LINK" ] && [ ! -L "$MODEL_LINK" ]; then
    ln -s "$BUNDLED_MODEL_DIR" "$MODEL_LINK"
  fi
fi

# 如果已经初始化过，只跳过凭据/数据库生成；上面的模型链接仍须在升级时补齐。
if [ -f "$CONFIG_DB" ] && [ -f "$CRED_FILE" ]; then
  exit 0
fi

mkdir -p "$DATA_DIR"

# 生成随机凭据。
#
# 允许环境变量覆盖：安装器（包括 Windows 上的 WSL 引导安装）已经在界面上
# 向用户收过这些值，若不接受传入就只能两个地方各生成一份，用户看到的那份
# 与实际写入 config.db 的那份不一致——那种错会直接表现为「登录不上」。
API_KEY="${HOLOGRAM_API_KEY:-$(cat /proc/sys/kernel/random/uuid 2>/dev/null | tr -d '-' || echo "hologram$(date +%s)")}"
WEBUI_USER="${WEBUI_USER:-admin}"
WEBUI_PASS="${WEBUI_PASS:-$(openssl rand -hex 12 2>/dev/null || echo "hologram")}"

# 初始化数据库
if [ -x "$INITCONFIG_BIN" ]; then
  "$INITCONFIG_BIN" \
    -data "$DATA_DIR" \
    -username "$WEBUI_USER" \
    -password "$WEBUI_PASS" \
    -apikey "$API_KEY" 2>/dev/null
fi

# 保存凭据
cat > "$CRED_FILE" << CRED
===================================
  HologramAgent 初始配置信息
  请妥善保管，安装后仅此一份
===================================
WebUI 地址:    http://localhost:8080
API Key:       ${API_KEY}
WebUI 用户名:  ${WEBUI_USER}
WebUI 密码:    ${WEBUI_PASS}
CLI Socket:    ${DATA_DIR}/cli.sock
===================================
CRED
chmod 600 "$CRED_FILE"

# 配置 waiter CLI
cat > "$WAITER_CONF" << WAITER
socket: "${DATA_DIR}/cli.sock"
api_key: ${API_KEY}
default: local
connections:
  - name: local
    socket: "${DATA_DIR}/cli.sock"
    api_key: ${API_KEY}
WAITER

echo ""
echo "============================================"
echo "  HologramAgent 初始化完成"
echo "============================================"
cat "$CRED_FILE"
