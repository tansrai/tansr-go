package archive

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tansrai/tansr-go/api"
	"github.com/tansrai/tansr-go/canonical"
	"github.com/tansrai/tansr-go/internal/wire"
)

var (
	ErrIntegrity       = errors.New("archive: integrity or identity mismatch")
	ErrCapacity        = errors.New("archive: configured capacity exceeded")
	ErrPendingAck      = errors.New("archive: resolve the durable pending ACK first")
	ErrClosed          = errors.New("archive: store is closed")
	ErrBindingExists   = errors.New("archive: session already has a binding; reopen it explicitly")
	ErrReceipt         = errors.New("archive: ACK receipt does not prove completed original operation")
	ErrSnapshotChanged = errors.New("archive: publication changed during sampling; retry before receiving a new page")
)

// Client uses only manifest-generated routes. It never retries mutations with a new identity.
type Client struct {
	api           *api.Client
	persistenceMu sync.Mutex
	persistence   map[string]*SourcePersistence
}

func NewClient(client *api.Client) *Client {
	return &Client{api: client, persistence: make(map[string]*SourcePersistence)}
}

func (c *Client) call(ctx context.Context, operation, definition string, options api.CallOptions, status int, out any) error {
	if c == nil || c.api == nil {
		return errors.New("archive: API client is required")
	}
	result, err := c.api.Call(ctx, operation, options)
	if err != nil {
		return err
	}
	if result.Status != status || result.ContentType != "application/json" {
		return ErrIntegrity
	}
	if _, err = wire.Decode(Protocol, definition, result.Body); err != nil {
		return fmt.Errorf("archive %s: %w", definition, err)
	}
	return json.Unmarshal(result.Body, out)
}
func readOptions(bindingID string) api.CallOptions {
	return api.CallOptions{Params: map[string]string{"id": bindingID}, Query: map[string]string{"protocol": Protocol}}
}
func generationQuery(g Generations) map[string]string {
	return map[string]string{"protocol": Protocol, "historyEpoch": g.HistoryEpoch, "deletionGeneration": g.DeletionGeneration, "projectionRevision": g.ProjectionRevision}
}
func validate(name string, value any) error { return wire.Validate(Protocol, name, value) }
func seq(s string) int64                    { n, _ := strconv.ParseInt(s, 10, 64); return n }
func sameString(a, b *string) bool          { return a == nil && b == nil || a != nil && b != nil && *a == *b }
func has(a []string, b string) bool {
	for _, v := range a {
		if v == b {
			return true
		}
	}
	return false
}
func canon(value any) ([]byte, error) {
	return canonical.Encode(value, canonical.Options{MaxBytes: 2 << 20})
}
func equal(a, b any) bool {
	x, e := canon(a)
	y, f := canon(b)
	return e == nil && f == nil && string(x) == string(y)
}
func digest(bytes []byte) string { return canonical.Hex(sha256.Sum256(bytes)) }
func domainDigest(domain string, bytes []byte) string {
	value, e := canonical.DomainDigest(domain, bytes)
	if e != nil {
		return ""
	}
	return canonical.Hex(value)
}

func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	var value Capabilities
	err := c.call(ctx, api.OpArchiveCapabilities, "CapabilitiesResponse", api.CallOptions{}, 200, &value)
	if err == nil && (value.Limits.InflightReserveBytes > value.Limits.PendingBytes || len(value.ArchiveAckFormats) != boolInt(has(value.Capabilities, "archive-transfer-v1"))) {
		err = ErrIntegrity
	}
	if err == nil {
		err = validateEpoch(&value.OperationEpoch, value.Limits.EpochLifetimeMs)
	}
	return value, err
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
func (c *Client) BindingTarget(ctx context.Context, sessionID string) (BindingTarget, error) {
	var value BindingTarget
	if err := validate("LegacyId", sessionID); err != nil {
		return value, err
	}
	err := c.call(ctx, api.OpArchiveBindingTarget, "BindingTargetView", readOptions(sessionID), 200, &value)
	if err == nil && (value.Target.SessionID != sessionID || value.BindingID == nil && (value.Revision != "0" || value.OperationEpoch != nil)) {
		err = ErrIntegrity
	}
	if err == nil {
		err = validateEpoch(value.OperationEpoch, 0)
	}
	return value, err
}

