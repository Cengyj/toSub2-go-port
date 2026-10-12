# 纯 Go JavaScript 宿主需求与公开 SDK 证据

记录日期：2026-10-12。本文保留迁移时读取三个旧 CJS 宿主与公开 SDK 所得的技术证据；旧 Node/jsdom/Python 源码和工具现已清理，当前只运行 Go 宿主。调查匿名 GET Sentinel 公开 loader 和其同源版本 SDK，没有调用 `token()` / `init()` / `sessionObserverToken()`、发送 `/req` 请求、读取账号凭据或执行真实登录。

## 证据边界

| 材料 | 结果 | 可证明的范围 |
| --- | --- | --- |
| 公开 loader | 1,482 字节，SHA-256 `ecae8c6c2e84376359d13ab8f9def9e38c646d2423b09c3a33efe6118dddcf95` | loader 的脚本加载行为 |
| 公开版本 SDK | `/sentinel/20260219f9f6/sdk.js`，30,864 字节，SHA-256 `4f8ef8d5870894fd0101fc40ff45ea13c0f8e25c71c2ba28e5df5baf98babbb5` | 此版本代码的静态需求及初始化行为 |
| SDK 初始化追踪 | 迁移时 Node/jsdom 宿主的临时副本，250 ms 有界运行，正常退出 | 本次初始化实际调用的接口；不证明 token 或服务端下发程序可用 |
| 本地合成 CF/Sentinel fixtures | 已有二进制、异步 frame、延后 execute 握手、取消与清理测试 | 明确构造的行为契约；不等于真实线上 CF 脚本覆盖 |
| CF / Turnstile | 本次没有获取或执行当前线上挑战脚本 | 只能确认原宿主提供哪些接口及其缺口 |

公开 loader 和 SDK 只保存于本次私有临时目录，未提交动态脚本或 token。使用 Babel AST 读取 SDK 的常量字符串表并解析浏览器属性名；无法静态确定的动态属性访问明确保留为未知，没有把猜测写成调用证据。

## Sentinel 实际初始化

SDK 在初始化时即创建 iframe，并在父 realm 和子 realm 分别安装 `message` 监听器。追踪未发现生成的 HTTP 请求；没有调用 token 方法。

| 调用 | 追踪次数 | 说明 |
| --- | --- | --- |
| 父 `document.createElement("script")` | 1 | loader 动态加入 SDK 脚本 |
| 父 `document.currentScript` | 3 | SDK 从当前脚本 URL 推导 API origin 和版本 |
| 父 `Node.appendChild` | 2 | SDK script 和 iframe 连入文档 |
| 父 `crypto.randomUUID` | 1 | SDK 初始化实例标识 |
| 父 `document.createElement("iframe")` | 1 | SDK 初始化隐藏 iframe |
| 父 `HTMLIFrameElement.contentWindow` | 1 | 创建和连接子 realm |
| 子 `document.createElement("script")` | 1 | 现有宿主在 frame 中创建 SDK 当前脚本节点 |
| 父/子 `addEventListener("message")` | 各 1 | 双向请求/响应桥 |
| 父 `addEventListener("load")` | 1 | 等待 iframe ready |
| 父 `dispatchEvent` | 1 | 现有宿主投递 frame load |

上述调用包含 SDK 触发的宿主中介操作，并非每条都由 SDK 直接调用。追踪代码没有保留属性值、消息内容、异常栈或请求 body。

## 静态可确认的 Sentinel 需求

