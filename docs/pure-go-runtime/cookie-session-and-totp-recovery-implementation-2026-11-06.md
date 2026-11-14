# Cookie 会话与 2FA 恢复实施记录

> 历史记录：本文描述当时版本，涉及 Cookie 持久化、登录 checkpoint 或远端恢复的能力现已移除。当前行为与清理范围见 [登录会话清理](authentication-session-removal-2026-11-08.md)。

> 全项目范围纠正（2026-11-08）：本文涉及的 AccountSession、Cookie 快照持久化、共享会话与 owner/dirty 机制也是后来新增，现已纳入整体清理；不能再把它们作为“原版普通会话”保留。当前边界见 [全项目清理记录](cookie-persistence-cleanup-2026-11-08.md)。下文仅作历史追溯。


> 历史记录（已被后续清理取代）：本文描述的独立核实、远端恢复、重认证或专项 Cookie 方案不再是当前功能。当前实现以 [设置与重置 2FA](totp-reset.md) 为准；保留本文仅供审计追溯。

> 历史实施快照：下文的会话发布、退出顺序和成功后核实描述不代表当前行为。当前临时会话采用本地退役，见 [当前重置说明](totp-reset.md)；成功确认规则见 [结果确认边界](totp-outcome-confirmation-2026-11-07.md)。

日期：2026-11-06。工作区：`/Users/ceng/Downloads/toSub2-go-port`。本记录对应同目录的 `cookie-session-and-totp-recovery-plan-2026-11-06.md`。源码已实现并通过回归，前端与候选程序已构建；未切换运行程序、重启服务或操作真实账号。

后续同日清理见 [Cookie 管理清理说明](cookie-management-cleanup-2026-11-06.md)：集中旧 wire 兼容边界，移除重复投影与未用接口，直接逐条使用标准 Set-Cookie parser。本文件中的测试结果和候选程序摘要属于本轮清理之前的实现批次；后续清理的最终验证在新文档单独填写。

## 实际行为

- 使用固定版本 `go.nhat.io/cookiejar v0.4.1` 的有限 vendored fork，保留来源、BSD 许可证与扩展说明。HTTP、TLS 与 Go 内脚本使用同一个 Jar。
- 会话准入不再识别 `__Secure-next-auth.session-token` 或 `.0/.1` 连续分片。名称和值保持不透明；库处理标准作用域、过期、删除、Secure、HttpOnly 与前缀规则。是否已登录由官方身份响应确认。
- 快照保存 session Cookie、绝对到期时间、作用域、创建序列、完整属性与 revision。业务 checkpoint 引用独立加密会话；账号会话不会随 workflow checkpoint 删除而消失。
- 收到响应头便保存 Cookie 轮换。disable/enroll/activate 的最多 64 KiB 响应先写入私有回执，再解析业务字段；已收到的完整或部分响应可随错误一并返回。
- 失败恢复始终绑定原 operation、账号、传输身份与会话。先只读确认官方状态，不自动重新登录、发邮箱验证，也不重放结果未知的移除、登记或激活请求。
- 账号身份读取阶段尚未完成就失败时，也保存准备会话；下次点击保留原 operation 和轮换 Cookie。
- 本地保存失败时，受限 owner 保留最新资料和账号 OS 锁，runner 结束不会丢失它。再次点击先 flush 并读回；失败期间不得发新网络请求。
- pending seed 必须可靠保存并读回才能激活。成功收尾绑定同账号、同 operation、同因子及登记材料摘要；单独的 HTTP success 或激活布尔标志不够。
- 顺序为核实 journal → 有效 vault → 保持门禁的 metadata → 独立会话发布 → 完成回执 → journal 持久删除 → 解除门禁。已核实记录可在 Cookie 过期时纯本地收尾。
- 启动仅恢复记录和操作入口，等待用户点击；不自动续接远端操作、不提交待恢复密钥、不发布会话或删除恢复记录。
- 原点击意图有持久去重回执。完成并重启后重放同一意图不会重新移除 2FA；新意图才表示新一轮操作。
- fresh reset 中用户明确选择的“退出所有设备”在新密钥核实、保存及原会话发布后执行；后续 Cookie 轮换仍更新同一会话。恢复和检查入口始终不附带退出、重新授权或上传。

## 主要代码

| 范围 | 位置 |
| --- | --- |
| Cookie 标准规则与完整快照 | `internal/third_party/cookiejar/`、`internal/transport/cookie.go` |
| 响应头屏障、部分响应与脚本共享 | `internal/transport/{http,tlsclient,response_limit}.go`、`internal/legacyruntime/client.go` |
| 独立账号会话、版本引用与交接 | `internal/protocol/account_session*.go`、`checkpoint.go` |
| 重置回执、owner、激活证据与续接 | `internal/protocol/totp_reset*.go` |
| 原子替换、目录同步、加密 envelope | `internal/privatefile/durable*.go`、`internal/credentials/state_protector.go`、`internal/privatestate/` |
| runner 与本地完成 | `cmd/tosub2/main.go`、`cmd/tosub2/totp_reset.go` |
| 启动投影、去重、门禁及界面 | `internal/jobs/totp_*.go`、`internal/console/totp_actions.go`、`web/src/lib/totpIntent.js` |

