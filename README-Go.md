# toSub2 Go

支持 [Sub2API / CLIProxyAPI（CPA）推送目标选择](docs/pure-go-runtime/cpa-upload.md)。在服务配置中保存 CPA 管理地址和管理密钥后，可手动单条／批量推送，或在重登、2FA 弹窗中选择授权后推送目标。CPA 接收 ChatGPT/Codex 授权；BPS 授权使用 Sub2API。CPA 密钥由服务端保护，不保存在浏览器中。

新增 [账号用量与状态查询](docs/pure-go-runtime/account-insights.md)，支持服务端定时采集、单账号/所选账号/全部账号手动查询与独立状态展示；总开关默认关闭。启用后可在“账号状态与用量”栏刷新全部账号用量。具体支持范围、失败恢复和存储边界见功能指南。

当前模块覆盖、问题修复与验收见 [维护台账](docs/maintenance-review-2026-11-10.md#全项目模块审查当前执行)；[第一轮质量优化](docs/quality-review-2026-10-31.md)及[第二轮增量优化](docs/quality-review-2026-10-31-round2.md)保留其历史版本的验收范围。升级、回退及故障恢复见 [部署说明](docs/go-deployment.md#可重复验证与安全升级)。

当前程序只使用 Go 认证：Go HTTP/TLS 与同一可执行文件中的 Sobek worker 执行原版协议流程。旧运行模式、选择参数、Node、Python 和 Chromium 自动化已移除。网页控制台仍在用户的普通浏览器中使用。

## 构建与运行

需要 Go 1.26.0 或更高版本：

模块保留 Go 1.26 最低兼容性，`toolchain go1.27.3` 固定当前生产构建工具链（修复已确认的标准库漏洞）。默认 Go 工具链选择会使用该版本；离线构建应先准备相同工具链。`GOTOOLCHAIN=go1.26.0` 仅用于最低版本兼容验证，不作为当前生产安全基线。

```sh
make build
./bin/tosub2 --host=127.0.0.1 --port=4399 --output-root=/absolute/path/to/private-data
```

`make build` 通过 `go run ./cmd/build-web -build-go`，先调用 Go esbuild API 从 `web/src` 与 `web/vendor` 构建网页，再以 `CGO_ENABLED=0` 编译主程序。`make frontend` 只更新 `web/dist` 与 `cmd/tosub2/web/dist`。浏览器库及许可证已保存在项目中，不需要包管理器安装。只验证并编译现有嵌入资源可运行 `go run ./cmd/build-web -check -build-go`；裸 `go build` 不参与此构建锁和一致性检查。

发布现有资源前用 `go run ./cmd/build-web -check` 检查两份产物与当前源码逐文件一致，检查不会覆盖产物。同一工作区的前端构建、检查和正式 Go 构建使用同一进程锁，持有到所有目标编译及候选发布结束；第二个并发命令明确失败。锁由操作系统在所有者退出时释放，不应删除 `.build-web.lock`。编译器读取私有模块副本中的源码和嵌入资源，输出暂存候选；编译失败保留原程序，普通发布失败回滚。强制终止所有者后，仍运行的编译器不能发布最终程序；其暂存目录可能保留，确认相关进程退出后才能清理。跨文件或两份前端目录的切换不具备进程崩溃原子性，中断后须重新构建并检查再发布。CI 同时拒绝遗漏提交的生成文件。

`TOSUB2_BUILD_DIR=/absolute/temporary/path ./scripts/build.sh`（交叉构建同样支持）可将候选程序及运行时许可证输出到独立目录；`scripts/verify.sh` 默认在临时目录构建，不覆盖工作区已有程序。输出目录须在 `cmd/tosub2`、`internal` 以外，避免把候选目录再次复制进源码。构建副本只包含当前模块的 `go.mod`、`go.sum`、`cmd/tosub2` 和 `internal`，不会复制运行数据；正式构建期间仍应停止编辑源码，构建锁不限制编辑器写入。

打开 `http://127.0.0.1:4399`。页面请求 `/api/bootstrap` 获取后续 API 使用的 token；可通过 `TOSUB2_CONSOLE_TOKEN` 指定 token，API 支持 `Authorization: Bearer TOKEN` 或 `X-Console-Token`。服务默认只监听 loopback。更新前端并重启后，应刷新已打开的页面。

macOS 默认 4401 的隔离测试实例：

```sh
./scripts/local-legacy-macos.sh start
./scripts/local-legacy-macos.sh status
./scripts/local-legacy-macos.sh stop
TOSUB2_LOCAL_REBUILD=1 ./scripts/local-legacy-macos.sh restart
./scripts/local-legacy-macos.sh clean --yes
```

前端源码有变化时先执行 `make frontend`。`stop` 保留资料；`clean --yes` 删除本实例账号、任务、导出、日志和专属 Keychain 条目。运行目录外的备份保留。完整范围见 [本地测试与卸载](docs/local-macos.md)。Docker、Linux、Windows 服务见 [部署说明](docs/go-deployment.md)。

## 命令行账号流程

不提供账号操作参数时启动控制台。常用入口：

```sh
./bin/tosub2 --email you@example.com --output-mode sub2api
./bin/tosub2 --email you@example.com --authorization-method bps --output-mode sub2api
./bin/tosub2 --complete-oauth-grant private/oauth-grant.json
./bin/tosub2 --refresh-sub2api private/sub2api-import-oauth.json
./bin/tosub2 --email you@example.com --setup-totp
./bin/tosub2 --email you@example.com --add-password
```

默认仅输出 OAuth 授权文件，支持 `--output-mode sub2api`、`--sub2api-out`、`--complete-oauth-grant`、`--totp-result`、`--password-add-result`、`--workspace-id` 等参数；完整列表见 `--help`。登录 checkpoint、旧会话续接、session 和 WebOnly 导出入口已经移除。TOTP 和添加密码的私有结果默认保存到 `tmp/chatgpt-totp-setup.json`、`tmp/chatgpt-password-add-result.json`。

密码、TOTP、新密码和邮箱 API 可通过 `CHATGPT_LOGIN_PASSWORD`、`CHATGPT_TOTP_SECRET`、`CHATGPT_NEW_PASSWORD`、`CHATGPT_MAIL_API_URL` 提供。手动验证码和手机号从标准输入读取，输入 `q` 取消。密码保留首尾空白；验证码和手机号去除首尾空白。输入接受后任务转为 working，重复提交返回 409。自动邮箱轮询期间网页允许人工覆盖，CLI 仅在明确人工提示时读取输入。

## 批量导入、凭据与恢复

批量导入只保存资料，需显式开始操作。带分隔符的结构化行按本次资料替换密码、TOTP 和邮箱取码配置，未提供的相应字段清空；只含邮箱的行继续复用已保存资料。自动密码被拒绝后会等待新输入，只有验证成功才保存新的已验证密码。邮箱 API 与账号密码是不同资料，缺少密码不保证能自动转为邮箱验证码登录。

可在末尾 2FA 后添加本地到期时间：`邮箱----密码----2FA----16.30`。`16.30` / `16:30` 表示浏览器时区的导入当天 16:30；`7-18 08:00` / `7-18 8:00` 表示同一时区导入当年的 7 月 18 日 08:00，也支持 `2026-12-31 16:30` 或带时区偏移的完整时间。已经过去的时间直接标记为已到期，不顺延到明天或明年。手动时间优先于已知套餐／订阅到期时间，不使用 Token 到期时间推算。重复导入未填写时间时保留原手动值，在账号信息中可编辑或清除并回退套餐时间。POST 收码格式、时区、未知值和恢复边界见 [账号到期时间](docs/pure-go-runtime/account-actions.md#账号到期时间)。这项本地提醒不会自动踢人、停用、删除或上传账号。

账号行直接提供重登授权、重置 2FA、删除、上传和日志按钮，“更多”中可查看账号资料卡、下载授权等；批量操作位于顶部工具栏。重登授权可选择成功后自动上传 Sub2API。账号资料卡展示当前保存的邮箱、密码、2FA 和收码配置，敏感字段默认隐藏。

授权弹窗支持 **ChatGPT / BPS** 二选一，默认保留 ChatGPT。BPS 使用独立的 OAuth 应用与统一 token 接口，纯 Go 直接完成认证，无需启动浏览器。每个账号保存所选方式；新的独立登录可以重新选择方式，旧登录阶段不能阻止新操作。重置 2FA 和退出设备仍使用 ChatGPT 安全流程，完成后才按选择的方式重新授权。CLI 的 `--authorization-method chatgpt|bps` 用于新登录，已取得 token 的本地补保存和正常刷新必须匹配授权身份。HAR 分析、接口依据与验证边界见 [BPS 授权](docs/pure-go-runtime/bps-authorization.md)。

BPS 导出和上传带有 Sub2 的 `openai_excel_bps` 标记，并在 `credentials` 中保存可从当前 token 获取的用户、组织、原始套餐及 access token 到期时间。旧导出在上传时补齐缺失字段；切换回 ChatGPT 时关闭 BPS 标记。指纹种子、计费、隐私及显式模型映射配置保留；BPS 兼容模型列表遵循下述已确认规则。订阅期限不会从 token 到期时间推测。

BPS 在未配置模型列表时按默认模板写入 9 个模型，具体名单见 [BPS 配置模板](docs/pure-go-runtime/bps-authorization.md#用户指定的-bps-配置模板)。新导出和缺失/null 字段使用该 `extra.openai_excel_bps_models` 默认数组；上传、更新、刷新及巡检同步保留已有显式模型列表（包括空数组），不覆盖用户路由选择。其余模板为 `initial` 配置模式、忽略加密内容、过滤不支持的工具、403 自动禁用和转组（目标组 `0`、恢复间隔 `60` 分钟），模型映射模式默认 `whitelist`。同步会将上一版 `defaults` 模式迁移为 `initial`，其余设置补缺失项并保留远端显式值；切回 ChatGPT 时清除 BPS 专属配置。模型选择不证明授权有效、上游权限可用或套餐符合自动恢复规则。

普通任务元数据、日志和公开列表 API 不保存密码、TOTP、Cookie、验证码、OAuth token、代理密码或管理员 key。OAuth 授权文件、纯 token 保存结果与受保护的业务结果仍是私有资料，输出目录应保持私有。

Cookie 仅保留于当次操作的内存 CookieJar；取消、结束或退出后，不保存或恢复登录会话。包括原仓库在内的任务级 `login-checkpoint.json`、session 导出、共享账号 Cookie 仓库和跨任务旧 Cookie 复用均已移除。已取得 OAuth token、轮换 refresh grant 和已确认激活的新密钥仅允许纯本地补保存。详见 [登录会话与历史数据清理](docs/pure-go-runtime/authentication-session-removal-2026-11-08.md) 和 [三轮严格复审](docs/pure-go-runtime/authentication-session-review-2026-11-09.md)。

| 平台 | 敏感凭据存储 |
| --- | --- |
| macOS | Keychain 与私有加密文件；本地脚本使用专属 namespace |
| Windows | AES-256-GCM 文件，DPAPI CurrentUser 包装密钥；当前用户专属 protected DACL |
| Linux | 进程内存储，重启后重新提供密码、TOTP、API key 等敏感配置 |

需要持久安全存储的操作在后端不可用时明确报错。短信订单和自动取码恢复资料同样依赖安全存储；重启后不恢复旧登录或旧验证码提交；供应商订单管理不代表认证会话仍可续接。已发起但未确认的完成/释放请求不会在重启后自动重复；请先到供应商核实。Sub2API 监控默认每 5 分钟运行，支持冷却、永久跳过、按套餐排除自动重登和只处理尚未发起的待同步授权；自动回传开始后若中断或结果未全部确认，会保留授权与目标并暂停，请核对远端后手动上传，不会重登或自动重放修改；管理员 key 不写入监控 JSON。账号列表展示授权元数据中的套餐，设置入口与识别范围见 [套餐显示与排除规则](docs/pure-go-runtime/account-plans.md)。

旧 macOS 全局凭据不会自动进入本地隔离 namespace。离线导入需先停止实例，再指定旧凭据目录及账号来源；源只读，目标已有账号默认跳过，详见 [本地凭据导入](docs/local-macos.md#显式导入旧-macos-凭据)。metadata 恢复成功不代表凭据已导入。

当次 Go 认证保留代理、TLS profile、UA、设备和 OAuth state/PKCE/client/redirect 绑定。等待密码、邮箱验证码、TOTP 和兑换回调码都在当前内存流程中完成；进程重启后必须发起新登录，不加载已保存的 callback code 或登录阶段。

OAuth code 换取 token 后先保存不含 Cookie、回调码及 PKCE 的纯 token 结果，再写最终导出。新任务操作使用独立的 `oauth-grant-<结果标识>.json`，旧结果不会被新登录覆盖或误认为新登录成功；历史 `oauth-grant.json` 与明确指定的 CLI 结果仍支持本地完成。输出失败使用 `--complete-oauth-grant <结果文件>` 只做本地保存，不再次消费 code。refresh token 轮换成功后先写目标旁的 `.<文件名>.refresh-grant-recovery.json`；重复命令验证源和目标身份后保存已经取得的结果，不重复 grant。多账号 JSON 只刷新首项，保留其他账号、业务字段与大整数。`REFRESH_GRANT_UNSAVED` 表示连结果文件也无法保存，此时不能保证补保存，程序不会自动重复 grant。

## 配置和网络

| 配置 | 默认值或用途 |
| --- | --- |
| `ONBOARDING_HOST` / `--host` | `127.0.0.1` |
| `ONBOARDING_PORT` / `--port` | `4399` |
| `ONBOARDING_OUTPUT_ROOT` / `--output-root` | `tmp/chatgpt-onboarding-console` |
| `TOSUB2_MAX_ACTIVE_JOBS` | 最大活动任务数，默认 20 |
| `TOSUB2_AUTH_TLS_PROFILE` / `--auth-tls-profile` | 认证 TLS profile，默认 `chrome146` |
| `TOSUB2_TLS_PROFILE` / `--tls-profile` | 独立 HTTP 路径的 profile，默认 `auto` |
| `TOSUB2_SENTINEL_SDK_URL` / `--sentinel-sdk-url` | 默认公开 Sentinel loader；HTTPS，loopback fixture 可用 HTTP |
| `CHATGPT_PROXY_URL` / `CHATGPT_PROXY_URLS` | 固定代理或代理池 |
| `TOSUB2_REQUEST_TIMEOUT_MS` | 默认 30000 |
| `TOSUB2_SHUTDOWN_TIMEOUT_MS` | 默认 10000 |
| `SUB2API_BASE_URL` / `SUB2API_ADMIN_API_KEY` | Sub2API 后端地址及管理员 key |

服务配置 → Sub2API 可设置“请求时区”，默认 `America/Los_Angeles`，支持有效的 IANA 时区名称（如 `Asia/Shanghai`、`UTC`）。手动上传、重登/重置 2FA 后自动上传及号池自动重登同步均携带 `extra.openai_request_timezone`；更新已有账号时合并该字段，保留其他号池配置。设置保存后，刷新页面和重启服务仍会保留。

认证 HTTP、动态 SDK 请求与 `document.cookie` 使用同一 Go transport 和权威 CookieJar；JS 无法读取 HttpOnly cookie 或响应中的 Set-Cookie。支持 HTTP、HTTPS、SOCKS5、SOCKS5h 代理。已有认证会话不得任意更换代理或 TLS 身份；修改代理后需显式重新登录。

本机代理使用实际 loopback 地址；Docker 内的 `127.0.0.1` 指容器本身，访问 Mac 宿主代理可用 `host.docker.internal`。HTTP 403/429 表示已收到 HTTP 响应，不能据此认定 TLS 不兼容。`context deadline exceeded` 只说明预算耗尽，需结合阶段、请求数和状态排查；日志不会打印动态脚本、Cookie 或完整请求内容。

## 测试与边界

```sh
go test ./...
go test -race ./...
go vet ./...
TOSUB2_LEGACY_RUNTIME_TEST=1 go test -count=1 ./cmd/tosub2 -run TestLegacyRuntimeProductionLoginAndResume
./scripts/build-cross.sh
```

`scripts/verify.sh` 执行全仓普通/race/vet 与构建。`scripts/test-container.sh` 在已有生产镜像中执行无外网的 Go 合成 fixture。Windows 上使用 `scripts/test-windows.ps1`；交叉编译不能替代 DPAPI、DACL 和服务现场验收。Keychain、长请求与监控测试有显式启用条件，跳过不算通过。

宿主覆盖范围和资源限制见 [纯 Go 运行时](docs/pure-go-runtime/README.md)。单个/批量重置 2FA 支持可选退出所有设备，再重登授权并自动上传 Sub2API；使用方式、恢复行为及模拟验证见 [重置 2FA](docs/pure-go-runtime/totp-reset.md)。账号资料卡与原始信息导出均读取当前保存的资料，重置成功后使用新 2FA 密钥。当前线上 challenge、真实账号、短信/邮箱服务与 Sub2API 完整流程需独立验收。旧分支历史报告仅在外部备份中保留。
