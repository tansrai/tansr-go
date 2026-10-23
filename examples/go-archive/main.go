// Command go-archive demonstrates explicit archive binding, verified downloads,
// encrypted local persistence and ACK after durability. It is not a context manager.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/tansrai/tansr-go/archive"
	"github.com/tansrai/tansr-go/examples/internal/demoutil"
)

type options struct {
	base, session, binding, source, path string
	mode                                 string
	recoverAck                           string
	maxPages                             int
}

func bindFlags(flags *flag.FlagSet, opts *options) {
	flags.StringVar(&opts.base, "base", "http://127.0.0.1:8787", "Serve origin")
	flags.StringVar(&opts.session, "session", "", "session to bind; required if -binding is absent")
	flags.StringVar(&opts.binding, "binding", "", "reopen an existing archive binding")
	flags.StringVar(&opts.source, "source", "go-demo", "stable source identity for a new binding")
	flags.StringVar(&opts.mode, "mode", "terminal", "explicit persistence mode: terminal or cloud")
	flags.StringVar(&opts.path, "file", "", "local encrypted archive filename (terminal mode only)")
	flags.IntVar(&opts.maxPages, "max-pages", 64, "maximum pages this run; rerun to continue")
	flags.Func("recover-ack", "recover one pending ACK with REQUEST_ID, then exit; requires -binding and an existing -file; may upgrade local format (older SDKs cannot reopen)", func(requestID string) error {
		if requestID == "" {
			return errors.New("recover-ack requires a request ID")
		}
		opts.recoverAck = requestID
		return nil
	})
}

func main() {
	var opts options
	bindFlags(flag.CommandLine, &opts)
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := run(ctx, opts); err != nil {
		fmt.Fprintln(os.Stderr, "go-archive:", demoutil.Text(demoutil.Describe(err).Error()))
		os.Exit(1)
	}
}

func archiveKey() ([]byte, error) {
	key, err := hex.DecodeString(os.Getenv("TANSR_ARCHIVE_KEY"))
	if err != nil || len(key) != 32 {
		return nil, errors.New("TANSR_ARCHIVE_KEY must contain a 32-byte key encoded as 64 hex characters")
	}
	return key, nil
}

func run(ctx context.Context, opts options) error {
	if (opts.mode != "" && opts.mode != "terminal" && opts.mode != "cloud") || opts.mode != "cloud" && opts.path == "" || opts.session == "" && opts.binding == "" || opts.session != "" && opts.binding != "" || opts.maxPages < 1 || opts.maxPages > 1024 {
		return errors.New("choose terminal/cloud, provide -file for terminal, exactly one of -session / -binding, and max-pages between 1 and 1024")
	}
	if opts.mode == "cloud" && (opts.path != "" || opts.recoverAck != "") {
		return errors.New("cloud uses the trusted host archive; local archive/ACK recovery options do not apply")
	}
	if opts.recoverAck != "" {
		if opts.binding == "" {
			return errors.New("recover-ack requires -binding for the existing archive binding")
		}
		info, err := os.Stat(opts.path)
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("recover-ack requires an existing regular archive file; retain the original file and key")
		}
	}
	transport, err := demoutil.Client(opts.base)
	if err != nil {
		return err
	}
	client := archive.NewClient(transport)
	var binding archive.Binding
	if opts.binding != "" {
		binding, err = client.Binding(ctx, opts.binding)
	} else {
		target, readErr := client.BindingTarget(ctx, opts.session)
		if readErr != nil {
			return readErr
		}
		if target.BindingID != nil {
			binding, err = client.Binding(ctx, *target.BindingID)
		} else {
			if opts.mode == "cloud" {
				return archive.ErrSourceManaged
			}
			key, keyErr := demoutil.RequestID()
			if keyErr != nil {
				return keyErr
			}
			fmt.Println("binding request:", key)
			binding, err = client.Create(ctx, opts.session, opts.source, key)
		}
	}
	if err != nil {
		return err
	}
	fmt.Println("binding:", demoutil.Text(binding.BindingID))
	status, err := client.Status(ctx, binding.BindingID)
	if err != nil {
		return err
	}
	identity, err := archive.IdentityFrom(binding, status)
	if err != nil {
		return err
	}
	if binding.Persistence != nil && binding.Persistence.Mode != opts.mode && opts.mode != "" {
		return errors.New("requested mode differs from the trusted Serve deployment")
	}
	if opts.mode == "cloud" {
		if binding.Persistence == nil || binding.Availability != "source-managed" {
			return archive.ErrSourceManaged
		}
		fmt.Println("cloud archive verified; durable publication and execution journal belong to the configured platform source; no local store or ACK was created")
		return nil
	}
	key, err := archiveKey()
	if err != nil {
		return err
	}
	defer clear(key)
	path, err := filepath.Abs(opts.path)
	if err != nil {
		return err
	}
	// The developer supplies a private terminal directory and OS key store.
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	store, err := archive.OpenFileStore(archive.StoreOptions{Path: path, Key: key, Identity: identity,
		CheckAccess: func(current archive.Identity) error {
			// This short-lived CLI process pins its authenticated binding. An app
			// must additionally consult its current login and revocation state.
			if current != identity {
				return archive.ErrIntegrity
			}
			return ctx.Err()
		}})
	if err != nil {
		return err
	}
	defer store.Close()
	if opts.recoverAck != "" {
		fmt.Println("Explicit ACK recovery: a confirmed stale revision may upgrade this archive to format v2; older SDKs cannot reopen it. Existing prepared recovery keeps its saved request identity. Keep the same file, key and recovery ID if interrupted.")
		result, err := archive.RecoverPending(ctx, client, store, opts.recoverAck)
		return showRecoveryResult(os.Stdout, result, err)
	}
	for page := 0; page < opts.maxPages; page++ {
		request, err := demoutil.RequestID()
		if err != nil {
			return err
		}
		result, err := archive.SyncOnce(ctx, client, store, request)
		if err != nil {
			return err
		}
		fmt.Printf("page %d: %d verified records, complete=%v, recovered-pending-ack=%v\n", page+1, result.Records, result.Complete, result.Recovered)
		if result.Complete {
			fmt.Println("archive synchronized; persisted archive coverage is independent of SSE position")
			return nil
		}
	}
	return errors.New("page limit reached; rerun with the same binding, file and key to continue")
}

func showRecoveryResult(output io.Writer, result archive.SyncResult, err error) error {
	if err != nil {
		return err
	}
	if !result.Recovered || result.Receipt == nil || result.Receipt.State != "completed" {
		return archive.ErrReceipt
	}
	_, err = fmt.Fprintf(output, "pending ACK confirmed; receipt request: %s\nRecovery-only run ended; rerun without -recover-ack to synchronize remaining pages.\n", demoutil.Text(result.Receipt.Request.RequestID))
	return err
}
