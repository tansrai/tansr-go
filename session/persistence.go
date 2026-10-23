package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tansrai/tansr-go/api"
	"github.com/tansrai/tansr-go/canonical"
	"github.com/tansrai/tansr-go/internal/wire"
)

const PersistenceContract = "serve-persistence-v1"

// PersistenceView is a trusted deployment fact, never a client choice or authority.
type PersistenceView struct {
	Contract         string             `json:"contract"`
	SessionID        string             `json:"sessionId"`
	Mode             string             `json:"mode"`
	SourceID         string             `json:"sourceId"`
	SourceGeneration string             `json:"sourceGeneration"`
	Coverage         []string           `json:"coverage"`
	Handoff          PersistenceHandoff `json:"handoff"`
	LocalDisk        bool               `json:"localDisk"`
}
type PersistenceHandoff struct {
	Archive          string `json:"archive"`
	TrustedControl   bool   `json:"trustedControl"`
	ExecutionJournal bool   `json:"executionJournal"`
	InputReceipts    bool   `json:"inputReceipts"`
}
type InputReceipt struct {
	SessionID  string `json:"sessionId"`
	RequestID  string `json:"requestId"`
	Digest     string `json:"digest"`
	Generation string `json:"generation"`
	State      string `json:"state"`
	AcceptedAt string `json:"acceptedAt"`
}
type InputReceiptPage struct {
	Contract   string         `json:"contract"`
	SessionID  string         `json:"sessionId"`
	Receipts   []InputReceipt `json:"receipts"`
	NextCursor *string        `json:"nextCursor"`
}

