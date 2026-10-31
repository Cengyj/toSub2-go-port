# 2026-10-31 第二轮质量优化

## 基线与实施范围

本轮承接[第一轮报告](quality-review-2026-10-31.md)，以 `main` / `1a029e7` 加全部未提交成果为基线。开始时有 28 个已修改文件、4 个旧生成资源删除、13 个未跟踪交付项；未 reset、覆盖、暂存、提交或推送。未发现新的 AGENTS.md。保留纯 Go、Go esbuild、受控 vendor、Sobek 与同一程序认证 worker，没有依赖升级、数据迁移或通用框架引入。

仍只使用合成账号、临时私有目录、内存凭据及本地模拟 HTTP；4401 只做公开健康读取，不读取账号、token 或私有配置。

## 增量功能矩阵与确认问题

第一轮完整功能矩阵仍适用；本轮针对未覆盖的失败边界补充：

| 能力 | 调用链与实际问题 | 实施与回归 | 副作用边界 |
|---|---|---|---|
| 删除与取消 | `jobs.DeleteAccounts` 在检查请求取消前移除队列、取消 runner | 进入锁后先检查请求与服务状态；已取消请求、运行任务、关闭后的排队保护 | 未准入请求不影响现有操作；已准入删除的恢复语义不变 |
| 停止任务回执 | `Cancel` / `CancelAll` 忽略保存错误，HTTP 总返回成功 | checked API 区分本进程停止请求与磁盘确认；失败回 409/uncertain，支持修复存储后再次确认 | 不因保存失败重新放回执行队列；活动 runner 的取消仍不是远端完成证明 |
| 重登上传与 2FA | panic 展开 defer 时命名 error 仍为 nil，可能错误落盘 succeeded | 未开始同步记 skipped、开始后记 unknown，工作流不再成功；原 panic 继续传播 | 保留历史与新授权；不重放，持久化不记录 panic 内容 |
| 自动上传读取 | `UploadSavedAuthorization` 缺少查询路径已有的私有文件、父目录与恢复 journal 校验 | 复用 `readCommittedAuthorization`；四种不安全输入均验证零远端请求 | 不更改文件权限、不删除恢复材料；上传 16 MiB / 查询 2 MiB 上限保留 |
| OAuth HTTP 预算 | `OAuth → Client.Do → Request → transport` 丢失 MaxResponseBytes | 贯穿包装与重试；code/refresh 响应明确限制 1 MiB | 超限停止，不 challenge 重放；保留已接收 Cookie |
| Sub2 身份切换 | 同 client 换 workspace 且缺省新 refresh 时保留旧身份 refresh | 身份变更先移除旧 refresh，新值再覆盖；两种授权方式及手动/巡检共用路径回归 | 同 workspace 缺省 refresh 仍兼容，其他远端配置保留 |
| 列表状态 | `useAccountPolling` 验证前提交 setJobs，坏 200 响应破坏有效列表 | 整份快照校验后发布；坏响应保留列表、选择、分页与统计 | 接受有效空列表及真实 API 的空邮箱投影；不改变账号资格 |
| 配置轮询 | monitor / insight settings 仅成功响应检查 revision，旧错误可覆盖新保存 | 错误与超时同样检查读取版本；生产 hook 测试旧/当前版本 | 当前读取失败仍可见，无有副作用请求重放 |

以上问题均有修改前失败、修改后通过的回归证据。优先级按安全副作用、身份/业务正确性、状态可靠性和 UI 排列。

## 架构与兼容说明

- 应用层共享已提交授权的安全读取，保留各调用方的大小限制、JSON 精度、单文档校验和账号身份核对。
- 调度层负责本进程停止与保存结果，HTTP 层只转换回执；原 `Cancel` / `CancelAll` 包装保留内部调用兼容。成功 HTTP 字段保持，只有确实保存失败从伪成功改为 409。
- 工作流 panic 只在结算边界识别并重新抛出，不添加通用吞错恢复。意外程序 panic 仍可能终止服务，本轮未宣称进程级容错。
- 前端把整个读取快照作为接受单位；配置 revision 同时管控成功和失败，未改查询开关、授权偏好或表单草稿语义。
- ChatGPT/BPS、历史授权、2FA 顺序、手动到期优先、Free 自动恢复排除及 BPS 全模型字段缺失语义全部保留。

