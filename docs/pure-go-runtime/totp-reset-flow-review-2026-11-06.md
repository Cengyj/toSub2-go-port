# 2FA 重置全流程审查与优化

> 历史记录：本文描述当时版本，涉及 Cookie 持久化、登录 checkpoint 或远端恢复的能力现已移除。当前行为与清理范围见 [登录会话清理](authentication-session-removal-2026-11-08.md)。

> 全项目范围纠正（2026-11-08）：本文涉及的 AccountSession、Cookie 快照持久化、共享会话与 owner/dirty 机制也是后来新增，现已纳入整体清理；不能再把它们作为“原版普通会话”保留。当前边界见 [全项目清理记录](cookie-persistence-cleanup-2026-11-08.md)。下文仅作历史追溯。


> 历史记录（已被后续清理取代）：本文描述的独立核实、远端恢复、重认证或专项 Cookie 方案不再是当前功能。当前实现以 [设置与重置 2FA](totp-reset.md) 为准；保留本文仅供审计追溯。

日期：2026-11-06。审查对象：当前工作区的控制台、任务调度、生产 runner、协议状态机、Cookie 持久化、凭据与会话交接，以及相关导出和上传入口。

后续的多轮故障注入与跨实例修复见[严格审查报告](totp-reset-strict-review-2026-11-06.md)。本文保留上轮结果与旧候选记录；新候选和最终验收以严格审查报告为准。

本轮在已有 Cookie 管理改造上修复可复现的问题，不推倒现有状态机。所有故障注入均使用合成账号、模拟凭据库、临时目录和本机测试服务；未操作真实账号、系统凭据库、现有运行服务或真实恢复材料。候选构建与最终验收记录见文末。

## 结论与原日志的含义

`TOTP_RESET_RECOVERY_REQUIRED` 表示远端操作、本地持久证据或二者的一致性尚未确认，不能据此断言“官方移除失败”或“Cookie 格式不合格”。原日志只有通用错误和阶段，不能单凭它还原 2026-11-05 那次请求究竟在哪一步成功。`CODEX_AUTH_LOGIN_REQUIRED` 也不能证明 Cookie 保存文件损坏；它是认证路径要求登录的信号，需要结合当次接口响应和私有恢复证据区分。

重置要把三件事情分别记录：远端因子是否变化、新 seed 是否可靠保存、可续接的会话是否可靠交接。任一项未知，都不能用“从头再登录、再移除”替代恢复。本轮修复了确实可能导致状态误判、重复交接失败或保护入口不一致的代码路径，未宣称每个修复都是原历史故障的唯一原因。

Cookie 仍由固定版本 `go.nhat.io/cookiejar v0.4.1` 的 vendored core 管理，项目保留无损 Snapshot、加密落盘、操作绑定和保存失败屏障。不是直接依赖第三方库自动完成跨文件事务。请求发送、响应更新及恢复都通过同一适配器和 core；不依赖登录 Cookie 名称，不拼接或校验 `.0/.1` 连续性。域、路径、Secure、过期等是 Cookie 标准选择规则；原会话是否仍获服务端认可，由实际只读请求确认。

## 实际运行路径

| 层 | 入口与职责 |
| --- | --- |
| 页面与 API | `web/src/lib/totpIntent.js`、`internal/console/totp_actions.go`：操作意图、资格、单条和批量排队；公开状态不输出 seed、Cookie 或原始私有响应。 |
| 账号调度 | `internal/jobs/totp_reset.go`、`totp_workflow.go`、`restore.go`：同账号排他、隔离、凭据/元数据提交、恢复和重启状态。 |
| 流程编排 | `internal/application/totp_workflow.go`：2FA → 可选退出 → 可选重新授权 → 可选上传；前一步未确认，后续不启动。 |
| 生产 runner | `cmd/tosub2/main.go`、`totp_reset.go`：原会话/代理/传输身份恢复，配置凭据回调，优先处理已经验证的纯本地收尾。 |
| 协议状态机 | `internal/protocol/totp_reset.go`、`totp_reset_resume.go`、`totp_reset_state.go`、`totp_reset_verify.go`：读取身份与因子、发送请求、分类未知结果和有限只读复核。 |
| 恢复证据 | `totp_reset_session.go`、`totp_reset_owner.go`、`totp_reset_receipt.go`、`totp_reset_evidence.go`：原会话快照、收到的回执、阶段与操作绑定、dirty 状态和本地重试。 |
| 会话交接 | `internal/protocol/account_session*.go`：candidate 写入/读回、journal 交接事实、active 指针与历史 receipt，阻止较旧操作覆盖较新会话。 |
| 存储与 Cookie | `internal/credentials`、`internal/privatestate`、`internal/privatefile`、`internal/transport/cookie.go`、`internal/third_party/cookiejar`。 |

