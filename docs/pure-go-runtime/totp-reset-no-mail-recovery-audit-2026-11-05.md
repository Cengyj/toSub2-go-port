# reset-2fa 无邮箱验证码条件下的中断恢复审计

> 历史记录：本文描述当时版本，涉及 Cookie 持久化、登录 checkpoint 或远端恢复的能力现已移除。当前行为与清理范围见 [登录会话清理](authentication-session-removal-2026-11-08.md)。

> 历史记录（已被后续清理取代）：本文描述的独立核实、远端恢复、重认证或专项 Cookie 方案不再是当前功能。当前实现以 [设置与重置 2FA](totp-reset.md) 为准；保留本文仅供审计追溯。

> 本文保留修复前的审计结论及当时证据。随后实施的恢复会话、状态机及用户入口改进见 [原会话恢复改进](totp-reset-recovery-2026-11-05.md)。本次新增的现状刻画测试已逐步转换为修复验收测试；不要把本文的历史缺口描述当作改进后的现状。

审计日期：2026-11-05（Asia/Shanghai）。代码基线：HEAD bb5259c 加审计时已有的未提交工作树。已有大量修改，本报告以实际工作树为准，不能据此认定运行中的二进制与源码一致。

本轮只增加审计测试和本报告，不修改生产逻辑、不部署、不操作真实账号、不读取实际 Cookie 或密钥。测试均用合成账号、本地 HTTP 服务或可控传输；初始登录成功可由绑定好的 checkpoint 表示。测试中的远端状态机是故障模型，不是对官方实际会话策略的验证。

## 1. 五个直接回答

1. **核心风险成立。** 旧因子在远端已删除后，响应丢失、解析异常、成功回包不符合硬编码字段、本地 journal 写入失败等，都能让本地直接退出；没有新因子时，旧 seed 已无法认证，而可用旧会话又没有跨任务保存。用户描述的第 6 步要精确修正：当前隔离路径通常直接阻止再次点击，连重新登录都不执行；不是依靠重新登录完成恢复。这种更早的阻断并没有消除“无法继续操作”的结果。
2. **未解决核心恢复问题。** 当前有实质的防重复修改、凭据持久化和有限状态复核保护；但没有普通 reset 的持久恢复会话及生产可用的分阶段续接入口。因此不能称为完整恢复方案。
3. **无邮箱验证码时，再次点击不能作为可行恢复方法。** 当前按钮/API/runner 都会拦截未确认重置。只有“新因子已激活且已核实、正确新 seed 已进入私有 journal，只是最终凭据提交失败”的狭窄分支，在存储恢复可用且无凭据冲突时，重启可纯本地补保存，不需要邮箱。该例外不能覆盖移除响应丢失或激活响应丢失。
4. **该日志仅确认项目把这次操作停在待恢复状态。** 不能确认旧因子是否已删除、新因子是否启用、正确 seed 是否存在、Cookie 是否仍有效。必须结合同次私有 journal、受保护凭据的字段存在性、相邻日志和运行版本；远端实际状态最终仍需有权限的只读核实。
5. **最小可落地修复需要跨协议、持久化、任务入口三层。** 在删除前保存专用恢复会话和进度，未知回包用原会话只读对账，把“恢复未完成重置”接入用户实际操作入口，并严格按已知阶段继续；不得解除隔离后从头 reset。会话已被官方撤销且重新认证要求邮箱时，在给定条件下无法保证恢复。

“项目无法继续完成当前流程”已经被代码及模拟证明；“该账号在官方永远无法恢复”并未证明。是否还有其他设备会话、其他认证方法或官方人工恢复不在当前证据范围，也不能作为满足用户条件的默认前提。

## 2. 实际调用链和停止位置

### 2.1 登录与会话准备

