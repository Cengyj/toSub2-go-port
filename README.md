# toSub2-go-port

toSub2 的 Go 迁移项目。当前认证、HTTP/TLS、任务调度、控制台及动态脚本宿主均由 Go 实现，动态 JavaScript 通过嵌入的 Sobek 引擎执行。项目不再保留 Node、Python 或 Chromium 自动化运行路径。普通浏览器只用于打开网页控制台。

## 构建与启动

安装 Go 1.25.0 或更高版本：

```sh
make build
./bin/tosub2 --host=127.0.0.1 --port=4399
```

打开 `http://127.0.0.1:4399`。`make build` 使用 Go esbuild API 构建 React 网页，再编译嵌入静态资源的 Go 可执行文件；构建和运行都不需要外部脚本运行时。分发二进制时请附上生成的第三方许可证。

批量导入只保存账号；选择账号并点击“开始授权”才启动认证。

控制台提供账号管理、服务配置和号池监控三个页面，支持暖灰、纯白、深色及跟随系统的主题。接码平台、邮件 API、Sub2API 和代理在服务配置中编辑；账号行直接提供重登授权、重置 2FA、删除、上传和日志按钮，账号资料卡、下载等操作位于“更多”中，批量操作位于顶部工具栏。账号套餐以徽章展示，Sub2API 设置支持按套餐排除自动重登；重登授权支持成功后自动上传，重置 2FA 可选择退出所有设备后重登并自动上传。组件结构与验证记录见 [前端重构](docs/pure-go-runtime/frontend-refactor.md)。

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
- [部署说明](docs/go-deployment.md)
- [原版兼容流程与剩余差距](docs/legacy-runtime.md)
- [纯 Go 动态脚本宿主及验证边界](docs/pure-go-runtime/README.md)

[重置 2FA、可选退出所有设备及后续重登上传](docs/pure-go-runtime/totp-reset.md)、账号资料卡和原始信息导出均已实现；重置成功后查看和导出使用当前保存的新 2FA 密钥。本地合成测试不能替代真实账号、当前线上 challenge、第三方服务或 Windows 专有功能验收。旧分支源码、工具及部署说明已清理，历史副本仅在外部备份中保留。
