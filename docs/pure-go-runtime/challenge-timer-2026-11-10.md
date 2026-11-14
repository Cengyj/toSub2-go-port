# 2026-11-10 challenge 定时回调兼容修复

本轮修复了一个能在纯本地夹具中产生相同错误摘要的 JavaScript 宿主缺陷。
真实历史脚本的出错语句尚未取得，不能将合成复现等同于线上根因已经完全确认。
此前 [11 月 9 日的修改](challenge-script-exception-2026-11-09.md) 改善了错误阶段诊断，
本轮进一步修复定时器执行语义；没有放宽挑战成功判断。

## 事故证据与定位边界

只读核对 4401 运行目录中的任务元数据，匹配到任务
`job-84f147237096ae42be90` 在北京时间 14:23:56 开始、14:24:02 失败的
`relogin` 操作。失败阶段为 `legacy_challenge`，后续上传已跳过。
仅记录时间、阶段和布尔标志，没有输出账号、凭据、Cookie 或远端脚本正文。

该记录仍有 `totp_reset_phase=credential_persisted`、
`totp_activation_verified=true`、`totp_reset_committed=true`。
这里的错误属于后续重登授权，不能据此推翻已确认的 2FA 激活与保存，
也不应重做 2FA。`result_saved=true` 表示仍有既有保存结果，不代表本次重登成功。

| 错误字段 | 可以确定的事实 |
| --- | --- |
| `stage=parent_execution` | 父页面正在执行 |
| `frame=false child=false init=false extraParams=false execute=false` | 相应事件尚未被宿主观察到 |
| `requests=2 last_http=200` | 已处理两个运行时请求，最后响应为 HTTP 200；不是挑战通过证据 |
| `parent_errors=1 child_errors=0` | 记录到父运行时异常，未记录子运行时异常 |
| `SCRIPT_EXCEPTION [phase=timer_callback]` | 定时回调执行边界捕获普通或自定义抛出；不包含原始出错语句 |

可用 HAR 日期早于本次事故，不能据此还原本次脚本。只做只读结构检查和摘要校验，
没有修改 HAR、导入其认证材料或执行真实响应。没有主动请求远端挑战或操作真实账号。

## 已证实的两个宿主缺陷

旧 `setTimeout` / `setInterval` 将用户回调包在箭头函数中，再以 `fn(...args)` 调用。
虽然 Go 层给外层回调传入了全局对象，箭头函数不接收该 `this`，内部普通调用也不会
把它传给用户函数。因此严格模式回调得到 `undefined`，与浏览器定时器提供所属窗口
全局对象的行为不符。依赖 `this.document` 或显式检查接收者的代码可能在创建 iframe
之前抛错。显式绑定函数和箭头回调的自身接收者则应继续保留。

另一个缺陷是非函数 handler 到触发时才执行 `String(handler)`：注册后对象变化会改变
执行源码，转换异常也被推迟到异步阶段。正确行为是在注册时转换一次；转换失败应同步
返回调用方，并且不能留下待执行任务。重复定时器也不能每次重新转换。

生产修改仅在 `internal/jshost/runtime.go` 的定时器包装处：

- 捕获初始化时的 `Reflect.apply`、`String`、间接 `eval` 和所属全局对象。
- 函数回调用捕获的 `apply` 提供窗口接收者及原参数，保持 bound/arrow 的语义。
- 字符串回调在注册时转换，并在所属全局环境执行，不暴露包装闭包。
- 保留原 Go 排程、取消、interval 间隔、任务上限、realm 清理、进程和时间限制。

没有吞掉异常或将异常变成成功。真正的回调抛错仍经过原 `timer_callback` 分类及脱敏；
challenge 仍要求新 clearance 和后续页面验证。没有新增远端重试、Cookie 落盘或登录恢复。

## 行为复现与复审

第一轮先在旧生产代码上运行新增测试：strict receiver、注册时字符串转换、同步转换失败
三组测试按预期失败。完整合成流程在两个本地 HTTP 200 响应之后，严格定时回调检查
`this === globalThis` 并抛出普通 Error，复现用户的完整错误摘要：

```text
LEGACY_RUNTIME_CHALLENGE: challenge execution failed [stage=parent_execution frame=false child=false init=false extraParams=false execute=false requests=2 last_http=200 parent_errors=1 child_errors=0]: GO_JS_RUNTIME: SCRIPT_EXCEPTION [phase=timer_callback]
```

