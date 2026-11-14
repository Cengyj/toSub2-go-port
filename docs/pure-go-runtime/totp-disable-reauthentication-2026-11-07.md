# 2FA 停用请求 HTTP 401 与旧会话恢复修复（2026-11-07）

> 历史记录：本文描述当时版本，涉及 Cookie 持久化、登录 checkpoint 或远端恢复的能力现已移除。当前行为与清理范围见 [登录会话清理](authentication-session-removal-2026-11-08.md)。

> 历史记录（已被后续清理取代）：本文描述的独立核实、远端恢复、重认证或专项 Cookie 方案不再是当前功能。当前实现以 [设置与重置 2FA](totp-reset.md) 为准；保留本文仅供审计追溯。

## 本次故障

2026-11-07 11:18（Asia/Shanghai）的失败发生在停用旧因子阶段。官方完整 HTTP 401 正文为：

    {"detail":"User must re-authenticate to enroll/disable a factor"}

程序已保存完整且绑定本次请求的拒绝回执，尚未创建或激活新密钥。旧实现只识别 error.code=recent_auth_required 或 detail 内嵌该 JSON，而且认证恢复仅覆盖 enroll。因此这次 disable 的纯文本 detail 被泛化为 HTTP 401，未显示认证续接入口。

Cookie 恢复成功不代表服务端认可其近期认证资格。这条回执不证明 Cookie 丢失或 Free 套餐不支持 2FA。old_factor_invalidated=true 是本地隔离标志，不是服务器删除成功的证据。远端当前状态仍须在用户继续时核实。

## 本次调整

- 新建或完整 initial 阶段且尚未发送安全修改时，服务端明确返回匿名、401 或登录跳转，可以在本次用户操作中登录一次。以前完成过登录不再使此入口永久停住。身份错配、未确认身份、5xx、超时或不完整修改记录不授予登录许可。
- 普通恢复、只读检查与 prepared 记录仍保持原会话边界；重启不会自行登录或修改 2FA。
- 完整匹配的 disable/enroll 认证拒绝均开放“验证身份并恢复”。精确兼容本次 detail，保留机器码及旧嵌套格式，不泛化任意401。
- 候选和采用证明版本2绑定账号、操作、具体步骤、修改请求 attempt、认证 attempt 和候选摘要。旧版本1仅用于 enroll，不能取得 disable 权限。
- 停用前的独立认证可完成官方原因子 MFA。自动种子仅来自当前重置操作匹配的安全凭据，不借用环境变量、请求输入或待激活种子。无有效旧种子时等待手动验证码；enroll 模式不使用旧因子。
- 认证成功后核实同一账号和唯一原因子。原因子仍在则归档原拒绝回执与证明，建立新的停用尝试；若已明确关闭，续接登记。错误账号、不同因子或未知状态停止。
- 停用完整成功回执可靠保存后直接登记新因子，不插入停用后的额外 GET。
- Cookie 仍由标准 cookiejar 管理，不按名称、分片或外形判登录。原会话、dirty 状态、账号锁和一次性请求防重放保留。
- 界面公开有限的 totpReauthenticationStep 枚举，停用前拒绝不再显示“不再次移除旧因子”等不适用文案。新日志区分等待官方身份验证与未知结果；不重写历史日志或真实恢复证据。

## 验证范围

仅使用合成账号、本地 HTTP 服务与临时安全存储，覆盖拒绝分类、API准入、独立密码/MFA/回调、身份和因子核对、历史与候选归档、普通恢复禁止隐式登录、未知请求不重放以及界面确认。最终验收结果如下。

另对用户提供的 chatgpt.com.har 作只读比较：issue_challenge、verify 和 disable_in_house 请求使用的因子 ID 均与该抓包 mfa_info 的 TOTP 因子一致。本次仅输出字段名和相等性，不输出 Cookie、密码、验证码、因子 ID 或其他账号资料；未重放请求。

修复不代表真实账号已恢复。若官方要求无法取得的邮箱验证码，必须等待验证完成，不能承诺绕过。


## 最终验收与上线

- 全仓 go test -p 4 ./... 通过；protocol 161.644 秒、runner 120.054 秒、console 92.162 秒。
- 新状态机、候选认证、初始会话回退、真实 runner、API/UI 和错误诊断的相关 race 回归通过；go vet、前端构建、CGO=0 生产构建及 diff 检查通过。
- 首轮全量测试发现的两类回归已修正：不完整400/401/403保留HTTP拒绝诊断并停止，不增加多轮状态读取；旧测试中完整匿名空对象不再误作身份未知，另验证允许登录后仍须严格核实身份。
- 2026-11-07 12:43:27（Asia/Shanghai）完成 4401 验收，PID 59235。健康检查与 HTML/CSS/JS 均匹配构建，后台原设置已恢复，无活动任务。
- 完整备份 324 个数据文件；9 个账号保留，36 个凭据、Cookie 和私有恢复文件逐字节不变，原 OAuth 授权输出也逐字节不变。
- 部署守卫拦下失败任务 result_saved 的 true→false。只读独立审查证实这是已有 restore 规则：该任务选择后续重登，但重登状态为 skipped；规则与 git HEAD 相同，旧授权文件没有删除。仅接受这一项明确校正，其余业务字段严格一致，没有回写或删改真实恢复资料。
- 通过本地只读 API 确认本次失败账号 canRecoverTotp=true、totpReauthenticationRequired=true、totpReauthenticationStep=disable。没有点击或执行真实账号恢复，重启也未开始认证或安全修改。
- 二进制 SHA-256：ef20948e48e9c18b00c89025f6271368cb528737d2d93acceee9fe6f0ae5e235。

部署回执：[ui/totp-disable-reauth-20261107-deployment.json](ui/totp-disable-reauth-20261107-deployment.json)。本轮无 Git 提交、推送或真实账号试跑。