// Create prepares a new single-source binding. On an uncertain create response,
// inspect the original operation by requestID rather than creating a replacement.
// Existing bindings must be explicitly reopened with Binding.
func (c *Client) Create(ctx context.Context, sessionID, sourceID, requestID string) (Binding, error) {
	var zero Binding
	caps, err := c.Capabilities(ctx)
	if err != nil {
		return zero, err
	}
	if caps.Persistence != nil {
		return zero, ErrSourceManaged
	}
	if !has(caps.Capabilities, "archive-transfer-v1") || !has(caps.ArchiveAckFormats, "split-receipts-v1") {
		return zero, errors.New("archive: archive-transfer-v1 is unavailable")
	}
	target, err := c.BindingTarget(ctx, sessionID)
	if err != nil {
		return zero, err
	}
	if target.BindingID != nil {
		return zero, ErrBindingExists
	}
	input := BindingCreateRequest{Protocol: Protocol, Request: RequestIdentity{requestID, caps.OperationEpoch.ID}, Target: target.Target, ExpectedRevision: target.Revision,
		RequiredCapabilities: []string{"archive-transfer-v1"}, OptionalCapabilities: []string{}, Archive: BindingArchive{"single-authorized-source", sourceID, "source-ack-with-durable-spool", "required", "legacy-complete", "split-receipts-v1"}}
	if has(caps.Capabilities, "context-materials-v1") {
		input.OptionalCapabilities = append(input.OptionalCapabilities, "context-materials-v1")
	}
	value, err := c.CreateBinding(ctx, input)
	if err != nil {
		return zero, err
	}
	if !limitsWithin(value.Limits, caps.Limits) {
		return zero, ErrIntegrity
	}
	return value, nil
}
func (c *Client) CreateBinding(ctx context.Context, input BindingCreateRequest) (Binding, error) {
	var value Binding
	if err := validate("BindingCreateRequest", input); err != nil {
		return value, err
	}
	seen := map[string]bool{}
	for _, s := range append(append([]string{}, input.RequiredCapabilities...), input.OptionalCapabilities...) {
		if seen[s] {
			return value, ErrIntegrity
		}
		seen[s] = true
	}
	err := c.call(ctx, api.OpArchiveBindingCreate, "BindingView", api.CallOptions{Body: input, IdempotencyKey: input.Request.RequestID, IfMatch: strconv.Quote(input.ExpectedRevision)}, 201, &value)
	if err != nil {
		return value, err
	}
	if err = validateBinding(value); err != nil {
		return value, err
	}
	if !equal(input.Target, value.Target) || value.SourceID != input.Archive.SourceID {
		return value, ErrIntegrity
	}
	decided := map[string]bool{}
	for _, s := range value.AcceptedCapabilities {
		decided[s] = true
	}
	for _, s := range value.RejectedCapabilities {
		decided[s.Capability] = true
	}
	if len(decided) != len(seen) {
		return value, ErrIntegrity
	}
	for s := range decided {
		if !seen[s] {
			return value, ErrIntegrity
		}
	}
	for _, s := range input.RequiredCapabilities {
		if !has(value.AcceptedCapabilities, s) {
			return value, ErrIntegrity
		}
	}
	return value, nil
}
func limitsWithin(a, b Limits) bool {
	x, err := canon(a)
	if err != nil {
		return false
	}
	y, err := canon(b)
	if err != nil {
		return false
	}
	av, err := canonical.Decode(x, canonical.Options{MaxBytes: 2 << 20})
	if err != nil {
		return false
	}
	bv, err := canonical.Decode(y, canonical.Options{MaxBytes: 2 << 20})
	if err != nil {
		return false
	}
	for k, v := range av.(map[string]any) {
		if v.(int64) > bv.(map[string]any)[k].(int64) {
			return false
		}
	}
	return true
}
func validateBinding(value Binding) error {
	if err := validate("BindingView", value); err != nil {
		return err
	}
	if value.Persistence != nil && value.Persistence.SourceID != value.SourceID {
		return ErrIntegrity
	}
	if value.Limits.InflightReserveBytes > value.Limits.PendingBytes {
		return ErrIntegrity
	}
	if err := validateEpoch(value.OperationEpoch, value.Limits.EpochLifetimeMs); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, s := range value.AcceptedCapabilities {
		if seen[s] {
			return ErrIntegrity
		}
		seen[s] = true
	}
	for _, s := range value.RejectedCapabilities {
		if seen[s.Capability] {
			return ErrIntegrity
		}
		seen[s.Capability] = true
	}
	if has(value.AcceptedCapabilities, "archive-transfer-v1") != (value.ArchiveAckFormat != nil && *value.ArchiveAckFormat == "split-receipts-v1") {
		return ErrIntegrity
	}
	return nil
}
func validateEpoch(epoch *Epoch, maximum int) error {
	if epoch == nil {
		return nil
	}
	issued, err := time.Parse(time.RFC3339Nano, epoch.IssuedAt)
	if err != nil {
		return ErrIntegrity
	}
	expires, err := time.Parse(time.RFC3339Nano, epoch.ExpiresAt)
	if err != nil {
		return ErrIntegrity
	}
	duration := expires.Sub(issued)
	if duration <= 0 || maximum > 0 && duration > time.Duration(maximum)*time.Millisecond {
		return ErrIntegrity
	}
	return nil
}
func (c *Client) Binding(ctx context.Context, id string) (Binding, error) {
	var value Binding
	if err := validate("Id", id); err != nil {
		return value, err
	}
	err := c.call(ctx, api.OpArchiveBindingGet, "BindingView", readOptions(id), 200, &value)
	if err == nil && value.BindingID != id {
		err = ErrIntegrity
	}
	if err == nil {
		err = validateBinding(value)
	}
	if err == nil {
		err = c.observePersistence(id, value.Persistence)
	}
	return value, err
}
func (c *Client) Status(ctx context.Context, id string) (Status, error) {
	var value Status
	if err := validate("Id", id); err != nil {
		return value, err
	}
	err := c.call(ctx, api.OpArchiveStatus, "ArchiveStatus", readOptions(id), 200, &value)
	if err != nil {
		return value, err
	}
	if value.BindingID != id || value.Persistence != nil && (value.Persistence.SourceID != value.SourceID || value.Persistence.SourceGeneration != value.SourceGeneration) {
		return value, ErrIntegrity
	}
	if err = c.observePersistence(id, value.Persistence); err != nil {
		return value, err
	}
	if value.PublishedThroughSequence != nil && seq(*value.PublishedThroughSequence) < 1 || value.ReleasableThroughSequence != nil && seq(*value.ReleasableThroughSequence) < 1 {
		return value, ErrIntegrity
	}
	if value.AcknowledgedCoverage == nil {
		if value.ReleasableThroughSequence != nil {
			return value, ErrIntegrity
		}
	} else {
		if !validCoverage(*value.AcknowledgedCoverage) || value.PublishedThroughSequence == nil || seq(value.AcknowledgedCoverage.ThroughSequence) > seq(*value.PublishedThroughSequence) || value.ReleasableThroughSequence != nil && seq(*value.ReleasableThroughSequence) > seq(value.AcknowledgedCoverage.ThroughSequence) {
			return value, ErrIntegrity
		}
	}
	return value, nil
}
func validCoverage(c Coverage) bool {
	return validate("Coverage", c) == nil && seq(c.FromSequence) >= 1 && seq(c.ThroughSequence) >= seq(c.FromSequence)
}

