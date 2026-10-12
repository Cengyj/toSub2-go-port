# 纯 Go 认证宿主的生产程序验证

> 历史验证：本文的源码摘要、二进制摘要与十场景运行结果属于清理外部运行时之前的 Go 宿主版本，用于保留生产入口验证证据。当前已删除 Node/浏览器自动化分支，当前工作树和本轮部署验收以 local-deployment.md 为准。

记录日期：2026-10-12。工作树包含前序未提交修改；没有重置项目。Go 1.27.1，macOS arm64，Docker Engine 29.8.0 / Linux arm64。

本文保留首轮五场景的历史构建快照，并记录运行时源码冻结后的最终十场景验证。首轮之后的 Worker、ReadableStream、Storage、结构化消息、事件循环及固定诊断修改均进入最终重编译；最终结果见下一节。

本次使用本地合成 TLS 账号服务、合成 SDK 和合成账号输入，没有发起真实账号请求。测试证明生产程序的 Go worker 启动、通信和登录恢复链路，不代表上游每次更新的 Sentinel / Cloudflare 脚本都已兼容。

## 冻结源码后的最终验证

两个程序重新以 `CGO_ENABLED=0 GOOS=linux GOARCH=arm64` 编译，`file` 确认都是静态链接的 ELF Linux / aarch64；镜像仍使用下文所示的 `FROM scratch`，只包含 `/tosub2` 和 `/fixture-driver`。运行时没有 Node、Python、Chrome、shell、动态库或系统 CA，容器使用 `--network none` 和独立 `/tmp` tmpfs。

结果：**十个实际生产 CLI 子场景全部 PASS，0 FAIL / 0 SKIP，整体 2.75 s**。其中八个需要 SDK 的场景都通过 `/proc` 观察到 worker 来自同一个 `/tosub2` 可执行文件；已认证 checkpoint 和验证码已接受的 `web_continue` checkpoint 两个场景不启动 worker、不再次登录或消费验证码。所有场景只开始一次认证 session，OAuth code 只交换一次，私有 worker 输入文件均删除。

| 最终检查 | 结果 | 耗时 |
| --- | --- | --- |
| 静态生产程序的 scratch 十场景 CLI | PASS；10 个子场景实际执行 | 2.75 s |
| 同一冻结源码的 macOS Go runner 十场景，普通 | PASS | 11.121 s |
| 同一冻结源码的 macOS Go runner 十场景，race | PASS | 12.006 s |

后两组与项目全量验证同时运行，耗时包含本机并行构建和测试的资源竞争。

构建前、容器验证后，下面的源码集合摘要相同。集合包含 `rg --files` 可见的 Go、JS、CJS、JSX、CSS、HTML 文件及 go.mod / go.sum，不包含文档、私有账号数据或临时构建文件；这是选定源码内容与文件名的摘要，不是一个 Git commit：

```sh
rg --files -0 -g '*.go' -g '*.js' -g '*.cjs' -g '*.jsx' \
  -g '*.css' -g '*.html' -g go.mod -g go.sum \
  | LC_ALL=C sort -z | xargs -0 shasum -a 256 | shasum -a 256
```

```text
source-set sha256=67998d5a674956324d84f7bea81a1fa86ee538bfdbb4dd6caae840ba508b0a00
production sha256=5cc907cf06664d46419739a3593ce3f77f7e7c8bb75df8cae89affb7f37c09f0
fixture-driver sha256=bb4070ffa7e95d635323048b5768a367f11303248403f7f5aebcb881344869d6
scratch manifest sha256=d39b82b840cc49568a9d0ce7326118dbb1db839d70d1810621d48f3db17c4bbb
```

最终容器标准输出：

