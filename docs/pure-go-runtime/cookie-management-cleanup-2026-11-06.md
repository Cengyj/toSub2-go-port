# Cookie 管理清理说明

> 历史记录：本文描述当时版本，涉及 Cookie 持久化、登录 checkpoint 或远端恢复的能力现已移除。当前行为与清理范围见 [登录会话清理](authentication-session-removal-2026-11-08.md)。

> 全项目范围纠正（2026-11-08）：本文涉及的 AccountSession、Cookie 快照持久化、共享会话与 owner/dirty 机制也是后来新增，现已纳入整体清理；不能再把它们作为“原版普通会话”保留。当前边界见 [全项目清理记录](cookie-persistence-cleanup-2026-11-08.md)。下文仅作历史追溯。


日期：2026-11-06。状态：**清理完成，本轮全仓测试、竞态回归、静态检查及候选构建通过**。本文件说明本轮清理的调用链、删除范围与兼容性约束；以下验收均基于本轮最终源码，未切换或重启现有服务。

前序设计与实现见 [Cookie 会话与 2FA 恢复方案](cookie-session-and-totp-recovery-plan-2026-11-06.md)和[实施记录](cookie-session-and-totp-recovery-implementation-2026-11-06.md)。早期[分段会话设计](totp-reset-chunked-session-design-2026-11-06.md)保留为审计历史，其中固定业务 Cookie 名称及连续分片判断已被取代。

## 目的

Cookie 名称和值由服务端定义。客户端不拼接分片，不判断 `.0/.1` 是否连续，不要求某个登录 Cookie 名称存在，也不根据 Cookie 形状宣告登录成功。每枚 Cookie 独立交给标准规则引擎；账号身份与会话是否可用，由原会话访问官方接口的结果确认。

本轮消除重复的旧格式写入、未使用导出和手工解析。保留无损快照、脚本访问控制和同步持久化这些现有标准公开 API 无法独立提供的能力。

## 当前调用链

唯一活动 Cookie 状态位于 `internal/third_party/cookiejar` 的 `entries`。`internal/transport.CookieJar` 是访问与持久化适配器，不再维护另一份运行时 Cookie map。

| 入口 | 调用链与职责 |
| --- | --- |
| HTTP/TLS 请求 | 同一 `transport.CookieJar` → `HeaderFor` → core 的 `Cookies(url)`；域、路径、Secure、到期和排序由 core 选择。native HTTP 与 TLS 底层的隐式 Jar 均关闭。 |
| HTTP/TLS 响应 | `Header.Values("Set-Cookie")` → 每个原始 header value 调用 `net/http.ParseSetCookie` → core `Apply`；整批接受的变更在同一锁内生成 revision 和快照。 |
| 响应后的保存 | transport 的同步 hook → 当前会话或 reset owner → 私有加密存储及读回。保存失败保留受限 dirty 状态，并在下一次网络请求前阻断。 |
| `document.cookie` 读取 | `internal/jshost/dom.js` → worker 的 cookie RPC → 父进程原 Jar → core `DocumentCookies`；脚本拿不到 HttpOnly Cookie。 |
| `document.cookie` 写入 | 同一个 RPC → `SetDocumentCookie` → 标准单行解析 → core `Apply` 的脚本模式 → 相同持久化 hook 与网络屏障。 |
| 恢复 | 校验账号、操作、路径和传输绑定 → 无损 `CookieSnapshot` → core `Restore`；普通 workflow checkpoint 通过引用加载独立账号会话。 |

`internal/jshost/*.js` 是当前 Sobek worker 的嵌入式 Web API 实现，不是残留 Node Cookie 管理器。`document.cookie` 只做代理；`Headers` 对 `set-cookie` 的表示及 fetch/XHR 对该响应头的隐藏属于脚本平台边界，不能当作旧 CookieJar 一起删除。脚本 HTTP RPC 不再回传整份 Cookie 集合。

## 清理范围与完成标准

