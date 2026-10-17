# 2026-10-13 代码质量与架构升级

## 基线与范围

工作区初始 `git status --porcelain=v1` 为空，基线提交 `2b9591d`。本次直接在该工作区增量修改，没有 checkout/reset 覆盖本地内容，没有推送或发布外部服务。基线工具链为 `go1.27.1 darwin/arm64`；安全扫描后将生产工具链固定至 Go 1.27.2，并按修复范围升级四项依赖。模块最低 Go 版本仍为 1.25.0，数据格式未迁移。

原功能参考 `/Users/ceng/Downloads/toSub2-main` 在本次环境中不存在。当前实现、既有协议夹具和回归测试是主要契约来源；没有因无法对照原项目而退回旧实现。CLIProxyAPI-main 作为界面设计参考，未导入其服务架构。

纯 Go 主链路：`cmd/tosub2` → CLI/Console 装配 → `jobs.Manager` → `protocol` → Go HTTP/TLS + `legacyruntime` → 同一二进制的 `jshost.WorkerMain`/Sobek。React 使用 `cmd/build-web` 的 Go esbuild API 和 `web/vendor`，两份 dist 嵌入同一服务。`legacyruntime`、`jshost` 和 worker 均是保留的生产组件。

## 功能矩阵

| 现有能力 | 核心实现 | 数据/副作用 | 已有验证及本次保护 | 边界/缺口 |
|---|---|---|---|---|
| 启动、CLI、HTTP、关闭 | `cmd/tosub2/main.go`、`cli.go`、平台 service | 监听端口、子进程、关闭持久化 | lifecycle/process fixture、worker 显式回归 | Linux/Windows 服务原生验收需目标机器 |
| 导入、账号合并 | `console/batch_parser.go`、`jobs.Manager` | 私有资料、metadata；导入不认证 | 原格式/重复导入测试；新增 parser fuzz/基准 | 不读取真实账号文件做测试 |
| ChatGPT/BPS 授权与恢复 | `protocol`、`authentication_runtime.go`、`authorization_method.go` | CookieJar、代理/TLS/设备/OAuth 身份、checkpoint | BPS/身份/恢复/一次性结果夹具 | 真实线上 challenge 不属于合成验收 |
| 授权偏好 | `authorizationMethod.js`、弹窗、账号保存方式 | 浏览器偏好；队列身份不变 | 重新打开/刷新、locked retry、自动恢复身份测试 | 浏览器存储不可用须告知用户 |
| 重登/刷新 | `application`、`refresh_recovery.go`、`saved_grant.go` | 先存 grant recovery，再导出/同步 | 原 refresh/轮换/历史授权保留测试 | 保存 recovery 本身失败仍需人工保护结果 |
| 2FA → 保存 → 退出设备 → 重登 → 上传 | `application/totp_workflow.go`、`jobs/totp_*`、`protocol/totp_*` | OS 凭据、私有结果、工作流状态 | 顺序/部分失败/隔离/当前密钥回归；提交失败后证据保留 | 不自动执行真实 2FA/退出设备 |
| 任务排队/取消/恢复 | `jobs/manager.go`、`restore.go`、`discovery.go` | 同账号 lease、metadata 原子替换、恢复证据 | 新增自然结束 context 释放、多账号取消、磁盘失败及重试测试 | 全局锁仍含有序凭据/磁盘工作，已测量，未盲目异步化 |
| Sub2 更新/去重 | `sub2api/upsert.go`、`upload_outcome.go` | 查找后更新；首次批量创建 | 字段保留、BPS、时区、并发去重；新增拒绝/未知/部分完成回归 | 更新 `{}`/空响应仍按既有远端协议视为确认 |
| 巡检/自动恢复 | `sub2api/monitor.go`、`console/monitor.go` | 冷却、资格、套餐排除、pending 目标 | 原套餐/排队/时序测试；新增 200 业务拒绝终止后续步骤 | pending 按远端账号保存，未改为逐 HTTP 步骤日志 |
| HTTP 有界重试 | `transport`、各协议调用点 | 读重试与单次副作用请求分开 | 断连/503/非法 JSON/截断/取消回归 | 未把业务拒绝或全部 POST 泛化重试 |
| 凭据/文件安全 | `credentials`、`privatefile`、`store`、`logging` | Keychain/DPAPI、原子写、私有权限、脱敏 | 原权限/故障注入；独立 namespace 合成 Keychain 跨进程测试 | Linux 敏感值仍仅存内存；未更改平台存储契约 |
| 账号/配置/监控页面 | `web/src/hooks/serverState.js`、`lib/polling.js`、组件 | 服务数据、草稿、偏好、暂态 UI | 新增生产 JS 的 Go+Sobek 竞态/超时/离线/焦点/存储回归 | 浏览器实际验收与逻辑测试分别记录 |
| 交付与清理 | Go esbuild、build/verify/cross、本地脚本、CI | 两份一致前端、CGO=0 二进制、受限卸载 | 增加格式/差异/显式 worker gate，CI 校验已提交资源 | 交叉编译不等于原生 DPAPI/服务验收 |

