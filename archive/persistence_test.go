package archive

import "testing"

func TestSourceManagedIdentityCannotDrift(t *testing.T) {
	b, s, _, _ := fixture(t)
	p := &SourcePersistence{Contract: "serve-persistence-v1", Mode: "cloud", SourceID: b.SourceID, SourceGeneration: s.SourceGeneration, Handoff: "cloud-immutable-publish", TrustedControl: true}
	b.Availability, b.Persistence, s.Persistence = "source-managed", p, p
	s.SessionPersistence = "source-managed"
	if _, err := IdentityFrom(b, s); err != nil {
		t.Fatal("source-managed identity", err)
	}
	bad := *p
	bad.SourceID = "another"
	b.Persistence = &bad
	if err := validateBinding(b); err == nil {
		t.Fatal("outer and inner source mismatch accepted")
	}
	c := NewClient(nil)
	if err := c.observePersistence("binding", p); err != nil {
		t.Fatal(err)
	}
	bad = *p
	bad.Mode, bad.Handoff = "terminal", "terminal-durable-ack"
	if err := c.observePersistence("binding", &bad); err == nil {
		t.Fatal("mode drift accepted")
	}
	if err := c.observePersistence("binding", nil); err == nil {
		t.Fatal("source-managed downgraded to legacy")
	}
}
