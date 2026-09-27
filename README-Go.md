# toSub2 Go

Go 主程序位于 `cmd/tosub2`。生产运行时只需要 Go 编译出的二进制和已安装的 Chromium/Chrome（网页登录认证从首个请求绑定同一浏览器会话）；不需要 Node.js、npm 或 Python。

迁移工作目录为 `/Users/ceng/Downloads/toSub2-go-port`；原项目 `/Users/ceng/Downloads/toSub2-main` 保留作为兼容基线。目标是保留旧项目全部功能、交互和数据行为。本轮逐源行的功能兼容修复见 docs/go-port-full-parity-review.md。当前已有可独立运行的 Go 版本；未绑定浏览器身份的旧待验证 checkpoint 仍需显式重新登录，真实授权账号、真实 SMS/Sub2API、上游 challenge 和 Windows DPAPI 实机验收仍未完成，因此不能宣称全部功能已通过。

**本项目可以在 macOS 原生运行，也可以在本机 Docker 的 Linux 容器运行，并非仅支持 Windows。** Windows 待验收的是 DPAPI、DACL、Job Object 和 Windows Service 等平台专有功能，不影响启动 macOS/Linux 版本。2026-09-26 的独立 macOS 启动复验已通过，见 `docs/go-port-macos-local-smoke.json`。

本轮最终测试和部署结果见 docs/go-port-full-parity-review.md 与 docs/go-port-full-parity-review.json；源码、运行二进制及静态资源均已核对一致。当前页面若保留了输入，请先保留内容，再刷新以加载新前端。

## 本机原生运行与清理

当前本机测试使用 macOS 原生 Go 服务，地址为 `http://127.0.0.1:4400`。toSub2 的测试容器、专用数据卷、网络与镜像已移除。其他项目的 Docker 服务不受影响。

```sh
cd /Users/ceng/Downloads/toSub2-go-port
./scripts/local-macos.sh start
./scripts/local-macos.sh status
./scripts/local-macos.sh stop
./scripts/local-macos.sh restart
# 修改 Go 源码后显式重新编译并重启：
TOSUB2_LOCAL_REBUILD=1 ./scripts/local-macos.sh restart
# 永久删除本实例的运行目录、全部数据/归档和隔离 Keychain 条目：
./scripts/local-macos.sh clean --yes
```

所有运行文件集中在 `/Users/ceng/Library/Application Support/toSub2-local/6428e6fb3abbc4ae`；通用规则是 `$HOME/Library/Application Support/toSub2-local/<源项目规范路径SHA-256前16位>`。其中包括 `bin`、`data/credentials`、`archive`、`logs`、`tmp` 和内部 launchd plist。脚本只注册当前 GUI 会话，不复制登录自启动 plist 到 `~/Library/LaunchAgents`。

`start` 已运行时直接返回；`stop` 停止服务但保留数据；`restart` 等待卸载和进程退出后重新启动。首次缺少二进制时或设置 `TOSUB2_LOCAL_REBUILD=1` 才需要 Go 编译器，日常启动不需要 Go、Node、Python 或容器。Chrome 保持可视且启用 sandbox。

`clean --yes` 永久删除上述整个运行目录及本实例专属 Keychain 条目；停止或 Keychain 清理失败时保留目录。专属父目录 `toSub2-local` 仅在空时删除，其他项目存在则保留，不操作通用 `Application Support` 父级。源码、原项目 `/Users/ceng/Downloads/toSub2-main`、共享缓存、系统 Chrome 和旧全局 Keychain 均保留。如果连 Go 源目录也不再需要，应先成功 clean，再在 Finder 删除 `/Users/ceng/Downloads/toSub2-go-port`。完整目录、命名空间和清理边界见 `docs/local-macos.md`。

2026-09-26 的本机部署历史复验：全仓普通测试 28.579 秒、race 29.566 秒、vet 0.163 秒均通过；16 包、948 个 PASS 事件（含子测试）、55 个明确 SKIP。最终脚本的真实临时 Go 源码副本已通过启动、重启、无编译器 PATH 启动、两条隔离 Keychain 条目清理和重复 clean；当时的运行二进制与当时受测源码重建产物的 SHA-256 一致。263 个历史临时文件和 26 个目录已归入本 runtime 的 `archive/migration-temporary-artifacts`，随 clean 一并删除。完整记录见 `docs/go-port-local-runtime-verification.json`；这些验证不代表 Windows 或真实账号验收完成。

