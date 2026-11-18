# 登录后重置 2FA：GitHub v2026.11.17 与当前工作树

比较日期：2026-11-18，北京时间。**发布版要求先找到唯一旧因子才开始；当前版在没有旧因子时也能设置。反过来，发布版可以凭可信激活回执提交，而当前版的新操作必须先从同一账号的状态中核对本次登记的因子。** 多做的查询不是再次激活，也不承诺此后账号只有一个验证器。

比较对象是[固定发布提交](https://github.com/Cengyj/toSub2-go-port/tree/39fcf956ac36eda52fda374bd8d805672b658f46)，不是主分支。带注解标签对象是 `adc4c180716b7e44b8a074e350e2662373ce9352`，解除标签引用后的源码提交及临时检出的 `HEAD` 均为 `39fcf956ac36eda52fda374bd8d805672b658f46`。工作树 `main` 没有初始提交；比对时 `totp_reset.go`、`totp_reset_continue.go`、`job-logs.jsx` 的 SHA-256 前 16 位分别为 `3619453a6b7de2e2`、`402514974c1f203f`、`f145ff5bc64a6a7d`。[上次验收报告](totp-reset-acceptance-2026-11-18.md)的“修前”是中间工作树，不是这个标签。例如它的无旧因子路径已经有登记，发布版的实收登记却是 0。

## 正常入口：两版各 19 个回环场景

两边都提交控制台的显式“重置 2FA”操作，经过各自的 Manager、runner、协议和临时保护存储。假上游处理函数相同，只回答产品发来的请求。下表 `查 / 移 / 登 / 激` 是上游**实际收到**的状态 GET、移除、登记合计、激活；初次和正式登记用的是同一请求，列中不重复相加。登录、读日志不计入；19 个场景各登录一次。唯一有退出的场景单列。**测试通过指行为符合该版本，不代表该次重置都应该或已经成功。**

| 输入（测试名） | v2026.11.17 实收及任务结果 | 当前工作树实收及任务结果 | 用户能从当前保留日志判断什么 |
|---|---|---|---|
| 已明确停用 `empty` | `1/0/0/0`，未开始 | `2/0/1/1`，提交 | `NO_ORDINARY_TOTP`：跳过移除；第二次查是核对新因子。 |
| 一个已知旧因子 `old` | `1/1/1/1`，提交 | `2/1/1/1`，提交 | 两版完整移除回执后都直接登记；当前多的是**激活后**的查询。 |
| 首查停用；初登明确冲突，旧目标已获准 `initial-conflict` | `1/0/0/0`，未到初登 | `3/1/2/1`，提交 | 初登明确被拒；原会话查到匹配旧目标，移除一次，正式登记一次，核对新因子。 |
| 同上，正式登记也冲突 `formal-conflict` | `1/0/0/0`，未到初登 | `2/1/2/0`，停止 | `FORMAL_CONFLICT`：旧移除已有证据，新未确认；没有第二次移除，没有激活。 |
| 首查就有旧因子，移除后的登记冲突 `formal-after-remove` | `1/1/1/0`，停止 | `1/1/1/0`，停止 | 写次数没变。发布版重点是 HTTP 409；当前首要结果是“旧因子移除已有证据，新因子尚未确认”。 |
| 仅可选状态一直缺失 `optional-state` | `1/0/0/0`，停止 | `5/0/1/1`，提交 | `OPTIONAL_STATE_MISSING`，不当成明确停用。原会话初查及三次退避复读，最后一查核对新因子。 |
| 模糊冲突 `ambiguous` | `1/0/0/0`，未到初登 | `1/0/1/0`，停止 | `ENROLLMENT_UNCONFIRMED`；没有触发移除。 |
| 冲突后目标变化 `changed`；没独立绑定 `unbound` | 各 `1/0/0/0`，未到初登 | 各 `2/0/1/0`，停止 | `TARGET_NOT_AUTHORIZED`；只看到一个 ID 也不会追删。 |
| 无旧种子，本次登录已手工核验旧因子 `manual-proof` | `1/0/0/0`，未到初登 | `3/1/2/1`，提交 | 本次登录的因子核验能提供绑定；不需要用户交出旧种子。 |
| 激活有回执，查询会同时显示旧、新因子 `new-and-old` | `1/1/1/1`，提交；**未查该列表** | `2/1/1/1`，提交 | 新 F2 已匹配；原定移除的 F1 仍可见，旧因子状态待核对。 |
| 激活有回执，查询只会显示另一因子 `new-not-visible` | `1/1/1/1`，提交；**未查该列表** | `2/1/1/1`，未提交 | `EXPECTED_FACTOR_NOT_VISIBLE`，本轮有激活回执和已读回的待核对材料，没有再激活或移除。 |
| 激活回执丢失，查询匹配 / 不匹配 `lost-then-matched` / `lost-then-other` | 各 `2/1/1/1`；匹配时提交，否则停止 | 次数及是否提交均相同 | 后续 GET 不替代失去的回执；`activation_receipt=unknown`，并保留 `last_write_issue=NO_RESPONSE`。上游实收一次不等于客户端收到了回执。 |
| 旧 ID 被 400 拒绝 `stale` | `1/1/0/0`，停止 | `1/1/0/0`，停止 | `old_removal=refused`。本地已隔离不是远端已删除的证据。 |
| 登记材料不完整 `partial` | `1/1/1/0`，停止 | `1/1/1/0`，停止 | 指出本例缺少登记 `session_id`；没有发激活，没有把部分材料写成可用凭据。 |
| 状态回包账号不符 `wrong-account` | `1/0/0/0`，停止；本夹具还报告停用 | `1/0/0/0`，停止 | 当前明确是 `IDENTITY_UNVERIFIED`。相同次数不能证明标签也检查了状态回包的账号。 |
| 网页会话 200、受保护查询 401 `session-200-protected-401` | `1/0/0/0`，停止 | `1/0/0/0`，停止 | `SESSION_REVOKED`，不把网页会话 200 当业务授权有效。 |
| 用户另选退出；完整 200/null `logout-accepted` | `1/1/1/1`，另退出 1 次 | `2/1/1/1`，另退出 1 次 | 两边退出时 Manager 都已最终提交。发布版写“退出所有设备：已完成”；当前写请求已被接受，不宣称已观测所有会话失效。 |

**入口限制未放开。** 两边的[控制台准入](../../internal/console/totp_actions.go#L151)相同：本地既无已有密钥、也无已启用记录的全新账号，仍提示“请使用首次设置”。上表 `empty` 指**已获准执行重置**、登录后远端明确显示停用，不是用测试或后台路径绕过按钮。发布版凭据结构没有因子 ID 字段；比较只在有此字段的版本写入合成旧绑定，不替发布版模拟这项能力。

## 为什么结果不同

| 决策或证据 | 标签实现 | 工作树实现 |
|---|---|---|
| 选择旧因子 | [初查和选择](https://github.com/Cengyj/toSub2-go-port/blob/39fcf956ac36eda52fda374bd8d805672b658f46/internal/protocol/totp_reset.go#L134)：状态不等于“一个明确启用的普通因子”就退出，没有初登回退。 | [初查分流](../../internal/protocol/totp_reset.go#L157)区分已停用、可选字段欠缺、危险或矛盾输入。[冲突回退](../../internal/protocol/totp_reset_initial_enroll.go#L153)在独立绑定或本次登录核验与唯一目标一致时才继续；若正式登记再冲突就停止。显式重置才有此权限。 |
| 激活回执与成功 | [激活后的两个分支](https://github.com/Cengyj/toSub2-go-port/blob/39fcf956ac36eda52fda374bd8d805672b658f46/internal/protocol/totp_reset_continue.go#L128)：完整、绑定正确的业务回执本身可成为确认来源。仅结果不明时查状态；走查状态分支时要求整表唯一。不是“HTTP 200 一律成功”。 | [激活后的必经查询](../../internal/protocol/totp_reset_continue.go#L132)、[按已知 ID 匹配](../../internal/protocol/totp_status.go#L26)：新操作必须核对同账号、同次登记的一个普通匹配项。不要求整表唯一；不会挑别的条目去删除。重复 ID、目标仅是恢复因子、条目类型异常都不是成功。 |
| 凭据保存和下一次登录 | [凭据适配](https://github.com/Cengyj/toSub2-go-port/blob/39fcf956ac36eda52fda374bd8d805672b658f46/cmd/tosub2/totp_reset.go#L27)已有 pending 和读回；登记因子 ID 在受保护 journal 中，而不在有效或 pending 的凭据字段中。[后续登录](https://github.com/Cengyj/toSub2-go-port/blob/39fcf956ac36eda52fda374bd8d805672b658f46/internal/protocol/flow.go#L551)取首个符合条件的候选。 | [保存绑定](../../cmd/tosub2/totp_reset.go#L25)把本轮新因子 ID 与 pending、最终密钥一起校验。[后续登录](../../internal/protocol/flow_mfa.go#L10)按密钥已有绑定选；无绑定且有多个候选不猜，只有一个候选可在成功验证后学习绑定。不会把“另一个默认展示项”判为密钥无效。 |
| 本地提交和旧记录 | [仅本地完成](https://github.com/Cengyj/toSub2-go-port/blob/39fcf956ac36eda52fda374bd8d805672b658f46/cmd/tosub2/totp_reset.go#L121)、写前记录、同账号互斥、隔离、可选退出前的 Manager 提交均已存在。 | 仍可只补本地，网络增量 0；所需证明按操作创建时的规则验证。[新证据版本 3](../../internal/protocol/totp_reset_evidence.go#L79)要求有时间和绑定的状态核对；旧版本 2 的合格回执仍可按原语义完成本地保存，不倒填“当时做过查询”。展示日志不提供恢复授权。 |
| 网络不确定 | [移除回执及只读复核](https://github.com/Cengyj/toSub2-go-port/blob/39fcf956ac36eda52fda374bd8d805672b658f46/internal/protocol/totp_reset_state.go#L138)：完整成功移除回执就继续；明确拒绝停止；结果不明不重放。完整回执后的连接清理错误也不是新增的失败理由。 | [状态读预算](../../internal/protocol/totp_reset_verify.go#L35)统一控制，单个读阶段最多四次、退避、30 秒截止、可取消；[请求分类](../../internal/protocol/totp_reset.go#L403)区分挑战、授权、限流、服务端故障、未收到响应等。确证未发送与可能已发不同，不能把一次逻辑写意图当 HTTP 实收。 |

**两条容易误归因的线索（函数级对照）。** 两版的激活分类器都接受 `success:true` 单独出现或附带字符串 `request_id`，负面回执在两版都不是成功；这只是激活回执，不是最终提交。标签根本没有初登冲突分类器，不能说它是“因为多了 `request_id` 才回退失败”。当前[冲突分类器](../../internal/protocol/totp_enrollment_conflict.go#L12)及[回执投影](../../internal/protocol/totp_receipt_projection.go#L16)对有、无该字符串的明确拒绝结论一致；矛盾结果、错误类型、登记材料和模糊文案不因此变成移除授权。

两版都用同一个 `preserveAccountCredentialBaseline` 输入测试 JSON 读回：`MailHeaders` 为 `nil` 或相同的非空表时都通过；仅从空表变成 `nil` 时，标签拒绝，当前通过；把操作号改掉时两版都拒绝。标签使用全结构 `reflect.DeepEqual`，没有名为 `saveBoundTOTPCredentials` 的函数；当前的该函数使用[只归一化空头表的比较](../../internal/credentials/store.go#L76)，还验证密钥和因子等反例。本项是刻意构造的存储往返，**不是实测过的线上失败原因**。

## 用户复制到的证据

同一个 `new-not-visible` 场景，两边都从各自控制台的日志 API 喂给各自的生产 `JobLogs` 组件，触发“复制当前显示的日志”，捕获写给剪贴板的文本。这里是组件测试，不是读取系统剪贴板。

- [标签复制原文](totp-tag-comparison-evidence-2026-11-18/tag-clipboard.txt)：末条是“2FA 已重置，新密钥已安全保存”。它确实完成了标签定义的本地提交；它没有查激活后的列表，日志也不能证明查到了 F2。
- [工作树复制原文](totp-tag-comparison-evidence-2026-11-18/worktree-clipboard.txt)：操作 `O27849559487c0d90` 的末条写“服务端已确认激活；本次预期 F2，当前查询显示 F3”，并写明“待核对材料已安全保留。下一步：核对因子绑定”。详情为 `last_trusted_step=activation`、`pending_readback=verified`、`local_commit=not_performed`，没有再激活、移除或退出。此结论以该轮只读观察为限。

发布版[已记录操作、时间、阶段、状态和消息](https://github.com/Cengyj/toSub2-go-port/blob/39fcf956ac36eda52fda374bd8d805672b658f46/internal/jobs/operation_state.go#L160)，已有归档和分页；文案上限 512 字符，相邻相同项可合并。[旧复制按钮](https://github.com/Cengyj/toSub2-go-port/blob/39fcf956ac36eda52fda374bd8d805672b658f46/web/src/components/job-logs.jsx#L270)只拼接当前可见行。当前仍用原日志链：协议 `emitData` → runner → `JobEvent` → [类型白名单](../../internal/resetlog/detail.go#L29) → [LogEntry](../../internal/jobs/operation_state.go#L241) → 原归档及 API → [日志面板](../../web/src/components/job-logs.jsx#L310)。不同尝试保留独立编号；原因、写入不确定性、材料状态和下一步是结构化字段，不靠长文案。复制首行标注本次/历史、筛选、可见条数、是否还有更早页及北京时间 `+08:00`。旧日志缺少的事实保持缺失；**两版都不能从没看到一行推出没发生请求**。

## 复测与适用范围

- **本次通过：** 19 对正常入口（给定各版可表达的凭据），从接收端计数；标签 5 份与工作树 19 份组件复制视图；两版的激活附加字段及凭据预检同输入测试。工作树另跑了冲突负例、按已保存 ID 登录和多因子只读核对。两版 `internal/jobs`、`cmd/tosub2` 的归档、同账号锁及本地完成定向 `go test -race` 通过；工作树还覆盖本地存储故障。不是整仓全量 race。
- **测试工具修正：** 首次标签运行因 macOS `/tmp` 与 `/private/tmp` 的 overlay 路径不一致，测试文件未加载，未产生业务样本；改用物理路径后全部通过。没有改发布版产品源码来适配测试。
- **历史实测，未重跑：** [并发报告](totp-concurrency-account2-evidence-2026-11-18.md)中的 S1～S3 登记 10 次、激活 10 次，全部 200，没有线上“已存在”样本。[双密钥报告](totp-dual-key-login-verification-2026-11-18.md)中两份密钥曾各自完成登录；[退出报告](totp-concurrency-logout-evidence-2026-11-18.md)中网页会话 200 与受保护业务 401 可同时成立。本次所有冲突响应都是合成夹具。没有真实账号、真实凭据后端、HAR 身份材料、历史在线驱动或部署，**当前线上行为未验证**。

重跑计数：在当前工作树设 `W=$(pwd -P)`；`R` 指向**另行检出的临时**发布提交，不能指向共享工作树。使用已缓存的 Go 1.27.4 和依赖，两个进程各用自己的临时存储。下例沿用本次的测试 overlay，区别只在旧版不存在的测试字段、旧版预期结果和日志格式，假服务、控制台请求和上游计数器不变。

```sh
export GOPROXY=off GOTOOLCHAIN=local TOSUB2_KEYCHAIN_TEST=0 TOSUB2_LEGACY_RUNTIME_TEST=0
W=$(pwd -P)
R=$(cd "$R" && pwd -P)
test "$(git -C "$R" rev-parse HEAD)" = 39fcf956ac36eda52fda374bd8d805672b658f46 || exit 1
E="$W/docs/pure-go-runtime/totp-tag-comparison-evidence-2026-11-18"
D=$(mktemp -d); D=$(cd "$D" && pwd -P)
patch -s -o "$D/tag_ledger_test.go" "$W/cmd/tosub2/totp_reset_acceptance_test.go" "$E/tag-ledger.patch"
jq -n --arg src "$R/cmd/tosub2/tag_ledger_test.go" --arg dst "$D/tag_ledger_test.go" '{Replace:{($src):$dst}}' > "$D/overlay.json"
(cd "$R" && go test -overlay "$D/overlay.json" -count=1 -v ./cmd/tosub2 -run '^TestResetConsoleRequestLedger$')
(cd "$W" && go test -count=1 -v ./cmd/tosub2 -run '^TestResetConsoleRequestLedger$')
```

夹具源文件 SHA-256：`6a3b50408f64c147ab3cf407f351897a42f4cd30fcda269523ef02ede464a4aa`。`tag-ledger.patch` 只适配临时测试文件，不要应用到产品代码。操作员日常处理见[一页排障](totp-reset-support-2026-11-18.md)。
