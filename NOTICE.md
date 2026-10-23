# License scope / 许可范围

2026-10-07，经权利人明确授权，Go SDK、Go Demo、Go 测试与其 SDK 文档采用根目录 [MIT License](LICENSE)。安装和使用这些 Go 包不再要求事先签署原 Go 仓的商业协议。

随本 Go SDK 分发的 `contract/` 平层 schema、golden、manifest 等机器协议资产同属此次 MIT 发行范围，包括由 `contract/embed.go` 嵌入 Go 包的 schema。这是这些 SDK 副本的授权，不改变上游 CLI 仓同名源文件或 Serve 实现的许可。

`contract/reference/` 是按原字节冻结的上游规范、行为参考和介质附件，保留来源许可：该目录的 [LICENSE](contract/reference/LICENSE) 来自 `cpple/tansr`；原 `packages/api-client`、`packages/protocol`、`packages/sdk` 的 MIT 许可在各自副本目录继续保留。尤其 `contract/reference/packages/server/` 内的两个 TypeScript 参考文件并未改授 MIT。既往公开的 18 份参考字节及其原 Git pin 保持不变。本次 PST-SERVE-02 仅升级公共机器合同；共 43 份随包合同/参考文件，私有 agent-session TS 新源仅登记 SHA 元数据，不复制新源码或内部 RFC。

Go SDK 的 MIT 授权不改变 Serve / kernel 或上游仓库其他代码的许可，也不授予平台服务、模型、数据或商标的使用权。私有 Serve 验收 bundle 只在本地测试归档中生成，不进入本模块或发行附件。

The Go SDK, demos, Go tests, SDK documentation and the machine-readable assets distributed directly under `contract/` are licensed under the root MIT License. Frozen upstream materials under `contract/reference/` retain their source licenses, including the existing MIT exceptions recorded within that subtree. This release does not relicense the corresponding upstream sources or the Serve/kernel implementation, or publish the private integration-host bundle.
