# 账号页右上角入口清理（2026-11-06）

按用户要求移除账号页标题右侧的回收站和刷新全部账号按钮。

## 删除范围

- 删除 `AccountsPageActions` 组件、主页面 import/render、`refreshAllInsights` 回调，以及桌面和移动端专属 CSS。页头不留下空操作容器。
- 删除仅由该按钮调用的前端 `useAccountInsights.submitAll` 入口，刷新提交统一接收明确账号 ID 列表。
- 原 `allRefreshControl` 重命名为 `refreshSummary`，只供概览展示全局查询状态；清除旧全量按钮的描述关联 ID 与操作文案。

## 保留范围

- 侧栏回收站、直接 `#trash` 路由、回收站返回账号及设置入口。
- 列表删除、批量删除、回收恢复和永久清理。
- 选中刷新、账号行/详情用量刷新、套餐胶囊刷新，以及原有资格检查、防重复提交和只读状态核实。
- 全部/所选推理探针、后台自动查询、监控与服务端刷新接口。

不修改真实账号数据或查询配置，不以真实账号请求验证界面。

## 验证与部署

- 全部前端回归通过：`go test ./internal/console -run TestFrontend -count=1 -timeout=120s`，5.612 秒；`go vet ./...`、`gofmt` 与 diff 检查通过。
- 合成 Chrome 验证 1440px、390px 及暗色模式，页头无按钮和空操作区，回收站侧栏/直接路由/返回正常，选中刷新仅提交勾选 ID，不包含 `all: true`。原行/详情/套餐/探针入口仍可用，无页面异常或外部测试请求。
- 2026-11-06 22:16（Asia/Shanghai）已实际更新 4401，PID `84443`；11 个账号的业务状态与原开关一致，健康检查正常，JS/CSS 与构建文件逐字节一致。
- 程序 SHA-256：`d956136e036ec532356baa839dfb7d45226ec89f9e40a8b831a9778bef43042b`。
- 旧程序和数据备份：`~/Library/Application Support/toSub2-local/6428e6fb3abbc4ae-legacy/backups/account-header-cleanup-20261106-221626`。
- 证据：`ui/account-header-cleanup-20261106-validation.json`、`ui/account-header-cleanup-20261106-deployment.json`，以及同前缀桌面/移动截图。
