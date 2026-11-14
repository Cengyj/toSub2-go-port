# 登录会话持久化与恢复移除验收（2026-11-08）

以 `/Users/ceng/Downloads/toSub2-go-port` 的当前工作树为准。开始时已有 604 条 Git 状态记录，涉及账号页面、网络、2FA、凭据及其他功能；未 reset、clean、整文件回退、提交或推送。本次既删除执行机制，也实际清除指定项目运行目录及历史副本中的认证会话。

后续三轮严格审查、操作独立的纯 token 结果路径及复扫结果，见 [2026-11-09 复审报告](authentication-session-review-2026-11-09.md)。下文保留本次执行时的历史记录。

## 参考与证据

- 原仓库参考：本地 `origin/main` 与 `HEAD` 均为 `bb5259c2286556a6b827d2a68341b1f3d4ed43f4`，标题为 `Harden account workflows, refine usage controls, and update BPS models`。直接读取该提交的登录、OAuth 和 2FA 源码对照业务顺序。连接 GitHub 失败，不能声称已核实远端最新 HEAD。
- 原版本身的任务级 Cookie checkpoint、OAuth 待兑换 code 和网页登录导出也在删除范围。没有把原版本作为整仓回退目标。
- HAR：只读 `/Users/ceng/Downloads/chatgpt.com.har`，清理后 SHA-256 仍为 `212494dce20dcd1a218269cce5ccdd64673d8f936810949fa931dc72b92880ce`。不导入任何认证材料。
- HAR 第 959 项停用响应为 HTTP 200，明确 `Factor disabled`，附加同步字段为 null。第 960 项 `mfa_info` 是随后一次页面状态读取；它不是明确停用成功后的后端强制确认依据。HAR 不包含新登记/激活样本，这些契约沿用源码和合成测试的明确形状。

## 代码与入口

| 范围 | 最终行为 |
| --- | --- |
| `internal/protocol/flow.go`、`recovery.go`、`checkpoint.go` | 每次新操作清空认证上下文并正常登录；登录阶段仅在当前调用内存中存在。旧 checkpoint 读写/反序列化/Restore 只拒绝旧输入，不能读取或导入磁盘内容。 |
| Cookie 与传输 | 删除完整 Jar JSON 导出/导入、响应 DTO 中整份 Cookie 快照及旧传输绑定恢复代码。保留当次内存 Jar、HTTP/JS Cookie 规则和网络重试；TLS 候选隔离不是认证探活或登录跳过。 |
| `internal/protocol/token_checkpoint.go` | `oauth-grant.json` 只保存已取得 token 和账号/client/redirect/origin 绑定。无 Cookie、code、PKCE 或登录中间状态。纯本地保存入口没有网络传输对象。 |
| `cmd/tosub2`、`internal/jobs`、`internal/application` | 删除从旧任务检查点恢复登录、旧 Cookie 候选、失败回退会话和 callback 跨进程兑换。新操作不受旧检查点授权方式影响。任务恢复保留账号和结果，不能恢复未完成登录。 |
| CLI/config | 删除 checkpoint、resume-checkpoint、session 输出路径、WebOnly 等可执行入口；输出仅正常 OAuth 授权文件。新增 `--complete-oauth-grant` 仅提交已取得 token 的本地结果。 |
| Console/API/前端 | 旧远端 `recover-2fa`、`inspect-2fa`、`reauthenticate` 及旧认证输入不可执行；旧输入即使为 null/false 也不能降级成新操作。`complete-2fa` 保留为严格零网络本地保存。 |
| SMS | 删除旧待投递验证码、原始已读验证码集合的落盘与跨进程重投。保留当次内存交互，以及供应商订单/API 凭据等业务资料；不自动续接旧登录收码。 |
| 导出/刷新/日志 | 拒绝带明确认证会话结构的混合授权导出，避免通过下载、上传、刷新写回另存旧材料。正常 token 与其他业务字段保留。2FA 回执只保存被解释的必要业务字段，不保存任意响应原文或未知扩展。 |
| 文档与构建资源 | 当前文档改为内存登录与纯本地结果保存；历史审计明确标记为历史。两份前端 dist 由当前源码重新生成。 |

共享 AccountSessionStore、Cookie owner/dirty/保存屏障和独立远端 2FA 恢复体系在开始时已从当前源码删除；本次复核没有重新引入。`go.mod`、`go.sum` 没有新增依赖，现有网络与 JS 宿主依赖仍有正常用途。

