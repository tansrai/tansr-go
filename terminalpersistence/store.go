package terminalpersistence

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tansrai/tansr-go/canonical"
	"github.com/tansrai/tansr-go/executor"
	"github.com/tansrai/tansr-go/internal/atomicmedia"
	"github.com/tansrai/tansr-go/internal/wire"
)

var magic = []byte("Tansr-Go-TerminalPersistence/1\n")

// FileStore holds one physical writer lock for the independent encrypted layout.
// Each mutation replaces one snapshot, including reference reclamation. Historical
// tickets retain Root results, not a promise to retain unreferenced historical bodies.
type FileStore struct {
	indexed                     *indexedMedia
	custodyAssert               func() error
	mu                          sync.Mutex
	options                     Options
	root                        *os.Root
	lock                        *os.File
	lockInfo, fileInfo          os.FileInfo
	aead                        cipher.AEAD
	aad                         []byte
	state                       obj
	closed, uncertain           bool
	copyOnly                    bool
	keyDigest                   [32]byte
	commits                     uint64
	encryptions, encryptedBytes uint64
}

func noLink(root *os.Root, name string) error {
	i, e := root.Lstat(name)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	if !i.Mode().IsRegular() {
		return ErrIntegrity
	}
	return nil
}
func (s *FileStore) current() (executor.Scope, error) {
	c, e := s.options.CurrentScope()
	if e != nil {
		return c, e
	}
	if wire.Validate("sdk2-ext-v1", "Scope", c) != nil || c.ApplicationScopeID != s.options.Identity.ApplicationScopeID || c.EndUserID != s.options.Identity.EndUserID {
		return c, AdapterError("request_conflict")
	}
	return c, nil
}
func (s *FileStore) fixed() error {
	parent := filepath.Dir(s.options.Path)
	real, e := filepath.EvalSymlinks(parent)
	if e != nil {
		return e
	}
	same := filepath.Clean(real) == filepath.Clean(parent)
	if filepath.Separator == '\\' {
		same = strings.EqualFold(real, parent)
	}
	if !same {
		return ErrIntegrity
	}
	i, e := os.Stat(parent)
	if e != nil {
		return e
	}
	f, e := s.root.Open(".")
	if e != nil {
		return e
	}
	j, e := f.Stat()
	f.Close()
	if e != nil || !os.SameFile(i, j) {
		return ErrIntegrity
	}
	for name, expected := range map[string]os.FileInfo{filepath.Base(s.options.Path): s.fileInfo, filepath.Base(s.options.Path) + ".lock": s.lockInfo} {
		if e = noLink(s.root, name); e != nil {
			return e
		}
		i, e = s.root.Stat(name)
		if expected == nil {
			if !errors.Is(e, os.ErrNotExist) {
				return ErrIntegrity
			}
		} else if e != nil || !os.SameFile(i, expected) {
			return ErrIntegrity
		}
	}
	return nil
}

