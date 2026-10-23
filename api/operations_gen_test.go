package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tansrai/tansr-go/internal/manifestgen"
)

var update = flag.Bool("update", false, "rewrite operations_gen.go from contract/api-manifest.json")

// TestOperationsGenerated is the generator's -check gate: operations_gen.go must equal the rendering
// of the vendored manifest byte for byte, and the embedded source hash must match the file.
func TestOperationsGenerated(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "contract", "api-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := manifestgen.Render(raw)
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile("operations_gen.go", want, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("operations_gen.go rewritten")
	}
	got, err := os.ReadFile("operations_gen.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("operations_gen.go is stale: run `go run ./internal/gen/manifest2go` (or `go test ./api -run TestOperationsGenerated -update`)")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != ManifestSourceSHA256 {
		t.Fatalf("ManifestSourceSHA256 %s differs from manifest file %s", ManifestSourceSHA256, hex.EncodeToString(sum[:]))
	}
	if len(operations) != 91 {
		t.Fatalf("expected 91 operations, got %d", len(operations))
	}
	if len(ClosureOperationNames()) != 87 {
		t.Fatalf("expected 87 closure operations, got %d", len(ClosureOperationNames()))
	}
	// revision 7 three-header facts are carried verbatim (U7-HDR §3 inventory): 15 versioned operations
	// (archive 6 / cache 6 / terminal 3 carry an ETag), 9 write operations accept If-Match (archive 3 /
	// cache 4 / terminal 2, of which terminal.configuration.commit is the only integer kind), 11 families.
	versioned, ifMatch := 0, 0
	for _, op := range operations {
		if op.Versioned() {
			versioned++
		}
		if op.AcceptsIfMatch() {
			ifMatch++
			if op.Kind != "write" {
				t.Fatalf("%s: expectedRevision on a %s operation", op.Name, op.Kind)
			}
		}
	}
	if versioned != 15 || ifMatch != 9 {
		t.Fatalf("three-header facts: %d versioned / %d if-match operations", versioned, ifMatch)
	}
	if len(FamilyRequestIDPaths) != 12 {
		t.Fatalf("expected 12 registered families, got %d", len(FamilyRequestIDPaths))
	}
	approval, ok := Lookup(OpApprovalCredentialSubmit)
	if !ok || approval.Method != "POST" || approval.Path != "/api/approvals/:id/credential" || approval.Kind != "write" || approval.Family != "agent-session-v1" {
		t.Fatalf("approval.credential.submit: %+v", approval)
	}
	commit, _ := Lookup(OpTerminalConfigurationCommit)
	if commit.ExpectedRevision == nil || commit.ExpectedRevision.Kind != "integer" || strings.Join(commit.ETagPath, ".") != "configuration.revision" {
		t.Fatalf("terminal.configuration.commit facts: %+v", commit)
	}
	if p := mustOp(t, OpArchiveAckCommit).RequestIDPath(); strings.Join(p, ".") != "request.requestId" {
		t.Fatalf("sdk2-ext-v1 requestIdPath: %v", p)
	}
	if p := approval.RequestIDPath(); p != nil {
		t.Fatalf("agent-session-v1 has no requestIdPath, got %v", p)
	}
}
