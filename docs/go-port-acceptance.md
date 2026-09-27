# Go 移植逐项验收表

审阅日期：2026-09-27（Asia/Shanghai）。本表对应 50 项功能和 29 项测试；本轮测试、构建与部署证据见 docs/go-port-full-parity-review.json，逐源行修复见 docs/go-port-full-parity-review.md。

**当前结论：Go 本机服务已更新并通过本轮本地验收，但全部功能验收尚未完成。** 旧未绑定浏览器的待验证 checkpoint 无缝续行仍有缺口；真实账号、第三方服务及 Windows 原生环境未验。历史公开 SDK/SO 生成成功不等于所有安全验证或认证端点接受。

状态含义：

- **本地已验**：实现有本地测试或实际构建/浏览器运行证据；不等于真实第三方服务通过。
- **部分 / 待接入**：有实现，但调用链、恢复边界或组合用例尚未完整闭合。
- **未实现**：明确缺少对应行为。
- **真实环境待验**：可执行路径存在，但当前未用获授权的真实账号、后端或目标操作系统验证。
- **SKIP**：测试明确跳过，不能计入通过数量。

本轮补充回归：原子输入接受/重复409、密码原文贯通CLI和协议、自动邮箱不抢stdin、短信安全订单不可恢复时退出假忙、自动与人工提交竞争、HTTP/2重拨取消和多流隔离、DPAPI返回前取消丢弃明文。对应第5、7–10、17、21、34、38、45、47–49项，完整说明见迁移状态的“输入、取消和短信恢复的复核修复”。

## 50 项功能

