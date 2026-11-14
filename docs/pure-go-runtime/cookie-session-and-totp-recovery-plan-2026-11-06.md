# Cookie 管理与 2FA 重置续接方案

> 历史记录：本文描述当时版本，涉及 Cookie 持久化、登录 checkpoint 或远端恢复的能力现已移除。当前行为与清理范围见 [登录会话清理](authentication-session-removal-2026-11-08.md)。

> 全项目范围纠正（2026-11-08）：本文涉及的 AccountSession、Cookie 快照持久化、共享会话与 owner/dirty 机制也是后来新增，现已纳入整体清理；不能再把它们作为“原版普通会话”保留。当前边界见 [全项目清理记录](cookie-persistence-cleanup-2026-11-08.md)。下文仅作历史追溯。


> 历史记录（已被后续清理取代）：本文描述的独立核实、远端恢复、重认证或专项 Cookie 方案不再是当前功能。当前实现以 [设置与重置 2FA](totp-reset.md) 为准；保留本文仅供审计追溯。

> 历史方案存档：下文“成功后核实”的步骤不是当前强制门槛。当前已支持完整业务回执直接提交，见 [结果确认边界](totp-outcome-confirmation-2026-11-07.md)；本轮不继续扩展 Cookie 持久化或恢复框架。

日期：2026-11-06（Asia/Shanghai）。状态：方案已实施并通过回归，尚未部署。具体改动、验收结果、候选构建与限制见同目录 `cookie-session-and-totp-recovery-implementation-2026-11-06.md`。下文的“当前代码/缺口”保留为实施前审查基线；源码能力不代表运行中的版本已经切换。本次未重启服务、未操作真实账号。

用户已确定：程序重启后恢复未完成记录和操作入口，等用户点击继续；不在启动时自动续接远端重置。

本方案更新此前“识别单段/分段会话 Cookie”的设计方向：后续实施应移除这类业务格式准入，保留标准 Cookie 规则、实时官方身份核实和已有副作用恢复证据。

## 1. 目标行为与判断

目标是让一笔重置从头到尾拥有同一个操作身份和可恢复会话。用户再次点击时，应读取原记录、复用原会话、核实官方当前状态，然后从已经完成的步骤之后继续。

针对本次描述的场景：

1. 使用旧 2FA 登录成功，确认当前账号，完整保存原会话及重置准备记录。
2. 保存“移除请求可能发出”的记录，再向官方发送移除请求。
3. 官方已移除，但响应超时、解析失败或本地保存异常：保留原会话、请求证据和已经收到的响应，不退回登录流程。
4. 存储正常时，在同一次操作中有界地只读核实；存储异常先暂停远端访问并保留最新内存资料。
5. 再次点击时先解决本地未保存资料，再用原会话确认是同一账号。若官方确认旧 2FA 已停用，且有可靠证据表明新密钥登记尚未发送，直接进入新密钥登记。
6. 新密钥和登记身份可靠保存后才激活；核实本次新因子已启用后提交有效密钥，完成本地清理。

Cookie 的名称、值、分片后缀是服务器自己的协议内容。`.0`、`.1` 应作为两枚普通 Cookie 独立保存和发送，不拼接、不解码、不判断分片连续性，也不要求某个固定名称存在。官方以后更换名称或分片方式，不应触发本地“登录凭证不合格”。是否已登录，由原会话访问官方接口的结果判断。

库仍需处理标准 Cookie 规则：域名、路径、Secure、过期、删除、同名不同作用域、HttpOnly。将已经过期的 Cookie 排除出请求属于正常 Cookie 管理；它不能被扩大解释为“所有登录凭证失效”或触发恢复流程自动重新登录。

这项修改同时涉及 Cookie 管理、保存时机和重置续接。仅替换 Cookie 库，不能解决“远端已修改，本地不知道走到哪一步”的问题。

## 2. 当前代码已经具备什么，以及缺口在哪里

