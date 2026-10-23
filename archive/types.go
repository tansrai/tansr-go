package archive

// Protocol is the archive wire family carried by the unified API.
const Protocol = "sdk2-ext-v1"

type Scope struct {
	ApplicationScopeID    string `json:"applicationScopeId"`
	EndUserID             string `json:"endUserId"`
	AuthorizationRevision string `json:"authorizationRevision"`
}
type RequestIdentity struct {
	RequestID      string `json:"requestId"`
	OperationEpoch string `json:"operationEpoch"`
}
type Generations struct {
	HistoryEpoch       string `json:"historyEpoch"`
	DeletionGeneration string `json:"deletionGeneration"`
	ProjectionRevision string `json:"projectionRevision"`
}
type Target struct {
	SessionID            string      `json:"sessionId"`
	Generations          Generations `json:"generations"`
	SourceSnapshotDigest string      `json:"sourceSnapshotDigest"`
}
type Epoch struct {
	ID        string `json:"id"`
	IssuedAt  string `json:"issuedAt"`
	ExpiresAt string `json:"expiresAt"`
	State     string `json:"state"`
}
type Limits struct {
	ControlBytes               int `json:"controlBytes"`
	RecordBytes                int `json:"recordBytes"`
	PageRecords                int `json:"pageRecords"`
	PageBytes                  int `json:"pageBytes"`
	AttachmentBytes            int `json:"attachmentBytes"`
	ChunkBytes                 int `json:"chunkBytes"`
	MaterialConcurrent         int `json:"materialConcurrent"`
	MaterialQueue              int `json:"materialQueue"`
	MaterialCandidates         int `json:"materialCandidates"`
	MaterialBytes              int `json:"materialBytes"`
	MaterialDeadlineMs         int `json:"materialDeadlineMs"`
	PendingRecords             int `json:"pendingRecords"`
	PendingBytes               int `json:"pendingBytes"`
	InflightReserveBytes       int `json:"inflightReserveBytes"`
	OfflineMs                  int `json:"offlineMs"`
	EventRetentionMs           int `json:"eventRetentionMs"`
	EventRetentionFrames       int `json:"eventRetentionFrames"`
	EventRetentionBytes        int `json:"eventRetentionBytes"`
	TerminalReceiptRetentionMs int `json:"terminalReceiptRetentionMs"`
	EpochLifetimeMs            int `json:"epochLifetimeMs"`
	MaterialChunkBytes         int `json:"materialChunkBytes"`
}
type Capabilities struct {
	Persistence       *SourcePersistence `json:"persistence,omitempty"`
	Protocol          string             `json:"protocol"`
	Availability      string             `json:"availability"`
	Capabilities      []string           `json:"capabilities"`
	Limits            Limits             `json:"limits"`
	OperationEpoch    Epoch              `json:"operationEpoch"`
	ArchiveAckFormats []string           `json:"archiveAckFormats"`
}
type BindingTarget struct {
	Protocol       string  `json:"protocol"`
	Target         Target  `json:"target"`
	Revision       string  `json:"revision"`
	BindingID      *string `json:"bindingId"`
	OperationEpoch *Epoch  `json:"operationEpoch"`
}
type Binding struct {
	Persistence          *SourcePersistence   `json:"persistence,omitempty"`
	Protocol             string               `json:"protocol"`
	BindingID            string               `json:"bindingId"`
	Scope                Scope                `json:"scope"`
	Target               Target               `json:"target"`
	Revision             string               `json:"revision"`
	State                string               `json:"state"`
	SourceID             string               `json:"sourceId"`
	AcceptedCapabilities []string             `json:"acceptedCapabilities"`
	RejectedCapabilities []RejectedCapability `json:"rejectedCapabilities"`
	Availability         string               `json:"availability"`
	OperationEpoch       *Epoch               `json:"operationEpoch"`
	Limits               Limits               `json:"limits"`
	ArchiveAckFormat     *string              `json:"archiveAckFormat"`
}
type RejectedCapability struct {
	Capability string `json:"capability"`
	Reason     string `json:"reason"`
}
type BindingCreateRequest struct {
	Protocol             string          `json:"protocol"`
	Request              RequestIdentity `json:"request"`
	Target               Target          `json:"target"`
	ExpectedRevision     string          `json:"expectedRevision"`
	RequiredCapabilities []string        `json:"requiredCapabilities"`
	OptionalCapabilities []string        `json:"optionalCapabilities"`
	Archive              BindingArchive  `json:"archive"`
}
type BindingArchive struct {
	Strategy            string `json:"strategy"`
	SourceID            string `json:"sourceId"`
	Durability          string `json:"durability"`
	Delivery            string `json:"delivery"`
	SessionAvailability string `json:"sessionAvailability"`
	AckFormat           string `json:"ackFormat"`
}
type ArtifactRef struct {
	ArtifactID string `json:"artifactId"`
	SourceID   string `json:"sourceId"`
	Bytes      int    `json:"bytes"`
	SHA256     string `json:"sha256"`
	MediaType  string `json:"mediaType"`
}
type Coverage struct {
	FromSequence    string `json:"fromSequence"`
	ThroughSequence string `json:"throughSequence"`
	HeadDigest      string `json:"headDigest"`
}
type Record struct {
	RecordID          string        `json:"recordId"`
	Sequence          string        `json:"sequence"`
	Target            Target        `json:"target"`
	TurnID            string        `json:"turnId"`
	RecordKind        string        `json:"recordKind"`
	TurnState         string        `json:"turnState"`
	PredecessorDigest string        `json:"predecessorDigest"`
	RecordDigest      string        `json:"recordDigest"`
	Payload           ArtifactRef   `json:"payload"`
	Attachments       []ArtifactRef `json:"attachments"`
	SourceEventRange  *EventRange   `json:"sourceEventRange,omitempty"`
	Projection        *Projection   `json:"projection,omitempty"`
	PayloadDigest     string        `json:"payloadDigest"`
}
type EventRange struct {
	FirstSeq int64 `json:"firstSeq"`
	LastSeq  int64 `json:"lastSeq"`
}
type Projection struct {
	Coverage         Coverage `json:"coverage"`
	AssemblerVersion string   `json:"assemblerVersion"`
	SourceHeadDigest string   `json:"sourceHeadDigest"`
}
type Page struct {
	Protocol                 string      `json:"protocol"`
	BindingID                string      `json:"bindingId"`
	Generations              Generations `json:"generations"`
	Records                  []Record    `json:"records"`
	NextAfterSequence        *string     `json:"nextAfterSequence"`
	Complete                 bool        `json:"complete"`
	PublishedThroughSequence *string     `json:"publishedThroughSequence"`
}
type ArtifactChunk struct {
	Protocol    string      `json:"protocol"`
	BindingID   string      `json:"bindingId"`
	ArtifactID  string      `json:"artifactId"`
	SourceID    string      `json:"sourceId"`
	Generations Generations `json:"generations"`
	Offset      int         `json:"offset"`
	Bytes       int         `json:"bytes"`
	TotalBytes  int         `json:"totalBytes"`
	SHA256      string      `json:"sha256"`
	ChunkSHA256 string      `json:"chunkSha256"`
	Base64      string      `json:"base64"`
}
type Status struct {
	Persistence               *SourcePersistence `json:"persistence,omitempty"`
	Protocol                  string             `json:"protocol"`
	BindingID                 string             `json:"bindingId"`
	Revision                  string             `json:"revision"`
	Generations               Generations        `json:"generations"`
	SourceID                  string             `json:"sourceId"`
	SourceGeneration          string             `json:"sourceGeneration"`
	PublishedThroughSequence  *string            `json:"publishedThroughSequence"`
	AcknowledgedCoverage      *Coverage          `json:"acknowledgedCoverage"`
	ReleasableThroughSequence *string            `json:"releasableThroughSequence"`
	PendingBytes              int                `json:"pendingBytes"`
	PendingRecords            int                `json:"pendingRecords"`
	SessionPersistence        string             `json:"sessionPersistence"`
	State                     string             `json:"state"`
}
type ArtifactReceipt struct {
	ArtifactID string `json:"artifactId"`
	SHA256     string `json:"sha256"`
	State      string `json:"state"`
}
type Ack struct {
	Protocol         string            `json:"protocol"`
	Request          RequestIdentity   `json:"request"`
	BindingID        string            `json:"bindingId"`
	ExpectedRevision string            `json:"expectedRevision"`
	Generations      Generations       `json:"generations"`
	SourceID         string            `json:"sourceId"`
	SourceGeneration string            `json:"sourceGeneration"`
	Coverage         Coverage          `json:"coverage"`
	Attachments      []ArtifactReceipt `json:"attachments"`
	AckFormat        string            `json:"ackFormat"`
	Payloads         []ArtifactReceipt `json:"payloads"`
}
type MutationReceipt struct {
	Protocol       string          `json:"protocol"`
	Request        RequestIdentity `json:"request"`
	BindingID      string          `json:"bindingId"`
	Operation      string          `json:"operation"`
	SemanticDigest string          `json:"semanticDigest"`
	State          string          `json:"state"`
	Revision       string          `json:"revision"`
	OutcomeRef     string          `json:"outcomeRef"`
}

// Identity is supplied by the host from an authenticated binding and status,
// never restored as authority from an archive file.
type Identity struct {
	ApplicationScopeID string      `json:"applicationScopeId"`
	EndUserID          string      `json:"endUserId"`
	BindingID          string      `json:"bindingId"`
	SessionID          string      `json:"sessionId"`
	Generations        Generations `json:"generations"`
	SourceID           string      `json:"sourceId"`
	SourceGeneration   string      `json:"sourceGeneration"`
}
type Head struct {
	Sequence     string `json:"sequence"`
	RecordDigest string `json:"recordDigest"`
}
type SyncResult struct {
	Records   int
	Complete  bool
	Recovered bool
	Receipt   *MutationReceipt
}
