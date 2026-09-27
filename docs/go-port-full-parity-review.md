# 旧版功能与行为完整性复核

本轮以 /Users/ceng/Downloads/toSub2-main 的 1.7.1 实际源码为依据，继续修复 Go 版的行为差异。原版快照的 42 个文件哈希重新核对未变。模块存在、路由存在、前端按钮存在、本地合成测试通过，均不能单独作为“全部功能完成”的依据。

用户要求是完整移植旧项目。既有批量执行、登录、授权和输入交互必须以旧代码为准；真实 Chromium 的引入用于满足原迁移要求中的浏览器环境和会话绑定，不构成新增产品功能，也不能作为扩大交互差异的理由。

## 本轮已定位并修复的行为差异

| 旧版行为与依据 | Go 版缺口与修复 | 验证方式 |
| --- | --- | --- |
| 原版前端通过任务轮询重复取得待设置 TOTP 密钥 | 私有密钥接口原先只请求一次并吞掉错误，503 或成功空值后永久不显示；现在同阶段串行重试，有请求超时，退出阶段取消，成功即停止 | 实际嵌入 React、Go API、JobManager 和真实 Chrome，依次返回 503、空值、密钥；修前失败，修后 race 通过 |
| console-server.mjs:2936–2949 优先导出 mailRequestBody，不要求已有 POST method 标记 | 导出旧资料时 body 被 URL 替代；恢复原优先级 | 有/无密码 × method 缺失/GET，修前失败、修后通过 |
| protocol-login.mjs:1614–1646、2186–2193 对 channel/session 相关 400/409 做一次无 channel 手机发送重试 | Go 只看小范围 code，忽略 message-only 错误；恢复安全分类和旧分支 | 本地真实 HTTP 检查请求体、重试次数与错误脱敏 |
| protocol-login.mjs:2180–2184 识别会话失效，允许有界恢复 | 无 code 的失效描述被 Go 丢弃，无法走恢复；保留分类布尔值，不把响应原文写入日志 | HTTP 契约用例，包括禁止把权限拒绝当成会话失效 |
| protocol-login.mjs:267–276 的 getJson 始终 JSON.parse | Go 忽略返回对象时接受空体、HTML、串接 JSON；现在仍严格验证单个 JSON 值 | /api/auth/providers 等协议响应回归 |
| 原版仅请求 sub2api 输出但同时禁止 OAuth 导出时返回错误（:646–647） | Go 把参数改写为 session 并错误报告成功；现在登录模式在开始网络请求前拒绝矛盾参数 | 配置与 CLI race；合法 session/both 及旧子操作分支保持可用 |
| console-server.mjs:2460–2494 的 Sub2API 单次 HTTP 超时为 120 秒，HTTP server 没有 60 秒总体写截止时间 | Go 客户端 30 秒、服务写截止 60 秒导致旧版可成功的慢调用失败；恢复单请求预算、移除提前写截止 | 生产 runConsole + 本地 Sub2API 实际等待 65 秒；普通测试显式跳过这项长测 |
| 服务关闭应取消正在进行的外部请求 | Go 前台请求没有继承服务 root context；现在 HTTP BaseContext 绑定服务生命周期 | 真实前台请求保持挂起，关闭服务，验证立即取消远端请求并正常退出；修前失败、修后 race 通过 |
| console-server.mjs:549–550、570–600、965–968、2240–2241 的响应契约 | 补回 logs.id、cancel.job；单操作成功恢复 HTTP 200，重复取消保持幂等，retry 区分 404/409 | 六类操作、终态取消、冲突与缺失任务 HTTP 回归 |
| console-server.mjs:625、634–645 的邮箱筛选计数 | requested 统计规范化后唯一邮箱数；不带筛选时明确为 0 | 重复、大小写、空白与无筛选组合 |
| console-server.mjs:1214–1217 添加密码后更新登录方式 | 安全保存成功后立即更新并持久化 LoginMode=password，不等服务重启 | 内存状态、落盘状态与失败不更新回归 |
| console-server.mjs:1805、2820–2830、3012–3015 的公开恢复字段 | 补回 restartRequired、autoRepairEligible、autoRepairEligibilityReason；普通轮询不反复解锁 Keychain | 状态/凭据/拦截九种资格分支及敏感字段排除 |
| console-server.mjs:1940、2068、2121、3002、4367 的短信服务标识 | smsServiceLabel 贯通取号、API、元数据、安全订单、恢复及完成；兼容 luban_service_id | 三类供应商与旧字段、恢复完成链、敏感内容脱敏 |
| mail-otp.mjs:194–201 折行 Base64 先去空白 | Go 原先按未折叠长度判断，导致 MIME 内容丢码；恢复 LF/CRLF/tab/space 解码 | 同一邮件解码内容、候选指纹和无效数据对照 |
| mail-otp.mjs:175–183 的时间解析 | 补齐 +0800、分数秒、本地无时区时间、分钟精度、RFC/ISO 日期；避免旧码被错误当新码 | 与旧 JS 合成输入的 epoch 比较、旧码时间过滤 |
| custom-sms.mjs:62、76 复用邮箱抓取器 | 自定义短信 API 恢复跟随重定向和 60 秒预算；仍保存旧短信基线和去重状态 | 真实本地 302、旧/新码、恢复订单、重定向循环与取消 |
| luban-sms.mjs:50–57 的号码和订单字符串 | JSON 数值恢复普通十进制，避免被 Go 格式化为科学计数法 | 取号、取码、释放中的原订单标识与 JS 安全整数边界 |
| console-server.mjs:449–469 上传保留扩展字段 | 仅删除 proxy_key，保留 notes/expires_at/扩展字段和既有 proxy_id；未指定分组传 [] | 无覆盖/显式覆盖、输入对象不被改写、真实本地上传体断言 |
| credential-store.mjs:7–8、46、142、220–232 的旧 macOS 凭据 | 隔离 runtime 不隐式读全局旧 vault；新增显式离线迁移，读取选定账号，重新加密进本实例 namespace，不覆盖已有目标 | 合成旧文件、旧全局 key reader、取消/部分失败/权限/源不变/重复执行；真实 Keychain 验证单独记录 |

