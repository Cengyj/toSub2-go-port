# reset-2fa 原会话恢复改进（2026-11-05）

> 历史记录：本文描述当时版本，涉及 Cookie 持久化、登录 checkpoint 或远端恢复的能力现已移除。当前行为与清理范围见 [登录会话清理](authentication-session-removal-2026-11-08.md)。

> 历史记录（已被后续清理取代）：本文描述的独立核实、远端恢复、重认证或专项 Cookie 方案不再是当前功能。当前实现以 [设置与重置 2FA](totp-reset.md) 为准；保留本文仅供审计追溯。

本次改进针对“有账号和旧 2FA seed、无法获取邮箱验证码、出错后只能再次点击”的使用条件。恢复不以重新登录或邮箱验证码为默认前提；普通登录、资料导出及新的破坏性重置仍受隔离保护。

## 用户可见行为

- 新重置在移除旧因子前保存独立的私有恢复会话及阶段，并读回检查。不能可靠保存时不发送删除请求。
- 删除或激活已发送但回包丢失、解析异常时，使用原会话查询实际状态，不重复发送该修改请求。
- 中途查询暂不可用、凭据保存失败或进程退出后，有完整恢复记录的账号显示“恢复 2FA”。点击后使用原任务的 Cookie 和登记材料，核对远端状态并继续可证明安全的步骤。
- 新因子已核实且证据已可靠保存，仅凭据提交、元数据更新或 journal 清理失败时，恢复只做本地收尾；不要求会话继续有效，也不重新访问远端。
- 恢复对话框不提供退出设备、重新登录、上传选项；恢复不重放原操作的这些可选后续步骤。
- 没有完整会话的历史记录明确显示不可用原因。升级不会重新构造旧 Cookie，也不会通过清空待恢复标志来放行。
- 程序重启不自动发送账号修改。恢复记录保留，用户通过恢复入口继续。

## 持久化与绑定

普通登录 checkpoint 仍与重置恢复分离。totp-setup-result.json 在原 version=1 基础上新增 recovery_schema=1，并原子保存：

- operation_id、email、account_id、old_totp_factor_id。
- recovery_checkpoint：Cookie、设备/web 状态、认证来源、TLS profile、User-Agent、有效代理摘要及操作/账号绑定。
- enroll_attempted、activation_attempted 及明确阶段；这些是写前意图，不是远端成功声明。
- 新 secret、otpauth URI、new_totp_factor_id、session_id、登记因子结构及 enrollment_validated。
- 删除/激活回执与只读确认事实，activation_verified、credential_persisted。

文件沿用私有 0600 / Windows 当前用户 DACL；读取拒绝符号链接、非普通文件、过大内容、公开权限、旧 schema、缺少真实会话 Cookie、阶段或身份绑定不一致。恢复记录不进入公开任务 metadata、日志或 API。有效代理地址仍存于受保护凭据，私有会话只携带绑定摘要。

每个关键响应及状态查询之后保存当前 Cookie，包含已收到的正常 Cookie 轮换；恢复时核对传输绑定、实时邮箱和账号 ID。实时身份响应必须成功且明确匹配，不再因缺邮箱或会话读取被拒绝而回退网页 token 放行。

收到可识别的部分 enrollment 材料会先私有保存。资料不完整时不能激活；材料存在也不等于已经验证。新 seed 写受保护凭据后读回一致，才发送首次激活。

## 恢复状态机

1. 仅读取本任务的私有记录并恢复绑定会话，不调用 LoginWeb。
2. 读取当前网页授权、实时身份和 mfa_info。被拒绝、身份不符、会话失效时停止，不自动重登。
3. 远端明确 enabled，唯一非 recovery 因子与本次新 ID 匹配，且对应完整新 seed 已保存：只补本地凭据提交和读回，不重新激活。
4. 远端明确 disabled，enroll_attempted=false：允许首次登记。
5. 已有完整且经过校验的登记材料，activation_attempted=false：补保存 pending seed，然后激活一次。
6. enroll_attempted=true 而材料缺失：不再登记，保留会话和证据并报告限制。
7. activation_attempted=true 而远端尚未确认 enabled：只进行有界状态查询，不重放激活。明确拒绝也不会被无限重试。

恢复成功前，普通操作保持隔离。新凭据提交后同步同邮箱任务。受保护凭据通过 `totpResetOperationId` 绑定所属操作；网络请求前及保存回调中均校验，已有 pending seed 也必须匹配。旧 journal 不能覆盖较新的重置或另一把待恢复密钥。

完整的 `activation_verified` 私有证据与当前同一操作的凭据匹配时，可以不构造网络传输地完成本地保存、metadata 提交及 journal 清理，Cookie 可以已经过期或为空。凭据仍 pending 时先保存新 seed 并完整读回；已经提交时必须与 journal 的 seed 完全相同。最终 journal 写入或删除失败后仍提供恢复收尾入口。未知结果不会进入该分支。

## 入口与并发

专用接口为 POST /api/jobs/{id}/recover-2fa，请求体为空对象；批量恢复使用 recover-2fa-batch。恢复不接受调用方注入 password、seed、proxy 或后续操作配置。单账号旧 reset 请求在待恢复时可路由到恢复分支；普通批量重置继续跳过待恢复账号，避免携带旧操作选项。

Manager 保持同邮箱排他调度。新增操作系统文件锁，为同一系统用户、同一规范化服务 origin 与邮箱的 setup/reset/recover 操作提供跨进程互斥，独立于输出目录。锁随进程退出释放，锁文件不删除以防 inode 竞态。它不是跨机器锁，也不宣称约束其他用户或外部客户端；普通登录/导出的跨进程全局串行不在本次锁范围内。

