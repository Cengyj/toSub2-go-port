# toSub2 Go 迁移审计

初始审计：2026-09-25；基线补验与实施复核：2026-09-26（Asia/Shanghai）
审计对象：/Users/ceng/Downloads/toSub2-main
源版本：package.json 版本 1.7.1。此目录不包含 Git 元数据，因此本报告以当前工作树快照为基线。
审计方式：静态阅读用户指定源码、控制台前端、Cloudflare/Sentinel runtime、测试和部署文档；用 rg/sed 提取路由、状态、配置和数据字段；实际执行现有基线检查并迁移本地回归。没有读取用户真实账号凭据；系统凭据集成测试只使用专用合成条目并清理。

## 基线执行结果

本轮补做了原项目 package.json 中 `check` 的全部 20 个命令（9 项语法检查和 11 项测试脚本），使用构建期 Node 24.19.0。首次 17 项通过，3 项因系统 Python 缺少 curl_cffi 失败；随后仅为旧版基线在仓库外临时 venv 安装原 requirements.txt 锁定的 curl_cffi 0.15.0，三项重试全部通过。因此旧版这 20 项命令均有通过记录，原始失败与重试结果均保留，没有把环境错误覆盖成首次通过。

- 首次失败：`runtime-process.mjs`、`tls-transport.mjs`、`protocol-smoke.mjs`（refresh 阶段）。
- 隔离环境重试：以上三项全部通过；其他 17 项首次即通过。
- 证据：`docs/go-port-legacy-baseline.json` 保存每项命令、退出码、耗时和合成 fixture 输出。
- 基线快照：`docs/go-port-source-manifest.json` 记录原项目指定源码/测试/文档的 SHA-256，便于判断未来审计是否仍针对同一版本。
- 这些 Node/Python 依赖只服务于旧版回归基线；Go 构建、测试、生产启动与新的 Chromium CDP 路径不调用它们。所有网络认证/短信/后端行为都是本地 mock，不是本人账号或第三方服务验收。

## 1. 当前模块职责