上述改变均落在实际生产调用链。修改前失败、修改后通过的原始日志保留在私有运行目录，不覆盖成首次通过。协议错误分类另补齐 nested/top-level/OAuth/string/plain-text 六类旧响应形式；空密码在申请 Sentinel 或发请求前停止。

## 离线旧凭据迁移

新增 Go 参数 --import-legacy-credentials 与 --credential-emails-file；支持旧单任务 JSON、聚合 jobs、JSON 列表和每行邮箱。先完整校验并去重，最多 1 MiB/10000 项，之后才构造目标凭据服务。只输出 requested/imported/skippedExisting/missing/failed/unprocessed 六个计数；缺失、失败或未处理任一非零均返回非零退出码。

本机脚本提供 scripts/local-macos.sh import-credentials；必须先停止当前实例，避免与另一个进程的凭据写入竞争。不会为导入自动取消运行任务，也不会删除旧全局 Keychain。旧源与新目标必须分开，源目录和文件限定所有者、权限、类型，不跟随符号链接；导入后在目标重新读取验证，已存在目标保持不变。此命令只迁移凭据，旧任务与输出继续由既有兼容读取器负责。

本轮只使用唯一合成账号验证，不读取或迁入用户真实 vault。未找到原项目默认输出目录和默认旧 macOS vault，不把“文件未找到”推断为用户从未保存过凭据，也不据此断定任务密码提示的唯一原因。

## 手机错误后的手动恢复

旧版除明确失效/非法步骤之外允许在手机发送与短信验证失败后人工继续。Go 原来把 429/5xx 直接终止；本轮恢复 phone_send_deferred、sms_otp_deferred、sms_resend_deferred 结构化分支，保留当前号码、PKCE/OAuth checkpoint 及安全存储中的订单。phoneRetryRequired 作为非敏感暂停状态贯通任务、公开 API、普通元数据与 checkpoint。