| # | 用户要求 | 当前状态与本地证据 | 尚缺内容 / 验收边界 |
|---|---|---|---|
| 1 | 本地网页控制台 | 本地已验：embed React、frozen Vite 构建、受保护 API；真实 Chrome 核心列表和账号操作测试 | 账号操作测试使用真实 React/API/Manager，远端 runner 为合成 fixture；不代表真实账号已登录 |
| 2 | 单账号和批量创建任务 | 本地已验：单/批量 API、完整批次预检后原子提交、邮箱数组共同字段、显式清空、并发重复邮箱；真实 Chrome 创建 1+24 项 | 真实登录结果另见 7–17 |
| 3 | 最多 20 并发、超出 FIFO | 本地调度及TestChromiumPoolTwentyJobsManualInputAndQueuedCancellation：20活跃、4真实Chrome、16浏览器池等待、第21项FIFO排队；取消后slot归零 | 20个真实上游账号压测未执行 |
| 4 | 同邮箱任务锁 | 本地已验：`TestManagerConcurrencyAndEmailLock`、`TestSubmitUniqueEmailConcurrentBatchAndSingle` | 无已知独立代码缺口 |
| 5 | 取消、停止全部、重试、重新授权/登录/生成 | 本地已验：UI 行操作、批量参数/计数、重试原子校验、凭据更新等待旧会话清理；TOTP/密码子操作取消保留已有授权；手动恢复清监控阻断 | 远端授权与账号修改仍需本人或明确授权账号验证 |
| 6 | 分页、筛选、跨页多选、批量删/下载 | 真实 Chrome 已验 25 项的 20/5 分页、跨页选择、筛选、日志、清选择、批量删除；账号操作测试实际保存单/批量 JSON 下载并解析 | UI 下载使用合成账号结果；生产进程另有 27 项恢复/下载/删除回归 |
| 7 | 手动邮箱验证码 | 本地已验一次性输入、拒码/重发；生产 CLI+真实 Chromium 路径验证手动拒码重试和 React 手动输入 | 真实邮箱登录待验 |
| 8 | 邮箱 API 自动轮询 | 本地回归覆盖发送前baseline、Candidate.Key去重、相同码不同邮件、60秒fetch、总timeout转人工/r重发恢复自动、POST/header/body、checkpoint cutoff；mailStatus/mailApiError 已经由结构化事件接入公开状态 API | 真实邮箱 API 与中断期间来信组合待验；公开状态只接受固定枚举与脱敏短错误 |
| 9 | 密码登录 | 本地已验严格旧协议 fixture 的密码 Sentinel flow、密码拒绝后手动重输、Cookie/header/body/调用次数 | 真实账号待验 |
| 10 | 邮箱验证码登录后 TOTP 2FA | 本地已验严格邮箱→MFA→workspace 链、已消费输入恢复；生产 CLI+真实 Chromium 测试覆盖自动邮箱/TOTP及手动输入分支 | 远端账号与 SDK 仍为本地 fixture，真实账号待验 |
| 11 | 已登录账号设置新 TOTP | 本地已验 Cookie 会话复用、明确过期仅重登一次、激活前私有种子落盘、确认查询失败保留已成功激活；React 私有种子显示/激活及 CLI 成败判定 | 真实账号设置、锁屏和重启组合待验；不得把已有 OAuth 输出误作设置成功 |
| 12 | 无密码账号添加随机/指定强密码 | 本地已验重新认证邮箱 OTP、旧强密码兼容、候选密码安全恢复、私有结果；React 添加操作与 CLI 退出码/私有路径 | 真实账号待验；Linux CLI 无安全交付方式时拒绝随机密码或新 TOTP，不能静默丢弃 |
| 13 | Codex OAuth 授权 | 本地已验严格协议 mock 与生产 CLI→真实 Chromium→SDK fixture→BrowserTransport→OAuth 导出链 | 真实 OAuth 上游待验 |
| 14 | PKCE state/verifier/challenge | 本地已验：`TestPKCES256`、OAuth state 检查 | 无已知独立代码缺口 |
| 15 | workspace 选择 | 本地已验 web/OAuth 两次选择、无 ID 跳过、组织优先、CLI 指定 workspace、选择后中断不重复提交 | 控制台保持旧版默认选择行为；真实组织权限/响应待验 |
| 16 | 已绑定手机号自动跳过 | 本地已验严格 phone_bound_direct_session_callback 分支直接到 callback，无手机号输入/发码 | 真实已绑定账号响应待验 |
| 17 | 手动手机号、短信验证码 | 本地已验 E.164、手机号页 cookie/send fallback、拒码/重发/换号及 checkpoint；真实 React 验证换号/重发/提交 | 真实短信发送和校验待验 |
| 18 | LubanSMS | 本地 HTTP 回归覆盖取号/轮询/释放、旧消息与多候选排序、API key 校验、相对 endpoint/base query、pending/终态分类 | 真实服务待验；没有完成接口时 Complete 保持旧版 no-op |
| 19 | SMSBower | 本地已验文本协议、价格/库存/国家、base query、API key/错误分类；console→真实 Manager→本地 SMS HTTP 订单链 | 真实供应商待验；ready 在协议接受号码后发送，ready 失败不丢弃有效 lease |
| 20 | 自定义手机号与 API 池 | 本地已验共享占用、格式/去重/500 条、旧短信过滤；恢复校验原池、游标、已分配 URL、lease 与消息基线 | 真实 API 格式待验；Linux 敏感池状态仅进程内保存 |
| 21 | 短信轮询、重发、换号、释放、完成 | 完整本地 HTTP/Manager 回归：phone_otp 后开始计时、timeout 暂停、重发新周期、重复码停轮询、HTTP429手动接管、换号/释放/完成一次；安全保存 pending/seen/deadline/stopped 状态 | 真实订单状态待验；Linux 重启不保存敏感订单资料 |
| 22 | 登录 checkpoint 保存/恢复 | v1解析/邮箱/源站/session校验；BrowserBound按原身份恢复；email_verified只读验证；callback_ready独立PKCE交换；tokens_received离线完成 | 未绑定原生浏览器的旧待密码/邮件/MFA/手机/短信阶段无缝续行尚未实现，保留文件并要求显式重新登录；旧JS不支持新增阶段 |
| 23 | 中断后继续邮箱、手机号、短信、OAuth | 已消费输入、cutoff、phone_otp、workspace及BrowserBound恢复；429/5xx后保留号码和订单、清掉残留输入并暂停自动投递；两个OS进程覆盖一次性grant | 旧未绑定浏览器身份的待验证阶段受第22项限制；真实会话有效期及非幂等请求未知结果仍需现场判断 |
| 24 | OAuth code 换 access/refresh/id token | 本地已验：`TestOAuthTokenExchangeAndSub2APIShape` | 真实上游待验 |
| 25 | refresh token 重新授权 | 稳定目标旁私有sidecar先存已接受grant；两OS进程、两CLI随机job ID与生产runner只请求一次；多账号仅首项，其他字段/大整数保留；失败重启retry保留刷新操作 | 真实token生命周期待验；连sidecar无法保存时明确REFRESH_GRANT_UNSAVED，不保证恢复、不自动重登 |
| 26 | 兼容 sub2api-import-oauth.json | 本地已验 v1、JWT 身份优先级、未知字段、六类 golden、CLI 输出模式；无效源在远端 token 变更前拒绝 | 真实 Sub2API 导入仍待验 |
| 27 | 单账号与批量下载 | 真实 Chrome 已走原 React Blob/link 和下载管理器，把单/批量 JSON 保存至测试临时目录并解析；API 校验 Content-Disposition、no-store及损坏输出 | 已覆盖本地 UI 下载，不代表真实账号导出接受 |
| 28 | 直接上传 Sub2API | 本地完整 HTTP 链覆盖全部选择预检、重复 ID 去重、缺失/损坏输出拒绝、旧 selected/uploaded/skipped 计数、groupIds和幂等键 | 获授权真实后端权限/导入待验 |
| 29 | Sub2API 号池与代理读取 | 本地已验 groups/proxies/options、旧响应 envelope/名称/status 回退、代理凭据脱敏 | 真实后端响应变体待验 |
| 30 | Sub2API 账号监控 | 本地已验错误账号分页、openai/error 本地过滤、字符串数字 ID/分组、严格整数检查、公开摘要；缺失本地任务不算 matched | 真实后端待验 |
| 31 | 每 5 分钟监控异常账号 | 默认5分钟/最低1秒/0冷却兼容；TestConsoleDefaultFiveMinuteMonitorAndInflightShutdown实际等待300秒并验证在途HTTP取消与metadata保存 | 后端为本地HTTP，真实Sub2API长期运行仍未验 |
| 32 | 自动重登、授权、远端更新 | 本地真实 console/Manager/HTTP 后端链覆盖同邮箱多个远端 ID只重登一次、身份校验、字段合并、更新/清错、失败即停；queued 也视为忙碌 | 上游登录 runner 为 fixture；真实登录+真实 Sub2API 更新待验 |
| 33 | 冷却、永久跳过、中断重传 | 本地已验部分远端更新失败后 Manager/Server 重启仅重传剩余 ID、不重登；只用最新邮箱任务、永久目标清理、手动恢复与冷却 | 真实后端故障恢复待验；本地重启测试使用注入的安全凭据存储 |
| 34 | HTTP/HTTPS/SOCKS5/SOCKS5h | 本地已验 Go代理/DNS/取消；真实 Chrome HTTP/SOCKS认证、SOCKS本地DNS、HTTPS代理不可信证书拒绝、源站401不接收代理凭据 | 真实代理证书和供应商仍待验；不可信HTTPS拒绝测试不等于可信HTTPS代理上游全链验证 |
| 35 | sid/session 轮换 | 本地已验随机唯一 session、占位符/用户名/密码格式；runner 新连接阶段使用代理池和有限次数轮换 | 真实代理供应商待验；已存在的 checkpoint 保持原身份，不自动换 sid |
| 36 | 同代理风控重试 | 本地已验预算/延迟、同一Chromium上下文重试；OAuth非幂等交换不因普通拒绝/挑战失败重放；403/503头及Cookie已收到但body挂起时也不换代理 | 真实挑战接受结果待验；仅首次响应前的连接失败允许有界换代理 |
| 37 | 多代理会话轮换 | runner 已接入配置池与新的连接尝试；登录/checkpoint 形成前才允许换身份 | 不支持把已有认证状态跨代理任意迁移；真实代理池连接故障待验 |
| 38 | TLS profile 选择与降级 | Go独立grant使用tls-client实际ClientHello/ALPN probe、profile fallback及UA/Client Hints；新网页认证首请求起使用Chromium原生TLS；native-http为显式诊断模式 | TOSUB2_TLS_PROFILE作用于Go路径，不修改Chromium ClientHello；旧profile别名按依赖可用profile降级；真实上游兼容性待验 |
| 39 | Cloudflare challenge | 本地真实 Chrome已验 HttpOnly clearance、域隔离、provider新响应观察和转BrowserTransport重放 | 本地fixture不是Cloudflare安全验证；真实挑战接受仍未验 |
| 40 | Turnstile iframe | 真实 Chrome本地iframe/postMessage、response/callback、新响应等待；真实跨站 OOPIF DOM读取、隔离world及detach清理已覆盖 | 真实 Turnstile未验；不以自动点击或绕过验证作为迁移目标 |
| 41 | 动态 Sentinel loader/SDK/token | 本地真实Chrome动态loader/版本SDK/iframe/fetch/MutationObserver、缓存恢复及取消已验；公开真实SDK另在干净可视Chrome中生成token | 两次先前无头运行分别loader_failed与HTTP403验证等待失败，仍未证明无头上游成功；可视SDK生成不代表认证端点接受或真实账号流程 |
| 42 | Sentinel Session Observer Token | 本地真实Chrome fixture覆盖SO生成、缺失/失败与必需策略；公开真实SDK独立可视运行报告SO available | 真实认证端点对SO的接受未验；非必需策略允许空SO但明确状态，必需策略失败即报错 |
| 43 | 代理/Cookie/设备/UA/TLS/浏览器复用 | 新网页认证首请求前绑定Chromium；实际HTTPS fixture将ClientHello/UA/设备/Cookie贯通登录、SDK/SO和OAuth，另一种Go TLS负对照被拒绝；旧Cookie不进入独立grant | 实际HTTPS证据限本地fixture，真实上游与跨重启有效性待验；需要Chrome的认证路径不回切tls-client |
| 44 | macOS Keychain | 本机真实读写删除及两个独立OS进程加载/更新/复读；旧格式迁移、锁定、取消、并发与非全局锁读取覆盖 | 服务用户、锁屏和系统重启组合待验 |
| 45 | Windows DPAPI | AES-256-GCM文件+Go原生DPAPI CurrentUser包装密钥、旧DPAPI兼容、返回内存所有权；privatefile在写入前设置/验证当前用户DACL、拒绝宽松/NULL DACL；Windows Service控制测试已编写 | Windows build-tag测试未在本机执行；交叉编译不等于Windows DPAPI/DACL/SCM现场验收 |
| 46 | Linux 安全失败、不明文密码/TOTP/代理 | 实际Linux arm64普通/race执行TestLinuxRefusesPersistence及内存回退，不写普通秘密元数据 | 与旧行为一致，仍无持久安全凭据服务；重启需重输缺失资料 |
| 47 | 优雅关闭、取消、停止浏览器、保存元数据 | macOS/Linux实际Go进程退出；真实Chrome进程清理；macOS临时launchd安装两次bootstrap/bootout，恢复golden输出并保存metadata；Windows有原生SCM实现 | launchd从Downloads直接启动未ready，复制到临时安装目录成功；Windows SCM及Linux systemd未实际部署验收 |
| 48 | 重启恢复任务/checkpoint/输出 | 20种旧artifact/操作组合、CLI热发现、tokens_received和refresh sidecar恢复；保留旧下载、lastOperationAt及已完成账号设置；监控部分重传不重登 | 旧待验证checkpoint无缝续行受第22项限制；真实掉电/上游会话失效待验；Linux敏感资料不持久化 |
| 49 | 敏感字段不进普通metadata/日志/公开API | 公开事件白名单/脱敏/原子私有文件；新增sidecar canary覆盖jobs/query/logs/export-source/静态路径/单下载参数伪选file/batch均不泄露 | Windows DACL仍待原生执行；明确私有导出与普通API区分 |
| 50 | 现有JSON导入/导出/checkpoint兼容 | 六类golden、未知字段、大整数、旧目录/Keychain、CLI选项及聚合metadata读取回归；显式旧macOS凭据导入重新加密且不覆盖目标 | 旧v1解析不保证未绑定浏览器的待验证阶段续行；Go新增tokens_received/refresh sidecar旧JS不可恢复；真实后端接受待验 |


