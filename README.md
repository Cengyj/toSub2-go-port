# toSub2-go-port

toSub2 的 Go 迁移项目。当前认证、HTTP/TLS、任务调度、控制台及动态脚本宿主均由 Go 实现，动态 JavaScript 通过嵌入的 Sobek 引擎执行。项目不再保留 Node、Python 或 Chromium 自动化运行路径。普通浏览器只用于打开网页控制台。

新增 [账号用量与状态查询](docs/pure-go-runtime/account-insights.md)：在服务配置中启用并设定更新周期，支持手动与批量查询、BPS 权限、额度及订阅期限。默认关闭，只读查询不会登录、刷新授权或上传账号。

[账号回收站](docs/pure-go-runtime/recycle-bin.md)：删除默认保留同邮箱全部历史、日志、凭据与授权文件，可恢复或明确彻底删除。服务配置可设置 1–3650 天后自动清理，默认 0 天表示关闭自动清理。旧版本已经裁掉的日志无法补回；非空回收站不能直接用不支持此格式的旧程序回退。

## 构建与启动

安装 Go 1.26.0 或更高版本：

```sh
make build
./bin/tosub2 --host=127.0.0.1 --port=4399
```

打开 `http://127.0.0.1:4399`。`make build` 使用 Go esbuild API 构建 React 网页，再编译嵌入静态资源的 Go 可执行文件；构建和运行都不需要外部脚本运行时。分发二进制时请附上生成的第三方许可证。

批量导入只保存账号；选择账号并点击“开始授权”才启动认证。

支持选择推送到 **Sub2API 或 CLIProxyAPI（CPA）**：在“服务配置 → CPA”保存管理地址与管理密钥，在账号页或重登／2FA 弹窗选择推送目标。CPA 支持 ChatGPT/Codex 授权，BPS 授权仍使用 Sub2API。单条、批量及授权后自动推送使用相同目标规则，详见 [CPA 推送配置](docs/pure-go-runtime/cpa-upload.md)。

控制台提供账号管理、回收站、服务配置和号池监控页面，支持暖灰、纯白、深色及跟随系统的主题。接码平台、邮件 API、Sub2API、CPA 和代理在服务配置中编辑；账号行直接提供重登授权、重置 2FA、移入回收站、上传和日志按钮，账号资料卡、下载等操作位于“更多”中，批量操作位于顶部工具栏。账号套餐以徽章展示，号池监控共用巡检开关、时间和套餐排除规则，选择 Sub2API 或 CPA 后保存生效；重登授权支持成功后自动上传，重置 2FA 可选择退出所有设备后重登并自动上传。组件结构与验证记录见 [前端重构](docs/pure-go-runtime/frontend-refactor.md)。

## macOS 本地测试

```sh
./scripts/local-legacy-macos.sh start
./scripts/local-legacy-macos.sh status
# 源码更新后，构建前端并更新同一实例：
make frontend
TOSUB2_LOCAL_REBUILD=1 ./scripts/local-legacy-macos.sh restart
./scripts/local-legacy-macos.sh stop
# 永久删除该实例的运行文件、账号资料和专属 Keychain 条目：
./scripts/local-legacy-macos.sh clean --yes
```

默认地址为 `http://127.0.0.1:4401`。`stop` 保留资料；`clean` 不删除源码或运行目录之外的备份。范围及站点配置清理见 [本地测试与卸载](docs/local-macos.md)，实际部署验收见 [本地部署记录](docs/pure-go-runtime/local-deployment.md)。

## 测试和文档

```sh
go test ./...
go test -race ./...
go vet ./...
```

- [运行、配置和故障排查](README-Go.md)
- [账号模型质量检测、六题规则与上传门禁](docs/pure-go-runtime/account-quality.md) · [本轮实施与验证](docs/pure-go-runtime/account-quality-2026-11-03.md)
- [当前全项目模块维护台账](docs/maintenance-review-2026-11-10.md#全项目模块审查当前执行)
- [2026-10-31 质量优化与验收（历史）](docs/quality-review-2026-10-31.md)
- [2026-10-31 第二轮：取消持久化、授权边界与轮询故障](docs/quality-review-2026-10-31-round2.md)
- [部署说明](docs/go-deployment.md)
- [原版兼容流程与剩余差距](docs/legacy-runtime.md)
- [纯 Go 动态脚本宿主及验证边界](docs/pure-go-runtime/README.md)

[重置 2FA、可选退出所有设备及后续重登上传](docs/pure-go-runtime/totp-reset.md)、账号资料卡和原始信息导出均已实现；重置成功后查看和导出使用当前保存的新 2FA 密钥。本地合成测试不能替代真实账号、当前线上 challenge、第三方服务或 Windows 专有功能验收。旧分支源码、工具及部署说明已清理，历史副本仅在外部备份中保留。
