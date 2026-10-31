# 账号用量与状态查询设计方案

日期：2026-10-31（Asia/Shanghai）。本页保留方案阶段的研究与决策；实施与本地验收已完成，见 [实施与验收记录](account-insights-validation-2026-10-31.md)。当前使用、配置、恢复和实际支持边界见 [功能指南](account-insights.md)，部署状态以 [部署记录](local-deployment.md) 为准。

## 1. 研究基线与结论

- 当前项目基线：`ae6806ea80da525ae8e40f10a10f5c6b07427fa9`；开始研究时工作区干净。
- 参考仓库：`ranxi2001/sub2api`，固定到 `d14ed1807bdfe43aabed1739c0e062c1f4512ddf`（提交时间 2026-10-31 09:36:28 +08:00），避免按浮动分支描述协议。
- 本轮仅阅读源码并编写设计；未读取真实账号凭据、HAR、token，未调用账号上游接口，未更改 4401 服务或 GitHub。
- 推荐增加独立的「账号用量与状态查询」模块：服务端按用户配置采集，前端只读缓存。保留现有认证、2FA、上传、套餐排除和 Sub2 恢复巡检语义。
- 必须分别表达：OAuth 凭据状态、查询接口是否可访问、额度窗口、BPS 模型权限、订阅到期、Sub2 调度状态。不能合并成一个笼统的“正常/异常”。
- 参考项目同时支持“主凭据就是 BPS”和“普通主 OAuth + 独立 BPS 凭据”两种布局。其用量查询采用主 OAuth 调用 Codex quota 接口；源码没有证明所有 BPS-only token 都被该接口接受。

## 2. 当前项目的能力和边界

| 现有能力 | 核心实现 | 对本次设计的约束 |
| --- | --- | --- |
| ChatGPT/BPS 授权、身份与恢复 | `internal/protocol/authorization_method.go`、`oauth.go`、`internal/jobs/refresh_recovery.go` | 查询沿用账号保存的授权方式及工作区，不读取浏览器登录偏好决定身份 |
| 套餐与订阅到期 | `internal/protocol/oauth_metadata.go`、`internal/jobs/expiry.go` | 当前主要从授权元数据读取；手动到期优先，token 到期不能当套餐到期 |
| 主任务、操作与授权能力 | `internal/console/server.go`、`internal/jobs` | `status`、`operationStatus`、`lastError`、`ResultSaved` 不能被查询结果覆盖；保存授权不等于实时可用 |
| Sub2 巡检、自动重登与同步 | `internal/sub2api/monitor.go`、`internal/console/monitor.go` | 新查询不能隐式触发重登、上传、解除远端禁用或改变套餐排除规则 |
| 浏览器账号轮询 | `web/src/hooks/serverState.js`、`web/src/lib/console.js` | `/api/jobs` 每 900ms 被读取；不得把上游查询挂到列表 GET 上 |
| 浏览器读取生命周期 | `web/src/lib/polling.js`、`latestRead.js` | 可复用取消和过期响应保护；默认轮询上限为 30 秒，不能直接拿来承载用户设置的分钟级后台采集 |
| 列表选择数据 | `internal/console/server.go` 的 `publicSelectionJob` | selection 包含所有匹配账号，需保持轻量，不能附完整额度明细 |
| 查看账号信息 | `internal/console/account_info.go` | 该接口会读取密码、2FA 等资料；额度详情必须用独立的非敏感接口 |

当前没有独立的实时额度采集服务。这是实现缺口，不是证明上游完全不支持查询。

## 3. 参考项目的真实查询链路

### 3.1 普通 OpenAI OAuth 的额度查询

`OpenAIQuotaService.QueryUsage` 使用主 OAuth access token、账号/工作区身份和账号代理请求：

```text
GET https://chatgpt.com/backend-api/wham/usage
GET https://chatgpt.com/backend-api/wham/rate-limit-reset-credits
```

第一条返回套餐、额度窗口、额外功能窗口、可消费 credits 等；第二条补充重置券数量及到期明细，失败不应抹掉第一条已成功的数据。

关键字段：

| 字段 | 含义 | 展示约束 |
| --- | --- | --- |
| `rate_limit.allowed / limit_reached` | 上游额度规则的允许/耗尽标志 | 缺失不能默认成 false，也不能等同账号是否被封 |
| `primary_window / secondary_window` | 一个或两个额度窗口 | 依据 `limit_window_seconds` 标注 5h、7d 或实际窗口，不依赖数组顺序 |
| `used_percent` | 已使用比例 | 已用/剩余文案明确；缺失与 0 区分 |
| `reset_at / reset_after_seconds` | 额度窗口重置时间 | 与订阅到期、access token 到期分开 |
| `additional_rate_limits` | 特定功能的额外窗口 | 单独按功能展示，不覆盖全局额度 |
| `credits.balance / unlimited` | Codex 可消费 credits | 保留十进制字符串与未知/无限语义，不标成美元或 BPS 余额 |
| `rate_limit_reset_credits` | 可用于重置额度的券 | 与可消费 credits 分开；只查询，不消费 |