- cmd/tosub2/main.go:169–187：makeJobRunnerWithCredentials 识别 reset-2fa，先验证 durable store 和历史 journal。
- main.go:234–242、610–623：如果允许恢复且普通 checkpoint 存在，验证后导入。不能笼统说所有重置都必然丢弃既有 checkpoint。
- cmd/tosub2/cli.go:315–324：控制台默认允许 checkpoint，CLI 需显式恢复意图，relogin 不恢复。
- main.go:441、512–527：无可恢复 checkpoint 时创建新 CookieJar 和认证 transport；本次 protocol client 与 transport 共用 Jar。
- internal/protocol/recovery.go:81–92、108–114：有候选会话可直接使用，否则 LoginWeb。候选 Cookie 的存在本身不是远端有效性证明。
- internal/protocol/flow.go:422–477、593–625：登录根据上游路由进入密码、邮箱 OTP 或 MFA。邮箱挑战要求邮箱码，已有原 TOTP 不替代邮箱步骤。

### 2.2 移除前的准备不代表远端删除

internal/protocol/totp_reset.go:37–153：读取账号和旧因子，要求单一明确的非 recovery TOTP；记录 old_totp_factor_id、disable_intent、attempted_at。

old_factor_invalidated=true 在 :137 **发送前**写入，是本地隔离含义；不是官方删除成功事实。:141–145 调用 TOTPResetBeforeDisable，实际由 cmd/tosub2/totp_reset.go:27–33 接到 Manager.BeginTOTPReset。

internal/jobs/totp_reset.go:106–158 将 vault 的 TOTPResetPending 写为 true，隔离同邮箱任务并读回检查；旧 seed 在受保护凭据中保留，内存可用 seed 被移除。失败前是否发送过请求必须另外判断。

关键缺口：totp_reset.go:149–151 在删除请求前清除普通登录 checkpoint，:324–334 删除文件并将 f.checkpoint=nil；没有先替换成独立恢复会话。

### 2.3 远端成功、本地无法确认的分支

| 窗口 | 实际停止路径 | 结果与恢复缺口 |
|---|---|---|
| disable 已生效，但超时/断连/读 body 失败 | totp_reset.go:154–160；request :428–439 | 未证明未发送则 unknown，立即退出；尚存内存会话也不进行状态 GET |
| disable 已生效，但 JSON 解析失败 | :441–443 → :155–160 | unknown，立即退出 |
| JSON 可解析，但 message 文案或结构变化 | :162–164 | 不等于精确字符串 Factor disabled 即停止；没有状态核实 |
| disable ACK 正确，但写 journal 失败 | :166–170 | ACK 只在内存，后续 GET/enroll 都不执行；磁盘可能仍是 disable_intent |
| ACK 后无法确认 disabled | :172–175 | 有限同会话 GET 复核用尽或读失败后停止，未创建新 seed |
| disabled 已确认，但保存失败 | :177–180 | 旧因子已删除，尚未 enrollment，直接退出 |
| enroll 请求失败、响应丢失或不完整 | :182–199 | 可能未登记，也可能远端已创建 pending enrollment；不能等同未执行 |
| enroll 有 secret/sid/id，但缺 is_recovery 等字段 | :187–196 | 完整校验先于保存，已收到的可识别恢复资料也被丢弃 |
| 新 seed 的 journal / enrollment 回调保存失败 | :196–205 | 不发激活请求，但旧因子已删除；已有材料的保存程度取决于失败位置 |
| activation_intent 保存失败 | :211–214 | 不发激活，已保存 seed 仍在 journal |
| activate 远端成功，但响应丢失或 JSON 无法解析 | :215–219 | unknown，seed 已预存；不查询新因子，不提升凭据 |
| activate 远端成功，但 success 缺失/类型改变 | :220–222 | 当前代码把 success != true 全归 failed；这是将不确定回包误归明确失败的缺口 |
| activate ACK 后保存失败 / 状态核实失败 | :224–236 | 新因子可能已生效；保留 seed，阻止 logout，但没有用户可用续接 |
| 新因子已核实，vault 保存/读回/metadata 提交失败 | :240–242；cmd/tosub2/totp_reset.go:41–57 | 正确 seed 及 verified 证据通常已在 journal，重启存在纯本地收尾例外 |

异常 defer 在 totp_reset.go:56–95 将操作标记为 recovery_required，尝试再次保存 journal，并再次移除 checkpoint。这里的清理与保存返回值被忽略；若原故障就是存储不可用，不能承诺最后一版状态已经落盘。先前成功的意图/凭据隔离仍可能保留下来。

### 2.4 拒绝、未知、任务失败是不同概念

