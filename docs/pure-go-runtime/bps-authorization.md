# BPS 登录授权抓包依据

分析日期：2026-10-13。

来源为用户提供的 `cdn.openai.com_2026_10_13_12_42_53.har`，共 278 条请求。本说明只保留协议结构和公开应用常量，不包含邮箱、密码、验证码、Cookie 值、授权码、PKCE 私密值或工作区 ID。分析只解析本地文件，未执行抓包中的脚本、请求或授权。

## 实际网络记录

以下 entry 编号从 0 开始。登录过程在 `auth.openai.com` 完成，回调地址属于 `bps.openai.com`。

| Entry | 请求 | 请求字段 | 观察到的结果 |
| --- | --- | --- | --- |
| 1 | `GET /api/accounts/authorize` | 下节 OAuth 参数 | HTTP 302，转到 `/log-in` |
| 2 | `GET /log-in` | 无请求体 | HTTP 200，登录页面 |
| 202 | `POST /api/accounts/authorize/continue` | `username: {kind: "email", value: <email>}` | `page.type=login_password`，`continue_url` 指向 `/log-in/password` |
| 234 | `POST /api/accounts/password/verify` | `password` | `page.type=mfa_challenge`，返回可用 MFA 因子 |
| 237 | `POST /api/accounts/mfa/issue_challenge` | `id`、`type: "totp"`、`force_fresh_challenge: false` | HTTP 200，更新认证会话中的因子挑战信息 |
| 246 | `POST /api/accounts/mfa/verify` | `id`、`type: "totp"`、`code` | `page.type=workspace`，返回可选工作区 |
| 249 | `POST /api/accounts/workspace/select` | `workspace_id` | `page.type=external_url`，返回 BPS callback URL |
| 252 | `GET <BPS callback URL>` | query：`code`、`scope`、`state` | HTTP 200，静态回调页面；本次抓包到此没有出现 token 交换 |

这些 POST 的请求体均为 JSON。流程响应使用 `continue_url`、`method`、`page.type`、`page.payload` 和 `oai-client-auth-session`。会话中报告的应用为 `basispoints` / `Basispoints`。

Entry 202 的 `openai-sentinel-token` 使用 flow `authorize_continue`；Entry 234 使用 `password_verify`。本次记录中的 MFA issue、MFA verify、workspace select 请求没有该 header。Sentinel token 必须按当前会话动态生成，不能使用抓包值。认证每个阶段都会更新统一会话 Cookie，应保持 Cookie jar、设备和传输会话一致。

本次返回了 `organization` 和 `personal` 两种工作区，实际选择 `organization`。工作区 ID 属于具体账号，不是 BPS 的公开应用参数；实现必须从当前响应选择，不能写死抓包中的 ID。

本次链路没有请求 ChatGPT 的 `/api/auth/providers`、`/api/auth/csrf`、`/api/auth/signin/openai`，也没有先完成 ChatGPT 网页登录再发起 Codex 授权。现有 ChatGPT 授权方式应保留为独立选择。

## 公开 OAuth 应用参数

```text
authorization endpoint = https://auth.openai.com/api/accounts/authorize
client_id = app_fnr0pYvVwwFDocDumLG3H2Bp
audience = https://api.openai.com/v1
scope = openid offline_access email profile organization.read
response_type = code
code_challenge_method = S256
platform = PC
redirect_uri = https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback
state = bps.<本次新生成的随机 state>.PC
```

回调路径中的 UUID 来自部署公开路径和 HTML `bps-auth-base-path`，不是用户或工作区 ID；它可能随 BPS 部署变更。`code_challenge`、`state` 必须每次重新生成，`redirect_uri` 必须在发起授权与兑换授权码时保持一致。

## 由抓包内公开 JavaScript 补足的信息

Entry 270 保存了 `x-square-B1_sKaT6.js`。以下是对该文本的静态分析，区别于前节实际发生的请求。