func messageBody(key string, value any, requestID string) map[string]any {
	b := map[string]any{key: value}
	if requestID != "" {
		b["requestId"] = requestID
	}
	return b
}
func strictPersistence(raw []byte, out any, keys ...string) error {
	// These SDK1 HTTP projections are strict JSON, not the SDK2 canonical-byte envelope.
	v, err := canonical.Decode(raw, canonical.Options{MaxBytes: 262144})
	if err != nil {
		return err
	}
	m, ok := v.(map[string]any)
	if !ok || len(m) != len(keys) {
		return fmt.Errorf("session: invalid persistence shape")
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return fmt.Errorf("session: missing %s", k)
		}
	}
	if handoff, present := m["handoff"]; present {
		h, ok := handoff.(map[string]any)
		if !ok || len(h) != 4 {
			return fmt.Errorf("session: invalid handoff shape")
		}
		for _, k := range []string{"archive", "trustedControl", "executionJournal", "inputReceipts"} {
			if _, ok := h[k]; !ok {
				return fmt.Errorf("session: missing handoff %s", k)
			}
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(out); err != nil {
		return err
	}
	return nil
}
func persistenceSource(s string) bool {
	if len(s) == 0 || len(s) > 1024 || !utf8.ValidString(s) || strings.TrimSpace(s) == "" {
		return false
	}
	for _, r := range s {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}
func (s *Session) Persistence(ctx context.Context) (PersistenceView, error) {
	var v PersistenceView
	r, e := s.client.transport.Call(ctx, api.OpSessionPersistenceRead, api.CallOptions{Params: map[string]string{"id": s.ID()}})
	if e != nil {
		return v, e
	}
	if r.Status != 200 {
		return v, fmt.Errorf("session: unexpected persistence status")
	}
	if e = strictPersistence(r.Body, &v, "contract", "sessionId", "mode", "sourceId", "sourceGeneration", "coverage", "handoff", "localDisk"); e != nil {
		return v, e
	}
	if v.Contract != PersistenceContract || v.SessionID != s.ID() || v.LocalDisk || !persistenceSource(v.SourceID) || !persistenceSource(v.SourceGeneration) || v.Coverage == nil || len(v.Coverage) > 8 {
		return v, fmt.Errorf("session: invalid persistence identity")
	}
	expected := "terminal-durable-ack"
	if v.Mode == "cloud" {
		expected = "cloud-immutable-publish"
	} else if v.Mode != "terminal" {
		return v, fmt.Errorf("session: invalid persistence mode")
	}
	if v.Handoff.Archive != expected || v.Mode != "cloud" && v.Handoff.ExecutionJournal {
		return v, fmt.Errorf("session: invalid persistence handoff")
	}
	seen := map[string]bool{}
	for _, d := range v.Coverage {
		if seen[d] || !strings.Contains("|history|memory|checkpoints|executions|input-receipts|artifacts|events|control|", "|"+d+"|") {
			return v, fmt.Errorf("session: invalid coverage")
		}
		seen[d] = true
	}
	return v, nil
}
func checkInputReceipt(r InputReceipt, id, key string) error {
	if r.SessionID != id || key != "" && r.RequestID != key || wire.Validate("sdk2-ext-v1", "Id", r.RequestID) != nil || wire.Validate("sdk2-ext-v1", "Digest", r.Digest) != nil || !persistenceSource(r.Generation) || (r.State != "accepted" && r.State != "unknown") {
		return fmt.Errorf("session: invalid input receipt")
	}
	_, e := time.Parse(time.RFC3339Nano, r.AcceptedAt)
	return e
}

// QueryInput returns nil for not found; neither nil nor unknown authorizes a replacement send.
func (s *Session) QueryInput(ctx context.Context, requestID string) (*InputReceipt, error) {
	if e := wire.Validate("sdk2-ext-v1", "Id", requestID); e != nil {
		return nil, e
	}
	r, e := s.client.transport.Call(ctx, api.OpSessionInputQuery, api.CallOptions{Params: map[string]string{"id": s.ID(), "targetId": requestID}})
	if e != nil {
		return nil, e
	}
	var v struct {
		Contract  string        `json:"contract"`
		SessionID string        `json:"sessionId"`
		Receipt   *InputReceipt `json:"receipt"`
	}
	if r.Status != 200 {
		return nil, fmt.Errorf("session: unexpected receipt status")
	}
	if e = strictPersistence(r.Body, &v, "contract", "sessionId", "receipt"); e != nil {
		return nil, e
	}
	if v.Contract != PersistenceContract || v.SessionID != s.ID() {
		return nil, fmt.Errorf("session: wrong receipt source")
	}
	if v.Receipt != nil {
		e = checkInputReceipt(*v.Receipt, s.ID(), requestID)
	}
	return v.Receipt, e
}
func (s *Session) ListInputReceipts(ctx context.Context, cursor string, limit int) (InputReceiptPage, error) {
	var v InputReceiptPage
	if limit < 1 || limit > 100 {
		return v, fmt.Errorf("session: invalid page limit")
	}
	q := map[string]string{"limit": strconv.Itoa(limit)}
	if cursor != "" {
		if e := wire.Validate("sdk2-ext-v1", "Id", cursor); e != nil {
			return v, e
		}
		q["cursor"] = cursor
	}
	r, e := s.client.transport.Call(ctx, api.OpSessionInputList, api.CallOptions{Params: map[string]string{"id": s.ID()}, Query: q})
	if e != nil {
		return v, e
	}
	if r.Status != 200 {
		return v, io.ErrUnexpectedEOF
	}
	if e = strictPersistence(r.Body, &v, "contract", "sessionId", "receipts", "nextCursor"); e != nil {
		return v, e
	}
	if v.Contract != PersistenceContract || v.SessionID != s.ID() || v.Receipts == nil || len(v.Receipts) > limit {
		return v, fmt.Errorf("session: invalid receipt page")
	}
	prev := cursor
	for _, r := range v.Receipts {
		if e = checkInputReceipt(r, s.ID(), ""); e != nil {
			return v, e
		}
		if r.RequestID <= prev {
			return v, fmt.Errorf("session: unordered receipt page")
		}
		prev = r.RequestID
	}
	if v.NextCursor != nil && (len(v.Receipts) == 0 || *v.NextCursor != prev) {
		return v, fmt.Errorf("session: invalid receipt cursor")
	}
	return v, nil
}