## 29 项测试要求

测试源码与实际执行分开登记。常规测试不依赖 Chromium；真实 Chrome/Keychain 使用显式开关。下表给出主要断言入口，本轮无缓存命令、测试事件和 SKIP 名称见 `docs/go-port-full-parity-review.json`；既有测试入口保留历史证据，不合并不同轮次计数。

| # | 用户测试要求 | 实际测试 / 命令 | 状态与缺口 |
|---|---|---|---|
| 1 | `go test ./...` | 本轮无缓存全仓执行 | 退出0，16包，1557 PASS事件 / 530顶层通过，68 SKIP |
| 2 | `go test -race ./...` | 本轮无缓存全仓执行 | 退出0，16包，1557 PASS事件 / 530顶层通过，68 SKIP |
| 3 | 配置解析 | `TestParseBoundsAndLegacyFlags`、`TestLegacyCLIFlags`、`TestCLIOptionsNeverSilentlyStartConsole`、`TestConfigurationErrorDoesNotEchoSecretArgument` | CLI/env/旧参数与错误脱敏覆盖；生产 CLI 另有路径、模式和退出码测试 |
| 4 | Cookie Jar | `TestResponseCookiesRespectDomainPathAndDeletion`、`TestCookieCheckpointPreservesHostOnlyHTTPOnlyAndSameSite`、`TestPartitionedCookieCheckpointRetainsIsolation` | 域/路径/前缀/删除/旧格式/分区隔离本地覆盖 |
| 5 | 手动重定向/Set-Cookie | `TestFingerprintManualRedirectRawCookiesAndSingleJar`、`TestBrowserTransportManualRedirectCookiesAndMethod`、`TestBrowserTransportProtocolCacheIsolationAndRedirectVariants` | Go/tls-client 与真实 Chrome本地测试覆盖301/302/303/307/308、原始Cookie与方法/body；不改为页面fetch |
| 6 | OAuth PKCE | `TestPKCES256`、`TestOAuthCallbackRequiresStateAndRedactsUntrustedErrors`、`TestCLIResumeBrowserEmailMFAWorkspaceAndOAuth` | 最后一项使用生产 runner、真实Chrome和本地OAuth端点检查PKCE/单次交换 |
| 7 | TOTP | `TestTOTPVector`、`TestTOTPEnrollmentRejectsBadSeedBeforePublication`、`TestLegacyTOTPActivationPersistsPendingAndSurvivesConfirmationFailure` | 算法、种子/手动码、私有结果写入时序、不重放激活和确认失败覆盖；真实账号待验 |
| 8 | 邮箱 OTP 提取 | `TestMessageMetadataOverridesEnvelopeAndDeduplicatesRepresentations`、`TestLegacyMailboxTimestampRankingAndPOSTRequest`、`TestStructuredMailEventsAllowOnlyPublicTypedValues`、`TestConsoleMailboxStatusEventsRemainPublicAndSanitized` | 消息时间/cutoff/候选数字、POST请求及状态公开契约覆盖；真实邮箱服务待验 |
| 9 | LubanSMS | `TestLubanHTTPLeasePollingReleaseAndComplete`、`TestLubanLegacyCodeCandidatePriorityAndTimestamps`、`TestProviderAPIKeyNormalizationAndBaseURLResolution` | 本地HTTP wire协议和URL/key兼容已验；真实服务未验 |
| 10 | SMSBower | `TestSMSBowerPlainTextProtocol`、`TestSMSBowerConsoleManagerFlowReadinessAfterAcceptance`、`TestSMSProviderHTTP429PreservesManualChangeAndReleasesOnce` | console/真实Manager/本地provider HTTP完整状态链已验；真实供应商未验 |
| 11 | 自定义池 | `TestCustomPoolDeduplicatesPhoneAndCaps`、`TestCustomPoolRestoreRejectsUnrelatedOrUnallocatedLease`、`TestCustomSMSPoolPositionSurvivesSecureRestart` | 上限、共享占用、游标/lease归属/旧消息baseline安全恢复覆盖 |
| 12 | Job 队列 | `TestQueueOperationIsAtomic`、`TestTwentyConcurrencyFIFOAndSnapshot`、`TestConsoleNewestUniqueEmailOrderingAndFIFOPosition` | 原子排队、FIFO、公开queuePosition本地通过 |
| 13 | 20 并发 | `TestTwentyConcurrencyFIFOAndSnapshot` | 实际调度20并发/后续FIFO已验；20个真实上游任务未压测 |
| 14 | 同邮箱锁 | `TestManagerConcurrencyAndEmailLock`、`TestSubmitUniqueEmailConcurrentBatchAndSingle` | 并发唯一邮箱/批次与单任务竞争已验 |
| 15 | 取消/超时 | `TestSecurityOperationCancellationRetainsAuthorization`、`TestCredentialReadCancellationDoesNotBlockManager`、`TestSMSConsoleTimeoutResendDuplicateThenManualComplete`、`TestChromiumParentCancellationTerminatesProcessAndProfile` | 子操作保留授权、凭据锁/短信超时手动接管、真实Chrome取消清理覆盖 |
| 16 | 服务关闭 | `TestConsoleProcessRestoreDownloadDeleteAndRestart`、`TestConsoleCancellationWaitsForMetadataAndClosesHTTP`、`TestConsoleFailedRestorePreservesMetadataAndClosesListener`、`TestSMSShutdownUsesOneDeadlineForTwentyLeases` | macOS生产进程和Go关闭回归已验；Windows Service控制测试仅目标机可执行 |
| 17 | checkpoint 恢复 | `TestExpiredCheckpointReloginsExactlyOnce`、`TestLoginCheckpointResumesAfterConsumedPasswordOrOTPAndMFA`、`TestLegacyPhoneCheckpointReopensPageWithoutRepeatingSend`、`TestOAuthWorkspaceCheckpointAvoidsReselectingAfterInterruption` | 不重复已消费认证/手机号发送/workspace选择；真实会话有效期待验 |
| 18 | JSON 输出兼容 | `TestGoldenStoreAllSixFilesRoundtrip`、`TestGoldenMetadataCheckpointAndPrivateResultsRecovery`、`TestGoldenSub2APIBuildShapeMatchesLegacy`、`TestRefreshRotationSurvivesMissingOpaqueOrChangedIDToken` | 六类golden、未知字段、JWT/opaque id_token、刷新结果保留覆盖 |
| 19 | Sub2API 上传 | `TestClientAuthAndUpload`、`TestSub2APIUploadValidatesWholeSelectionAndLegacyCounts`、`TestSyncAccountCredentialsRejectsWrongRemoteIdentity` | 本地真实HTTP请求、全选择预检/计数/字段/身份/生命周期已验；真实后端待验 |
| 20 | Sub2API 监控/自动修复 | `TestSub2APIMonitorConsoleManagerRepairAndUploadFlow`、`TestMonitorPartialTransferResumesAfterManagerAndServerRestart`、`TestMonitorFiveMinuteDefaultAndConfiguredTimerCancellation`、`TestMonitorZeroCooldownIgnoresRestoredAndReconfiguredState`、`TestMonitorZeroCooldownBypassesJobCooldownAndPendingRetry` | 本地Manager/Server恢复、合法1秒真实timer、剩余ID重传且不再登录覆盖；真实账号和后端待验 |
| 21 | 代理 session 轮换 | `TestProxySessionsAreUniqueAcrossInstancesAndRotateCredentials`、`TestPrepareFingerprintHTTPRejectionDoesNotRotateProxy`、`TestChromiumProxySessionReplacementHasFreshCookiesAndProfile` | session/身份/清理本地覆盖；安全拒绝不作为换代理依据 |
| 22 | TLS profile fallback | `TestTLSProfileProbeUsesRealClientHelloAndFallback`、`TestProfilesResolveBundledIdentityAndPlatform`、`TestNativeTransportExplicitIdentityAndManualCookies` | 实际ClientHello/ALPN与fallback已验；native-http明确区分且不冒充指纹模式 |
| 23 | Cloudflare 浏览器集成 | `TestChromiumCloudflareHttpOnlyAndTurnstileCallbackFixtures`、`TestChromiumChallengeWaitsForFreshProviderResponse`、`TestChromiumCrossSiteOOPIFDOMAndDetach` | **真实Chrome本地fixture通过；真实Cloudflare/Turnstile未验** |
| 24 | Sentinel 动态 SDK 集成 | `TestChromiumDynamicSDKAndSessionObserver`、`TestChromiumSentinelLoaderRecoveryAndObserverPolicy`、`TestChromiumCanceledSentinelAbortsFetchAndRecovers`、`TestLivePublicSentinelBrowserCompatibility` | 本地真实Chrome fixture通过；公开真实SDK+SO独立可视运行通过，未提交账号；两次无头失败保留，认证端点接受未验 |
| 25 | Cookie 浏览器/协议同步 | `TestChromiumPartitionedCookieCheckpointRoundtrip`、`TestBrowserCookieHandoffRejectsDifferentDevice`、`TestBrowserTransportCompleteAuthenticationHeaders`、`TestBrowserProtocolTargetDoesNotChangeSDKServiceWorker` | 本地真实Chrome覆盖Cookie分区/设备绑定/headers/缓存隔离；真实会话接受待验 |
| 26 | macOS Keychain | `TestMacOSKeychainIntegration`、`TestLegacyKeychainMigrationHonorsExplicitClears`、`TestKeychainLockedOrMissingNeverReplacesKeyOrCiphertext` | 已在显式Keychain全仓集成中完成本机真实读写删除；默认关闭开关时SKIP；部署用户/锁屏/重启组合待验 |
| 27 | Windows DPAPI | `TestDPAPICurrentUserRoundTripOwnsReturnedMemory`、`TestDPAPIEmptyInputRejected`、`TestWindowsPrivateFileDoesNotInheritEveryoneAccess`、`TestWindowsPrivateVerifierRejectsPermissiveAndNullDACL` | Windows平台测试及交叉编译；**未在Windows运行**，不登记为平台执行通过 |
| 28 | 敏感脱敏 | `TestRedactTextJSONHeadersURLsAndOTP`、`TestSanitizeEventAndPublicJob`、`TestConsoleMailboxStatusEventsRemainPublicAndSanitized`、`TestPrivateFileAtomicReplacementRetainsPermissions` | 事件白名单/API/日志/metadata/原子文件权限覆盖；Windows权限断言仍需目标机 |
| 29 | JS smoke 对应 Go 回归 | `docs/go-port-js-regressions.md`；strict legacy protocol、真实React、生产CLI/browser、SMS/monitor HTTP链及进程测试 | 旧20项mock基线最终通过；Go新增覆盖见逐断言映射，真实服务与Windows执行不冒充已验 |

