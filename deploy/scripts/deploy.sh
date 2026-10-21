#!/usr/bin/env bash
set -euo pipefail

# HologramAgent 部署脚本
# 用法: cd <project-root> && sudo bash deploy/scripts/deploy.sh

PROJECT_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BIN_DIR="/usr/local/bin"
DATA_DIR="/home/newqqagent"
SERVICE_FILE="/etc/systemd/system/hologramagent.service"

echo "=== 构建 homed / waiter ==="
cd "$PROJECT_ROOT"
HOME=/root GOPATH=/root/go GOMODCACHE=/root/go/pkg/mod GOCACHE=/root/.cache/go-build make build build-cli

echo "=== 安装二进制 ==="
cp build/homed "$BIN_DIR/homed"
cp build/waiter "$BIN_DIR/waiter"
chmod 755 "$BIN_DIR/homed" "$BIN_DIR/waiter"

echo "=== 创建数据目录 ==="
mkdir -p "$DATA_DIR/plugins"

echo "=== 部署 QQ 插件 ==="
if [ -d "$PROJECT_ROOT/plugins/qq" ]; then
    mkdir -p "$DATA_DIR/plugins/qq"
    cp "$PROJECT_ROOT/plugins/qq/plugin.json" "$DATA_DIR/plugins/qq/"
    cp "$PROJECT_ROOT/plugins/qq/plugin.so" "$DATA_DIR/plugins/qq/"
    echo "QQ 插件已部署"
fi

echo "=== 安装 systemd 服务 ==="
cp "$(dirname "$0")/hologramagent.service" "$SERVICE_FILE"
systemctl daemon-reload

echo ""
echo "=== 部署完成 ==="
echo ""
echo "启动:   systemctl start hologramagent"
echo "状态:   systemctl status hologramagent"
echo "日志:   journalctl -u hologramagent -f"
echo "停止:   systemctl stop hologramagent"
echo "WebUI:  http://localhost:8080"
echo "CLI:    /home/newqqagent/cli.sock"
echo ""
echo "首次使用请通过 WebUI → 设置 配置 API 密钥"
echo "工作目录: 在 WebUI 设置 → core.agent.workdir 中配置（如 /home/newqqagent/workspace）"
