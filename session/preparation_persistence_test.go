package session

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tansrai/tansr-go/api"
)

type preparationResource func() error

func (f preparationResource) Stop() error { return f() }

func TestPreparationPersistenceReadyGateAndOwnership(t *testing.T) {
	var calls, stops atomic.Int32
	c, e := preparationClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		preparationReply(t, w, r)
	})
	entered, ready := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-ready:
		default:
			close(ready)
		}
	}()
	type result struct {
		prepared *PreparedSession
		err      error
	}
	done := make(chan result, 1)
	var stages []PreparationStage
	go func() {
		p, err := c.Prepare(context.Background(), CreateOptions{RequestID: "original-provider-key"}, PreparationOptions{
			Execution: e,
			PreparePersistence: func(context.Context) (PersistenceResource, error) {
				close(entered)
				<-ready
				return preparationResource(func() error { stops.Add(1); return nil }), nil
			},
			OnProgress: func(p PreparationProgress) {
				if p.Status == "completed" {
					stages = append(stages, p.Stage)
				}
			},
		})
		done <- result{p, err}
	}()
	<-entered
	if calls.Load() != 0 {
		t.Fatal("HTTP started before persistence ready")
	}
	// 独立 cloud 准备不等待这个 provider，不存在全局队列。
	cloud, err := c.Prepare(context.Background(), CreateOptions{RequestID: "cloud-key"}, PreparationOptions{Execution: e})
	if err != nil || cloud.Stop() != nil || stops.Load() != 0 {
		t.Fatalf("independent cloud preparation: %v", err)
	}
	close(ready)
	select {
	case got := <-done:
		if got.err != nil || stops.Load() != 0 {
			t.Fatalf("owned resource stopped before handoff: %v", got.err)
		}
		want := []PreparationStage{PreparationPersistence, PreparationConnecting, PreparationSessionConfirmed, PreparationInitializingCapabilities, PreparationReady}
		if !reflect.DeepEqual(stages, want) {
			t.Fatalf("stages: %v", stages)
		}
		if got.prepared.Stop() != nil || got.prepared.Stop() != nil || stops.Load() != 1 {
			t.Fatal("owned stop was not exactly once")
		}
		if _, err := got.prepared.Session.Meta(context.Background()); err != nil {
			t.Fatalf("local stop must not close remote session: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("preparation did not settle after readiness")
	}
}

func TestPreparationPersistenceFailureDoesNotCreate(t *testing.T) {
	var calls int
	c, e := preparationClient(t, func(http.ResponseWriter, *http.Request) { calls++ })
	want := errors.New("provider unavailable")
	_, err := c.Prepare(context.Background(), CreateOptions{}, PreparationOptions{Execution: e,
		PreparePersistence: func(context.Context) (PersistenceResource, error) { return nil, want }})
	var failure *PreparationError
	if !errors.As(err, &failure) || !errors.Is(err, want) || failure.Stage != PreparationPersistence || failure.Session != nil || calls != 0 || failure.Stop() != nil {
		t.Fatalf("provider failure changed or performed HTTP: %#v, calls=%d", failure, calls)
	}
}

func TestPreparationPersistenceLateCancelledResourceIsStopped(t *testing.T) {
	var calls, stops int
	c, e := preparationClient(t, func(http.ResponseWriter, *http.Request) { calls++ })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := c.Prepare(ctx, CreateOptions{}, PreparationOptions{Execution: e,
		PreparePersistence: func(control context.Context) (PersistenceResource, error) {
			if control != ctx {
				t.Fatal("startup control changed")
			}
			cancel()
			return preparationResource(func() error { stops++; return nil }), nil
		}})
	var failure *PreparationError
	if !errors.As(err, &failure) || !errors.Is(err, context.Canceled) || calls != 0 || stops != 1 || failure.Stop() != nil || stops != 1 {
		t.Fatalf("late resource abandoned or create issued: %v, calls=%d stops=%d", err, calls, stops)
	}
}

func TestPreparationPersistenceCleanupPreservesOriginalFailure(t *testing.T) {
	var stops int
	c, e := preparationClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testPath(api.OpExecutionInitialize, map[string]string{"id": "s1"}) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		preparationReply(t, w, r)
	})
	cleanup := errors.New("stop result unknown")
	_, err := c.Prepare(context.Background(), CreateOptions{RequestID: "unchanged-key"}, PreparationOptions{Execution: e,
		PreparePersistence: func(context.Context) (PersistenceResource, error) {
			return preparationResource(func() error { stops++; return cleanup }), nil
		}})
	var failure *PreparationError
	if !errors.As(err, &failure) || failure.Stage != PreparationInitializingCapabilities || failure.Session == nil || failure.Session.ID() != "s1" || failure.Cause == cleanup || !errors.Is(failure.CleanupError, cleanup) || stops != 1 {
		t.Fatalf("original confirmed/error lost: %#v, stops=%d", failure, stops)
	}
	if !errors.Is(failure.Stop(), cleanup) || stops != 1 {
		t.Fatal("unknown cleanup repeated or lost")
	}
}

func TestPreparationPersistenceBorrowedHasNoStopAuthority(t *testing.T) {
	var prepared int
	c, e := preparationClient(t, func(w http.ResponseWriter, r *http.Request) { preparationReply(t, w, r) })
	p, err := c.Prepare(context.Background(), CreateOptions{RequestID: "borrowed-original-key"}, PreparationOptions{Execution: e,
		PreparePersistence: func(context.Context) (PersistenceResource, error) { prepared++; return nil, nil }})
	if err != nil || prepared != 1 || p.Stop() != nil || p.Stop() != nil {
		t.Fatalf("borrowed readiness failed: %v", err)
	}
}

func TestPreparationPersistenceUnexpectedUnwindStillCleansOwned(t *testing.T) {
	var stops int
	c, e := preparationClient(t, func(http.ResponseWriter, *http.Request) { panic("original transport panic") })
	defer func() {
		if got := recover(); got != "original transport panic" || stops != 1 {
			t.Fatalf("panic replaced or owned resource abandoned: %v, stops=%d", got, stops)
		}
	}()
	_, _ = c.Prepare(context.Background(), CreateOptions{RequestID: "original-key"}, PreparationOptions{Execution: e,
		PreparePersistence: func(context.Context) (PersistenceResource, error) {
			return preparationResource(func() error { stops++; return nil }), nil
		}})
	t.Fatal("original transport panic was swallowed")
}

func TestPreparationPersistenceOriginalDeadlinePreventsProvider(t *testing.T) {
	var calls, prepares int
	c, e := preparationClient(t, func(http.ResponseWriter, *http.Request) { calls++ })
	_, err := c.Prepare(context.Background(), CreateOptions{RequestID: "original-key", WriteOptions: WriteOptions{Deadline: time.Now().Add(-time.Second)}}, PreparationOptions{Execution: e,
		PreparePersistence: func(context.Context) (PersistenceResource, error) { prepares++; return nil, nil }})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 0 || prepares != 0 {
		t.Fatalf("expired original deadline started work: %v calls=%d providers=%d", err, calls, prepares)
	}
}
