package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

type RunnerOptions struct {
	Client              *Client
	Registration        Registration
	Journal             Journal
	Tools               map[string]Tool
	MemoryPublication   MemoryPublicationHost
	TerminalPersistence TerminalPersistenceHost
	PersistenceCustody  TerminalPersistenceHost
	// Authorize must check the local session, binding and current host permission. A Serve claim
	// alone is not permission to use the device. It runs before durable claim and again before IO.
	Authorize    func(context.Context, Operation) error
	PollInterval time.Duration
	OnReceipt    func(context.Context, Operation, Receipt) error
	// Status overrides the default controller-authorized execution.status read. An executor-only
	// host can use Client.ExecutorStatus after the controller has negotiated the terminal binding.
	// Every callback response is still schema/digest/scope checked and matched to the original op.
	Status func(context.Context, Operation) (Status, error)
}
type activeOperation struct {
	operation Operation
	cancel    context.CancelFunc
}
type Runner struct {
	options    RunnerOptions
	connectMu  sync.Mutex
	mu         sync.Mutex
	connection *Connection
	active     *activeOperation
	running    bool
	executeMu  sync.Mutex
}

func NewRunner(options RunnerOptions) (*Runner, error) {
	if options.Client == nil || options.Journal == nil || options.Authorize == nil {
		return nil, ErrInvalid
	}
	if err := validateRegistration(options.Registration); err != nil {
		return nil, err
	}
	platform := CurrentPlatform()
	if options.Registration.Platform.Platform != platform.Platform || options.Registration.Platform.Arch != platform.Arch {
		return nil, ErrInvalid
	}
	// This implementation is deliberately a business-tool host. It cannot accidentally advertise
	// filesystem/process operations for which no local policy or protected backend was supplied.
	count := len(options.Tools)
	if options.MemoryPublication != nil {
		count++
		if options.MemoryPublication.RequiresEncryptedJournal() {
			j, ok := options.Journal.(interface{ EncryptedAtRest() bool })
			if !ok || !j.EncryptedAtRest() {
				return nil, ErrUnsupported
			}
		}
	}
	if options.TerminalPersistence != nil {
		if options.MemoryPublication != nil {
			return nil, ErrUnsupported
		}
		count++
		if options.TerminalPersistence.RequiresEncryptedJournal() {
			j, ok := options.Journal.(interface{ EncryptedAtRest() bool })
			if !ok || !j.EncryptedAtRest() {
				return nil, ErrUnsupported
			}
		}
	}
	if options.PersistenceCustody != nil {
		count++
		if options.TerminalPersistence == nil || !options.PersistenceCustody.RequiresEncryptedJournal() {
			return nil, ErrUnsupported
		}
		journal, ok := options.Journal.(interface{ EncryptedAtRest() bool })
		if !ok || !journal.EncryptedAtRest() {
			return nil, ErrUnsupported
		}
	}
	if _, ok := options.Tools[PersistenceCustodyToolName]; ok {
		return nil, ErrUnsupported
	}
	if _, ok := options.Tools[TerminalPersistenceToolName]; ok {
		return nil, ErrUnsupported
	}
	if _, ok := options.Tools[MemoryPublicationToolName]; ok {
		return nil, ErrUnsupported
	}
	if _, ok := options.Tools["MemoryPublication"]; ok {
		return nil, ErrUnsupported
	}
	if len(options.Registration.Operations) != 1 || options.Registration.Operations[0] != "tool.invoke" || count == 0 || count != len(options.Registration.Tools) {
		return nil, ErrUnsupported
	}
	tools := make(map[string]Tool, len(options.Tools))
	for _, definition := range options.Registration.Tools {
		if definition.Name == PersistenceCustodyToolName {
			if options.PersistenceCustody == nil || definition.DefinitionDigest != PersistenceCustodyDefinitionDigest {
				return nil, ErrUnsupported
			}
			continue
		}
		if definition.Name == TerminalPersistenceToolName {
			if options.TerminalPersistence == nil || definition.DefinitionDigest != TerminalPersistenceDefinitionDigest {
				return nil, ErrUnsupported
			}
			continue
		}
		if definition.Name == MemoryPublicationToolName {
			if options.MemoryPublication == nil || definition.DefinitionDigest != MemoryPublicationDefinitionDigest {
				return nil, ErrUnsupported
			}
			continue
		}
		tool, ok := options.Tools[definition.Name]
		if !ok || tool.Handle == nil || tool.DefinitionDigest != definition.DefinitionDigest {
			return nil, ErrInvalid
		}
		tools[definition.Name] = tool
	}
	if options.PollInterval == 0 {
		options.PollInterval = 250 * time.Millisecond
	}
	if options.PollInterval < 10*time.Millisecond || options.PollInterval > time.Minute {
		return nil, ErrInvalid
	}
	options.Tools = tools
	options.Registration = clone(options.Registration)
	return &Runner{options: options}, nil
}
func (r *Runner) Connection() (Connection, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.connection == nil {
		return Connection{}, false
	}
	return *r.connection, true
}
func (r *Runner) Connect(ctx context.Context) (Connection, error) {
	r.connectMu.Lock()
	defer r.connectMu.Unlock()
	if err := ctx.Err(); err != nil {
		return Connection{}, err
	}
	if existing, ok := r.Connection(); ok {
		return existing, liveConnection(existing)
	}
	connection, err := r.options.Client.Register(ctx, r.options.Registration)
	if err != nil {
		return Connection{}, err
	}
	r.mu.Lock()
	r.connection = &connection
	r.mu.Unlock()
	return connection, nil
}

