# 使用现有授权刷新订阅套餐

日期：2026-11-06（Asia/Shanghai）。

## 使用入口

账号列表和查询详情中的套餐胶囊本身就是刷新入口，不再显示独立“刷新套餐”按钮或“上次查询”时间行。胶囊保留套餐名称与轻量刷新图标，支持鼠标及键盘操作；查询中图标旋转且禁止重复点击，暂不可查询时显示禁用状态及悬停原因。单击只查询该账号，不要求启用用量查询总开关，不新增批量或后台自动查询。成功后立即更新套餐；查询时间与来源继续保存在业务数据中。设置、套餐选项等无刷新回调的标签仍为静态胶囊。

接口为 `POST /api/jobs/{id}/refresh-subscription`，请求体严格为 `{}`。它读取本任务已提交的 `sub2api-import-oauth.json`，不会接受调用方传入 token、代理或登录选项。成功返回 public job、已核验 subscription 及安全 warnings。GET 账号列表及页面轮询只读本地状态，不触发上游请求。

## 查询与授权边界

- 复用现有授权读取和账号查询 lease，核对邮箱、工作区、授权方式、文件完整摘要、私有文件与固定代理。账号操作进行中、2FA 待恢复、授权轮换结果尚待本地提交时拒绝查询。
- 使用已有 access token，只调用项目现有订阅白名单的 GET 接口；不查询用量、BPS、额度重置，不发起登录、邮箱验证或 OAuth token refresh，也不改写授权文件。
- access token 已知过期时在联网前停止。查询失败时套餐显示 `Unknown`，请求状态沿用用量区的单条最新记录；存在授权文件不代表它必然具有订阅查询权限。
- 旧 token 的套餐 claim 可能已过时，不作为实时套餐的身份约束。允许同一已验证工作区的 Free→Plus、Plus→Free 等变化；身份不符、两个实时来源明确冲突、未知套餐名称不能当作有效更新。
- 已从账号接口核实套餐，但可选到期日期查询失败或未给出可靠日期时，仍可更新已确认套餐，辅助请求异常进入统一请求状态；不继承另一套餐的日期，也不把 token 过期时间当订阅到期时间。

## 保存与显示

账号查询 lease 持有到结果核验和本地提交结束。应用层查询前后校验授权快照，Manager 提交时在锁内再次核验 lease、隔离状态、授权文件 SHA-256 及工作区。授权或账号已变化的查询不落盘，也不启动新的认证任务。

成功保存 `planType`、独立订阅日期及公共来源字段：

- `subscriptionPlanCheckedAt`：本次实时查询的观察时间。
- `subscriptionPlanSource`：`chatgpt_accounts` 或 `chatgpt_subscriptions`。
- `subscriptionPlanStale`：原观察不再对应当前有效授权时标记。

操作历史、登录次数、人工到期时间、密码、2FA 密钥和授权文件均不因套餐查询而修改。授权摘要和工作区绑定只保存在私有 metadata，公开 job 不包含这些绑定或任何 token。

相同授权的实时查询结果在重启后优先于旧 claim。授权替换并采用新快照时清除旧查询的来源和时间，避免把未查询的新 claim 贴上旧的实时查询日期。授权文件缺失但保留历史观察时，保留其原来源与日期并标记历史。

查询失败时，列表套餐标签及详情显示英文 `Unknown`，套餐下方不再铺失败或辅助警告文案；用量区仍只显示最新一个请求状态。最后核验的 `planType` 保留在业务数据中，不为显示未知而改成 Free 或覆盖原套餐，避免影响上传映射及套餐规则。普通 metadata 写入失败回滚；文件已替换但 fsync 结果不确定时返回 `503`、`outcome: unknown`，本次界面同样显示 `Unknown`，只重新读取本地账号，不自动重发查询。

已持久化的当前套餐失败记录在刷新页面后仍显示 `Unknown`；后续已核验成功恢复套餐标签。展示比较保留纳秒时间精度，并排除旧授权/旧操作的失败。来自同次成功查询的辅助端点失败不会覆盖已核验套餐。网络中断或本地持久化彻底失败、没有留下服务端记录时，仅能保留当前页面的未知状态，不能保证刷新后恢复未保存的诊断。

前端不会用更旧的 insights 订阅日期覆盖更新的套餐观察，即使新结果没有可用到期日期；旧请求返回时也不能覆盖后来已更新的账号状态。

## 验证范围

测试只使用合成账号、可控传输和本机代理陷阱，不读取真实账号授权，不调用真实上游服务。

