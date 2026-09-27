# Go 服务部署

生产部署使用 `tosub2` 二进制和系统 Chrome/Chromium。React 资源已嵌入，不需要部署 `node_modules`、Node.js、Python、Vite 或 PM2。当前本机按用户要求改为 macOS 原生测试，toSub2 容器、专用卷、网络和镜像已移除；历史容器验证记录仍保留。Linux systemd和Windows SCM尚未实机安装，未替换用户已有服务。

## 本机当前入口

当前 macOS 原生服务监听 `127.0.0.1:4400`，运行目录是 `/Users/ceng/Library/Application Support/toSub2-local/6428e6fb3abbc4ae`。所有应用运行文件及 Chrome 临时 profile 均在该目录内，运行时不依赖 Docker、Node 或 Python。

```sh
cd /Users/ceng/Downloads/toSub2-go-port
./scripts/local-macos.sh start
./scripts/local-macos.sh status
./scripts/local-macos.sh stop
./scripts/local-macos.sh restart
# 仅在需要重新编译时设置；已有二进制日常启停不需要 Go：
TOSUB2_LOCAL_REBUILD=1 ./scripts/local-macos.sh restart
# 不可恢复：删除本运行目录全部数据、归档与隔离 Keychain 条目：
./scripts/local-macos.sh clean --yes
```

`start` 对已运行实例直接返回；`stop` 只停止本实例并保留数据；`restart` 等待 launchd 注销和 PID 退出后才重新注册。默认端口可通过 `TOSUB2_LOCAL_PORT` 在下一次启动或重启时设置。首次缺二进制或显式请求 rebuild 时才执行 Go 构建，使用仓库现有 embed 资源，不运行前端构建。

通用运行路径为 `$HOME/Library/Application Support/toSub2-local/<源项目规范路径SHA-256前16位>`。脚本验证目录归属、所有者与关键链接状态，并只管理本项目摘要对应的 launchd 标签和 Keychain service。plist 保存在运行目录内，仅注册当前 GUI 会话；当前部署没有复制 `~/Library/LaunchAgents` 登录自启动配置。

`clean --yes` 会停止本实例，删除其隔离 Keychain 条目及整个运行目录（`data`、`archive`、`bin`、`logs`、`tmp` 和内部 plist）。失败时保留运行目录；专属父目录 `toSub2-local` 仅为空时删除，不删除通用 `Application Support` 或其他 runtime。源码、原项目 `/Users/ceng/Downloads/toSub2-main`、共享缓存、系统 Chrome 和旧全局 Keychain 保留。若还要移除 `/Users/ceng/Downloads/toSub2-go-port` 源目录，先在原路径成功 clean，再用 Finder 删除源目录。详见 `docs/local-macos.md`。

当前 runtime 的 `archive/migration-temporary-artifacts` 已收纳 263 个历史临时文件和 26 个目录，随上述 clean 一并删除。最终脚本已用真实 Go 源码临时副本通过启停、重启、无 Go 编译器 PATH 启动、两条隔离 Keychain 条目删除和重复 clean；记录见 `go-port-local-runtime-verification.json`。Downloads 下曾确认 launchd EX_CONFIG 78 和空应用日志，未证明 TCC 原因；当前使用 Application Support 路径。

## 构建

```sh
# 仅当修改了 React 源码时需要 Node 和 pnpm；锁文件不应在构建中变化。
./scripts/build-frontend.sh
./scripts/build.sh
```

Node 或 pnpm 不在 PATH 时，用 `NODE_BIN=/absolute/path/to/node PNPM=/absolute/path/to/pnpm ./scripts/build-frontend.sh`。脚本在安装前设置 Node 的 PATH，使用 `pnpm install --frozen-lockfile --ignore-scripts`，然后构建并复制 embed 资源。

直接构建现有已提交静态资源：`go build -trimpath -o bin/tosub2 ./cmd/tosub2`。运行 `./bin/tosub2 --host=127.0.0.1 --port=4399 --output-root=/absolute/path/to/data`。

始终使用同一操作系统用户运行服务与控制台，确保 Keychain/DPAPI 用户范围一致。系统凭据服务不可用时程序明确失败，不将密码、TOTP 或代理密码降级存为明文。数据目录保持私有并纳入用户自己的备份。