// Run maintains the lease while executing serially and polls remote status while a handler runs.
// Lease loss or remote cancellation cancels that handler's context. Run waits for its cleanup;
// handlers must honor cancellation. An empty poll or closed transport is never task completion.
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return ErrAlreadyRunning
	}
	r.running = true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.running = false; r.mu.Unlock() }()
	if _, err := r.Connect(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		err := r.monitor(ctx)
		done <- err
		if err != nil {
			r.mu.Lock()
			active := r.active
			r.mu.Unlock()
			if active != nil {
				active.cancel()
			}
			cancel()
		}
	}()
	var result error
	for ctx.Err() == nil {
		connection, _ := r.Connection()
		leaseCtx, leaseCancel := connectionContext(ctx, connection)
		batch, err := r.options.Client.Poll(leaseCtx, connection)
		leaseCancel()
		if err != nil {
			result = err
			break
		}
		for _, operation := range batch.Operations {
			if ctx.Err() != nil {
				break
			}
			receipt, err := r.execute(ctx, operation, true)
			if err != nil {
				result = err
				break
			}
			// A lost submit response is reconciled by querying the original operation. No handler is
			// re-run, and no replacement operation/receipt ID is minted.
			_, err = r.options.Client.Submit(ctx, operation, receipt)
			if err != nil {
				status, statusErr := r.readStatus(ctx, operation)
				if statusErr != nil || !equal(status.Operation, operation) || status.Receipt == nil || !equal(*status.Receipt, receipt) {
					result = err
					break
				}
			}
			if r.options.OnReceipt != nil {
				if err = r.options.OnReceipt(ctx, clone(operation), clone(receipt)); err != nil {
					result = err
					break
				}
			}
		}
		if result != nil {
			break
		}
		timer := time.NewTimer(r.options.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	cancel()
	monitorErr := <-done
	if monitorErr != nil && !errors.Is(monitorErr, context.Canceled) {
		return monitorErr
	}
	if result != nil {
		return result
	}
	return ctx.Err()
}
func connectionContext(ctx context.Context, connection Connection) (context.Context, context.CancelFunc) {
	expires, err := time.Parse(time.RFC3339Nano, connection.ExpiresAt)
	if err != nil {
		expires = time.Now()
	}
	return context.WithDeadline(ctx, expires)
}
func (r *Runner) monitor(ctx context.Context) error {
	connection, _ := r.Connection()
	next := time.Now().Add(time.Duration(connection.HeartbeatAfterMS) * time.Millisecond)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		connection, _ = r.Connection()
		if err := liveConnection(connection); err != nil {
			return err
		}
		if !time.Now().Before(next) {
			leaseCtx, leaseCancel := connectionContext(ctx, connection)
			renewed, err := r.options.Client.Heartbeat(leaseCtx, connection)
			leaseCancel()
			if err != nil {
				return err
			}
			r.mu.Lock()
			r.connection = &renewed
			active := r.active
			r.mu.Unlock()
			if active != nil && renewed.ConnectionRevision != active.operation.Binding.Target.ConnectionRevision {
				active.cancel()
			}
			next = time.Now().Add(time.Duration(renewed.HeartbeatAfterMS) * time.Millisecond)
		}
		r.mu.Lock()
		active := r.active
		r.mu.Unlock()
		if active != nil {
			leaseCtx, leaseCancel := connectionContext(ctx, connection)
			status, err := r.readStatus(leaseCtx, active.operation)
			leaseCancel()
			if err != nil {
				return err
			}
			if !equal(status.Operation, active.operation) {
				return ErrConflict
			}
			if status.Status != "pending" {
				active.cancel()
			}
		}
	}
}

