# Go 迁移状态

## 2026-09-27 完整行为复核与本地部署

本轮修复已进入生产调用链并在本机部署：批量凭据/密码输入、隐藏浏览器请求目标、TOTP 私有密钥重试、API 响应契约、邮件解析、短信错误后保留订单人工恢复、Sub2API 字段与慢请求、离线旧 Keychain 凭据导入，以及首次请求即绑定 Chromium 的认证会话。逐源行对照见 docs/go-port-full-parity-review.md。

**全部功能验收尚未完成。** 旧的未绑定原生浏览器且仍待密码/邮件/MFA/手机/短信的 checkpoint 保留文件并报 CHECKPOINT_SESSION_UNVERIFIED，必须显式重新登录，未实现无缝续行。真实授权账号、真实服务和 Windows DPAPI/服务仍待原生现场验收。

| 验证 | 退出码 | 耗时 | PASS 事件 / 顶层测试 | SKIP |
| --- | --- | --- | --- | --- |
| 全仓普通 | 0 | 36.636s | 1557 / 530 | 68 |
| 全仓 race | 0 | 37.956s | 1557 / 530 | 68 |
| go vet | 0 | 0.315s | — | — |
| 真实本地 Chrome 三包 race | 0 | 63.622s | 348 / 152 | 10 |
| macOS Keychain 包 race | 0 | 4.357s | 76 / 38 | 1 |
| 65 秒 HTTP 与服务关闭 race | 0 | 66.792s | 2 / 2 | 0 |

最终源码与测试期间哈希一致，部署二进制与受测源码重建一致；四平台构建和 Windows 四包测试交叉编译通过。机器记录为 docs/go-port-full-parity-review.json。首轮 umask 测试夹具失败保留于 first-pass；补齐剩余手机错误前的全量验证保留于 before-phone-error-completion。生产凭据权限校验未被放宽。

以下 971/948/780 等数字及对应报告属于历史轮次，不与本轮合并，也不代表本轮源码。

历次目标复核发现的传输取消、凭据取消、输入和短信恢复问题均已修复。首次完整验收暴露的Linux EOF/取消竞态和浏览器池旧状态断言也已修正；原失败保留于机器记录和verification-history。

**上一轮浏览器/批量密码修复的历史验证。** 普通测试 32.962 秒、race 33.362 秒、vet 0.159 秒；普通与 race 各 16 包、971 个 PASS 事件（450 个顶层测试）、58 个明确 SKIP。另显式开启真实本地 Chrome 的三包 race 45.225 秒、230 个 PASS 事件（129 个顶层测试）、8 个明确 SKIP。该轮受测源码哈希未变化，该轮运行二进制与对应源码重建产物 SHA-256 一致；见 `docs/go-port-browser-batch-regression.json`。

此前原生部署的 948 个 PASS/55 个 SKIP 与生命周期验收仍保留于 `docs/go-port-local-runtime-verification.json`，属于修复前的历史构建，不与最新统计相加。

历史 R2 全仓记录单独保留：普通与 race 各 15 包、780 个 PASS 事件、395 个通过的顶层测试、49 个 SKIP；启用 Chrome/Keychain 的全仓 race 为 15 包、830 个 PASS 事件、433 个通过的顶层测试、11 个 SKIP。原命令、代码指纹和构建记录仍以 `docs/go-port-verification.json` 及 `docs/go-port-go-source-manifest.json` 为准，不与本轮统计相加。

Go主程序可以独立启动，生产服务、协议客户端、任务、TLS/代理、凭据和部署入口均走Go。保留的React源码只在构建阶段使用Node生成静态资源，Go通过 `embed.FS` 托管；生产调用链不启动Node、Python、Vite middleware、jsdom或Playwright Node driver。Chromium由Go直接通过CDP控制。

**尚未达成“全部功能的真实环境验收”。** 本地HTTP/协议fixture、真实Chrome本地页面、生产CLI/服务进程和macOS Keychain已有验证证据，但真实授权账号、短信/邮箱API、Sub2API和Windows目标系统环境未提供。公开真实上游测试在可视、干净Chrome中已生成真实动态Sentinel SDK token及Session Observer token；两次先前无头测试失败仍保留。可视SDK执行成功不等于认证端点接受、真实账号流程或所有Cloudflare/Turnstile分支通过。

## 当前本机部署与清理

本轮修复和验证见 `docs/go-port-full-parity-review.md`；此前浏览器/批量密码修复记录仍保留于 `docs/go-port-browser-batch-regression.md`：BrowserTransport 请求使用经验证没有窗口的 hidden target，人工验证主页面仍保留；结构化批量重导入明确清空省略的旧凭据字段；已被拒绝的自动密码不再从 Manager 输入队列重复提交。提示区分缺少密码与服务拒绝密码，已导入的有效密码仍自动使用。原版也有这些正常的密码输入分支，本轮不虚构缺密码强制转 OTP 的兼容能力。

