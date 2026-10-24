ID:10 / KERNEL-REGISTRY-SETLOCK 收口补丁（CI 侧）
================================================

## 这个文件是什么

ID:10 的收口要求里，有三条是**落在 CI 线（ci/boot-graph-tests）**上的，
不在本分支（fix/registry-setlock，基于 f408448）的改动范围内：

  1. 删掉 ci.yml race 门排除表里 `internal/kernelboot/modules` 那一行
  2. 删掉 evidence 步骤 ② 的内核 setter 静态断言
  3. 更新那段"整包永久排除不被接受"的警告框为收口记录

本分支**不许碰 ci.yml**（任务边界），所以这三步没有在本分支落地，
而是做成下面这份可评审的补丁，交给 CI 线合入。

## 补丁内容（已在本机实测通过，见下）

patch: id10_ci_closeout.patch（相对路径 a/.github/workflows/ci.yml，
可直接 `git apply`；对 ci/boot-graph-tests@efe0cf1 的 ci.yml）

改动只有三处，全部是"删排除 + 记录收口"，无新增门禁逻辑：

  ① 排除表：删掉 `pkg|internal/kernelboot/modules|…|KERNEL-REGISTRY-SETLOCK` 整行
      ⇒ 该包重新纳入 -race（这正是本次修复的目的：它现在跑 -race 是绿的）

  ② evidence 步骤：删掉 ② 整段（查 7 个 Set* 是否仍"单行无锁直写"的静态断言）
      ⇒ 它存在的唯一目的是"修好后提醒去删排除项"，排除项已删，它已完成使命
      ⇒ 收尾提示语从"两个排除项…仍在"改为"webui 排除项…仍在"

  ③ 警告框：把"唯一出口是给 Set* 加锁，修好后做三步"改成"已收口"记录，
     并写清"去重不能替代加锁"的实测结论

## 为什么删完排除项后门禁仍然有效（实跑验证）

排除表删到只剩 webui 一项后：
  · `pkgs=` 为空 —— 剔除循环 `for e in $EXCL` 不执行，无断言误报
  · `tests=TestOpenAIStreamIsActuallyStreaming` —— test 级排除照常生效
  · `count=1` —— 表还在，机制没被架空

实测（本机 bash 跑该步骤原文）：

    ════════ -race 排除表 ════════
      [1] test 级: TestOpenAIStreamIsActuallyStreaming
          包: internal/plugins/webui
          原因: gzipResponseWriter 的 mode/wroteHeader/buf 无锁；…
          修复单: WEBUI-GZIP-RACE
    ════════ 共排除 1 项（不是静默跳过：每次 CI 都打印本表）════════
    EXIT=0
    --- GITHUB_OUTPUT ---
    tests=TestOpenAIStreamIsActuallyStreaming
    pkgs=
    count=1

## 为什么这个补丁不是"拍脑袋认为 CI 会绿"

按 ID:10 收口要求第 2 条「**复跑一次** -race 确认整机判据在 -race 下也稳定」，
在**修复后**的代码上重复跑了 25 次整机判据（未删排除项条件下的同一条命令）：

    go test -race ./internal/kernelboot/modules/ -count=1 -timeout 10m

    修复后：25/25 次 0 条 DATA RACE，全部 ok（本线 f408448 + 修复补丁实测）
    修复前（同一棵树、同一命令）：2/25 次报红，累计 14 条 DATA RACE

修复前那次的原始栈（证明命中的正是本单缺陷，不是别的竞争）：

    WARNING: DATA RACE
    … internal/kernelmod/agent.wirePluginSDK()
          internal/kernelmod/agent/mod.go:251 (SetLuaVM)
          internal/kernelmod/agent/mod.go:252 (SetBaseAPIKey)
          … :253 :254 :255 :256 :257
    … internal/kernelmod/pluginhost.injectBaseDeps()
          internal/kernelmod/pluginhost/mod.go:289 (SetLuaVM)
          … :290 :292 :294 :295 :296 :299
    … internal/plugin/registry.go:231 (SetLuaVM 无锁直写)
    … internal/plugin/registry.go:232 (SetBaseAPIKey 无锁直写)
    … internal/plugin/registry.go:271 / :272

即：agent 与 plugin-host 两个 goroutine 在第 4 层并行写同一批字段，
与 ID:10 描述完全一致。

## 另一个边界：去重不能替代加锁（实测）

本次同时做了"去掉重复注入"，但**它替代不了加锁**。做了对照实验：
回退 registry.go 到未加锁版、只保留 agent 侧去重（即 injectBaseDeps 不动），
然后人为制造"去重后仍然存在的写-读并发"：

    写侧：agent 侧去重后**保留**的 SetStatusProvider
    读侧：pluginhost 侧加载插件时 buildSDK 读 r.status

结果：**1 条 DATA RACE**（SetStatusProvider vs buildSDK），即去重后竞争仍在。
所以 registry 侧的锁才是修复本体，去重只是消除了 7 项无意义的重复写。

## 应用方式

在 ci/boot-graph-tests 分支上：

    git checkout ci/boot-graph-tests
    git apply id10_ci_closeout.patch
    python3 .github/scripts/check-ci-gates.py    # 应通过
    git commit -m "ci: ID:10 收口 —— 删掉 kernelboot/modules 的 -race 排除项"

## 验收清单（全部已在本机实测）

  [x] go build ./...                            0
  [x] go vet ./...                              0
  [x] gofmt（改动文件）                          干净
  [x] check-ci-gates.py                         通过（EXIT=0）
  [x] 排除表步骤实跑                             EXIT=0，pkgs 为空、tests 有值
  [x] -race 整机判据 25 次                       0 条 DATA RACE
  [x] -race 内核三包全量（kernelboot/plugin/kernelmod）  全 ok，0 条 DATA RACE
  [x] 门禁脚本仍能咬住（变异 A：缺原因 ⇒ EXIT=1）  ✔
  [x] 门禁脚本仍能咬住（变异 B：判空改 false + 留文案 ⇒ EXIT=1）  ✔