## 清理依据

自动上传原独立文件读取代码由共享的更完整校验替代；删除的是重复读取实现，不是上传功能。不同 workspace 的旧 refresh 仅因身份不再匹配而清理。前端构建按 HTML 引用更新 hash 资源；vendor、许可证、平台实现与恢复夹具保留。旧上传测试仅改合成文件的私有创建方式以满足 Windows DACL 语义，没有删除断言。

## 回归证据

- [删除请求取消](../internal/jobs/delete_context_test.go)：修复前已取消请求会把排队任务改 canceled，并取消活动 runner；修复后两条保护及关闭后拒绝删除均通过。
- [取消保存失败](../internal/console/cancellation_persistence_test.go)：单条／全部停止在损坏存储时原为 HTTP 200；现验证 409、不泄露私有路径、当前队列不执行，以及修复后再次确认和重启状态。
- [工作流 panic](../internal/application/workflow_panic_test.go)：覆盖授权、2FA、重登与上传阶段；验证未开始／未知结果、历史授权保留及原 panic 继续传播。
- [上传文件安全](../internal/application/upload_safety_test.go)：目录 symlink、0644 文件、refresh journal 原来均发起 GET+POST；现为零远端请求，另验证有效文件和取消原因。
- [OAuth 响应限制](../internal/protocol/oauth_response_limit_test.go)、[workspace grant 身份](../internal/sub2api/oauth_grant_identity_test.go)：覆盖 200/403、普通/SingleAttempt、ChatGPT/BPS、code/refresh、手动上传／巡检。
- [生产前端 hook](../internal/console/frontend_server_state_test.go)：非法快照、旧成功／错误／超时版本、当前错误、恢复、计时器清理，及真实 HTTP 投影和历史恢复兼容。

补查验证推翻了“历史恢复可能产生缺失邮箱”假设：`decodeLegacyJob` 明确拒绝空邮箱；当前 API 对内存空邮箱记录显式投影 `email: ""`。因此保留严格字段校验，没有扩大恢复资格。

## 验证结果

精确命令、测试名称、skip 和平台隔离参数见[机器可读摘要](pure-go-runtime/quality-validation-2026-10-31-round2.json)。所有测试均使用 `-count=1`，下表不把子测试或 helper 重复计为独立顶层测试。

| 验证 | 本轮结果 |
|---|---|
| `go test -json -count=1 ./...` | 20 测试包、1166 顶层测试通过，0 失败 |
| `go test -json -race -count=1 ./...` | 20 包、1166 顶层测试通过，0 失败 |
| `GOTOOLCHAIN=go1.26.0 CGO_ENABLED=0 go test -json -count=1 ./...` | 20 包、1166 顶层测试通过；仅最低版本源码兼容 |
| `gofmt -l cmd internal scripts`、`git diff --check`、`go vet ./...`、`go mod verify`、全部 shell 脚本语法 | 通过 |
| `TOSUB2_LEGACY_RUNTIME_TEST=1` 显式 jshost/legacyruntime/cmd race | 3 包、191 顶层测试通过，含生产 worker 与登录恢复合成夹具 |
| 五分钟巡检与 inflight shutdown | 显式运行通过，300.08 秒 |
| 65 秒 HTTP 响应预算 | 显式运行通过，65.03 秒 |
| 超过 30 秒 transport | native / fingerprint 各 31.01 秒，通过；这三项不再沿用上一轮未测结论 |
| Go esbuild 与 CGO=0 生产构建 | 通过；两份 dist 完全一致，生产工具链 go1.27.3 |
| darwin arm64/amd64、linux amd64、windows amd64 | 四种生产产物交叉编译通过 |
| Windows amd64 测试程序 | application/jobs/console/protocol/sub2api 五包编译通过；未原生运行 |
| macOS Keychain | 独立合成 namespace 的三项原生 race 测试通过并清理 |
| Linux ARM64 原生合成测试 | Docker Linux VM 架构匹配，8 顶层／19 子场景、0 fail、0 skip；scratch、禁网、只读、UID 10001，无宿主资料挂载 |
| `govulncheck@v1.9.0 -json ./...` | 退出 0；0 符号可达、0 已导入包级，2 项仅模块级提示（GO-2026-5841、GO-2026-5932）；不是依赖完全无漏洞 |
| 最终生产二进制隔离启动 | 空合成目录；health 与 HTML/JS/CSS 逐文件一致；SIGTERM 正常退出 0 |

