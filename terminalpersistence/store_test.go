package terminalpersistence

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/tansrai/tansr-go/contract"
	"github.com/tansrai/tansr-go/executor"
	"github.com/tansrai/tansr-go/internal/wire"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
)

func fixture(t *testing.T) (Options, Owner) {
	t.Helper()
	o := Owner{Scope: executor.Scope{ApplicationScopeID: "app", EndUserID: "user", AuthorizationRevision: "1"}, SessionID: "session", Binding: executor.Binding{BindingID: "binding", Revision: "1", Target: executor.Target{ExecutorID: "device", ConnectionID: "connection", ConnectionRevision: "1", WorkspaceID: "workspace", WorkspaceRevision: "1"}}}
	directory, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return Options{Path: filepath.Join(directory, "new.bin"), Mode: "create", Key: bytes.Repeat([]byte{9}, 32), Identity: Identity{ApplicationScopeID: "app", EndUserID: "user", SourceID: "source", SourceGeneration: "1", DomainKey: "domain"}, CurrentScope: func() (executor.Scope, error) { return o.Scope, nil }}, o
}
func req(action string) Request {
	return Request{"contract": Contract, "action": action, "sourceId": "source", "sourceGeneration": "1", "domainKey": "domain"}
}

type planned struct {
	begin  Request
	puts   []Request
	commit Request
}
type valueEntry struct {
	primary, secondary string
	raw                []byte
}

