# 2FA 重置多轮严格审查与修复

> 历史记录：本文描述当时版本，涉及 Cookie 持久化、登录 checkpoint 或远端恢复的能力现已移除。当前行为与清理范围见 [登录会话清理](authentication-session-removal-2026-11-08.md)。

> 全项目范围纠正（2026-11-08）：本文涉及的 AccountSession、Cookie 快照持久化、共享会话与 owner/dirty 机制也是后来新增，现已纳入整体清理；不能再把它们作为“原版普通会话”保留。当前边界见 [全项目清理记录](cookie-persistence-cleanup-2026-11-08.md)。下文仅作历史追溯。


> 历史记录（已被后续清理取代）：本文描述的独立核实、远端恢复、重认证或专项 Cookie 方案不再是当前功能。当前实现以 [设置与重置 2FA](totp-reset.md) 为准；保留本文仅供审计追溯。

日期：2026-11-06。对象：当前工作区在[上一轮全流程审查](totp-reset-flow-review-2026-11-06.md)之后的进一步故障注入、交叉复核与集成验证。

本轮不把历史日志直接归因于某一代码缺陷：2026-11-05 的通用恢复错误本身不能证明远端移除失败，也不能证明 Cookie 格式不合法。以下问题均由当前代码的可达路径和合成故障测试确认。

## 审查方法和范围

1. 第一轮按协议请求、Cookie/账号会话存储、runner 跨步骤、控制台/调度/上传四个边界分别审查，先构造失败测试，再作最小修复。
2. 第二轮交换审查人，针对第一轮修改补充反例：早期已发送请求与最终本地拒绝、嵌套混合错误、历史 reset 后的正常 setup、拒绝配合取消的上传消费者。
3. 第三轮进一步核对多实例边界，区分进程内账号 lease 与操作系统账号锁，验证取消、清理以及正常历史流程。
4. 最后冻结生产源码，统一执行定向 race、全仓测试、静态检查、前端嵌入一致性和双平台构建。验收结果在文末记录。

全部测试只使用临时目录、合成账号、内存/测试保护器和本机假服务器。没有读取或修改真实账号 Cookie、真实恢复 journal、系统 Keychain/DPAPI；没有向官方发起重置或登录，也没有部署或重启现有服务。

## 第一轮确认的问题

| 问题及复现 | 修复及限制 |
| --- | --- |
| disable/enroll/activate 请求返回取消，随后的阶段保存也失败时，只返回保存错误，原请求 cause 被覆盖。 | 三处返回 `errors.Join(requestErr, saveErr)`，保留两种 cause。安全公开错误与私有分类证据继续分离。 |
| enroll 意图已经保存，但 `BeforeRequest` 或 Jar 持久化屏障在调用 transport 前拒绝请求；实际 `Do` 零调用，仍被记为可能已发送，无法执行第一次登记。 | 仅在两个真正的 pre-dispatch 边界产生封闭的明确未发送证据。用户明确继续后可以首次发送；任何响应后的保存错误仍属于已发送/未知。 |
| 独立账号会话第一次保存失败后，第二个已经在途的响应更新了唯一 Jar，但 dirty owner 丢弃新快照；以后 Flush 存的是较旧 Cookie。 | 同 lineage 的严格更高 revision 可以更新内存 pending，保留原 SessionID/expected。Flush 成功前仍阻断网络；跨操作、回退、同代不同内容及非法快照不覆盖候选。超限保留最后有界证据并保持阻断。 |
| reset 完成到自动 relogin 之间，另一实例可以提交新操作/新 seed，而旧 Manager 仍镜像旧 seed。红测中实际 runner 已越过边界发出两次合成认证 GET。 | 在重新取得 OS 账号锁、读取 vault 后，于保存凭据或联网前验证本轮 reset 操作 ID 与 seed。已变化则停止自动重登/上传，保留较新凭据。红测确认的是旧流程已开始认证，不声称实际复现了新 seed 被覆盖。 |
| 手动上传在预检查后等待远端列表时，同邮箱 reset 可以启动；关闭质量检测时没有 query lease，ids/raw 两路仍各发一次 POST。 | 共享上传 session 对已知账号全程持有可取消的账号 lease，质量检测关闭也一样。reset 排队取消上传；调度器必须等待消费者 Release 后才能执行 reset。质量检测开启复用同一 lease，未知外部账号保留原支持。 |
| 批量下载在选中任务后读取文件，期间 `ResultSaved` 被失效，仍返回已选旧授权。 | 对实际纳入导出的任务在读后再次检查当前保存状态，并再次核实账号恢复边界。 |
| A 任务的缓存文件实际装 B 的授权，只检查 A 的恢复标记，可以导出 B。 | 统一读取入口要求一条任务绑定一个规范化邮箱对应的账号；错绑文件拒绝下载和上传。单条下载保留原 bytes；批量/上传解码用 `json.Number` 保留大整数。 |
| 旧 Manager 缓存 `ResetTOTP=false`，另一 Manager 已写下验证完成 journal 并提交 vault、但尚未完成会话交接；旧门禁忽略 journal 并放行。 | 判断完成证据不再只依赖内存 ResetTOTP 标志；检查已知任务实际 journal。无 journal 的普通路径和旧 setup 历史保持兼容。 |
| 实际导出门禁在 vault 读取失败时当作“无待恢复状态”，可能在私有凭据仍隔离时放行。 | 实际 export/upload full gate 对非 NotExist 读取错误返回固定安全错误；不泄露 provider/path/secret。公开 UI 投影和纯本地 refresh 恢复不增加 vault 依赖；无本地记录的外部账号仍正常。共享上传入口也执行相同检查。 |