暂停期间原子阻止自动号码/验证码投递，并清掉可能复制残留的 phone、OTP、code、value 候选；用户重试相同号码保留原订单，输入新号码/显式换号才释放原订单。取消释放一次，服务关闭与重启保留可恢复状态。没有新增遇到限流就自动换代理或付费取号的行为。

真实本地 HTTP 回归覆盖发送、验证、重发的 429/500/503，人工重试/取消/换号及两种暂停 checkpoint 恢复零 POST；明确 invalid_auth_step/session_expired/invalid_state/login_required 仍终止，不误归为临时恢复。控制台另有十六种订单与并发场景，覆盖订阅事件延迟、旧号码候选残留、恢复订单及避免重复购买。

再次对照旧 send/validate/resend 的 catch 发现，旧版还允许其他 HTTP 响应和普通网络错误后人工继续。本轮继续扩展到未分类的 400/409/422、非明确失效的 401/403、404/405/408/410，以及连接中断、响应截断、无效 JSON 和单次请求超时。未知结果暂停自动投递，保留原号码、订单和 checkpoint；明确号码/验证码拒绝继续使用原输入分支。任务 context 的取消/超时、已知代理/TLS/浏览器身份不可用及明确失效仍终止；单次 HTTP deadline 不被误当整个任务结束。

这里仍有明确行为差异：对已确认失效的发送/重发步骤，Go 会结束当前会话，不保留无法继续的输入循环；不会以重复发送已消费操作来冒充恢复完成。新增错误矩阵先在修复前记录失败，再修复生产分支；最后以修复后全仓结果为准。

旧凭据导入新增 TestMacOSKeychainLegacyImportIntegration：实际 macOS Keychain、唯一合成旧全局条目与临时 .enc 文件、独立本机 namespace，验证导入全字段、源不变、重复跳过、不覆盖已更新目标、独立加密密钥及精确清理。普通和 race 均已执行通过；未枚举或读取真实账号条目。

## 认证会话身份连续性

复核发现旧 Go 入口先使用 tls-client 建立 home/providers/CSRF/signin 会话，首次 Sentinel 才切换真实 Chromium，不能证明先前 Cookie 的 UA/TLS 绑定有效。现已修改生产 runner：新的网页认证在首个请求前创建绑定设备和代理的 Chromium 会话，认证状态机、重定向与输出仍由 Go 控制。独立 refresh、cookie 无关的授权 code 交换、邮箱、短信及 Sub2API 保留 Go HTTP 路径。浏览器不会在同一认证中途换回 tls-client。

新增 TestFreshAuthenticationTLSIdentityThroughOAuth 使用本地真实 HTTPS 和系统 Chrome，绑定首个 ClientHello 的 cipher/group/version/signature/ALPN（只去除 GREASE 预留值）、真实 UA、设备和 Cookie。它执行首次登录、302 原始 Cookie、动态版本化 SDK、Session Observer、密码验证、303 登录回调和完整 PKCE/OAuth 输出。把相同 UA/设备/Cookie 放到另一种 Go TLS 客户端的负对照被拒绝。此项 race 已通过；只给临时测试 Chrome 精确信任 fixture 证书 SPKI，不修改生产证书校验或系统信任库。

这证明本地可控服务中的实际网络身份连续性，不证明真实供应方接受或所有 challenge 分支可用。浏览器会话池等待现在发生在网页认证开始前，仍最多四个；任务最多二十个及额外 FIFO 队列保持原功能。

## 旧 checkpoint 的具体兼容边界

