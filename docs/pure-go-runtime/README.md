# 纯 Go 认证运行时

生产 CLI、控制台任务和 checkpoint 恢复统一使用 Go 认证。Go 父进程保留 HTTP/TLS、代理、身份和 CookieJar，同一个可执行文件的独立 worker 使用 Sobek 执行动态 JavaScript。生产运行无需 Node、Python、Chrome、cgo 或原生 JS 动态库；认证时允许同一文件的多个进程。

旧运行分支、选择参数与外部脚本工具已删除。React 前端构建也改为 Go esbuild API，浏览器客户端库与许可证存放在 `web/vendor`；普通浏览器只用于查看网页控制台。该依赖清理不代表线上所有动态程序已经验收。

## 构建、运行、清理

需要 Go 1.25.0 或更高版本：

```sh
make build
./bin/tosub2 --host=127.0.0.1 --port=4399 --output-root=/absolute/path/to/private-data
```

`make build` / `scripts/build.sh` 先构建前端，再以 `CGO_ENABLED=0` 编译程序，并将认证依赖许可证复制到 `bin/AUTH_RUNTIME_THIRD_PARTY_NOTICES.txt`。分发时应附上适用的第三方许可证。`scripts/build-cross.sh` 交叉构建 macOS arm64/amd64、Linux amd64 和 Windows amd64。

macOS 隔离本地实例：

```sh
./scripts/local-legacy-macos.sh start
./scripts/local-legacy-macos.sh status
# 更新同一实例二进制，保留数据和 Keychain：
make frontend
TOSUB2_LOCAL_REBUILD=1 ./scripts/local-legacy-macos.sh restart
./scripts/local-legacy-macos.sh stop
# 永久删除此实例数据、日志和专属 Keychain 项：
./scripts/local-legacy-macos.sh clean --yes
```

默认地址为 `http://127.0.0.1:4401`。脚本名仅沿用现有实例命名空间，不选择运行分支。停止保留数据；clean 删除本实例及专属 Keychain，保留其他实例、源码和外部备份。浏览器站点配置不属于服务目录。详细范围见 [本地部署](../local-macos.md)，实际服务更新另记在 [部署记录](local-deployment.md)。

## 执行与会话边界

1. `cmd/tosub2/main.go` 在应用配置/存储初始化前识别私有 worker 参数。CLI/console runner 使用唯一的 Go 认证实现；`internal/legacyruntime` 为沿用的内部包名。
2. 父进程动态获取 loader、发现 SDK 版本并下载 SDK。没有固定线上版本、验证 token 或算法结果。私有输入文件有权限限制，退出与取消后由父进程清理。
3. `internal/jshost` 在每个认证 worker 中创建独立 JS realm。iframe 的 realm 共用一个所有者事件循环；Web Worker 具有自己的引擎和循环，通过复制消息通信。没有跨 goroutine 操作同一引擎，也不直接跨引擎共享 JS 对象。
4. DOM、iframe、WindowProxy、事件、Promise/计时器、structured clone/ArrayBuffer transfer、fetch/XHR 和有限 Web API 由嵌入代码与 Go bridge 提供。
5. HTTP 请求及 document.cookie 操作经 stdin/stdout 协议返回父进程。所有请求复用原会话 transport 和权威 CookieJar；JS 看不到 HttpOnly cookie/Set-Cookie 响应头。没有独立 shadow cookie jar 或 Go JS 宿主 HTTP client。
6. checkpoint 绑定 TLS/UA/代理/设备。已接受验证码、OAuth code 和已授权结果按相应阶段恢复；失败不清空绑定、重建会话或重放已消费输入。离线恢复、token 刷新和导出均由 Go 处理。

闭集错误码避免把脚本异常、URL 查询、Cookie 或认证输入写入错误日志。worker 的超时、取消、错误退出与资源 guard 可被父进程识别。150 秒执行期限、256 MiB GC 目标、384 MiB HeapAlloc 采样 guard 和任务/realm/worker 配额见 [资源与诊断](engine-probe/worker-boundaries.md)。采样 guard **不是 OS 硬内存上限**；父 transport 仍按既有方式缓冲 HTTP 响应，不能据此宣称主进程或整条链路内存有硬上限。

## 支持范围与尚未验证部分

详细 API、原型、事件和消息证据见 [宿主需求及语义](host-requirements.md) 与 [Web Platform 验证](engine-probe/web-platform-validation.md)。主要边界：

- DOM 解析、动态 classic script、初始 inline script 与实际 Sentinel/CF frame 协议已接入。没有完整浏览器 parser/layout/canvas、通用 iframe HTML/srcdoc 自动导航、JS modules 或 WebAssembly。当前初始外部脚本策略延续原 CJS 的 outside-only 模式，外部资源由动态 script 路径加载。
- Classic Web Worker 实际执行独立 JS 并支持 Blob/远程 source、importScripts、消息/transfer、退出与取消。模块 worker 明确拒绝。Blob object URL 当前供 Worker/importScripts 读取，`fetch(blob:)` 未实现。
- ReadableStream 支持 default producer、队列、backpressure、reader、tee、pipe、cancel/error。byte stream/BYOB 明确拒绝；未暴露空的 WritableStream/TransformStream 构造器。
- `crypto.subtle` 只提供 SHA-1/256/384/512 digest，其他密码 API 尚未实现。local/sessionStorage 按会话和 origin 隔离、限额 5 MiB，未实现磁盘持久化或 storage 事件。
- 计时器及请求有运行时间记录；沿用旧 CF 输入的 navigation performance 阶段值是估算，不能当作实际浏览器网络阶段测量。详见源码 `internal/legacyruntime/challenge.go`。
- 固定 Sobek 不支持 RegExp `d`/`v` 和 async generators。未知必要能力会失败并返回受限诊断，不会用空 token 或切换外部运行时掩盖。
- 公开 Sentinel loader/SDK 的匿名下载与初始化、合成完整登录、真实账号登录是三个层次。前两项有独立证据；真实账号、当前线上 CF/Turnstile 全流程、SMS/Sub2API 及 Windows 系统功能未在本轮实机验收。没有将这些项标成通过。

## 验证与选型

Sobek 与 Goja 的实际语法/异步/中断/多 realm 比较、版本与许可证取舍见 [引擎探针](engine-probe/README.md)；依赖许可见 [THIRD_PARTY_NOTICES.txt](THIRD_PARTY_NOTICES.txt)。Sobek 在实测覆盖相同的情况下要求 Go 1.25，比较的 Goja 版本要求 Go 1.26。

生产 CLI 密码/MFA/邮箱 OTP/恢复场景与 scratch 容器证据见 [生产 fixture 验收](production-fixture-verification.md)。全仓、最低 Go、交叉构建及产物身份见 [检查记录](final-verification.md)。这些记录中的外部宿主对照是迁移时证据，旧运行路径已清理。

```sh
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -o bin/tosub2 ./cmd/tosub2
GOTOOLCHAIN=go1.25.0 CGO_ENABLED=0 go test ./...
./scripts/build-cross.sh

TOSUB2_LEGACY_RUNTIME_TEST=1 \
  go test -race ./internal/jshost ./internal/legacyruntime ./cmd/tosub2 -count=1
```

所有测试凭据和模拟 token 都仅存在于合成 fixture。系统 Keychain、长时测试及子进程 helper 有明确启用条件；不能把 skip 算作通过。本地测试不执行真实账号登录。