- request :425–426 把非 2xx 按 HTTP 状态处理：5xx 作为未知，低于 500 的非 2xx 作为拒绝。已收到的 HTTP 400/401/403 与未收到确认回包不同；拒绝某个后续请求也不会撤销先前成功的 disable。
- 明确的 activation success:false 可以作为该回包拒绝证据；缺字段或类型漂移不能同等处理。目前 :220 对二者没有区分。
- 网络错误只有 transport.IsRequestDefinitelyUnsent 成立才能证明该请求未发出并走安全回退。普通超时、EOF、连接断开不满足这个证明。
- 本地保存失败并不回滚远端操作。当前有些保存失败会令 reset_totp_status=failed；该标签只能说明流程失败，不能据此认定远端未发生修改。
- internal/jobs/manager.go:305–310 将隔离任务的总 status 设为 failed，而协议 journal 可以是 unknown。总任务 failed 不是远端因子存在状态。
- 没有在本次审计中验证官方所有 HTTP 状态的事务承诺；不能把“程序按拒绝处理”升级成“已证明整个重置没有任何副作用”。

## 3. Cookie、任务和密钥的生命周期

| 生命周期 | 当前行为 | 无邮箱条件下含义 |
|---|---|---|
| 同一次正常执行 | protocol.Client.Jar（client.go:36–45）及 legacyruntime.Client.jar 持有会话；删除 checkpoint 不清空 Jar | ACK 正常时，后续 disabled 检查、enroll、activate 共享原会话 |
| 同次 disable 异常 | 直接 return，不利用仍在的 Jar 对账 | 仍有效的会话没有被用于解围 |
| runner 退出 | main.go:520–524 defer Close；legacyruntime/client.go:108–123 标记关闭、取消 context、关闭本地 HTTP | Flow/Jar 没有跨任务存储或注册；这是本地丢弃，不是官方 logout 证据 |
| 再次点击 | manager.go:1485 入队检查、totp_reset.go:31–38 拒绝隔离账号；直接 runner 也受 main.go:184、255–256 阻止 | 不恢复历史阶段，也不消费上次 Cookie；通常零登录请求 |
| 程序重启 | journal、vault pending、job metadata 保留隔离；restore.go:617–621 不续跑 | 普通 checkpoint 已删除，journal 不含 Cookie，不能恢复原会话 |
| 新 seed 正常收到 | journal 在 :196–200 保存；TOTPEnrollment :201–205 把 pending seed 写受保护凭据；激活在之后发生 | 防止“激活成功但连 seed 都没有”的一大类风险，但并不提供恢复入口 |
| 未知激活 | 正确新 seed 通常仍在 pending vault/journal，旧 seed 也可能仍作为隔离数据存在 | 不是 seed 丢失；是无法识别/提升/续接的产品与状态机缺口 |
| 已验证激活后的本地失败 | restore.go:741–775 要求 secret + activation_succeeded + activation_verified 才完成本地提交、读回并解锁 | 可不依赖邮箱，但仅限这一个已确认远端结果的收尾分支 |

普通 reset journal 缺少 account_id、Cookie、认证 transport 绑定和 enrollment session_id；enroll POST 前也没有独立 enroll_intent。即使能重新获得会话，单看 disabled_verified 的旧日志也不能严格证明 enrollment 从未发送。

没有发现删除 TOTP 必然使官方会话失效的代码证据。失败时可选 logout 被禁止；真正 logout 调用只在 totp_reset.go:254–255、完成新密钥验证与持久化之后。反过来，项目也无法保证官方会话永久有效，或设置新因子时不会要求新的身份验证。

另有误导性文案：restore.go:608–609 因本地没有 output 就提示“原登录会话已失效”。这个分支没有请求官方验证会话有效性，应改为“本地未保留可复用会话/需要重新取得授权”，不能把应用丢失会话说成官方注销。

## 4. 真正恢复入口的核实

### 4.1 普通界面没有可用续接

