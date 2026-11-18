# 同账号独立会话 2FA 并发：现有源码与入口互斥核查

本报告是 2026-11-17 当前工作区的源码事实与本地入口试验，不将其当成远端并发实测结果。未改业务代码。临时试验通过 Go overlay 注入测试文件，只使用指定测试账号的身份字符串，不读取密码或 TOTP 密钥，不创建任何远端请求。HAR 分析和远端时序实验另行记录。

2026-11-18 再次只读复核当前源码、上述入口试验材料及已有脱敏事件，未重新运行账号请求。本次补充主要限定 `mfa_info` 的证明范围，并区分明确拒绝、结果未知和传输确认未发送的重试策略。下列行号均重新对应当前工作区文件。

## `mfa_info` 的解释边界

`classifyTOTPState`（`internal/protocol/totp_status.go:30`）只检查本次 JSON 的 `mfa_enabled_v2` 和 `factors.totp`。它在该数组恰好含一个普通因子时返回 `enabled`（`:66`、`:76`）；没有查询其他因子清单、验证其他密钥，也没有证明该列表穷尽账号全部可用因子。`native_default_factor_id` 不参与该分类。

已有实测材料 `.local-run/concurrency-audit/run-02/events.jsonl` 中，记录 #84 于北京时间 23:14:59 返回单个 F3；#86 移除 F3 返回 HTTP 200、`{"message":"Factor disabled","passkey_credential_manager_sync":null}`；随后 #87、#103、#104、#106 均返回单个 F2。这里仅引用已落盘证据，不新增远端实验。该现象不能证明隐藏因子的具体服务端机制，但足以否定“新因子成为返回列表唯一项，所以旧因子已经移除或旧密钥必然失效”的推理。

因此本报告中的“唯一”仅指**本次返回列表恰好一个普通 TOTP 条目**。代码在 `totp_reset_continue.go:139` 声称“确认当前账号只启用了本次新验证器”，实际检查只有 `waitForTOTPResetState` 的上述分类及 ID 匹配（`totp_reset_verify.go:88`）。报告不能把该日志升级为账号所有验证器的完备证明；因子不再展示，也不能直接标记其密钥失效。

## 接口及执行入口

| 接口 | 当前用途及请求结构 | 源码位置 |
| --- | --- | --- |
| `GET /api/auth/session` | 使用当前 Cookie 读取网页用户、账号及 access token；校验邮箱、账号 ID、令牌账号绑定。它不是 TOTP 登记会话。 | `internal/protocol/totp_reset.go:273`，`currentTOTPAuthorization` |
| `GET /backend-api/accounts/mfa_info` | 读取 MFA 开关与 TOTP 列表；带 bearer token、账号 ID、设备/浏览器上下文以及禁缓存头。 | `internal/protocol/totp_reset_verify.go:31`、`:72`；`totp_reset.go:352`、`:376` |
| `POST /backend-api/accounts/mfa/user/disable_in_house` | 按保存的旧验证器 ID 移除，正文只有 `factor_id`。 | `internal/protocol/totp_reset_state.go:190`、`:209` |
| `POST /backend-api/accounts/mfa/enroll` | 登记，正文 `factor_type=totp`、`source=settings`；接收新 secret、登记 session_id、factor 信息。初次设置与移除后设置共用接口，但控制流不同。 | `internal/protocol/totp_reset_initial_enroll.go:140`；`totp_reset_continue.go:45` |
| `POST /backend-api/accounts/mfa/user/activate_enrollment` | 激活，正文 `factor_type`、登记 `session_id`、该 secret 当前生成的验证码、`source`；在待激活密钥安全保存后发送。 | `internal/protocol/totp_reset_continue.go:88`、`:104`、`:110` |
| `POST /backend-api/accounts/logout_all` | 退出所有设备，空正文；当前流程先保存退出意图，然后发送。 | `internal/protocol/totp_reset.go:453`、`:479` |

产品 runner 在 `cmd/tosub2/job_runner.go:105` 先取得账号锁，再检查重置条件、构建协议运行环境。协议入口是 `Flow.ResetTOTP`（`internal/protocol/totp_reset.go:39`）。临时实验若直接构建两个 Flow 并执行该协议入口，不会经过 runner/Manager 的产品账号互斥；这属于模拟另一设备的协议层实验，不能报告为产品允许两个相同账号任务同时运行。

