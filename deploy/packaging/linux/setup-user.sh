#!/bin/sh
# 创建 HologramAgent 的系统用户与组（幂等）。
#
# ## 为什么需要单独一个脚本
#
# 服务单元现在写死了 `User=hologramagent`。若安装时没有这个用户，
# systemd 会在启动时报 "Failed to determine user credentials" 并拒绝启动
# —— 这是**响亮失败**，比默默用 root 好，但用户装完起不来同样是事故。
#
# 三条安装路径（deb postinst / rpm %pre / tar install.sh）都要它，
# 各写一遍必然漂移（本仓已多次踩过这个坑）。所以收敛到这一个文件，
# 由打包脚本装到 /usr/lib/hologram/ 并调用。
#
# ## 为什么优先 systemd-sysusers
#
# sysusers 是声明式、幂等的，且不会：
#   · 给用户分配一个可猜测的 UID（useradd --system 也还行）
#   · 在 /etc/passwd 里留下不一致的 home/shell
# 老系统没有 sysusers 时退回 useradd。
#
# ## 为什么 shell 是 nologin
#
# 服务不需要登录。留 /bin/sh 会让这个账户成为一个可用的远程登录目标
# （配合弱密码或密钥泄露）。nologin 让"有这个账户"不再是攻击面。
set -e

USER_NAME="${HOLOGRAM_USER:-hologramagent}"
GROUP_NAME="${HOLOGRAM_GROUP:-hologramagent}"
DATA_DIR="${HOLOGRAM_DATA:-/var/lib/hologram}"

if ! command -v getent >/dev/null 2>&1; then
    echo "W: 找不到 getent，跳过用户创建检查" >&2
    exit 0
fi

# 组
if ! getent group "$GROUP_NAME" >/dev/null 2>&1; then
    if command -v groupadd >/dev/null 2>&1; then
        groupadd --system "$GROUP_NAME" 2>/dev/null || true
    fi
fi

# 用户
if ! getent passwd "$USER_NAME" >/dev/null 2>&1; then
    if command -v systemd-sysusers >/dev/null 2>&1; then
        # 声明式：重复执行无副作用
        printf 'u %s - "HologramAgent service" %s %s\n' \
            "$USER_NAME" "$DATA_DIR" "/usr/sbin/nologin" \
            | systemd-sysusers - 2>/dev/null || true
    fi
    # sysusers 未生效（老系统）时退回 useradd
    if ! getent passwd "$USER_NAME" >/dev/null 2>&1 && command -v useradd >/dev/null 2>&1; then
        useradd --system \
            --gid "$GROUP_NAME" \
            --home-dir "$DATA_DIR" \
            --shell /usr/sbin/nologin \
            --comment "HologramAgent service" \
            "$USER_NAME" 2>/dev/null || true
    fi
fi

if ! getent passwd "$USER_NAME" >/dev/null 2>&1; then
    echo "E: 无法创建用户 $USER_NAME —— 服务单元要求 User=$USER_NAME，将无法启动。" >&2
    echo "E: 请手工创建：useradd --system --gid $GROUP_NAME --home-dir $DATA_DIR --shell /usr/sbin/nologin $USER_NAME" >&2
    exit 1
fi

# 数据目录属主（首次安装时目录可能还不存在）
mkdir -p "$DATA_DIR" 2>/dev/null || true
chown -R "$USER_NAME:$GROUP_NAME" "$DATA_DIR" 2>/dev/null || true

exit 0
