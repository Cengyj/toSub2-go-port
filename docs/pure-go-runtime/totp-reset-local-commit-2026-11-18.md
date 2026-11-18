# 重置 2FA：本地提交与旧记录保护验收

2026-11-18，北京时间。**本轮改的是激活证据怎样读、密钥怎样提交；没有改登录、移除目标、冲突重试预算或退出策略。** 使用临时受保护目录、内存凭据提供方与回环假上游；没有登录真实账号、重放 HAR 或读取系统凭据。支持人员先看[一页排障](totp-reset-support-2026-11-18.md)。

**正常入口：** 用户选择重置 → 检查存储、登录并核对账号 → 查询是否有本轮可替换的普通验证器 → 没有就直接登记，有则按已绑定目标移除 → 新登记材料暂存并读回 → 激活 → 完整匹配的激活成功回执直接进入本地提交 → 提交完成才执行已选退出。不为成功回执另查列表。首次明确冲突最多走一轮兼容处理；响应不确定不是补发许可。只补本地保存不重登、不查询、不激活、不退出。

## 问题与用户看到的结果

`查/移/登/激/退` 为假上游**实收**的状态查询、移除、登记、激活及退出数；不含登录。标为“网络增量 0”时包括登录在内的所有上游请求都为零。右列摘自日志面板复制文本，`cause_reason` 等是详情字段，不是额外请求。

| 问题 | 修前证据（本地合成） | 修后行为 | 请求次数 | 用户实际看到的日志（摘录） |
|---|---|---|---|---|
| 无旧因子 / 有已绑定旧因子 | 两条入口原本都正确 | 保持凭激活回执直接保存；勾选退出才发退出 | `1/0/1/1/0`；`1/1/1/1/0`；有旧且勾选退出 `1/1/1/1/1` | “网站已确认激活，新密钥已保存；未另查验证器列表” |
| 初登明确冲突，复查有获准目标 / 复查明确未启用 | 修前入口均只多登记一次。冲突是夹具，不是 HAR 样本 | 同一会话继续；复查为空不发移除 | `2/1/2/1/0` / `2/0/2/1/0` | “已固定本轮旧因子，不更换移除目标” / “不发移除请求，再登记一次” |
| 正式登记再次冲突 | 修前入口停止 | 保留第一次拒绝及移除回执，不发第三次登记 | `2/1/2/0/0` | “旧因子移除已有证据，新因子尚未确认。正式登记提示已有因子；不再移除或登记” |
| 已确认激活，旧操作记录只有一个会话标记 | **函数级红测**：9 种单标记各自被滤掉后，直接标记持久化竟能改写原文件。**控制台红测**：仅留 `recovery_checkpoint` 的合成封存记录在本地完成后字节改变 | 读出数据不带会话材料，只读属性保留；已验证的证据仍可提交凭据，原记录逐字节不变 | 恢复网络增量 **0**；待提交凭据写 **1** 次 | “本轮只补本地保存，未联网”；`reason_code=LOCAL_COMPLETE`。不凭公开日志推断历史文件是否被改写，后者单独比对字节 |
| 有效凭据已保存，仅旧格式缺因子 ID | 正向控制原已正确；**控制台红测**中补 ID 写入失败与读回失败均未提交，却都缺 `cause_reason` | 只在密钥和操作绑定匹配时补 ID。两种失败均不提交、不退出；写入后的读回失败不等于写入没发生 | 恢复网络增量 **0**；写尝试 **1** 次 | “服务端已确认激活；本地提交尚未确认；可仅补本地保存，无需重新设置”；详情分别为 `STORAGE_WRITE_FAILED` / `STORAGE_READBACK_FAILED` |
| 有效密钥、因子 ID 和操作号已完全匹配 | 正向控制原已正确 | 仅确认任务提交，不重写凭据 | 恢复网络增量 **0**；凭据写 **0** 次 | `last_trusted_step=local_commit`、`local_commit=committed`，本轮远端写计数全零 |
| 任务最终提交时，凭据提供方两次读取返回不同值 | **Manager 级红测**：首读匹配，未经校验的第二次读值却进入两个同轮任务的内存输入。不代表线上发生过 | 最终提交只使用一份已核对的读回；反例中首读已不匹配则保持隔离、不提交 | 函数不联网；该次 Manager 提交的凭据读取由 **2** 次变 **1** 次 | 没有为这个注入单独造界面成功日志。普通控制台提交仍由既有终态报告；不能从日志推断凭据库读取次数 |

[真实日志面板“本次／异常”的复制文本](totp-reset-local-readback-clipboard-2026-11-18.txt)来自本轮合成读回故障。先看首行筛选范围，再找 `terminal:true`。用 `op_ref` 关联原操作；该视图的 `activation_receipt=accepted`、`last_trusted_step=activation` 来自上次激活，不是本次补发。`pending_readback=verified`、`local_commit=failed`、`cause_reason=STORAGE_READBACK_FAILED` 与 `next_action=complete_local_only` 可同时成立。没有凭据值或原始回包。

## 源码与兼容

