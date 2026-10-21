<#
.SYNOPSIS
  在 WSL2 中安装 HologramAgent（homed + 插件 + WebUI）。

.DESCRIPTION
  Windows 不再提供 homed 的原生安装。原因见 cmd/homed/platform_windows.go：
  homed 的插件体系依赖「继承的 fd」与「统一共享内存区的段内偏移解引用」，
  Windows 的句柄模型无法表达这两者；强行适配等于再维护一套平台专属 ABI，
  而 C ABI 时代三套 ABI 并存正是「改写型插件在某个平台上静默失效」的根因。

  本脚本因此把 Windows 安装流程变成一条引导链：
    检测 WSL → 必要时引导安装 → 配置（默认版本 2 / systemd）
    → 把 **Linux 包** 送进发行版 → 在 WSL 内按 Linux 的方式安装。

  它复用 Linux 侧的安装包与初始化脚本，不另写一套安装逻辑——
  「WSL 里就是普通 linux/amd64」这一点必须保持成立，否则等于又开了第三个平台。

.PARAMETER PayloadDir
  内含 Linux 安装包的目录（安装器把它解到临时目录后传进来）。
  优先取 *.deb；没有 deb 时回退 *.tar.gz。

.PARAMETER Distro
  目标发行版名。省略则用默认发行版；没有发行版时引导安装 Ubuntu。

.PARAMETER DataDir
  WSL 内的数据目录。默认 /var/lib/hologram（与 Linux 原生安装一致）。
  不建议放 /mnt/c/...：跨文件系统 IO 慢，且 inotify 语义受限。

.NOTES
  ⚠️ 本脚本在开发环境（Linux）中只能做语法/逻辑审查，**未在真实 Windows + WSL
  上执行过**。首次使用请逐段核对输出；下面每个阶段都打印了实际执行的命令，
  便于定位到具体哪一步与预期不符。
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$PayloadDir,
    [string]$Distro = "",
    [string]$DataDir = "/var/lib/hologram",
    [string]$ApiKey = "",
    [string]$WebUIUser = "",
    [string]$WebUIPass = "",
    [switch]$Uninstall
)

$ErrorActionPreference = "Stop"
$script:StageNo = 0
$script:DistroName = $Distro

function Write-Stage([string]$Text) {
    $script:StageNo++
    Write-Host ""
    Write-Host ("=" * 64) -ForegroundColor DarkGray
    Write-Host ("[$script:StageNo] $Text") -ForegroundColor Cyan
    Write-Host ("=" * 64) -ForegroundColor DarkGray
}

function Write-Ok([string]$Text) { Write-Host "  ✓ $Text" -ForegroundColor Green }
function Write-Warn2([string]$Text) { Write-Host "  ! $Text" -ForegroundColor Yellow }
function Fail([string]$Text, [string]$Hint = "") {
    Write-Host ""
    Write-Host "  安装中止：$Text" -ForegroundColor Red
    if ($Hint) { Write-Host "  $Hint" -ForegroundColor Yellow }
    exit 1
}

# ── 0. 前置检查 ────────────────────────────────────────────────────────────
Write-Stage "前置检查"

$identity = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
if (-not $identity.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    # 装 WSL 与写 \\wsl$ 都需要管理员。不静默提权：用户应当看到发生了什么。
    Fail "需要管理员权限" "请以管理员身份重新运行安装程序。"
}
Write-Ok "管理员权限"

if (-not (Get-Command wsl.exe -ErrorAction SilentlyContinue)) {
    Write-Warn2 "未找到 wsl.exe"
    Write-Host "  homed 不再提供 Windows 原生版本，必须通过 WSL2 运行。"
    Write-Host ""
    Write-Host "  在管理员 PowerShell 中执行：" -ForegroundColor Yellow
    Write-Host "      wsl --install" -ForegroundColor White
    Write-Host "  然后重启 Windows，再重新运行本安装程序。"
    Write-Host ""
    Write-Host "  （Windows 10 需 2004+ 且启用虚拟机平台；Windows 11 开箱可用）"
    exit 20
}
Write-Ok "wsl.exe 可用"

# ── 1. 检测 WSL 状态与发行版 ───────────────────────────────────────────────
Write-Stage "检测 WSL 与发行版"

# wsl -l -v 在「没有发行版」时返回非零，且输出是 UTF-16LE——直接解析会踩编码坑。
# 用 --status 取默认发行版，再单独枚举列表。
$distros = @()
try {
    $raw = (& wsl.exe -l -q 2>$null | Out-String)
    $distros = $raw -split "`r?`n" | ForEach-Object { $_.Trim() } | Where-Object { $_ -ne "" }
} catch {
    $distros = @()
}

