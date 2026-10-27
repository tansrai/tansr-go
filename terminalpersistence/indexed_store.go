package terminalpersistence

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tansrai/tansr-go/canonical"
	"github.com/tansrai/tansr-go/executor"
	"github.com/tansrai/tansr-go/internal/atomicmedia"
	"github.com/tansrai/tansr-go/internal/wire"
)

const indexedFormat = "tansr-go-terminal-persistence-indexed-v2"
const indexedHeadMaximum = 262144

var indexedMagic = []byte("Tansr-Go-TerminalPersistence-Indexed/2\n")

type indexedFailure struct{ cause error }

func indexedMust(err error) {
	if err != nil {
		panic(indexedFailure{err})
	}
}
func indexedCaught(err *error) {
	if v := recover(); v != nil {
		switch x := v.(type) {
		case indexedFailure:
			*err = x.cause
		case AdapterError:
			*err = x
		default:
			panic(v)
		}
	}
}
func parseIndexed(raw []byte, max int) any {
	v, e := canonical.ParseStrict(raw, canonical.Options{MaxBytes: max})
	need(e == nil)
	return v
}
func encodeIndexed(value any, max int) []byte {
	b, e := canonical.Encode(value, canonical.Options{MaxBytes: max})
	need(e == nil, "capacity_exceeded")
	return b
}

type indexedFile struct {
	kind, key string
	plain     []byte
}
type indexedMedia struct {
	store                *FileStore
	budgetKey            []byte
	head                 obj
	budget               obj
	context              context.Context
	scope                executor.Scope
	readFiles, readBytes uint64 // actual returned file bytes; per-instance diagnostics only
	readKinds            map[string]uint64
}

// OpenIndexedFileStore explicitly selects the proof-backed source layout.
// It never detects/upgrades a legacy snapshot and never audits unselected rows.
// Original Host/CustodyHost/CopyTo APIs operate on the same FileStore handle.
func OpenIndexedFileStore(o Options) (*FileStore, error) { return openIndexedFileStore(o, nil, false) }

