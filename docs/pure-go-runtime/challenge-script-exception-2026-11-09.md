# 2026-11-09 网站验证脚本异常审查

## 结论与证据

本次日志对应操作 `2026-11-09T01:04:49.840984Z`，任务
`job-9357670cfde24ce99d97`。只读取该任务的阶段、状态、时间及成功标志，
没有发起真实账号登录、验证、刷新 token、退出设备或修改 2FA。

本地任务记录位于
`/Users/ceng/Library/Application Support/toSub2-local/6428e6fb3abbc4ae-legacy/data/job-meta.json`。
以下时间为北京时间（UTC+8）：

| 时间 | 记录 | 含义 |
| --- | --- | --- |
| 09:05:03 | `totp_reset_read` | 进入 2FA 状态读取 |
| 09:05:05 | `totp_reset_disable` | 执行旧因子移除步骤 |
| 09:05:07 | `totp_reset_enroll` | 登记新因子 |
| 09:05:10 | `totp_reset_complete` | 新因子激活及保存完成 |
| 09:05:12 | `totp_logout_succeeded` | 退出所有设备成功 |
| 09:05:13 | `totp_workflow_relogin` | 开始后续重新登录授权 |
| 09:05:25 | `oauth` | 进入 OAuth 授权 |
| 09:05:26 | `legacy_challenge` | 收到网站验证页，执行验证脚本 |
| 09:05:29 | `legacy_challenge / failed` | Go JavaScript 运行时异常，授权未完成 |

当前记录仍保留 `credential_persisted`、`activated`、
`totp_reset_committed=true`、`totp_activation_verified=true`、
`totp_recovery_pending=false`。`result_saved=false` 对应本次新授权文件未取得，
不代表新 2FA 密钥未保存。当前操作字段可能已被后续独立操作更新，
上述历史时间线按原始 `operationAt` 精确匹配。

## 原错误各字段的真实含义

- `SCRIPT_EXCEPTION` 是封闭错误分类。普通 `Error`、自定义异常名或抛出值都可能
  得到此码；它不是原始异常正文，也不能据此断言缺少哪一个浏览器 API。
- `stage=parent_execution` 表示父验证页处于执行阶段。
  `frame/child/init/extraParams/execute=false` 仅表示相应事件尚未被观察到，
  不表示没有运行 JavaScript。
- `requests=2 last_http=200` 表示已处理两个运行时请求、最后一个响应状态是 200。
  它不表示网站验证通过或 OAuth 成功。
- 原 `parent_errors=0` 不可信：致命异常直接输出 failure，不输出 final 错误列表；
  此外，工作进程先退出时，发送响应的路径会直接返回缓存的终止异常，
  绕过 failure 事件分支。
- `verification [REDACTED]` 是固定文案 `verification failed` 被验证码脱敏规则
  误匹配。不是服务端返回了一段被隐藏的验证码。
- 原日志仍标记 `operation=reset-2fa`，因为日志保留整个工作流的操作身份；
  后续重登使用任务副本，不会改写原操作 ID。这不能将后续授权失败解释为重置失败。

旧日志没有保留具体 JavaScript 出错位置；本次不能证明真实脚本故障是哪一个
API、语法或服务端分支引起。也没有证据证明它由删除 Cookie 持久化直接导致。
当前流程仍为新操作创建内存 CookieJar。

## 本次源码修复

1. `internal/legacyruntime/challenge.go`：固定文案改为
   `challenge execution failed`；按父/子运行时记录致命异常，覆盖直接 failure、
   终止事件、网络/脚本响应写回及父子消息转发时发现的终止错误。
2. `internal/jshost/protocol.go`、`runtime.go`：在宿主执行边界添加封闭 `phase`
   枚举，例如 `initial_script`、`dynamic_script`、`timer_callback`。
   只输出固定分类，不输出远端错误正文、堆栈、URL、脚本或认证材料。
   `phase` 是宿主捕获边界，不是精确脚本语句位置；跨 JavaScript 桥的既有异常
   转换行为和脚本可见错误文案保留，不从脚本可伪造的错误字符串反解析阶段。
3. `internal/legacyruntime/runtime.go`：对收到的阶段再次进行白名单校验，
   保持安全错误类型，拒绝未知阶段；兼容没有阶段字段的既有工作进程消息。
4. `internal/jshost/workers.go`：通过 `errors.As` 识别包装后的安全诊断，
   防止 Worker 的 `SCRIPT_TYPEERROR` 被误降级为泛化异常；未知代码仍拒绝输出。
5. `internal/application/totp_workflow.go`：仅在通过已有密钥保存门槛且进入重登后，
   为返回错误附加“新 2FA 已保存；后续重登授权失败”。保留底层 cause、
   整体失败状态、成功证据、操作身份及账号锁。

没有吞掉脚本异常、将 HTTP 200 判为验证成功、绕过当次验证、增加远端修改重试，
也没有恢复 Cookie 落盘、检查点、旧会话续接或远端 2FA 恢复能力。

## 本地验证

合成页面分别在初始脚本、外链脚本、定时器回调中抛出同一个普通异常，
验证三种场景在相同 `SCRIPT_EXCEPTION` 下能得到不同捕获阶段。
逐项断言请求次数、父子错误归属、临时输入删除、认证材料不泄露、失败不被当作成功，
以及已消费的挑战响应不能用于第二次操作。

覆盖工作进程已退出、failure 消息还在排队时写回响应的竞态；
覆盖子页面致命异常、Worker 错误监听与取消默认行为、未知诊断字段拒绝。

2FA 工作流模拟断言顺序为重置 → 退出设备 → 重登；重登失败不上传，
不重做已完成步骤，不修改保存的合成密钥，保留 `errors.Is/As`。
保存前失败和独立重登均不会误报“新 2FA 已保存”。

聚焦诊断测试已连续三轮通过；相关工作流测试及其竞态检查通过。
全仓 `go test ./...`、`go vet ./...`、`go build ./...` 通过。
全仓测试后对 JavaScript 可见错误文案又做了保持旧行为的收紧，单独补跑相应回归。
最终 `jshost`、`legacyruntime`、`application`、`logging` 的相关 `go test -race`
回归均通过；收紧后的构建、静态检查及 `git diff --check` 通过。

## 运行实例与后续边界

审查时本机 `127.0.0.1:4401` 仍由 PID `66018` 监听。
本次未替换运行二进制、未部署、未重启服务，源码修复尚未进入该运行实例。
未修改真实账号数据，未读取或导入 HAR 认证材料，未保存真实验证脚本副本。

本次以当前未提交工作区为准；本地 `HEAD` 和 `origin/main` 均为
`bb5259c2286556a6b827d2a68341b1f3d4ed43f4`，没有将原仓库覆盖到工作区，
也没有声称该本地引用是远端最新版本。

本次已修复能够复现的诊断和错误语义问题，尚未证明真实网站验证能够成功。
后续应依据增强后的捕获阶段和可脱敏复现定位具体运行时兼容问题。
该任务的 2FA 已完成，不应为这条错误重做整套重置；需要新授权时，应由用户明确
发起现有“重登授权”，建立新的当次操作，继续服从原有锁和凭据保护。
