# 2FA 临时 Cookie：HAR 与改造前源码独立核验

> 历史记录：本文描述当时版本，涉及 Cookie 持久化、登录 checkpoint 或远端恢复的能力现已移除。当前行为与清理范围见 [登录会话清理](authentication-session-removal-2026-11-08.md)。

> 全项目范围纠正（2026-11-08）：本文涉及的 AccountSession、Cookie 快照持久化、共享会话与 owner/dirty 机制也是后来新增，现已纳入整体清理；不能再把它们作为“原版普通会话”保留。当前边界见 [全项目清理记录](cookie-persistence-cleanup-2026-11-08.md)。下文仅作历史追溯。


> 历史记录（已被后续清理取代）：本文描述的独立核实、远端恢复、重认证或专项 Cookie 方案不再是当前功能。当前实现以 [设置与重置 2FA](totp-reset.md) 为准；保留本文仅供审计追溯。

日期：2026-11-07，Asia/Shanghai。本报告是本轮只读事实审查；源码锚点指审查时工作区函数，而不是历史结论。最终改造、故障注入和全仓验收由实施记录另行汇总。

## 方法与边界

- 阅读当前 README、首次登录、停用快速路径、严格审查及多轮审查记录，再重新核对真实源码。工作区与上级目录未发现适用的 AGENTS.md。
- Python 标准库只读解析指定 HAR：73,612,041 字节、963 条 entries，索引从 0 开始。仅输出端点路径、状态码、结构、数量及不敏感常量。没有打印真实账号/因子 ID、访问令牌、认证头、Cookie 或整份 HAR。
- 因子关联仅在内存比较并输出布尔结果；静态 JS 只提取固定接口片段及字符偏移。没有导入、重放 HAR 会话或访问真实账号安全接口。
- **Cookie 仅属于当前重置且在规定节点清理，是用户的产品要求，不是 HAR 推导。**

## 网络证据

| Entry | 北京时间 | 请求/结果 | 可证明范围 |
| --- | --- | --- | --- |
| #949 | 02:21:44.244 | GET `/backend-api/accounts/mfa_info`，200；两个 enabled 字段 true；TOTP 数量 1；native_default_factor_id 非 null | 此时抓包账号有已启用 TOTP |
| #959 | 02:22:08.914 | POST `/backend-api/accounts/mfa/user/disable_in_house`，200；JSON 只有 factor_id，与 #949 因子匹配；响应 message=Factor disabled、passkey_credential_manager_sync=null | 此次完整响应明确确认所选旧因子停用 |
| #960 | 02:22:10.148 | GET 同一 mfa_info，200；两个 enabled 字段 false；TOTP 数量 0；native_default_factor_id=null | 此次停用后只读状态与成功响应一致 |

实际网络路径 `/backend-api/accounts/mfa/enroll`、`/backend-api/accounts/mfa/user/activate_enrollment`、`/backend-api/accounts/logout_all` 的出现次数均为 **0**。没有完整的新登记、激活和退出设备网络链。#559 `/api/accounts/mfa/issue_challenge`、#567 `/api/accounts/mfa/verify` 属于认证挑战，不能冒充新因子的登记和激活。

### 缺失字段的正确解释

- request Cookie headers、response Set-Cookie headers、非空 request cookies arrays、非空 response cookies arrays，四项计数均为 **0**。仅证明导出材料缺少 Cookie；不能证明服务端不会轮换，不能据此取消响应持久化。
- **593** 条 response.content 缺少 text；另有 **2** 条 text 明确为空字符串。必须区别缺失与空。
- #559/#567 都是 200 且 text 缺失、bodySize=-1，但 content.size 分别 **880/1531**。不能说服务端空响应。
- 三条状态/停用响应都有 text，但单次样本不能保证其他账号、状态或未来版本。HAR 没有给出近期认证有效期、Cookie 生命周期、退出传播时延或退出幂等重试保证。

## 静态前端证据

偏移针对解码后的 response.content.text 字符，而非压缩字节。

