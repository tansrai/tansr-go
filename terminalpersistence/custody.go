package terminalpersistence

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/tansrai/tansr-go/canonical"
	"github.com/tansrai/tansr-go/internal/wire"
)

const CustodyContract = "persistence-custody-handoff-v1"

type Custody struct {
	CustodyID       string `json:"custodyId"`
	CustodyRevision string `json:"custodyRevision"`
	Mode            string `json:"mode"`
}
type MigrationBudget struct {
	Bytes   int `json:"bytes"`
	Objects int `json:"objects"`
}

func DefaultMigrationBudget() MigrationBudget { return MigrationBudget{1 << 30, 1048576} }

// CustodyAuthority must be explicitly implemented by the developer's shared, authenticated
// CAS control plane. All writers of a domain must use the same authority. No local fallback.
type CustodyAuthority interface {
	Execute(context.Context, Request) (Response, error)
	BeginHandoff(context.Context, Request) (HandoffLease, error)
	AcquireWriter(context.Context, Identity, Custody, Request) (WriterLease, error)
}
type WriterLease interface {
	AssertCurrent() error
	Finish(context.Context, map[string]any) error
}
type HandoffLease interface {
	AssertCurrent() error
	WriterFence() string
	RecordPrepared(context.Context, map[string]any) (map[string]any, error)
}

func custodyValidate(def string, v any) error { return wire.Validate(CustodyContract, def, v) }
func custodyNormalize(def string, v any) (obj, error) {
	b, e := canonical.Encode(v, canonical.Options{MaxBytes: 32768})
	if e != nil {
		return nil, e
	}
	x, e := wire.Decode(CustodyContract, def, b)
	if e != nil {
		return nil, e
	}
	return x.(map[string]any), nil
}
func custodyHash(tag string, v ...any) string { return hv(append([]any{tag}, v...)) }
func custodyOperation(r obj) obj              { return fields(r, "operationId", "intentSha256") }
func custodyEnvelope(r obj) obj {
	return fields(r, "contract", "identity", "operationId", "intentSha256", "action")
}
func (s *FileStore) custodySelection() Custody {
	if v, ok := s.state["custodySelection"]; ok {
		b, _ := json.Marshal(v)
		var c Custody
		_ = json.Unmarshal(b, &c)
		return c
	}
	return *s.options.Custody
}
func (s *FileStore) CustodyHandoffEnabled() bool {
	return s.options.Custody != nil && s.options.CustodyAuthority != nil
}
func (s *FileStore) custodyCheckpoint(ctx context.Context, assert func() error) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if e := assert(); e != nil {
		return e
	}
	return nil
}
func custodyProofBounds(proof obj, b MigrationBudget) error {
	if e := custodyValidate("RootProof", proof); e != nil {
		return e
	}
	if n(proof["bytes"]) > b.Bytes || n(proof["objects"]) > b.Objects {
		return AdapterError("capacity_exceeded")
	}
	return nil
}
func validateCustodyResponse(r, out obj) error {
	if e := custodyValidate("Response", out); e != nil {
		return e
	}
	for _, k := range []string{"contract", "identity", "operationId", "intentSha256", "action"} {
		if !same(r[k], out[k]) {
			return ErrIntegrity
		}
	}
	for _, k := range []string{"ticket", "claim"} {
		if out[k] != nil {
			t := m(out[k])
			if t["operationId"] != r["operationId"] || t["intentSha256"] != r["intentSha256"] {
				return ErrIntegrity
			}
			if k == "ticket" && !same(t["identity"], r["identity"]) {
				return ErrIntegrity
			}
		}
	}
	if r["action"] == "query" && out["preparing"] == true && out["ticket"] != nil {
		return ErrIntegrity
	}
	return nil
}
func (s *FileStore) custodyControl(ctx context.Context, r obj) (obj, error) {
	out, e := s.options.CustodyAuthority.Execute(ctx, r)
	if e != nil {
		return nil, e
	}
	v, e := custodyNormalize("Response", out)
	if e != nil {
		return nil, e
	}
	return v, validateCustodyResponse(r, v)
}
func (s *FileStore) readTicket(ctx context.Context, r obj) (obj, error) {
	q := custodyEnvelope(r)
	q["action"] = "query"
	out, e := s.custodyControl(ctx, q)
	if e != nil {
		return nil, e
	}
	if out["preparing"] == true || out["ticket"] == nil {
		return nil, ErrUnknown
	}
	return m(out["ticket"]), nil
}

