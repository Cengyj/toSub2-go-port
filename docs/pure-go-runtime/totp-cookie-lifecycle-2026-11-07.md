# 重置 2FA 临时 Cookie 生命周期、审查与回归

> 历史记录：本文描述当时版本，涉及 Cookie 持久化、登录 checkpoint 或远端恢复的能力现已移除。当前行为与清理范围见 [登录会话清理](authentication-session-removal-2026-11-08.md)。

> 全项目范围纠正（2026-11-08）：本文涉及的 AccountSession、Cookie 快照持久化、共享会话与 owner/dirty 机制也是后来新增，现已纳入整体清理；不能再把它们作为“原版普通会话”保留。当前边界见 [全项目清理记录](cookie-persistence-cleanup-2026-11-08.md)。下文仅作历史追溯。


> 历史记录（已被后续清理取代）：本文描述的独立核实、远端恢复、重认证或专项 Cookie 方案不再是当前功能。当前实现以 [设置与重置 2FA](totp-reset.md) 为准；保留本文仅供审计追溯。

日期：2026-11-07（Asia/Shanghai）。以本轮开始时的工作区为基准保留原有修改。没有部署、提交、推送，没有访问真实账号服务或修改其安全状态；网络验证只使用 localhost 与合成凭据。

## 业务规则与证据边界

重置产生或保存的 Cookie 只属于本次操作。规定清理时点来自本次产品要求，**不是 HAR 协议推论**。HAR 重新核验的全部统计、entry、静态代码偏移及各项建议见 [独立 HAR 报告](totp-cookie-har-evidence-2026-11-07.md)。

- HAR 共 963 entries；#949 查询旧 MFA，#959 完整停用成功回执，#960 查询到停用状态。
- 没有新因子 enroll、activate_enrollment 或 logout_all 的实际网络请求。
- Cookie/Set-Cookie headers 与 cookie arrays 均缺失，不能推断服务端不会轮换 Cookie。
- 593 个 response.content 没有 text，2 个明确空 text；#559/#567 的 text 缺失而 content.size 为 880/1531，不能认定服务器空响应。
- 静态前端只说明捕获版本的行为；TOTP 240 秒检查不等于服务端时效保证，也不能与高级保护的 30 秒逻辑混淆。

完整停用成功回执的快速路径、未知结果只读核实、有限状态读取重试、一次性回调和修改请求防重放均保留。没有为了减少请求删除保护。

## 改造前后调用链

UI 的 TotpActionDialog → Console setup/recover/inspect HTTP → Manager 准入、邮箱排他调度和操作回执 → application.TOTPWorkflowRunner → cmd 的实际 runner → Flow.ResetTOTP / ResumeTOTPReset / 显式认证续接 → transport CookieJar → 私有 journal、凭据库及本地结果提交。

原成功分支还会进入 PublishTOTPResetSession，写 AccountSession record、CookieRef 和 active pointer；首次登录的 SaveCheckpoint 即使关闭通用响应保存 hook，仍会生成另一份共享记录。

现在首次登录与响应轮换都只进入当前 operation 的 journal；候选认证进入独立、同 operation 的候选存储。新因子已核实、凭据保存并完整读回、Manager 结果已提交后：

1. **不退出设备**：生成密封的无 Cookie 完成/退休回执，立即清理临时材料，然后才允许独立重登或上传。
2. **退出设备**：只让原重置 Jar 继续本次退出；保存 succeeded、failed、unknown、not_attempted、skipped 任一终态后，立即退休并清理，再检查后续门禁。
3. **中断、尚无完整完成证据**：保留专用恢复材料，禁止普通功能读取。不能依据 HTTP 200、内存 seed 或页面提示提前清理。
4. **新 2FA 已完成，清理失败**：保留不含 Cookie 的完成/清理回执，显示“2FA 已成功，临时会话清理待完成”。重启仅恢复事实；用户明确继续时只进行幂等本地收尾。

退出失败/未知不撤销新 2FA，仍阻止原组合流程的后续授权和上传；恢复入口绝不因历史勾选而重新发送退出。

## 每份 Cookie 的归属与清理

