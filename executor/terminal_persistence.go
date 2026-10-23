package executor

// TerminalPersistenceToolName selects the separately approved durable profile.
const TerminalPersistenceToolName = "TansrTerminalPersistenceV1"
const TerminalPersistenceDefinitionDigest = "33029a264edf81f3fda2a13fc382403d0cd7ffefa1f38088bb9366f387a13587"

const PersistenceCustodyToolName = "TansrPersistenceCustodyHandoffV1"
const PersistenceCustodyDefinitionDigest = "7f3b94cb801bf29be071674cdab0450855e70c55ab44bea27508c30dfbebb5e7"

// TerminalPersistenceHost preserves the same execution and encrypted journal fence.
type TerminalPersistenceHost = MemoryPublicationHost