## 分支行为

### 初始状态读取

`readTOTPResetStateWithRetry` 最多 4 次，使用同一登录会话，间隔 0.5、1、2 秒。明确读到 enabled 即返回；未启用或结构可理解但不明确的状态会读满预算；401/403、畸形响应和不允许初次登记的结构立即停止。HTTP 500/502/503/504 可在相同预算内重读（`totp_reset_verify.go:13`、`:31`）。底层只读网络重试还有独立的连接重试配置，不能把“最多 4 次状态检查”解释为无条件只有 4 个底层网络尝试。

enabled 的代码条件是 `mfa_enabled_v2=true`，返回数组中恰好一个普通 TOTP、`is_recovery=false`、非空字符串 ID，且没有业务拒绝或结构矛盾（`totp_status.go:30`）。多个条目、恢复验证器、畸形字段不等于“没有 2FA”；返回一个条目也不证明账号仅一个有效验证器。

| 最终初始读取结果 | 当前动作 | 判断位置 |
| --- | --- | --- |
| 唯一明确 enabled | 保存旧 ID，准备与隔离凭据，移除旧因子，再登记/激活新因子。 | `totp_reset.go:149`、`:171` |
| disabled | 使用 `enroll_first`，不发送移除，直接尝试一次初次登记。 | `totp_reset.go:152`、`:165` |
| unknown，但缺失信息仍通过 `totpInitialEnrollmentAllowed` | 同样走 `enroll_first`。这里是一次受控写入尝试，不是将 unknown 认定为 disabled。 | `totp_reset_initial_enroll.go:16` |
| 多因子、恢复因子、业务拒绝或不允许的畸形结构 | 在修改之前停止。 | `totp_reset.go:145` |

### 初次登记冲突与移除后登记冲突并不等价

初次登记会留下独立 `initial_enroll_*` 发送编号、回执与结果，移除后登记使用 `enroll_*`，不会覆盖前一回执。

初次登记的完整响应必须匹配当前操作与尝试编号，HTTP 为 2xx、400、409 或 422，并被严格“已存在”解析器识别，才进入回退（`totp_reset_initial_enroll.go:88`、`:146`）。回退流程：同会话最多重新读 4 次，取得唯一普通 TOTP 的 ID，保存为旧 ID，移除一次，再进行一次普通登记。这里没有回到初次登记的循环。

解析器只接受限定的 `error`、`code`、`message`、`detail` 结构；机器码包括 `totp_already_enrolled`、`totp_already_enabled`、`totp_already_exists`、相应 `mfa_*`、`factor_already_enrolled`/`factor_already_exists`，以及有限完整短句。普通 409、“conflict”、未知字段包装、同时返回登记材料、矛盾成功字段，都不被认定为可靠冲突（`totp_enrollment_conflict.go:12`、`:94`）。这些白名单是否匹配真实服务端响应，要靠本次远端实测，不能从模拟测试通过推出。

**移除后的登记没有此回退。** `continueTOTPReset` 直接登记，不先重查 MFA。如果 A 移除后暂停、B 已设置成功，再恢复 A，A 会直接向 enroll 发送请求（`totp_reset_continue.go:32`、`:45`）。此处非 2xx 返回错误；2xx 明确业务拒绝同样停止；不调用初次冲突解析器、不重新读取 B 的 ID、不再移除、不再登记。普通 409 被当作明确失败；HTTP 408 或 5xx 则属于结果未知。未知且无法取得完整可靠材料时保留证据并停止，不重复登记（`:63`；HTTP 分类见 `totp_reset.go:402`、`:412`）。

### 移除结果

移除采用当时保存的 `old_totp_factor_id`，发送前不重新读取。如果 B 在移除前保存 F0，另一会话将其替换成 FA，B 仍会请求 F0；服务端是否严格按该 ID 处理，必须实测。

- 完整成功业务回执：立即继续登记；不额外 GET 复核停用（`totp_reset_state.go:245`）。
- 明确拒绝（如 401/403/409，或 2xx 业务拒绝）：停止，不以“可能已被另一会话删除”为理由补做读取并继续（`:232`）。HTTP 408 是下项的未知结果例外。
- 结果未知（例如 HTTP 408/5xx、断线或 2xx 不明确业务结果）：只读核对 disabled；确认后可继续登记。不会重复移除。读取发现一个第三方因子、与旧/预期 ID 不一致时停止（`totp_reset_verify.go:92`）。
- 本地持久化、隔离或发送前证据失败：不发送；保存与发送中断可能留下保守未知状态。

