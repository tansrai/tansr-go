// Command go-tools hosts one explicitly installed, read-only business tool.
// All model calls, permissions and orchestration remain in Serve.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"

	"github.com/tansrai/tansr-go/examples/internal/demoutil"
	"github.com/tansrai/tansr-go/executor"
	"github.com/tansrai/tansr-go/session"
)

// This is the same declaration and business result as the existing Serve demo
// (examples/serve-demo/execution-policy.ts), not a new server-side tool contract.
const declarationJSON = `{"name":"DemoOrderStatus","description":"Read the status of sample order DEMO-001; this is demonstration data.","parameters":{"orderId":{"type":"string","description":"Sample order ID: DEMO-001"}},"readOnly":true}`
const toolName = "DemoOrderStatus"

type options struct{ base, session, application, user, revision, executor, journal, mode string }

func main() {
	var opts options
	flag.StringVar(&opts.base, "base", "http://127.0.0.1:8787", "Serve origin")
	flag.StringVar(&opts.session, "session", "", "existing session declared with DemoOrderStatus; omit to create one")
	flag.StringVar(&opts.application, "application", os.Getenv("TANSR_APPLICATION_SCOPE_ID"), "authenticated application scope")
	flag.StringVar(&opts.user, "user", os.Getenv("TANSR_END_USER_ID"), "authenticated end-user identity")
	flag.StringVar(&opts.revision, "authorization-revision", os.Getenv("TANSR_AUTHORIZATION_REVISION"), "current authenticated authorization revision")
	flag.StringVar(&opts.executor, "executor", "go-demo", "executor identity authorized by Serve policy")
	flag.StringVar(&opts.mode, "mode", "terminal", "explicit execution persistence mode: terminal or cloud")
	flag.StringVar(&opts.journal, "journal", "", "private durable execution journal directory (terminal only)")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, opts); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "go-tools:", demoutil.Text(demoutil.Describe(err).Error()))
		os.Exit(1)
	}
}

func declaration() map[string]any {
	var value map[string]any
	if err := json.Unmarshal([]byte(declarationJSON), &value); err != nil {
		panic(err)
	} // compile-time sample constant
	return value
}

func orderStatus(ctx context.Context, args map[string]any) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	order, ok := args["orderId"].(string)
	if !ok || len(args) != 1 {
		return nil, &executor.Rejected{Code: "EINVAL"}
	}
	if order != "DEMO-001" {
		return map[string]any{"status": "error", "message": "Sample order not found / 演示订单不存在"}, nil
	}
	return map[string]any{"status": "ok", "content": []any{map[string]any{"t": "text", "text": "DEMO-001: awaiting shipment (sample data) / 待发货（演示数据）"}}}, nil
}

func run(ctx context.Context, opts options) error {
	if opts.application == "" || opts.user == "" || opts.revision == "" || (opts.mode != "cloud" && opts.journal == "") || (opts.mode != "" && opts.mode != "terminal" && opts.mode != "cloud") || (opts.mode == "cloud" && opts.journal != "") {
		return errors.New("provide application, user, authorization-revision and a private journal directory; these must match the token and Serve policy")
	}
	transport, err := demoutil.Client(opts.base)
	if err != nil {
		return err
	}
	scope := executor.Scope{ApplicationScopeID: opts.application, EndUserID: opts.user, AuthorizationRevision: opts.revision}
	client, err := executor.NewClient(transport, scope)
	if err != nil {
		return err
	}
	conversation, err := session.New(transport)
	if err != nil {
		return err
	}
	var current *session.Session
	if opts.session != "" {
		current, err = conversation.Attach(ctx, opts.session)
	} else {
		current, err = conversation.Create(ctx, session.CreateOptions{ClientTools: []json.RawMessage{json.RawMessage(declarationJSON)}})
	}
	if err != nil {
		return err
	}
	fmt.Println("session:", demoutil.Text(current.ID()))
	var runner *executor.Runner
	var journal executor.Journal
	if opts.mode == "cloud" {
		view, e := current.Persistence(ctx)
		if e != nil {
			return e
		}
		if view.Mode != "cloud" || !view.Handoff.ExecutionJournal {
			return errors.New("Serve did not advertise cloud execution journal")
		}
		cloud, e := executor.NewCloudJournal(client, current.ID(), func() (executor.Connection, error) {
			if runner == nil {
				return executor.Connection{}, executor.ErrConflict
			}
			connection, ok := runner.Connection()
			if !ok {
				return connection, executor.ErrConflict
			}
			return connection, nil
		})
		if e != nil {
			return e
		}
		defer cloud.Close()
		journal = cloud
	} else {
		path, e := filepath.Abs(opts.journal)
		if e != nil {
			return e
		}
		local, e := executor.NewFileJournal(path)
		if e != nil {
			return e
		}
		defer local.Close()
		journal = local
	}
	digest, err := executor.DefinitionDigest(declaration())
	if err != nil {
		return err
	}
	workspace := executor.Workspace{WorkspaceID: "go-business", Revision: "1"} // logical scope, not a filesystem root
	registration := executor.Registration{Protocol: executor.Protocol, ExecutorID: opts.executor, Platform: executor.CurrentPlatform(),
		Workspaces: []executor.Workspace{workspace}, Operations: []string{"tool.invoke"}, Tools: []executor.ToolDefinition{{Name: toolName, DefinitionDigest: digest}}}
	var expected *executor.Binding
	runner, err = executor.NewRunner(executor.RunnerOptions{Client: client, Registration: registration, Journal: journal,
		Tools: map[string]executor.Tool{toolName: {DefinitionDigest: digest, Handle: orderStatus}},
		Authorize: func(call context.Context, operation executor.Operation) error {
			if err := call.Err(); err != nil {
				return err
			}
			if expected == nil || operation.SessionID != current.ID() || operation.Scope != scope || operation.ToolName != toolName || !reflect.DeepEqual(operation.Binding, *expected) {
				return executor.ErrConflict
			}
			// Explicit permission for this static, read-only synthetic operation.
			// A real host adds its current login, revocation and resource policy.
			return nil
		},
		OnReceipt: func(_ context.Context, operation executor.Operation, receipt executor.Receipt) error {
			fmt.Printf("operation %s: %s\n", demoutil.Text(operation.OperationID), demoutil.Text(receipt.Status))
			return nil
		}})
	if err != nil {
		return err
	}
	connection, err := runner.Connect(ctx)
	if err != nil {
		return err
	}
	closure, err := transport.SessionCapabilities(ctx, current.ID())
	if err != nil {
		return err
	}
	initialized, err := client.Initialize(ctx, current.ID(), registration.Platform, []string{toolName}, closure.ClosureID)
	if err != nil {
		return err
	}
	closure, err = transport.SessionCapabilities(ctx, current.ID())
	if err != nil {
		return err
	}
	bound, err := client.Bind(ctx, current.ID(), connection, workspace, initialized.CapabilityRevision, closure.ClosureID)
	if err != nil {
		return err
	}
	expected = bound.Binding
	available := false
	for _, tool := range bound.EffectiveTools {
		if tool.Name == toolName && tool.Available {
			available = true
		}
	}
	if !available {
		return errors.New("Serve did not enable DemoOrderStatus in this session's capability boundary")
	}
	fmt.Printf("ready: in a second terminal run go run ./examples/go-chat -base %q -resume %q\n", opts.base, current.ID())
	fmt.Println("Ask: 查询订单 DEMO-001。Keep this executor running; Ctrl+C stops it. No shell or filesystem tools are advertised.")
	return runner.Run(ctx)
}
