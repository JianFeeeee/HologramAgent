#!/bin/bash
# 生产部署方案（**待确认，不自动执行**）
#
# 用法：
#   bash deploy/scripts/deploy-plan.sh check     # 只做部署前检查，不改任何东西
#   bash deploy/scripts/deploy-plan.sh backup    # 备份当前二进制与适配器
#   bash deploy/scripts/deploy-plan.sh deploy    # 替换二进制并重启（需显式确认）
#   bash deploy/scripts/deploy-plan.sh rollback  # 回滚到备份
#
# ★ 本脚本刻意**不包含**任何"自动回滚"逻辑：回滚要不要做、什么时候做，
#   是人的判断。脚本只负责把状态保全好，让回滚成为一条可执行的命令。

set -uo pipefail

BIN=/usr/local/bin/homed
DATA=${HA_DATA}
BAK=/var/tmp/homed-backup-$(date +%Y%m%d-%H%M%S)
NEW=${NEW_BIN:-/tmp/homed-ort}
ADAPTERS=$DATA/adapters
SVC=hologramagent.service

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }
info() { printf '  %s\n' "$*"; }

# ---------------------------------------------------------------- check
do_check() {
  say "部署前检查"
  info "服务状态: $(systemctl is-active $SVC)"
  info "当前二进制: $BIN ($(stat -c %s $BIN) 字节, $(stat -c %y $BIN | cut -d. -f1))"

  if [ ! -x "$NEW" ]; then
    info "★ 新二进制不存在: $NEW"
    return 1
  fi
  info "新二进制: $NEW ($(stat -c %s $NEW) 字节)"

  # ★ 最关键的一条：必须是 onnxruntime 构建，否则依存句法/多模态失效
  if ! go version -m "$NEW" 2>/dev/null | grep -Eq 'build[[:space:]]+-tags=.*onnxruntime'; then
    info "★★ 新二进制**不是** onnxruntime 构建 —— 拒绝部署"
    info "   （package-linux.sh:139 同样会拒绝；缺它会让依存句法与多模态失效）"
    return 1
  fi
  info "✓ onnxruntime 构建标签正确"

  # 运行期依赖
  if [ ! -f /opt/onnxruntime/libonnxruntime.so ] \
     && ! ls /usr/local/lib/libonnxruntime.so* /usr/lib/libonnxruntime.so* >/dev/null 2>&1; then
    info "★ 找不到 libonnxruntime.so —— provider 会降级"
  else
    info "✓ libonnxruntime.so 就位"
  fi

  # 模型资产（运行期目录，不影响编译）
  local mdl=$(du -sh $DATA/models 2>/dev/null | awk '{print $1}')
  info "模型资产: ${mdl:-无}（运行期目录，部署不改动）"

  # 适配器：d3eaff4 的新逻辑在"无历史清单"时不动文件
  if [ -f "$ADAPTERS/.bundled" ]; then
    info "适配器清单: 存在（升级时落后的会被更新，用户改过的会保留）"
  else
    info "适配器清单: 无 ⇒ 首次升级**不动**任何已存在的适配器文件"
  fi
  info "适配器文件数: $(ls $ADAPTERS/*.lua 2>/dev/null | wc -l)"
  return 0
}

# ---------------------------------------------------------------- backup
do_backup() {
  say "备份"
  mkdir -p "$BAK"
  cp -a "$BIN" "$BAK/homed.bin"
  [ -d "$ADAPTERS" ] && cp -a "$ADAPTERS" "$BAK/adapters"
  cp -a /etc/systemd/system/$SVC "$BAK/" 2>/dev/null || true
  info "已备份到: $BAK"
  info "  homed.bin / adapters / unit 文件"

  # ★ E-2：数据备份必须**连密钥一起**，否则恢复到别的机器时解不开。
  #
  # 背景：config.db 里的敏感项与变量库 variables.json 是 AES-GCM 加密的，
  # 密钥来自 systemd credential / $HOMED_SECURITY_KEY / <data>/security.key。
  # 只搬 config.db 而不搬密钥 ⇒ 新机器上密文全部解不开。内核会在启动时
  # 审计并告警（“检测到 N 条密文无法用当前主密钥解开”），但那时已经太晚。
  #
  # ⚠ 密钥与数据**必须分开保管**：把两者放进同一个 tar 包，
  #   等于把加密降级成明文（拿到包的人两者都有）。
  info ""
  info "★ 数据备份提示（E-2）："
  info "  config.db / variables.json 里的密钥字段是加密存储的。"
  info "  跨机器恢复必须同时具备**原机器的密钥**，否则内核会报「密文无法解开」。"
  info "  密钥位置（按优先级）：systemd credential homed_security_key"
  info "                        \$HOMED_SECURITY_KEY"
  info "                        ${DATA}/security.key （+ security.keyring 若有轮换）"
  info "  ⚠ 请把密钥与数据**分开存放**：放同一个包里等于没有加密。"
  info "  同机恢复只需照常恢复数据目录，密钥不变即可。"
  # 回滚命令
  cat > "$BAK/ROLLBACK.sh" <<RB
#!/bin/bash
# 回滚到 $BAK
set -e
systemctl stop $SVC
cp -a $BAK/homed.bin $BIN
[ -d $BAK/adapters ] && rm -rf $ADAPTERS && cp -a $BAK/adapters $ADAPTERS
systemctl start $SVC
systemctl is-active $SVC
RB
  chmod +x "$BAK/ROLLBACK.sh"
  info "回滚命令已生成: bash $BAK/ROLLBACK.sh"
}

