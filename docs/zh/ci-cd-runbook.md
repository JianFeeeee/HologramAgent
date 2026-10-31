# CI/CD 流水线手册（GitHub Actions）

> **状态：操作手册（现行）。** 照着做即可；命令与配置如有变更需同步更新本文。

> 2026-09-29 随仓库迁移 gitcode → GitHub 而建。此前 AtomGit 停止对普通用户
> 提供流水线，本仓无任何自动化验证。本文记录两条流水线的用法、机制与边界。
> 分支模型见 `docs/git-branching.md`（本文与其 §七「发版产物清单」衔接）。

## 0. 仓库与流水线总览

| 仓 | 位置 | 流水线 | 触发 |
| --- | --- | --- | --- |
| 主仓 HologramAgent | `github.com/JianFeeeee/HologramAgent`（公开） | `ci.yml` + `release.yml` | push / PR、`release/**` push |
| SDK 仓 hologramagentsdk | `github.com/JianFeeeee/hologramagentsdk`（公开） | `release.yml` | `release/**` push |
| gitcode 镜像 | `gitcode.com/JianFeeeee/HologramAgent`（与主仓同名） | 无（由主仓 Release 的 sync job 同步） | — |

**分支保护**（main，公开仓免费）：10 项必须检查（Go build/vet/test、Race、
cross×5、GUI、C gates、Docs）+ `strict`（必须与 main 同步）+ 禁 force push。
⇒ **feature 合入 main 前必须过流水线**（PR 的 checks 全绿才能 merge）。

**gitcode 的角色**：只读镜像（git push 同步）。Release 附件由 sync job 自动
补传，或手工跑 `deploy/scripts/upload_assets.py`（见 §3.4）。

**gitcode 仓名的单点化**（2026-10-21 加）：gitcode 镜像目标仓名曾在多处写死，
改名时要遍改且易漏（漏一处就是一个小死链）。现在收继到**每文件一个定义处**，
消费点全部走变量：

| 定义处 | 形式 | 消费点 |
| --- | --- | --- |
| `.github/workflows/release.yml` | `env.GC_REPO`（job 级） | `${GC_REPO}` ×4 |
| `.github/workflows/ci.yml` | `env.GC_REPO`（job 级） | `${GC_REPO}` ×2 |
| `deploy/scripts/upload_assets.py` | `GITCODE_REPO` 环境变量默认值 | 脚本自身 |

两个 workflow 是独立 workflow，因此各自一个定义（不能跨文件共享 env）。
**gitcode 侧以后再改仓名，只改这三个定义点即可**，消费点无需动。

**当前取值 `JianFeeeee/HologramAgent`**（与 GitHub 主仓同名）。gitcode 侧镜像仓
**已由 `HologramAgent` 改名为 `HologramAgent`**；旧地址仍 301 跳转，跟随后也能到，
但走重定向，不应再用。实测（2026-10-21 20:1x，直连不带代理，
`curl -s -o /dev/null -w '%{http_code}' https://gitcode.com/JianFeeeee/<repo>`）：

| 仓名 | HTTP | 说明 |
| --- | --- | --- |
| `HologramAgent` | **200** | 当前取值（主仓镜像） |
| `HologramAgent` | 301 | 旧名，跳转到 `HologramAgent` |
| `homeagent-sdk` | 200 | SDK 仓，另一对象 |
| `hologramagentsdk` | 200 | SDK 仓，另一对象 |
| `homeagentsdk` | 404 | 不存在 |
| `HologramAgent-sdk` | 404 | 不存在 |

判定「301 不是另一个仓、就是同一仓改名」的依据：两个地址 `git ls-remote`
得到的 refs 列表 md5 相同（同一组分支与 SHA）。

API 侧同样兼容两个名字：`/api/v5/repos/JianFeeeee/{HologramAgent,HologramAgent}`
不带 token 都返回 **400**（需要鉴权，不是仓不存在），
`/releases/tags/v1.3.13` 都返回 **200** 且响应体一致。

**失败面（两个 job 不一样，别搞混）**：

- `release.yml` 的 `sync-gitcode`：`needs: [prepare, publish]`。前置检查 `exit 1`
  ⇒ **只红镜像同步这一格**；GitHub 侧 release 产物已经发出去了。