## 可选 Docker Desktop 部署（本机已移除）

Docker Desktop 在 macOS 上运行 Linux 容器；它与 macOS 原生运行是两种已提供的启动方式，均不需要 Windows。根目录 `Dockerfile` 多阶段构建 Go，生产阶段安装 Chromium 和字体；`compose.yaml` 只将 `4399` 映射到宿主 `127.0.0.1`。

```sh
docker compose build
docker compose up -d --wait
docker compose ps
docker compose logs --tail=50 console
```

网页地址为 `http://127.0.0.1:4399`，可用 `TOSUB2_DOCKER_PORT` 修改宿主端口。容器内必须监听 `0.0.0.0:4399` 才能接收端口映射；应用仍输出非 loopback 监听提示，但宿主只发布 loopback，不是公网部署。不要自行改成 `4399:4399` 向所有网卡发布。

**连接Mac宿主机上的代理时，在控制台填写 `http://host.docker.internal:端口`，不要填写 `http://127.0.0.1:端口`。** 后者在Go服务容器内指向容器本身。例如宿主代理端口为7897，Docker控制台应填 `http://host.docker.internal:7897`；macOS原生启动则仍可使用 `http://127.0.0.1:7897`。协议前缀应与代理实际提供的HTTP或SOCKS服务一致；不要为连接问题关闭TLS证书验证。程序不自动改写代理身份。

运行用户为 UID/GID `10001`。命名卷 `tosub2-go_console-data` 挂载 `/data`，保存任务、checkpoint 和明确的私有输出；临时浏览器 profile 位于内存 `/tmp`。镜像根文件系统只读，删除所有附加 capabilities，禁止提权；init 负责回收 Chromium 子进程，`/dev/shm` 为 512 MiB，SIGTERM 最多等待 30 秒优雅关闭。自定义 bind mount 必须预先允许 UID 10001 以私有权限写入，程序不会静默改用不安全目录。

```sh
docker compose stop
docker compose start --wait
docker compose down
```

以上命令保留数据卷；不要加 `--volumes`，除非要永久删除其中数据。该卷并不能让 Linux 获得 macOS Keychain：密码、TOTP、代理密码、短信供应商凭据和 Sub2API 管理员 key 仍仅在进程内，重启后需重新提供。

默认保持 Chromium sandbox。某些容器内核/安全配置不允许 Chromium 建立 namespace，会明确启动失败；仅在受控测试环境可显式执行：

```sh
TOSUB2_BROWSER_NO_SANDBOX=1 docker compose up -d --wait
TOSUB2_BROWSER_NO_SANDBOX=1 ./scripts/test-container.sh
```

此选项只关闭 Chromium 自身 sandbox，容器非 root、只读文件系统和宿主 loopback 限制仍保留；不能将其验收结果称为 sandbox 已验证。默认无头浏览器没有远程桌面，遇到需要人工点击的站点验证请在 macOS 原生环境使用 `TOSUB2_BROWSER_HEADLESS=0` 完成，容器不会自动绕过验证。

### 在 Mac 上显示验证窗口

没有桌面的容器只设置 `TOSUB2_BROWSER_HEADLESS=0` 仍无法显示窗口。当前本机入口为 `./scripts/local-macos.sh start`，使用系统 Chrome、可视模式和开启的 Chromium sandbox。当前地址为 `http://127.0.0.1:4400`，数据路径与生命周期命令见本文“本机当前入口”。先前 `run-macos-visible.sh` 与 `com.tosub2.console.visible` 是历史独立启动方式，不是当前服务的管理入口。

在原生控制台填写实际宿主代理地址，例如 `http://127.0.0.1:7897`。如需重新登录，由用户在控制台显式启动，并在程序创建的同一 Chrome 窗口内完成网站要求的验证。启动脚本本身不重试真实账号，也不从日常浏览器复制 Cookie。

迁移前必须先停止写入任务，再移动或复制保存的私有数据；不能让两个进程写同一目录。Linux 容器的进程内密码、TOTP、邮箱/短信 API 凭据和 Sub2API 管理员 key 不会随元数据恢复；监控配置也不应复制后默认启用。2026-09-26 的容器及旧可视 Agent 验证保留在 `go-port-visible-console-verification.json` 等历史报告中，当前 toSub2 容器已移除。

