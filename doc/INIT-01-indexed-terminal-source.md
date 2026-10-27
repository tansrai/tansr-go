# INIT-01 / A05：Go 按需 terminal 持久源（未发布候选）

`terminalpersistence.OpenIndexedFileStore(options)` 显式创建或重开认证索引来源，返回原 `*FileStore`，沿用 `NewHost`、`NewCustodyHost`、`Execute`、`CopyTo` 和原共享写者权限。`OpenFileStore` 继续选择旧单快照，保持原完整读取成本；两个入口互拒对方的格式，不探测后转换，不自动迁移。

## 接入

`examples/go-memory` 的 TPV1 分支支持 `--layout authenticated-indexed-v2`。首次 `--mode create` 和后续 `--mode reopen` 须显式使用同一个布局；省略布局仍为 `legacy-snapshot-v1`。原身份、独立加密日志密钥、设备绑定请求、供材授权以及 `--session` 要求不变。离线复制使用来源布局，结果仍为原只读候选，不激活写者。

标准初始化的 `PreparationOptions.PreparePersistence` 可直接返回已打开的 `*FileStore`：该类型新增的 `Stop()` 等同 `Close()`，由同一准备生命周期收回资源。实际来源重开、就绪先于建会的定向例是 `session/TestPreparationOwnsActualIndexedSource`。此入口只提供源本身；需要独立供材桥的 terminal 恢复仍由宿主先安装并等待桥 ready，再返回准备完成。现有 go-memory 是绑定已存在会话的设备示例；SDK 不虚构一个建会前设备协议，也不以空回调宣称供材就绪。cloud 来源省略此端侧资源，仍由原核心装配。

## 读取与提交

格式标识为 `tansr-go-terminal-persistence-indexed-v2`，密文 magic 为 `Tansr-Go-TerminalPersistence-Indexed/2`。主路径保存认证头；独立对象、原键行和 Patricia 节点以内容地址保存在同一私有根内。每件采用原 AES-256-GCM 与宿主密钥，AAD 绑定新格式、完整原身份、文件种类及原键。原独占介质锁、目录／文件身份检查、写入同步和原子替换保持。

头绑定当前 TPV1 root、完整活动 transfer ID、原逻辑容量聚合、认证树根及叶数、当前 custody 和原迁移记录引用。当前工作 W 与精确查询所需原件才被解密；永久 transfer、双索引和回执值按原键查取。原 engine 的 row/object/capacity 接口共用原 begin/put/commit/reuse/CAS/容量规则，所选缓存不作为全状态送入原 Audit。正文 range 只取覆盖页及块，分别沿原页 hash、块 hash 和 AEAD 核验。

树节点编码复用 `tansr-authenticated-patricia-sha256-v1`，共享向量 SHA256：`e1f9493b10dc9dda962149449743c4f24fbf281728c1a626fd61f33381fba90e`。空键必须由认证路径证明；缺节点或原键行不能当作 absent 后重新执行。逻辑回收写入认证墓碑，永久原键不删除。

每次先持久化带原身份摘要的 HMAC 资源预算，再封存新密文、最后原子发布头；预算不是业务回执。加密次数、字节和不可变文件实占沿原上限计额，未发布孤儿仍占额。seal 只写最终可达的新树节点，中间节点不落盘。活动 transfer 在原锁内保留剩余完成所需物理／密钥预算，按最大路径与认证叶数推导上界，保留原 4 MiB 正文和逻辑容量帽。首次写入及普通重开不枚举目录。

写后失回保持原 unknown，关闭并以原路径／密钥／原 transfer 重开查询；不会重放。已发布头是业务权威。创建尚未发布头即中断时可能只留下资源预算，须保留介质作显式处置，不能删除预算后盲建。新版不自动清理历史节点及孤儿，因此物理空间可能早于逻辑额度用尽；此时明确拒收，不抬高原 128 MiB 物理帽。

## 显式完整操作与边界

`Audit(ctx)`、原 `CopyTo` 和托管迁移显式遍历全部认证原件，调用原完整验证／迁移归约器；完成后释放完整视图。迁移大目录独立存储，普通开库不取。原 shared authority 决定冻结、cutover、旧写者拒绝和后续接管，读到原事实不授写权。

普通打开不宣称未选区域已审计；无关损坏延迟到被选取或显式 Audit 时拒绝。此布局不检测整套介质被一致回滚成旧备份，外部代际／custody 权威仍承担该边界。物理返回字节统计含密文、认证节点、原键行、头与预算文件，不等于操作系统块 IO；测试只用隔离合成文件，不含真实用户数据。