- 常量 `VMe` 指向 `/api/accounts/authorize`；生产常量 `BMe` 指向 `https://auth.openai.com/oauth/token?unified=true`。
- 函数 `kK` 以 `application/x-www-form-urlencoded` POST 兑换授权码，表单键为 `grant_type=authorization_code`、`code`、`client_id`、`redirect_uri`、`code_verifier`。
- 函数 `WJt` 使用相同的 `?unified=true` token endpoint 刷新，表单键为 `grant_type=refresh_token`、`refresh_token`、`client_id`。刷新表单没有 `redirect_uri`。
- 函数 `Ber` 使用 64 随机字节生成 base64url verifier；`Fer` 计算 SHA-256 challenge；`Her` 使用 32 随机字节生成 base64url state；`zer` 加上 `bps.` 与 `.PC`。符合 OAuth PKCE 要求的其他安全长度亦可使用，但必须保存同一次请求的原始 verifier/state。
- callback 组件检查 `state`、`code` 是否存在，再通过 Office `messageParent` 或 Sheets `window.opener.postMessage` 交回发起方。发起方精确比较 state 后兑换授权码。纯 Go 流程可以截获目标 callback URL 并自行兑换，不需要执行 callback 页面或引入 Office/浏览器依赖。

### Token 响应与 claim 的证据边界

HAR 的实际请求中没有 `/oauth/token`，也没有 token JSON 响应。因此，不能依据本次抓包确认某个真实账号的 token 响应是否同时包含 access、refresh、ID 三种 token，不能声称已经用这些 token 完成 Sub2API 上游调用。

公开 bundle 的 `uQt` 只强制要求非空 `access_token`；`refresh_token` 允许缺省，`expires_in` 接受数字或数字字符串，过期时间也可读取 access token 的 `exp`。`id_token` 仅作为用户元数据的补充来源。项目若要求可长期刷新的导出，应明确检查 refresh token；不能因 ID token 缺省而伪造 ID token，也不能丢弃已经收到的一次性授权结果后自动重放授权码。

bundle 的 `bO` 同时接受以下 auth claim 结构：

```json
{
  "https://api.openai.com/auth": {
    "chatgpt_account_id": "<account-id>",
    "chatgpt_account_user_id": "<account-user-id>",
    "chatgpt_user_id": "<user-id>",
    "chatgpt_plan_type": "<plan>",
    "chatgpt_compute_residency": "<residency>",
    "sso_connection_id": "<connection-id>"
  },
  "https://api.openai.com/profile": {
    "email": "<email>",
    "name": "<name>"
  },
  "sub": "<subject>"
}
```

其中 `https://api.openai.com/auth` 也可能使用短键 `auth`。以上仅说明代码可识别的键，不是本次抓包中某个真实 token 的内容。解析 JWT 元数据也不等于本地验证了 JWT 签名。

## 项目接入约束

1. 用户明确选择 `ChatGPT` 或 `BPS`，默认保留 ChatGPT；BPS 直接进入 BPS OAuth 登录链。
2. 授权方式、客户端与回调身份必须贯穿任务、checkpoint、断点恢复、授权结果和刷新，不能跨方式复用未完成的 grant。
3. 必须精确验证回调目标和 state。包括 HTTP 302 在内，达到配置的 BPS callback 后应停止跟随，无须把授权码再提交给 callback 站点。
4. 授权码兑换和 refresh 都可能消费一次性凭据。只允许可证明尚未发送的网络重试；收到成功响应后优先持久保存 token，再生成最终导出。
5. BPS 刷新使用 BPS 的 `client_id` 与 unified token endpoint；上传更新已有 Sub2API 账号时，授权关联的客户端字段随新凭据更新，其他远程配置保持原值。
6. BPS 登录不提供 ChatGPT 网页会话的等价保证；重置 2FA 等依赖 ChatGPT 网页会话的账号安全操作继续使用其独立流程。

