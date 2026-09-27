# 恢复纯协议路径的可实施性调查

核查日期：2026-09-27。原项目：`/Users/ceng/Downloads/toSub2-main`；迁移项目：`/Users/ceng/Downloads/toSub2-go-port`。

本次针对“为什么授权打开浏览器，是否能恢复纯协议”的源码、依赖、官方文档和本地测试调查。没有修改生产 Go/React 代码、启动真实账号任务、提交真实凭据、重启服务或更改现有浏览器窗口。本文提出的模式名称是设计建议，**不是已经可用的配置开关**。

## 1. 结论

**可以恢复“普通请求优先 Go 协议、程序不擅自打开浏览器”的结构。不能据现有实现承诺“所有旧版功能都只用 HTTP、完全没有浏览器或 JS 运行时”。**

这两个判断并不矛盾：

1. 当前新网页登录从首个请求就强制使用 Chromium，是 `cmd/tosub2/main.go:404`、`:493` 的入口选择，不是 Go 协议状态机的技术限制。这个选择偏离了原要求“普通协议请求优先使用 Go transport”，需要作为行为差异处理。
2. 原版没有打开 Chrome，但其生产验证链是 **Node 状态机 + Python curl_cffi + Node/jsdom**。看不到浏览器窗口，不等于只有 HTTP。
3. Cookie、原始 Set-Cookie、手动重定向、PKCE、Token 交换/刷新及其持久化已有 Go 实现，不必重写。
4. 当前 Go 的生产 Sentinel/SO 实现只有 Chromium provider。关闭 Chrome 后，完整新登录缺少验证运行时；修改 transport 不会补上这一能力。
5. 无 Chromium 的 Go JS 宿主在一般工程意义上不能断言永远不可能，但当前没有生产实现，也没有提供方支持和真实授权验收的证据。不得把这种研究假设写成“全部功能可用”。
6. 用户原先要求 CF/Turnstile/Sentinel 通过真实浏览器或合规授权流程，仍然有效。不能以模拟/伪造验证环境、重放 Token 或变换身份绕过拒绝来满足“纯协议”。

## 2. 三种目标必须分别验收

| 目标 | 当前可实施判断 | 是否等于全部旧功能已迁移 |
| --- | --- | --- |
| 主程序、协议、任务、存储均 Go，允许必要时 Chromium | 当前生产架构满足运行时定义；完整功能仍有未验收项 | 否；运行时语言与功能等价是两项验收 |
| 不由程序启动/控制浏览器，普通流量走 Go HTTP | 可以设计并实现严格边界；需要验证时停止并给出明确状态，或走获支持的人工授权入口 | 仅做到停止/提示，不能算完整登录功能 |
| 无 Chromium、无 JS，仅 HTTP 仍覆盖全部旧登录/验证分支 | 当前证据不足，不能承诺；动态 SDK 与第三方挑战是具体缺口 | 否 |
| 无 Chromium、无 Node/Python，但允许 Go 内嵌 JS 引擎 | 需要独立宿主兼容工程与获支持的授权路径；当前未实现 | 否；能执行一段 JS 不能作为 SDK 验收 |

`TOSUB2_BROWSER_HEADLESS=1` 只隐藏 Chrome UI，仍有 Chrome 进程、CDP 和浏览器网络。它不是纯协议修复。当前 `--native-http` 是标准 Go TLS 的诊断路径，既不等于 tls-client 路径，也不能替代 Sentinel。`--browser=false` 当前会阻止新网页登录，并不会启用另一套完整实现。

## 3. 原版究竟做了什么

以下 O 为原项目相对路径；G 为 Go 项目相对路径。行号以本次源码为准。

### 普通协议请求

- O `src/protocol-login.mjs:154`：通过自定义 transport 或 Node fetch 发送请求；`:218` 使用手动重定向；`:234` 逐个处理 Location、301/302/303/307/308 和 OAuth callback。
- O `src/tls-transport.mjs:184`：通过 NDJSON 把请求送到 Python worker。
- O `src/tls_transport.py:211`、`:255`：curl_cffi session 发请求，禁用自动跳转，保留重复响应头和 Cookie。
- O `src/protocol-login.mjs:1746`、`:1848`：OAuth code exchange、refresh 使用 HTTP 表单请求。

