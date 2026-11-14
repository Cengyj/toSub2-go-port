# 导入后首次重置 2FA 的登录准备修复

> 历史记录：本文描述当时版本，涉及 Cookie 持久化、登录 checkpoint 或远端恢复的能力现已移除。当前行为与清理范围见 [登录会话清理](authentication-session-removal-2026-11-08.md)。

> 历史记录（已被后续清理取代）：本文描述的独立核实、远端恢复、重认证或专项 Cookie 方案不再是当前功能。当前实现以 [设置与重置 2FA](totp-reset.md) 为准；保留本文仅供审计追溯。

日期：2026-11-07（Asia/Shanghai）。用户明确说明账号尚未登录，导入后直接执行重置。

后续完整调用链审计发现：生产 SaveCheckpoint 在尚未完成登录时也会生成 CookieRef；原中断测试未挂载该生产回调。因此本文的首次修复及部署记录不代表所有候选/认证状态问题已经解决，参见 [后续 Cookie 与认证状态审计](cookie-authentication-audit-2026-11-07.md) 的剩余问题。

## 原因

原版协议运行时的连接预检先访问首页，可能取得设备或防护 Cookie。Cookie 管理清理后，canReuseLogin 将任意 Cookie 视为可供服务器核实的候选；但重置调用的 prepareEnrollmentSession 在候选存在时直接跳过 LoginWeb，没有补上服务端未登录时的首次登录分支。

随后 ResetTOTP 只从启用页面的 HTML 提取 accessToken，提取不到立即报 TOTP_RESET_NOT_STARTED。它尚未读取 /api/auth/session，也未发送移除因子的请求。这与用户“导入后直接重置”的操作相符；不是从空 Cookie 中提取 Cookie，也不是成熟 Cookie Jar 拒绝分段字段。

另一个独立缺口是：有效会话也可能只有 /api/auth/session 返回 accessToken，而页面不内嵌它。旧逻辑忽略该接口中的令牌，因此同样提前失败。日志本身不能区分这两个分支。

## 修复

- 首次重置将 Cookie 作为候选交给服务器核实；仅明确登录跳转、HTTP 401 或有效空会话对象允许尚未完成登录的账号执行一次 LoginWeb。成功登录后重新读取网页会话，才允许查询、移除和替换因子。
- 当前会话接口的 accessToken 优先；只有实时邮箱和 account ID 核验成功后，才允许同次页面令牌作为兼容来源。网络、挑战、非成功 HTTP、结构错误和身份冲突不能通过页面令牌绕过。
- Reset、Resume/Inspect 和独立 Recover 使用相同令牌读取逻辑。恢复和只读检查仍禁止重新登录、重复未知修改及退出设备。
- 私有准备 journal 保存 initial_login_allowed。完成登录的原 checkpoint、已确认网页身份、已有账号会话引用都会关闭资格；该状态先保存并读回，再允许后续请求，避免再次点击或进程重启后误触发首登。
- 兼容本次旧失败记录时，必须同时具备 mutation_schema=1、完整 initial 阶段、三个 dispatch 均为 not_sent、无账号/旧因子/新密钥/发送时间证据、完整原准备快照，且当前 runner 和原快照均无完成登录证据。已有 false 或类型异常标记不能迁移为 true；缺 schema 或缺准备快照也不授予首次登录资格。
- 首次登录失败单独显示“首次网页登录未完成；尚未修改 2FA”，保留错误链，不将其笼统归为 Cookie 保存或恢复资料失败。

## 回归覆盖

新增 totp_web_authorization_test.go 和 totp_reset_first_login_test.go，以合成服务完成完整的登录和因子流程。覆盖页面无令牌、会话令牌更新、匿名 Cookie、401/空会话、身份拒绝、首次读取和输入中断后的显式重试、登录完成后后续失败、旧记录兼容及资格关闭、恢复/检查不重登、修改请求不重复。

旧实现已通过红测试复现原文报错；修复后定向协议与边界测试通过。未使用真实账号执行登录、查询因子或重置验收。

## 最终验证与部署

- 全仓回归通过：go test ./... -count=1 -timeout=240s，protocol 144.728s、cmd/tosub2 130.968s、jobs 122.329s、console 102.533s。
- 对旧准备记录的显式 null 会话引用补充兼容回归后，再次通过首次登录、令牌、准备阶段的竞态测试（6.653s）；go vet ./...、生产构建及 git diff --check 均通过。
- 2026-11-07 01:15:25（Asia/Shanghai）已更新本地 4401，PID 36580，健康检查通过。9 个账号业务状态、25 个受保护凭据/会话文件及原设置均保持一致；前端仍是已验收的用量与探针设计。
- 安装程序 SHA-256：8dc4ed7cfe7f3972801c3decd1990335df20d41c15b64fe52883702ef6203f65。
- 备份位于原运行目录 backups/totp-first-login-20261107-011523，包含旧程序、数据、日志和启动配置。部署回执：ui/totp-first-login-20261107-deployment.json。
- 升级不自动触发用户重置；原已启用监控按原设置恢复运行。真实登录和因子修改仍等待用户在页面点击并完成官方验证。