func (r *Runner) readStatus(ctx context.Context, operation Operation) (Status, error) {
	var status Status
	var err error
	if r.options.Status != nil {
		status, err = r.options.Status(ctx, clone(operation))
	} else {
		status, err = r.options.Client.Status(ctx, operation.SessionID, operation.OperationID)
	}
	if err != nil {
		return status, err
	}
	if err = validate("ExecutionStatus", status); err != nil {
		return status, err
	}
	if err = r.options.Client.validateStatus(status); err != nil {
		return status, err
	}
	if !equal(status.Operation, operation) {
		return status, ErrConflict
	}
	return status, nil
}
func (r *Runner) check(ctx context.Context, operation Operation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	connection, ok := r.Connection()
	if !ok {
		return ErrLeaseExpired
	}
	if err := liveConnection(connection); err != nil {
		return err
	}
	target := operation.Binding.Target
	if operation.Scope != r.options.Client.scope || target.ExecutorID != connection.ExecutorID || target.ConnectionID != connection.ConnectionID || target.ConnectionRevision != connection.ConnectionRevision {
		return ErrConflict
	}
	workspaceOK := false
	for _, workspace := range r.options.Registration.Workspaces {
		if workspace.WorkspaceID == target.WorkspaceID && workspace.Revision == target.WorkspaceRevision {
			workspaceOK = true
		}
	}
	if !workspaceOK {
		return ErrConflict
	}
	if err := r.options.Authorize(ctx, clone(operation)); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, ok := r.Connection()
	if !ok || current.ExecutorID != connection.ExecutorID || current.ConnectionID != connection.ConnectionID || current.ConnectionRevision != connection.ConnectionRevision {
		return ErrConflict
	}
	return liveConnection(current)
}
func receiptFor(operation Operation, status, code string, result *Resource) Receipt {
	receipt := Receipt{Protocol: Protocol, ExecutorID: operation.Binding.Target.ExecutorID, ConnectionID: operation.Binding.Target.ConnectionID, OperationID: operation.OperationID, Digest: operation.Digest, Status: status, Result: result}
	if code != "" {
		receipt.ErrorCode = &code
	}
	return receipt
}

// Execute applies the same validated, durable execution path used by Run. It does not submit the
// receipt. Callers using this method directly must maintain the connection lease themselves.
func (r *Runner) Execute(ctx context.Context, operation Operation) (Receipt, error) {
	return r.execute(ctx, operation, false)
}