func IdentityFrom(b Binding, s Status) (Identity, error) {
	value := Identity{b.Scope.ApplicationScopeID, b.Scope.EndUserID, b.BindingID, b.Target.SessionID, b.Target.Generations, b.SourceID, s.SourceGeneration}
	if validateBinding(b) != nil || validate("ArchiveStatus", s) != nil || b.BindingID != s.BindingID || b.SourceID != s.SourceID || b.Target.Generations != s.Generations || b.Revision != s.Revision || b.State != s.State || b.State == "closed" || !samePersistence(b.Persistence, s.Persistence) || b.Persistence != nil && b.Persistence.SourceGeneration != s.SourceGeneration {
		return Identity{}, ErrIntegrity
	}
	return value, nil
}
func (c *Client) ReadRecords(ctx context.Context, b Binding, after *string) (Page, error) {
	var value Page
	if err := validateBinding(b); err != nil {
		return value, err
	}
	query := generationQuery(b.Target.Generations)
	query["limit"] = strconv.Itoa(b.Limits.PageRecords)
	query["maxBytes"] = strconv.Itoa(b.Limits.PageBytes)
	if after != nil {
		if err := validate("Sequence", *after); err != nil {
			return value, err
		}
		query["afterSequence"] = *after
	}
	err := c.call(ctx, api.OpArchiveRecordsRead, "ArchivePage", api.CallOptions{Params: map[string]string{"id": b.BindingID}, Query: query, MaxResponseBytes: int64(b.Limits.PageBytes)}, 200, &value)
	if err == nil {
		err = verifyPage(b, after, value)
	}
	return value, err
}
func verifyPage(b Binding, after *string, p Page) error {
	if err := validate("ArchivePage", p); err != nil {
		return err
	}
	if p.BindingID != b.BindingID || p.Generations != b.Target.Generations || len(p.Records) > b.Limits.PageRecords {
		return ErrIntegrity
	}
	encoded, err := canon(p)
	if err != nil {
		return err
	}
	if len(encoded) > b.Limits.PageBytes {
		return ErrCapacity
	}
	previous := int64(0)
	if after != nil {
		previous = seq(*after)
	}
	priorDigest := ""
	ids := map[string]bool{}
	refs := map[string]ArtifactRef{}
	for _, r := range p.Records {
		if seq(r.Sequence) != previous+1 || ids[r.RecordID] || r.Target.SessionID != b.Target.SessionID || r.Target.Generations != b.Target.Generations || previous == 0 && r.PredecessorDigest != strings.Repeat("0", 64) || priorDigest != "" && r.PredecessorDigest != priorDigest {
			return ErrIntegrity
		}
		ids[r.RecordID] = true
		if err := verifyRecord(r, b.Limits.RecordBytes); err != nil {
			return err
		}
		for _, ref := range append([]ArtifactRef{r.Payload}, r.Attachments...) {
			if ref.SourceID != b.SourceID || ref.Bytes > b.Limits.AttachmentBytes {
				return ErrIntegrity
			}
			if old, ok := refs[ref.ArtifactID]; ok && old != ref {
				return ErrIntegrity
			}
			refs[ref.ArtifactID] = ref
		}
		previous = seq(r.Sequence)
		priorDigest = r.RecordDigest
	}
	next := after
	if len(p.Records) > 0 {
		next = &p.Records[len(p.Records)-1].Sequence
	}
	if !sameString(p.NextAfterSequence, next) {
		return ErrIntegrity
	}
	if p.PublishedThroughSequence == nil {
		if len(p.Records) != 0 || after != nil || !p.Complete {
			return ErrIntegrity
		}
	} else if seq(*p.PublishedThroughSequence) < 1 || previous > seq(*p.PublishedThroughSequence) || p.Complete != (previous == seq(*p.PublishedThroughSequence)) || !p.Complete && len(p.Records) == 0 {
		return ErrIntegrity
	}
	return nil
}
func verifyRecord(r Record, limit int) error {
	if err := validate("ArchiveRecord", r); err != nil {
		return err
	}
	raw, err := canon(r)
	if err != nil {
		return err
	}
	if len(raw) > limit {
		return ErrCapacity
	}
	value, err := canonical.Decode(raw, canonical.Options{MaxBytes: 2 << 20})
	if err != nil {
		return err
	}
	m := value.(map[string]any)
	delete(m, "recordDigest")
	bytes, err := canon(m)
	if err != nil {
		return err
	}
	if domainDigest("tansr.sdk2.record.v1", bytes) != r.RecordDigest || r.SourceEventRange != nil && r.SourceEventRange.FirstSeq > r.SourceEventRange.LastSeq || r.Projection != nil && !validCoverage(r.Projection.Coverage) {
		return ErrIntegrity
	}
	return nil
}

