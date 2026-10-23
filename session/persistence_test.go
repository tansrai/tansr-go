package session

import (
	"context"
	"net/http"
	"testing"
)

func TestPersistenceDiscoveryStrictModes(t *testing.T) {
	for _, mode := range []string{"terminal", "cloud"} {
		t.Run(mode, func(t *testing.T) {
			handoff := "terminal-durable-ack"
			if mode == "cloud" {
				handoff = "cloud-immutable-publish"
			}
			body := map[string]any{"contract": PersistenceContract, "sessionId": "s1", "mode": mode, "sourceId": "source", "sourceGeneration": "1", "coverage": []string{"history", "control"}, "localDisk": false, "handoff": map[string]any{"archive": handoff, "trustedControl": true, "executionJournal": mode == "cloud", "inputReceipts": true}}
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) { response(w, "session", 200, body) })
			if got, err := ref(c).Persistence(context.Background()); err != nil || got.Mode != mode {
				t.Fatal(got, err)
			}
			delete(body["handoff"].(map[string]any), "inputReceipts")
			if _, err := ref(c).Persistence(context.Background()); err == nil {
				t.Fatal("missing required false-valued field accepted")
			}
			body["handoff"].(map[string]any)["inputReceipts"] = true
			body["sessionId"] = "another"
			if _, err := ref(c).Persistence(context.Background()); err == nil {
				t.Fatal("wrong session accepted")
			}
		})
	}
	var out map[string]any
	if err := strictPersistence([]byte(`{"sessionId":"s1","sessionId":"s2"}`), &out, "sessionId"); err == nil {
		t.Fatal("duplicate key accepted")
	}
	if err := strictPersistence([]byte(`{"b":true,"a":1}`), &out, "a", "b"); err != nil {
		t.Fatal("ordinary JSON incorrectly required canonical bytes", err)
	}
}
