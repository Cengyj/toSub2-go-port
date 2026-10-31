# BPS 登录完成与可用性门禁核查（2026-10-31）

本次核查基于当前工作区（包含未提交改动）。没有修改生产成功判定、查询或上传规则，没有重启4401，没有读取真实账号凭据或调用真实登录、刷新、权限查询及上传。

后续配置变更：用户随后要求 BPS 上传启用全部模型兼容模式，已将原模型选择名单改为省略 `openai_excel_bps_models`；见 [BPS 配置说明](bps-authorization.md)。该路由设置变更不改变本报告的权限门禁结论，下文关于默认模型名单的描述记录的是核查时状态。

## 结论

当前“BPS授权成功”证明OAuth流程完成、满足本地导出条件且完成必要保存；它不证明BPS业务权限、模型可用性或额度可用。独立账号查询能读取BPS allowed、模型限制及HTTP401/403，但其结果不参与现有上传资格。只要授权编排成功、通过各入口已有门禁且Sub2管理API接受写入，就仍可继续上传并报告同步成功。

## 代码边界

| 层次 | 已有判断 | 未覆盖的可用性判断 |
| --- | --- | --- |
| OAuth回调、交换 | state/code/error、HTTP状态、JSON、非空access token | 2xx同时携带error和完整token时没有单独拒绝error |
| BPS导出 | access/refresh、至少可解析的ID或access声明、账号ID、认证邮箱与请求邮箱一致 | scope/entitlement、当前时间下access是否过期、token声明中的client是否匹配BPS |
| 保存与恢复 | 一次性grant先保存；原子写入失败不报告成功；恢复校验client/redirect/state/email/origin且禁止重放code | 已保存grant可以离线完成导出，没有在线权限确认 |
| 账号查询 | 独立读取BPS allowed、模型限制、来源错误 | 快照不反馈至上传/自动恢复资格；查询成功不等于业务允许 |
| Sub2上传 | 配置、文件安全/结构、邮箱、OAuth client字段一致性、字段类型、expiry非负、远端更新结果 | 不读取BPS allowed/查询错误，不验证模型调用；expiry未与当前时间比较 |
| 远端错误恢复 | 确认更新授权后，对原error账号清错并恢复调度 | 这些管理操作成功不证明BPS恢复可用 |

主要代码入口：

- internal/protocol/oauth.go：ExchangeOAuthCode、oauthTokenRequest、BuildSub2APIOAuthExport、validateExportTokens。
- internal/protocol/flow.go：交换→保存tokens_received→构建导出→私有写入→清checkpoint→完成；任何返回错误阻止本次授权完成。
- internal/protocol/token_checkpoint.go：FinalizeSavedOAuthGrant及savedGrantTransport禁止恢复过程中网络调用。
- internal/protocol/account_insights_client.go、bps_access.go：独立权限查询及解析。
- internal/application/relogin_upload.go、upload.go、totp_workflow.go：各应用编排均没有BPS业务权限gate。
- internal/console/monitor.go：syncRepairOutput复核套餐排除，不验证BPS权限。
- internal/sub2api/upsert.go、upload_recovery.go：写入后清错/恢复调度。
- internal/protocol/bps_settings.go：默认模型列表明确是配置模板，不是发现的entitlement。

## 可重现的合成诊断

使用现有bpsFixture及Flow.Run，传输只接受auth.example.test，其他host请求立即使测试失败；只修改合成/oauth/token响应。下列八例的当前行为已通过普通与race测试确认：

| 响应变化 | 当前Flow.Run结果 |
| --- | --- |
| 原fixture：有access/refresh与账号邮箱，没有entitlement探测 | 成功并导出 |
| HTTP403且body仍含token与error=access_denied | 失败 |
| 删除access_token | 失败 |
| 删除refresh_token | 失败，已有grant仍按恢复逻辑保存 |
| access声明仅保留合成邮箱，缺account ID | 失败 |
| HTTP200、保留原token，追加error=access_denied | 成功并导出 |
| access声明含exp=1、合成账号ID和邮箱 | 成功并导出 |
| access声明client_id为Codex，合成账号ID和邮箱仍匹配 | 成功并导出 |

后三项是合成异常响应下的代码行为，不代表已观察到真实上游返回这些响应；access过期也不能单独证明refresh无效。临时诊断测试已删除，避免把缺失的防护固化成预期业务契约。

现有定向回归（GOTOOLCHAIN=go1.27.3）通过：BPS登录/导出/缺refresh/恢复、OAuth明确拒绝不重试、BPS权限解析、授权client一致性、BPS配置保留、重登后上传、2FA顺序、自动修复套餐门禁、Sub2错误状态恢复。显式执行TOSUB2_LEGACY_RUNTIME_TEST=1的TestProductionBPSGrantFinalizesOfflineWithSelectedClient并通过race；临时八例同样通过race。完整仓库test/race/vet没有在此次只读审查重新运行，不沿用上一轮结果声称本轮全量验证。

## 后续改进应遵守的边界（尚未实施）

1. 将“授权取得并保存”“业务可用性”“同步结果”分开记录，不能用单一成功状态覆盖。
2. 一次性OAuth结果先安全保存为候选授权，再执行有界、只读的BPS可用性确认；同一账号、client、代理、TLS身份和时区必须保持，不能拿旧授权查询缓存证明新授权可用。
3. 明确allowed=false或新授权被对应权限端点拒绝时阻止自动上传和清错/恢复调度；网络超时、挑战、限流或格式异常记录为待核实，提供仅重试验证入口。不能重跑已消费的OAuth code或抹掉历史仍有效授权。
4. 用量查询403、额度耗尽、模型级限制与BPS总体权限必须分别解释；allowed=true也不保证任意模型实际生成成功，默认配置模型清单不是权限证据。
5. 在统一应用层接入所有相关入口并补跨模块回归，包括手动上传、重登上传、2FA后上传及巡检回传；权限安全门禁不能依赖用户是否开启定时用量查询。