## 第二轮反例和交叉复核

- **累计发送事实不能被末次错误覆盖。** 真正发出 HTTP 的早期只读尝试失败后，下一次本地屏障拒绝，整个调用仍不能标为未发送。相反，全程明确未发送、随后被取消时应保留未发送证据。补齐 `preserveRetryEvidence` 和 `retryParentFailure` 对新封闭包装的处理。
- **混合错误必须逐分支判断。** pre-dispatch 包装内的 `Join(DNS timeout, unknown storage)` 不应借 DNS 超时获得重试许可；交叉审查使用临时 overlay 先复现，再补永久回归与纯 DNS 对照。
- **新门禁不能误伤后续 setup。** 历史 reset 的 handoff ID 会保留在任务上。测试用关闭/恢复 Manager 后重新排队 setup 复现新门禁误拦；现从 Manager 取得真实流程来源，仅 reset 比较 handoff ID，所有自动 2FA 重登均比较当前有效 seed。正常 setup 允许继续、已被替换的 setup seed 仍拒绝；独立 relogin 不受此约束。
- **取消不等于网络工作已停止。** 使用不配合取消的消费者，验证取消后的 lease 仍占用账号；其他邮箱可继续执行，同邮箱 reset 必须等 Release。
- **Cookie dirty 修复反向复核。** 更高 revision 仅在有效、同 lineage 且大小合法时替换 pending；原 expected 不变、非法候选不抹掉最后可恢复材料，明确 Flush 前不解除屏障。

已排除、未扩大修改的疑点：按既定耐久顺序清理 journal 后，可以用 metadata 与 vault 证明本地交接完成，不要求后来合法新会话仍保持旧 active；普通 AccountSession Flush 的同目录同步已由 record 写入覆盖，不等同于上一轮纯 handoff replay 的同步遗漏。

## 第三轮：跨实例互斥

第一轮的 query lease 只保护同一个 Manager。进一步用真实 OS 锁双向注入发现：上传检查已经通过且进程内 lease 为 Busy 时，另一个实例仍可取得相同账号的系统锁；反过来，另一实例先持有系统锁，上传检查仍通过。这说明多次重复检查只能缩小窗口，不能覆盖等待远端列表至实际上传之间的生命周期。

手动及监控上传现先取得进程内 lease，再取得与认证/reset 相同 origin 和规范化邮箱的系统账号锁；随后重新检查恢复状态，持有到消费者结束。取消不提前释放，cleanup 先释放系统锁、再释放会唤醒调度器的 query lease。质量检测加载 grant 失败也不自行释放借用的 lease，避免异常路径破坏释放顺序。未知外部账号的原行为保留。

永久回归用临时文件真实 `flock` 验证双向互斥、默认 origin/规范化身份、重复检查、取消、幂等 cleanup、锁取得失败及 grant 加载失败。

自动 workflow 也复现了相同窗口：认证步骤返回已释放系统锁，但后续上传只剩 Manager 执行权。另一实例先持锁时，原自动上传仍完成一次读和一次写；上传 POST 在途时，另一实例也能取得锁。现由 `UploadSavedAuthorizationAtOrigin` 在整个上传期间取得并持有系统锁，生产 runner 显式传入与 reset 相同的 `ChatGPTBase`。旧调用入口保留默认服务 origin。

自动上传取得锁后重新执行窄 `CheckTOTPWorkflowExport`：仅已验证/保存/交接完成、重新授权成功并已合法进入 upload 的自身 active reset，能够豁免“自身正在执行”这一标记；其他任务、prepared、dirty、completion journal、待恢复凭据和 vault 错误均继续阻断，当前 handoff 操作 ID 也须一致。锁取得失败或预检查拒绝保持确定零写的错误分类，不把它标为远端上传结果未知。

这一窄豁免通过 20 个状态场景验证，包括合法当前上传以及未完成退出、错阶段、未保存授权、同账号其他任务、待恢复资料和凭据读取失败；另用真实调度器跑通 relogin/setup/reset 三种流程，各自覆盖正常、认证返回后凭据被隔离、quality 检查后凭据被隔离，共 9 个集成场景。拒绝路径零 HTTP 请求、错误为确定未上传，退出时均释放锁。

## 全仓集成验收中修正的测试前提

首轮全仓验收发现以下旧测试前提与明确的新边界不一致。逐项复现后修正测试夹具及断言，没有为通过测试而放宽生产保护：