控制台使用 `ResetTOTP`、`ResumeTOTPReset`、`InspectTOTPReset`。另一个 `RecoverTOTP` 底层实现目前没有生产调用方，不能把它当成页面“恢复 2FA”的实际执行路径。

## 一次正常重置的顺序

1. **接受意图并取得账号执行权。** 拒绝同账号冲突操作；要求可用的持久凭据存储，核验此前是否仍有恢复材料。已有效的原网页会话优先复用；恢复入口本身禁止重新登录。
2. **保存原始基线和会话。** 代理、传输身份与旧凭据基线先保存并读回。允许“有效网页会话、尚无本地旧 seed”的账号建立空凭据基线；其他读取失败不能假装为空。
3. **只读验证账号及唯一旧因子。** 读取设置页获取网页授权，读取 `/api/auth/session`，再读取 `/backend-api/accounts/mfa_info`。账号必须匹配；必须有且仅有一个明确的普通 TOTP 因子。页面与接口读取均禁用挑战处理重放。
4. **保存 prepared 与隔离意图。** journal 包含账号、操作 ID、原会话、旧因子和原凭据摘要，并读回核验；凭据与任务隔离确认后，才允许移除。清理普通认证 checkpoint 不会清空当前 Jar，也不删除专用恢复会话。
5. **移除旧因子。** 在发送 `/backend-api/accounts/mfa/user/disable_in_house` 前先记录 dispatch 意图。收到的 Cookie/回执及阶段立即保存。只通过原会话 GET 核实停用，不因超时或响应字段变化再发一次移除。
6. **登记新因子。** 保存登记意图，再调用 `/backend-api/accounts/mfa/enroll`。收到的 seed、登记 session、因子 ID 进入私有恢复材料；材料不完整时停止，不重复未知登记。
7. **保存新 seed 后激活。** pending seed 写入凭据库并读回，保存激活意图和会话。完成这些本地操作后，紧邻发送时生成 TOTP 验证码，再调用 `/backend-api/accounts/mfa/user/activate_enrollment`。
8. **核实并提交有效密钥。** 激活回执不是完整完成条件：再次 GET 必须显示本轮新因子明确启用。先保存这一验证事实，再把相同操作的 seed 提交为有效凭据，读回并提交任务元数据。
9. **交接原会话。** 保存并读回账号会话 candidate，写 journal 交接事实，再耐久更新 active 指针。重复交接必须验证同源与同版本内容，不能覆盖较新操作的 active session。
10. **可选后续与清理。** 仅新重置可执行用户选中的退出设备；先交接会话再退出，结果可靠保存。退出失败/未知阻断所选重新授权和上传。journal 删除和 handoff 完成标记按既定顺序执行，失败保留待收尾状态。

## 失败、中断与再次点击