| 材料 | 来源与绑定 | 保存位置、读取者、更新时机 | 删除/退休条件 |
| --- | --- | --- | --- |
| 主 CookieJar | 初始账号会话或本次首次登录；账号、operation、认证阶段、TLS/UA/代理绑定 | 内存；仅当前 Flow/transport，每次响应头先保存快照和进度 | 持久退休回执成立后 RetireTOTPResetMemory；不可再次安装 hook、恢复旧快照或接收晚到 Cookie |
| preparation_checkpoint、initial_login_progress | 本次首次登录、密码后邮箱等待及 initial 阶段 | 密封 totp-setup-result.json；原操作 initial 继续与检查 | 未完成保留；完成后随 journal 清理；不调用通用 SaveCheckpoint |
| recovery_checkpoint | 初始准备、响应轮换、停用/登记/激活及身份核实 | 同 journal，额外绑定服务账号/因子及每次请求 attempt；原操作恢复/检查 | 退休前保持权威快照；退休后只允许本地清理，网络检查/重新认证也被拒绝 |
| 独立重新认证候选 | 用户显式授权、官方密码/必要验证码/回调/实时账号验证 | totp-reauth-checkpoint.json；独立 Jar 和候选 owner；响应与阶段即时保存 | 采用/拒绝事实与原 operation 匹配；退休时清理活动候选及其 owner |
| 候选归档 | 已采用或被明确拒绝的原认证尝试 | operation/attempt 摘要命名的密封 archives；仅该操作认证恢复 | 精确验证账号与 operation 后删除全部本操作归档，保留其他操作资料 |
| 旧首登 login checkpoint、共享 record、CookieRef、active pointer | 旧版本另一条 SaveCheckpoint 写路径 | 普通读取前迁移隔离；依赖真实 operation/ref、传输身份与完整 Cookie 世代/快照证据判断归属 | 证实归属的本操作副本/引用退休并删除；同账号和 origin 有可信 reset 上下文、但无法证明具体归属的旧记录写 DeleteAllowed=false 禁用标记并保留文件；后来独立会话和其他账号不得误删 |
| 旧成功发布 session | 旧 Published/ActivationVerified/credential_persisted 元数据、vault operation 与实际 session record 相互佐证 | 普通 Load/Save/Flush/Ref 已拒绝 reset provenance；恢复只做本地迁移 | 无 journal 时用明确 LegacyEvidence 回执，不编造历史 factor ID；不充分记录继续隔离而不假称完成 |
| owner、dirty buffer、持久化 closure | 同操作的内存 journal/candidate、保存失败保留状态 | 在账号 lease 和 owner 锁内同步；保存失败阻止后续请求 | 退休屏障先落盘再删除，清空 owner 和 Jar；旧 callback/Flush/兼容迁移不得写回 |

退休回执保存于同 job 目录的 per-operation 文件及 latest 清理记录。它包含账号、operation、完成事实、退出终态、清理状态及不可用于认证的材料摘要；不含 Cookie、seed、access token 或可恢复 checkpoint。每 operation 的屏障在 later operation 后仍保留。

账号的最终新 TOTP secret 保留在凭据库；独立授权产物、其他账号、后来独立会话均不作为重置 Cookie 清理对象。删除范围不是整个 account-sessions 目录。

新独立认证的 Origin=independent-login 仅由 AccountSessionStore 内部写入，不能从 checkpoint Extra 输入伪造。旧来源不明的记录只有在同账号、同 origin 存在可信重置上下文时才进入上述禁用兼容路径；该路径禁止复用而不假装已证明删除归属。

## 完成与门禁

TOTPResetCommitted 表示完整新因子核实及匹配的安全凭据提交；TOTPCookiesCleaned 表示密封 cleaned receipt 与 journal 不存在、无 dirty owner 共同成立。TOTPSessionPublished 只保留为旧记录兼容输入，不再授权普通复用或新完成。

MarkTOTPResetCommitted 重读完整完成 journal、vault operation 与 seed 一致性。CompleteTOTPResetHandoff 必须重读已清理回执，不能仅看 bool 或文件缺失。退出资格要求原 fresh 操作仍持有临时会话；重登/上传门禁直接复核密封回执与材料已清理。普通恢复、检查、启动恢复不自动执行退出、重登或上传。

主 runner 在取消后使用有界本地上下文收尾，前提仍是新因子可靠完成及结果已提交。这不是无条件 defer 删除：未完成操作与未可靠保存的退出结果继续保留恢复材料。

## 三轮审查

### 第一轮：事实和设计

重新读取项目说明、近期记录和真实源码；独立脚本重分析 HAR。确认并修复首次登录 SaveCheckpoint 共享出口、退出前与成功后发布共享会话、Published 完成门禁、退出错误/取消可能跳过本地清理等耦合。明确产品时点与 HAR 证据分开。

### 第二轮：故障与并发

