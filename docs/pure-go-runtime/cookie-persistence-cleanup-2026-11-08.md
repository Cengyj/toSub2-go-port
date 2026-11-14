# 全项目 Cookie 持久化清理

> 历史记录：本文描述当时版本，涉及 Cookie 持久化、登录 checkpoint 或远端恢复的能力现已移除。当前行为与清理范围见 [登录会话清理](authentication-session-removal-2026-11-08.md)。

## 原版基线与范围纠正

2026-11-08 核对用户指定仓库 `https://github.com/Cengyj/toSub2-go-port`：`git ls-remote origin HEAD refs/heads/main` 返回 `bb5259c2286556a6b827d2a68341b1f3d4ed43f4`，与本地 HEAD 一致。对照使用该提交源码，不把当前未提交实现当作原版。

此前 2026-11-07 的清理仅移除了 2FA 专项恢复，仍保留 AccountSessionStore 等后来新增的全项目 Cookie 持久化层，并把它误称为原有普通会话机制。该范围判断不符合用户要求，本记录予以纠正。

原版已经有任务自己的 login-checkpoint.json，包含 cookies、账号、登录阶段、OAuth 状态和传输绑定；显式 session 私有导出也会包含 Cookie。这些原版输出保留。原版没有共享 account-sessions 仓库、CookieRef 解引用、逐响应 Cookie 保存 hook 或 dirty-owner 生命周期。

## 本轮清理目标

- 全项目回到原版任务级 checkpoint 和当次内存 CookieJar；不增加共享会话仓库、自动迁移或恢复框架。
- 移除 AccountSessionStore、全局 cookie_ref/cookie_state、Cookie 专用 owner/lease/dirty、Cookie 保存屏障及所有共享会话运行器 fallback。原有账号互斥锁与 2FA 业务证据保存继续保留。
- 移除专门为该方案引入的 Cookie 快照引擎、持久化 API 和专用测试；保留与其无关的传输和 Cookie 安全修复。
- 保留 2FA 业务成功回执直接推进、未知结果当次有限核实、可靠密钥保存、真实确认来源、账号核对和防重复修改。
- 普通 OAuth continue/resume 不跨授权方式；2FA 的 ChatGPT 安全会话不得错误恢复 BPS 的 OAuth 进度。
- 不操作真实账号，不部署，不删除用户现有私有文件，不覆盖工作区其他未提交功能。

## 实际实施

| 层级 | 已完成改动 | 保留内容 |
| --- | --- | --- |
| transport / legacyruntime | 恢复原版 CookieJar 接口；删除快照、持久化 hook/barrier、dirty 错误和新增第三方 Cookie 引擎 | 当次共享内存 Jar、标准 Cookie 规则、JS HttpOnly 防护、完整/截断响应回执及有界网络重试 |
| protocol | 删除 account_session、account_session_auth、account_session_owner；取消全局 Cookie 引用、保存钩子与 session_candidate 续接死分支 | 原版 Checkpoint.Cookies、Flow.save/Restore、账号/origin/授权方式与传输绑定 |
| runner | 删除共享会话 Load/Save/Flush/Enable/Bind 和跨任务 fallback | 任务自己的 checkpoint、正常新登录、原账号互斥锁 |
| jobs / 本地完成 | 归档与本地完成不再经过 Cookie owner；使用普通 accountlock | 已确认激活证据、精确密钥读回、持久元数据提交、未知操作隔离 |
| 文档 / 测试 | 原专项测试移除或迁移；历史说明标记范围纠正 | 原版 OAuth 与凭据安全测试，本轮 2FA 成功/未知结果请求顺序和次数断言 |

原版的 authorization method mismatch 校验本来就存在，本轮没有删除它。普通 OAuth continue/resume 继续拒绝混用 ChatGPT 与 BPS。2FA 安全操作遇到同账号、来源与客户端有效的 BPS 登录进度时，使用正常的新 ChatGPT 登录，不导入旧 BPS Cookie 或 OAuth 状态；如果仍有 callback 或已接收 token 材料，则保留原文件并要求先完成原授权，避免覆盖唯一的一次性凭据。

## 数据与功能边界

- 清理的是源码中的新增 Cookie 体系，不会删除用户现存 account-sessions、旧 checkpoint 或私有 2FA 文件。它们不再被全局加载或迁移。
- 原版任务 checkpoint 和显式 session 导出依然可能包含 Cookie；“移除新增持久化方案”不等于禁止所有原版 Cookie 输出。
- 旧 cookie_ref/cookie_state checkpoint 只读拒绝；已确认旧 2FA 业务证据仍可只读用于本地保存，不能用于重新连接远端。
- 原版 token checkpoint、refresh grant recovery、凭据加密和账号互斥锁不属于 Cookie 专项体系，继续保留。业务证据的加密格式与历史 key reference 不改名，避免现有密钥或证据不可读。
- 没有重置工作树或覆盖无关功能。编辑前保存了源码快照；逐文件核对没有发现预期清理目录之外的改动。

