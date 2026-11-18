# 重置 2FA：用一份日志判断下一步

**先找“本轮结果”，不要先看最后一个 HTTP 200。** 如果写着“网站已确认激活，新密钥已保存；未另查验证器列表”，重置已完成，不用再点一次。它证明的是网站接受了本次激活、任务保存了对应密钥，不证明所有旧密钥已失效。不要向用户索取 HAR、邮箱、旧密钥、验证码、Cookie、恢复码或凭据文件。

**要取新密钥：** 完成保存后打开控制台「账号信息 → 2FA 密钥」，不用导入或填写 `factor_id`。网站的状态查询返回验证器信息，不返回旧密钥。待核对材料不会通过「账号信息」作为有效密钥导出。

**取得同一轮证据。** 在操作日志选择“本次 → 全部 → 复制当前显示的日志”。先读首行的条数、筛选和分页范围。有归档警告或“尚有更早页”，可加载更早日志、切到“全部历史”再复制。按 `op_ref` 找 `terminal:true`；按 `seq` 排序，`attempt_ref` 区分同阶段不同请求。跨重启补本地时 `op_ref` 不变，但会有新的操作时间和终态。请求详情带北京时间 `+08:00`，`duration_ms` 是单调耗时；原始 `at` 的 `Z` 是 UTC。

| 看见什么 | 能确定什么 | 安全下一步 |
|---|---|---|
| `NO_ORDINARY_TOTP` / `OPTIONAL_STATE_MISSING` | 前者是明确未启用；后者只是原会话只读复核后仍无可移除目标。两者都不授权删除。 | 看随后的一次登记，不重新登录。 |
| `INITIAL_CONFLICT` → `TARGET_BOUND`；或 `TARGET_NOT_AUTHORIZED` / `TARGET_UNRESOLVED` | 前者有登记拒绝回执，并有与本次授权相符的唯一普通旧目标；后两者没有。 | 无法绑定就停，核对本账号的验证器；不要按当前显示的另一个 ID 追删。 |
| `CONFLICT_STATE_DISABLED` / `REMOVAL_BUDGET_USED` / `FORMAL_CONFLICT` | 分别是：冲突后复查明确未启用，只再登记一次；本轮已移除过但仍有因子，不再删；第二次登记也被拒，不发第三次。 | 只有第一种自动继续；其他情况看 `old_removal`，再去账号安全设置核对。 |
| `old_removal=accepted` 或 `state_read`，新因子未核对 | 分别代表有移除回执、仅由状态读取证实停用。**旧因子已有移除证据，新因子尚未确认。** 本地隔离标志不是移除证明。 | 先告知这一事实；正式登记再冲突、部分材料、旧 ID 的通用 400 均不补发。 |
| `activation_receipt=accepted`、`factor_match=skipped`、`local_commit=committed` | 网站已确认激活且新密钥已保存；本流程未另查验证器列表，没有可见因子数量。 | 取用新密钥。无需自行验证旧密钥、重新激活或再次重置。 |
| `CONTRADICTORY_RECEIPT` | 收到了完整回包，但同一份回包既表示激活又表示停用。它不是超时，也不证明成功。 | 看同轮只读结果。若只见 F3 而预期 F2，且 `pending_readback=verified`，密钥已安全暂存，但未作为可用密钥提交；不再激活、不退出。 |
| `expected_factor=F2`、`visible_factor=F3` | 新流程中是激活回执不明确，查询也没匹配 F2；旧历史可能确有激活回执，以该轮 `activation_receipt` 为准。两者都不证明新密钥无效。 | 如果 `pending_readback=verified`，待核对材料已安全保存。核对绑定，不再次激活、移除或覆盖另一轮凭据。 |
| `RESET_SAVED_OTHER_VISIBLE` | 新密钥已核对并保存，但当时还看到别的验证器。`old_also_visible=true` 特指原定移除的 F1；不带它不能把另一项指认为 F1。 | 用户可取新密钥。不重置、不追删。担心盗用时，在网站的安全设置处理陌生验证方式和登录设备。该提醒不会阻止已选的退出，也不要求再次点击确认。 |
| 最终 `visible_count=1` | 核对时只显示本次验证器，不是“只有这份密钥能登录”。无该字段的旧记录也不能推断数量。 | 若需证明旧验证器已撤销，不能以“列表里看不到”代替服务端证据。 |
| `RECEIPT_LOCAL_COMMIT_REQUIRED` / `LOCAL_COMMIT_REQUIRED` / `UNCERTAIN_COMMIT` | 前两者分别凭完整激活回执或列表匹配，只差本地提交。第三种尚不能证明恢复记录可用。 | 前两者用“完成本地保存”，不联网；第三种先检查安全存储，不重置。 |
| “完成本地保存”仍未成功 | 看本轮终态的 `cause_reason`：`STORAGE_WRITE_FAILED` 是有效凭据写入失败；`STORAGE_READBACK_FAILED` 是写入后或最终提交时未能读到匹配的凭据，**不能据此判定写入一定失败**；`STORAGE_COMMIT_FAILED` 是任务元数据提交失败。此前的激活回执仍有效。 | 先恢复安全存储，再选“完成本地保存”。已匹配的有效凭据不会重写；缺旧版因子 ID 不需要用户手填。不要再登记、激活或退出。 |
| `logout=accepted` / `unknown` | 分别表示服务端接受了退出请求、退出结果未确认；都不能证明每台设备已经下线。即使 session 200，受保护业务也可能已撤销。 | 若本轮新密钥已核对并保存，仍在「账号信息」取用它，不重做 2FA、不重发退出。 |
| `logout=not_performed` / `proven_unsent` | 前者是未记录退出回执，不足以判断请求是否发出；只有后者是确定未发送。旧记录若只有 `local_commit=committed`，只能确认本地提交，不能补写“新密钥已核对”。 | 看同一操作的 `factor_match` 和退出回执，别从缺字段推断结果。 |

**看材料，不猜材料。** `journal_enrollment_material=partial` 表示受保护记录读回了部分登记字段，不足以激活；`complete` 只是该记录里的登记材料齐全。另看 `pending_readback`（凭据库待确认材料）、`local_commit`（任务最终提交）和 `last_trusted_step`。`STORAGE_WRITE_FAILED`、`STORAGE_READBACK_FAILED`、`STORAGE_COMMIT_FAILED` 是三个不同故障点。没有读回证据时不要写“密钥已保留”。登记的 `session_id` 不是网页登录会话。

**看次数，不数日志行。** `writes_this_run` 分移除、初登、正式登记、激活、退出。`received` 表示收到了 HTTP 响应（含拒绝），`possible` 表示可能已发、未收到响应，`unsent` 表示确定未发。写前意图和回包可共享一个 A 号；不要把二者加作两次。`last_write_issue` / `write_issue_attempt` 保留先前 POST 的问题；后来的 GET 200 不补造回执。中断恢复的 `logical_writes_from_journal` 只按受保护记录数逻辑步骤，不是完整的传输尝试计数。旧日志缺字段就是没有该证据；展示日志不能解锁恢复。

[本轮本地提交：修前修后对照、复制样本和复测命令](totp-reset-local-commit-2026-11-18.md) · [激活回执与冲突兼容验收](totp-reset-receipt-path-2026-11-18.md) · [历史可见性实验](totp-reset-key-visibility-2026-11-18.md)
