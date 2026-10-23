package terminalpersistence

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/tansrai/tansr-go/canonical"
	"github.com/tansrai/tansr-go/executor"
)

type testCustodyAuthority struct {
	onExecute   func()
	ticket      obj
	prepare     obj
	selected    Custody
	frozen      bool
	claims      map[string]bool
	loseCutover bool
}

func (a *testCustodyAuthority) Execute(_ context.Context, r Request) (Response, error) {
	if a.onExecute != nil {
		a.onExecute()
	}
	out := custodyEnvelope(r)
	switch r["action"] {
	case "query":
		out["preparing"] = a.frozen && a.ticket == nil
		out["ticket"] = detached(a.ticket)
	case "writer-claim", "writer-query", "writer-finish":
		claim := fields(r, "operationId", "intentSha256")
		claim["claimId"], claim["writerFence"] = "synthetic-claim", "1"
		claim["status"], claim["selectedCustody"], claim["responseDigest"] = "active", a.selected, nil
		if r["action"] == "writer-finish" {
			claim["status"] = "settled"
		}
		out["claim"] = claim
	case "verify":
		if a.ticket == nil || !same(r["proof"], a.ticket["proof"]) {
			return nil, ErrIntegrity
		}
		a.ticket["phase"] = "verified"
		out["ticket"] = detached(a.ticket)
	case "cutover":
		if a.ticket["phase"] != "verified" && a.ticket["phase"] != "published" {
			return nil, ErrIntegrity
		}
		a.ticket["phase"] = "published"
		d := m(a.ticket["destination"])
		a.selected = Custody{str(d["custodyId"]), "2", str(d["mode"])}
		a.ticket["selectedCustody"] = a.selected
		a.frozen = false
		out["ticket"] = detached(a.ticket)
		if a.loseCutover {
			a.loseCutover = false
			return nil, ErrUnknown
		}
	default:
		return nil, ErrIntegrity
	}
	return out, nil
}

func TestCustodyAuthorizationRecheckedAfterAuthorityWait(t *testing.T) {
	for _, action := range []string{"query", "writer-claim", "writer-query", "writer-finish"} {
		for _, revoke := range []string{"management", "scope"} {
			t.Run(action+"/"+revoke, func(t *testing.T) {
				f := custodyFixture(t)
				o, owner := fixture(t)
				current, allowed, called := owner.Scope, true, false
				o.CurrentScope = func() (executor.Scope, error) { return current, nil }
				a := &testCustodyAuthority{ticket: m(detached(f["ticket"])), selected: Custody{"source-device", "1", "terminal"}, claims: map[string]bool{}}
				a.onExecute = func() {
					called = true
					if revoke == "scope" {
						current.AuthorizationRevision = "2"
					} else {
						allowed = false
					}
				}
				o.Custody, o.CustodyAuthority = &a.selected, a
				s, err := OpenFileStore(o)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				r := custodyRequest(f, action)
				if action != "query" {
					r["custody"] = a.selected
					r["operationId"] = hv("synthetic-writer-operation")
				}
				if action == "writer-claim" {
					r["expectedWriterFence"] = "1"
				}
				if action == "writer-finish" {
					r["claimId"], r["writerFence"] = "synthetic-claim", "1"
					r["settlement"] = obj{"state": "rolled-back"}
				}
				_, err = s.ExecuteCustody(context.Background(), r, owner, func(Request) error {
					if !allowed {
						return ErrIntegrity
					}
					return nil
				})
				if !called || !errors.Is(err, ErrIntegrity) {
					t.Fatalf("authority wait bypassed revocation: called=%v err=%v", called, err)
				}
				if action == "writer-claim" || action == "writer-finish" {
					if !errors.Is(err, ErrUnknown) {
						t.Fatal("possibly committed authority write lost unknown outcome", err)
					}
				}
			})
		}
	}
}
func (a *testCustodyAuthority) BeginHandoff(_ context.Context, r Request) (HandoffLease, error) {
	if !same(a.selected, r["source"]) {
		return nil, ErrIntegrity
	}
	a.prepare = m(detached(obj(r)))
	a.frozen = true
	return &testHandoff{a}, nil
}

type testHandoff struct{ a *testCustodyAuthority }

func (l *testHandoff) AssertCurrent() error {
	if !l.a.frozen {
		return ErrIntegrity
	}
	return nil
}
func (l *testHandoff) WriterFence() string { return "1" }
func (l *testHandoff) RecordPrepared(_ context.Context, p map[string]any) (map[string]any, error) {
	r := l.a.prepare
	l.a.ticket = obj{"identity": r["identity"], "operationId": r["operationId"], "intentSha256": r["intentSha256"], "phase": "prepared", "source": r["source"], "destination": r["destination"], "writerFence": "1", "proof": p, "selectedCustody": l.a.selected}
	return m(detached(l.a.ticket)), nil
}

type testWriter struct {
	a       *testCustodyAuthority
	custody Custody
	id      string
}

