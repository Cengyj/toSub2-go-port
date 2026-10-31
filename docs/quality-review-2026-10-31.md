# 2026-10-31 系统质量优化

## 基线与边界

基线为 `main`、提交 `1a029e7`，开始时工作区干净；以全部本地内容为依据，没有 checkout/reset、提交、推送或外部发布。根目录、父目录及项目子目录没有发现 AGENTS.md。Go 工具链为 `go1.27.3 darwin/arm64`，保持 go.mod 的版本、受控 vendor、Go esbuild、Sobek 与同一二进制认证 worker。原始功能参考目录本次不存在，使用当前代码、既有协议夹具和已确认规则理解业务；CLIProxyAPI 仅用于视觉参考。

首轮 `go test ./...` 通过（含缓存），不以此替代最终非缓存回归。仅对已有 4401 实例执行无副作用健康检查；测试与 UI 验收使用合成账号、模拟服务和独立目录，不读取真实凭据、HAR 或私有配置，不触发真实登录、刷新、2FA、退出设备或上传。

## 功能矩阵

| 现有能力 | 核心实现 | 数据与副作用 | 验证依据 | 边界与当前缺口 |
|---|---|---|---|---|
| 入口、依赖、关闭 | `cmd/tosub2/main.go`、`cli.go`、平台 service | listener、manager、监控、查询、worker；共享取消预算 | lifecycle/process/CLI tests | 无原子维护期封锁，自动部署不能仅凭空闲快照 |
| 纯 Go 授权 | `authentication_runtime.go`、`legacyruntime`、`jshost`、`protocol` | CookieJar、代理/TLS/设备/OAuth 身份及 checkpoint | production fixture、identity/transport tests | 合成协议不代表当前线上 challenge 可用 |
| 导入/重复资料 | `console/batch_parser.go`、`server.go`、`jobs` | 只保存资料，不登录 | batch/import/expiry tests；本轮部分导入回执 | 中途失败不保证整批事务回滚，明确已完成范围 |
| 重新授权/刷新/恢复 | `application`、`saved_grant.go`、`refresh_recovery.go` | 先保存一次性 grant，再导出与同步；保留历史授权 | saved-grant/refresh/recovery tests | recovery 自身无法落盘仍需人工保护已取得结果 |
| 授权方式与偏好 | `authorizationMethod.js`、`authorization_method.go`、jobs DTO | 浏览器手动偏好与工作流身份分离 | frontend/authorization-method tests | 浏览器存储失败不得宣称已记住 |
| 2FA 工作流 | `application/totp_workflow.go`、`jobs/totp_*`、`protocol/totp_*` | 保存新密钥→确认退出设备→重登→上传 | workflow/partial-recovery tests | 无真实账号破坏性验收；本轮修正密钥只读轮询 |
| 队列/账号锁/取消 | `jobs/admission.go`、`manager.go`、`restore.go` | 先持久化后入队，同账号 lease；恢复证据保留 | concurrency/admission/recovery/race | 不为形式异步化锁内顺序，不宣称性能提升 |
| Sub2 新建/更新 | `sub2api/upsert.go`、`upload_outcome.go`、`application/upload.go` | 去重、详情合并、授权方式字段清理、未知结果 | upload/merge/outcome tests；本轮精度/空期限回归 | 无跨客户端 ETag/事务，远端并发修改仍可能竞争 |
| 巡检与自动恢复 | `console/monitor.go`、`sub2api/monitor.go`、`accountplan/policy.go` | 冷却、套餐否决、排队、pending 同步 | monitor/policy/transfer tests | 本轮保护管理员明确停用/暂停状态 |
| 到期、用量、权限 | `accountexpiry`、`accountinsight`、`protocol/account_insights*` | 用户到期优先；各来源、窗口、时间分开 | expiry/parser/cache/observation tests | 查询权限与可续期 OAuth 授权不是同一状态 |
| HTTP 与存储 | `transport`、`privatefile`、`store`、`credentials` | 有界读重试、单次副作用、原子替换、平台 vault | network/fault/permission tests | Linux 凭据仍为内存；平台原生验收与交叉编译分开 |
| 前端与交付 | hooks/components、Go build-web、CI、scripts | 受控轮询、草稿、偏好、双份一致 dist | Sobek/HTTP 回归及实际浏览器验收 | 不把构建通过等同于视觉/交互验收 |

