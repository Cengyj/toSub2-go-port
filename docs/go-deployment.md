# Go 服务部署

服务使用单个 Go 可执行文件提供认证、控制台与 Sobek 动态脚本 worker。认证时可启动同一可执行文件的子进程。前端通过 Go esbuild API 构建并嵌入，构建与运行均无需 Node、Python 或 Chromium 自动化。Go 最低版本为 1.25.0。

当前生产工具链固定 Go 1.27.2（go.mod toolchain、CI 和 Docker 一致）；Go 1.25.0 继续单独验证源码兼容，不能将旧标准库的兼容构建作为安全生产产物。工具链与依赖变化依据见本次质量升级记录。

## 构建

```sh
make build
./bin/tosub2 --host=127.0.0.1 --port=4399 --output-root=/absolute/path/to/private-data
```

`make frontend` 只构建网页；`scripts/build-cross.sh` 生成 macOS arm64/amd64、Linux amd64 与 Windows amd64 产物。直接执行 `CGO_ENABLED=0 go build -o bin/tosub2 ./cmd/tosub2` 使用当前嵌入资源。分发时附上适用的第三方许可证。

服务与交互控制台应使用相同操作系统用户，以保持 Keychain/DPAPI 用户范围一致。Linux 敏感凭据当前仅保存在进程内，重启需重新提供密码、TOTP 和 API key。私有输出使用独立目录，避免两个服务同时写入。

## 可重复验证与安全升级

```sh
./scripts/verify.sh
./scripts/build-cross.sh
# 联网执行固定版本 Go 漏洞分析工具，不修改本模块依赖：
TOSUB2_VERIFY_VULN=1 ./scripts/verify.sh
```

verify 检查格式与 diff，运行普通/race/vet、显式 Sobek worker 和本地协议恢复夹具，再构建前端和 CGO=0 主程序。CI 验证重建前端与已提交的两份 dist 一致。漏洞检查退出非零时须检查原因：工具不兼容、数据库不可达和发现漏洞均不能写成通过。Go 最低版本兼容测试与生产工具链安全维护是不同的检查。

需要复核长超时、真实五分钟周期或最低版本时，分别运行以下独立验收。它们只使用本地合成服务，但真实等待时间不会缩短：

```sh
GOTOOLCHAIN=go1.25.0 CGO_ENABLED=0 go test -count=1 ./...
TOSUB2_LONG_TRANSPORT_TEST=1 go test -count=1 ./internal/transport \
  -run '^TestTransportConfiguredBudgetCanExceedThirtySeconds$'
TOSUB2_LONG_MONITOR_TEST=1 TOSUB2_SLOW_HTTP_TEST=1 go test -count=1 ./cmd/tosub2 \
  -run 'TestConsole(DefaultFiveMinuteMonitorAndInflightShutdown|LongSub2APIResponseKeepsLegacyTimeout)$' -timeout=10m
```

macOS 原生凭据专项需当前用户的 Keychain 可用；设置独立的 `TOSUB2_MAC_KEYCHAIN_SERVICE`（以 `com.local.chatgpt-onboarding.credentials.local.` 开头）和 `TOSUB2_KEYCHAIN_TEST=1`，再执行 credentials 包的 `TestMacOSKeychainIntegration`、`TestMacOSKeychainLegacyImportIntegration`、`TestNativeCredentialStoreSurvivesProcessRestart`。测试只创建合成身份并负责删除；不要把生产账号作为测试输入。其他 opt-in 的跳过原因与平台边界须另行记录。

升级顺序：