## 实际数据清理

只扫描本项目工作区、启动脚本派生的两个实例目录，以及查明与本项目相关的临时目录。未扩展至其他项目、浏览器个人资料、HAR 或通用系统资料。

清理工具 `scripts/session-cleanup` 无网络、无启动服务、无账号登录。先解密辨认已有格式；删除纯会话容器；对混合 2FA journal 仅移除 Cookie、嵌套登录 checkpoint、会话引用及重新认证续接材料，原位加密写回并解密读回核对。没有制作旧会话备份，没有移动或改名保存旧文件。

| 实际位置 | 删除纯会话文件 | 净化混合 2FA 记录 |
| --- | ---: | ---: |
| `~/Library/Application Support/toSub2-local/6428e6fb3abbc4ae-legacy/data` | 16 | 0 |
| 同一 legacy 实例的 backup、archive、rollback、maintenance 等历史目录 | 79 | 17 |
| `~/Library/Application Support/toSub2-local/6428e6fb3abbc4ae/data` | 1 | 0 |
| `/tmp/tosub2-auth-only-review-nyzu39p0/candidate` | 2 | 0 |
| 项目遗留的 `TestResetTOTPActivationVerificationFailurePreservesNewCredential3762950527` 专用临时测试目录 | 1 | 1 |
| 合计 | **99** | **18** |

删除类型包括 account-session 加密快照及活动指针、login-checkpoint、认证候选 checkpoint/cookie-snapshot，以及测试遗留会话。另删除 43 个已空的会话目录。表中最后一行是合成测试材料，未混称为真实账号会话。

保护与核对：

- 1,334 个已有 `.cred` 与正常 OAuth 授权文件清理前后逐字节一致。
- 保留密码、合法 token、2FA 种子、账号/操作/因子绑定、必要登记 session_id、成功证据和未知操作隔离信息。登记 session_id 仅作为因子绑定证据；没有恢复认证或重发操作的入口。
- 实际旧登录 checkpoint 没有已取得 token；account-session 的 login 子对象也没有 tokenResponse，不需要转移已授权业务结果。
- 已净化的历史 journal 不伪造成功或提交标记；未知远端结果仍要求人工处理。
- 19 份剩余加密 TOTP 业务结果均重新解密扫描，包括回执编码内容；会话清理复扫为零待处理、零无法分类项。
- 扩展扫描 9,972 个相关文本文件，未发现 Cookie 请求头、登录日志标识/PKCE/回调认证材料。工作区 `tmp` 为空；`.local-run` 是历史部署说明。
- 专用临时源码归档中仅发现合成 golden fixture 的拒绝测试输入，没有真实登录数据；没有删除源码历史或正常测试夹具。
- 实例及历史 metadata 无 SMS 供应商记录，也没有匹配的 SMS 凭据文件，因此本次没有删除供应商订单或 API key。
- 项目实际使用的 `~/Library/Caches/toSub2` 中只有普通账号互斥 `.lock` 文件，无 Cookie/会话缓存；按要求保留这些锁文件，不破坏跨进程互斥。

可复查的本次工具输出在 `/tmp/tosub2-session-removal-applied.json` 和 `/tmp/tosub2-session-removal-verified.json`；只含路径、计数和验证状态，不含认证值。清理工具对无法可靠分类的混合业务凭据会报错保留，不擅自删除；本次实际运行没有这类未清理项。

## 保留的正常能力

- 当次登录、密码/邮箱验证码/TOTP 等待继续、取消、超时、有界重试。
- 当前账号核对、OAuth state/PKCE/client/redirect 校验。旧登录材料不能阻挡独立新操作，也没有删除当前交易的必要校验。
- 正常 OAuth token 文件、凭据库及正常 refresh。已经取得的轮换 grant 可以零网络补保存，不再 refresh。
- 首次设置和重置的完整正常链路。明确停用成功后登记，材料完整且登记成功后激活，明确激活成功后保存并完成；不追加确认相同修改的 GET。
- `/api/auth/session` 只作为本次登录后的账号身份/访问令牌来源使用，不作为旧 Cookie 探活。初始因子读取、修改结果未知时的有限只读核实仍保留。
- 请求可能已经发送时不盲目重发。核实成功必须匹配当前账号与本次因子；登记材料丢失不能用再次登记掩盖；只有明确验证码错误可以重试验证码。
- 激活结果必须有真实 business_receipt 或 state_read 来源，密钥先保护再激活，可靠保存后读回核对。确认激活但本地保存失败可完成本地提交，无网络、登录或远端修改。
- 普通账号锁、并发互斥、同意图去重、在途/未知/未提交隔离。已完成操作允许明确再次发起重置，不设次数限制。