## 已确认问题与实施

1. **上传精度损失**：应用层读取保存授权、Sub2 payload 再解码、远端详情读取使用 `json.Unmarshal(any)`，未知 JSON 数字经 float64 舍入并回写。合成 HTTP 回归先重现大整数及高精度小数变更，再改为 `UseNumber` 并继续拒绝第二份 JSON/尾随垃圾。保留外部可扩展对象，无格式迁移。
2. **巡检恢复越过管理员状态**：延后同步无条件清错并恢复调度。按现有手动上传语义，只对明确 `error`（及旧接口缺失状态的兼容路径）执行恢复；`disabled` 与显式 `active` 的暂停设置保留。
3. **未知订阅期限被拒绝**：已有合并器支持 `subscription_expires_at:null` 清除旧期限，入口统一字符串校验却拒绝它。只对该可空字段保持现有语义，不放宽 token/身份字段校验。
4. **部分导入没有回执**：逐条保存后发生错误，仅返回通用 409，先前保存结果未交代。本轮回传已完成、失败、未知、未处理和中断位置；保留 HTTP/错误兼容，不回滚、不登录、不自动重试。
5. **2FA 密钥轮询静默无限重试**：账号行独立每秒 GET，不受后台/离线暂停约束，HTTP 拒绝不可见。复用只读轮询 hook，明确读取失败和用户重试，不改变 2FA 修改步骤。
6. **缓存损坏被误当未查询**：启动读取查询缓存失败、版本不支持或账号身份不一致，原来均静默忽略。现在区分缺失文件与已有缓存不可读，呈现 `cache_unavailable`、保留原文件，不发布部分数据；显式查询成功后恢复。前端显示“历史缓存不可读／本地缓存”，不伪造上游失败或 0 用量。
7. **导入同帧重复提交**：React 状态更新前可连续发出两次 POST。新增同步提交锁；部分失败合并已确认保存的账号并刷新只读列表，保留原输入，不自动重放。
8. **表格空余与长错误布局**：基础列宽由 1490 调为 1240，额外空间分配至信息列，空表取消强制宽度；实际浏览器发现长错误摘要撑高整行后，限制折叠摘要三行，展开保留完整诊断；时间可换行，四字操作标签保留可读宽度。
9. **查询来源状态泛化**：原“查询部分更新”不能区分新观察和保留旧值。按各来源观察时间、本次开始/结束时间及错误显示“已更新／未更新／保留历史结果／结果不完整”；缺少时间证据或整体过期时不宣称本次成功。详情保留实际错误，去掉重复的固定泛化提示。原文案断言随展示契约更新，并新增九种时序/来源组合，历史数值和无副作用断言保留。

### 保持的资格契约

BPS 的 HTTP 成功、token 结构与导出身份分别由 `protocol/oauth.go` 校验；独立的 `bps_access` 查询才提供模型权限观察，区分 true/false/unknown、身份错误、无效结构及 HTTP 失败。全模型兼容模板不参与权限判定。Free 排除是当前自动修复策略，巡检筛选、入队、工作流执行及回传前均检查；手动首次授权、重登和上传仍允许 Free（`console/monitor_transfer_plan_test.go` 明确保留此行为）。未把未知套餐当 Free，未把“保存了可续期授权”表述成“上游模型权限已验证”。若要将手动流程也强制按 allowed/Free 阻断，需要新的产品契约，本轮未擅自改变。

## 风险与待测量假设

- 已确认部署缺口：无维护期 admission gate；检查空闲至停服可新增任务。遵照数据安全要求，本轮不以连续空闲读取或直接 restart 冒充消除竞态。
- 潜在风险：读取远端详情后，其他客户端仍可修改 Sub2 配置；需要远端 ETag/事务能力才能全面解决，本轮只修正本进程已确认的错误副作用。
- 待测量假设：锁内磁盘/vault 延迟可能影响大批量提交吞吐，未做同条件前后测量，不据此改异步提交、不宣称提速。
- 待独立验收：当前线上认证协议、真实账号权限、Windows 原生 DPAPI/服务及 Linux systemd 部署；合成测试和交叉编译不替代它们。