## 已覆盖的关键故障

1. 任意合法 Cookie 名称、多枚分片、session Cookie、绝对过期、删除、同名不同作用域、HttpOnly、Secure 与受限分区 Cookie。
2. 真实本地 TLS 服务先完成 disable 并返回轮换 Cookie，响应体卡住；父进程确认加密 journal 已保存头部后终止子进程。新子进程只用轮换后的原会话完成本次操作，disable/enroll/activate 的服务端计数各为 1，未访问登录接口。
3. enroll 响应头时制造磁盘写入失败，仍保留随后收到的完整 seed 回执；第二次点击 flush 失败时网络计数不变；磁盘恢复后从原材料继续，enroll 不再发送。
4. dirty owner 的真实 Unix flock 跨进程竞争：runner 结束、flush 失败均不释放锁；当前 runner 完成本地保存并退出才释放。
5. 激活已核实但 vault/metadata/session publication 失败，等待点击后补本地保存；启动不执行这些步骤。
6. 旧 journal 重现、旧 checkpoint 引用、同会话更高 revision、退出响应里的新 Cookie：不能回退或覆盖更新操作的会话。
7. pending seed 保存/读回失败、账号/因子/operation/证据不匹配、旧记录缺发送证据：禁止不具备证据的修改。
8. 截断、超限、取消、JSON 结构变化、服务端认证拒绝以及脚本捕获保存错误：保留可用证据，禁止错误后的隐式网络继续。
9. 启动读取 vault 期间 journal 消失、变成目录、损坏或被替换，以及明确 pending 但 journal 缺失：保持只读和隔离，vault 写入与文件清理均为零；普通密码恢复及已完成历史记录不受影响。

所有新增故障数据均为合成数据；密钥保护测试采用临时测试 provider 或平台调用 mock，不访问真实 Keychain。

## 验证与构建

| 验证 | 结果 |
| --- | --- |
| `go test ./... -count=1 -timeout=5m` | 全部通过，27 个有测试的包；包含 cmd、protocol、jobs、console、脚本构建检查。 |
| `go vet ./...` | 通过。 |
| transport、vendored cookiejar、legacyruntime 整包 `go test -race` | 通过。 |
| protocol owner、opaque/empty/expired candidate、新子进程崩溃恢复、准备会话、激活证据的定向 `-race` | 通过。 |
| runner fresh logout/后续 Cookie 轮换、dirty enrollment 定向 `-race` | 通过。 |
| jobs handoff/intent/logout 门禁与 public API 私有字段隔离测试 | 通过。 |
| 最后补充的 pending seed 回调前置要求及准备态恢复测试 | 通过；缺少可靠保存回调时零远端请求。 |
| `scripts/build-frontend.sh` | 通过；`web/dist` 与 `cmd/tosub2/web/dist` 逐文件一致。 |
| `CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' ... ./cmd/tosub2` | macOS ARM64 候选程序构建通过。 |
| Windows AMD64：`go test ./cmd/tosub2 -c ...` 与最终 `go build ... ./cmd/tosub2` | 测试二进制及最终程序交叉编译通过，未进行原生 Windows 运行。 |
| `git diff --check` | 通过。 |

候选程序：`bin/tosub2-cookie-recovery-20261106`。使用独立名称，不覆盖运行中的服务文件。

SHA-256：`722ab869bf7b02c26f9ead41b0993f7dc7990e9589677a931ef84165cd72b4b4`。

## 明确边界

- 远端已登记、完整响应从未到达客户端，或首次可靠同步前断电时，客户端不能凭空找回 seed/Cookie；保留未知状态，不重复未知写请求。
- 每笔私有 journal/账号快照上限 1 MiB；reset owner 与普通 session owner 各最多保留 64 笔。超限会保持阻断和最后完整受限副本，不能宣称超限更新已保存，也不能自动将旧副本当成最新状态。
- 内存 dirty 资料不能抵抗进程退出；界面会区分尚未可靠保存的状态。
- 标准 Cookie 到期或官方撤销原会话仍可能使恢复暂停。库替换不能重新生成已失效的会话，也不保证救回所有历史记录。
- 旧版读取支持不代表可以用旧二进制消费新加密格式。上线仍需停止提交来源、排空或停止操作、备份并切换；不能用旧备份覆盖已经发生的远端修改。
- Windows 的构建验证不等于原生 Windows 上的文件系统与 DPAPI 故障验收。
- 新的引用式 checkpoint 只能位于同一已注册输出根目录内；跨根自定义 checkpoint 路径会明确拒绝，不能跨目录静默读取会话。
