# 2FA 核验、恢复与历史方案清理审计

> 历史记录：本文描述当时版本，涉及 Cookie 持久化、登录 checkpoint 或远端恢复的能力现已移除。当前行为与清理范围见 [登录会话清理](authentication-session-removal-2026-11-08.md)。

> 全项目范围纠正（2026-11-08）：本文涉及的 AccountSession、Cookie 快照持久化、共享会话与 owner/dirty 机制也是后来新增，现已纳入整体清理；不能再把它们作为“原版普通会话”保留。当前边界见 [全项目清理记录](cookie-persistence-cleanup-2026-11-08.md)。下文仅作历史追溯。


> 历史记录（已被后续清理取代）：本文描述的独立核实、远端恢复、重认证或专项 Cookie 方案不再是当前功能。当前实现以 [设置与重置 2FA](totp-reset.md) 为准；保留本文仅供审计追溯。

审计日期：2026-11-07（Asia/Shanghai）。依据当前工作区源码、仓库内调用关系和本地模拟测试；保留已有未提交改动，不访问真实账号，不读取或删除真实恢复材料，不部署或切换运行程序。

## 结论

**没有整体清理掉之前的核验、恢复及专项会话体系。** 上一轮完成的是成功判定和推进规则的最小修复：正常明确成功不追加确认 GET，完成证据接受业务回执和精确只读核实两种来源。它没有撤销现有 UI/API 恢复入口，也没有将重置完全切回普通账号会话持久化路径。

“没有新增或继续扩展 Cookie 框架”与“既有 Cookie 专项框架已经删除”是两件事，后者目前不成立。

## 已移除的执行门槛

| 场景 | 当前行为 | 主要位置 |
| --- | --- | --- |
| 首次设置明确激活成功 | 直接保存，无确认 GET | `internal/protocol/flow.go` 的 `setupTOTPAttempt` |
| 重置明确移除成功 | 直接登记，无确认 GET | `internal/protocol/totp_reset_state.go` 的 `removePreparedTOTP` |
| 重置明确激活成功 | 直接提交新密钥，无确认 GET | `internal/protocol/totp_reset_resume.go` 的 `commitAcknowledgedTOTPReset` |
| 激活已确认但本地保存失败 | 消费原证据完成本地提交，不要求状态查询 | `cmd/tosub2/totp_reset.go` 的 `finalizeTOTPResetLocally`；`resumeTOTPReset` 的提前返回分支 |
| 新记录的完成证据 | 区分 `business_receipt` 与 `state_read`，回执路径不伪造读取标志 | `internal/protocol/totp_reset_evidence.go` |

未知结果的内部只读核实仍然需要保留：它与独立“核实原会话”产品入口不是同一个概念。

## 仍然活跃的生产链路

| 模块 | 仍存在的职责与调用 |
| --- | --- |
| 核实原会话 | `web/src/components/accounts.jsx` / `web/src/main.jsx` 显示并提交 `inspect-2fa`；`internal/console/server.go`、`internal/jobs/totp_recovery.go`、`cmd/tosub2/main.go` 最终调用 `InspectTOTPReset` |
| 恢复 2FA | 单个及批量 `recover-2fa` 入口、准备态继续、恢复资格和同账号互斥仍在；生产调用 `ResumeTOTPReset` |
| 验证身份并恢复 | `/recover-2fa` 的显式 `reauthenticate:true` 模式仍调用 `ResumeTOTPResetWithReauthentication`；独立认证候选、私有 checkpoint 和归档仍在 |
| 重置专项会话保存 | `ResetTOTP` 要求会话绑定回调并挂接捕获器；`saveTOTPResetJournal` 将 Cookie 快照放入准备/恢复 checkpoint；Jar persistence hook 持续保存它 |
| 脏状态、锁与响应回执 | owner、`AcquireTOTPResetLease`、`FlushTOTPReset`、完整/部分原始响应回执、operation/attempt 绑定仍由生产使用 |
| 成功后的本地退役 | `finishTOTPResetHandoff` 仍依次执行提交标记、`RetireTOTPResetSession`、完成交接；未完成时保持保护 |
| 下载、导出、上传限制 | 继续检查 pending/prepared/dirty/handoff、同账号正在进行的安全修改和 vault 状态；不是多余的远端确认 GET |

关键证据：`cmd/tosub2/main.go` 对 reset 路径跳过普通 `EnableAccountSession`，并在普通 checkpoint 保存回调中要求改用专用恢复记录。因此既有专项机制仍在，不宜描述为已完全改回普通会话流程。

## 仍然会联网核实的两个边界