### 看不到窗口的验证运行时

- O `src/protocol-login.mjs:768`：登录的特定操作需要 Sentinel，生产路径要求 TLS worker；测试假值有独立测试条件，不能进入生产。
- O `src/tls_transport.py:315`：创建 SentinelClient，让 SDK 请求共用现有 curl session；Token 和 Observer 完成后关闭这次 Node runtime。
- O `src/cloudflare-ctf/sentinel_dynamic.py:90`：动态加载 loader，再定位版本化 SDK；`:235` 将脚本网络请求桥接回 curl session。
- O `src/cloudflare-ctf/sentinel_runtime.cjs:315`：使用 JSDOM；`:265` 与 `:368` 管理 iframe、SDK 初始化、Token 和 Session Observer。
- O `src/cloudflare-ctf/cloudflare_solver.py:240`、`:335`：识别特定 challenge 页面，启动父/子 Node runtime 执行页面与 iframe；并非真实 Chromium。

**原版可作为功能和数据行为基线，但原版源码中存在验证算法，不等于这些算法在 2026-09-27 仍被真实服务接受。** 本次没有运行原版真实账号登录，也没有把源码中的模拟行为当作有效授权证据。

## 4. 当前功能的纯协议可行性矩阵

“可独立 HTTP”均以对端允许该授权/会话为前提；远端要求挑战时必须明确报告，不能保证所有网络状态都成功。

| 功能 | 现有 Go 核心 | 不启动 Chromium 的结论 | 关键边界 |
| --- | --- | --- | --- |
| 控制台、任务队列、20 并发、邮箱锁、取消、分页、批量操作 | Go + embed 静态资源 | 可保留 | 登录 runner 的选择不应迫使这些模块改写 |
| Cookie Jar、原始 Set-Cookie、手动重定向、超时、代理、TLS profile | `internal/transport` | 可独立运行，已有真实本地 TLS 测试 | profile 是网络身份，不能制造 JS 环境；会话成立后不能任意轮换 |
| PKCE/state、已取得 code 的兑换、有效 refresh token 刷新 | Go OAuth + runner 独立分支 | 已可无浏览器运行 | challenge/授权失效时不能伪装成功；refresh 失败不等于已有重新登录授权 |
| 已接受 grant 的离线恢复、JSON 导出/下载 | Go protocol/store/console | 已可无浏览器运行 | 不重复兑换一次性 code，不丢失已轮换 refresh token |
| 邮箱 API、LubanSMS、SMSBower、自定义池、Sub2API 管理接口 | Go HTTP | 正常 API 路径不依赖 Chromium | 真正供应商验收与本地 fixture 仍须区分 |
| 新账号资料、密码验证、邮箱验证码验证、MFA 验证 | Go 状态机和 HTTP 已有；指定步骤先取 Sentinel | **完整新登录目前不具备无浏览器生产实现** | 所需 Sentinel provider 不能用空值或测试 token 替代 |
| 手机号发送/短信验证、重发/换号 | Go HTTP 状态机 | 单步没有主动 Sentinel 调用 | 必须已有正确登录会话；不能据单步接口推断整个新登录可无浏览器 |
| workspace、session 选择、OAuth authorize | Go HTTP/手动跳转 | 单步可 HTTP | 上游页面仍可能回到密码/OTP/MFA 或 challenge |
| 已登录账号 TOTP enroll/activate | Go HTTP；TOTP 计算为 Go | 已有效登录会话下具备 HTTP 操作核心 | session 失效时重新认证可能需要 Sentinel；当前 runner 仍先建浏览器 |
| 添加密码 | Go HTTP | **当前完整路径需 Sentinel** | `password_reset` Token，以及可能出现的 OTP/MFA 重新认证 |
| CF、Turnstile、动态 Sentinel、Session Observer | 生产只有 BrowserSolver | **严格纯 HTTP 未完成且无可靠全功能替代证据** | 必须获支持的授权流程；本地 fixture 通过不证明上游接受 |
| 旧 checkpoint | 可读取结构；按阶段和身份恢复 | callback_ready/tokens_received 可独立处理，其余需分别验证 | 不可为切换 transport 擦除 BrowserBound；不能将旧 Cookie 跨身份无条件复用 |