上游 GET 与参考项目服务层的“无副作用”不能画等号：其 token provider 可能刷新凭据；QueryUsage 会保存快照并在严格条件下恢复其记录的限流状态；管理端 handler 还会通知自动重置服务。当前项目不能直接把这些副作用一起搬进查询功能。

### 3.2 BPS 的两种凭据布局

| 布局 | 参考项目如何授权/保存 | 额度请求采用谁 | 对当前项目的影响 |
| --- | --- | --- | --- |
| 直接 BPS 登录 | Excel/BPS OAuth 兑换结果直接写主 credentials | 主 credentials 的 token | 当前项目主要对应此类；源码有调用路径，但上游接受能力仍需受控验证 |
| 普通授权后开启 BPS | 主 Codex OAuth 保留，另获取并加密保存独立 BPS grant | 主 Codex token，不使用独立 BPS grant | 不能把这种账号的成功直接推导到 BPS-only；也不能偷偷为本项目账号补发第二种授权 |

参考 QueryUsage 检查 OpenAI/OAuth/账号身份，但并未以 client_id 或 scope 证明该 token 能查询 wham。设计因此采用明确的查询能力状态，而不是无条件承诺“所有 BPS 均可查额度”，也不是一开始就禁止 BPS 查询。

BPS 的模型访问查询使用 `/basispoints/api/responses/access?include_models=true`。它回答模型访问权限，不等于剩余额度接口，也不能推出订阅截止日期。

### 3.3 不应照搬的主动探测

参考项目账号表格的 `/accounts/:id/usage` 并不总是纯额度 GET：普通账号缺少/过期快照时，可通过 POST Codex responses 的测试请求采样响应头。BPS 的 403 自动恢复路径也会真正请求模型输出并检查结果，然后可能恢复配置。

本功能默认只执行已确认的只读信息 GET，不发送测试对话、不消费重置券、不进行 BPS 分组恢复、不因查询错误自动登录。若未来增加模型可用性实测，应作为单独的明确操作，并说明会产生模型请求。

### 3.4 参考代码证据

以下均固定到研究提交：