真实 runner/本地 Chrome 回归复现并修复 import/vault 两种来源的重复密码提交，覆盖 checkpoint 回填、新密码提交、完整合成 OAuth、安全存储与清理。首次全量仅旧目录权限测试因继承严格 umask 导致准备阶段权限不符而失败；明确设置合成目录的初始权限后全量重跑通过，生产权限逻辑未改，首轮记录保留。当前服务重启后任务身份和取消状态保持，未重新执行真实账号。

当前本机已切换为 macOS 原生 Go 服务，监听 `http://127.0.0.1:4400`。运行目录为 `/Users/ceng/Library/Application Support/toSub2-local/6428e6fb3abbc4ae`；toSub2 的容器、专用数据卷、网络和镜像已移除，历史验证报告仍保留。后文描述“当时继续运行的容器”或旧可视 Agent 的段落是历史状态，不是当前部署。

统一使用 `scripts/local-macos.sh start|status|stop|restart` 管理当前实例；`start` 已运行时直接返回。已有二进制无需 Go 工具链，只有缺少二进制或 `TOSUB2_LOCAL_REBUILD=1` 才构建。原生 Chrome 为可视模式且启用 sandbox；运行不依赖 Node、Python 或 Docker。脚本的 plist 保留在运行目录 `launchd/` 内，只注册当前 GUI 会话，没有登录自启动 plist。

`./scripts/local-macos.sh clean --yes` 会先停止该实例、确认隔离 Keychain 清理成功，再永久删除本运行目录的 `data`、`archive`、`bin`、`logs`、`tmp`、内部 plist 和目录本身。停止或 Keychain 清理失败时保留目录。专属父目录 `toSub2-local` 仅在空时删除；其他 runtime 和通用 `Application Support` 父级保留。

清理不删除源项目 `/Users/ceng/Downloads/toSub2-go-port`、原项目 `/Users/ceng/Downloads/toSub2-main`、共享缓存、系统 Chrome 或旧全局 Keychain。如需连 Go 源目录一起移除，先在原路径成功 clean，再通过 Finder 删除 Go 源目录；不要先删脚本或改变项目路径。完整命令、当前命名空间与清理边界见 `docs/local-macos.md`。

最终脚本已用真实 Go 源码临时副本验证启动、重启、Go 编译器不在 PATH 时启动、两条合成隔离 Keychain 条目删除及重复 clean。263 个历史临时文件与 18 个目录已统一迁入 `archive/migration-temporary-artifacts`，包含在本 runtime 的永久清理范围内。Downloads 路径下的失败只确认 launchd 退出 EX_CONFIG 78 且应用日志为空，未据此断定 TCC 或其他系统机制；当前 Application Support 部署已上线。

本机运行和生命周期验证不补齐 Windows 原生平台、真实授权账号、短信/邮箱 API、Sub2API 或所有上游挑战验收；上述未完成边界继续有效。

## 已实现模块与本地覆盖

下表描述实际实现及已有回归入口。完整50项功能与29项测试对应 `docs/go-port-acceptance.md`；原JS/Python逐文件断言映射对应 `docs/go-port-js-regressions.md`。这里的“已实现”不等于所有真实第三方服务均通过。

