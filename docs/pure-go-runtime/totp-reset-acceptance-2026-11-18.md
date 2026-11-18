# 登录后重置 2FA：修前对照与离线验收（2026-11-18）

成功标准只有两个：**本轮登记的普通因子已在受保护的账号查询中核对，新凭据已读回并最终提交。** 激活回执、旧因子移除、退出接受是独立证据。不保证账号只有一个验证器。[一页排障](totp-reset-support-2026-11-18.md) · [本轮完整复制文本](totp-reset-evidence-2026-11-18/clipboard.txt) · [26 份复制结果的终态原文](totp-reset-evidence-2026-11-18/clipboard-terminals.txt)。

## 四条主路径

下表“前”是在本次工作树上修业务代码前，用正常队列 runner 和回环上游记录的次数。“后”从控制台实际的“重置 2FA”路由开始，经过 Manager、runner 和协议；由假上游独立计数。**不是从日志行数、成功回执数或准备脚本倒推。** `G / D / I / E / A` 分别是状态 GET、移除、初次登记、正式登记、激活。I 和 E 是同一 POST 端点的两个不同阶段。前后数字均不含登录及支持人员读取日志。除特别标注外退出都是 0，每个联网用例只有一次登录。

| 问题 | 修前证据及结果 | 修后动作及请求次数（前 → 后） | 最后可信步骤、材料 | 界面日志原文摘录（可在附件按场景查找） |
|---|---|---|---|---|
| 无普通旧因子 `empty` | 明确停用仍先取满初始状态预算；随后登记、激活、核对、提交。 | `5/0/1/0/1 → 2/0/1/0/1`。首次 GET 后直接 I，另一个 GET 核对新因子。 | 最终提交；新密钥和因子绑定已读回。 | “状态显示没有普通 TOTP，跳过移除”；“本次新因子已在远端核对，密钥读回和任务提交均已完成”。 |
| 已知普通旧因子 `old` | 原本就固定 ID，取得完整移除回执后直接登记；不插入不必要的 GET。 | `2/1/0/1/1 → 2/1/0/1/1`。这条路径做回归，没有重写。 | 最终提交；旧移除回执与新因子匹配分列。 | “已固定本轮旧因子，不更换移除目标”；“已取得所选旧因子的完整移除回执”。 |
| 初次登记明确冲突 `initial-conflict` | 合成的明确拒绝、预存绑定匹配；修前已能在原会话回退，但多查了三次初始状态。 | `6/1/1/1/1 → 3/1/1/1/1`。先 I；只读核对同一账号及唯一旧目标后 D、E、A；最后 GET。 | 两份登记回执分属两次尝试；最终保存的是正式登记的新因子。 | “初次登记明确被拒；只读核对已有绑定”；终态 `RESET_COMPLETE`。 |
| 已移除，正式登记再次冲突 `formal-conflict` | 初登被拒，固定旧目标移除成功，第二份登记拒绝后停止；没有完整的终态计数与分步摘要。 | `5/1/1/1/0 → 2/1/1/1/0`。不再次走初登回退。若一开始就有旧目标，`formal-after-remove` 为 `1/1/0/1/0`。 | 最后可信步骤 `old_removal`；有移除及拒绝回执，无新登记材料、无激活。 | “旧因子移除已有证据，新因子尚未确认。正式登记提示已有因子；不再移除或登记。下一步：核对因子绑定”。 |

## 故障、并发与恢复

表中短引文来自真实日志消息；小写字段来自同一份界面复制内容，不是测试自造的中文解释。没有独立修前次数的场景不补造数字。