```text
=== RUN   TestLegacyRuntimeProductionLoginAndResume
=== RUN   TestLegacyRuntimeProductionLoginAndResume/new-password-login
    legacy_runtime_integration_test.go:315: production CLI passed: same_executable_worker=true external_worker=false private_inputs_removed=true
=== RUN   TestLegacyRuntimeProductionLoginAndResume/legacy-password-checkpoint
    legacy_runtime_integration_test.go:315: production CLI passed: same_executable_worker=true external_worker=false private_inputs_removed=true
=== RUN   TestLegacyRuntimeProductionLoginAndResume/legacy-authenticated-checkpoint
    legacy_runtime_integration_test.go:315: production CLI passed: same_executable_worker=false external_worker=false private_inputs_removed=true
=== RUN   TestLegacyRuntimeProductionLoginAndResume/new-password-mfa-rejection
    legacy_runtime_integration_test.go:315: production CLI passed: same_executable_worker=true external_worker=false private_inputs_removed=true
=== RUN   TestLegacyRuntimeProductionLoginAndResume/legacy-mfa-checkpoint
    legacy_runtime_integration_test.go:315: production CLI passed: same_executable_worker=true external_worker=false private_inputs_removed=true
=== RUN   TestLegacyRuntimeProductionLoginAndResume/new-email-login
    legacy_runtime_integration_test.go:315: production CLI passed: same_executable_worker=true external_worker=false private_inputs_removed=true
=== RUN   TestLegacyRuntimeProductionLoginAndResume/new-email-rejection-resend
    legacy_runtime_integration_test.go:315: production CLI passed: same_executable_worker=true external_worker=false private_inputs_removed=true
=== RUN   TestLegacyRuntimeProductionLoginAndResume/legacy-email-checkpoint
    legacy_runtime_integration_test.go:315: production CLI passed: same_executable_worker=true external_worker=false private_inputs_removed=true
=== RUN   TestLegacyRuntimeProductionLoginAndResume/legacy-email-checkpoint-rejection
    legacy_runtime_integration_test.go:315: production CLI passed: same_executable_worker=true external_worker=false private_inputs_removed=true
=== RUN   TestLegacyRuntimeProductionLoginAndResume/legacy-email-accepted-checkpoint
    legacy_runtime_integration_test.go:315: production CLI passed: same_executable_worker=false external_worker=false private_inputs_removed=true
--- PASS: TestLegacyRuntimeProductionLoginAndResume (2.75s)
    --- PASS: TestLegacyRuntimeProductionLoginAndResume/new-password-login (0.04s)
    --- PASS: TestLegacyRuntimeProductionLoginAndResume/legacy-password-checkpoint (0.03s)
    --- PASS: TestLegacyRuntimeProductionLoginAndResume/legacy-authenticated-checkpoint (0.01s)
    --- PASS: TestLegacyRuntimeProductionLoginAndResume/new-password-mfa-rejection (0.34s)
    --- PASS: TestLegacyRuntimeProductionLoginAndResume/legacy-mfa-checkpoint (0.31s)
    --- PASS: TestLegacyRuntimeProductionLoginAndResume/new-email-login (0.29s)
    --- PASS: TestLegacyRuntimeProductionLoginAndResume/new-email-rejection-resend (0.84s)
    --- PASS: TestLegacyRuntimeProductionLoginAndResume/legacy-email-checkpoint (0.30s)
    --- PASS: TestLegacyRuntimeProductionLoginAndResume/legacy-email-checkpoint-rejection (0.59s)
    --- PASS: TestLegacyRuntimeProductionLoginAndResume/legacy-email-accepted-checkpoint (0.01s)
PASS
```

本地合成服务已覆盖登录、OTP 纠正、checkpoint 恢复、动态 proof、Cookie / HTTP 身份与输出清理；没有使用真实账号，没有修改本地部署。真实服务尚未验证，因此 `config.Default`、CLI 帮助及启动脚本均保留默认 `node`；纯 Go 通过 `--auth-transport=legacy --legacy-js-runtime=go --browser=false` 或 `TOSUB2_LEGACY_JS_RUNTIME=go` 显式启用，不自动回退外部运行时。内存与时间限制的实际边界见 [worker-boundaries.md](engine-probe/worker-boundaries.md)。