- [操作记录读写边界](../../internal/protocol/totp_reset_journal.go)：私有 JSON 解码为 map 后检查封存身份与操作绑定；只有要 JSON 的调用方才重新编码。先识别旧会话字段，再过滤它们。读出视图中的只读标记只会**限制**写入，不是证据版本、激活证明或恢复授权，不迁移历史文件。[新重置准入](../../internal/protocol/totp_reset.go)即使遇到看似尚未发送的 `initial` 也不能覆盖这样的旧记录；这是协议入口正反例，不拿它充作新增控制台操作。
- [回执外层检查](../../internal/protocol/totp_reset_receipt.go)供[激活证据判断](../../internal/protocol/totp_reset_evidence.go)复用，核对操作、尝试、完整性和解码。不合并各端点业务语义；空白标识、错误账号、矛盾回包、缺绑定不能仅凭 200 通过。历史按列表确认的证据不降级成仅凭回执确认。
- [正常和离线入口](../../cmd/tosub2/totp_reset.go)共用[一处本地凭据提交](../../cmd/tosub2/totp_factor_credentials.go)。保留 Manager 的[独立证据和凭据核对](../../internal/jobs/totp_completion.go)、账号锁及最终任务提交。普通登录和仅查状态不取得重置的修改权限。
- 公开日志沿既有 `Flow.emitData` / runner → `JobEvent` → [保留日志](../../internal/jobs/operation_state.go) → API → [日志组件](../../web/src/components/job-logs.jsx)输出；离线入口由 runner 提交同一类型的诊断，不创建传输。未增加日志字段白名单或更改旧日志。旧日志没有 `cause_reason` 仍表示未记录，不补猜。

## 本地复测

以下命令在项目根目录运行。`D` 必须是新临时目录，避免混入旧 API 样本。浏览器使用临时配置和回环服务，比对复制按钮交给剪贴板的原文，不改用户的剪贴板。

```sh
export GOPROXY=off TOSUB2_KEYCHAIN_TEST=0 TOSUB2_LEGACY_RUNTIME_TEST=0 TOSUB2_LONG_MONITOR_TEST=0 TOSUB2_SLOW_HTTP_TEST=0
D=$(mktemp -d)
go test -count=1 ./internal/protocol -run 'Test(ResetJournalSingleSessionMarker|ResetPreMutationJournalDoesNotOverrideLegacyProvenance|AcknowledgedResetActivation|ActivationEvidence)'
go test -count=1 ./internal/jobs -run 'Test(FinalTOTPCommitUsesValidatedReadback|TOTPFinalMetadataFailureRetainsQuarantine)'
go test -count=1 -v ./cmd/tosub2 -run '^(TestResetConsoleRequestLedger|TestResetConsoleCompletesLocalBindings|TestResetStorageCheckpointsAndOfflineContinuation|TestResetCancellationAndInterruptedRestore)$' -reset-public-pages="$D"
go test -count=1 ./internal/console -run '^TestFrontendResetClipboard$' -reset-public-pages="$D"
go test -count=1 ./internal/console -run '^TestFrontend(Reset|LocalReset)ClipboardInBrowser$' -reset-public-pages="$D" '-reset-browser=/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'
go test -p 2 -count=1 ./internal/protocol ./internal/privatestate ./internal/jobs ./internal/application ./internal/console ./internal/resetlog ./cmd/tosub2
go test -race -p 2 -count=1 ./internal/protocol ./internal/jobs ./cmd/tosub2 -run '^(TestResetConcurrentReceiptsCommitOnlyOwnEnrollment|TestResetJournalSingleSessionMarker|TestResetPreMutationJournalDoesNotOverrideLegacyProvenance|TestResetSecondEnrollmentCannotReplayOrDropFirstReceipt|TestAcknowledgedResetActivationRequiresCompleteBoundReceipt|TestFinalTOTPCommitUsesValidatedReadback|TestLogArchiveRetainsBeyondSummaryAndPagesAfterTrashRestart|TestResetConsoleCompletesLocalBindings|TestResetStorageCheckpointsAndOfflineContinuation|TestResetCancellationAndInterruptedRestore|TestTOTPCompletionUsesConfiguredAccountLockWithoutNetwork)$'
go run ./cmd/build-web -check
```

**本轮结果（Go 1.27.4，macOS arm64）：** 修前三组红测分别捕获旧记录改写、补绑定失败无原因码、最终提交使用未经核对的第二次读取。修后 40 个正常重置入口场景、5 个本地提交入口场景、存储故障／取消／恢复场景均通过。54 份新生成的公开 API 日志页经过实际日志组件的复制检查，6 个场景经隔离 Chromium 逐字比对通过。受影响 Go 包、上列 11 个定向 `-race` 用例及嵌入前端一致性通过；`internal/resetlog` 无独立包内测试，其输出由 jobs、控制台与复制测试覆盖。最终上述验收无失败。未跑整仓 race、显式启用的长时或原生凭据集成、线上操作或部署。

**证据范围：** 历史 HAR 的成功回包形状与 S1～S3 的观察见[早先报告](totp-reset-receipt-path-2026-11-18.md)；它们不是本轮实测。本轮不访问真实账号、私有凭据或历史线上并发驱动。不会由成功回执、列表或退出请求推断“所有旧密钥都已失效”或“所有设备已下线”。当前线上状态未验证。