- `ci.yml` 的 `sync-gitcode-main`：`needs` 全绿（go/race/cross/gui/csrc/docs）后才跑。
  前置检查 `exit 1` ⇒ 该 job 红 ⇒ **main 的整条 CI 红**。

所以仓名写错的实际后果是「每次 release/main CI 必红一格 + 国内镜像不更新」，
**不是**「发不出包」。

## 1. CI（ci.yml）—— 每次推送到 main / release/** / PR 都跑

六个 job，**全部命令均本地实测过**（原则：不写"应该有用"的未验证步骤）：

| job | 内容 | 时长 |
| --- | --- | --- |
| Go build / vet / test | `go build/vet/test ./...` + `make check-client-versions` | ~12m |
| Race detector | `go test -race`（core + waiter） | ~8m |
| Cross-compile ×5 | waiter/initconfig/mock-server 五平台（CGO=0） | ~5m |
| GUI (node) | `cd cmd/gui && npm test`（纯 Node，零依赖，秒级） | <1m |
| C infrastructure gates | `make check-csrc`（告警/ABI/ASan/跨架构） | ~6m |
| Docs build | mkdocs.yml 可解析性检查 | <1m |

**明确不进 CI**（依赖真机/密钥/内网，跑了只会变 flaky 噪音）：
`deploy-*.sh`、waiter 真机（192.168.2.x）、`npm run test-live`（需真
Electron+Xvfb+真后端）、`scripts/kernel-stress/*`、需 `DEEPSEEK_API_KEY` 的
真实 LLM 测试（自带 t.Skip）。

### 已知边界（不是缺陷）

- **只有 waiter/initconfig/mock-server 能纯交叉编译**。homed/memgc/
  homed-kb-migrate 依赖 cgo（gojieba/onnx），必须原生构建（见 Makefile）。
- **`cmd/gui` 是纯 Electron 目录**（0 个 .go、无 go.mod）。`go test ./...`
  不会包含它（Go 的 `./...` 语义）；只有显式 `go test ./cmd/gui` 才报
  "no Go files"。前端测试走 `npm test`。
- **CGO 必须开**（CI 里 `CGO_ENABLED: 1`）：gojieba 需要。

## 2. 发布（release.yml）—— release/** 推送即发版

### 2.1 标准发版流程（以 1.3.14 为例）

```bash
# 1. 在发布线的 worktree（干净）里 bump 版本
git worktree add /tmp/rel-v1.3.14 release/v1.3.x
cd /tmp/rel-v1.3.14
sed -i 's/Version = "1.3.13"/Version = "1.3.14"/' internal/meta/meta.go
git commit -am "release: 1.3.14"
git push origin release/v1.3.x
# 2. 流水线自动：读版本 → go build/test 门 → 下载资产 → 打包
#    → 打 tag v1.3.14 → 建 GitHub release → 传附件 → 回读校验
#    → （配了 GITCODE_TOKEN 时）同步 gitcode
# 3. 在 GitHub Actions 页看进度；全绿即发版完成
```

**版本号唯一事实源是 `internal/meta/meta.go` 的 Version**（SDK 仓是
`meta/meta.go`）。改它并推送 = 发版指令。

### 2.2 幂等闸门：tag 已存在 ⇒ 整轮跳过

Prepare 先查 `refs/tags/v<version>` 是否存在。已存在（比如改文档的推送、
cherry-pick 维护提交）则 Build/Publish/Sync 全部 skipped。**不会重复发版**。

### 2.3 发版门：go build 硬门 + go test 可显式跳过

- `go build ./...` —— **硬门**，不可跳过。
- `go test ./...` —— 默认跑。历史维护线若存在**既存红测试**（如
  release/v1.3.x 的 deepsearch 测试，main 上已修），在**改动 meta 的那个
  提交**的正文里写 `[skip-release-tests]` 即可跳过：

  ```bash
  git commit -am "release: 1.3.14 [skip-release-tests]

  （正文里写明跳过理由——为什么本线的红是既存的、与本次发版无关）"
  ```

  跳过时 CI 输出 `::warning`，决定记录在发版 commit 里可审计。
  ★ 标记查在**改动 meta 的提交**上而非 HEAD：发版提交后常还会跟几个
  维护提交，只看 HEAD 会让标记被顶掉、静默失效。

