# 重置 2FA：停用后状态查询 HTTP 500 诊断

> 历史记录：本文描述当时版本，涉及 Cookie 持久化、登录 checkpoint 或远端恢复的能力现已移除。当前行为与清理范围见 [登录会话清理](authentication-session-removal-2026-11-08.md)。

> 历史记录（已被后续清理取代）：本文描述的独立核实、远端恢复、重认证或专项 Cookie 方案不再是当前功能。当前实现以 [设置与重置 2FA](totp-reset.md) 为准；保留本文仅供审计追溯。

> 历史调查快照：文中的“当前 HEAD/当前代码”指调查时版本，不是本轮源码现状。当前明确移除、激活成功均不追加确认 GET，见 [结果确认边界](totp-outcome-confirmation-2026-11-07.md)。

日期：2026-11-07（Asia/Shanghai）。本轮为代码、运行元数据与本地 fixture 诊断；没有修改生产逻辑、重启服务或调用真实账号接口。

## 结论与证据边界

用户报告的完整错误包含：

> 旧 2FA 停用状态未能核实；尚未创建新密钥，恢复会话已保留。账号安全请求返回 HTTP 500；不会自动重试

这段错误中的 HTTP 500 来自停用请求之后的 `GET /backend-api/accounts/mfa_info`。它不是首次登录前的 token 缺失错误，也不能直接解释为停用 POST 失败。

可以确定：

- 本次已经走过登录，并成功读取了足以选择唯一旧 TOTP 因子的状态，随后才进入停用阶段。
- 停用请求已经进入发送流程，随后程序转入只读状态核实；核实请求收到 HTTP 500。
- 本次在创建新密钥之前停止；未进入后续 enroll/activate 流程。
- 本地把修改结果标为 unknown，并保留账号隔离，阻止普通登录、重新发起重置和凭据导出。
- 当前四次核实预算不覆盖 HTTP 500；HTTP 错误会立即退出核实循环。

目前不能确定：

- 前一个停用 POST 是明确成功、5xx、断连，还是响应结构发生变化；这些路径均可能进入相同的核实分支。
- 旧因子当前到底存在还是已停用。
- 上游为什么返回 500，或者 500 具体由应用服务、网关或代理中的哪一层生成。
- 真实恢复会话在服务端现在是否仍有效。元数据未报告会话脏状态或保存失败，不等于当前会话有效性已经重新验证。

本轮未解密实际私有恢复记录、读取 Keychain 或输出 Cookie、token、TOTP 密钥。现有公开字段不足以补全上述证据。

## 本次实际运行阶段

从运行实例的 `data/job-meta.json` 筛选本次操作的时间、阶段和错误匹配结果。下表全部为 2026-11-07 北京时间（UTC+08:00），没有列出邮箱或凭据。

| 时间 | 记录 | 含义 |
| --- | --- | --- |
| 01:27:10.442 | queued / reset-2fa | 本次操作开始 |
| 01:27:11.540 | totp_reset_read | 读取初始会话 |
| 01:27:12.406 | totp_reset_login | 进入首次登录 |
| 01:27:12.457 | login | 执行登录流程 |
| 01:27:27.892 | totp_reset_read | 登录后再次读取 |
| 01:27:31.803 | totp_reset_disable | 进入旧因子停用请求 |
| 01:27:33.558 | totp_reset_verify_disabled | 进入停用后的只读核实 |
| 01:27:38.807 | totp_reset_verify_disabled / failed | 匹配用户的完整 HTTP 500 错误 |

最终元数据：`totp_reset_status=unknown`、`totp_reset_phase=recovery_required`、`totp_recovery_pending=true`、`totp_credential_invalidated=true`、`totp_session_dirty=false`、`totp_activation_verified=false`。

`totp_credential_invalidated=true` 是本地隔离标记，不能当成远端已经停用旧因子的事实。`totp_known_enabled=false` 同样不能代替远端核实。

本轮核对时 4401 的监听进程仍为 PID 36580。stderr 没有匹配本次任务或该时间窗口的记录，未能补充 POST 回执。

## 调用链与停止位置

1. `internal/protocol/totp_reset.go:142` 准备 Web 授权，`:152` 首次读取 mfa_info，`:157–164` 要求唯一明确的旧因子。
2. `internal/protocol/totp_reset_state.go:225` 发出 `POST /backend-api/accounts/mfa/user/disable_in_house`。
3. POST 成功且 message 匹配时，`:239` 记录 `old_factor_removal_acknowledged=true`；POST 结果不确定时，`:237` 记录 `disable_response_error` 和 `disable_unconfirmed`。后者也进入只读核实，不重发停用 POST。
4. `totp_reset_state.go:244–246` 进入核实并生成本次错误前缀。实际查询位于 `internal/protocol/totp_reset_verify.go:22`。
5. `internal/protocol/totp_reset.go:421–422` 将非 2xx 响应包装为“账号安全请求返回 HTTP …；不会自动重试”。该文案同时用于 GET 和 POST，没有表达请求类型。
6. `totp_reset_verify.go:28–31` 遇到错误立即返回。只有成功取得可解析的响应但状态尚未同步，才继续最多四次状态读取，间隔为 0.5、1、2 秒。
7. `internal/transport/read_retry.go:265–267` 收到 HTTP 响应即返回；其网络重试预算不会补上 HTTP 500 重试。
8. `totp_reset.go:174–178` 只有停用核实成功后才进入创建与激活新因子的流程；因此本次错误发生时尚未创建新密钥。
9. `totp_reset.go:103–105` 将最终阶段统一为 recovery_required；`internal/jobs/totp_reset.go:231–255` 将状态与隔离结果投影到账号，`internal/jobs/totp_reset_diagnostic.go:13–25` 组合用户看到的第二层警告。

