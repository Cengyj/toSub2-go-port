# JavaScript/Python 回归断言对照

2026-09-27 补充：本轮逐源行为修复和最终测试见 docs/go-port-full-parity-review.md / .json。以下原有统计保留为历史轮次，不代表当前构建；新增短信完整错误分类、首次请求身份绑定和旧 checkpoint 限制以最新复核为准。


更新时间：2026-09-26（Asia/Shanghai）。本文对照 `legacy/test/` 的可观察行为，区分Go调用链、本地真实浏览器与远端服务验收。合成HTTP端点或SDK fixture通过，不代表真实账号、短信供应商或真实Sentinel已经接受；交叉编译也不代表Windows执行通过。

本轮新增了严格旧协议请求断言、生产CLI与BrowserTransport串联、真实React账号操作/实际下载、console→Manager→SMS HTTP链和monitor部分重传重启。下表不再把这些已经实现并本地验证的链路列为缺失。未逐字保留旧stdout日志的部分，按新的结构化JobEvent契约检验。

| 旧测试文件 | 旧测试断言范围 | Go映射与本轮证据 | 未覆盖或边界 |
|---|---|---|---|
| `console-job-lock-smoke.mjs` | 同邮箱并发创建/更新只启动一次；邮箱POST/Authorization；取消；并发relogin的attempt与冲突状态。 | `internal/jobs/manager_test.go`、`unique_email_test.go`、`lifecycle_compatibility_test.go`；`internal/console/account_compatibility_test.go`覆盖共同输入、完整预检、显式清空；`TestCLIResumeBrowserEmailMFAWorkspaceAndOAuth`检查实际邮箱HTTP method/header/body。 | 原10次HTTP请求的201/409分布和旧文本日志出现3次没有逐字移植；同邮箱互斥、只重启一次和等待旧会话清理有Manager回归。 |
| `console-restore-shutdown-smoke.mjs` | failed/completed/checkpoint/私有密码与TOTP恢复和删除；monitor请求在关闭时取消；中断元数据。 | `internal/jobs/recovery_test.go`、`restore_context_test.go`、`internal/store/golden_test.go`；`TestConsoleProcessRestoreDownloadDeleteAndRestart`实际main进程恢复27输出、下载、删除和重启；`TestConsoleCancellationWaitsForMetadataAndClosesHTTP`、`TestMonitorShutdownCancelsRequest`覆盖关闭时序。 | 没有逐字段复刻旧四个固定UUID场景；生产Go进程SIGINT/SIGTERM已验，Windows SCM目标系统未验。 |
| `console-risk-retry.mjs` | profile探测/环境覆盖、代理session预算、stale guard、取消/新run/shutdown/delete、错误分类与脱敏。 | `internal/transport/fingerprint_integration_test.go`、`http_test.go`、`internal/jobs/lifecycle_compatibility_test.go`；`TestPrepareFingerprintHTTPRejectionDoesNotRotateProxy`及`TestChromiumProxySessionReplacementHasFreshCookiesAndProfile`验证新身份边界和清理。 | Go不再抽取JS私有函数或解析stdout；真实代理供应商和上游安全挑战接受仍待验，安全拒绝不触发任意换身份。 |
| `console-smoke.mjs` | bootstrap/token/静态页；批量格式；mail配置；邮箱/MFA/phone；refresh/TOTP/password；下载/批量操作；Sub2API options/upload/monitor；重试与批量删除。 | `internal/console/batch_parser_test.go`、`frontend_contract_test.go`、`account_compatibility_test.go`；`TestChromiumEmbeddedConsoleInteractions`真实React创建25任务并验证分页/筛选/跨页选择/日志/删除；`TestChromiumConsoleAccountActionsAndDownloads`真实React/API/Manager覆盖邮箱/MFA、换号/重发、TOTP私有响应/激活、添加密码、重新授权/登录、取消/重试/停止全部及实际单/批量下载；`sms_flow_test.go`与`sub2api_flow_test.go`闭合HTTP链。 | React账号测试的远端runner是fixture；profile/about_you现由严格协议fixture检验，真实账号、短信与Sub2API后端仍待授权验收。 |
| `credential-store.mjs` | DPAPI mock与磁盘无明文；Keychain key只创建一次、旧service/直接JSON/截断迁移；Linux拒绝持久化。 | `internal/credentials/security_test.go`、`store_test.go`、`legacy_truncated_test.go`、`keychain_darwin_test.go`、`dpapi_windows_test.go`；`internal/privatefile/`验证创建前权限、原子替换和平台限制；`TestCredentialReadCancellationDoesNotBlockManager`验证锁定读取不占Manager全局锁。 | macOS真实Keychain已显式启用通过；Windows DPAPI/DACL执行仍缺；截断JSON只恢复完整顶层字符串，不恢复损坏envelope；Linux无安全持久服务时只保留内存。 |
| `password-mfa-workspace-smoke.mjs` | 密码Sentinel、TOTP MFA、web/OAuth两次workspace、邮箱OTP+MFA、无workspace、直接callback、TOTP激活与脱敏。 | `TestLegacyPasswordEmailMFAWorkspaceSmoke`严格TLS fixture验证body/Origin/Referer/device、每逻辑请求UUID、Sentinel flow/请求数、两次workspace、已绑手机号直接callback、密码/MFA拒绝重输和输出；`TestCLIResumeBrowserEmailMFAWorkspaceAndOAuth`实际运行生产CLI/runner、真实Chrome、SDK fixture、邮箱HTTP、交互stdin与导出；`legacy_enrollment_regression_test.go`覆盖激活。 | 未运行获授权的真实密码/MFA/workspace账号。生产CLI链的远端服务、SDK和vault为本地fixture，不是原Node mock server进程。 |
| `protocol-smoke.mjs` | 手机页、邮箱重发/拒码；OAuth输出/checkpoint；添加密码；refresh；risk/invalid_state/unexpected/deactivated；profile字段。 | `legacy_auth_smoke_test.go`、`legacy_phone_smoke_test.go`、`legacy_enrollment_regression_test.go`、`password_recovery_test.go`、`cmd/tosub2/cli_test.go`、`refresh_compatibility_test.go`；严格手机号fixture检查send/validate次数、页cookie、换号/重发、错误分类和恢复不重复发送；严格profile必须进入预期ChatGPT callback。 | 本地fixture不证明上游接受随机profile或真实登录；原CLI stdout文案不作为Go结构化状态契约。 |
| `sms-providers.mjs` | SMSBower文本协议和1/6/8状态；价格/库存/国家；custom池500上限/重复/旧消息；邮箱JSON与POST/header/body/cutoff。 | `internal/providers/sms_test.go`、`luban_test.go`、`legacy_regression_test.go`、`wire_compatibility_test.go`；`internal/mail/otp_test.go`与`legacy_regression_test.go`；API key预检、base URL query/相对endpoint、错误分类、多候选时间优先级均有回归。 | 真实价格、库存、API错误码和远端订单状态仍待验；敏感API key不在普通metadata/日志中保存。 |
| `sms-provider-console-smoke.mjs` | bootstrap provider列表/options；自动取号/轮询/ready/complete；429后release/换号；custom旧新短信；公开provider/status。 | `TestSMSBowerConsoleManagerFlowReadinessAfterAcceptance`、`TestSMSConsoleTimeoutResendDuplicateThenManualComplete`、`TestSMSProviderHTTP429PreservesManualChangeAndReleasesOnce`使用实际console HTTP、真实Manager和本地SMSBower服务，断言ready时序、超时/重复码、resend/poll、HTTP429手工接管和release/complete一次；`sms_store_test.go`覆盖安全重启与custom游标。 | protocol runner和provider远端为fixture；真实供应商在进程崩溃后的状态/费用/完成语义未验。不能再写成“仅handler/provider单测”。 |
| `tls-transport.mjs` | raw Cookie/expiry、手动redirect、UA/Client Hints、profile探测/fallback、sid轮换、同代理retry、Cloudflare重放、Sentinel复用。 | `internal/transport/fingerprint_integration_test.go`、`tlsclient_test.go`、`proxy_test.go`、`socks_test.go`；`internal/browser/transport_auth_test.go`、`transport_cache_test.go`、`cookie_partition_test.go`、`proxy_lifecycle_test.go`与`internal/challenge/sentinel_lifecycle_test.go`；真实Chrome本地验证认证headers、cache/SDK隔离、代理认证/替换与Cookie分区。 | 不运行旧Python curl_cffi worker；native-http仅显式兼容模式，不冒充TLS指纹；真实CF/Turnstile完整分支未验；公开真实Sentinel SDK/SO已独立可视通过，两次更早无头失败仍保留，认证端点接受未验。 |
| `console-restore-shutdown-smoke.mjs`引用的mock servers | auth expiry、queue、phone页redirect/cookie、invalid_auth_step/invalid_state。 | `internal/protocol/recovery_test.go`、`legacy_phone_smoke_test.go`、`oauth_challenge_test.go`和`internal/transport/http_test.go`；`TestRestoreCancelsEveryCredentialRecoveryCall`新增所有安全存储恢复分支取消。 | 使用Go原生HTTP fixture重建契约；没有复制全部旧固定响应和原始错误文案，公开错误须脱敏。 |
| `mock-add-phone-page-server.mjs` | csrf/cookie、OTP Sentinel、空JSON resend、phone页Cookie、invalid state/auth step、workspace、token、profile字段。 | `legacy_auth_smoke_test.go`严格断言request body/headers/UUID与profile；`TestLegacyPhoneFallbackChangeAndResend`、`TestLegacyPhoneStopsOnInvalidStepChallengeAndExpiredSession`、`TestLegacyPhoneCheckpointReopensPageWithoutRepeatingSend`覆盖确切请求计数和恢复；`TestStructuredPhoneEventsOverrideLegacyHookAndClearMetadata`覆盖状态清除。 | mock只证明本地协议契约；真实手机号、上游跳转与安全挑战仍需授权验证。 |
| `mock-auth-expiry-server.mjs` | authorize/account选择/callback；每请求唯一x-access-flow-invocation-id；phone invalid_state。 | `legacy_auth_smoke_test.go`现在验证每个逻辑auth写请求的新UUID与重复拒绝；`recovery_test.go`区分明确过期/网络/拒绝/取消/challenge，最多重新登录一次；`TestOAuthWorkspaceCheckpointAvoidsReselectingAfterInterruption`验证消费后恢复。 | 已补调用ID断言；真实OAuth会话寿命、真实账号重登接受仍未验。 |
| `mock-luban-sms-server.mjs` | JSON getNumber/getSms pending→nested六位码/setStatus。 | `internal/providers/luban_test.go`、`legacy_regression_test.go`、`wire_compatibility_test.go`；pending重试、嵌套msg/sms_msg候选排名、继承cutoff和危险ID/日期字段排除。 | 无completion endpoint时Complete保持旧no-op；真实API错误码与订单语义待验。 |
| `mock-password-mfa-server.mjs` | 密码/MFA、TOTP enroll/activate、personal/org workspace、直接callback、激活后确认失败。 | `TestLegacyPasswordEmailMFAWorkspaceSmoke`、`TestLegacyTOTPActivationPersistsPendingAndSurvivesConfirmationFailure`、`TestLegacyAddPasswordReauthenticationEmailOTP`、`TestLegacyStrongPasswordCompatibility`；严格检查MFA headers/UUID、激活前私有seed/otpauth参数、查询失败不撤销成功。 | 没有远端账号变更验收；本地确认查询失败行为已覆盖，不能再列为未迁移。 |
| `mock-protocol-login.mjs` | CLI refresh、TOTP/password/phone/risk/resume/monitor-banned和输出文件。 | `cmd/tosub2/cli_test.go`、`cli_operation_test.go`、`refresh_compatibility_test.go`、`runner_browser_test.go`；`internal/jobs/manual_recovery_test.go`、`internal/console/sub2api_flow_test.go`覆盖手动清永久阻断/部分重传；CLI子操作不因旧OAuth文件误判成功，刷新不读无关checkpoint。 | Go内部使用JobEvent，旧stdout/stderr文本解析与逐字日志不继续保留；真实账户封禁/重登仍待验。 |
| `mock-queue-protocol.mjs` | email_otp等待、取消/relogin、同邮箱锁、attempt。 | `internal/jobs/manager_test.go`、`recovery_test.go`、`unique_email_test.go`、`manual_recovery_test.go`；`TestChromiumConsoleAccountActionsAndDownloads`真实UI完成取消/重试/停止全部。 | 旧进程stdout启动次数未逐字移植；Go调度/事件状态是当前断言接口。 |
| `runtime-process.mjs` / `runtime-process.py` | Node进程文件/管道、stderr flood、事件队列、spawn失败、stdin超时；jsdom iframe消息与crash/partial stdout清理。 | `internal/browser/session_test.go`、`proxy_lifecycle_test.go`、`frames_test.go`、`internal/challenge/browser_integration_test.go`、`sentinel_lifecycle_test.go`；真实Chrome父context取消/临时目录清理、OOPIF detach、取消SDK fetch并恢复。 | 生产Go不启动Node/jsdom worker，因此旧stdout协议无等价目标；实际CDP生命周期已覆盖，不能据此声称真实挑战通过。 |
| 其余 `mock-*` HTTP fixture | 给旧smoke提供确定性端点响应。 | 对应protocol/providers/console的httptest与浏览器fixture；`internal/console/sub2api_flow_test.go`另覆盖实际Manager/Server重启后的部分远端转传。 | fixture是测试设施，不是第三方服务验收；仍需真实环境接受证据。 |

