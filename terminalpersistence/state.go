package terminalpersistence

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"github.com/tansrai/tansr-go/canonical"
	"github.com/tansrai/tansr-go/internal/wire"
	"sort"
	"strconv"
)

type obj = map[string]any
type engine struct {
	state obj
	// The indexed media port provides original facts and exact aggregates. It is
	// not a partial full-state view and is never passed to the legacy audit.
	rows      *indexedWorking
	changed   bool
	rejection error
}

func (e *engine) row(table, key string) any {
	if e.rows != nil {
		return e.rows.get(table, key)
	}
	return m(e.state[table])[key]
}
func (e *engine) setRow(table, key string, value any) {
	if e.rows != nil {
		e.rows.set(table, key, value)
		return
	}
	m(e.state[table])[key] = value
}
func (e *engine) objectRow(key string, material bool) obj {
	if e.rows != nil {
		return e.rows.object(key, material)
	}
	value := m(e.state["objects"])[key]
	if value == nil {
		return nil
	}
	return m(value)
}
func (e *engine) transferCount() int {
	if e.rows != nil {
		return e.rows.totals()["transferFacts"]
	}
	return len(m(e.state["transfers"]))
}

func need(ok bool, code ...string) {
	if !ok {
		c := "integrity_mismatch"
		if len(code) > 0 {
			c = code[0]
		}
		panic(AdapterError(c))
	}
}
func caught(err *error) {
	if value := recover(); value != nil {
		if e, ok := value.(AdapterError); ok {
			*err = e
		} else {
			panic(value)
		}
	}
}
func m(value any) obj      { x, ok := value.(map[string]any); need(ok); return x }
func arr(value any) []any  { x, ok := value.([]any); need(ok); return x }
func str(value any) string { x, ok := value.(string); need(ok); return x }
func n(value any) int {
	switch x := value.(type) {
	case int:
		return x
	case int64:
		need(x >= 0 && x <= 1<<53)
		return int(x)
	default:
		need(false)
		return 0
	}
}
func hash(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func enc(v any) []byte {
	b, e := canonical.Encode(v, canonical.Options{MaxBytes: 8 << 20})
	need(e == nil)
	return b
}
func hv(v any) string    { return hash(enc(v)) }
func same(a, b any) bool { return bytes.Equal(enc(a), enc(b)) }
func detached(v any) any {
	switch x := v.(type) {
	case map[string]any:
		r := obj{}
		for k, v := range x {
			r[k] = detached(v)
		}
		return r
	case []any:
		r := make([]any, len(x))
		for i, v := range x {
			r[i] = detached(v)
		}
		return r
	default:
		return x
	}
}
func validate(def string, v any)      { need(wire.Validate(Contract, def, v) == nil, "invalid_request") }
func key(kind string, sha any) string { return kind + ":" + str(sha) }
func decode(v any) []byte {
	raw, e := base64.StdEncoding.Strict().DecodeString(str(v))
	need(e == nil && len(raw) <= MaxObject && base64.StdEncoding.EncodeToString(raw) == v)
	return raw
}
func fields(v obj, names ...string) obj {
	r := obj{}
	for _, k := range names {
		r[k] = v[k]
	}
	return r
}
func exact(v obj, names ...string) bool {
	if len(v) != len(names) {
		return false
	}
	for _, k := range names {
		if _, ok := v[k]; !ok {
			return false
		}
	}
	return true
}
func stringsList(v any) []string {
	r := []string{}
	for _, x := range arr(v) {
		r = append(r, str(x))
	}
	return r
}
func bitset(values []bool) string {
	raw := make([]byte, (len(values)+7)/8)
	for i, v := range values {
		if v {
			raw[i/8] |= 1 << uint(i%8)
		}
	}
	return base64.StdEncoding.EncodeToString(raw)
}
func initial(id Identity, limits Limits) obj {
	ib, _ := canonical.Encode(id, canonical.Options{MaxBytes: 32768})
	i, _ := canonical.Decode(ib, canonical.Options{MaxBytes: 32768})
	lb, _ := canonical.Encode(limits, canonical.Options{MaxBytes: 32768})
	l, _ := canonical.Decode(lb, canonical.Options{MaxBytes: 32768})
	return obj{"format": format, "identity": i, "limits": l, "root": nil, "objects": obj{}, "transfers": obj{}, "primary": obj{}, "secondary": obj{}, "writes": 0, "encryptedBytes": 0}
}
func (e *engine) object(kind string, sha any) []byte {
	v := e.objectRow(key(kind, sha), true)
	raw := decode(v["base64"])
	need(v["kind"] == kind && v["sha256"] == sha && len(raw) == n(v["byteLength"]) && hash(raw) == sha)
	return raw
}
func (e *engine) page(kind string, index int, sha any) []any {
	raw := e.object(kind, sha)
	v, err := canonical.ParseStrict(raw, canonical.Options{MaxBytes: MaxObject})
	need(err == nil)
	p := m(v)
	field := "refs"
	if kind == "index-page" {
		field = "entries"
	}
	need(exact(p, "version", "kind", "index", field) && n(p["version"]) == 1 && p["kind"] == kind && n(p["index"]) == index)
	return arr(p[field])
}
func (e *engine) bodyRefs(root any) []any {
	refs := []any{}
	if root == nil {
		return refs
	}
	b := m(m(root)["body"])
	for i, sha := range arr(b["pageHashes"]) {
		v := e.page("body-page", i, sha)
		need(len(v) == min(64, n(b["blockCount"])-i*64))
		refs = append(refs, v...)
	}
	need(len(refs) == n(b["blockCount"]))
	for i, v := range refs {
		validate("Ref", v)
		need(n(m(v)["byteLength"]) == min(MaxObject, n(b["byteLength"])-i*MaxObject))
	}
	return refs
}
func (e *engine) plan(row obj) ([]any, []any, bool) {
	b := m(row["begin"])
	accepted := m(row["accepted"])
	complete := true
	result := [][]any{{}, {}}
	for position, part := range []struct {
		kind, plan, count string
		cap               int
	}{{"body-page", "body", "blockCount", 64}, {"index-page", "index", "entryCount", 32}} {
		p := m(b[part.plan])
		for i, sha := range arr(p["pageHashes"]) {
			count := min(part.cap, n(p[part.count])-i*part.cap)
			if _, ok := accepted[key(part.kind, sha)]; !ok {
				result[position] = append(result[position], make([]any, count)...)
				complete = false
				continue
			}
			values := e.page(part.kind, i, sha)
			need(len(values) == count)
			for _, v := range values {
				def := "Ref"
				if position == 1 {
					def = "Entry"
				}
				validate(def, v)
			}
			result[position] = append(result[position], values...)
		}
	}
	for i, v := range result[0] {
		if v != nil {
			need(n(m(v)["byteLength"]) == min(MaxObject, n(m(b["body"])["byteLength"])-i*MaxObject))
		}
	}
	previous := ""
	secondary := map[string]bool{}
	for _, v := range result[1] {
		if v == nil {
			continue
		}
		entry := m(v)
		pk, sk := str(entry["primaryKey"]), str(entry["secondaryKey"])
		need(pk > previous && !secondary[sk])
		previous = pk
		secondary[sk] = true
	}
	return result[0], result[1], complete
}
func accept(row obj, kind string, sha any, source string) {
	a := m(row["accepted"])
	k := key(kind, sha)
	if _, ok := a[k]; !ok {
		a[k] = source
	}
}
func (e *engine) reuse(row obj) {
	b := m(row["begin"])
	base := row["baseRoot"]
	if base != nil {
		old := arr(m(m(base)["body"])["pageHashes"])
		for i, sha := range arr(m(b["body"])["pageHashes"]) {
			if i < len(old) && old[i] == sha {
				e.page("body-page", i, sha)
				accept(row, "body-page", sha, "reused")
			}
		}
	}
	refs, entries, _ := e.plan(row)
	old := obj{}
	for _, v := range e.bodyRefs(base) {
		ref := m(v)
		old[key("body-block", ref["sha256"])] = ref
	}
	for _, v := range refs {
		if v != nil {
			ref := m(v)
			if same(old[key("body-block", ref["sha256"])], ref) {
				e.object("body-block", ref["sha256"])
				accept(row, "body-block", ref["sha256"], "reused")
			}
		}
	}
	for _, v := range entries {
		if v == nil {
			continue
		}
		entry := m(v)
		found := e.row("primary", str(entry["primaryKey"]))
		if found != nil && same(m(found)["entry"], entry) && e.row("secondary", str(entry["secondaryKey"])) == entry["primaryKey"] {
			ref := m(entry["value"])
			e.object("receipt-value", ref["sha256"])
			accept(row, "receipt-value", ref["sha256"], "reused")
		}
	}
}
func (e *engine) progress(row obj) obj {
	refs, entries, _ := e.plan(row)
	a := m(row["accepted"])
	b := m(row["begin"])
	pages, body, values := []bool{}, []bool{}, []bool{}
	for _, p := range []struct{ k, p string }{{"body-page", "body"}, {"index-page", "index"}} {
		for _, sha := range arr(m(b[p.p])["pageHashes"]) {
			_, ok := a[key(p.k, sha)]
			pages = append(pages, ok)
		}
	}
	for _, v := range refs {
		ready := false
		if v != nil {
			_, ready = a[key("body-block", m(v)["sha256"])]
		}
		body = append(body, ready)
	}
	for _, v := range entries {
		ready := false
		if v != nil {
			_, ready = a[key("receipt-value", m(m(v)["value"])["sha256"])]
		}
		values = append(values, ready)
	}
	received := 0
	for k, source := range a {
		if source == "received" {
			received += n(e.objectRow(k, false)["byteLength"])
		}
	}
	return obj{"pagesReady": bitset(pages), "bodyReady": bitset(body), "valuesReady": bitset(values), "receivedBytes": received}
}
func (e *engine) planned(row obj) map[string]int {
	refs, entries, complete := e.plan(row)
	if !complete {
		return nil
	}
	objects := map[string]int{}
	add := func(kind string, sha any, size int) {
		k := key(kind, sha)
		old, ok := objects[k]
		need(!ok || old == size, "invalid_request")
		objects[k] = size
	}
	b := m(row["begin"])
	for _, p := range []struct{ k, p string }{{"body-page", "body"}, {"index-page", "index"}} {
		for _, sha := range arr(m(b[p.p])["pageHashes"]) {
			add(p.k, sha, len(e.object(p.k, sha)))
		}
	}
	for _, v := range refs {
		r := m(v)
		add("body-block", r["sha256"], n(r["byteLength"]))
	}
	for _, v := range entries {
		r := m(m(v)["value"])
		add("receipt-value", r["sha256"], n(r["byteLength"]))
	}
	total := 0
	for _, size := range objects {
		total += size
	}
	d := m(b["declared"])
	need(len(objects) == n(d["objects"]) && total == n(d["bytes"]), "invalid_request")
	return objects
}
func (e *engine) capacity() obj {
	if e.rows != nil {
		return e.rows.capacity(e)
	}
	s := e.state
	objects := m(s["objects"])
	retained := 0
	for _, v := range objects {
		o := m(v)
		retained += n(o["byteLength"]) + len(enc(fields(o, "kind", "sha256", "byteLength")))
	}
	for _, v := range m(s["primary"]) {
		retained += len(enc(v))
	}
	if s["root"] != nil {
		retained += len(enc(s["root"]))
	}
	u := obj{"activeTransfers": 0, "stagingBytes": 0, "receiptEntries": len(m(s["primary"])), "transferFacts": len(m(s["transfers"])), "objects": len(objects), "retainedBytes": 0, "reservedBytes": 0, "reservedObjects": 0, "reservedReceiptEntries": 0}
	inc := func(k string, value int) { u[k] = n(u[k]) + value }
	for _, v := range m(s["transfers"]) {
		row := m(v)
		size := len(enc(fields(row, "begin", "owner", "baseRoot", "transfer")))
		retained += size
		for k, amount := range transferUsage(row, func(k string) obj { return m(objects[k]) }) {
			inc(k, n(amount))
		}
	}
	u["retainedBytes"] = retained
	return obj{"limits": detached(s["limits"]), "used": u}
}
func (e *engine) checkCapacity() obj {
	c := e.capacity()
	u := m(c["used"])
	reserve := map[string]string{"objects": "reservedObjects", "receiptEntries": "reservedReceiptEntries", "retainedBytes": "reservedBytes"}
	for k, v := range m(c["limits"]) {
		total := n(u[k])
		if r := reserve[k]; r != "" {
			need(n(u[r]) >= 0)
			total += n(u[r])
		}
		need(total <= n(v), "capacity_exceeded")
	}
	return c
}
func expected(root any) any {
	if root == nil {
		return nil
	}
	r := m(root)
	return obj{"commitRoot": r["commitRoot"], "generation": r["generation"], "bodyEtag": m(r["body"])["sha256"], "indexRoot": m(r["index"])["root"], "indexCount": m(r["index"])["count"]}
}
func (e *engine) collect() {
	if e.rows != nil {
		e.rows.collect(e)
		return
	}
	live := map[string]bool{}
	protect := func(root any) {
		if root != nil {
			for _, sha := range arr(m(m(root)["body"])["pageHashes"]) {
				live[key("body-page", sha)] = true
			}
			for _, v := range e.bodyRefs(root) {
				live[key("body-block", m(v)["sha256"])] = true
			}
		}
	}
	protect(e.state["root"])
	for _, v := range m(e.state["transfers"]) {
		row := m(v)
		if m(row["transfer"])["status"] == "staging" {
			protect(row["baseRoot"])
			for k := range m(row["accepted"]) {
				live[k] = true
			}
		}
	}
	for _, v := range m(e.state["primary"]) {
		live[key("receipt-value", m(m(m(v)["entry"])["value"])["sha256"])] = true
	}
	for k := range m(e.state["objects"]) {
		if !live[k] {
			delete(m(e.state["objects"]), k)
		}
	}
}
func (e *engine) reject(row obj, code string) {
	t := m(row["transfer"])
	t["status"] = "rejected"
	t["progress"] = nil
	t["result"] = nil
	t["rejection"] = obj{"code": code, "observedRoot": detached(e.state["root"])}
	row["baseRoot"] = nil
	row["accepted"] = obj{}
	e.collect()
	e.changed = true
	e.rejection = AdapterError(code)
}
func (e *engine) run(r obj, owner any, recovery func(obj) bool) (out obj, err error) {
	defer caught(&err)
	out = e.execute(r, owner, recovery)
	return
}
func (e *engine) execute(r obj, owner any, recovery func(obj) bool) obj {
	action := str(r["action"])
	out := fields(r, "contract", "action", "sourceId", "sourceGeneration", "domainKey")
	root := e.state["root"]
	if action == "head" {
		out["root"] = detached(root)
		out["capacity"] = e.checkCapacity()
		return out
	}
	if action == "read" || action == "lookup" {
		need(root != nil && m(root)["commitRoot"] == r["commitRoot"], "revision_conflict")
		rt := m(root)
		b := m(rt["body"])
		out["commitRoot"] = rt["commitRoot"]
		if action == "lookup" {
			q := m(r["key"])
			k := str(q["digest"])
			if q["kind"] == "secondary" {
				v := e.row("secondary", k)
				k = ""
				if v != nil {
					k = str(v)
				}
			}
			out["entry"] = nil
			if v := e.row("primary", k); v != nil {
				entry := m(detached(m(v)["entry"]))
				ref := m(entry["value"])
				raw := e.object("receipt-value", ref["sha256"])
				need(len(raw) == n(ref["byteLength"]))
				entry["base64"] = base64.StdEncoding.EncodeToString(raw)
				out["entry"] = entry
			}
			out["indexRoot"] = m(rt["index"])["root"]
			out["indexCount"] = m(rt["index"])["count"]
			return out
		}
		out["part"] = r["part"]
		var raw []byte
		if r["part"] == "body-page" {
			i := n(r["pageIndex"])
			pages := arr(b["pageHashes"])
			need(i < len(pages), "invalid_request")
			raw = e.object("body-page", pages[i])
			out["pageIndex"] = i
		} else {
			offset := n(r["offset"])
			need(offset <= n(b["byteLength"]), "invalid_request")
			end := min(n(b["byteLength"]), offset+n(r["length"]))
			raw = []byte{}
			for i := offset / MaxObject; i < (end+MaxObject-1)/MaxObject; i++ {
				pageIndex := i / 64
				refs := e.page("body-page", pageIndex, arr(b["pageHashes"])[pageIndex])
				need(len(refs) == min(64, n(b["blockCount"])-pageIndex*64))
				ref := m(refs[i%64])
				validate("Ref", ref)
				need(n(ref["byteLength"]) == min(MaxObject, n(b["byteLength"])-i*MaxObject))
				data := e.object("body-block", ref["sha256"])
				raw = append(raw, data[max(0, offset-i*MaxObject):min(len(data), end-i*MaxObject)]...)
			}
			out["bodyEtag"] = b["sha256"]
			out["offset"] = offset
			out["nextOffset"] = end
			out["complete"] = end == n(b["byteLength"])
		}
		out["base64"] = base64.StdEncoding.EncodeToString(raw)
		out["byteLength"] = len(raw)
		out["payloadDigest"] = hash(raw)
		return out
	}
	id := str(r["transferId"])
	v := e.row("transfers", id)
	var row obj
	if v != nil {
		row = m(v)
		need(m(row["begin"])["intentSha256"] == r["intentSha256"], "request_conflict")
		if !same(row["owner"], owner) {
			proof := obj{"identity": detached(e.state["identity"]), "transferId": id, "originalOwner": detached(row["owner"]), "currentOwner": detached(owner)}
			need(action == "query" && recovery != nil && recovery(proof), "request_conflict")
		}
	}
	if action == "query" || row == nil && action != "begin" {
		out["transfer"] = obj{"transferId": id, "intentSha256": r["intentSha256"], "status": "unknown", "progress": nil, "result": nil, "rejection": nil}
		if row != nil {
			out["transfer"] = detached(row["transfer"])
		}
		return out
	}
	if action == "begin" {
		intent := m(detached(r))
		delete(intent, "intentSha256")
		need(hv(intent) == r["intentSha256"], "invalid_request")
		b, i := m(r["body"]), m(r["index"])
		need(n(i["addedCount"]) <= n(i["entryCount"]), "invalid_request")
		need(n(b["blockCount"]) == (n(b["byteLength"])+MaxObject-1)/MaxObject && len(arr(b["pageHashes"])) == (n(b["blockCount"])+63)/64 && len(arr(i["pageHashes"])) == (n(i["entryCount"])+31)/32, "invalid_request")
		need(n(b["byteLength"]) != 0 || b["sha256"] == hash(nil), "invalid_request")
		if row == nil {
			row = obj{"begin": detached(r), "owner": detached(owner), "baseRoot": detached(root), "accepted": obj{}, "transfer": obj{"transferId": id, "intentSha256": r["intentSha256"], "status": "staging", "progress": nil, "result": nil, "rejection": nil}}
			e.setRow("transfers", id, row)
			if !same(expected(root), r["expected"]) {
				m(row["transfer"])["progress"] = obj{"pagesReady": "", "bodyReady": "", "valuesReady": "", "receivedBytes": 0}
				need(e.transferCount() <= n(m(e.state["limits"])["transferFacts"]), "capacity_exceeded")
				e.reject(row, "revision_conflict")
				e.checkCapacity()
			} else {
				e.reuse(row)
				m(row["transfer"])["progress"] = e.progress(row)
				e.checkCapacity()
				e.changed = true
			}
		} else {
			need(same(row["begin"], r), "request_conflict")
		}
	} else if m(row["transfer"])["status"] == "staging" {
		if action == "put" {
			e.put(row, r)
		} else if action == "commit" {
			e.commit(row)
		}
	}
	if m(row["transfer"])["status"] == "rejected" {
		e.rejection = AdapterError(str(m(m(row["transfer"])["rejection"])["code"]))
	}
	out["transfer"] = detached(row["transfer"])
	return out
}
func (e *engine) put(row, r obj) {
	kind, sha := str(r["kind"]), r["sha256"]
	raw := decode(r["base64"])
	need(len(raw) == n(r["byteLength"]) && hash(raw) == sha)
	b := m(row["begin"])
	allowed := false
	if kind == "body-page" || kind == "index-page" {
		part := "body"
		if kind == "index-page" {
			part = "index"
		}
		for _, h := range arr(m(b[part])["pageHashes"]) {
			allowed = allowed || h == sha
		}
	} else {
		refs, entries, _ := e.plan(row)
		values := refs
		if kind == "receipt-value" {
			values = []any{}
			for _, v := range entries {
				if v != nil {
					values = append(values, m(v)["value"])
				}
			}
		}
		for _, v := range values {
			if v != nil {
				ref := m(v)
				allowed = allowed || ref["sha256"] == sha && n(ref["byteLength"]) == len(raw)
			}
		}
	}
	need(allowed, "invalid_request")
	k := key(kind, sha)
	o := obj{"kind": kind, "sha256": sha, "byteLength": len(raw), "base64": r["base64"]}
	oldObject := e.objectRow(k, true)
	need(oldObject == nil || same(oldObject, o))
	if _, ok := m(row["accepted"])[k]; ok {
		return
	}
	e.setRow("objects", k, o)
	accept(row, kind, sha, "received")
	var checkErr error
	func() { defer caught(&checkErr); e.reuse(row); e.planned(row) }()
	if checkErr != nil {
		code := "integrity_mismatch"
		if checkErr == AdapterError("invalid_request") {
			code = "invalid_request"
		}
		e.reject(row, code)
		return
	}
	m(row["transfer"])["progress"] = e.progress(row)
	e.checkCapacity()
	e.changed = true
}
func (e *engine) commit(row obj) {
	planned := e.planned(row)
	need(planned != nil, "invalid_request")
	for k, size := range planned {
		_, ok := m(row["accepted"])[k]
		need(ok, "invalid_request")
		need(n(e.objectRow(k, false)["byteLength"]) == size)
	}
	b := m(row["begin"])
	if !same(expected(e.state["root"]), b["expected"]) {
		e.reject(row, "revision_conflict")
		return
	}
	refs, entries, _ := e.plan(row)
	h := sha256.New()
	for _, v := range refs {
		h.Write(e.object("body-block", m(v)["sha256"]))
	}
	if fmt.Sprintf("%x", h.Sum(nil)) != m(b["body"])["sha256"] {
		e.reject(row, "integrity_mismatch")
		return
	}
	count, generation := int64(0), int64(1)
	previous := hv([]any{"TPV1-INDEX", e.state["identity"]})
	if e.state["root"] != nil {
		root := m(e.state["root"])
		count, _ = strconv.ParseInt(str(m(root["index"])["count"]), 10, 64)
		generation, _ = strconv.ParseInt(str(root["generation"]), 10, 64)
		need(generation < 9223372036854775807, "capacity_exceeded")
		generation++
		previous = str(m(root["index"])["root"])
	}
	added := []any{}
	for _, v := range entries {
		entry := m(v)
		pk, sk := str(entry["primaryKey"]), str(entry["secondaryKey"])
		old, other := e.row("primary", pk), e.row("secondary", sk)
		if old != nil || other != nil {
			if old == nil || !same(m(old)["entry"], entry) || other != pk {
				e.reject(row, "request_conflict")
				return
			}
		} else {
			added = append(added, entry)
		}
	}
	if len(added) != n(m(b["index"])["addedCount"]) {
		e.reject(row, "request_conflict")
		return
	}
	need(count <= 9223372036854775807-int64(len(added)), "capacity_exceeded")
	for _, v := range added {
		entry := m(v)
		count++
		ordinal := strconv.FormatInt(count, 10)
		previous = hv([]any{"TPV1-ENTRY", previous, ordinal, entry})
		e.setRow("primary", str(entry["primaryKey"]), obj{"ordinal": ordinal, "entry": detached(entry)})
		e.setRow("secondary", str(entry["secondaryKey"]), entry["primaryKey"])
	}
	root := obj{"generation": strconv.FormatInt(generation, 10), "body": detached(b["body"]), "index": obj{"root": previous, "count": strconv.FormatInt(count, 10)}}
	root["commitRoot"] = hv([]any{"TPV1-ROOT", e.state["identity"], root})
	e.state["root"] = root
	t := m(row["transfer"])
	t["status"] = "committed"
	t["progress"] = nil
	t["result"] = detached(root)
	t["rejection"] = nil
	row["baseRoot"] = nil
	row["accepted"] = obj{}
	e.collect()
	e.checkCapacity()
	e.changed = true
}

// audit is called before exposing a reopened authenticated state, including absent lookup.
func (e *engine) audit(id Identity, limits Limits) (err error) {
	defer caught(&err)
	s := e.state
	base := fields(s, "format", "identity", "limits", "root", "objects", "transfers", "primary", "secondary", "writes", "encryptedBytes")
	extra := 0
	if v, ok := s["custodySelection"]; ok {
		need(custodyValidate("Custody", v) == nil)
		extra++
	}
	if v, ok := s["custodyMigration"]; ok {
		need(v != nil)
		extra++
	}
	need(len(s) == len(base)+extra && s["format"] == format && same(s["identity"], id) && same(s["limits"], limits))
	need(n(s["writes"]) >= 1 && n(s["writes"]) <= 1<<20 && n(s["encryptedBytes"]) >= 1 && int64(n(s["encryptedBytes"])) <= 1<<36)
	for k, v := range m(s["objects"]) {
		o := m(v)
		need(exact(o, "kind", "sha256", "byteLength", "base64"))
		kind := str(o["kind"])
		need(kind == "body-page" || kind == "index-page" || kind == "body-block" || kind == "receipt-value")
		validate("Ref", fields(o, "sha256", "byteLength"))
		need(k == key(kind, o["sha256"]))
		e.object(kind, o["sha256"])
	}
	ordered := []obj{}
	for k, v := range m(s["primary"]) {
		item := m(v)
		need(exact(item, "ordinal", "entry"))
		validate("Sequence", item["ordinal"])
		validate("Entry", item["entry"])
		entry := m(item["entry"])
		need(entry["primaryKey"] == k && m(s["secondary"])[str(entry["secondaryKey"])] == k)
		ref := m(entry["value"])
		need(len(e.object("receipt-value", ref["sha256"])) == n(ref["byteLength"]))
		ordered = append(ordered, item)
	}
	need(len(m(s["secondary"])) == len(ordered))
	sort.Slice(ordered, func(i, j int) bool {
		a, _ := strconv.ParseInt(str(ordered[i]["ordinal"]), 10, 64)
		b, _ := strconv.ParseInt(str(ordered[j]["ordinal"]), 10, 64)
		return a < b
	})
	chain := map[string]string{"0": hv([]any{"TPV1-INDEX", s["identity"]})}
	previous := chain["0"]
	for i, item := range ordered {
		ordinal := strconv.Itoa(i + 1)
		need(item["ordinal"] == ordinal)
		previous = hv([]any{"TPV1-ENTRY", previous, ordinal, item["entry"]})
		chain[ordinal] = previous
	}
	rootCheck := func(root any, material bool) {
		if root == nil {
			return
		}
		validate("Root", root)
		r := m(root)
		generation, err := strconv.ParseInt(str(r["generation"]), 10, 64)
		need(err == nil && generation > 0)
		body := m(r["body"])
		need(n(body["blockCount"]) == (n(body["byteLength"])+MaxObject-1)/MaxObject && len(arr(body["pageHashes"])) == (n(body["blockCount"])+63)/64)
		need(m(r["index"])["root"] == chain[str(m(r["index"])["count"])])
		need(r["commitRoot"] == hv([]any{"TPV1-ROOT", s["identity"], fields(r, "generation", "body", "index")}))
		if material {
			h := sha256.New()
			for _, v := range e.bodyRefs(root) {
				ref := m(v)
				raw := e.object("body-block", ref["sha256"])
				need(len(raw) == n(ref["byteLength"]))
				h.Write(raw)
			}
			need(fmt.Sprintf("%x", h.Sum(nil)) == body["sha256"])
		}
	}
	rootCheck(s["root"], true)
	if s["root"] == nil {
		need(len(ordered) == 0)
	} else {
		need(m(m(s["root"])["index"])["count"] == strconv.Itoa(len(ordered)))
	}
	generations := obj{}
	for id, v := range m(s["transfers"]) {
		row := m(v)
		need(exact(row, "begin", "owner", "baseRoot", "accepted", "transfer"))
		b, t := m(row["begin"]), m(row["transfer"])
		validate("BeginRequest", b)
		validate("Owner", row["owner"])
		validate("Transfer", t)
		intent := m(detached(b))
		delete(intent, "intentSha256")
		need(b["transferId"] == id && t["transferId"] == id && b["intentSha256"] == t["intentSha256"] && b["intentSha256"] == hv(intent))
		for _, k := range []string{"sourceId", "sourceGeneration", "domainKey"} {
			need(b[k] == m(s["identity"])[k])
		}
		for _, k := range []string{"applicationScopeId", "endUserId"} {
			need(m(m(row["owner"])["scope"])[k] == m(s["identity"])[k])
		}
		body, index := m(b["body"]), m(b["index"])
		need(n(index["addedCount"]) <= n(index["entryCount"]))
		need(n(body["blockCount"]) == (n(body["byteLength"])+MaxObject-1)/MaxObject && len(arr(body["pageHashes"])) == (n(body["blockCount"])+63)/64 && len(arr(index["pageHashes"])) == (n(index["entryCount"])+31)/32)
		need(n(body["byteLength"]) != 0 || body["sha256"] == hash(nil))
		if t["status"] == "staging" {
			base := row["baseRoot"]
			rootCheck(base, true)
			need(same(b["expected"], expected(base)))
			refs, entries, _ := e.plan(row)
			allowed, reuse := map[string]bool{}, map[string]bool{}
			for _, p := range []struct{ k, p string }{{"body-page", "body"}, {"index-page", "index"}} {
				for i, sha := range arr(m(b[p.p])["pageHashes"]) {
					k := key(p.k, sha)
					allowed[k] = true
					if p.k == "body-page" && base != nil {
						old := arr(m(m(base)["body"])["pageHashes"])
						reuse[k] = i < len(old) && old[i] == sha
					}
				}
			}
			for _, v := range e.bodyRefs(base) {
				reuse[key("body-block", m(v)["sha256"])] = true
			}
			for _, v := range refs {
				if v != nil {
					allowed[key("body-block", m(v)["sha256"])] = true
				}
			}
			for _, v := range entries {
				if v != nil {
					entry := m(v)
					k := key("receipt-value", m(entry["value"])["sha256"])
					allowed[k] = true
					if old := m(s["primary"])[str(entry["primaryKey"])]; old != nil && same(m(old)["entry"], entry) {
						reuse[k] = true
					}
				}
			}
			for k, source := range m(row["accepted"]) {
				need(allowed[k] && m(s["objects"])[k] != nil && (source == "received" || source == "reused" && reuse[k]))
			}
			need(same(t["progress"], e.progress(row)))
			e.planned(row)
		} else {
			need(row["baseRoot"] == nil && len(m(row["accepted"])) == 0)
			if t["status"] == "committed" {
				r := m(t["result"])
				rootCheck(r, false)
				need(same(r["body"], b["body"]) && generations[str(r["generation"])] == nil)
				generations[str(r["generation"])] = r
			} else {
				need(t["status"] == "rejected")
				rootCheck(m(t["rejection"])["observedRoot"], false)
			}
		}
	}
	if s["root"] == nil {
		need(len(generations) == 0)
	} else {
		r := m(s["root"])
		need(same(generations[str(r["generation"])], r) && str(r["generation"]) == strconv.Itoa(len(generations)))
	}
	e.checkCapacity()
	return nil
}