## 初始脚本宿主与会话回归

| 检查 | 结果 | 耗时 |
| --- | --- | --- |
| `TestGo.*PureGo` | PASS；共享 Cookie / HTTP 身份、二进制请求、Cloudflare 父子宿主、异步 Sentinel iframe 的两种调用顺序 | 1.322 s |
| `TestGoWorkerBridge` / `TestRuntimeRequest` / `TestRuntimeCookie` race | PASS | 1.877 s |
| 生产 runner 的 Go 宿主登录 / 恢复 race | PASS；五个场景 | 5.136 s |
| 同一生产 runner 的显式 Node oracle 登录 / 恢复 race | PASS；五个场景 | 5.672 s |

Go 模式的 fixture 传入不存在的 Node 和 CJS 路径，验证不会在执行失败时自动切换到外部脚本宿主。Node 用于行为对照时必须显式选择。

## 不含外部运行时的生产 CLI

两个程序以 `CGO_ENABLED=0 GOOS=linux GOARCH=arm64` 单独编译：

- `go build -o <private-build-dir>/tosub2 ./cmd/tosub2` 是生产可执行文件，实际执行 `main`、公开 CLI 参数解析和认证 runner。
- `go test -c -o <private-build-dir>/fixture-driver ./cmd/tosub2` 只负责启动合成 TLS 服务、创建合成 checkpoint、调用生产程序并检查结果。

`file` 确认两个文件均为静态链接的 Linux / aarch64 ELF。镜像内容：

```dockerfile
FROM scratch
COPY tosub2 /tosub2
COPY fixture-driver /fixture-driver
```

镜像没有 Node、Python、Chrome、shell、动态库或系统 CA。运行命令：

```sh
docker run --rm --network none --tmpfs /tmp:mode=1777 \
  -e TOSUB2_LEGACY_RUNTIME_TEST=1 \
  -e TOSUB2_LEGACY_JS_RUNTIME=go \
  -e TOSUB2_PRODUCTION_FIXTURE_BINARY=/tosub2 \
  <owned-scratch-image> /fixture-driver \
  '-test.run=^TestLegacyRuntimeProductionLoginAndResume$' \
  -test.v -test.timeout=240s
```

`--network none` 保留容器内回环接口，禁止访问容器外账号服务。fixture-driver 生成自己的临时 CA；生产子进程通过 `SSL_CERT_FILE` 验证 TLS，没有关闭证书验证。生产进程的 `PATH` 指向不存在的目录，且显式传入不存在的 `--legacy-node` / `--legacy-runtime-dir`。

实际结果：整体 PASS，0.67 s，五个子场景均通过。

首轮构建身份：

```text
production sha256=c2443fbf826b354336fa8b742ef1ca0c6a6e3a707b412ae2def40427378c6ff7
fixture-driver sha256=0fd4220fc60f17bbc9caded98fba3aa33a2e8d2b248a9aa7e8953edc43dbeeb9
scratch manifest sha256=10543939815956ab2e31882ef1978e1da2ec869dd6967b5bc4cdcce95fad690e
```

容器标准输出的原始测试结果（无账号或运行时异常文本）：

