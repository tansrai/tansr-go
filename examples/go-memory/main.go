// Command go-memory runs a dedicated encrypted publication host for an existing
// Serve session. Memory extraction and selection remain entirely in Serve.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"

	"github.com/tansrai/tansr-go/api"
	"github.com/tansrai/tansr-go/canonical"
	"github.com/tansrai/tansr-go/examples/internal/demoutil"
	"github.com/tansrai/tansr-go/executor"
	"github.com/tansrai/tansr-go/internal/wire"
	"github.com/tansrai/tansr-go/memorypublication"
	"github.com/tansrai/tansr-go/terminalpersistence"
)

type options struct{ base, session, executor, workspace, file, journal, mode, access, identity, bindingRequest, target, profile, layout string }

func flags(f *flag.FlagSet, o *options) {
	f.StringVar(&o.profile, "profile", "memory-publication", "memory-publication or terminal-persistence-v1; new profile requires an independent media path and TANSR_JOURNAL_KEY")
	f.StringVar(&o.layout, "layout", "legacy-snapshot-v1", "terminal-persistence-v1 media: legacy-snapshot-v1 or authenticated-indexed-v2; use the same explicit value for create/reopen")
	f.StringVar(&o.base, "base", "http://127.0.0.1:8787", "Serve origin")
	f.StringVar(&o.session, "session", "", "existing session selected by trusted controller (required)")
	f.StringVar(&o.executor, "executor", "", "executor identity authorized by Serve (required)")
	f.StringVar(&o.workspace, "workspace", "go-memory", "logical workspace, not a filesystem path")
	f.StringVar(&o.file, "file", "", "private encrypted publication file (required)")
	f.StringVar(&o.journal, "journal", "", "private encrypted execution journal directory (required)")
	f.StringVar(&o.mode, "mode", "reopen", "create, reopen, rekey-publication or rekey-journal (offline)")
	f.StringVar(&o.target, "target", "", "absent destination for one offline rekey step")
	f.StringVar(&o.access, "access-file", "", "host-controlled live authorization Scope JSON (required)")
	f.StringVar(&o.identity, "identity-file", "", "host-provided publication Identity JSON (required)")
	f.StringVar(&o.bindingRequest, "binding-request", "", "original terminal binding request ID (required; retain on lost reply)")
}
func main() {
	var o options
	flags(flag.CommandLine, &o)
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if e := runCommand(ctx, o); e != nil {
		fmt.Fprintln(os.Stderr, "go-memory:", demoutil.Text(demoutil.Describe(e).Error()))
		os.Exit(1)
	}
}

// runCommand applies the command's interrupt policy after run has preserved errors.
func offlineRekeyMode(mode string) bool {
	return mode == "rekey-publication" || mode == "rekey-journal"
}
func runCommand(ctx context.Context, o options) error {
	err := run(ctx, o)
	if !offlineRekeyMode(o.mode) && errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
func readJSON(path string, value any) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, 8193))
	if e != nil || len(raw) > 8192 {
		return errors.New("bounded host configuration required")
	}
	if _, e = canonical.Decode(raw, canonical.Options{MaxBytes: 8192}); e != nil {
		return e
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(value)
}

// scopeReader reads the current trusted authorization independently of receipt delivery.
func scopeReader(ctx context.Context, path string) (func() (executor.Scope, error), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Cancellation stops new execution, while an already known outcome still needs
	// durable storage. Current authorization is independently reread for every IO.
	return func() (executor.Scope, error) {
		var scope executor.Scope
		if err := readJSON(path, &scope); err != nil {
			return scope, err
		}
		if err := wire.Validate("sdk2-ext-v1", "Scope", scope); err != nil {
			return scope, err
		}
		return scope, nil
	}, nil
}

