# 完成条件逐项复核

目标来源：`/Users/ceng/.codex/attachments/c3f20341-992b-4bd5-b1b6-2f2ada8ce7a4/goal-objective.md`。2026-09-26重新全文读取，保留原50项功能、29项测试、全部安全约束及15项完成条件；不把目标缩减为本地fixture通过。

当前结论：**尚未证明完整目标达成。** 原生Windows关键凭据测试未执行，授权真实账号及第三方服务流程也有未验证部分。现有代码、测试和发布产物继续保留，原Node/Python参考项目不删除。

## 证据范围

- `go-port-acceptance.md`的50项功能与29项测试表分别列出实现、本地证据和缺口。
- `go-port-audit.md`的第1至12节覆盖要求的模块、状态、API、JSON、环境、TLS、挑战、凭据、旧测试、模块映射、兼容风险和HTTP/Chromium分界。
- `go-port-verification.json`、`go-port-linux-verification.json`、`go-port-binary-verification.json`记录实际执行，不能将其中SKIP、交叉构建或本地fixture改称原生Windows／真实账号验收。
- 源码清单`go-port-go-source-manifest.json`的250个文件在本次复核开始时逐一重算SHA，均匹配576e668a6116ea8ce29050842ad678260e7038872e316a0ae7a9e53a584d8b89。后续源文件变化必须重跑相关验证并重新绑定证据。

## 15项完成条件

| # | 原要求 | 当前证明及边界 | 状态 |
|---|---|---|---|
| 1 | Go服务独立启动 | macOS发布产物的临时HOME/最小PATH；Linux原生二进制的最小及空PATH启动，health/bootstrap/静态文件/鉴权/SIGTERM实际通过 | 已证明本机及Linux |
| 2 | 生产不需要Node.js | 生产子进程仅配置的Chromium及macOS系统security；React预构建并embed；Linux空可执行搜索路径仍启动Go服务 | 已证明受测生产入口 |
| 3 | 生产不需要Python | 同上；Python仅本次开发验收编排使用，不进入Go生产链 | 已证明受测生产入口 |
| 4 | 普通协议走Go transport | protocol.Client调用transport.Doer；native/tls-client的真实HTTP链、headers/Set-Cookie/手动跳转测试和生产CLI fixture已执行 | 本地链已证明；真实认证待验 |
| 5 | TLS profile、代理、session rotation可用 | 实际ClientHello/ALPN及profile降级、四种代理、sid/session、取消/设备绑定测试通过 | 本地已证明；真实代理供应商待验 |
| 6 | 三类挑战真实浏览器可工作或明确未完成原因 | 真实Chromium本地iframe/loader/SDK/SO/挑战观察测试；公开真实Sentinel可视运行成功。无头上游曾403；认证端点接受和完整Cloudflare/Turnstile分支未验，原因已明确登记 | 实现及边界已登记；不宣称完整上游成功 |
| 7 | 前端全部交互可用 | 保留React；真实Chrome覆盖分页、多选、操作、输入、下载及API链。真实账号runner和外部后端仍使用fixture | 本地交互证据存在；全部真实流程尚未证明 |
| 8 | 旧JSON可读或有迁移工具 | 六类golden、旧每任务metadata/聚合metadata、旧凭据和checkpoint恢复、未知字段与大整数回归；新tokens_received/refresh sidecar明确仅Go支持 | 已采样旧格式已证明；不承诺未见扩展数据 |
| 9 | 恢复、取消、重试、关闭正确 | 20任务FIFO/邮箱锁、20类恢复组合、一次性grant跨进程恢复、monitor重传、服务及浏览器取消、launchd启停有实际证据 | 已证明受测组合；系统掉电/真实会话过期待验 |
| 10 | macOS/Windows构建及关键凭据测试 | macOS构建与真实Keychain跨进程测试通过；Windows二进制和测试二进制仅交叉编译，DPAPI/DACL未在Windows执行 | **未达成** |
| 11 | go test ./...通过 | macOS/Linux均15包通过，具体版本、命令、事件见机器记录；Linux Go1.22.0最低版也通过 | 已证明受测源码 |
| 12 | go test -race ./...通过 | macOS/Linux全仓及显式真实浏览器测试通过；Windows race尚未运行 | 已证明macOS/Linux，不能扩展为Windows |
| 13 | README-Go安装等内容完整 | 已存在安装、浏览器、启动、CLI、代理、凭据、测试、恢复及故障排查节；实际启动路径和部署限制对应测试 | 文档已提供 |
| 14 | 迁移状态列齐六类内容 | 模块、未完成、旧版差异、Go依赖、实测浏览器版本、真实账号待验均有专节 | 文档已提供 |
| 15 | 如实报告是否全部功能完成 | README/状态/验收表均明确未完成真实账号、第三方服务及Windows原生验证；没有把cross-build或SKIP称PASS | 持续遵守；总体仍未完成 |

