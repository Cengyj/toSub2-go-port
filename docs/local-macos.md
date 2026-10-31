# macOS 本地测试与卸载

当前只有 Go 认证实现。`scripts/local-legacy-macos.sh` 保留原有 4401 实例的名称和数据位置，脚本名中的 legacy 不代表可选择的旧运行分支。普通浏览器打开网页控制台，服务不启动浏览器自动化。实际部署与二进制核验见 [本地部署记录](pure-go-runtime/local-deployment.md)。

## 启动、升级与清理

使用当前登录的 macOS 桌面用户：

```sh
cd /absolute/path/to/toSub2-go-port
./scripts/local-legacy-macos.sh start
./scripts/local-legacy-macos.sh status
./scripts/local-legacy-macos.sh stop
./scripts/local-legacy-macos.sh restart
# 前端或 Go 源码更新后：
make frontend
TOSUB2_LOCAL_REBUILD=1 ./scripts/local-legacy-macos.sh restart
# 永久删除此测试实例及其资料：
./scripts/local-legacy-macos.sh clean --yes
```

默认地址为 `http://127.0.0.1:4401`。`start` 已运行时只显示状态，不隐式重启。`restart` 先等待旧服务与 worker 退出，再启动。缺少本地二进制时使用已构建的 `bin/tosub2` 或 Go 工具链；显式 rebuild 需要 Go。日常启动编译好的程序不需要开发工具。

`restart` 没有业务任务排空或维护期提交封锁能力。升级前须遵循 [安全升级](go-deployment.md#可重复验证与安全升级) 的维护窗口要求；不能仅凭瞬时空闲计数自动重启正在使用的实例。

`TOSUB2_LOCAL_PORT=4418 ./scripts/local-legacy-macos.sh restart` 可改端口，仍使用同一数据目录。批量导入只保存账号，启动服务不会自动登录。

## 运行目录与删除范围

4401 实例目录格式（实际路径由 `status` 显示）：

```text
$HOME/Library/Application Support/toSub2-local/<project-hash>-legacy
```

launchd 标签为 `com.tosub2.local.<project-hash>.legacy`，Keychain service 为 `com.local.chatgpt-onboarding.credentials.local.<project-hash>.legacy`。标识来自源项目规范绝对路径的 SHA-256 前 16 位；移动源码前应在原路径完成清理。示例中的占位符须替换为实际实例标识，不能作为命令直接执行。

运行目录集中保存 `bin/`、`data/credentials/`、任务、checkpoint、导出、`logs/`、`tmp/`、内部 `launchd/` 和归属标记。目录及可执行文件权限为 0700，私有数据与日志为 0600。worker 临时输入位于内部 TMPDIR。脚本只注册当前 GUI 会话，不向 `~/Library/LaunchAgents` 安装登录自启动配置。

`stop` 保留资料。`clean --yes` 先在 30 秒总预算内停止本实例，确认所属进程已退出，再删除精确 Keychain service 下的条目及整个运行目录。内部 `archive/` 与升级回退副本也属于删除范围；运行目录之外的项目备份保持不变。

停止超时、归属不匹配、链接异常或 Keychain 删除失败时，脚本报错并保留目录。不要先手工删除归属标记。专属父目录 `toSub2-local` 只在为空时移除，其他实例的数据不受影响。

源码、原始功能参考项目、系统工具链、共享缓存及其他应用凭据不属于实例清理范围。需要连 Go 源码一起删除时，先成功 clean，再将源码目录移到废纸篓。

网页的 localStorage 位于用户浏览器站点存储，服务 clean 不操作它。需要清除时使用网页配置清除操作，或在浏览器中清除 `127.0.0.1:4401` 的站点数据；改过端口时同时处理相应地址。

## 另一个本地实例

`scripts/local-macos.sh` 默认端口 4400，也运行相同 Go 程序，使用不带 `-legacy` 的目录：

```text
$HOME/Library/Application Support/toSub2-local/<project-hash>
```

它的 launchd 标签、Keychain service 也不带 `.legacy`。管理命令与上文一致，只需使用 `local-macos.sh`。两个入口只隔离实例资料；不提供不同认证实现。清理其中一个不会删除另一个。

## 显式导入旧 macOS 凭据

隔离实例不会自动读取旧全局凭据。metadata 恢复成功不代表密码、TOTP 或 API 配置已经进入当前凭据库。离线导入先停止实例，再提供明确的源凭据目录与账号来源：

```sh
./scripts/local-legacy-macos.sh stop
./scripts/local-legacy-macos.sh import-credentials /absolute/old/credentials /absolute/old/job-meta.json
./scripts/local-legacy-macos.sh start
```

账号来源也可以是每行一个邮箱的 UTF-8 文件。导入只读取源目录，使用当前隔离密钥重新加密，目标已有账号默认跳过。它不会自动登录、发送验证码或复制任务输出目录；输出只含计数。密钥不可用、格式不合法或目标权限不安全会明确失败。

导入的新副本随当前实例 clean 删除；源目录与旧全局 Keychain 不在删除范围。Linux 容器内存中的密码/API 配置无法仅靠 metadata 恢复。

## 验证边界

脚本测试使用隔离的临时目录、合成账号与 mock 系统命令，覆盖启停、重启、无 Go 工具链启动、专属 Keychain 清理、重复 clean 和停止失败时保留资料。实际 4401 部署状态以独立部署记录为准。脚本或本地 fixture 通过不代表真实账号、第三方服务或 Windows 专有功能已经验收。