## 已确认问题与落地修复

1. **完成任务 context 未释放**：成功或失败返回后，原实现删除 active 条目但未 cancel 本次 context。修复将 cancel 绑定单次执行，在结果分类后释放，避免误将正常完成记为取消。新增测试先在原实现重现两分支失败。
2. **恢复发布不原子，证据过早删除**：多账号恢复中途取消/metadata 提交失败可能留下部分内存队列；密码/2FA 结果在全局提交前被删除。现在恢复批次在锁内准备，失败撤回内存发布；三类私有结果仅在 metadata 成功后清理。已经安全保存的新凭据不倒退，失败仍保留本地重试证据。
3. **写入确认过于乐观**：创建接口的空/null/畸形/矛盾响应被默认记作全成功，更新和巡检写入忽略 200 内业务拒绝。集中有类型的结果解析，区分 rejected、unknown、not attempted；保留已确认部分，禁止因无法确认就重复创建。
4. **应用流程反向依赖 HTTP 层**：重登、2FA 和上传编排提取至 `internal/application`；`cmd` 保留装配入口，HTTP 与应用共用 `sub2api.ParseUploadSettings`。没有引入通用工作流框架或逐结构体接口。原入口级测试继续执行新实现。
5. **轮询重叠/过期覆盖/错误静默**：统一账号、监控、启动、日志的只读轮询；有界超时、取消、串行刷新合并、退避、后台/离线暂停与恢复。变更型请求没有自动重放。
6. **“已记住”与实际存储不一致**：存储函数返回真实结果；页面区别服务已生效与浏览器保存失败；主题纳入清理范围。弹窗焦点和层叠 Escape 统一管理，长内容标签允许合理换行。
7. **解析器重复编译固定正则**：候选字段反复调用邮箱/分隔符校验，单条结构化导入有 4,199 次分配。将固定语法预编译，保持全部分隔/歧义规则；通过原格式回归和新增 fuzz。
8. **同步结果与整体工作流混淆**：明确拒绝或尚未发出写入的失败不再显示为远端未知；断连、5xx、矛盾响应及未跟随的写入重定向仍显示未知。手动上传用 HTTP 502 和结构化结果保留已完成计数；2FA 保留已保存的新密钥与授权，分别展示同步失败/未知。
9. **可达依赖漏洞**：固定 `govulncheck v1.9.0` 复扫后，符号可达发现从 16 项降为 0，已导入包级为 0；仍有 11 项仅模块级记录。升级 `circl 1.6.3`、`x/net 0.56.0` 及 MVS 必需的 `x/crypto 0.53.0`、`x/sys 0.46.0`，保留 Go 1.25 兼容与现有 TLS 指纹库版本。完整证据见 [安全升级记录](pure-go-runtime/security-upgrade-2026-10-13.md)。
10. **前台慢响应被隐藏预算截断**：默认跳过的原 65 秒真实等待回归，在组合及独立执行中均于 80.02 秒失败。`git show HEAD` 确认原客户端将显式超时再次截断到 30 秒，与保留的生命周期回归契约矛盾。本次保持默认 30 秒及原断言，仅移除显式配置的隐藏上限；groups/options 元数据读取单次 120 秒、整体父预算分别 120/240 秒，后台巡检和上传仍用原默认。前端 Sub2 options 245 秒、SMS options 25 秒（服务端 20 秒），状态轮询继续 10 秒。调用方取消、并发隔离和副作用重试规则保留。
11. **worker 退出覆盖安全终态**：末轮全量 race 揭示 `TestChallengeImmediateScriptErrorReportsSafeStage` 间歇失败，独立 100 次复测出现 3 次。父进程完成 HTTP 后回复 IPC 时，退出/EPIPE 可能盖过 stdout 中已写出的脚本失败。新增确定性夹具在旧实现上稳定失败；修复后 reader 先缓存经现有白名单处理的安全终态，回复失败在原发送/调用方预算内取得该诊断，HTTP 完成状态也在回复前记录。保留未知退出错误、取消优先及私有输入清理，不传播脚本原文或 URL。原故障用例和新增夹具各 100 轮 race 通过。

