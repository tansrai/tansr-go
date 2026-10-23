# Changelog · tansr-go

`v0.1.0` 为原 UAPI 骨架；`v0.2.0` 汇总后续 UAPI r7、高层 SDK 和三平台运行验收；`v0.3.0` 迁移到 `tansrai` 的模块路径。合同以 `contract/LOCK.json` 和 `contract/PROVENANCE.json` 为准。旧版本发行证据见 GO-02，迁移版本的标签及公开代理结果见 [GO-03](doc/GO-03-tansrai开源迁移与发布.md)。

## Unreleased — PST-SERVE-02

- Add explicit terminal/cloud diskless Serve discovery, preinstalled archive bindings and source-managed SSE.
- Preserve message request IDs and query original input receipts after a lost reply.
- Reuse executor Runner with a cloud journal; first-claim tokens, unknown outcomes and cold reconciliation remain distinct.
- Migrate complete memory catalogs and permanent receipts with shared writer fencing and explicit budgets.
- Extend archive/tools demos with explicit cloud mode; no forced local archive or journal.
- Upgrade public contract metadata to revision 17; historical private references keep their already-public bytes.

## v0.3.0（2026-10-07）

### tansrai 开源迁移（GO-03）

- 仓库迁移到 `tansrai/tansr-go`，Go Modules 路径改为 `github.com/tansrai/tansr-go`；源码、Demo、生成器及当前教程统一使用新模块身份。
- 从 `github.com/cpple/tansr-go v0.2.0` 升级时，须同时修改 `go.mod` 的依赖路径和全部 Go imports，再运行 `go mod tidy`。新旧路径是不同模块，不能混用其客户端、DTO、错误或接口类型，也不能只改下载地址而保留旧 imports。
- 模块路径变化以新次版本发行；保留仓库历史、`v0.1.0` / `v0.2.0` 标签和原公开模块校验和，不覆盖旧标签或改写旧版本的 `go.mod`。
- API 协议、运行行为、公开方法、存储格式及 Go 1.25 最低版本均不变；39 份冻结合同文件和生成操作目录保持原字节，未增加第三方依赖。
- Go SDK 与 Demo 继续采用 MIT；上游参考资料及 Serve 核心的许可不变，范围见 [NOTICE](NOTICE.md)。

## v0.2.0（2026-10-07）

这是 v0 次版本升级，包含下述源码不兼容变化；不是 `v0.1.0` 的兼容补丁。模块路径仍是 `github.com/cpple/tansr-go`，Go 最低版本保持 1.25。

### 三平台运行与发行（GO-02）

- Go SDK 与 Demo 经权利人授权改为 MIT；冻结上游资料保留来源许可和原始指纹，Serve 核心许可不变，见 `NOTICE.md`。
- 增加同源 Serve 便携测试宿主构建入口，使三端可运行相同真实内核与 HTTP/SSE 链路；产物仅留本地验收归档，不成为 Go 运行依赖或公开发行内容。
- 增加真实跨进程档案与工具回执恢复检查；三平台最终结果和公开代理消费记录见 `doc/GO-02-三平台运行验收与公开发布.md`。
- 补充显式档案 ACK 修订恢复：先原样重放旧 ACK，仅在明确修订冲突后准备耐久恢复意图，使用冻结 rebase 操作并保存映射与完成回执；响应丢失后续办同一请求。`go-archive -recover-ack` 演示一次恢复。首次实际准备恢复会原子升级本地档案格式，旧 SDK 拒绝读取；默认同步不创建新恢复请求。
- 测试临时目录按物理路径准备，兼容系统临时目录本身是链接的环境，保留存储对链接路径的拒绝策略。

### 高层 SDK 与合同冻结（GO-01）

- 冻结 SDK2/UAPI 消费基线：CLI `83c64b2c`、manifest r7、81 操作/11族、39份源与语义文件；增加只读 `contractcheck`，不修改协议或自动跟随上游。
- 增加高层 `session`，支持会话、流式多轮、人工交互、插入输入、快照、压缩及原音频调用；完成、取消、会话结束与断流分开处理。
- 将 `executor` / `archive` 的旧占位接口替换为真实冻结 DTO 和实现：显式业务工具及持久执行记录、输出分块、档案完整性与加密单端保存、耐久后 ACK 及待决恢复。占位接口不是已验证的 wire DTO，使用原接口的源码须按新类型迁移。
- 三个独立命令行示例：`go-chat`、`go-tools`、`go-archive`；不开放任意 shell，不把单端档案能力宣称为 Node 全部记忆、同步与备份能力。
- 修复注入 HTTPClient 后可跟随重定向、流读取与关闭竞态、异常/EOF未及时释放连接、完整 CR 帧等待下一字节；补统一 JSON 的 UTF-8 和有界解析检查。
- 具体验收、真实 Serve 前置修复及发布状态见 `doc/GO-01-Go-SDK与Demo开发及验收.md`，不以本节代替验收结果。

### UAPI r7 对齐（U8-GO，2026-10-03）

此部分此前未独立打标签，与上述高层 SDK 一起进入 v0.2.0。

### 破坏性变更(D19 统一码为主的公开异常模型,用户拍板,0.14.0 批次)

`*api.APIError` 以统一码(`code` 19 码表)+ `retryAction` 为主字段;族码退为 `Detail.DomainCode`。`errors.Is` / `errors.As` 边界不变:`*APIError`(统一信封)、`*DomainError`(未包装族信封,今日仅 `archive-sync-v1`)、`*ClientError`(本地)、`*ContractUnavailableError`(非 unified-v1 Serve)。