## 执行记录与 SKIP 口径

`docs/go-port-verification.json`记录全仓命令、实际计数、时间和构建；`docs/go-port-go-source-manifest.json`记录受测Go源码及指纹。后续源码变更必须重新核对，PASS事件包含子测试，不等于独立顶层测试数量。

| 检查 | 结果 | PASS事件 / 通过的顶层测试 | SKIP |
|---|---|---|---|
| `go test -count=1 -json ./...` | 15包，退出0 | 780 / 395 | 49 |
| `go test -count=1 -race -json ./...` | 15包，退出0 | 780 / 395 | 49 |
| 显式Chrome/Keychain全仓race | 15包，退出0 | 830 / 433 | 11 |
| `go vet ./...` | 退出0，无诊断 | — | — |

- Chrome/Keychain全仓为11个SKIP：5个由父测试调用的子进程helper、2个独立live上游入口、3个长时间测试入口、1个Linux专属/proc检查；确切名称与原因以机器JSON为准。五分钟monitor与31秒transport另行显式执行通过，不把默认SKIP记为PASS。
- 普通与默认race为49个SKIP，其中包括未启用的真实浏览器/Keychain入口。明确启用浏览器后启动/CDP/断言失败必须FAIL；Windows build-tag未在macOS执行。
- `TestChromiumConsoleAccountActionsAndDownloads`实际用Chrome验证React邮箱/MFA、换号/重发、TOTP私有响应与激活、添加密码、重新授权/登录、取消/重试/停止全部；单/批量下载写入临时目录并解析。真实React/API/Manager运行，远端runner为fixture。
- `TestCLIResumeBrowserEmailMFAWorkspaceAndOAuth`使用生产CLI/runner、真实Chrome、动态SDK fixture、BrowserTransport、邮箱HTTP和交互stdin，核对PKCE、一次token交换、私有导出/checkpoint清理与脱敏；远端账号、SDK和OS vault仍是fixture。
- 真实公开SDK测试独立使用 `TOSUB2_LIVE_BROWSER_TEST=1 TOSUB2_BROWSER_HEADLESS=0 CHROME_PATH=... go test -count=1 -race ./cmd/tosub2 -run ^TestLivePublicSentinelBrowserCompatibility$ -v`，退出0，生成真实SDK token且SO为 `available`。这一独立结果不并入全仓830个PASS事件；未提交账号、自动点击验证或修改CSP。
- 两次此前无头尝试仍保留在 `live_upstream`：第一次验证页CSP阻止SDK，返回 `SENTINEL_SDK_FAILED: loader_failed`；第二次等待同浏览器正常页面仍收到 `HTTP 403` / `Cf-Mitigated: challenge`并返回 `SENTINEL_PAGE_CHALLENGE`。可视成功不能改写这些失败，也不证明无头、认证端点或所有Cloudflare/Turnstile分支成功。
- Windows DPAPI、CurrentUser DACL、Chromium Job Object和原生Service的目标测试只完成交叉编译，尚未执行Windows平台验收。
- 独立二进制与产物见 `docs/go-port-binary-verification.json`：最小PATH为 `/usr/bin:/bin`，health/bootstrap/嵌入JS和CSS正常、未授权jobs为403、SIGTERM退出0、日志无启动token。该烟雾关闭浏览器，与浏览器/真实账号验收分开。
- 原 `package.json` 的20项检查最终均通过；最初3项缺curl_cffi，在仓库外临时venv安装原版 `0.15.0`后重试通过。命令及原始失败保留于 `docs/go-port-legacy-baseline.json`，不成为Go生产依赖。

