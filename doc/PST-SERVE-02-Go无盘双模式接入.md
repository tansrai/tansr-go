# PST-SERVE-02 Go：无盘 Serve 双模式接入

2026-10-23。本批是源码能力更新，不重写既有 v0.3.0 标签。公开合同精确锁见 `contract/LOCK.json`；历史参考源码按逐文件旧 Git pin 保留，不随新合同公开上游实现。

## 存储责任

| 显式模式 | Serve | Go 应用 |
| --- | --- | --- |
| terminal | 有界工作集和 TTL 内存缓存；预装 source-managed binding；无本地文件兜底 | 自有持久存储。Archive 验证并原子保存后 ACK；执行 journal、记忆目录与永久回执保持耐久 |
| cloud | 由开发者预装可信 SQL／OSS 平台源，持久化控制及对象；Serve 自身仍无盘 | 消费预装绑定和独立云 journal；不强制创建本地档案目录 |

模式来自受信任的 Serve 部署配置。终端只能发现和检查，不能用一份响应或导入包给自己授予云源、管理权或 writer 身份。

## 发现和原键查询

```go
transport, err := api.New(api.Options{
    BaseURL: origin, Token: shortLivedToken,
    SessionFamily: "sdk1", EventEnvelope: true,
})
if err != nil { return err }
client, err := session.New(transport)
if err != nil { return err }
conversation, err := client.Attach(ctx, savedSessionID)
if err != nil { return err }
view, err := conversation.Persistence(ctx)
if err != nil { return err }
archives := archive.NewClient(transport)
binding, status, err := archives.OpenInstalled(ctx, savedSessionID)
if err != nil { return err }
_ = view; _ = binding; _ = status
```

`source-managed` 必须消费服务预装绑定，不重建旧式 binding，不通过填空本地 archive 模拟云模式。`archive.Events` 同样严格验证 source-managed 状态帧；SSE cursor 不是耐久 ACK。

发消息前保存稳定 `requestId`，使用 `session.WriteOptions{RequestID: originalKey}`。请求失回时用 `QueryInput(ctx, originalKey)` 或分页 `ListInputReceipts` 查询。`nil`／`unknown` 不是成功，也不授权换新键重发。此项是正文键，不能用通用 `Idempotency-Key` 代替。执行绑定和应用权限必须先实际建立。

## 云 journal 与原 Runner

`executor.NewCloudJournal(client, sessionID, connectionProvider)` 实现原 `executor.Journal`，可直接传入 `executor.NewRunner`。connectionProvider 必须返回当前已认证连接；Runner 的 `Authorize` 仍检查登录、绑定和本机资源权限。

- 只有首次 `granted + claimToken` 可执行一次。token 仅保存在内存，不通过通用 HTTP 幂等缓存复用。
- 认领失回、`pending` 或无原 token 的冷启动只可原键 `Read`／`List` 对账，不可推测执行权，不替别人提交 `unknown`。
- 完成失回保留原 operation、digest 和 receipt。完整终态查询确认后释放内存认领槽；不同来源／不同 operation 拒绝。
- 重开应用可以查询永久回执，不会恢复已经丢失的首次 token。恢复权不由导入正文或本机缓存决定。

业务工具示例选择：

```sh
go run ./examples/go-tools -mode cloud -session SESSION -application APP -user USER -authorization-revision 1
# terminal 模式显式指定自己的持久目录：
go run ./examples/go-tools -mode terminal -journal PRIVATE_DIRECTORY -application APP -user USER -authorization-revision 1
```

档案示例：

```sh
go run ./examples/go-archive -mode cloud -session SESSION
# terminal 仍需 TANSR_ARCHIVE_KEY（32 字节密钥的 hex）与私有文件：
go run ./examples/go-archive -mode terminal -session SESSION -file PRIVATE_FILE
```

cloud 路径不读取本地档案密钥、不创建目录、不伪造终端 ACK。terminal 示例保留原已验证持久化和失回恢复流程。

## 完整记忆域迁移

`terminalpersistence.FileStore` 复用原 AES-GCM 持久介质与原 transfer、root、双键索引引擎，新增 `ExecuteCustody` 的 prepare/export-page/export-object/import-page/import-object/verify/cutover/query/abort。迁移不是仅复制正文，包含目录、永久双键回执、原始迁移票据、未决 staging 及已预留容量。

开发者必须注入配对的 `Options.Custody` 和 `CustodyAuthority`。该权威是所有写者共享的认证 CAS 控制面：`BeginHandoff` 冻结写者，`RecordPrepared` 登记完整证明，`AcquireWriter` 返回可复核 lease；执行过程中和最终原子替换前重查 lease，已持久但回执不确定时保留原 claim 并查询原键，不自动重试。`CustodyClient` 仅提供公开的只读发现／查询，不能代替共享管理权威。

`NewCustodyHost` 与 `NewHost` 共同接入原 Runner，注册保留工具 `TansrPersistenceCustodyHandoffV1`，其定义摘要使用 `executor.PersistenceCustodyDefinitionDigest`；需要现有 MemoryPublication 权限、独立管理授权及加密 journal。不允许通用业务工具覆盖这些保留名称。

导入的 published JSON 本身不授权。只有共享权威核对原 operation、intent、proof、writer fence、旧 custody revision 后确认 published，目标才激活新 writer。冷开读取持久化的选定 epoch，不能删除 Authority 参数降级为旧式无围栏写者。只读 Copy 不允许通过 custody 旁路改写。

`MigrationBudget` 是完整目录与控制对象的迁移总预算，默认 1 GiB／1,048,576 对象；业务 `Limits`、物理文件上限、永久索引与已受理迁移完成预留仍独立约束。超过预算明确拒绝，保留原源，不截断回执，不扩大原业务容量。

当前 Go 接收器是显式 FileStore，而非内置任意 SQL／OSS 通用接收器；云模式的真正 source/receiver 由开发者在可信服务配置中接线。端侧、跨端或云端均沿用相同合同及授权边界，不能把一次 JSON 导入当作整个托管切换。

## 本批验证范围

共享真实 Serve HTTP 宿主运行在本机，使用产品路由、kernel、执行器与 Archive，模型／SQL／对象后端为受控合成介质。三个场景：terminal Read、cloud Read、cloud 业务 Runner；验证发现、预装绑定、SSE、稳定输入失回查询、执行 journal 和终结回执。它不声称验证真实生产 MySQL/OSS，也不替代服务介质的进程重启耐久性验证。

另由真实 Node 加密 SQLite 生成的 r2 目录，经 Go 真正加密 FileStore import→verify→失回 cutover 查询→重开：13 页、49 块、40 双键永久回执、2 张票据全部保留；原 staging 票据在新 writer 下完成，旧 writer 和权限撤销拒绝。合成 fixture 源 SHA256 为 `2fae89332fdeed88d6f41a068b66bc6e0125a64bf168d09cb40e7b7638dea67c`。

只运行本批受影响包、反例及真实宿主联调；未跑全量、模型或远端 CI。主线收编、推送和发布由本轮负责人统一执行。
