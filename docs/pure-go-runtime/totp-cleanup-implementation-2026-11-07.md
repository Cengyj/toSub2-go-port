# 2FA 旧流程清理实施记录

> 历史记录：本文描述当时版本，涉及 Cookie 持久化、登录 checkpoint 或远端恢复的能力现已移除。当前行为与清理范围见 [登录会话清理](authentication-session-removal-2026-11-08.md)。

> 范围纠正（2026-11-08）：本文只完成了 2FA 专项清理，所称“普通账号会话”的 AccountSessionStore 也是本地新增，不属于原仓库逻辑。整个项目的 Cookie 持久化清理以 [全项目清理记录](cookie-persistence-cleanup-2026-11-08.md) 为准。本文保留用于记录当时范围，不能作为全项目已清理的结论。

本记录覆盖源码、任务调度、控制台、前端和测试。当前使用说明见 [设置与重置 2FA](totp-reset.md)。保留工作区既有未提交改动；没有操作真实账号或部署。

## 清理前实际问题

此前已经实现明确停用成功后直接登记、兼容 HAR 中 null 附加字段，以及激活成功回执和状态核实两种确认来源。但旧的独立核实、跨任务恢复、重认证候选、专项 Cookie owner/snapshot、会话退休和完成交接仍是可执行功能，不是仅有历史文件。

尤其是“已经激活成功”与“本地保存完成”的下游条件仍和特殊会话生命周期耦合。只删除一次 GET 或隐藏恢复按钮不足以删除这些依赖。

## 已移除的执行能力

| 层级 | 清理内容 | 当前边界 |
| --- | --- | --- |
| 协议 | 独立 RecoverTOTP、ResumeTOTPReset、InspectTOTPReset、重认证与远端续接 | 只保留一次设置/重置中的必要步骤及未知结果的有限核实 |
| 协议 | reset 专项 Cookie 快照、dirty owner、跨任务 lease、prepared 续接 | 沿用普通账号登录与普通会话；操作证据不包含 Cookie 或 checkpoint |
| 协议/任务 | session publish/retire/handoff 和强制“查过才完成”依赖 | 激活证据＋精确凭据保存读回＋持久元数据提交 |
| 队列/应用 | inspect/recover/reauth 队列、输入豁免、批量恢复、启动自动续接 | 未知状态隔离；complete-2fa 只完成本地保存 |
| 控制台/前端 | 核实、恢复、验证身份恢复按钮与公开能力字段 | 旧路由拒绝；“完成本地保存”仅对已确认激活的记录开放 |
| 测试/文档 | 只服务旧产品的测试及当前说明 | 替换为当前链路测试；历史审计明确标记已被取代 |

没有保留永远返回失败的旧 API stub，也没有把旧远端恢复改名后继续执行。

## 保留的保护及原因

1. **当次会话内的有限核实**：只处理未知移除或激活结果，不是独立核实产品。明确成功时不会进入此分支。
2. **操作证据与待保存密钥**：记录操作、账号、因子、attempt、完整响应及确认来源，避免因响应丢失重复修改，支持可靠保存。
3. **正常账号会话与账户锁**：沿用项目已有体系。纯本地完成可复用普通 owner 已持有的账户锁，不加载或刷新 Cookie，不建立连接。
4. **隔离字段**：TOTPRecoveryPending 等历史命名仍用于防重复修改、导出和无关操作；它们不再授权任何恢复入口。
5. **只读历史防误用检查**：拒绝把旧专用 Cookie、旧 checkpoint 或已退休快照导入普通会话。不能写新快照、候选、迁移标记或退休回执。
6. **历史资料原字节保留**：旧 session-bearing journal 可能是唯一的 Cookie 来源证据，不能为了本地完成把该证据删掉。有效旧激活证据可参与本地凭据和元数据核对，原文件保持只读；未知记录仍需人工处理。

