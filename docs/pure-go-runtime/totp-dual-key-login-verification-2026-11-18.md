# 并发生成的两份 TOTP 密钥：实际登录补测

> 2026-11-18 02:57 后续补测已完成同因子同时移除和旧 ID 移除：同时删除本轮均成功，双方后续设置与密钥登录验证成功；延迟使用旧 ID 返回 400，程序停止，新因子保留。为独立准备本轮，原 K10/K11 及中间 K13/K14 对应因子已明确移除；它们在下文的有效性属于历史时点。当前最后验证可用并加密保存的是 **F16/K16**，位于 `account2-remove-20261118/private/final-credentials`。详见[同时移除补测](/Users/ceng/Downloads/toSub2-go-port-2026.11.17/docs/pure-go-runtime/totp-simultaneous-removal-2026-11-18.md)。

2026-11-18 02:43—02:45，北京时间。使用前一轮 S3CC 已保存的 A=F11/K11、B=F10/K10，不生成新密钥。**两份密钥均已通过实际新登录验证：K11 对 F11 成功，K10 对 F10 成功，再用 K11 对 F11 仍成功。** 本次验证期间没有 disable、enroll、activate_enrollment 或 logout_all 请求。

这补足此前“仅 A 已验证，B 有效性未知”的限制。本结论针对该账号本次并发生成并保留的两份材料，不能推广成任何错误 ID 与任意密钥都能互相验证，也不代表每种客户端都会正确选择因子。

## 普通产品逻辑与显式因子实验

先用当前产品 `Flow.LoginWeb` 自然执行，两次新登录分别载入 K11 和 K10。登录服务在密码验证响应中列出 **F11、F10 两个普通 TOTP**；当前 [verifyMFA](/Users/ceng/Downloads/toSub2-go-port-2026.11.17/internal/protocol/flow.go:554) 总是取列表中的第一个 TOTP ID，且挑战与验证码验证均提交该 ID。它没有根据保存的密钥绑定正确因子。

因此自然执行中，A 使用 F11/K11 成功；B 虽载入 K10，却仍向 F11 提交验证码，服务端返回 `403/incorrect_code`。这只能证明这组 ID/密钥不匹配，不能证明 K10 已失效。产品之后等待用户手动输入验证码，没有自动改用 F10。

为单独验证 F10/K10，另立新的协议实验，建立全新 B 登录。临时传输观察器只在登录服务已明确提供 F10 后，才将 issue_challenge 和 verify 的 ID 选择为保存的 F10；验证码仍由原 Flow 使用 K10 生成。响应不改写，不替换密钥，不复用前一失败会话的 Cookie。必要的挑战 Referer 与请求长度同步处理。它是显式选择因子的测试，**不是产品已经具备的自动恢复能力**。

B 成功后，再建立新的 A 登录，显式选择 F11/K11，验证仍成功。A 与 B 都完成 MFA 推进、网页身份核对和受保护查询，不能仅用激活回执或 HTTP 200 的空响应冒充登录成功。

## 实际证据

D 指 `.local-run/concurrency-audit/account2-dual-seed-20261118/events.jsonl`，M 指 `.local-run/concurrency-audit/account2-matched-seed-20261118/events.jsonl`；# 为 record/行号，F/K 与原 S3CC 的代号一致。密码、验证码、Cookie、token 和 secret 均不输出。

| 执行 | 材料与实际因子 | MFA 验证请求起止 | 响应及后续证明 |
| --- | --- | --- | --- |
| 普通 A | K11 → F11 | 02:43:04.907111—02:43:06.948867，D#11 | 200，page_type=external_url；D#12 自动密钥验证回调；D#16 session 200、identity_matches=true；D#18 mfa_info 200 |
| 普通 B | K10 → **F11** | 02:43:17.335795—02:43:17.760118，D#28 | 403；error.code=incorrect_code，message=Incorrect code；D#29—31 提示验证码被拒绝并等待手动输入。未提供另一份密钥或验证码补成功 |
| 显式选择 B | K10 → **F10** | 02:44:36.188649—02:44:38.041086，M#13 | 200、external_url；M#14 自动密钥验证回调；M#18 session 200、身份匹配；M#20 mfa_info 200 |
| B 成功后的新 A | K11 → F11 | 02:45:07.781627—02:45:09.827171，M#34 | 200、external_url；M#35 验证回调；M#39 session 200、身份匹配；M#41 mfa_info 200；此后 B 的 M#42 受保护查询也仍200 |