func (l *testWriter) AssertCurrent() error {
	if l.a.frozen || l.a.selected != l.custody {
		return ErrIntegrity
	}
	return nil
}
func (l *testWriter) Finish(_ context.Context, _ map[string]any) error {
	delete(l.a.claims, l.id)
	return nil
}
func (a *testCustodyAuthority) AcquireWriter(_ context.Context, _ Identity, c Custody, r Request) (WriterLease, error) {
	if a.frozen || a.selected != c {
		return nil, ErrIntegrity
	}
	id := hv(r)
	if a.claims[id] {
		return nil, ErrUnknown
	}
	a.claims[id] = true
	return &testWriter{a, c, id}, nil
}
func custodyFixture(t *testing.T) obj {
	t.Helper()
	b, e := os.ReadFile("testdata/custody-consumer.json")
	if e != nil {
		t.Fatal(e)
	}
	v, e := canonical.Decode(b, canonical.Options{MaxBytes: 1 << 20})
	if e != nil {
		t.Fatal(e)
	}
	return m(v)
}
func custodyOwner(t *testing.T, v any) Owner {
	t.Helper()
	b, _ := json.Marshal(v)
	var o Owner
	if e := json.Unmarshal(b, &o); e != nil {
		t.Fatal(e)
	}
	return o
}
func custodyRequest(f obj, action string) Request {
	r := fields(m(f["prepare"]), "contract", "identity", "operationId", "intentSha256")
	r["action"] = action
	return r
}
func testImport(t *testing.T, s *FileStore, f obj, owner Owner) {
	t.Helper()
	ctx := context.Background()
	permit := func(Request) error { return nil }
	for i, p := range arr(f["pages"]) {
		r := custodyRequest(f, "import-page")
		r["proof"], r["page"] = m(f["ticket"])["proof"], p
		if _, e := s.ExecuteCustody(ctx, r, owner, permit); e != nil {
			t.Fatalf("page %d: %v", i, e)
		}
		if _, e := s.ExecuteCustody(ctx, r, owner, permit); e != nil {
			t.Fatalf("same page %d: %v", i, e)
		}
	}
	for i, c := range arr(f["chunks"]) {
		r := custodyRequest(f, "import-object")
		r["chunk"] = c
		if _, e := s.ExecuteCustody(ctx, r, owner, permit); e != nil {
			t.Fatalf("chunk %d: %v", i, e)
		}
	}
	r := custodyRequest(f, "verify")
	r["proof"] = m(f["ticket"])["proof"]
	if _, e := s.ExecuteCustody(ctx, r, owner, permit); e != nil {
		t.Fatalf("verify: %v", e)
	}
}
func TestCustodyNodeExportImportColdRecovery(t *testing.T) {
	f := custodyFixture(t)
	owner := custodyOwner(t, f["owner"])
	o, _ := fixture(t)
	o.CurrentScope = func() (executor.Scope, error) { return owner.Scope, nil }
	authority := &testCustodyAuthority{ticket: m(detached(f["ticket"])), selected: Custody{"source-device", "1", "terminal"}, frozen: true, claims: map[string]bool{}}
	o.Custody = &Custody{"target-device", "1", "terminal"}
	o.CustodyAuthority = authority
	s, e := OpenFileStore(o)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	testImport(t, s, f, owner)
	// A verified copy does not itself grant writes.
	if _, e = s.Execute(context.Background(), m(m(f["pending"])["begin"]), owner); e == nil {
		t.Fatal("unpublished target writer granted")
	}
	r := custodyRequest(f, "cutover")
	r["proof"], r["expectedWriterFence"], r["expectedCustodyRevision"] = m(f["ticket"])["proof"], "1", "1"
	authority.loseCutover = true
	if _, e = s.ExecuteCustody(context.Background(), r, owner, func(Request) error { return nil }); !errors.Is(e, ErrUnknown) {
		t.Fatalf("expected lost cutover, got %v", e)
	}
	q := custodyRequest(f, "query")
	answer, e := s.ExecuteCustody(context.Background(), q, owner, func(Request) error { return nil })
	if e != nil || m(answer["ticket"])["phase"] != "published" {
		t.Fatal("original query", e)
	}
	if _, e = s.ExecuteCustody(context.Background(), r, owner, func(Request) error { return nil }); e != nil {
		t.Fatal("same key activation", e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	o.Mode = "reopen"
	unsafeOptions := o
	unsafeOptions.Custody, unsafeOptions.CustodyAuthority = nil, nil
	if unsafeStore, err := OpenFileStore(unsafeOptions); err == nil {
		unsafeStore.Close()
		t.Fatal("cold open dropped shared writer authority")
	}
	s, e = OpenFileStore(o)
	if e != nil {
		t.Fatal("cold reopen", e)
	}
	defer s.Close()
	for _, name := range []string{"original", "pending"} {
		q := m(m(f[name])["query"])
		out, e := s.Execute(context.Background(), q, owner)
		if e != nil {
			t.Fatalf("%s original key: %v", name, e)
		}
		expected := "committed"
		if name == "pending" {
			expected = "staging"
		}
		if m(out["transfer"])["status"] != expected {
			t.Fatal(name, out)
		}
	}
	for _, v := range arr(f["receipts"]) {
		entry := m(v)
		for _, kind := range []string{"primary", "secondary"} {
			q := req("lookup")
			q["commitRoot"], q["key"] = m(f["root"])["commitRoot"], obj{"kind": kind, "digest": entry[kind+"Key"]}
			got, err := s.Execute(context.Background(), q, owner)
			if err != nil || !same(got["entry"], entry) {
				t.Fatalf("permanent %s receipt mismatch: %v", kind, err)
			}
		}
	}
	// The original encrypted source still has the old epoch after cold open: shared CAS fences it.
	selected := authority.selected
	authority.selected = Custody{"different-writer", "3", "cloud"}
	if _, err := s.Execute(context.Background(), m(m(f["pending"])["begin"]), owner); err == nil {
		t.Fatal("revoked writer accepted")
	}
	authority.selected = selected
	// Replacing a loaded option cannot downgrade the durable selected epoch.
	s.options.Custody = &Custody{"source-device", "1", "terminal"}
	if s.custodySelection() != selected {
		t.Fatal("durable custody epoch downgraded")
	}
	// Re-export after migration preserves the original root and every permanent receipt.
	newReq := m(detached(f["prepare"]))
	newReq["operationId"] = "return-to-cloud"
	newReq["source"] = authority.selected
	newReq["destination"] = obj{"custodyId": "cloud", "mode": "cloud"}
	newReq["expectedWriterFence"] = "1"
	newReq["intentSha256"] = hv(fields(newReq, "identity", "source", "destination", "expectedWriterFence", "expectedRoot"))
	out, e := s.ExecuteCustody(context.Background(), newReq, owner, func(Request) error { return nil })
	if e != nil {
		t.Fatal("re-export", e)
	}
	proof := m(m(out["ticket"])["proof"])
	if proof["indexCount"] != "40" || proof["originalCommitRoot"] != m(f["root"])["commitRoot"] {
		t.Fatal("root changed")
	}
	// Re-export preserves the pending ticket. Releasing this test's handoff allows its new writer
	// to complete the original admitted operation without replacing its key or reservation.
	authority.frozen = false
	for _, put := range arr(m(f["pending"])["puts"]) {
		if _, err := s.Execute(context.Background(), m(put), owner); err != nil {
			t.Fatal("pending put", err)
		}
	}
	if _, err := s.Execute(context.Background(), m(m(f["pending"])["commit"]), owner); err != nil {
		t.Fatal("pending commit", err)
	}
	result, err := s.Execute(context.Background(), m(m(f["pending"])["query"]), owner)
	if err != nil || m(result["transfer"])["status"] != "committed" {
		t.Fatal("pending completion", err)
	}
}
func TestCustodyWrongSourceBudgetAndBrokenPage(t *testing.T) {
	f := custodyFixture(t)
	owner := custodyOwner(t, f["owner"])
	o, _ := fixture(t)
	o.Custody = &Custody{"target-device", "1", "terminal"}
	o.CustodyAuthority = &testCustodyAuthority{ticket: m(detached(f["ticket"])), claims: map[string]bool{}}
	o.MigrationBudget = MigrationBudget{1, 1}
	s, e := OpenFileStore(o)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	r := custodyRequest(f, "import-page")
	r["proof"], r["page"] = m(f["ticket"])["proof"], arr(f["pages"])[0]
	if _, e = s.ExecuteCustody(context.Background(), r, owner, func(Request) error { return nil }); e == nil {
		t.Fatal("budget ignored")
	}
	r["identity"] = obj{"applicationScopeId": "other", "endUserId": "user", "sourceId": "source", "sourceGeneration": "1", "domainKey": "domain"}
	if _, e = s.ExecuteCustody(context.Background(), r, owner, func(Request) error { return nil }); e == nil {
		t.Fatal("wrong identity")
	}
	p := m(detached(arr(f["pages"])[0]))
	p["sha256"] = hash([]byte("bad"))
	if validateCustodyPage(p) == nil {
		t.Fatal("bad page accepted")
	}
}

func TestCustodyWriterRevokedBeforeAtomicReplace(t *testing.T) {
	o, owner := fixture(t)
	a := &testCustodyAuthority{selected: Custody{"device", "1", "terminal"}, claims: map[string]bool{}}
	o.Custody, o.CustodyAuthority = &a.selected, a
	s, err := OpenFileStore(o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.options.CommitHook = func(stage string) error {
		if stage == "before_replace" {
			a.frozen = true
		}
		return nil
	}
	p := plan([]byte("new body"), nil, "revoked", nil, 0)
	if _, err = s.Execute(context.Background(), p.begin, owner); err == nil {
		t.Fatal("write crossed revoked fence")
	}
	s.options.CommitHook = nil
	got, err := s.Execute(context.Background(), query(p), owner)
	if err != nil || m(got["transfer"])["status"] != "unknown" {
		t.Fatal("revoked write was persisted", err)
	}
}