## 尚需授权环境验收

本轮追加的边界回归已进入最终全仓复验范围：配置显式零值；SMS timeout=0 转人工、interval=0 最小计时和迟到事件不重投验证码；代理退避/20次连接失败上限；GET/HEAD 的准确重试次数及写请求不重放；TLS 探测预算、并发候选隔离和仅选中 Cookie 合并；Chromium 专属进程树和幂等清理错误。主要新增入口为 TestSMSZeroTimeoutKeepsManualResendAndCompletion、TestSMSZeroIntervalStillPollsAndCompletes、TestConfiguredReadRetryBudgetAndZeroDelay、TestTLSProbeSlowPreferredDoesNotStarveFallback、TestTLSProbeCandidatesIsolateAndAdoptOnlySelectedCookies、TestProcessTreeCancellationAndParentExitCleanDescendants、TestChromiumOwnedDescendantsExitAfterCancellation 和 TestChromiumOwnedDescendantsExitAfterUnexpectedParentExit。后两项使用真实 Chrome，只对可归属本实例的后代断言；Windows build-tag 仍未在本机执行。

1. 使用本人或明确授权账号验证邮箱、密码、邮箱后 MFA、TOTP 设置、添加密码、手机号、workspace、OAuth、refresh 和中断恢复。
2. 可视浏览器公开真实SDK/SO生成已通过；仍需同一真实浏览器/代理会话中的认证端点接受、无头上游兼容及Cloudflare/Turnstile完整分支验收，不能用本地fixture或公开token生成代替。
3. 使用获授权的 SMS、邮箱 API 和 Sub2API 后端验证取号/接码/释放/完成、导入/更新/清错、自动修复、部分失败重传以及服务重启。
4. 使用真实 HTTP/HTTPS/SOCKS 代理验证证书、TLS profile、DNS、sid/session 和多代理连接失败；不得把安全拒绝作为换身份绕过验证的依据。
5. 在Windows实际执行DPAPI、privatefile DACL和原生Windows Service测试，验证运行用户/锁屏/权限、SCM停止与系统重启；在macOS/Linux部署目标验证launchd/systemd生命周期。交叉构建不计作平台功能验收。
6. 把本地已验的React账号操作/真实下载、生产CLI/browser以及SMS/monitor HTTP fixture链迁入获授权服务验收，并观察默认五分钟监控的长时运行。