- 退休屏障先于删除；部分删除、最终清理回执保存失败、删除后 Manager 元数据提交失败，均只能进入本地收尾。
- transport.Retire 在 mutationMu 下清空 Jar 并永久拒绝所有晚到写入口；与正在执行的持久化 callback 串行。
- 独立审查发现旧首登的 login checkpoint 已被删除后仍有共享 record，补查 journal 与 Cookie 世代来识别孤立副本。
- 独立红测试发现单个共享偏好 Cookie 的摘要会误伤后来独立登录，收紧为完整快照/世代归属证明。
- 修复空材料摘要及 active pointer receipt 列表经 JSON omitempty 后 nil/empty 差异导致的读回误报。
- 修复清理回执诊断再次覆盖早期脱敏结果的问题；回执只存非敏感终态摘要，公开诊断保留已脱敏细节。
- 同 job 的后来 reset/relogin 不被旧 receipt 重新绑定；退休后的 inspect 和显式认证恢复不能再次使用残留 journal。
- 首登真实 HTTP 红测试复现 password → email challenge 未正确进入邮箱等待，补齐原操作断点续接，避免再次提交已完成密码。
- 最后只读复核发现无 journal 的旧 Published 会话在本地迁移前仍有普通读取缺口：QuarantineLegacy 现在也读取同账号、同 origin 的确证 reset-operation record，对无法证明具体归属的旧 login 副本立即持久禁用。该保护在 Store Load/Save/Flush/CookieRef 入口生效，不依赖 Manager 启动恢复；撤销修复的隔离红测试确实失败，最终修复后通过。
- 随后的全仓验收实际抓到该扫描的保存顺序回归：凭据服务暂不可读时，TestStrictStorageDirtyExistingSessionPreservesOriginalPointerExpectation 发现最新响应尚未进入 dirty owner 就提前返回。修复把扫描错误保留为发布前屏障，仅允许已绑定同 lineage 的合法最新快照进入内存恢复材料，保持原 pointer expectation；立即返回 StorageBlocked，绝不在扫描失败时发布。Flush 仍须重新完整核验。原断言保留，并把 TestStrictStorage 加入最终竞态筛选。
- 跨进程证据沿用并重新执行真实子进程 OS 账号锁测试：TestResetOwnerRetainsOSLockAcrossRunnerAndFlushFailure、TestTOTPReauthenticationDirtyRetainsOSAccountLock 和 TestAccountLockProcessExit；重复点击由 Manager 并发准入及真实 HTTP intent replay 回归验证。没有把单进程 race 检测冒充跨进程验证。

### 第三轮：真实入口回归

所有远端为本机合成服务；真实 Console TCP HTTP → Manager.Start 调度 → 生产 runner 用于重置、取消、恢复和首登。显式重新认证回归使用真实 Console Handler、Manager.Start 调度、生产 runner 和 TCP 本机上游；其 Console 请求由 httptest Recorder 驱动，不将其描述为 Console TCP 链路。此判断按当前 sessionAuditExecute 实际源码核对，而非沿用旧版直接 runner 的测试描述。结果统计见下方验收记录。

| 需求 | 验证入口/测试 |
| --- | --- |
| 未退出立即清理、Cookie 在退出请求中可用 | TestTOTPCookieRetirementHTTPOutcomes |
| 退出成功、拒绝、5xx、断连、在途取消、发送前取消、明确未发送 | 同上；保留各自终态，退出请求最多一次 |
| 首次登录与密码后邮箱等待隔离、取消重启同 operation 续接 | TestTOTPFirstLoginIsolationHTTP |
| 显式重新认证及候选/归档隔离 | 既有真实 runner Sentinel/password/MFA 回归与新增退休协议测试 |
| 未完成故障/崩溃仍保留专用恢复；ordinary 被隔离 | 原 session-loss、process-exit、dirty-enrollment、prepared/resume 系列保留 |
| 部分清理、删除后回执写失败、元数据写失败；重启本地收尾 | TestTOTPCookieCleanupFailureRestartsOnlyLocalHTTPRecovery；HTTP inspect/reauth 均拒绝 |
| 独立后续授权/上传成功或失败不影响原 Cookie | TestTOTPCookieRetirementHTTPIndependentFollowups：reset 为生产链，独立授权/上传为明确的 deterministic runner stand-ins；另有真正生产重登失败 HTTP case |
| 普通 Load 无测试 Cookie；新会话/新 seed/独立产物不误删 | Cookie canary、later session、后续幂等 Retire，以及独立偏好 Cookie 冲突红测试 |
| 旧版未完成/已发布兼容、旧 ref 与晚写不复活 | TestTOTPLegacyPublishedNoJournalHTTPRecoveryIsLocal 的六种历史退出终态走完整 TCP 恢复调度；另有 AccountSession retirement/migration、fence、owner、旧 high-revision/digest 测试 |
| 画面不把清理失败说成 2FA 失败 | TestFrontendTOTPCleanupPreservesFactorSuccessAndUploadGate |

## 实际验收记录