| 领域 | API / 语义 | 为什么需要 |
| --- | --- | --- |
| DOM | `document.currentScript.src`、`document.scripts`、`document.documentElement.getAttribute("data-build")` | 从 SDK URL 选择同源 frame/API；proof 配置会读取 script 列表和页面 build |
| frame | `createElement("iframe")`、`style.display`、`src`、`body.appendChild`、`contentWindow`、`window.top`、`location.href/origin/pathname/search` | 识别父/子执行路径；同源信息和版本 frame URL 必须准确 |
| 消息 | `addEventListener("load"/"message")`、`postMessage`、`MessageEvent.data/source/origin` | 父页面按 requestId 关联 Promise；子页面检查发送源；消息支持字符串 targetOrigin 和 options 对象形式 |
| 标准对象 | `Map/WeakMap/Set`、Promise、async/await、可迭代对象、对象展开、可选链、null 合并 | SDK 的原生 JavaScript 控制流及动态程序执行 |
| 编码 | `TextEncoder.encode`、`atob/btoa`、`encodeURIComponent/decodeURIComponent`、`URL/URLSearchParams` | UTF-8/base64 proof 和 frame URL 构造；Latin-1 btoa 与 UTF-8 编码不能混同 |
| 身份 | `navigator.userAgent/language/languages/hardwareConcurrency` | proof 配置读取；要与绑定 HTTP profile 一致 |
| 原型与枚举 | `Object.keys(document/window/Object.getPrototypeOf(navigator))`、属性读取、`toString`、`Function.bind.apply` 构造 | SDK 会观察原型和属性，并可把服务端给出的指令映射为属性访问、调用或构造 |
| 时间与随机 | `Date`、`performance.now/timeOrigin/memory.jsHeapSizeLimit`、`Math.random`、`crypto.randomUUID/getRandomValues` | proof 配置、预算和随机实例标识 |
| 调度 | `setTimeout/clearTimeout`、`requestIdleCallback` 的 `timeRemaining/didTimeout`、Promise 微任务 | 非阻塞 proof 计算与消息等待；必须在同一个 JS event loop 执行 |
| cookie | `document.cookie` | 获取 `oai-did` 等浏览器可见 cookie；HttpOnly 不可见，不能影子复制权威 CookieJar |
| storage | 服务端指令可通过通用解释器读取 `localStorage/sessionStorage` | 同 host / origin 数据共享，不同会话与 origin 隔离；不能以共享全局 map 替代会话生命周期 |
| HTTP | `fetch`、响应 `json()`、`credentials:"include"` | 子 frame 的 `/req`；此路径本次仅静态确认，未发送请求 |
| 服务端动态程序 | `turnstile.dx`、`so.collector_dx/snapshot_dx` 对应的内置指令解释器 | 可间接读取任意 window 属性、调用函数、构造实例并等待 Promise；初始化 trace 无法覆盖这些后续 API |

SDK 本身含通用指令解释器。仅列出代码明文出现的 API 不能证明所有服务端下发程序已经覆盖。应以公开 SDK 初始化、合成动态程序和获准的实际流程逐层验证；出现未知接口时返回明确的不支持诊断。

## 三个历史宿主提供的 DOM / browser 面

| 领域 | 迁移时的实现依赖 | 纯 Go 行为要求 |
| --- | --- | --- |
| DOM 树及 HTML | jsdom 解析 HTML；`Document/Node/Element/HTMLElement/HTMLIFrameElement` 原型；`body/head/documentElement`、`createElement`、`appendChild`、`querySelector(All)`、`document.write/open/close`、`currentScript` | 使用真实 HTML parser；维护 ownerDocument、父子关系、DocumentFragment 转移、attributes、textContent/innerHTML 和树查询；不支持的 selector 明确抛异常 |
| 脚本 | CF 动态外部 script 插入、初始 inline scripts、load/error、currentScript | script 插入和 src 变化由 Go 拥有加载与执行；不可重放同一个 load；执行时设置/恢复 currentScript |
| iframe | 创建真实独立 JS realm、稳定 contentWindow、parent/top/frameElement；CF 父子握手 | 对每个 frame 维护稳定 WindowProxy、URL 和 realm；按浏览器 origin 约束 contentDocument；不能把两个 frame 的消息混合 |
| 事件 | EventTarget、Event、MessageEvent、捕获监听、load/message/error、`source/origin/isTrusted` | 支持捕获、目标、冒泡、once、移除监听、stopPropagation/immediate、preventDefault；系统事件与脚本构造事件区分 trusted |
| DOM mutation | CF `MutationObserver` 观察新增后代；appendChild 拦截 | mutation records 在微任务中按顺序交付；移动、移除、属性更新和子树匹配准确；资源加载按节点/URL 去重 |
| 网络与二进制 | fetch/XHR/Headers；text/json/arrayBuffer/blob；clone；XHR headers 和 load/loadend/readystatechange | 原始字节作为权威内容，UTF-8 解码只用于 text；CookieJar、TLS profile、proxy 和预算继续由父 Go 进程持有 |
| performance | `PerformanceEntry/ResourceTiming/NavigationTiming` 原型、`getEntries(ByType)`、PerformanceObserver、内存值 | navigation/resource records、原型关系和过滤一致；不能返回伪造的已完成网络结果 |
| 诊断和生命周期 | NDJSON 计数诊断、deadline、owned process tree、私有输入删除 | 不输出 URL 查询、cookie、body、动态异常文本；保留 context cause；取消要停止所有 realm、timer、worker 和待处理 RPC |