## 安全约束核对

| 要求 | 当前实现与执行证据 |
|---|---|
| 日志/普通job-meta/公开API不泄漏密码、TOTP、验证码、Cookie、token、代理密码或管理员key | logging脱敏、JobEvent明确白名单、私有结果隔离、sidecar canary遍历公开HTTP路径；其余字段与导出边界见50项表第49项 |
| 下载Content-Disposition正确 | console单/批下载HTTP断言及真实Chrome写入下载文件后解析 |
| 原子替换与安全权限 | privatefile先建权限、fsync/rename、POSIX模式实测；Windows protected DACL代码和测试存在但原生执行缺失 |
| HTTP context超时、子进程取消清理 | config请求预算、mail/provider/Sub2API预算、长于30秒真实测试、Chrome/代理连接取消及进程组/Job Object实现；Windows Job Object尚待原生执行 |
| LAN明确提醒、默认不公开 | config默认127.0.0.1，runConsole非loopback警告；实际smoke仅loopback |
| 不把真实凭据写入仓库 | 本次使用合成账号/golden/fixture和独立OS测试条目；未读取或提交真实账号、供应商密钥、远端管理员配置进行测试 |
| 真实浏览器/授权流程处理安全验证 | 使用原生CDP及同一浏览器会话；没有通过伪DOM、自动点击绕过、关闭CSP或因403换身份补成功 |

## 当前不可自行补造的证明

1. Windows本机执行DPAPI CurrentUser、私有DACL、跨进程恢复、Job Object及服务测试。当前执行主机为macOS；本次检查未发现Parallels/VirtualBox/QEMU/UTM命令，工具清单也没有可用Windows远程执行连接。此观察仅针对当前已开放环境，不宣称用户没有其他Windows机器。
2. 用户本人或明确授权账号以及允许修改密码/TOTP、绑定手机等操作的范围；真实邮箱/SMS/Sub2API和代理配置。不能根据参考源码中的地址、旧数据或未确认的配置推定账号授权，也不能购买短信来填补验收。

这些条件未取得前目标保持未完成。发现可修复的本地代码缺口应先修复和验证；不能只重复已有PASS，也不能用文档声明替代缺失的原生或远端执行。

## 本次复核实际进展

重新检查后修复了HTTP/2重拨context丢失、原生TLS握手取消连接残留、DPAPI返回前取消仍交付凭据、输入期间重复提交、CLI/协议密码被裁剪、自动邮箱抢读stdin、短信安全订单重启假忙以及短信输入事件竞争。后续Linux全仓又暴露取消与正常EOF竞态，已补body读取后的取消检查；没有把窄重跑暂时变绿当作修复。

20df源码首次全仓验收的Linux取消FAIL及macOS浏览器池旧状态断言FAIL均保留。浏览器池现在严格断言4个邮箱输入等待、16个working浏览器池等待、1个FIFO队列项；没有放宽到忽略未知状态。修复后使用新的独立源码快照重新执行全部验收。

只读证据审查确认：50功能与29测试编号连续完整，96个唯一测试引用有实际同名函数，12个审计章节齐备；全部要求API有静态或动态处理，六类JSON有真实读写/恢复入口。这个检查只证明引用和入口，不等于Windows/上游验收。

本轮最终受测源码为259文件，SHA-256为d071b72b90ba352f16ff81c5aebfbff6271bfd05347f677a7fb60b2a63baf9f1。macOS普通/race分别15包全部通过，780 PASS事件/395顶层/49 SKIP；Chrome/Keychain全仓race为830/433/11。Linux普通/race为781/396/45，真实Chromium全仓race为829/432/9，Go1.22.0最低版普通实际通过。五分钟monitor和31秒请求另外执行并记录，不加入全仓计数。发布构建、最小/空PATH smoke和临时launchd启停已通过。Windows原生和真实授权环境缺口仍未补足，因此总体完成条件依然未达成。