# ---------------------------------------------------------------- deploy
do_deploy() {
  say "部署"
  do_check || { info "检查未通过，中止"; return 1; }
  echo
  read -r -p "确认替换 $BIN 并重启 $SVC ? 输入 yes 继续: " ans
  [ "$ans" = "yes" ] || { info "已取消"; return 1; }

  do_backup
  say "替换二进制"
  systemctl stop "$SVC"
  # ★ install 而非 cp：保留 setuid/权限语义，且原子替换
  install -m 0755 "$NEW" "$BIN"
  info "已安装: $BIN ($(stat -c %s $BIN) 字节)"
  systemctl start "$SVC"
  sleep 8
  if systemctl is-active --quiet "$SVC"; then
    info "✓ 服务已启动: $(systemctl is-active $SVC)"
  else
    info "✗ 服务启动失败 —— 回滚：bash $BAK/ROLLBACK.sh"
    journalctl -u "$SVC" --since '-2 min' --no-pager | tail -20
    return 1
  fi
  say "部署后验证"
  # 只说"等 20s 看日志"是不够的 —— 进程活着 ≠ agent 起来了。
  # 逐项核对，每项都对应一个真实故障模式：
  info "等待 agent 就绪（最多 60s）…"
  local ready=0
  for i in $(seq 1 30); do
    if journalctl -u "$SVC" --since '-2 min' --no-pager 2>/dev/null \
         | grep -q 'kernel ready'; then ready=1; break; fi
    sleep 2
  done
  if [ "$ready" = "1" ]; then
    info "✓ kernel ready"
  else
    info "✗ 60s 内没看到 'kernel ready' —— 回滚：bash $BAK/ROLLBACK.sh"
    journalctl -u "$SVC" --since '-2 min' --no-pager | tail -25
    return 1
  fi
  # 插件加载：生产有 18 个，少于 10 个说明加载异常
  local nplug
  nplug=$(journalctl -u "$SVC" --since '-2 min' --no-pager 2>/dev/null \
          | grep -c 'SetToolRegistrar registering tool')
  info "注册工具条目: $nplug"
  # LLM 可达：不可达会进 rollback 循环（生产设了 max_retries=100000）
  local unreach
  unreach=$(journalctl -u "$SVC" --since '-2 min' --no-pager 2>/dev/null \
           | grep -c 'LLM API unreachable')
  if [ "${unreach:-0}" -gt 3 ]; then
    info "✗ LLM 不可达 ${unreach} 次 —— 内核在 rollback 循环"
    info "   回滚：bash $BAK/ROLLBACK.sh"
    return 1
  fi
  info "✓ LLM 可达（unreachable=${unreach:-0}）"
  info "✓ ONNX provider：见日志 'onnx' 相关行（缺失时应为明确错误+降级，不静默）"
  journalctl -u "$SVC" --since '-2 min' --no-pager | grep -iE 'onnx|provider' | tail -5
}

# ---------------------------------------------------------------- rollback
do_rollback() {
  say "回滚"
  local latest
  latest=$(ls -dt /var/tmp/homed-backup-* 2>/dev/null | head -1)
  if [ -z "$latest" ]; then info "找不到备份"; return 1; fi
  info "使用备份: $latest"
  systemctl stop "$SVC"
  cp -a "$latest/homed.bin" "$BIN"
  [ -d "$latest/adapters" ] && { rm -rf "$ADAPTERS"; cp -a "$latest/adapters" "$ADAPTERS"; }
  systemctl start "$SVC"
  sleep 5
  info "回滚后: $(systemctl is-active $SVC)"
}

case "${1:-check}" in
  check)    do_check ;;
  backup)   do_backup ;;
  deploy)   do_deploy ;;
  rollback) do_rollback ;;
  *) echo "用法: $0 {check|backup|deploy|rollback}"; exit 2 ;;
esac
