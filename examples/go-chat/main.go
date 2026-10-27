// Command go-chat owns presentation; Serve owns the agent loop and context.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/tansrai/tansr-go/examples/internal/demoutil"
	"github.com/tansrai/tansr-go/executor"
	"github.com/tansrai/tansr-go/session"
)

type options struct {
	base, resume, model, message string
	application, user, revision  string
	timeout                      time.Duration
	close                        bool
}

func main() {
	var opts options
	flag.StringVar(&opts.base, "base", "http://127.0.0.1:8787", "Serve origin")
	flag.StringVar(&opts.resume, "resume", "", "resume the same session, never create a replacement")
	flag.StringVar(&opts.model, "model", "", "model for a new session; application policy applies")
	flag.StringVar(&opts.message, "message", "", "one turn; omit for interactive multi-turn chat")
	flag.StringVar(&opts.application, "application", os.Getenv("TANSR_APPLICATION_SCOPE_ID"), "authenticated application scope")
	flag.StringVar(&opts.user, "user", os.Getenv("TANSR_END_USER_ID"), "authenticated end-user identity")
	flag.StringVar(&opts.revision, "authorization-revision", os.Getenv("TANSR_AUTHORIZATION_REVISION"), "current authenticated authorization revision")
	flag.DurationVar(&opts.timeout, "timeout", 10*time.Minute, "overall deadline, 0 disables it")
	flag.BoolVar(&opts.close, "close", false, "close on exit; default preserves the session for resume")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if opts.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.timeout)
		defer cancel()
	}
	if err := run(ctx, opts, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "go-chat:", demoutil.Text(demoutil.Describe(err).Error()))
		os.Exit(1)
	}
}

func run(ctx context.Context, opts options, input io.Reader, out io.Writer) (runErr error) {
	transport, err := demoutil.Client(opts.base)
	if err != nil {
		return err
	}
	client, err := session.New(transport)
	if err != nil {
		return err
	}
	execution, err := executor.NewClient(transport, executor.Scope{ApplicationScopeID: opts.application, EndUserID: opts.user, AuthorizationRevision: opts.revision})
	if err != nil {
		return fmt.Errorf("set authenticated application/user/authorization-revision for initialization: %w", err)
	}
	create := session.CreateOptions{Model: opts.model}
	if opts.resume != "" {
		create = session.CreateOptions{Resume: &session.ResumeReference{SessionID: opts.resume}}
	}
	prepared, err := client.Prepare(ctx, create, session.PreparationOptions{Execution: execution,
		OnSessionConfirmed: func(s *session.Session) { fmt.Fprintln(out, "session:", demoutil.Text(s.ID())) },
		OnProgress:         func(p session.PreparationProgress) { fmt.Fprintln(out, "prepare:", p.Stage, p.Status) },
	})
	if err != nil {
		return err
	}
	current := prepared.Session
	if opts.close {
		defer func() {
			end, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := current.Close(end, session.WriteOptions{}); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("session close failed: %w", err))
			}
		}()
	}
	meta, err := current.Meta(ctx)
	if err != nil {
		return err
	}
	if meta.Status == "ended" || !meta.Live {
		return errors.New("session is not live after resume")
	}
	if meta.Status == "running" && opts.message != "" {
		return errors.New("session has a running turn; resume without -message to observe or cancel it")
	}
	// Old control frames may replay when resuming a running turn. Its metadata
	// sequence is the floor: an old turn.completed must not finish this turn.
	cursor := strconv.FormatInt(meta.LastSeq, 10)
	if meta.Status == "running" {
		cursor = "0"
	}
	reading, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := current.Events(reading, cursor)
	if err != nil {
		return err
	}
	defer stream.Close()
	events := make(chan eventResult, 16)
	go func() {
		defer close(events)
		for {
			event, err := stream.Next()
			select {
			case events <- eventResult{event: event, err: err}:
			case <-reading.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	write := func(action func(context.Context, session.WriteOptions) error) error {
		key, err := demoutil.RequestID()
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "request:", key)
		request, done := context.WithTimeout(ctx, 30*time.Second)
		defer done()
		deadline, _ := request.Deadline()
		return action(request, session.WriteOptions{IdempotencyKey: key, Deadline: deadline})
	}
	hooks := chatHooks{
		send: func(call context.Context, text string) (int64, error) {
			before, err := current.Meta(call)
			if err != nil {
				return 0, err
			}
			if before.Status != "idle" {
				return 0, errors.New("session is not idle; wait or /cancel")
			}
			err = write(func(request context.Context, opts session.WriteOptions) error {
				_, err := current.Send(request, text, opts)
				return err
			})
			return before.LastSeq, err
		},
		interrupt: func(call context.Context) error {
			request, cancel := context.WithTimeout(call, 5*time.Second)
			defer cancel()
			_, err := current.Interrupt(request, session.WriteOptions{})
			return err
		},
		permission: func(_ context.Context, ticket, digest, verdict string) error {
			return write(func(request context.Context, opts session.WriteOptions) error {
				_, err := current.Permission(request, ticket, digest, verdict, opts)
				return err
			})
		},
		answer: func(_ context.Context, ticket string, answers []session.Answer) error {
			return write(func(request context.Context, opts session.WriteOptions) error {
				_, err := current.Answer(request, ticket, answers, opts)
				return err
			})
		},
	}
	fmt.Fprintln(out, "Commands: /cancel, /allow <ticket>, /deny <ticket>, /answers <ticket> <JSON array>, /quit")
	fmt.Fprintln(out, "No approval is automatic. Type a message while idle to start a new turn.")
	return chatLoop(ctx, hooks, readLines(reading, input), events, out, opts.message, meta.Status == "running", meta.LastSeq)
}