// ExecuteCustody implements the approved handoff on the same encrypted durable store.
// Independent management authorization must be rechecked after waits and immediately before commits.
func (s *FileStore) ExecuteCustody(ctx context.Context, input Request, owner Owner, authorize func(Request) error) (out Response, err error) {
	if !s.CustodyHandoffEnabled() || authorize == nil {
		return nil, AdapterError("capability_unavailable")
	}
	r, err := custodyNormalize("Request", input)
	if err != nil {
		return nil, err
	}
	if !same(r["identity"], s.Identity()) || owner.Scope.ApplicationScopeID != s.Identity().ApplicationScopeID || owner.Scope.EndUserID != s.Identity().EndUserID {
		return nil, ErrIntegrity
	}
	check := func() error {
		if e := ctx.Err(); e != nil {
			return e
		}
		c, e := s.current()
		if e != nil {
			return e
		}
		if c != owner.Scope {
			return ErrIntegrity
		}
		return authorize(r)
	}
	if err = check(); err != nil {
		return nil, err
	}
	envelope := custodyEnvelope(r)
	action := str(r["action"])
	switch action {
	case "prepare":
		if hv(fields(r, "identity", "source", "destination", "expectedWriterFence", "expectedRoot")) != r["intentSha256"] {
			return nil, ErrIntegrity
		}
		lease, e := s.options.CustodyAuthority.BeginHandoff(ctx, r)
		if e != nil {
			return nil, e
		}
		assert := func() error {
			if e := check(); e != nil {
				return e
			}
			return lease.AssertCurrent()
		}
		proof, e := s.freezeCustody(ctx, r, lease.WriterFence(), assert)
		if e != nil {
			return nil, e
		}
		if e = assert(); e != nil {
			return nil, e
		}
		ticket, e := lease.RecordPrepared(ctx, proof)
		if e != nil {
			return nil, e
		}
		if e = assert(); e != nil {
			return nil, e
		}
		envelope["ticket"] = ticket
	case "query", "writer-claim", "writer-query", "writer-finish":
		response, e := s.custodyControl(ctx, r)
		if e != nil {
			return nil, e
		}
		if e = check(); e != nil {
			if action == "writer-claim" || action == "writer-finish" {
				return nil, errors.Join(ErrUnknown, e)
			}
			return nil, e
		}
		return response, nil
	default:
		ticket, e := s.readTicket(ctx, r)
		if e != nil {
			return nil, e
		}
		if e = check(); e != nil {
			return nil, e
		}
		switch action {
		case "export-page":
			p, c, e := s.exportCustodyPage(ctx, r, check)
			if e != nil {
				return nil, e
			}
			envelope["proof"] = ticket["proof"]
			envelope["page"] = p
			envelope["nextCursor"] = c
		case "export-object":
			chunk, e := s.exportCustodyObject(ctx, r, check)
			if e != nil {
				return nil, e
			}
			envelope["chunk"] = chunk
		case "import-page":
			if !same(r["proof"], ticket["proof"]) {
				return nil, ErrIntegrity
			}
			if e = s.importCustodyPage(ctx, ticket, m(r["page"]), check); e != nil {
				return nil, e
			}
			envelope["ticket"] = ticket
			envelope["pageDigest"] = m(r["page"])["sha256"]
		case "import-object":
			if e = s.importCustodyObject(ctx, ticket, m(r["chunk"]), check); e != nil {
				return nil, e
			}
			c := m(r["chunk"])
			envelope["ticket"] = ticket
			envelope["object"] = c["object"]
			envelope["nextOffset"] = n(c["offset"]) + n(c["byteLength"])
		case "verify", "cutover":
			if !same(r["proof"], ticket["proof"]) {
				return nil, ErrIntegrity
			}
			if e = s.verifyCustody(ctx, ticket, check); e != nil {
				return nil, e
			}
			if action == "cutover" && (r["expectedWriterFence"] != ticket["writerFence"] || r["expectedCustodyRevision"] != m(ticket["source"])["custodyRevision"]) {
				return nil, ErrIntegrity
			}
			response, e := s.custodyControl(ctx, r)
			if e != nil {
				return nil, e
			}
			if e = check(); e != nil {
				return nil, e
			}
			if action == "cutover" {
				if e = s.activateCustody(ctx, m(response["ticket"]), check); e != nil {
					return nil, e
				}
			}
			return response, nil
		case "abort":
			if r["expectedWriterFence"] != ticket["writerFence"] || r["expectedCustodyRevision"] != m(ticket["source"])["custodyRevision"] {
				return nil, ErrIntegrity
			}
			response, e := s.custodyControl(ctx, r)
			if e != nil {
				return nil, e
			}
			if e = s.abortCustody(ctx, m(response["ticket"]), check); e != nil {
				return nil, e
			}
			return response, nil
		default:
			return nil, ErrIntegrity
		}
	}
	if err = check(); err != nil {
		return nil, err
	}
	err = validateCustodyResponse(r, envelope)
	return envelope, err
}
func validateCustodyPage(page obj) error {
	if e := custodyValidate("Page", page); e != nil {
		return e
	}
	i, e := strconv.ParseInt(str(page["index"]), 10, 64)
	if e != nil {
		return e
	}
	d := str(page["previousDigest"])
	for _, r := range arr(page["records"]) {
		d = custodyHash("PST-CUSTODY-RECORD", strconv.FormatInt(i, 10), d, r)
		i++
	}
	if d != page["sha256"] {
		return ErrIntegrity
	}
	return nil
}
func validateCustodyChunk(c obj) ([]byte, error) {
	if e := custodyValidate("ObjectChunk", c); e != nil {
		return nil, e
	}
	b, e := base64.StdEncoding.Strict().DecodeString(str(c["base64"]))
	if e != nil || base64.StdEncoding.EncodeToString(b) != c["base64"] || len(b) != n(c["byteLength"]) || hash(b) != c["payloadDigest"] || n(c["offset"])+len(b) > n(m(c["object"])["byteLength"]) {
		return nil, ErrIntegrity
	}
	return b, nil
}
func (s *FileStore) beginWriter(ctx context.Context, r obj) (WriterLease, error) {
	if !s.CustodyHandoffEnabled() {
		return nil, nil
	}
	switch r["action"] {
	case "head", "read", "lookup", "query":
		return nil, nil
	}
	lease, e := s.options.CustodyAuthority.AcquireWriter(ctx, s.Identity(), s.custodySelection(), r)
	if e != nil {
		return nil, e
	}
	if lease == nil {
		return nil, ErrIntegrity
	}
	if e = lease.AssertCurrent(); e != nil {
		return nil, e
	}
	return lease, nil
}
func (s *FileStore) finishWriter(ctx context.Context, lease WriterLease, commits uint64, result Response, executionErr error) error {
	if lease == nil {
		return executionErr
	}
	if s.uncertain || errors.Is(executionErr, ErrUnknown) {
		return executionErr
	}
	settlement := obj{"state": "rolled-back"}
	if executionErr == nil {
		settlement = obj{"state": "committed", "responseDigest": hv(result)}
	} else if s.commits != commits {
		return errors.Join(ErrUnknown, executionErr)
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if e := lease.Finish(finishCtx, settlement); e != nil {
		return errors.Join(ErrUnknown, executionErr, e)
	}
	return executionErr
}