Go 1.27.3，darwin/arm64；使用 GOTOOLCHAIN=local、GOPROXY=off、GOSUMDB=off。以下最终验收在最后一次保存顺序修复和全部新增 HTTP 测试冻结后执行。执行前后核对了 816 个 Go 源码、测试、模块及前端源码/构建文件的 SHA-256，文件集合和内容均未变化。

| 验证 | 本轮实际结果 |
| --- | --- |
| 全仓 `go test -p 4 ./... -count=1` | **通过，退出码 0**；27 个有测试的包全部通过，cmd/build-web 无测试。cmd/tosub2 219.770s，protocol 239.593s，jobs 153.805s，console 132.323s，application 55.216s |
| 六个核心包扩展竞态测试，包含 StrictStorage | **通过，退出码 0，无 data race 报告**；protocol 332.942s、jobs 99.427s、console 73.605s、application 10.001s、transport 1.899s、cmd/tosub2 235.548s |
| `go vet ./...` | 最终版本通过，退出码 0 |
| `CGO_ENABLED=0 go build -trimpath -o /tmp/tosub2-cookie-retirement ./cmd/tosub2` | 最终版本通过，退出码 0；仅本地构建，未部署 |
| `make frontend` | 通过；web/dist 与 cmd/tosub2/web/dist 同步，后续仅有 Go 修复，前端内容冻结校验一致 |
| 清理三类故障 + 六种无 journal 旧发布终态的真实 HTTP 测试 | 定向通过 5.591s；同组 `-race` 通过 6.994s；也包含在最终全仓及扩展竞态中 |
| 保存顺序故障修复 | 原测试实际先失败 0.669s，保留原断言；完整 StrictStorage/legacy/晚到保存组通过 1.215s，加入选择性解密故障新用例后同组竞态通过 2.674s |
| 格式与差异 | 本轮相关 Go 文件 `gofmt -l` 无输出；`git diff --check` 通过 |

最终竞态命令：

```sh
go test -race -p 4 ./internal/protocol ./internal/jobs ./internal/console \
  ./internal/application ./internal/transport ./cmd/tosub2 \
  -run 'TOTP|Reset|Reauth|Retir|Cookie|AccountSession|Checkpoint|Workflow|FirstLogin|StrictStorage' \
  -count=1
```

表中耗时是各包报告的执行时间，不是相加后的整体墙钟时间。最终日志保存在 `/tmp/totp-cookie-acceptance-full-tests.log`、`/tmp/totp-cookie-acceptance-race.log`、`/tmp/totp-cookie-acceptance-vet.log`、`/tmp/totp-cookie-acceptance-build.log`；前端日志为 `/tmp/totp-cookie-final-frontend.log`。

没有忽略中途失败：较早全仓/竞态暴露与新业务冲突的旧共享发布断言，已更新业务结果并保留身份、持久化和防重放检查；最后一次扫描改造导致的 dirty snapshot 回归则通过修复生产代码解决，没有放宽原严格存储断言。最终两个完整验收命令均重新执行并通过。

全工作区格式扫描另报告五个本轮未编辑的既有测试文件：`internal/console/totp_inspection_result_test.go`、`internal/protocol/rejected_code_recovery_test.go`、`legacy_auth_smoke_test.go`、`flow_test.go`、`checkpoint_transition_test.go`。为保留工作区既有改动，未对它们做无关格式重排；这些文件均参加并通过最终全仓测试。

## 未验证边界

- 未执行真实账号登录、近期认证、2FA 安全变更、退出传播或线上服务验收，未重放 HAR 凭据。
- 后续授权/上传完整成功链使用明确的 runner stand-ins 验证组合流程边界，不声称证明真实 OAuth 或第三方上传服务成功。
- 普通功能隔离通过共享 AccountSession/ReadCheckpoint/CookieRef 读写入口、Manager 门禁及测试 Cookie 标记验证；未逐个运行登录、刷新、账号查询、授权和上传的完整成功网络链。
- 并发点击已在 Manager 层验证，完成后的相同 intent 通过真实 HTTP 重放验证；真实子进程锁和进程退出恢复已测，未运行两个完整 Console 进程同时竞争重置、清理与保存的端到端场景。
- 其他账号及同账号后来 operation 的保护已在协议/Manager 层验证；没有同时驱动两个账号 HTTP 清理的独立端到端用例。
- macOS 本地测试不能替代 Windows 专属文件锁/安全凭据实现、真实突然掉电、磁盘损坏及 OS 备份/文件系统快照中的取证式擦除；外部人为备份或跨目录复制也不是受管理材料的自动追踪范围。
- 无法证明账号/operation 归属的历史资料不臆造完成、不粗删整个账号目录；保持隔离并保留供人工核验的材料。
- 退休屏障为本版本及兼容路径提供保证；回退到不认识该格式的旧二进制不属于支持的恢复方式。