- [额度端点、DTO、QueryUsage](https://github.com/ranxi2001/sub2api/blob/d14ed1807bdfe43aabed1739c0e062c1f4512ddf/backend/internal/service/openai_quota_service.go#L26)。
- [主 OAuth 凭据、账号身份和代理选择](https://github.com/ranxi2001/sub2api/blob/d14ed1807bdfe43aabed1739c0e062c1f4512ddf/backend/internal/service/openai_quota_service.go#L500)。
- [账号表格 BPS/普通用量分流](https://github.com/ranxi2001/sub2api/blob/d14ed1807bdfe43aabed1739c0e062c1f4512ddf/backend/internal/service/account_usage_service.go#L712)。
- [普通账号生成式探测](https://github.com/ranxi2001/sub2api/blob/d14ed1807bdfe43aabed1739c0e062c1f4512ddf/backend/internal/service/account_usage_service.go#L833)。
- [BPS 模型访问查询](https://github.com/ranxi2001/sub2api/blob/d14ed1807bdfe43aabed1739c0e062c1f4512ddf/backend/internal/service/openai_excel_bps_model_access.go#L100)。
- [BPS 凭据状态投影](https://github.com/ranxi2001/sub2api/blob/d14ed1807bdfe43aabed1739c0e062c1f4512ddf/backend/internal/service/excel_bps_credential_state.go#L10)。
- [BPS 403 真实生成探测](https://github.com/ranxi2001/sub2api/blob/d14ed1807bdfe43aabed1739c0e062c1f4512ddf/backend/internal/service/openai_excel_bps_recovery.go#L225)。
- [带代次检查的限流恢复](https://github.com/ranxi2001/sub2api/blob/d14ed1807bdfe43aabed1739c0e062c1f4512ddf/backend/internal/service/openai_quota_recovery.go#L10)。
- [参考前端的浏览器自动刷新设置](https://github.com/ranxi2001/sub2api/blob/d14ed1807bdfe43aabed1739c0e062c1f4512ddf/frontend/src/views/admin/AccountsView.vue#L1050)。

### 3.5 套餐截止日期还有独立查询来源

参考 `openai_privacy_service.go` 包含两个只读来源：

```text
GET https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27
GET https://chatgpt.com/backend-api/subscriptions?account_id=<目标账号>
```

前者解析对应工作区的套餐和 `entitlement.expires_at`，后者补充个人订阅的 `active_until`。`openai_oauth_service.go:311-339` 专门防止把个人套餐与另一个 workspace 的到期时间混配。普通和直接 BPS 授权都会尝试补充元数据，但失败是 best-effort，不能据此证明所有 BPS token 均可读取。

不能整个复用其 `enrichTokenInfo`：函数末尾还调用关闭训练数据共享的写操作。本项目应只实现元数据 GET 与身份校验，不改变用户隐私设置。

补充证据：[元数据读取和订阅解析](https://github.com/ranxi2001/sub2api/blob/d14ed1807bdfe43aabed1739c0e062c1f4512ddf/backend/internal/service/openai_privacy_service.go#L110)、[归属校验与隐私写操作边界](https://github.com/ranxi2001/sub2api/blob/d14ed1807bdfe43aabed1739c0e062c1f4512ddf/backend/internal/service/openai_oauth_service.go#L294)、[直接 BPS 新建账号](https://github.com/ranxi2001/sub2api/blob/d14ed1807bdfe43aabed1739c0e062c1f4512ddf/frontend/src/components/account/CreateAccountModal.vue#L6425)、[独立 BPS 凭据保存](https://github.com/ranxi2001/sub2api/blob/d14ed1807bdfe43aabed1739c0e062c1f4512ddf/backend/internal/repository/openai_excel_oauth_repo.go#L47)。

## 4. 产品范围与配置

### 4.1 推荐范围

首版提供：只读额度/状态查询、BPS 权限查询、套餐期限补充、单账号/批量手动更新、后台定时更新、开关、独立缓存和错误展示。现有 Sub2 状态只作独立来源的补充，不作为上游账号健康的替代。

首版不包含：模型生成探针、额度券消费、自动解除 Sub2 禁用、自动切换登录方式、自动补发另一 OAuth 客户端授权、数据库、队列服务或多用户平台。

### 4.2 配置建议

以下数值是本项目的保守工程建议，不是经上游确认的调用限额；在隔离验证和用户授权的少量实测后再调整。

| 配置 | 建议默认 | 行为 |
| --- | --- | --- |
| 启用账号查询 | 关闭 | 关闭时禁止新的手动和自动上游查询，保留已有缓存供查看 |
| 更新方式 | 自动 | 功能启用后可选“自动”或“仅手动”；仅手动不启动周期采集 |
| 更新间隔 | 15 分钟 | 预设 5/15/30/60 分钟；自定义整数 1–1440 分钟，校验在服务端 |
| 自动查询范围 | 有已保存授权的账号 | 未授权账号不登录；可对单个账号关闭自动查询，手动查询仍受总开关控制 |
| 查询时自动刷新过期授权 | 关闭 | 后续完成 refresh-only 安全拆分才提供的高级选项；不能进入完整重登或上传编排 |
| 包含重置券明细 | 关闭 | 可消费 credits 随核心额度返回；券明细作为可选读取，绝不调用 consume |

总开关、更新方式、间隔、查询范围和凭据刷新策略存到服务端；刷新网页、关闭浏览器、重启服务或换浏览器后保持一致。浏览器偏好只保存面板展开等纯 UI 状态。

配置写入遵循“验证 → 原子保存成功 → 发布新配置和重排定时器”；确定失败保留旧配置，不能让 UI 宣称已记住。`ErrDurabilityUncertain` 表示 rename 后持久性未确认，必须读回核对版本、显示存储不确定并进行有界恢复，不能假设磁盘仍是旧内容或盲目回滚。关闭请求可先停止当前进程采集以降低风险，但持久化未确认时必须明确提示“本次已停止，重启后的配置尚未确认”。并发设置保存使用配置版本检查，防止陈旧页面覆盖新值。

关闭总开关后取消待执行查询和在途信息读取，不删除历史授权与缓存；已经收到的刷新 token 必须先可靠保存，再结束操作，不能因开关变化丢掉一次性结果。

### 4.3 BPS 查询能力按实际证据处理

1. 用账号自身的 BPS grant 请求 BPS 模型权限，单独表达“权限信息可读取/受限/未知”。
2. 参考项目存在 BPS 主 token 调用 wham 的路径；本项目可在启用查询后，以同一账号自己的 token 做一次受控只读查询能力判断。不能改 client_id、借别的账号或另一工作区的 token。
3. 上游接受且身份一致时，标为“Codex 额度（当前授权查询）”。这个成功仍不能证明存在独立的 BPS 剩余积分池。
4. 明确 scope/权限不支持时标为“当前授权不支持此查询”，缓存能力结果并延长重试间隔；普通 401/403、Cloudflare HTML 或网络错误不构成永久“不支持”的证据。
5. BPS access 成功而 wham 失败时显示“BPS 权限信息已更新；额度暂不可查询”，不能标整个账号失效。
6. 若后续证据确认必须单独 Codex grant 才能查询，可设计独立的“补充额度查询授权”；须明确由用户发起、验证同账号同工作区、独立安全保存，不改变 BPS 主授权、浏览器偏好、上传身份。该扩展不作为首版的隐式行为。

## 5. 状态和数据模型

采用独立 `AccountInsightSnapshot`，不扩展或覆盖主任务状态机。允许各来源独立成功、独立过期：

| 数据块 | 核心内容 |
| --- | --- |
| 身份与版本 | 本地账号 ID、授权方式、内部授权代次、工作区绑定、查询配置代次、查询序号 |
| 最新尝试 | 查询 ID、排队/执行/完成时间、完整/部分/失败/跳过/取消结果、安全错误码 |
| 查询资格 | 是否可查、暂缓原因、下次允许查询时间；由后端统一计算 |
| 额度快照 | 来源、原始窗口时长、已用百分比、重置时间、允许/耗尽标志、额外功能窗口、可空 credits |
| BPS 访问快照 | allowed、可用/受限模型、安全原因，不推断生成实测通过 |
| 授权观察 | 主授权/查询授权各自的未过期、需刷新、查询拒绝、明确撤销或未知；注明依据 |
| 订阅快照 | 套餐、订阅范围、上游截止日期、来源与获取时间；保留手动到期最高优先级 |
| Sub2 状态快照 | 后端实例+远端账号 ID、远端状态、可调度开关、限流/冷却时间、观察时间 |
| 新鲜度和持久化 | 每个数据块自己的 observedAt/有效期；latestAttempt 与 lastSuccess 分开；持久化是否成功 |

约束：

- 未知值用缺失/可空字段，不用 0、false、空时间充当正常值；有效的 0% 需要展示。
- “剩余比例”只由有效已用比例推导；不把百分比换算成请求数、token 数或货币。图形可限幅，异常原始数值仍需保留诊断，不能静默伪造。
- 窗口重置时间到了，显示“预计已重置，待更新”；下一次成功查询前，不能擅自归零并宣称全部恢复。
- 查询失败保留最后一次成功的值和原观察时间，附“历史结果/已过期”。部分成功不能给所有字段套上新的时间。
- 授权方式、工作区或账号归属改变，旧身份数据不能继续显示为当前额度；晚到结果必须丢弃。
- 手动到期、订阅到期、token 到期、额度重置时间、重置券到期分别命名；Team 成员可能被移除，订阅日期不能保证成员持续可用。
- 不把新查询到的套餐直接写回现有自动修复策略；首版作为独立观察值。若未来同步 PlanType，需通过明确的身份匹配和现有策略更新入口，重新评估队列资格，避免旁路套餐排除保护。
- 同一来源成功响应中的显式 unknown 与查询失败区别处理：前者清除该来源已失效的字段，后者保留历史值；不同来源只在身份和套餐一致时合并。

现有到期列、查询详情与账号信息的日期需要一个共用的纯展示投影：`手动到期 → 当前身份且同订阅范围/套餐的最新可信查询值 → 已保存授权中的订阅日期`。可信查询明确返回 unknown 时，不再退回该来源已被否定的旧日期；只是查询失败则继续展示带历史标记的旧值。发现套餐或范围冲突时显示“到期信息待确认”，详情并列两份来源，不能拼出“旧套餐+新日期”。该投影不改写授权导出或历史 Job 元数据；手动日期编辑后仍立即优先。

## 6. 后台调度与故障处理

### 6.1 调度流程

```text
启用/修改设置/手动提交
  → 校验授权与操作资格
  → 同账号请求合并、限流与冷却
  → 捕获身份/授权/查询代次
  → 只读协议查询（每个来源独立结果）
  → 再次校验代次，丢弃陈旧结果
  → 原子保存非敏感快照
  → 公开缓存，计算下次到期时间
```

- 一套后台调度器，默认全局并发 2、同账号最多 1 个请求组。浏览器数量不影响上游请求数。
- 周期是目标频率，不保证每个账号同一时刻全部完成；采用按账号到期队列和小幅抖动，避免启动/恢复时集中请求。
- 定时、手动、授权完成后的刷新请求合并；任务运行较慢时只保留一次待刷新意图，不积累无限队列。
- 授权完成等生命周期触发同样受总开关、自动模式和单账号自动查询设置控制；仅手动模式只失效旧快照，等待明确手动提交，不因授权完成自动发请求。
- 一个账号失败不取消整批；批次分别统计成功、部分、失败、跳过、取消、待执行。受理/已排队不能计为成功。
- 账号导入没有授权时不采集；Free 账号已有授权可以只读查询，不因“查询资格”突破 Free 自动重登/上传排除。
- 遇到同账号认证、token 轮换、2FA、安全恢复、删除等操作，查询暂缓或取消；主业务操作优先，使用同一账号生命周期门禁。现有门禁按规范化 email 协调，同一邮箱的不同 job 也要纳入，不能只按 jobID 新建一把互不相关的查询锁。
- 重启只恢复设置、上次成功值和有效冷却期限；在途查询标为已中断，不立即全量重放。根据持久化的 nextDue 和启动抖动恢复。
- 休眠后或时钟前跳只补一轮到期查询，不重放漏掉的每个周期；异常未来时间要显式降级并重排。
- 关闭服务时根 context 取消、timer 停止、worker 退出、HTTP body/transport 释放，遵循现有关闭预算。

### 6.2 请求预算与重试

建议每账号整组查询总预算 30 秒：核心 wham GET 单次不超过 15 秒，BPS access 不超过 5 秒，可选元数据与券详情使用剩余预算。每个请求受同一整组预算约束，不能分接口重置总预算。

只读 GET 对符合条件的临时网络错误最多增加 1 次重试，带抖动；预算耗尽立即停止。429 尊重 Retry-After，进入调度冷却而不是即时连打；临时 5xx/网络异常指数退避，并设置上限。401/403/格式不匹配不套用网络重试。

使用现有 transport 的代理、TLS 和取消能力；请求目标固定到允许的 HTTPS 主机和路径，禁止携带认证头跟随跨主机重定向。账号 ID 作为受控参数编码，不能由远端响应拼成任意 URL。BPS 与 Codex 请求头分开构造，不混入另一身份的 Cookie/ticket。

时区沿用当前账号/服务已保存的请求时区，默认 America/Los_Angeles；时间持久化使用 UTC，界面按当前显示时区格式化。查询不能重写认证会话或临时改变代理来“试通”。

### 6.3 错误语义

| 观察 | 结果和后续 | 不能做的事 |
| --- | --- | --- |
| 正确身份的有效 200 | 更新该接口支持的字段 | 推断所有模型都能生成或账号永久健康 |
| 明确额度耗尽 | 显示对应窗口已用尽和重置时间 | 自动重新登录、自动上传 |
| 401 | 查询授权被拒绝；区分本地 token 已过期与明确撤销 | 直接标账号封禁、删除历史授权 |
| 403 | 权限受限/挑战/拒绝待判定；结合结构化错误与来源 | 所有 403 都等于 Team 被踢或账号封禁 |
| 429 | 查询被限流，显示 nextAllowedAt | 自动把剩余额度填 0 |
| 502/超时/断网/代理失败 | 保留历史快照，退避重试 | 覆盖主任务 lastError 或授权状态 |
| JSON 空字段/异常类型/HTML | 协议不匹配或部分信息未知 | 解码零值冒充正常 |
| 额度成功、券/套餐/BPS access 失败 | 部分更新，各自保留时间和错误 | 全部成功或全部归零 |
| 返回其他账号/工作区 | 丢弃该结果，提示身份不匹配 | 以邮箱相似为由拼接身份 |
| 本地保存失败 | 展示“已获取，未持久保存”；确定失败保留旧值，提交结果未知需读回核对 | 宣称已保存、无依据断言旧文件仍在、写进日志当备份 |

仅有可信的明确错误码和匹配身份才能显示撤销/成员不可用等判断；无法区分时保留“未知/需核实”。

## 7. 凭据、安全与现有业务隔离

- 本地 OAuth grant 当前保存在已提交的私有导出中，而非 `credentials.Store`；后者主要保存密码/TOTP/代理/邮件配置。应从 `internal/application/upload.go:33-58` 和 `internal/jobs/plan.go:52-80` 抽取小型私有授权 reader，保留 Lstat/SameFile、文件大小上限、类型/email/身份验证。不能假设密码凭据库里有 OAuth token，也不能另存一份查询 token 副本造成轮换不同步。
- 代理配置可沿用 `jobs.Manager.LoadCredentialsContext` 的关闭取消、凭据库和 2FA 恢复保护，只消费必要字段。不读取密码/TOTP 来模拟登录，不使用账号资料弹窗接口取得 token。日志和公开 DTO 不含 token、Cookie、账号秘密、原始响应体或凭据指纹。
- 查询凭据不能在业务全局锁内做磁盘/Keychain 读取或网络请求；短锁捕获快照，锁外读取，再以代次核验提交。不能仅靠“查询前看一次 idle”防竞态。
- 新增内部授权代次，所有初次授权、重授权、刷新、恢复完成、工作区/代理身份变更和删除入口统一更新；查询发起时捕获、提交时 CAS 校验，删除需 tombstone/记录不存在检查，防止晚到结果复活账号。
- 默认不轮换 token。高级刷新策略只在完成 refresh-only 业务拆分后开放：仅复用 `cmd/tosub2/refresh_recovery.go` 的凭据轮换、私有 journal、先保存新结果、本地补完和防覆盖原语，不调用可能回退完整重登/上传的 runner，也不通过 QueueOperation 改写主任务状态。读取 reservation 必须先释放，再按既有账号门禁取得刷新权限，不能持读权限等待自身升级造成死锁。新 token 先安全保存，再发布代次、导出或重新查额度；结果未知进入现有恢复边界，不无限重复刷新。暂不能安全拆分时首版不显示该高级选项，只提示“授权已过期，需要刷新”。
- BPS refresh 继续使用 BPS client 和 unified 端点；ChatGPT refresh 保持原客户端。辅助查询授权如未来增加，必须有独立 purpose/存储和恢复路径，不能覆盖主授权文件。
- 刷新失败不抹掉仍有效的已保存授权；查询成功也不能覆盖另一路刷新凭据明确失败、2FA 未完成或远端同步未知。
- 现有 CookieJar、设备、代理、TLS、OAuth 身份由现有会话装配规则保持；查询只借用所需的不可变身份/传输配置，不并发篡改进行中的认证会话。
- 不复用会执行网络探测或选择新代理的 `session_transport.go` 装配路径。查询 client 单独管理资源，保持已选择的传输身份；若必需的保存上下文不可用则明确暂不可查，不通过重登录补齐。
- `protocol/client.go` 存在 challenge replay，信息查询必须禁用自动挑战重放，不能因查询触发认证 worker。现有 transport 适配器有无界 ReadAll 路径，另一个重试助手又会丢弃非 2xx body；新查询适配层需对成功/错误 body 显式限长，仅解析允许的结构化错误码，关闭响应体，不能原样复用后声称状态解析完整。
- 从 Sub2 读取已有状态应按后端实例+远端 ID 绑定，不按邮箱把不同工作区结果强行合并。优先读取已有安全投影和缓存，不定时调用会刷新 token/启动 probe/恢复远端状态的 active usage 管理接口。
- 不调用 Sub2 clear-error、schedulable、分组迁移或上传接口。查询正常但远端仍是 401 时同时展示两者；修复仍由用户现有“重新授权并上传”流程完成。

## 8. 最小架构与持久化

保持一个 Go 服务和文件存储。下面是建议的新文件边界，不是要求为每个结构体创建接口：

| 位置 | 职责 |
| --- | --- |
| `internal/accountinsight/` | 类型、资格/状态规则、查询协调、定时器、快照与配置存取 |
| `internal/protocol/account_usage.go` | wham GET、窗口/credits 解析及身份检查；无业务状态写入 |
| `internal/protocol/bps_access.go` | BPS access GET 与权限/模型解析 |
| `internal/protocol/account_subscription.go` | accounts/check 和 subscriptions 的只读解析、归属匹配 |
| `internal/application/account_insights.go` | 连接账号门禁、安全授权读取和查询服务；可选受保护 token 刷新 |
| `internal/console/account_insights.go` | HTTP 输入验证、授权检查、安全 DTO；不承载协议/调度规则 |
| `cmd/tosub2/main.go` | 装配服务、注册授权生命周期通知、启动和关闭 |
| `web/src/hooks/accountInsights.js` | 当前页摘要及详情缓存读取、提交后的状态跟踪 |
| `web/src/components/account-insights*.jsx` | 紧凑摘要、只读详情、配置区与监控概览 |

仅在网络请求、时间/定时、持久化和已有账号访问边界引入必要的可替换依赖，便于 httptest/fake clock/故障注入；不引入通用工作流引擎。

`jobs.Manager` 当前没有可直接给外部查询使用的 lease。需要由 Manager 增加与现有 emailUse、导入、删除协调的短锁 reservation/代次快照机制：只在锁内登记，网络期间不持锁；主业务操作可取消查询并等待资源退出。不要直接把查询塞入 `QueueOperation`，其 `beginOperationState` 会改写账号当前操作状态。

现有 `console/monitor.go:225-297` 的单 goroutine、可唤醒 timer 和关闭模式可以参考；不能复用 `monitor.Check`，它会执行 RetryPending/StartRepair。现有 `jobs/auto_repair.go` 的按 attempt/operation/time/target 校验后提交是并发保护范例，但新服务仍需自己的身份与请求代次。

配置建议 `account-insights-settings.json`，带 schemaVersion 和配置 revision；快照位于独立 `account-insights/` 命名空间，以安全本地 ID 为键。复用 `internal/store` 的原子写、私有权限、路径校验和已提交结果错误语义，不在每次采样时重写全部 `job-meta.json`。

只存规范化的非敏感必要数据、最后成功结果和最近尝试；不保存无限历史，不保存完整上游 JSON。重试冷却/nextDue 应持久化但设置合理保存频率，单账号顺序写入，不用无序异步写绕过一致性。

`internal/store` 不负责协调多写者，新服务必须自行串行化同配置/同快照文件写入。使用其带目录同步和提交结果不确定语义的路径，不复制其他仅 rename 的较弱保存实现。服务在现有 `console.CloseContext` 关闭阶段退出，再进入 `manager.Shutdown`，共享总关闭预算。

旧版本无这些文件时默认关闭；未知高版本配置安全降级并保留原文件。回滚二进制忽略新目录，无需恢复旧账号元数据。彻底 clean 可随实例目录删除新增文件；只清网页设置不关闭服务端查询，界面需明确这一区别。

## 9. HTTP 接口建议

以下是拟新增接口，不是当前可调用接口；统一使用现有 console token、来源校验与错误响应约定：

| 方法与路径 | 语义 |
| --- | --- |
| GET `/api/account-insights/settings` | 只读设置和持久化状态 |
| PUT `/api/account-insights/settings` | 带 revision 保存设置；确定失败保留旧值，关闭请求和持久性不确定按第4节的明确例外处理 |
| GET `/api/account-insights?ids=...` | 仅读当前页指定账号的缓存摘要，ID 去重且有数量上限 |
| GET `/api/account-insights/:id` | 只读完整安全快照，不触发远端请求或读取密码/TOTP |
| GET `/api/account-insights/status` | 调度启停、下次运行、批次计数和存储/网络告警 |
| POST `/api/account-insights/refresh` | 提交单个/批量 ID，202 返回查询 ID、受理/合并/跳过原因 |
| POST `/api/account-insights/queries/:id/cancel` | 显式取消查询，不改变认证任务 |

手动连续点击按账号+授权代次合并；手动操作不绕过安全门禁、在途去重、冷却或总开关。未明确完成前按钮只显示排队/查询中。列表和详情 GET 绝不借“缓存未命中”执行上游网络请求。

## 10. UI 与交互设计

### 10.1 配置

在“服务配置”增加“账号查询”卡片，展示启用开关、自动/仅手动和间隔；高级刷新策略仅在安全拆分验证通过后开放。沿用草稿、保存/放弃、未保存离开保护；保存成功后才标“已保存”。说明自动采集由后台执行，关闭网页不停止；关闭总开关或停止服务可以停止采集，切到仅手动停止周期采集。

### 10.2 账号列表

表格现有宽度已较大，优先在账号区域增加最多两行摘要，而不是追加多列。例子均为合成展示：

```text
ChatGPT · Plus        Codex 5h 已用 42% · 7d 已用 18%
3 分钟前更新          [用量与状态]

BPS · Team            BPS 权限信息可读 · 额度暂不可查询
查询被拒绝 · 保留历史结果   [查看原因]
```

摘要明确标明 Codex/BPS 来源。使用统一紧凑条形图和状态标签；不靠颜色单独表达，不显示没有来源的“BPS 余额”。更多菜单提供“更新用量与状态”，与“刷新授权”“重新授权并上传”分开。批量操作同样使用后端资格结果。

推荐独立读取当前页的查询摘要，避免把详细窗口塞入每 900ms 返回的 jobs/selection；摘要接口使用 revision/ETag 或版本去重。查询面板可每 5 秒读取本地执行状态，空闲时减少读取；这是本地缓存刷新频率，不是用户设置的上游采集周期。

### 10.3 详情

独立“用量与状态”弹窗包含：额度窗口和重置时间、credits/可选券、BPS 模型权限、订阅到期、主授权/查询授权观察、Sub2 状态、各来源时间与最新查询原因。额度与重置时间不足时显示未知，失败时仍能看到带时间的历史值。

复用 DialogFrame 的 Escape/焦点圈定/关闭后焦点恢复。关闭弹窗仅取消前端读取，不隐式取消已受理后台任务；显式“取消查询”按钮独立提供。长邮箱、长套餐、长错误与 320/390px 窄屏需验收；数字条保留可读文本。

### 10.4 监控和一致性

监控页新增独立查询概览，不混入 Sub2 自动修复计数。顶部提示、账号摘要、详情和日志使用同一结果投影：已排队、部分更新、被跳过、查询失败、存储失败分别显示。重复周期失败仅在状态变化时提示一次，持续错误放在对应行和查询日志。

新增浏览器偏好如有，纳入 BROWSER_SETTINGS_KEYS 并测试存储失败；服务端配置不存 localStorage。

## 11. 验证与验收

本轮未实现功能，以下均为实施验收计划，不是已通过结果。

| 测试层 | 必须覆盖 |
| --- | --- |
| 协议解析 | 缺失/null/0、主次窗口换序、不同窗口长度、额外窗口、未知字段、credits 小数字符串/无限/缺失、券与套餐到期不混淆、畸形/超大响应 |
| 授权兼容 | ChatGPT、BPS-only、同账号不同工作区、BPS access 成功但 quota 拒绝、无 account ID、过期 token、请求头和代理隔离 |
| 时间与状态 | 绝对重置时间、相对秒数基准、UTC/上海显示、手动到期优先、窗口已到重置时间但未更新、明确 unknown 与网络失败 |
| 调度 | 开关/模式/间隔热更新、保存失败、singleflight、最大并发、慢响应、休眠/时钟跳变、冷却、重启无风暴、退出无 goroutine/timer 泄漏 |
| 竞态 | 查询时重登/刷新/2FA/删除/切工作区、旧响应晚到、同账号跨页重复、取消和批量部分成功 |
| 安全与失败 | 磁盘满/原子提交结果未知、凭据库不可用、跨域跳转、代理失败、401/403/429/5xx、日志/DTO 脱敏、刷新 token 已消费后保存失败 |
| 业务契约 | 未授权导入不登录、Free 排除不被旁路、历史有效授权不抹除、查询不上传/清除远端401、不发模型生成/consume/隐私写请求 |
| React | 多浏览器不会多次上游查询、202 不当成功、失败保留旧时间、配置刷新/重开保持、关闭面板不误取消、键盘焦点、长文本和窄屏 |

使用合成 token、httptest 服务、独立临时数据目录及可注入时间源。测试应记录服务收到的方法/路径，并断言没有授权码兑换、token 轮换（策略关闭时）、2FA、退出设备、上传、模型生成、消费券或隐私修改。

实施完成后执行格式/差异检查、`go test ./...`、`go test -race ./...`、`go vet ./...`、Go esbuild 前端构建、CGO_ENABLED=0 生产构建、显式认证 worker/恢复回归、既有跨平台构建和必要原生平台测试。交叉编译不算原生验收。

性能只做可重复测量：同环境、同响应延迟和账号规模（例如 100/1000 个合成账号），比较新增前后本地列表延迟、请求数、分配/常驻内存、goroutine、落盘频率及批次完成时间。未测量前不声称提升。

最终真实兼容验证仍需用户授权后进行少量只读查询；本方案不把源码测试或模拟服务成功说成真实 BPS/ChatGPT 账号已通过。

## 12. 分阶段落地与回退

1. **协议与契约保护**：先完成只读解析器、来源/身份模型和合成测试；覆盖 BPS 能力拒绝与各来源部分成功。此时功能仍关闭。
2. **查询协调和后台开关**：接入安全授权读取、账号门禁、独立配置/缓存、手动/定时 API、取消/重启/落盘故障测试。禁止默认自动刷新授权。
3. **UI 与一致性**：加入配置卡、摘要、详情和监控统计，完成 React 交互、焦点、窄屏及重复查询验证。
4. **兼容核验和交付**：全量回归与构建后，根据授权做少量真实只读兼容核验，明确 BPS-only 的实际可用范围，再决定是否需要辅助查询授权。仅在用户批准实施/部署后更新现有实例。

上线时默认总开关关闭；保留原二进制回退。部署前检查执行/排队/待同步任务，更新后核对服务健康、二进制和前端资源，不改账号/凭据/配置。关闭功能即可停止新增采集；回滚不需要用旧 job-meta 覆盖后续业务成果。

## 13. 已确认、待验证与本轮交付

**已确认**：参考项目的 wham/BPS access/订阅读取代码路径及两种凭据布局；普通 usage 和 BPS 恢复存在生成式探测；部分参考查询服务有状态写入；当前项目尚无独立采集模块且浏览器轮询不适合直接作为上游频率控制。

**待验证**：本项目保存的 BPS-only grant 对 wham/订阅端点的真实权限、不同套餐的字段覆盖、上游可接受频率和错误分类；这些不能通过源码推定。未找到参考项目实现独立 BPS usage/billing 协议，不能承诺其额度单位。

**本轮交付**：本设计文档及代码证据。没有修改运行代码，没有启用查询，没有运行账号登录/刷新/安全操作/上传，没有更新 4401 或推送 GitHub。
