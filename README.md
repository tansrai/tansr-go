# tansr-go

Tansr Serve 的 Go SDK，由 `tansrai` 维护：终端负责接入、呈现、受控业务工具和本地档案，Serve 负责智能体循环、会话、上下文、权限与用量。统一通过 `/api` 调用，标准库实现，无第三方运行依赖。Go SDK 与 Demo 采用 [MIT License](LICENSE)，许可范围见 [NOTICE](NOTICE.md)；仓库迁移和发行记录见 [GO-03](doc/GO-03-tansrai开源迁移与发布.md)。

Go SDK for Tansr Serve. The client stays lightweight: Serve owns the agent loop, context and permission decisions; Go hosts presentation, explicitly installed business tools and local archive storage.

**v0.3.0** 将 Go Modules 路径迁移为 `github.com/tansrai/tansr-go`。升级时同时更新 `go.mod` 和所有 Go 导入；旧路径与新路径是不同模块，不可混用两边的 SDK 类型。原 `v0.2.0` 已提供的 `session`、`executor` / `archive` 和三个 Demo 保持相同功能与 API 协议行为，历史标签不重打。详情见 [CHANGELOG](CHANGELOG.md)。

合同以 [SDK2 / UAPI 冻结记录](doc/GO-01-SDK2与UAPI合同冻结-2026-10-07.md) 和 `contract/LOCK.json` 为准。功能范围见 [GO-01 开发及验收](doc/GO-01-Go-SDK与Demo开发及验收.md)；原 `v0.2.0` 三平台运行及发行证据保留在 [GO-02](doc/GO-02-三平台运行验收与公开发布.md)，新模块的验收与公开消费结果见 [GO-03](doc/GO-03-tansrai开源迁移与发布.md)。

## 能力与边界

模块为 `github.com/tansrai/tansr-go`，要求 Go 1.25 或以上。

| 包 | 当前源码实现 |
| --- | --- |
| `api` | 91 个 manifest 操作的通用调用、发现和能力围栏、HTTP / SSE、统一错误与三请求头 |
| `session` | 创建、附着、恢复、多轮流式消息、图片块、人工审批与提问、取消、插入输入、历史、快照、压缩、语音请求 |
| `executor` | terminal 本地或 cloud 云 journal、平台声明、执行器注册与心跳、绑定、操作校验、显式业务函数、持久执行记录和回执；工具输出分块写入器 |
| `archive` | source-managed 预装绑定、双模式发现、绑定、档案页与附件校验、AES-GCM 本地存储、落盘后 ACK、待决 ACK 恢复及材料交接 API |
| `canonical` / `sse` | 冻结合同的严格 canonical JSON、域摘要和 SSE 帧解析 |

`executor.Runner` 承接显式安装的 `tool.invoke` 业务函数及独立的 MemoryPublication 保留 profile；不会默认开放 shell、文件系统或回落到 Serve 宿主执行。输出写入器需要已协商的终端绑定与限额，不会自行开启终端服务。档案示例是单端有界存储，不等于完整记忆发布、跨设备同步、保留策略、备份恢复或高级缓存管理。通用 API 可调用已冻结操作，不意味着所有操作都有 Go 高层编排或本批 Demo。

三个 Demo 都显式选择 **`sdk1` 会话族，通过统一 `/api` 接入**，然后按部署能力使用 SDK2 执行与档案扩展；这里的族名不是旧路由。`session.Client` 也允许显式选择 `sdk2-offload-v1`，但调用方必须完整接线该族要求的 Source / 档案生命周期，不能仅改字符串就视为完成历史卸载。服务未安装、应用未授权或围栏关闭时返回明确错误，不自动切换族。

当前源码已补齐 Serve 无盘 terminal/cloud 双模式和完整记忆目录交接；这不等于已发布新版本。接入、责任与实证边界见 [PST-SERVE-02 Go 接入说明](doc/PST-SERVE-02-Go无盘双模式接入.md)。

## 快速开始

SDK 直接通过 **Go Modules** 集成到业务项目。已有项目可以执行 `go get github.com/tansrai/tansr-go@v0.3.0`，也可以在 `go.mod` 中声明固定版本：

```go
module example.com/my-agent-app

go 1.25

require github.com/tansrai/tansr-go v0.3.0
```

