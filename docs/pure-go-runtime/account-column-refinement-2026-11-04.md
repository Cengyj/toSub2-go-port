# 三列内容精简与视觉复审

日期：2026-11-04。范围为账号表格的操作类型、已用比例、推理探针单元格。继承当前 `main` 全部未提交成果，表头和业务判定保持。已于当日 10:43（Asia/Shanghai）完成 4401 部署与在线验收。

## 已落实的设计

- **操作类型**：14px 操作名与 11px 无框授权方式紧邻；下一层只保留一个 21px 高、11px 字号的状态胶囊。普通状态使用已完成、操作失败等短称；授权成功但同步失败、待核实、回传暂停等关键区别保留，完整文案仍在提示与无障碍名称中。整块摘要打开既有日志。
- **已用比例**：每个窗口独立一条 20px 计量行，11px 窗口名称、3px 连续细轨、右对齐的 14px 原始比例；两行相距 5px。底部一个 18px 说明行，正常明确显示“重置”及逐窗倒计时，历史／查询／错误／用尽／保存待核实优先。长数值仅数字部分省略，百分号始终保留，精确原值留在提示和详情。未知不转换成 0%。
- **推理探针**：比分与判定合成一个 28px 结果胶囊，分子 16px、分母及判定 11px。其它状态使用 23px 高、12px 字号的短状态；历史作为下一层 11px 说明。本次失败、排队、检测中、保存未确认不能被旧通过结果覆盖。整格仍是唯一只读入口，没有新增右侧按钮或箭头。

相关实现：[`account-operation.jsx`](../../web/src/components/account-operation.jsx)、[`styles.css`](../../web/src/styles.css)、[`account-usage.jsx`](../../web/src/components/account-usage.jsx)／[CSS](../../web/src/components/account-usage.css)、[`account-quality.jsx`](../../web/src/components/account-quality.jsx)／[CSS](../../web/src/components/account-quality.css)。基准列宽继续为操作 174px、用量 216px、探针 138px；分别有 150／192／114px 内容宽度。

## 复审发现与修正

独立审查先指出多种胶囊同时竞争、辅助字过小、未知用量重复三层说明、探针比分与六段图形重复。重做后再检查实际截图，继续修正：

1. 共享 `.account-pill` 的加载顺序把操作状态重新覆盖成 10px／18px；改用明确的组合选择器，浏览器最终计算值确认是 11px／21px。
2. 裸写 `5h 2h · 7d 3d` 无法区分窗口时长和倒计时；补上单个“重置”标识。
3. 实测“历史＋刷新中＋HTTP 401＋用尽＋待核实”宽 196px，超出 192px；仅缩小状态说明行的元素间距，保留每项含义，复测不再溢出。

第二次独立检查正常、长文案、密集异常等四张截图，未发现这些样本中必须继续修正的三列视觉问题。该复审是静态视觉检查；主流程另做实际交互、焦点和动态状态验收，没有用截图代替业务测试。

## 删除与保留

删除授权方式的重复胶囊、窗口和错误的重复边框、空用量装饰块与常驻重置未知行、探针六段图形及其提示／CSS。六段图形完全由比分派生，未提供额外业务信息；相应回归改为单复合结果、真实比分和分母保护，没有删除评分或历史判定。

未修改 `describeJob`、`describeQuality`、查询调度、认证、上传门禁或持久化格式；没有更改依赖。测试继续保护历史授权、同步未知、BPS 不支持、0–6 分、`3/4`／`0/4` 分母、真实 0%、未知窗口、历史失败及保存未确认。

## 验证

固定 `GOTOOLCHAIN=go1.27.3`，最终源码冻结后：

- 全部前端回归通过（4.097s）；重点回归见 [`frontend_indicator_contract_test.go`](../../internal/console/frontend_indicator_contract_test.go)、[`frontend_account_quality_presentation_test.go`](../../internal/console/frontend_account_quality_presentation_test.go)、[`frontend_usage_refinement_test.go`](../../internal/console/frontend_usage_refinement_test.go)。
- `scripts/verify.sh` 通过：gofmt、`git diff --check`、`go test -count=1 ./...`、`go test -count=1 -race ./...`、`go vet ./...`。
- `TOSUB2_LEGACY_RUNTIME_TEST=1` 下 jshost、legacyruntime、cmd/tosub2 显式 race 回归通过。Go esbuild 构建、`CGO_ENABLED=0` 生产构建通过，两份 dist 一致。
- `scripts/build-cross.sh` 通过 Darwin arm64／amd64、Linux amd64、Windows amd64。仅本机 Darwin arm64 原生运行验证，其余属于交叉编译。
- 无依赖变更；本轮未额外执行漏洞扫描、模糊测试或性能基准，不作服务性能提升声明。未做独立屏幕阅读器原生验收。