## 本轮补齐的跨模块断言

- **控制台与CLI。** 两个真实React测试分别覆盖列表/选择和账号操作/下载；生产CLI浏览器测试实际使用runner、BrowserTransport、动态SDK fixture、HTTP邮箱、交互输入和私有导出。测试临时下载目录不会污染用户Downloads。refresh回归保留未知字段、每账号client_id和轮换token；缺失或opaque id_token不丢弃已经成功的refresh结果。
- **短信。** 自动lease从协议接受号码/phone_otp阶段开始计时；SMSBower ready为best-effort且在号码接受后执行；timeout/重复码/terminal HTTP429停止自动轮询并保留手动输入、重发、换号与完成；释放/完成有次数断言。pending OTP、seen codes、deadline和停止状态在安全存储中恢复，旧provider显示名/idle清理保持兼容。
- **监控与上传。** 上传在任何请求前校验整组选中任务并去重，坏输出不静默跳过，旧计数/分组/数字success保留。真实Manager/Server重启fixture验证部分ID成功后只重传剩余ID、不重复登录、只用邮箱最新任务；queued也视为忙碌。默认间隔/冷却均5分钟有断言；interval最小1000ms，空值/Number转换/非法fallback按旧readDurationEnv。`TestMonitorDurationEnvironmentMatchesLegacyNumbers`、`TestMonitorZeroCooldownIgnoresRestoredAndReconfiguredState`和`TestMonitorZeroCooldownBypassesJobCooldownAndPendingRetry`覆盖显式0禁冷却、旧持久计时失效与转传不再被近期attempt阻断，实际1秒timer及关闭后1100ms无请求已验。公开分钟标签保持旧round/min1显示契约，0是否禁用以内部配置而非该标签判断。
- **安全凭据和文件。** OS凭据读取不持有Manager全局锁；取消能中断恢复/读取。privatefile在写入秘密前建立POSIX权限或Windows CurrentUser DACL，原子替换保留权限且不改已有共享导出父目录。Windows有DACL/NULL ACL/reparse/文件系统能力测试，仍需Windows执行。
- **文件兼容差异。** Go向输出根目录写聚合 `job-meta.json`（version/jobs/updated_at），仍兼容读取旧每任务metadata；不宣称写入布局逐文件相同。控制台TOTP/密码结果文件是权限受限的恢复临时文件，在结果安全保存后删除；恢复也先安全保存再删除。CLI默认/显式私有输出有独立交付与保留策略，无安全存储时不退回普通明文持久化。
- **公开事件。** `TestStructuredMailEventsAllowOnlyPublicTypedValues`及`TestConsoleMailboxStatusEventsRemainPublicAndSanitized`验证mailStatus仅允许ready/polling/found/error/timeout，mailApiError仅字符串、先脱敏再截短、空串清除；任意provider数据不通过JobEvent白名单进入订阅、普通metadata或公开状态API。手机号事件同样保持明确公开字段。
- **旧业务协议。** strict legacy fixture补齐每逻辑请求invocation UUID、Origin/Referer/device/CSRF、Sentinel flow、密码/邮箱/MFA拒绝后输入、两次workspace、已绑手机号直接回调、profile必须推进至预期callback；TOTP确认查询失败不重放已成功激活，密码重认证重发/错码与候选密码恢复有回归。