已测量假设：调度锁包含磁盘与 vault 延迟，但本次测量未证明应异步化或改写调度器；因此保留提交顺序。潜在风险：第三方实时协议变化、Windows 原生凭据/服务、系统磁盘或 Keychain 突发故障仍依赖相应环境与操作边界；这些不因单元测试通过而视为消除。

## 性能证据

条件：同一 Apple M6、darwin/arm64、Go 1.27.1；`go test ./internal/console -run '^$' -bench BenchmarkSmartAccount -benchmem -count=3`，同一合成输入，前后各三次，表中耗时为中位数。结果仅说明此解析路径，不外推到真实授权网络耗时。

| 输入 | 修改前 ns/op | 修改后 ns/op | 分配次数前 → 后 | B/op 前 → 后（约） |
|---|---:|---:|---:|---:|
| 结构化 | 170077 | 11523 | 4199 → 158 | 635786 → 8660 |
| 无序字段 | 100005 | 5465 | 2477 → 79 | 398465 → 3582 |
| 歧义输入 | 167384 | 8879 | 4120 → 133 | 629411 → 6850 |

调度测量：`BenchmarkManagerQueueOperation` 使用 1/100 个合成账号、无存储/原子 metadata/内存 vault/每次调用 1ms 延迟 vault，同一 `-cpu=1 -benchtime=30x -count=3`。原 HEAD 在独立临时副本测量，没有覆盖工作区。100 账号原子 metadata + vault 中位数 11.869 → 11.851 ms/op；合成慢 vault 为 15.734 → 15.989 ms/op，3 次 vault 调用约占 3.39ms。不宣称调度提速；磁盘与 vault 确实影响锁占用，保持同步提交顺序，未来按真实规模再评估。

## 删除与保留依据

- 移除 `upsert.go` 中默认成功的旧解析器：有 15 个先失败的回归场景，已被集中结果解析替代。
- 移除 cmd 中重复业务编排及 Console 独占配置实现：原签名转为薄适配，业务代码迁至明确应用层/集成层；不是删功能。
- 移除旧独立轮询/重复焦点处理：统一受测实现替代，组件保留各自业务数据。
- 旧 hash 的 dist 资源由 Go 构建器替换；许可证和受控 vendor 保留。
- 保留全部安全检查、协议夹具、平台实现、恢复路径、worker 与本地卸载归属校验。没有仅因长文件/旧名字删除代码。

## 验证记录

所有业务测试只用合成账号、httptest、独立目录；没有真实登录、2FA 重置、退出设备、短信采购或 Sub2 上传。最终源码冻结后使用 Go 1.27.2 darwin/arm64 验证，测试用 `-count=1` 禁用结果缓存。