1. 在源码目录完成上述验证。记录当前运行二进制路径、版本/摘要、前端资源 hash；不复制或打印 console token。
2. 在控制台确认全局活动数和队列数均为零，并确认监控没有正在执行。存在待执行任务时先完成或按原有流程取消/暂停，不在升级脚本中隐式取消真实操作。
3. 保留当前可执行文件作为私有回退副本；需要数据备份时先停止服务，按整个输出目录做私有文件系统快照，凭据仍依赖同一用户与原 Keychain/DPAPI namespace。不要导出明文凭据用于备份。
4. 通过平台原有生命周期入口优雅停止，等待服务和 worker 退出；以同一用户、端口、输出目录和 namespace 安装新二进制，再启动。禁止两个进程同时写同一输出目录。
5. 核对 `/api/health`、账号计数、全局活动/排队数、当前运行路径和文件摘要；核对 HTML 引用的 JS/CSS 与构建产物一致，然后刷新浏览器。不要通过真实授权、2FA 或上传来探测升级。
6. 健康或资源核对失败时立即停止新进程，恢复先前二进制并以原参数启动；保留失败现场和 recovery 文件，不用空目录覆盖旧资料。本次应用提取不迁移数据 schema。

## 故障恢复

- 网络或业务拒绝：读取当前操作及独立同步状态。已保存授权仍有效时保留文件；`failed`、`unknown`、`skipped` 和 `queued` 不等于同一结果。
- OAuth 一次性 code 已换取 token：先恢复 `tokens_received` checkpoint，导出失败只重做本地步骤。refresh 轮换优先恢复同目录的 grant recovery sidecar，不能重新消费旧 refresh token。
- 2FA 部分完成：先确认新密钥安全保存和退出设备状态；待核实/隔离状态不得重置或上传掩盖。账号信息与原始资料导出应读取当前 vault，而非历史输入。
- Sub2 返回未知：先核实远端对应账号，再选择适当重试；不把空响应、矛盾计数或断连当创建成功，也不自动重复创建。已完成的部分会保留在结果中。
- 磁盘/凭据服务故障：修复权限、容量或用户会话后再恢复；metadata 提交失败时私有结果仍保留，成功保存的新密钥不会回滚。禁止以删除 checkpoint/私有结果作为通用修复手段。
- 本地提交结果未知：文件已替换后目录同步失败时，当前进程不会执行新操作，但重启可能恢复磁盘上的排队意图。先核实账号与队列、旧授权和 checkpoint；不要把错误提示视为确定未提交，也不要立即整批重交。磁盘修复后的重试应按当前恢复资格逐项进行。
- 批量提交：`queued` 只表示已受理，`skipped` 是资格跳过，`failed` 是确定提交失败，`unknown` 需先核实；它们都不表示授权或上传完成。部分提交的 HTTP 200 同样可能是 `outcome=partial`、`ok=false`。界面只移除确定入队项，保留失败、跳过和未知项的选择；确认框关闭后先检查结果再重新操作。
- 浏览器保存失败：界面说明服务端是否已生效及本浏览器是否保存；不要把未持久化的偏好写成“已记住”。

代码证据、删除依据和验收边界见 [第二轮审查优化](quality-review-2026-10-14.md)；完整功能矩阵见 [首轮质量升级记录](quality-upgrade-2026-10-13.md)。

## macOS 本地测试

```sh
./scripts/local-legacy-macos.sh start
./scripts/local-legacy-macos.sh status
make frontend
TOSUB2_LOCAL_REBUILD=1 ./scripts/local-legacy-macos.sh restart
./scripts/local-legacy-macos.sh stop
./scripts/local-legacy-macos.sh clean --yes
```

默认地址为 `http://127.0.0.1:4401`。此脚本名只为沿用该实例路径；运行实现与普通主程序相同。`stop` 保留资料，`clean` 删除本实例运行目录和专属 Keychain，保留外部备份。准确范围见 [本地测试与卸载](local-macos.md)，实际部署验收见 [部署记录](pure-go-runtime/local-deployment.md)。

## Docker

Dockerfile 的构建阶段使用 Go 构建前端和主程序，运行阶段包含程序、CA 证书和健康检查工具。容器不安装外部脚本运行时或浏览器自动化依赖。

```sh
docker compose build
docker compose up -d --wait
docker compose ps
docker compose logs --tail=50 console
```

打开 `http://127.0.0.1:4399`。`TOSUB2_DOCKER_PORT=4418 docker compose up -d --wait` 可更改宿主端口。容器内监听 `0.0.0.0:4399`，宿主仅发布到 loopback。Mac 宿主代理使用 `http://host.docker.internal:实际端口`；容器里的 `127.0.0.1` 指向容器自身。