普通/race/最低版本套件各有 13 个显式测试 skip，以及 build-web 一个无测试包。Keychain、生产登录恢复和三类长时测试均单独补跑；五个子进程 helper 由父测试启动，不把它们作为独立通过。公开 SDK fixture 仍未提供，未用真实 HAR 替代。

没有性能前后测量，不宣称吞吐或时延改善；本轮无解析器改动，未重复第一轮 fuzz。Windows 原生 DPAPI/DACL/SCM、Linux systemd、完整发行 Docker 镜像、远端 CI、真实上游账号操作和其他浏览器均未验收，不能由交叉编译或 Linux scratch 夹具替代。

## 实际 UI 验收

Codex 内置浏览器使用独立合成 Go 服务和最终资源：

- 1600px 宽屏与可用宽度 375px 窄屏，页面均无横向溢出；窄屏账号表只在自身区域滚动，键盘右方向键实际改变 scrollLeft。
- 已选择一条账号后注入 HTTP 200 `{}`：三条账号、已选项、分页和统计保留，只出现一个明确连接错误；恢复有效响应后自动恢复已连接。实际验收发现提示重复句号，已修正并重新构建复验。
- 查询配置保存为 30 分钟并刷新，仍为 30；服务端查询保持关闭，不触发上游。旧读取失败跨保存的竞态由生产 hook 的延迟拒绝／超时回归验证，不把普通浏览器保存测试冒充精确竞态注入。
- BPS 选择刷新后保持；弹窗 Shift+Tab／Tab 环绕、Escape 关闭并恢复原触发按钮焦点。未点击执行授权。
- 清除合成 origin 网页设置后恢复 ChatGPT 默认；浏览器临时视口复原、标签页关闭。

截图仅含合成数据：[坏响应保留列表（宽屏）](pure-go-runtime/ui/quality-round2-20261031-wide.jpg)、[窄屏](pure-go-runtime/ui/quality-round2-20261031-mobile.jpg)。

## 部署、回退与剩余风险

**4401 仍未更新、未重启。** 当前服务公开健康检查通过，PID 77230 与上一轮一致；安装二进制摘要仍为 `c2b4d07dbea76a8516cc4bd392cad5ea303358b2735debc561905c48dc08300e`。本轮 `bin/tosub2` 摘要为 `7002a6d34d3f88c562b6a776e7ee9989827c6ab9b70b5a18a583a64d8c550fe0`，最终 JS 为 `index-87021163236b.js`、CSS 为 `index-c873df8a9587.css`，与运行实例 HTML 不同，未把构建完成误报为上线。

阻塞仍是现有实例没有维护期原子准入封锁；本轮取消持久化修复不等于部署门禁。需在受控维护窗口停止提交来源、确认排空，再按[安全升级与回退](go-deployment.md#可重复验证与安全升级)保留旧二进制／私有数据和同一凭据 namespace 后安装。本轮无数据迁移，当前实例未动，因此无需回退。

仍保留的风险：跨客户端 Sub2 更新没有远端 ETag/事务保护；活动取消不证明远端副作用已停止；panic 仍可能终止服务；一次性 OAuth 超限响应不能确认结果时不可盲目重放。锁内磁盘／vault 性能没有同条件测量，不据此异步化。

合成 UI 与生产 smoke 已停止，临时 Docker 容器、镜像、测试二进制和测试数据目录清理后只保留摘要与合成截图；正式生产／跨平台产物保留在 bin。最终 main 工作区共 66 项（38 修改、4 旧生成资源删除、24 未跟踪交付项），包含两轮累计成果；未暂存、未提交、未推送。运行、配置、备份、恢复、回退、排障与彻底卸载入口仍为 [README-Go](../README-Go.md)、[部署说明](go-deployment.md)、[macOS 运行与卸载](local-macos.md)。