- internal/console/totp_actions.go:145–161 对待恢复状态关闭 canRetry、canResume、canForceRelogin、canResetTotp 等。
- internal/console/server.go:1210–1234 对不符合条件的操作返回 409。
- internal/jobs/totp_reset_test.go:38–96 及 internal/console/totp_actions_test.go:317–388 验证重试、重新登录、输入、资料修改、提交、导出受阻。隔离中已有正确 seed，也不是填回输入框便能恢复。
- web/src/hooks/totpSetup.js:5–29 的 retry 仅轮询已有 totp_setup_otp 的密钥，不是终态失败任务的续接器。

所以“请先恢复有效密钥”在当前产品中不是一个可按提示完成的闭环操作。存在“必须先恢复才能登录/编辑，而恢复所需会话及专用入口不可用”的实际阻塞。

### 4.2 RecoverTOTP 存在，但没有接线也不覆盖所有阶段

全量搜索生产 Go 文件的 RecoverTOTP 调用只有 internal/protocol/totp_recover.go:23 的定义，使用点在测试。CLI 命令列表 internal/config/cli.go:28–55 没有恢复命令，控制台没有接线；docs/pure-go-runtime/totp-reset.md:60 也说明这一点。

该原语的前置条件与限制：

1. :24–29 要求已有认证会话、全新私有 journal、保护凭据回调。它不负责从旧任务找回已丢失的 Cookie。
2. :79–81 遇到登录重定向停止，不自动重新登录。
3. :99–102 仅接受明确 disabled 状态；新因子已启用但 activation 回包丢失，也会拒绝，而非匹配历史新因子后只补本地保存。
4. 新 journal 写了 session_id、失败时保留 checkpoint，但这些只属于该独立方法，不能归算为普通 ResetTOTP 已具备的能力。

### 4.3 会话失效的不可绕过边界

重新认证是否必须邮箱取决于官方路由，代码不能证明必然要求，也不能承诺凭密码和旧 seed 足够。一旦官方要求邮箱，用户无法完成；旧因子已经删除后，其 seed 不能代替新有效因子。即使已正确保存新 seed，也不保证能绕过独立邮箱挑战。

如果所有可用会话都已失效，并且官方要求用户无法提供的验证，本项目不能在没有其他经过确认的认证方式时保证恢复。恢复设计应明确报告这个边界，不能隐藏成“请重试”。

## 5. 现有保护的评价

| 目标 | 结论 |
|---|---|
| 降低中途停止概率 | 部分做到：安全读取的网络重试、兼容精简 disabled 返回、ACK 后最多四次状态复核（0.5/1/2 秒间隔）；不消除写操作回包丢失、崩溃或持久化失败 |
| 不把未知当未执行 | 网络发送后未知通常正确隔离；明确未发出才回退。activation 缺字段/类型漂移以及某些本地错误的 failed 标签仍有混淆 |
| 保留正确新密钥 | 正常 enrollment 后激活前有 journal 和 pending vault；部分不完整 enrollment 的可识别材料被丢弃 |
| 保留续接会话和进度 | 未做到：普通 checkpoint 主动删除；无独立恢复 Cookie；缺 account_id、session_id、enroll_intent |
| 无邮箱实际恢复 | 一般做不到；只有已激活且已验证的新 seed 的本地收尾例外 |
| 停止进一步误操作 | 已做到相当多；同邮箱隔离、禁止重放、禁止过早 logout/export 都有意义，应保留 |

“暂停”解决的是继续破坏的风险；“续接恢复”解决的是失去操作能力的风险。两者必须分别验收。

## 6. 故障模拟和测试证据

以下新增测试属于现状刻画测试：**PASS 表示缺口已复现，并不表示修复验收通过。** 修复后应把相应断言改成安全续接结果，而不能永远保持“预期不可恢复”。

### 6.1 协议层：远端先成功再让本地失败

internal/protocol/totp_remote_success_audit_test.go：

- :18，6 个回包场景：disable 已改变远端状态后返回坏 JSON、变更 message、缺 message；activate 已生效后返回坏 JSON、缺 success、success 类型变化。核验远端因子、journal、seed、checkpoint、内存 Cookie、后续 GET/POST 次数、同一 Flow 再调用受阻。
- :87，disable 成功并返回 ACK 后，用不可替换的文件路径注入真实 journal 原子替换失败；最后成功持久化阶段仍是 disable_intent；不查询、不 enroll。
- :123，4 个登记/激活失败场景：enroll 403、合法 secret/sid/id 但缺 is_recovery、activate 400、明确 success:false。旧因子已删除，核查新材料保留情况。
- :166，新因子已激活且已验证，但 vault commit 回调失败；正确 seed 保留，未知状态、禁止 logout。