| 位置 | 已核对的事实 | 本次处理 |
| --- | --- | --- |
| [cookie.go](/Users/ceng/Downloads/toSub2-go-port/internal/transport/cookie.go:119) | 目前是项目自写 Jar；HTTP、TLS、脚本环境共享它。`Cookies()` 是过滤过期后的集合。 | 保留单一 Jar 架构，将规则引擎换成经过审查的库，增加真正完整的快照接口。 |
| `session_cookie_candidate.go:60`（历史文件，已移除） | 固定识别 `__Secure-next-auth.session-token` 及数字分片，并检查连续性、数量、大小。 | 从会话准入和登录成功判断中移除；不继续增加服务器私有格式规则。 |
| [recovery.go](/Users/ceng/Downloads/toSub2-go-port/internal/protocol/recovery.go:82)、[flow.go](/Users/ceng/Downloads/toSub2-go-port/internal/protocol/flow.go:549) | Cookie 形态参与是否复用会话的决定；通用准备路径在判定不可复用后可能调用 `LoginWeb`。 | 待恢复操作必须进入只使用原会话的执行路径；普通登录完成也以实时会话结果确认。 |
| `totp_reset_session.go:117`（历史文件，已移除） | 恢复读取器以固定 Cookie 形态作准入。 | 读取器只核对私有记录、账号、操作和传输绑定，不用 Cookie 名称决定能否查询官方。 |
| [HTTP](/Users/ceng/Downloads/toSub2-go-port/internal/transport/http.go:232)、[TLS](/Users/ceng/Downloads/toSub2-go-port/internal/transport/tlsclient.go:237) | 收到响应头即更新内存 Cookie；上层持久化通常要等请求返回。 | 接收头时增加同步保存屏障，覆盖 body 卡住、截断、取消之前的 Cookie 轮换。 |
| [totpResetRequest](/Users/ceng/Downloads/toSub2-go-port/internal/protocol/totp_reset.go:333) | JSON 解析失败时不保留可重读的原响应；部分传输错误也会丢掉上层可见 body。 | 增加有限、私有、先保存后解释的变更响应回执。 |
| [resetDispatch](/Users/ceng/Downloads/toSub2-go-port/internal/protocol/totp_reset_state.go:159)、`ResumeTOTPReset`（历史接口，已移除） | 已有发送前证据、同会话只读核实、不重放未知请求、准备态续接。 | 继续使用并补齐入口、存储与异常分支，不另起一套重置引擎。 |
| [本地收尾](/Users/ceng/Downloads/toSub2-go-port/cmd/tosub2/totp_reset.go:136) | 新因子严格核实后，可不依赖 Cookie 完成本地提交。 | 保留，补齐持久化顺序和删除语义。 |
| [vault 写入](/Users/ceng/Downloads/toSub2-go-port/internal/credentials/store.go:540)、[checkpoint 写入](/Users/ceng/Downloads/toSub2-go-port/internal/protocol/checkpoint.go:113) | 文件 Sync + Rename，未统一覆盖父目录同步；journal 自身已有目录同步。 | 统一持久替换/删除原语，区分失败与持久化结果不确定。 |

`old_factor_invalidated` 目前表示本地隔离旧密钥，不是官方已经移除的证据。公开状态应明确区分这两件事。

本次错误文本只能证明程序暂停，不能仅凭它证明官方已移除旧因子，也不能证明 Cookie 文件写坏。`CODEX_AUTH_LOGIN_REQUIRED` 表示某次授权路径要求登录；它与重置暂停有关联的可能，但不是 Cookie 损坏的直接证据。

## 3. Cookie 库选型与接入边界

### 3.1 选型结论

优先采用 **`go.nhat.io/cookiejar v0.4.1` 的小范围维护分支**，上游提交 `9643ce7184897889c72ec568776719ee8bf72e34`：使用其基于 Go 标准库的 Cookie 规则引擎，补充快照与脚本访问接口；由项目已有私有存储层负责原子持久化。实施时固定上游 tag、commit、补丁和许可证，并保留清晰的上游升级路径。

这不是“加一个依赖就自动具备可靠恢复”。该版本的默认持久化路径还不满足本项目要求，必须明确维护以下有限扩展，并在接入前通过对照实验。若扩展无法限制在这些边界或基础兼容性不过关，退回直接基于项目 Go 1.27.3 标准库的相同快照扩展；不采用两套独立 Jar 拼接持久化的做法。