### 2.4 产物与验证

产物（v1.3.13 实测）：

```text
homeagent-client_1.3.13_amd64.deb      83.5MB   客户端（不含模型）
homeagent-server_1.3.13_amd64.deb     730.1MB   服务端（含向量模型）
homeagent-full_1.3.13_amd64.deb       808.3MB   全量（模型+ORT+GUI）
homeagent_1.3.13_linux_amd64.tar.gz   839.2MB   内核+CLI+GUI 打包
SHA256SUMS                                       全量校验和
```

流水线内置的验证（不通过即不发版）：

- deb 元数据（Package/Version/Architecture）逐包核对；
- full/server 包内**必须真的含** `TextEncoder.onnx` 与 `libonnxruntime.so`
  （防"默认启用但装完不能用"的假包）；
- `sha256sum -c SHA256SUMS`（产物先平铺再验 —— 脚本把校验和写成平铺名）；
- 发布后**回读**：把附件下载回来再验一遍校验和。

### 2.5 构建资产（ci-assets-v1 release）

server/full 的打包需要 719MB 模型 + 24MB ONNX Runtime。它们**内容不随版本
变**，故作为 `ci-assets-v1` release 的附件一次性托管，每次发版由流水线下载
并 `sha256sum -c` 校验后使用：

```text
chinese-clip-vit-b16-onnx.tar         718.8MB   Chinese-CLIP ViT-B/16 ONNX
onnxruntime-linux-amd64-1.28.0.tar     23.5MB   ORT 1.28.0 + LICENSE + TPN
SHA256SUMS
```

模型或 ORT 升级时：改好本地 `CHINESECLIP_BUNDLE_DIR` /
`ONNXRUNTIME_ASSET_DIR` 指向的目录 → 重新打包 → 在 GitHub 上新建
`ci-assets-v2` release 并上传 → 同步更新 release.yml 里的 `ASSETS_TAG`。

## 3. 运维要点（都踩过坑）

### 3.1 workflow 文件必须存在于目标分支