## 尚需真实环境或不同断言口径

1. 获授权真实账号的邮箱OTP、密码+TOTP、手机号、workspace、OAuth/refresh、设置TOTP、添加密码与跨重启恢复尚未验收。本地fixture的完整链与真实账号登录成功分开登记。
2. `TestLivePublicSentinelBrowserCompatibility`已在干净可视Chrome中独立通过，生成公开真实SDK token及SO（available）。两次先前无头尝试仍登记失败：验证页CSP阻止SDK返回loader_failed；先等待正常文档后仍为HTTP403 / Cf-Mitigated: challenge，返回SENTINEL_PAGE_CHALLENGE。未提交账号资料、自动点击验证或修改CSP。可视SDK执行成功不等于无头上游成功、认证端点接受、真实账号流程及完整Cloudflare/Turnstile分支。
3. 真实SMS/邮箱API/Sub2API后端的费用、订单状态、取号/接码/释放/完成、远端更新/清错、服务中断重传仍需获授权服务。现有完整console/Manager/HTTP链是本地mock验收。
4. Windows DPAPI、DACL及原生Service停止/重启未在目标系统执行；交叉构建只证明编译。macOS Keychain已显式启用真实读写删除测试，但部署用户、锁屏/重启组合仍待验。
5. 默认五分钟监控已有实际约300秒本地timer及在途关闭记录，另有缩短周期重复timer；真实后端长期运行仍未验。旧Node stdout次数/worker message bridge不逐字映射，Go使用结构化事件和真实Chromium CDP。
6. 原smoke的少量固定HTTP冲突分布/固定UUID逐字段样本没有原样复刻；对应核心锁/恢复/公开契约已有Go测试。不能用文件或函数存在推断未采样生产变体已经通过。