// Independent rows keep the old control canonical node bound unchanged. The outer
// encrypted snapshot contains canonical row strings and is bounded separately.
func packState(state obj, maximum int) (raw []byte, err error) {
	defer caught(&err)
	packed := m(detached(state))
	for _, name := range []string{"objects", "transfers", "primary"} {
		rows := obj{}
		for key, value := range m(state[name]) {
			rows[key] = string(enc(value))
		}
		packed[name] = rows
	}
	return canonical.Encode(packed, canonical.Options{MaxBytes: maximum})
}
func unpackState(raw []byte, maximum int) (state obj, err error) {
	defer caught(&err)
	value, e := canonical.ParseStrict(raw, canonical.Options{MaxBytes: maximum})
	need(e == nil)
	state = m(value)
	for _, name := range []string{"objects", "transfers", "primary"} {
		rows := obj{}
		for key, value := range m(state[name]) {
			row, e := canonical.ParseStrict([]byte(str(value)), canonical.Options{MaxBytes: 8 << 20})
			need(e == nil)
			rows[key] = row
		}
		state[name] = rows
	}
	return
}
func defaultLimits(l Limits) (Limits, error) {
	if l == (Limits{}) {
		l = DefaultLimits()
	}
	max := DefaultLimits()
	if l.ActiveTransfers > max.ActiveTransfers || l.StagingBytes > max.StagingBytes || l.ReceiptEntries > max.ReceiptEntries || l.TransferFacts > max.TransferFacts || l.Objects > max.Objects || l.RetainedBytes > max.RetainedBytes {
		return l, AdapterError("capacity_exceeded")
	}
	if wire.Validate(Contract, "CapacityLimits", l) != nil || l.ActiveTransfers < 1 || l.StagingBytes < 1 || l.ReceiptEntries < 1 || l.TransferFacts < 1 || l.Objects < 1 || l.RetainedBytes < 1 {
		return l, AdapterError("capacity_exceeded")
	}
	return l, nil
}
func normalize(r Request) (obj, error) {
	raw, e := canonical.Encode(r, canonical.Options{MaxBytes: 32768})
	if e != nil {
		return nil, AdapterError("invalid_request")
	}
	v, e := wire.Decode(Contract, "Request", raw)
	if e != nil {
		return nil, AdapterError("invalid_request")
	}
	return v.(map[string]any), nil
}
func ownerObject(o Owner) (obj, error) {
	raw, e := canonical.Encode(o, canonical.Options{MaxBytes: 8192})
	if e != nil {
		return nil, AdapterError("invalid_request")
	}
	v, e := wire.Decode(Contract, "Owner", raw)
	if e != nil {
		return nil, AdapterError("invalid_request")
	}
	return v.(map[string]any), nil
}

// OpenFileStore requires an existing protected parent and explicit create/reopen.
func OpenFileStore(o Options) (*FileStore, error) {
	return openFileStore(o, nil, false)
}

