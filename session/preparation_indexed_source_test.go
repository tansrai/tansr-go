package session

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/tansrai/tansr-go/executor"
	"github.com/tansrai/tansr-go/terminalpersistence"
)

func TestPreparationOwnsActualIndexedSource(t *testing.T) {
	scope := executor.Scope{ApplicationScopeID: "app", EndUserID: "user", AuthorizationRevision: "1"}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	options := terminalpersistence.Options{Path: filepath.Join(directory, "source.bin"), Mode: "create", Key: bytes.Repeat([]byte{5}, 32), Identity: terminalpersistence.Identity{ApplicationScopeID: "app", EndUserID: "user", SourceID: "source", SourceGeneration: "1", DomainKey: "domain"}, CurrentScope: func() (executor.Scope, error) { return scope, nil }}
	source, err := terminalpersistence.OpenIndexedFileStore(options)
	if err != nil {
		t.Fatal(err)
	}
	source.Close()
	options.Mode = "reopen"
	ready := false
	client, execution := preparationClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !ready {
			t.Fatal("request before actual source readiness")
		}
		preparationReply(t, w, r)
	})
	prepared, err := client.Prepare(context.Background(), CreateOptions{RequestID: "indexed-source-key"}, PreparationOptions{Execution: execution, PreparePersistence: func(ctx context.Context) (PersistenceResource, error) {
		var err error
		source, err = terminalpersistence.OpenIndexedFileStore(options)
		ready = err == nil
		return source, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = prepared.Stop(); err != nil {
		t.Fatal(err)
	}
	if err = source.Audit(context.Background()); !errors.Is(err, terminalpersistence.ErrClosed) {
		t.Fatalf("provider was not closed: %v", err)
	}
}