// ReadArtifact assembles bounded chunks, validates each chunk and the full hash.
func (c *Client) ReadArtifact(ctx context.Context, b Binding, ref ArtifactRef) ([]byte, error) {
	if err := validateBinding(b); err != nil {
		return nil, err
	}
	if err := validate("ArtifactRef", ref); err != nil {
		return nil, err
	}
	if ref.SourceID != b.SourceID || ref.Bytes > b.Limits.AttachmentBytes {
		return nil, ErrIntegrity
	}
	out := make([]byte, 0, ref.Bytes)
	for offset := 0; offset < ref.Bytes; {
		maximum := min(b.Limits.ChunkBytes, ref.Bytes-offset)
		query := generationQuery(b.Target.Generations)
		query["offset"] = strconv.Itoa(offset)
		query["maxBytes"] = strconv.Itoa(maximum)
		var chunk ArtifactChunk
		err := c.call(ctx, api.OpArchiveArtifactRead, "ArtifactChunk", api.CallOptions{Params: map[string]string{"id": b.BindingID, "targetId": ref.ArtifactID}, Query: query, MaxResponseBytes: 1 << 20}, 200, &chunk)
		if err != nil {
			return nil, err
		}
		bytes, err := base64.StdEncoding.Strict().DecodeString(chunk.Base64)
		if err != nil || base64.StdEncoding.EncodeToString(bytes) != chunk.Base64 || chunk.BindingID != b.BindingID || chunk.ArtifactID != ref.ArtifactID || chunk.SourceID != b.SourceID || chunk.Generations != b.Target.Generations || chunk.SHA256 != ref.SHA256 || chunk.TotalBytes != ref.Bytes || chunk.Offset != offset || chunk.Bytes <= 0 || chunk.Bytes > maximum || len(bytes) != chunk.Bytes || digest(bytes) != chunk.ChunkSHA256 {
			return nil, ErrIntegrity
		}
		out = append(out, bytes...)
		offset += len(bytes)
	}
	if digest(out) != ref.SHA256 {
		return nil, ErrIntegrity
	}
	return out, nil
}
func (c *Client) Acknowledge(ctx context.Context, ack Ack) (MutationReceipt, error) {
	var value MutationReceipt
	if err := validate("ArchiveAckRequest", ack); err != nil {
		return value, err
	}
	if !validCoverage(ack.Coverage) || seq(ack.Coverage.ThroughSequence)-seq(ack.Coverage.FromSequence) >= 128 {
		return value, ErrIntegrity
	}
	err := c.call(ctx, api.OpArchiveAckCommit, "MutationReceipt", api.CallOptions{Params: map[string]string{"id": ack.BindingID}, Body: ack, IdempotencyKey: ack.Request.RequestID, IfMatch: strconv.Quote(ack.ExpectedRevision)}, 200, &value)
	if err == nil && (value.BindingID != ack.BindingID || value.Request != ack.Request || value.Operation != "archive-ack") {
		err = ErrReceipt
	}
	return value, err
}
func (c *Client) Operation(ctx context.Context, bindingID, operation string, request RequestIdentity) (MutationReceipt, error) {
	var value MutationReceipt
	if err := validate("OperationStatusRequest", map[string]any{"protocol": Protocol, "operation": operation, "bindingId": bindingID, "request": request}); err != nil {
		return value, err
	}
	query := map[string]string{"protocol": Protocol, "operation": operation, "bindingId": bindingID, "operationEpoch": request.OperationEpoch, "requestId": request.RequestID}
	err := c.call(ctx, api.OpArchiveOperationQuery, "MutationReceipt", api.CallOptions{Query: query}, 200, &value)
	if err == nil && (value.Request != request || value.BindingID != bindingID || value.Operation != operation) {
		err = ErrReceipt
	}
	return value, err
}