1. **手动核实入口。** `InspectTOTPReset` 的 `inspectionOnly=true` 跳过激活确认后的本地提交分支，仍读取页面、当前身份和 `mfa_info`。当激活已确认但 vault 保存失败、任务仍 pending/quarantine 时，该入口仍可能可用。它的错误单独保存在 `inspection_error`，不会撤销原成功证据，也不是完成密钥保存的必要步骤；若产品要求已确认结果不再提供任何查询入口，这部分尚未收紧。
2. **尚未激活的中断恢复。** 已明确移除或登记成功、但还没有激活成功证据时，`resumeTOTPReset` 仍先读取当前状态，再解释登记回执并决定后续修改。这是既有恢复逻辑中的重新核对，上一轮没有删除。

这两个边界必须与正常成功链路区分，不能用“正常成功无 GET”的测试结论代表所有手动恢复/观察分支都不查询。

## 未发现仓库内生产调用的清理候选

| 符号/链路 | 引用情况与边界 |
| --- | --- |
| `Flow.RecoverTOTP` / `createTOTPRecoveryJournal` | 所有 13 个方法调用均在 `totp_recover_test.go`；实际控制台恢复使用 `ResumeTOTPReset`，不是这个独立实现 |
| `abortTOTPResetBeforeSend` / `TOTPResetAbortBeforeSend` | helper 无调用；runner 仍注册回调，但执行回调的唯一 helper 未接入。相关 jobs 方法及测试仍保留 |
| `validateTOTPEnrollmentRetryHistory` | 未发现生产或测试调用的旧转发函数 |
| `MarkTOTPResetSessionPublished` | 未发现调用的旧名称兼容别名，转到提交标记，不实际发布会话 |
| `PublishTOTPResetSession` | 仅测试调用的旧接口拒绝桩；删除需同时处理其兼容测试 |
| `prepareTOTPReauthentication` / `archiveAdoptedTOTPReauthentication` | 包装方法只由测试调用，生产使用相应 `ForStep` 实现 |
| `canContinueTotp` 投影及部分布尔 wrapper | 当前前端未使用该投影；活跃路径使用 `canRecoverTotp` 和能区分 disable/enroll 的步骤接口 |

这些是源码瘦身候选，不能与整条生产恢复链一并按名称删除。本次没有删除这些函数或其测试。

## 需要保留或迁移后才能删除的兼容内容

- `activation_confirmed_by_read`：旧 schema/v1 记录以及真实读取确认仍使用；新回执记录已不要求它。
- `TOTPSessionPublished`：新流程不再发布重置 Cookie，但旧已发布记录的迁移、核对和退役仍读取该字段。
- `totp_status_pending`：当前生产已无 emitter；前端映射可用于历史日志显示，不会执行查询。
- 私有 journal、pending seed、操作身份及凭据读回：仍承担“远端已变更但本地提交失败”的保护，不能直接清空解除隔离。
- 成功后代码会清理该操作的临时会话、journal、认证候选/归档并保留退役回执；这是运行时材料清理，不代表源码中的机制已移除。本次未核查真实账号材料是否已经删除。

## 本次已经纠正的说明残留

- `web/src/components/job-logs.jsx`：未知停用结果的备用文案不再写成“已获成功回执后仍核实”。
- `web/src/main.jsx`：普通恢复提示不再断言必先核实远端；明确说明已有激活证据可本地提交。
- jobs/console/runner 的通用保护文案：不再把所有未完成状态都说成“远端尚未确认”，涵盖本地收尾未完成。保护条件及路由未变。
- 当前 `totp-reset.md`：修正旧强制核实语义，说明两种确认来源、仍存在的恢复机制，以及 `RecoverTOTP` 未接线事实。
- 三份历史方案/实施/HTTP 500 调查文档：添加历史快照标记和当前规则链接，不抹去历史证据。

前端源文件说明已更新，现有 `web/dist`、嵌入 bundle 与运行程序未重新构建或部署；源码审计结论不等于运行实例已切换。

## 清理边界建议

若目标只是清掉多余查询和旧说法，正常成功路径已经满足，说明残留已补正。若目标是彻底取消独立核实/恢复产品和专项 Cookie 机制，还需要一轮独立的功能收缩：同步处理 UI/API、调度资格、专项 checkpoint/会话保存、重新认证候选、收尾条件和旧记录迁移；必须继续保留当前操作未知结果的有界核实、可靠密钥保存、身份绑定和防重复修改。不能靠删除几个无调用 helper 宣称这一目标已完成。

## 本次验证

- 本地协议测试：明确业务回执无 GET、本地保存重试无 GET、inspection 错误独立，均通过。
- `go test ./internal/jobs ./internal/console ./cmd/tosub2 -run 'TestJobLogs|TestFrontendTOTP|TestTOTPResetProductionRunner|TestTOTPRecovery|TestTOTPInspection' -count=1`：三包全部通过，耗时分别 12.721s、11.467s、16.852s。
- 日志与前端恢复/核实/重新认证专项测试另行通过；修改文件的 `git diff --check` 通过。
- 本次生产代码改动仅为显示/错误文案及注释，不改变恢复准入、派发、网络查询或数据清理逻辑。历史文档加注，不删除审计记录。