| 模块 | 当前实现 | 本地证据与边界 |
|---|---|---|
| `cmd/tosub2`、`config` | 控制台/CLI、启动token、参数和旧环境兼容；登录/恢复/refresh、TOTP/密码子操作；both/session/sub2api、web-only、no-sub2api-export、私有输出路径；原生Windows Service入口 | CLI子操作退出码反映操作结果，不被旧OAuth文件误判；checkpoint输出路径不自动代表恢复意图。进程启动/端口失败/信号关闭有回归；发布构建和最小PATH独立二进制烟雾已通过，精确产物/源码指纹见机器记录 |
| `console`、静态React | 保留现有API及localStorage行为；单/批量创建、分页/筛选/跨页选择、日志、批量删除/下载；手动邮箱/MFA/手机号/短信、重新授权/登录、TOTP设置/密码添加等操作 | `TestChromiumEmbeddedConsoleInteractions`覆盖列表/跨页操作；`TestChromiumConsoleAccountActionsAndDownloads`真实运行React/API/Manager，并将单/批量JSON下载保存到临时目录后解析。该账号操作测试的远端runner是fixture |
| `jobs` | 最多20并发、FIFO、同邮箱互斥、批次完整预检、取消/停止全部/重试/恢复；原子凭据更新、删除前清理、结构化JobEvent；TOTP/密码子操作取消保留已有授权 | 并发/FIFO、同邮箱竞争、取消优先、凭据锁外读取及取消、失败恢复与操作标签均有回归。事件Data采用明确公开字段白名单，mailStatus/phone状态与安全错误通过事件更新API |
| `protocol` | Cookie Jar、手动301/302/303/307/308、原始Set-Cookie；密码、邮箱OTP、TOTP/MFA、profile/about_you、workspace、已绑定手机号跳过、手机号发送/验证/重发/换号；Codex OAuth PKCE/state和token交换 | strict legacy fixture检查请求body/headers、每逻辑认证请求UUID、Sentinel flow、Cookie、确切调用次数及预期callback。真实账号未验；普通协议请求优先走Go transport，必须浏览器绑定的请求走BrowserTransport |
| `protocol`恢复与账号设置 | checkpoint阶段/邮箱/源站/session绑定校验；已消费密码/OTP/MFA恢复；workspace和手机号发送不重复；TOTP/添加密码私有结果、激活前保存与安全候选恢复 | 仅明确会话失效允许重登一次；网络错误、取消、普通拒绝/challenge不借机重登；不自动重放已提交设置或token交换。web-only不能把恢复的Cookie直接当新登录成功 |
| `protocol`导出/refresh | code换access/refresh/id_token；refresh重新授权；兼容sub2api-data v1；保留未知字段和每账号client_id、轮换refresh token | 缺失/opaque id_token不丢弃成功refresh结果；无效源在远端token变更前拒绝；refresh不依赖无关checkpoint或ChatGPT连通性。真实token生命周期/导入接受未验 |
| `mail`、CLI邮箱输入 | API请求/轮询、手动覆盖、拒码去重、消息身份/时间cutoff、发送/重发前旧邮箱baseline、单请求超时；低置信候选需要独立轮询确认，超时暂停并保留手动/重发 | `TestMailBaselinePrecedesSendingAndResendButNotPendingResume`、`TestMailTimeoutPausesUntilResendAndPreservesManualOverride`及状态API脱敏测试覆盖时序和恢复。生产CLI+真实Chromium测试包含自动邮箱HTTP与手动输入；远端邮箱是fixture |
| `providers`及console SMS | LubanSMS、SMSBower、自定义池（最多500条）、参数/API key预检、价格/库存、候选验证码/旧短信过滤；取号、轮询、重发、换号、释放、完成、安全lease恢复 | 实际console HTTP→Manager→本地SMSBower链覆盖号码接受后ready、phone_otp后开始计时、timeout/重复码/HTTP429手动接管、只释放/完成一次。pending/seen/deadline/stopped状态安全保存，供应商真实费用/订单状态未验 |
| `sub2api`、console monitor | 号池/代理/options、整组选中预检上传、去重/兼容计数、远端身份校验、凭据与extra合并、更新/清错；异常账号监控、冷却、永久跳过、自动重登/授权及中断转传 | 本地真实Manager/Server重启测试覆盖部分ID成功后只重传剩余ID、不重登、只使用邮箱最新任务、queued视为忙碌。监控间隔和冷却默认均5分钟；interval最低1000ms，显式0禁冷却并忽略旧持久计时。合法1秒timer和真实默认五分钟间隔均已执行；后者等待约300秒并验证关闭取消在途HTTP和metadata保存，真实后端长期运行未验 |
| `transport` | tls-client/fhttp、实际ClientHello profile、UA/Client Hints一致、profile探测与降级、Cookie/手动redirect、超时、HTTP/HTTPS/SOCKS5/SOCKS5h、sid/session/代理池与连接重试预算 | profile探测有分候选/总预算和Cookie隔离，不让慢首选饿死fallback；只有新会话连接失败可轮换身份，HTTP403/429等安全拒绝不触发换身份。`--native-http`已实现为显式标准Go HTTP兼容模式，不宣称Chrome TLS指纹，也不作为指纹失败的静默fallback |
| `browser` | Go原生CDP管理已安装Chromium；每会话独立私有profile、代理绑定/认证、实例限额、context取消与进程/目录清理；Cookie双向/分区同步、设备绑定、OOPIF、原生浏览器UA/平台 | 真实Chrome本地覆盖代理认证/DNS、代理替换清理、父context取消、HTTPS代理不可信证书拒绝、源站401不获得代理密码；BrowserTransport的认证headers、redirect/body、缓存和SDK service worker隔离均有回归 |
| `challenge` | 同一真实浏览器会话处理页面/iframe/DOM/postMessage/fetch/MutationObserver；动态loader/版本SDK、Sentinel与Session Observer、SDK恢复/缓存更新/取消；Cloudflare/Turnstile结果观察 | 本地Chrome fixture已验证loader/iframe/token/Cookie及生命周期；可视干净Chrome另已生成公开真实SDK与SO token。先前无头运行在验证页失败；认证端点接受及真实账号仍未验。没有用伪DOM引擎、关闭CSP或自动点击绕过验证来补成功 |
| `credentials` | macOS Keychain + AES-256-GCM、Windows DPAPI CurrentUser；旧envelope/直接JSON/完整截断字段迁移；锁定/错误密钥/取消显式失败，Linux安全失败策略 | macOS真实读写删除及跨进程读取/更新/复读已有通过证据；Windows DPAPI仅目标代码/测试二进制交叉编译。Linux缺安全持久服务时只存内存，不写普通密码/TOTP/敏感代理资料 |
| `store`、`privatefile`、`logging` | 六类兼容JSON、原子替换、权限/句柄校验、未知字段、metadata和已完成输出恢复；结构化日志、错误/URL/header/OTP/token脱敏 | POSIX私有目录/0600文件、写入前权限及原子替换有测试；Windows保护DACL在创建前设置并回读验证，不继承宽松权限，目标系统未执行。既有共享导出父目录不被改权 |