func plan(body []byte, entries []valueEntry, id string, root any, added int) planned {
	objects := map[string][]byte{}
	kinds := map[string]string{}
	object := func(kind string, raw []byte) obj {
		sha := hash(raw)
		k := key(kind, sha)
		objects[k] = raw
		kinds[k] = kind
		return obj{"sha256": sha, "byteLength": len(raw)}
	}
	refs := []any{}
	for at := 0; at < len(body); at += MaxObject {
		refs = append(refs, object("body-block", body[at:min(len(body), at+MaxObject)]))
	}
	pages := []any{}
	for at := 0; at < len(refs); at += 64 {
		pages = append(pages, object("body-page", enc(obj{"version": 1, "kind": "body-page", "index": at / 64, "refs": refs[at:min(len(refs), at+64)]}))["sha256"])
	}
	items := []any{}
	for _, v := range entries {
		items = append(items, obj{"primaryKey": v.primary, "secondaryKey": v.secondary, "value": object("receipt-value", v.raw)})
	}
	sort.Slice(items, func(i, j int) bool { return str(m(items[i])["primaryKey"]) < str(m(items[j])["primaryKey"]) })
	indexPages := []any{}
	for at := 0; at < len(items); at += 32 {
		indexPages = append(indexPages, object("index-page", enc(obj{"version": 1, "kind": "index-page", "index": at / 32, "entries": items[at:min(len(items), at+32)]}))["sha256"])
	}
	total := 0
	for _, raw := range objects {
		total += len(raw)
	}
	begin := req("begin")
	begin["transferId"] = id
	begin["expected"] = expected(root)
	begin["body"] = obj{"byteLength": len(body), "sha256": hash(body), "blockCount": len(refs), "pageHashes": pages}
	begin["index"] = obj{"entryCount": len(items), "addedCount": added, "pageHashes": indexPages}
	begin["declared"] = obj{"objects": len(objects), "bytes": total}
	begin["intentSha256"] = hv(begin)
	keys := []string{}
	for k := range objects {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := kinds[keys[i]], kinds[keys[j]]
		ap, bp := a == "body-page" || a == "index-page", b == "body-page" || b == "index-page"
		if ap != bp {
			return ap
		}
		return keys[i] < keys[j]
	})
	puts := []Request{}
	for _, k := range keys {
		raw := objects[k]
		put := req("put")
		put["transferId"] = id
		put["intentSha256"] = begin["intentSha256"]
		put["kind"] = kinds[k]
		put["sha256"] = hash(raw)
		put["byteLength"] = len(raw)
		put["base64"] = base64.StdEncoding.EncodeToString(raw)
		puts = append(puts, put)
	}
	commit := req("commit")
	commit["transferId"] = id
	commit["intentSha256"] = begin["intentSha256"]
	return planned{begin, puts, commit}
}
func execute(t *testing.T, s *FileStore, r Request, o Owner) Response {
	t.Helper()
	v, e := s.Execute(context.Background(), r, o)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func publish(t *testing.T, s *FileStore, p planned, o Owner) any {
	t.Helper()
	execute(t, s, p.begin, o)
	for _, put := range p.puts {
		execute(t, s, put, o)
	}
	return m(execute(t, s, p.commit, o)["transfer"])["result"]
}
func query(p planned) Request {
	q := Request(detached(map[string]any(p.commit)).(map[string]any))
	q["action"] = "query"
	return q
}
func open(t *testing.T, o Options) *FileStore {
	t.Helper()
	s, e := OpenFileStore(o)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func TestFrozenVectors(t *testing.T) {
	raw, e := os.ReadFile("../contract/terminal-persistence-v1.golden.json")
	if e != nil {
		t.Fatal(e)
	}
	var g struct {
		SchemaSHA          string `json:"schemaSha256"`
		Positive, Negative []struct {
			ID, Definition string
			Value          any
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if e = d.Decode(&g); e != nil {
		t.Fatal(e)
	}
	schema, _ := contract.ReadSchema(Contract)
	if hash(schema) != g.SchemaSHA || len(g.Positive) != 38 || len(g.Negative) != 32 {
		t.Fatal("frozen vector drift")
	}
	for _, v := range g.Positive {
		if e = wire.Validate(Contract, v.Definition, v.Value); e != nil {
			t.Fatalf("%s: %v", v.ID, e)
		}
	}
	for _, v := range g.Negative {
		if e = wire.Validate(Contract, v.Definition, v.Value); e == nil {
			t.Fatalf("%s accepted", v.ID)
		}
	}
}
func TestDurableRootIndexReopenAndHistoricalQuery(t *testing.T) {
	opts, o := fixture(t)
	s := open(t, opts)
	first := plan([]byte{0xff, 0, 0xfe}, []valueEntry{{hash([]byte("primary")), hash([]byte("secondary")), []byte("private receipt")}}, "first", nil, 1)
	root := publish(t, s, first, o)
	for _, kind := range []string{"primary", "secondary"} {
		r := req("lookup")
		r["commitRoot"] = m(root)["commitRoot"]
		r["key"] = obj{"kind": kind, "digest": hash([]byte(kind))}
		if m(execute(t, s, r, o)["entry"])["base64"] != base64.StdEncoding.EncodeToString([]byte("private receipt")) {
			t.Fatal("lookup")
		}
	}
	s.Close()
	opts.Mode = "reopen"
	s = open(t, opts)
	newRoot := publish(t, s, plan([]byte("new"), nil, "next", root, 0), o)
	if same(newRoot, root) || !same(m(execute(t, s, query(first), o)["transfer"])["result"], root) {
		t.Fatal("original result changed")
	}
	if len(m(s.state["objects"])) != 3 {
		t.Fatal("unreferenced body/page retained")
	}
	raw, e := os.ReadFile(opts.Path)
	if e != nil || bytes.Contains(raw, []byte("private receipt")) {
		t.Fatal("unencrypted medium", e)
	}
}
func TestUnknownCommitOriginalKey(t *testing.T) {
	for _, stage := range []string{"before_replace", "replaced", "directory_synced"} {
		t.Run(stage, func(t *testing.T) {
			opts, o := fixture(t)
			s := open(t, opts)
			p := plan([]byte("body"), nil, "first", nil, 0)
			execute(t, s, p.begin, o)
			for _, put := range p.puts {
				execute(t, s, put, o)
			}
			s.options.CommitHook = func(current string) error {
				if current == stage {
					return errors.New("injected")
				}
				return nil
			}
			if _, e := s.Execute(context.Background(), p.commit, o); !errors.Is(e, ErrUnknown) {
				t.Fatal(e)
			}
			if _, e := s.Execute(context.Background(), req("head"), o); !errors.Is(e, ErrUnknown) {
				t.Fatal(e)
			}
			s.Close()
			opts.Mode = "reopen"
			s = open(t, opts)
			status := m(execute(t, s, query(p), o)["transfer"])["status"]
			want := "committed"
			if stage == "before_replace" {
				want = "staging"
			}
			if status != want || len(m(s.state["transfers"])) != 1 {
				t.Fatal(status)
			}
		})
	}
}
func TestProtectedBaseAndCASReject(t *testing.T) {
	opts, o := fixture(t)
	s := open(t, opts)
	root := publish(t, s, plan([]byte("old"), nil, "initial", nil, 0), o)
	waiting := plan([]byte("old"), nil, "waiting", root, 0)
	execute(t, s, waiting.begin, o)
	publish(t, s, plan([]byte("new"), nil, "winner", root, 0), o)
	if len(m(s.state["objects"])) != 4 {
		t.Fatal("active base collected")
	}
	if _, e := s.Execute(context.Background(), waiting.commit, o); e != AdapterError("revision_conflict") {
		t.Fatal(e)
	}
	if len(m(s.state["objects"])) != 2 || m(execute(t, s, query(waiting), o)["transfer"])["status"] != "rejected" {
		t.Fatal("terminal reclamation")
	}
}
func TestOwnerQueryOnlyAndPostCommitRevocation(t *testing.T) {
	testOwnerQueryOnlyAndPostCommitRevocation(t, open)
}
func TestIndexedOwnerQueryOnlyAndPostCommitRevocation(t *testing.T) {
	testOwnerQueryOnlyAndPostCommitRevocation(t, indexedOpen)
}
func testOwnerQueryOnlyAndPostCommitRevocation(t *testing.T, open func(*testing.T, Options) *FileStore) {
	opts, o := fixture(t)
	revoked := false
	opts.CurrentScope = func() (executor.Scope, error) {
		if revoked {
			return o.Scope, errors.New("revoked")
		}
		return o.Scope, nil
	}
	opts.AuthorizeRecovery = func(_ Identity, _ string, previous, current Owner) bool { return previous.Scope == current.Scope }
	s := open(t, opts)
	p := plan([]byte("body"), nil, "transfer", nil, 0)
	execute(t, s, p.begin, o)
	other := o
	other.Binding.Target.ConnectionRevision = "2"
	execute(t, s, query(p), other)
	if _, e := s.Execute(context.Background(), p.commit, other); e != AdapterError("request_conflict") {
		t.Fatal(e)
	}
	for _, put := range p.puts {
		execute(t, s, put, o)
	}
	s.options.CommitHook = func(stage string) error {
		if stage == "replaced" {
			revoked = true
		}
		return nil
	}
	if _, e := s.Execute(context.Background(), p.commit, o); !errors.Is(e, ErrUnknown) {
		t.Fatal(e)
	}
	s.Close()
	revoked = false
	opts.Mode = "reopen"
	s = open(t, opts)
	if m(execute(t, s, query(p), o)["transfer"])["status"] != "committed" {
		t.Fatal("committed fact lost")
	}
}
func Test513GenerationsPreserveIndexAndCollectObjects(t *testing.T) {
	opts, o := fixture(t)
	state := initial(opts.Identity, DefaultLimits())
	state["writes"] = 1
	state["encryptedBytes"] = 1
	own, _ := ownerObject(o)
	var root any
	for i := 0; i < 513; i++ {
		v := strconv.Itoa(i)
		p := plan([]byte("body"+v), []valueEntry{{hash([]byte(v)), hash([]byte("secondary" + v)), []byte(v)}}, v, root, 1)
		requests := append([]Request{p.begin}, p.puts...)
		requests = append(requests, p.commit)
		for _, r := range requests {
			nr, e := normalize(r)
			if e != nil {
				t.Fatal(e)
			}
			work := &engine{state: state}
			out, e := work.run(nr, own, nil)
			if e != nil || work.rejection != nil {
				t.Fatal(i, e, work.rejection)
			}
			if r["action"] == "commit" {
				root = m(out["transfer"])["result"]
			}
		}
		if len(m(state["objects"])) != i+3 {
			t.Fatal("leaking unreferenced bodies", i)
		}
	}
	if e := (&engine{state: state}).audit(opts.Identity, DefaultLimits()); e != nil {
		t.Fatal(e)
	}
	if m(m(root)["index"])["count"] != "513" {
		t.Fatal("lost index")
	}
}

func TestFourMiBOpaqueBodyOneBlockDelta(t *testing.T) {
	opts, o := fixture(t)
	s := open(t, opts)
	body := bytes.Repeat([]byte{255}, MaxBody)
	root := publish(t, s, plan(body, nil, "large", nil, 0), o)
	changed := bytes.Clone(body)
	for i := MaxObject; i < 2*MaxObject; i++ {
		changed[i] = 254
	}
	p := plan(changed, nil, "changed", root, 0)
	execute(t, s, p.begin, o)
	wantedPage := arr(m(p.begin["body"])["pageHashes"])[0]
	wantedBlock := hash(bytes.Repeat([]byte{254}, MaxObject))
	sent := 0
	for _, put := range p.puts {
		if put["kind"] == "body-page" && put["sha256"] == wantedPage || put["kind"] == "body-block" && put["sha256"] == wantedBlock {
			execute(t, s, put, o)
			sent++
		}
	}
	if sent != 2 {
		t.Fatal(sent)
	}
	root = m(execute(t, s, p.commit, o)["transfer"])["result"]
	unchanged := plan(changed, nil, "same", root, 0)
	start := m(execute(t, s, unchanged.begin, o)["transfer"])
	if n(m(start["progress"])["receivedBytes"]) != 0 {
		t.Fatal("reuse received count")
	}
	root = m(execute(t, s, unchanged.commit, o)["transfer"])["result"]
	if m(m(root)["body"])["sha256"] != hash(changed) {
		t.Fatal("body hash")
	}
	s.Close()
	opts.Mode = "reopen"
	s = open(t, opts)
	r := req("read")
	r["part"] = "body"
	r["commitRoot"] = m(root)["commitRoot"]
	r["offset"] = MaxObject
	r["length"] = MaxObject
	got := execute(t, s, r, o)
	if got["base64"] != base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{254}, MaxObject)) {
		t.Fatal("cold bytes")
	}
}
func TestCapacityReservationAndEncryptedFailures(t *testing.T) {
	opts, o := fixture(t)
	opts.Limits = DefaultLimits()
	opts.Limits.RetainedBytes = 280000
	s := open(t, opts)
	p := plan([]byte("a"), nil, "first", nil, 0)
	execute(t, s, p.begin, o)
	if _, e := s.Execute(context.Background(), plan([]byte("b"), nil, "second", nil, 0).begin, o); e != AdapterError("capacity_exceeded") {
		t.Fatal(e)
	}
	if len(m(s.state["transfers"])) != 1 {
		t.Fatal("failed begin created ticket")
	}
	s.Close()
	original, e := os.ReadFile(opts.Path)
	if e != nil {
		t.Fatal(e)
	}
	opts.Mode = "reopen"
	opts.Key = bytes.Repeat([]byte{1}, 32)
	if _, e = OpenFileStore(opts); e == nil {
		t.Fatal("wrong key")
	}
	got, _ := os.ReadFile(opts.Path)
	if !bytes.Equal(got, original) {
		t.Fatal("source changed")
	}
	opts.Key = bytes.Repeat([]byte{9}, 32)
	for _, mode := range []string{"bit", "truncate"} {
		t.Run(mode, func(t *testing.T) {
			bad := bytes.Clone(original)
			if mode == "bit" {
				bad[len(bad)-1] ^= 1
			} else {
				bad = bad[:len(bad)-15]
			}
			if e = os.WriteFile(opts.Path, bad, 0600); e != nil {
				t.Fatal(e)
			}
			if _, e = OpenFileStore(opts); e == nil {
				t.Fatal("damaged data accepted")
			}
			got, _ = os.ReadFile(opts.Path)
			if !bytes.Equal(got, bad) {
				t.Fatal("damaged source changed")
			}
		})
	}
}