| Entry / offset | 捕获版本代码行为 | 证据限制 |
| --- | --- | --- |
| #943 / 40224 | TOTP POST `/accounts/mfa/enroll`，factor_type=totp、source；retry=false，staleTime=Infinity，gcTime=0，关闭 focus/reconnect refetch | 支持不自动重复登记的客户端意图，不保证服务端幂等 |
| #943 / 41567 | activate_enrollment 发送 factor_type、session_id、六位 code、source；检查 success 后刷新 MFA 查询 | 没有实际激活响应；不可替代项目中因子匹配、seed 保存及读回 |
| #943 / 56582、57506 | disable 发送选中 factor_id；onSuccess 刷新 MFA 查询并关闭窗口 | GET 用于界面刷新，不证明协议必须在完整停用成功后额外 GET；未知结果仍需核实 |
| #943 / 64235 | TOTP enable/disable 判断 Date.now()-pwd_auth_time 非负且小于 24e4，disable 还检查 MFA required=yes，否则官方重新认证 | 是前端 240 秒判断，不是服务端有效期保证 |
| #943 / 62741 | 同资源另有高级保护函数，把秒/毫秒归一后检查 <=3e4 和 MFA 标记 | 不应把不同上下文 30 秒判断与 TOTP 240 秒混淆 |
| #582 / 24652 附近 | bootstrap 白名单字段 reauth=password、max_age="0"、connection=password | 其他身份字段不输出；单 HTML 配置不是完整认证保证 |
| #944 / 62566 | Authenticator app 状态来自 factors.totp.length>0 | show_sms/show_passkey 是能力展示，不能代替启用证据 |
| #944 / 156834 | 设置页 POST `/accounts/logout_all` 后执行本地 logout | 无对应网络 entry，不知道真实 body 与传播语义 |

静态 text UTF-8 SHA-256：#943 `1b795f0df9da4f909eabc4f25bd3731b480a66bfd5813c68841e11fefd11109e`；#944 `156df7e927195d653e75f49139921fbf19147bca86e27ba5f214bd6ae7d560a8`。这是公开代码摘要。

## 改造前 Cookie 流向及缺口

| 数据/引用 | 来源、归属和保存位置 | 读取者与更新时间 | 原删除条件/缺口 |
| --- | --- | --- | --- |
| 主 Jar | runner 从普通账号会话/旧 checkpoint 恢复，或首登响应生成；内存 | protocol/transport，每次响应更新 | reset 禁用通用响应 hook，但仍有另一条 SaveCheckpoint 共享出口 |
| preparation_checkpoint | `<job>/totp-setup-result.json` 私有 journal；邮箱、operation、web 认证进度、transport | 原操作准备/initial 重试；先保存 credential baseline，之后响应 hook 和阶段写入 | account 确认且离开 initial 后转 recovery；未完成保持材料正确 |
| recovery_checkpoint | 同 journal，另绑定 account ID | Resume/Inspect、认证采用、local finalize、原 publication；每次响应及显式保存更新 | 原成功路径先共享发布再删除 journal，因此不是临时专用 |
| 首次登录 checkpoint | loginForTOTPReset 调 previousSave；main callback 调 AccountSession.SaveCheckpoint 或 WriteCheckpoint | 登录各阶段，包括密码后邮箱等待 | **已证实共享写出口**；禁用 EnableAccountSession 不影响它；删除 login-checkpoint 不清除共享 store/active |
| AccountSession/active | `<root>/account-sessions/<hash(email,origins)>/<hash(session,operation)>.json`、active.json | 普通 runner Load、ReadCheckpoint 的 CookieRef 解引用；通用响应 hook/SaveCheckpoint 更新 | 原 Load 无 reset purpose 拒绝；首次登录及成功发布副本可成为通用候选 |
| previous_session_ref/session_ref | journal 中旧来源与发布版本引用 | publication、兼容及身份校验 | 只删字段不够；不可粗删账号目录，须保护后来独立会话 |
| 独立认证候选 | `<job>/totp-reauth-checkpoint.json`，purpose=totp-reauthentication | 显式认证，独立 Jar、专用 SaveCheckpoint、in-flight/密码/回调/身份证明 | 已禁用原 SaveCheckpoint/输出路径/CookieRef；隔离基本正确，清理原依赖 handoff |
| 候选归档 | `<job>/totp-reauth-archive-<operation hash>-<attempt hash>.json` | 原操作拒绝/采用证明；轮换前密封保存、读回 | 字面遍历、验证所有归属再删除已有合理保护，需加入新终态收尾 |
| owner/cache/callback | reset owner journal bytes、reauth owner record、dirty 状态、hook closure；内存 | Flush、BeforeRequest 屏障；保存失败保留锁 | 需退休凭证和写屏障阻止清理后 late callback/dirty flush 重建 |
| 退出阶段 Cookie | 原 reset Jar 和 recovery_checkpoint | 仅当前原生 ResetTOTP 可选 logout；普通恢复禁用 | 原 BeforeLogout 先发布共享会话；终态后删 journal 仍留下共享 Cookie |