未执行的真实环境步骤不登记为通过，当前不能宣称全部功能验收完成。

## 历史平台补验（2026-09-26）

该轮受测源码清单为259个文件，SHA-256为d071b72b90ba352f16ff81c5aebfbff6271bfd05347f677a7fb60b2a63baf9f1。macOS普通780 PASS事件/395顶层/49 SKIP，真实Chrome与Keychain全仓race为830/433/11。长预算和默认五分钟测试独立运行，不并入这一全仓计数。

Linux arm64使用Go1.26.1、Chromium149.0.7827.53及--init隔离容器；普通/race各781/396/45，真实Chromium全仓race为829/432/9；Go1.22.0全仓普通也真实通过。容器显式no-sandbox，所以不是Linux sandbox验收。Linux详细命令、时间、binary smoke与源码匹配见独立平台报告。

macOS发布二进制在最小PATH/临时HOME下运行，无Node/Python；另将同一SHA的产物复制到临时安装目录，以部署plist运行两次launchd启停、恢复golden输出、验证metadata保存和无残留注册。直接从Downloads路径启动的尝试未在15秒内ready，该失败没有抹除；具体原因尚未断定。此验证不包含系统重启、交互挑战或其他用户Keychain。

Linux平台逐条命令、普通/race/浏览器/最低Go版本、空PATH二进制启动及容器清理记录见 `docs/go-port-linux-verification.json`。
