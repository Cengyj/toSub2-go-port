# 纯 Go 运行时最终检查

> 历史验收：本文及 final-verification.json 记录彻底清理之前的宿主实现验证，包含现已删除的 Node/浏览器对照用例。旧计数、源码摘要及临时日志路径不代表当前工作树；原始临时构建日志已清理，当前部署和本轮验证以 local-deployment.md 为准。

日期：2026-10-12。macOS arm64。检查使用包含既有未提交修改的工作树，未重置、提交或发布；现有真实部署未重启。主要工具链 Go 1.27.1，最低版本和交叉构建另外使用 Go 1.25.0。所有测试使用 `-count=1`，没有将缓存结果计为本轮通过。

## 全仓与构建

| 实际命令 | 结果 | 顶层测试 PASS / FAIL / SKIP | 含子测试 PASS / FAIL / SKIP |
| --- | --- | --- | --- |
| `go test -json ./... -count=1` | 18 包通过，退出 0 | 669 / 0 / 71 | 1807 / 0 / 71 |
| `go test -race -json ./... -count=1` | 18 包通过，退出 0 | 669 / 0 / 71 | 1807 / 0 / 71 |
| `GOTOOLCHAIN=go1.25.0 CGO_ENABLED=0 go test -json ./... -count=1` | 18 包通过，退出 0 | 669 / 0 / 71 | 1807 / 0 / 71 |
| `go vet ./...` | 退出 0，无输出 | 不适用 | 不适用 |
| `CGO_ENABLED=0 go build -o bin/tosub2 ./cmd/tosub2` | 退出 0，已生成 macOS arm64 生产程序 | 不适用 | 不适用 |
| `GOTOOLCHAIN=go1.25.0 ./scripts/build-cross.sh` | 四个目标构建通过，均禁用 cgo | 不适用 | 不适用 |
| `git diff --check` / 修改后的 shell 脚本 `sh -n` | 退出 0 | 不适用 | 不适用 |

普通、race 的 JSON 首末事件跨度分别为 68.317 s 和 70.479 s，命令并行执行；这不是各包耗时之和。普通、race、公开 SDK 单项的 stderr 均为空。最低 Go 全仓同样是 18 包，scripts 包为 64.541 s。机器可读计数、包耗时、所有 skip 项及原始日志摘要见 [final-verification.json](final-verification.json)。

71 个 skip 是显式门控：现有 browser/可见窗口、真实 Keychain、线上浏览器诊断、长时网络/monitor、专用 helper、Node 依赖链接检查，以及默认不启用的完整登录和公开 SDK 测试。后两项已另行实际运行并通过，见下表。其余 skip 仍未验收，未算作 PASS。与 [迁移前基线](baseline.md) 对照，没有新增失败；新增 jshost 包与相关测试进入全仓。

## 宿主、业务与依赖独立性

| 层次 | 实际证据 | 边界 |
| --- | --- | --- |
| 宿主语义 | 全仓实际运行 DOM/iframe/prototype/event、origin/source/structured clone/transfer、微任务/定时器、公平调度、fetch/XHR/二进制、Worker、Stream、Storage、取消与资源子进程测试；近期审查回归另通过 race 3.314 s | 有限浏览器宿主，明确不支持项见 host-requirements.md |
| 现有 Node 对照 | 相同本地 Go transport/SDK/CF 契约、十种生产 runner 场景；最后父 bridge 诊断变更后的 Node runtime/cancel race 10.567 s，十场景 runner race 7.826 s | 比较结果结构、调用、网络、cookie、消息和状态，不比较随机 token 字符串 |
| 完整本地 Go 协议 | 冻结源码的实际密码、MFA、邮箱 OTP、拒码纠正、重发、checkpoint、OAuth 导出；普通 11.121 s，race 12.006 s | 合成 TLS 服务和账号输入 |
| 公开 SDK | 匿名下载的真实 loader 动态发现 SDK 路径；最终 `TestPublicSDKInitialization` PASS 0.03 s，包 0.261 s；执行父子两 realm、检查 token/observer 接口且禁止初始化额外请求 | 没有调用真实 token 方法或账号登录 |
| 隔离生产入口 | 重新编译实际生产 `/tosub2` 与独立 driver；`FROM scratch` 仅两静态 Go 程序，`--network none`；十场景 PASS 2.75 s；`/proc` 验证认证 worker 同一可执行文件 | Linux arm64 实际执行；CA 由 fixture 提供，没有关闭 TLS 校验 |
| 取消与隔离 | 真 Go 子进程 deadline、四处 CF 父子取消、并发会话/存储隔离、无限 JS/微任务、Busy Worker、内存 guard 退出与新 worker 恢复 | 内存 guard 是采样软保护，非 OS 硬内存上限 |
| 真实账号/当前线上 challenge | 未执行 | 无获准测试账号；不以 fixture 或公开 SDK 初始化代替 |

