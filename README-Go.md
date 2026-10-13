# toSub2 Go

当前程序只使用 Go 认证：Go HTTP/TLS 与同一可执行文件中的 Sobek worker 执行原版协议流程。旧运行模式、选择参数、Node、Python 和 Chromium 自动化已移除。网页控制台仍在用户的普通浏览器中使用。

## 构建与运行

需要 Go 1.25.0 或更高版本：

```sh
make build
./bin/tosub2 --host=127.0.0.1 --port=4399 --output-root=/absolute/path/to/private-data
```

`make build` 先调用 Go esbuild API，从 `web/src` 与 `web/vendor` 构建网页，再以 `CGO_ENABLED=0` 编译主程序。`make frontend` 只更新 `web/dist` 与 `cmd/tosub2/web/dist`。浏览器库及许可证已保存在项目中，不需要包管理器安装。只编译现有嵌入资源可运行 `CGO_ENABLED=0 go build -o bin/tosub2 ./cmd/tosub2`。

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
./bin/tosub2 --resume-checkpoint private/login-checkpoint.json
./bin/tosub2 --refresh-sub2api private/sub2api-import-oauth.json
./bin/tosub2 --email you@example.com --setup-totp
./bin/tosub2 --email you@example.com --add-password
```

支持 `--output-mode both|session|sub2api`（默认 `both`）、`--out`、`--sub2api-out`、`--checkpoint`、`--totp-result`、`--password-add-result`、`--web-only`、`--no-sub2api-export`、`--workspace-id` 等参数；完整列表见 `--help`。只有显式 `--resume-checkpoint` 恢复旧会话；`--checkpoint` 指定输出路径。TOTP 和添加密码的私有结果默认保存到 `tmp/chatgpt-totp-setup.json`、`tmp/chatgpt-password-add-result.json`。

密码、TOTP、新密码和邮箱 API 可通过 `CHATGPT_LOGIN_PASSWORD`、`CHATGPT_TOTP_SECRET`、`CHATGPT_NEW_PASSWORD`、`CHATGPT_MAIL_API_URL` 提供。手动验证码和手机号从标准输入读取，输入 `q` 取消。密码保留首尾空白；验证码和手机号去除首尾空白。输入接受后任务转为 working，重复提交返回 409。自动邮箱轮询期间网页允许人工覆盖，CLI 仅在明确人工提示时读取输入。

## 批量导入、凭据与恢复

批量导入只保存资料，需显式开始操作。带分隔符的结构化行按本次资料替换密码、TOTP 和邮箱取码配置，未提供的相应字段清空；只含邮箱的行继续复用已保存资料。自动密码被拒绝后会等待新输入，只有验证成功才保存新的已验证密码。邮箱 API 与账号密码是不同资料，缺少密码不保证能自动转为邮箱验证码登录。

账号行直接提供重登授权、重置 2FA、删除、上传和日志按钮，“更多”中可查看账号资料卡、下载授权等；批量操作位于顶部工具栏。重登授权可选择成功后自动上传 Sub2API。账号资料卡展示当前保存的邮箱、密码、2FA 和收码配置，敏感字段默认隐藏。

授权弹窗支持 **ChatGPT / BPS** 二选一，默认保留 ChatGPT。BPS 使用独立的 OAuth 应用与统一 token 接口，纯 Go 直接完成认证，无需启动浏览器。每个账号保存所选方式；继续中断任务沿用检查点方式，切换方式会完整重新登录。重置 2FA 和退出设备仍使用 ChatGPT 安全流程，完成后才按选择的方式重新授权。CLI 的 `--authorization-method chatgpt|bps` 用于首次登录，恢复和刷新默认继承已保存身份。HAR 分析、接口依据与验证边界见 [BPS 授权](docs/pure-go-runtime/bps-authorization.md)。

BPS 导出和上传带有 Sub2 的 `openai_excel_bps` 标记，并在 `credentials` 中保存可从当前 token 获取的用户、组织、原始套餐及 access token 到期时间。旧导出在上传时补齐缺失字段；切换回 ChatGPT 时关闭 BPS 标记。既有 BPS 模型配置、指纹种子、计费与隐私配置保留，订阅期限不会从 token 到期时间推测。

BPS 新账号采用用户最终确认的 Sub2 模板：`initial` 配置模式、忽略加密内容、过滤不支持的工具、403 自动禁用和转组（目标组 `0`、恢复间隔 `60` 分钟），模型默认 `gpt-6-astra` / `gpt-5.6-sol` / `gpt-5.6-terra`，模型映射模式 `whitelist`。刷新及自动重登同步会将上一版 `defaults` 模式迁移为 `initial`，其余设置补缺失项并保留远端显式值；切回 ChatGPT 时清除 BPS 专属配置。该模型列表是配置模板，不代表对账号调用权限的探测结果。

普通任务元数据、日志和公开列表 API 不保存密码、TOTP、Cookie、验证码、OAuth token、代理密码或管理员 key。session、OAuth、checkpoint 和私有结果包含恢复或导入所需资料，输出目录应保持私有。

| 平台 | 敏感凭据存储 |
| --- | --- |
| macOS | Keychain 与私有加密文件；本地脚本使用专属 namespace |
| Windows | AES-256-GCM 文件，DPAPI CurrentUser 包装密钥；当前用户专属 protected DACL |
| Linux | 进程内存储，重启后重新提供密码、TOTP、API key 等敏感配置 |

需要持久安全存储的操作在后端不可用时明确报错。短信订单和自动取码恢复资料同样依赖安全存储；重启后需继续对应任务才恢复轮询，暂停重试的任务不会因重启自动提交。Sub2API 监控默认每 5 分钟运行，支持冷却、永久跳过、按套餐排除自动重登和只重传未完成更新；管理员 key 不写入监控 JSON。账号列表展示授权元数据中的套餐，设置入口与识别范围见 [套餐显示与排除规则](docs/pure-go-runtime/account-plans.md)。

旧 macOS 全局凭据不会自动进入本地隔离 namespace。离线导入需先停止实例，再指定旧凭据目录及账号来源；源只读，目标已有账号默认跳过，详见 [本地凭据导入](docs/local-macos.md#显式导入旧-macos-凭据)。metadata 恢复成功不代表凭据已导入。

Go 认证的 checkpoint 绑定实际代理、TLS profile、UA 和设备身份。恢复中间认证会话要求已有当前 Go 身份绑定；无绑定或旧运行时绑定的 checkpoint 需显式重新登录，文件保留。已取得的 token、可交换的 callback code 和已保存结果按相应恢复阶段处理，不能把旧会话解析成功视为可继续认证。

OAuth code 换取 token 后先保存 `tokens_received` checkpoint，再写最终导出；输出失败后继续任务只做本地恢复，不再次消费 code。refresh token 轮换成功后先写目标旁的 `.<文件名>.refresh-grant-recovery.json`；重复命令验证源和目标身份后恢复结果，不重复 grant。多账号 JSON 只刷新首项，保留其他账号、未知字段与大整数。`REFRESH_GRANT_UNSAVED` 表示连恢复文件也无法保存，此时不能保证恢复，程序不会自动重复 grant。

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