因此两个会话同时删 F0 后，失败方是否继续登记，取决于服务端真实返回是成功、明确拒绝还是当前程序分类的未知；代码没有通用“重复删除当成功”的处理。

### 登记、激活、远端匹配、本地保存是不同检查点

1. 登记响应中收到 secret，只能证明拿到了材料。`preserveEnrollmentMaterial` 保留可识别字段，但完整验证还要求 secret、登记 session_id、因子 ID/类型/恢复标志及绑定一致（`totp_reset_receipt.go:59`）。
2. `TOTPEnrollment` 回调先把待激活密钥写入受保护凭据，并读回核对；随后才提交激活（`cmd/tosub2/totp_reset.go:27`；`totp_reset_continue.go:88`）。
3. 激活明确拒绝：停止，不换 ID、不重新登记、不重新激活。未知响应则只读核对新因子，最多 4 次；确认新 ID 才继续（`totp_reset_continue.go:114`、`:148`）。
4. 直接初次登记成功的路径，激活收到成功回执后仍要求 GET 返回数组只包含新 ID（`:132`）。**只有接口实际同时返回多个 TOTP 条目时**，分类器才会返回 unknown，读满后停止；若服务端还有未展示因子，该检查无法发现，不能据此断言只有一个有效 TOTP。
5. 先移除后普通登记的路径，可仅凭完整匹配的激活业务回执提交，不再 GET 证明结束时仍是该因子（`:146`）。此时 `activation_verified=true` 的确认来源是 `business_receipt`，不能自动改写成“已只读核实远端最终因子”。
6. 最后安全保存、读回、提交元数据，才设置 `credential_persisted=true`（`:185`；`cmd/tosub2/totp_reset.go:56`）。

会话失效的 401/403 不触发重新登录。401 若匹配已知 `recent_auth_required` 结构可给专门文案；其他撤销类 401 在产品错误中主要表现为 HTTP 401，原始业务码可能被回执投影归一（`totp_reset.go:404`；`totp_receipt_projection.go:100`）。实验必须从传输边界另存脱敏真实业务结构，不能仅凭产品错误文字判断 token_revoked。

### 已成功之后的过期状态

当前重置函数完成后没有为成功任务持续查询远端因子。另一设备之后替换或移除该因子，不会自动撤回这一任务的历史成功，也不会自动修正其已保存密钥。初次登记路径的最终 GET 也只能证明读取当时接口展示了什么，不能保证之后未变化，更不能排除接口未展示的因子。这是源码能力边界；是否在本次实验中出现“双方都报告成功、最终只有一方有效”，仍须独立证明密钥有效性，不能仅凭最后展示的因子 ID 下结论。

## 账号互斥、凭据隔离与已执行入口试验

产品有两层本地排他：

- Manager 拒绝同邮箱存在排队/运行任务的另一重置，且待完成/未知 2FA 会阻止普通账号操作（`internal/jobs/manager.go:1598`；`internal/jobs/totp_reset.go:33`）。
- runner 使用用户缓存目录下的 OS 非阻塞文件锁。锁身份为规范化 origin 与小写去空格邮箱；不同输出目录不绕过同一用户锁命名空间。持锁进程退出/句柄关闭会释放；不会通过删除锁文件释放（`internal/accountlock/accountlock.go:24`、`:36`、`:53`；`accountlock_unix.go:12`）。

锁是本机同文件系统/用户缓存命名空间机制，不是服务端全账号分布式锁。宿主机原生程序与 Docker 若使用不同缓存目录/文件系统，不自动共享这把锁；其他设备当然也不共享。

2026-11-17 已执行如下临时入口试验，账号仅以 `TEST_ACCOUNT` 代号记录：

| 北京时间 | 试验 | 实际结果 |
| --- | --- | --- |
| 23:01:32.755914 至 23:01:32.756140 | 对指定测试邮箱取得真实用户级账号锁，再调用当前 `reset-2fa` 正常 runner。 | `ACCOUNT_OPERATION_BUSY`，单调间隔 0.226750 ms；发生在网络构建/凭据加载之前。释放后可再次取得锁，无删除锁文件。 |
| 23:01:32.667538 至 23:01:32.667601 | 临时 Manager 用 `Submit` 真实排队同账号普通任务 A，再对已导入的 B 调用 `QueueOperation(reset-2fa)`；未启动调度器。 | “同账号已有任务排队或运行，请结束后再重置 2FA”，单调间隔 0.062458 ms。 |