func run(ctx context.Context, o options) error {
	migrating := offlineRekeyMode(o.mode)
	if o.profile == "" {
		o.profile = "memory-publication"
	}
	persistence := o.profile == "terminal-persistence-v1"
	openPersistence := terminalpersistence.OpenFileStore
	if o.layout == "authenticated-indexed-v2" && persistence {
		openPersistence = terminalpersistence.OpenIndexedFileStore
	} else if o.layout != "" && o.layout != "legacy-snapshot-v1" {
		return errors.New("unsupported explicit persistence layout")
	}
	if o.profile != "memory-publication" && !persistence {
		return errors.New("unsupported explicit storage profile")
	}
	if persistence && o.mode == "rekey-journal" {
		return errors.New("new profile does not use legacy rekey media")
	}
	if o.mode != "create" && o.mode != "reopen" && !migrating {
		return errors.New("mode must be create, reopen, rekey-publication or rekey-journal")
	}
	if o.executor == "" || o.file == "" || o.journal == "" || o.access == "" || o.identity == "" {
		return errors.New("executor, file, journal, access-file and identity-file are required")
	}
	if migrating && o.target == "" {
		return errors.New("offline rekey requires an absent target; stop and drain all writers first")
	}
	if !migrating && (o.session == "" || o.bindingRequest == "") {
		return errors.New("session and original binding-request are required")
	}
	key, e := hex.DecodeString(os.Getenv("TANSR_MEMORY_KEY"))
	if e != nil || len(key) != 32 {
		return errors.New("TANSR_MEMORY_KEY must be 64 hex characters from the host key store")
	}
	defer clear(key)
	var identity memorypublication.Identity
	if e = readJSON(o.identity, &identity); e != nil {
		return e
	}
	readCurrent, e := scopeReader(ctx, o.access)
	if e != nil {
		return e
	}
	// Publication operations and offline rekey keep their original cancellation checks.
	current := func() (executor.Scope, error) {
		if err := ctx.Err(); err != nil {
			return executor.Scope{}, err
		}
		return readCurrent()
	}
	scope, e := current()
	if e != nil {
		return e
	}
	check := func() error {
		s, e := readCurrent()
		if e != nil {
			return e
		}
		if s != scope {
			return errors.New("current principal or authorization changed; retain original files and restart after host reconciliation")
		}
		return nil
	}
	path, e := filepath.Abs(o.file)
	if e != nil {
		return e
	}
	journalPath, e := filepath.Abs(o.journal)
	if e != nil {
		return e
	}
	if scope.ApplicationScopeID != identity.ApplicationScopeID || scope.EndUserID != identity.EndUserID {
		return executor.ErrConflict
	}
	if migrating {
		nextKey, err := hex.DecodeString(os.Getenv("TANSR_MEMORY_NEW_KEY"))
		if err != nil || len(nextKey) != 32 || bytes.Equal(key, nextKey) {
			return errors.New("TANSR_MEMORY_NEW_KEY must contain a different 32-byte host key (64 hex characters)")
		}
		defer clear(nextKey)
		target, err := filepath.Abs(o.target)
		if err != nil {
			return err
		}
		if persistence {
			source, err := openPersistence(terminalpersistence.Options{Path: path, Mode: "reopen", Key: key, Identity: identity, CurrentScope: current})
			if err != nil {
				return err
			}
			migrated, copyErr := source.CopyTo(ctx, terminalpersistence.CopyOptions{Path: target, Key: nextKey})
			closeErr := source.Close()
			if migrated != nil {
				closeErr = errors.Join(closeErr, migrated.Close())
			}
			if err = errors.Join(copyErr, closeErr); err != nil {
				return err
			}
			fmt.Println("read-only copy verified; original retained; cutover pending; no writer activation")
			return nil
		}
		if o.mode == "rekey-publication" {
			migrated, err := memorypublication.Rekey(memorypublication.Options{Path: path, Key: key, Identity: identity, CurrentScope: current}, target, nextKey)
			if err != nil {
				return err
			}
			if err = migrated.Close(); err != nil {
				return err
			}
		} else {
			err = executor.RekeyEncryptedFileJournal(ctx, journalPath, target, executor.JournalMigrationOptions{
				Source: executor.JournalEncryption{Key: key, ApplicationScopeID: scope.ApplicationScopeID, EndUserID: scope.EndUserID, ExecutorID: o.executor, CheckAccess: check}, TargetKey: nextKey,
			})
			if err != nil {
				var migration *executor.JournalMigrationError
				if errors.As(err, &migration) {
					return fmt.Errorf("%w; retain encrypted stage %q and target %q for reconciliation", err, migration.StagingDirectory, migration.TargetDirectory)
				}
				return err
			}
		}
		fmt.Println("verified offline rekey: original retained; verify both media before one explicit cutover; never resume both copies")
		return nil
	}
	// The caller creates a private directory/ACL. Missing recovery data are never rebuilt.
	if o.mode == "reopen" {
		if _, e = os.Stat(filepath.Join(journalPath, ".journal-mode")); e != nil {
			return e
		}
	}
	var host executor.MemoryPublicationHost
	name, digest := executor.MemoryPublicationToolName, executor.MemoryPublicationDefinitionDigest
	journalKey := key
	if persistence {
		journalKey, e = hex.DecodeString(os.Getenv("TANSR_JOURNAL_KEY"))
		if e != nil || len(journalKey) != 32 || bytes.Equal(key, journalKey) {
			return errors.New("new profile requires an independent 32-byte TANSR_JOURNAL_KEY")
		}
		defer clear(journalKey)
		store, err := openPersistence(terminalpersistence.Options{Path: path, Mode: o.mode, Key: key, Identity: identity, CurrentScope: current})
		if err != nil {
			return err
		}
		defer store.Close()
		if store.CopyVerifiedCutoverPending() {
			return errors.New("read-only copy: cutover pending; executor not started")
		}
		host, e = terminalpersistence.NewHost(store, true)
		name, digest = executor.TerminalPersistenceToolName, executor.TerminalPersistenceDefinitionDigest
	} else {
		store, err := memorypublication.OpenFileStore(memorypublication.Options{Path: path, Mode: o.mode, Key: key, Identity: identity, CurrentScope: current})
		if err != nil {
			return err
		}
		defer store.Close()
		host, e = memorypublication.NewHost(store, true)
	}
	if e != nil {
		return e
	}
	journal, e := executor.NewEncryptedFileJournal(journalPath, executor.JournalEncryption{Key: journalKey, ApplicationScopeID: scope.ApplicationScopeID, EndUserID: scope.EndUserID, ExecutorID: o.executor, CheckAccess: check})
	if e != nil {
		return e
	}
	defer journal.Close()
	client, e := demoutil.Client(o.base)
	if e != nil {
		return e
	}
	execution, e := executor.NewClient(client, scope)
	if e != nil {
		return e
	}
	workspace := executor.Workspace{WorkspaceID: o.workspace, Revision: "1"}
	registration := executor.Registration{Protocol: executor.Protocol, ExecutorID: o.executor, Platform: executor.CurrentPlatform(), Workspaces: []executor.Workspace{workspace}, Operations: []string{"tool.invoke"}, Tools: []executor.ToolDefinition{{Name: name, DefinitionDigest: digest}}}
	var expected *executor.Binding
	runnerOptions := executor.RunnerOptions{Client: execution, Registration: registration, Journal: journal, Authorize: func(c context.Context, op executor.Operation) error {
		if e := c.Err(); e != nil {
			return e
		}
		if e := check(); e != nil {
			return e
		}
		if expected == nil || op.Scope != scope || op.SessionID != o.session || op.ToolName != "MemoryPublication" || !reflect.DeepEqual(op.Binding, *expected) {
			return executor.ErrConflict
		}
		return nil
	}}
	if persistence {
		runnerOptions.TerminalPersistence = host
	} else {
		runnerOptions.MemoryPublication = host
	}
	runner, e := executor.NewRunner(runnerOptions)
	if e != nil {
		return e
	}
	connection, e := runner.Connect(ctx)
	if e != nil {
		return e
	}
	closure, e := client.SessionCapabilities(ctx, o.session)
	if e != nil {
		return e
	}
	initialized, e := execution.Initialize(ctx, o.session, registration.Platform, nil, closure.ClosureID)
	if e != nil {
		return e
	}
	closure, e = client.SessionCapabilities(ctx, o.session)
	if e != nil {
		return e
	}
	bound, e := execution.Bind(ctx, o.session, connection, workspace, initialized.CapabilityRevision, closure.ClosureID)
	if e != nil {
		return e
	}
	expected = bound.Binding
	if expected == nil {
		return errors.New("execution binding missing")
	}
	ref := map[string]any{"sessionContract": "sdk1", "sessionId": o.session}
	body := map[string]any{"contract": memorypublication.Contract, "requestId": o.bindingRequest, "session": ref, "executionBinding": expected, "required": []string{"memory-lifecycle-v1"}, "optional": []string{}}
	if e = wire.Validate(memorypublication.Contract, "BindingRequest", body); e != nil {
		return e
	}
	result, e := client.Call(ctx, api.OpTerminalBindingCreate, api.CallOptions{Body: body})
	if e != nil {
		return e
	}
	decoded, e := wire.Decode(memorypublication.Contract, "BindingResponse", result.Body)
	if e != nil {
		return e
	}
	response := decoded.(map[string]any)
	raw, _ := canonical.Encode(expected, canonical.Options{})
	got, _ := canonical.Encode(response["executionBinding"], canonical.Options{})
	scopeBytes, _ := canonical.Encode(scope, canonical.Options{})
	returnedScope, _ := canonical.Encode(response["scope"], canonical.Options{})
	refBytes, _ := canonical.Encode(ref, canonical.Options{})
	returnedRef, _ := canonical.Encode(response["session"], canonical.Options{})
	if response["requestId"] != o.bindingRequest || string(raw) != string(got) || string(scopeBytes) != string(returnedScope) || string(refBytes) != string(returnedRef) {
		return executor.ErrConflict
	}
	accepted := false
	for _, f := range response["accepted"].([]any) {
		if f == "memory-lifecycle-v1" {
			accepted = true
		}
	}
	if !accepted {
		return errors.New("memory-lifecycle-v1 unavailable")
	}
	fmt.Println("ready: encrypted publication and execution journal; preserve files, identity, key and original transfer IDs on failure")
	return runner.Run(ctx)
}