```text
=== RUN   TestLegacyRuntimeProductionLoginAndResume
=== RUN   TestLegacyRuntimeProductionLoginAndResume/new-password-login
    legacy_runtime_integration_test.go:250: production CLI passed: same_executable_worker=true external_worker=false private_inputs_removed=true
=== RUN   TestLegacyRuntimeProductionLoginAndResume/legacy-password-checkpoint
    legacy_runtime_integration_test.go:250: production CLI passed: same_executable_worker=true external_worker=false private_inputs_removed=true
=== RUN   TestLegacyRuntimeProductionLoginAndResume/legacy-authenticated-checkpoint
    legacy_runtime_integration_test.go:250: production CLI passed: same_executable_worker=false external_worker=false private_inputs_removed=true
=== RUN   TestLegacyRuntimeProductionLoginAndResume/new-password-mfa-rejection
    legacy_runtime_integration_test.go:250: production CLI passed: same_executable_worker=true external_worker=false private_inputs_removed=true
=== RUN   TestLegacyRuntimeProductionLoginAndResume/legacy-mfa-checkpoint
    legacy_runtime_integration_test.go:250: production CLI passed: same_executable_worker=true external_worker=false private_inputs_removed=true
--- PASS: TestLegacyRuntimeProductionLoginAndResume (0.67s)
    --- PASS: TestLegacyRuntimeProductionLoginAndResume/new-password-login (0.03s)
    --- PASS: TestLegacyRuntimeProductionLoginAndResume/legacy-password-checkpoint (0.02s)
    --- PASS: TestLegacyRuntimeProductionLoginAndResume/legacy-authenticated-checkpoint (0.01s)
    --- PASS: TestLegacyRuntimeProductionLoginAndResume/new-password-mfa-rejection (0.32s)
    --- PASS: TestLegacyRuntimeProductionLoginAndResume/legacy-mfa-checkpoint (0.30s)
PASS
```

| 场景 | 同一生产程序的 Go worker | 验证结果 |
| --- | --- | --- |
| 新密码登录 | 是 | 动态 SDK proof 与 observer、会话 Cookie、设备和 UA 到达密码验证；OAuth 交换一次 |
| 旧密码 checkpoint 恢复 | 是 | 保留认证身份和 Cookie；动态 SDK 执行一次；OAuth 交换一次 |
| 已认证 checkpoint 恢复 | 无需启动 | 不再次登录、不加载 SDK；直接完成 OAuth，交换一次 |
| 新密码登录后 MFA 错码纠正 | 是 | 合成 `403 incorrect_code` 后读取手动验证码；同一会话、Cookie 和身份继续；密码不重放 |
| MFA checkpoint 错码纠正 | 是 | 保留选定 TOTP 因子和 checkpoint 绑定；同一会话继续；不重放密码 |

fixture-driver 在 Linux `/proc` 中观察生产主进程的直接子进程：所有需要 SDK 的场景都观察到 `/proc/<pid>/exe` 为 `/tosub2` 且参数为隐藏的 `--auth-js-worker`；未观察到其他程序。每个场景只发生一个 `legacy_session_start`，认证成功导出合成 OAuth 结果，退出后临时目录没有遗留 `tosub2-js-*` / `tosub2-node-*` 输入文件。

首轮和最终容器均使用 `--rm` 自动删除；两个 owned 镜像及本次私有构建目录最终单独清理。没有运行全局 Docker prune，没有修改本地部署、用户账号数据、Keychain 或 sibling browser 实例。

## 扩展夹具的阶段结果

生产 runner 已扩展到十种场景，新增五种邮箱 OTP 路径：无密码的邮箱验证码登录、`403 incorrect_code` 后显式重发与纠正、待验证邮箱 checkpoint 成功恢复、待验证邮箱 checkpoint 错码纠正、验证码已接受的 `web_continue` checkpoint 继续登录。最后一种不得重复消费已接受的 OTP 或重新加载 SDK；所有 checkpoint 恢复不得重做 signin，所有场景只能交换一个 OAuth code。

| 十场景生产 runner | 结果 | 耗时 |
| --- | --- | --- |
| Go 宿主，普通 | PASS | 3.842 s |
| Go 宿主，race | PASS | 7.622 s |
| 显式 Node oracle，普通 | PASS | 6.471 s |
| 显式 Node oracle，race | PASS | 8.838 s |

`internal/legacyruntime/go_host_lifecycle_test.go` 的实际 Sentinel / Cloudflare parent-worker-HTTP pipeline 也已运行：