| 问题 → 修前证据 | 修后行为、上游实收 `G/D/I/E/A` | 用户可据以判断的日志 / 下一步 |
|---|---|---|
| 只有可选信息缺失，不等于停用 | `optional-state`：`5/0/1/0/1`；同一会话初查及三次退避复核，登记后再核对。 | `OPTIONAL_STATE_MISSING`：“缺少可选状态；允许一次登记，不代表已证明未启用”；每次再读都有余额。 |
| 普通 409 和含 already 的任意文字不能授权删除 | `ambiguous`：`4/0/1/0/0 → 1/0/1/0/0`。明确业务码和可信拒绝形状之外，一律不走移除回退。 | “登记未取得可信的新材料，不再登记或激活”；无新凭据。 |
| 冲突时出现与绑定不同的目标，或根本没有独立目标绑定 | `changed` / `unbound`：`5/0/1/0/0 → 2/0/1/0/0`。确认冲突后只读，不追删。 | `TARGET_NOT_AUTHORIZED`：“可见因子不匹配已授权的旧因子，未发送移除”。 |
| 没有旧密钥，但本次登录已成功验证指定 MFA 因子 | `manual-proof`：`2/0/1/0/0 → 3/1/1/1/1`。临时证明来自本次登录核验；用户输入合成验证码，不提交旧种子。 | `INITIAL_CONFLICT → TARGET_BOUND → RESET_COMPLETE`。与上行无绑定停止构成正反例。 |
| 旧 ID 被通用 400 拒绝 | `stale`：`1/1/0/0/0`，不把本地隔离或 `Invalid request` 算作移除。 | `HTTP_REFUSED`，`old_removal=refused`：“请求被拒绝，未把 HTTP 响应当作成功”。 |
| 多因子含本次 ID，以前误用“列表只能一项” | `new-and-old`：`5/1/0/1/1 → 2/1/0/1/1`，从误停改为提交。 | “原定移除的 F1 仍可见，旧因子状态待核对”；不宣称全部替换。 |
| 激活已收到回执，但只展示另一因子 | `new-not-visible`：前后均 `2/1/0/1/1`；原来只有提示、隔离，无法从保留日志关联次数；现在仍不提交。 | “服务端已确认激活；本次预期 F2，当前查询显示 F3”；`pending_readback=verified`，`next_action=review_binding`。 |
| 激活回执丢失，GET 后续成功 | `lost-then-matched` / `lost-then-other` 均 `2/1/0/1/1`。上游实收 A=1，客户端是可能已发 1、收到 0。只有匹配本次 ID 才提交。 | `last_write_issue=NO_RESPONSE` 与写尝试号保留；`activation_receipt=unknown`。查到别的因子则只保留 pending。 |
| 登记只给部分材料 | `partial`：`1/1/0/1/0`，没有激活。 | “session_id：预期 string，实际 absent”；“受保护记录只读回部分登记材料，不足以激活”。 |
| 安全存储不可用 / 待确认材料写入或读回事故 | `preflight`：**所有网络 0**；`pending-write` / `pending-read` 均 `1/1/0/1/0`。 | `STORAGE_BLOCKED` / `STORAGE_WRITE_FAILED` / `STORAGE_READBACK_FAILED`。后两种有登记回执，但不能声称已读回可用的 pending 凭据。 |
| 本地提交失败后重启 | `local-commit`：`2/1/0/1/1`；`local-only` 通过“完成本地保存”后，假上游的**总请求增量为 0**。 | 同一 `Oec624936f609da21`：#26 “远端因子已核对，本地提交尚未确认；仅补本地保存”，#27 “依据已绑定记录完成本地提交；本轮没有远端写入”。 |
| 激活请求已到达，取消或进程在回包前中断 | 激活共 1，无追加。取消从控制台发起；另一个 Manager 恢复此时真实生成的合成快照，恢复后队列与网络增量都为 0。 | `REQUEST_CANCELED` 与 `INTERRUPTED_WRITE` 的来源不同；后者只列 journal 逻辑意图，不虚构 HTTP 或传输次数。 |
| 登录核对通过，状态回包的账号却不同 | `wrong-account`：`1/0/0/0/0`；新增入口测试曾发现终态误写 `TARGET_UNRESOLVED`，现已定位到状态回包。 | `IDENTITY_UNVERIFIED`，最后可信步骤仍为此前的 `identity`；不采用这份列表。重复账号字段也不按后值取胜。 |
| 可选退出与业务授权不一致 | `logout-accepted`：`2/1/0/1/1` **另有退出 1**；处理退出时 Manager 已最终提交。独立 `session-200-protected-401`：`1/0/0/0/0`。 | 前者 `logout=accepted`；后者 `SESSION_REVOKED`：“业务授权已撤销；停止本会话的修改”。产品没有在退出后额外模拟观察器查询。 |

**盲读示例。** 只给 [复制文本](totp-reset-evidence-2026-11-18/clipboard.txt)，不需要读取 vault、HAR 或请求账本：操作 `Oe96286b564eda60e`，终态 #26，最后可信步骤是激活回执，A15 的状态只见 F3，不见预期 F2。移除、正式登记、激活各收到一次响应，没有可能已发而无回包的写入；待核对凭据已读回，没有最终提交或退出。下一步是核对因子绑定，不是重试重置。附件第二个文件的每行前缀是**测试场景标签**，制表符后才是复制文本里的终态行；不要把 26 份独立视图当成一条时间线。

## 证据的出处和限制

