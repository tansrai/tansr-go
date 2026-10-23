package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"

	"github.com/tansrai/tansr-go/api"
)

// ClaimUnavailable is not a local pending claim. Runner must not execute or persist unknown
// on behalf of another owner (including an earlier process that lost its claim token).
type ClaimUnavailable struct{ Disposition string }

func (e *ClaimUnavailable) Error() string { return "executor: cloud journal claim " + e.Disposition }

type JournalRecord struct {
	Operation    Operation `json:"operation"`
	AttemptID    string    `json:"attemptId"`
	ConnectionID string    `json:"connectionId"`
	Status       string    `json:"status"`
	Receipt      *Receipt  `json:"receipt"`
}
type JournalPage struct {
	Protocol   string          `json:"protocol"`
	Records    []JournalRecord `json:"records"`
	NextCursor *string         `json:"nextCursor"`
}
type cloudClaim struct {
	operation      Operation
	connection     Connection
	attempt, token string
	granted        bool
}

// CloudJournal implements the existing Journal without local files. Only a first granted
// claim owns execution. Tokens live in RAM; process restart can reconcile but cannot regain them.
type CloudJournal struct {
	client     *Client
	sessionID  string
	connection func() (Connection, error)
	mu         sync.Mutex
	claims     map[string]*cloudClaim
	closed     bool
}