## 可选 Docker 构建（本机已停用）

```sh
docker compose build
docker compose up -d --wait
docker compose ps
```

打开 `http://127.0.0.1:4399`。端口占用时使用 `TOSUB2_DOCKER_PORT=4418 docker compose up -d --wait`。镜像包含 Go 二进制、嵌入前端和 Chromium，不含 Node.js/Python/Go 开发运行时；无需挂载原项目或真实凭据。Docker 构建只发送白名单中的 Go 源码和嵌入资源。

默认非 root、只读根文件系统、宿主 loopback 端口、私有持久数据卷、临时浏览器目录和 init 进程回收。`docker compose stop` 优雅停止，`docker compose start --wait` 再次启动，`docker compose down` 删除容器但保留数据卷。不要加 `--volumes`，除非确实要删除保存的任务和导出。

Chromium sandbox 默认开启。如果受控本机容器不支持所需 namespace，可显式使用 `TOSUB2_BROWSER_NO_SANDBOX=1 docker compose up -d --wait`；这会关闭浏览器自身 sandbox，不能当作已验证 sandbox 的生产配置。容器为无头模式，需要人在浏览器内操作的验证请使用 macOS 原生可视浏览器运行。Linux 容器仍采用内存凭据策略，重启后需重新提供敏感配置。完整说明见 `docs/go-deployment.md`。

构建后可用 `./scripts/test-container.sh` 在同一镜像运行真实 Chromium 本地集成测试；若显式关闭 sandbox，测试命令也要设置 `TOSUB2_BROWSER_NO_SANDBOX=1`。测试需要宿主 Go 工具链，但生产容器不需要。

本机 Docker Desktop 在2026-09-26实测默认sandbox启动失败，容器内 `unshare --user` 被拒绝；已使用上述显式 `TOSUB2_BROWSER_NO_SANDBOX=1` 配置部署测试。相同镜像的13项浏览器/React/恢复/本地OAuth集成测试通过，0跳过；HTTP、权限与重启也通过，见 `docs/go-port-container-verification.json`。这些结果不包含真实账号或Windows系统验收。原生macOS与容器同时运行时请使用不同端口。

## 安装和启动

```sh
go build -o tosub2 ./cmd/tosub2
./tosub2 --host=127.0.0.1 --port=4399 --output-root=tmp/chatgpt-onboarding-console
```

默认只监听 `127.0.0.1`。打开 `/` 后页面会请求公开的 `/api/bootstrap`，后续 API 使用返回的 token；也可以设置 `TOSUB2_CONSOLE_TOKEN` 或使用 `Authorization: Bearer TOKEN` / `X-Console-Token`。

React 页面在构建阶段生成到 `web/dist`，并复制到 `cmd/tosub2/web/dist` 由 `go:embed` 打包。前端源码和最小构建依赖清单保存在本项目 `web/`，执行 `./scripts/build-frontend.sh` 即可构建；这不会成为生产运行时依赖。

## 命令行账号流程

不提供账号操作参数时启动控制台。兼容的命令行入口包括：

```sh
./tosub2 --email you@example.com --output-mode sub2api
./tosub2 --resume-checkpoint private/login-checkpoint.json
./tosub2 --refresh-sub2api private/sub2api-import-oauth.json
./tosub2 --email you@example.com --setup-totp
./tosub2 --email you@example.com --add-password
```

TOTP 和添加密码的私有结果默认路径与旧版一致：tmp/chatgpt-totp-setup.json、tmp/chatgpt-password-add-result.json，可用显式参数覆盖。

只有显式 --resume-checkpoint 会恢复 CLI 旧会话；--checkpoint 只指定输出路径，--web-only 始终重新完成 Web 登录。