| 情况 | 实际处理 |
| --- | --- |
| 移除确定未发送 | 保留 prepared 和原会话；用户再次点击后，可完成准备并发送第一次移除。不会由重启自动发送。 |
| 移除响应丢失/结果未知 | 沿用原会话只读核实；确认停用才能登记，不根据网络错误推断移除没发生。 |
| 登记响应丢失，缺少完整 seed/session | 保留已有证据并停止；无法凭空重建从未收到的新 seed，不重发未知登记。 |
| 激活已发送但结果未知 | 只读核实是否是本轮新因子；不再次激活未知请求。已明确未发送的路径与未知路径分开。 |
| 收到 Cookie/seed 后本地保存失败 | 进程内保留受限 dirty owner 和最后收到的证据，阻断后续网络修改，允许明确的本地 flush；不能把内存状态宣告为已落盘。 |
| 新因子已实时验证，只差本地提交 | 明确点击恢复后，核对同一操作与 seed，允许零网络完成凭据、元数据和会话交接。会话过期不妨碍这类纯本地收尾。 |
| 原会话失效、官方要求重新认证 | 恢复入口停止并说明原因，不自动重新登录、不自动请求邮箱码。 |
| 只读检查 | 仅核实并记录观察结果，可持久保存最新 Cookie/诊断；不移除、登记、激活、退出、重登、上传，也不解除账号隔离。 |
| 程序重启 | 恢复记录、按钮和状态；未完成操作变为需人工继续，**等待用户点击**。不自动重置、登录、提交 vault、发布会话或清理 journal。 |

移除/激活后的可见性复核最多四次 GET，间隔 0.5、1、2 秒；底层传输另有有界读取重试。4xx/因子冲突/取消不会触发新的登录；5xx 写响应可能来自已执行的操作，不能等同于“未发送”。

## 本轮确认并修复的问题

| 问题 | 修复与边界 |
| --- | --- |
| 只读检查/恢复/本地完成覆盖此前退出设备结果 | 保留原 requested、时间、succeeded/failed/unknown 及诊断；恢复的未来执行开关仍为关闭。页面标明历史结果，`not_attempted` 不再降级既有终态。 |
| 验证码生成过早 | 改为完成同步持久化后、发送激活前生成。模拟本地保存耗时 65 秒的测试，验证真正发送的是当时验证码。 |
| 设置页 GET 可进入通用 challenge solver，且挑战失败被误标为本地保存错误 | 重置、恢复、检查统一采用禁止 challenge replay 的读取方法；403 challenge 回归确认 solver 零调用、仅一次页面 GET、无修改请求，公开诊断明确说明官方要求额外访问验证，并保留底层原因。 |
| 错误包装丢失取消/存储等底层原因 | 协议读取用安全文案和私有 cause 包装；读取失败与保存失败同时发生时保留两者；公开字符串不暴露私有路径和 canary。 |
| 退出设备结果保存失败仍返回 nil | 保存失败向上返回 typed storage error，不发布已保存成功的事件；成功的远端退出结果仍保留，不被 defer 改为 skipped，后续可只补本地保存。 |
| active 指针 rename 可见但目录同步失败后，幂等重放绕过耐久确认 | 同 active 与历史 receipt 分支重写并读回未变化的指针，成功后才能确认交接；保留较新 active 和 receipt。注入连续同步故障时持续阻断清理。 |
| legacy Cookies 反复转换导致同 revision 不同 digest | 首次转换完整 Snapshot 后先耐久写回 recovery checkpoint，再写 candidate；后续复用 Creation/Seq/Revision。保留 digest/来源冲突检查。 |
| 缺失旧凭据被误当作代理绑定错误 | 仅 `os.ErrNotExist` 可建立空基线；保存/读回成功才联网。其他凭据错误继续阻断。 |
| inspect 入口错误走缺 journal 的完成捷径 | 排除 inspect，保持真正的检查路径。此前下层 jobs 已拒绝实际提交，本次修正入口和错误分类，非声称此前存在提交绕过。 |
| 检查成功但 workflow 保持 pending，重启显示中断 | inspect 纳入仅包含 TOTP 检查的 workflow；已持久终态在重启保留，但账号隔离不解除。未执行完的检查仍不自动排队。 |
| 页面禁用但单条/批量下载 API 仍能返回旧授权 | 读取前/后采用同账号实时恢复门禁，覆盖同邮箱其他任务、prepared、dirty、handoff、completion 及凭据隔离；同步公开 canDownload 和上传限制。门禁只读，不提交或清理恢复材料。 |
| 已完成退出但旧授权文件仍在磁盘，直接下载仍返回成功 | 单条下载同时核验 `ResultSaved`，读取后再次检查，停用后不再因物理文件仍存在就返回旧授权；正常已保存历史授权不受影响。 |
| 手动上传只检查选中任务缓存标记 | 发送前重新验证账号恢复边界；正常完成的历史导出及其他账号保持可用。手工 payload 的账号映射也必须经过保护检查。 |