| 清理项 | 处理方式 | 兼容性约束 |
| --- | --- | --- |
| `GetSetCookie` 的手工逗号拆分 | 删除猜测边界的扫描器，直接保留每条 `Set-Cookie` header value，并交给标准单行 parser。 | 不再猜测一行里是否合并了多枚 Cookie；见下节。 |
| 无生产调用的适配接口 | 删除项目级 `ParseSetCookie`、`ToJSON`、`CookieJarFromJSON`、`NamesFor`、`Has`；测试通过真实响应入口、快照或请求选择断言。 | 不删除仍有调用的标准 `net/http.ParseSetCookie`、无损快照类型或设备身份读取。 |
| 旧格式与新快照双写 | 新恢复记录只写权威 snapshot，或引用独立会话；保留旧格式读取边界。 | 不同时维护可分歧的 `cookies` 与 `cookie_state`；显式历史导出另见兼容章节。 |
| fingerprint 选择后的投影复制 | 只有全新空 Jar 可并行探测；选中 Jar 以 Snapshot/Restore 交接，传播恢复错误，已是同一 Jar 时不重复复制。 | 空 entries 不代表没有历史；Revision/NextSeqNum 不得因探测交接回退。 |
| 底层隐式 Jar 注入 | native HTTP 对请求局部 client 副本关闭 Jar；TLS 在全部 extraOptions 后强制关闭隐式 Jar。 | 不修改调用者传入的 HTTP client 或 Jar 实例，不让自定义选项生成第二个 Cookie 状态源。 |
| 响应与脚本桥的整份 Cookie 副本 | 清理无读者的响应字段及 RPC 镜像，保留必要的私有原始响应头证据。 | `Set-Cookie` 不暴露给脚本；恢复回执仍可保存已收到的头部证据。 |
| 静态复制、业务 Cookie 解析和清理 | 调用唯一 Jar 的明确操作，不在 runtime/protocol 再实现另一套解析、expiry 或作用域规则。 | 设备 Cookie 的读取/设置是协议行为，不等于凭 Cookie 名判断登录成功。 |
| 旧 wire 表示 | 将旧 JSON 的类型、兼容解码和显式导出集中在独立兼容文件。 | 运行时变更和新恢复持久化不得通过旧 wire 表示往返。 |

## Set-Cookie 的标准边界

服务端应以多条 `Set-Cookie` header value 表示多枚 Cookie。逗号也会出现在 `Expires` 日期或标准 parser 接受的值中，项目不再自行推测某个逗号是否表示下一枚 Cookie。

因此，一条非标准合并响应头是否能够解析、以及解析出的内容，完全取决于 `net/http.ParseSetCookie` 对该原始行的结果。项目既不拆分它，也不把它修复成多条响应头。需要多枚 Cookie 的合成服务应使用 `Header.Add` 写多条值；真实上游若发送合并头，应修正上游格式，不能重新引入猜测性拆分。

逐条调用 `ParseSetCookie` 是有意保留的薄适配。本项目工具链的 `http.Response.Cookies()` 内部批量读取器，在超过其 Cookie 数量限额时会静默返回空集合；用它代替逐行解析会改变整批 Cookie 的接收行为。资源上限继续由项目显式的快照/持久化边界负责，超限不能伪装为一批空 Cookie。

## 必须保留的 core 扩展

所用 core 来自固定版本 `go.nhat.io/cookiejar v0.4.1`；来源、许可证与补丁范围见 `internal/third_party/cookiejar/UPSTREAM.md`（历史文件，已随旧 Cookie 引擎移除）。这些扩展不构成第二个运行时 Jar：

1. **完整快照与原子恢复。** 保留 session/persistent 区分、绝对到期时间、HostOnly、HttpOnly、Quoted、SameSite、创建/访问时间、排序序列和分区元数据。`Cookies(url)` 是发送投影，不能无损替代它。恢复先验证完整输入再替换，不能部分合并，也不能通过重放 Max-Age 延长原期限。
2. **同批 revision 与快照。** mutation 和对应快照必须来自同一锁内的同一个状态。不能先调用 `SetCookies`，释放锁后再额外获取 Snapshot，形成版本与内容不对应的窗口。持久化回调在 core 锁外有序执行，避免死锁。
3. **脚本访问限制。** `document.cookie` 不能读取或覆盖 HttpOnly 项，不能绕过 Secure 及安全作用域覆盖限制。网络与脚本共用 selection/mutation 引擎，不能建立脚本 shadow Jar。
4. **标准安全属性。** `__Secure-`、`__Host-` 是标准前缀，不是 ChatGPT 私有业务名称。标准 parser 的语法检查不能替代其接收和恢复约束，也不能替代公共后缀验证。
5. **现代属性与限制。** 保留 SameSite 信息，但不声称实现浏览器完整跨站策略。缺少 top-level site 上下文时保留分区元数据，分区 Cookie 不作为普通 HTTP Cookie 或 document Cookie 发送。
6. **大 Max-Age 的安全换算。** 巨大正值在算术前饱和至支持的最大时间，避免溢出为过去时间。已保存的快照恢复只使用绝对期限。
7. **私有快照结构验证。** schema、排序、重复作用域和资源上限属于文件完整性与资源边界，不是 Cookie 业务名称、值或分片布局的准入条件。