启动时对私有 TOTP 结果的本地恢复也取得相同账号锁，从首次凭据读取前持续持有，涵盖同目录残留的密码结果恢复；校验操作 ID、待恢复 seed 及 journal 未变化后才允许提交 TOTP，避免旧凭据快照覆盖另一进程的重置。旧 setup 结果也不能用不同 seed 覆盖已带重置操作 ID 的有效密钥。没有服务绑定的历史记录使用生产 ChatGPT origin；它不为旧记录补造可用登录会话。

## 无法保证恢复的边界

- 旧版本已经删除了唯一 Cookie，现有记录缺专用恢复会话：升级不能补回，也不会假装可以续接。
- 官方已经使所有可用会话失效，或后续操作要求新的邮箱验证：用户无法提供该验证时，项目无法保证恢复。
- enrollment 已在远端创建，但回包连 seed/session_id 都未拿到：现有代码没有经过确认的查询或幂等接口来找回这些资料，不会猜测接口或重新创建。
- 激活请求已发出但状态一直无法确认、多个因子或身份冲突：保持隔离及具体原因，不把未知当成成功或未执行。
- 写前意图保存后、真正发送前的崩溃也可能保守记录为已尝试；没有发送证明时不以可用性为由重放。
- 最新会话快照写失败或官方轮换 Cookie 的响应完全丢失：旧快照仍可能失效。跨远端与本地的多个步骤不是原子事务，不能承诺所有故障都可恢复。
- 传输层确认删除请求根本未发送的安全中止，仍回到原新重置路径，可能需要重新登录。本次免重登录续接针对已进入破坏性操作后的恢复；不把这个未发送场景宣称为无需邮箱保证。

## 验证

全部验证使用合成账号、本机 HTTP 服务或可控传输，不访问真实账号和 Keychain。测试服务“无有效 Cookie 时要求邮箱”是明确模拟的用户条件，不是对官方每次登录策略的断言。

- 原故障矩阵已改为修复验收：远端先成功再真实断连；有效会话可同次对账完成，过期会话不重新登录。
- GET 暂不可用后再次点击自动路由恢复；验证新的请求没有重复 disable 或已发出的 activate。
- 真实子进程 os.Exit(86)，不运行 defer；重启后从保留的会话继续，删除及激活各一次。
- Cookie 轮换保存复用、pending seed/最终 commit 保存失败收尾、快照失败零删除、邮箱/代理身份不符零网络。
- 最终 journal 写入和删除分别真实失败；会话过期后再次点击，零网络完成已提交结果的本地收尾。
- 已验证激活但 vault 提交失败，删除 journal 的 Cookie 后仍能以匹配的操作证据零网络提交。
- 在入队前或入队后、runner 开始前切换 vault 操作 ID，恢复都在任何网络请求之前拒绝；不等到登记新因子后才检查冲突。
- pending seed 已保存后真实阻断 metadata 写入，确认没有发送激活；解除存储障碍后续接相同 enrollment。最终凭据读回的 operation ID 被更换时，拒绝提交及后续恢复，保留证据。
- enrollment 先在远端创建再丢回包、缺 secret/sid 时始终不重发。
- UI handler 实际提交空请求，恢复卡片不附带全局代理或旧 logout/relogin/upload 偏好。
- 同邮箱并发恢复、其他任务冲突、旧 schema/符号链接/权限/大小/身份篡改阻断；进程间文件锁竞争及崩溃释放。

主要测试文件：

- internal/protocol/totp_reset_resume_test.go
- internal/protocol/totp_remote_success_audit_test.go
- internal/jobs/totp_recovery_test.go
- internal/jobs/totp_recovery_identity_test.go
- internal/console/totp_recovery_test.go
- internal/console/frontend_totp_recovery_test.go
- cmd/tosub2/totp_session_loss_audit_test.go
- cmd/tosub2/totp_resume_acceptance_test.go
- cmd/tosub2/totp_recovery_durability_test.go
- internal/accountlock/accountlock_test.go

## 最终验证结果

2026-11-05，最终生产代码及测试合并后执行：

```sh
./scripts/build-frontend.sh
go test ./... -count=1
go vet ./...
go test -race ./cmd/tosub2 ./internal/jobs ./internal/protocol ./internal/console ./internal/accountlock -run 'TOTP|Checkpoint|AccountLock|Acquire|ProcessLock' -count=1
```

- 前端构建成功；两个 dist 目录已同步。构建先完成，再启动依赖嵌入资源的测试。
- 全仓测试通过，包含最终 startup 锁上移和旧 setup 覆盖保护；cmd 51.159s、jobs 83.557s、console 78.135s、protocol 49.990s。
- 上述五包竞态检查通过：cmd 39.965s、jobs 28.656s、protocol 36.188s、console 22.879s、accountlock 2.243s。
- `go vet ./...`、`git diff --check`、临时目录中的 `go build ./cmd/tosub2` 通过。
- Windows/amd64 的 accountlock 测试二进制交叉编译通过；这不等于在 Windows 上执行了锁的运行测试。
- runner 恢复矩阵含 25 个分支，另增加 2 个真实持久化异常测试；服务端先提交因子变更再断开连接，及真实子进程退出都包含在验收中。

本次代码与前端构建完成后仍需使用新二进制运行并刷新页面才会生效；没有借测试自动重启或修改真实账号，也没有对用户日志对应的历史账号执行恢复。