当前 Git HEAD 基线同样在停用 POST 后执行一次 mfa_info GET，并在读取失败时停止。因此没有证据将这个 HTTP 500 归因于 Cookie 管理库迁移。

## 本地策略与诊断缺陷

### 只读 HTTP 错误直接终止

防止重放不确定的修改请求是必要保护，但当前处理把状态查询的短暂 HTTP 5xx 也直接当成终止条件。四次核实不是“四次 HTTP 错误重试”。这解释了本次为何未继续核实便进入恢复状态，但并不解释远端生成 500 的原因。

### failure_site 没有随核实阶段更新

`totp_reset_state.go:192` 在停用派发时设为 `disable_request`，随后进入 GET 核实前没有更新。若仅看这个字段，容易把 GET 失败误判成 POST 失败。

全仓 Go 引用检索没有发现业务判断读取该字段；这属于诊断归因缺陷，不是本次停止或重复请求的原因。

### 公开日志无法区分停用回执

公开任务状态只提升通用状态、阶段、错误及隔离标志，没有提升 `old_factor_removal_acknowledged`、`disable_receipt` 或 `reset_totp_last_phase`。

`internal/protocol/totp_reset_receipt.go:50–54` 在私有记录保存 step、http_status、body_complete、termination、received_at 等回执数据。未读取真实私有记录时，不能判断本次 POST 的具体回执。

“恢复会话已保留”也应结合最终保存结果解释：`totp_reset.go:125–127` 会在保存失败时追加错误，不能只凭通用前缀证明持久化成功。

## 本地验证

子代理通过临时 Go overlay 对现有生产函数注入合成响应，七个分支全部通过（1.370s），临时文件已清理，生产源码未变更：

| 合成场景 | 已确认行为 |
| --- | --- |
| 停用 POST 明确成功，随后 GET 500 | 同一用户错误；停用 POST 1 次，后置 GET 1 次，enroll/activate 0 次 |
| 停用 POST 500，模拟远端已经删除，随后 GET 500 | 同一用户错误；未重放停用 |
| 停用 POST 500，模拟远端尚未删除，随后 GET 500 | 同一用户错误；未重放停用 |
| 停用响应 EOF，模拟远端已经删除，随后 GET 500 | 同一用户错误；未重放停用 |
| 停用 POST 200 但响应字段变化，随后 GET 500 | 同一用户错误；未重放停用 |
| 连续四次成功读取但仍为旧因子 | 使用全部四次核实预算 |
| 首次读取仍为旧因子，第二次 GET 500 | 第二次立即停止，不使用剩余预算 |

上述 fixture 中，模拟轮换 Cookie 可从私有恢复 checkpoint 读回；之后仅进行只读 inspect 可分别观察到 disabled 或 old_factor_enabled。它们验证代码在这些情形下的行为，不能代替真实账号的回执与状态。

现有定向测试也通过：协议不确定请求与远端成功后响应异常等测试 8.411s，任务诊断保留/脱敏测试 0.836s，公开 API 隐私边界测试 0.664s。

## 建议的后续优化（本轮未实施）

1. 仅对状态核实 GET 的明确可重试 5xx（例如 500/502/503/504）增加有界退避，明确总预算与取消期限；不把该许可扩展到结果不确定的停用、登记、激活 POST。
2. 401/403、账号不匹配、因子冲突、恢复数据保存失败等继续走各自的停止路径；不靠重复登录消解修改结果未知。
3. 记录脱敏的请求方法、固定 endpoint、当前阶段、核实次数和状态码；区分停用回执“确认成功 / 结果未知 / 确定未发送”和后续读取失败，避免公开原始响应与凭据。
4. 在进入停用后、激活后核实阶段时更新 failure_site；对保存失败使用单独的诊断阶段。
5. 当前这笔任务应先复用绑定的原会话做只读核实。确认停用后才进入原任务续接；确认旧因子仍在也不能直接抹掉历史派发证据、自动重发停用请求。

本轮只新增本诊断文档。没有更改 Cookie 管理库、清除账号隔离、重启服务或对真实账号执行检查/恢复/重置。