这 12 个子场景与现有 TestResetTOTPUnsentRemovalRestoresSafeRetryButUnknownRemovalDoesNot 区分“确实未发送”和“已发送但不知结果”。

### 6.2 任务层：再次点击和重启

internal/jobs/totp_resume_audit_test.go:17，5 个持久边界：删除回包丢失、删除后登记失败、激活回包丢失、激活 ACK 未验证、激活已验证但最终提交失败。

同进程 QueueOperation/Retry 全部拒绝，不二次调用 runner；重启仅最后一个分支能纯本地恢复，其他仍隔离且不排队。测试明确不冒充远端/Cookie 模拟，应与协议及真实 runner 测试合看。

### 6.3 真实 runner、本地 HTTP 和进程退出

cmd/tosub2/totp_session_loss_audit_test.go:295 的 8 个子场景全部通过：disable 成功后真实断开连接、disable 成功后 message 变化、enroll 403、activate 成功后真实断开连接，分别组合旧 Cookie 仍有效和已失效。

handler 在 :120–134 先改变远端因子，再用 Hijack().Close() 丢弃删除回包；:144–154 对激活同样先变更远端再断连。:199–204 通过真实 checkpoint 将已认证会话导入生产 runner，成功登录是明确前置条件，不冒称本测试重新验证了完整登录流程。

:210 的独立测试 oracle 用测试自身保存的 Cookie 副本证明“原会话仍有效”和“无 Cookie/已失效”两种状态，避免把客户端错误当成远端状态。:233 检查 checkpoint 消失、journal 无 Cookie/session_id、旧 seed 仍隔离、激活丢回包时新 seed 仍在 pending vault/journal；:273 和 :343 检查再次点击、直接 runner 调用以及 Manager 恢复后均零新增网络请求、零登录，继续隔离。

:370 的真实进程退出测试也通过：子进程于 :382 调 os.Exit(86)，不执行 Go defer 或正常 shutdown。父进程检查远端确已 disabled、ACK 已落盘、原 Cookie 仍有效，但 checkpoint 不存在；重启及再次点击仍无法继续。该测试使用只存合成数据的 0600 文件凭据库，没有接触真实钥匙串。

该集成测试普通运行通过（1.673s），加 race detector 后通过（3.014s）。本轮新增 3 个测试文件共覆盖 26 个场景：协议 12、任务 5、生产 runner 8、真实进程退出 1。

测试服务“无 Cookie/失效 Cookie 时要求邮箱码”是用户条件的模拟策略，不据此推断官方每次登录必然要求邮箱。

### 6.4 已运行的回归

~~~sh
go test ./internal/protocol -run '^TestAuditResetTOTP' -count=1 -v
go test ./internal/jobs -run TestTOTPResumeAudit -count=1 -v
go test ./internal/protocol ./internal/transport -run 'TOTP|Reset|Retry|TruncatedBody|ResponseLimit' -count=1
go test ./internal/jobs ./internal/console -run 'TOTP|Totp|QueueOperationIsAtomic|RejectedRetryDoesNotModifyCredentials|SameEmail' -count=1
go test -race ./internal/jobs -run 'TestTOTPResumeAudit|TestManagerConcurrencyAndEmailLock|TestTOTPResetConcurrentExport|TestTOTPResetCommitRequires|TestTOTPResetOtherJob|TestTOTPResetRestoreDoesNotReplace' -count=1
go test ./cmd/tosub2 -run 'TestCheckpointRestoreRequiresCLIIntent|TestAuthenticationCheckpointRejectsDifferentOriginBeforeNetwork|TestRemovedBrowserCheckpointPreservedWithoutNetwork|TestTOTPResetProductionRunner' -count=1 -v
go test ./internal/protocol -run 'TestRecoverTOTP|TestRecoveryLoginNeverCreatesOrChangesProfile' -count=1 -v
go test ./cmd/tosub2 -run '^TestTOTPResetAudit(RemoteSuccessLosesReusableSession|RealProcessExitAfterDisable)$' -count=1 -v
go test -race ./cmd/tosub2 -run '^TestTOTPResetAudit(RemoteSuccessLosesReusableSession|RealProcessExitAfterDisable)$' -count=1 -v
go test ./internal/protocol ./internal/jobs ./internal/console ./cmd/tosub2 -run 'TOTP|Totp|PrivateRecoveryEvidenceSurvivesMetadataFailure' -count=1
~~~