支持 `--output-mode both|session|sub2api`（默认 `both`）、`--out`、`--sub2api-out`、`--checkpoint`、`--totp-result`、`--password-add-result`、`--web-only`、`--no-sub2api-export`、`--workspace-id`，以及 Sub2API 名称、并发、优先级和倍率选项。使用 --help 查看完整参数。默认网页认证从首个请求使用 Go BrowserTransport 和 Chromium 原生 HTTP/TLS，避免已建立会话中途切换 UA/TLS；独立 refresh 和 cookie 无关的 callback code 交换使用 Go tls-client。TOSUB2_TLS_PROFILE 选择后者的真实打包 profile，不修改 Chromium ClientHello。显式 --native-http 保留旧版诊断开关，使用 Go 标准 HTTP/TLS、声明 toSub2-Go/1.0，且不携带 Chrome Client Hints。该模式不宣称具有 Chrome TLS 指纹，也不会在 tls-client 出错时自动启用；遇到必须使用 Sentinel/浏览器的步骤会明确停止，不能在已建立认证身份后悄悄切换。

密码、TOTP、新密码和邮箱 API 可分别通过 `CHATGPT_LOGIN_PASSWORD`、`CHATGPT_TOTP_SECRET`、`CHATGPT_NEW_PASSWORD`、`CHATGPT_MAIL_API_URL` 提供。手动验证码和手机号从标准输入读取，输入 `q` 取消；敏感输入不写日志。session、OAuth 和 checkpoint 是显式私有导出，包含恢复或导入所需的凭据，必须保管好输出目录。

密码输入保留首尾空白；邮箱/短信/TOTP验证码与手机号去除首尾空白。输入接受后任务显示working，重复提交返回409。自动邮箱轮询期间网页允许人工覆盖；CLI仅在明确人工提示出现时读取stdin，避免自动流程消耗后续密码/验证码。

## 已接受授权的中断恢复

OAuth code 成功换取 token 后，Go 先保存私有 checkpoint 的 `tokens_received` 阶段，再写最终导出。输出失败后继续原任务只做本地恢复，不再次消费 code，也不要求原代理、浏览器或凭据服务在线。login/full/resume 入口均覆盖；账号、origin、client、PKCE 和输出内容仍严格校验。

refresh token 轮换成功后先写稳定目标旁的 `.<文件名>.refresh-grant-recovery.json`。重启后重试或再次运行相同 CLI 命令会恢复此私有结果，不重复 grant；source/target 路径、源内容、目标原状态与首账号身份必须匹配。多账号 JSON 只刷新首项，其余账号、未知字段及大整数保留；source 与 target 不同时不改源文件。CLI 随机 job 目录仅缓存已成功写出的新结果，不提前放入旧 token。

这两个恢复机制是 Go 扩展：旧 JS 不支持 tokens_received 或 refresh sidecar，尚未完成恢复时请继续使用 Go。六类原数据文件及旧 v1 checkpoint 仍可解析，但解析成功不等于每个旧会话可以续行：未绑定原生浏览器、仍等待密码/邮箱/MFA/手机/短信的 checkpoint 保留原文件并返回 CHECKPOINT_SESSION_UNVERIFIED，要求显式重新登录；已绑定原生浏览器、email_verified、callback_ready 和 tokens_received 的分阶段处理见下文。若返回 `REFRESH_GRANT_UNSAVED`，服务器已接受 grant，但连恢复文件也无法持久化；不能保证恢复，程序不会自动重试或重新登录。先解决存储故障，再评估明确授权的新登录。

## 配置

兼容 `ONBOARDING_HOST`、`ONBOARDING_PORT`、`ONBOARDING_OUTPUT_ROOT`、`TOSUB2_MAX_ACTIVE_JOBS`、`TOSUB2_TLS_PROFILE`、`CHATGPT_BASE`、`AUTH_BASE`、`CHATGPT_PROXY_URL`、`CHROME_PATH`、`TOSUB2_BROWSER_ENABLED`、`SUB2API_BASE_URL`、`SUB2API_ADMIN_API_KEY` 等环境变量，也支持对应命令行参数。支持 `http://`、`https://`、`socks5://` 和 `socks5h://` 代理；代理 sid/session 模板可通过 `{sid}` 或 `{session}` 轮换。