Sentinel 的具体 Go 调用点：G `internal/protocol/flow.go:464` 新资料、`:542` 密码、`:590` 邮箱 OTP、`:668` MFA、`:1548` 添加密码。`auth()` 只有 flow 非空时才取 Sentinel（`:300`）。这说明协议核心并未普遍依赖浏览器，但新登录通常会经过至少一个依赖点。

## 5. 为何只改一个开关不成立

### 已有替换点不等于已有替代实现

G `internal/protocol/flow.go:47` 的 `Sentinel` 回调可以注入其他实现；`:141` 明确拒绝缺失 provider；生产装配位于 G `cmd/tosub2/main.go:611`，实际调用 BrowserSolver。仓库没有已实现的 Goja/QuickJS 无浏览器 provider。

G `internal/transport/tlsclient.go:41`、`:69` 已使用真实 tls-client profile 与手动跳转；换成这个 client 不会补齐 DOM、iframe、SDK 和 Observer。tls-client 官方说明的核心是可配置浏览器网络 profile 的 HTTP client；这不构成浏览器脚本运行时。[S3]

### 会话一致性必须按实际证据处理

当前“第一请求即 Chromium”是为避免先 Go TLS、后 Chrome 导致 UA/ClientHello/会话切换。但已有本地 HTTPS 测试只证明**一个会绑定这些属性的测试服务**会拒绝身份变化，不证明真实上游所有步骤一律需要 Chromium。不能用该 fixture 推导所有普通 HTTP 必须永久改为浏览器。

反过来，也不能只复制 Cookie、UA、设备 ID 后就假定两个 transport 等价。代理、Cookie 属性、实际 TLS、浏览器 context 和 SDK 状态均需验证。无法证明可恢复时保存旧 checkpoint，给出明确阶段与原因；不重新提交已消费的验证码/密码/code，不覆盖旧输出。

## 6. 可实施路线比较

| 路线 | 实际得到什么 | 优点 | 限制与判断 |
| --- | --- | --- | --- |
| A. 严格协议模式 | 普通请求全部 Go tls-client；程序绝不启动 Chromium；需要交互或 SDK 时明确停在该阶段 | 能落实“不擅自开窗口”，可独立验收网络与会话行为 | **只是可明确失败的协议模式，不是全部新登录功能完成** |
| B. 受支持的外部人工授权 / Device Code | 程序显示授权地址/一次性码；用户在自己的浏览器完成，Go 处理获支持的回调/轮询、Token 与保存 | 能避免程序控制用户浏览器和创建多个授权标签 | 仍有人类浏览器步骤；toSub2 客户端是否获支持、返回凭据兼容性须核实；不覆盖原账号设置功能 |
| C. 协议优先 + 明确的浏览器分支 | 普通请求优先 Go，确有需要才进入单独有说明的浏览器授权流程 | 最接近原先“普通 Go HTTP，必要时 Chromium”的要求 | 仍不属于严格纯协议；不能无验证迁移同一进行中的会话 |
| D. Go JS 引擎替代 jsdom | 无 Node/Python/Chrome 的动态 JS 宿主 | 从一般工程角度可研究宿主兼容性 | 当前未实现；涉及完整 Web API 语义，且不能用于规避要求真实浏览器的检查；不建议作为承诺全部迁移的依据 |
| E. Chrome headless | 仅隐藏窗口 | 改动小 | **不满足纯协议定义，不应当作本问题修复** |

官方 Codex 文档记录了 device code 登录：需要先启用相关设置，CLI 显示入口和一次性码，用户仍须在浏览器中登录确认。文档针对官方客户端，不能自动推导 toSub2 的 client_id 和既有导入格式均获支持。API key 登录又是不同的访问/计费方式，不能拿它替换用户要求的 OAuth/refresh/id token 兼容。[S1]