## 本轮中断与时间预算回归

- TestProductionSavedGrantFinalizesWithoutBrowserProxyOrCredentialService覆盖login/full/resume与串号拒绝；TestProductionPendingRefreshBypassesUnavailableDependencies覆盖refresh/regenerate/reauthorize。
- TestRefreshRotationSurvivesOutputFailureAcrossProcesses和TestCLIRefreshRecoversWithNewRandomJobID验证已接受旋转grant只请求一次；TestCLIRefreshCacheFailureNeverLeavesOldGrant避免旧token被热发现误当新结果。
- TestJobsHTTPDiscoversLateCLIOutputAndCheckpoint补齐旧console轮询热发现；20种旧artifact/操作组合覆盖下载、设置结果、时间与邮箱私密配置迁移。
- TestConsoleDefaultFiveMinuteMonitorAndInflightShutdown真正等待默认五分钟，本地后端而非真实Sub2API；不开长测开关明确SKIP。
- TestTransportConfiguredBudgetCanExceedThirtySeconds和TestChromiumSOCKSRequestBudgetCanExceedThirtySeconds验证31秒响应；body中断后的已接受Cookie新增/删除仍进入可恢复jar。
- TestNativeCredentialStoreSurvivesProcessRestart在macOS真实Keychain执行父进程保存、独立子进程加载更新、第二子进程复读；Windows相同入口尚待原生执行。