GitHub 用**被推送 commit 里的** `.github/workflows/*.yml` 决定是否触发。
给旧发布线补流水线时，要把 workflow 文件本身 commit 到那条分支
（实测：只在 main 有时，推 release/** 什么都不触发）。

### 3.2 runner 上没有 electron 缓存

打包脚本从两处找 Electron：`~/.cache/electron` 的 zip，或
`cmd/gui/node_modules/electron/dist`。全新 runner 两处都没有 ⇒ release.yml
在打包前 `npm ci`（electron 已在 package-lock 锁定；**不能**
`npm install --production`，electron 是 devDependency 会被跳过）。

### 3.3 管道里的 `grep -q` 会因 SIGPIPE 误杀检测

`dpkg-deb -c <800M 包> | grep -q <目标>`：grep -q 匹配即退出、关闭读端 ⇒
tar 写 stdout 收到 EPIPE ⇒ pipefail 判失败。**包越大越易触发**
（80M 的 client 没事、800M 的 full 炸）。检测存在性一律
`grep <pat> >/dev/null`。

### 3.4 gh 在非 git 目录要显式 `--repo`

`gh release download` 在 `/tmp/back` 这类非 git 目录里会报
"not a git repository"（gh 从 cwd 的 git 上下文推断仓库）。必须
`gh release download "$TAG" --repo "$GITHUB_REPOSITORY"`。

### 3.5 gitcode 上传的路径语义

`upload_assets.py` 拼路径是 `os.path.join(ASSET_DIR, name)`：
要 `cd` 进资产目录、`ASSET_DIR=.`、传**裸文件名**。SDK 仓的产物多数无
扩展名，必须显式列名（自动扫描按后缀识别，会静默一个都不传）。

### 3.6 gitcode 凭据（`GITCODE_TOKEN`）

CI 的 sync job 需要仓库 secret `GITCODE_TOKEN`；**未配置时该 job 显式跳过**
（不阻断 GitHub 侧发布）。

**2026-09-29 已配置**：两仓均已设同名 secret，取值自 `~/.git-credentials`
里那条 `https://JianFeeeee:<token>@gitcode.com`。配置时 SDK 仓还叫
`homeagentsdk`，2026-10-21 改名 `hologramagentsdk` 后 secret 随仓迁移，
无需重设；下面命令写的是**当前**仓名。
配置方式（经 stdin 传入，避免 token 出现在进程列表）：

```bash
printf '%s' "$TOKEN" | gh secret set GITCODE_TOKEN --repo JianFeeeee/HologramAgent
printf '%s' "$TOKEN" | gh secret set GITCODE_TOKEN --repo JianFeeeee/hologramagentsdk
```

验收：sync job 只在**新版本**发版时运行（`prepare.outputs.exists == 'false'`），
历史 tag 触发不了，所以无法用旧版本实跑。等价验证三道：

```bash
# ① secret 存在
gh secret list --repo JianFeeeee/HologramAgent | grep GITCODE_TOKEN
# ② token 有效
curl -s "https://gitcode.com/api/v5/user?access_token=$TOKEN"
# ③ job 用的 private-token 头可读 release（两仓都测）
#    ⚠ gitcode 侧主仓镜像现已改名为 HologramAgent（与 GitHub 同名），
#      旧名 HologramAgent 仍 301 跳转。实测 2026-10-21 20:1x（直连不带代理）：
#      /api/v5/repos/JianFeeeee/{HologramAgent,HologramAgent} 不带 token 均 400
#      （需鉴权，非仓不存在）；/releases/tags/v1.3.13 两名字均 200 且响应一致。
curl -s -H "private-token: $TOKEN" \
  "https://gitcode.com/api/v5/repos/JianFeeeee/HologramAgent/releases/tags/v1.3.13"
```

★ **token 是宽范围的个人令牌**（可读 92 仓/48 私有、有写权限），而 CI 只需要这两个仓。
更稳的做法是去 gitcode 建一枚**仅限这两仓**的令牌再替换 —— 这样 CI 泄漏时
影响面不扩到其他仓。当前未做（按用户 2026-09-29 的决定）。

手工补发的完整流程（下载 GitHub 产物 → 建 release 条目 → 上传）：

```bash
# token 放 ~/.git-credentials（https://JianFeeeee:<token>@gitcode.com）
# GITCODE_REPO 是 gitcode 镜像仓名（当前 HologramAgent，旧名 HologramAgent 为 301）。
ASSET_DIR=<产物目录> GITCODE_REPO=JianFeeeee/HologramAgent \
  python3 deploy/scripts/upload_assets.py <tag> <token>
# release 条目必须先存在（脚本向 releases/<tag>/upload_url 取 OBS 签名 URL）
```

## 4. SDK 仓（third_party/homeagent-sdk）

与主仓同构，差异：

| | 主仓 | SDK |
| --- | --- | --- |
| 版本源 | `internal/meta/meta.go` | `meta/meta.go` |
| 产物 | 3 deb + 1 tar.gz（2.4GB） | `hmapdev_*` 5 平台 + SHA256SUMS（~140MB） |
| CGO | 必须（gojieba） | 不需要（CGO_ENABLED=0 纯交叉） |
| 测试 | `go test ./...` | **两处**：根模块 + `tools/hmapdev`（独立 module，根的 ./... 不含它） |
| 门 | 同一机制 | 同一机制（`[skip-release-tests]`、幂等闸门） |

发版：改 `meta/meta.go` 的 Version 推 `release/vX.Y.x`。SDK 版本随核心的
中版本走、patch 恒为 `.0`（见 git-branching.md §七.1）。

## 5. 通知

Actions 失败会给仓库 owner 发邮件（GitHub 默认）。若嫌吵：
github.com/settings/notifications → Actions 关闭，或仓库页 Watch → Custom
取消 Actions。注意失败邮件也可能是"验证步骤自身 bug"的假警报 ——
先看是哪个 job/step 红了再判断（对照 §3 的坑）。

## 6. 开发环境的诊断噪音（`cmd/gui [setup failed]`）

### 6.1 症状

每轮改完文件，pi-lens 的回合末摘要里会冒一条：

```text
FAIL ./cmd/gui [setup failed]
```

它看着像仓库里有测试红了，**实际是工具缺陷**。真实状态：

```bash
go test ./...          # 退出码 0，43 个包全过、0 FAIL
find cmd/gui -name '*.go' | wc -l   # 0 —— 该目录根本没有 Go 代码
go test ./cmd/gui      # “no Go files in .../cmd/gui”
cd cmd/gui && npm test # 这才是它的测试（node 的 .test.mjs），通过
```

### 6.2 根因（pi-lens 的两个缺陷叠加）

1. **runner 按仓库根选**，不按被跑的文件选。本仓根有 `go.mod` ⇒ 选中 go runner；
   而 `cmd/gui/*.test.mjs` 命中通用测试命名（`detectFileRole` 与 runner 无关）
   ⇒ 对 `cmd/gui` 生成 `go test -run . ./cmd/gui` ⇒ 必失败。
2. **failed-first 把误报变成永久**：失败项进 `failedTestsByRunner`（进程内 Map），
   此后**每次**编辑都优先重跑它（与当前编辑的文件无关）；而该条目只在测试
   **通过**时才移除 ⇒ 对这条永远失败的命令，永不自愈。

日志里的形态（`${PI_HOME}-lens/sessionstart.log`）：

```text
turn_end: README.md → test go cmd/gui/sse-backoff.test.mjs (failed-first)
```

注意触发者是 `README.md` —— 目标是**与本次编辑无关**的陈旧失败项。

### 6.3 为什么仓库里没有 `.pi-lens.json`

pi-lens 的 `ignore` 是**本地工具配置**，随每个人的工具版本与偏好变化，
不该进仓库（它对 CI 与其他贡献者毫无意义，还会成为 merge 噪音）。
需要的配置由各人在自己的机器上放一份即可。

★ 而**根因分析要留在仓库里** —— 就是本节 6.1/6.2：
上游缺陷（runner 不按目录语言选、failed-first 让误报永不自愈）
不知道哪年修，修之前每个新 clone 的人都会再踩一遍。
所以是「配置外置、结论内留」。

### 6.4 为什么不能用项目级配置关掉

`.pi-lens.json` 是**项目级**，只认一小排键
（`ignore` / `rules` / `maxProjectFiles` / `reviewGraph` / `trivy` + 三个改动开关）。
`tests` 是**全局级**键，写进项目文件会被忽略并告警：

```text
"tests" is a global-only pi-lens setting and is not honored in a project .pi-lens.json
```

而全局关掉（`~/.pi-lens/config.json` 的 `{"tests":{"enabled":false}}`）
会一起关掉**所有项目**的回合末测试反馈 —— 为一个仓库的误报付全局代价，不值。
另：`ignore` 也挡不住，因为它只作用于扫描，不参与测试目标选择（`failed-first`
的回退分支根本不看候选文件）。

### 6.5 修法：本机补丁（已打）

补丁位置：`~/.pi/agent/npm/node_modules/pi-lens/dist/index.js`。
在 `getTestRunTarget` 返回目标前加一道校验：

> 该 runner 是否**真能跑**这个目标？只有 go 做实质检查 ——
> 目标所在目录要有至少一个 `.go` 文件。不能跑就返回 null，
> 并顺手把这条不可运行的记录从 `failed-first` 集合里移除。

原方法体改名为 `selectTestRunTargetRaw`，外面套一层校验（`runTestFileAsync`
只有这一个调用点，所以这里是唯一收口）。补丁全文已用 `node --check` 验语法，
用 `/usr/bin/diff` 核对为**纯新增、零删除**。

**立即生效（不必重启会话）**：在会话里执行内置命令 **`/reload`**
（重载扩展且不重启 `pi-web-sessiond`）；不手动重载则在**下个会话**自然生效。

**验证**：改一个仓库文件但先不提交，等回合结束，然后
`grep 'turn_end: .*→ test' ${PI_HOME}-lens/sessionstart.log | tail -3`
—— 应不再出现 `test go cmd/gui/...`；而编辑一个真 Go 测试文件时仍应正常触发。

**会被覆盖**：pi-lens 升级/重装后补丁消失，误报会回来（不影响仓库，只是噪音）。
备份在同目录 `index.js.orig-*`，回退就是拷回去：

```bash
cd ~/.pi/agent/npm/node_modules/pi-lens/dist
cp -a index.js.orig-<时间戳> index.js    # 然后 /reload
```

> 上游缺陷：runner 选择应先确认目标文件属于该语言（或至少确认目录内有该语言的源文件）。
