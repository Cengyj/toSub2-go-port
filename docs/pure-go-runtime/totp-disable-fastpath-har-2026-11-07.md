# 依据 HAR 调整 2FA 停用成功后的续接

> 历史记录：本文描述当时版本，涉及 Cookie 持久化、登录 checkpoint 或远端恢复的能力现已移除。当前行为与清理范围见 [登录会话清理](authentication-session-removal-2026-11-08.md)。

> 全项目范围纠正（2026-11-08）：本文涉及的 AccountSession、Cookie 快照持久化、共享会话与 owner/dirty 机制也是后来新增，现已纳入整体清理；不能再把它们作为“原版普通会话”保留。当前边界见 [全项目清理记录](cookie-persistence-cleanup-2026-11-08.md)。下文仅作历史追溯。


> 历史记录（已被后续清理取代）：本文描述的独立核实、远端恢复、重认证或专项 Cookie 方案不再是当前功能。当前实现以 [设置与重置 2FA](totp-reset.md) 为准；保留本文仅供审计追溯。

日期：2026-11-07（Asia/Shanghai）。用户要求成功停用旧 2FA 后直接创建新 2FA，仅在结果不确定时读取网页可见状态。

## HAR 证据与范围

只读分析用户指定的 `/Users/ceng/Downloads/chatgpt.com.har`（73,612,041 字节、963 条记录）；未重放请求、导入其中的会话或将真实凭据复制到测试。以下 entry 索引从 0 开始。

- #949：北京时间 02:21:44.244，GET `/backend-api/accounts/mfa_info`，HTTP 200，两个 MFA enabled 字段为 true，TOTP 因子数量为 1。
- #959：02:22:08.914，POST `/backend-api/accounts/mfa/user/disable_in_house`，HTTP 200，响应为 `message: Factor disabled`、`passkey_credential_manager_sync: null`。
- #960：02:22:10.148，GET 同一个 `mfa_info`，HTTP 200，`mfa_enabled` 与 `mfa_enabled_v2` 均为 false，`factors.totp` 为空数组，默认因子为 null。
- #944 的网页代码使用 `factors.totp.length > 0` 判断 Authenticator app 是否启用；show_sms/show_passkey 是显示能力，不能作为已启用证据。
- #943 的 disable onSuccess 会刷新 MFA 查询以更新界面。该 HAR 没有实际 enroll/activate 请求，没有 HTTP 500 或 recent_auth_required 响应；不能据此声称捕获了完整的重置成功链。
- 仅在内存中将 #912 会话身份与当前失败任务的公开身份作比较，结果不同。此 HAR 是接口行为样本，不能代替当前失败账号的实时证据；未记录身份值。

## 已实施

1. 同次操作取得完整、明确的停用成功响应后，先保存成功回执与 acknowledged 状态，直接进入创建新密钥，不再插入停用后 GET。
2. 快速路径保持 `disable_acknowledged`，不伪造 `old_factor_removal_confirmed_by_read` 或 `disabled_verified`。本地保存失败仍阻止创建请求。
3. 返回结构不明确、网络结果不明或 5xx 时，仅使用原会话读取上述 mfa_info；确认关闭才续接，不重发删除。
4. 状态核实的 500/502/503/504 使用同一个四轮查询预算和 0.5/1/2 秒退避。401/403/429/501、取消以及保存失败不通过此分支重试。每轮内部原有连接故障重试规则保留。
5. 跨进程恢复仍先核实当前账号和因子状态，避免把历史成功回执当作重启后的当前状态。新密钥先可靠保存再激活、激活后确认以及凭据提交次序保持。
6. 修正失败位置记录，区分停用响应保存和未知停用后的状态读取。

## HAR 额外确认的近期认证要求

#943 静态资源 `/cdn/assets/async/152707.a0352ec675.js` 的 eoyz 模块（offset 约 61995，判断函数约 63826）在 TOTP enable/disable 前检查 `Date.now() - claims.pwd_auth_time` 是否非负且小于 240000 毫秒；disable 还检查 MFA required 标记。此值是捕获版本的前端判断，不是服务端保证的有效期限。

不满足时网页调用独立重新认证；#582 HTML 的 reauthenticationParams 为 `reauth=password`、`max_age=0`、`connection=password`，login_hint 由当前账号决定。快速路径这一版尚未提供该专用分支。后续同日的[多轮严格审查](totp-reset-strict-review-2026-11-07.md)增加了明确拒绝登记后的独立认证续接，相关验证与部署以该记录为准。

快速路径优化本身不会解除既有 `401 + recent_auth_required` 记录，不会清除 attempted 标记，也不能保证当前失败账号已恢复。后续的正式认证续接需要用户明确点击并完成官方验证；HAR 中另一个账号的 Cookie 和登录提示不能用于替换它。

## 验证

新增回归覆盖：成功停用后禁止中间状态 GET、磁盘回执先于 enroll、未知停用使用 HAR 关闭结构续接、短暂 5xx 恢复、四轮预算、认证拒绝不重试、取消与保存失败停止、修改请求不重复。现有协议及运行器夹具改为由模拟远端因子状态驱动，不再用 GET 次数假定远端状态。真实进程退出回归在成功回执落盘后、enroll 意图落盘前注入退出，核实重启时保留 acknowledged 且未登记。

- 协议全包通过（50.578 秒），运行器 TOTP/Cookie/AccountSession 组通过（34.665 秒）；jobs 与 console 对应定向组通过（34.837 秒、24.175 秒）。新增成功回执保存失败的边界测试另行通过（0.955 秒）；相关 go vet、格式与差异检查通过，生产构建成功。
- 2026-11-07 04:29:12（Asia/Shanghai）已更新 4401，PID 31331，健康及前端资源验收通过。原 9 个账号业务状态与 27 个受保护凭据/会话/恢复文件保持一致；后台配置已恢复。
- 停服后完整备份并核对 299 个数据文件，再原子替换程序。安装 SHA-256 为 b3bba7f83768bbfab2004639f2b184c8a46d92208b2a6b79fe28f3aa3d34e5e7；部署回执见 [ui/totp-disable-fastpath-20261107-deployment.json](ui/totp-disable-fastpath-20261107-deployment.json)。
- 本地验收没有向真实账号发送登录、移除、登记、激活或核实请求，没有导入 HAR 中的会话，也没有修改既有失败操作的恢复状态。