输出目录使用原子替换保存 `job-meta.json`、`login-checkpoint.json`、`sub2api-import-oauth.json`、TOTP/密码私有结果和 `sub2api-monitor.json`。普通任务元数据、日志和公开列表 API 不保存密码、TOTP、Cookie、验证码、OAuth token、代理密码或 Sub2API 管理员 API key。macOS 使用 Keychain，Windows 使用 AES-256-GCM 加密文件，并由 DPAPI CurrentUser 包装随机 256 位密钥；仍可读取旧 DPAPI 文件。应用私有文件和目录在创建时设当前用户专属 protected DACL，拒绝无持久 ACL 的文件系统；Linux 当前使用进程内凭据存储，不持久化密码、TOTP、代理密码、短信服务凭据或管理员 key，重启后需要重新提供。要求持久保存私有 TOTP/密码结果的操作在无安全存储时明确报错。

短信订单、待提交验证码、重试动作、自定义池游标和旧短信基线保存到独立的安全凭据条目。 安全条目缺失、锁定或无效时，界面明确停止自动收码并保留人工入口，不假显示仍在轮询。重启后恢复订单，用户继续对应 checkpoint 后才恢复轮询；若 phoneRetryRequired 已暂停，仍须用户明确重试、输入验证码或换号才推进，不因重启自动重复提交。停服保留未完成订单，不误报完成或释放。Sub2API 监控默认每 5 分钟运行，支持冷却、永久跳过和仅重传未完成的远端更新；管理员 key 同样不写监控 JSON。Linux 的这些敏感恢复资料只在当前进程有效。

## 浏览器与代理

浏览器通过纯 Go CDP 客户端启动系统 Chrome，默认最多 4 个隔离会话。`CHROME_PATH` 指向 Chrome 可执行文件；`TOSUB2_BROWSER_HEADLESS=0` 可显示浏览器供本人完成交互，默认开启 Chromium sandbox。实际版本、UA、平台和语言来自浏览器自身，不伪造 Chrome 版本。

Sentinel 使用真实 script loader 加载动态 SDK、iframe 和 Session Observer Token。TOSUB2_SENTINEL_SDK_URL 或 --sentinel-sdk-url 可指定可信 HTTPS loader（本地 fixture 仅允许 loopback HTTP），切换版本或失败后会重新加载。Session Observer 缺失/失败/空值会明确报告状态，不把缺失 Token 视为成功。需要浏览器的敏感请求使用同一 Chromium profile 内的 BrowserTransport，通过 CDP 捕获原始重定向和 Set-Cookie，原 SDK 页面保持存活。普通 Go tls-client 与 Chromium 的 TLS stack 不相同，不能把 Cookie 同步视为跨 TLS 会话验证通过。

当前由 Go 驱动登录协议状态机；网页登录在首次 home/CSRF/signin 请求之前确定 Chromium、代理和设备身份，后续 SDK 与认证请求使用同一上下文。邮箱 API、短信、Sub2API 继续使用独立 Go HTTP；refresh 成功时不启动 Chrome。BrowserTransport 用 CDP 发送方法、请求头及请求体，内部请求使用经验证没有浏览器窗口的 hidden target，不在标签栏逐个打开页面；供本人交互的验证页面保持可见。它不进行账号页面的自动填表或点击。原 JS 版本使用 Node/jsdom 和 Python curl_cffi；本次迁移使用真实 Chromium，因此传输路径存在明确差异。不会把浏览器绑定的 token 直接搬回另一套 Go TLS 会话并假定有效。

Chrome 必须支持 `Target.createTarget(hidden: true)`，并能通过 `Browser.getWindowForTarget` 确认该请求 target 没有窗口；仅接受参数不算验证通过。不支持时返回 `BROWSER_HIDDEN_TARGET_UNSUPPORTED`，先关闭探测 target，不回退成可见请求标签页。取消或请求结束会关闭内部 target；若无法确认关闭，只终止该任务拥有的浏览器会话。该能力在本机 Chrome 154.0.8037.58 上验证，不能据此推定所有旧版本支持。