core 的普通 `SetCookies` 与扩展 `Apply` 已共享同一个 mutation engine；域名、默认路径、Cookie 条目构建和请求选择继续复用上游规则。清理应缩减重复入口，不能用“采用标准 API”作为丢失无损状态或取消持久化屏障的理由。

## 最终一致性约束

### 空 Jar 与 fingerprint 探测

允许并行探测的“全新空 Jar”必须同时满足 `len(Entries) == 0`、`Revision == 0`、`NextSeqNum == 0`。已经接收再删除或清空 Cookie 的 Jar 仍有状态历史；即使当前没有条目，也按已有会话串行使用原 Jar，不用新建候选 Jar 的空状态覆盖其 revision 或创建序列。持久化已经阻断时，在探测前直接返回原错误。

选中客户端与调用者使用不同 Jar 时，交接完整 Snapshot 并传播 Restore 错误；使用同一个 Jar 时不重复恢复。这样既保留选中的连接，也不会把历史状态变成新的零版本会话。

### JSON 中的无损计数与引用

Cookie snapshot 中的 `Revision`、`NextSeqNum`、条目 `SeqNum` 以及会话引用中的 revision 使用 `uint64`。经过普通 `map[string]any` 的 JSON 解码会把数字变成 `float64`，超过其精确整数范围时可能改变原值，进而破坏引用检查、排序或 dirty 读回比较。

本轮修复将 checkpoint 的 JSON 合并保持为 `json.RawMessage`，并在 journal 的 `cookie_state`、`cookie_ref`、`previous_session_ref`、`session_ref` 等权威 Cookie 字段中保留原始 JSON；只在需要验证时解码为有类型的 snapshot/reference。journal 其余业务证据仍沿用原有 map 表示。

该约束必须贯穿 checkpoint → journal → dirty owner 保存/读回 → runner 本地完成的整条链，而不只是单次 Marshal/Unmarshal。runner 集成与大整数 round-trip 的最终结果仍在下方验收表填写，不能仅凭低 revision 的普通用例宣称无损。

### 关闭底层隐式 Jar

native HTTP 已自行设置权威 Cookie header；若调用者传入的 `http.Client.Jar` 非空，底层还会附加请求 Cookie 并独立接收响应。处理方式是为该请求复制 client 并将副本的 Jar 置空，不更改调用者原实例。

TLS 的 `extraOptions` 同样不能改变 Cookie 所有权：在全部自定义选项之后追加关闭隐式 Jar 的选项。调用者提供的 Jar 对象保持原样，但不参与本客户端的请求或响应。HTTP/TLS 的 Cookie 接收、快照与保存屏障继续只经过项目的同一适配器。

## 旧格式读取与显式导出

- 仍读取历史 `cookies` JSON，支持原格式的毫秒时间戳、RFC3339 字符串和 `null` 到期值。缺少新快照时，只在兼容入口转换为当前 core 状态。
- 历史格式没有完整创建序列等信息，不能反过来当作新快照的权威来源。存在新快照或独立会话引用时，使用其经过验证的状态，不再由旧数组覆盖。
- 对旧格式的字段归一化只属于迁移边界；它不允许根据服务端私有 Cookie 名称推断身份、解码内容或补齐分片。旧格式本来缺失的属性也不能宣称已无损恢复。
- 仍有明确消费者的旧版输出可以保留显式导出投影。这是兼容输出，不是另一份实时 Jar，也不能重新用于新恢复记录的双写或状态交接。
- `CookieEntry`/`CookieSnapshot` 等类型用于当前快照、深拷贝和摘要，应保留。`Value("oai-did", url)` 用于绑定设备身份，不承担会话有效性判断。

## 旧脚本与文档的处置

仓库当前运行入口使用 Go 程序和同一可执行文件的 Sobek worker。`README-legacy.md`、`DEPLOYMENT-legacy.md` 以及早期设计中的 Node/Python 描述是历史资料，不是新的运行路径。`docs/pure-go-runtime/engine-probe` 是独立实验模块；其旧 Node/CJS 参考程序已移除。

