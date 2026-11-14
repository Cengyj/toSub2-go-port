# Cookie 来源与认证状态分层审计

> 历史记录：本文描述当时版本，涉及 Cookie 持久化、登录 checkpoint 或远端恢复的能力现已移除。当前行为与清理范围见 [登录会话清理](authentication-session-removal-2026-11-08.md)。

> 全项目范围纠正（2026-11-08）：本文涉及的 AccountSession、Cookie 快照持久化、共享会话与 owner/dirty 机制也是后来新增，现已纳入整体清理；不能再把它们作为“原版普通会话”保留。当前边界见 [全项目清理记录](cookie-persistence-cleanup-2026-11-08.md)。下文仅作历史追溯。


日期：2026-11-07（Asia/Shanghai）。本轮响应用户要求，追溯“修改 Cookie 存储前，导入后直接重置会自动登录；修改后为何新账号被判断有 Cookie”。

## 已确认结论

匿名 Cookie 在改动前同样会产生。回归不是 Cookie 管理库给新账号伪造了登录 Cookie，而是业务层将“有可供服务器核实的 Cookie 候选”误用于“可以跳过登录”。

### 导入与第一次 Cookie 的时间线

1. 批量导入 createBatch 固定使用 ImportOnly:true；Manager 将账号设为 imported，未加入执行队列。导入本身不请求账号服务、不构造登录会话。
2. 点击重置后才启动 runner。没有 checkpoint 或同邮箱历史 account session 时，prepareLegacyAuthentication 创建空 Jar，然后先 GET 首页。
3. 首页即使尚未登录也可以返回 Set-Cookie。TLS transport 在响应头到达时将它们交给同一 Jar，后续请求按 URL 选择和发送。
4. 因此，刚导入时无 Cookie，与执行登录前已有匿名 Cookie，并不矛盾。Cookie 出现在点击重置后的连接预检阶段。

对应入口：internal/console/server.go createBatch；internal/jobs/manager.go Submit；internal/jobs/admission.go；cmd/tosub2/main.go fresh runtime 分支；cmd/tosub2/authentication_runtime.go prepareLegacyAuthentication；internal/transport/tlsclient.go SetFromResponse。

### 改动前后真正的差异

Git 基线和当前代码均有空 Jar → 首页 GET → 收取 Set-Cookie 的预检。旧 CookieJar 也接收全部合法响应 Cookie。

旧 canReuseLogin 只在指定 session Cookie 或完成阶段 checkpoint 存在时返回 true。新实现为 ReuseSessionOnly，或者存在任意 checkpoint/任意 Snapshot.Entries。去掉固定业务 Cookie 名称判断是为了接受服务端分段或名称变化，但 prepareEnrollmentSession 仍将 true 直接解释为不需要 LoginWeb。这是没有同步修改调用者语义的回归。

Snapshot 是完整持久化内容，可能含其他域、路径、分区或尚未按请求清理的过期项；它也不是当前 URL 实际会发送的 Cookie 集合。Cookie 总数、CookieRef 和磁盘文件存在都不能证明认证成功。

## 隔离和恢复核查

- NewCookieJar 无参数时为空。legacyruntime.New 每次创建独立实例，没有找到全局共享 Cookie 容器。
- protocol、legacy runtime 与 TLS transport 共享的是同一次 operation 的 Jar；底层 TLS/net/http 的额外 Jar 已禁用。
- AccountSession 按输出根目录、规范化邮箱、ChatGPT/Auth origin 隔离，读取时校验身份、origin、操作与私有绑定。
- 同邮箱重新导入可能更新旧账号行并恢复同邮箱历史候选，不等于全新身份；先前失败的准备记录也可能被恢复。这与跨账号污染不同。

## 进一步确认的公共状态问题

本轮并非仅复述上一轮重置入口补丁，还发现了以下剩余问题：

1. SetupTOTP 仍使用 prepareEnrollmentSession。匿名 Cookie 加 200 应用外壳时，当前实现跳过登录，随后仅报 TOTP_ACCESS_TOKEN_MISSING。临时 Go overlay 对比确认：同一合成场景当前 login=0/page=1/session=0；仅换回 Git 基线的判断，login=1 且设置成功。不能将这个结论泛化到 AddPassword 或主 OAuth：前者另有重认证流程，后者 fresh 路径直接调用 LoginWeb。
2. AccountSession 保存的是候选，却在 record.checkpoint() 中统一构造 email_verified 阶段。web_email_otp 的未完成登录候选也会生成 CookieRef。引用只证明保存来源和版本，不证明登录完成。
3. 上一轮重置补丁以 CookieRef 非空关闭首次登录资格，生产 SaveCheckpoint 回调会在首次邮箱/MFA 阶段就产生引用。因此首次登录中断后的重试仍可能被拒绝。使用真实 AccountSessionStore.SaveCheckpoint 回调的合成复现已确认；上一轮仅使用内存协议 fixture 的中断测试未覆盖这个边界。

上述两项剩余功能问题在本轮审计中被确认，不能将上一轮部署的重置入口修复描述为认证状态分层已经全部完成。后续修复应显式区分候选持久化、未完成登录进度、服务端认证确认及只允许原会话恢复，不应恢复依赖固定 Cookie 名称或通过清空 Cookie 掩盖判断错误。

## 本轮验证与范围

新增 cmd/tosub2/anonymous_preflight_cookie_test.go，直接调用生产 prepareLegacyAuthentication，而非手工往协议 fixture 塞 Cookie：首次首页请求不带 Cookie；响应下发匿名 Cookie 后同 Jar 能发送它们；服务器仍返回空会话；两个 operation 的 Jar 独立；构造 runtime 本身不联网。

测试仅使用本机 httptest。已有 Cookie 单权威、隔离、快照和持久化边界定向测试通过。本轮仅新增审计文档和预检回归，没有更改生产认证逻辑、重启服务、读取真实 Cookie/Keychain 或操作真实账号。
