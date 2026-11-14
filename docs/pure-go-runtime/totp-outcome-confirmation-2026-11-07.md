# 2FA 结果确认边界（2026-11-07）

> 历史记录：本文描述当时版本，涉及 Cookie 持久化、登录 checkpoint 或远端恢复的能力现已移除。当前行为与清理范围见 [登录会话清理](authentication-session-removal-2026-11-08.md)。

> 历史记录（已被后续清理取代）：本文描述的独立核实、远端恢复、重认证或专项 Cookie 方案不再是当前功能。当前实现以 [设置与重置 2FA](totp-reset.md) 为准；保留本文仅供审计追溯。

本轮只处理网络异常、响应兼容、成功判定与流程推进。沿用当前登录和当次会话、身份核对、可靠密钥保存及防重复修改机制；不新增 Cookie 持久化或恢复框架，不操作真实账号，不部署。

## HAR 事实

本地 `chatgpt.com.har` 共 963 个 entry，以下编号为零基索引。

| entry | 请求 | 可公开的结果 |
| --- | --- | --- |
| 949 | GET `/backend-api/accounts/mfa_info` | HTTP 200；MFA 为启用，TOTP 列表含 1 个因子 |
| 959 | POST `/backend-api/accounts/mfa/user/disable_in_house` | HTTP 200；`{"message":"Factor disabled","passkey_credential_manager_sync":null}` |
| 960 | GET `/backend-api/accounts/mfa_info` | HTTP 200；MFA 为停用，TOTP 列表为空 |

entry 960 在停用请求约 1.23 秒后由浏览器脚本发起，符合页面状态刷新表现。HAR 并不能证明该读取是停用成功的必要条件，完整的停用成功回执本身足以推进。附加同步字段为 null 不影响停用语义。

HAR **没有**新 TOTP 的登记或激活请求。登录阶段的 MFA challenge/verify 不属于新因子设置。设置响应判断以当前源码和已有合成测试为依据，不能称为 HAR 已验证的接口格式。文档不记录任何账号标识、Cookie、token、密钥或验证码。

## 已有保护与本轮缺口

当前移除实现已经在明确成功后直接登记；已有 fast-path 测试验证额外状态查询若会失败，也不应发起该查询。该行为继续保留。首次设置原本在成功后仍发送冗余状态 GET，但其失败只记 warning；重置则强制等待 GET 确认，且下游提交只接受读取确认。这两种不同问题分别修复，不能只删除查询而不更新下游证据规则。

响应兼容仅采用局部已知语义：现有合成测试包括停用文案 `Factor successfully disabled`、机器字段 `success: true`、激活 `activated: true`。字符串形式的 `success`、未知包装层、空对象及解析失败均不能自动认定成功。不递归搜索任意位置的成功字段或密钥。明确失败与成功证据冲突时归为未知；若返回因子或登记会话绑定，则必须与本次操作匹配。

## 完成与异常规则

- 完整、可靠且属于本次请求的业务成功回执直接授权下一步；激活成功直接可靠保存密钥和收尾。记录实际采用的回执确认来源，不伪造读取确认。
- 原请求结果未知时才进行当前会话内有界只读核实，核实必须匹配账号及本次因子；不能因任意因子启用就认定新密钥有效。
- 可能已经发送的移除、登记或激活请求不盲目重发。登记丢失必要材料时报告缺失，保留已有材料，不能通过重复登记掩盖不确定性。
- 验证码重试只由明确的机器错误码触发。超时、格式变化、缺少成功字段和未知状态都不是验证码错误。
- 页面状态刷新与已确认的操作完成解耦；显示刷新失败不能推翻成功或阻塞密钥保存。

本地回归应同时断言最终结果、确认来源及请求顺序/次数，覆盖明确成功无冗余 GET、未知结果只读核实、关键材料缺失、明确失败与矛盾响应。

## 本轮实现与验证入口

- `totp_response.go` 在已知结构内分类成功、拒绝和未知，拒绝已解释字段的重复键；附加字段和无关 null 不影响结果。
- `flow.go` 首次设置的正常序列为页面 → 初始状态 → 登记 → 激活 → 本地保存；激活明确成功不再刷新状态。
- `totp_reset_state.go`、`totp_reset_resume.go` 重置正常序列为页面 → 当前账号身份 → 初始状态 → 移除 → 登记 → 激活 → 本地保存；两个修改成功之后都没有确认 GET。
- `totp_reset_evidence.go` 用 `business_receipt` / `state_read` 记录真实确认来源。新证据绑定操作、账号、邮箱、登记材料摘要和激活尝试，已有读取确认记录继续兼容。完成、凭据提交与退役仍经过统一证据检查。
- 完整响应后的迟到连接错误，以及完整回执发布时的一次本地错误，不会触发重复修改或确认 GET。持续本地保存失败仍保留恢复记录并阻止后续操作。
- `totp_reset_outcome_test.go`、`totp_setup_outcome_test.go`、`totp_activation_evidence_test.go`、`totp_response_test.go` 和生产 runner 测试覆盖结果、来源及请求顺序/次数。

剩余边界：首次登记的既有样本允许只有密钥和登记会话。该情况下完整激活回执可以确认成功；如果激活响应丢失且没有有效新因子 ID，则不能凭任意启用因子核实，只能保留材料并明确停止自动推进。未知包装层或无法解释的字段类型不扩展为虚构接口格式；没有充分证据时保持未知。

验证结果（全部使用本地 mock/httptest）：

- 协议包全量测试：`go test ./internal/protocol -count=1` 通过。
- 最终跨层回归：`go test ./internal/protocol ./cmd/tosub2 ./internal/jobs ./internal/application ./internal/console -run 'TOTP|Totp|Enrollment|ManagedCheckpoint' -count=1`，五包全部通过。
- 对新增正常成功、未知核实、完整回执、本地保存恢复、来源绑定和解析测试运行 `go test -race`，通过。
- 修改文件的 `gofmt` 和 tracked diff 空白检查通过；没有执行真实账号操作或部署。