`scripts/local-legacy-macos.sh` 仍是现有隔离实例的有效启动包装器，不能仅因名称含 `legacy` 删除。`scripts/local-macos.sh` 内旧 Node/Python helper 的名称用于识别旧进程和安全清理迁移残留，不表示仍启动这些解释器。

历史故障、设计选择、测试和部署回执保留原文，在文首明确适用时期并链接当前规则。不得改写历史部署事实来宣称本轮已发布。

## 候选程序

本轮已构建独立候选 `bin/tosub2-cookie-cleanup-20261106`（macOS ARM64，`CGO_ENABLED=0`）。SHA-256：

```text
410ff65a4965f9277176bb451db1ba5240ab639bcc3f9d8f1b30668759e7d26a
```

Windows AMD64 程序交叉编译通过，验证产物位于 `/tmp/tosub2-cookie-cleanup-20261106-windows-amd64.exe`，未执行原生 Windows 运行验证。构建使用 `go build -trimpath -ldflags='-s -w'`，目标为 `./cmd/tosub2`。

前序 `bin/tosub2-cookie-recovery-20261106` 保持不变，已重新核对其 SHA-256 仍为 `722ab869bf7b02c26f9ead41b0993f7dc7990e9589677a931ef84165cd72b4b4`。新候选未替换运行程序，也未部署服务。

## 验证记录

工具链：`go1.27.3 darwin/arm64`。验证使用本地合成服务、临时目录和测试保护 provider，没有读取真实账号 Cookie 或访问真实 Keychain。

| 验证项 | 本轮结果 |
| --- | --- |
| 无效/重复旧接口及旧规则调用点扫描 | 通过；已删除接口无生产调用；业务登录 Cookie 固定名称无生产匹配；`Jar.Cookies()` 生产消费者只剩显式私有历史格式导出。 |
| 多条 Set-Cookie、Expires 逗号、quoted 值、非标准合并头边界 | 通过；包括限制 `httpcookiemaxnum=2` 时三条合法响应头仍完整接收，以及不推测拆分合并字段。 |
| Cookie core、transport、脚本桥及 persistence 故障回归 | 通过；整批保存、Retain 的一次修订/无变化不保存、删除后保存失败屏障、HttpOnly 与直接 RPC 响应头隔离均覆盖。 |
| 旧 JSON 读取、无损 snapshot 恢复、单一权威新写入 | 通过；旧记录仍能恢复，新 checkpoint/journal 不再同时写旧数组与快照；构造器不重复导入。 |
| protocol/runner 恢复与 fingerprint 状态交接回归 | 通过；全包测试覆盖原会话恢复、崩溃/断线、dirty enrollment、准备状态、密钥保存与会话交接；探测只交接选中完整快照，失败不返回可用 client。 |
| 空条目但有 Revision/NextSeqNum 历史的 fingerprint 探测 | 通过；有删除历史/仅排序历史均保留，未发请求时不重写状态，已有 Jar 不重复持久化。 |
| 大整数 snapshot/reference 经 checkpoint、journal、dirty owner、runner 读回保持精确 | 通过；`>2^53` 的 revision/seq 经序列化、实际 journal 读回、重保存、dirty Flush、账号 handoff 及幂等重放均保持精确。 |
| native HTTP.Jar 与 TLS extraOptions 注入关闭，调用者实例保持不变 | 通过；外部 Jar 的读写计数均为零，native 普通/SingleAttempt 与 TLS 请求均只使用共享 Jar。 |
| race、全仓测试与 vet | `go test ./... -count=1 -timeout=5m`：27 个有测试包全部通过；`go vet ./...` 通过。transport、core、legacyruntime、jshost 整包 race 通过；最终 transport 注入修复后再次整包 race 通过；protocol/cmd Cookie、journal、handoff、恢复故障定向 race 通过。 |
| 构建与静态差异检查 | macOS ARM64 与 Windows AMD64 候选构建通过；`git diff --check` 通过。本轮未改前端源码，不重新生成前端资源。 |

本轮没有启动、停止或部署服务，没有修改真实账号、会话或凭据。仍按既定约定：程序重启后恢复记录和按钮，等待用户点击才继续；不自动重新登录或重放结果未知的 2FA 修改。Cookie 标准化和代码清理不能保证服务端已撤销或完全丢失的会话可恢复。
