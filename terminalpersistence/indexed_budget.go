package terminalpersistence

// Reserve pending completion under the same lock as the authenticated head.
// Immutable Patricia versions and unpublished objects are real physical cost:
// they remain charged, unlike logical collected objects. This deliberately uses
// the shared worst-case 257-node proof bound, not an assumed average hash depth.
// No directory inventory is needed to admit or continue an original transfer.
func indexedCompletionReservation(state obj, leafCount int) (bytes, writes int64) {
	overhead := int64(len(indexedMagic) + 28)
	rootObjects := int64(0)
	maximumBody := 0
	if state["root"] != nil {
		body := m(m(state["root"])["body"])
		rootObjects = int64(n(body["blockCount"]) + len(arr(body["pageHashes"])))
		maximumBody = len(enc(body))
	}
	newLeaves := int64(leafCount)
	for _, value := range m(state["transfers"]) {
		row := m(value)
		if m(row["transfer"])["status"] == "staging" {
			b := m(row["begin"])
			newLeaves += int64(n(m(b["declared"])["objects"]) - len(m(row["accepted"])) + 2*n(m(b["index"])["addedCount"]))
			maximumBody = max(maximumBody, len(enc(b["body"])))
		}
	}
	// Exact node encoding is at most 261 bytes; leave room for every fixed field.
	nodeMaximum := int64(len(enc(obj{"kind": "branch", "key": hash(nil), "bit": 255, "left": hash(nil), "right": hash(nil)})))
	headMaximum := int64(4096 + len(enc(state["identity"])) + len(enc(state["limits"])) + maximumBody + 8*128*6)
	for _, value := range m(state["transfers"]) {
		row := m(value)
		if m(row["transfer"])["status"] != "staging" {
			continue
		}
		begin := m(row["begin"])
		declared := m(begin["declared"])
		missing := int64(n(declared["objects"]) - len(m(row["accepted"])))
		need(missing >= 0)
		// Original accepted-key strings are bounded digest/kind values. Each put can
		// update its object proof and the original transfer row. Commit can update
		// permanent dual keys/value pins and collect all old/current temporary refs.
		rowMaximum := int64(len(enc(row))) + missing*128 + 8192
		commitRows := 3*int64(n(m(begin["index"])["addedCount"])) + rootObjects + int64(n(declared["objects"])) + 1
		rowUpdates := 2*missing + commitRows
		nodes := missing*min(int64(514), 2*newLeaves-1) + min(257*commitRows, 2*newLeaves-1)
		writes += nodes + rowUpdates + missing + missing + 1
		bytes += nodes*(nodeMaximum+overhead) + rowUpdates*(1024+overhead) + missing*(MaxObject+overhead) + (missing+1)*(rowMaximum+headMaximum+overhead)
	}
	return
}
