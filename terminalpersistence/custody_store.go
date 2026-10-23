package terminalpersistence

import (
	"context"
	"encoding/base64"
	"sort"
	"strconv"

	"github.com/tansrai/tansr-go/canonical"
)

func (s *FileStore) custodyTransaction(ctx context.Context, check func() error, run func(obj) error) (err error) {
	defer caught(&err)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.uncertain {
		return ErrUnknown
	}
	if err = check(); err != nil {
		return err
	}
	before, err := s.current()
	if err != nil {
		return err
	}
	if err = s.load(); err != nil {
		return err
	}
	if s.copyOnly {
		return AdapterError("read_only_copy")
	}
	st := m(detached(s.state))
	if err = run(st); err != nil {
		return err
	}
	if err = check(); err != nil {
		return err
	}
	s.custodyAssert = check
	defer func() { s.custodyAssert = nil }()
	return s.save(st, before, ctx)
}
func (s *FileStore) custodyRead(ctx context.Context, check func() error, run func(obj) error) (err error) {
	defer caught(&err)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.uncertain {
		return ErrUnknown
	}
	if err = check(); err != nil {
		return err
	}
	if err = s.load(); err != nil {
		return err
	}
	if err = run(s.state); err != nil {
		return err
	}
	return check()
}
func migration(st obj, operation obj, role string) obj {
	v := m(st["custodyMigration"])
	need(same(v["operation"], custodyOperation(operation)) && (role == "" || v["role"] == role))
	return v
}
func migrationSeed(identity, proof obj) string {
	return custodyHash("PST-CUSTODY-DIRECTORY", identity, proof["originFormat"], proof["fixedRoot"])
}
func sortedKeys(m obj) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func portableTransfer(row, objmap obj) obj {
	received, reused := []any{}, []any{}
	actual, desc := 0, 0
	for _, k := range sortedKeys(m(row["accepted"])) {
		o := m(objmap[k])
		ref := fields(o, "kind", "sha256", "byteLength")
		if m(row["accepted"])[k] == "received" {
			received = append(received, ref)
		} else {
			reused = append(reused, ref)
		}
		actual += n(o["byteLength"])
		desc += len(enc(ref))
	}
	v := fields(row, "begin", "owner", "baseRoot", "transfer")
	v["received"], v["reused"] = received, reused
	v["reservedBytes"], v["reservedObjects"], v["reservedEntries"], v["stagingBytes"] = 0, 0, 0, 0
	if m(row["transfer"])["status"] == "staging" {
		b := m(row["begin"])
		d := m(b["declared"])
		remaining := n(d["bytes"]) - actual
		v["reservedBytes"] = remaining + metadataReserve - len(enc(fields(row, "begin", "owner", "baseRoot", "transfer"))) - desc
		v["reservedObjects"] = n(d["objects"]) - len(m(row["accepted"]))
		v["reservedEntries"] = n(m(b["index"])["addedCount"])
		v["stagingBytes"] = remaining + n(m(m(row["transfer"])["progress"])["receivedBytes"])
	}
	need(custodyValidate("TransferMetadata", v) == nil)
	return v
}
func (s *FileStore) freezeCustody(ctx context.Context, r obj, fence string, check func() error) (proof obj, err error) {
	err = s.custodyTransaction(ctx, check, func(st obj) error {
		if old, ok := st["custodyMigration"].(map[string]any); ok && same(old["operation"], custodyOperation(r)) {
			need(old["role"] == "source" && old["phase"] == "frozen")
			proof = m(old["proof"])
			return nil
		}
		if old := st["custodyMigration"]; old != nil {
			phase := m(old)["phase"]
			need(phase == "published" || phase == "aborted")
		}
		need(same(r["source"], s.custodySelection()))
		root := st["root"]
		var commit any
		if root != nil {
			commit = m(root)["commitRoot"]
		}
		need(same(commit, r["expectedRoot"]))
		objects := obj{}
		for k, v := range m(st["objects"]) {
			objects[k] = detached(v)
		}
		store := func(kind string, b []byte) obj {
			need(len(b) <= 524288)
			ref := obj{"kind": kind, "sha256": hash(b), "byteLength": len(b)}
			v := m(detached(ref))
			v["base64"] = base64.StdEncoding.EncodeToString(b)
			objects[key(kind, ref["sha256"])] = v
			return ref
		}
		control := obj{"format": "terminal-persistence-control-v1", "capacity": m((&engine{state: st}).capacity())["used"], "limits": st["limits"]}
		need(custodyValidate("ControlState", control) == nil)
		first := obj{"kind": "root", "root": root, "controlRef": store("custody-control", enc(control))}
		fixed := custodyHash("PST-CUSTODY-SNAPSHOT", st["identity"], first)
		records := []any{first}
		ordered := []obj{}
		for _, v := range m(st["primary"]) {
			ordered = append(ordered, m(v))
		}
		sort.Slice(ordered, func(i, j int) bool {
			a, _ := strconv.ParseInt(str(ordered[i]["ordinal"]), 10, 64)
			b, _ := strconv.ParseInt(str(ordered[j]["ordinal"]), 10, 64)
			return a < b
		})
		for _, v := range ordered {
			records = append(records, obj{"kind": "receipt-index", "ordinal": v["ordinal"], "entry": v["entry"]})
		}
		for _, id := range sortedKeys(m(st["transfers"])) {
			row := m(m(st["transfers"])[id])
			meta := portableTransfer(row, m(st["objects"]))
			records = append(records, obj{"kind": "transfer", "transferId": id, "intentSha256": m(row["begin"])["intentSha256"], "status": m(row["transfer"])["status"], "metadataRef": store("custody-transfer", enc(meta))})
		}
		total := 0
		for _, k := range sortedKeys(objects) {
			o := m(objects[k])
			total += n(o["byteLength"])
			records = append(records, obj{"kind": "object", "object": fields(o, "kind", "sha256", "byteLength")})
		}
		proof = obj{"originFormat": Contract, "fixedRoot": fixed, "originalCommitRoot": commit, "indexRoot": nil, "indexCount": "0", "inventoryDigest": "", "records": len(records), "objects": len(objects), "bytes": total}
		if root != nil {
			proof["indexRoot"] = m(m(root)["index"])["root"]
			proof["indexCount"] = m(m(root)["index"])["count"]
		}
		d := migrationSeed(m(st["identity"]), proof)
		for i, v := range records {
			need(custodyValidate("Record", v) == nil)
			d = custodyHash("PST-CUSTODY-RECORD", strconv.Itoa(i), d, v)
		}
		proof["inventoryDigest"] = d
		if e := custodyProofBounds(proof, s.options.MigrationBudget); e != nil {
			return e
		}
		st["custodyMigration"] = obj{"operation": custodyOperation(r), "role": "source", "phase": "frozen", "proof": proof, "writerFence": fence, "records": records, "objects": objects, "digest": d, "complete": true, "budget": s.options.MigrationBudget}
		return nil
	})
	return
}
func (s *FileStore) exportCustodyPage(ctx context.Context, r obj, check func() error) (page, cursor obj, err error) {
	err = s.custodyRead(ctx, check, func(st obj) error {
		state := migration(st, r, "source")
		need(state["phase"] == "frozen" || state["phase"] == "published")
		proof := m(state["proof"])
		records := arr(state["records"])
		index := 0
		previous := migrationSeed(m(st["identity"]), proof)
		if r["cursor"] != nil {
			c := m(r["cursor"])
			need(same(custodyOperation(c), custodyOperation(r)) && c["fixedRoot"] == proof["fixedRoot"] && c["inventoryDigest"] == proof["inventoryDigest"])
			i, e := strconv.Atoi(str(c["index"]))
			need(e == nil && i >= 0 && i < len(records))
			index = i
		}
		for i := 0; i < index; i++ {
			previous = custodyHash("PST-CUSTODY-RECORD", strconv.Itoa(i), previous, records[i])
		}
		if r["cursor"] != nil {
			need(m(r["cursor"])["previousDigest"] == previous)
		}
		chunk := []any{}
		d := previous
		for i := index; i < len(records) && len(chunk) < n(r["limit"]); i++ {
			if len(enc(append(chunk, records[i]))) > 28000 {
				break
			}
			chunk = append(chunk, records[i])
			d = custodyHash("PST-CUSTODY-RECORD", strconv.Itoa(i), d, records[i])
		}
		need(len(chunk) > 0)
		next := index + len(chunk)
		page = obj{"index": strconv.Itoa(index), "previousDigest": previous, "sha256": d, "records": chunk, "complete": next == len(records)}
		if next < len(records) {
			cursor = custodyOperation(r)
			cursor["fixedRoot"], cursor["inventoryDigest"], cursor["index"], cursor["previousDigest"] = proof["fixedRoot"], proof["inventoryDigest"], strconv.Itoa(next), d
		}
		return nil
	})
	return
}
func custodyObject(state, objref obj) []byte {
	o := m(m(state["objects"])[key(str(objref["kind"]), objref["sha256"])])
	need(same(fields(o, "kind", "sha256", "byteLength"), objref))
	b, e := base64.StdEncoding.Strict().DecodeString(str(o["base64"]))
	need(e == nil && len(b) == n(o["byteLength"]) && hash(b) == o["sha256"])
	return b
}
func (s *FileStore) exportCustodyObject(ctx context.Context, r obj, check func() error) (chunk obj, err error) {
	err = s.custodyRead(ctx, check, func(st obj) error {
		state := migration(st, r, "source")
		ref := m(r["object"])
		b := custodyObject(state, ref)
		offset := n(r["offset"])
		need(offset < len(b))
		part := b[offset:min(len(b), offset+n(r["length"]))]
		chunk = obj{"object": ref, "offset": offset, "byteLength": len(part), "base64": base64.StdEncoding.EncodeToString(part), "payloadDigest": hash(part)}
		return nil
	})
	return
}
func (s *FileStore) importCustodyPage(ctx context.Context, ticket, page obj, check func() error) error {
	if e := validateCustodyPage(page); e != nil {
		return e
	}
	return s.custodyTransaction(ctx, check, func(st obj) error {
		selection := s.custodySelection()
		dest := m(ticket["destination"])
		need(dest["custodyId"] == selection.CustodyID && dest["mode"] == selection.Mode && same(ticket["identity"], st["identity"]))
		proof := m(ticket["proof"])
		if e := custodyProofBounds(proof, s.options.MigrationBudget); e != nil {
			return e
		}
		var state obj
		if old := st["custodyMigration"]; old != nil {
			state = m(old)
			if !same(state["operation"], custodyOperation(ticket)) {
				need(state["phase"] == "published" || state["phase"] == "aborted")
				state = nil
			}
		}
		if state == nil {
			need(st["root"] == nil && len(m(st["primary"])) == 0 && len(m(st["transfers"])) == 0 && len(m(st["objects"])) == 0)
			state = obj{"operation": custodyOperation(ticket), "role": "target", "phase": "staging", "proof": proof, "records": []any{}, "objects": obj{}, "digest": migrationSeed(m(st["identity"]), proof), "complete": false, "budget": s.options.MigrationBudget}
			st["custodyMigration"] = state
		}
		need(state["role"] == "target" && same(state["proof"], proof))
		records := arr(state["records"])
		index, e := strconv.Atoi(str(page["index"]))
		need(e == nil && index >= 0)
		if index < len(records) {
			incoming := arr(page["records"])
			need(index+len(incoming) <= len(records) && same(records[index:index+len(incoming)], incoming))
			return nil
		}
		need(state["phase"] == "staging" && state["complete"] == false && index == len(records) && page["previousDigest"] == state["digest"])
		for _, v := range arr(page["records"]) {
			if len(records) == 0 {
				first := m(v)
				need(first["kind"] == "root" && custodyHash("PST-CUSTODY-SNAPSHOT", st["identity"], first) == proof["fixedRoot"])
			}
			record := m(v)
			if record["kind"] == "object" {
				ref := m(record["object"])
				k := key(str(ref["kind"]), ref["sha256"])
				need(m(state["objects"])[k] == nil)
				o := m(detached(ref))
				o["base64"] = ""
				m(state["objects"])[k] = o
			}
			records = append(records, detached(v))
		}
		need(len(records) <= n(proof["records"]))
		total := 0
		for _, v := range m(state["objects"]) {
			total += n(m(v)["byteLength"])
		}
		need(total <= n(proof["bytes"]) && len(m(state["objects"])) <= n(proof["objects"]))
		state["records"], state["digest"], state["complete"] = records, page["sha256"], page["complete"]
		if page["complete"] == true {
			need(len(records) == n(proof["records"]) && page["sha256"] == proof["inventoryDigest"] && total == n(proof["bytes"]) && len(m(state["objects"])) == n(proof["objects"]))
		}
		return nil
	})
}
func (s *FileStore) importCustodyObject(ctx context.Context, ticket, chunk obj, check func() error) error {
	piece, e := validateCustodyChunk(chunk)
	if e != nil {
		return e
	}
	return s.custodyTransaction(ctx, check, func(st obj) error {
		state := migration(st, ticket, "target")
		need(same(state["proof"], ticket["proof"]))
		ref := m(chunk["object"])
		o := m(m(state["objects"])[key(str(ref["kind"]), ref["sha256"])])
		need(same(fields(o, "kind", "sha256", "byteLength"), ref))
		b, e := base64.StdEncoding.Strict().DecodeString(str(o["base64"]))
		need(e == nil)
		offset := n(chunk["offset"])
		if offset < len(b) {
			need(offset+len(piece) <= len(b) && string(b[offset:offset+len(piece)]) == string(piece))
			return nil
		}
		need(state["phase"] == "staging" && offset == len(b))
		b = append(b, piece...)
		need(len(b) <= n(ref["byteLength"]))
		if len(b) == n(ref["byteLength"]) {
			need(hash(b) == ref["sha256"])
		}
		o["base64"] = base64.StdEncoding.EncodeToString(b)
		return nil
	})
}
func (s *FileStore) verifyCustody(ctx context.Context, ticket obj, check func() error) error {
	return s.custodyTransaction(ctx, check, func(st obj) error {
		state := migration(st, ticket, "target")
		proof := m(state["proof"])
		need(same(proof, ticket["proof"]))
		if state["phase"] == "verified" || state["phase"] == "published" {
			return nil
		}
		need(state["phase"] == "staging" && state["complete"] == true && state["digest"] == proof["inventoryDigest"])
		records := arr(state["records"])
		need(len(records) == n(proof["records"]))
		first := m(records[0])
		need(first["kind"] == "root")
		root := first["root"]
		var commit, index any
		count := "0"
		if root != nil {
			commit = m(root)["commitRoot"]
			index = m(m(root)["index"])["root"]
			count = str(m(m(root)["index"])["count"])
		}
		need(same(commit, proof["originalCommitRoot"]) && same(index, proof["indexRoot"]) && count == proof["indexCount"])
		controlBytes := custodyObject(state, m(first["controlRef"]))
		c, e := canonical.ParseStrict(controlBytes, canonical.Options{MaxBytes: 524288})
		need(e == nil && custodyValidate("ControlState", c) == nil)
		control := m(c)
		// The destination's configured business/physical capacity is not enlarged by migration.
		primary, secondary, transfers, objects := obj{}, obj{}, obj{}, obj{}
		total := 0
		objcount := 0
		previousObject := ""
		previousTransfer := ""
		phase := 0
		for _, v := range records[1:] {
			r := m(v)
			switch r["kind"] {
			case "receipt-index":
				need(phase == 0)
				entry := m(r["entry"])
				pk, sk := str(entry["primaryKey"]), str(entry["secondaryKey"])
				need(primary[pk] == nil && secondary[sk] == nil && r["ordinal"] == strconv.Itoa(len(primary)+1))
				primary[pk] = fields(r, "ordinal", "entry")
				secondary[sk] = pk
			case "transfer":
				need(phase <= 1)
				phase = 1
				id := str(r["transferId"])
				need(id > previousTransfer)
				previousTransfer = id
				b := custodyObject(state, m(r["metadataRef"]))
				value, e := canonical.ParseStrict(b, canonical.Options{MaxBytes: 524288})
				need(e == nil && custodyValidate("TransferMetadata", value) == nil)
				meta := m(value)
				need(m(meta["begin"])["transferId"] == id && m(meta["transfer"])["intentSha256"] == r["intentSha256"] && m(meta["transfer"])["status"] == r["status"])
				row := fields(meta, "begin", "owner", "baseRoot", "transfer")
				accepted := obj{}
				for _, field := range []string{"received", "reused"} {
					prior := ""
					for _, v := range arr(meta[field]) {
						o := m(v)
						k := key(str(o["kind"]), o["sha256"])
						need(k > prior && accepted[k] == nil)
						prior = k
						_ = custodyObject(state, o)
						accepted[k] = field
					}
				}
				row["accepted"] = accepted
				need(same(portableTransfer(row, m(state["objects"])), meta))
				transfers[id] = row
			case "object":
				phase = 2
				ref := m(r["object"])
				k := key(str(ref["kind"]), ref["sha256"])
				need(k > previousObject)
				previousObject = k
				b := custodyObject(state, ref)
				total += len(b)
				objcount++
				if ref["kind"] != "custody-control" && ref["kind"] != "custody-transfer" {
					o := m(detached(ref))
					o["base64"] = base64.StdEncoding.EncodeToString(b)
					objects[k] = o
				}
			default:
				return ErrIntegrity
			}
		}
		need(total == n(proof["bytes"]) && objcount == n(proof["objects"]) && len(m(state["objects"])) == objcount)
		st["root"], st["primary"], st["secondary"], st["transfers"], st["objects"] = root, primary, secondary, transfers, objects
		work := &engine{state: st}
		need(same(m(work.capacity())["used"], control["capacity"]))
		work.checkCapacity()
		if e := work.audit(s.Identity(), s.options.Limits); e != nil {
			return e
		}
		if e := s.reserveAdmission(st); e != nil {
			return e
		}
		state["phase"] = "verified"
		return nil
	})
}
func (s *FileStore) activateCustody(ctx context.Context, ticket obj, check func() error) error {
	return s.custodyTransaction(ctx, check, func(st obj) error {
		state := migration(st, ticket, "")
		need(ticket["phase"] == "published" && same(state["proof"], ticket["proof"]))
		if state["role"] == "target" {
			need(state["phase"] == "verified" || state["phase"] == "published")
			selected, destination := m(ticket["selectedCustody"]), m(ticket["destination"])
			need(selected["custodyId"] == destination["custodyId"] && selected["mode"] == destination["mode"])
			st["custodySelection"] = ticket["selectedCustody"]
		} else {
			need(state["phase"] == "frozen" || state["phase"] == "published")
		}
		state["phase"] = "published"
		return nil
	})
}
func (s *FileStore) abortCustody(ctx context.Context, ticket obj, check func() error) error {
	return s.custodyTransaction(ctx, check, func(st obj) error {
		state := migration(st, ticket, "")
		need(ticket["phase"] == "aborted" && state["phase"] != "published")
		state["phase"] = "aborted"
		return nil
	})
}