## 旧 CJS 基线与纯 Go Worker / ReadableStream

本段的空壳描述针对迁移前的 CF 父/子 **CJS 宿主**。其中 `Worker` 只保存 URL/options，`postMessage()` 和 `terminate()` 为空方法；不会加载或运行 worker，也不会产生 message/error。它们的 `URL.createObjectURL` 只保存随机 URL 的集合，未保存 Blob 内容，因而不能供 Worker 加载 blob 源码。这些旧实现不是当前 `internal/jshost` 纯 Go 宿主的行为。

旧 CJS 的 `ReadableStream` 只保存 source/strategy，`read()` 恒返回 done，没有调用 producer 的 start/pull/cancel；`tee()` 构造两个空流；`pipeTo()` 立即成功。这会丢失实际字节和背压，不能作为纯 Go 宿主的实现依据。

本次公开 Sentinel SDK 的静态文本、字符串表和初始化 trace 均未出现 Worker/ReadableStream 调用。没有当前 CF 脚本执行证据，因此不能声称 CF 不需要它们，也不能声称它们是本次线上超时的已证实原因。

纯 Go 宿主已实现并以本地合成 fixtures 验证以下行为：

- Worker：classic Worker 普通/blob URL 源码加载、独立 goroutine / JS realm 和 globals、双向异步 postMessage、structured clone（含 ArrayBuffer/typed array）、transfer、message/error、importScripts、terminate 和自行 close 后回收。父 HTTP/cookie 会话继续拥有网络；发送队列有界且不会同步阻塞创建方的 JS event loop。
- Blob URL：保存实际 Blob 字节、MIME、创建 origin、所有权和 revoke 状态，供 Worker / importScripts 读取；独立 host 不共享其注册表。目前没有 `fetch(blobURL)` 路径。
- ReadableStream：真实 default stream 的 start/pull/cancel、controller.enqueue/close/error、队列与 pending read Promise、锁、reader.closed、releaseLock、背压、tee、异步迭代和 pipeTo/pipeThrough 的取消及错误传播。

这些能力不等于完整 Web Platform。ReadableStream 的 byte source / BYOB reader 明确抛 `NotSupportedError`；没有 `WritableStream`、`TransformStream`、`TextEncoderStream` 或 `TextDecoderStream` 构造器。`pipeTo` 需要具有真实 ready/closed/write/close/abort/releaseLock 行为的 writer，不能向空目的端报告成功。`crypto.subtle` 目前只有 SHA-1/256/384/512 digest；Request/Response 支持 string、URLSearchParams、Blob、BufferSource 和 default stream body，不提供 FormData / multipart。

Web Platform 的具体测试、Node 原生 oracle 对照与编码等边界见 [web-platform-validation.md](engine-probe/web-platform-validation.md)。认证子进程的 150 秒执行期限、32 MiB 输入 / 协议限制、256 MiB Go GC 目标、384 MiB 采样 heap guard、8 MiB goroutine stack，以及 worker / realm / queue 配额见 [worker-boundaries.md](engine-probe/worker-boundaries.md)。这些内存策略不是操作系统硬内存上限；不能将合成测试通过称为所有远程动态程序或真实账号认证已经验收。

## DOM 实现接口

DOM bootstrap 由嵌入 JavaScript 提供，不依赖 Node。Go 宿主提供 `__host.parseHTML(html[,contextTag])`、`inserted(node)`、`frame(node)`、`cookieGet()` / `cookieSet(value)`、`resolveURL(value,base)` 和单 event loop 的 `queueMicrotask`。可选 `removed(node)` 在已连接节点从树移除时收到移除根节点，供宿主退役该子树的 frame、timer 和 worker；独立 DOM fixture 不要求实现此 hook。

`parseHTML` 的整页结果为 `{nodeType,tag,attrs,text,children}`；fragment 模式应使用 parser 的上下文语义，尤其 table/tbody。`inserted` 在 script/frame 连入 document 或其 src 变化后通知 Go；Go 按稳定节点和当前 URL 管理加载。`frame` 返回稳定代理，其 document getter 由 Go 实施同源约束。系统事件经 `__domDispatchTrusted` 投递；普通 `new Event()` 不能自行取得 trusted 状态。

本清单不宣称提供浏览器渲染、Canvas/WebGL、完整 CSS layout、完整 DOM 或全部当前 CF 行为。涉及这些能力的动态执行应有单独证据和测试。