## 未完成的真实环境验收

1. **真实账号流程。** 未提供本人或明确授权账号，尚未实际验收邮箱/密码/MFA、TOTP设置、添加密码、手机号、workspace、OAuth code/refresh及上游会话失效/恢复组合。本地合成账号链不能替代这些步骤。
2. **真实安全验证的剩余边界。** `TestLivePublicSentinelBrowserCompatibility`在2026-09-26以 `TOSUB2_BROWSER_HEADLESS=0`独立通过：干净可视Chrome执行公开动态SDK，生成Sentinel token，Session Observer状态为 `available`。两次更早的无头失败保留：第一次验证页CSP阻止SDK，返回 `SENTINEL_SDK_FAILED: loader_failed`；第二次先等待同浏览器正常页面，仍遭 `HTTP 403` / `Cf-Mitigated: challenge` 并返回 `SENTINEL_PAGE_CHALLENGE`。没有提交账号资料、自动点击验证或修改CSP。已证明可视浏览器公开SDK执行，尚未证明无头上游成功、认证端点接受、真实账号流程及完整Cloudflare/Turnstile分支。三次尝试分别保存在机器记录 `live_upstream` 中。
3. **真实第三方后端。** LubanSMS、SMSBower、自定义短信池、邮箱API及Sub2API授权环境未提供，未实际验证库存/费用、远端lease时效/完成、导入/更新/清错、监控重登与中断重传。现有完整HTTP链使用本地服务器和合成secret。
4. **真实代理。** HTTP/HTTPS/SOCKS认证、DNS和本地TLS隧道有回归；供应商证书、sid/session语义、长连接故障、指纹兼容及同一真实浏览器会话的上游接受仍需现场验收。不可信HTTPS代理拒绝测试不等于可信代理全链已验。
5. **Windows与部署机器。** Windows主程序、DPAPI/DACL/原生Service测试已具备代码和交叉编译检查，未在Windows运行。macOS Keychain本机成功不覆盖另一运行用户、锁屏、服务账号及系统重启；macOS当前GUI用户的临时launchd安装两次启停已通过；systemd/SCM及另一部署用户仍需目标机验证。
6. **长时及规模。** 20并发/FIFO测试验证本地调度，未对20个真实上游账号压测。真实本地生产runConsole默认五分钟timer已等待完整周期并验证关闭；真实后端长期运行未验。20任务与4个真实Chrome的组合也已验证，不等于20个上游账号压测。

这些边界是实际缺少的运行证据，不能通过改写状态或把SKIP计入PASS来消除。发现可修复的实现缺口应先修复并复验，再调整状态。

## 与旧实现的行为差异和兼容策略

- **运行架构。** Node/Python子进程与stdout解析改为Go调用、context和JobEvent；保留旧JS/Python源码作为参考。旧worker的stdout/message bridge逐字行为不作为Go协议，等效的Chromium进程取消、CDP、iframe和清理由对应测试覆盖。
- **前端和接口。** React源码、API路径/主要字段和localStorage保持兼容；生产阶段只有静态资源。普通列表不返回TOTP secret/URI；显式私有TOTP接口与下载受启动token保护，私有文件受权限保护。下载实际设置Content-Disposition，坏输出不静默混入批量导出/上传。
- **传输身份。** 默认保留真实tls-client profile功能；旧profile别名解析为依赖实际支持的profile，UA/Client Hints随之匹配。显式 `--native-http` 使用Go原生HTTP，identity明确不同。Chromium无法套用tls-client的ClientHello；浏览器绑定的敏感请求继续走原浏览器会话，不以Cookie复制宣称TLS/session等价。
- **安全验证和重试。** 采用真实浏览器或合规授权流程，不自动点击绕过验证，不关闭CSP。已有checkpoint保持其代理/设备/浏览器绑定；新的连接失败可在预算内换profile/代理，安全拒绝保持当前身份等待正常验证，不把换身份作为绕过方式。
- **凭据持久化。** Linux当前没有新增安全持久服务，敏感凭据/订单资料只在内存，重启可能需重新输入。CLI没有安全交付方式时拒绝生成后无法交付的新TOTP/随机密码，不退回明文。macOS/Windows凭据错误也不静默退回明文。
- **元数据布局。** Go写入输出根目录的聚合 `job-meta.json`（version/jobs/updated_at），恢复时仍读取旧版每任务目录的同名metadata；聚合记录优先，缺少时再检查旧文件和恢复产物。写入布局改变，不宣称与旧版逐文件布局完全相同。
- **私有设置结果。** 控制台的 `totp-setup-result.json` / `password-add-result.json`是权限受限的临时恢复文件；结果进入安全凭据存储后删除，恢复时也先安全保存再删除。安全保存失败不得当作成功清除；Linux无持久安全存储时不落这些敏感结果。CLI支持自己的默认/显式私有导出路径与交付策略，不能据控制台清理行为认定CLI不保留导出。
- **账号恢复。** `--checkpoint`输出路径与明确恢复选项区分；web-only必须真正完成web登录，恢复Cookie本身不表示已认证。已消费的手机号/workspace/OTP步骤尽量恢复，不重放已成功设置或非幂等token交换。
- **监控时间。** 默认间隔/冷却与旧版均为5分钟，interval合法最小1秒，冷却显式0禁用。环境转换对齐旧readDurationEnv的空值/数值/非法fallback。公开intervalMinutes/cooldownMinutes保持旧版round且最小1的标签契约，因此0冷却时该标签仍为1，实际冷却已禁用。
- **供应商。** LubanSMS无完成endpoint时Complete保持旧版no-op；不虚构成功远端调用。SMS终态错误/超时停止自动轮询但保留手动、重发和换号，Linux重启不虚构已安全保存的lease凭据。

