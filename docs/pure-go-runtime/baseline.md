# 纯 Go 认证宿主迁移前测试基线

> 历史基线：本文记录已移除的 Go + Node 过渡实现。引用的旧源码、依赖和命令仅存在于外部项目备份，不应按本文重新安装或运行。当前认证与前端构建均使用 Go，见 README.md 和 local-deployment.md。

记录日期：2026-10-12。环境：macOS arm64，Go 1.27.1，Node 24.19.0，本地 jsdom 已安装。

这是当前含前序未提交修改的工作树基线：Go HTTP/TLS + Node/jsdom 动态脚本宿主。四组命令在新增 `CookieJar.DocumentCookie` / `SetDocumentCookie` API 修改前启动并完成；不包含纯 Go JS 宿主或新的脚本 cookie API。没有重置前序工作。

所有命令使用 `-count=1`，没有将缓存结果作为基线。仅使用本地合成 HTTP/TLS fixture；未启用真实账号、上游在线测试或真实 Keychain 集成。

| 检查 | 包 PASS / FAIL | 顶层测试 PASS / FAIL / SKIP | 含子测试 PASS / FAIL / SKIP |
| --- | --- | --- | --- |
| 全量 `go test -json ./... -count=1` | 17 / 0 | 578 / 0 / 70 | 1652 / 0 / 70 |
| 生产登录及恢复本地集成 | 1 / 0 | 1 / 0 / 0 | 1 / 0 / 0 |
| legacyruntime + transport race | 2 / 0 | 77 / 0 / 1 | 135 / 0 / 1 |
| 生产登录及恢复本地集成 race | 1 / 0 | 1 / 0 / 0 | 1 / 0 / 0 |

四组命令的退出码均为 0，stderr 均为空。PASS / FAIL / SKIP 按 `go test -json` 的终态事件计数；表中的“含子测试”包含父测试，不能与顶层数量相加。

执行命令：

```sh
go test -json ./... -count=1

TOSUB2_LEGACY_RUNTIME_TEST=1 NODE_BIN=<absolute-node> \
  go test -json ./cmd/tosub2 -run TestLegacyRuntimeProductionLoginAndResume -count=1

TOSUB2_LEGACY_NODE=<absolute-node> \
  go test -race -json ./internal/legacyruntime ./internal/transport -count=1

TOSUB2_LEGACY_RUNTIME_TEST=1 NODE_BIN=<absolute-node> \
  go test -race -json ./cmd/tosub2 -run TestLegacyRuntimeProductionLoginAndResume -count=1
```

`<absolute-node>` 为本机已安装的 Node 24.19.0 可执行文件。全量命令通过现有 `legacy/node.path` 找到 Node，因此 `internal/legacyruntime` 的全部动态脚本 fixture 实际执行，没有跳过。

相关包的实际结果：

| 包 / 检查 | 普通测试耗时 | race 耗时 | 顶层 PASS / SKIP | 含子测试 PASS / SKIP |
| --- | --- | --- | --- | --- |
| `internal/legacyruntime` | 7.391 s | 8.677 s | 24 / 0 | 41 / 0 |
| `internal/transport` | 2.446 s | 14.248 s | 53 / 1 | 94 / 1 |
| `TestLegacyRuntimeProductionLoginAndResume` | 4.386 s | 6.405 s | 1 / 0 | 1 / 0 |

完整集成在独立子进程内运行五种场景：新密码登录、旧密码 checkpoint、已认证 checkpoint、新密码登录后 MFA 错码纠正、绑定的 MFA checkpoint 错码纠正。父进程仅在子进程失败时转发输出，因此 JSON 计数为一个测试；这五种场景均实际通过。

全量 SKIP 的分类：

| 分类 | 数量 |
| --- | --- |
| 未显式启用的本地 Chromium / 可见窗口集成 | 53 |
| 未显式启用的上游在线浏览器诊断 | 2 |
| 未显式启用的系统凭据 / Keychain 集成 | 3 |
| 仅由子进程调用的 helper 测试入口 | 6 |
| 未显式启用的长请求预算 / 长时间 monitor 测试 | 4 |
| 完整 Go/Node 登录集成的默认门控 | 1 |
| 未在默认环境提供 Node 路径的依赖链接安全检查 | 1 |

最后两项中，完整登录集成已由本次额外普通和 race 命令实际验证；Node 依赖链接检查仍为本次全量命令的 SKIP。runtime race 唯一的 SKIP 是 `TestTransportConfiguredBudgetCanExceedThirtySeconds`，需显式设置 `TOSUB2_LONG_TRANSPORT_TEST=1` 才运行实际 31 秒预算验证。

原始 JSON 和 stderr 仅用于本次统计，未写入仓库；保留的文档没有账号、凭据、代理地址、远程 body 或异常文本。