Cloudflare 当前官方文档将缺少 JS 的命令行客户端列为不支持，并明确生产 challenge 不支持受控浏览器/浏览器自动化框架。因此，“使用真实 Chromium 引擎”也不能单独作为真实 challenge 已通过的证据。提供方支持的人工浏览器授权仍应与当前 CDP 管理流程区分。[S2]

## 7. Go 内嵌 JS 路线的真实工作量边界

这不是接入一个 `Eval()` 后即可完成的模块。Goja 是 JS 引擎；连 timers/event loop 都需要宿主提供。jsdom 则是为 Node 实现的 HTML/DOM 环境，不是可以直接复制进 Goja 的一个函数库。[S4][S5]

在**提供方支持的使用场景或自有测试页面**中，至少需要独立验收：

1. window/document、动态 script、iframe/contentWindow 与销毁后引用。
2. postMessage 的 origin/source、跨 frame 上下文、Event、MutationObserver 和 Promise 微任务顺序。
3. fetch/XHR/Headers/Blob 的正文、重定向、Set-Cookie、同代理/同 Cookie Jar、取消与错误语义。
4. navigator/screen/performance、requestIdleCallback 等真实语义或明确支持范围；不能用假值伪称真实浏览器环境。
5. crypto.getRandomValues、TextEncoder、atob/btoa、typed arrays、timer 生命周期和内存/任务回收。
6. 动态 loader 与版本化 SDK、Session Observer 同一会话、SDK 更新和失败分类。

原脚本本身含 `Worker.postMessage` 空实现、简化 `ReadableStream` 和 `XHR.abort` 空函数；机械照抄这些 stub 也不满足本次完整实现/可取消的要求。它们位于 O `cf_runtime.cjs:136`、`:151`、`:253`，O `turnstile_runtime.cjs:118`、`:127`、`:216`，O `sentinel_runtime.cjs:174`（均在 `src/cloudflare-ctf/`）。

当前没有取得公开、受支持的“Sentinel 纯 HTTP Token 生成接口”依据；这是**没有发现可依赖的正式替代合同**，不是对所有技术可能性作数学上的否定。遇到需要浏览器的验证必须保留边界，不能固定 Token、移植伪造特征或用测试密钥替代真实验证。

## 8. 若实施协议优先修正，需要改哪些位置

以下是下一轮实现清单，本文没有暗中部署这些变化。

1. **入口与配置：** 从 `main.go` 拆出 transport/authorization policy；区分严格协议、外部人工授权、兼容浏览器三条路径。现有任务 API 和 localStorage 不变，新选项需有明确默认/迁移说明，不能依赖 headless 冒充协议模式。
2. **复用现有真实 Go TLS：** 普通请求使用 FingerprintClient；保留 profile 探测、原始重定向、Cookie、proxy sid/session、超时/取消。普通 403/429 不作为轮换 profile、代理或启动浏览器的理由。
3. **授权能力：** 保持 Sentinel 接口，但没有受支持的 provider 时返回可解释的具体阶段；不注入测试结果。不将一个“等待人工验证”的按钮计为已实现密码/OTP/MFA 登录。
4. **外部授权资格：** 先确认官方允许的客户端接入方式、callback/PKCE/state、返回 Token 与原 Sub2API 格式；不直接调用官方 Node CLI充当生产依赖，不假设任意 client_id 可用。
5. **任务与恢复：** 保留 FIFO20、邮箱锁、取消、重试和全部批量 API；记录授权方式与网络身份。给出等待外部授权的清晰状态，防止监控修复循环不断打开窗口或重复提交已消费步骤。
6. **存量 checkpoint：** BrowserBound 只能用其已记录身份；旧协议 checkpoint 是否可继续要按阶段单独验证。不得为追求“无浏览器”移除校验或清空 Cookie。callback/grant 离线恢复继续独立。
7. **不混合敏感数据：** 新流程仍沿用 Keychain/DPAPI、原子写入、脱敏和公开字段白名单，不读取用户日常 Chrome profile，不全量导入浏览器 Cookie。
8. **功能验收：** 先证明严格协议场景 Chrome 启动次数为零、Node/Python 子进程为零；再验证每条受支持的真实授权链及数据兼容。没有通过的 Sentinel/CF/Turnstile/账号设置项继续列为未完成。