- `internal/protocol/account_subscription_refresh_test.go`：升级/降级、只读端点、身份和实时来源冲突、未知字段、401/403、部分失败、敏感字段不外泄。
- `internal/application/subscription_refresh_test.go`：真实授权加载到协议再到持久提交、授权字节不变、过期/失效/替换拒绝、调用方及 lease 取消。
- `internal/jobs/subscription_test.go`：重启和旧 claims、授权替换、lease 失效、存储失败及已发布但持久性不确定、人工日期和操作历史不变。
- `internal/console/subscription_refresh_test.go`：HTTP 方法和空对象限制、无有效授权/过期/隔离/排队/待保存轮换全部零网络、公开字段与本地缓存语义。
- `internal/console/frontend_subscription_refresh_test.go`：单击/双击、独立开关、查询中、失败/取消、即时更新、历史来源、旧缓存与延迟响应、保存不确定性。

初次开发验证没有重启服务；后续按用户明确要求更新了 4401 实例，见下方部署记录。

## 最终验证结果

2026-11-06 完成以下验证，全部通过：

- `go test ./... -count=1`：全仓测试通过。
- `go test -race ./internal/protocol ./internal/application ./internal/jobs ./internal/console -run 'Subscription|AccountQuery|Insight|Plan' -count=1`：订阅、账号查询及关联回归的竞态检查通过。
- `go test ./internal/console -run '^TestFrontend' -count=1`：全部前端测试通过；后续文案与来源展示调整的对应回归也通过。
- `go vet ./...`：静态检查通过。
- `./scripts/build-frontend.sh`：前端构建通过，两处嵌入资源同步更新。
- `go build -o <临时目录>/tosub2 ./cmd/tosub2`：完整程序构建通过，输出位于临时目录，未替换或重启运行中的服务。

以上结果验证的是模拟协议响应、持久化及界面行为；未使用真实账号验证上游是否接受其现有授权，实际查询仍受 token 有效期和权限约束。

## Unknown 显示与 4401 部署

2026-11-06 21:52（Asia/Shanghai），按用户要求完成实际升级：

- `jobs`、`application`、`console`、`cmd/tosub2` 受影响包完整测试通过；仅测试账号锁目录通过 overlay 隔离。`go vet ./...`、前端构建、两处 dist 一致性及 diff 检查通过。
- 真实 Chrome 合成数据验证 1440/390 宽度：失败立即 Unknown、重载保留已持久化失败、详情一致、后续成功恢复 Plus；用量始终仅最新一条 HTTP，旧状态被替换；无真实账号测试请求。
- 已停止原 4401 进程、备份旧二进制及 data/launchd、安装包含新静态资源的程序，并用原脚本恢复服务。原账号数据和监控/查询开关保留。
- 服务 PID：`82074`；11 个账号的业务状态及保存套餐在升级前后相同，健康检查正常，实际服务 JS/CSS 与构建产物逐字节一致。已启用监控随原启动流程恢复。
- 程序 SHA-256：`b849b13d33da1ae3f1ae2bd42988a32f3eff7acee60c80fa4358f6d9315370bc`。
- 备份：`~/Library/Application Support/toSub2-local/6428e6fb3abbc4ae-legacy/backups/subscription-unknown-20261106-215201`。
- 视觉验证：`ui/subscription-unknown-20261106-validation.json`；部署校验：`ui/subscription-unknown-20261106-deployment.json`。

## 套餐胶囊刷新入口更新

2026-11-06 22:02（Asia/Shanghai），按用户补充要求，将账号行和详情中的套餐胶囊改为原生按钮，移除独立刷新按钮及查询时间行。胶囊保留套餐名称，小图标在查询中旋转；忙碌或不允许查询时禁用，键盘焦点可见。其他用途的套餐标签继续使用静态 span。后台请求、去重、Unknown 显示及用量最新一条状态规则不变。

- `go test ./internal/console -run '^TestFrontend' -count=1 -timeout=2m`、`go vet ./...`、`git diff --check` 全部通过，前端构建及两份 dist 一致。
- 真实 Chrome 合成数据覆盖 1440/390 × 明暗主题，验证点击、Enter/Space、查询中防重复提交、失败 Unknown 与成功恢复、静态标签；未对真实账号发起测试请求。
- 4401 已备份并实际更新，PID `83215`，11 个账号状态及原有开关保持一致，实际 JS/CSS 与构建文件逐字节一致。
- 程序 SHA-256：`102536d8e3a3cada64559c8915b3ac1b9061284260784412584a35ebb3d4fe6b`。
- 备份：`~/Library/Application Support/toSub2-local/6428e6fb3abbc4ae-legacy/backups/subscription-capsule-20261106-220221`。
- 校验记录：`ui/subscription-capsule-20261106-deployment.json` 与 `ui/subscription-capsule-20261106-validation.json`。