业务代码按需导入 `github.com/tansrai/tansr-go/api`、`github.com/tansrai/tansr-go/session` 等包，再运行 `go mod tidy`。提交业务项目的 `go.mod` 和生成的 `go.sum`；尚未在代码中使用的依赖会被 `tidy` 移除。SDK 安装不需要检出本仓、下载 Release 附件或使用本地 `replace`。本机 Go 应用通过 HTTP / SSE 连接单独部署的 Serve。

从空项目到流式会话的完整程序见 [Go 开发手册](doc/Go开发手册.md)；显式业务工具、分块输出、加密档案与 ACK 恢复见 [Go 工具与档案接入](doc/Go工具与档案接入.md)。两个手册以 **v0.3.0** 为准，API 符号索引见 [pkg.go.dev](https://pkg.go.dev/github.com/tansrai/tansr-go@v0.3.0)。

三个 Demo 也可以直接安装为命令：

```sh
go get github.com/tansrai/tansr-go@v0.3.0
go install github.com/tansrai/tansr-go/examples/go-chat@v0.3.0
go install github.com/tansrai/tansr-go/examples/go-tools@v0.3.0
go install github.com/tansrai/tansr-go/examples/go-archive@v0.3.0
```

安装的命令位于 `GOBIN`，未设置时位于 `GOPATH/bin`。以下源码示例中的 `go run ./examples/go-chat` 可替换为安装后的 `go-chat`，其他两个命令同理。

准备可访问的统一 API Serve，并由你的登录服务签发**短期终端用户令牌**。不要把平台 `appkey` 或模型密钥分发给终端。示例每次请求读取 `TANSR_TOKEN_FILE`；没有该配置时读取 `TANSR_TOKEN`。令牌文件优先，续期必须保持相同应用和用户；切换账号请销毁客户端实例再建立新实例。

从当前源码根目录运行，PowerShell：

```powershell
# 令牌文件由你的认证服务写入，内容是一枚短期 bearer token。
$env:TANSR_TOKEN_FILE = "$env:LOCALAPPDATA\Tansr\user-token.txt"
go run ./examples/go-chat -base http://127.0.0.1:8787
```

macOS / Linux：

```sh
export TANSR_TOKEN_FILE="$HOME/.config/tansr/user-token.txt"
go run ./examples/go-chat -base http://127.0.0.1:8787
```

进入后输入普通文字开始一轮；上一轮结束后继续输入即可。工具状态和文本增量实时显示。命令：

| 命令 | 行为 |
| --- | --- |
| `/cancel` | 请求 Serve 中断当前轮，等待明确终态；受理不冒充完成 |
| `/allow <ticket>` / `/deny <ticket>` | 人工决定已显示的权限票据，SDK 原样带回 digest；过期与归属由 Serve 复核 |
| `/answers <ticket> <JSON array>` | 回答已显示的问题，例如下面的 JSON |
| `/quit` | 离开；活动轮会尝试中断，会话默认保留 |

```text
/answers q-ticket [{"questionId":"q1","selectedOptionIds":[],"freeText":"使用演示数据"}]
```

`Ctrl+C` 或整体超时会停止读取并尝试中断活动轮。没有自动批准工具的开关。可以指定一次性消息，或者恢复输出中记录的同一个 `sessionId`：

```sh
go run ./examples/go-chat -base http://127.0.0.1:8787 -message "你好"
go run ./examples/go-chat -base http://127.0.0.1:8787 -resume SESSION_ID
```

`-timeout` 默认 10 分钟，`-timeout 0` 关闭整体限时；`-close` 显式在离开时关闭会话。恢复依赖 Serve 已配置的持久化与归属检查，不会在恢复失败时悄悄创建新会话。正在运行的会话应不带 `-message` 恢复观察，避免制造并发轮。SSE 重放缺口会中止示例并要求核对历史；示例不会猜测缺失事件或重新发送原消息。

### 客户端业务工具

`go-tools` 复用已有 Serve Demo 的 `DemoOrderStatus` 声明和 `DEMO-001` 合成订单。它创建会话，注册本机 Go 执行器，完成平台初始化和会话绑定，再领取工具任务。Serve 必须安装执行扩展，并在可信应用策略中允许该执行器和该工具；初始化所报平台和工具名不能扩大应用权限。

该示例还需要包含 **GO-01 首次设备绑定修复**的 Serve 源码或构建。联调发现原围栏曾要求“已有绑定”才能建立首次绑定，修复保持冻结合同不变。现有 npm 发行包不能仅凭版本号推定包含此修复；以[本轮验收和收编记录](doc/GO-01-Go-SDK与Demo开发及验收.md)核对。Go 新源码和 Serve 新源码的推送均不等于 npm 发版。

```powershell
# 三项应来自可信登录结果 / 应用配置，并与令牌主体及当前授权代际一致。
$env:TANSR_APPLICATION_SCOPE_ID = "YOUR_APPLICATION_SCOPE"
$env:TANSR_END_USER_ID = "YOUR_END_USER"
$env:TANSR_AUTHORIZATION_REVISION = "1"
go run ./examples/go-tools -base http://127.0.0.1:8787 -executor go-demo -journal "$env:LOCALAPPDATA\Tansr\go-tools-journal"
```

当输出 `ready` 后，在第二个终端使用相同用户令牌运行它打印的 `go-chat -resume ...` 命令，然后输入“查询订单 DEMO-001”。第一个终端负责业务函数，第二个负责交互与审批。工具不调用模型、不读业务数据库、不执行命令。你可以替换明确的业务函数和对应声明；声明摘要须匹配，`DefinitionDigest` 只接受其文档说明的受支持声明子集。

**控制者与受限执行器的接线不同。** 当前 `go-tools` 在同一进程创建会话、初始化与绑定，并运行执行器；部署认证必须允许这两类操作。`RunnerOptions.Status` 未配置时，Runner 通过 `Client.Status` 调用控制者的 `execution.status`，用于运行中的取消观察和失回对账；仅持受限执行器票不能直接完成本例所有步骤。

分离部署时，由控制者先完成会话和终端绑定协商，受限执行器再配置 `RunnerOptions.Status` 回调：从 `runner.Connection()` 读取当前连接，并调用 `Client.ExecutorStatus(ctx, sessionReference, connection, operation)`。`sessionReference` 使用已协商的 `TerminalSessionReference{SessionContract, SessionID}`；该方法只走受限的 `terminal.execution.state`，不代做绑定、不提升票据权限，也不回落到控制者端点。SDK 仍核对回执的 schema、主体、原操作摘要及连接；宿主的 `Authorize` 仍必须检查当前本地授权。

执行记录目录必须由宿主保护，Windows 应设置当前 OS 用户的目录 ACL；Unix 权限位不等于 Windows ACL。该记录不是加密业务数据库。**普通 handler 错误、崩溃或失联按结果未知处理，不自动重新执行**；只有确认没有发生作用时才返回 `executor.Rejected`。保留目录供同一个执行器恢复，对未知副作用先查询原操作，不删记录、不换键重做。

### 加密本地档案

Serve 需安装 `archive-transfer-v1` 和 `split-receipts-v1`。从安全密钥保管机制设置 `TANSR_ARCHIVE_KEY`（32 字节，64 位十六进制），同一档案持续使用同一密钥。示例不会生成后丢弃密钥，也不把它写入档案或日志。

```powershell
# TANSR_ARCHIVE_KEY 已由你自己的密钥保管机制注入。
go run ./examples/go-archive -base http://127.0.0.1:8787 -session SESSION_ID -source go-demo -file "$env:LOCALAPPDATA\Tansr\archive.bin"
# 已有绑定也可直接重开：
go run ./examples/go-archive -base http://127.0.0.1:8787 -binding BINDING_ID -file "$env:LOCALAPPDATA\Tansr\archive.bin"
```

示例核对绑定和当前身份、下载并校验字节与记录链，加密持久化后才确认覆盖；失去 ACK 响应时下次使用原 ACK 恢复。它最多同步 `-max-pages` 页（默认 64），达到上限可保留同一绑定、文件和密钥再次运行。错误密钥、损坏或身份不符都失败退出，不清空文件重建。默认文件存储限额为 4096 条记录、64 MiB；大型档案应实现应用自己的存储适配。

若原 ACK 被 Serve 明确拒绝为 `412 / if_match_stale`，可主动使用已有绑定和档案执行一次恢复：

```powershell
go run ./examples/go-archive -base http://127.0.0.1:8787 -binding BINDING_ID -file "$env:LOCALAPPDATA\Tansr\archive.bin" -recover-ack RECOVERY_REQUEST_ID
```

`RECOVERY_REQUEST_ID` 使用独立的稳定请求标识；中断后保留同一文件、密钥和标识重试，已持久化的恢复始终使用原身份。恢复先重发原 ACK；仅在服务端明确证明原修订过期后，才准备恢复并原子升级本地介质为 v2，**旧版 SDK 将拒绝读取升级后的档案**。默认同步不会创建恢复请求或自动升级，但会续办已持久化的恢复意图。未知网络错误、忙碌、撤权、过期等不作为更换请求身份的理由。命令只恢复一次并报告确认回执；成功后退出，不代表全部档案同步完成，需另行去掉 `-recover-ack` 继续同步。

Explicit recovery is also available as `-recover-ack RECOVERY_REQUEST_ID` with an existing `-binding` and archive `-file`. It replays the original ACK first; only a proven stale revision permits durable recovery preparation and a format-v2 upgrade, which older SDKs reject. Preserve the same file, key and recovery ID after interruption. The recovery command confirms one pending ACK and exits; run normal synchronization separately for remaining pages. Normal synchronization never creates a recovery intent or silently upgrades the file, but can resume an intent already saved by explicit recovery.

`archive.Store` 是自有数据库或其他耐久介质的适配入口；`SyncOnce` 与 `RespondMaterials` 均接收该接口，`FileStore` 是内置实现。适配器须在 `Receive` 返回前原子保存已验证的记录、全部正文/附件和原始待决 ACK，`Confirm` 须核对并耐久保存原操作的完成回执；本地 Head、待决 ACK 和已确认 Coverage 不能互换。`Identity` / `StorageLimits` 在实例生命周期内固定，`CheckAccess` 接当前宿主授权。内置文件格式不是 Node 的 SQLite 存储格式，不提供直接互读、自动副本切换或自动会话恢复。

Demo 的 `CheckAccess` 在本次进程中固定已认证绑定并响应退出；正式应用必须接当前登录、撤权、删除代际等授权状态，不能凭档案文件内的身份字段决定谁可读取。摘要、模型上下文和材料采用决策仍由 Serve 核心负责；本地写入成功不等于材料已被核心消费。

`RespondMaterials` 的接收回执也不代表 `core-consumed`，应继续通过 `MaterialStatus` 查询。返回的原始响应请求须保留用于失回查账；重试不得更换请求身份。读取期间发生正常发布水位变化可能返回 `ErrSnapshotChanged`，本次尚未落盘，应重新取样，不能据此放宽完整性检查或提前 ACK。

## SDK 接入

`session` 与低层 API 共用同一传输、令牌回调和冻结合同：

```go
transport, err := api.New(api.Options{
    BaseURL: "http://127.0.0.1:8787",
    SessionFamily: "sdk1",
    EventEnvelope: true,
    TokenFunc: readCurrentUserToken, // func(context.Context) (string, error)
})
if err != nil { return err }
client, err := session.New(transport)
if err != nil { return err }
conversation, err := client.Create(ctx, session.CreateOptions{})
if err != nil { return err }
// Subscribe before sending; see go-chat for cancellation, manual approvals,
// replay floors, and success / failure / EOF handling.
```

未提供高层包装的操作可通过 `transport.Call(ctx, api.Op..., api.CallOptions{...})` 调用。路径、方法及查询键只来自 manifest 生成器；严格域的 schema、canonical、digest、ACK 与 execution 约束不能靠普通 Go struct 映射取代。

### 错误与写前置

`*api.APIError` 使用 19 个统一码（`Code`）与 6 个重试动作（`RetryAction`）；原族码在 `Detail.DomainCode`。`errors.Is(err, &api.APIError{Code: api.CodeNotFound})` 只比较统一码。`result_unknown` 应查询原操作，不能换 requestId 重发。

`api.CallOptions` 提供 `IdempotencyKey`、`IfMatch`、`Deadline`；`Meta.ETag` 只接受强 revision。`IfMatch` 仅限 manifest 声明支持的操作，截止时间不会因为重试自动延长。`session.WriteOptions` 暴露幂等键与截止时间；受围栏约束的高层会话内写操作会先刷新能力围栏，不缓存旧授权决定。`Close` 也支持休眠或已结束会话，因此不要求活体围栏，仍由 Serve 核对归属及关闭语义。

## 合同纪律

1. 缺统一合同头或事件协商回响即报错，不回退旧前缀。
2. 不从响应提取请求 URL，不跟随 HTTP 重定向传递凭据。
3. 未知副作用不自动重放；只有明确的同请求重试规则允许保持原键重试。
4. 可信主体来自认证；体内会话、executor、scope 引用不是授权。
5. HTTP 202、SSE EOF、`session.ended` 不等于当前轮成功。
6. 事件游标、档案覆盖、输出水位、材料消费与 ACK 回执保持独立。
7. 不手写路径，不修改冻结合同来适配客户端。

## Verification / English quick start

Starting with v0.3.0, the module path is `github.com/tansrai/tansr-go`. Update both `go.mod` and all imports when migrating from `github.com/cpple/tansr-go`; the two paths define different Go types and must not be mixed. SDK behavior and the frozen API contract are unchanged. Existing v0.2.0 tags and evidence remain intact. See [GO-03](doc/GO-03-tansrai开源迁移与发布.md) for migration and publication results, and [NOTICE](NOTICE.md) for the MIT scope and upstream license boundaries.

Integrate directly through Go Modules: add `require github.com/tansrai/tansr-go v0.3.0` to your application's `go.mod`, import the `api` / `session` packages you use, then run `go mod tidy`. Commit `go.mod` and `go.sum`. No local `replace`, source checkout or release attachment is required. The [developer guide](doc/Go开发手册.md) contains a complete streaming client; the [tools and archive guide](doc/Go工具与档案接入.md) covers durable execution, storage and ACK recovery. Both describe v0.3.0; Serve is deployed separately.

Install the SDK with `go get github.com/tansrai/tansr-go@v0.3.0`, or install a demo with `go install github.com/tansrai/tansr-go/examples/go-chat@v0.3.0`. Go 1.25+ is required. Set `TANSR_TOKEN_FILE` to a short-lived end-user token, then run `go-chat -base http://127.0.0.1:8787` (or `go run ./examples/go-chat` from a source checkout). Omit `-message` for interactive chat; use `-resume SESSION_ID` to continue the same session. Approval is always manual. `go-tools` demonstrates an explicitly bound read-only order lookup and requires both controller and executor operations. An executor-only host must configure `RunnerOptions.Status` using `Client.ExecutorStatus` after a controller establishes the terminal binding. `go-archive` requires a host-managed encryption key and an archive-enabled Serve; custom durable storage implements `archive.Store`. The demos do not promise all Node/Electron capabilities or silently switch session families.

For a persisted archive ACK rejected with a confirmed stale revision, `archive.RecoverPending` and `go-archive -recover-ack NEW_UNIQUE_REQUEST_ID` expose explicit recovery. Keep the same binding, file and encryption key. A saved recovery intent always keeps its original identity on retry. The first actual recovery preparation atomically upgrades the local archive format to v2; older SDKs reject that format. Default synchronization does not start a new recovery or upgrade. Recovery does not silently bypass a revoked binding, expired epoch or unknown network outcome.

```sh
gofmt -l .                                      # must be empty
go vet ./...
go test ./...
go run ./internal/gen/manifest2go -check
go run ./internal/gen/contractcheck               # frozen-source copy check
# Add go test -race ./... when a supported CGO toolchain is available.
```

`integration/` 对本地真实 Serve 的公开 `/api` 路由运行合成会话、工具与档案场景，并可编译运行三个 Demo；这与模拟 HTTP 单测不同，也不代表付费真实模型、生产部署或正式发行。运行结果、操作系统覆盖与剩余外部条件以本批验收记录为准。Go SDK 与 Demo 采用 [MIT License](LICENSE)；冻结上游参考资料保留来源许可，详见 [NOTICE](NOTICE.md)。

## PST-05 unpublished persistence candidate

`memorypublication` adds a dedicated encrypted MemoryPublication store and trusted host through the original executor loop. `executor.NewEncryptedFileJournal` also protects full execution receipts. The original Archive API remains unchanged. See [MemoryPublication 接入与保证 / setup and guarantees](doc/PST-05-MemoryPublication.md) for the `go-memory` source demo, current scope checks, bounded capacity, recovery and key handling. These new symbols are not included in published v0.3.0.

PST-05 第四批增加 `executor.RekeyEncryptedFileJournal` 和 `go-memory -mode rekey-publication|rekey-journal` 离线调用：停写后显式保留原件迁移，保留原 claim/永久 receipt/pending/unknown，同版本写入与 journal 迁移由 OS 锁互斥。两个介质没有事务，验证双成功后才切换，不能并行双写或覆盖旧目录；详见上面的中英接入说明。

The fourth PST-05 batch adds explicit source-preserving encrypted journal key rotation and two offline demo modes. Original operation keys and all durable outcomes remain intact. There is no transaction across publication and journal: stop writers, verify both destinations, then switch once. These symbols remain unpublished.