本说明记录协议依据，不替代实时端到端授权验证。协议模拟测试应使用完全合成的账号、验证码和 token，不能把原始 HAR 作为测试夹具提交。

## Sub2 正常 BPS 导出的字段核对

后续用户提供的 Sub2 正常登录导出确认使用同一 BPS client ID。该导出与 HAR 是两份独立资料：导出提供了 token 和 Sub2 字段的对应关系，不代表项目已经在线完成一次 BPS 登录。这里只记录脱敏结构，不复制样本凭据。

| 字段 | 生成或更新规则 |
| --- | --- |
| `extra.openai_excel_bps` | BPS 为 `true`；切换到 ChatGPT 时为 `false`，移除所有 `openai_excel_bps_` 专属设置 |
| `credentials.client_id` | 保留实际 OAuth 客户端，刷新与上传均沿用 |
| `credentials.chatgpt_user_id` | 优先读取专用用户 claim，其次通用用户 claim |
| `credentials.expires_at` | access token 的 `exp`，Unix 秒；不使用 ID token 的到期时间，不在恢复时重新计算 TTL |
| `credentials.plan_type` | 保留上游原始套餐值；页面展示另行规范化 |
| `credentials.organization_id` | 明确组织 ID，或组织列表中的唯一默认/唯一组织；有歧义则省略 |
| `credentials.subscription_expires_at` | 只采用确有来源的订阅到期字段；不能拿 token 到期时间替代 |

旧授权文件上传时，从自身 token 补齐缺失的元数据和 BPS 标记。刷新优先读取新 token；已知工作区变更时，不从旧 ID token 继承组织、套餐或订阅期限。相同工作区保留 Sub2 已补充的订阅到期时间；切换工作区或套餐时清除不再对应的旧期限。

### 用户指定的 BPS 配置模板

用户最终确认采用 2026-10-13 08:41:06 UTC 导出中的 BPS 配置，替代此前 08:28:01 UTC 的样本。下列值是用户指定的配置默认值，并非从某个账号 token 推导的权限或上游动态目录。

| 字段 | 默认值 |
| --- | --- |
| `credentials.model_mapping_mode` | `whitelist` |
| `extra.openai_excel_bps_config_mode` | `initial` |
| `extra.openai_excel_bps_ignore_encrypted_content` | `true` |
| `extra.openai_excel_bps_omit_unsupported_tools` | `true` |
| `extra.openai_excel_bps_auto_disable_on_403` | `true` |
| `extra.openai_excel_bps_auto_move_on_403` | `true` |
| `extra.openai_excel_bps_403_target_group_id` | `0` |
| `extra.openai_excel_bps_403_recovery_interval_minutes` | `60` |
| `extra.openai_excel_bps_models` | `gpt-6-astra`、`gpt-5.6-sol`、`gpt-5.6-terra` |

导出、刷新、旧文件上传和监控重登后的同步共用此模板。上一版 `defaults` 模式在处理时迁移为本次确认的 `initial`；其他默认值只补缺失或 null 字段，已有远端显式设置（包括 false、0、空模型列表、自定义模式或组 ID）保持不变。上传更新继续保留原有 Sub2 账号的配置，模板不会覆盖远端的自定义模型列表或模型映射策略。切回 ChatGPT 时清理 BPS 专属设置，通用模型映射配置保留。部署不会主动改写真实账号，已有记录会在下次刷新或同步时按此规则处理。

HAR 中的 BPS bundle 通过 `/responses/access?include_models=true` 获取动态目录，本模板不代表已验证所有账号都有这三种模型的调用权限。指纹模式、时区、并发和号池继续由现有控制台设置控制；不复制样本的指纹种子、成本倍率、订阅期限或隐私状态，也不将 `privacy_mode=training_off` 当作已经完成上游隐私设置的证据。没有对真实账号进行调用或回放附件请求。