## 架构与删除依据

沿用已有应用层和单体 Go 边界，没有将认证身份链路搬迁或引入框架。上传应用层负责验证已保存文档并保持精度，Sub2 集成层负责外部 JSON 与管理状态，Console 通过类型化 DTO 表达部分导入，查询服务负责缓存恢复语义；前端的 2FA 读取 hook 复用已有调度器，私有密钥不进入共享账号缓存。没有修改数据 schema、依赖版本、凭据 namespace 或授权方式默认值。

- 删除账号行中原 35 行独立 2FA 读取循环：被有错误表达、取消及后台暂停的公共调度 hook 替代；功能与按钮保留。
- Go 前端构建器替换两份 dist 中旧 hash JS/CSS：HTML 引用与新资源逐文件核对；vendor 和许可证保留。
- 更新过时的 BPS 模型名单文档，改为已确认的字段缺失语义；macOS 操作文档使用示例路径和 project-hash 占位符。历史验收报告未作为现状重写。
- 本轮临时 UI 夹具、生产 smoke 数据、测试日志、Windows 测试程序及临时 Docker 镜像在摘要提取和停服后清理。回归测试、合成截图、正式生产/跨平台产物保留。未删除测试或业务路径。

## 验证结果

精确命令、各套件计数与 skip 清单见 [机器可读摘要](pure-go-runtime/quality-validation-2026-10-31.json)。所有最终测试使用 `-count=1`。

| 验证 | 结果与边界 |
|---|---|
| `gofmt -l cmd internal scripts`、`git diff --check`、交付脚本 `sh -n` | 通过 |
| `go test -json -count=1 ./...` | 20 个测试包、1151 个顶层测试通过，0 失败；build-web 无测试单列 |
| `go test -json -race -count=1 ./...` | 20 包、1151 个顶层测试通过，0 失败 |
| `go vet ./...`、`go mod verify` | 通过 |
| `GOTOOLCHAIN=go1.26.0 CGO_ENABLED=0 go test -json -count=1 ./...` | 最低版本兼容：20 包、1151 个顶层测试通过，不代替生产安全工具链 |
| 显式 Sobek/worker/生产登录与恢复夹具（race） | `TOSUB2_LEGACY_RUNTIME_TEST=1`，3 包、191 个顶层测试通过 |
| Go esbuild、`make build`、`CGO_ENABLED=0` | 通过；重复构建资源一致，web/dist 与嵌入 dist 一致 |
| `scripts/build-cross.sh` | darwin arm64/amd64、linux amd64、windows amd64 通过；这是交叉编译 |
| Windows 测试程序 | credentials/privatefile/jobs/accountinsight/sub2api 五包 amd64 编译通过，未原生运行 |
| macOS Keychain 原生（race） | 独立合成 namespace，保存读取、旧凭据合成导入、跨进程重启三项通过并清理账号 |
| Linux/aarch64 原生隔离夹具 | 10 个顶层测试通过、0 skip；scratch、只读根、禁网、UID 10001；不是生产 Docker 镜像验收 |
| 解析器 fuzz | 列表 5 秒 194869 次、创建回执 5 秒 187950 次，无失败，2 workers |
| `govulncheck@v1.9.0 ./...` | 退出 0；0 可达符号级、0 已导入包级，2 项仅模块级提示；不是所有依赖无漏洞 |
| 生产二进制独立 smoke | 空合成目录启动，health 成功，在线 HTML/JS/CSS 与最终构建一致；SIGTERM 正常退出 0 |

集成期间首次全仓运行曾在新增 `frontend_insight_errors_test.go` 的字符串边界报语法错误，已修正并重新执行最终全仓普通/race/最低版本测试，未跳过该包。针对实际业务缺陷的新增测试均在修复前复现失败，修复后通过。没有性能前后测量，不宣称提速。

### 跳过与未验证

普通套件共 13 个 skip；其中 Keychain 三项和生产认证恢复已经显式补跑，子进程 helper 是被父测试启动的入口，不能单独算独立通过。精确名称保留在摘要中。