验证观察区分「页面可复验」和「上游已接受」。当前主 frame、loader、URL 的 HTTP 响应和 DOM 状态仅允许在同一浏览器重放受保护请求，协议层继续检查其真实状态码。普通 403 拒绝页不会因 HTML 类型而进入挑战循环；有明确 challenge 证据时才等待本人验证。验证超时、取消或明确拒绝会结束本轮，不自动重新导航；普通瞬态故障仍使用有限重试。错误只含 HTTP 类别和 DOM 状态，不包含页面内容或 token。

HTTP/HTTPS 代理支持认证；SOCKS5/SOCKS5h 使用每会话 Go HTTP/CONNECT 桥，支持用户名/密码。SOCKS5 在本机解析 DNS，SOCKS5h 由上游解析。桥绑定 loopback、使用随机认证，并随浏览器关闭。

取消或异常退出时，Unix 清理本实例的独立进程组，Windows 通过专属 Job Object 管理进程树；不会按 Chrome 名称终止其他浏览器。临时 profile 在启动前建立私有权限，清理失败会返回明确任务错误。Windows 相关代码已交叉编译，仍需目标系统测试。

TOSUB2_MAC_CREDENTIAL_ROOT 同时控制 macOS 凭据新写入和读取，早期 Go 默认目录仍作为兼容读取来源。已有共享导出父目录不会被修改权限。

同代理瞬态验证故障最多额外重试 3 次，CHATGPT_SAME_PROXY_RISK_RETRY_DELAY_MS 控制间隔；验证仍未完成、超时、取消和明确拒绝不会触发自动重新导航。连接重试另由 CHATGPT_SAME_PROXY_CONNECTION_RETRY_DELAY_MS 控制，不因安全拒绝轮换认证身份。

连接失败沿用 PROXY_CONNECTION_RETRY_BASE_MS 的指数退避（默认 1000ms，最长 15 秒，累计最多 20 次）；显式 CHATGPT_SAME_PROXY_CONNECTION_RETRY_DELAY_MS 包括 0 可覆盖基数。TLS 探测每个候选 15 秒、总预算 300 秒、最多同时 4 个，普通请求超时不截断后续候选；HTTP 403/429 表明已收到 HTTP 响应，只有明确挑战标记才触发真实浏览器验证，普通拒绝或限流仍返回相应 HTTP 错误。CHATGPT_PROXY_MAX_ATTEMPTS=0 禁止可轮换代理启动新 session，仍允许固定代理和直连。

短信超时显式设为 0 会立即转人工输入，号码仍可重发、换号或完成；短信间隔 0 沿用旧定时器的 1ms 最小值。SUB2API_AUTO_REPAIR_COOLDOWN_MS=0 禁用修复冷却；未设置时默认 5 分钟。相对输出路径基于启动时工作目录，服务部署建议指定绝对 --output-root。

checkpoint 绑定实际代理 session、TLS profile、UA 和浏览器路径。已有 BrowserBound checkpoint 按记录身份继续恢复，不要求新增标志；身份不匹配报 CHECKPOINT_SESSION_MISMATCH。无原生浏览器绑定的旧 email_verified checkpoint 在实际 Chrome 中只读请求 /api/auth/session，验证同一账号、有效期和 accessToken 后再继续；callback_ready 只用 PKCE/state 经 Go 交换 code，不携带旧 Cookie；tokens_received 仍可离线完成。旧的待密码/验证码/手机号 checkpoint 缺少可验证的原生身份时返回 CHECKPOINT_SESSION_UNVERIFIED，文件保留，必须显式重新登录。这是尚未实现无缝恢复的兼容边界，不能算全部旧 checkpoint 已迁移成功。新的登录连接可以使用配置的代理池和有限重试；挑战请求保留当前代理并转入真实浏览器。不会将已有认证状态任意迁移到另一个代理身份。

systemd、launchd 和 Windows 原生 SCM 服务及当前用户 Task Scheduler 示例见 [Go 部署说明](docs/go-deployment.md)，它们替代 PM2，不需要 Node/Python。

## 批量导入与密码提示

