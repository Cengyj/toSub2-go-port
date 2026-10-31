# Go 服务部署

服务使用单个 Go 可执行文件提供认证、控制台与 Sobek 动态脚本 worker。认证时可启动同一可执行文件的子进程。前端通过 Go esbuild API 构建并嵌入，构建与运行均无需 Node、Python 或 Chromium 自动化。Go 最低版本为 1.26.0。

当前生产工具链固定 Go 1.27.3（go.mod toolchain、CI 和 Docker 一致）；Go 1.26.0 继续单独验证源码兼容，不能将旧标准库的兼容构建作为安全生产产物。工具链与依赖变化依据见本次质量升级记录。

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
GOTOOLCHAIN=go1.26.0 CGO_ENABLED=0 go test -count=1 ./...
TOSUB2_LONG_TRANSPORT_TEST=1 go test -count=1 ./internal/transport \
  -run '^TestTransportConfiguredBudgetCanExceedThirtySeconds$'
TOSUB2_LONG_MONITOR_TEST=1 TOSUB2_SLOW_HTTP_TEST=1 go test -count=1 ./cmd/tosub2 \
  -run 'TestConsole(DefaultFiveMinuteMonitorAndInflightShutdown|LongSub2APIResponseKeepsLegacyTimeout)$' -timeout=10m
```

macOS 原生凭据专项需当前用户的 Keychain 可用；设置独立的 `TOSUB2_MAC_KEYCHAIN_SERVICE`（以 `com.local.chatgpt-onboarding.credentials.local.` 开头）和 `TOSUB2_KEYCHAIN_TEST=1`，再执行 credentials 包的 `TestMacOSKeychainIntegration`、`TestMacOSKeychainLegacyImportIntegration`、`TestNativeCredentialStoreSurvivesProcessRestart`。测试只创建合成身份并负责删除；不要把生产账号作为测试输入。其他 opt-in 的跳过原因与平台边界须另行记录。

升级顺序：

1. 在源码目录完成上述验证。记录当前运行二进制路径、版本/摘要、前端资源 hash；不复制或打印 console token。
2. 在控制台确认全局活动数和队列数均为零，并确认巡检、账号查询及前台同步没有正在执行。存在待执行任务时先完成或按原有流程取消/暂停，不在升级脚本中隐式取消真实操作。**单次读取零计数不构成安全停服的原子保证**：当前 HTTP 服务没有维护期入队封锁接口，生命周期脚本锁也只协调脚本之间的启停。无人值守升级若不能在检查至停服期间阻止浏览器、CLI 和自动恢复提交新工作，应保留已验证构建并中止部署；不要把普通 `restart` 当作已有任务排空协议。人工维护窗口须先停止提交来源并确认，不能靠连续读取两次零计数代替封锁。
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
- 停止任务保存失败：停止接口返回 HTTP 409 和 `uncertain:true`，表示本进程已请求取消、排队项不会继续执行，但磁盘可能仍保留旧排队意图。不要立即重启。先修复存储，再在同一进程通过单账号 `/api/jobs/{id}/cancel` 或 `/api/jobs/cancel-all` 再次确认；这些接口允许无新任务时刷新取消状态。活动任务仍须等待 worker 清理完成，HTTP 成功及 `canceled` 计数不是远端操作已停止的证明。
- 自动上传提示授权文件不安全或待恢复：保留现场，核对预期私有目录、所有者和权限；存在 refresh recovery journal 时先按原流程完成已取得授权的本地恢复。不要删除 journal、跟随替换目录或放宽读取权限强行上传。
- 操作意外 panic：工作流会尽力将已开始同步记为 unknown、未开始同步记为 skipped，不把 panic 当成功；原 panic 仍传播，进程可能退出。启动后先核实已完成步骤与私有恢复证据，不据此自动重放安全操作。
- 批量提交：`queued` 只表示已受理，`skipped` 是资格跳过，`failed` 是确定提交失败，`unknown` 需先核实；它们都不表示授权或上传完成。部分提交的 HTTP 200 同样可能是 `outcome=partial`、`ok=false`。界面只移除确定入队项，保留失败、跳过和未知项的选择；确认框关闭后先检查结果再重新操作。
- 批量导入中途失败：HTTP 409 仍可能已有账号资料保存。检查回执中的 `completed`、`created`、`updated`、`failed`、`unknown`、`unprocessed` 与 `stoppedAt`，先核对列表，再仅处理未完成资料；不要把整批错误理解为整批回滚。导入不会自动登录。
- 浏览器保存失败：界面说明服务端是否已生效及本浏览器是否保存；不要把未持久化的偏好写成“已记住”。

当前代码证据、功能矩阵、删除依据与验收边界见[第一轮质量优化](quality-review-2026-10-31.md)和[第二轮增量优化](quality-review-2026-10-31-round2.md)；历史记录保留各自版本的验证范围。

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

## 彻底卸载与备份范围

先停止本实例并确认所属 worker 全部退出，再决定哪些备份需要保留。备份至少应包含整个私有输出目录（账号 metadata、加密凭据文件、配置、checkpoint、授权输出及 grant recovery sidecar），以同一用户和原 namespace 恢复；单独复制 metadata 不等于复制了可用凭据。不要将私有备份提交 Git 或同步到未获授权的位置。Linux 进程内凭据不在磁盘备份中。

- macOS 本地脚本部署：`clean --yes` 删除该实例目录及专属 Keychain 条目；外部备份与浏览器站点设置需单独处理。独立 LaunchAgent 先 bootout，再移除对应 plist、可执行文件和明确的数据目录，不能借用另一实例的 clean。
- Linux 用户服务：先 disable/stop 并移除该应用 unit、daemon-reload；确认不再需要后，删除自己安装的可执行文件与该应用 state 目录。不要清理共享 Go 缓存、用户主目录或其他应用资料来“彻底卸载”。
- Docker：`docker compose down` 保留命名卷；明确确认永久删除本部署数据时才使用 `down --volumes`。外部 bind mount 和手工备份不随该命令清除。
- Windows：计划任务先注销；SCM 服务先 `Stop-Service toSub2-Go` 并确认停止，再 `sc.exe delete toSub2-Go` 注销。确认备份范围后手动删除实际安装目录和该实例私有数据目录；不要清除用户 DPAPI 主密钥或其他应用凭据。
- 浏览器：清除对应主机和端口的网页配置／站点数据；这不会替代服务端数据清理，也不会关闭服务端账号查询开关。

卸载及删除备份不可作为修复未知授权结果、2FA 部分完成或磁盘故障的手段。恢复时先保留现场，按工作流已完成步骤继续；有格式变更时须先对备份副本验证迁移，本轮无格式迁移。

## 当前验证范围

合成 fixture、静态构建与本机启动分别验证不同边界。交叉编译不证明目标平台凭据或服务正常；本地 fixture 不证明当前线上 challenge、真实账号或第三方服务完整可用。旧运行路径与历史部署报告仅在外部备份中保留。
