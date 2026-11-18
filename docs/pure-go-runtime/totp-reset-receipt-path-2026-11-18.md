# 登录后重置 2FA：凭激活回执直接保存

2026-11-18，北京时间。**登记返回新密钥不等于激活成功。** 登记材料绑定完整、待激活密钥已在安全存储读回后，才提交一次验证码。取得本次激活的完整成功回执，就直接提交凭据和任务，不再查验证器列表。勾选的退出仍在最终提交之后。日常看[一页排障](totp-reset-support-2026-11-18.md)；这里记录证据和复测方式。

## 哪些是观察，哪些是产品决定

- **历史 HAR（离线结构观察）**：`chatgpt.com_2026_11_17_22_02_13.har` 显示一个普通因子，移除 200（`Factor disabled`），两次查询未启用，登记 200 有 `secret`、`session_id`、`factor.id`，激活 200 有 `success:true`、没有回显 `factor`。网页随后确有四次已启用查询；退出是 200/`null`，随后受保护状态查询 401。不能说 HAR 中没有查询，也不能由此断定查询是激活成功的必要条件。未读取或输出身份字段、密钥值、验证码或头，未重放请求。
- **发布源码**：[v2026.11.17 固定提交的成功分支](https://github.com/Cengyj/toSub2-go-port/blob/39fcf956ac36eda52fda374bd8d805672b658f46/internal/protocol/totp_reset_continue.go#L128)凭可信激活回执直接提交，未确认才读状态。本次复核了该固定提交的源码。未整版回滚：保留了无旧因子可登记、回执丢失时按本次绑定匹配、密钥读回、日志及本地完成。此前的[两版入口比较](totp-reset-v2026.11.17-comparison.md)是修前快照。
- **历史线上实测，未重跑**：[S1～S3](totp-concurrency-final-2026-11-18.md)的 10 次登记、10 次激活均 200，没有“已存在”的线上样本。[两份密钥曾各自完成登录](totp-dual-key-login-verification-2026-11-18.md)。本次冲突、并发、断线、账号不符都属**合成夹具**；不能把一次成功、一次列表快照或退出回执当作所有旧密钥失效的证明。当前线上状态未验证。

## 从控制台入口验收

下表 `查/移/登/激` 是本地假上游**实际收到**的状态查询、移除、登记总数和激活请求。登录、查看账号信息、读日志不计入；退出另列。数字不是数日志行得出的。「修前」来自本轮定向入口基线、红测或注明的历史入口快照。右列为真实日志面板复制文字的摘录，不是 HAR 中的文案。

| 问题 / 输入 | 修前证据 | 修后行为及请求次数 | 用户实际看到的日志（摘录） |
|---|---|---|---|
| 明确未启用 | `empty`：`2/0/1/1`，激活后查一次 | `1/0/1/1`，激活后查 **0**；可在「账号信息」取新密钥 | 网站已确认激活，新密钥已保存；未另查验证器列表 |
| 已知旧因子 | `old`：`2/1/1/1` | `1/1/1/1`。勾选退出另实收一次，接收时任务已提交 | 网站已确认激活，新密钥已保存；未另查验证器列表；已收到旧验证器 F1 的移除回执 |
| 初登明确冲突，复查得到唯一获准旧目标 | `manual-proof`：`3/1/2/1` | `2/1/2/1`。同一登录会话，绑定目标后移除一次；首次拒绝与第二次登记回执各自保留 | 初次登记明确被拒；只读核对已有绑定 → 已固定本轮旧因子，不更换移除目标 |
| 初登明确冲突，复查却明确未启用 | `conflict-then-disabled`：`5/0/1/0`，停在找不到目标 | `2/0/2/1`，不移除，再登记一次；第二次仍冲突为 `2/0/2/0`，停止 | 登记提示已有验证器，复查显示未启用；不发移除请求，再登记一次 |
| 已经移除过，首次登记才报冲突 | 红测 `removed-conflict-then-disabled`：`1/1/1/0` | 复查明确停用 `2/1/2/1`；两次都冲突 `2/1/2/0`；复查看到另一因子 `2/1/1/0`。不追删 | 旧因子移除已有证据，新因子尚未确认。复查仍显示验证器；本轮已移除过旧因子，不再移除或登记 |
| 模糊 409、目标多个或不匹配 | 修前 `unbound`：`2/0/1/0`；历史入口快照也拒绝模糊文案 | 裸 409 `1/0/1/0`；冲突后多个或无授权绑定 `2/0/1/0`；初查多个 `1/0/0/0` | 登记未取得可信的新材料，不再登记或激活；或：可见因子不匹配已授权的旧因子，未发送移除 |
| 激活回包丢失，复查只显示其他因子 | `lost-then-other`：`2/1/1/1` | 仍为 `2/1/1/1`。本轮只收到一份激活请求；客户端记录“可能已发”，不提交、不退出。若正确匹配，同样次数即可提交 | 本次预期 F2，当前查询显示 F3。当前查询未匹配本次登记的因子；未再次激活或移除；待核对材料已安全保留 |
| 激活 200 同时回显另一账号 | 本轮针对回执快路径的红测误提交，**非线上样本** | `1/0/1/1`；拒用矛盾回包，后续查询、提交与退出均为零；脱敏不把它变回成功 | 当前身份或账号未核对，停止修改；待核对材料已安全保留 |
| 激活确认后本地凭据提交失败 | [前一版入口](totp-reset-acceptance-2026-11-18.md)：`2/1/1/1`，依赖列表匹配 | `1/1/1/1`；同一操作号在重启后仅补本地，**所有网络请求增量 0** | 服务端已确认激活；本地提交尚未确认；可仅补本地保存，无需重新设置；待提交的新密钥已安全保留 |
| 勾选退出，退出回包为 503 | 修前退出也是一次，不重发 | 无旧因子 `1/0/1/1`，另退出 1；新密钥可取，后续重登和上传不会误继续 | 网站已确认激活，新密钥已保存；未另查验证器列表；未确认退出结果，不重发退出请求 |

[完整成功样本：日志面板复制原文](totp-reset-receipt-clipboard-2026-11-18.txt)。操作 `O760961a91d89f16f`，范围为本次、全部、40 条；终态是 `last_trusted_step=local_commit`、`activation_receipt=accepted`、`factor_match=skipped`、`local_commit=committed`。没有可见因子数量。该样本已与隔离浏览器的剪贴板参数逐字核对。

## 兼容与复测

- [协议工作流](../../internal/protocol/totp_reset_continue.go)、[登记与冲突复查](../../internal/protocol/totp_reset_initial_enroll.go)、[证据校验](../../internal/protocol/totp_reset_evidence.go)、[受保护操作记录校验](../../internal/protocol/totp_reset_state.go)。不改登录入口、状态查询的权限，不自动移除多个因子、恢复因子或其他设备的新因子。
- 可信回执使用已有 schema 2 的 `business_receipt`；有实际匹配读取时才写 schema 3 的 `state_read` 及时间。不迁移、重写或降级旧 schema 3，缺少历史列表标记不推断“跳过”或“匹配”。新分支用独立尝试编号；中断后不据展示日志授权联网。只有已确认、可读回的私有操作证据和匹配凭据才允许本地完成。未声明旧可执行程序能恢复新增的第二次登记记录。
- [本地保存与退出门槛](../../cmd/tosub2/totp_reset.go)、[终态投影](../../internal/protocol/totp_reset_log.go)、[日志白名单](../../internal/resetlog/detail.go)、[日志面板](../../web/src/components/job-logs.jsx)。并发测试仍在测试文件里；产品使用原同账号互斥。存储预检失败网络为 0；待保存凭据写或读回失败，激活为 0。中断、归档、去重、历史缺字段和普通日志不泄露合成敏感标记均有回归。

精简复测命令（项目根目录；生成夹具的两步必须按顺序）：

```sh
export GOPROXY=off GOTOOLCHAIN=local TOSUB2_KEYCHAIN_TEST=0 TOSUB2_LEGACY_RUNTIME_TEST=0
D=$(mktemp -d)
go test -count=1 -v ./cmd/tosub2 -run '^(TestResetConsoleRequestLedger|TestResetStorageCheckpointsAndOfflineContinuation|TestResetCancellationAndInterruptedRestore)$' -reset-public-pages="$D"
go test -count=1 ./internal/console -run '^TestFrontendResetClipboard$' -reset-public-pages="$D"
go test -count=1 ./internal/console -run '^TestFrontendResetClipboardInBrowser$' -reset-public-pages="$D" '-reset-browser=/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'
go test -count=1 ./internal/protocol ./internal/jobs ./internal/application ./internal/console ./internal/resetlog ./cmd/tosub2
go test -race -count=1 ./internal/protocol ./internal/jobs ./cmd/tosub2 -run 'TestResetConcurrentReceiptsCommitOnlyOwnEnrollment|TestResetSecondEnrollmentCannotReplayOrDropFirstReceipt|TestReceiptOnlySuccessRequiresReceiptKeyAndNoListClaim|TestLogArchiveRetainsBeyondSummaryAndPagesAfterTrashRestart|TestResetStorageCheckpointsAndOfflineContinuation|TestResetCancellationAndInterruptedRestore|TestTOTPCompletionUsesConfiguredAccountLockWithoutNetwork'
go run ./cmd/build-web -check
```

**结果：** 39 个正常入口场景通过；48 份公开 API 视图经生产日志面板复制，4 份另经隔离 Chromium 对比通过。上述受影响包、定向 race、嵌入前端产物一致性通过。`internal/resetlog` 的白名单由 jobs 与入口测试覆盖，没有单独的包内测试。红测先观察到重复 GET、冲突后少登记一次、跨账号回包误提交；修复后无相关失败。一次浏览器测试因先于复制夹具生成而未能比较，按上述顺序重跑全部通过。未跑整仓 race、真实账号登录、系统私有凭据库、历史线上驱动、部署或无关的显式长耗时测试；新密钥是否未来仍有效、其他设备何时退出，均不在本地验收可证范围内。

## 按 11 月 17 日 HAR 复测：哪个 200 可以直接保存？

本轮离线核对了指定 HAR 的字段名与类型。**历史观察**：移除回包有 `message: "Factor disabled"` 和空的 `passkey_credential_manager_sync`；登记有 `email:null`、`factor.metadata:null`；激活回包只有布尔 `success:true`。**本地入口**：[假上游](../../cmd/tosub2/totp_runner_fixture_test.go)按这些形状生成全新的合成数据；没有读取 HAR 中的凭据值或重放请求。只有明确的首次登记拒绝才进入一次兼容处理，HAR 本身没有捕获这种拒绝。

下表为假上游实收次数，顺序为 **状态查询 / 移除 / 登记 / 激活 / 退出**。只有标明勾选退出的行才应有退出请求。

| 问题 | 本轮修前证据 | 修后行为与请求次数 | 界面复制日志（摘录） |
|---|---|---|---|
| 无旧因子；或只有本次可替换的旧因子 | 两条入口基线都已凭激活回执保存 | 补齐 HAR 可选字段仍为 `1/0/1/1/0`、`1/1/1/1/0`。后一情况勾选退出为 `1/1/1/1/1` | 网站已确认激活，新密钥已保存；未另查验证器列表 |
| 初登明确报已有，复查为空／得到本轮授权的旧因子 | 两个合成入口基线均正确 | `2/0/2/1/0`／`2/1/2/1/0`；只多登记一次，无新登录或激活后查询。第二次仍冲突就停 | 登记提示已有验证器，复查显示未启用；不发移除请求，再登记一次；或：已固定本轮旧因子，不更换移除目标 |
| **同一份激活回包既表示成功，又表示因子已停用**，复查仅见另一因子；勾选退出 | 合成函数测试误判成功；控制台红测 `1/0/1/1/1`，误提交、误退出 | `2/0/1/1/0`，只读复查一次，不重复激活，不提交，不退出。暂存材料仍与本轮绑定 | 回包同时表示激活和停用，不能作为成功凭据；仅核对状态，不重发修改（HTTP 200） |

这次改的是[回执分类](../../internal/protocol/totp_response.go)和[已有日志诊断](../../internal/protocol/totp_reset_log.go)，不是删除权限或重试次数。[矛盾回包的界面复制原文](totp-reset-mixed-signals-clipboard-2026-11-18.txt)来自隔离浏览器逐字比对的“本次／异常”视图。`A13` 收到了完整的 200，`A14` 是只读复查；终态保留 `last_write_issue=CONTRADICTORY_RECEIPT`、`last_trusted_step=pending`、`pending_readback=verified` 和 `logout=not_performed`。这不同于没有收到响应，也不同于已保存可用密钥。旧日志和凭据格式不变，不作迁移；只有矛盾回执的历史待提交记录不能仅凭它完成本地提交。

复用上节命令。追加的红绿定向用例：`go test -count=1 ./internal/protocol -run '^TestTOTPMutationResponseCompatibility$'` 和 `go test -count=1 ./cmd/tosub2 -run '^TestResetConsoleRequestLedger$/^activation-mixed-signals-logout$'`。本轮 40 个入口场景、49 份复制视图、5 份隔离 Chromium 剪贴板对比、受影响 Go 包、定向 `-race`、嵌入前端一致性均通过。未跑整仓 race、真实账号或部署；不推断线上所有旧密钥已失效。
