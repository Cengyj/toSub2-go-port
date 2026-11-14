# 用量刷新操作生命周期（2026-11-03）

## 现象与证据

账号页有两个直接入口：顶部的“刷新全部账号用量”和工具栏的“刷新所选用量”。旧前端只在 POST 请求期间设置 busy，收到 HTTP 202 后立即解锁，后台查询仍可能排队或执行，因此用户可以继续点击。

后端原本已通过 `flights` 实现同账号去重，重复受理返回 `merged`；不能将按钮可连点等同于每次点击都访问上游。本轮保留该准入机制、HTTP 202、并发与队列上限，以及成功至少 5 秒、失败至少 1 分钟的现有冷却。

已确认的其他问题：

- `Snapshot.CanQuery` 原先没有投影真实在途 reservation，取消尚未释放或授权 revision 改变时，仅看最后一次尝试不足以判断能否再次查询。
- 原 `/status` 没有全量和所选范围资格；当前页摘要最多 100 个 ID，不能作为整个库存的完成信号。
- 新提交之前发起的旧 GET 可能在提交之后返回，必须防止旧的空闲状态提前解锁。
- 原轮询器可能丢弃旧 GET 期间发出的显式刷新：旧 GET 返回不可自动重试的 4xx 时，待执行的只读核对也被吞掉。
- 最终浏览器验收补充发现：首次状态读取就失败时，错误也必须记录所属 token/选择范围，否则未确认范围会被误显示为持续“核对中”。回归覆盖首次加载失败及只读恢复。

## 实施设计

### 服务端负责可刷新资格

`Status.refresh` 表示全部库存；请求 `/api/account-insights/status?ids=a,b` 时额外返回 `selection`。两者共用一次库存、时间和服务锁快照，包含 `total`、`eligible`、`queued`、`active`、`coolingDown`、`unavailable`、`retryAfterSeconds`，有冷却时附最早的 `nextAllowedAt`。未知所选 ID 计入 unavailable；去重后统计，原始分项最多 1000 个。

`Snapshot.CanQuery` 和聚合共用资格函数。真实 flights 优先于历史状态；取消请求已经确认、但 worker 尚未释放账号时，仍不可重复受理。它不改变登录授权、BPS 权限、2FA、上传或自动查询排除规则。

### 前端按范围和阶段呈现

| 阶段 | 按钮行为 | 含义 |
| --- | --- | --- |
| 可查询 | 可点击，保留全量/所选直接入口 | 范围中存在可受理账号 |
| 提交中 | 两入口同步阻止重复提交 | 共享 ref 阻止同帧双击和交叉点击 |
| 核对中 | 暂不可再次刷新 | 202 只是受理，需要提交结束后发起的 GET |
| 排队/执行 | 范围内按钮禁用并显示数量 | 服务端真实在途状态 |
| 冷却 | 禁用，不显示执行 spinner | 等待服务端确认冷却结束 |
| 状态未知/读取失败 | 禁用，保留最近结果，提供只读核对 | 不自动重放刷新 POST |
| 不可查询 | 禁用，说明原因 | 未启用、无选择、无资格或队列容量不足等 |

全量入口依据整个库存，因此任意账号在途时暂不可重复全量刷新。所选入口只看所选范围；另一个账号正在自动或手动查询时，不阻塞独立账号的合法所选刷新。改变选择后先读取新范围，旧范围响应不得覆盖。

短时 `actionBusy` 与刷新核对屏障分别维护，避免将后台查询等待误用于锁住“取消查询”。页面卸载清理请求与定时器；离线、隐藏页面暂停读取，恢复只执行 GET 并重新确认资格。

### 失败与兼容

- POST 超时、503 或无效受理回执视为结果尚未确认，随后 GET 核对；不自动重试 POST。
- 状态结构不完整或计数不一致不替换已确认状态，也不解锁按钮。
- 旧服务缺少新增聚合字段时安全禁用并提示核对，部署须保持前后端同一构建。
- 受理、合并、跳过仍分别展示；状态恢复为可查询不代表上一批成功。
- 不修改持久化数据格式、真实账号资料、授权结果、用户查询配置或冷却参数。

## 实现与回归位置

- `internal/accountinsight/projection.go`、`types.go`：一致的状态聚合和 CanQuery 投影。
- `internal/console/account_insights.go`：只读范围状态 API。
- `web/src/hooks/accountInsights.js`、`web/src/lib/insightRefresh.js`：提交屏障、范围资格、响应校验。
- `web/src/lib/polling.js`：显式读取刷新不被旧请求的自动重试策略吞掉。
- `web/src/main.jsx`、`components/shell.jsx`、`components/account-insights.jsx`：两个入口、清晰反馈、取消可用。
- `internal/accountinsight/refresh_availability_test.go`、`internal/console/account_insights_availability_test.go`：真实在途、冷却边界、并发去重、API 限制。
- `internal/console/frontend_insight_refresh_lifecycle_test.go`、`frontend_refresh_controls_test.go`：双击、旧响应、取消、错误恢复和 UI 契约。
- `internal/console/frontend_polling_explicit_refresh_test.go`：旧 4xx 不吞掉显式核对；隐藏与离线仍取消读取，恢复仅执行一次读取。

