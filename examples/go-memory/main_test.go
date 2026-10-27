package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"github.com/tansrai/tansr-go/executor"
	"github.com/tansrai/tansr-go/memorypublication"
	"github.com/tansrai/tansr-go/terminalpersistence"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReopenDefaultAndMissingInputs(t *testing.T) {
	var o options
	f := flag.NewFlagSet("go-memory", flag.ContinueOnError)
	flags(f, &o)
	if e := f.Parse(nil); e != nil {
		t.Fatal(e)
	}
	if o.mode != "reopen" {
		t.Fatal("unsafe create default")
	}
	if e := run(context.Background(), o); e == nil {
		t.Fatal("missing identity accepted")
	}
}
func TestHostConfigurationRejectsAmbiguousAndUnknownFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "scope.json")
	for _, raw := range []string{`{"v":"a","v":"b"}`, `{"unexpected":"x"}`} {
		if e := os.WriteFile(p, []byte(raw), 0600); e != nil {
			t.Fatal(e)
		}
		var v struct {
			V string `json:"v"`
		}
		if e := readJSON(p, &v); e == nil {
			t.Fatal("ambiguous host configuration")
		}
	}
}

func TestOfflineRekeyStepsRetainOriginalsAndPartialPair(t *testing.T) {
	directory := t.TempDir()
	scope := executor.Scope{ApplicationScopeID: "app", EndUserID: "user", AuthorizationRevision: "1"}
	identity := memorypublication.Identity{ApplicationScopeID: "app", EndUserID: "user", SourceID: "source", SourceGeneration: "1", DomainKey: "domain"}
	o := options{executor: "device", file: filepath.Join(directory, "memory.bin"), journal: filepath.Join(directory, "journal"), access: filepath.Join(directory, "scope.json"), identity: filepath.Join(directory, "identity.json")}
	for path, value := range map[string]any{o.access: scope, o.identity: identity} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	oldKey, newKey := bytes.Repeat([]byte{4}, 32), bytes.Repeat([]byte{5}, 32)
	t.Setenv("TANSR_MEMORY_KEY", hex.EncodeToString(oldKey))
	t.Setenv("TANSR_MEMORY_NEW_KEY", hex.EncodeToString(newKey))
	storeOptions := memorypublication.Options{Path: o.file, Mode: "create", Key: oldKey, Identity: identity, CurrentScope: func() (executor.Scope, error) { return scope, nil }}
	store, err := memorypublication.OpenFileStore(storeOptions)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	journalOptions := executor.JournalEncryption{Key: oldKey, ApplicationScopeID: "app", EndUserID: "user", ExecutorID: "device", CheckAccess: func() error { return nil }}
	journal, err := executor.NewEncryptedFileJournal(o.journal, journalOptions)
	if err != nil {
		t.Fatal(err)
	}
	if err = journal.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(o.file)
	if err != nil {
		t.Fatal(err)
	}
	o.mode = "rekey-publication"
	o.target = filepath.Join(directory, "next.bin")
	if err = run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	publicationTarget := o.target
	o.mode = "rekey-journal"
	o.target = filepath.Join(directory, "occupied")
	if err = os.Mkdir(o.target, 0700); err != nil {
		t.Fatal(err)
	}
	if err = run(context.Background(), o); err == nil {
		t.Fatal("existing journal target accepted")
	}
	if _, err = os.Stat(publicationTarget); err != nil {
		t.Fatal("first successful medium lost", err)
	}
	o.target = filepath.Join(directory, "next-journal")
	if err = run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(o.file)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("publication source changed", err)
	}
	storeOptions.Path = publicationTarget
	storeOptions.Mode = "reopen"
	storeOptions.Key = newKey
	store, err = memorypublication.OpenFileStore(storeOptions)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	journalOptions.Key = newKey
	journal, err = executor.NewEncryptedFileJournal(o.target, journalOptions)
	if err != nil {
		t.Fatal(err)
	}
	journal.Close()
	if err = run(context.Background(), o); err == nil {
		t.Fatal("new journal overwritten")
	}
}
func TestOfflineRekeyRequiresExplicitInputs(t *testing.T) {
	var o options
	f := flag.NewFlagSet("go-memory", flag.ContinueOnError)
	flags(f, &o)
	if err := f.Parse([]string{"-mode", "rekey-journal"}); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), o); err == nil {
		t.Fatal("missing inputs")
	}
}

