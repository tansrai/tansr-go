package manifestgen

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "write api/operations_gen.go from contract/api-manifest.json")

func TestRender(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "contract", "api-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	source, err := Render(raw)
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, want := range []string{
		"package api", "const ManifestRevision = 17",
		`Path: "/api/sessions/:id/tool-results/:targetId", Params: []string{"id", "targetId"}`,
		`OpApprovalCredentialSubmit, Method: "POST", Path: "/api/approvals/:id/credential"`,
		`ETagPath: []string{"configuration", "revision"}, ExpectedRevision: &ExpectedRevision{Path: []string{"expectedRevision"}, Kind: "integer"}`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("rendered source lacks %q", want)
		}
	}
	for _, want := range []string{`"sdk2-ext-v1":\s+\[\]string\{"request", "requestId"\}`, `"agent-session-v1":\s+nil`, `"terminal-services-v1":\s+\[\]string\{"requestId"\}`} {
		if !regexp.MustCompile(want).MatchString(text) {
			t.Fatalf("rendered FamilyRequestIDPaths lacks %s", want)
		}
	}
	if !regexp.MustCompile(`OpDiscoveryManifest\s+= "discovery\.manifest"`).MatchString(text) {
		t.Fatal("rendered source lacks the OpDiscoveryManifest constant")
	}
	if strings.Contains(text, "/v2/") || strings.Contains(text, "/v3/") {
		t.Fatal("rendered source must not contain legacy prefixes")
	}
	if *update {
		out := filepath.Join("..", "..", "api", "operations_gen.go")
		if err := os.WriteFile(out, source, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", out)
	}
}

func TestConstName(t *testing.T) {
	cases := map[string]string{
		"discovery.manifest":               "OpDiscoveryManifest",
		"session.message.send":             "OpSessionMessageSend",
		"terminal.executor.events.observe": "OpTerminalExecutorEventsObserve",
		"cache.core.diagnostics":           "OpCacheCoreDiagnostics",
	}
	for in, want := range cases {
		if got := ConstName(in); got != want {
			t.Errorf("ConstName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := strings.Join(TemplateParams("/api/archive/bindings/:id/materials/:targetId/uploads/:uploadId/chunks"), ","); got != "id,targetId,uploadId" {
		t.Fatalf("TemplateParams: %s", got)
	}
}

func TestRenderRejectsInconsistentManifest(t *testing.T) {
	bad := `{"format":"tansr-api-manifest-v1","contract":"unified-v1","revision":1,"schemaHash":"x","operations":[{"name":"a.b","domain":"session","family":"f","method":"GET","apiPath":"/v2/x","aliases":[],"kind":"read","sse":false,"query":[]}]}`
	if _, err := Render([]byte(bad)); err == nil {
		t.Fatal("legacy apiPath must be rejected")
	}
	bad = `{"format":"tansr-api-manifest-v1","contract":"unified-v1","revision":1,"schemaHash":"x","operations":[{"name":"a.b","domain":"session","family":"f","method":"GET","apiPath":"/api/x","aliases":[],"kind":"stream","sse":false,"query":[]}]}`
	if _, err := Render([]byte(bad)); err == nil {
		t.Fatal("kind/sse mismatch must be rejected")
	}
	if _, err := Render([]byte(`{"format":"other"}`)); err == nil {
		t.Fatal("unknown format must be rejected")
	}
	// revision 7 facts: expectedRevision on a read, empty key paths and unregistered families are rejected
	families := `"families":[{"id":"f","requestIdPath":["requestId"]}]`
	bad = `{"format":"tansr-api-manifest-v1","contract":"unified-v1","revision":1,"schemaHash":"x",` + families + `,"operations":[{"name":"a.b","domain":"session","family":"f","method":"GET","apiPath":"/api/x","aliases":[],"kind":"read","sse":false,"query":[],"etagPath":null,"expectedRevision":{"path":["expectedRevision"],"kind":"sequence"}}]}`
	if _, err := Render([]byte(bad)); err == nil {
		t.Fatal("expectedRevision on a read operation must be rejected")
	}
	bad = `{"format":"tansr-api-manifest-v1","contract":"unified-v1","revision":1,"schemaHash":"x",` + families + `,"operations":[{"name":"a.b","domain":"session","family":"f","method":"GET","apiPath":"/api/x","aliases":[],"kind":"read","sse":false,"query":[],"etagPath":[],"expectedRevision":null}]}`
	if _, err := Render([]byte(bad)); err == nil {
		t.Fatal("empty etagPath must be rejected")
	}
	bad = `{"format":"tansr-api-manifest-v1","contract":"unified-v1","revision":1,"schemaHash":"x","families":[],"operations":[{"name":"a.b","domain":"session","family":"f","method":"GET","apiPath":"/api/x","aliases":[],"kind":"read","sse":false,"query":[],"etagPath":null,"expectedRevision":null}]}`
	if _, err := Render([]byte(bad)); err == nil {
		t.Fatal("operation family missing from families[] must be rejected")
	}
	good := `{"format":"tansr-api-manifest-v1","contract":"unified-v1","revision":1,"schemaHash":"x",` + families + `,"operations":[{"name":"a.b","domain":"session","family":"f","method":"POST","apiPath":"/api/x","aliases":[],"kind":"write","sse":false,"query":[],"etagPath":["revision"],"expectedRevision":{"path":["expectedRevision"],"kind":"sequence"}}]}`
	if _, err := Render([]byte(good)); err != nil {
		t.Fatalf("minimal revision-7 manifest must render: %v", err)
	}
}
