package terminalpersistence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func indexedOpen(t *testing.T, o Options) *FileStore {
	t.Helper()
	s, e := OpenIndexedFileStore(o)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestIndexedPhysicalReadsFixedWorkGrowingPermanentFacts(t *testing.T) {
	for _, count := range []int{1, 96} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			opts, owner := fixture(t)
			s := indexedOpen(t, opts)
			entries := make([]valueEntry, count)
			for i := range entries {
				entries[i] = valueEntry{hash([]byte(fmt.Sprintf("primary:%d", i))), hash([]byte(fmt.Sprintf("secondary:%d", i))), bytes.Repeat([]byte{byte(i + 1)}, 8192)}
			}
			first := plan([]byte("fixed current work"), entries, "initial", nil, count)
			root := publish(t, s, first, owner)
			s.Close()
			opts.Mode = "reopen"
			s = indexedOpen(t, opts)
			coldBytes := s.indexed.readBytes
			if coldBytes > 16384 || s.indexed.readKinds["body"] != 0 || s.indexed.readKinds["row"] != 0 {
				t.Fatalf("cold full read: %d %v", coldBytes, s.indexed.readKinds)
			}
			start := s.indexed.readBytes
			execute(t, s, req("head"), owner)
			lookup := req("lookup")
			lookup["commitRoot"], lookup["key"] = m(root)["commitRoot"], obj{"kind": "primary", "digest": entries[0].primary}
			execute(t, s, lookup, owner)
			lookupBytes := s.indexed.readBytes - start
			if lookupBytes > 65536 {
				t.Fatalf("selected lookup %d", lookupBytes)
			}
			start = s.indexed.readBytes
			next := plan([]byte("fixed next work"), nil, "next", root, 0)
			publish(t, s, next, owner)
			writeBytes := s.indexed.readBytes - start
			if writeBytes > 1<<20 {
				t.Fatalf("first write full read %d", writeBytes)
			}
			t.Logf("R=%d unrelated_receipt_body_bytes=%d cold_file_bytes=%d head_plus_one_lookup_bytes=%d first_begin_put_commit_file_bytes=%d", count, count*8192, coldBytes, lookupBytes, writeBytes)
			if err := s.Audit(context.Background()); err != nil {
				t.Fatal("explicit complete audit", err)
			}
		})
	}
}

func TestIndexedMissingPermanentRowIsNotAbsent(t *testing.T) {
	opts, owner := fixture(t)
	s := indexedOpen(t, opts)
	p := plan([]byte("current"), nil, "original", nil, 0)
	publish(t, s, p, owner)
	working := newIndexedWorking(s.indexed, s.indexed.head)
	digest := working.tree().get(str(s.indexed.head["tree"]), hv([]any{"transfers", "original"}))
	path := filepath.Join(filepath.Dir(opts.Path), s.indexed.name("row", digest))
	s.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	opts.Mode = "reopen"
	s = indexedOpen(t, opts)
	execute(t, s, req("head"), owner)
	if _, err := s.Execute(context.Background(), query(p), owner); err == nil {
		t.Fatal("missing original became unknown")
	}
	if _, err := s.Execute(context.Background(), p.begin, owner); err == nil {
		t.Fatal("missing original granted execution")
	}
	if err := s.Audit(context.Background()); err == nil {
		t.Fatal("explicit audit missed corruption")
	}
}

func TestIndexedUnpublishedBurnAndBoundedCapacity(t *testing.T) {
	opts, owner := fixture(t)
	s := indexedOpen(t, opts)
	p := plan([]byte("work"), nil, "burn", nil, 0)
	execute(t, s, p.begin, owner)
	for _, put := range p.puts {
		execute(t, s, put, owner)
	}
	writes := n(s.indexed.budget["writes"])
	s.options.CommitHook = func(stage string) error {
		if stage == "before_replace" {
			return errors.New("stop before root")
		}
		return nil
	}
	if _, err := s.Execute(context.Background(), p.commit, owner); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
	s.Close()
	opts.Mode = "reopen"
	s = indexedOpen(t, opts)
	if n(s.indexed.budget["writes"]) <= writes {
		t.Fatal("orphan encryption budget refunded")
	}
	if m(execute(t, s, query(p), owner)["transfer"])["status"] != "staging" {
		t.Fatal("unpublished head authorized")
	}
	execute(t, s, p.commit, owner)
	smallOptions, smallOwner := fixture(t)
	smallOptions.MaxFileBytes = 1 << 20
	small := indexedOpen(t, smallOptions)
	large := plan(indexedDistinctBody(MaxObject*64), nil, "oversized", nil, 0)
	if _, err := small.Execute(context.Background(), large.begin, smallOwner); err != AdapterError("capacity_exceeded") {
		t.Fatalf("admission did not reserve completion: %v", err)
	}
	if m(execute(t, small, query(large), smallOwner)["transfer"])["status"] != "unknown" {
		t.Fatal("rejected admission persisted")
	}
}

func indexedDistinctBody(size int) []byte {
	raw := make([]byte, size)
	for i := 0; i < size; i += MaxObject {
		raw[i] = byte(i / MaxObject)
		if i+1 < size {
			raw[i+1] = byte((i / MaxObject) >> 8)
		}
	}
	return raw
}