func (r *Runner) execute(ctx context.Context, operation Operation, monitored bool) (Receipt, error) {
	r.executeMu.Lock()
	defer r.executeMu.Unlock()
	if err := validateOperation(operation); err != nil {
		return Receipt{}, err
	}
	operation = clone(operation)
	if operation.Request.Operation != "tool.invoke" {
		return Receipt{}, ErrUnsupported
	}
	tool, ok := r.options.Tools[operation.ToolName]
	if operation.Request.Args["name"] == PersistenceCustodyToolName && r.options.PersistenceCustody != nil {
		ok = true
		tool = Tool{DefinitionDigest: PersistenceCustodyDefinitionDigest, Handle: func(c context.Context, a map[string]any) (any, error) {
			return r.options.PersistenceCustody.Execute(c, operation, a)
		}}
	}
	if operation.Request.Args["name"] == TerminalPersistenceToolName && r.options.TerminalPersistence != nil {
		ok = true
		tool = Tool{DefinitionDigest: TerminalPersistenceDefinitionDigest, Handle: func(c context.Context, a map[string]any) (any, error) {
			return r.options.TerminalPersistence.Execute(c, operation, a)
		}}
	}
	if operation.Request.Args["name"] == MemoryPublicationToolName && r.options.MemoryPublication != nil {
		ok = true
		tool = Tool{DefinitionDigest: MemoryPublicationDefinitionDigest, Handle: func(c context.Context, a map[string]any) (any, error) {
			return r.options.MemoryPublication.Execute(c, operation, a)
		}}
	}
	if !ok || operation.Request.Args["definitionDigest"] != tool.DefinitionDigest {
		return Receipt{}, ErrUnsupported
	}
	if err := r.check(ctx, operation); err != nil {
		return Receipt{}, err
	}
	claim, err := r.options.Journal.Claim(ctx, operation)
	if err != nil {
		return Receipt{}, err
	}
	if claim.Receipt != nil {
		if claim.Claimed {
			return Receipt{}, ErrConflict
		}
		if err := validateReceipt(operation, *claim.Receipt); err != nil {
			return Receipt{}, err
		}
		return clone(*claim.Receipt), nil
	}
	var receipt Receipt
	if !claim.Claimed {
		receipt = receiptFor(operation, "unknown", "execution_outcome_unknown", nil)
	} else {
		expires, err := time.Parse(time.RFC3339Nano, operation.ExpiresAt)
		if err != nil {
			return Receipt{}, ErrInvalid
		}
		executionCtx, stop := context.WithDeadline(ctx, expires)
		leaseStop := func() {}
		// Run's monitor follows renewed leases and cancels the run when renewal fails. The direct
		// Execute convenience path has no renewal loop, so it cannot outlive its current lease.
		if !monitored {
			connection, _ := r.Connection()
			executionCtx, leaseStop = connectionContext(executionCtx, connection)
		}
		active := &activeOperation{operation: operation, cancel: stop}
		r.mu.Lock()
		r.active = active
		r.mu.Unlock()
		func() {
			defer func() {
				r.mu.Lock()
				if r.active == active {
					r.active = nil
				}
				r.mu.Unlock()
				leaseStop()
				stop()
			}()
			if err := r.check(executionCtx, operation); err != nil {
				receipt = receiptFor(operation, "failed", "authorization_rejected", nil)
				return
			}
			args, err := toolObject(operation.Request.Args["argsJson"].(string))
			if err != nil {
				receipt = receiptFor(operation, "failed", "invalid_arguments", nil)
				return
			}
			output, err := invokeTool(executionCtx, tool, args)
			if err != nil {
				var rejected *Rejected
				if errors.As(err, &rejected) && rejected.Code != "" && len(rejected.Code) <= 128 {
					receipt = receiptFor(operation, "failed", rejected.Code, nil)
				} else {
					receipt = receiptFor(operation, "unknown", "execution_outcome_unknown", nil)
				}
				return
			}
			data, err := json.Marshal(output)
			if err != nil || verifyToolResult(string(data)) != nil {
				receipt = receiptFor(operation, "unknown", "invalid_tool_result", nil)
				return
			}
			receipt = receiptFor(operation, "completed", "", &Resource{Operation: "tool.invoke", Args: map[string]any{"resultJson": string(data)}})
		}()
	}
	if err := validateReceipt(operation, receipt); err != nil {
		return Receipt{}, err
	}
	// Once a handler has started, outer cancellation cannot erase the local outcome fact.
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := r.options.Journal.Complete(commitCtx, operation, receipt); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}
func invokeTool(ctx context.Context, tool Tool, args map[string]any) (value any, err error) {
	defer func() {
		if recover() != nil {
			value = nil
			err = fmt.Errorf("%w: handler panicked", ErrOutcomeUnknown)
		}
	}()
	return tool.Handle(ctx, args)
}