- 已记录 BrowserBound 的旧 Go checkpoint 按实际原生 UA、profile、代理、设备校验后继续，不因缺少新标志直接作废。
- 旧 email_verified 会话通过实际 Chrome 的只读 /api/auth/session 验证同账号、有效 token 和有效期；空数据、错误账号、过期、重定向或 HTML 都不能冒充验证通过。
- callback_ready 的 code 由 PKCE/state 验证后走不携带旧 Cookie 的 Go token 交换；tokens_received 保留原离线完成路径。
- 未绑定原生浏览器、仍等待密码/邮件/MFA/手机号/短信的旧 checkpoint 没有可用的只读账号/阶段证明。当前保留原文件并明确返回 CHECKPOINT_SESSION_UNVERIFIED，要求显式重新登录；不盲目重放已消费步骤。这部分尚未达到旧版跨身份无缝续行，必须列为未完成兼容项。

另外修复 runner 决定是否恢复以后 Flow 再次自行读取同一输出路径的旁路：新的 CheckpointRestoreManaged 仅由 runner 设置，避免 fresh CLI 的 --checkpoint 输出文件意外恢复旧会话；独立 protocol 调用默认仍支持原自动读取语义。

## 完成判定

当前不宣称“全部功能完全移植完成”。上述旧待验证 checkpoint 的无缝迁移仍有具体缺口，Windows 原生 DPAPI/服务、真实授权账号和真实第三方服务尚缺现场验收。已实现但缺少现场证据，与无法无缝兼容的行为分开记录。

## 运行与证据边界

普通服务仍为本机 Go 进程；Node 只在 React 构建阶段使用。测试使用合成账号/密钥、本地 HTTP/代理和系统 Chrome；未重试用户真实账号、未发起收费取号、未代替用户点击验证。

私有原始日志统一保存在本实例 runtime 的 archive/full-parity-review 目录，随 scripts/local-macos.sh clean --yes 清理。原始账号资料、系统凭据和源项目不会被测试脚本输出到报告。

## 2026-09-27 最终本地验证与部署

| 验证 | 退出码 | 耗时 | PASS 事件 / 顶层测试 | SKIP |
| --- | --- | --- | --- | --- |
| 全仓普通 | 0 | 36.636s | 1557 / 530 | 68 |
| 全仓 race | 0 | 37.956s | 1557 / 530 | 68 |
| go vet | 0 | 0.315s | — | — |
| 真实本地 Chrome 三包 race | 0 | 63.622s | 348 / 152 | 10 |
| macOS Keychain 包 race | 0 | 4.357s | 76 / 38 | 1 |
| 65 秒 HTTP 与服务关闭 race | 0 | 66.792s | 2 / 2 | 0 |

各阶段均无缓存执行。PASS 事件包含子测试；SKIP 未计作通过，精确命令、跳过名称、日志 SHA-256 与源码哈希见 docs/go-port-full-parity-review.json。普通测试不启用 Chrome/Keychain；单独阶段明确启用真实 Chrome（含可视 hidden-target 回归）、系统 Keychain 和慢请求开关。测试不提交真实账号、不调用收费短信服务。

macOS arm64/amd64、Linux amd64、Windows amd64 四个生产构建通过；Windows credentials/privatefile/browser/main 测试二进制交叉编译通过，未登记为 Windows 原生执行。

本机已重建并部署到 http://127.0.0.1:4400；仅监听 loopback，Chrome 可视且 sandbox 开启。1 个既有终态任务的身份与状态保持，未重试真实账号。运行二进制与受测源码重建 SHA-256 一致：47834a143221f12ce9419b57caba9ae662520021975f2eaf779003e512740e42。三个实际 HTTP 静态资源与受测 embed 资源逐字节一致；CLI help 说明同步。

现有控制台页面检测到已填写内容，未强制刷新；没有读取或记录具体输入值。新前端在刷新后加载，刷新前请保留需要的输入。所有测试日志、构建产物和验证脚本都在本实例 archive/full-parity-review，随既有 clean 命令清理；没有新增项目 tmp/bin/runtime 目录。

这轮本地验收不消除上文列出的旧未绑定浏览器 checkpoint 兼容缺口，也不替代真实账号/第三方服务或 Windows 原生验收；全部功能完成结论仍为否。