改造前源码锚点：main.go 的 runOperation（原 314–343、674–689、773–775、782–827）；totp_web_authorization.go 的 loginForTOTPReset；totp_reset_owner.go 的 attachResetCapture；totp_reset_session.go 的 saveTOTPResetSnapshot；account_session.go 的 SaveCheckpoint/Load/loadReference；account_session_handoff.go 的 PublishTOTPResetSession；cmd/tosub2/totp_reset.go 的 configureTOTPReset/finishTOTPResetHandoff。行号会随本轮修改移动，按函数核对。

## 逐项建议、证据和验证

| 项目/证据 | 现状与建议 | 收益/适用条件 | 回归要求 |
| --- | --- | --- | --- |
| #959；removePreparedTOTP | **保留**完整成功回执可靠保存后直接登记的快速路径 | 同次明确响应且保存成功时减少请求 | 禁止中间 GET；回执写失败禁止 enroll |
| #960；totpResetRequest/resetDispatch | **保留**未知结果只读核实、attempt/operation 回执和防重放 | 5xx/截断/断连不是明确拒绝；有限重试只适用 GET | 四轮预算、取消/保存失败停止、未知 mutation 不重放、进程退出 |
| #943 静态登记/激活；continueTOTPReset/verifiedResetActivation | **保留**distinct factor、seed 预存、激活、实时匹配、vault save/readback 与 metadata commit | HAR 无完整新因子网络证据，静态 success 不足以清理 | 错 factor、seed 不一致、vault/readback/metadata 失败不得退休 |
| Cookie 字段缺失；attachResetCapture | **保留**响应即时持久化，但只保存 operation 专用材料 | 不可假定无轮换；崩溃恢复必须事先保存 | header 旋转后崩溃可恢复，普通功能读不到测试标记 Cookie |
| 用户要求；loginForTOTPReset previousSave | 首登只写 operation journal/进度；runner callback 防御性隔离 | 关闭证实的第二共享出口 | 生产 callback + HTTP→Manager→runner；首登和邮箱等待无共享副本 |
| 用户要求；PublishTOTPResetSession | 用无 Cookie 的完成/清理回执及退休屏障替代真实共享发布 | 未选退出在可靠提交后清理；选退出仅保留至本次退出终态可靠保存 | success/failed/unknown/not_attempted/skipped 均清理；后续授权上传无关 |
| #944 静态 logout、无网络；logoutAfterTOTP | **保留**退出状态和门禁，终态后清理；不篡改为成功 | 退出失败不撤销已完成新 2FA，不自动重放 | 200/拒绝/5xx/断连/取消/明确未发送；恢复启动 logout 计数 0 |
| AccountSession Load/ReadCheckpoint/Flush 与 owner | 兼容隔离+退休拒绝，精确清理本操作记录/指针/checkpoint；读写和迁移都检查 | 未完成旧任务材料保留；其他账号/后来独立会话保护 | 旧发布与未完 journal、旧 ref、并发 flush、late callback、重启不复活 |

原 jobs/totp_handoff.go 的 TOTPSessionPublished 门禁须改为可靠新凭据、operation 完成和 Cookie 退休/清理凭证；不能只删除 Publish，再强置 published=true，或略过身份/幂等检查。

## 独立验证及未验证项

脱敏脚本已完成上述计数、三条响应结构、因子 ID 关联布尔比较、静态偏移和摘要核验。执行 `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test ./internal/protocol -run "TestResetTOTPConfirmedRemovalContinuesWithoutStatusRead|TestResetTOTPUncertainRemovalChecksHARStateAfterTransientFailures|TestResetTOTPConfirmedRemovalSaveFailureDoesNotEnroll|TestTOTPStatusHTTPRetriesAreBoundedAndExcludeRefusals|TestTOTPStatusHTTPRetryStopsOnCancellationAndPersistenceFailure" -count=1 -timeout=90s` 通过，包耗时 1.329 秒。这证明审查时现有快速路径/未知结果保护仍可用，不替代新生命周期改造后的完整验收。

本报告不证明真实线上当前协议、新登记/激活/logout 响应形态、Cookie 轮换规则、注销传播时延或第三方授权上传。本地合成回归只能验证代码约束，不能代替 HAR 未包含的线上事实。