## 本轮恢复与传输补齐

- OAuth code兑换成功先写tokens_received，离线恢复覆盖真实任务的login/full/resume；不依赖锁定Keychain或失效代理，也不改写既有自动化授权记录。旧JS不支持此新增阶段。
- refresh成功先写稳定目标旁私有sidecar，再写导出；两个独立进程和两次不同随机job ID的真实CLI验证只发一次grant。I/O、身份、路径或文件变动错误不会自动转为重登；连sidecar也无法写入时明确REFRESH_GRANT_UNSAVED，不宣称可恢复。多账号仅刷新首项，其余账号和大整数未知字段保留。
- 恢复覆盖20种metadata/output/checkpoint/账号设置结果组合；保留旧下载及原lastOperationAt，仅安全未启动任务自动排队，已完成TOTP/密码设置不重放。旧邮箱API配置进入安全凭据服务或内存，新增metadata不保留秘密。
- 运行中的jobs GET/query与monitor增量发现CLI输出及checkpoint；凭据读取在全局锁外可取消，导入期间邮箱reservation阻止并发启动。多账号监控按身份选择，旧HTTP响应不能覆盖后来手动重登或永久跳过。
- native/tls-client/浏览器代理桥移除隐藏30秒上限，尊重调用context。body中断仍保存服务器或真实Chrome已接受的Cookie及删除；Chrome关闭临时请求页后用独立短预算读取真实cookie库，失败明确报告。
- Linux容器真实Chrome验证取消及父进程退出。无init容器出现的僵尸仅在确认无FD/映射/命令行后区别标记；带--init的复验零Chrome、零僵尸。容器显式no-sandbox，不能算Linux sandbox验收。
- macOS真实Keychain跨进程读取/更新/复读已验；默认五分钟monitor确实等待完整周期并验证关闭。20任务/4个真实Chrome/16个浏览器池等待/第21项FIFO排队组合已验，不等于20个真实上游账号压测。

## 输入、取消和短信恢复的复核修复

2026-09-26重新逐项审计时发现以下实际缺口，已修实现并新增回归；这些记录不替代Windows原生或真实第三方账号验收。

- 响应完成取消：Linux完整验收暴露取消与正常EOF同时发生时返回成功的竞态；native/fingerprint现在在body读取后无论是否读错误都检查context。原真实TLS fixture和严格deadline断言保留，新增取消/deadline四分支先复现旧实现全失败，再在macOS/Linux各20轮race验证修复。
- TLS请求：tls-client v1.9.0的HTTP/2重拨会丢失请求context；每个FingerprintClient现在用可取消的串行请求入口和独立拨号作用域，等待时间纳入原请求预算，保留H2连接复用、ClientHello及代理行为。不同任务的client仍可并发。原生HTTP只在拨号/TLS握手期间持有取消钩子，最后一层握手完成立即解绑；真H2双流回归确认取消一条不伤另一条。31秒请求仍尊重调用者预算，没有恢复隐藏30秒截断。
- 输入：HTTP同阶段接受变为原子操作，接受后立即显示working，重复提交409；SMS暂停/释放准备结束后才交付可消费输入，取消或新提示使旧提交失效。密码从CLI scanner、Manager消费到Flow.ask和真实协议请求body全部保留首尾空白；OTP/手机号仍trim，空密码及不支持的控制命令拒绝。
- 邮箱：自动轮询时网页保留手动覆盖入口；轮询进度只更新mailStatus/mailApiError，不重开已接受的输入。CLI不因自动等待事件提前读stdin或在EOF退出；超时转手动和r重发仍由明确的手动提示驱动。
- 短信：缺失、锁定、损坏或身份不匹配的安全订单恢复为明确错误和人工入口，不再假显示正在收码。协议消费输入后的同阶段running事件不会误释放号码或重置提交；自动号码/验证码和手动输入竞争只接受仍属于当前提示的结果。
- 凭据：Windows DPAPI调用返回后再次检查取消，已取消的Load不返回解密凭据；临时密钥/明文清理。该取消边界已有可移植回归，真实DPAPI CurrentUser、DACL及服务仍需Windows执行。