func openIndexedFileStore(o Options, seed obj, copyOnly bool) (_ *FileStore, err error) {
	defer indexedCaught(&err)
	if !filepath.IsAbs(o.Path) || len(o.Key) != 32 || o.CurrentScope == nil || (o.Mode != "create" && o.Mode != "reopen") || (o.Custody == nil) != (o.CustodyAuthority == nil) {
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
	if o.MigrationBudget == (MigrationBudget{}) {
		o.MigrationBudget = DefaultMigrationBudget()
	}
	if wire.Validate(Contract, "Identity", o.Identity) != nil || custodyValidate("MigrationBudget", o.MigrationBudget) != nil || o.Custody != nil && custodyValidate("Custody", *o.Custody) != nil {
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
	s := &FileStore{options: o, root: root, keyDigest: sha256.Sum256(o.Key), copyOnly: copyOnly}
	success := false
	defer func() {
		if !success {
			_ = s.closeFiles()
		}
	}()
	name := filepath.Base(o.Path)
	if e = noLink(root, name+".lock"); e != nil {
		return nil, e
	}
	s.lock, e = root.OpenFile(name+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = atomicmedia.LockExclusive(s.lock); e != nil {
		return nil, e
	}
	s.lockInfo, e = s.lock.Stat()
	if e != nil {
		return nil, e
	}
	if e = noLink(root, name); e != nil {
		return nil, e
	}
	s.fileInfo, e = root.Stat(name)
	exists := e == nil
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return nil, e
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
	mk := hmac.New(sha256.New, o.Key)
	_, _ = mk.Write(encodeIndexed([]any{indexedFormat, "budget-key", o.Identity}, 32768))
	media := &indexedMedia{store: s, budgetKey: mk.Sum(nil), context: context.Background()}
	s.options.Key = nil
	media.readKinds = map[string]uint64{}
	s.indexed = media
	media.scope, e = s.current()
	if e != nil {
		return nil, e
	}
	if e = s.guard(media.scope, media.context); e != nil {
		return nil, e
	}
	if !exists {
		if _, e = root.Stat(media.budgetName()); !errors.Is(e, os.ErrNotExist) {
			if e != nil {
				return nil, e
			}
			return nil, ErrIntegrity
		}
		media.budget = obj{"format": indexedFormat, "identityDigest": hv(o.Identity), "writes": 0, "encryptedBytes": 0, "storedBytes": 0}
		st := initial(o.Identity, o.Limits)
		if seed != nil {
			st = seed
		}
		if o.Custody != nil && st["custodySelection"] == nil {
			st["custodySelection"], e = custodyNormalize("Custody", *o.Custody)
			if e != nil {
				return nil, e
			}
		}
		if e = media.saveComplete(st, media.scope, media.context); e != nil {
			return nil, e
		}
	} else {
		media.loadHead()
		media.loadBudget()
		s.copyOnly = media.head["readOnly"] == true
		s.state = media.smallState()
		_ = newIndexedWorking(media, media.head)
	}
	if e = s.guard(media.scope, media.context); e != nil {
		return nil, e
	}
	success = true
	return s, nil
}
func (media *indexedMedia) name(kind, key string) string {
	base := filepath.Base(media.store.options.Path)
	if kind == "head" {
		return base
	}
	if kind == "body" {
		key = hv([]any{kind, key})
	}
	need(digestValid(key))
	return base + ".indexed." + kind + "." + key
}
func (media *indexedMedia) budgetName() string {
	return filepath.Base(media.store.options.Path) + ".indexed.budget"
}
func (media *indexedMedia) aad(kind, key string) []byte {
	return encodeIndexed([]any{indexedFormat, media.store.options.Identity, kind, key}, 32768)
}
func (media *indexedMedia) check() { indexedMust(media.store.guard(media.scope, media.context)) }
func (media *indexedMedia) readRaw(name string, max int) []byte {
	media.check()
	indexedMust(noLink(media.store.root, name))
	f, e := media.store.root.Open(name)
	if e != nil {
		indexedMust(errors.Join(ErrIntegrity, e))
	}
	b, e := io.ReadAll(io.LimitReader(f, int64(max)+1))
	ce := f.Close()
	indexedMust(e)
	indexedMust(ce)
	media.readFiles++
	media.readBytes += uint64(len(b))
	need(len(b) <= max)
	media.check()
	return b
}
func (media *indexedMedia) read(kind, key string, max int) []byte {
	b := media.readRaw(media.name(kind, key), max+len(indexedMagic)+28)
	media.readKinds[kind] += uint64(len(b))
	need(bytes.HasPrefix(b, indexedMagic) && len(b) >= len(indexedMagic)+28)
	b = b[len(indexedMagic):]
	raw, e := media.store.aead.Open(nil, b[:12], b[12:], media.aad(kind, key))
	need(e == nil && len(raw) <= max)
	return raw
}
func (media *indexedMedia) smallState() obj {
	s := initial(media.store.options.Identity, media.store.options.Limits)
	s["root"] = detached(media.head["root"])
	if v, ok := media.head["custodySelection"]; ok {
		s["custodySelection"] = detached(v)
	}
	return s
}
func (media *indexedMedia) emptyHead() obj {
	return obj{"format": indexedFormat, "identity": initial(media.store.options.Identity, media.store.options.Limits)["identity"], "limits": initial(media.store.options.Identity, media.store.options.Limits)["limits"], "tree": "", "leaves": 0, "root": nil, "active": []any{}, "totals": obj{"objects": 0, "objectBytes": 0, "receiptEntries": 0, "receiptBytes": 0, "transferFacts": 0, "transferBytes": 0}, "readOnly": media.store.copyOnly, "writes": 0, "encryptedBytes": 0, "storedBytes": 0, "migration": ""}
}
func (media *indexedMedia) loadHead() {
	head := m(parseIndexed(media.read("head", "", indexedHeadMaximum), indexedHeadMaximum))
	base := fields(head, "format", "identity", "limits", "tree", "leaves", "root", "active", "totals", "readOnly", "writes", "encryptedBytes", "storedBytes", "migration")
	extra := 0
	if _, ok := head["custodySelection"]; ok {
		extra = 1
	}
	need(len(head) == len(base)+extra)
	need(head["format"] == indexedFormat && same(head["identity"], media.store.options.Identity) && same(head["limits"], media.store.options.Limits))
	need(head["readOnly"] == true || head["readOnly"] == false)
	need(head["tree"] == "" || digestValid(str(head["tree"])))
	need(n(head["leaves"]) <= 1<<20 && (head["tree"] == "") == (n(head["leaves"]) == 0))
	need(head["migration"] == "" || digestValid(str(head["migration"])))
	need(len(arr(head["active"])) <= media.store.options.Limits.ActiveTransfers)
	seen := map[string]bool{}
	for _, id := range arr(head["active"]) {
		validate("Id", id)
		need(!seen[str(id)])
		seen[str(id)] = true
	}
	t := m(head["totals"])
	need(exact(t, "objects", "objectBytes", "receiptEntries", "receiptBytes", "transferFacts", "transferBytes"))
	for _, v := range t {
		need(n(v) >= 0 && n(v) <= media.store.options.MaxFileBytes)
	}
	need(n(t["objects"]) <= media.store.options.Limits.Objects && n(t["receiptEntries"]) <= media.store.options.Limits.ReceiptEntries && n(t["transferFacts"]) <= media.store.options.Limits.TransferFacts)
	if head["root"] != nil {
		validate("Root", head["root"])
		r := m(head["root"])
		need(r["commitRoot"] == hv([]any{"TPV1-ROOT", head["identity"], fields(r, "generation", "body", "index")}))
		need(m(r["index"])["count"] == fmt.Sprint(n(t["receiptEntries"])))
	} else {
		need(n(t["receiptEntries"]) == 0)
	}
	if v, ok := head["custodySelection"]; ok {
		need(custodyValidate("Custody", v) == nil && media.store.CustodyHandoffEnabled())
	} else {
		need(!media.store.CustodyHandoffEnabled())
	}
	media.head = head
}
func (media *indexedMedia) budgetMAC(raw []byte) string {
	h := hmac.New(sha256.New, media.budgetKey)
	_, _ = h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}
func (media *indexedMedia) loadBudget() {
	outer := m(parseIndexed(media.readRaw(media.budgetName(), 4096), 4096))
	need(exact(outer, "value", "mac"))
	value := m(outer["value"])
	raw := encodeIndexed(value, 2048)
	want := media.budgetMAC(raw)
	need(hmac.Equal([]byte(want), []byte(str(outer["mac"]))))
	need(exact(value, "format", "identityDigest", "writes", "encryptedBytes", "storedBytes") && value["format"] == indexedFormat && value["identityDigest"] == hv(media.store.options.Identity))
	need(n(value["writes"]) <= 1<<20 && int64(n(value["encryptedBytes"])) <= 1<<36 && n(value["storedBytes"])+indexedHeadMaximum+4096 <= media.store.options.MaxFileBytes)
	if media.head != nil {
		for _, key := range []string{"writes", "encryptedBytes", "storedBytes"} {
			need(n(value[key]) >= n(media.head[key]))
		}
	}
	if media.budget != nil {
		for _, key := range []string{"writes", "encryptedBytes", "storedBytes"} {
			need(n(value[key]) >= n(media.budget[key]))
		}
	}
	media.budget = value
}
func (media *indexedMedia) atomic(name string, raw []byte, head bool) (err error) {
	s := media.store
	media.check()
	suffix := make([]byte, 12)
	if _, err = rand.Read(suffix); err != nil {
		return err
	}
	temp := fmt.Sprintf(".%s.%x.tmp", filepath.Base(s.options.Path), suffix)
	f, err := s.root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer s.root.Remove(temp)
	n, err := f.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	if err == nil && head {
		err = s.hook("written")
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil && head {
		err = s.hook("file_synced")
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	media.check()
	if head {
		if err = s.hook("before_replace"); err != nil {
			s.uncertain = true
			return errors.Join(ErrUnknown, err)
		}
	}
	if err = atomicmedia.ReplaceDurable(s.root, filepath.Join(filepath.Dir(s.options.Path), name), temp); err != nil {
		s.uncertain = true
		return errors.Join(ErrUnknown, err)
	}
	if head {
		s.commits++
		s.fileInfo, err = s.root.Stat(name)
		if err == nil {
			err = s.hook("replaced")
		}
		if err == nil {
			err = s.hook("directory_synced")
		}
		if err != nil {
			s.uncertain = true
			return errors.Join(ErrUnknown, err)
		}
	}
	return nil
}
func (media *indexedMedia) encrypt(kind, key string, plain []byte) []byte {
	nonce := make([]byte, 12)
	_, err := rand.Read(nonce)
	indexedMust(err)
	blob := append(append([]byte(nil), indexedMagic...), nonce...)
	return media.store.aead.Seal(blob, nonce, plain, media.aad(kind, key))
}
func (media *indexedMedia) publish(head obj, files map[string]indexedFile, state obj) {
	// Reserve every new immutable byte before creating it. Failed/unpublished
	// files remain charged and are not authority; no inventory scan or GC.
	writes := make([]indexedFile, 0, len(files))
	stored, encrypted := 0, 0
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f := files[name]
		indexedMust(noLink(media.store.root, name))
		_, err := media.store.root.Stat(name)
		if err == nil {
			old := media.read(f.kind, f.key, len(f.plain))
			need(bytes.Equal(old, f.plain))
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			indexedMust(err)
		}
		writes = append(writes, f)
		stored += len(f.plain) + len(indexedMagic) + 28
		encrypted += len(f.plain)
	}
	next := m(detached(media.budget))
	next["writes"] = n(next["writes"]) + len(writes) + 1
	headEncryptionBytes := len(encodeIndexed(head, indexedHeadMaximum)) + 128
	next["encryptedBytes"] = n(next["encryptedBytes"]) + encrypted + headEncryptionBytes
	next["storedBytes"] = n(next["storedBytes"]) + stored
	futureBytes, futureWrites := indexedCompletionReservation(state, n(head["leaves"]))
	need(int64(n(next["writes"]))+futureWrites <= 1<<20 && int64(n(next["encryptedBytes"]))+futureBytes <= 1<<36 && int64(n(next["storedBytes"])+indexedHeadMaximum+4096)+futureBytes <= int64(media.store.options.MaxFileBytes), "capacity_exceeded")
	for _, key := range []string{"writes", "encryptedBytes", "storedBytes"} {
		head[key] = next[key]
	}
	rawHead := encodeIndexed(head, indexedHeadMaximum)
	need(len(rawHead) <= headEncryptionBytes)
	rawBudget := encodeIndexed(obj{"value": next, "mac": media.budgetMAC(encodeIndexed(next, 2048))}, 4096)
	indexedMust(media.atomic(media.budgetName(), rawBudget, false))
	media.budget = next
	for _, f := range writes {
		indexedMust(media.atomic(media.name(f.kind, f.key), media.encrypt(f.kind, f.key, f.plain), false))
	}
	indexedMust(media.atomic(media.name("head", ""), media.encrypt("head", "", rawHead), true))
	media.head = head
	media.store.state = media.smallState()
	media.store.copyOnly = head["readOnly"] == true
	if err := media.store.guard(media.scope, media.context); err != nil {
		media.store.uncertain = true
		indexedMust(errors.Join(ErrUnknown, err))
	}
}
func (media *indexedMedia) execute(ctx context.Context, input Request, owner Owner) (out Response, err error) {
	defer indexedCaught(&err)
	r, e := normalize(input)
	if e != nil {
		return nil, e
	}
	own, e := ownerObject(owner)
	if e != nil {
		return nil, e
	}
	s := media.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.uncertain {
		return nil, ErrUnknown
	}
	media.context = ctx
	media.scope, e = s.current()
	if e != nil {
		return nil, e
	}
	if media.scope != owner.Scope {
		return nil, AdapterError("request_conflict")
	}
	media.check()
	id := s.Identity()
	if r["sourceId"] != id.SourceID || r["sourceGeneration"] != id.SourceGeneration || r["domainKey"] != id.DomainKey {
		return nil, AdapterError("stale_generation")
	}
	media.loadHead()
	media.loadBudget()
	s.copyOnly = media.head["readOnly"] == true
	s.state = media.smallState()
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
		defer func() {
			s.custodyAssert = nil
			if failure := recover(); failure != nil {
				s.uncertain = true
				panic(failure)
			}
			err = s.finishWriter(ctx, lease, commits, out, err)
		}()
	}
	// Validation/IO failures must be converted before the writer settlement
	// observes them; a rejected admission is not a committed null response.
	defer indexedCaught(&err)
	w := newIndexedWorking(media, media.head)
	engine := &engine{state: w.state, rows: w}
	recovery := func(proof obj) bool {
		if s.options.AuthorizeRecovery == nil {
			return false
		}
		raw, _ := json.Marshal(proof["originalOwner"])
		var previous Owner
		if json.Unmarshal(raw, &previous) != nil {
			return false
		}
		media.check()
		ok := s.options.AuthorizeRecovery(id, str(proof["transferId"]), previous, owner)
		media.check()
		return ok
	}
	result, e := engine.run(r, own, recovery)
	if e != nil {
		return nil, e
	}
	if wire.Validate(Contract, "Response", result) != nil {
		return nil, ErrIntegrity
	}
	media.check()
	if engine.changed {
		engine.checkCapacity()
		head := w.seal()
		media.publish(head, w.files, w.state)
	}
	media.check()
	if engine.rejection != nil {
		return nil, engine.rejection
	}
	return Response(result), nil
}