| 最终验证 | 结果 | 范围 |
|---|---|---|
| `gofmt -l cmd internal scripts`、`git diff --check` | 通过 | 无格式/空白错误；交付脚本 `sh -n` 通过 |
| `go test -count=1 ./...` | 通过 | 两项末轮缺陷修复后的最终源码：18 个测试包、885 个顶层测试通过；另有无测试的 build-web 包 |
| `go test -race -count=1 ./...` | 通过 | 最终源码 18 个测试包、885 个顶层测试通过，0 失败；前一轮暴露的 worker 退出诊断缺陷及修复证据见问题 11 |
| `go vet ./...`、`go mod verify` | 通过 | 最终依赖与工具链 |
| `TOSUB2_LEGACY_RUNTIME_TEST=1 go test -race -count=1 ./internal/jshost ./internal/legacyruntime ./cmd/tosub2` | 通过 | 最终源码 3 包、186 个顶层测试，显式认证 worker 与生产 CLI 合成恢复 |
| `GOTOOLCHAIN=go1.25.0 CGO_ENABLED=0 go test -count=1 ./...` | 通过 | 最终源码 18 包、885 个顶层测试；仅表示最低版本兼容 |
| `make build` | 通过 | Go esbuild + Go 1.27.2、CGO=0 生产二进制 |
| `scripts/build-cross.sh` | 通过 | darwin/arm64、darwin/amd64、linux/amd64、windows/amd64；Linux 产物静态链接 |
| 前端重复构建、两份 dist 比较 | 通过 | 重建前后 SHA256 不变，嵌入资源与 web/dist 逐文件相同 |
| 原生 Keychain 专项（race） | 通过 | 保存读取、合成旧凭据导入、跨进程更新恢复 3 项；独立身份并清理合成条目 |
| 原五分钟巡检与在途关闭专项 | 通过 | 实测间隔 5m0.008089209s，无重叠，关闭取消请求并保存有效 metadata |
| 原 native/fingerprint 长预算专项 | 通过 | 两种 transport 均完成 31.00 秒请求 |
| 原 65 秒 Console 响应及关闭取消（race） | 修复后通过 | 响应实测 1m5.0065325s；关闭取消 0.04s，未改等待或断言 |
| 实际生产 Dockerfile 构建 | 未完成 | Docker Hub 请求 `golang:1.27.2-alpine` manifest 超时，尚未开始编译；无镜像可启动验收 |

- 初始 `go test ./...` 通过。
- 针对性应用/HTTP/配置/恢复/JS 逻辑回归通过。
- `FuzzSmartAccount`：8 秒、2 workers、116280 次执行，无失败。
- `FuzzUploadCreationOutcome`：5 秒、2 workers、175135 次执行，无失败。
- 独立 namespace 的 Keychain 保存/读取、合成旧凭据导入和跨进程恢复三项原生测试通过，并删除合成条目。旧凭据导入测试只访问其本次随机生成且事先确认不存在的合成身份。
- 老 `govulncheck@v1.1.4` 在 Go 1.27 类型分析时崩溃；此轮失败不计为扫描通过。最终 Go 1.27.2 + `govulncheck@v1.9.0 ./...` 退出 0，结果为 0 符号级、0 导入包级、11 模块级；不能表述为所有依赖无漏洞。工具采用 `go run ...@固定版本`，不作为应用依赖加入。
- Linux/arm64 静态 fixture 在一次性 scratch 容器实际执行：10 个顶层测试全部通过，无跳过；禁网、UID 10001、只读根文件系统、独立 tmpfs。覆盖 worker 取消/超时/脱敏、Cookie 与身份隔离、合成授权与 checkpoint 恢复。worker 末轮修复后已重新执行这 10 项，并额外让原故障及新增终态夹具各执行 25 轮，均通过。该结果不是完整生产镜像验收。一次性镜像与容器已清理。
- Windows amd64/arm64 的 credentials、privatefile、legacyruntime 六个测试二进制交叉编译通过；不算 Windows 原生运行。
- 前端实际浏览器使用仅含合成账号、无外连的 fixture，验证授权偏好刷新、锁定续跑身份、主题与清理、焦点恢复、Tab/Escape、1600/390/320 宽度、长内容、网络恢复及上传部分/未知状态。截图与步骤见 [前端验收](pure-go-runtime/frontend-quality-upgrade.md)。