第二轮修复后，同一测试完成：挑战页 → 资源一 → 资源二 → 子页面 → 完成 POST → 验证 GET。
父页面创建 iframe，子页面收到 execute 后通过严格定时回调发送 complete；只有服务端
响应完成 POST 才设置合成 clearance。测试严格断言方法、顺序、次数、旧挑战不可复用，
以及操作后私有运行输入已删除。新端到端测试连续 10 次 race 通过。

第三轮由独立审查核对取消、资源限制、消息顺序和成功门槛；补充原始 intrinsic 被替换、
父子 realm 隔离、interval 只转换一次、bound/arrow 接收者及参数身份测试。已有真实异常、
旧 clearance、验证返回挑战页或 403、取消超时和敏感信息过滤夹具继续通过。

主要文件：`internal/jshost/runtime.go`、`internal/jshost/timer_semantics_test.go`、
`internal/legacyruntime/challenge_timer_semantics_test.go`。没有修改业务持久化格式、
前端、依赖版本或真实业务凭据。

## 最终验证和运行实例

工具链为 Go 1.27.3，平台 darwin/arm64；最终代码与开始验证时的源码摘要一致。

| 验证 | 结果 |
| --- | --- |
| `go test -count=1 ./...` | 28 个包通过，118.14 秒 |
| `go test -count=1 -race ./...` | 28 个包通过，142.16 秒 |
| `go vet ./...`、`go build ./...` | 通过 |
| `TOSUB2_LEGACY_RUNTIME_TEST=1 go test -count=1 -race ./internal/jshost ./internal/legacyruntime ./cmd/tosub2` | 通过，104.17 秒 |
| `go run ./cmd/build-web -check` | Go esbuild 重建比对通过，两份静态资源与源码一致，无产物变化 |
| Go 格式、`git diff --check`、新增文件空白检查 | 通过 |
| CGO 禁用、trimpath、裁剪调试信息的独立候选构建 | 通过；没有覆盖工作区 `bin/tosub2` |

全仓测试默认跳过的认证扩展项已由上述显式命令补跑；辅助子进程测试入口按设计由
父测试启动。`TestPublicSDKInitialization` 因没有 `TOSUB2_PUBLIC_SDK_FIXTURE` 跳过，
没有为此抓取线上 SDK。本轮未再次启用 Keychain、长时巡检与长请求预算测试，
未运行 Windows 原生验收或 Linux 独立生产二进制夹具；本轮没有更改这些平台实现。
跳过项不计为通过，上一轮维护的结果也不冒充本轮重新执行。

完成三轮审查及上述验证后，按既有授权更新 4401 一次。切换前确认账号操作、探针、
查询、巡检与回收站任务空闲；暂时停用原已启用的巡检并再次核对空闲，完成后恢复
原配置。未取消新任务，也没有通过真实登录、2FA、token 刷新、模型探针或上传验收。

- 运行目录：`/Users/ceng/Library/Application Support/toSub2-local/6428e6fb3abbc4ae-legacy`。
- PID：`58319` → `93194`。候选程序 SHA256：
  `2e9e9ab840fd5582121084db26a6015807f618f2a667f4fb8d5ec5b53d8bcd61`。
- 前后均为 11 个账号、7 个回收站条目；477 份账号相关文件摘要不变。
  账号身份/状态、查询/探针/回收站配置、巡检配置及 launchd plist 校验一致。
- 健康检查通过；实际 HTTP 提供的 HTML、JS、CSS、第三方声明与候选逐字节一致。
- 只备份旧程序至运行目录下
  `backups/program-before-timer-fix-20261110-2gnyk2np/tosub2`，未备份 Cookie 或旧认证会话。

部署检查脚本第一次准备时将 `git diff --no-index --check` 的“存在新增差异”返回值 1
误认为检查失败，未生成最终验证清单；部署门禁在任何服务操作之前拒绝执行。
修正该审计脚本的返回值判断并确认没有空白错误后才实际切换，没有额外停启运行实例。

本轮证据目录为
`/var/folders/08/gy1fn1f90kd0t95s02d35mk80000gn/T/tosub2-timer-review-20261110-w9hvjkc6`，
包含合成测试、源码增量、脱敏阶段及部署摘要，不包含真实认证材料副本。

## 剩余不确定性

本轮证明的是两个宿主兼容缺陷已修复，且该缺陷能够产生相同错误摘要。摘要刻意不包含
真实异常正文、堆栈和认证脚本，因此无法反推出历史真实脚本的准确触发点。
本地成功不保证当前上游挑战必然成功，其他未覆盖的浏览器 API 差异仍可能存在。
后续若仍失败，应依据新的当次操作和脱敏阶段继续定位；不能为此重做已完成的 2FA、
复用旧挑战、恢复旧会话，或把 HTTP 200 当作成功。