新增证据入口包括：handshake_lifetime_test.go、dpapi_cancellation_test.go、input_contract_test.go、input_state_test.go、sms_restore_http_test.go、sms_input_lifecycle_test.go、cli_input_contract_test.go，以及密码verify body和五场景生产CLI/真实Chrome邮箱链。机器记录绑定新冻结源码；旧验收报告保留于docs/verification-history/。

## 兼容数据文件

| 文件 | 用途及恢复策略 |
|---|---|
| `job-meta.json` | Go写输出根目录聚合version/jobs/updated_at，兼容读取旧版每任务metadata；非敏感任务/队列/操作/监控字段采用白名单，密码/TOTP/代理密码不进入普通metadata |
| `login-checkpoint.json` | 私有阶段、Cookie和OAuth会话恢复；校验版本、邮箱、源站、传输绑定与已消费步骤 |
| `sub2api-import-oauth.json` | 私有v1账号导入输出；refresh/远端合并保留未知字段并验证身份 |
| `.<目标文件名>.refresh-grant-recovery.json` | Go扩展的私有已接受refresh恢复，绑定原source/target与账号；只做本地完成，旧JS不支持 |
| `totp-setup-result.json` | 私有种子/URI和激活状态，防止已提交设置被错误重放；控制台安全保存后删除临时恢复文件，CLI按私有输出选项处理 |
| `password-add-result.json` | 私有密码设置结果与候选恢复校验；控制台安全保存后删除临时恢复文件，CLI按私有输出选项处理 |
| `sub2api-monitor.json` | 兼容监控配置/状态/永久跳过/冷却；旧明文API key迁入安全存储或仅在内存使用后从普通JSON移除 |

六类golden位于 `docs/fixtures/go-port-golden/`，有roundtrip、恢复和未知字段回归。尚未采样的生产扩展字段不能仅凭golden推断全部兼容。敏感文件采用权限受限临时文件、必要fsync与原子rename；Windows额外检查当前用户DACL及文件系统支持。

## 第三方依赖

`go.mod`声明Go最低语言版本 `1.22.0`，已在Linux实际运行全仓普通测试。当前macOS以Go1.22.0运行曾被dyld拒绝（missing LC_UUID），此失败保留；当前macOS构建与验收采用Go1.27.1，平台CI采用stable工具链。直接依赖如下，确切完整依赖树以 `go.mod`/`go.sum` 为准：

| module | 固定版本 | 用途 |
|---|---|---|
| `github.com/bogdanfinn/tls-client` | `v1.9.0` | 指纹TLS客户端与可用profile |
| `github.com/bogdanfinn/fhttp` | `v0.5.34` | tls-client HTTP请求/响应类型 |
| `github.com/gorilla/websocket` | `v1.5.3` | Chromium CDP WebSocket |
| `golang.org/x/net` | `v0.35.0` | 网络/代理配套 |
| `golang.org/x/sys` | `v0.30.0` | Windows DPAPI、DACL和Service系统API |

间接依赖包括 `bogdanfinn/utls v1.6.5`、`x/crypto v0.33.0`、`x/text`、Brotli/compress、QUIC、CIRCL与HPKP。浏览器控制是项目内Go CDP实现，没有引入Node驱动。React/Vite相关依赖只用于前端构建；旧curl_cffi只用于旧版基线补验，不进入Go生产服务。

## 浏览器要求

- 安装Chromium或Google Chrome，通过 `CHROME_PATH` 或 `--chrome-path` 指定；程序不会用Node driver启动浏览器。默认查找常见安装路径。
- 需要支持现代headless、远程调试、CDP Fetch/Network/ExtraInfo、代理认证、Cookie含分区信息和跨进程iframe Target功能。内部协议请求还要求 `Target.createTarget(hidden:true)`，并通过 `Browser.getWindowForTarget` 验证无窗口；不支持或静默忽略时明确报错，不回退为可见请求标签。默认启用浏览器sandbox，需在特定容器显式选择相关启动配置；不以默认关闭sandbox获取测试通过。
- 浏览器UA、平台、Client Hints和版本从真实实例读取；不能任意声明不存在的Chrome版本。Go任务并发上限20与浏览器池分开限制，生产runner共用最多4个实例的池。
- 本机记录的浏览器版本是 `Google Chrome 154.0.8037.58`，Go工具链是 `go version go1.27.1 darwin/arm64`；版本字段会随冻结验证记录更新。这是已使用版本，不是最低兼容版本保证。可视浏览器公开SDK执行已有独立通过证据；无头模式及认证端点仍有待验边界。
- `TOSUB2_BROWSER_TEST=1`开启本地真实浏览器fixture；`TOSUB2_LIVE_BROWSER_TEST=1`独立访问公开真实上游。后者本次可视成功需 `TOSUB2_BROWSER_HEADLESS=0`；不据此声称无头上游成功或所有安全验证可自动完成。

## 历史测试与发布证据

记录见 `docs/go-port-verification.json`，Go源码清单及指纹见 `docs/go-port-go-source-manifest.json`。普通、race、vet及Chrome/Keychain全仓命令均退出0；每轮精确起止时间、耗时和源码一致性由机器记录维护，PASS事件包含子测试，不等于独立顶层测试数量。