if ($distros.Count -eq 0) {
    Write-Warn2 "WSL 已安装，但没有任何发行版"
    Write-Host ""
    Write-Host "  请先安装发行版（推荐 Ubuntu）：" -ForegroundColor Yellow
    Write-Host "      wsl --install -d Ubuntu" -ForegroundColor White
    Write-Host ""
    Write-Host "  首次启动 Ubuntu 会要求创建 Linux 用户名与密码，完成后重新运行本安装程序。"
    exit 21
}

if ($script:DistroName -eq "") {
    try {
        $script:DistroName = (& wsl.exe --status 2>$null | Select-String -Pattern "Default Distribution" |
            ForEach-Object { ($_ -split ":")[1].Trim() })
    } catch { }
    if (-not $script:DistroName) { $script:DistroName = $distros[0] }
}
Write-Ok "发行版：$($script:DistroName)（共 $($distros.Count) 个：$($distros -join ', ')）"

# ── 2. 确保是 WSL2 ─────────────────────────────────────────────────────────
Write-Stage "确保使用 WSL2"

# WSL1 没有真正的 Linux 内核、没有 systemd，且在共享内存/事件语义上与 WSL2 不同。
# homed 依赖 eventfd + mmap 语义，WSL1 会以难以诊断的方式失败，因此显式要求 WSL2。
try {
    $verLine = (& wsl.exe -l -v 2>$null | Out-String) -split "`r?`n" |
        Where-Object { $_ -match [regex]::Escape($script:DistroName) } | Select-Object -First 1
    if ($verLine -match "\b1\b") {
        Write-Warn2 "该发行版当前是 WSL1，正在升级为 WSL2 ..."
        & wsl.exe --set-version $script:DistroName 2
        if ($LASTEXITCODE -ne 0) { Fail "WSL2 升级失败" "可手动执行：wsl --set-version $($script:DistroName) 2" }
    }
} catch { }
& wsl.exe --set-default-version 2 | Out-Null
Write-Ok "已使用 WSL2"

# ── 3. 准备 Linux 包 ───────────────────────────────────────────────────────
Write-Stage "准备 Linux 安装包"

$deb = Get-ChildItem -Path $PayloadDir -Filter "*.deb" -ErrorAction SilentlyContinue | Select-Object -First 1
$tar = Get-ChildItem -Path $PayloadDir -Filter "*.tar.gz" -ErrorAction SilentlyContinue | Select-Object -First 1
if ($deb) {
    $pkg = $deb.FullName
    $pkgKind = "deb"
} elseif ($tar) {
    $pkg = $tar.FullName
    $pkgKind = "tar"
} else {
    Fail "在 $PayloadDir 下既没找到 .deb 也没找到 .tar.gz" "安装器应把 Linux 包解到该目录。"
}
Write-Ok "使用 $(Split-Path $pkg -Leaf)（$pkgKind）"

# ── 4. 把包送进 WSL ────────────────────────────────────────────────────────
Write-Stage "把安装包送入 WSL"

# 走 /mnt/c 而不是 \\wsl$：前者是 WSL 稳定的对外通道，且不需要额外的 UNC 权限；
# 后者在某些 Windows 版本上对 Program Files 路径有重定向限制。
$winPath = (Resolve-Path $pkg).Path
$mntPath = "/mnt/" + $winPath.Substring(0, 1).ToLower() + ($winPath.Substring(2) -replace '\\', '/')
Write-Host "  源：$mntPath"

& wsl.exe -d $script:DistroName -u root -- bash -lc "mkdir -p /tmp/hologramagent-install"
if ($LASTEXITCODE -ne 0) { Fail "无法在 WSL 内创建临时目录" "确认发行版可正常启动：wsl -d $($script:DistroName)" }
& wsl.exe -d $script:DistroName -u root -- bash -lc "cp '$mntPath' /tmp/hologramagent-install/"
if ($LASTEXITCODE -ne 0) { Fail "复制安装包失败" }
Write-Ok "已送到 /tmp/hologramagent-install/"

# ── 5. 在 WSL 内安装 ───────────────────────────────────────────────────────
Write-Stage "在 WSL 内安装 homed"

# 凭据经环境变量传给 setup.sh（它已支持 HOLOGRAM_API_KEY / WEBUI_USER / WEBUI_PASS）。
# 不传的话就会「界面显示一份、config.db 里另一份」，用户直接登录不上。
$credEnv = ""
if ($ApiKey)    { $credEnv += "export HOLOGRAM_API_KEY='$ApiKey'; " }
if ($WebUIUser) { $credEnv += "export WEBUI_USER='$WebUIUser'; " }
if ($WebUIPass) { $credEnv += "export WEBUI_PASS='$WebUIPass'; " }