func openFileStore(o Options, seed obj, copyOnly bool) (_ *FileStore, err error) {
	if (o.Custody == nil) != (o.CustodyAuthority == nil) {
		return nil, AdapterError("invalid_request")
	}
	if o.MigrationBudget == (MigrationBudget{}) {
		o.MigrationBudget = DefaultMigrationBudget()
	}
	if custodyValidate("MigrationBudget", o.MigrationBudget) != nil || o.Custody != nil && custodyValidate("Custody", *o.Custody) != nil {
		return nil, AdapterError("invalid_request")
	}
	if !filepath.IsAbs(o.Path) || len(o.Key) != 32 || o.CurrentScope == nil || (o.Mode != "create" && o.Mode != "reopen") {
		return nil, AdapterError("invalid_request")
	}
	o.Path = filepath.Clean(o.Path)
	o.Limits, err = defaultLimits(o.Limits)
	if err != nil {
		return nil, err
	}
	if o.MaxFileBytes == 0 {
		o.MaxFileBytes = 128 << 20
	}
	if o.MaxFileBytes < 1<<20 || o.MaxFileBytes > 128<<20 {
		return nil, AdapterError("capacity_exceeded")
	}
	if wire.Validate(Contract, "Identity", o.Identity) != nil {
		return nil, AdapterError("invalid_request")
	}
	parent := filepath.Dir(o.Path)
	real, e := filepath.EvalSymlinks(parent)
	if e != nil {
		return nil, e
	}
	samePath := real == parent
	if filepath.Separator == '\\' {
		samePath = strings.EqualFold(real, parent)
	}
	if !samePath {
		return nil, ErrIntegrity
	}
	root, e := os.OpenRoot(parent)
	if e != nil {
		return nil, e
	}
	s := &FileStore{options: o, root: root, copyOnly: copyOnly, keyDigest: sha256.Sum256(o.Key)}
	s.options.Key = nil
	defer func() {
		if err != nil {
			s.closeFiles()
		}
	}()
	before, e := s.current()
	if e != nil {
		return nil, e
	}
	name := filepath.Base(o.Path)
	if err = noLink(root, name+".lock"); err != nil {
		return nil, err
	}
	s.lock, err = root.OpenFile(name+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = atomicmedia.LockExclusive(s.lock); err != nil {
		return nil, err
	}
	s.lockInfo, err = s.lock.Stat()
	if err != nil {
		return nil, err
	}
	if err = noLink(root, name); err != nil {
		return nil, err
	}
	s.fileInfo, err = root.Stat(name)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if o.Mode == "create" && exists {
		return nil, os.ErrExist
	}
	if o.Mode == "reopen" && !exists {
		return nil, os.ErrNotExist
	}
	block, e := aes.NewCipher(o.Key)
	if e != nil {
		return nil, e
	}
	s.aead, e = cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	identity, _ := canonical.Encode(o.Identity, canonical.Options{MaxBytes: 32768})
	s.aad = append(append([]byte{}, magic...), identity...)
	if !exists {
		s.state = initial(o.Identity, o.Limits)
		if seed != nil {
			s.state = seed
		}
		if o.Custody != nil && s.state["custodySelection"] == nil {
			selection, e := custodyNormalize("Custody", *o.Custody)
			if e != nil {
				return nil, e
			}
			s.state["custodySelection"] = selection
		}
		if err = s.save(s.state, before, context.Background()); err != nil {
			return nil, err
		}
		return s, nil
	}
	if err = s.load(); err != nil {
		return nil, err
	}
	after, e := s.current()
	if e != nil || after != before {
		return nil, AdapterError("request_conflict")
	}
	if err = s.fixed(); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *FileStore) load() error {
	if s.indexed != nil {
		return s.indexed.loadComplete()
	}
	f, e := s.root.Open(filepath.Base(s.options.Path))
	if e != nil {
		return e
	}
	blob, e := io.ReadAll(io.LimitReader(f, int64(s.options.MaxFileBytes)+1))
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if len(blob) > s.options.MaxFileBytes || !bytes.HasPrefix(blob, magic) || len(blob) < len(magic)+s.aead.NonceSize()+s.aead.Overhead() {
		return ErrIntegrity
	}
	sealed := blob[len(magic):]
	plain, e := s.aead.Open(nil, sealed[:s.aead.NonceSize()], sealed[s.aead.NonceSize():], s.aad)
	if e != nil {
		return ErrIntegrity
	}
	defer clear(plain)
	state, e := unpackState(plain, s.options.MaxFileBytes)
	if e != nil {
		return ErrIntegrity
	}
	// A migrated domain must never become an unfenced legacy writer on reopen.
	if (state["custodyMigration"] != nil || state["custodySelection"] != nil) && !s.CustodyHandoffEnabled() {
		return AdapterError("capability_unavailable")
	}
	marker, present := state["readOnlyCopy"]
	if present && marker != true {
		return ErrIntegrity
	}
	delete(state, "readOnlyCopy")
	if s.copyOnly && !present {
		return ErrIntegrity
	}
	s.copyOnly = present
	if e = (&engine{state: state}).audit(s.options.Identity, s.options.Limits); e != nil {
		return ErrIntegrity
	}
	s.encryptions = max(s.encryptions, uint64(n(state["writes"])))
	s.encryptedBytes = max(s.encryptedBytes, uint64(n(state["encryptedBytes"])))
	s.state = state
	return nil
}
func (s *FileStore) guard(before executor.Scope, ctx context.Context) error {
	if s.custodyAssert != nil {
		if e := s.custodyAssert(); e != nil {
			return e
		}
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	c, e := s.current()
	if e != nil {
		return e
	}
	if c != before {
		return AdapterError("request_conflict")
	}
	return s.fixed()
}

// completionBytes bounds the packed snapshot while every admitted transfer finishes.
// Frozen digest/ref shapes bound each missing object plus its accepted-map entry by
// 512 encoded bytes (including packState's nested JSON escaping), each new primary
// row plus secondary mapping by 768, and root/result/progress growth by 4096 per
// transfer. Existing begin/owner/base facts are already present in rawBytes. Raw
// objects need base64 space with padding counted separately for every object.
func completionBytes(st obj, rawBytes int) int64 {
	total := int64(rawBytes) + 128 // persisted write/byte counter digit growth
	for _, v := range m(st["transfers"]) {
		row := m(v)
		if m(row["transfer"])["status"] != "staging" {
			continue
		}
		begin := m(row["begin"])
		d := m(begin["declared"])
		accepted := m(row["accepted"])
		remaining := int64(n(d["bytes"]))
		for key := range accepted {
			remaining -= int64(n(m(m(st["objects"])[key])["byteLength"]))
		}
		count := int64(n(d["objects"]) - len(accepted))
		total += 4*((remaining+2*count)/3) + 512*count + 768*int64(n(m(begin["index"])["addedCount"])) + 4096
	}
	return total
}
func (s *FileStore) reserveAdmission(st obj) error {
	raw, err := s.pack(st)
	if err != nil {
		return AdapterError("capacity_exceeded")
	}
	defer clear(raw)
	overhead := len(magic) + s.aead.NonceSize() + s.aead.Overhead()
	if completionBytes(st, len(raw))+int64(overhead) > int64(s.options.MaxFileBytes) {
		return AdapterError("capacity_exceeded")
	}
	return nil
}
func (s *FileStore) budget(st obj, rawBytes int) (err error) {
	defer caught(&err)
	writes := int64(1)
	for _, v := range m(st["transfers"]) {
		row := m(v)
		if m(row["transfer"])["status"] != "staging" {
			continue
		}
		d := m(m(row["begin"])["declared"])
		a := m(row["accepted"])
		writes += int64(n(d["objects"]) - len(a) + 2)
	}
	maximum := min(int64(s.options.MaxFileBytes), completionBytes(st, rawBytes))
	need(int64(max(s.encryptions, uint64(n(st["writes"]))))+writes <= 1<<20 && int64(max(s.encryptedBytes, uint64(n(st["encryptedBytes"]))))+writes*maximum <= 1<<36, "capacity_exceeded")
	return nil
}
func (s *FileStore) hook(stage string) error {
	if s.options.CommitHook != nil {
		return s.options.CommitHook(stage)
	}
	return nil
}
func (s *FileStore) pack(st obj) ([]byte, error) {
	if !s.copyOnly {
		return packState(st, s.options.MaxFileBytes)
	}
	packed := m(detached(st))
	packed["readOnlyCopy"] = true
	return packState(packed, s.options.MaxFileBytes)
}
func (s *FileStore) save(st obj, before executor.Scope, ctx context.Context) error {
	if s.indexed != nil {
		return s.indexed.saveComplete(st, before, ctx)
	}
	raw, e := s.pack(st)
	if e != nil {
		return AdapterError("capacity_exceeded")
	}
	if e = s.budget(st, len(raw)); e != nil {
		return e
	}
	st["writes"] = int(max(s.encryptions, uint64(n(st["writes"]))) + 1)
	st["encryptedBytes"] = int(max(s.encryptedBytes, uint64(n(st["encryptedBytes"])))) + len(raw) + 128
	raw, e = s.pack(st)
	if e != nil {
		return AdapterError("capacity_exceeded")
	}
	defer clear(raw)
	if len(raw)+len(magic)+s.aead.NonceSize()+s.aead.Overhead() > s.options.MaxFileBytes {
		return AdapterError("capacity_exceeded")
	}
	if e = s.guard(before, ctx); e != nil {
		return e
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return e
	}
	blob := append(append([]byte{}, magic...), nonce...)
	s.encryptions = uint64(n(st["writes"]))
	s.encryptedBytes = uint64(n(st["encryptedBytes"]))
	blob = s.aead.Seal(blob, nonce, raw, s.aad)
	suffix := make([]byte, 12)
	if _, e = rand.Read(suffix); e != nil {
		return e
	}
	temp := fmt.Sprintf(".%s.%x.tmp", filepath.Base(s.options.Path), suffix)
	f, e := s.root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer s.root.Remove(temp)
	count, e := f.Write(blob)
	if e == nil && count != len(blob) {
		e = io.ErrShortWrite
	}
	if e == nil {
		e = s.hook("written")
	}
	if e == nil {
		e = f.Sync()
	}
	if e == nil {
		e = s.hook("file_synced")
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = s.guard(before, ctx); e != nil {
		return e
	}
	if e = s.hook("before_replace"); e != nil {
		s.uncertain = true
		return errors.Join(ErrUnknown, e)
	}
	if e = s.guard(before, ctx); e != nil {
		return e
	}
	if e = atomicmedia.ReplaceDurable(s.root, s.options.Path, temp); e != nil {
		s.uncertain = true
		return errors.Join(ErrUnknown, e)
	}
	s.commits++
	s.fileInfo, e = s.root.Stat(filepath.Base(s.options.Path))
	if e != nil {
		s.uncertain = true
		return errors.Join(ErrUnknown, e)
	}
	if e = s.hook("replaced"); e == nil {
		e = s.hook("directory_synced")
	}
	if e == nil {
		e = s.guard(before, ctx)
	}
	if e != nil {
		s.uncertain = true
		return errors.Join(ErrUnknown, e)
	}
	s.state = st
	return nil
}
func (s *FileStore) Identity() Identity         { return s.options.Identity }
func (s *FileStore) Capabilities() Capabilities { return Capabilities{true, true} }
func (s *FileStore) Execute(ctx context.Context, input Request, owner Owner) (output Response, outcome error) {
	if s.indexed != nil {
		return s.indexed.execute(ctx, input, owner)
	}
	r, e := normalize(input)
	if e != nil {
		return nil, e
	}
	own, e := ownerObject(owner)
	if e != nil {
		return nil, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.uncertain {
		return nil, ErrUnknown
	}
	before, e := s.current()
	if e != nil {
		return nil, e
	}
	if before != owner.Scope {
		return nil, AdapterError("request_conflict")
	}
	if e = s.guard(before, ctx); e != nil {
		return nil, e
	}
	id := s.options.Identity
	if r["sourceId"] != id.SourceID || r["sourceGeneration"] != id.SourceGeneration || r["domainKey"] != id.DomainKey {
		return nil, AdapterError("stale_generation")
	}
	if e = s.load(); e != nil {
		return nil, e
	}
	if s.copyOnly && r["action"] != "head" && r["action"] != "read" && r["action"] != "lookup" && r["action"] != "query" {
		return nil, AdapterError("read_only_copy")
	}
	lease, e := s.beginWriter(ctx, r)
	if e != nil {
		return nil, e
	}
	if lease != nil {
		commits := s.commits
		s.custodyAssert = lease.AssertCurrent
		defer func() { s.custodyAssert = nil; outcome = s.finishWriter(ctx, lease, commits, output, outcome) }()
	}
	state := m(detached(s.state))
	newAdmission := false
	if r["action"] == "begin" {
		_, existing := m(state["transfers"])[str(r["transferId"])]
		newAdmission = !existing
	}
	work := &engine{state: state}
	recovery := func(proof obj) bool {
		if s.options.AuthorizeRecovery == nil {
			return false
		}
		raw, _ := json.Marshal(proof["originalOwner"])
		var previous Owner
		if json.Unmarshal(raw, &previous) != nil {
			return false
		}
		if s.guard(before, ctx) != nil {
			return false
		}
		approved := s.options.AuthorizeRecovery(id, str(proof["transferId"]), previous, owner)
		return approved && s.guard(before, ctx) == nil
	}
	result, e := work.run(r, own, recovery)
	if e != nil {
		return nil, e
	}
	if wire.Validate(Contract, "Response", result) != nil {
		return nil, ErrIntegrity
	}
	if e = s.guard(before, ctx); e != nil {
		return nil, e
	}
	commits := s.commits
	if work.changed {
		// New admissions reserve all pending completions under the original media lock.
		// Older admitted tickets remain queryable/continuable; no format migration or eviction.
		if newAdmission {
			if e = s.reserveAdmission(state); e != nil {
				return nil, e
			}
		}
		if e = s.save(state, before, ctx); e != nil {
			return nil, e
		}
	}
	if e = s.guard(before, ctx); e != nil {
		if s.commits != commits {
			s.uncertain = true
			return nil, errors.Join(ErrUnknown, e)
		}
		return nil, e
	}
	if work.rejection != nil {
		return nil, work.rejection
	}
	return Response(result), nil
}
func (s *FileStore) closeFiles() error {
	var result error
	if s.lock != nil {
		result = errors.Join(result, s.lock.Close())
		s.lock = nil
	}
	if s.root != nil {
		result = errors.Join(result, s.root.Close())
		s.root = nil
	}
	return result
}
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	e := s.closeFiles()
	s.closed = true
	s.aead = nil
	if s.indexed != nil {
		clear(s.indexed.budgetKey)
		s.indexed.budgetKey = nil
	}
	return e
}