默认 UID/GID 为 10001，私有命名卷挂载 `/data`；根文件系统只读，临时文件使用 `/tmp`，init 回收 worker，SIGTERM 允许 30 秒优雅退出。自定义 bind mount 需允许运行用户以私有权限写入。

```sh
docker compose stop
docker compose start --wait
docker compose down
# 确实要永久删除本部署卷中数据时：
docker compose down --volumes
```

前三条命令保留数据卷。Linux 的进程内密码与 API 配置不会随卷持久化。容器测试入口 `scripts/test-container.sh` 在已有镜像中挂载静态 Go 测试二进制，禁用外网，只运行本地合成 fixture；结束后删除临时测试文件，不修改部署数据卷。

## Linux systemd 用户服务

模板为 [tosub2.service](../deploy/tosub2.service)：

```sh
install -d -m 700 "$HOME/.local/bin" "$HOME/.local/state/tosub2" "$HOME/.config/systemd/user"
install -m 700 bin/tosub2 "$HOME/.local/bin/tosub2"
install -m 600 deploy/tosub2.service "$HOME/.config/systemd/user/tosub2.service"
systemctl --user daemon-reload
systemctl --user enable --now tosub2
journalctl --user -u tosub2 -f
```

`systemctl --user stop tosub2` 优雅停止。卸载注册先 `systemctl --user disable --now tosub2`，再删除已安装的 unit 并执行 daemon-reload；数据目录与二进制独立保留。systemd 不改变 Linux 凭据存储限制。本轮未在目标 Linux 机器安装服务。

## 可选 macOS 登录自启动

本地测试脚本没有安装登录自启动 Agent。需要另行部署时，可使用 [com.tosub2.console.plist](../deploy/com.tosub2.console.plist)，将 `__HOME__` 替换为当前用户绝对路径并正确 XML 转义。

```sh
install -d -m 700 "$HOME/.local/bin" "$HOME/Library/LaunchAgents"
install -m 700 bin/tosub2 "$HOME/.local/bin/tosub2"
# 将替换好路径的 plist 放入以下位置，再校验和注册：
plutil -lint "$HOME/Library/LaunchAgents/com.tosub2.console.plist"
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.tosub2.console.plist"
```

停止并注销使用 `launchctl bootout "gui/$(id -u)/com.tosub2.console"`。该独立安装不属于本地脚本的 clean 范围，端口与数据目录也应独立。

## Windows

使用当前用户任务模板 [install-task.ps1](../deploy/install-task.ps1)：

```powershell
.\deploy\install-task.ps1 -Binary C:\toSub2\tosub2.exe
Start-ScheduledTask -TaskName toSub2-Go
```

删除注册使用 `Unregister-ScheduledTask -TaskName toSub2-Go`。Task Scheduler 的强制停止不等同于优雅退出；需要前台交互退出时在终端运行程序并按 Ctrl+C。

原生 SCM 服务使用 [install-service.ps1](../deploy/install-service.ps1)，在管理员 PowerShell 执行：

```powershell
.\deploy\install-service.ps1 -Binary C:\toSub2\tosub2.exe -OutputDirectory C:\toSub2\private-data
Start-Service toSub2-Go
Stop-Service toSub2-Go
```

安装交互读取当前 Windows 用户的服务登录凭据，由 SCM 保存；账户需有“作为服务登录”权限。服务与控制台必须使用同一用户以读取 DPAPI。私有文件使用当前用户专属 protected DACL，不支持持久 ACL 的文件系统会明确失败。本轮未安装 Windows 服务；应在目标机器执行 `scripts/test-windows.ps1` 验证。

## 当前验证范围

合成 fixture、静态构建与本机启动分别验证不同边界。交叉编译不证明目标平台凭据或服务正常；本地 fixture 不证明当前线上 challenge、真实账号或第三方服务完整可用。旧运行路径与历史部署报告仅在外部备份中保留。