## 第二轮独立审查与真实首登补充

以下由 HAR/独立审查代理根据本轮实时源码与本地合成测试核对，并非沿用历史结论。

| 发现 | 代码与可复现证据 | 修复/回归范围 |
| --- | --- | --- |
| 首次网页登录密码接受后返回邮箱验证页，没有转入 email_otp continuation | 新增 TestTOTPFirstLoginIsolationHTTP/password-email-interrupted 实际失败：TOTP_RESET_NOT_STARTED；password-success 同轮通过 | flow.go 在 reset capture 场景保存 web_email_otp，继续 verifyEmail；HTTP 取消、重建 Manager、显式原 operation 恢复，不重复 signin/password。独立复跑两子项通过，4.598 秒 |
| 按任一 Cookie 值摘要认定归属，会误伤新独立登录的公共 language=zh 偏好 | TestRetirementFencePreservesIndependentLoginWithSamePreferenceCookie 初次实际失败，0.414 秒 | 退休匹配改用 lossless 完整 lineage 与旧格式完整 snapshot；新 jar 相同偏好不等于原操作 Cookie 归属 |
| 旧已发布完成记录迁移未收集匹配 AccountSession 的退休摘要 | 源码 MigratePublishedTOTPResetSession 初版仅 matched=true，receipt 未设 MaterialDigests | 补匹配记录摘要、原指针/receipt 佐证；迁移后迟到旧 inline checkpoint 必须被拒绝的兼容回归断言 |
| 保留后来独立 active pointer、删除唯一旧 reset receipt 后，空非 nil slice 被 JSON omitempty 读回 nil | 新版 legacy 与 durability 两项实际返回 ACCOUNT_SESSION_POINTER_READBACK_FAILED，定向批次4.781秒 | 必须规范空列表并保留 active 身份，不能绕过 readback 或删除整个账号目录 |

首登真实入口测试位于 cmd/tosub2/totp_first_login_isolation_http_test.go：真实 TCP HTTP Console → Manager.Start 调度 → makeJobRunnerWithCredentials → 本机 Sentinel/账号/MFA 模拟服务；从没有预置 login checkpoint 的合成账号起步。阶段 Cookie 标记来自本地服务，断言登录、密码、邮箱、回调与 MFA 阶段均无可发现共享 AccountSession 或普通 login-checkpoint；成功后核对 committed/cleaned 回执及 journal 删除。没有使用 HAR 凭据。

旧的“成功发布并保留可复用会话”测试改为退休/兼容业务断言：保留原身份、同代 digest、uint64 精度、持久化失败、幂等、旧 journal 防回滚、后来独立会话及跨账号保护，不再把保留 reset Cookie 作为成功条件。

上述回归闭环：空 active receipt 列表已规范为 nil。更新后的旧兼容断言、首登 SaveCheckpoint 回调隔离、公共偏好保护定向批次通过，包耗时 **2.519 秒**。独立竞态复跑命令：

    GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race ./internal/protocol ./cmd/tosub2 -run 'TestResetTOTPInitialLoginUsesProductionSessionCheckpointPersistence|TestAccountSession(Retirement|LegacyRetirement|ResetCookiesRetire)|TestRetirementFencePreservesIndependentLogin|TestTOTPFirstLoginIsolationHTTP' -count=1 -timeout=120s

结果：internal/protocol **7.192 秒通过**，cmd/tosub2 **6.657 秒通过**，未报告竞态。首登协议回归分别确认 ordinary-login 仍能使用通用会话，reset-login 的通用 SaveCheckpoint 回调调用数为零，等待进度只保存在本操作 journal。

兼容归属保守边界：新独立登录由 store 内部写来源标记；遇到同账号/服务来源存在可信 reset 证据、但旧共享记录缺来源且无法用 ref/完整 lineage 证明归属时，仅写不可用于删除的永久禁用标记。该未知旧文件保持不可被普通功能读取，但不宣称已经证明属于本次操作，也不删除；已证明归属的副本才精确清理。

本节独立竞态与定向结果不代替主任务最终全仓、vet、构建验收。显式重新认证现有测试通过 ServeHTTP recorder 驱动 Console Handler，当前 sessionAuditExecute 使用 Manager.Start 调度生产 runner，再访问本机 TCP 上游；它未覆盖 Console TCP。本文新增首登测试以及主任务新增退出/清理失败测试覆盖 Console TCP 与 Manager.Start 调度。