旧 macOS 全局凭据库不会被本机隔离 namespace 自动读取。新增离线迁移入口为 scripts/local-macos.sh import-credentials；先 stop，再显式指定旧凭据目录和旧 job-meta.json 或每行邮箱文件，最后 start。默认保留目标已有记录、源只读，输出仅计数；详见 docs/local-macos.md。任务 metadata 恢复成功不能代替凭据迁移成功。

迁移目标是完整保留旧项目的功能和使用方式。旧代码会使用已经提供或安全保存的密码；进入邮箱验证码分支且配置邮箱 API 时会轮询取码。任务卡片要求密码可能是当前凭据缺失，也可能是服务拒绝了候选密码，必须结合导入、凭据读取和协议分支判断，不能仅凭出现输入框断定用户资料有问题。邮箱 API 不代表账号密码；旧代码中没有“缺少密码就强制转邮箱验证码”的通用回退。

浏览器窗口是迁移后的实现变化：旧版通过 Node/jsdom 与 Python 执行安全验证相关逻辑，Go 版按迁移要求使用真实 Chromium。普通任务的输入、队列和结果行为仍需逐项保持兼容。早期 Go 版本中途从 tls-client 接管到原生 Chromium，已有 Cookie 的 UA/TLS 连续性无法保证；本轮新登录已改为首个请求前绑定同一 Chromium。旧未绑定浏览器的待验证 checkpoint 仍受恢复限制；具体修复与测试边界见 docs/go-port-full-parity-review.md。

自动密码候选交给协议前，会在 checkpoint 保存后的输入队列中原子移除相同旧值。这样服务拒绝密码后会等待新输入，不再从队列重复提交旧密码；不同的新手动输入和安全存储中的凭据不会被此清理误删。只有验证成功才保存新的已验证密码。

带分隔符的结构化批量行按本次资料替换密码、TOTP 和邮箱取码配置，未提供的对应字段明确清空，避免重导入后误用旧凭据；只含邮箱的行继续复用已保存资料。POST 邮箱格式保留公共配置的请求 URL 并使用本行请求体。这与原版语义一致。不会把密码写进普通任务元数据、日志或公开任务接口；修复后的提示也不回显密码。

## 测试

额外慢请求回归：TOSUB2_SLOW_HTTP_TEST=1 go test -race ./cmd/tosub2 -run TestConsoleLongSub2APIResponseKeepsLegacyTimeout。它启动生产 runConsole，确实等待本地 Sub2API 65 秒；验证单次 120 秒预算没有被控制台提前截断。未显式开启时普通测试会报告 SKIP。

```sh
go test ./...
go test -race ./...
go vet ./...
```

`./scripts/verify.sh` 可依次执行无缓存普通测试、race、vet 和本机构建。`./scripts/build-cross.sh` 生成 macOS arm64/amd64、Linux amd64、Windows amd64 二进制到 `bin/`；交叉构建不等于在目标平台执行测试。在 Windows 当前用户会话执行 `./scripts/test-windows.ps1` 才会实际验证 DPAPI、全仓测试和原生构建；race 需要支持的 Windows C 工具链，脚本不会把缺失工具链当作成功跳过。

```sh
TOSUB2_BROWSER_TEST=1 CHROME_PATH="/path/to/chrome" go test -count=1 -race ./internal/browser ./internal/challenge ./internal/protocol ./cmd/tosub2
TOSUB2_KEYCHAIN_TEST=1 go test ./internal/credentials -run TestMacOSKeychainIntegration -v
```

公开上游独立检查（不提交账号）：TOSUB2_LIVE_BROWSER_TEST=1 TOSUB2_BROWSER_HEADLESS=0 CHROME_PATH="/path/to/chrome" go test -count=1 -race ./cmd/tosub2 -run TestLivePublicSentinelBrowserCompatibility -v。2026-09-26 07:40:47 UTC 的独立记录中，可视 Chrome 已实际通过：真实动态 Sentinel SDK 生成 Token，Session Observer 状态为 available；测试本身耗时 6.32 秒。没有提交账号或 OAuth grant、自动点击验证或修改 CSP。此前两次无头测试的 HTTP 403 / Cf-Mitigated: challenge 失败仍保留在报告中；可视测试成功不代表无头环境或认证端点已验证。