| 执行 | 包数 | PASS事件 | 通过的顶层测试 | SKIP |
|---|---|---|---|---|
| `go test -count=1 -json ./...` | 15 | 780 | 395 | 49 |
| `go test -count=1 -race -json ./...` | 15 | 780 | 395 | 49 |
| 显式Chrome/Keychain的全仓race | 15 | 830 | 433 | 11 |
| `go vet ./...` | — | 无诊断，退出0 | — | — |

显式Chrome/Keychain全仓有11个SKIP：5个由父测试启动的子进程helper、2个独立live上游入口、3个长时间测试入口、1个Linux专属/proc检查。完整名称和理由见机器JSON。五分钟监控与31秒transport另行显式运行通过，不把默认SKIP计为PASS；默认普通/race共49个SKIP，另含未开启的浏览器与Keychain。Windows build-tag在macOS不执行。

公开真实SDK独立命令为 `TOSUB2_LIVE_BROWSER_TEST=1 TOSUB2_BROWSER_HEADLESS=0 CHROME_PATH=... go test -count=1 -race ./cmd/tosub2 -run ^TestLivePublicSentinelBrowserCompatibility$ -v`。它退出0并报告真实SDK token和SO `available`；该次独立结果不并入上表830个PASS事件。之前两次无头失败及其原因也保留在 `live_upstream`。

关键证据按层次区分：

- **协议与CLI。** `TestLegacyPasswordEmailMFAWorkspaceSmoke`、`TestLegacyPhoneFallbackChangeAndResend`及账号设置/恢复回归核对旧HTTP契约；`TestCLIResumeBrowserEmailMFAWorkspaceAndOAuth`使用生产CLI/runner、真实Chrome、动态SDK fixture、邮箱HTTP和交互stdin验证PKCE/导出/checkpoint清除。
- **真实UI和服务。** 两个React浏览器测试覆盖列表/账号控件/真实下载；`TestConsoleProcessRestoreDownloadDeleteAndRestart`使用生产入口恢复27个合成输出、下载/删除及多次信号重启；这不表示真实账号登录成功。
- **provider/monitor。** console真实Manager与本地SMS/Sub2API HTTP链、部分转传重启、显式零冷却、1秒timer和关闭测试已具备覆盖；这些修正均已纳入通过的全仓普通/race/Chrome及vet检查。
- **操作系统。** `TestMacOSKeychainIntegration`在显式启用的全仓集成中通过本机读写删除；Windows DPAPI、DACL和Service测试由目标机器的 `scripts/test-windows.ps1`执行，当前只完成交叉编译检查。
- **独立二进制。** `docs/go-port-binary-verification.json`记录最小PATH、临时HOME下的health/bootstrap、受保护API、embed JS/CSS、SIGTERM及日志检查；该类烟雾与Chrome/真实上游验收分开记录。最小PATH为 `/usr/bin:/bin`，health/bootstrap和静态JS/CSS为200，未授权jobs为403，SIGTERM退出0且日志不含启动token；构建产物与代码指纹随机器记录核对。该独立烟雾关闭浏览器，不代替上述浏览器集成。
- **旧版基线和源码。** `docs/go-port-legacy-baseline.json`保留原20项检查的初次结果及缺curl_cffi后重试；旧依赖仅在仓库外临时环境运行。`docs/go-port-source-manifest.json`用于确认参考源码未改写；`docs/go-port-go-source-manifest.json`记录受测Go源码，`docs/go-port-verification.json`核对测试及构建期间指纹一致性。

构建/启动脚本位于 `scripts/`，部署模板位于 `deploy/`，包含systemd用户服务、launchd Agent、原生Windows Service及兼容Task Scheduler安装脚本。本轮只执行了临时launchd安装副本的两次启停验收，注册与目录已清理；没有保留持久系统服务。安装、代理、凭据、浏览器和故障排查说明见 `README-Go.md`及 `docs/go-deployment.md`。

当前交付是可独立运行的Go实现与持续复验的本地覆盖；全部真实功能验收仍未达成，可视公开SDK/SO成功不消除无头验证失败、Windows未执行和未提供的授权后端边界，不能用“基本完成”代替逐项验收。

## 历史平台证据（2026-09-26）

该轮受测代码与构建统一绑定259文件SHA-256：d071b72b90ba352f16ff81c5aebfbff6271bfd05347f677a7fb60b2a63baf9f1。Linux普通/race/vet、真实Chromium全仓、原生构建和Go1.22最低版普通均实际执行；环境和SKIP记录见独立Linux报告，不与macOS Keychain数量相加。容器显式关闭sandbox，未把该配置当默认部署。

macOS临时launchd安装两次启动/停止、恢复golden输出、私有metadata落盘、进程与注册清理已验；直接使用Downloads中的二进制未ready，复制同一产物到临时安装目录通过。部署应沿用文档中的安装路径。Windows原生测试及真实授权账号/服务验收仍是未完成项。

Linux平台逐条命令、普通/race/浏览器/最低Go版本、空PATH二进制启动及容器清理记录见 `docs/go-port-linux-verification.json`。

