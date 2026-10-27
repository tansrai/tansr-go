package terminalpersistence

import (
	"context"
	"github.com/tansrai/tansr-go/executor"
)

// Complete traversal is reserved for the existing explicit custody/copy/audit
// operations. Execute never calls this path or passes a partial view to audit.
func (media *indexedMedia) loadComplete() (err error) {
	defer indexedCaught(&err)
	media.context = context.Background()
	media.scope, err = media.store.current()
	if err != nil {
		return err
	}
	media.loadHead()
	media.loadBudget()
	state := media.smallState()
	state["writes"], state["encryptedBytes"] = media.budget["writes"], media.budget["encryptedBytes"]
	working := newIndexedWorking(media, media.head)
	visited := 0
	working.tree().visit(str(media.head["tree"]), func(keyHash, digest string) {
		visited++
		need(visited <= 1<<20, "capacity_exceeded")
		raw := media.read("row", digest, 8<<20)
		need(hash(raw) == digest)
		row := m(parseIndexed(raw, 8<<20))
		need(exact(row, "table", "key", "value"))
		table, key := str(row["table"]), str(row["key"])
		need(keyHash == hv([]any{table, key}))
		need(table == "objects" || table == "primary" || table == "secondary" || table == "transfers")
		value := working.get(table, key)
		if value != nil {
			if table == "objects" {
				value = working.object(key, true)
			}
			m(state[table])[key] = detached(value)
		}
	})
	need(visited == n(media.head["leaves"]))
	if digest := str(media.head["migration"]); digest != "" {
		raw := media.read("maintenance", digest, media.store.options.MaxFileBytes)
		need(hash(raw) == digest)
		state["custodyMigration"] = parseIndexed(raw, media.store.options.MaxFileBytes)
	}
	if err = (&engine{state: state}).audit(media.store.options.Identity, media.store.options.Limits); err != nil {
		return err
	}
	totals := completeIndexedTotals(state)
	need(same(totals, media.head["totals"]))
	media.store.state = state
	media.store.copyOnly = media.head["readOnly"] == true
	return nil
}

func completeIndexedTotals(state obj) obj {
	totals := obj{"objects": 0, "objectBytes": 0, "receiptEntries": 0, "receiptBytes": 0, "transferFacts": 0, "transferBytes": 0}
	for _, spec := range []struct{ table, count, bytes string }{{"objects", "objects", "objectBytes"}, {"primary", "receiptEntries", "receiptBytes"}, {"transfers", "transferFacts", "transferBytes"}} {
		for _, value := range m(state[spec.table]) {
			count, size := rowCharge(spec.table, value)
			totals[spec.count] = n(totals[spec.count]) + count
			totals[spec.bytes] = n(totals[spec.bytes]) + size
		}
	}
	return totals
}

func (media *indexedMedia) saveComplete(state obj, before executor.Scope, ctx context.Context) (err error) {
	defer indexedCaught(&err)
	media.scope, media.context = before, ctx
	media.check()
	head := media.emptyHead()
	if media.head != nil {
		head = m(detached(media.head))
	}
	// This is an explicitly complete state from the existing maintenance reducer.
	// Rebuild its authenticated projection without modifying the original facts.
	working := newIndexedWorking(media, head)
	desired := obj{}
	for _, v := range m(state["primary"]) {
		desired[terminalpersistenceValueKey(m(m(m(v)["entry"])["value"]))] = true
	}
	oldKeys := map[string]map[string]bool{}
	for _, table := range []string{"objects", "primary", "secondary", "transfers"} {
		oldKeys[table] = map[string]bool{}
	}
	working.tree().visit(str(head["tree"]), func(keyHash, digest string) {
		raw := media.read("row", digest, 8<<20)
		need(hash(raw) == digest)
		row := m(parseIndexed(raw, 8<<20))
		need(exact(row, "table", "key", "value"))
		table, key := str(row["table"]), str(row["key"])
		need(keyHash == hv([]any{table, key}))
		need(oldKeys[table] != nil)
		if row["value"] != nil {
			oldKeys[table][key] = true
		}
	})
	for _, table := range []string{"objects", "primary", "secondary", "transfers"} {
		for key := range oldKeys[table] {
			if m(state[table])[key] == nil {
				working.set(table, key, nil)
			}
		}
		for _, key := range sortedKeys(m(state[table])) {
			value := detached(m(state[table])[key])
			working.set(table, key, value)
			if table == "objects" {
				working.descriptors[key]["permanent"] = desired[key] == true
				working.bodies[key] = decode(m(value)["base64"])
			}
		}
	}
	working.state["root"] = detached(state["root"])
	head = working.seal()
	head["totals"] = completeIndexedTotals(state)
	if value, ok := state["custodySelection"]; ok {
		head["custodySelection"] = detached(value)
	} else {
		delete(head, "custodySelection")
	}
	head["migration"] = ""
	if value := state["custodyMigration"]; value != nil {
		raw := encodeIndexed(value, media.store.options.MaxFileBytes)
		digest := hash(raw)
		working.stage("maintenance", digest, raw)
		head["migration"] = digest
	}
	media.publish(head, working.files, state)
	state["writes"], state["encryptedBytes"] = media.budget["writes"], media.budget["encryptedBytes"]
	media.store.state = media.smallState()
	return nil
}

// Audit explicitly verifies all current original facts and material. Normal
// indexed open/Execute intentionally do not claim unselected media was audited.
func (s *FileStore) Audit(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.discardCompleteView()
	if s.closed {
		return ErrClosed
	}
	if s.uncertain {
		return ErrUnknown
	}
	before, err := s.current()
	if err != nil {
		return err
	}
	if err = s.guard(before, ctx); err != nil {
		return err
	}
	if err = s.load(); err != nil {
		return err
	}
	return s.guard(before, ctx)
}

// Stop lets the existing standard preparation lifecycle own this explicit
// source. Closing it never grants authority or closes the remote session.
func (s *FileStore) Stop() error { return s.Close() }

func (s *FileStore) discardCompleteView() {
	if s.indexed != nil && s.indexed.head != nil {
		s.state = s.indexed.smallState()
	}
}