浏览器 React 验收入口：`TOSUB2_BROWSER_TEST=1 go test -count=1 -race ./cmd/tosub2 -run TestChromiumEmbeddedConsoleInteractions -v`。生产进程恢复验收：`go test -count=1 -race ./cmd/tosub2 -run TestConsoleProcess -v`，覆盖启动 token、27 条输出恢复、分页/邮箱筛选、下载、批量删除、重启和 SIGINT/SIGTERM；使用合成账号，并把认证地址限定为不可连接的本机端口，不会访问真实账号。

这些测试使用本地 HTTP/SOCKS fixture，覆盖真实 Chrome 的动态 loader/版本化 SDK/跨域 iframe、token/SO、手动重定向、Cookie 删除、认证代理和进程清理，不使用真实账号。`CHROME_PATH` 可省略以使用系统安装；未启用开关或没有 Chrome 时明确跳过，Chrome 启动/CDP/断言失败会使测试失败。真实 SDK 在可视浏览器的独立验证已通过；Cloudflare/Turnstile 各分支和完整账号流程仍需本人或明确授权环境验收。邮箱自动化支持发送前旧消息基线、相同六位码的新邮件、总轮询超时后人工接管和重发后恢复自动收码。

本机已实际验证 Chrome `154.0.8037.58`、macOS Keychain及跨进程凭据恢复；另已在Linux arm64实际运行Chromium149.0.7827.53全仓测试。最终无缓存普通测试、race 和 vet 全部通过；同时启用浏览器与 Keychain 的全仓 race 也通过。真实 Chromium 对嵌入的 React 完成单任务、24 条批量任务、20/5 分页、跨页选择 25 项、筛选、日志展开和批量删除，无浏览器控制台错误。另有真实 React 账号操作和实际单/批量下载测试，以及生产 CLI/BrowserTransport 的五条邮箱/MFA/workspace/OAuth 本地链。实际发布二进制另在仅有 `PATH=/usr/bin:/bin` 的干净环境启动，health、bootstrap、静态资源、API 鉴权和 SIGTERM 正常退出均通过。

历史 R2 冻结验收：普通和 race 各 15 包通过，780 个 PASS 事件（395 个顶层测试），49 个明确 SKIP；启用 Chrome/Keychain 的全仓 race 为 830 个 PASS 事件（433 个顶层测试），11 个 SKIP。公开 SDK 的可视上游测试另行记录，不混入本地 fixture 统计。源码与发布构建使用同一 SHA-256 清单绑定，见 docs/go-port-go-source-manifest.json。

可复核证据见 `docs/go-port-verification.json`、`docs/go-port-binary-verification.json` 和 `docs/go-port-legacy-baseline.json`（原版 20 项检查的首次与重试结果）。这些合成数据测试不替代真实账号、SMS、Sub2API 或 Windows DPAPI 实机验收；50 项功能与 29 项测试逐项状态见 `docs/go-port-acceptance.md`。

长预算测试：`TOSUB2_LONG_TRANSPORT_TEST=1 go test -count=1 -race ./internal/transport -run TestTransportConfiguredBudgetCanExceedThirtySeconds -v`。加浏览器开关可运行 `TestChromiumSOCKSRequestBudgetCanExceedThirtySeconds`，真实等待超过30秒。默认五分钟监控实测入口：`TOSUB2_LONG_MONITOR_TEST=1 go test -count=1 -race ./cmd/tosub2 -run TestConsoleDefaultFiveMinuteMonitorAndInflightShutdown -v`，真实等待五分钟后验证在途HTTP关闭；后端为本地fixture。不开开关明确SKIP。

Windows完整入口为 `./scripts/test-windows.ps1 -Browser`，可用 `-ChromePath` 指定浏览器；覆盖原生DPAPI跨进程恢复、Job Object、Chromium/React/CLI和全仓race，指定浏览器测试后缺Chrome会失败。

## 故障排查