测试命令通过 `.local-run/concurrency-audit/lock-overlay.json` 注入临时测试文件；当前业务源文件未变化。测试身份与用户指定账号一致，但元数据目录和空内存凭据库是临时的，不是向运行中的 HTTP 控制台提交任务。两个试验都没有读取/写入密码或密钥、没有真实网络请求。结构化证据在 `.local-run/concurrency-audit/product-lock.json`。

正常持久凭据按账号身份管理，`BeginTOTPReset` 先保存 `TOTPResetPending` 与操作 ID，立即隔离同账号相关任务，再读回核对；即使读回失败也不会让其他任务继续使用原密钥。保存待激活密钥与最终密钥均要求操作 ID、材料与当前凭据绑定一致（`internal/jobs/totp_reset.go:112`、`:152`、`:158`、`:178`；`cmd/tosub2/totp_reset.go:27`、`:56`）。实验 A/B 使用独立受保护存储根时，应标为另一设备的各自本地保存，不能混用生产同账号凭据文件或把 A 的材料注入 B。

上述 Manager 实测不是“两次重置任务都成功进入运行后再竞争”：第二个入口已经被本地拦截。若前一个排队/运行任务本身是重置，`checkTOTPActionLocked` 还会在更早的 `internal/jobs/totp_reset.go:38` 返回 `ErrTOTPRecoveryRequired`。两种拒绝文案属于不同入口条件，不能混为同一实测。

## 结果分类与重试的准确边界

- `totpResetRequest` 在 `totp_reset.go:404` 处理 HTTP 拒绝；`:412` 只将 HTTP 408 和所有 5xx 标为 `uncertain=true`。普通 400/401/403/409/422 不因“也许另一个会话已经处理”而自动变成未知。
- 外层收尾在 `totp_reset.go:89` 起初保守标记状态，再由 `totpResetError.uncertain` 覆盖为 failed/unknown（`:95`）；如果已经记录激活成功却未完成确认/保存，则强制 unknown（`:102`）。进入修改准备后的错误保留最后阶段并置为 `manual_review_required`（`:105`）。因此“需要人工核对”阶段不等于每种错误都被分类成未知。
- 业务写请求配置了 `RetryBeforeSend`（`totp_reset.go:378`、`:479`）。`internal/transport/read_retry.go:240` 遇到已收到任何响应即返回；`:249` 仅允许瞬态网络错误、且传输明确证明请求未发送时重试写请求。结果未知、已收到拒绝或已发出但丢响应，不获得重复发送许可。底层 `SingleAttempt` 为每个实际尝试关闭库内重放（`:232`）。本次 S5 临时探针另显式使用 `SingleAttempt=true`，不能把实验严格一次调用推广为生产无条件零连接重试。
- 初次冲突识别先检查完整原始业务结构，再将**已识别**结构规范化为 `totp_already_enrolled`（`totp_receipt_projection.go:21`）。未知错误通常投影为 `unrecognized_remote_error`（`:100`），所以判断实际服务端文案必须引用实验脱敏传输记录，不能只看产品保存后的投影。
- 直接初次登记的冲突回退最多发生一次：`tryInitialTOTPEnrollment` 返回旧 ID 后，顶层调用一次移除、再一次普通登记；普通登记路径不回到初次登记分支（`totp_reset.go:165`、`:171`、`:176`；`totp_reset_continue.go:32`）。源码没有自动互删互设循环。

## 报告引用用的源码定位表