可先交付 A 的严格行为和现有 Go 能力，再核实 B 的客户端资格；C 必须明确仍可能需要浏览器。D 没有足够证据给出完成时间或成功率，不能作为默认承诺。不能把“A 的拒绝/等待行为正确”混写为“全部旧登录能力移植完成”。

## 9. 本次实际测试结果

2026-09-27 在本机 macOS arm64 运行。所有选定测试只使用本地/内存模拟服务与合成账号；关闭浏览器集成开关，设置不存在的 CHROME_PATH，未请求启动浏览器；没有 Keychain 真实凭据读取或真实账号提交。源码 290 个 Go/模块文件测试前后 SHA-256 一致。

| 范围 | 命令范围 | 顶层 PASS | 含子场景 PASS 事件 | SKIP | 退出码 | 墙钟时间 |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| Go transport 全包 | `go test -count=1 -race -json ./internal/transport` | 51 | 92 | 1 | 0 | 12.920s |
| 协议定向 21 项 | 同样 race 参数；PKCE、TOTP、HTTP 登录状态机、手机、JSON、拒绝/重放边界 | 21 | 54 | 0 | 0 | 2.233s |
| runner 定向 16 项 | 同样 race 参数；无浏览器 refresh/callback/保存 grant/恢复/身份边界 | 16 | 81 | 0 | 0 | 3.515s |
| 合计 | 3 次命令 | **88** | **227** | **1** | **全部 0** | — |

唯一跳过为 `TestTransportConfiguredBudgetCanExceedThirtySeconds`：需要 `TOSUB2_LONG_TRANSPORT_TEST=1` 的真实 31 秒长请求测试，未在本次定向验证启用。不是浏览器跳过后冒充成功。

**测试局限：** 密码、OTP、MFA 等协议测试注入测试 Sentinel header。因此它们证明状态机可以用 Go HTTP 和可注入 provider 运行，**不证明生产已有无浏览器 Sentinel，也不证明真实上游接受**。原版 smoke 也使用测试 Token，不提供额外 live 接受证据。Cloudflare 官方同样将自动测试密钥与生产挑战区分；自有 fixture 和真实挑战必须分别报告。[S6]

完整命令、日志摘要和校验值见同目录 `go-pure-protocol-feasibility.json`。本次是定向竞态回归，没有把此前全仓测试数字当作本次重新运行。

## 10. 资料来源与可追溯性

官方/维护者资料均于 2026-09-27 读取，不以第三方营销或“绕过验证”教程作为结论依据。公共文档原始摘录和来源校验值保存在项目所属私有 runtime/archive/protocol-feasibility，随现有本地清理脚本可移除。

- [S1] OpenAI 官方认证文档：<https://developers.openai.com/codex/auth>，当前文档页面 <https://learn.chatgpt.com/docs/auth>。本次前一个入口在普通 HTTP 客户端返回 308，改读后者得到 200；没有把重定向响应当文档内容。
- [S2] Cloudflare supported browsers：<https://developers.cloudflare.com/cloudflare-challenges/reference/supported-browsers/>，页面标记更新 2026-08-18。
- [S3] tls-client 维护者 README：<https://github.com/bogdanfinn/tls-client>。本地锁定依赖为 v1.9.0；文档主分支的新功能不能视为本地已升级。
- [S4] Goja 维护者 README：<https://github.com/dop251/goja>。
- [S5] jsdom 维护者 README：<https://github.com/jsdom/jsdom>；原项目锁定 jsdom 26.1.0。
- [S6] Cloudflare Turnstile testing：<https://developers.cloudflare.com/turnstile/troubleshooting/testing/>，页面标记更新 2026-05-05。

## 最终判断

**恢复普通 Go 协议为主、移除未经说明的强制 Chrome 依赖具有明确实施路径；“完全不使用浏览器/JS，同时全部新登录和挑战能力等价”目前没有充分实现或验收依据。** 本次没有为隐藏缺口切换 headless、删去验证、伪造 provider 或宣称全部功能完成。应把可恢复的协议主路径与尚缺受支持替代方案的验证能力分开交付、分开验收。
