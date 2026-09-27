# 本机部署说明

部署日期：2026-09-21。

- 控制台：http://127.0.0.1:4399/
- 运行方式：当前 Windows 用户的 PM2 后台进程 `tosub2`，异常退出后自动重启；当前用户登录时由计划任务 `toSub2 PM2 Resurrect` 自动恢复，另有 `toSub2 PM2 Watchdog` 每 5 分钟检查并恢复 PM2 应用列表。
- Node.js：22.23.2。
- Python：项目 `.venv` 中的 3.13.9，已安装 `curl_cffi==0.15.0`。
- 数据目录：`D:\Downloads\toSub2-1.7.1\tmp\chatgpt-onboarding-console`。
- 日志目录：`C:\Users\Ceng\.pm2\logs`。

PM2 配置会优先使用 `TOSUB2_PYTHON` 环境变量，否则自动使用项目的 `.venv`。

## 管理命令

在 PowerShell 中进入项目目录后执行：

```powershell
Set-Location -LiteralPath 'D:\Downloads\toSub2-1.7.1'
npm.cmd run daemon:start     # 启动
npm.cmd run daemon:restart   # 重启
npm.cmd run daemon:stop      # 停止
npm.cmd run daemon:logs      # 查看日志，Ctrl+C 退出日志查看
pm2.cmd status              # 查看运行状态
```

PM2 进程列表已保存。已配置当前用户登录触发的 Windows 计划任务 `toSub2 PM2 Resurrect`，以及每 5 分钟运行的 `toSub2 PM2 Watchdog`；两者执行 `pm2 resurrect` 恢复已保存的应用列表。电脑重启后登录当前用户即可自动恢复；若任务被禁用或删除，可重新执行 `npm.cmd run daemon:start`，再按需重新注册这些计划任务。

## 验证结果

首页、前端 JSX 转换资源、初始化接口、任务列表接口、监控状态接口均正常，浏览器已成功渲染控制台。

源文件语法检查通过；TLS、凭据存储、短信平台、任务锁、控制台、短信控制台、协议流程、密码与 2FA 流程共 8 项测试通过。

原有 `console-restore-shutdown-smoke` 测试在 Windows 上失败：测试用 `SIGTERM` 终止子进程后期待退出码 0，但 Windows 返回 null。PM2 在 Windows 上也会强制结束进程，因此停止或重启服务前，应先在页面停止正在进行的任务。

若需重新执行项目检查：

```powershell
$env:TOSUB2_PYTHON = (Resolve-Path -LiteralPath '.\.venv\Scripts\python.exe').Path
npm.cmd run check
```

此部署已验证本机运行；真实账号登录和第三方服务配置需要使用者自行提供账号及配置后使用。

## 2026-09-23 本机修复记录

- 修复直连 TLS 兜底配置只写入日志、未传入下一次登录进程的问题；保留每次手动操作最多一次直连兜底。
- 修复 Windows 子进程管道使用 `selectors` 导致的 `WinError 10038`。Cloudflare 与 Sentinel 的本地运行时改用线程和队列处理输入输出，增加超时、错误输出和资源清理。
- 修复等待 TLS 筛选或邮箱基线时，旧异步结果覆盖取消、重新登录等新状态的问题。
- 失败提示保留具体原因，并区分直连和代理；新增进程与任务重试回归测试，接入 `npm run check`。

本机测试中，直连 `https://chatgpt.com/` 出现 TLS 连接重置；通过正在运行的 Clash 本地 HTTP 代理 `http://127.0.0.1:7897` 请求首页返回 200。需要使用此代理时，在控制台的“代理 IP”中填写该地址；端口依赖本机 Clash 配置，关闭 Clash 后无法使用。该检查仅验证公开首页连接，不代表真实账号授权已完成。

新增运行时测试含 11 项本地子进程用例。测试脚本共 10 项通过；原有 `console-restore-shutdown-smoke` 仍因 Windows `SIGTERM` 退出码为 null 而失败。完整检查会在该项中断，其后三项已分别执行并通过。