func NewCloudJournal(c *Client, sessionID string, connection func() (Connection, error)) (*CloudJournal, error) {
	if c == nil || connection == nil || validate("LegacyId", sessionID) != nil {
		return nil, ErrInvalid
	}
	return &CloudJournal{client: c, sessionID: sessionID, connection: connection, claims: map[string]*cloudClaim{}}, nil
}
func (j *CloudJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.closed = true
	clear(j.claims)
	return nil
}
func (j *CloudJournal) conn() (Connection, error) {
	j.mu.Lock()
	closed := j.closed
	j.mu.Unlock()
	if closed {
		return Connection{}, ErrInvalid
	}
	c, e := j.connection()
	if e == nil {
		e = validate("ExecutorConnection", c)
	}
	return c, e
}
func (j *CloudJournal) verify(op Operation, c Connection, current bool) error {
	if e := validateOperation(op); e != nil {
		return e
	}
	s := j.client.Scope()
	t := op.Binding.Target
	if op.SessionID != j.sessionID || op.Scope.ApplicationScopeID != s.ApplicationScopeID || op.Scope.EndUserID != s.EndUserID || t.ExecutorID != c.ExecutorID {
		return ErrConflict
	}
	if current && (op.Scope != s || t.ConnectionID != c.ConnectionID || t.ConnectionRevision != c.ConnectionRevision) {
		return ErrConflict
	}
	return nil
}
func (j *CloudJournal) record(r JournalRecord, c Connection, expected *Operation) error {
	if e := validate("ExecutionJournalRecord", r); e != nil {
		return e
	}
	if e := j.verify(r.Operation, c, false); e != nil {
		return e
	}
	if expected != nil && !equal(*expected, r.Operation) || r.ConnectionID != r.Operation.Binding.Target.ConnectionID {
		return ErrConflict
	}
	if r.Receipt != nil {
		if r.Status != r.Receipt.Status {
			return ErrConflict
		}
		return validateReceipt(r.Operation, *r.Receipt)
	}
	if r.Status != "pending" && r.Status != "unknown" {
		return ErrConflict
	}
	return nil
}
func sameConnection(a, b Connection) bool {
	return a.ExecutorID == b.ExecutorID && a.ConnectionID == b.ConnectionID && a.ConnectionRevision == b.ConnectionRevision
}
func claimBody(c *cloudClaim) map[string]any {
	return map[string]any{"protocol": Protocol, "executorId": c.connection.ExecutorID, "connectionId": c.connection.ConnectionID, "operationId": c.operation.OperationID, "digest": c.operation.Digest, "attemptId": c.attempt}
}
func (j *CloudJournal) Claim(ctx context.Context, op Operation) (ClaimResult, error) {
	c, e := j.conn()
	if e != nil {
		return ClaimResult{}, e
	}
	if e = j.verify(op, c, true); e != nil {
		return ClaimResult{}, e
	}
	random := make([]byte, 16)
	if _, e = rand.Read(random); e != nil {
		return ClaimResult{}, e
	}
	claim := &cloudClaim{operation: clone(op), connection: c, attempt: hex.EncodeToString(random)}
	j.mu.Lock()
	if j.closed || len(j.claims) >= 128 || j.claims[op.OperationID] != nil {
		j.mu.Unlock()
		return ClaimResult{}, &ClaimUnavailable{"unconfirmed_or_active"}
	}
	j.claims[op.OperationID] = claim
	j.mu.Unlock()
	var out struct {
		Protocol    string        `json:"protocol"`
		Disposition string        `json:"disposition"`
		Record      JournalRecord `json:"record"`
		ClaimToken  *string       `json:"claimToken,omitempty"`
	}
	// Never attach Idempotency-Key: a cached granted response would replay execution permission.
	e = j.client.call(ctx, api.OpExecutionJournalClaim, "ExecutionJournalClaimResponse", api.CallOptions{Params: map[string]string{"id": c.ExecutorID}, Body: claimBody(claim)}, &out)
	if e != nil {
		return ClaimResult{}, e
	}
	if e = j.record(out.Record, c, &op); e != nil {
		return ClaimResult{}, e
	}
	if out.Disposition == "completed" {
		if out.Record.Receipt == nil {
			return ClaimResult{}, ErrConflict
		}
		j.mu.Lock()
		delete(j.claims, op.OperationID)
		j.mu.Unlock()
		return ClaimResult{Receipt: out.Record.Receipt}, nil
	}
	if out.Disposition != "granted" {
		return ClaimResult{}, &ClaimUnavailable{out.Disposition}
	}
	if out.ClaimToken == nil || out.Record.AttemptID != claim.attempt || out.Record.ConnectionID != c.ConnectionID || out.Record.Status != "pending" || out.Record.Receipt != nil {
		return ClaimResult{}, ErrConflict
	}
	fresh, e := j.conn()
	if e != nil {
		return ClaimResult{}, e
	}
	if !sameConnection(c, fresh) {
		return ClaimResult{}, ErrConflict
	}
	if e = j.verify(op, fresh, true); e != nil {
		return ClaimResult{}, e
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return ClaimResult{}, ErrInvalid
	}
	claim.token = *out.ClaimToken
	claim.granted = true
	return ClaimResult{Claimed: true}, nil
}
func (j *CloudJournal) Read(ctx context.Context, op Operation) (*JournalRecord, error) {
	c, e := j.conn()
	if e != nil {
		return nil, e
	}
	if e = j.verify(op, c, false); e != nil {
		return nil, e
	}
	var out struct {
		Protocol string         `json:"protocol"`
		Record   *JournalRecord `json:"record"`
	}
	e = j.client.call(ctx, api.OpExecutionJournalRead, "ExecutionJournalReadResponse", api.CallOptions{Params: map[string]string{"id": c.ExecutorID, "targetId": op.OperationID}, Query: map[string]string{"protocol": Protocol, "connectionId": c.ConnectionID}}, &out)
	if e != nil {
		return nil, e
	}
	if out.Record != nil {
		if e = j.record(*out.Record, c, &op); e != nil {
			return nil, e
		}
		if out.Record.Receipt != nil {
			j.mu.Lock()
			local := j.claims[op.OperationID]
			if local != nil && !equal(local.operation, op) {
				j.mu.Unlock()
				return nil, ErrConflict
			}
			delete(j.claims, op.OperationID)
			j.mu.Unlock()
		}
	}
	return out.Record, nil
}
func (j *CloudJournal) Complete(ctx context.Context, op Operation, receipt Receipt) error {
	if e := validateReceipt(op, receipt); e != nil {
		return e
	}
	c, e := j.conn()
	if e != nil {
		return e
	}
	if e = j.verify(op, c, false); e != nil {
		return e
	}
	j.mu.Lock()
	original := j.claims[op.OperationID]
	var claim cloudClaim
	if original != nil {
		claim = *original
	}
	j.mu.Unlock()
	if original == nil || !claim.granted {
		stored, e := j.Read(ctx, op)
		if e != nil {
			return e
		}
		if stored != nil && stored.Receipt != nil && equal(*stored.Receipt, receipt) {
			return nil
		}
		return &ClaimUnavailable{"not_owner"}
	}
	if !equal(claim.operation, op) || !sameConnection(claim.connection, c) {
		return ErrConflict
	}
	body := claimBody(&claim)
	body["claimToken"] = claim.token
	body["receipt"] = receipt
	var out struct {
		Protocol string         `json:"protocol"`
		Record   *JournalRecord `json:"record"`
	}
	e = j.client.call(ctx, api.OpExecutionJournalComplete, "ExecutionJournalReadResponse", api.CallOptions{Params: map[string]string{"id": c.ExecutorID}, Body: body}, &out)
	if e != nil {
		return e
	}
	if out.Record == nil {
		return ErrConflict
	}
	if e = j.record(*out.Record, c, &op); e != nil {
		return e
	}
	if out.Record.AttemptID != claim.attempt || out.Record.Receipt == nil || !equal(*out.Record.Receipt, receipt) {
		return ErrConflict
	}
	j.mu.Lock()
	delete(j.claims, op.OperationID)
	j.mu.Unlock()
	return nil
}
func (j *CloudJournal) List(ctx context.Context, cursor string, limit int) (JournalPage, error) {
	var out JournalPage
	if limit < 1 || limit > 16 {
		return out, fmt.Errorf("executor: journal page limit must be 1..16")
	}
	c, e := j.conn()
	if e != nil {
		return out, e
	}
	q := map[string]string{"protocol": Protocol, "connectionId": c.ConnectionID, "sessionId": j.sessionID, "limit": strconv.Itoa(limit)}
	if cursor != "" {
		if e = validate("Id", cursor); e != nil {
			return out, e
		}
		q["cursor"] = cursor
	}
	e = j.client.call(ctx, api.OpExecutionJournalList, "ExecutionJournalListResponse", api.CallOptions{Params: map[string]string{"id": c.ExecutorID}, Query: q}, &out)
	if e != nil {
		return out, e
	}
	previous := cursor
	for _, r := range out.Records {
		if e = j.record(r, c, nil); e != nil {
			return out, e
		}
		if r.Operation.OperationID <= previous {
			return out, ErrConflict
		}
		previous = r.Operation.OperationID
	}
	if len(out.Records) > limit || out.NextCursor != nil && (len(out.Records) == 0 || *out.NextCursor != previous) {
		return out, ErrConflict
	}
	return out, nil
}
