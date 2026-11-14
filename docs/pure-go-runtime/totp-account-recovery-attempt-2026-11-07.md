# 已授权的账号 2FA 恢复尝试

> 历史记录：本文描述当时版本，涉及 Cookie 持久化、登录 checkpoint 或远端恢复的能力现已移除。当前行为与清理范围见 [登录会话清理](authentication-session-removal-2026-11-08.md)。

> 历史记录（已被后续清理取代）：本文描述的独立核实、远端恢复、重认证或专项 Cookie 方案不再是当前功能。当前实现以 [设置与重置 2FA](totp-reset.md) 为准；保留本文仅供审计追溯。

日期：2026-11-07（Asia/Shanghai）。用户要求优先恢复当前账号并设置新 2FA。

## 实际操作与结果

- 对既有恢复任务调用一次本地控制台 `recover-2fa` 正式入口，使用新的幂等操作标识。未直接重放任何停用请求，未清除隔离标志。
- 提交前记录显示，01:46:09 的只读核实已确认 disabled；恢复资格为可用。
- 02:19:36.479 本次恢复操作开始。
- 02:19:39.430 使用保存的原会话再次核实账号和停用状态后，进入新密钥登记。
- 02:19:40.740 恢复失败，阶段为 `totp_reset_enroll`，HTTP 401。
- 无新密钥返回，未执行激活，新 2FA 尚未设置成功。原会话和恢复记录仍保留。

## 已保存响应中的明确原因

通过项目现有安全存储读取器在内存中解读回执，仅输出非秘密白名单诊断；未打印 Cookie、token、密码、密钥或原始恢复记录，也未改写 journal。读取前后 journal SHA-256 一致。

- 原停用回执：HTTP 200，响应完整，业务消息匹配 `Factor disabled`；`old_factor_removal_acknowledged=true`。这补全了此前只查公开日志时无法确定的停用回执证据。
- 本次登记回执：HTTP 401，响应完整。`detail` 内的错误码为 `recent_auth_required`，类型为 `invalid_request_error`。
- 官方返回的信息为：`User must re-authenticate to enroll/disable a factor`。
- 当前记录 `old_factor_removal_confirmed_by_read=true`、`enroll_attempted=true`、`enrollment_validated=false`、`activation_attempted=false`、`credential_persisted=false`。
- 本地存在保存的密码，没有配置邮箱取码 API，没有待激活的新密钥。

上述错误表示这次安全修改请求要求重新验证身份。当前 Cookie 仍能读取账号与因子状态，但这种读取能力不足以满足本次登记请求的验证要求；不能通过重复查询或直接重复提交登记解决。

## 当前阻塞与后续要求

用户已明确表示无法接收邮箱验证码。正在检查能否使用保存密码完成一次隔离的官方身份验证；若要求邮箱码，立即停止并保留记录，不重发验证码，不绕过官方要求。当前恢复实现不自动重新登录，并且已发送登记但无完整材料时会阻止重发；需要基于明确拒绝回执处理后续恢复，不能直接删改 `enroll_attempted` 或伪造未发送证据。

## 隔离的密码认证尝试

用户确认无法收取邮箱验证码后，额外执行了一次仅认证的临时助手：复用原 Cookie 快照副本、同一设备及代理；同账号互斥锁；100 秒期限；零网络重试；signin 与密码验证最多各一次；底层请求限制禁止修改任何 2FA 因子或退出设备。原生产服务和恢复流程没有被改写。

- 请求到达首页、providers、csrf、一次 signin 和两次认证页面导航。
- 最终结果为 `authentication_failed`，`password_authenticated=false`。没有完成密码验证，没有产生可证明近期认证成功的候选会话。
- 此次助手未保留底层失败原因，因此不能把这次认证失败直接归因为“服务器已经要求邮箱验证码”。可确定的官方拒绝理由仍是先前登记回执中的 `recent_auth_required`。
- 原 journal 和 credential 主文件的前后 SHA-256 一致。独立候选文件加密保存在私有临时目录，未绑定回生产重置记录。
- 未再次调用 enroll、activate 或 disable。

本轮未重启服务、未部署新生产代码、未执行再次删除、退出设备、Codex 重登授权或上传操作。账号尚未恢复成功，仍需完成官方要求的重新验证身份，之后才能继续设置新 2FA。