公开 SDK 字节身份保存在 [sdk-manifest.json](engine-probe/sdk-manifest.json)，SDK SHA-256 为 `4f8ef8d5870894fd0101fc40ff45ea13c0f8e25c71c2ba28e5df5baf98babbb5`。下载的 JS 没有提交到仓库，也没有在生产永久固定该版本。执行命令：

```sh
TOSUB2_PUBLIC_SDK_FIXTURE=/absolute/path/to/anonymous-loader-and-sdk-directory \
  go test -json ./internal/jshost -run TestPublicSDKInitialization -count=1
```

生产 CLI 容器的完整命令、十个场景的原始安全输出、构建身份及清理记录见 [production-fixture-verification.md](production-fixture-verification.md)。测试结束只删除该次拥有的容器、镜像和临时构建目录，没有全局 Docker prune 或真实数据清理。

## 产物与源码身份

生产与测试源码集合摘要：`67998d5a674956324d84f7bea81a1fa86ee538bfdbb4dd6caae840ba508b0a00`。计算规则见生产验证文档；该规则不包含 Markdown、脚本包装器或运行数据，不等同于干净 Git commit。源码冻结后最终 scratch 重编译前后摘要一致，本轮最终再次核对一致。工作树继续保留前序未提交改动。

| 产物 | 工具链 / 目标 | SHA-256 |
| --- | --- | --- |
| `bin/tosub2` | Go 1.27.1 / darwin arm64 | `043ef51fca39d96b0278e0c3145e0125475616b35ace017b3125500f3eb7c216` |
| `bin/tosub2-darwin-arm64` | Go 1.25.0 / darwin arm64 | `d6b1bd80b57da2037a66cfe4e69b427a50a6a12e5fec6e929f2504692ff3eb68` |
| `bin/tosub2-darwin-amd64` | Go 1.25.0 / darwin amd64 | `8e606306e90f4d0b0592345bafe8f1cbf929c192b3de1c6f2767e16928ac69d2` |
| `bin/tosub2-linux-amd64` | Go 1.25.0 / linux amd64 | `5a92611f7292827f483f55d097b7887e33c0e9437ad8cb463dfe09304ca32c9a` |
| `bin/tosub2-windows-amd64.exe` | Go 1.25.0 / windows amd64 | `1eedd08678f8367d38ab08c703e8c18bafd3e1477a70f7bd88db5ff884452574` |

四个平台产物的 build metadata 均为 `CGO_ENABLED=0`；它们的交叉构建成功不代表四个平台实机执行成功。实际本轮执行环境是 macOS arm64 及 scratch Linux arm64。`go list -deps -json` 的最低 Go/cgo 禁用生产依赖共 376 包，零 CgoFiles。Linux amd64 为静态 ELF；Windows PE 导入表只有 `kernel32.dll`；macOS 有正常系统框架引用，没有 JS 引擎动态库。没有把 macOS 系统动态链接误称完全静态。

`bin/AUTH_RUNTIME_THIRD_PARTY_NOTICES.txt` 与源许可证全文字节一致。本轮选型及依赖版本固定在 go.mod/go.sum；主模块不链接探针对照用的 Goja。脚本包装器的实际无外部 JS 运行时行为和旧实例升级/清理规则进入全仓 scripts 测试。

## 需求完成审查

- 保留工作树与基线：保留既有差异，有独立迁移前测试记录及用户要求的完整可恢复项目备份。
- 选型：实际执行 Sobek/Goja 语法、异步、异常、中断和跨 realm 探针；最低 Go 与许可证分别验证。
- 宿主：执行动态 loader/SDK、CF 父子路径和必要 Web API，没有空 Worker/ReadableStream 冒充成功；范围外能力明确记录。
- 会话：HTTP/TLS、Cookie、代理、设备身份、checkpoint、输入消费和授权导出由原 Go 路径持有，fixture 检查约束与失败恢复。
- 接入：生产 main、CLI、console、配置、恢复、取消、错误展示、构建和本地脚本均已接入。默认值保留，Go 显式选择且不回退。
- 验收：宿主/Node 对照/本地协议/公开 SDK/真实生产隔离入口及要求的全仓命令均有独立实际证据；真实账号等未获验证条件的项如实列为未验证。
- 交付：源码、测试、单可执行文件产物、许可证、可复现命令、架构与取舍、限制及 Node 使用范围均已提供。远端 CI 未运行，不宣称其已通过。