## 本轮 DOM 验证

`internal/jshost/dom_semantics_test.go` 使用 Sobek 和隔离的本地 host bridge，未执行网络或账号流程。`go test -race ./internal/jshost -run '^TestDOM' -count=1` 已通过，覆盖以下行为：

- 事件捕获 / 目标 / 冒泡、once、分发中移除监听、passive、取消、stopImmediatePropagation、可信系统事件及脚本再次分发后的非可信状态。
- live childNodes / children、静态 querySelectorAll、DocumentFragment 转移、ownerDocument、clone / import、Document 根节点替换及失败时保留原树。
- HTML fragment 的 table 上下文、相邻多节点顺序、组合 / 关系 / nth / escaped selectors、attributes / dataset / classList / inline style、接口原型和文本转义。
- MutationObserver 微任务顺序、重复观察的 oldValue 合并、takeRecords / disconnect，以及子树移除后首次通知前的临时观察。
- 仅连接的节点通知 loader、当前 base URL 解析、iframe 代理契约和同源 getter 拒绝、cookie bridge、document.write 生命周期事件。

迁移时另用 Node/jsdom 26.1.0 做过一次只含本地 HTML 的 oracle 检查；组合 / 关系 / nth / escaped selectors、table fragment、相邻节点顺序和 normalize 输出一致。该外部宿主现已删除。jsdom 对已移除子树没有产生首次通知前的 attribute record；Go fixture 明确按 DOM 的临时观察规则覆盖此情况，没有把这项差异称为 jsdom 等价验证。

当前 DOM 仍有明确边界：`document.write` 累积并重解析 HTML，而非完整浏览器增量 parser；没有 HTMLTemplateElement.content、TreeWalker、shadow DOM、完整 selector / CSS parser、layout 和绘图接口；CSSStyleDeclaration 仅处理基础 inline declarations。通过上述 fixtures 不代表当前远程挑战程序、所有指纹观察或账号认证已验收。

`storage.go/storage.js` 另提供 host 持有的 local/session areas：按 origin 共享，独立 host 不共享，包含 Storage 原型、方法、named properties / 枚举、DOMString coercion 与每 area 5 MiB UTF-16 字节配额。`go test -race ./internal/jshost -run '^TestStorage' -count=1` 已通过同源共享、跨 origin / host / area 隔离、孤立 surrogate 保存、原子 quota 失败及容量释放、opaque origin 拒绝测试。Worker globals 不暴露 storage。本轮未实现 `storage` 事件或持久化到磁盘；数据跟随认证 host 生命周期。

## 本轮宿主回归与加载边界

`internal/jshost/audit_regression_test.go` 使用本地构造的脚本与响应，覆盖 XHR 在 readyState 2 / 3 的回调中 abort 或重新 open 后丢弃旧响应；同一 Request 不能再次 fetch，锁定但未消费的 Request 在 releaseLock 后仍可发送；Fetch / XHR 对 JavaScript 隐藏响应的 Set-Cookie / Set-Cookie2。33 个依次自行 close 的 Worker 仍可使用已回收的活动配额；测试保留原 3 秒 deadline，没有调宽超时。

脚本回归覆盖 loader 和动态 SDK 在 document.scripts 中可见、执行期间的 currentScript 节点身份、执行后及其微任务 / load 回调中的 currentScript 为空，以及顶层 let / var 绑定可被后续脚本读取。初始父/子页面的 inline classic scripts 保留 DOM 节点身份与执行顺序，application/json 数据脚本保留在文档中且不执行。动态 inline classic script 同步执行，移除后重插不重复执行；加载器忽略文本节点。

2026-10-12，`go test -race ./internal/jshost -run '^Test(Audit|ZeroDelayTimers|WindowTransfer)' -count=1` 已通过（包耗时 3.314 秒），包含上述全部 audit 回归，以及零延迟 timer 的公平调度和 Window 消息 transfer 回归。此记录只覆盖该目标集合，全仓最终验收另行记录。

初始 HTML 的自动执行范围为 inline classic scripts，与旧 Node/jsdom 的 outside-only 路径一致；初始 HTML 中的外部 script 不会由 parser 自动加载，动态外部 script 使用宿主加载桥。Module script 明确返回 `JS_API_UNSUPPORTED_MODULE_SCRIPT`。没有通用 iframe srcdoc 或任意 iframe HTML 的自动装载能力；现有认证的父/子专用桥不能作为完整 iframe 导航实现的证明。