## 历史执行记录（2026-09-26）

全仓普通、race、vet及显式Chrome/Keychain均已退出0。`docs/go-port-verification.json`保存确切命令、时间、构建和执行证据，`docs/go-port-go-source-manifest.json`保存受测Go源码与指纹；PASS事件包含子测试，不等于独立顶层测试数量。

| 检查 | 包数 | PASS事件 / 通过的顶层测试 | SKIP |
|---|---|---|---|
| `go test -count=1 -json ./...` | 15 | 780 / 395 | 49 |
| `go test -count=1 -race -json ./...` | 15 | 780 / 395 | 49 |
| `TOSUB2_BROWSER_TEST=1 TOSUB2_KEYCHAIN_TEST=1 CHROME_PATH=...`全仓race | 15 | 830 / 433 | 11 |
| `go vet ./...` | — | 无诊断，退出0 | — |

显式Chrome/Keychain全仓有11个SKIP：5个由父测试启动的子进程helper、2个独立live上游入口、3个长时间测试入口、1个Linux专属/proc检查。完整名称和理由见机器JSON。五分钟监控与31秒transport另行显式运行通过，不把默认SKIP计为PASS；默认普通/race共49个SKIP，另含未开启的浏览器与Keychain。Windows build-tag在macOS不执行。