- 当前公开 SDK 夹具未提供，`TestPublicSDKInitialization` 未执行；不使用真实 HAR 替代。
- 本轮未重复 opt-in 五分钟巡检、65 秒 HTTP、31 秒 transport 长时测试；既有普通/race 中的预算、取消和关闭回归通过，不把历史长测计入本轮。
- Windows 原生 DPAPI/DACL/SCM、Linux systemd 安装、完整生产 Docker 镜像、远端 CI 未执行。
- 未执行真实账号登录、刷新、权限调用、2FA、退出设备或上传；实际线上协议和套餐权限仍须独立受控验收。
- Chrome 连接器不可用，实际 UI 验收改用 Codex 内置普通浏览器；未声称通过 Chrome/Safari 的跨浏览器验收。真实浏览器存储配额异常未注入，现有生产 JS/Sobek 回归覆盖存储失败与清理键。

## 实际 UI 验收

使用独立纯 Go 合成服务和最终前端资源；服务没有真实账号、持久凭据或上游连接。

- 1600px 视口、侧栏展开：表格容器与滚动宽均 1273px，无多余横滚；1440px 收起侧栏后均 1269px。
- 390/320px 窄屏：页面不横向溢出，1240px 表格局部横滚；390px 时键盘右方向键使表格 scrollLeft 达 80px，页面不横滚。
- 长邮箱省略但完整标题/动作身份可读，长错误折叠摘要 51px（三行），详情保留完整内容；多窗口、未知窗口及 HTTP 403 位于用量/重置时间语义下，旧额度不会归零。未识别套餐使用明确未知状态，未伪装成 Free。
- 密钥读取 403 显示错误与“重新读取密钥”；明确点击后读取成功，未重置 2FA。验收请求计数与手动重试/刷新次数一致，没有静默无限请求。
- BPS 偏好刷新及重开后保留，2FA 后重登弹窗读取同一偏好；Tab/Shift+Tab 焦点在弹窗内循环，Escape 关闭并恢复到原按钮。
- 合成批量导入返回一条已保存、第二条失败、后一条未处理；UI 展示回执并保留三行输入，没有自动重交。
- 暂停合成服务器引发真实读超时，UI 明示连接中断并保留 4 行账号及历史用量；恢复服务器后自动回到已连接。测试未产生账号变更请求。
- 暖灰/深色及刷新验证完成；网页配置清理后手动授权回到 ChatGPT 默认。只清理合成 origin。

截图：[宽屏](pure-go-runtime/ui/quality-20261031-wide.jpg)、[窄屏](pure-go-runtime/ui/quality-20261031-mobile.jpg)。截图仅包含合成数据。浏览器临时视口已恢复、测试标签页已关闭。

## 部署、回退与最终状态

**4401 未更新。** 现有服务健康，运行 PID 77230；安装二进制 SHA-256 为 `c2b4d07dbea76a8516cc4bd392cad5ea303358b2735debc561905c48dc08300e`，新产物为 `b92e93e6474bfa1627afae5e218ada7d27d72fe7e8e4f4769956da5bc6dd67ec`。两者及 HTML 摘要不同，确认未把构建完成误报为已上线。没有读取真实 token/账号列表、改动真实数据或重启既有服务。

阻塞是现有服务没有原子维护期入队封锁，生命周期脚本锁只协调脚本之间的启停。保留 `bin/tosub2` 和四平台产物供受控维护窗口使用；部署前按 [安全升级](go-deployment.md#可重复验证与安全升级) 保留当前二进制及私有数据备份。上线后如健康或资源不一致，停止新进程，恢复旧二进制，以同一用户、输出目录和凭据 namespace 启动；本轮没有数据迁移。本次未部署，因此当前运行实例不需要回退。

所有代码在原 `main` 工作区增量保留，未暂存、未提交、未推送。临时服务已停止，原生隔离容器/镜像和测试目录清理后仅保留交付文件。完整运行、配置、备份、恢复、回退、排障及卸载入口为 [部署说明](go-deployment.md)、[macOS 生命周期](local-macos.md) 和 [README-Go](../README-Go.md)。