func TestIndexedMaximumBodyAdmissionRetainsOriginalLimit(t *testing.T) {
	opts, owner := fixture(t)
	s := indexedOpen(t, opts)
	p := plan(indexedDistinctBody(MaxBody), nil, "max-body", nil, 0)
	execute(t, s, p.begin, owner)
	if m(execute(t, s, query(p), owner)["transfer"])["status"] != "staging" {
		t.Fatal("original maximum not admitted")
	}
	used := m(m(execute(t, s, req("head"), owner)["capacity"])["used"])
	if n(used["activeTransfers"]) != 1 {
		t.Fatal("reservation missing")
	}
	t.Logf("original MaxBody=%d admitted under original physical budget=%d", MaxBody, s.options.MaxFileBytes)
}

func TestIndexedPerMediumLockDoesNotSerializeOtherSources(t *testing.T) {
	aOpts, aOwner := fixture(t)
	a := indexedOpen(t, aOpts)
	bOpts, bOwner := fixture(t)
	b := indexedOpen(t, bOpts)
	aOpts.Mode = "reopen"
	if duplicate, err := OpenIndexedFileStore(aOpts); err == nil {
		duplicate.Close()
		t.Fatal("same medium did not lock")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	a.options.CommitHook = func(stage string) error {
		if stage == "before_replace" {
			close(entered)
			<-release
		}
		return nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := a.Execute(context.Background(), plan([]byte("a"), nil, "a", nil, 0).begin, aOwner)
		done <- err
	}()
	<-entered
	ready := make(chan error, 1)
	go func() { _, err := b.Execute(context.Background(), req("head"), bOwner); ready <- err }()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("global media lock")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestIndexedRangeAuthenticatesOnlySelectedBlocks(t *testing.T) {
	opts, owner := fixture(t)
	opts.Limits = DefaultLimits()
	state := initial(opts.Identity, opts.Limits)
	state["writes"], state["encryptedBytes"] = 1, 1
	engine := &engine{state: state}
	own, err := ownerObject(owner)
	if err != nil {
		t.Fatal(err)
	}
	body := indexedDistinctBody(MaxBody)
	p := plan(body, nil, "full-size", nil, 0)
	requests := []Request{p.begin}
	requests = append(requests, p.puts...)
	requests = append(requests, p.commit)
	for _, request := range requests {
		r, err := normalize(request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = engine.run(r, own, func(obj) bool { return false }); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.audit(opts.Identity, opts.Limits); err != nil {
		t.Fatal(err)
	}
	s, err := openIndexedFileStore(opts, state, false)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	opts.Mode = "reopen"
	s = indexedOpen(t, opts)
	root := m(execute(t, s, req("head"), owner)["root"])
	read := req("read")
	read["commitRoot"], read["part"], read["offset"], read["length"] = root["commitRoot"], "body", 0, 128
	before := s.indexed.readKinds["body"]
	out := execute(t, s, read, owner)
	if !bytes.Equal(decode(out["base64"]), body[:128]) {
		t.Fatal("selected bytes")
	}
	if readBytes := s.indexed.readKinds["body"] - before; readBytes > 24576 {
		t.Fatalf("read whole body or pages: %d", readBytes)
	}
	last := body[(len(body)-1)/MaxObject*MaxObject:]
	path := filepath.Join(filepath.Dir(opts.Path), s.indexed.name("body", key("body-block", hash(last))))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	execute(t, s, read, owner)
	read["offset"], read["length"] = len(body)-1, 1
	if _, err := s.Execute(context.Background(), read, owner); err == nil {
		t.Fatal("selected missing block accepted")
	}
	if err := s.Audit(context.Background()); err == nil {
		t.Fatal("explicit audit omitted unselected corruption")
	}
}

type indexedObservedAuthority struct {
	*testCustodyAuthority
	settlement string
}
type indexedObservedLease struct {
	WriterLease
	authority *indexedObservedAuthority
}

func (a *indexedObservedAuthority) AcquireWriter(ctx context.Context, id Identity, c Custody, r Request) (WriterLease, error) {
	lease, err := a.testCustodyAuthority.AcquireWriter(ctx, id, c, r)
	if err != nil {
		return nil, err
	}
	return &indexedObservedLease{lease, a}, nil
}
func (l *indexedObservedLease) Finish(ctx context.Context, settlement map[string]any) error {
	l.authority.settlement = str(settlement["state"])
	return l.WriterLease.Finish(ctx, settlement)
}
func TestIndexedRejectedAdmissionSettlesOriginalWriterRolledBack(t *testing.T) {
	opts, owner := fixture(t)
	opts.MaxFileBytes = 1 << 20
	authority := &indexedObservedAuthority{testCustodyAuthority: &testCustodyAuthority{selected: Custody{"source-device", "1", "terminal"}, claims: map[string]bool{}}}
	opts.Custody, opts.CustodyAuthority = &authority.selected, authority
	s := indexedOpen(t, opts)
	p := plan(indexedDistinctBody(MaxObject*64), nil, "rejected", nil, 0)
	if _, err := s.Execute(context.Background(), p.begin, owner); err != AdapterError("capacity_exceeded") {
		t.Fatal(err)
	}
	if authority.settlement != "rolled-back" {
		t.Fatalf("rejected admission settlement=%s", authority.settlement)
	}
}

func TestIndexedSharedAuthenticationVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/authentication-tree-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	if hash(raw) != "e1f9493b10dc9dda962149449743c4f24fbf281728c1a626fd61f33381fba90e" {
		t.Fatal("vector drift")
	}
	var v struct {
		Steps []struct {
			Root                     *string
			Key, Value, ExpectedRoot string
			ExpectedWrites           *int
		}
		Absence []struct {
			Root *string
			Key  string
		}
	}
	if err = json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	nodes := map[string][]byte{}
	writes := 0
	tree := authenticationTree{read: func(h string) []byte { return nodes[h] }, write: func(h string, b []byte) { writes++; nodes[h] = b }}
	for _, step := range v.Steps {
		root := ""
		if step.Root != nil {
			root = *step.Root
		}
		writes = 0
		if got := tree.put(root, step.Key, step.Value); got != step.ExpectedRoot {
			t.Fatalf("root %s != %s", got, step.ExpectedRoot)
		}
		if step.ExpectedWrites != nil && writes != *step.ExpectedWrites {
			t.Fatal("non-idempotent write")
		}
		if tree.get(step.ExpectedRoot, step.Key) != step.Value {
			t.Fatal("lookup")
		}
	}
	for _, step := range v.Absence {
		root := ""
		if step.Root != nil {
			root = *step.Root
		}
		if tree.get(root, step.Key) != "" {
			t.Fatal("absence")
		}
	}
	last := v.Steps[len(v.Steps)-1]
	delete(nodes, last.ExpectedRoot)
	var readErr error
	func() { defer caught(&readErr); tree.get(last.ExpectedRoot, last.Key) }()
	if readErr == nil {
		t.Fatal("deleted path accepted as absent")
	}
}

func TestIndexedOriginalEngineColdKeyAndCopy(t *testing.T) {
	opts, owner := fixture(t)
	s := indexedOpen(t, opts)
	first := plan([]byte("current work"), []valueEntry{{hash([]byte("p")), hash([]byte("s")), []byte("durable receipt")}}, "original", nil, 1)
	root := publish(t, s, first, owner)
	pending := plan([]byte("next work"), nil, "pending", root, 0)
	execute(t, s, pending.begin, owner)
	if err := s.Audit(context.Background()); err != nil {
		t.Fatal("audit", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	opts.Mode = "reopen"
	s = indexedOpen(t, opts)
	if m(execute(t, s, query(first), owner)["transfer"])["status"] != "committed" {
		t.Fatal("original missing")
	}
	if m(execute(t, s, query(pending), owner)["transfer"])["status"] != "staging" {
		t.Fatal("pending missing")
	}
	for _, p := range pending.puts {
		execute(t, s, p, owner)
	}
	execute(t, s, pending.commit, owner)
	copy, e := s.CopyTo(context.Background(), CopyOptions{Path: filepath.Join(filepath.Dir(opts.Path), "copy.bin"), Key: bytes.Repeat([]byte{7}, 32)})
	if e != nil {
		t.Fatal("copy", e)
	}
	defer copy.Close()
	if !copy.CopyVerifiedCutoverPending() {
		t.Fatal("copy writer")
	}
	if m(execute(t, copy, query(first), owner)["transfer"])["status"] != "committed" {
		t.Fatal("copy lost receipt")
	}
	if _, e = copy.Execute(context.Background(), pending.begin, owner); e == nil {
		t.Fatal("copy writes")
	}
}

func TestIndexedUnknownAndReaderLayoutFence(t *testing.T) {
	opts, owner := fixture(t)
	s := indexedOpen(t, opts)
	p := plan([]byte("body"), nil, "unknown", nil, 0)
	execute(t, s, p.begin, owner)
	for _, put := range p.puts {
		execute(t, s, put, owner)
	}
	s.options.CommitHook = func(stage string) error {
		if stage == "replaced" {
			return errors.New("lost confirmation")
		}
		return nil
	}
	if _, err := s.Execute(context.Background(), p.commit, owner); !errors.Is(err, ErrUnknown) {
		t.Fatalf("lost commit %v", err)
	}
	s.Close()
	opts.Mode = "reopen"
	if old, err := OpenFileStore(opts); err == nil {
		old.Close()
		t.Fatal("old reader accepted indexed")
	}
	s = indexedOpen(t, opts)
	if m(execute(t, s, query(p), owner)["transfer"])["status"] != "committed" {
		t.Fatal("same key reconcile")
	}
	legacyOptions, _ := fixture(t)
	old := open(t, legacyOptions)
	old.Close()
	legacyOptions.Mode = "reopen"
	if next, err := OpenIndexedFileStore(legacyOptions); err == nil {
		next.Close()
		t.Fatal("indexed auto migrated legacy")
	}
}