- **历史实测，未重跑。** 根据[并发报告](totp-concurrency-account2-evidence-2026-11-18.md)及指定的脱敏事件，S1 至 S3 的 10 次登记、10 次激活均为 HTTP 200，没有“已存在”样本。[同时移除报告](totp-simultaneous-removal-2026-11-18.md)区分重叠移除与过期 ID 的 400；[双密钥核验](totp-dual-key-login-verification-2026-11-18.md)说明两份密钥曾各自匹配因子完成登录，单项状态不是有效密钥全集。[退出报告](totp-concurrency-logout-evidence-2026-11-18.md)中退出被接受、session 200 与业务 401 同时成立，不外推采样窗口。[总报告](totp-concurrency-final-2026-11-18.md)和原始历史记录未改动。两份指定 HAR 只引用既有脱敏契约：移除 message、登记 `factor.id`、激活 success、退出 null；没有打开或重放身份材料。
- **修前源码与函数级复现。** [已有基线](totp-reset-baseline-2026-11-18.md)记录的是较早快照，其中 `request_id` 和 `MailHeaders` 的红测不能当作这次接手时仍然失败。接手时 `TestTOTPEnrollmentConflictRecognizesExplicitRefusal` 中同码有/无字符串 `request_id` 均通过；类型错误、矛盾 success、混入材料、重复键仍失败，回执投影前后做同样判定。`TestBoundCredentialReadbackJSONEquivalence` 表明空 map 经 JSON 成 nil 可以通过 `EqualReadback`；改变当前/待用密钥、因子、操作或非空头会失败。后者是函数级 JSON 往返，不是线上故障实证。按绑定选登录因子和激活后只读屏障已存在，没有重复实现。
- **本地入口和 UI。** `TestResetConsoleRequestLedger` 的 19 个场景先建立临时合成账号，再 POST 控制台“重置 2FA”，由正常队列调度。测试只决定假服务回什么，不替产品发送补救请求。手工码只在正常等待输入时提供；账户旧 metadata 只让按钮可选，不充当因子绑定。`TestResetStorageCheckpointsAndOfflineContinuation` 和 `TestResetCancellationAndInterruptedRestore` 同样经过真实控制台操作。界面测试消费 GET 日志 API 的实际输出，不读取 journal 或支持测试的上游计数。不是在真人账号上做人工盲测。
- **并发和兼容。** 协议层 A/B 反例用两个客户端、不同种子、独立日志和事件屏障；两方都取得回执，只有读取到自己的新 ID 的一方提交。没有移除正常入口的同账号互斥。旧日志、旧受保护成功记录允许按既有规则读取；不把缺少的新字段补成成功或传输次数。恢复不导入网页登录会话，日志受损也不是再次派发凭据。只有本轮绑定或本次已验证的登录因子才能用于**冲突后**回退；只看到一个此前未绑定的 ID 不自动删除。真实服务当前行为、真实凭据后端、本机以外的浏览器及实际系统剪贴板均**未验证**。

## 源码定位

| 边界 | 文件与决策点 |
|---|---|
| 授权和动作 | [控制台入口](../../internal/console/totp_actions.go) `normalizeTOTPAction` → [runner](../../cmd/tosub2/job_runner.go) → [reset](../../internal/protocol/totp_reset.go)。[冲突回退](../../internal/protocol/totp_reset_initial_enroll.go)、[目标固定](../../internal/protocol/totp_reset_capture.go)、[同次登录证明](../../internal/protocol/flow.go)。普通登录和状态查询没有新增删除入口。 |
| 解释回执 / 核对绑定 | [端点分类](../../internal/protocol/totp_response.go)、[冲突分类](../../internal/protocol/totp_enrollment_conflict.go)、[回执投影](../../internal/protocol/totp_receipt_projection.go)。[状态分类](../../internal/protocol/totp_status.go) 保留旧目标唯一约束；[读取预算](../../internal/protocol/totp_reset_verify.go) 与 [commitReconciledTOTPReset](../../internal/protocol/totp_reset_continue.go) 对已知新 ID 作匹配，不放宽含重复、异常或恢复目标的反例。 |
| 保存与提交 | [受保护 journal](../../internal/protocol/totp_reset_journal.go)、[凭据适配](../../cmd/tosub2/totp_reset.go)、[等价读回](../../internal/credentials/store.go)、[Manager 最终提交](../../internal/jobs/totp_completion.go)。本地提交成功后才允许已选择的退出。 |
| 用户拿到的记录 | [协议诊断](../../internal/protocol/totp_reset_log.go) → `Flow.emitData` → runner `JobEvent` → [类型白名单](../../internal/resetlog/detail.go) / [脱敏](../../internal/jobs/security.go) → [LogEntry](../../internal/jobs/operation_state.go) → [归档分页](../../internal/jobs/log_archive.go) → [API](../../internal/console/server.go) → [日志面板](../../web/src/components/job-logs.jsx)。[CLI](../../cmd/tosub2/cli.go)显示同一终态。没有并行的恢复状态机或日志平台。 |

