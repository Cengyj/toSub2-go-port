# toSub2-go-port

toSub2 的 Go 迁移项目。仓库包含 Go 服务端、React 前端源码与嵌入静态资源、测试、构建/部署脚本、迁移文档及旧版参考代码。

生产服务使用 Go 编译后的程序；当前网页登录路径需要已安装的 Chromium/Chrome，生产运行时不需要 Node.js 或 Python。Node.js 仅用于重新构建 React 静态资源。

## 构建与启动

安装符合 [go.mod](go.mod) 要求的 Go 工具链后：

```sh
mkdir -p bin
go build -o bin/tosub2 ./cmd/tosub2
./bin/tosub2 --host=127.0.0.1 --port=4399 --output-root=tmp/chatgpt-onboarding-console
```

打开 `http://127.0.0.1:4399`。浏览器准备、配置、macOS 本地运行与彻底清理方法见 [README-Go.md](README-Go.md)。Windows 可将输出文件名改为 `bin/tosub2.exe`。

## 测试

```sh
go test ./...
go test -race ./...
go vet ./...
```

浏览器、系统凭据及慢请求集成测试有独立启用条件，未启用时明确跳过，详见 [Go 使用文档](README-Go.md#测试)。仓库中的 fixture 使用合成数据；本机任务、真实凭据、日志和浏览器 profile 不随源码上传。

## 文档与迁移状态

- [安装、运行、配置和故障排查](README-Go.md)
- [完整迁移审计](docs/go-port-audit.md)
- [迁移状态与未完成项](docs/go-port-migration-status.md)
- [功能兼容核查与测试记录](docs/go-port-full-parity-review.md)
- [纯协议路径可实施性调查](docs/go-pure-protocol-feasibility.md)
- [部署说明](docs/go-deployment.md)
- [旧版文档](README-legacy.md)与 [旧版参考源码](legacy/)

当前已有可独立运行的 Go 版本，但不宣称全部功能验收完成：部分旧中间 checkpoint 仍需显式重新登录，真实账号/第三方服务/生产 challenge 和 Windows 专有凭据功能仍有待验收范围。当前新网页登录仍使用 Chromium；严格无浏览器的完整登录路径尚未完成。
