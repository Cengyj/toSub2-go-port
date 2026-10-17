# 依赖安全升级记录（2026-10-13）

本次修复依据固定版本 `govulncheck v1.9.0` 的源码扫描与官方 Go 漏洞数据库，
数据库快照更新时间为 `2026-10-08T22:31:09Z`。完整版本、调用点摘要和保留项的官方记录链接见
[机器可读审计记录](quality-security-audit-2026-10-13.json)。

## 扫描结论

- 修复前：Go 1.27.1，发现 **16 个符号级可达漏洞 ID**；包含未达项的模块记录合计 42 个 ID。
- 修复后：Go 1.27.2，**0 个符号级可达漏洞、0 个已导入受影响包漏洞**。
- 仍保留 **11 个所需模块级漏洞 ID**，本次源码扫描未发现受影响包被导入或受影响符号被调用。
- 正式文本模式扫描退出码为 0。该结果不表示“全部依赖无漏洞”；JSON 模式的退出码也不能单独作为安全通过依据。

## 版本选择与兼容性

| 项目 | 原版本 | 新版本 | 依据 |
| --- | --- | --- | --- |
| 生产构建 Go 工具链 | 1.27.1 | 1.27.2 | 修复扫描命中的标准库 HTTP/TLS/MIME 漏洞；`go.mod` 最低版本仍为 1.25.0 |
| `github.com/cloudflare/circl` | 1.6.2 | 1.6.3 | 修复 GO-2026-4550；该模块要求 Go 1.22.0 |
| `golang.org/x/net` | 0.48.0 | 0.56.0 | 修复 GO-2026-5025～5030，以及 DNS 解析 panic GO-2026-5942；该模块要求 Go 1.25.0 |
| `golang.org/x/crypto` | 0.46.0 | 0.53.0 | x/net 0.56.0 所需的最小版本选择结果 |
| `golang.org/x/sys` | 0.39.0 | 0.46.0 | x/net 0.56.0 所需的最小版本选择结果 |

依赖变更使用 `GOTOOLCHAIN=go1.25.0 go get` 和明确版本完成，未提高最低 Go 版本。
直接依赖 `golang.org/x/text v0.39.0` 保持，没有降级。
`tls-client v1.16.0`、`utls v1.7.8-barnius`、`fhttp v0.6.9` 与项目指纹配置均未修改。
未引入外部运行时或 CGO 依赖。

未直接采用 x/net 0.60.0：其官方 `go.mod` 要求 Go 1.26.0，会改变现有 Go 1.25 兼容承诺。
0.56.0 是在 0.55.0 修复 HTML/IDNA 问题后，额外修复已报告 DNS 问题的最小版本。

## 已执行验证

以下验证使用合成数据与本地测试服务，没有执行真实账号登录、2FA 修改或上传：

```sh
GOTOOLCHAIN=go1.25.0 go test ./internal/transport ./internal/sub2api \
  ./internal/protocol ./internal/jshost ./internal/legacyruntime \
  ./internal/credentials ./internal/privatefile
mkdir -p tmp
GOTOOLCHAIN=go1.25.0 CGO_ENABLED=0 go build -trimpath -o tmp/quality-vulnerability-go125 ./cmd/tosub2
GOTOOLCHAIN=go1.27.2 go mod verify
GOTOOLCHAIN=go1.27.2 go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

上述测试、构建和模块校验均通过。验证二进制的构建信息确认为 Go 1.25.0、`CGO_ENABLED=0`。
源码扫描固定为 `source ./...`；原始扫描与修复后扫描的配置及逐项摘要均保存在审计 JSON 中。
本记录只覆盖依赖变更验证，不能替代全仓 race、认证 worker、恢复流程及原生平台验收。

## 保留的模块级记录

| 模块及受影响组件 | 漏洞 ID | 官方修复版本 |
| --- | --- | --- |
| `github.com/klauspost/compress/s2` | GO-2026-5841 | 1.18.7 |
| `golang.org/x/crypto/openpgp` | GO-2026-5932 | 无修复版本；官方标记该包不再维护且设计不安全 |
| `golang.org/x/crypto/ssh` | GO-2026-6303；GO-2026-6354、6355 | 分别为 0.55.0；0.56.0 |
| `golang.org/x/net/http2` | GO-2026-6603、6610、6611、6612、6617 | 0.60.0，要求 Go 1.26.0 |
| `golang.org/x/text/secure/precis` | GO-2026-6629 | 0.41.0 |

这些记录没有被过滤或忽略。静态扫描的未达结论限于当前代码、构建条件和依赖图，
不能保证其他平台、测试工具或未来新增调用同样未受影响。新增相关功能或调整最低 Go 版本时，
应重新扫描并评估这些记录；如果要采用 x/net 0.60.0，应同时明确 Go 版本支持范围，
并重新验证纯 Go 构建、代理/TLS 指纹、CookieJar 和认证恢复契约。