| 候选及核查版本 | 结果 |
| --- | --- |
| [Go `net/http/cookiejar`](https://pkg.go.dev/net/http/cookiejar)，项目工具链 Go 1.27.3 | 标准规则基础成熟，支持公共后缀列表；公开 API 没有完整导出/导入。`Cookies(url)` 只有请求需要的 Name/Value/Quoted，不能拿来无损保存全部 Cookie。作为规则基线及备选。 |
| [go.nhat.io/cookiejar v0.4.1](https://github.com/nhatthm/go-cookiejar/tree/v0.4.1) | 单一内部 entries，已有全量序列化结构，可保留 session Cookie；Go 1.25 起，兼容项目声明。默认 `Sync` 先截断旧文件，加载错误仅记录日志；导入导出漏 `Quoted`，`SameSite=None`、Partitioned 需补齐。适合作有限扩展后接入。 |
| [juju/persistent-cookiejar v1.0.0](https://github.com/juju/persistent-cookiejar/tree/v1.0.0) | 基础实现较旧；默认序列化排除 session Cookie，`AllCookies` 明确不能重建全部内部状态；Save 非原子。补齐现代语义成本较高。 |
| [orirawlings/persistent-cookiejar v0.3.2](https://github.com/orirawlings/persistent-cookiejar/tree/v0.3.2) | 增加保存 session Cookie 选项，但仍继承旧属性与文件截断保存等问题，本次不采用。 |
| [tls-client v1.16.0 CookieJar](https://github.com/bogdanfinn/tls-client/blob/v1.16.0/jar.go) | `GetAllCookies` 不能代表符合本项目要求：自己的 host 桶/名称去重会丢失作用域区别，发送路径存在绕开底层规则的问题；不将当前 TLS 传输依赖直接当持久化 Jar。 |
| [fhttp v0.6.9 cookiejar](https://github.com/bogdanfinn/fhttp/tree/v0.6.9/cookiejar) | 无公开完整快照，Cookie 类型与 `net/http` 不同，缺分区属性；不带来足够接入收益。 |

`go.nhat.io/cookiejar` 使用 BSD-3-Clause，分发时保留其及继承的 Go 源码许可证。其近期更新不等于完整覆盖浏览器语义；本方案的选择依据是规则来源、唯一内部状态、可审查的扩展边界及验收结果。

已做的验证：该版本 `jar.go`、`punycode.go` 与本机 Go 1.27.3 源码逐行比较，除 ASCII 包导入路径和 import 分组外一致。仓库外独立模块的 9 个 API 实验通过，确认了上述 session Cookie 保存、作用域、绝对期限及属性丢失/静默空 Jar 等现状；这些实验没有实现本方案，也不能替代修改后的验收。公开证据：[版本与提交](https://proxy.golang.org/go.nhat.io/cookiejar/@v/v0.4.1.info)、[Jar 内核](https://github.com/nhatthm/go-cookiejar/blob/v0.4.1/jar.go)、[默认持久化及 Entry 映射](https://github.com/nhatthm/go-cookiejar/blob/v0.4.1/persistent_jar.go)。

### 3.2 只有一个运行时 Cookie 状态

每个账号操作拥有一个 Jar，HTTP、TLS、重定向、Sobek 的 `document.cookie` 全部访问同一实例。TLS 客户端不再额外开启隐式 Jar，也不维护独立“持久化镜像 Jar”。

保留 `transport.CookieJar` 作为项目接口适配器，内部由选定库管理唯一 entries。有限扩展为：

- `Snapshot()`：锁内复制全量状态，包含 session Cookie、Persistent、HostOnly、HttpOnly、Secure、SameSite、Quoted、绝对到期时间、创建/排序信息及 SeqNum/nextSeqNum、PartitionKeyOpaque 和分区元数据；分区键也深复制，快照是不可变的序列化输入。
- `Restore(snapshot)`：显式返回错误；核验快照格式后一次恢复同一实例，不通过 `Cookies(url)` 推导，不把旧文件与新状态盲目合并。
- `DocumentCookies(url)` / `SetDocumentCookie(url, value)`：共享同一 entries 和锁；脚本不能读取、创建、覆盖或删除 HttpOnly Cookie，不安全页面不能覆盖 Secure Cookie。URL 由宿主可信 realm 提供，不能信任脚本可写属性。项目已有标准前缀保护继续有效。
- 将 `SameSite=None` 和 `Quoted` 纳入保存/恢复；保留 Partitioned 与 PartitionKey。没有浏览器顶层站点上下文时，分区 Cookie 保持不可发送，绝不降级成普通 Cookie。SameSite 元数据完整保存不等于已经实现浏览器全部跨站策略。
- 显式配置项目已有 `publicsuffix.List`，不能使用默认 nil。保留 HTTP、脚本、恢复入口的 `__Secure-` / `__Host-` 标准前缀约束；所选内核不会自动执行这些浏览器接收规则。Quoted 同时用于 HTTP/TLS 的正确请求序列化。
- 安全折算超大 Max-Age，沿用明确的上限/饱和策略，避免 duration 乘法溢出改变有效期。补充隔离试验已确认所选库原实现有此边界；不能在替换时丢掉当前解析器的保护。

应用整批 `Set-Cookie` 后，在同一锁内产生会话 revision 与对应不可变快照，避免多枚 Cookie 被存成半批。释放锁后把这对值交给持久化回调，不能回调中事后再抓另一个时刻的 Snapshot。所有会话写入由该操作的 owner 串行协调，不能让旧 revision 覆盖新 revision。

`document.cookie` 写入同样经过 revision/dirty/保存屏障；桥接层需能传播保存失败，或阻止该 owner 随后的 fetch，不能只有 HTTP 响应才能触发保存。

标准解析继续使用 Go HTTP Cookie 解析器，域名/path/覆盖/删除/公共后缀规则交给库。未知业务 Cookie 名称和后缀直接接受。标准属性解析失败与“认证已失效”分开处理，不恢复已被服务器删除的值。

### 3.3 保存语义

- 无 Expires 的 session Cookie 也保存，以实现程序重启后的会话续接。恢复不意味着服务端保证它继续有效。
- `Max-Age` 只在接收时折算一次绝对到期时间；重启不能重新开始倒计时。已过期 Cookie 不发送、不延寿。
- 同名不同 Domain/Path 的 Cookie 分开保存；有分区键时也属于不同作用域。只按名称去重不可接受。
- 活跃 Cookie 与最近一次过期/删除的最小诊断分开记录；诊断不无限保留旧值，也不能成为恢复旧 Cookie 的来源。
- 新增独立的账号会话存储语义，复用相同加密 codec 和持久替换 helper；它只保存可重用的会话与传输绑定，不包含 OAuth 一次性 code/state、seed 或响应回执。普通登录的流程 checkpoint 仍保留自己的中间步骤；登录或导出成功时清理流程 checkpoint，不能顺带清理账号会话。
- 新写普通流程 checkpoint 保存 session 引用和流程步骤，不再维护第二份可独立演进的 Cookie；旧 checkpoint 的 Cookie 只通过兼容读取器迁移。恢复中间流程时先核对其 session 归属和 revision，不能盲目合并旧 Cookie。
- 进入重置前将最新完整账号会话转入重置记录并可靠提交。重置期间以同一个恢复 envelope 为权威持久记录，账号会话文件暂停更新和使用，不能形成两套并发权威。
- 重置成功后也保留同一会话的最新 Cookie；先按第 7 节交接回账号会话存储，再删除恢复记录。原会话表示持续接受服务器轮换和删除的 Jar，不表示永远固定第一次登录拿到的值。
- 能否复用会话不由 Cookie 数量或名称决定。可读取完整且绑定正确的本地记录时，允许原会话进行有界只读身份核实；即使某个熟悉的 Cookie 不存在，也不切入登录。

## 4. 同一操作的恢复记录

沿用现有每次重置的私有 journal，升级为带版本的 recovery envelope；不新增数据库，不把 Cookie、响应和操作进度拆成多个必须同时提交的文件。

记录包含：schema、operation ID、账号绑定、原传输身份、单调 revision、Cookie snapshot、现有 phase/dispatch、原密钥基线绑定、原因子 ID、各步骤有限回执、新登记材料、最新只读观察及保存状态。

继续使用已有 `not_sent / proven_unsent / may_have_been_sent` 和核实证据。逐步改用有类型的内部结构和统一状态验证；旧字段由单向投影生成，避免 `status`、`phase`、`attempted` 各自成为互相冲突的事实来源。界面可压缩成准备、移除、登记、激活、提交、完成六个宏阶段，但不能以宏阶段替代发送证据。

错误只说明为什么暂停，不抹掉上次可靠阶段。记录必须分清：

1. 请求是否可能已经发出；
2. 服务器这次实际返回或观察到什么；
3. 哪些内容已经可靠落盘，哪些仅存于当前进程。

响应体变化可以导致“目前无法解释”，不能变成“请求没有发送”。字段缺失不能默认成 false/not_sent。

## 5. 响应接收与持久化顺序

### 5.1 修改之前

确认持久存储和保护密钥可用，获取已有账号排他锁；核对原账号与唯一原因子，可靠保存 prepared 记录并读回。按现有逻辑完成 vault 与 operation 绑定。每次修改前先将该步 `may_have_been_sent` 可靠落盘，再调用传输。

修改请求保持单次发送语义：不能因挑战、重定向、响应读取失败或状态暂不可见而自动重放。只有当前控制流或传输提供可靠的“确未发送”证据，才允许将该次记为 proven_unsent。崩溃后缺少响应不是未发送证据。

### 5.2 收到响应头时

1. 接受整个 `Set-Cookie` 批次，更新唯一 Jar。
2. 将最新 Cookie revision、当前发送证据、HTTP 状态和请求步骤绑定写入同一个 envelope；不等 body 读取完成。
3. 保存成功，才允许进入后续重定向或新请求。
4. 保存失败时，远端请求可能已经成功，不能丢弃已收到的资料；标记 durability blocked。

即使第 2 步失败，仍可在**当前请求剩余时限和大小预算内**读取同一响应，以尽量保住 enroll 返回的新密钥；不得发下一请求、跟随重定向或重试。明确取消仍要尊重，只保留已经收到的内容。

### 5.3 收到响应体时

对 disable/enroll/activate 保存有限私有回执，再解析业务结构。回执包含步骤/attempt 标识、HTTP 状态、必要的响应类型信息、解码后的原 body、是否完整以及中断原因；不保存任意响应头、完整请求 URL 或所有网页正文。

建议这些变更响应的 decoded body 上限先设为 64 KiB，envelope 总上限沿用 1 MiB，并通过合成及已脱敏样本验收；超限明确报 `response_too_large`，保留前缀和截断标记。上限是资源保护，不是固定 Cookie 名称或分片数量限制。partial 回执不自动当成完整成功响应，也不能据此直接激活。

例如 enroll 返回字段层级变化，完整回执已保存后可由更新后的解析器离线重读；恢复原登记材料后继续同一 operation。未知结构不能靠猜字段来认定成功，不能重新发 enroll 覆盖原登记。

### 5.4 保存失败后的内存保留

由应用层在 manager 生命周期内为未完成操作保留 owner，持有最新不可变快照与 dirty 回执；runner 返回不能销毁它。存在 dirty 数据时，跨进程账号锁的所有权也必须移交 owner，直到 flush 成功或进程退出，不能随着 runner 的 defer 释放；否则第二个进程可能用旧磁盘记录覆盖尚未保存的资料。owner 不持有已失效的请求 context。

发送变更前为该操作预留受限回执和快照的内存预算；预算不足拒绝开始新操作，不能淘汰尚未落盘的新 seed。续接由同一 owner 使用既有锁，避免自己重新获取锁造成死锁。

下一次点击首先以有界本地保存 context 执行 flush、读回和绑定核对，成功之前不访问网络。磁盘正常恢复后才能核实官方并继续。dirty 数据不能从磁盘旧快照反向覆盖，也不能被另一次新任务替换。

dirty 内存不能抵抗进程退出。UI 必须明确显示“最新会话/回执尚未保存”，不能继续宣称这些新内容已安全保存。

### 5.5 私有内容保护及落盘原语

重置期间的 Cookie、原 enroll 回执、seed/session ID 都属于敏感恢复内容，只进入该次加密 envelope，不进入公开任务元数据、日志、错误字符串或前端。仅为恢复及账号会话快照增加窄范围 Seal/Open 适配，复用当前 AES-GCM、Keychain/DPAPI 保护能力，绑定 schema、用途、operation 和账号为 AAD，每次使用新 nonce；不修改原 `.cred` 格式，不做全项目加密迁移。

在发送任何变更前完成密钥准备和加解密读回测试；安全持久存储不可用就不开始重置，也不降级为明文。现有不支持持久凭据的平台限制继续明确表达。

未完成 envelope 存在时不得删除或替换其唯一解密密钥；密钥服务暂不可用、AAD 不符或密文损坏时保留文件并显示恢复受阻，不能当作“没有恢复记录”创建新 reset。

统一私有存储替换/删除 helper：同目录临时文件 → 私有权限 → 写入 → 文件同步 → 原子替换 → 父目录同步 → 读回核对。替换成功但目录同步失败，返回“持久化结果不确定”；不能声称原文件肯定没变化。Unix 和 Windows 分别实现、验证相应语义，不把不支持的同步静默包装成完整掉电保证。

不会声称消灭全部故障窗口：远端已轮换/登记、响应未到达客户端，或已经接收但首次可靠同步尚未完成时突然掉电，仍可能丢失新 Cookie 或 seed。此时继续保留最近可靠证据，准确说明缺什么，不伪造成功或重复未知写请求。

## 6. 核实与续接规则

恢复执行器只拥有“复用原会话”能力，不能调用 LoginWeb、resetExpiredSession、邮箱验证或自动重新授权。所有 reset/recover/inspect 入口及后台修复都通过同一待恢复检查；重复点击返回原 operation，不创建新操作绕过它。

只使用当前代码已有的官方路径：网页授权上下文、`/api/auth/session`、`/backend-api/accounts/mfa_info`，以及原 disable/enroll/activate 接口。不假设存在幂等键、找回遗失 seed、验证旧 seed 仍有效等官方接口。

身份接口必须确认原账号，因子接口必须提供可明确解释的状态。身份不明、另一个账号、另一个因子、相互矛盾的数据不授权修改。真实服务器身份核实与“按 Cookie 名猜测登录成功”是两个不同职责。

| 已保存的证据 | 原会话实时观察 | 下一步 |
| --- | --- | --- |
| 所有变更明确未发送，准备绑定完整 | 仍是记录中的唯一旧因子 | 完成准备并首次发送 disable。 |
| disable 可能已发出；enroll、activate 明确未发送 | 已明确停用 | 保存读取证据，直接进入首次 enroll；不再 disable。 |
| disable 可能已发出 | 仍见旧因子 | 有界只读等待；仍不确定则暂停，不重复 disable。 |
| enroll 明确未发送 | 已明确停用 | 首次 enroll。 |
| enroll 可能已发出，材料缺失或回执不完整 | 即使仍显示停用 | 先尝试离线重读完整已存回执；仍缺材料则暂停，不再次 enroll。停用不证明没有待激活登记。 |
| 完整新 seed/session ID/因子 ID 已保存，activation 明确未发送 | 仍明确停用且无冲突 | 先确保 pending seed 持久保存并读回，再首次 activate。 |
| activation 可能已发出，材料完整 | 本次保存的新因子已启用 | 保存核实证据，进入本地提交；不再次 activate。 |
| activation 可能已发出 | 仍停用或暂不可见 | 有界只读核实；超限后保留同一登记，等待下次点击。 |
| 已持久保存同账号只读核实到本次 enrollment 新因子的证据及匹配材料 | Cookie 过期、缺失或网络不可用 | 继续核对同 operation/seed；无冲突时无需访问官方，仅完成本地提交/清理。HTTP `success:true` 单独不满足此条件。 |
| 任一阶段 | 明确认证拒绝/要求重新认证 | 暂停并显示官方要求；保留资料，不启动新登录。 |
| 任一阶段 | 超时、5xx、未知结构、身份或因子冲突 | 分类显示原因；不当作 Cookie 损坏，不授权未知修改。 |

保留当前最多 4 次状态观察及 0.5/1/2 秒间隔作为初始策略；与底层只读网络重试共享总请求时限，避免嵌套放大重试。只有只读请求允许这种重试。用户取消后停止后续网络，只做有界的本地资料保存。

远端“已停用”的读取证据允许继续尚未发送的 enroll；它不允许重发历史上可能已经发送的 enroll。这是本次六步场景可以恢复与真正缺失新密钥场景必须停下的分界。

## 7. 新密钥提交与清理

顺序固定为：

1. 保存完整登记回执与经过验证的 seed、session ID、因子 ID。
2. 同一 operation 的 pending seed 写入 vault，可靠同步并读回；失败不激活。
3. 保存 activation 发送边界后发请求，并通过只读状态确认本次新因子。
4. 把严格核实证据写入 journal 并持久化。
5. 新 seed 提交为 vault 有效值，可靠同步、读回并核对 operation。
6. 提交凭据状态元数据，但保持会话操作的交接门禁；从 journal/dirty owner 中的最新权威快照持久发布账号会话候选，保留 session ID、同 operation、revision 和传输绑定。重启后的新建空 Jar 不是发布来源；不通过新登录重新生成 Cookie。
7. 写入持久完成回执，journal 标记本地完成，再持久删除恢复记录。完成回执只含去重和归属所需信息，不含 Cookie/seed。

会话交接采用可重入的发布顺序，不要求两个文件同时原子提交：候选持久写入并读回 → journal 记载已发布的 session ID/revision/摘要 → metadata 持久提交会话引用及完成回执 → journal 完成并持久删除。metadata 的活动会话引用保存 session ID、来源 operation 和交接 revision 下限；候选仅在引用匹配时被使用，不能仅凭 journal 不存在就接管。完成后的常规 Cookie 更新仍在同一会话文件原子保存，revision 单调增加，不必每次改动 metadata；另一个 session 的接管必须重新发布引用。

交接未完成时维持账号锁和 `session_publish_pending` 门禁，不能随现有 CommitTOTPReset 解除凭据隔离就放行其他会话操作。未完成 journal 是权威来源，候选不能抢先接管。会话发布失败显示“2FA 已完成，会话保存待完成”，后续只补本地发布和清理；不能把旧 Cookie 重新合并进来。旧 journal 重现时，若活动会话已由更新操作接管，不得覆盖它；只在完成回执可核对时清理旧残留，否则报冲突。

任何中间失败都保留恢复材料；再次点击仅补剩余步骤。vault 已提交后不能回滚成旧 seed。删除失败显示“2FA 已完成，待清理”，不能重新做一次重置。掉电导致已完成 journal 再出现时，以同 operation 的有效密钥核对后幂等收尾。

恢复过程中不附带退出所有设备、重新授权、上传等动作。保住原会话是续接条件；这些独立功能在重置完成后另行执行。

## 8. 用户入口与准确提示

用户点击原来的重置入口时，服务端先检查同账号未完成 operation。有记录就返回或进入相应续接动作。每次点击意图携带本地请求身份，服务端持久绑定 operation，并在完成元数据中保留去重回执；同一意图即使在完成、删除 journal 和重启后重放，也返回原结果。只有明确的新一轮意图才能创建新 operation；已过保留期的旧请求身份返回过期，不能默认为新操作。这是本地接口去重，不假设官方提供幂等键。按钮由服务端能力投影，不靠前端推测 Cookie 或恢复阶段。

| 状态 | 提示及操作 |
| --- | --- |
| 已准备、变更未发送 | “原会话已保存，尚未移除旧 2FA” → 继续本次重置。 |
| disable 结果待核实 | “移除结果待确认，原会话已保留” → 使用原会话继续。 |
| 已停用、尚未登记 | “已确认旧 2FA 停用，等待设置新密钥” → 继续设置。 |
| 登记材料已保存 | “新密钥已保存，待完成激活/核实” → 继续本次设置。 |
| dirty 未保存 | “最新会话或响应尚未保存” → 重试本地保存；成功后才续接。 |
| 新因子已核实、提交未完成 | “官方已启用新 2FA，本地保存待完成” → 完成本地保存。 |
| 真实会话拒绝/材料缺失/冲突 | 显示具体原因和保留情况；仅提供当前证据允许的核实/处理入口。 |

“检查状态”继续保持只读；“继续本次重置”可以在证据充分时进入尚未发送的下一步。两者不混用。

按用户选择，启动只读取本地资料、恢复状态和按钮，不自动访问官方或续接重置；已核实待提交的 vault 写入、会话交接、journal 清理也等待点击“完成本地保存”。需要调整当前 restore 中启动即本地提交/清理的分支，不能只禁止启动网络请求。重复点击、两个标签页、同账号多任务及第二进程必须沿用现有排他和文件锁，并核对 operation/revision。

错误结构至少区分：本地读取/写入失败、持久化结果不确定、远端认证拒绝、网络暂不可用、响应无法解释、登记材料缺失、身份/因子冲突。普通 HTTP 401/403 或网页读取失败不能再落成笼统的“本地状态保存错误”。403 挑战或权限拒绝不自动等同 Cookie 过期。

## 9. 旧记录迁移

- 旧 Cookie 名称保持原样。迁移保留已有 Domain/Path/HostOnly/HttpOnly/到期时间等属性；旧格式没有的信息标记未知，不能虚构作用域、分区键或有效期。
- 旧记录先只读解析、保留原文件；迁移必须取得同账号锁，生成新版本、完成校验和可靠写入后才切换权威格式。迁移失败不覆盖唯一可读副本。
- `operation/account/transport` 绑定完整、但缺 dispatch 或基线的旧记录，可以在通过现有私有资料一致性校验后支持只读核实；不能填 `not_sent` 来解锁写请求。
- 缺账号/操作绑定的旧记录不能通用自动接续，保留并明确列出缺失。根据历史日志推测“像是删除前失败”，不等于有可用于自动变更的可靠证据。
- 已有严格激活证据和匹配新密钥的旧记录继续支持本地收尾，不因 Cookie 新 schema 拒绝它。
- 已被服务端撤销的旧 Cookie、完全未收到的 enroll secret、已经删除的历史数据，都不能靠库替换重新生成。不得承诺本次迁移必然救回所有历史账号。

## 10. 实施顺序与代码范围

| 批次 | 范围 | 出口条件 |
| --- | --- | --- |
| 1. Cookie 接入试验 | 固定库版本和有限补丁；保持 `transport.CookieJar` 对外适配；合成快照、HTTP/TLS/脚本对照。 | session Cookie、作用域、删除/过期、HttpOnly、Quoted、现代属性及完整 round-trip 全部通过；保存错误能返回调用方。 |
| 2. 会话准入修正 | `session_cookie_candidate.go`、`flow.go`、`recovery.go`、`totp_reset_session.go`；runner/jobs 待恢复入口。 | 任意合法业务名称可访问官方核实；恢复中没有 login/OTP/清空会话 fallback；普通登录不能把普通 200 页面当已登录。 |
| 3. 持久化与回执 | `transport/http.go`、`tlsclient.go`、`privatefile`、`credentials` 的窄保护适配、checkpoint/journal 写入、manager operation owner。 | 响应头及 body 故障不丢已接收内存资料；dirty 阻止下一请求；envelope 原子保存及同 operation 读回。 |
| 4. 续接与提交整合 | `totp_reset*.go`、`cmd/tosub2/totp_reset.go`、`internal/jobs/totp_{prepared,recovery,reset}.go`、restore/cleanup。 | 本次六步场景可成功续接；未知写不重放；新 seed 保存先于激活；保留完成后会话；启动不提交 vault 或清理 journal，等待点击。 |
| 5. 界面和迁移 | `internal/console/totp_actions.go`、相关前端 hook/component、旧记录读取器。 | 重启等待点击、原按钮接续原 operation、只读检查不隐式修改、原因可理解。 |

上述是可分别审查的实现批次，作为一组验收后发布；不能只上线 Cookie 门禁放宽而漏掉入口约束或持久化屏障。

发布前记录源码/二进制版本，按“封锁提交来源 → 排空或按生命周期停止操作并停服 → 一致性备份 → 切换程序”的顺序进行。当前没有现成的原子入队封锁，实施时需补最小封锁或人工停止全部提交来源，不能仅凭队列暂空开始切换。使用本地合成服务验收，不以真实账号移除 2FA 作为回归测试。旧程序不能消费新格式未完成操作；回滚必须识别新 schema，并保留最新恢复记录，不能恢复旧备份覆盖已发生的远端变更。

## 11. 必须通过的故障验收

| 注入场景 | 必须观察到的结果 |
| --- | --- |
| Cookie 改名、任意合法后缀、多枚 Cookie | 原样保存/按标准发送，零业务名称或连续分片判断。 |
| 无 Expires 的会话 Cookie，进程重启 | 完整恢复，首次点击复用会话；不能仅保存 persistent Cookie。 |
| 同名不同域/路径，PSL、Secure、HttpOnly、Quoted | 作用域及序列化正确；脚本不可访问或覆盖 HttpOnly。 |
| Max-Age 到期、删除 Cookie、轮换、新格式恢复 | 不延寿、不删除后复活、不回退旧 generation。 |
| SameSite/Partitioned/未知分区上下文 | 元数据保留，缺分区上下文时不可作为普通 Cookie 发送。 |
| disable 在服务端成功后断开/超时/改变确认文案 | 再次点击只读核实停用后继续首次 enroll；服务端实际 disable 次数为 1。 |
| 轮换响应头已接收但 body 卡住，杀子进程 | 若头快照屏障已完成，重启后使用新 Cookie；若未完成，准确保持未知，不宣称完整保留。 |
| headers 保存失败、enroll body 随后到达 | 当前进程保留受限 body 与新 Cookie；下一次点击先 flush，期间零新网络请求。 |
| enroll 完整响应字段结构变化 | 回执可离线重读；更新解析后复用原登记，不再 enroll。 |
| enroll 远端成功但整个响应丢失 | 无 seed 时准确暂停；不重复登记、不生成另一把 seed。 |
| pending seed 写入失败/读回 operation 不符 | 不发送 activate，不覆盖其他 operation 凭据。 |
| activate 成功但响应丢失/返回结构变化 | 根据同一新因子实时状态提交；activate 不重放。 |
| 已核实激活后 vault 或 metadata 保存失败，Cookie 过期 | 同 operation/seed 可核对时再次点击纯本地收尾；身份冲突继续暂停；零登录、零因子变更。 |
| 账号会话候选已写但引用未提交，或引用已提交但 journal 未删除 | 保留可核对记录；仅本地补交接；最新 Cookie 成为普通操作的会话，旧 checkpoint 不抢先接管。 |
| 后续会话更新后旧 journal 重现 | 不覆盖更新会话；核对完成回执后清理旧残留，无法核对则报冲突。 |
| 错误密钥、AAD 跨 operation、密文截断、密钥服务中途失败 | 保留记录并明确报错；不能误判无记录，不能开始新 reset 或发送下一请求。 |
| file Sync、Rename、目录 Sync 各边界失败 | 保留可判定的最近记录，正确表达不确定，不因读回可见就宣称掉电安全。 |
| journal 删除失败/删除后重启重现 | 已完成结果保持完成；仅清理，不重新 reset。 |
| 取消、重复点击、两标签页、两进程、重启 | 相同 operation、无并发覆盖、无邮箱/登录 fallback；启动不自动续接。 |
| 第一轮完成并重启后重放原点击请求 | 返回原完成回执，disable 实际计数仍为 1。 |
| 已 verified 但尚未本地提交时重启 | 用户点击前 vault 写入、账号会话交接、journal 删除次数均为 0。 |
| 旧记录缺 dispatch/账号绑定，或因子改变 | 不伪造未发送/身份事实；只暴露证据允许的动作。 |
| 服务器明确认证拒绝、403 挑战、5xx、未知 body | 原因分开，不都归为 Cookie 失效或本地持久化错误。 |

除规则测试外，必须使用合成官方服务统计实际收到的 disable/enroll/activate/login 次数，并有子进程终止/重启测试。重新创建一个 Flow 或使用内存 vault 只能验证部分逻辑，不能替代真实文件故障和进程崩溃验证；普通单元测试也不证明全部硬件掉电保证。

此前审查中全量测试有两个 jobs 展示比较测试失败，以及一处格式检查问题；这些不是本方案实现测试结果。实施前先重跑、归因并修复基线，再运行 Cookie、protocol、transport、credentials、jobs、console 的相关测试和必要全量检查。设计完成不代表修复已实现或部署。