type cancelAfterStageContext struct {
	context.Context
	parent string
}

func (c cancelAfterStageContext) Err() error {
	stages, _ := filepath.Glob(filepath.Join(c.parent, ".tansr-journal-import-*"))
	if len(stages) > 0 {
		return context.Canceled
	}
	return nil
}
func TestOfflineCommandCancellationPreservesMigrationFailure(t *testing.T) {
	directory := t.TempDir()
	opts := options{mode: "rekey-journal", executor: "device", file: filepath.Join(directory, "memory.bin"), journal: filepath.Join(directory, "journal"), target: filepath.Join(directory, "next-journal"), access: filepath.Join(directory, "scope.json"), identity: filepath.Join(directory, "identity.json")}
	for path, raw := range map[string]string{opts.access: `{"applicationScopeId":"app","endUserId":"user","authorizationRevision":"1"}`, opts.identity: `{"applicationScopeId":"app","endUserId":"user","sourceId":"source","sourceGeneration":"1","domainKey":"domain"}`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	key := bytes.Repeat([]byte{6}, 32)
	t.Setenv("TANSR_MEMORY_KEY", hex.EncodeToString(key))
	t.Setenv("TANSR_MEMORY_NEW_KEY", hex.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	journal, err := executor.NewEncryptedFileJournal(opts.journal, executor.JournalEncryption{Key: key, ApplicationScopeID: "app", EndUserID: "user", ExecutorID: "device", CheckAccess: func() error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = journal.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := cancelAfterStageContext{Context: context.Background(), parent: directory}
	err = runCommand(ctx, opts)
	var failure *executor.JournalMigrationError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &failure) || failure.Published || failure.StagingDirectory == "" {
		t.Fatal("offline command swallowed migration cancellation/recovery state", err)
	}
	if !strings.Contains(err.Error(), "retain encrypted stage") {
		t.Fatal("recovery instructions lost", err)
	}
	if _, err = os.Stat(failure.StagingDirectory); err != nil {
		t.Fatal("stage lost", err)
	}
	// Normal runner interruption still exits quietly, preserving the original command behavior.
	opts.mode = "reopen"
	opts.session = "session"
	opts.bindingRequest = "original-request"
	if err = runCommand(ctx, opts); err != nil {
		t.Fatal("normal interrupt behavior changed", err)
	}
}

func TestLocalStopStillPersistsKnownReceiptAndChecksCurrentScope(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "scope.json")
	scope := executor.Scope{ApplicationScopeID: "app", EndUserID: "user", AuthorizationRevision: "1"}
	writeScope := func(value executor.Scope) {
		raw, _ := json.Marshal(value)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeScope(scope)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	current, err := scopeReader(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	check := func() error {
		got, err := current()
		if err != nil {
			return err
		}
		if got != scope {
			return executor.ErrConflict
		}
		return nil
	}
	encryption := executor.JournalEncryption{
		Key: bytes.Repeat([]byte{31}, 32), ApplicationScopeID: "app", EndUserID: "user", ExecutorID: "device", CheckAccess: check}
	journal, err := executor.NewEncryptedFileJournal(filepath.Join(directory, "journal"), encryption)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	op := executor.Operation{Protocol: executor.Protocol, OperationID: "original-op", SessionID: "original-session", Scope: scope,
		Binding:  executor.Binding{BindingID: "original-binding", Revision: "1", Target: executor.Target{ExecutorID: "device", ConnectionID: "original-connection", ConnectionRevision: "1", WorkspaceID: "workspace", WorkspaceRevision: "1"}},
		ToolName: "Lookup", Request: executor.Resource{Operation: "tool.invoke", Args: map[string]any{"name": "Lookup", "definitionDigest": strings.Repeat("a", 64), "argsJson": "{}"}}, ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}
	op.Digest, err = executor.OperationDigest(op)
	if err != nil {
		t.Fatal(err)
	}
	if claim, err := journal.Claim(ctx, op); err != nil || !claim.Claimed {
		t.Fatal(claim, err)
	}
	stop() // handler has returned a known fact; outer shutdown must not erase it.
	receipt := executor.Receipt{Protocol: executor.Protocol, ExecutorID: "device", ConnectionID: "original-connection", OperationID: op.OperationID, Digest: op.Digest, Status: "completed",
		Result: &executor.Resource{Operation: "tool.invoke", Args: map[string]any{"resultJson": `{"status":"ok","content":[{"t":"text","text":"known durable result"}]}`}}}
	if err := journal.Complete(context.WithoutCancel(ctx), op, receipt); err != nil {
		t.Fatal("local stop erased known outcome", err)
	}
	if err = journal.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err = executor.NewEncryptedFileJournal(filepath.Join(directory, "journal"), encryption)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	claim, err := journal.Claim(context.Background(), op)
	if err != nil || claim.Receipt == nil || !reflect.DeepEqual(*claim.Receipt, receipt) {
		t.Fatal("original receipt absent", claim, err)
	}
	revoked := scope
	revoked.AuthorizationRevision = "2"
	writeScope(revoked)
	if _, err := journal.Claim(context.Background(), op); err == nil {
		t.Fatal("actual current authorization change accepted")
	}
	if _, err := scopeReader(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatal("already stopped initialization accepted", err)
	}
}

func TestNewProfileOfflineRekeyReturnsOnlyReadOnlyCandidate(t *testing.T) {
	testProfileOfflineRekey(t, "legacy-snapshot-v1", terminalpersistence.OpenFileStore)
}
func TestIndexedProfileOfflineRekeyUsesSelectedLayout(t *testing.T) {
	testProfileOfflineRekey(t, "authenticated-indexed-v2", terminalpersistence.OpenIndexedFileStore)
}
func testProfileOfflineRekey(t *testing.T, layout string, openSource func(terminalpersistence.Options) (*terminalpersistence.FileStore, error)) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	scope := executor.Scope{ApplicationScopeID: "app", EndUserID: "user", AuthorizationRevision: "1"}
	identity := terminalpersistence.Identity{ApplicationScopeID: "app", EndUserID: "user", SourceID: "source", SourceGeneration: "1", DomainKey: "domain"}
	o := options{profile: "terminal-persistence-v1", mode: "rekey-publication", executor: "device", file: filepath.Join(directory, "original.bin"), journal: filepath.Join(directory, "journal"), access: filepath.Join(directory, "scope.json"), identity: filepath.Join(directory, "identity.json"), target: filepath.Join(directory, "copy.bin")}
	o.layout = layout
	for path, value := range map[string]any{o.access: scope, o.identity: identity} {
		raw, _ := json.Marshal(value)
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	oldKey, newKey := bytes.Repeat([]byte{3}, 32), bytes.Repeat([]byte{4}, 32)
	t.Setenv("TANSR_MEMORY_KEY", hex.EncodeToString(oldKey))
	t.Setenv("TANSR_MEMORY_NEW_KEY", hex.EncodeToString(newKey))
	opts := terminalpersistence.Options{Path: o.file, Mode: "create", Key: oldKey, Identity: identity, CurrentScope: func() (executor.Scope, error) { return scope, nil }}
	source, err := openSource(opts)
	if err != nil {
		t.Fatal(err)
	}
	source.Close()
	original, _ := os.ReadFile(o.file)
	if err = runCommand(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(o.journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("offline copy started journal/executor", err)
	}
	after, _ := os.ReadFile(o.file)
	if !bytes.Equal(original, after) {
		t.Fatal("source changed")
	}
	opts.Path = o.target
	opts.Key = newKey
	opts.Mode = "reopen"
	copied, err := openSource(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	if !copied.CopyVerifiedCutoverPending() {
		t.Fatal("Demo copy granted writes")
	}
	if err = runCommand(context.Background(), o); err == nil {
		t.Fatal("duplicate target allowed")
	}
}
