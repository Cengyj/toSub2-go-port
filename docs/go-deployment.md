# Go 服务部署

服务使用单个 Go 可执行文件提供认证、控制台与 Sobek 动态脚本 worker。认证时可启动同一可执行文件的子进程。前端通过 Go esbuild API 构建并嵌入，构建与运行均无需 Node、Python 或 Chromium 自动化。Go 最低版本为 1.25.0。

## 构建

```sh
make build
./bin/tosub2 --host=127.0.0.1 --port=4399 --output-root=/absolute/path/to/private-data
```

`make frontend` 只构建网页；`scripts/build-cross.sh` 生成 macOS arm64/amd64、Linux amd64 与 Windows amd64 产物。直接执行 `CGO_ENABLED=0 go build -o bin/tosub2 ./cmd/tosub2` 使用当前嵌入资源。分发时附上适用的第三方许可证。

服务与交互控制台应使用相同操作系统用户，以保持 Keychain/DPAPI 用户范围一致。Linux 敏感凭据当前仅保存在进程内，重启需重新提供密码、TOTP 和 API key。私有输出使用独立目录，避免两个服务同时写入。

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
