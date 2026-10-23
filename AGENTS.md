# tansr-go 协作入口

本仓是 tansr 统一 `/api` 合同的 Go SDK。UAPI 骨架已完成，GO-01 在其基础上实现高层 SDK 与 Demo。执行任务前读取 tansr-cli 仓的
`doc/工程工作纪律.md`、`doc/90-SDK技术手册.md` 第 16 章(§16.6 七条纪律)与 RFC-UAPI-1。

## SDK2 / UAPI 冻结基线

先读 `doc/GO-01-SDK2与UAPI合同冻结-2026-10-07.md` 与本轮 `doc/GO-01-Go-SDK与Demo开发及验收.md`。
GO-01 已收编；后续三平台实际运行与公开发行按 `doc/GO-02-三平台运行验收与公开发布.md` 执行，沿用同一冻结合同，不重开 GO-01 的任务分母。
GO-03 的 `tansrai` 迁移按 `doc/GO-03-tansrai开源迁移与发布.md` 执行。当前模块路径为 `github.com/tansrai/tansr-go`，v0.3.0 起所有源码 imports 和当前接入教程统一使用该路径；GO-01/02 的历史证据及 v0.1.0/v0.2.0 标签保持原样。MIT 范围与上游参考资料许可见 `NOTICE.md`，不因仓库归属迁移修改冻结合同或参考资料。
PST-SERVE-02 已显式升级：`contract/LOCK.json` 的公共合同锁定 CLI `d59972fb` / manifest r17（91 操作、87 围栏、12 families），43 份随包文件中的 18 份历史参考仍保留原 `83c64b2c` 字节和逐文件 pin。agent-session 私有 TS 新源只登记 source-only SHA，不随包复制；公开默认 checker 仅核元数据对应，可选 `-source` 作者模式才核私有 Git blob。详见 `doc/PST-SERVE-02-Go无盘双模式接入.md`。SDK 实现不准顺带改 schema、金样、操作目录或冻结字节。
执行 `go run ./internal/gen/contractcheck` 校验本地锁，跨仓对照增加 `-source J:/tansr/tansr-cli`。新合同须先提修订和会签，再显式更新基线，不自动跟随上游 HEAD。

## 事实源(只读,路径以 tansr-cli 仓根为准)

| 本仓副本 | 事实源 | 用途 |
| --- | --- | --- |
| `contract/api-manifest.json` | `packages/server/contract/api-manifest.json`(revision 17,91 操作 / 87 围栏操作) | **唯一**的操作目录事实源 → `api/operations_gen.go`(含三头事实:每操作 `etagPath` / `expectedRevision`、每族 `requestIdPath`) |
| `contract/unified-v1.schema.json` | `doc/rfc/unified-v1.schema.json` | `api/schema.go` 逐定义对照的校验规则来源(只读对照物,Go 不在运行时加载) |
| `contract/unified-v1.golden.json` | `doc/rfc/unified-v1.golden.json`(166 向量) | `api` 类型形状与正负向量测试(`api/golden_test.go` 锁定向量计数);`api/replay_test.go` 经假 Serve 走 `/api` 全量回放,不可上线的区分按名锁定 |
| `contract/canonical-cross-vectors.json` | `scripts/api/fixtures/canonical-cross-vectors.json` | `canonical` 127 交叉向量 |
| `contract/sdk2-wire-v1.json` | `scripts/sdk2/fixtures/sdk2-wire-v1.json` | canonical 字节金样、路径段编码 |

来源提交与 sha256 登记在 `contract/PROVENANCE.json`;更新副本时同步更新该文件,并同步 `api/golden_test.go` 的向量计数锁、`api/client_test.go` / `internal/manifestgen/render_test.go` 的锁定 revision。

## 生成

```
go run ./internal/gen/manifest2go            # 重生成 api/operations_gen.go
go run ./internal/gen/manifest2go -check     # 校验已提交文件未过期
go test ./api -run TestOperationsGenerated   # 等价的 -check(go test 恒执行)
go test ./internal/manifestgen -update       # 宿主拒绝启动新编译可执行文件时的改写入口
```

- **禁止手写路径、方法、查询键、域名**:一切来自 manifest;`operations_gen.go` 不得手改。
- `api/schema.go` 的校验规则逐条对应 `unified-v1.schema.json` 的定义;改 schema 先改副本与 PROVENANCE,再跑 golden 测试。
- `EventEnvelope` 严格按 D18 七键 `{contract, eventId, domain, type, cursorSet, terminalStatus, raw}` 解析:缺键 / 第八键(含旧形 `seq` / `payload`)/ `raw` 非对象 → `invalid_envelope`;`archiveCoverage` 唯一允许对象形(schema `ArchiveCoverage`)。不再容忍过渡形。
- `sse.Reader` 按 WHATWG 分发规则:无 `data` 字段的帧不分发(`id:` 仍推进 `LastEventID`),与 Node `SseParser` 同律。
- **D19 异常模型(统一码为主)**:`*APIError` 的主字段是 `Code ErrorCode`(19 码,`Code*` 常量)与 `RetryAction RetryAction`(`Action*` 常量);族码只在 `Detail.DomainCode`(`ErrorDetail` 结构,`Raw` 保留原 detail)。`errors.Is(err, &APIError{Code: …})` 只按统一码匹配。不新增统一码、不把族码当统一码;`DomainError` 仅为未包装族信封的残余形(今日仅 `archive-sync-v1`)。
- **三头接线(r7)**:`CallOptions.IdempotencyKey` / `IfMatch` / `Deadline` 对应 `Idempotency-Key` / `If-Match` / `deadline`;`Meta.ETag` 只收强形 `"<revision>"`;`If-Match` 只对 `Operation.AcceptsIfMatch()`(manifest `expectedRevision` 非 null 的写操作)放行;deadline 已过本地即拒、`RetrySameRequest` 不越 deadline 重放,SDK 永不延长截止。语义对照 Node `@tansr/api-client` `./api`。

## 门禁

`gofmt -l .` 为空;`go vet ./...`;`go test ./...`(可用 CGO 时加 `-race`);
`GOOS=linux|darwin|windows GOARCH=amd64 go build ./...`；`go run ./internal/gen/contractcheck`；`go run ./internal/gen/manifest2go -check`。零第三方依赖(标准库;`golang.org/x/` 不引入除非必要)。

## 提交

格式 `type(scope): 具体变化 (任务号)`，合同迁移历史使用 UAPI-01，高层 SDK 与 Demo 使用 GO-01，三平台运行和公开发行使用 GO-02，tansrai 仓库与模块迁移使用 GO-03；UTF-8、LF、无 BOM(`git commit -F`)。不自行添加远端、不推送开发分支；远端发布仍由主线发布负责人执行。
