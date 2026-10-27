package terminalpersistence

import (
	"encoding/base64"
	"sort"
)

// indexedWorking is a scoped, proof-backed original-fact reader. Its maps are
// private caches, never passed to engine.audit or represented as complete state.
type indexedWorking struct {
	media            *indexedMedia
	state            obj
	head             obj
	before           map[string]obj
	loaded           map[string]map[string]bool
	descriptors      map[string]obj
	priorDescriptors map[string]obj
	bodies           map[string][]byte
	initial          obj
	nodes            map[string][]byte
	files            map[string]indexedFile
	stagedBytes      int
}

func newIndexedWorking(media *indexedMedia, head obj) *indexedWorking {
	state := initial(media.store.options.Identity, media.store.options.Limits)
	state["root"] = detached(head["root"])
	if v, ok := head["custodySelection"]; ok {
		state["custodySelection"] = detached(v)
	}
	w := &indexedWorking{media: media, state: state, head: head, before: map[string]obj{}, loaded: map[string]map[string]bool{}, descriptors: map[string]obj{}, priorDescriptors: map[string]obj{}, bodies: map[string][]byte{}, nodes: map[string][]byte{}, files: map[string]indexedFile{}}
	for _, table := range []string{"objects", "primary", "secondary", "transfers"} {
		w.before[table] = obj{}
		w.loaded[table] = map[string]bool{}
	}
	for _, id := range arr(head["active"]) {
		v := w.get("transfers", str(id))
		need(v != nil && m(m(v)["transfer"])["status"] == "staging")
	}
	w.initial = obj{"root": detached(state["root"]), "transfers": detached(state["transfers"])}
	return w
}
func (w *indexedWorking) tree() authenticationTree {
	return authenticationTree{read: func(h string) []byte {
		if b, ok := w.nodes[h]; ok {
			return b
		}
		b := w.media.read("node", h, 512)
		w.nodes[h] = b
		return b
	}, write: func(h string, b []byte) { w.nodes[h] = b; w.stage("node", h, b) }}
}
func (w *indexedWorking) stage(kind, key string, plain []byte) {
	name := w.media.name(kind, key)
	if old, ok := w.files[name]; ok {
		need(string(old.plain) == string(plain))
		return
	}
	need(w.stagedBytes+len(plain) <= w.media.store.options.MaxFileBytes, "capacity_exceeded")
	w.stagedBytes += len(plain)
	w.files[name] = indexedFile{kind: kind, key: key, plain: append([]byte(nil), plain...)}
}
func (w *indexedWorking) load(table, key string) any {
	if w.loaded[table][key] {
		return m(w.state[table])[key]
	}
	w.loaded[table][key] = true
	digest := w.tree().get(str(w.head["tree"]), hv([]any{table, key}))
	var value any
	if digest != "" {
		raw := w.media.read("row", digest, 8<<20)
		need(hash(raw) == digest)
		row := m(parseIndexed(raw, 8<<20))
		need(exact(row, "table", "key", "value") && row["table"] == table && row["key"] == key)
		value = row["value"]
	}
	if table == "objects" && value != nil {
		d := m(value)
		need(exact(d, "kind", "sha256", "byteLength", "permanent"))
		validate("Ref", fields(d, "sha256", "byteLength"))
		need(key == terminalObjectKey(d) && n(d["byteLength"]) > 0 && n(d["byteLength"]) <= MaxObject && (d["permanent"] == true || d["permanent"] == false))
		w.descriptors[key] = m(detached(d))
		w.priorDescriptors[key] = m(detached(d))
		value = fields(d, "kind", "sha256", "byteLength")
	}
	if value != nil {
		validateIndexedRow(w.state, table, key, value)
		m(w.state[table])[key] = value
	}
	w.before[table][key] = detached(value)
	return value
}
func terminalObjectKey(d obj) string { return key(str(d["kind"]), d["sha256"]) }
func validateIndexedRow(state obj, table, key string, value any) {
	switch table {
	case "primary":
		row := m(value)
		need(exact(row, "ordinal", "entry"))
		validate("Sequence", row["ordinal"])
		validate("Entry", row["entry"])
		need(m(row["entry"])["primaryKey"] == key)
	case "secondary":
		need(digestValid(key) && digestValid(str(value)))
	case "objects":
		d := m(value)
		need(exact(d, "kind", "sha256", "byteLength") && (d["kind"] == "body-page" || d["kind"] == "body-block" || d["kind"] == "index-page" || d["kind"] == "receipt-value"))
	case "transfers":
		row := m(value)
		need(exact(row, "begin", "owner", "baseRoot", "accepted", "transfer"))
		b, t := m(row["begin"]), m(row["transfer"])
		validate("BeginRequest", b)
		validate("Owner", row["owner"])
		validate("Transfer", t)
		intent := m(detached(b))
		delete(intent, "intentSha256")
		need(b["transferId"] == key && t["transferId"] == key && b["intentSha256"] == t["intentSha256"] && b["intentSha256"] == hv(intent))
		for _, k := range []string{"sourceId", "sourceGeneration", "domainKey"} {
			need(b[k] == m(state["identity"])[k])
		}
		for _, k := range []string{"applicationScopeId", "endUserId"} {
			need(m(m(row["owner"])["scope"])[k] == m(state["identity"])[k])
		}
		need(t["status"] == "staging" || t["status"] == "committed" || t["status"] == "rejected")
		if t["status"] != "staging" {
			need(row["baseRoot"] == nil && len(m(row["accepted"])) == 0)
		}
	default:
		need(false)
	}
}
func (w *indexedWorking) get(table, key string) any { return w.load(table, key) }
func (w *indexedWorking) set(table, key string, value any) {
	w.load(table, key)
	if table == "objects" && value != nil {
		o := m(value)
		raw := decode(o["base64"])
		need(hash(raw) == o["sha256"] && len(raw) == n(o["byteLength"]))
		permanent := false
		if old := w.descriptors[key]; old != nil {
			permanent = old["permanent"] == true
		}
		d := fields(o, "kind", "sha256", "byteLength")
		d["permanent"] = permanent
		w.descriptors[key] = d
		w.bodies[key] = raw
		value = fields(o, "kind", "sha256", "byteLength")
	}
	if value == nil {
		delete(m(w.state[table]), key)
		if table == "objects" {
			delete(w.descriptors, key)
		}
	} else {
		m(w.state[table])[key] = value
	}
	if table == "primary" && value != nil {
		ref := m(m(m(value)["entry"])["value"])
		k := terminalpersistenceValueKey(ref)
		need(w.object(k, false) != nil)
		w.descriptors[k]["permanent"] = true
	}
}
func terminalpersistenceValueKey(ref obj) string { return key("receipt-value", ref["sha256"]) }
func (w *indexedWorking) object(key string, material bool) obj {
	v := w.load("objects", key)
	if v == nil {
		return nil
	}
	meta := m(v)
	if !material {
		return meta
	}
	b, ok := w.bodies[key]
	if !ok {
		b = w.media.read("body", key, MaxObject)
		need(len(b) == n(meta["byteLength"]) && hash(b) == meta["sha256"])
		w.bodies[key] = b
	}
	o := m(detached(meta))
	o["base64"] = base64.StdEncoding.EncodeToString(b)
	return o
}
func rowCharge(table string, value any) (int, int) {
	if value == nil {
		return 0, 0
	}
	switch table {
	case "objects":
		o := m(value)
		return 1, n(o["byteLength"]) + len(enc(fields(o, "kind", "sha256", "byteLength")))
	case "primary":
		return 1, len(enc(value))
	case "transfers":
		return 1, len(enc(fields(m(value), "begin", "owner", "baseRoot", "transfer")))
	}
	return 0, 0
}
func (w *indexedWorking) totals() map[string]int {
	t := map[string]int{}
	for k, v := range m(w.head["totals"]) {
		t[k] = n(v)
	}
	for _, spec := range []struct{ table, count, bytes string }{{"objects", "objects", "objectBytes"}, {"primary", "receiptEntries", "receiptBytes"}, {"transfers", "transferFacts", "transferBytes"}} {
		for key := range w.loaded[spec.table] {
			a, b := rowCharge(spec.table, w.before[spec.table][key])
			c, d := rowCharge(spec.table, m(w.state[spec.table])[key])
			t[spec.count] += c - a
			t[spec.bytes] += d - b
			need(t[spec.count] >= 0 && t[spec.bytes] >= 0)
		}
	}
	return t
}
func transferUsage(row obj, object func(string) obj) obj {
	u := obj{"activeTransfers": 0, "stagingBytes": 0, "reservedBytes": 0, "reservedObjects": 0, "reservedReceiptEntries": 0}
	if m(row["transfer"])["status"] != "staging" {
		return u
	}
	size := len(enc(fields(row, "begin", "owner", "baseRoot", "transfer")))
	actual, desc := 0, 0
	for k := range m(row["accepted"]) {
		o := object(k)
		actual += n(o["byteLength"])
		desc += len(enc(fields(o, "kind", "sha256", "byteLength")))
	}
	b := m(row["begin"])
	d := m(b["declared"])
	remaining := n(d["bytes"]) - actual
	need(remaining >= 0 && size+desc <= metadataReserve, "capacity_exceeded")
	u["activeTransfers"] = 1
	u["stagingBytes"] = remaining + n(m(m(row["transfer"])["progress"])["receivedBytes"])
	u["reservedBytes"] = remaining + metadataReserve - size - desc
	u["reservedObjects"] = n(d["objects"]) - len(m(row["accepted"]))
	u["reservedReceiptEntries"] = n(m(b["index"])["addedCount"])
	return u
}
func (w *indexedWorking) capacity(e *engine) obj {
	t := w.totals()
	retained := t["objectBytes"] + t["receiptBytes"] + t["transferBytes"]
	if e.state["root"] != nil {
		retained += len(enc(e.state["root"]))
	}
	u := obj{"activeTransfers": 0, "stagingBytes": 0, "receiptEntries": t["receiptEntries"], "transferFacts": t["transferFacts"], "objects": t["objects"], "retainedBytes": retained, "reservedBytes": 0, "reservedObjects": 0, "reservedReceiptEntries": 0}
	for _, v := range m(w.state["transfers"]) {
		for k, amount := range transferUsage(m(v), func(k string) obj { return w.object(k, false) }) {
			u[k] = n(u[k]) + n(amount)
		}
	}
	return obj{"limits": detached(w.state["limits"]), "used": u}
}
func (w *indexedWorking) live(e *engine, state obj) map[string]bool {
	live := map[string]bool{}
	protect := func(root any) {
		if root == nil {
			return
		}
		for _, sha := range arr(m(m(root)["body"])["pageHashes"]) {
			live[key("body-page", sha)] = true
		}
		for _, v := range e.bodyRefs(root) {
			live[key("body-block", m(v)["sha256"])] = true
		}
	}
	protect(state["root"])
	for _, v := range m(state["transfers"]) {
		r := m(v)
		if m(r["transfer"])["status"] == "staging" {
			protect(r["baseRoot"])
			for k := range m(r["accepted"]) {
				live[k] = true
			}
		}
	}
	return live
}
func (w *indexedWorking) collect(e *engine) {
	old, next := w.live(e, w.initial), w.live(e, w.state)
	for k := range w.loaded["objects"] {
		old[k] = true
	}
	for k := range old {
		if next[k] {
			continue
		}
		if w.object(k, false) != nil && w.descriptors[k]["permanent"] != true {
			w.set("objects", k, nil)
		}
	}
}
func (w *indexedWorking) seal() obj {
	root := str(w.head["tree"])
	leaves := n(w.head["leaves"])
	for _, table := range []string{"objects", "primary", "secondary", "transfers"} {
		keys := []string{}
		for key := range w.loaded[table] {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			before, value := w.before[table][key], m(w.state[table])[key]
			if table == "objects" {
				before = w.priorDescriptors[key]
				value = w.descriptors[key]
				if w.descriptors[key] == nil {
					value = nil
				}
				if w.priorDescriptors[key] == nil {
					before = nil
				}
			}
			if same(before, value) {
				continue
			}
			if table == "objects" && value != nil {
				d := m(value)
				b, ok := w.bodies[key]
				if !ok {
					b = decode(w.object(key, true)["base64"])
				}
				need(hash(b) == d["sha256"])
				w.stage("body", key, b)
			}
			raw := enc(obj{"table": table, "key": key, "value": value})
			digest := hash(raw)
			w.stage("row", digest, raw)
			keyHash := hv([]any{table, key})
			if w.tree().get(root, keyHash) == "" {
				leaves++
			}
			root = w.tree().put(root, keyHash, digest)
		}
	}
	head := m(detached(w.head))
	head["tree"] = root
	head["leaves"] = leaves
	// Only publish the final immutable tree. Intermediate versions created while
	// sealing several keys have never been authoritative and need no disk space.
	live := map[string]bool{}
	var visit func(string)
	visit = func(h string) {
		name := w.media.name("node", h)
		f, ok := w.files[name]
		if !ok || live[name] {
			return
		}
		live[name] = true
		v := m(parseIndexed(f.plain, 512))
		if v["kind"] == "branch" {
			visit(str(v["left"]))
			visit(str(v["right"]))
		}
	}
	if root != "" {
		visit(root)
	}
	for name, f := range w.files {
		if f.kind == "node" && !live[name] {
			delete(w.files, name)
		}
	}
	head["root"] = detached(w.state["root"])
	active := []any{}
	for _, id := range sortedKeys(m(w.state["transfers"])) {
		if m(m(m(w.state["transfers"])[id])["transfer"])["status"] == "staging" {
			active = append(active, id)
		}
	}
	need(len(active) <= w.media.store.options.Limits.ActiveTransfers)
	head["active"] = active
	totals := obj{}
	for k, v := range w.totals() {
		totals[k] = v
	}
	head["totals"] = totals
	return head
}