默认 suite 的 skip 被单独核对：原生 Keychain、认证完整恢复及长时测试分别用 opt-in 专项补验；5 个 `*ProcessHelper` 是父测试启动的子进程入口，主测试进程中按设计跳过，不单独计为通过。`TestPublicSDKInitialization` 未运行，因为未提供匿名获取的当前公开 SDK 夹具；已有内嵌 host/协议合成测试不能替代该线上版本检查。Windows 原生 DPAPI/服务、Linux systemd 安装、GitHub Actions 远端运行均未执行。生产 Dockerfile 的网络阻塞与 Linux scratch fixture 的成功分别记录，不混同。

新的快速预算回归使用 Go 标准库 `testing/synctest` 执行真实重试循环，验证默认整体预算 123 秒、显式 120 秒时整体预算 483 秒，以及更早父截止的终止；前台 120/240 秒父预算先行生效。options 的前一个读取重试可能消耗后一个读取的剩余预算，不保证两个逻辑读取各占完整 120 秒。合成时钟没有用于缩短原三项慢回归。

完整测试 JSON 已归纳为 [机器可读验收摘要](pure-go-runtime/quality-validation-2026-10-13.json)，保留各命令的包/顶层测试计数、失败数及跳过清单；跳过不计为通过。

## 本地部署验收

2026-10-13 23:01（Asia/Shanghai）已用原脚本更新现有 4401 实例。部署前检测到账号数由早先快照的 2 个变为 3 个，重新确认当前基线稳定且执行中/排队均为 0、巡检未运行，随后保留旧二进制并重启。没有回写旧快照或覆盖新增账号。

新 PID 为 `33627`，原运行目录、端口、Keychain namespace 和数据位置不变。服务健康、仅监听 loopback；运行二进制与最终构建逐字节一致，SHA-256 为 `d5c0e60cb8949b7b57bb302e789e50e70bdfe40b6ac272654222391f9d04b8c6`。在线 HTML/JS/CSS 与最终 dist 一致。重启后 3 个账号、无执行中或排队任务；账号公开身份/能力、巡检配置及加密凭据文件聚合指纹保持一致。

原产物私有回退副本、恢复命令与实际资源名见 [4401 当前部署](pure-go-runtime/local-deployment.md)。没有数据迁移、真实账号操作、Git 提交或推送。

收尾再次确认健康、空闲、数据指纹和资源一致后，逐一核对归属并删除 32 个本轮临时文件及 3 个空测试目录，包括六个 Windows 编译测试程序、原始测试日志、只读探针及只含聚合指纹的升级快照。测试计数/跳过清单和故障证据已归纳到本记录及机器可读摘要，安全扫描 JSON 单独保留。合成 UI 服务/标签页、Linux 隔离测试容器/镜像及其临时目录已清理；只检测到现有 4401 服务进程。保留源码、回归测试、协议夹具、合成截图、正式构建产物和明确命名的旧二进制回退副本；未清理真实数据、凭据或外部备份。

## 运行边界与后续

- `main.go` 的底层认证适配仍较大；这次只抽出可独立验证的重登/2FA/上传用例，不一次性搬动认证身份与 CookieJar 边界。
- 巡检的远端目标 pending 是现有恢复粒度；已明确失败/未知，不新增复杂框架。
- 合成服务不能证明第三方当前线上协议或真实账号授权可用。
- Windows 原生 DPAPI/服务、Linux systemd 安装和完整生产容器仍需目标环境验收；交叉编译、Linux 隔离 fixture 与 macOS 本机验收分别报告。
- 完整构建、升级、回退、恢复与卸载步骤见 [Go 部署](go-deployment.md) 和 [macOS 本地生命周期](local-macos.md)。