## 验证

验证使用本地模拟 HTTP 服务、合成账号/密钥和现有脱敏夹具，未调用真实账号。

核心行为覆盖：

1. 完整登录和问码保持内存，旧文件不读取、不探活、不续接；Cookie/登录进度无法序列化。
2. 离线清理测试实际删除检查点及副本，保留混合 journal 的密钥、token、密码、因子与证据；正常授权文件字节不变。
3. 首次设置/重置明确成功路径断言请求顺序和数量；额外查询即使设置为失败也不发起。
4. 已知响应变体继续，矛盾/空/重复字段/缺少必要材料不误判成功；网络/解析异常不当作验证码错误。
5. 响应丢失时仅在当前会话内核实对应因子，不重复停用、登记或激活；过期会话不重新登录续接远端修改。
6. 2FA、已取得 OAuth token 与轮换 refresh grant 的本地补保存零网络。
7. 四轮同账号、同任务的完整真实模拟 HTTP 重置；每轮新的登录、因子、密钥、登记绑定和 operation ID。每轮停用/登记/激活各一次、只有初始状态 GET；在途及完成后的同意图重复提交均不重做。
8. API 旧入口/旧输入拒绝，任务与队列不变；混合认证导出不能下载、上传或通过刷新回写。
9. 2FA 回执含 Cookie/回调/PKCE canary 时不落盘；过滤后保持成功、失败、未知、矛盾和验证码重试语义。

| 检查 | 结果 |
| --- | --- |
| `go test -count=1 ./...` | 通过，26 个含测试包 |
| `go test -count=1 -race ./...` | 最终独立运行全部通过，无 data race 报告 |
| `TOSUB2_LEGACY_RUNTIME_TEST=1 go test -count=1 -race ./internal/jshost ./internal/legacyruntime ./cmd/tosub2` | 通过，包含本地脚本 worker 与完整认证运行路径 |
| `go vet ./...`、`go build ./...` | 通过；未写出部署二进制 |
| 前端构建 | 通过，`web/dist` 与嵌入 dist 的 4 个文件逐字节一致 |
| `gofmt -l cmd internal scripts`、`git diff --check` | 通过 |
| 清理工具及回执投影补充 race 回归 | 通过，包含 pending 业务凭据、大整数保真与空白错误语义 |
| 历史数据最终复扫 | 零待清理项、零无法分类项；1,334 个凭据/授权文件保留 |

最初同时运行普通整仓测试与 race 套件时，两套 application 测试使用相同合成账号，竞争用户级账号锁，造成模型检测的两项 busy 断言失败。完成信号之前已同步释放本进程租约，不是生产锁释放顺序缺陷。单独 application race 通过；最终整仓 race 单独执行，未为绕过检查而改变生产锁或测试断言。

## 运行实例与未部署边界

没有停止、重启、部署、替换或执行旧服务二进制。核查时 PID **6628** 仍运行：

`~/Library/Application Support/toSub2-local/6428e6fb3abbc4ae-legacy/bin/tosub2`

该旧进程的内存不受本次磁盘清理控制，旧程序仍可能重新生成 Cookie/检查点。磁盘清理与当前源码验收不能表述成运行实例已升级或内存会话已清除。最终复扫反映检查时状态，不保证旧实例后续不再写入。

既有构建产物保留：工作区 `bin` 有 24 个可执行文件；legacy 实例及其历史目录有 97 个可执行文件，另有早期专用临时构建产物。它们未替换，也不能视为已采用新代码。构建检查使用 `go build ./...`，不调用会覆盖 `bin/tosub2` 的 `scripts/build.sh`。

仍需用户自行安排部署才能让长期运行实例采用新代码。结果未知的旧远端 2FA 操作没有自动恢复入口；这类业务隔离数据和凭据按要求保留。本次没有验证真实账号或真实上游端点行为，也没有声称完成非本机平台运行验收。