| 报告结论 | 当前源码位置 |
| --- | --- |
| 产品 runner 在网络前取得同账号 OS 锁 | `cmd/tosub2/job_runner.go:105`、`:106` |
| Manager 拒绝同账号其他排队/运行任务期间的新重置 | `internal/jobs/manager.go:1601`、`:1607` |
| 正在重置或凭据隔离中的账号更早被阻止 | `internal/jobs/totp_reset.go:33`、`:61` |
| 用户缓存锁命名空间及 origin/email 身份 | `internal/accountlock/accountlock.go:24`、`:29`、`:36`；`accountlock_unix.go:12` |
| 初始 GET 与最终分支选择 | `internal/protocol/totp_reset.go:137`、`:149`、`:165` |
| 最多四次状态检查及 0.5/1/2 秒等待 | `internal/protocol/totp_reset_verify.go:11`、`:31`、`:58` |
| 本次列表单个普通因子的分类 | `internal/protocol/totp_status.go:30`、`:66` |
| 允许缺失/滞后结构做一次初次登记的条件 | `internal/protocol/totp_reset_initial_enroll.go:16` |
| 初次登记完整冲突回执门槛与一次回退 | `internal/protocol/totp_reset_initial_enroll.go:88`、`:146`、`:185` |
| 冲突结构/机器码/文案白名单 | `internal/protocol/totp_enrollment_conflict.go:17`、`:76`、`:94` |
| 固定旧 ID 移除、明确拒绝停止、未知只读核对 | `internal/protocol/totp_reset_state.go:209`、`:232`、`:252` |
| 移除后直接登记；没有再查/冲突回退 | `internal/protocol/totp_reset_continue.go:32`、`:45`、`:63` |
| 登记材料完整性和不同 ID 检查 | `internal/protocol/totp_reset_receipt.go:59`、`:79`；`totp_reset_continue.go:18` |
| 激活前待保存回调、之后生成验证码和发送 | `internal/protocol/totp_reset_continue.go:88`、`:104`、`:110` |
| 初次登记要求读取匹配；普通路径可凭激活回执提交 | `internal/protocol/totp_reset_continue.go:132`、`:146`、`:162`、`:174` |
| 激活明确拒绝停止；未知只读核对 | `internal/protocol/totp_reset_continue.go:148`、`:154` |
| 远端已激活、本地保存失败时仍保留材料并标未知 | `internal/protocol/totp_reset_continue.go:201`、`:204` |
| 产品待激活/最终凭据按操作 ID 核对、保存及读回 | `cmd/tosub2/totp_reset.go:27`、`:56`、`:76`、`:85` |
| Manager 凭据隔离和最终元数据提交 | `internal/jobs/totp_reset.go:112`、`:152`、`:178`、`:187` |
| session 验证身份但没有在函数内证明业务 token 有效 | `internal/protocol/totp_reset.go:273`、`:294`、`:319` |
| logout 意图先保存、发送、按回执结束 | `internal/protocol/totp_reset.go:473`、`:479`、`:508` |
| logout 的 200/null 兼容判定 | `internal/protocol/totp_reset.go:511`、`:524` |

## 退出所有设备与只读探针

现有 `logoutAfterTOTP` 只发一次逻辑 logout_all 请求，收到完整 2xx、没有拒绝/挑战即记 succeeded；`200/null` 属于支持的成功响应（`totp_reset.go:511`）。这表示退出请求回执成功，不表示程序已测量所有 Cookie/token 都失效。网络结果未知不重复发送；5xx 记 unknown，其他非 2xx 记 failed；本地写意图失败不发送。

`GET /api/auth/session` 200 且含用户、账号和 token 能通过当前身份结构检查，但没有在这个函数内确认该 token 访问受保护业务的结果。必须另用 `GET /backend-api/accounts/mfa_info` 检验。项目 transport 正常会将 Set-Cookie 写回 Cookie Jar；固定 Cookie 观察路径必须由临时探针保持快照或每次用独立副本，不能直接复用正常轮换 Jar 后仍称固定快照。

探针应使用精确方法/路径允许列表，禁止自动登录、跳转登录、OAuth刷新或任何写接口。单次实际发送时间应记录在最内层传输前，完成时间记录响应读取完毕；以单调时钟计算间隔并另输出北京时间。一次逻辑只读探针若启用底层网络重试，需分别记录每个实际尝试，网络故障/挑战/429/5xx不能计为明确会话撤销。

## 待实测验证而非预设结论

当前白名单是否包含真实冲突；两次 enroll 是否均成功、登记材料是否互相失效；激活竞争是否替换或产生多因子；重复/过期 ID 移除是否幂等；退出后 token 与 Cookie 的实际撤销延迟，均不能由源码或 HAR 他人会话得出。

本报告不提出或执行业务修改。修改建议应在远端时序证据收集完成后，针对已观察结果给出；不把程序未具备的回退能力写成已执行行为。