- Sentinel：HTTP 等待、CPU 死循环、无限 Promise 微任务链，分别经过 caller cancel、owner cancel、Close、solver timeout，十二个子场景。取消或超时保留原始 context 错误；私有输入被删除。仅调用者取消 / 超时后，同一个客户端及 CookieJar 可再次获取有效 token。
- Cloudflare：等待实际 child HTTP 请求时经过上述四种停止方式，取消时同时拥有两个 parent / child 私有输入；两个 worker 都被回收，不错误签发 clearance。仅调用者取消 / 超时后，同一客户端和 CookieJar 可验证一个新的 challenge。
- 并发隔离：八个客户端同时执行动态 SDK，分别使用不同 device、flow、JS global、CookieJar 和服务端响应 cookie。Go 还必须将 `document.cookie` 写回自己的父进程 CookieJar；旧 Node oracle 的 jsdom document writes 留在 jsdom，因此不把该旧行为视为 Go 的验收目标。

| 生命周期与并发 Cookie / global 隔离 | 结果 | 耗时 |
| --- | --- | --- |
| Go Sentinel，普通 | PASS | 3.709 s |
| Go Cloudflare，普通 | PASS | 2.847 s |
| Go Sentinel + Cloudflare，race | PASS | 8.217 s |
| Node Sentinel，普通，含 origin storage 隔离 | PASS | 6.019 s |
| Node Cloudflare，普通 | PASS | 4.127 s |
| Node Sentinel + Cloudflare，race，含 origin storage 隔离 | PASS | 10.722 s |

以上是本地合成服务的真实脚本子进程链路，不是仅调用引擎 `Run` 的单位测试。它们未替代文首十场景独立生产 CLI 的 scratch 验收。新增 `TestGoSentinelPureGoConcurrentOriginStorageIsolation` 曾暴露 Go host 缺少 localStorage / sessionStorage；功能补齐并接入 realm 后该八会话真实 worker 夹具已通过，0.774 s。默认仍为 `node`，原因是尚未使用真实账号验证上游服务。

结构化消息的额外结果：

- `TestGoCloudflarePureGoWindowTransferRoundTrip` 通过，普通 0.537 s / race 1.773 s。Cloudflare 父、子两个真实 Go worker 往返传递 Uint8Array / ArrayBuffer，保留 Map 中的共享引用及自循环；两端转移后原缓冲区都被 detach，消息来源、origin 和 isTrusted 正确，完成及 clearance 复验各一次。
- `TestGoWorkerBridgeKeepsQueuedAndLiveWindowGraphsOpaque` race 通过，1.552 s。独立协议 fixture 在 frame 出现前先发消息，验证 graph 在 child 初始队列和 child → parent 实时桥接都保留；graph 故意使用父 HTTP 层不识别的版本和结构，父进程只透传，不解析或改写对象语义。
- 全部 Go `TestGo.*PureGo`、桥接、请求、Cookie 夹具 race 整组通过，9.087 s。该整组在新增 opaque 初始队列 fixture 前启动；后者另行执行了上述 race 检查。

诊断桥接的后续修订也已验证：父进程只接受 `jshost.IsDiagnosticCode` 的闭集，以及旧 oracle 的固定 `EXECUTION_FAILED`。未知全大写字符串仍拒绝为协议错误；脚本异常文本、exec 错误和 stderr 不显示给用户。显式 Go worker 的退出状态 73 映射到固定 `JS_RESOURCE_LIMIT_MEMORY`，普通退出只返回泛化错误；调用者取消保留 context 原因，清理错误单独保留。

`TestGoWorkerBridgeFixedExitPreservesOnlyKnownResourceDiagnosis` 在 Sentinel 和 Cloudflare 两种完整父桥接流程中分别验证状态 73 / 74 与私有 stderr，普通通过 0.679 s。加入此修订后的全 Go 脚本、桥接、请求和 cookie race 再次通过 9.086 s。随后额外 `TestRuntimeExternalHostExitCannotClaimGoMemoryLimit` 证明外部 host 的状态 73 不会被误认作 Go 内存限额；与上述四种退出子场景一起执行 race 通过 1.613 s。