## 复现命令和结果

工作目录为项目根目录。仅使用已有 Go 缓存、回环传输、临时保护器和合成凭据。`-reset-public-pages` 需要全新目录；只有经日志 API 脱敏的响应才可导出。

```sh
export GOTOOLCHAIN=local GOPROXY=off TOSUB2_KEYCHAIN_TEST=0 TOSUB2_LEGACY_RUNTIME_TEST=0
go run ./cmd/build-web
go run ./cmd/build-web -check
P=$(mktemp -d /tmp/totp-reset-acceptance.XXXXXX)
go test -count=1 ./cmd/tosub2 -v -run '^Test(ResetConsoleRequestLedger|ResetStorageCheckpointsAndOfflineContinuation|ResetCancellationAndInterruptedRestore)$' -args -reset-public-pages "$P"
go test -count=1 ./internal/console -run '^TestFrontendResetClipboard$' -args -reset-public-pages "$P"
go test -count=1 ./internal/console -run '^TestFrontendResetClipboardInBrowser$' -args -reset-public-pages "$P" -reset-browser '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'
go test -count=1 -p 2 ./internal/protocol ./internal/jobs ./internal/credentials ./internal/transport ./internal/accountlock ./internal/console ./cmd/tosub2 -skip 'ConsoleProcess|ConsoleShutdown|ConsoleLongSub2API|ConsoleDefaultFiveMinute|ConsoleFailedRestore|ConsoleCancellation|OfflineCredentialImportProcess'
go test -race -p 1 -count=1 ./internal/protocol ./internal/jobs ./internal/console ./cmd/tosub2 -run '^Test(ResetConsoleRequestLedger|ResetStorageCheckpointsAndOfflineContinuation|ResetCancellationAndInterruptedRestore|RunnerUsesAndLearnsVerifiedFactorBinding|ResetConcurrentSuccessfulRemovalsContinueThenCheckOwnFactor|ResetMultiFactorVisibilityIsNotRemovalAuthority|InterruptedResetLogDoesNotInventReceipts|ResetLogBoundaryAcceptsOnlyTypedEvidence|LogArchiveRetainsBeyondSummaryAndPagesAfterTrashRestart|FrontendResetClipboard|BoundCredentialReadbackJSONEquivalence|ResetStateReadStillRequiresConfirmationEvidenceSave)$'
```

前端构建和 Go embed 编译须顺序运行，保留既有构建锁。定向回归还覆盖 429/5xx、挑战及撤销、回包完成后的清理错误、过期拒绝、读预算与取消、拒绝跨账号和跨轮材料、归档超过 200 条及回收后恢复、敏感标记和旧数据。原来“登录核验 chosen、状态却返回 old”的用例拆为相符时成功和不符时零修改；没有删除不安全输入。

| 最终验收项 | 结果与范围 |
|---|---|
| 构建与嵌入资源校验 | `build-web`、`build-web -check` 均通过。Go 1.27.4，darwin/arm64，本地依赖缓存。 |
| 正常入口与计数 | 19 个控制台路由场景通过；4 个存储故障场景及取消/中断恢复通过。`local-commit` 注入最终凭据写失败，并非假称一次真实的磁盘故障。最终 **Manager 元数据** 注入失败另由 `TestTOTPFinalMetadataFailureRetainsQuarantine` 验证：返回 `STORAGE_COMMIT_FAILED`，隔离不解开、远端证明仍可读。 |
| 界面复制 | 26 份本轮复制视图各只有一个终态。真实无头 Chrome 使用临时配置、生产 React 和该日志组件，取回回环 API 页后点击复制按钮，`writeText` 收到的文本与 `clipboard.txt` 逐字一致（文件附带末尾换行）。测试替换剪贴板接收端，未读取或覆盖系统剪贴板。首次因浏览器隔离规则误挡回环加载而失败；只允许回环后通过。没有独立人工阅卷。 |
| 受影响包 | `internal/protocol`、`internal/jobs`、`internal/credentials`、`internal/transport`、`internal/accountlock`、`internal/console`、`cmd/tosub2` 在上列离线筛选下全部通过。`internal/resetlog` 无独立测试文件，由边界和入口测试覆盖。 |
| 并发检测 | 上列四包定向 `go test -race` 全部通过；不是整仓全量 race。没有遗留的本轮红测。 |

跳过真实 Keychain / DPAPI / 容器、原生控制台及凭据导入进程、历史在线并发驱动、部署和线上复验；因此不把这些写作通过。构建并行造成过一次 embed 失败；另有账号绑定诊断、重复字段和存储错误分类的红测，分别修复后定向重跑，没有通过重试安全写入解决测试问题。