以上均通过。最后统一回归结果：protocol 16.141s、jobs 14.471s、console 11.762s、cmd/tosub2 13.250s。不是完整全仓测试，也不是对真实官方账号的端到端验收。OS keychain 由 fixture 代替，不能据此证明真实磁盘/钥匙串在断电下的全部保证。

## 7. 本次日志的证据边界

给定日志时间 2026-11-05T09:33:45.091169Z，即北京时间 2026-11-05 17:33:45.091169；operationAt 为北京时间 17:33:22.523502，相隔约 22.57 秒。这个耗时不能用于确定具体请求超时或请求次数。

已知：operation=reset-2fa，最后记录 stage=totp_reset_disable，任务和 operation 均 failed，项目报告待恢复并暂停相关入口。

不能从该单行确认：

- disable 是否真正发送、官方是否执行、是否返回完整 ACK。
- 是否曾收到新 seed、是否激活新因子、secret 是否已进入 journal / pending vault。
- 原 Cookie 是否远端失效，还是仅本地没有保存/复用。
- 故障是传输、解析、状态不同步、文件系统、凭据库还是进程退出。
- 当前工作树是否就是 17:33 时运行的二进制。

totp_reset_disable 事件是在 POST 前发出（totp_reset.go:153）；它不是成功回执。manager.go:305–310 的 failed 是任务终态；old_factor_invalidated 又是发送前的隔离标志。三者都不能单独证明远端因子情况。

当前工作树的 internal/jobs/totp_reset_diagnostic.go:13–26 会尽量追加“具体原因”。历史日志只有通用提示，可能涉及运行版本或记录缺少根因；不能凭这一点确定原因。

进一步所需证据（只核查，不修改、不清空标记）：

1. 对应 job ID、同 operationAt 的前后脱敏日志、运行二进制版本/构建提交；不要只拿当前 HEAD 代替。
2. 对应 totp-setup-result.json 的 version、phase/last_phase、status、attempted_at、ACK、activation_succeeded、activation_verified、credential_persisted，以及 secret/new ID 是否存在。因子 ID 可做一致性比较，不必公开值。
3. 受保护凭据中 TOTPResetPending、TOTPSecret/PendingTOTPSecret 的存在性与一致性；无需在日志/报告输出真实 seed。
4. login-checkpoint 及任何独立会话快照是否存在；不存在不等于远端已注销。
5. 若确实仍有获授权的有效会话，用已有只读状态能力核对账号身份和当前因子 ID；没有有效会话时应诚实记录“远端未知”，不能通过重置或登录挑战替代核实。

## 8. 最小可落地修复及验收

### 8.1 必须一起完成的最小闭环

1. **修改前保存专用恢复记录。** 持久化并读回账号 email/account ID、同次 operation ID、Cookie/会话、设备标识、UA/TLS/代理绑定、原因子 ID、阶段。与普通登录 checkpoint 分离，不让普通 resume 误用。恢复信息受保护、权限最小化；保存失败不允许发送 disable。后续响应更新 Cookie/会话凭据时同步更新专用快照，恢复时重新校验绑定与有效性；不能只保存删除前的一份旧 Cookie。
2. **阶段和材料齐全。** 至少记录 disable_intent / disable_unknown / disabled_verified / enroll_intent / enrollment_received / activation_intent / activation_unknown / activated_verified / credential_persisted。收到 enrollment 可识别 seed、URI、new factor ID、session_id 后先安全保存，再校验是否有资格激活；不完整资料只能作为证据，不授权发请求。
3. **未知响应进入原会话只读对账。** 不重发 disable/activate。先核对身份，再用现有 mfa_info 查询；明确 disabled 且能证明尚未 enroll 才进入登记；远端明确 enabled、唯一非 recovery 因子匹配本次 new factor ID、账号身份一致且有对应 seed 时，只补凭据提交。仅发现相同 ID 不足以证明激活完成，不能把 pending enrollment 当成有效因子。原 factor 仍可见可能是延迟，不应据一次 GET 就重发删除；多因子、身份不明、第三个因子一律保持未知。
4. **接入专用恢复入口。** 同进程保留任务级会话；跨点击/跨重启从专用记录恢复。用户再次点击未完成 reset 时路由到受限恢复器，不先全新 LoginWeb、不从头 reset。恢复操作获得同邮箱独占权，可以读受保护恢复材料；普通登录、reset、导出继续隔离。
5. **正确分类与展示。** success:false 与字段缺失/类型变化分开；任务失败和远端未知分开；保留阶段化且脱敏的具体原因。文案显示“会话未保存”“会话被官方拒绝”“尚未确认”各自事实，不让“已失效”掩盖本地丢弃。