## 第一轮验证

所有测试使用本地合成账号与响应，不使用 HAR 中的认证资料。

- `go build ./...`：通过。
- `go test ./... -run '^$'`：全部测试包编译通过。
- `go test ./... -count=1`：最终整仓全部通过，25 个有测试的包及 1 个无测试包。cmd/tosub2 89.728s、application 44.472s、console 96.295s、jobs 108.915s、protocol 110.506s；传输、legacyruntime 与其余包也全部通过。
- `go vet ./...`、`git diff --check`：通过。
- transport 与 legacyruntime 完整 `-race`：通过（12.656s / 13.264s）。
- 协议 2FA 与原 checkpoint 定向 `-race`：通过（24.693s）。
- 运行器本次新增 BPS→2FA 首登、未完成 OAuth 保护、旧共享仓库忽略、普通账号锁与本地完成用例 `-race -count=3`：通过（43.827s）。
- 旧激活业务证据本地完成用例 `-race -count=3`：通过（3.502s）。
- 前端源码及构建产物无需变更，两份 dist 仍逐文件一致。

首次整仓回归发现一条旧测试仍要求通过扫描旁边的恢复记录拒绝普通 Cookie checkpoint；该扫描已随框架删除。已将测试改为验证普通任务 checkpoint 保持可读且字节不变、旧业务 journal 保持只读、已确认密钥可靠完成且零网络请求；明确旧 cookie_ref/cookie_state 格式拒绝仍有独立测试。修正后重新运行整仓并全部通过，没有恢复旧扫描或放松未知 2FA 隔离。

最终源码扫描未发现 AccountSessionStore、CookieSnapshot、CookieRef、CookieState、Cookie 保存 hook/barrier 的生产调用，也未发现脚本或前端存在隐藏共享会话入口；少量旧格式字段仅用于拒绝读取。CookieJar、fingerprint 合并、challenge clearance、正常认证 Cookie 绑定、token checkpoint 和 session binding 已逐项与原版核对。

## 二次全项目复查（2026-11-08）

用户再次要求检查整个项目后，继续核对协议层、传输层、legacyruntime、CLI/服务启动、应用层、任务恢复与调度、HTTP 单个/批量入口、前端源码、两份 dist、依赖与构建脚本、文档，以及工作区已有二进制。第一轮删除了共享 Cookie 框架，但仍有下面的行为和死接口残留，不能仅凭名称扫描就断言已经没有问题。

### 新发现并修复的问题

| 位置 | 实际问题 | 本次处理 |
| --- | --- | --- |
| `internal/protocol/totp_reset_verify.go` 及三个调用文件 | 每次只读状态 GET 后仍执行旧 Cookie 方案留下的保存回调；尚未解释匹配的成功状态，就可能被无变化记录的保存失败阻断 | 删除 GET 后的重复保存参数和回调。真正的修改意图、`state_read` 确认来源、错误证据及凭据提交仍保存 |
| `internal/transport/http.go`、`read_retry.go`、`internal/protocol/flow.go` | Cookie 屏障删除后，`BeforeDispatchError` 和 `ExistingAccountOnly` 只剩无生产调用的接口、分支及专用测试；`hasTOTP` 也未使用 | 删除死接口和分支；将仍有价值的重试证据测试移到 transport，使用真实本地 HTTP 截断、拨号失败与取消路径 |
| `internal/console/server.go` | 单个操作拒绝 `reauthenticate`，批量请求却会静默丢弃旧字段，可能把旧恢复请求降级为新操作 | 对该旧字段按“是否出现”拒绝，`false`、`null` 也不能绕过；未全局收紧无关未知字段 |
| `internal/jobs/restore.go`、`discovery.go` | 首次设置的私有结果恢复硬编码默认 origin 取锁，可能与配置 origin 的远端操作使用不同锁 | 复用既有 `accountOperationLease`；服务在 Restore 前配置，discovery 在管理器锁内取得回调快照；未新增 Cookie owner 或会话锁框架 |
| `internal/jobs/restore.go`、`operation_state.go` | 首次设置和相邻密码恢复只凭 Save 返回 nil 就允许清理私有结果；旧凭据、旧成功元数据或旁路密码成功还可能掩盖失败 | 保存后读回完整凭据并精确匹配才允许清理；失败保留私有结果、明确本地保存未完成，不自动排队重放远端修改 |
| `cmd/tosub2/totp_workflow.go`、`internal/application/upload.go` | 两个仅测试使用的旧包装隐含默认 origin，增加绕开显式配置的入口 | 删除默认 origin 包装，六处测试迁移到既有显式 origin API；生产路径未改写 |
| 历史文档和注释 | 四处链接仍指向已删除的 Cookie 专项文件，部分注释仍描述旧恢复/扫描路径 | 链接改为明确的历史文件说明，更新注释；保留历史审计正文和作废提示 |