`scripts/test-container.sh` 将现有 Go 测试交叉编译后，挂入同一生产镜像，以生产用户及资源约束执行。测试容器禁用外部网络，只访问合成本机 fixture，结束后移除测试容器和临时文件；不修改部署数据卷、不提交真实账号，也不把非 race 的测试二进制称为 race 验收。

2026-09-26本机部署补验：Docker Desktop Linux arm64、Debian12、Chromium154.0.8037.57。默认sandbox的6项启动检查均失败，`unshare --user`明确被容器拒绝，记录保留；显式关闭sandbox后13项顶层/18个PASS事件、0SKIP全部通过。当时的受控容器测试使用显式关闭配置；当前该容器已移除，本机原生 Chrome 保持 sandbox 开启。镜像与文件SHA、非root/权限检查、退出0及重启证据见 `go-port-container-verification.json`。

## Linux systemd 用户服务

示例：[tosub2.service](../deploy/tosub2.service)。普通用户运行可保留 Chromium sandbox，不需要 root。

```sh
install -d -m 700 "$HOME/.local/bin" "$HOME/.local/state/tosub2" "$HOME/.config/systemd/user"
install -m 700 bin/tosub2 "$HOME/.local/bin/tosub2"
install -m 600 deploy/tosub2.service "$HOME/.config/systemd/user/tosub2.service"
systemctl --user daemon-reload
systemctl --user enable --now tosub2
journalctl --user -u tosub2 -f
```

停止：`systemctl --user stop tosub2`。systemd 发送 SIGTERM，允许 30 秒优雅关闭，随后清理服务控制组。修改 `CHROME_PATH` 时使用 `systemctl --user edit tosub2` 添加 `[Service]` 和 `Environment=CHROME_PATH=/path/to/chromium`，再重启。不要将真实 API key、密码或代理凭据写入 unit 示例或仓库。

Linux 当前不提供持久安全凭据后端；需要密码/TOTP 的恢复任务可能要求重新输入。单纯安装服务并不能改变该限制。

## 可选 macOS 登录自启动 Agent（当前未采用）

当前本机使用上文的 `local-macos.sh`，没有安装登录自启动 plist。以下是另行选择登录自启动时的独立部署示例，不属于本地脚本的安装或清理范围，不能与当前 4400 实例共用端口和数据目录。

示例：[com.tosub2.console.plist](../deploy/com.tosub2.console.plist)。先将 `__HOME__` 替换成当前用户的绝对目录，并在复制前检查 XML；用户名目录含 XML 特殊字符时必须进行 XML 转义。

```sh
install -d -m 700 "$HOME/.local/bin" "$HOME/Library/LaunchAgents"
install -m 700 bin/tosub2 "$HOME/.local/bin/tosub2"
# 将替换了 __HOME__ 的 plist 保存到以下路径，然后校验。
plutil -lint "$HOME/Library/LaunchAgents/com.tosub2.console.plist"
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.tosub2.console.plist"
```

停止并注销：`launchctl bootout "gui/$(id -u)/com.tosub2.console"`。使用登录用户的 Agent，Keychain 可以使用同一用户授权。Chromium 默认从 `/Applications` 查找；其他位置可在 plist 的 ProgramArguments 中添加 `--chrome-path=/absolute/path`。

## Windows 原生服务

新增 deploy/install-service.ps1，注册纯 Go SCM 服务；主程序 --windows-service 处理 Stop、Shutdown 和 PreShutdown，共用 HTTP/任务/短信/浏览器关闭及持久化逻辑。Running 状态只在绑定端口、恢复元数据与启动完成后报告；启动失败或超时关闭返回非零服务状态。

在目标 Windows 的管理员 PowerShell 中自行审查和运行：

    .\deploy\install-service.ps1 -Binary C:\toSub2\tosub2.exe -OutputDirectory C:\toSub2\private-data -ChromePath 'C:\Program Files\Google\Chrome\Application\chrome.exe'
    Start-Service toSub2-Go
    Stop-Service toSub2-Go