已有 RecoverTOTP 可作为“已认证且明确 disabled”分支的实现参考，但直接接到按钮不够：它不支持已有新因子对账、旧 journal 续接和所有 enrollment 中间状态。

### 8.2 不可承诺自动完成的窗口

- enroll 远端已创建，但响应连 seed/sid 都未收到：必须有经过确认的查询、恢复或幂等能力才可继续。当前代码不能证明这些能力存在；不能随意再 enroll 或假定 pending 因子可重建。
- 老版本 journal 没有 enroll_intent，无法区分“未发”与“已发但响应丢失”，不能仅根据 disabled_verified 自动发新的 enroll。
- 原 Cookie 过期/撤销或上游要求新的邮箱验证：只能保留证据并报告需要外部认证。在用户无邮箱码条件下，不承诺自动恢复。
- 多个独立远端请求不是本地可实现的原子事务；进程崩溃、存储完全损坏、服务端 Cookie 轮换回包丢失仍有剩余风险。持久恢复会话降低风险，不保证“永远不会失去访问”。

### 8.3 修复验收标准

对本轮故障矩阵逐个改变断言：有效原会话下重复点击不产生登录/邮箱请求，不重复 disable，不重复已确认的 activate；从正确阶段续接并最终完成因子 ID 匹配、seed 持久化及读回。

实际子进程在每个持久化边界前后退出再启动，必须能找到专用会话和阶段；恢复时同时检查远端与本地状态，而非只看没有报错。激活已生效而回包丢失时，应只 GET 对账和本地 commit；新 seed 不得覆盖成旧 seed。session_id 缺失或 enroll 结果未知时，应停在明确边界并保留会话，不声称成功。

Cookie 已失效的用例应明确进入需认证状态；模拟上游要求邮箱时，测试应断言当前条件无法自动恢复，而不是用空 OTP、额外测试特权或取消隔离使测试变绿。

## 9. 主问题之外的相关隐患

- 单 Manager 内按邮箱串行有保护：manager.go:223–256、1492–1495；checkTOTPActionLocked 阻止重复 reset。已运行相关 race 测试通过，但并非跨进程并发证明。
- 旧任务 journal 防覆盖：totp_reset.go:75–91；CommitTOTPReset :163–198 在凭据读回后同步同邮箱任务；applyTOTPResetResult :209–213 防旧结果撤销已提交状态。
- 重启遇到历史新 seed 与当前已确认有效 seed 不同，restore.go:743–746 保留隔离且不覆盖，属于应保留的保护。
- 未发现跨进程账号锁或 fencing；多实例/独立 CLI 共享凭据的并发保护未被证明，需要在恢复器接入时补充或明确单实例限制。
- currentTOTPAccount（protocol/totp_reset.go:336–371）仅在远端 email 非空时比较邮箱；会话缺邮箱可继续，部分 HTTP 拒绝还会回退 token account ID。恢复器应要求可验证的实时身份绑定；这是代码确认的额外身份一致性缺口，本轮没有声称发生过真实串号。

不建议用清除 pending、删除历史 journal、把新 seed 强行标有效、重复从头执行或关闭隔离来“恢复”。这些办法绕过证据约束，不能满足本次无邮箱验证码条件下的安全恢复要求。