// CreationOperation queries the original binding-create operation after a lost
// response; it neither creates a new binding nor substitutes a request identity.
func (c *Client) CreationOperation(ctx context.Context, sessionID string, request RequestIdentity) (MutationReceipt, error) {
	var value MutationReceipt
	if err := validate("OperationStatusRequest", map[string]any{"protocol": Protocol, "operation": "binding-create", "sessionId": sessionID, "request": request}); err != nil {
		return value, err
	}
	query := map[string]string{"protocol": Protocol, "operation": "binding-create", "sessionId": sessionID, "operationEpoch": request.OperationEpoch, "requestId": request.RequestID}
	err := c.call(ctx, api.OpArchiveOperationQuery, "MutationReceipt", api.CallOptions{Query: query}, 200, &value)
	if err == nil && (value.Request != request || value.Operation != "binding-create") {
		err = ErrReceipt
	}
	return value, err
}
func verifyReceipt(identity Identity, ack Ack, receipt MutationReceipt) error {
	if err := validate("MutationReceipt", receipt); err != nil {
		return err
	}
	if receipt.State != "completed" || receipt.Operation != "archive-ack" || receipt.BindingID != identity.BindingID || receipt.BindingID != ack.BindingID || receipt.Request != ack.Request || seq(receipt.Revision) <= seq(ack.ExpectedRevision) {
		return ErrReceipt
	}
	bytes, err := canon(ack)
	if err != nil {
		return err
	}
	value, err := canonical.Decode(bytes, canonical.Options{MaxBytes: 2 << 20})
	if err != nil {
		return err
	}
	semantic := value.(map[string]any)
	delete(semantic, "request")
	bytes, err = canon(map[string]any{"scope": []string{identity.ApplicationScopeID, identity.EndUserID}, "operation": "archive-ack", "semantic": semantic})
	if err != nil {
		return err
	}
	if domainDigest("tansr.sdk2.operation.v1", bytes) != receipt.SemanticDigest {
		return ErrReceipt
	}
	return nil
}