普通登录因子列表见 D#9/#26；显式实验再次由服务端提供两个 ID，见 M#9/#30。显式选择事件见 M#10/#12 和 M#31/#33。三个成功会话的 token 指纹不同，分别使用独立客户端与 Cookie Jar，没有共享已登录状态绕过 MFA。

D 中最初失败的 B 保持在原验证码输入点，直到本次取证完成后由驱动明确取消（D#34/#35）。没有把取消写成程序自行停止，也没有把 M 的新登录算作 D 的恢复成功。

## 单个 mfa_info 条目的实际含义

本次进一步看到：

- 登录认证响应同时列出了 F11 和 F10，但 mfa_info 每次只展示一个。
- 普通 A 成功后，D#18 显示 F11。
- 显式 B 成功后，M#20 显示 F10；原 A 会话的 D#32/#33 和 B 的 M#21/#22 也一致显示 F10。
- 再次使用 A 成功后，A/B 的 M#41/#42 都显示 F11。

期间没有新增、激活或删除因子的接口调用。**展示条目变化不能解释成另一份密钥被移除或失效**；两份密钥已各自实际登录成功。列表变化与相应因子的认证相邻发生，但本实验没有在 issue_challenge 与 verify 之间插入 mfa_info 查询，不能精确断言是哪一个认证请求更新了默认展示项，也不能唯一确定服务端内部实现。

## 保存与操作边界

两份原材料继续保留于：

- K11：`/Users/ceng/Downloads/toSub2-go-port-2026.11.17/.local-run/concurrency-audit/account2-20261118/private/final-credentials`，以及 `private/S3CC/A/credentials`。
- K10：`/Users/ceng/Downloads/toSub2-go-port-2026.11.17/.local-run/concurrency-audit/account2-20261118/private/S3CC/B/credentials` 的 pending 字段及原加密操作记录。历史 S3CC 程序的 unknown 不被这次外部验证回写成产品成功。
- 本次成功验证 K10 的隔离凭据副本：`/Users/ceng/Downloads/toSub2-go-port-2026.11.17/.local-run/concurrency-audit/account2-matched-seed-20261118/private/Matched/B/credentials`；K11 对应同目录 A。

读取时核对原 journal 的账号、操作 ID、factor ID 与 seed 绑定，B 明确读取 PendingTOTPSecret，未误用它的旧 TOTPSecret。存储采用 macOS Keychain 服务 `com.local.chatgpt-onboarding.credentials.local.concurrency-account2-20261118` 保护，private 文件没有组/其他用户访问权限。本次未将新材料导入容器生产账号。

只改临时驱动和报告，842 个业务文件 SHA-256 一致。没有移除、登记、激活或退出所有设备请求；本次是新的登录实验，不延长或改写此前退出探针的观察窗口。探针进程已关闭，但关闭进程不等于服务端退出这些新登录会话，本次没有再次调用退出接口。

初次本地启动曾因 journal 保护根注册错误在读取材料时失败，没有账号请求；修正临时工具后才正式建立上述新会话。空证据文件保留为 `init-local-error-empty.jsonl`。校验记录在 `.local-run/concurrency-audit/dual-seed-integrity-20261118.json`。

## 对程序的具体建议（尚未修改）

应保存并使用 `账号 + factor_id + secret` 的绑定；登录前检查服务端提供的因子列表，优先使用与保存密钥匹配的 ID。多个因子时直接取第一个，可能把有效密钥配错因子，报出误导性的“验证码错误”。缺少可验证绑定时应明确报告因子选择不确定，不以任意错误或轮流试密钥代替确认。

成功/失败日志应同时区分“本次使用哪个因子”“验证码是否通过”“是否完成登录”“受保护查询是否成功”。mfa_info 的单项展示不能继续被称为“账号只有一个有效验证器”。此前并发实验关于回退、保存、停止的记录仍成立；关于 B K10 有效性未确认的结论由本次成功登录证据补充为已确认可用。