| 当前模块 | 职责和外部边界 | 迁移目标 |
|---|---|---|
| src/console-server.mjs | Node HTTP server；启动 token；Vite middleware；任务创建/批量、队列、邮箱锁、取消、重试、恢复、日志、下载、导出、短信轮询、Sub2API 上传与每 5 分钟监控 | internal/console、internal/jobs、internal/store、internal/sub2api、internal/logging；net/http + embed.FS |
| src/protocol-login.mjs | Cookie Jar；手动 301/302/303/307/308；ChatGPT 邮箱/密码登录；MFA/TOTP；workspace；Codex OAuth PKCE；手机绑定；token 交换/刷新；checkpoint；sub2api-data 输出 | internal/protocol；使用 transport 接口，不使用 stdout 文本作为内部状态 |
| src/tls-transport.mjs | Node 到 Python NDJSON worker 的生命周期、代理会话轮换、TLS profile 探测/降级、同代理风险重试、Cloudflare/Sentinel 调用 | internal/transport、internal/challenge；优先 tls-client，必要时 utls |
| src/tls_transport.py | curl_cffi session；profile fallback；代理；Set-Cookie；Cloudflare solver 调用；Sentinel 动态客户端调用 | internal/transport；Go 进程内实现，生产不调用 Python |
| src/credential-store.mjs | macOS Keychain 保存 AES-256-GCM 密钥；Windows PowerShell DPAPI；Linux 明确不持久化 | internal/credentials；Keychain/DPAPI/系统凭据服务；无安全服务时安全失败 |
| src/mail-otp.mjs | 邮箱 API GET/POST；2 MB 限制；JSON/HTML/文本递归解析；上下文评分；6 位 OTP；邮件时间和请求时间过滤 | internal/mail |
| src/sms-providers.mjs | provider 定义、字段、配置验证、统一适配器 | internal/providers |
| src/luban-sms.mjs | LubanSMS getNumber/getSms/release，验证码提取，E.164 规范化 | internal/providers/luban |
| src/smsbower.mjs | SMSBower 取号、价格/库存、状态 1/6/8、轮询和 6 位码提取 | internal/providers/smsbower |
| src/custom-sms.mjs | 最多 500 个手机号/API；重复手机号最后一行覆盖；取号前记录旧验证码，只接受新验证码 | internal/providers/custom |
| src/cloudflare-ctf/cloudflare_solver.py | 解析 Cloudflare challenge，维护 curl_cffi session，调用本地 jsdom Node runtime，处理父 challenge 和 Turnstile iframe，检查 clearance 后重放 | internal/challenge；必须改为真实 Chromium BrowserSession |
| src/cloudflare-ctf/cf_runtime.cjs | jsdom + vm 模拟 window/document/iframe/postMessage/XHR/fetch/MutationObserver/performance/crypto，执行 challenge 脚本 | 仅作为迁移参考；生产用 rod/chromedp |
| src/cloudflare-ctf/turnstile_runtime.cjs | jsdom 内 iframe、消息桥、XHR/fetch、动态脚本执行、Turnstile 事件 | 仅作为参考；生产用 Chromium |
| src/cloudflare-ctf/sentinel_dynamic.py | 下载动态 loader 和版本化 SDK，运行 Node Sentinel runtime，复用 curl_cffi session/cookie/device ID | internal/challenge + BrowserSession；不得依赖 Python/Node |
| src/cloudflare-ctf/sentinel_runtime.cjs | jsdom 父页/iframe、动态 Sentinel SDK、环境属性和网络拦截 | 仅作为参考；真实 Chromium 页面执行 |
| src/cloudflare-ctf/runtime_process.py | 启停本地 Node JSON runtime，线程读取 stdout/stderr，超时和清理 | Go context + Chromium 生命周期 |
| src/cloudflare-ctf/sentinel_client.py | SentinelClient 封装 loader、token、Session Observer Token | internal/challenge |
| src/sentinel-sdk.js | 旧静态 SDK 文件，当前动态路径不再直接使用 | 归档；Go 不应依赖静态 PoW/DX |
| web/src/main.jsx | React 控制台、轮询、分页/筛选/跨页选择、批量操作、验证码/短信交互、Sub2API 配置、localStorage | 保留源码；构建成 web/dist，由 go:embed 托管 |
| web/src/styles.css | 控制台样式 | 构建产物随 embed.FS 发布 |
| test/*.mjs | mock server、协议、TLS、凭据、队列、锁、恢复、短信和控制台 smoke | 迁移为 Go 单元/集成测试；保留 mock fixture |
| test/runtime-process.py | Cloudflare/Sentinel runtime 子进程 fixture | 改为 Go BrowserSession 集成 fixture |

## 2. 当前任务状态机

控制台 job 初始为 queued。调度按 queuedAt/createdAt 升序，最多 20 个非终止任务占用槽位；同邮箱通过大小写不敏感的邮箱锁串行。启动模式包括 full、refresh、totp_setup、password_add。

可观察状态及含义：

- queued：等待 FIFO 槽位。
- starting：建立登录会话或重新登录。
- refreshing：用现有 refresh token 直接生成新 OAuth 文件。
- password_add_starting、password_add：登录后添加密码。
- totp_starting、totp_setup_otp：登录后设置新 TOTP，后者等待手动 6 位激活码。
- working、finalizing：协议请求/Token 交换/文件收尾。
- password：等待密码。
- email_otp：等待邮箱 6 位码，支持 r 重发和邮件 API 自动轮询。
- mfa_otp：等待登录 TOTP 6 位码。
- phone：等待 E.164 手机号，可由 provider 自动取号。
- phone_otp：等待短信码，支持 r 重发、p 换号、provider 自动轮询。
- resume_available：checkpoint 保留，可继续手机号/OAuth，或服务重启后恢复。
- reauth_required：旧登录状态失效或安全凭据缺失，需要重新授权。
- failed：不可继续的流程错误。
- canceled：用户取消或服务优雅关闭。
- completed：sub2api-import-oauth.json 已写入并可下载。

终止状态是 completed、failed、canceled、reauth_required、resume_available。安全校验触发时任务停止并保留 checkpoint；带可轮换 sid 的代理最多使用 10 个 HTTP 风控会话，连接失败最多累计 20 次；同代理风险重试默认 3 次。refresh 失效会自动回退 full 登录。成功完整登录会记录 password/emailOtp/mfa 是否自动完成，供 Sub2API 自动修复资格判断。

协议 checkpoint 阶段为 email_verified、phone_required、phone_otp、oauth_continue、callback_ready。核心恢复分支：

1. 读到完整 sub2api 输出：恢复 completed，并尝试恢复中断的密码/TOTP 结果。
2. 只有 login-checkpoint：恢复 resume_available，并携带 OAuth 手机阶段。
3. 只有 job-meta：queued/starting 可重排队；其他非终止状态恢复 failed；安全凭据缺失的 queued 任务恢复 reauth_required。
4. 服务关闭：取消 active child、停止邮件/短信轮询、等待元数据写入、关闭 HTTP server/Vite。

## 3. 当前 API 路由清单

所有 /api 请求（/api/bootstrap 除外）必须带 x-console-token；token 在进程启动时随机生成，不持久化。JSON 响应 no-store、nosniff，正文限制约 1 MB。

| 方法 | 路径 | 行为 |
|---|---|---|
| GET | /api/bootstrap | 返回 console token 和 feature flags |
| GET | /api/jobs?page=N | 分页任务、跨页 selection、统计 |
| POST | /api/jobs/query | 按邮箱列表筛选并分页 |
| POST | /api/jobs | 单账号创建或按邮箱更新资料/代理 |
| POST | /api/jobs/batch | 批量解析账号文本并创建/更新，最多 500 |
| POST | /api/jobs/download-batch | 合并已完成 sub2api-data，Content-Disposition 下载 |
| POST | /api/jobs/export-source | 导出可重新导入的账号源文本，敏感字段从安全凭据服务读取 |
| POST | /api/jobs/delete-batch | 按邮箱删除任务目录和安全凭据 |
| POST | /api/jobs/reauthorize-batch | 对 completed 走 refresh，其余可恢复状态重新授权 |
| POST | /api/jobs/relogin-batch | 批量强制清除 checkpoint 后重新登录 |
| POST | /api/jobs/setup-2fa-batch | 批量设置 TOTP |
| POST | /api/jobs/add-password-batch | 批量添加密码 |
| POST | /api/jobs/cancel-all | 取消所有 active/queued 任务 |
| POST | /api/mail-request-config | 配置全局邮件 API GET/POST、请求体和自定义头 |
| POST | /api/sub2api/groups | 读取 OpenAI 号池 |
| POST | /api/sub2api/options | 读取号池和代理列表 |
| POST | /api/sub2api/upload | 通过 x-api-key 批量上传已完成账号 |
| GET | /api/sub2api/monitor | 返回监控状态（不返回 API Key） |
| POST | /api/sub2api/monitor | 启用/禁用并持久化监控 |
| POST | /api/sub2api/monitor/check | 立即执行一次异常账号巡检 |
| POST | /api/sms-providers/smsbower/options | 查询国家价格和库存 |
| POST | /api/jobs/:id/input | 提交 password/email_otp/mfa_otp/totp_setup_otp/phone/phone_otp/resend/change_phone |
| POST | /api/jobs/:id/cancel | 取消单任务并释放号码 |
| POST | /api/jobs/:id/retry | 失败、取消或 checkpoint 任务重试 |
| POST | /api/jobs/:id/regenerate | completed 任务用 refresh token 重新生成 |
| POST | /api/jobs/:id/relogin | 强制重新登录并授权 |
| POST | /api/jobs/:id/setup-2fa | 设置新 TOTP |
| POST | /api/jobs/:id/add-password | 添加强密码 |
| GET | /api/jobs/:id/logs | 读取脱敏任务日志 |
| GET | /api/jobs/:id/totp-setup | 受 token 保护的设置流程 secret 一次性读取，不进入任务列表或元数据 |
| GET | /api/jobs/:id/download | 下载单账号 OAuth 文件 |
| POST | /api/jobs/:id/sms-number | 通过 luban/smsbower/custom 取号并提交 |
| POST | /api/jobs/:id/luban-number | 旧前端兼容的 LubanSMS 取号别名 |

下载响应使用 attachment 文件名并含时间戳；Go 端必须保留这些路径和字段，若内部 API 重构需提供兼容 handler。

## 4. 当前 JSON 文件格式

### sub2api-import-oauth.json

顶层固定为 type=sub2api-data、version=1、exported_at ISO 时间、proxies 数组、accounts 数组。账号包含 name、platform=openai、type=oauth、credentials 和 extra。credentials 至少有 access_token、refresh_token、id_token、email、chatgpt_account_id；extra 含 account_id、chatgpt_account_id、chatgpt_user_id、client_id、email 及四个 OpenAI 能力/隐私字段；并保留 concurrency、priority、rate_multiplier、auto_pause_on_expired。批量下载去重 proxies 但不修改账号字段。参见 docs/fixtures/go-port-golden/sub2api-import-oauth.json。

### login-checkpoint.json

version=1、stage、updated_at、email、chatgpt_base、auth_base、warning、cookies、web；OAuth 阶段额外保存 oauth.codeVerifier、state、sessionId、addPhoneUrl、current、workspaceId、phone、phoneReferer、callbackUrl。cookies 是 name/value/domain/path/secure/expires 数组。此文件包含登录 Cookie 和 OAuth 状态，权限必须 0600、原子替换，不能进入普通日志或公开 API。

### job-meta.json

version=1；email、status、prompt、last_error、result_saved、completed_at、attempt、security_check_required、queued_mode/queued_at、created_at/updated_at、last_operation_at/type、login_mode、mail_api_url/mail_request_body；只保存 has_stored_credentials、has_password、has_totp_key、totp_known_enabled 等布尔标记，不保存 password/totp/proxy secret；保存 checkpoint 可用性、代理风险计数、短信 provider/order/number/status、自动化登录资格和 Sub2API auto-repair 状态。参见 fixture。当前实现会把 mail_api_url、sms_number 和 sub2api monitor API Key 以外的非 token 元数据写入磁盘；Go 端应继续避免新增敏感字段。

### totp-setup-result.json

version=1、email、secret、otpauth_uri、activation_succeeded、activated_at。设置流程激活后由控制台安全存储 secret，再删除结果文件；中断时保留以便恢复。权限 0600。

### password-add-result.json

version=1、email、password、added_at。成功后写入系统凭据并删除；恢复失败时保留。权限 0600。

### sub2api-monitor.json

version=1、enabled、config（baseUrl、adminApiKey、groupIds、proxyId、concurrency、loadFactor、priority、modelWhitelist、codexFingerprintMode）、state（lastCheckAt、lastError、lastResult）、updatedAt。当前版本把 adminApiKey 写入该 0600 文件，虽不出现在 job-meta、日志或导出；Go 端应迁移此字段到 CredentialStore，并对旧明文文件做一次性读取/加密迁移。

### 凭据存储 payload

系统存储的逻辑 payload 为 version=2、password、totpSecret、proxyUrl。macOS 以 Keychain 账户 email 保存 32 字节 AES key，再在凭据目录保存 AES-256-GCM 的 version=1 iv/tag/data。Windows 保存 DPAPI CurrentUser 加密后的 base64 文件。Linux 当前返回 501 并不落盘。所有路径和临时文件应保持安全权限和原子 rename。

## 5. 当前环境变量与默认值

| 变量 | 用途/默认 |
|---|---|
| ONBOARDING_HOST | 控制台监听地址，默认 127.0.0.1；0.0.0.0/:: 仅可信 LAN |
| ONBOARDING_PORT | PM2 配置默认 4399；console-server 通过 --port 或参数读取 |
| ONBOARDING_OUTPUT_ROOT | 默认 tmp/chatgpt-onboarding-console |
| ONBOARDING_PROTOCOL_SCRIPT | 替换协议入口，测试使用 mock-protocol-login.mjs |
| CHATGPT_BASE | 默认 https://chatgpt.com |
| AUTH_BASE | 默认 https://auth.openai.com |
| CHATGPT_PROXY_URL | HTTP/HTTPS/SOCKS5/SOCKS5h 账号代理 |
| TOSUB2_TLS_PROFILE | 默认 chrome146；auto 启用 profile 探测 |
| TOSUB2_PYTHON | 旧版本 TLS worker 的 Python 解释器 |
| CHATGPT_PROXY_MAX_ATTEMPTS | 代理会话额度，默认 10 |
| CHATGPT_SAME_PROXY_RISK_RETRY_DELAY_MS | 同代理风险重试间隔，默认 1000 ms |
| PROXY_CONNECTION_RETRY_BASE_MS | 控制台连接失败退避基数，默认 1000 ms |
| SMS_POLL_INTERVAL_MS | 短信轮询间隔，默认 3000 ms |
| SMS_POLL_TIMEOUT_MS | 短信轮询超时，默认 10 分钟 |
| LUBAN_SMS_POLL_INTERVAL_MS / LUBAN_SMS_POLL_TIMEOUT_MS | SMS_POLL 未配置时的旧兼容变量 |
| LUBAN_SMS_API_BASE | LubanSMS API 覆盖地址 |
| SMSBOWER_API_BASE | SMSBower API 覆盖地址 |
| SUB2API_MONITOR_INTERVAL_MS | 监控间隔，默认 5 分钟，最小 1000 ms |
| SUB2API_AUTO_REPAIR_COOLDOWN_MS | 自动修复冷却，默认 5 分钟 |
| CHATGPT_LOGIN_PASSWORD | 协议登录密码，仅进程环境传递，不日志 |
| CHATGPT_TOTP_SECRET | 协议登录 TOTP 密钥，仅进程环境传递，不日志 |
| CHATGPT_NEW_PASSWORD | 添加密码模式的新强密码 |
| TOSUB2_MAC_CREDENTIAL_ROOT | 测试或迁移时覆盖 macOS 凭据根目录 |
| LOCALAPPDATA | Windows 默认凭据根目录基路径 |
| TOSUB2_POWERSHELL | Windows DPAPI 使用的 PowerShell 命令 |
| NODE_ENV | 测试时允许 Sentinel token fixture |
| TOSUB2_TEST_SENTINEL_TOKEN / TOSUB2_TEST_SENTINEL_SO_TOKEN | 仅 NODE_ENV=test 的 Sentinel mock |
| PYTHONDONTWRITEBYTECODE / PYTHONIOENCODING | 仅旧测试子进程环境 |

命令行还兼容 --host、--port、--email、--phone、--proxy、--tls-profile、--output-mode、--checkpoint、--resume-checkpoint、--refresh-sub2api、--setup-totp、--add-password、--native-http、--chatgpt-base、--auth-base、--codex-client-id、--codex-redirect-uri、--sub2api-out 等参数。

## 6. 当前代理和 TLS 行为

代理接受 http、https、socks5、socks5h。用户名中含 sid-字段或密码符合 Kookeey 的 JP-8位-时长格式时支持随机 session rotation；固定代理只检测一次，风险失败后不伪造轮换。代理请求使用 curl_cffi impersonate profile，默认 chrome146；profile 不支持时按 chrome145、chrome142、chrome136 降级，并把实际 profile 与浏览器 UA/Client Hints/平台资料同步。直连可用 auto 探测实际可访问 ChatGPT 且不含 challenge 的 desktop Chrome profile。对代理本地地址关闭 TLS verify，远端默认校验。

每次协议请求 redirect=manual，显式处理 301/302/303/307/308 Location；Set-Cookie 同时从 rawSetCookie 和 worker cookie jar 合并。请求错误分为业务 JSON、Cloudflare challenge、连接失败；仅安全校验或连接失败进入风险重试。当前同代理风险最多 3 次，然后控制台重启任务；可轮换代理最多计数 10，纯连接失败最多 20。Go transport 必须保留这些语义，并让 context 超时取消连接。

## 7. Cloudflare、Turnstile、Sentinel 行为

Cloudflare solver 先识别 403 或带 cf-mitigated/x-cf-mitigated 的 challenge HTML，再在当前 curl_cffi session、同一代理、UA、TLS profile 和 Cookie 中执行父页面脚本。Turnstile 通过 child iframe 和 postMessage 事件桥处理，取得 cf_clearance 后重放原请求并检查 clearance 是否属于同一域。若无法完成，进入同代理重试或代理 session rotation。

Sentinel 每次 flow 实时请求 sentinel.openai.com loader，解析版本化 SDK URL，下载当前 SDK，在 jsdom 父页和 iframe 内运行。runtime 注入 navigator、screen、performance、crypto.getRandomValues、TextEncoder、atob/btoa、XHR、fetch、Headers、Blob、MutationObserver、定时器和 postMessage；生成 OpenAI-Sentinel-Token，尽可能生成 Session Observer Token。device ID、Cookie、UA、语言、代理和 TLS profile 与协议 session 复用，session 变化后销毁 runtime。

这套实现当前生产依赖 Python curl_cffi、Node.js 和 jsdom，并不能满足纯 Go 定义。Go 迁移已让 Chromium 直接完成 challenge、iframe、动态 SDK 和 token 尝试；普通 HTTP 请求若使用 challenge 绑定的 Cookie/设备状态，也应改由同一 BrowserTransport 发出。若临时保留 jsdom 仅作为迁移测试工具，必须在状态文档明确为未完成，不能宣称生产纯 Go。

## 8. 当前凭据存储行为

macOS：Keychain generic password service com.local.chatgpt-onboarding.credentials，account 为小写 email；随机 32 字节密钥存 Keychain，凭据 JSON 用 AES-256-GCM 写入 Application Support/toSub2/credentials/<sha256>.enc。写入采用临时文件后 rename，并在解密失败时返回空凭据。

Windows：PowerShell System.Security.Cryptography.ProtectedData，DataProtectionScope.CurrentUser，固定 entropy toSub2.credentials.v1；密文写 %LOCALAPPDATA%/toSub2/credentials/<sha256>.dpapi，权限 0600。PowerShell 不可用时明确报错。

Linux：当前不保存 password、TOTP 或 proxy，创建任务仍可运行邮箱手动流程，但重启后需要重新输入缺失资料；不得降级成明文文件。Go 端推荐 Secret Service/libsecret，找不到安全服务则返回明确错误，并让排队任务进入 reauth_required。

日志脱敏当前覆盖 URL query 的 code/token/state/csrf/nonce/otp 等，以及 access_token、refresh_token、id_token、password、totp_secret、2fa_key；短信错误会隐藏 API key、URL。Go 端还必须覆盖 Cookie、手机号验证码、代理密码和 x-api-key。

## 9. 当前测试覆盖范围

| 测试 | 覆盖 |
|---|---|
| test/tls-transport.mjs | mock HTTP、manual response、profile fallback、sid/session 轮换、风险重试、连接失败、Cloudflare worker/session、clearance 域隔离、Sentinel worker、profile probe、browser identity |
| test/runtime-process.mjs | Cloudflare/Turnstile/Sentinel 本地 Node runtime 子进程、stdin/stdout、取消和错误清理 |
| test/credential-store.mjs | macOS Keychain mock、AES 文件、Windows DPAPI mock、不可安全持久化时错误 |
| test/sms-providers.mjs | SMSBower 价格/取号/状态、Luban 码提取、custom pool、邮箱 OTP 解析 |
| test/protocol-smoke.mjs | mock OAuth、PKCE、checkpoint、手机绑定、refresh token、密码/TOTP 输出 |
| test/password-mfa-workspace-smoke.mjs | 密码登录、邮箱 OTP、MFA TOTP、workspace、profile/security 分支、TOTP setup |
| test/console-smoke.mjs | 控制台全链路、分页/筛选、批量下载/删除/导出、refresh、TOTP、密码、手机、Sub2API 上传/监控 |
| test/console-risk-retry.mjs | proxy risk race、取消/删除/shutdown 失效、direct TLS fallback、重启竞态 |
| test/console-job-lock-smoke.mjs | 同邮箱并发创建/更新、重试、取消、relogin 互斥 |
| test/console-restore-shutdown-smoke.mjs | 输出/checkpoint 恢复、密码/TOTP 结果恢复、Sub2API 请求取消、元数据落盘 |
| test/sms-provider-console-smoke.mjs | 控制台 Luban/SMSBower/custom 取号、轮询、重发、释放、完成 |
| test/protocol-smoke mocks | mock-protocol-login、mock-add-phone-page-server、mock-auth-expiry-server、mock-password-mfa-server、mock-queue-protocol |
| test/console-risk-retry mocks | 不依赖真实服务器，使用 vm 抽取控制台函数和假 child/probe |

用户要求的 Go 测试还必须新增配置解析、Cookie Jar、redirect/Set-Cookie、OAuth PKCE、TOTP、邮箱 OTP、三 provider、队列/20 并发/邮箱锁、取消超时、关闭、checkpoint、JSON golden、Sub2API、代理轮换、TLS fallback、Chromium challenge、Sentinel SDK、浏览器/transport Cookie 同步、Keychain/DPAPI、脱敏及 JS smoke 对应回归。

## 10. JS/Python 到 Go 模块映射

| 旧文件 | Go 包 |
|---|---|
| console-server.mjs | cmd/tosub2、internal/console、jobs、store、sub2api |
| protocol-login.mjs | internal/protocol |
| tls-transport.mjs、tls_transport.py | internal/transport |
| cloudflare-ctf/*.py、*.cjs、sentinel-sdk.js | internal/browser、internal/challenge |
| credential-store.mjs | internal/credentials |
| mail-otp.mjs | internal/mail |
| sms-providers.mjs、luban-sms.mjs、smsbower.mjs、custom-sms.mjs | internal/providers |
| web/src/main.jsx、styles.css | web/dist，internal/console 用 embed.FS |
| test/*.mjs、runtime-process.py | 各 Go 包 *_test.go、integration/browser_test.go |

建议第三方依赖：github.com/bogdanfinn/tls-client；必要时 github.com/refraction-networking/utls；github.com/go-rod/rod 或 github.com/chromedp/chromedp；macOS Keychain/Windows DPAPI/Secret Service 对应 Go bindings；标准库 net/http、crypto、context、embed、encoding/json 优先。不得引入 Node driver 的 Playwright-Go 作为生产运行时。

## 11. 迁移风险和兼容策略

1. ChatGPT/auth API 是上游动态接口，保留手动 redirect、Set-Cookie、header 顺序和 auth flow invocation ID；每个协议步骤写结构化 checkpoint。
2. 现有控制台通过子进程 stdout 标记驱动状态，Go 端应改为 JobEvent，不改变对前端暴露的 status/prompt 字段。
3. 当前任务目录文件名、version=1 JSON 和字段名是兼容边界；先读取旧文件再以原子方式写新文件，未知字段读取时保留。
4. 旧 sub2api-monitor.json 可能含明文 adminApiKey；Go 启动时只在 0600 文件中读取一次并迁移到 CredentialStore，API 返回永不暴露。
5. Linux 旧行为是不持久化敏感凭据；迁移不可偷偷写明文。缺失凭据的 queued job 进入 reauth_required。
6. 当前 server 启动 Vite middleware；生产 Go 必须在构建阶段生成 web/dist，运行时只使用 embed.FS。Node/npm 仅允许出现在开发构建环境。
7. Cloudflare/Turnstile/Sentinel 依赖真实浏览器环境、同代理和同 Cookie；不能把 jsdom token 或 clearance 跨 session 搬运。
8. 20 个协议任务不等于 20 个 Chromium；BrowserSession 要求代理 session 对应 context，并限制 Chromium 实例数和临时目录清理。
9. 同邮箱锁、FIFO、cancel/retry/relogin 与 proxy-risk restart 存在竞态；Go 测试需使用 -race 和 fake clock/transport。
10. 真实账号、邮箱 API、短信 provider、Sub2API 和 Cloudflare 不应进入 CI；真实流程需手工授权并在报告标记。

## 12. 普通 Go HTTP 与 Chromium 边界

适合普通 Go HTTP/transport：

- 邮箱 API 拉取和 OTP 提取。
- LubanSMS、SMSBower、自定义号码池。
- Sub2API groups/options/upload/monitor。
- 本地 console API、SSE/轮询、下载和静态资源。
- 已验证登录上下文中的普通 JSON 步骤、token exchange/refresh、checkpoint、Cookie Jar。
- 普通 HTTP redirect/Set-Cookie、代理解析、FIFO/job/store/credentials。

必须用 Chromium BrowserSession：

- Cloudflare challenge 页面、Turnstile iframe、MutationObserver/postMessage。
- Sentinel loader、版本化 SDK、动态 token 和 Session Observer Token。
- 需要 navigator/screen/performance/iframe/真实浏览器 Cookie 绑定的 auth/profile/security 检查。
- challenge 通过后绑定 clearance 的敏感请求，除非真实验证证明可安全同步到 transport。

建议 BrowserSession 接口包含 Navigate、Request、Cookies、SetCookies、DeviceID、UserAgent、Proxy、TLSProfile、Close；代理 sid/session 变化必须销毁旧 context，不能复用旧临时目录。

## 13. Golden fixtures 和基线

已建立脱敏 fixtures：

- docs/fixtures/go-port-golden/sub2api-import-oauth.json
- docs/fixtures/go-port-golden/login-checkpoint.json
- docs/fixtures/go-port-golden/job-meta.json
- docs/fixtures/go-port-golden/totp-setup-result.json
- docs/fixtures/go-port-golden/password-add-result.json
- docs/fixtures/go-port-golden/sub2api-monitor.json
- docs/fixtures/go-port-golden/README.md

fixture 值均为 synthetic placeholder；Go 测试应验证结构、必需字段、敏感字段不进入 job-meta/公开 API、读写 round-trip 和兼容未知字段。不得将真实 token、Cookie、密码、TOTP、API key 或代理密码替换进 fixture。

原项目审计结论：Node 主程序功能覆盖面较完整，但 TLS 指纹、Cloudflare/Turnstile、Sentinel 和凭据调用明确依赖 Python/Node/jsdom；原版本不是纯 Go。以上 1–12 节描述原版基线及迁移设计，不应把原版重试策略或建议依赖误读为当前 Go 实现。

## 14. 实施复核与兼容差异

Go 实现位于独立的 `toSub2-go-port` 目录；42 个原项目基线文件重新校验 SHA-256 均未改变，旧入口仍保留。生产只启动 Go 主程序、系统 Chrome 和 macOS `security` 系统工具；Windows 凭据直接调用 DPAPI，没有 PowerShell、Node driver 或 Python worker。浏览器采用纯 Go CDP 实现，未引入初始建议的 rod/chromedp。

- 原 checkpoint 阶段仍可解析；生产续行须符合浏览器身份校验。未绑定原生浏览器的旧待密码/邮件/MFA/手机/短信阶段不能无缝迁移，保留文件并要求显式重新登录。已绑定浏览器、email_verified、callback_ready 与 tokens_received 的分阶段处理见第17节及完整性复核。增加 `web_continue` 等网页登录阶段，保存已消费的邮箱码/密码/MFA 后继 URL，通过身份校验后恢复不重复提交已消费步骤。只有明确的会话失效响应才允许原身份下清理认证 Cookie 并重登一次；网络/取消/拒绝/风控不触发。OAuth code 交换、TOTP enrollment 和添加密码不纳入整体重放。
- Cookie 改为标准作用域校验：拒绝不匹配域、公共后缀和不合法的 `__Host-`/`__Secure-`；保留默认 path、Max-Age 优先级、到期删除和最长 path 顺序。跨源重定向移除来源绑定的敏感 header。
- BrowserTransport 使用实际 Chromium 的 WHATWG URL 规范化后匹配拦截；保留原始 30x/Set-Cookie，敏感请求留在同一代理、设备和浏览器会话。确认会话过期后的 reset 清认证 Cookie 与旧页面 SDK，保留设备 ID、代理和浏览器身份。
- 任务更新先等待旧 session 取消；批量请求整批预校验；删除等待任务清理后移除同邮箱历史与私有输出。启动恢复中的系统凭据 Load/Save 均接收取消 context。
- 原始直接 JSON Keychain 记录先读取再合并保存；显式清空字段不会因兼容迁移重新出现。空凭据新任务不创建系统凭据条目，Linux 继续不明文持久化敏感资料。
- 邮件/短信时间以消息接收时间确定，过期时间、时间戳小数、ID 和 HTML 属性不视为验证码。供应商终止态停止自动轮询，仍保留手动输入与订单完成/换号操作。
- React 仅调整敏感 TOTP 结果读取接口，localStorage 和主要交互保留；实际 Chrome 已验证创建、分页、筛选、跨页选择、日志与批量删除。

最终普通测试、race、vet、真实 Chrome/Keychain 全仓 race 的机器记录见 `go-port-verification.json`；发布二进制干净 PATH 启动及四平台产物见 `go-port-binary-verification.json`。本地测试通过不等于真实授权账号、Cloudflare/Turnstile/Sentinel 上游、短信服务、Sub2API 或 Windows DPAPI 实机通过，逐项边界见 `go-port-acceptance.md`。

## 15. 本轮逐分支补齐（2026-09-26）

以下是实际代码修改，而非重新规划；最终无缓存运行结果以机器报告为准。

- 生产 CLI/runner 由真实 Chromium 与本地严格协议串联，修复重复等待事件导致自动邮箱 stdin EOF、手动输入提前消耗下一验证码的问题。邮箱发送/重发之前采样 baseline，按邮件身份而非六位码去重；单次 fetch 60 秒，总轮询超时保留人工输入，r 重发重新启用自动轮询；邮箱状态通过显式白名单事件更新。
- CLI 的 --checkpoint 只指定输出，不隐式恢复旧文件；--resume-checkpoint 才恢复。--web-only 不把旧 Cookie/checkpoint 直接作为认证成功，而是重新完成 Web 登录。refresh/regenerate/reauthorize 不读取无关 checkpoint；令牌已轮换后，缺失、opaque 或损坏的 id_token 不阻止安全保存新 refresh token。
- TOTP 激活前持久保存待确认种子，后续确认接口失败不重放成功激活；添加密码中断后，仅本次服务端确实验证通过的同一候选密码可恢复成功。网页 workspace、about_you 外部回调、手机 channel/invalid_state、每次逻辑请求 UUID 与 fetch metadata 已做严格旧协议回归。
- SMSBower ready 在服务端接受号码后 best-effort 提交；短信 timeout/重复码保留有效号码的人工、重发、换号与完成操作。Sub2API 上传先校验完整选择、去重并保留旧计数；监控恢复只重传每邮箱最新任务尚未成功的远端 ID。
- BrowserTransport 使用 CDP 接管完整认证 headers/body。改写失败即取消原导航请求，不能向登录端点释放未改写的 GET。协议临时页面绕过 cache/service worker，原 SDK 页面完整保留；OOPIF、分区 Cookie、设备绑定、超时 fetch 取消、代理更换后的进程/目录清理已有真实 Chrome 回归。
- HTTPS CONNECT 使用可取消的 Go DialContext，保持证书验证；SOCKS5 与 SOCKS5h 分别使用本地与远端 DNS。--native-http 恢复旧显式诊断模式，声明 toSub2-Go/1.0，不冒充 Chrome 指纹，也不是 tls-client 失败后的隐式回退。同代理浏览器验证重试的次数/延迟与连接重试分别配置，不借挑战失败切换身份。
- macOS 的 TOSUB2_MAC_CREDENTIAL_ROOT 同时控制新写入与读取，仍能读取早期 Go OutputRoot/credentials 中的密文。Windows 新文件采用 AES-256-GCM，随机 256 位密钥由 DPAPI CurrentUser 包装；旧 Node/早期 Go 的 base64 DPAPI 文件继续可读，下一次保存才写新 envelope。新格式不保证旧 Node 能反向读取。
- 新 internal/privatefile 在写入内容前建立 POSIX 0600/0700 或 Windows 当前用户 protected DACL；Windows 不支持持久 ACL 时安全拒绝。显式应用数据根可以收紧；向既有 Downloads/tmp 等父目录导出时不修改父目录权限，只私有创建缺失目录和输出文件。
- Windows 原生 SCM 处理 StartPending/Running/StopPending、Stop/Shutdown/PreShutdown、共同关闭 deadline 和失败退出码；已有 systemd、launchd、当前用户 Task Scheduler 以及新的 SCM 安装脚本。没有安装系统服务，没有把交叉编译当作 Windows 运行验收。

新增/明确的配置：TOSUB2_SENTINEL_SDK_URL 与 --sentinel-sdk-url 指定可信动态 loader；CHATGPT_SAME_PROXY_RISK_RETRY_DELAY_MS 控制原浏览器同代理验证等待，CHATGPT_SAME_PROXY_CONNECTION_RETRY_DELAY_MS 控制连接失败退避；CHATGPT_SAME_PROXY_RISK_RETRIES 默认 3。TOSUB2_BROWSER_TEST、TOSUB2_KEYCHAIN_TEST 与 TOSUB2_LIVE_BROWSER_TEST 分别控制本地浏览器、真实 Keychain、公开上游验收，后者与本地 fixture 分开记录。

公开上游按实际环境分别记录：此前两次无头 Chrome 尝试得到 HTTP 403、Cf-Mitigated: challenge；修复验证页前置识别后仍超时返回 SENTINEL_PAGE_CHALLENGE。最终冻结代码随后设置 TOSUB2_BROWSER_HEADLESS=0，在干净可视 Chrome 中通过公开真实动态 SDK 测试，生成 Sentinel Token 及 available 状态的 Session Observer Token，测试耗时 6.32 秒（2026-09-26 07:40:47 UTC 记录完成）。三次原始结果均保留于 go-port-verification.json。没有关闭 CSP、自动点击或提交账号资料。该成功证明可视浏览器的公开 SDK 执行，不证明无头兼容、认证端点接受、完整账号流程或全部 Cloudflare/Turnstile 分支。

### 零值、退避与探测预算复核

- 监控间隔和自动修复冷却的旧默认值均为 5 分钟。间隔至少 1000ms；显式冷却 0 禁用冷却，包括恢复的旧计时、近期失败记录和远端更新重传。环境变量保留旧 Number 转换语义，公开分钟数继续兼容旧前端取整显示。
- CHATGPT_PROXY_MAX_ATTEMPTS 或 --proxy-max-attempts 显式为 0 时，可轮换代理在联网前返回额度耗尽，固定代理和直连仍可连接；不将显式 0 偷换成默认 10。
- SMS_POLL_TIMEOUT_MS 和兼容 LubanSMS 变量的显式 0/负值立即暂停自动收码，保留人工输入、重发、换号及订单完成。轮询间隔 0/负值按旧 Node 定时器的 1ms 最小间隔处理。HTTP 回归同时修复迟到 sms_otp 事件使已提交验证码重复投递的竞态。
- PROXY_CONNECTION_RETRY_BASE_MS 恢复默认 1000ms、最小 1ms 的指数退避，最长 15 秒，连续连接失败最多 20 次；显式 CHATGPT_SAME_PROXY_CONNECTION_RETRY_DELAY_MS（含 0）覆盖基数。协议只重试 GET/HEAD，不重放 OAuth grant 和其他写操作；配置允许的 0–20 次重试不再被客户端暗中截到 3 次。
- TLS profile 探测每候选最多 15 秒、总预算 300 秒、最多 4 个并发候选，取消会清理未选客户端。候选独立 Cookie Jar，仅合并选中的结果；已有 Cookie 的调用保守串行。任何有效 HTTP 响应（包括 403/429）都固定该优先 profile，不因安全验证选择另一身份。生产 runner 不再用普通请求的 30 秒总超时包裹整个探测。
- Go 二进制的相对输出路径基于当前工作目录；不硬编码旧源码安装目录。便携或服务部署必须使用固定 WorkingDirectory 或显式绝对 --output-root；部署示例已采用固定目录。
- 旧 /api/jobs/:id/luban-number 路径强制使用 LubanSMS 及顶层 apiKey/serviceId，不让冲突的新版 provider/config 参数改变旧别名行为；供应商 options 查询只接受 POST，其他方法不会发出供应商请求。两项均有真实 HTTP 路由回归。

### Chromium 进程清理复核

Chromium 启动前先建立私有临时 profile。Unix 每个实例使用独立 PGID，取消、显式关闭和父进程异常退出都清理该进程组；Windows 使用 CREATE_SUSPENDED，加入 KILL_ON_JOB_CLOSE Job Object 后才恢复主线程，避免子进程先于归属登记运行。没有通过可执行文件名查杀用户其他 Chrome。Session.Close 和 Manager.Close 保留清理错误并幂等返回，生产 runner 将清理错误合并进任务结果，不能静默宣称目录已删除。

合成三级进程树验证取消、父退出和显式关闭，且同名独立进程继续运行；真实 Chrome 测试对父子关系、PGID 或本次私有 profile 可归属的后代断言退出。新出现但无法可靠归属的 detached crash handler 只作为观察信息，不能混入本实例清理成功的结论。Windows Job Object/DACL 仍只有交叉编译证据，实际行为须 Windows 运行。

### 冻结验收与文件兼容确认

最终无缓存普通/race 各 15 包通过（780 PASS 事件、395 顶层测试、49 SKIP）；vet 通过；启用真实 Chrome/Keychain 的全仓 race 为 830 PASS 事件、433 顶层测试、11 SKIP。公开 SDK 可视上游测试单独通过，不混入本地测试数量。测试和构建期间 259 个源码/fixture/静态资源文件哈希保持一致，独立二进制及四平台构建成功。参考项目的 42 个原始文件哈希未变。

生产路由与六类 JSON 最后复核无缺失接线。Go 新写根目录聚合 job-meta.json，恢复仍读取旧每任务目录格式；控制台 TOTP/password 结果为私有中断恢复文件，写入系统凭据成功后清理，不作为永久明文存储。Linux 无安全凭据服务时不写这些密码/TOTP 文件；已有凭据、API Key 和带认证信息的代理仍不进入普通 metadata。

## 16. 最后中断恢复复核

继续逐操作检查发现并修复两处接线：tokens_received恢复必须支持实际任务名full/resume；有refresh sidecar的失败任务点击重试必须保留refresh/regenerate/reauthorize，不能改成full再次登录。CLI不再预拷贝旧source进随机job输出，只有新结果成功后缓存，防止热发现把旧token当新授权。

native/tls-client请求不再被底层固定30秒截断；body失败仍保留已接受Cookie/删除；Chrome请求页收尾后读取真实cookie库。20任务/4浏览器池、默认五分钟monitor、Mac Keychain跨进程与真实Linux全仓已实测。源码冻结259文件，原42基线文件未变；新增Go阶段/sidecar不宣称旧JS双向兼容。Windows原生及真实授权账号验收仍缺环境。

### 取消与输入兼容补充

本次完整目标复核还修复了HTTP/2重拨请求取消丢失和正常EOF后的取消误报成功；Go native、fingerprint保持已接收Cookie，但不把已取消响应当成功。密码原文贯通HTTP/CLI/任务/协议；OTP与手机号trim。自动邮箱进度不再覆盖等待状态，CLI不抢读后续人工输入；任务输入按阶段原子接受并关闭提示。短信安全恢复失败会明确退出假忙，已消费输入的running事件不会触发取号/轮询重启。这些差异纳入新增回归，原实现参考文件不变。


## 17. 2026-09-27 源码行为复核

新增逐源行的修复与验证记录见 docs/go-port-full-parity-review.md，包括公开 API 响应、旧导出字段、邮件时间/Base64、短信服务标识、Sub2API 上传/超时、添加密码后状态与隔离凭据导入。该记录也明确区分真实实现缺口与尚未取得真实账号证据的边界；不能把下述历史测试统计视为当前全部验收。

浏览器生产实现为项目内 Go CDP 客户端加 gorilla/websocket，非 Node driver，也未实际选用早期建议中的 rod/chromedp。go:embed 静态页面继续保留原 React 交互；私有 TOTP 种子从受保护端点获取并在同阶段有界间隔重试。
