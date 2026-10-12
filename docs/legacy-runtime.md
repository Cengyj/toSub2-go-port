# 原版认证流程的 Go 运行方式

更新：2026-10-12。当前唯一认证实现为 Go HTTP/TLS 与 Go 脚本宿主。Node、Python、Chromium 自动化和旧运行选择参数均已移除。普通网页控制台仍使用浏览器查看。

## 构建和启动

Go >=1.25.0：

```sh
make build
./bin/tosub2 --host=127.0.0.1 --port=4399
```

构建前端使用 Go esbuild API；生产运行使用编译好的 Go 程序，不提供宿主选择或外部运行时回退。

macOS 隔离测试实例默认端口 4401：

```sh
./scripts/local-legacy-macos.sh start
./scripts/local-legacy-macos.sh status
TOSUB2_LOCAL_REBUILD=1 ./scripts/local-legacy-macos.sh restart
./scripts/local-legacy-macos.sh stop
./scripts/local-legacy-macos.sh clean --yes
```

前端有变化时先执行 `make frontend`。升级脚本先确认旧实例及其 worker 已退出，再替换二进制并清除该部署内的旧辅助副本，保留账号资料。`clean` 删除整个实例及专属 Keychain 条目；外部备份保留，详见 [本地清理范围](local-macos.md)。

| 参数 | 环境变量 | 当前作用 |
| --- | --- | --- |
| `--auth-tls-profile` | `TOSUB2_AUTH_TLS_PROFILE` | 默认 `chrome146` |
| `--sentinel-sdk-url` | `TOSUB2_SENTINEL_SDK_URL` | 动态 SDK loader |

## 会话和恢复

Go 的 `tls-client`、`fhttp` 与 uTLS 处理认证 HTTP、TLS profile、代理、Cookie、SDK 下载及脚本网络桥。认证请求与动态脚本共用一个 Go TLS 客户端、CookieJar、代理和设备身份。Cookie 更新同步到 checkpoint，重定向由流程显式处理。每个 HTTP 请求独立处理暂时性网络错误，默认首次尝试后最多重试三次；写操作及一次性授权回调仅在可靠确认请求尚未发送时重试，HTTP 拒绝和明确业务错误不按网络波动重放。

认证子进程执行同一个 Go 可执行文件，通过 Sobek 运行动态 JavaScript。父进程处理 NDJSON 网络桥、二进制响应、截止时间与进程树清理；私有输入以 0600 临时文件传入。部署的 TMPDIR 位于专属运行目录，退出或取消后删除临时文件。诊断只输出阶段、计数和受限错误码。

默认 Chrome146 profile 是 Go TLS 指纹名称，不启动 Chrome。其兼容调整包括 TLS 扩展随机顺序、移除原版未发送的 trust_anchors、Chrome HTTP/2 头顺序、继承默认请求头和 zstd。额外请求头按字母排序，尚未完全保留原版调用方插入顺序。

父子 frame 在专用 Go 宿主中处理异步消息和 ready/execute 握手。失败报告保留 context 取消/超时原因，不包含动态脚本、完整 URL、Cookie 或远程异常内容。宿主并非完整 Web 浏览器，支持范围见 [宿主需求](pure-go-runtime/host-requirements.md)。

新 checkpoint 绑定 `tls-client:chrome_146` 等实际指纹。中间会话恢复要求当前 Go 身份绑定；无绑定或旧运行时绑定的 checkpoint 需显式重新登录。旧文件保留，有效 refresh token、callback code 和已保存授权结果继续按 Go 恢复流程处理。验证码明确错误响应允许输入新值；会话失效、权限、挑战、限流和服务器错误分别返回对应错误。

## 验证与剩余差距

```sh
go test ./...
go test -race ./internal/jshost ./internal/legacyruntime ./internal/protocol
TOSUB2_LEGACY_RUNTIME_TEST=1 go test ./cmd/tosub2 -run TestLegacyRuntimeProductionLoginAndResume -count=1
```

测试使用本地合成 HTTP/TLS/SDK 和验证码服务。本地通过不能替代当前线上挑战、真实账号或第三方服务验收。旧实现与其历史对照记录仅在外部备份中保留。

| 功能 | 当前状态 |
| --- | --- |
| HTTP/TLS、Sentinel/挑战桥接、进程管理 | Go 实现 |
| 动态 JavaScript 执行 | Sobek Go worker |
| 批量只导入，显式开始操作 | 已实现 |
| 账号行常用按钮、更多菜单与批量工具栏 | 已实现 |
| 2FA 重置、可选退出全部设备及后续重登上传 | 已实现；[流程与恢复行为](pure-go-runtime/totp-reset.md) |
| 重登授权成功后自动上传 Sub2API | 已实现；更新已有账号授权时保留其远端配置 |
| 查看账号资料卡及原始信息导出 API | 已实现；重置成功后读取当前保存的新 2FA 密钥 |
| 套餐徽章与按套餐排除自动重登 | 已实现；[识别与排除规则](pure-go-runtime/account-plans.md) |
| 当前远程挑战覆盖 | 公开 SDK 与合成 fixture 有证据，完整线上流程仍需验收 |
