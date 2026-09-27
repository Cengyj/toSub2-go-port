# macOS 本地测试与卸载

当前入口是 `scripts/local-macos.sh`，本机原生 Go 服务已在 `http://127.0.0.1:4400` 上线。使用当前登录的桌面用户运行，Chrome 为可视模式并开启 sandbox。React 静态资源已嵌入二进制；生产启动不调用 Node、Python、Vite 或 Docker。

## 启动、状态与停止

在源项目目录执行：

```sh
cd /Users/ceng/Downloads/toSub2-go-port
./scripts/local-macos.sh start
./scripts/local-macos.sh status
./scripts/local-macos.sh stop
./scripts/local-macos.sh restart
# 修改 Go 源码后显式重新编译并重启：
TOSUB2_LOCAL_REBUILD=1 ./scripts/local-macos.sh restart
```

`start` 使用已有本地二进制；缺少可执行文件时从本项目 Go 源码编译，因此首次构建需要 Go 工具链。已运行时只返回现状，即使设置了 `TOSUB2_LOCAL_REBUILD=1` 也不会隐式重启。`status` 显示状态，运行时同时显示端口和运行目录。`stop` 注销本实例的 launchd job，并在统一 30 秒预算内等待卸载和进程退出；它保留数据与凭据。`restart` 完成停止后再启动。

在下一次启动或重启前可设置 `TOSUB2_LOCAL_PORT`，例如 `TOSUB2_LOCAL_PORT=4418 ./scripts/local-macos.sh restart`；默认仍为 4400。该变量只改变端口，不创建另一份数据目录。`CHROME_PATH` 可指定系统 Chrome 可执行文件。本机代理填写自己的 loopback 地址，例如 `http://127.0.0.1:7897`，不使用容器的宿主机别名。

## 唯一运行目录

脚本根据源项目的规范绝对路径计算 SHA-256，取前 16 位作为项目标识。通用位置为 `$HOME/Library/Application Support/toSub2-local/<项目路径摘要>`。当前项目对应：

```text
/Users/ceng/Library/Application Support/toSub2-local/6428e6fb3abbc4ae
```

该目录统一保存 `bin/`、`data/`（含 `data/credentials/`）、`archive/`（如有迁入历史归档）、`logs/`、`tmp/`、`launchd/`、归属标记 `.owner` 及运行状态文件。目录和可执行二进制权限为 0700，私有配置、日志与数据文件为 0600。`TMPDIR` 指向内部 `tmp/`，本实例 Chrome 临时 profile 也保存在这里，优雅关闭会先停止浏览器。

运行文件不再放在 Downloads 下的源目录。脚本只注册当前 GUI 会话的 launchd job，标签为 `com.tosub2.local.6428e6fb3abbc4ae`；plist 留在运行目录的 `launchd/` 中，不复制到 `~/Library/LaunchAgents`，没有登录自启动配置。

Keychain 由系统保存加密密钥。本实例设置 `TOSUB2_MAC_KEYCHAIN_SERVICE=com.local.chatgpt-onboarding.credentials.local.6428e6fb3abbc4ae`，并将 `TOSUB2_MAC_CREDENTIAL_ROOT` 指向上述 `data/credentials`。隔离模式只访问专属 service 和指定凭据目录，不回退读取或删除旧安装的全局 service 或旧凭据路径。

项目标识取决于源目录路径。需要卸载或移动源目录时，应先在原路径完成清理；换路径后直接运行脚本会得到另一个项目标识，不能自动清理旧实例。

当前私有归档中还包括 `archive/migration-temporary-artifacts`：本轮将 263 个历史临时文件和 26 个目录统一迁入这里。它们属于下述 clean 的删除范围，不需要另找临时目录逐项清理。

## 永久清理运行目录

需要保留的输出应先自行备份，然后在原来的 Go 源项目目录执行：

```sh
cd /Users/ceng/Downloads/toSub2-go-port
./scripts/local-macos.sh clean --yes
```

`clean --yes` 会先停止且等待本实例退出，再循环删除其精确 Keychain service 下的条目，确认该 service 已无条目后，永久删除整个运行目录。删除范围包含任务、checkpoint、授权输出、加密凭据文件、历史 `archive/`、`bin/`、`logs/`、`tmp/` 和内部 launchd plist。完成后，`/Users/ceng/Library/Application Support/toSub2-local/6428e6fb3abbc4ae` 这个目录本身也会消失。