### 保留项的性质

- `recover-2fa`、`inspect-2fa` 和 `reauthenticate` 仅在拒绝旧请求、隔离旧排队记录时出现，没有可调用的远端恢复入口。`totpReauthenticate` 仅从旧输入中清除，不能用于恢复登录。
- `cookie_ref`、`cookie_state` 仅检查并拒绝旧 checkpoint 结构；不会加载 account-sessions、解引用 Cookie 或自动迁移旧会话。
- `complete-2fa` 在创建网络 transport 前进入本地完成流程，使用已经确认的激活业务证据、凭据保存/读回及元数据提交。它不是恢复远端登录。
- 只有未知结果才在当次已认证会话内进行有界状态 GET。明确业务成功仍直接推进或保存；核实结果仍绑定账号与本次因子，保留真实确认来源。
- 原版任务 checkpoint、内存 CookieJar、OAuth/token 的本地完成、普通账号锁、未知修改隔离均保留。`authorization method mismatch` 属于原版授权方式一致性校验，不能据此判断仍引用旧 Cookie 持久化方案。

### 前端、依赖、构建与文档

- 前端 58 个 JS/JSX/CSS 源文件均可从入口到达；源码及构建 JS 未发现旧恢复/核实动作或能力开关。`totpIntent` 只保存请求意图 ID，普通本地结果读取不是上游状态确认 GET。
- 两份 dist 各 4 个文件，逐字节一致；在临时目录重新构建后与当前 dist 的全部文件也一致，没有遗留旧 hash 文件或符号链接。
- 构建脚本、Makefile、Docker、CI 和依赖/许可文件没有旧 Cookie 库或重新拷回旧前端的路径。原版已有的未引用 CSS、历史阶段名称显示不属于该框架，未为消灭关键词而删除。
- 88 个 Markdown 文件的 367 个本地链接复核：本次涉及已删除文件的四处链接已处理；其余只有 `README-legacy.md` 原有的 `CHANGELOG.md`、`LICENSE` 两个缺失链接，不属于 Cookie 方案残留。

### 磁盘中仍存在的旧产物

以下是只读检查已有二进制固定符号的结果，没有运行二进制、读取账号资料或输出任意二进制字符串。不能把“当前源码已清理”表述成“磁盘和运行服务已全部替换”。

| 文件 | 观察结果 |
| --- | --- |
| `bin/tosub2-cookie-recovery-20261106` | 包含旧 AccountSessionStore / Cookie hook / 2FA 恢复符号 |
| `bin/tosub2-cookie-cleanup-20261106` | 包含上述旧方案符号 |
| `bin/tosub2-totp-disable-reauth-20261107` | 包含上述旧方案符号 |
| `bin/tosub2-totp-multiround-review-20261107` | 包含上述旧方案符号 |
| `bin/tosub2` | 未命中所查旧符号，但文件日期为 2026-11-05，也不是本次源码构建的版本 |

`scripts/run-production.sh` 直接执行 `bin/tosub2`；`scripts/local-macos.sh` 可以复用运行目录二进制，测试实例也可以复制已有 `bin/tosub2`。因此仅更新源码不会让现有运行实例自动采用修复。遵守本次“不自动部署”的限制，没有替换、删除或启动这些二进制，也没有切换或重启服务。用户已有私有历史文件同样没有删除。

### 本次验证

- 新增单个/批量首次设置/重置共 28 个旧 `reauthenticate` 输入案例，断言 HTTP 400、任务完全不变且没有入队。
- 新增 15 个私有结果恢复案例，覆盖 Save 假成功、部分字段丢失、读回失败、配置 origin 的真实锁冲突、旁路成功不能掩盖失败，并检查文件保留及后续本地完成。
- 协议测试区分“GET 后不应保存不变记录”和“真实确认必须保存证据”，断言请求顺序与次数；传输测试确保后一次拨号失败不能抹除前一次已发送证据。
- 上述 console/application/runner 定向测试通过；jobs 定向 `-race -count=3` 通过（7.308s）；协议相关 `-race` 通过（6.732s）；transport 完整 `-race` 通过（12.233s）。
- 本次全部源码改动完成后，`go test ./... -count=1` 全部通过：25 个有测试的包、1 个无测试包。cmd/tosub2 85.143s、application 41.506s、console 93.609s、jobs 104.589s、protocol 105.008s、scripts 58.360s；其余包也全部通过。日志：`/tmp/tosub2-residual-audit-tests-20261108.log`。
- `go build ./...`、`go vet ./...`、`git diff --check` 通过；最终文档补记后再次检查 diff，无空白错误。构建只做编译验证，没有输出或替换部署目录中的服务二进制。
