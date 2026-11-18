# Linux / Docker 2FA 凭据持久化修复（2026-11-17）

## 原因与影响

原版 `credentials.NewSecureFileStore` 只支持 macOS Keychain 和 Windows DPAPI。Docker 中运行 Linux，构造器返回 `ErrUnavailable`；`makeJobRunner` 随后使用 `MemoryStore`，并把 `durableCredentials` 设为 `false`。`validateTOTPResetStart` 在网络访问前返回：

```text
TOTP_RESET_PERSISTENCE_REQUIRED: 重置 2FA 需要可用的系统安全凭据存储
```

这一条错误表示持久化前置条件不满足，在该检查处尚未禁用旧因子或激活新因子。挂载 `/data` 只能保存应用实际写出的文件，无法让内存中的凭据自动持久化；容器也无法直接使用 macOS 宿主的 Keychain。

## 实现

- `internal/credentials/file_key*.go` 为 Linux 提供外部主密钥文件后端。`TOSUB2_CREDENTIAL_KEY_FILE` 指向独立的 32 字节随机密钥文件；校验私有目录、当前 UID、0600/0400 文件权限、普通文件类型和链接边界。
- 凭据及私有操作证据使用 AES-256-GCM。HMAC-SHA256 按用途和规范化账号派生独立密钥，并校验身份及操作关联数据；复制其他账号的密文不能冒充当前账号。
- 数据根的 `.file-key.json` 保存主密钥指纹。每次访问核对密钥和指纹；换密钥、丢密钥、丢配置或损坏已有密文时拒绝继续，保留原资料。已有密文确认可解密后才允许覆盖。
- 初始化器 `cmd/credential-key` 以进程锁、独占发布和文件/目录同步首次生成密钥。重复执行保留已有密钥；已有凭据而主密钥丢失时拒绝生成替代品。删除账号不会删除恢复证据使用的主密钥。
- `makeJobRunner` 区分未配置平台与错误配置。只有未配置且没有既有加密资料的 Linux 实例可以继续原有内存模式；明确配置失败时控制台在开始服务前返回错误，CLI 在认证前失败。
- Docker Compose 新增 `credential-key-init`，初始化成功后启动 `console`。主密钥位于独立 `console-key` 卷，控制台只读挂载；加密凭据及业务资料仍写入 `console-data` 卷。两个服务均以 UID 10001 运行。

这是由文件权限保护、独立提供主密钥的加密存储，不是 Linux 原生 Keychain/DPAPI。备份需要同时保存数据卷及匹配的密钥卷，并分别保护访问权限；不能用新生成的密钥恢复旧数据。

## 回归中发现的两处相关问题

1. `operationCredentialProtector` 精确比较保存前后对象，但空的 `MailHeaders` 经 JSON `omitempty` 后变成 `nil`。真实文件存储因此误报读回不一致，而内存模拟没有暴露这一问题。现在保存前将空 map 规范化为 `nil`，保留原有完整读回校验。
2. `setupTOTPAttempt` 原来使用普通私有 JSON 保存含新 TOTP 密钥的结果。现在三个写入点复用加密 TOTP journal；已启用 2FA 的结果也带操作 ID，以满足绑定校验。现有读取和恢复路径继续兼容历史记录。

未取消重置前的持久化要求、新密钥激活前保存、保存后读回、恢复隔离或禁止未知远端结果重放的检查。

## 验证

- `go test` 对 `cmd/tosub2`、`internal/protocol`、`internal/jobs`、`internal/console` 和 `internal/privatestate` 的完整测试通过；相关命令、协议和凭据包的 `go vet` 通过。
- credentials 全套测试、race、vet 和 Windows amd64 交叉编译通过；并发初始化专项重复 10 次通过。
- `scripts/test-container.sh` 在实际生产镜像、UID 10001、只读根文件系统和禁用外网的容器内通过全部指定夹具，无指定测试跳过。覆盖重置/设置 2FA、已启用因子、新进程读取、已确认激活后的本地补保存，以及缺失/替换密钥、篡改、账号串用、取消和启动失败。
- 独立 Compose 项目验证首次初始化、健康启动、强制重建容器、原凭据读回和主密钥只读挂载；测试容器与合成数据卷已删除。

验证使用合成账号与本地模拟服务。线上账号需由用户重新发起原操作，不会自动重试此前失败的重置。

## 本机部署

本机 `tosub2-go` Compose 项目已于 2026-11-17 11:57（Asia/Shanghai）完成更新，`tosub2-go-console-1` 后台运行且健康。升级先阻断旧容器外部连接，确认账号队列与自动任务空闲，再通过内存及匿名管道迁移原账号凭据，校验加密存储读回后替换进程。3 个账号的密码、TOTP 和邮件/代理资料在进程替换后逐项一致；迁移过程没有将凭据明文写入临时文件或日志。

验收时活动/排队任务均为 0；首页、JS/CSS 和 `/api/health` 均正常。`credential-key-init` 以退出码 0 完成，密钥目录为 0700，密钥文件为 0600，所有者均为 10001:10001；运行中的主服务对密钥卷只读。访问地址保持 `http://127.0.0.1:4399`。

旧 Linux 实例仅存于内存的 Sub2API 管理密钥没有导出接口，更新后需重新填写。此后通过服务配置保存的密钥可使用新的加密后端跨进程保留。