## macOS 与本机容器部署补验

2026-09-26用户明确允许在本机容器部署。项目并非Windows专用：macOS原生二进制再次直接从Downloads路径启动成功，13项检查通过，包含health/bootstrap、API鉴权、React及JS/CSS、SIGTERM退出0和0600元数据落盘。临时原生进程已清理，证据在 `docs/go-port-macos-local-smoke.json`。这条新成功记录不删除此前某次启动未ready的历史。

新增根目录 `Dockerfile`、`.dockerignore`、`compose.yaml`，使用多阶段Go构建和Debian Chromium运行镜像。生产镜像实际确认没有Node/npm、Python或Go工具链。Compose以UID10001、只读根文件系统、私有数据卷、init和loopback映射运行；`http://127.0.0.1:4399`的health、页面、API鉴权、停止退出0、再次启动和元数据保存均通过。运行证据单独保存在 `docs/go-port-container-verification.json`，不覆盖原R2全仓测试记录。

Windows缺少的仅是Windows专有运行验收；它不是macOS/Linux启动的前置条件。容器运行Linux，不能替代DPAPI/DACL/SCM实测。真实账号和外部服务的授权验收边界也不因容器部署成功而改变。

同一生产镜像Chromium154.0.8037.57的13项顶层集成测试已通过（18个PASS事件、0SKIP），包含四浏览器池/20任务、DOM/Cookie/手动跳转、进程清理、动态SDK/SO及挑战fixture、React操作/下载、27条任务重启恢复和五种本地邮箱/MFA/workspace/OAuth流程。均使用断网测试容器和静态测试二进制，不是race或真实上游验收。默认sandbox在本机Docker设置下不可启动，保留6项失败记录与unshare被拒诊断；仅显式关闭sandbox的受控容器测试通过，默认配置未被改为关闭。

## 容器代理与TLS诊断修复

2026-09-26控制台的 `TLS fingerprint transport unavailable for "auto": no compatible bundled TLS profile passed validation` 已定位为代理地址指向容器loopback。原 `http://127.0.0.1:7897` 在容器内拒绝连接；控制台已改为宿主代理 `http://host.docker.internal:7897`。实际Go探测使用 `auto` 成功选择 `chrome_133`，收到公开上游HTTP403；该结果证明TLS及HTTP连通，不代表登录成功或Cloudflare/Turnstile/Sentinel验收。未重试真实账号。

transport诊断补丁区分代理拒连、DNS、连接超时、407、CONNECT失败及证书失败，错误只携带安全类别，避免将网络失败误报为所有TLS profile不兼容。生产API验证还发现 `headers: proxy` 被既有脱敏规则误判为敏感字段，已调整诊断文案，并新增多轮脱敏回归；全局秘密过滤规则未放宽。

新镜像 `tosub2-go:tls-diagnostics` 在独立临时容器通过合成闭端口任务的API诊断和优雅退出检查。该轮排查时原用户容器保持原ID、镜像及启动时间，未重启，以保留Linux进程内凭据；配置修复当时已生效，新诊断代码尚未替换当时运行中的容器。当前部署已改为本文开头所列原生服务。全仓测试、平台编译、镜像及清理证据记录在 `docs/go-port-tls-proxy-verification.json`，不会将旧R2报告改写为本次补丁的验收，也不宣称全部真实账号功能已完成验收。

## 可视验证与取消分类修复

2026-09-26后续排查确认原Docker服务为无头Chromium且没有交互式桌面，不能仅修改 `TOSUB2_BROWSER_HEADLESS=0` 就显示窗口。新增 `scripts/run-macos-visible.sh`，并在当前Mac桌面用独立launchd Agent运行最新Go二进制，监听 `127.0.0.1:4400`；启用可视Chrome和Chromium sandbox。单条已取消任务的元数据恢复成功，控制台已打开并设置本机loopback代理。当时原4399容器继续运行，未重启；内存凭据和监控配置未转移。当前该容器已移除。没有重新启动真实账号流程，人工网站验证仍由用户完成。

同时修复JobManager误分类：子请求/浏览器验证自己的 `DeadlineExceeded` 或 `Canceled` 不再直接把整个任务写成 `canceled` 并抹掉原因；只有任务context本身取消才归类取消，服务关闭仍标记中断。实际Solver本地超时、持久化恢复、子操作授权结果保留、用户取消及服务取消均有新增回归。历史单个 `canceled` 字符串不足以确定当时是用户取消还是旧误分类，未据此断言本次事件的唯一原因。浏览器提示也明确无头进程无法动态转为可视，以及容器需要桌面/显示服务。

新源码全仓普通测试和race各15包通过、823个PASS事件、50个明确SKIP，`go vet ./...`通过。独立macOS可视Chrome本地fixture的窗口呈现、挑战等待和两类进程清理4项通过、0跳过；不等于真实上游challenge验收。该轮新可视服务包含本次修复及上一轮TLS诊断修复；当时的旧Docker容器保留旧镜像，当前容器已移除。完整机器证据在 `docs/go-port-visible-console-verification.json`。
