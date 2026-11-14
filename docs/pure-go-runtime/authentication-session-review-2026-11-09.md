# 登录会话移除后的三轮严格复审（2026-11-09）

本轮接续 `authentication-session-removal-2026-11-08.md`，以现有工作树和未提交修改为准。进行了调用链复查、可复现的反例测试与修复、独立交叉审查三轮。没有 reset、clean、整文件回退、部署、替换二进制、重启服务或调用真实账号。未引入会话恢复框架。

主要修改位置为 `internal/authmaterial/export.go`、`internal/logging/redact.go`、`internal/protocol/{oauth,token_checkpoint,totp_reset}.go`、`internal/jobs/{manager,restore,oauth_result}.go`、`cmd/tosub2/{main,refresh_recovery}.go`、`internal/console` 的输入/导出边界与前端、`scripts/session-cleanup` 及相应测试。本轮没有新增依赖。

原仓库参考仍为本地 `origin/main` / `HEAD` 的 `bb5259c2286556a6b827d2a68341b1f3d4ed43f4`。本轮没有把本地引用称作已核实的远端最新版本，也未回退原版本中的 Cookie/checkpoint 机制。

## 发现并修复的问题

| 范围 | 已复现的问题 | 修复及行为验证 |
| --- | --- | --- |
| 会话导出边界 | JSON 重复 `extra` 字段经 map 解码覆盖后，检查遗漏了原始文件中仍存在的 Cookie | 逐 token 检查原始 JSON，拒绝重复、损坏、多个 JSON 值和过深结构；单项/批量下载拒绝原始污染文件，源文件保持不变 |
| 编码和旧字段 | 完整 JSON 字符串、旧 `initial_login_progress` / `reauthentication_previous_checkpoint` 和实际 `oai-client-auth-session` 容器可能绕过检查 | 检查完整编码容器及明确旧认证字段；正常 token、因子 `session_id` 和普通业务 state/checkpoint/fingerprint 仍可导出 |
| 日志 | 多行 Cookie/header 容器只遮住首行；转义键、单引号和未引号的容器别名存在漏遮 | 消费完整 JSON 值；非标准容器也统一按 `SensitiveKey` 判断；截断私密容器保守隐藏余下片段。验证所有 canary 都不输出 |
| API | 重复 JSON 字段、大小写别名和不同命名形式的已废弃参数可能静默降级成新操作 | 保留原 2 MiB 和单 JSON 请求限制，入队前拒绝歧义字段、旧恢复参数与操作；断言任务和队列没有变化 |
| 当次交互 | 兼容 `Manager.Input` 重开结束任务、自动重登入队时可能带入上次验证码 | 只保留这次明确提供的输入；当前等待或已排队的本次输入仍可正常消费。入队保存失败还原原输入、结果标识和队列 |
| OAuth 本地保存 | grant 与 output 同一文件或硬链接可在保存后误删输出；旧调用可能删掉更新的已取得 grant | 登录前检查路径冲突；本地输出前和清理前检查完整 grant 一致性；持久写入和逐字节读回后才删除 grant。失败保留已取得 token |
| 身份与响应 | 重复身份字段可掩盖账号冲突；access token 与 ID token 邮箱冲突可能改变导出账号；2xx 同时含 error 可能被当作成功 | 拒绝歧义 JSON 和明确账号冲突，保留 opaque token 兼容；矛盾响应拒绝成功。完整响应附带晚到传输错误时保留实际已取得 token，未知/不完整响应不重发 |
| refresh 补保存 | 轮换结果的本地输出缺少读回；双 token 身份冲突可能被偏好的 token 掩盖 | 输出可靠写入并读回后再移除轮换凭据；身份冲突保留已取得 grant，不再次 refresh、不重新登录。两次调用断言只有一次远端 refresh |
| 前端本地完成 | 已取得 token 的补保存仍可能携带全局代理或切换授权方式 | 仅提交该结果绑定的授权方式；不附带代理、上传选项，保持本地完成的零网络边界 |
| 离线清理工具 | 重复字段可在净化中丢弃业务结果；无效 JSON、符号链接、未知容器可能静默跳过；Base64 回执存在重复字段隐藏副本风险 | 原始数据先检查再分类；明确报告不能处理的项目，不猜测删除；编码回执也先检查原始 JSON。测试断言未分类文件逐字节不变且报告不含私密值 |
| 混合凭据保护 | OAuth/checkpoint 非预期类型或 JSON 字符串内的合法 token 可能被误认为纯会话 | 拒绝未知容器结构；保护编码中的业务凭据；无法可靠分类时报告，不删除、不伪造完成 |

