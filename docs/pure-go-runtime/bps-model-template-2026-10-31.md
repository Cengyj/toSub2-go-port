# BPS 上传九模型模板（2026-10-31）

## 本轮契约与实现

用户最新提供的 Sub2 配置明确列出 9 个模型，并要求 BPS 上传按该配置处理。本轮只从附件提取模型名单与相关配置，不复制账号、token、Cookie 或其他真实身份资料；回归数据全部为合成数据。

精确名单及其他默认值见 [BPS 配置模板](bps-authorization.md#用户指定的-bps-配置模板)。该名单替代同日较早省略 `extra.openai_excel_bps_models` 的全模型兼容约定，保持附件顺序，不加入旧默认名单或动态推测模型。名单控制 Sub2 路由，不改变授权有效性、上游权限、free 排除及恢复资格检查。

唯一生产行为改动集中在 `internal/protocol/bps_settings.go` 的 `ApplyBPSSettings`：BPS 分支写入独立分配的九模型数组；切回 ChatGPT 继续清理 BPS 专属字段。`internal/sub2api/oauth_metadata.go` 仅更新旧注释。没有新增依赖、接口、远端请求或数据迁移。

| 路径 | 共用实现与边界 |
| --- | --- |
| 新生成授权导出 | `protocol.BuildSub2APIOAuthExport` 应用模板 |
| 新建与旧授权上传 | `BuildUploadPayload`、`decodeOAuthUpload` 规范化授权 |
| 已有账号更新 | `mergeOAuthExtra` 写入名单；保留远端模型映射、分组、代理、计费及未知扩展 |
| 巡检同步 | `SyncAccountCredentials` 复用相同合并规则 |
| token 刷新与恢复写回 | `updateRefreshedDocument` 应用模板，恢复流程复用该函数 |
| 历史文件下载 | 单账号与批量下载返回原保存内容；随后上传时套用当前模板 |

除模型名单外，已有显式配置（包括 `false`、`0`、自定义模式及组 ID）保持原规则；`defaults` 仍迁移为 `initial`。不会把模板名单加入通用 OAuth 凭据覆盖字段，避免重新授权误改远端 `model_mapping` 和 `model_mapping_mode`。默认请求时区仍为 `America/Los_Angeles`，保留显式配置。

## 回归与验证

先新增精确名单断言，确认旧删除字段实现导致定向测试失败，再修改生产实现。原“字段必须缺失”断言因用户最新契约改变而更新为精确数组断言；没有删除或放宽其他身份、历史授权、配置保留与生命周期断言。

- 协议回归：新导出、缺失／null／空／旧名单／JSON 数组／当前名单、跨账号数组隔离、重复应用幂等、ChatGPT 清理。
- HTTP 合成回归：4 种新建载荷、13 种配置分别经过上传与巡检的 26 个更新场景；精确校验名单和顺序，检查源对象不变、远端映射及其他设置保留。
- 刷新回归：新 token 元数据、现有 Sub2 设置、历史授权和最新名单同时保持；切回 ChatGPT 清理 BPS 配置。
- 独立只读审查确认新建、更新、巡检、刷新及恢复调用关系，没有发现本轮阻塞问题。

| 检查 | 结果 |
| --- | --- |
| `gofmt`、`git diff --check` | PASS |
| `go test -count=1 ./...` | PASS |
| `go test -count=1 -race ./...` | PASS |
| `go vet ./...` | PASS |
| `TOSUB2_LEGACY_RUNTIME_TEST=1 go test -count=1 -race ./internal/jshost ./internal/legacyruntime ./cmd/tosub2` | PASS，显式 worker、宿主与认证运行路径 |
| 显式 jobs 恢复测试：ManualRecovery / Restore / PrivateRecovery / VisibleQueuedMetadata | PASS，任务恢复与持久化保护 |
| `scripts/build-frontend.sh`、`CGO_ENABLED=0 go build` | PASS，由 `scripts/verify.sh` 调用 `scripts/build.sh` 执行 |
| `scripts/build-cross.sh` | PASS：darwin/arm64、darwin/amd64、linux/amd64、windows/amd64 |

整体验证使用固定 `GOTOOLCHAIN=go1.27.3 ./scripts/verify.sh`，退出码 0；交叉构建同样固定 Go 1.27.3。当前 macOS/arm64 执行了原生测试；其他平台仅交叉编译，不算原生功能验收。没有重新运行漏洞数据库扫描或 fuzz；本轮不改变依赖、解析器或网络传输实现。

本轮不修改 UI，不宣称重复完成浏览器视觉验收；未使用真实账号执行上传、登录、token 刷新、2FA 或模型生成测试。没有依赖升级、删除生产代码、性能优化或性能提升声明。

## 部署与回退

已于 2026-10-31 22:09（Asia/Shanghai）完成 4401 更新。用户明确确认本次维护窗口并暂停所有控制台/CLI 账号操作后，读取完整任务元数据，确认无执行中/排队/待同步任务及未知状态，且巡检凭据可持久化。保存原查询设置和巡检开关，再经现有 API 暂停两类调度，确认已保存且完全闲置后使用原生命周期脚本停服。这依赖用户确认的人工维护窗口，不代表旧服务已有原子维护锁。

- 原运行目录、数据目录及凭据 namespace 保持；停服后保存完整私有离线数据副本，0700 临时二进制经校验后原子安装。
- 已核对实际运行进程，仅监听原 `127.0.0.1:4401`，health 正常。实际执行路径、运行二进制与项目 `bin/tosub2` 一致，SHA-256 **cbca87083b1acff25a0403a940e934f213cdf5a2dadd477820eda5a23569ccdd**。
- 在线 HTML、`index-b23b97e48f05.js`、`index-632dc1c35c46.css` 与两份 dist 逐字节一致。本轮没有 UI 改动。
- 恢复自动功能前，离线副本与启动后的数据逐文件核对：只有 `job-meta.json` 的记录顺序/顶层更新时间、`sub2api-monitor.json` 的更新时间发生变化；按 ID 排序后每条账号记录字段一致，其余文件逐字节相同。
- 原查询设置完整恢复（revision 按协议递增）、巡检开关恢复，账号记录保持一致；最终无执行中、排队或待同步任务，查询与巡检闲置。恢复原自动功能可能立即执行已有策略；没有手动真实账号操作作为验收。
- 回退材料保存在私有运行目录，本文使用占位路径 `<runtime-root>/<private-backup-dir>/`：旧 `tosub2`（0700，单链接）、原设置 `bps-model-deploy-settings.json`（0600，单链接）及 `data-before-update/`（0700）。旧二进制 SHA-256 **9d88a1571dd68ab7c7a34147ace851310dcc85162c7e9b127779deaca6f04884**。离线数据副本包含暂停后的调度开关，原开关另存于设置备份；具体位置仅保留在被 Git 忽略的本地运维记录中。

已有 Sub2 账号会在后续上传或同步时采用新名单，不主动批量上传作为验收。临时 Go 维护工具与数据比较文件已删除，未创建测试服务；私有回退材料保留。此前未提交成果全部保留，当前分支仍为 `main`，本轮未暂存、提交或推送。

无需数据格式迁移。回退仍需维护窗口和暂停调度；停服后将本轮保留的旧二进制通过同目录私有临时文件原子替换，使用 `TOSUB2_LOCAL_REBUILD=0 TOSUB2_LOCAL_PORT=4401 ./scripts/local-legacy-macos.sh start` 启动，再恢复原设置。不要用离线数据副本覆盖后续更新的授权、凭据、配置或 checkpoint。构建、运行、恢复与卸载见 [部署说明](../go-deployment.md)。