# 安装逻辑复用 Linux 侧：deb 走 apt（postinst 会调用 setup.sh 生成凭据与 config.db），
# tar 则解包到你同一套布局再执行同一份 setup.sh。刻意不在这里重写安装步骤——
# 「WSL 里就是普通 linux/amd64」必须保持成立，否则等于又开了第三个平台。
if ($pkgKind -eq "deb") {
    $inWslPkg = "/tmp/hologramagent-install/" + (Split-Path $pkg -Leaf)
    & wsl.exe -d $script:DistroName -u root -- bash -lc @"
set -e
$credEnv
export HOLOGRAM_DATA='$DataDir'
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq '$inWslPkg'
"@
} else {
    $inWslPkg = "/tmp/hologramagent-install/" + (Split-Path $pkg -Leaf)
    & wsl.exe -d $script:DistroName -u root -- bash -lc @"
set -e
$credEnv
mkdir -p /opt/hologramagent /tmp/hologramagent-extract
tar -xzf '$inWslPkg' -C /tmp/hologramagent-extract
cd /tmp/hologramagent-extract
# 与 deb 完全相同的布局：/usr/bin/homed + /usr/lib/hologram/setup.sh。
# 两套安装若落到不同路径，之后的升级/排障就会出现「按文档找不到文件」。
install -m 0755 homed      /usr/bin/homed
install -m 0755 waiter     /usr/bin/waiter
[ -f initconfig ] && install -m 0755 initconfig /usr/bin/initconfig
if [ -f hologramagent.service ]; then
  install -m 0644 hologramagent.service /etc/systemd/system/hologramagent.service
fi
mkdir -p /usr/lib/hologram
if [ -f setup.sh ]; then install -m 0755 setup.sh /usr/lib/hologram/setup.sh; fi
export HOLOGRAM_DATA='$DataDir'
if [ -x /usr/lib/hologram/setup.sh ]; then bash /usr/lib/hologram/setup.sh; fi
"@
}
if ($LASTEXITCODE -ne 0) {
    Fail "WSL 内安装失败（退出码 $LASTEXITCODE）" "可进入 WSL 手动排查：wsl -d $($script:DistroName)"
}
Write-Ok "安装完成"

# ── 6. 启动与自启 ──────────────────────────────────────────────────────────
Write-Stage "启动 homed 与自启配置"

& wsl.exe -d $script:DistroName -u root -- bash -lc @"
if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files 2>/dev/null | grep -q hologramagent; then
    systemctl enable hologramagent 2>/dev/null || true
    systemctl restart hologramagent
    echo '  ✓ systemd 服务 hologramagent 已启动并设为自启'
else
    # 没有 systemd（WSL2 默认可能没开）：用 nohup 起，并把自启交给 Windows 侧的计划任务。
    pkill -f '/usr/bin/homed' 2>/dev/null || true
    nohup /usr/bin/homed -data '$DataDir' > /var/log/hologram-boot.log 2>&1 &
    echo '  ✓ 已用 nohup 启动（未检测到 systemd）'
fi
"@

$creds = & wsl.exe -d $script:DistroName -u root -- bash -lc "cat '$DataDir/credentials.txt' 2>/dev/null || true"

Write-Host ""
Write-Host "============================================================" -ForegroundColor Green
Write-Host " HologramAgent 已在 WSL2（$($script:DistroName)）内安装完成" -ForegroundColor Green
Write-Host "============================================================" -ForegroundColor Green
Write-Host ""
Write-Host "  WebUI：http://localhost:8080" -ForegroundColor White
Write-Host "  （WSL2 会把 WSL 内的端口映射到 Windows 的 localhost，无需额外配置）"
Write-Host ""
if ($creds) {
    Write-Host "  初始凭据（也保存在 WSL 内 $DataDir/credentials.txt）：" -ForegroundColor Yellow
    Write-Host $creds
} else {
    Write-Host "  未读到凭据文件，请进入 WSL 检查：cat $DataDir/credentials.txt" -ForegroundColor Yellow
}
Write-Host ""
Write-Host "  常用操作（在 PowerShell 中）："
Write-Host "      进入 WSL        : wsl -d $($script:DistroName)"
Write-Host "      查看日志        : wsl -d $($script:DistroName) -u root -- journalctl -u hologramagent -f"
Write-Host "      重启服务        : wsl -d $($script:DistroName) -u root -- systemctl restart hologramagent"
Write-Host ""
Write-Host "  注意：WSL 实例不会随 Windows 启动而自动拉起。若需要开机自启，"
Write-Host "        可创建一个登录时触发的计划任务执行："
Write-Host "            wsl -d $($script:DistroName) -u root -- systemctl start hologramagent"
exit 0