脚本检查项目归属标记、当前用户所有权及关键目录/文件的链接状态。停止超时、归属不符或 Keychain 清理失败时会报错并保留运行目录，不继续删除目录。不要先手工删除归属标记或运行目录，否则脚本不能再据此管理已存在的实例。

专属父目录 `/Users/ceng/Library/Application Support/toSub2-local` 仅在清空且没有其他项目 runtime 时通过 `rmdir` 删除；有其他内容则保留。脚本不删除或改变通用 `Library`、`Application Support` 父目录。

## 连源代码一起移除

清理运行目录后，源代码仍保留。若也不再需要源项目，先确认上述 `clean --yes` 成功，再在 Finder 中将 `/Users/ceng/Downloads/toSub2-go-port` 移到废纸篓。必须先 clean、后删除源目录，因为清理脚本和项目路径归属仍需要源项目；这些说明不会自动删除源码。

原项目 `/Users/ceng/Downloads/toSub2-main` 是独立兼容基线，`clean` 和上述移除 Go 源目录的操作均不包含它。本脚本还保留共享 Go/前端包管理缓存、系统 Chrome、系统诊断和旧安装的全局 Keychain 条目，不触碰其他项目的运行目录。

用户日常浏览器保存的控制台 localStorage 不在运行目录内。若也要清除网页配置，应在该浏览器中清除 `127.0.0.1:4400` 的站点数据，不要删除整个浏览器配置目录。

## 迁移与验收边界

### 显式导入旧 macOS 凭据

旧版全局 Keychain 与当前隔离 namespace 不同。任务或 checkpoint 被恢复，并不代表原密码、TOTP、代理密码和邮箱 API 配置已经进入隔离库。这是迁移后出现额外密码输入框的一种可能原因，需要核对导入结果，不能默认是账号密码错误。

新增离线入口使用旧版加密格式和 Keychain 解密，随后用当前隔离密钥重新加密。源目录只读；目标已有账号记录默认跳过，不覆盖本次批量导入已经明确更新或清空的字段。不会自动执行登录或发送验证码。

先停止本实例，再给出明确旧凭据目录和账号来源：

~~~sh
./scripts/local-macos.sh stop
./scripts/local-macos.sh import-credentials "/absolute/legacy/credentials" "/absolute/legacy/job-meta.json"
./scripts/local-macos.sh start
~~~

第二个参数也可用每行一个邮箱的 UTF-8 文件。支持旧单任务 metadata 的 email 字段、任务列表和 Go 根 metadata 的 jobs 列表；其余字段不进入导入输出。首次使用新源码而本地二进制尚未更新时，在导入命令前设置 TOSUB2_LOCAL_REBUILD=1。此操作不会隐式停止运行中的服务；必须先由用户执行 stop 并等退出，避免另一进程同时保存凭据。

输出只包含 Requested、Imported、SkippedExisting、Missing、Failed、Unprocessed 计数。缺失、失败或取消不能被报告为全部迁入；部分已成功账号会保留，可重试，已存在项仍跳过。密钥不可读、格式错误、非安全目标目录或非 macOS 环境返回明确错误，没有明文回退。

导入入口补齐的是凭据复制能力，不替代任务/输出目录的迁移，也不会静默开启跨 namespace 回退。旧库及旧全局 Keychain 仍不在本实例 clean 的删除范围内；新副本位于本实例 data/credentials 和隔离 Keychain 中，随 clean 删除。

历史容器数据可作为私有归档保存在本运行目录内；Linux 容器的进程内密码/API 配置不能随元数据恢复。迁移 macOS 加密凭据时仅在内存中解密并写入隔离存储，不把秘密写入普通元数据或日志。启动/停止和清理测试不会自动重试真实账号；账号登录与人工网站验证仍由账号本人或明确授权人员执行。

本机启动成功、脚本回归通过和 macOS Keychain 验证，不代表 Windows DPAPI/DACL/SCM 已在目标机器验收，也不代表真实账号、短信/邮箱服务、Sub2API 或所有上游 challenge 流程已通过。

最终脚本已在真实 Go 源码临时副本上通过启动、重启、无 Go 编译器 PATH 启动、两条合成隔离 Keychain 条目清理和重复 clean。全仓普通/race/vet复验与本机迁移证据见 `go-port-local-runtime-verification.json`；Downloads 启动失败只确认 EX_CONFIG 78 和空日志，未断定 TCC 原因。
