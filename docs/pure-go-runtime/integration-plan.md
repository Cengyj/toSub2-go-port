# Go 认证 worker 接入位点

2026-10-12 清理后的当前源码映射。认证默认并仅支持 Go HTTP/TLS 与同一可执行文件的 Sobek worker。运行方式选择参数及 Node/Python/浏览器自动化分支均已移除。

| 位点 | 当前实现与不变量 |
| --- | --- |
| `cmd/tosub2/main.go` | 配置、console、manager 和凭据初始化之前调用 `jshost.WorkerMain`，私有 worker 完成后直接退出。 |
| `internal/config/config.go` | 默认 `legacy` + `go`；拒绝外部脚本宿主和浏览器认证；保留 refresh/离线恢复等不启动脚本的路径。 |
| `cmd/tosub2/authentication_runtime.go` | CLI 与 console 共用 runner；校验保存的 HTTP 身份，不能恢复的旧绑定明确报错。 |
| `internal/legacyruntime/runtime.go` | 只启动当前 Go 可执行文件；父会话拥有 TLS/代理/CookieJar、请求预算和取消；严格处理二进制正文、cookie RPC、闭集错误码和进程树清理。 |
| `internal/legacyruntime/challenge.go` | 父子消息保持 source/origin，graph 字段透明传递，支持初始消息队列、双向 live transfer 和取消；不会将取消或脚本退出当作验证成功。 |
| `internal/jshost/` | Sobek、realm/WindowProxy、DOM/事件、动态脚本、事件循环、fetch/XHR、Workers、Storage 和有限 Web Platform；支持边界见 [宿主要求](host-requirements.md)。 |
| `scripts/run-legacy.sh` | 运行 Go 程序，拒绝被移除的外部宿主参数。 |
| `scripts/local-macos.sh` | 只部署 Go 文件；升级先等待本实例进程退出，再替换文件和清理旧 helper；保留原 data/Keychain namespace。 |
| `scripts/local-legacy-macos.sh` | 继续使用 legacy 实例标识与默认端口 4401；`stop` 保留数据，`clean --yes` 只清本实例。 |
| `cmd/build-web` / `scripts/build-frontend.sh` | 使用 Go esbuild API 与 `web/vendor` 中浏览器生产库构建 React；无 Node/npm/pnpm/Vite。 |
| `scripts/build.sh` / `build-cross.sh` | 禁用 cgo 构建；React 静态资源嵌入二进制，部署后无需编译工具。 |

worker 使用私有 stdin/stdout JSON 消息，错误与 stderr 不输出脚本私密内容；kind 限定为 sentinel/parent/child。运行时不接受任意用户脚本路径作为公开执行命令，下载的动态 JS 经现有认证接口进入。Web Worker 是已隔离认证进程内的独立 Go 引擎/事件循环。

checkpoint 继续绑定实际 Go HTTP profile、UA、代理和 Cookie，并保留设备身份。不静默改绑会话或重放已接受的验证码/OAuth code。旧 `curl_cffi:` 与 browser-bound 登录 checkpoint 要求显式重新登录；`callback_ready` 的独立 PKCE 授权交换清除旧 Cookie 后继续；`tokens_received` 和待保存的 refresh grant 优先离线恢复，避免重复消费授权。

当前部署与最后验收见 [本地部署](local-deployment.md)。`final-verification.md`、`baseline.md`、`production-fixture-verification.md` 是清理前宿主实现的历史证据，不是当前模式的安装步骤或本轮测试结论。
