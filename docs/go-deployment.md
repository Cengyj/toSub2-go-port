# Go 服务部署

服务使用单个 Go 可执行文件提供认证、控制台与 Sobek 动态脚本 worker。认证时可启动同一可执行文件的子进程。前端通过 Go esbuild API 构建并嵌入，构建与运行均无需 Node、Python 或 Chromium 自动化。Go 最低版本为 1.26.0。

当前生产工具链固定 Go 1.27.4（go.mod toolchain、CI 和 Docker 一致）；Go 1.26.0 继续单独验证源码兼容，不能将旧标准库的兼容构建作为安全生产产物。工具链与依赖变化依据见维护台账的 2026-11-14 本地工程质量优化记录。

## 构建

```sh
make build
./bin/tosub2 --host=127.0.0.1 --port=4399 --output-root=/absolute/path/to/private-data
```

`make frontend` 只构建网页；`scripts/build-cross.sh` 使用 `build-web -build-go -cross`，生成 macOS arm64/amd64、Linux amd64 与 Windows amd64 产物。`go run ./cmd/build-web -check -build-go` 在检查两份现有前端资源后编译程序。分发时附上适用的第三方许可证。裸 `go build` 不参与工作区构建锁，也不验证前端源码与产物一致性。

服务与交互控制台应使用相同操作系统用户，以保持 Keychain/DPAPI 用户范围以及 Linux 私有密钥文件的所有权一致。Linux 配置 `TOSUB2_CREDENTIAL_KEY_FILE` 后使用 AES-256-GCM 持久化账号凭据及私有操作证据；未配置且无既有加密存储时才使用进程内凭据。私有输出使用独立目录；新版本以实例锁拒绝两个控制台同时写同一根。核心查询、检测或回收站组件构造失败时，生产入口会在启动调度器与 HTTP 服务前停止并释放已取得资源；已成功加载服务的损坏策略仍按各自契约保持关闭或隔离，并显示恢复诊断。

`bootstrap` 下发进程 token 用于本地控制台隔离，不是多用户登录或公网身份认证。Host、监听端口、Origin 和 Fetch Metadata 的校验同时适用于静态资源与 API；通配监听仅允许本机接口地址，不能任意信任代理传入的 Host。反向代理需保持这些约束，并在外层提供所需访问控制；程序不信任 `X-Forwarded-*` 来放宽同源校验。新前端只在请求 header 携带 token。旧 `?token=` 调用仍兼容，但 URL 可能被浏览器历史、反代访问日志或复制操作保存，应迁到 `Authorization` 或 `X-Console-Token`。所有私有响应及错误均禁止缓存。

## 可重复验证与安全升级

```sh
./scripts/verify.sh
./scripts/build-cross.sh
# 联网执行固定版本 Go 漏洞分析工具，不修改本模块依赖：
TOSUB2_VERIFY_VULN=1 ./scripts/verify.sh
```

verify 检查格式、diff 以及源码与两份 dist 的一致性，运行普通/race/vet、显式 Sobek worker、当次登录和纯本地结果保存夹具，单独检查 engine-probe 嵌套模块的编译/vet，再构建前端和 CGO=0 主程序。修改前端后先执行 `make frontend`，过期产物会在重建前明确失败。CI 验证当前源码与已提交的两份 dist 一致。同工作区 build-web 构建/检查与正式 native/cross 构建互斥，锁覆盖前端生成、模块副本、全部 Go 编译和候选发布；遇到占用应等待原进程退出后重试，不删除 `.build-web.lock`（锁文件保留以避免 inode 分裂）。候选使用 `TOSUB2_BUILD_DIR` 独立输出。编译副本含 `go.mod/go.sum`、`cmd/tosub2` 与 `internal`（含 JS/embed），不含业务数据；编译关闭外部 Go workspace，使用该副本的模块依赖。

所有目标编译成功后才逐个发布程序及许可证，普通发布失败恢复原文件；恢复本身失败时保留暂存目录并返回其路径。强杀锁所有者后，孤儿编译只能访问原副本并写暂存候选，不能替换最终程序。`.tosub2-build-*` 残留应在确认对应编译器退出后处理。多目标发布和两份前端目录切换都不是跨进程崩溃的原子事务，中断后必须重新构建、检查再发布；构建期间不要编辑源码，锁不协调编辑器或裸 `go build`。

漏洞检查退出非零时须检查原因：工具不兼容、数据库不可达和发现漏洞均不能写成通过。Go 最低版本兼容测试与生产工具链安全维护是不同的检查。

需要复核长超时、真实五分钟周期或最低版本时，分别运行以下独立验收。它们只使用本地合成服务，但真实等待时间不会缩短：

```sh
GOTOOLCHAIN=go1.26.0 CGO_ENABLED=0 go test -count=1 ./...
TOSUB2_LONG_TRANSPORT_TEST=1 go test -count=1 ./internal/transport \
  -run '^TestTransportConfiguredBudgetCanExceedThirtySeconds$'
TOSUB2_LONG_MONITOR_TEST=1 TOSUB2_SLOW_HTTP_TEST=1 go test -count=1 ./cmd/tosub2 \
  -run 'TestConsole(DefaultFiveMinuteMonitorAndInflightShutdown|LongSub2APIResponseKeepsLegacyTimeout)$' -timeout=10m
```

macOS 原生凭据专项需当前用户的 Keychain 可用；设置独立的 `TOSUB2_MAC_KEYCHAIN_SERVICE`（以 `com.local.chatgpt-onboarding.credentials.local.` 开头）和 `TOSUB2_KEYCHAIN_TEST=1`，再执行 credentials 包的 `TestMacOSKeychainIntegration`、`TestMacOSKeychainLegacyImportIntegration`、`TestNativeCredentialStoreSurvivesProcessRestart`。测试只创建合成身份并负责删除；不要把生产账号作为测试输入。其他 opt-in 的跳过原因与平台边界须另行记录。

升级顺序：