## 验收记录

所有测试使用合成账号、模拟服务和临时目录。浏览器使用 `internal/console/testdata/ui-fixture`，未触发真实账号查询、登录、授权刷新、2FA 或上传。

- 先红后绿：旧后端的在途 CanQuery、缺少范围资格、非法空选择；旧 UI 缺少禁用状态、可见说明及只读恢复入口。
- 浏览器：全量与所选分别快速双击，各仅增加一次刷新 POST；202 后保持排队禁用。
- 浏览器：取消查询可用，历史用量仍保留；关闭详情恢复原触发按钮焦点。
- 浏览器：其他账号在途时，全量禁用而独立所选账号仍可刷新。
- 浏览器：状态 503 禁用两个入口并保留用量；只读核对恢复资格。
- 浏览器：先受理再丢失回执时，显示错误并 GET 核对为排队，无 POST 自动重放。
- 浏览器：键盘 Enter 可正常提交；查询完成后捕获两个入口的冷却禁用状态，`aria-busy=false`，冷却结束后正常恢复。
- 浏览器：390px 窄屏、1440px 宽屏检查，页面无水平溢出；窄屏表格沿用自身横向滚动。

本轮新增 23 个回归测试函数，并调整旧用例的“202 后立即可再次提交”预期为“先核对最新服务端状态”。未删除测试、跳过失败用例或放宽业务断言。最后一项首次读取失败修复完成后，重新执行完整验证：

| 验证 | 结果 |
| --- | --- |
| `gofmt -l cmd internal scripts`、`git diff --check` | 通过 |
| `go test -count=1 ./...` | 通过 |
| `go test -count=1 -race ./...` | 通过 |
| `go vet ./...` | 通过 |
| `TOSUB2_LEGACY_RUNTIME_TEST=1 go test -count=1 -race ./internal/jshost ./internal/legacyruntime ./cmd/tosub2` | 通过；显式 worker 与本地协议恢复夹具 |
| Go esbuild 前端构建与 `CGO_ENABLED=0` 生产构建 | 通过，生产工具链 `go1.27.3` |
| `scripts/build-cross.sh` | darwin/arm64、darwin/amd64、linux/amd64、windows/amd64 全部交叉编译通过 |
| `diff -qr web/dist cmd/tosub2/web/dist` | 通过，两份嵌入资源一致 |
| 浏览器最终构建复验 | 首次 503 停止 spinner；全量双击仅增加 1 个 POST；两入口保持排队；无 console error/warn |

宿主 macOS arm64 执行了上述测试；交叉编译不等于其他平台原生验证。未执行真实账号/第三方服务验收、其他平台原生凭据验证，以及需显式 opt-in 的长周期、Keychain 和联网漏洞检查。本轮无依赖或工具链变更，未提出未经测量的性能提升结论。

截图使用合成资料：[宽屏排队保护](ui/usage-refresh-lifecycle-20261103-wide.jpg)、[窄屏直接入口](ui/usage-refresh-lifecycle-20261103-mobile.jpg)。测试服务与本轮创建的浏览器标签页均已清理，临时 viewport 已恢复；用户原有标签页未关闭。

## 运行和回退边界

本次不包含数据迁移。运行、升级、备份恢复、回退与卸载继续使用 [Go 部署说明](../go-deployment.md) 和 [macOS 本地运维说明](../local-macos.md)。真实服务更新必须确认维护窗口、排空后台任务并避免检查后新增操作；不能以测试服务验收替代真实实例部署。不得沿用历史轮次的维护窗口确认。

未删除业务代码、平台实现、恢复路径或测试。删除项仅为 Go esbuild 重建时替换的旧内容哈希 JS/CSS 文件；两份 dist 均由同一构建生成。另纠正本地运维文档中“启动不会自动登录”的过宽表述，明确恢复队列与启用的自动任务可能继续执行。

本轮未要求推送 GitHub；代码、测试和文档保留在当前工作区供审阅。

交付基线：分支 `main`，原 HEAD `bb5259c`；本轮修改尚未提交或推送。用户随后确认本轮维护窗口；2026-11-03 13:58（Asia/Shanghai）已按运维流程更新 4401，核对健康、实际二进制、全部静态资源与账号资料后恢复原查询和巡检设置。部署证据及回退边界见 [本地部署记录](local-deployment.md)。