独立交叉审查补充的日志别名、Base64 重复字段和编码凭据反例均已纳入测试。校验没有扩展成按任意 `session` / `pending` 名称批量删除。

## 已取得结果的操作归属

新独立操作使用自己的纯业务结果标识和 `oauth-grant-<结果标识>.json`，任务 metadata 在入队时保存其选择。Retry/Restore 只识别当前操作的已取得结果，防止一次新登录失败后误把旧 token 宣告为新登录成功。

旧 token 结果留在原结果路径，可通过明确指定文件的 `--complete-oauth-grant` 纯本地保存。新登录不会覆盖或搬移旧 token，新的授权方式也不受旧登录 checkpoint 阻挡。结果标识不携带 Cookie、code、PKCE、登录阶段或认证会话，不能续接未完成登录。

## 数据复扫和保留边界

用强化后的离线清理工具复扫：

- `~/Library/Application Support/toSub2-local/6428e6fb3abbc4ae-legacy`，包含运行数据与备份、归档、rollback 等历史目录。
- `~/Library/Application Support/toSub2-local/6428e6fb3abbc4ae`。
- `/tmp/tosub2-auth-only-review-nyzu39p0`。
- 项目专用临时目录 `TestResetTOTPActivationVerificationFailurePreservesNewCredential3762950527`。

复扫结果为 **零待清理项、零无法分类项，1,334 个凭据/正常授权文件检查前后相同**。本轮没有新增会话文件需要删除；上一轮实际删除 99 个会话文件、净化 18 个混合记录的结果见前一份报告。本轮只含路径和计数的复扫记录为 `/tmp/tosub2-session-review-2026-11-09-scan.json`。

工作区 `tmp` 为空；`~/Library/Caches/toSub2` 再次检查仅含普通 `.lock` 账号锁，未删除这些互斥文件。

正常授权 token、密码、2FA 密钥、账号业务资料、成功证据、普通账号锁、未知远端修改的隔离信息仍保留。首次设置/重置明确成功后直接推进，不增加确认 GET；当次未知结果的有界核实和连续四轮完整模拟重置用例继续纳入整仓验收。

HAR 只读且未导入任何认证材料，SHA-256 仍为 `212494dce20dcd1a218269cce5ccdd64673d8f936810949fa931dc72b92880ce`。原有工作区和运行二进制哈希均与上一轮相同。

PID **6628** 仍运行 legacy 目录中的旧 `bin/tosub2`。未替换、停止或重启。它仍可能重新生成旧会话文件；磁盘复扫只说明检查时状态，不能声称运行实例或其内存会话已清理。旧构建产物保留，未自动部署。

## 验证记录

专项测试已覆盖上述反例，包含失败时文件不变、请求次数、零网络本地完成、无重复远端修改及未入队断言。前端已从现有源码重建到两个 dist 目录。

所有测试使用本地模拟服务器与合成凭据；普通整仓与 race 套件串行运行，避免相同测试账号的跨进程锁竞争。

| 检查 | 结果 |
| --- | --- |
| `go test -count=1 ./...` | 通过，27 个含测试包 |
| `go test -count=1 -race ./...` | 通过，27 个含测试包，无竞态报告 |
| `TOSUB2_LEGACY_RUNTIME_TEST=1 go test -count=1 -race ./internal/jshost ./internal/legacyruntime ./cmd/tosub2` | 通过，包含本地脚本 worker、完整登录和纯本地结果完成路径 |
| `go vet ./...`、`go build ./...` | 通过，没有写出部署二进制 |
| 前端 `./scripts/build-frontend.sh` | 通过，两个 dist 的 4 个文件逐字节一致 |
| `gofmt -l cmd internal scripts`、`git diff --check` | 通过 |
| 强化离线清理工具最终复扫 | 零待处理、零无法分类，1,334 个凭据与授权文件保留 |

本轮日志保存在 `/tmp/tosub2-session-review-2026-11-09-test.log`、`/tmp/tosub2-session-review-2026-11-09-race.log`、`/tmp/tosub2-session-review-2026-11-09-runtime.log`；只包含本地测试结果。