1. 在源码目录完成上述验证。记录当前运行二进制路径、版本/摘要、前端资源 hash；不复制或打印 console token。
2. 在服务配置中启用维护模式，并核对已持久化、当前版本及“已排空”。API 为 `GET /api/maintenance`、`POST /api/maintenance`，提交明确的 `enabled` 与读到的 `revision`。维护封锁新任务、查询、探测、上传和配置写入，暂停自动巡检、查询与回收清理；已有任务可继续输入、完成或取消。排空检查在本地任务空闲后取得排他工作租约，阻止同数据目录的新版本 CLI 与控制台重新准入；一个数据目录只允许一个控制台实例。旧版 CLI 不理解该协议，仍须先停止旧程序及其他写入来源。等待输入或未完成任务不会被维护操作隐式取消，未显示已排空时不得升级。普通 `restart` 不会自动进入维护或排空。
3. 保留当前可执行文件作为私有回退副本；需要数据备份时先完整停止服务与共享同一凭据库的写入者，再按下文“备份与恢复演练”核对范围。旧 Cookie、登录快照和未完成登录材料不进入业务备份；不能将未知结果的安全操作意图或待补保存的密码、TOTP、token 一并当作临时文件删除。macOS/Windows 凭据仍依赖同一用户与原 Keychain/DPAPI 保护材料；Linux 必须保留匹配的外部主密钥，并与数据备份分开保护。不要导出明文凭据用于备份。
4. 通过平台原有生命周期入口优雅停止，等待服务和 worker 退出；以同一用户、端口、输出目录和 namespace 安装新二进制，再启动。禁止两个进程同时写同一输出目录。
5. 核对 `/api/health`、账号计数、全局活动/排队数、当前运行路径和文件摘要；核对 HTML 引用的 JS/CSS 与构建产物一致，然后刷新浏览器。维护状态跨重启保留，核实完成后明确退出维护；退出持久性不确定时保持暂停并重新读取核实。不要通过真实授权、2FA 或上传来探测升级。排空不等于进程已退出，备份仍须在完整停止及 worker 退出后进行。
6. 健康或资源核对失败时立即停止新进程，保留失败现场和 recovery 文件，不用空目录覆盖旧资料。先检查数据格式及恢复标记兼容性，再选择兼容程序或同一时间点的完整备份回退；不能盲目只替换旧二进制。既有 job-meta v2 与回收站暂停标记继续保留，遵循 [回收站回退限制](pure-go-runtime/recycle-bin.md#回退限制)。本轮新增格式及回退边界见下节；恢复升级前备份还须核对升级后是否已有不可回滚的远端修改。

目录锁现按物理祖先和实际目录拼写识别别名；macOS 另读取卷的大小写语义，APFS 的 Unicode 组合形式也归一。它仅改变维护与配置的协调身份，不改变数据路径、加密绑定或根目录符号链接的拒绝规则。新旧版本可能计算出不同锁身份，因此此次升级必须先停止旧控制台、CLI 和 worker，不能混跑或滚动替换；不要删除仍可能被进程持有的旧锁文件。其他文件系统及 Windows 原生的特殊别名语义未据此宣称通过验收。

## 配置迁移与恢复边界

账号与历史仍使用 job-meta v2；本轮没有批量重写账号库或改用外部数据库。新增 `maintenance.json` schema 1、`console-settings/{sms,mail,sub2api,proxy}.json` 的加密 schema 1 配置，以及添加密码的受保护意图记录。Sub2API 监控新保存为 v2，使用 `credentialRef` 绑定单独保护的密钥版本；v1 固定引用继续可读。不要把这些变化概括成“没有格式迁移”。

`job-meta.json` 的读取与提交共同采用 256 MiB 上限，覆盖 1,000 账号、保留操作日志及回收站的聚合规模；普通设置 JSON 仍为 32 MiB。提交在替换前检查完整编码长度，超限会保留旧文件并报告失败，不截断历史或恢复证据。超过旧版 32 MiB 边界的现有元数据可以直接读取，无格式转换；旧二进制仍无法读取这些文件，回退必须先在隔离副本核对大小和恢复结果。256 MiB 是文件预算，不是进程内存预算；解析、投影及序列化需要额外内存。达到上限时先停止新操作、完整备份并检查日志与回收站保留策略，不手工删改恢复记录。

浏览器的短信、邮件、Sub2API 和代理秘密按范围逐项显式迁移。先读取服务端版本，再保存加密配置并完整读回；只有收到确认的保存回执才清除对应旧浏览器项。结果未知时保留浏览器原值，使用“读取/确认”完成纯本地同步与读回，不能重复外部业务请求。已有服务端配置与另一浏览器的旧值冲突时必须明确采用或编辑，不能自动覆盖。不同来源的未保存草稿也不会被后台读取覆盖。

“安全保存”“当前进程应用”“本浏览器偏好”是独立结果。保存配置本身不启动监控或联系供应商；邮件与 Sub2API 的显式应用携带保护配置版本，并持有与保存相同的范围锁，旧浏览器迟到的应用会被拒绝。清除浏览器偏好不删除服务端秘密、账号或关闭后台功能。保护存储不可用时不回退明文文件。

旧监控明文密钥只有在保护存储写入及读回确认后才迁移配置；失败保留唯一原文件并暂停自动巡检。新的 v2 引用缺失或不可读时不会借用旧固定密钥。配置提交结果未知时保留新旧密钥版本供核实，不能自动继续同步。回退旧二进制前须在隔离副本验证其兼容性；旧程序不理解维护围栏和新引用，须恢复同时间点的监控配置、保护凭据及必要证据，不能混用不同快照。

迁移演练使用临时目录、合成配置、故障注入和重开读回，覆盖旧版本、显式 false/0/空数组/扩展字段、版本冲突、替换后同步失败、密钥不可用和原文件保留。真实实例迁移需要本次会话的明确授权：先完成同时间点备份和副本演练，再按维护窗口执行。2026-11-24 用户明确要求更新本地容器，已按该流程完成本机 Docker 实例升级；这不授权真实登录、2FA、付费短信、模型调用或远端上传。macOS Keychain/Windows DPAPI 依赖原用户/namespace；Linux 依赖原独立主密钥，单独 JSON 备份不具备可恢复性。

显式离线凭据导入在打开目标库前取得输出目录的实例排他锁和工作排他租约，并逐项预检全部所选账号的跨进程锁；此时若控制台、CLI 或同账号操作仍在运行，导入保持全部未处理且不接触目标库。实际复制时逐账号重新取得并持有锁，预检之后发生的竞争会记录该账号失败及准确的部分结果，不覆盖既有凭据，也不同时保留上万个文件句柄。停服后允许在已持久化维护模式下执行该明确的离线操作，不会打开在线准入。目录围栏只覆盖同一输出根；若多个根通过 `TOSUB2_MAC_CREDENTIAL_ROOT` 或 Keychain namespace 共用存储，备份前仍须停止所有共享者，不能把单根排空声称为任意共享存储的一致快照。

控制台的结构化导入与账号编辑同样在读取、合并、保存和读回期间持有账号锁，另一进程正操作该账号时会拒绝写入。纯邮箱重复导入仍只复用账号，不写凭据或启动登录。编辑当前运行账号时，新增资料先留在该次执行的内存草稿中，显示“尚未确认持久保存”；会话退出后重新取得账号锁并合并最新保护记录。保存、读回、恢复核查或元数据提交失败时保留草稿并暂停自动重启，不重做远端操作。草稿不写普通元数据，进程退出会丢失，不能把已受理编辑等同于已可靠保存。人工密码提示仍只提交候选，须验证成功后才保存为登录凭据。

凭据编辑告警使用独立的 `credential_warning` 字段和 `ACCOUNT_CREDENTIAL_*` 分类；原业务的成功、失败、取消或中断结果分别保留。告警改变后同步保存元数据，告警自身保存失败会追加独立提示，不重放凭据写入。只有读回确认当前资料全部一致，才清除已解决的编辑告警；锁释放、日志归档或其他元数据故障不会随之消失。显式清空密码、2FA、代理或邮箱资料后，列表按确认后的凭据更新可用标记。重启及回收站恢复会明确说明上次进程的未保存草稿无法恢复，保留“确定失败”“保存待核实”“已保存但后续读取或锁释放失败”的区别；当前提示中的本次凭据编辑说明也会转为历史描述，独立业务错误和历史日志原文保持不变。发现已取得的 OAuth grant 或完成密码/2FA 本地补保存，也不能据此消除独立的账号编辑告警。

用量、订阅、模型目录、推理探测和上传共用账号租约：进程内预留成功后取得与 CLI 登录及安全操作相同的本机账号锁，再复核恢复记录和代际。取消只终止上下文，等待请求清理完成才释放锁，避免旧请求尚未退出时开始新登录。锁不可用会明确拒绝请求，不回退到仅进程内互斥；本机锁不控制其他主机或远端客户端。

普通查询还检查共享凭据库中的安全状态，包括首次 2FA 登记候选与未决重置；即使代理来自命令行或显式直连，也不省略检查。明确不存在登录凭据时仍可使用已提交 OAuth 授权；安全存储不可读时不能推断“没有未决操作”。查询在派发与提交前再次核对，迟到结果不能覆盖当前状态。不同输出目录可以共享凭据库，不能仅凭本目录没有恢复记录放行。

账号列表的默认 API 响应保持完整选择索引兼容。自带控制台通过 `GET /api/jobs?selection=compact` 或 `POST /api/jobs/query?selection=compact` 请求精简跨页索引；后者的请求体仍使用原邮箱数组和分页字段。响应以 `selectionMode: "compact"` 标识，索引保留账号身份、操作代际及显式的操作资格，当前页资料仍完整。筛选只生成匹配账号的公开资料，全局计数不随筛选缩小；脱敏使用同一次快照捕获的凭据上下文，锁外处理不会读取后续编辑的新凭据。每次读取仍检查实际 2FA 恢复记录，精简索引不缓存执行权限，提交前仍重新核对。页面、筛选或控制台 token 改变后，旧读取及旧刷新回调失效；离线或隐藏时暂停只读轮询，保留最后有效资料。

操作日志默认显示“关键进展”，仅收起结构完整、无异常标记的正常只读 HTTP 观测，并标明显示数量和收起数量；点击“全部”可查看完整的已加载记录。写请求、写前意图、拒绝、超时、截断、结果未知和安全存储失败始终保留。请求说明使用具体动作、HTTP 状态码和耗时，例如“激活新 2FA：收到完整响应（HTTP 200 · 698 ms）；待解析业务结果”；收到 HTTP 响应不表示该业务已经成功。2FA 结果标为“2FA步骤结果”，重新授权及上传可各自显示“步骤完成”，整个任务仍以最后的操作结果为准。

账号第二行、2FA 快捷按钮、当前操作与流程步骤共用进度解释。正常排队、重置、等待密码或验证码、激活后的本地保存分别展示；执行期间暂时禁止导出或再次修改，是本轮安全保护，不据此提示“需人工处理”。本轮结束后仍有可信激活记录但未提交，才提示完成本地保存；结果未知则保留核实提示。退出设备、重新授权和上传各有独立结果，已保存新密钥不代表后续步骤成功。取消请求后仍在收尾时继续禁用重复操作。

列表同时提供同邮箱的 2FA 任务占用情况，以及当前行实际占用的操作时间；前端只有核对与该行操作时间一致后才采用具体步骤。旧行或同邮箱历史行只提示同账号流程尚未结束，避免借用另一轮的状态。时间用于只读展示匹配，不能替代执行权限、账号锁或恢复证据。新增字段不写入元数据，无需数据迁移；实际操作仍按服务端现有安全规则复核。

“可读日志”复制当前筛选后的时间、阶段与说明；原复制按钮继续复制当前筛选的公开原始记录。两种格式均注明本次/历史范围、筛选条件、已加载页和北京时间；排查完整过程时先切换“全部”，并按需要加载更早日志。公开记录已经脱敏，不包含上游原始响应、密码或密钥。旧纯文本在 API 中使用仅供展示的 `legacyText` 标记，一条旧事件只显示一次，真正重复的事件及多行内容保留；该标记不写入原日志，归档格式、哈希和分页游标不变，无需迁移数据。

日志按各步骤的回执显示失败、待核实和未执行。授权保存后的同步异常、邮件自动收码超时、取消后仍未知的资料补全结果均可在异常筛选中查看。归档告警与凭据告警可同时出现。日志读取或归档有告警时，复制内容会提示分页完整性待核实。日志分组与记录时间均采用北京时间。回收站的历史摘要使用最后一次操作的结果和时间，旧记录缺少对应字段时使用原记录。

CLI 默认显示资料补全的触发、提交及结果。取消后如果收到提交结果待核实的记录，也会在退出前显示。CLI 输出失败会反映在退出结果中，受保护的业务文件可供后续核对。

新设置或重置 2FA 也会在任务准入、取得跨进程账号锁后检查共享库中的候选 seed、URI 和 factor，任一未决字段都阻止重新登记，排队期间其他进程保存的材料同样受到保护。仅保存导入和普通登录保留这些材料，不能把候选当成已验证密钥，也不能因候选字段不完整而清除隔离证据。已有可靠成功回执的本地补保存仍按原账号、操作和材料绑定执行；未知结果不能靠再次设置或重置来覆盖。

私有下载和上传采用相同的安全存储区分，关闭模型质量检测也不会绕过此检查。凭据及恢复记录的读取不占调度器全局锁，取消请求或关闭程序会中止保护存储读取。重置完成后的自动上传使用本次操作专属门禁，核对已确认的新因子与同一次凭据快照；它不会因为自己的重置任务仍在运行而被普通查询规则误挡，也不能借用历史成功记录放行。

## 备份与恢复演练

当前程序没有通用的备份、备份校验或整库恢复 CLI。维护接口负责准入与排空；离线凭据导入只复制明确选定的凭据；`--complete-oauth-grant` 只完成已取得 token 的本地保存。后者仍走普通 CLI 准入，维护开启时不会执行。`scripts/session-cleanup` 是旧认证会话的分类清理工具，不能用它的成功退出证明备份完整或可恢复。

停止所有写入者后，备份应在同一时间窗口覆盖以下资料，并保留相对目录、文件权限和对应的软件版本。归档和摘要清单也属于私有资料，应放在运行目录之外。

| 范围 | 必须保留及核对的内容 |
| --- | --- |
| 账号、历史与回收站 | 根 `job-meta.json`、各历史操作目录及其引用；包含归档记录、暂停状态、永久清理标记和未完成清理状态，不能只比较当前可见邮箱数。 |
| 授权与本地恢复 | 已提交的授权文件、独立 `oauth-grant.json`、refresh grant recovery sidecar，以及添加密码、首次设置和重置 TOTP 的受保护意图、候选、回执与隔离标记。CLI 刷新的 source/target 可以位于输出根之外，须一并保存对应文件及原路径关系。保留账号、操作 ID、结果 ID、factor 与文件之间的对应关系；未知结果仍是待核实。 |
| 配置、供应商订单与历史查询 | `maintenance.json`、`console-settings`、Sub2API/CPA 配置及保护凭据引用、短信订单管理材料、查询与探针历史、回收站设置。短信订单材料不等于可恢复旧登录或旧验证码。 |
| 保护存储 | 实际使用的完整凭据目录，包括环境变量指定的外部目录、Linux `.file-key.json` 绑定，以及相同时间点的 OS 保护材料或独立主密钥。仅保存环境变量中的 namespace 名称不能恢复已经删除的密钥。 |

不要根据 `tmp`、`recovery`、`intent`、`checkpoint` 名称或扩展名批量排除文件。旧登录会话与业务结果混在同一旧格式中、存在未识别文件或保护材料无法读取时，应保留原件并停止自动分类，先在隔离副本确认处理方式；不能靠丢弃未知内容获得“校验通过”。恢复前还要核对备份之后是否发生 token 轮换、密码/2FA 修改或远端上传，旧快照不会撤销这些远端事实。

恢复演练使用备份的独立副本，原备份保持只读。先离线核对文件清单、字节数、摘要、格式版本及引用完整性，再安排受控恢复；摘要相同只证明复制一致，不能证明凭据可解密或业务状态正确。启动程序会执行 `Manager.Restore`、必要的本地补保存和状态提交，即使维护已开启，也不是只读检查。未确认维护状态且未隔离外网时，不能通过启动备份副本来“看看能否恢复”：已经开启的巡检或查询可能开始工作。

运行恢复副本前，应确认其维护状态已持久化、使用独立输出目录并断开外网，同时停止所有共享保护存储的写入者。macOS 的同一 namespace 仍指向同一 Keychain，Windows 同一用户仍使用其 DPAPI 身份；复制数据目录本身不隔离它们。同 namespace 的本地补保存仍可能改变原 Keychain，必须纳入明确授权的恢复窗口；完全隔离的演练需要独立的 OS 保护环境或合成凭据。不要通过新建 namespace、重新生成主密钥或放宽权限来解决解密失败。恢复可以完成已有证据支持的本地提交，但不得自动重放旧登录、验证码、探针或未知的远端修改。验收需同时核对账号与历史、回收站、配置版本与秘密引用、已提交授权、待恢复结果和未知状态，之后才能决定是否切换实例。

refresh grant recovery sidecar 还绑定原始 source/target 的绝对路径。包含这类记录时，直接搬到新目录会被拒绝，不能编辑记录重绑或再次刷新 token。综合合成演练保留只读原备份，将工作副本放回原路径，再跨两个新进程验证本地补保存和重启暂停；真实恢复须在隔离环境提供相同路径，或先在原位置按既有流程完成本地补保存后重新建立一致备份。完整 v2 Sub2API 监控配置重启只装配运行态，不重写原历史和扩展字段；旧格式秘密迁移仍需保护存储读回与可靠提交。

以下命令验证代码中的维护围栏、恢复顺序与失败保护，只创建临时目录、合成账号和本地服务，不读取用户备份。它们不构成某份真实备份的验收，也不替代目标平台的 Keychain/DPAPI、容器卷或服务部署验证：

```sh
go test -count=1 ./internal/maintenance
go test -count=1 ./internal/console \
  -run '^TestMaintenanceHTTPClosesAdmissionAndDrainsExistingPrompt$'
go test -count=1 ./internal/jobs \
  -run '^(TestMetadataRoundTrip|TestShutdownRetainsQueuedAccountWithoutRestartingWork|TestPrivateRecoveryEvidenceSurvivesMetadataFailure|TestPasswordIntentOnlySuccessRecoversAfterMetadataCommit|TestPasswordIntentUnknownNeverCommitsCandidateOrQueuesReplay|TestTrashRestoreStaysPausedAcrossRestartAndKeepsPrivateResults)$'
go test -count=1 ./cmd/tosub2 \
  -run '^(TestProductionSavedGrantFinalizesWithoutBrowserProxyOrCredentialService|TestRefreshRotationSurvivesOutputFailureAcrossProcesses)$'
go test -count=1 ./cmd/tosub2 \
  -run '^TestRecoveryIntegrityBackupAcrossProcesses$'
go test -count=1 ./internal/credentials \
  -run '^TestFileKey(CredentialsAndRecoverySurviveReopen|MissingOrChangedRefusesReadWriteDeleteAndReinitialize|SurvivesProcessRestart)$'
```

## 故障恢复

- 网络或业务拒绝：读取当前操作及独立同步状态。已保存授权仍有效时保留文件；`failed`、`unknown`、`skipped` 和 `queued` 不等于同一结果。
- OAuth 一次性 code 已换取 token：从 `oauth-grant.json` 完成纯本地保存，导出失败不重新登录或兑换 code。refresh 轮换优先恢复同目录的 grant recovery sidecar，不能重新消费旧 refresh token。
- 2FA 部分完成：先确认新密钥安全保存和退出设备状态；待核实/隔离状态不得重置或上传掩盖。账号信息与原始资料导出应读取当前 vault，而非历史输入。
- Sub2 返回未知：先核实远端对应账号，再选择适当重试；不把空响应、矛盾计数或断连当创建成功，也不自动重复创建。已完成的部分会保留在结果中。
- 磁盘/凭据服务故障：修复权限、容量或用户会话后再恢复；metadata 提交失败时私有结果仍保留，成功保存的新密钥不会回滚。不要删除唯一业务凭据或已确认结果；旧 Cookie 与登录 checkpoint 不再读取或生成。
- 本地提交结果未知：文件已替换后目录同步失败时，当前进程不会执行新操作；重启保留业务资料并将旧排队/运行意图标为中断，不自动重放登录或远端修改。先核实账号、旧授权和已取得的业务结果；不要把错误提示视为确定未提交，也不要立即整批重交。磁盘修复后的重试应按当前恢复资格逐项进行。
- 停止任务保存失败：停止接口返回 HTTP 409 和 `uncertain:true`，表示本进程已请求取消、排队项不会继续执行，但磁盘可能仍保留旧操作意图（重启不自动执行）。不要用重启替代取消确认。先修复存储，再在同一进程通过单账号 `/api/jobs/{id}/cancel` 或 `/api/jobs/cancel-all` 再次确认；这些接口允许无新任务时刷新取消状态。活动任务仍须等待 worker 清理完成，HTTP 成功及 `canceled` 计数不是远端操作已停止的证明。
- 自动上传提示授权文件不安全或待恢复：保留现场，核对预期私有目录、所有者和权限；存在 refresh recovery journal 时先按原流程完成已取得授权的本地恢复。不要删除 journal、跟随替换目录或放宽读取权限强行上传。
- 操作意外 panic：任务执行边界捕获 runner panic，取消本次上下文并释放邮箱与并发占位；工作流保留已经开始同步的 unknown 与未开始步骤的 skipped，不把 panic 当成功。普通日志只显示安全诊断，不输出 panic 原值。此保护仅覆盖受管理任务，不能保证任意独立 goroutine 的 panic 都被捕获；恢复前仍须核实私有证据，不能重放远端修改。
- 添加密码：候选值与本次操作 ID 在发送前受保护保存；远端结果未知后禁止再次添加。明确成功但本地失败时只补保存，启动恢复核对结果、操作与当前保护凭据；清理在同账号跨进程锁内再次读回，不能删除别的进程刚写入的恢复证据。
- 首次设置 2FA：手工验证码通过仅证明激活请求成功，不证明保存的 seed 正确；尚未核实 seed 时显示 `totp_setup_seed_unverified` 并保留候选证据，不把候选密钥展示为当前密钥，也不重新登记。重置流程仍保留独立的意图、派发与只读核实约束。
- 批量提交：`queued` 只表示已受理，`skipped` 是资格跳过，`failed` 是确定提交失败，`unknown` 需先核实；它们都不表示授权或上传完成。部分提交的 HTTP 200 同样可能是 `outcome=partial`、`ok=false`。界面只移除确定入队项，保留失败、跳过和未知项的选择；确认框关闭后先检查结果再重新操作。
- 批量导入中途失败：HTTP 409 仍可能已有账号资料保存。检查回执中的 `completed`、`created`、`updated`、`failed`、`unknown`、`unprocessed` 与 `stoppedAt`，先核对列表，再仅处理未完成资料；不要把整批错误理解为整批回滚。导入不会自动登录。
- 浏览器保存失败：界面说明服务端是否已生效及本浏览器是否保存；不要把未持久化的偏好写成“已记住”。

当前功能契约、修改依据及实际验收边界见[功能台账](refactor-ledger.json)。[原维护台账](maintenance-review-2026-11-10.md)、[第一轮质量优化](quality-review-2026-10-31.md)和[第二轮增量优化](quality-review-2026-10-31-round2.md)保留各自历史版本的验证范围。

## macOS 本地测试

```sh
./scripts/local-legacy-macos.sh start
./scripts/local-legacy-macos.sh status
make frontend
TOSUB2_LOCAL_REBUILD=1 ./scripts/local-legacy-macos.sh restart
./scripts/local-legacy-macos.sh stop
./scripts/local-legacy-macos.sh clean --yes
```

默认地址为 `http://127.0.0.1:4401`。此脚本名只为沿用该实例路径；运行实现与普通主程序相同。`stop` 保留资料，`clean` 删除本实例运行目录和专属 Keychain，保留外部备份。准确范围见 [本地测试与卸载](local-macos.md)，实际部署验收见 [部署记录](pure-go-runtime/local-deployment.md)。

## Docker

Dockerfile 的构建阶段使用 Go 构建前端和主程序，运行阶段包含程序、CA 证书和健康检查工具。容器不安装外部脚本运行时或浏览器自动化依赖。

```sh
docker compose build
docker compose up -d --wait
docker compose ps
docker compose logs --tail=50 console
```

打开 `http://127.0.0.1:4399`。`TOSUB2_DOCKER_PORT=4418 docker compose up -d --wait` 可更改宿主端口。容器内监听 `0.0.0.0:4399`，宿主仅发布到 loopback。Mac 宿主代理使用 `http://host.docker.internal:实际端口`；容器里的 `127.0.0.1` 指向容器自身。

默认 UID/GID 为 10001，私有命名卷挂载 `/data`；根文件系统只读，临时文件使用 `/tmp`，账号跨进程锁使用 `XDG_CACHE_HOME=/data/.cache`，避免写入只读的用户主目录。锁文件只含互斥身份摘要，不能在运行中删除。init 回收 worker，SIGTERM 允许 30 秒优雅退出。自定义 bind mount 需允许运行用户以私有权限写入。

Compose 使用两个独立命名卷：`console-data` 保存账号、配置和加密凭据，`console-key` 保存 32 字节随机主密钥。一次性服务 `credential-key-init` 以 UID 10001 运行以下初始化器，成功退出后才启动 `console`：

```sh
/usr/local/bin/tosub2-key-init \
  --key-file /run/tosub2-secrets/credential.key \
  --credential-root /data/credentials
```

初始化器仅在首次部署且没有既有加密资料时生成密钥。重复运行会校验并保留已有密钥；已有数据而密钥缺失、损坏或不匹配时拒绝创建替代密钥。运行中的 `console` 只读挂载 `console-key`，通过 `TOSUB2_CREDENTIAL_KEY_FILE=/run/tosub2-secrets/credential.key` 读取。账号凭据和 2FA 恢复证据均加密持久化，保持账号及用途绑定，保存后继续执行原有读回校验。

密钥文件由操作系统文件权限保护，不是 Linux 原生 Keychain/DPAPI 服务。必须保护 Docker 主机与密钥卷访问权限。备份时将 `console-data` 和对应的 `console-key` 配套保存、分开保护；只有数据卷无法恢复凭据，丢失主密钥后不可通过重新生成密钥解密旧资料。

从旧版仅内存的 Linux 实例升级时，现有密码、TOTP 和 API key 不在数据卷内。停止旧进程前需完成受控迁移，否则升级后重新提供这些资料；新的加密后端不能找回已经丢失的内存值。旧版因 `TOTP_RESET_PERSISTENCE_REQUIRED: 重置 2FA 需要可用的系统安全凭据存储` 被拒绝的重置，在该检查处尚未发送远端 2FA 修改请求。配置加密存储后仍需用户重新发起操作，不会自动重试旧重置。

```sh
docker compose stop
docker compose up -d --wait
docker compose down
# 确实要永久删除本部署数据及主密钥时：
docker compose down --volumes
```

前三条命令保留两个命名卷；`down --volumes` 会同时删除数据卷和密钥卷。新后端成功保存的密码、TOTP 和 API key 使用同一主密钥跨容器重建读取。容器测试入口 `scripts/test-container.sh` 在已有镜像中挂载静态 Go 测试二进制，禁用外网，只运行本地合成 fixture；结束后删除临时测试文件，不修改部署数据卷。

## Linux systemd 用户服务

单独构建密钥初始化器：`go build -o bin/tosub2-key-init ./cmd/credential-key`。它只用于首次配置或验证已有密钥，不启动控制台。

模板为 [tosub2.service](../deploy/tosub2.service)：

```sh
install -d -m 700 "$HOME/.local/bin" "$HOME/.local/state/tosub2" "$HOME/.config/systemd/user"
install -m 700 bin/tosub2 "$HOME/.local/bin/tosub2"
install -m 600 deploy/tosub2.service "$HOME/.config/systemd/user/tosub2.service"
systemctl --user daemon-reload
systemctl --user enable --now tosub2
journalctl --user -u tosub2 -f
```

需要设置或重置 2FA 的 Linux 服务应先使用 `tosub2-key-init` 初始化独立私有目录中的密钥，并在该用户的 systemd unit 中设置 `Environment=TOSUB2_CREDENTIAL_KEY_FILE=/absolute/private/path/credential.key`。初始化器的 `--credential-root` 必须指向实际输出目录下的 `credentials`；主密钥必须是当前运行 UID 所有、权限 `0600` 或 `0400`、长度恰好 32 字节的普通文件，不能使用符号链接。不要把密钥内容放在 unit、命令行参数或环境变量中。

`systemctl --user stop tosub2` 优雅停止。卸载注册先 `systemctl --user disable --now tosub2`，再删除已安装的 unit 并执行 daemon-reload；数据目录、密钥文件与二进制独立保留。未配置主密钥的服务仍使用内存凭据，无法执行要求持久化的 2FA 操作。本轮未在目标 Linux 机器安装 systemd 服务。

## 可选 macOS 登录自启动

本地测试脚本没有安装登录自启动 Agent。需要另行部署时，可使用 [com.tosub2.console.plist](../deploy/com.tosub2.console.plist)，将 `__HOME__` 替换为当前用户绝对路径并正确 XML 转义。

```sh
install -d -m 700 "$HOME/.local/bin" "$HOME/Library/LaunchAgents"
install -m 700 bin/tosub2 "$HOME/.local/bin/tosub2"
# 将替换好路径的 plist 放入以下位置，再校验和注册：
plutil -lint "$HOME/Library/LaunchAgents/com.tosub2.console.plist"
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.tosub2.console.plist"
```

停止并注销使用 `launchctl bootout "gui/$(id -u)/com.tosub2.console"`。该独立安装不属于本地脚本的 clean 范围，端口与数据目录也应独立。

## Windows

`install-task.ps1`、`install-service.ps1` 与共用的 `windows-paths.ps1` 必须保持在同一目录。两个安装入口都会按执行时的 PowerShell 当前位置，将相对 `OutputDirectory` 一次性解析为绝对文件系统路径；创建目录与启动参数使用同一路径，空格和末尾路径分隔符按 Windows 参数规则处理。原生 Windows 构建产物为 `tosub2.exe`。

使用当前用户任务模板 [install-task.ps1](../deploy/install-task.ps1)：

```powershell
.\deploy\install-task.ps1 -Binary C:\toSub2\tosub2.exe
Start-ScheduledTask -TaskName toSub2-Go
```

删除注册使用 `Unregister-ScheduledTask -TaskName toSub2-Go`。Task Scheduler 的强制停止不等同于优雅退出；需要前台交互退出时在终端运行程序并按 Ctrl+C。

原生 SCM 服务使用 [install-service.ps1](../deploy/install-service.ps1)，在管理员 PowerShell 执行：

```powershell
.\deploy\install-service.ps1 -Binary C:\toSub2\tosub2.exe -OutputDirectory C:\toSub2\private-data
Start-Service toSub2-Go
Stop-Service toSub2-Go
```

安装交互读取当前 Windows 用户的服务登录凭据，由 SCM 保存；账户需有“作为服务登录”权限。服务与控制台必须使用同一用户以读取 DPAPI。私有文件使用当前用户专属 protected DACL，不支持持久 ACL 的文件系统会明确失败。本轮未安装 Windows 服务；应在目标机器执行 `scripts/test-windows.ps1` 验证。

## 彻底卸载与备份范围

先停止本实例并确认所属 worker 全部退出，再决定哪些备份需要保留。备份范围应包含经过分类核对的业务资料（账号 metadata、加密凭据文件、配置、授权输出、纯 token 保存结果及 grant recovery sidecar），排除 Cookie、旧登录快照、待兑换 code 和未完成认证会话材料。以同一用户和原 namespace 恢复；Linux 加密存储还需匹配的主密钥文件，恢复后保留私有权限并设置正确 UID。密钥备份与数据备份分开保护。单独复制 metadata 不等于复制了可用凭据。不要将私有备份提交 Git 或同步到未获授权的位置。未配置加密存储的旧 Linux 进程内凭据不在磁盘备份中。

- macOS 本地脚本部署：`clean --yes` 删除该实例目录及专属 Keychain 条目；外部密文备份虽然不会被删除，也可能随这些密钥的删除而失去解密能力。若需保留恢复能力，应使用 `stop` 保留保护材料，完成 OS 级保护材料备份及独立恢复验证后再决定是否永久清理。浏览器站点设置需单独处理。独立 LaunchAgent 先 bootout，再移除对应 plist、可执行文件和明确的数据目录，不能借用另一实例的 clean。
- Linux 用户服务：先 disable/stop 并移除该应用 unit、daemon-reload；确认不再需要后，删除自己安装的可执行文件、该应用 state 目录及其专用主密钥。不要清理共享 Go 缓存、用户主目录或其他应用资料来“彻底卸载”。
- Docker：`docker compose down` 保留 `console-data` 和 `console-key`；明确确认永久删除本部署数据与主密钥时才使用 `down --volumes`。外部 bind mount 和手工备份不随该命令清除。
- Windows：计划任务先注销；SCM 服务先 `Stop-Service toSub2-Go` 并确认停止，再 `sc.exe delete toSub2-Go` 注销。确认备份范围后手动删除实际安装目录和该实例私有数据目录；不要清除用户 DPAPI 主密钥或其他应用凭据。
- 浏览器：清除对应主机和端口的网页配置／站点数据；这不会替代服务端数据清理，也不会关闭服务端账号查询开关。

卸载及删除备份不可作为修复未知授权结果、2FA 部分完成或磁盘故障的手段。恢复时先保留现场，按工作流已完成步骤继续；格式变更须先对备份副本验证迁移与回退，包含配置版本、保护凭据引用、回收站、历史和未完成的本地恢复证据。

## 当前验证范围

全项目日志与显示核查已在 Go 1.27.4 下通过 `scripts/verify.sh`，耗时 762.475 秒。1,202 份构建与测试输入前后摘要一致。真实 Chrome 通过 12 个合成场景。另完成五目标纯 Go 编译，以及 Go 1.26.0 的 117 个定向顶层测试、262 个子测试与五包 vet。此次核查覆盖页面、日志、CLI 与服务状态，运行容器仍使用上一次部署的镜像。逐项改动和证据见[功能台账](refactor-ledger.json)的 `continuation_20261124_project_display_audit`。

2026-11-24 账号第二行、2FA 快捷按钮与当前流程展示修正，已在 Go 1.27.4 完成 `scripts/verify.sh`，耗时 749.944 秒，1,267 份构建/验证输入前后摘要一致。全仓普通测试、race、vet、双份资源一致性、显式 Go/Sobek runtime、嵌套 engine-probe 和隔离生产构建通过。此前因修正一行浏览器测试 URL 而主动终止的验证保留中止证据，不计为通过。

2FA 状态检查点的真实 Chrome 158.0.8127.88 共 7 个叶场景最终通过，覆盖现有账号命令和受保护设置，以及桌面浅色、窄屏深色的 2FA 状态序列、当前输入、草稿与焦点、候选密钥读取和纯本地补保存弹窗。新夹具首次两项因本地 URL 缺少显式路径在浏览器启动前失败，修正测试 URL 后仅重跑这两项并通过；未放宽生产校验或测试预算。普通与 race 定向各通过 32 个顶层、24 个子测试，其一项浏览器 opt-in 跳过已由上述显式验收补齐。

该 2FA 状态检查点的五目标 CGO=0 生产编译及精确 Go 1.26.0 的相关测试/vet 通过，后者共 37 个顶层、49 个子测试，零失败、零跳过。隔离副本的 1,156 份输入保持一致；主仓同期仅变化上述未在该专项执行的浏览器夹具 URL，原完整性失败记录和精确差异已保留。这不是最低版本全仓或目标平台原生验收。新增 API 字段仅供展示，无凭据或数据迁移；没有真实登录、2FA 修改、模型或上传请求，也未部署本轮源码到运行容器。适用证据与边界见[功能台账](refactor-ledger.json)的 `continuation_20261124_active_totp_presentation`。

上一日志检查点（2026-11-24）的可读性、只读网络降噪和旧文本重复展示修复后的源码，已在 Go 1.27.4 完成 `scripts/verify.sh`，耗时 737.567 秒，1,259 份构建/验证输入前后摘要一致。全仓普通测试、race、vet、双份资源检查、显式 Go/Sobek runtime、嵌套 engine-probe 和隔离生产构建均通过；四个脚本目标及补充 Linux arm64 的 CGO=0 生产编译通过。精确 Go 1.26.0 的 resetlog/protocol/console 日志专项共 13 个顶层、86 个子测试通过，零失败、零跳过，相关三包 vet 通过；这不是最低版本全仓重测，也不是目标平台原生验收。

该日志检查点的真实 Chrome 158.0.8127.88 共 14 个叶场景通过，零失败、零跳过，耗时 50.106 秒；258 份纳入浏览器清单的输入摘要一致。覆盖关键进展/全部/异常、可读与原始复制、分页与读取失败恢复、旧事件重复次数及多行告警、独立步骤结果、焦点、1280/390 宽度和深色模式，并重新执行原有重置/本地补保存复制及设置页面集成。56 条日志、默认保留 20 条和收起 36 条是专用合成夹具，不是对用户真实日志的统计。原始全量复制夹具显式选择“全部”，原恢复证据断言保留。临时目录、夹具和自有进程均已清理；没有调用真实上游或读取真实账号数据，也没有部署到运行容器。逐功能结论、红测试及一次测试导入遗漏记录见[功能台账](refactor-ledger.json)的 `continuation_20261124_log_presentation`。

上一账号列表、精简跨页索引、轮询作用域和 URL 诊断脱敏检查点在 Go 1.27.4 完成 `scripts/verify.sh`，耗时 702.383 秒，纳入清单的 1,253 份构建/验证输入前后摘要一致；另通过精确 Go 1.26.0 的 jobs/console 定向测试（31 个顶层、57 个子场景，零失败和跳过）及 vet。证据保留在 `continuation_20261124_account_lists`，不用于代替本轮结果。此前凭据展示检查点的 710.080 秒完整验证仍保留在 `continuation_20261124_presentation`。

账号列表检查点在 macOS darwin/arm64、Apple M6、Go 1.27.4、单进程、30 次采样及每账号 20 条合成日志条件下测量了 100/1,000 账号。1,000 账号默认完整列表 p50/p95 从 22.439/23.426ms 降至 19.109/20.701ms；自带前端使用精简索引后为 6.276/6.764ms，响应从 2,886,070B 降至 601,096B。筛选两个邮箱的完整 API p95 从 2.258ms 降至 0.162ms；精简筛选样本为 0.197ms，不声称每个场景都更快。这是合成基准，不是生产 SLA；没有改变同步提交、凭据格式或恢复规则，也不需要数据迁移。这批优化尚未部署到运行中的本地容器。

账号列表检查点的真实 Chrome 验收共 17 个叶场景通过，零失败、零跳过；覆盖真实 React 的跨页选择、筛选及 token 乱序、资格变化、隐藏/离线恢复、窄屏、深色、焦点、剪贴板和沙箱预览。首次新夹具失败源于 JSON 请求体被二次编码；修正后又发现隐藏刷新会让离线提示被旧请求版本挡掉，现已通过独立红测试复现并修复。两次失败均保留，未延长时限或删除断言。253 份纳入该浏览器清单的输入摘要一致，18 份合成夹具、独立 profile 和临时目录已清理，无自有浏览器进程遗留。

上一所有权检查点在 Go 1.27.4 完成完整 `scripts/verify.sh`，耗时 857.538 秒，1,241 份输入前后摘要一致；精确 Go 1.26.0 全仓普通测试及 vet 通过，2,448 个顶层测试通过、零失败、23 项既有跳过。初次全仓发现的查询夹具问题已修正：准入凭据编辑取锁、释放各一次，组合工作流内部仍禁止查询重入；原请求次数、顺序、恢复证据与三秒预算未变。初次失败、APFS 补项前主动中止以及此前空 ID 失败均保留。本批凭据和浏览器宿主增量另通过精确 Go 1.26.0 定向测试及 jobs/console vet，最终提示补项的两项测试（含子测试共 6 项）也通过；不声明新测试已经完成最低版本全仓验收。

浏览器验收使用独立合成数据和 loopback，浏览器不是生产依赖。ProtectedSettings 现使用仅用于测试的 CDP 宿主：真实时间每隔 25ms 最多授予 25ms 虚拟时间，原 16 秒虚拟总预算、30 秒墙钟时限和安全断言不变；其他浏览器入口保留原驱动。端点、响应 ID、session、目标及顶层页面身份均校验，首次终态冻结，迟到成功不能覆盖失败。受控实验确认自由虚拟时间可能在宿主真实延迟结束前耗尽等待，但不能据此证明历史 iframe 偶发失败的原因。CSP 事件继续仅被动观察；同时核对 srcdoc 策略、fetch 拒绝及模拟服务零到达。共享测试进程树清理保留取消、超时、后代退出与无关进程存活断言。

2026-11-24 临时原生 launchd 合成 job 验收通过；最终提示补项后又单次验证当前 CGO=0 二进制，1 项通过、零失败、零跳过，耗时 14.50 秒，1,172 份输入摘要一致。覆盖空启动、健康检查、仅保存批量导入、两次 SIGTERM 正常退出且不被 KeepAlive 重启、显式 kickstart 后恢复及租约释放；上游 mock 请求为零。测试只注册唯一临时 job，没有安装长期 LaunchAgent，也没有操作真实实例或写入真实账号凭据。测试、目录及精确合成 Keychain 身份均已清理，锁文件保留。前三次夹具失败及最终通过证据均保留。执行方式与隔离细节见 [macOS 说明](local-macos.md)。

较早重构检查点的 `scripts/build-cross.sh` 完成 Darwin arm64/amd64、Linux amd64、Windows amd64 四目标 CGO=0 生产构建通过（5.959 秒），另补充 Linux arm64 编译（1.674 秒）；产物在隔离目录生成、记录摘要后清理，未替换运行二进制。双份 dist 检查通过。此前浏览器测试宿主的 Windows/Linux/Darwin amd64 编译保持原检查点；交叉编译不等于目标平台原生执行。上一所有权检查点的 Linux arm64 `scratch` 专项通过 40 个顶层测试和 47 个子场景，一个大小写别名测试因 tmpfs 区分大小写跳过；该检查采用 `--init`、禁网、非 root、只读根、独立 tmpfs，资源已清理。macOS APFS 别名专项通过 31 个顶层 race 测试和 10 个子场景。这些结果及更早的原生 Keychain、Linux 密钥恢复、查询/上传、四种合成 CLI 授权均保持各自源码检查点，不冒充最新源码全部重测。

上一检查点的 `govulncheck@v1.9.0` 主模块扫描没有可达调用或已导入包漏洞，但存在仅涉及模块的 `GO-2026-5932`：`golang.org/x/crypto/openpgp` 不在当前导入图。工具报告模块版本 `v0.58.0`，修复版本 `v0.58.1-0.20261009181718-2f1f834330b2`；`cases` 与 `unicode/norm` 已在当次扫描图。本增量没有更改生产依赖；上述结论不代表没有任何公告。嵌套 `engine-probe` 已于本次另用 Go 1.27.4 和相同固定版本扫描器完成检查，退出码 0，未报告已知漏洞，9 个模块文件及 go.mod/go.sum 摘要未变；首次扫描器启动前的离线工具解析失败也已保留。

2026-11-24 曾用正式 Dockerfile 构建 Linux arm64 镜像，并在断网、非 root、只读根、独立数据卷和密钥卷中完成合成验收。随后的本机部署先核对任务状态，再备份同时间点的数据和密钥，隔离演练通过后才切换。验收确认启动健康、账号状态保留、前端资源一致以及维护状态读回。该部署早于当前 GitHub 源码版本。

部署回退前应核对升级后是否发生远端修改，再使用旧镜像和同一时间点的数据卷、密钥卷快照。该检查点的旧监控实现不支持新版 `credentialRef`。本机容器、卷标识及完整演练记录保存在本地取证副本中。

Windows 原生 DPAPI/DACL/SCM/PowerShell、systemd 服务、长期 LaunchAgent 安装和真实上游仍未验收；本次本地 Docker 部署不替代这些验证。根项目未附 LICENSE，`fhttp v0.6.9` 完整 fork 分发许可仍待确认，历史 iframe 间歇失败根因也未闭合。这些限制仍阻止宣称完整商业交付。