公开真实SDK以 `TOSUB2_LIVE_BROWSER_TEST=1 TOSUB2_BROWSER_HEADLESS=0 CHROME_PATH=... go test -count=1 -race ./cmd/tosub2 -run ^TestLivePublicSentinelBrowserCompatibility$ -v`单独退出0，生成SDK/SO token。它不并入上表830个PASS事件。之前两次无头失败仍与可视成功分别保留于 `live_upstream`，都不冒充真实账号流程。独立二进制、最小PATH、嵌入资源/受保护API及SIGTERM烟雾见 `docs/go-port-binary-verification.json`；该烟雾关闭浏览器。

## 旧版基线补验

原 `package.json` 中20项检查均已实际得到通过结果。首次3项因Python缺少curl_cffi失败，在仓库外临时venv安装原版 `0.15.0` 后重试通过；完整命令和初次失败/重试结果见 `docs/go-port-legacy-baseline.json`。这只是原版mock基线，不涉及真实账号，也不会成为Go生产运行时依赖。

## 再审计发现的输入与事件兼容差异

旧版密码输入保持原文、验证码/手机号trim的契约已贯通Go HTTP、CLI、任务消费与协议body。输入接受立即进入working并关闭该提示；新HTTP回归覆盖10种action、非法输入、重复409与日志/metadata脱敏。32并发提交只接受一个；准备SMS副作用期间不可提前消费，取消/新提示不会接受旧结果。

旧短信事件桥只在真正等待输入时启动号码/验证码处理。Go结构化事件现在明确区分等待与消费后的running；自动提交通过当前阶段原子校验，防止和人工提交竞争覆盖。missing/invalid secure订单重启后的错误提示与人工恢复由真实Manager、文件和HTTP路由测试覆盖。

生产CLI真实Chromium五个邮箱/MFA/workspace/OAuth场景还验证自动轮询不抢stdin、超时转手动、r重发继续自动、相同验证码的新邮件身份；动态SDK和账号响应仍是合成本地fixture。