- 两个真实 HTTP 上传分类测试原来直接调用仍排队的 reset runner，没有 scheduler active ownership；改用真实 `Manager.SetRunner + Start` 驱动，继续验证上游拒绝与只读查询失败的确定结果分类。全组 18 场景 race 通过。
- 监控部分上传测试直接 `Submit` 再发送 succeeded 事件，留下了未消费的 pending 项；修正为真实已结束任务的队列前提，继续检查部分成功和不重复登录。
- 上传第三个修改请求期间排入新操作，现在按设计取消旧请求。断言改为保留已确认的部分结果、未确认响应为 uncertain，且旧上传不能清掉新操作；不再要求这种竞态固定返回 200。
- refresh sidecar 隐私测试使用了故意不可读的 vault。现先验证实际下载安全拒绝且不泄漏 sidecar，再换合成可读凭据库验证原有固定文件名/正常旧导出。另一组纯本地 refresh 恢复仍在 vault 不可读、零 vault 访问条件下通过。

## 保持的行为约定

- 重启只恢复记录、状态和按钮，**等待用户点击继续**。
- disable/enroll/activate/logout 结果未知时不重放；仅有封闭证据证明没有发送的路径才能继续首次发送。
- Cookie 请求选择、响应接收与恢复继续使用 `go.nhat.io/cookiejar v0.4.1` 的 vendored core；本轮没有恢复 Cookie 名称、`.0/.1` 分片连续性或业务字段合法性判断。项目仍负责快照、加密持久化、操作绑定和事务边界。
- 持久 Cookie 不等于服务端永远认可会话；会话失效时停止并保留恢复材料，不自动重新登录替代恢复。
- 磁盘持续失败且进程随后死亡时，未落盘的最后一份内存材料无法保证恢复；本轮没有用成功标记掩盖这一限制。

## 新增回归入口

- `cmd/tosub2/totp_strict_runner_review_test.go`
- `internal/protocol/totp_strict_protocol_review_test.go`
- `internal/protocol/totp_strict_storage_review_test.go`
- `internal/transport/preflight_retry_review_test.go`
- `internal/jobs/totp_strict_boundary_review_test.go`
- `internal/jobs/totp_strict_workflow_export_test.go`
- `internal/console/totp_strict_boundary_review_test.go`
- `internal/application/account_quality_lock_review_test.go`
- `internal/application/totp_strict_workflow_upload_test.go`

## 最终验收与候选

最终测试使用 Go 1.27.3。为避免测试创建用户缓存锁文件，Go 测试的 `-overlay` 仅把 `internal/accountlock/accountlock.go` 中 `os.UserCacheDir()` 的目录定位替换为本次临时目录；系统文件锁、业务状态机和测试代码均保持原实现。静态检查和候选构建直接使用未 overlay 的生产源码。

| 检查 | 结果 |
| --- | --- |
| 冻结后的全部新增 Strict 定向 race | 通过：cmd 3.114s、protocol 5.420s、transport 1.389s、jobs 6.753s、application 5.215s、console 2.743s。命令：`go test -overlay <temporary-overlay> -race ./cmd/tosub2 ./internal/protocol ./internal/transport ./internal/jobs ./internal/application ./internal/console -run '^TestStrict' -count=1 -timeout=3m`。 |
| 全仓测试 | 修正旧测试前提后，`go test -overlay <temporary-overlay> ./... -count=1 -timeout=5m` 全部通过：cmd 126.938s、protocol 138.946s、jobs 119.459s、console 99.217s、application 32.903s；其余包全部通过。 |
| 静态、差异及前端嵌入检查 | 最终测试夹具修正后，未 overlay 源码的 `go vet ./...`、`git diff --check` 以及 `diff -qr web/dist cmd/tosub2/web/dist` 全部通过。本轮没有前端源码变更。 |
| 双平台构建 | 未 overlay 的生产源码使用 `go build -trimpath` 构建 macOS arm64 候选；`CGO_ENABLED=0 GOOS=windows GOARCH=amd64` 交叉编译通过。Windows 仅做编译验证，未执行 Windows 程序。 |

上一轮 `bin/tosub2-totp-audit-20261106` 保持原样，已复核 SHA-256 仍为 `5d1eedc382e51e4a0fb61c6f01e3d9ca7e485201f38ed005d4fa6a9a6981f260`；它不包含本轮完整修复。

新独立候选：`bin/tosub2-totp-hardening-20261106`。SHA-256：

```text
82f77b919fa44828f2262f9842fa0bce1c6b5f9c9c898d25e9aaeee868830b81
```

Windows amd64 临时交叉编译产物的 SHA-256：

```text
c2a6f33007de865ecf284473b209fa52e5f5b499cc4dec2258eeb7049bc16fef
```

本次验收日志保存在临时目录 `/var/folders/08/gy1fn1f90kd0t95s02d35mk80000gn/T/tosub2-strict-final-yt3f83zy`，包括 `race.log`、首轮 `full-test.log`、修正测试前提后的 `full-test-final.log` 和仅改锁缓存目录的 `overlay.json`。

本轮源码、测试和报告已完成；候选未替换现有运行程序，未启动或重启服务，未对真实账号做重置或登录验证。