生产二进制 SHA-256：`a96f0192acddfc447e31697d7391a449b68e019dcdd426c304340d900cae5a8b`。资源：`index-f6f8562d7fed.js`／`index-2935b49f2ba2.css`。

## 浏览器验收

所有交互使用独立纯 Go fixture 和 `.invalid` 合成账号：

- 暖灰、纯白、深色；320／390／980／1440／1920px 均无页面级横向溢出，窄屏维持表格内部滚动。1920px 下表格填满 1593px 可用宽度。
- 三列摘要顶边一致。长复合状态 141.84px 未超过 150px；“自动重登授权”与 ChatGPT 同行约 139.73px，无意外换行。
- 双倒计时 `4h59m／6d23h`、双待确认与额外窗口 `+1` 在 192px 内完整显示；长比例 `33.333333333333336%` 不改原值，百分号不会一起被截掉。
- 同一业务样本下排队、执行、结束阶段行高变化 0px；检测中／失败保留历史 6/6，但不显示当前通过胶囊。新增业务提醒会自然增加行高，未将其误计为刷新跳动。
- Enter 展开／收起日志，Enter 打开两类详情，Escape 关闭并回到原入口；悬浮有反馈，整格保留足够点击区域。只读交互期间查询／探针提交均为 0。随后只显式触发 1 次合成探针，确认排队及 2/6 低于阈值；没有真实上游请求。
- 390px 探针弹窗宽 366px，无内部横向溢出，关闭后回焦。320px 空筛选 colspan=9，清除后恢复六行；无探针能力的八列分支由组件／App 回归覆盖。浏览器 warning／error 日志为空。

证据：[三列局部](ui/account-column-refinement-20261104-focus.jpg)、[页面上下文](ui/account-column-refinement-20261104-context.jpg)、[宽屏](ui/account-column-refinement-20261104-wide.jpg)、[深色](ui/account-column-refinement-20261104-dark.jpg)、[密集异常](ui/account-column-refinement-20261104-dense.jpg)、[长文案](ui/account-column-refinement-20261104-long.jpg)、[窄屏详情](ui/account-column-refinement-20261104-mobile.jpg)、[测量记录](ui/account-column-refinement-20261104-validation.json)。局部图只裁切真实截图，没有重绘界面；记录保留最初溢出及修复后结果。

## 交付边界

用户明确要求后续默认更新 4401、不再询问部署或维护窗口。本轮按该授权完成后台暂停、两次在线排空检查、优雅停服与离线状态复核、备份、原子替换、验收和原设置恢复。生命周期锁覆盖维护操作，但旧服务没有 HTTP 提交锁；离线复核用于发现检查与停服之间的新增工作，不能把脚本锁描述成全局提交锁。

- 新 PID **39684**，实际程序映射与已验收产物一致，仅监听 **127.0.0.1:4401**。运行二进制 SHA-256 与本记录生产产物相同；在线 HTML、JS、CSS 和来源清单共 4 项逐字节一致。
- 停服后完整备份 **36 个数据文件、2 个服务日志**、旧／新程序、原运行配置及设置；私有备份为 `<runtime-root>/backup-ui-refinement-20261104-2420277842/`，含校验回执与 `ROLLBACK.md`。加密凭据沿用原用户和 Keychain namespace，没有导出明文或数据格式迁移。
- 10 个账号均 completed；两条已有授权的待回传记录原样保留，它们是持久化同步意图，并非执行队列。升级期间巡检在内存和磁盘中均为禁用，没有为排空而登录或上传。
- 数据清单无新增或丢失；账号完整业务字段、授权及凭据保持。首次严格校验发现两条巡检冷却原因变化，按 `MonitorService.Load` 的既有脱敏逻辑精确验证后放行，未忽略任意差异。冷却期限与目标保持，原错误仍在私有备份中。历史日志前缀完整，停服／启动阶段无新增 warning/error。
- 原用量查询、自动查询与巡检已恢复，查询完整配置与巡检稳定配置复读一致；探针配置完全保留，回收保留天数仍为 0。最终无活动／排队任务，健康检查通过。恢复原自动计划后的正常工作不属于验收测试。

两个合成服务已停止，临时标签已关闭，视口已恢复；验证日志和中间截图已可恢复地清理。运行、备份、升级、恢复、回退与卸载见[本地部署说明](local-deployment.md)。最终分支 `main`，59 项已跟踪变更、143 项未跟踪路径，包含前轮全部成果；未创建提交或推送。
