package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tansrai/tansr-go/api"
	"github.com/tansrai/tansr-go/archive"
	"github.com/tansrai/tansr-go/executor"
	"github.com/tansrai/tansr-go/session"
)

type pstHost struct {
	stdin    io.WriteCloser
	scan     *bufio.Scanner
	sequence int
}

func (h *pstHost) command(t *testing.T, command map[string]any, result any) {
	t.Helper()
	h.sequence++
	id := strconv.Itoa(h.sequence)
	command["id"] = id
	if err := json.NewEncoder(h.stdin).Encode(command); err != nil {
		t.Fatal(err)
	}
	for h.scan.Scan() {
		var r struct {
			Kind, ID string
			OK       bool
			Value    json.RawMessage
			Error    any
		}
		if err := json.Unmarshal(h.scan.Bytes(), &r); err != nil {
			continue
		}
		if r.Kind != "reply" || r.ID != id {
			continue
		}
		if !r.OK {
			t.Fatalf("host %v: %v", command["command"], r.Error)
		}
		if result != nil {
			if err := json.Unmarshal(r.Value, result); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	t.Fatal("host closed", h.scan.Err())
}

type pstSeed struct {
	BaseURL               string `json:"baseUrl"`
	Token                 string `json:"token"`
	SessionID             string `json:"sessionId"`
	ExecutorID            string `json:"executorId"`
	ApplicationScopeID    string `json:"applicationScopeId"`
	EndUserID             string `json:"endUserId"`
	AuthorizationRevision string `json:"authorizationRevision"`
	DefinitionDigest      string `json:"definitionDigest"`
	WorkspaceID           string `json:"workspaceId"`
	WorkspaceRevision     string `json:"workspaceRevision"`
}

func TestRealServeDisklessModes(t *testing.T) {
	root := os.Getenv("TANSR_GO_PST_SERVE_ROOT")
	if root == "" {
		t.Skip("explicit controlled Serve host required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--import", "./scripts/inject-globals.mjs", "--import", "tsx", "scripts/persistence/cross-client-host.ts")
	cmd.Dir = root
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &pstHost{stdin: stdin, scan: bufio.NewScanner(stdout)}
	h.scan.Buffer(make([]byte, 4096), 1<<20)
	defer func() {
		h.command(t, map[string]any{"command": "quit"}, nil)
		stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Error(err, diagnostic.String())
		}
	}()
	if !h.scan.Scan() {
		t.Fatal("no host ready", diagnostic.String())
	}
	var ready map[string]any
	if json.Unmarshal(h.scan.Bytes(), &ready) != nil || ready["kind"] != "ready" {
		t.Fatal("host not ready", string(h.scan.Bytes()))
	}
	for _, testCase := range []struct{ mode, toolMode, client string }{{"terminal", "resource", "go"}, {"cloud", "resource", "go"}, {"cloud", "business", "go-business"}} {
		mode, toolMode, clientID := testCase.mode, testCase.toolMode, testCase.client
		t.Run(mode+"-"+toolMode, func(t *testing.T) {
			var seed pstSeed
			h.command(t, map[string]any{"command": "seed", "mode": mode, "client": clientID, "toolMode": toolMode}, &seed)
			transport, err := api.New(api.Options{BaseURL: seed.BaseURL, Token: seed.Token, SessionFamily: "sdk1", EventEnvelope: true})
			if err != nil {
				t.Fatal(err)
			}
			sessions, err := session.New(transport)
			if err != nil {
				t.Fatal(err)
			}
			s, err := sessions.Attach(ctx, seed.SessionID)
			if err != nil {
				t.Fatal("attach", err)
			}
			persistence, err := s.Persistence(ctx)
			if err != nil || persistence.Mode != mode || persistence.LocalDisk || persistence.Handoff.ExecutionJournal != (mode == "cloud") {
				t.Fatal("persistence", persistence, err)
			}
			ac := archive.NewClient(transport)
			binding, status, err := ac.OpenInstalled(ctx, seed.SessionID)
			if err != nil || binding.Availability != "source-managed" || binding.Persistence == nil || binding.Persistence.Mode != mode {
				t.Fatal("installed binding", err)
			}
			eventctx, stopEvents := context.WithTimeout(ctx, 3*time.Second)
			events, err := ac.Events(eventctx, binding, "")
			if err != nil {
				stopEvents()
				t.Fatal("archive SSE", err)
			}
			frame, err := events.Next()
			events.Close()
			stopEvents()
			if err != nil || len(frame) == 0 {
				t.Fatal("source-managed frame", err)
			}
			scope := executor.Scope{ApplicationScopeID: seed.ApplicationScopeID, EndUserID: seed.EndUserID, AuthorizationRevision: seed.AuthorizationRevision}
			if scope.EndUserID == "" {
				meta, e := s.Meta(ctx)
				if e != nil {
					t.Fatal(e)
				}
				scope.EndUserID = meta.EndUserID
			}
			ec, err := executor.NewClient(transport, scope)
			if err != nil {
				t.Fatal(err)
			}
			platform := executor.CurrentPlatform()
			requested := []string{"Read"}
			if toolMode == "business" {
				requested = []string{"BusinessLookup"}
			}
			caps, err := ec.Initialize(ctx, seed.SessionID, platform, requested, "")
			if err != nil {
				t.Fatal("initialize", err)
			}
			work := executor.Workspace{WorkspaceID: seed.WorkspaceID, Revision: seed.WorkspaceRevision}
			var conn executor.Connection
			var journal executor.Journal
			var cloud *executor.CloudJournal
			if mode == "cloud" {
				cloud, err = executor.NewCloudJournal(ec, seed.SessionID, func() (executor.Connection, error) { return conn, nil })
				journal = cloud
				defer cloud.Close()
			} else {
				local, e := executor.NewEncryptedFileJournal(filepath.Join(t.TempDir(), "journal"), executor.JournalEncryption{Key: bytes.Repeat([]byte{3}, 32), ApplicationScopeID: scope.ApplicationScopeID, EndUserID: scope.EndUserID, ExecutorID: seed.ExecutorID, CheckAccess: func() error { return ctx.Err() }})
				err = e
				journal = local
				if local != nil {
					defer local.Close()
				}
			}
			if err != nil {
				t.Fatal("journal", err)
			}

			var runner *executor.Runner
			registration := executor.Registration{Protocol: executor.Protocol, ExecutorID: seed.ExecutorID, Platform: platform, Workspaces: []executor.Workspace{work}, Operations: []string{"fs.inspect", "fs.read"}}
			if toolMode == "business" {
				registration.Operations = []string{"tool.invoke"}
				registration.Tools = []executor.ToolDefinition{{Name: "BusinessLookup", DefinitionDigest: seed.DefinitionDigest}}
				runner, err = executor.NewRunner(executor.RunnerOptions{Client: ec, Registration: registration, Journal: journal, Tools: map[string]executor.Tool{"BusinessLookup": {DefinitionDigest: seed.DefinitionDigest, Handle: func(context.Context, map[string]any) (any, error) {
					return map[string]any{"status": "ok", "content": []any{map[string]any{"t": "text", "text": "synthetic business fact"}}}, nil
				}}}, Authorize: func(_ context.Context, op executor.Operation) error {
					if op.SessionID != seed.SessionID || op.Scope != scope {
						return executor.ErrConflict
					}
					return nil
				}})
				if err == nil {
					conn, err = runner.Connect(ctx)
				}
			} else {
				conn, err = ec.Register(ctx, registration)
			}
			if err != nil {
				t.Fatal("register", err)
			}
			_, err = ec.Bind(ctx, seed.SessionID, conn, work, caps.CapabilityRevision, "")
			if err != nil {
				t.Fatal("bind", err)
			}
			key := "go-original-" + mode
			h.command(t, map[string]any{"command": "drop-response", "mode": mode, "client": clientID, "method": "POST", "path": "/api/sessions/" + seed.SessionID + "/messages"}, nil)
			if _, err = s.Send(ctx, "synthetic original message", session.WriteOptions{RequestID: key}); err == nil {
				t.Fatal("expected accepted response loss")
			} else {
				t.Log("send loss:", err)
			}
			receipt, err := s.QueryInput(ctx, key)
			if err != nil || receipt == nil || receipt.State != "accepted" {
				var diagnostic any
				h.command(t, map[string]any{"command": "status"}, &diagnostic)
				t.Fatalf("original input query %v %v host=%+v", receipt, err, diagnostic)
			}
			page, err := s.ListInputReceipts(ctx, "", 10)
			if err != nil || len(page.Receipts) != 1 {
				t.Fatal("input page", err)
			}
			waitIdle := func() {
				for i := 0; i < 100; i++ {
					meta, e := s.Meta(ctx)
					if e != nil {
						t.Fatal(e)
					}
					if meta.Status == "idle" {
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
				t.Fatal("session not idle")
			}
			waitIdle()
			h.command(t, map[string]any{"command": "dispatch", "mode": mode, "client": clientID, "connectionId": conn.ConnectionID, "requestId": "go-tool-" + mode}, nil)
			done := map[string]bool{}
			for i := 0; i < 200; i++ {
				batch, e := ec.Poll(ctx, conn)
				if e != nil {
					t.Fatal("poll", e)
				}
				for _, op := range batch.Operations {
					if done[op.OperationID] {
						continue
					}
					if runner != nil {
						receipt, e := runner.Execute(ctx, op)
						if e != nil {
							t.Fatal("high-level Runner", e)
						}
						if _, e = ec.Submit(ctx, op, receipt); e != nil {
							t.Fatal(e)
						}
					} else {
						claimed, e := journal.Claim(ctx, op)
						if e != nil || !claimed.Claimed {
							t.Fatal("claim", e)
						}
						args := map[string]any{}
						switch op.Request.Operation {
						case "fs.inspect":
							args = map[string]any{"kind": "file", "size": 16, "mtimeMs": "1", "realpath": "synthetic.txt"}
						case "fs.read":
							num := func(k string) int {
								switch v := op.Request.Args[k].(type) {
								case json.Number:
									n, _ := v.Int64()
									return int(n)
								case float64:
									return int(v)
								default:
									t.Fatalf("bad %s %T", k, v)
									return 0
								}
							}
							raw := []byte("synthetic bytes\n")
							offset, length := num("offset"), num("length")
							if offset > len(raw) {
								offset = len(raw)
							}
							end := min(offset+length, len(raw))
							args["bytesBase64"] = base64.StdEncoding.EncodeToString(raw[offset:end])
						default:
							t.Fatal("unexpected resource", op.Request.Operation)
						}
						result := executor.Resource{Operation: op.Request.Operation, Args: args}
						r := executor.Receipt{Protocol: executor.Protocol, ExecutorID: conn.ExecutorID, ConnectionID: conn.ConnectionID, OperationID: op.OperationID, Digest: op.Digest, Status: "completed", Result: &result}
						if e = journal.Complete(ctx, op, r); e != nil {
							t.Fatal("complete", e)
						}
						if _, e = ec.Submit(ctx, op, r); e != nil {
							t.Fatal("submit", e)
						}
					}
					done[op.OperationID] = true
				}
				meta, e := s.Meta(ctx)
				if e != nil {
					t.Fatal(e)
				}
				if meta.Status == "idle" && len(done) > 0 {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			waitIdle()
			if len(done) == 0 {
				t.Fatal("no real kernel resource operations")
			}
			if cloud != nil {
				cold, e := executor.NewCloudJournal(ec, seed.SessionID, func() (executor.Connection, error) { return conn, nil })
				if e != nil {
					t.Fatal(e)
				}
				defer cold.Close()
				page, e := cold.List(ctx, "", 16)
				if e != nil || len(page.Records) != len(done) {
					t.Fatal("cold cloud journal", e)
				}
				for _, r := range page.Records {
					if r.Status != "completed" || r.Receipt == nil {
						t.Fatal("missing permanent execution receipt")
					}
				}
			} else {
				binding, status, err = ac.OpenInstalled(ctx, seed.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				identity, e := archive.IdentityFrom(binding, status)
				if e != nil {
					t.Fatal(e)
				}
				store, e := archive.OpenFileStore(archive.StoreOptions{Path: filepath.Join(t.TempDir(), "archive.bin"), Key: bytes.Repeat([]byte{4}, 32), Identity: identity, CheckAccess: func(archive.Identity) error { return ctx.Err() }})
				if e != nil {
					t.Fatal(e)
				}
				defer store.Close()
				for i := 0; i < 20; i++ {
					sync, e := archive.SyncOnce(ctx, ac, store, fmt.Sprintf("go-ack-%d", i))
					if e != nil {
						t.Fatal("durable terminal ACK", e)
					}
					if sync.Complete {
						break
					}
					if i == 19 {
						t.Fatal("archive incomplete")
					}
				}
			}
			t.Logf("%s real source-managed binding + SSE, lost input query, %d execution operations, %s journal", mode, len(done), mode)
		})
	}
}