因此，源码仍出现 recovery、is_recovery 或旧字段名并不等于旧功能仍可执行。is_recovery 是远端因子类型；正常 OAuth/session recovery 属于原项目；少量旧字段和文件名只用于拒绝误用或读取历史身份。

## 本轮进一步发现并修正的边界

- 普通 Cookie 保存失败可能保留账户锁。完成本地保存必须复用这个普通锁，不能另抢同一锁后把已确认成功阻塞。
- 旧 journal 的 Cookie/checkpoint 来源不可被“清洗后重写”抹掉，否则旧资料可能重新变成可复用的普通会话。旧文件改为不可写。
- 首次设置原保存路径缺少完整读回。现在激活后的完成标记在凭据保存和读回校验之后提交。完整激活回执也不再被普通 Cookie 保存或最后登录检查点保存失败推翻；普通会话的失败屏障仍保留。
- 新一轮明确手动重置不能被上一轮已完成 journal 永久阻塞，也不能自动覆盖未知记录。仅已完成且匹配凭据的当前无 Cookie 证据可以通过显式新操作的本地归档流程替换。
- 历史已完成任务不能随新一轮操作被重新绑定或被新 vault 的变化推翻。当前提交与历史完成分别校验。

## 验证要求

所有网络行为均由本地合成响应或模拟服务实现，并断言请求顺序和次数：

- 明确停用成功后直接登记，明确激活成功后直接保存；额外状态接口预设失败也不影响完成，因为该接口不应被调用。
- 响应丢失只核实，不重复停用、登记或激活；未知登记缺少材料就停止。
- 仅明确验证码错误进入验证码重试；空对象、解析失败、矛盾字段、错误账号/因子不被误判成功。
- business_receipt 和 state_read 两种来源均可完成，不伪造查询标记。
- 本地完成零建连、零登录，拒绝远端选项、错误操作绑定、缺失 seed 和冲突证据。
- 首次设置未知登记即使无 seed，也在当前进程及重启后阻止重新登记。
- 普通会话失败保存、密钥读回失败、元数据提交失败均保留正确边界；旧入口与旧批量入口拒绝。

## 最终验证结果

- go test ./... -count=1：全部通过。包含 cmd/tosub2、protocol、jobs、application、console、transport 及项目其余包。
- go vet ./...：通过。
- git diff --check：通过。
- protocol、jobs、cmd/tosub2 的成功回执、未知结果防重、本地完成、再次重置与历史证据重点用例，使用 -race -count=5 重复验证全部通过（44.243s、50.620s、37.014s）。
- 最后测试修正后，go test ./cmd/tosub2 -count=1 再次完整通过（49.745s）。
- 前端通过项目正常脚本构建；web/dist 与 cmd/tosub2/web/dist 逐文件一致，旧恢复/核实入口及能力字段不在产物中。
- 删除了仅供旧专用退休体系使用的 CookieJar.Retire/ErrCookieRetired，以及无生产调用者的 ReuseSessionOnly 旧选项；普通 Cookie 保存与账户 owner 保护保留。

首次整仓回归暴露的三组再次重置测试已更新为真实新操作意图和对应激活证据，并在最终整仓回归通过。历史文档中的旧测试结果和部署记录不能替代以上验证。

最后并发回归另发现两处测试在启动真实队列后，又手动调用同一任务的 runner，导致防重保护正确拒绝第二次执行。已将这两处改为只等待队列完成；没有放宽生产防重条件。修正后重复并发回归和 runner 包完整回归均通过。

重点并发验证命令：

```sh
go test -race ./internal/protocol ./internal/jobs ./cmd/tosub2 -run 'TestResetTOTPBusinessReceipts|TestResetTOTPActivationUnknown|TestSetupTOTPConfirmedActivation|TestTOTPComplete|TestTOTPCompletion|TestTOTPFinal|TestExplicitNewReset|TestHistoricalCompletion|TestSetupUnknown|TestTOTPNewResetArchives' -count=5
```