- `TLS fingerprint transport unavailable`：先区分代理连接、DNS、超时、证书或TLS协议错误，不能据此认定所有TLS profile都不兼容。Docker内的 `127.0.0.1` 指向容器自身；若代理在Mac宿主机，改用 `http://host.docker.internal:实际端口`。macOS原生运行仍使用自己的loopback代理。HTTP403/429表示已经取得HTTP响应，不应作为更换TLS身份的理由。
  修改控制台代理后，对失败任务点击“重新登录并授权”，该操作会携带当前代理配置。2026-09-26已确认容器通过宿主代理可完成Go `auto` TLS探测并选中 `chrome_133`；这里只验证连通性，不代表账号或安全验证成功。诊断补丁及测试记录见 `docs/go-port-tls-proxy-verification.json`。
- `BROWSER_REQUIRED`：设置 `CHROME_PATH`，确认 Chromium 可由当前用户启动。
- “网站验证尚未完成”或 `browser verification pending`：本轮未观察到完成证据；可能是挑战仍显示、网络未完成或页面证据不可用，不能仅凭超时确定根因。可视模式下在程序的 Chrome 窗口亲自完成验证；超时后本轮结束，确认网络与页面后手动重试。容器无头模式没有可交互桌面，Mac使用 `./scripts/local-macos.sh start` 和 `http://127.0.0.1:4400`。当前运行目录和清理步骤见 `docs/local-macos.md`；Linux 进程内凭据不能仅靠元数据迁移。
- `browser verification rejected`：当前文档是普通 HTTP 拒绝响应，且没有可交互挑战。程序停止反复等待 token；需根据网站响应和授权状态排查，不能通过重复验证导航保证成功。历史失败任务保留当时提示，新版修复不会把历史结果改写成通过。
- `BROWSER_IDENTITY_MISMATCH`：请求 UA 与安装的 Chrome 不同；浏览器请求必须采用 `Session.Identity()` 的实际 UA，不应把 Go TLS profile 的 UA 强行套到 Chromium。
- `SENTINEL_PAGE_CHALLENGE`：登录文档仍是安全验证页，尚未加载正常登录页及其 SDK；使用同一真实浏览器完成网站要求的验证。不会通过关闭 CSP 或更换认证身份绕过。
- `Chromium JavaScript execution failed`：loader 可能被页面 CSP、网络或上游变化阻止；错误信息不会包含 SDK token 或页面中的敏感值。
- `SENTINEL_TOKEN_MISSING`：动态 Sentinel SDK 未在同一浏览器/代理会话中生成有效 token；不要把 Turnstile 或 `cf_clearance` 值当作 Sentinel token。
- `secure credential service failed`：macOS 检查 Keychain 权限，Windows 检查当前用户 DPAPI。Linux 当前没有持久化系统凭据服务实现，请重新提供进程内所需凭据；程序不会改写为明文文件。
- `CHECKPOINT_SESSION_MISMATCH`：恢复资料与当前代理或传输身份不一致；使用原安全凭据和 session，或显式重新登录创建新流程。
- Sub2API 监控没有启动：检查后端地址、管理员 key、目标号池权限和后端是否允许账号更新/清错。

完整审计和迁移差异见 `docs/go-port-audit.md` 与 `docs/go-port-migration-status.md`。

Go语言最低版本1.22.0已在Linux实际通过普通全仓；当前macOS运行Go1.22.0测试出现dyld缺少LC_UUID错误，保留失败记录。本机已通过的开发/发布工具链为Go1.27.1；平台CI使用stable，Linux最低版单独验证。Linux容器浏览器测试显式no-sandbox，不代表普通用户sandbox场景已验。

历史临时 launchd 安装的启停及输出恢复记录保留。本机最近一次部署发现 Downloads 下的 launchd 实例以 EX_CONFIG 78 退出且应用日志为空，因此当前脚本将可执行文件、工作目录和全部运行数据统一放在 `Application Support/toSub2-local`。当前 4400 已上线；直接从某路径执行二进制的成功不能替代该路径下的 launchd 启动验收。

Linux平台逐条命令、普通/race/浏览器/最低Go版本、空PATH二进制启动及容器清理记录见 `docs/go-port-linux-verification.json`。
