# 重置与支持日志：修前冻结（2026-11-18，北京时间）

工作目录无初始 Git 提交。修前 SHA-256 前 16 位：`totp_reset.go` 3362b298e9785ff1、`totp_enrollment_conflict.go` 81a92958565dde81、`totp_factor_credentials.go` 996e12a0e77f9c28、`operation_state.go` dc952ab378c0ca2c、`job-logs.jsx` 0f94b476ca0493a0。

| 来源 | 请求序列或证据 | 能说明什么；不能说明什么 |
|---|---|---|
| `account2-20261118/events.jsonl`，S1 #45/59/62/70/73、S2 #154/157/165/168、S3AB #252/255/260/269、S3BAr2 #382/383/390/399、S3CC #484/485/498/499 | 登记 10，激活 10，全部 200；排除准备、清理与观察器 | 不能把“已存在”写成线上根因。双方收到激活回执不等于双方均完成当前因子核对。 |
| `account2-remove-20261118` S4 #88/89、Stale #228/241/242、观察器 #243/244 | 同一目标重叠移除两份 200；延迟使用旧目标 400 后停止 | 后两次只读不是产品自动追踪新因子。 |
| `account2-dual-seed-20261118` #11/28、`account2-matched-seed-20261118` #13/34 | 两个不同绑定在各自因子上验证；默认选错曾被拒 | 当时查询展示的单项不是有效密钥全集。正常源码已按保存绑定选因子，不需要再实现。 |
| `account2-s5-20261118` #43/44、#46–63 | 独立退出实验，退出接受后 session 200 与受保护查询 401 并存 | 不是产品执行了撤销探针；不外推到采样区间之外。 |
| 两份指定 HAR 的既有离线 `har-reference.md`/JSON | 停用 message，登记 secret/session_id/factor.id，激活 success，退出 null | 复用脱敏投影，不二次打开凭据、不重放；未观测真实冲突。 |
| 现版正常入口/协议夹具 | 已有因子：身份→状态→一次移除→登记→激活→只读匹配→本地提交；空状态：最多 4 次状态 GET→一次登记→激活→只读匹配 | 两条路径均已有激活后屏障。完整移除回执无需冗余查询。仅剩本地提交已有独立无网络入口。 |
| 冲突兼容夹具 | 初次登记带已知码、无 metadata：复查唯一普通因子→移除；同码加 request_id：停止。激活 success:true 加 request_id 仍接受 | 仅合成；修前唯一可见因子并不保证它是原获准目标。 |
| 本地 JSON 往返 | `mailHeaders,omitempty` 把空 map 读回为 nil；`saveBoundTOTPCredentials` 全结构 DeepEqual 拒绝 | 普通登录保护器已在写前归一化；JSON 库的正常 Load 通常已经返回 nil。函数级复现，不是已证实的线上失败原因。 |
| 展示边界 | JobEvent.Data 在 SanitizeJobEvent 过白名单，appendOperationLogLocked 只取文案、阶段等；详情/复制使用这些 LogEntry | 修前消息最长 512 字符，连续相同项被合并，长历史归档。没有逐请求关联号、写次数、可靠终态字段。 |

修前离线抽测：`GOPROXY=off GOTOOLCHAIN=local go test -count=1 ./internal/protocol ./cmd/tosub2 ./internal/jobs ./internal/console -run 'Test(ResetAcknowledgedActivationRequiresMatchingCurrentFactor|ResetConcurrentSuccessfulRemovalsContinueThenCheckOwnFactor|ResetStaleFactorGeneric400StopsWithoutReplacingTarget|RunnerUsesAndLearnsVerifiedFactorBinding|ResetPersistsFactorBeforeActivationAndCommitsMatchingBinding|TOTPEnrollmentConflictRecognizesExplicitRefusal|ReceiptProjectionRetainsOutcomeAndContradictionSemantics|.*InitialEnroll.*|.*JobLogs.*)$'`。protocol、cmd、console 通过；此筛选在 jobs 无匹配项，不计作覆盖。未读取 private、未运行历史驱动、未变更锁、未部署。

新增用例在业务代码修改前执行并失败：`TestBoundCredentialReadbackJSONEquivalence/empty-to-absent` 返回“密钥的因子绑定读回不一致”；`TestTOTPEnrollmentConflictRecognizesExplicitRefusal` 的 request_id 输入未认出。`TestResetSupportLogExplainsAcceptedButInvisibleFactor` 从正常队列经回环服务得到：状态 GET 2、移除/登记/激活各 1、终态失败并隔离。分页返回的原文包括“官方已确认新 2FA 激活，但当前返回的因子尚未与本次新密钥核对一致；这不代表新密钥无效。”没有 op_ref、回执来源、请求证据或可区分的写次数。这不是一个虚构的“登录失败”基线。
