package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tansrai/tansr-go/api"
	"github.com/tansrai/tansr-go/canonical"
	"github.com/tansrai/tansr-go/executor"
)

type preparationTransport func(*http.Request) (*http.Response, error)

func (f preparationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func preparationClient(t *testing.T, handle func(http.ResponseWriter, *http.Request)) (*Client, *executor.Client) {
	t.Helper()
	transport, err := api.New(api.Options{BaseURL: "http://localhost", Token: "fixture", SessionFamily: "sdk2-offload-v1", EventEnvelope: true,
		HTTPClient: &http.Client{Transport: preparationTransport(func(r *http.Request) (*http.Response, error) {
			w := httptest.NewRecorder()
			handle(w, r)
			return w.Result(), nil
		})}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(transport)
	if err != nil {
		t.Fatal(err)
	}
	e, err := executor.NewClient(transport, executor.Scope{ApplicationScopeID: "app", EndUserID: "user", AuthorizationRevision: "1"})
	if err != nil {
		t.Fatal(err)
	}
	return c, e
}

func preparationReply(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch r.URL.Path {
	case testPath(api.OpSessionCapabilities, nil):
		response(w, "session", 200, map[string]any{"protocol": "sdk2-ext-v1", "contracts": []any{map[string]any{"contract": "sdk2-offload-v1", "availability": "source-managed"}}})
	case testPath(api.OpSessionCreate, nil):
		response(w, "session", 201, map[string]any{"sessionId": "s1", "resumed": false, "lastSeq": 0, "contract": "sdk2-offload-v1", "availability": "source-managed"})
	case testPath(api.OpDiscoverySessionCapabilities, map[string]string{"id": "s1"}):
		closure(w, api.StateEnabled)
	case testPath(api.OpExecutionInitialize, map[string]string{"id": "s1"}):
		var body struct {
			Platform executor.Platform `json:"platform"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		out := executor.Capabilities{Protocol: executor.Protocol, SessionID: "s1", Platform: &body.Platform, CapabilityRevision: closureID, EffectiveTools: []executor.EffectiveTool{}, Binding: nil}
		raw, _ := json.Marshal(out)
		var value any
		_ = json.Unmarshal(raw, &value)
		encoded, err := canonical.Encode(value, canonical.Options{MaxBytes: 262144})
		if err != nil {
			t.Fatal(err)
		}
		w.Header().Set(api.HeaderContract, api.Contract)
		w.Header().Set(api.HeaderManifestRevision, "17")
		w.Header().Set(api.HeaderDomain, "execution")
		w.Header().Set(api.HeaderSchemaHash, "none")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
	case testPath(api.OpSessionGet, map[string]string{"id": "s1"}):
		response(w, "session", 200, map[string]any{"sessionId": "s1", "live": true, "lastSeq": 0, "status": "idle", "contract": "sdk2-offload-v1", "availability": "source-managed"})
	default:
		t.Fatalf("unexpected operation %s", r.URL.Path)
	}
}

func TestPreparationOriginalIntentAndToolPresence(t *testing.T) {
	for _, tools := range [][]string{nil, {}} {
		t.Run(map[bool]string{true: "omitted", false: "empty"}[tools == nil], func(t *testing.T) {
			var creates, initializes int
			var confirmed bool
			var progress []PreparationProgress
			c, e := preparationClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == testPath(api.OpSessionCreate, nil) {
					creates++
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["requestId"] != "original-key" || body["resume"].(map[string]any)["sessionId"] != "s1" {
						t.Fatalf("intent changed: %#v", body)
					}
				}
				if r.URL.Path == testPath(api.OpExecutionInitialize, map[string]string{"id": "s1"}) {
					initializes++
					if !confirmed {
						t.Fatal("identity not delivered before initialize")
					}
					body, _ := io.ReadAll(r.Body)
					var parsed map[string]json.RawMessage
					_ = json.Unmarshal(body, &parsed)
					_, exists := parsed["requestedTools"]
					if exists != (tools != nil) {
						t.Fatal("tool presence changed")
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
				}
				preparationReply(t, w, r)
			})
			ctx, cancel := context.WithCancel(context.Background())
			result, err := c.Prepare(ctx, CreateOptions{RequestID: "original-key", Resume: &ResumeReference{SessionID: "s1"}}, PreparationOptions{Execution: e, RequestedTools: tools,
				OnSessionConfirmed: func(s *Session) { confirmed = s.ID() == "s1" }, OnProgress: func(p PreparationProgress) { progress = append(progress, p) }})
			if err != nil {
				t.Fatal(err)
			}
			if result.Capabilities.CapabilityRevision != closureID || creates != 1 || initializes != 1 {
				t.Fatalf("unexpected result/calls %+v %d %d", result, creates, initializes)
			}
			var stages []PreparationStage
			for _, p := range progress {
				if p.Status == "completed" {
					stages = append(stages, p.Stage)
				}
			}
			if !reflect.DeepEqual(stages, []PreparationStage{PreparationConnecting, PreparationSessionConfirmed, PreparationInitializingCapabilities, PreparationReady}) {
				t.Fatal(stages)
			}
			cancel()
			if _, err = result.Session.Meta(context.Background()); err != nil {
				t.Fatalf("startup cancellation leaked into session: %v", err)
			}
		})
	}
}

func TestPreparationCancellationRetainsConfirmedSession(t *testing.T) {
	var requests int
	c, e := preparationClient(t, func(w http.ResponseWriter, r *http.Request) { requests++; preparationReply(t, w, r) })
	ctx, cancel := context.WithCancel(context.Background())
	_, err := c.Prepare(ctx, CreateOptions{RequestID: "retained"}, PreparationOptions{Execution: e, OnSessionConfirmed: func(*Session) { cancel() }, OnProgress: func(PreparationProgress) { panic("UI failure") }})
	var failed *PreparationError
	if !errors.As(err, &failed) || !errors.Is(err, context.Canceled) || failed.Session == nil || failed.Session.ID() != "s1" || requests != 2 {
		t.Fatalf("lost confirmation/cancellation: %#v requests=%d", err, requests)
	}
}

func TestPreparationFailureAndNoRetry(t *testing.T) {
	var creates, initializes int
	c, e := preparationClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testPath(api.OpSessionCreate, nil) {
			creates++
		}
		if r.URL.Path == testPath(api.OpExecutionInitialize, map[string]string{"id": "s1"}) {
			initializes++
			response(w, "execution", 200, map[string]any{"invalid": "fixture"})
			return
		}
		preparationReply(t, w, r)
	})
	_, err := c.Prepare(context.Background(), CreateOptions{RequestID: "retained"}, PreparationOptions{Execution: e})
	var failed *PreparationError
	if !errors.As(err, &failed) || failed.Session == nil || failed.Stage != PreparationInitializingCapabilities || creates != 1 || initializes != 1 || errors.Unwrap(failed) == nil {
		t.Fatalf("unexpected failure: %v", err)
	}
}

func TestPreparationRejectsMixedTransportBeforeCreate(t *testing.T) {
	var requests atomic.Int32
	handler := func(w http.ResponseWriter, r *http.Request) { requests.Add(1); preparationReply(t, w, r) }
	c, _ := preparationClient(t, handler)
	_, other := preparationClient(t, handler)
	if _, err := c.Prepare(context.Background(), CreateOptions{RequestID: "retained"}, PreparationOptions{Execution: other}); err == nil || requests.Load() != 0 {
		t.Fatal("mixed principal transport accepted")
	}
}

func TestPreparationDifferentAttemptsDoNotSerialize(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	releaseSlow := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseSlow()
	c, e := preparationClient(t, func(w http.ResponseWriter, r *http.Request) { preparationReply(t, w, r) })
	done := make(chan error, 1)
	go func() {
		_, err := c.Prepare(context.Background(), CreateOptions{RequestID: "first"}, PreparationOptions{Execution: e, OnSessionConfirmed: func(*Session) { once.Do(func() { close(entered) }); <-release }})
		done <- err
	}()
	<-entered
	second := make(chan error, 1)
	go func() {
		_, err := c.Prepare(context.Background(), CreateOptions{RequestID: "second"}, PreparationOptions{Execution: e})
		second <- err
	}()
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		releaseSlow()
		t.Fatal("independent attempt blocked")
	}
	releaseSlow()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPreparationLateCreateReceiptRetainedAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var confirmed bool
	c, e := preparationClient(t, func(w http.ResponseWriter, r *http.Request) {
		preparationReply(t, w, r)
		if r.URL.Path == testPath(api.OpSessionCreate, nil) {
			cancel()
		}
	})
	_, err := c.Prepare(ctx, CreateOptions{RequestID: "retained"}, PreparationOptions{Execution: e, OnSessionConfirmed: func(*Session) { confirmed = true }})
	var failed *PreparationError
	if !errors.As(err, &failed) || !errors.Is(err, context.Canceled) || failed.Session == nil || !confirmed {
		t.Fatalf("late validated ID was lost: %v", err)
	}
}

func TestPreparationDisabledCapabilityDoesNotInitialize(t *testing.T) {
	var initialize int
	c, e := preparationClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testPath(api.OpDiscoverySessionCapabilities, map[string]string{"id": "s1"}) {
			closure(w, api.StateUnavailable)
			return
		}
		if r.URL.Path == testPath(api.OpExecutionInitialize, map[string]string{"id": "s1"}) {
			initialize++
		}
		preparationReply(t, w, r)
	})
	_, err := c.Prepare(context.Background(), CreateOptions{RequestID: "retained"}, PreparationOptions{Execution: e})
	var denied *CapabilityError
	if !errors.As(err, &denied) || initialize != 0 {
		t.Fatalf("disabled initialization attempted: %v", err)
	}
}