联合全仓验收另发现并修正了公开状态投影的兼容性回归：新增导出能力投影只读取本地任务及恢复状态，不额外访问凭据库，不让已接收授权的纯本地保存依赖解锁凭据库。实际下载和上传仍执行完整后端准入，公开 `canDownload` 不是执行授权。

关键新增回归：`totp_review_regression_test.go`、`account_session_durability_test.go`、`account_session_legacy_handoff_test.go`、`totp_reset_empty_vault_test.go`、`totp_export_test.go`、`totp_export_gate_test.go`。生产 runner 的 legacy 本地完成回归确认零网络、原退出结果保留和重复交接幂等。

## 仍需明确的能力边界

- Cookie 持久化保存的是客户端拥有的会话材料，不能保证官方永不撤销会话。取消特定业务 Cookie 字段检查也不能绕过官方认证。
- 客户端不能把服务端移除与本地磁盘写入做成同一原子事务；目标是保存足够证据、禁止未知写重放并提供明确恢复路径。
- 进程仍活着时 dirty owner 能保留最新收到但未落盘的材料；若磁盘一直失败后进程又被杀死，最后未落盘数据无法保证恢复。
- 历史版本若已发布 legacy 会话却从未把 canonical Snapshot 写回 journal，现有摘要冲突仍保守拒绝。本轮防止新交接再制造该问题，不忽略摘要替旧记录强行确认成功。
- 恢复前提不成立时继续保留隔离；没有清空 Cookie、删除 journal 或强制解锁的“修复”步骤。

## 验收与构建

| 检查 | 结果 |
| --- | --- |
| `go test ./... -count=1 -timeout=5m` | 全仓通过；其中 cmd 122.085s、protocol 131.279s、jobs 110.322s、console 94.438s。第一次全仓验收暴露的公开状态兼容性回归已修正，此处记录修正后的成功运行。 |
| 最后补充挑战诊断后的 `go test ./internal/protocol -count=1 -timeout=5m` | 协议全包再次通过，50.272s；新错误文案和 cause 保留覆盖 reset/inspect/resume 三个入口。 |
| 协议审查、账号会话交接及检查的定向 race | 通过，9.507s；覆盖迟生成验证码、历史 logout、读写同时失败、敏感原因不外泄和指针/legacy 交接。 |
| 生产 runner 新增边界 race | 通过，7.398s；覆盖空凭据基线、inspect 入口和 legacy 纯本地完成。 |
| 最终导出/上传与兼容性定向 race | console 3.558s、jobs 3.289s，均通过；控制台全包独立复核也通过，38.317s。 |
| 前端构建与嵌入 | `sh scripts/build-frontend.sh` 通过；`web/dist` 与 `cmd/tosub2/web/dist` 的 `diff -qr` 无差异。 |
| 静态及格式检查 | 最终源码 `go vet ./...`、`git diff --check` 通过。 |
| 双平台构建 | Go 1.27.3；macOS arm64 独立候选和 `CGO_ENABLED=0 GOOS=windows GOARCH=amd64` 编译均通过。Windows 产物仅存于本次 `/tmp` 构建目录，未部署。 |

最终独立候选：`bin/tosub2-totp-audit-20261106`，使用 `go build -trimpath`。SHA-256：

```text
5d1eedc382e51e4a0fb61c6f01e3d9ca7e485201f38ed005d4fa6a9a6981f260
```

之前的 `tosub2-cookie-cleanup-20261106`、`tosub2-cookie-recovery-20261106` 保持原样，不包含本轮完整修复。当前报告与候选已完成，本轮未替换现有服务的可执行文件、未重启服务、未对真实账号进行重置验证。