脚本交互读取当前 Windows 用户的服务登录凭据，由 SCM 保存，不写脚本、账号导出或 job-meta。它拒绝切换到另一个用户以避免无法解开 DPAPI，不默认使用 SYSTEM。用户必须具有“作为服务登录”权限；本次没有安装服务。服务没有交互式桌面，需要人工完成浏览器挑战时应使用下方当前用户任务或前台控制台。部署前先运行 scripts/test-windows.ps1。

Windows 私有输出目录和临时文件使用当前用户专属、禁止继承的 DACL，创建时即施加并通过句柄验证；不支持安全 ACL 的文件系统会明确失败。已有文件的原子替换使用新私有文件的权限，不沿用旧宽松 ACL。

## Windows 当前用户任务

示例：[install-task.ps1](../deploy/install-task.ps1) 使用系统 Task Scheduler，在当前用户登录后运行 Go 二进制。它不保存 Windows 登录密码，也不创建以 SYSTEM 运行的服务。

```powershell
.\deploy\install-task.ps1 -Binary C:\toSub2\tosub2.exe
Start-ScheduledTask -TaskName toSub2-Go
```

注销启动任务：`Unregister-ScheduledTask -TaskName toSub2-Go`。Task Scheduler 的强制停止不等同于 SIGTERM；主动停机前应在控制台停止所有任务，确认元数据已写入。需要交互式优雅退出时直接在终端启动 Go 程序并按 Ctrl+C。该安装脚本未在本次 macOS 环境执行，Windows 现场的登录启动、DPAPI 和停止行为仍需验收。

## 浏览器与网络

- 安装当前稳定版 Chrome/Chromium，可通过 `CHROME_PATH` 指定路径。已执行本地集成测试的版本为 macOS Google Chrome `154.0.8037.58`；这不是最低版本承诺。
- 浏览器通过 CDP `Browser.getVersion` 记录实际版本、UA、平台和语言。Chromium 使用自己的 TLS stack，Go tls-client 使用其支持的 profile；敏感请求切换到同一 BrowserTransport 后不把二者宣称为相同 TLS 会话。
- 每个会话持有独立临时 profile，默认最多 4 个 Chromium 实例。代理 session 变化需要关闭旧 session 并创建新实例。
- HTTP/HTTPS 代理认证只响应 Proxy challenge，不将代理密码提供给源站 HTTP auth。SOCKS5/SOCKS5h 使用每会话的本地 Go HTTP/CONNECT 桥；桥仅监听 loopback，带随机认证，SOCKS5 使用本机 DNS，SOCKS5h 使用远端 DNS。
- `TOSUB2_BROWSER_HEADLESS=0` 显示浏览器，供本人完成需要交互的授权或 challenge；默认 headless。没有实现自动点击或绕过 Turnstile 的安全措施。
- Chromium sandbox 默认开启。仅在明确需要的受控容器中可设置 `TOSUB2_BROWSER_NO_SANDBOX=1`；它不应作为普通部署默认值。
- 默认绑定 loopback。若更改为 LAN 地址，服务会给出安全提示；启动 token 和 bootstrap 机制不是公网身份认证服务，不应直接暴露公网。

## 本次实际验收边界

2026-09-26 07:40:47 UTC 独立记录的真实公开 Sentinel SDK 与 Session Observer 已在 TOSUB2_BROWSER_HEADLESS=0 的可视 macOS Chrome 154.0.8037.58 中通过。此前默认无头环境在同一网络遇到 HTTP 403 验证页；该失败记录未删除。因此没有交互式桌面的服务部署仍须验证其网络/浏览器授权流程，不能用可视浏览器的成功作为无头服务验收。账号、短信供应商、Sub2API 和 Windows 原生凭据/SCM/Chromium Job Object 仍需目标环境执行。

macOS部署补验：同一SHA的发布二进制复制到临时安装目录后，使用示例plist完成两次bootstrap/bootout，健康检查、API鉴权、golden输出恢复、metadata落盘、进程退出均通过，测试注册已注销。直接从Downloads路径启动曾未在15秒内ready；不能据此断定具体系统原因，部署仍应先复制到文档的安装位置。Linux验证容器使用--init回收孤儿进程，并显式设置no-sandbox；不将这种容器配置当作普通用户部署默认值。