| 旧(≤ `870d098`) | 新 | 说明 |
|---|---|---|
| `APIError.Code string` | `APIError.Code ErrorCode` | 19 常量 `api.Code*`(`CodeInvalidRequest` … `CodeNotCanonical`);`ErrorCodes []string` 为词表。`CodePayloadTooLarge` 为无类型常量,同时可作 `ErrorCode` / `ClientErrorCode` |
| `APIError.RetryAction string` | `APIError.RetryAction RetryAction` | 6 常量 `api.Action*`(`ActionNone` / `ActionSameRequest` / `ActionQueryStatus` / `ActionRebind` / `ActionRefresh` / `ActionRediscover`);`RetryActions []string` 为词表 |
| `APIError.Detail map[string]any` | `APIError.Detail ErrorDetail` | 结构字段 `Domain` / `Family` / `DomainCode` / `DomainStatus` / `DomainRetryAction` / `Fallback` / `Reason` / `Header` / `LimitBytes` / `ClosureID` / `Operation` / `State`;`Raw map[string]any` 保留原 detail;`Present()` 表示信封携 detail |
| `APIError.DomainCode()` | `APIError.Detail.DomainCode` | 族码降为次级字段;取值方法删除 |
| `APIError.DomainStatus()` | `APIError.Detail.DomainStatus` | 同上 |
| `APIError.DomainRetryAction()` | `APIError.Detail.DomainRetryAction` | 同上 |
| `apiErr.Code == "not_found"` | `apiErr.Code == api.CodeNotFound` 或 `errors.Is(err, &api.APIError{Code: api.CodeNotFound})` | `APIError.Is` 只按统一码匹配(目标的其它字段须为零值) |
| `ErrorEnvelope.Code / RetryAction string` | `ErrorCode` / `RetryAction` | wire 结构同步定型 |
| `RetryAdvice.Action string` | `RetryAdvice.Action RetryAction` | 与 `api.Action*` 直接比较 |
| `DomainRetryActionMap map[string]string` | `map[string]RetryAction` | 族动作词 → 统一动作 |
| (无) | `api.Reason*` 17 常量 | `detail.reason` 细因词表(`not_installed` / `outside_closure` + 15 个请求头细因,如 `if_match_stale` / `deadline_exceeded` / `idempotency_key_reused`) |
| (无) | `ClientErrorCode`:`CodeInvalidIfMatch` / `CodeIfMatchNotApplicable` / `CodeInvalidDeadline` / `CodeDeadlineExceeded`;哨兵 `ErrDeadlineExceeded` | 三头本地守卫 |

迁移要点:`switch apiErr.Code { case api.CodeCapabilityUnavailable: … }` 是跨 SDK 的同一分支结构;需要族内细分时读 `apiErr.Detail.DomainCode` / `apiErr.Detail.Reason`。`DomainError` 不在统一码模型内,`api.Advice()` 仍把其族动作词映射到统一动作。

### 三头接线(manifest r7 / U7-HDR,零新码)

- `CallOptions.IfMatch string`:取上次读的 `Meta.ETag`(强形 `"<revision>"`,裸十进制上线时加引号);只对 `Operation.AcceptsIfMatch()` 的写操作放行(manifest `expectedRevision` 非 null,9 个操作),否则本地 `CodeIfMatchNotApplicable`;弱校验器 / 通配符本地 `CodeInvalidIfMatch`。
- `CallOptions.Deadline time.Time`:以 RFC 3339 UTC 上线;`Deadline ≤ now` 本地 `CodeDeadlineExceeded` 不出网;`RetrySameRequest` 在 `now + wait ≥ Deadline` 时拒绝且不休眠。`Options.Now` 可注入时钟。
- `Meta.ETag string`:只收强形;`HeaderIfMatch` / `HeaderETag` / `HeaderDeadline` 常量;`NormalizeIfMatch` / `FormatDeadline` 与 Node `./api` 同形。
- `IdempotencyKeyOf` 改按族 `requestIdPath`(`Operation.RequestIDPath()`,`FamilyRequestIDPaths`)取体内键:`sdk2-ext-v1` → `request.requestId`;`agent-session-v1` 无体内位,只认 `Idempotency-Key` 头(此前对任意族读取顶层 `requestId` 的行为移除)。
- `Operation` 新增 `ETagPath` / `ExpectedRevision{Path, Kind}` / `Versioned()` / `AcceptsIfMatch()`;生成器承载 r7 三头事实字段。

### 事实源对齐(r7)

- `contract/api-manifest.json` sha256 `f6255bc868b4de5c…`(revision 7,81 操作 / 77 围栏,新增 `approval.credential.submit` → `OpApprovalCredentialSubmit`);`unified-v1.schema.json` `fd07962916f7662e…`;`unified-v1.golden.json` `42531a3937563d8c…`(165 向量 = 41 正 / 124 负);cross-vectors / sdk2-wire 未变。
- `api/schema.go`:`KeyPath`、`reason` 17 枚举(门面错误仍只允许 2 个)、`expectedRevision` 只许写操作且 kind ∈ `sequence|integer`、`precondition_failed.retryAction ∈ {rediscover, refresh}`、`limitBytes` 整数 ≥ 1、旧路径前缀 `^/v[123]`。

### 测试

- `api/golden_test.go` 锁 165 / 41 / 124(EventEnvelope 7 / 18),按名锁 6 个 r7 向量;`api/replay_test.go`(A29)165 向量经假 Serve 走 `/api` 全量回放 + 能力交集 74 操作越界恒 `capability_unavailable`;`TestThreeHeads` 覆盖 412 / 409 / 408 与同键回执重放。
