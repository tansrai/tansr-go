package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A29 golden replay: every vector of the vendored unified-v1 golden is served over /api by a fake Serve
// and driven through the public client surface (Manifest / Capabilities / SessionCapabilities / Call /
// Events). Positives must come back as the typed success or the typed *APIError; negatives must never
// come back as a success - and the client must never attempt a legacy path while doing so.
//
// The wire cannot carry every golden distinction (a header value that is a JSON number, a header the
// typed client API cannot express, a build-form manifest Serve never serves). Such vectors are not
// skipped silently: they are classified by name below and the test fails when the classification and
// the vendored golden disagree.

// replayOutcome is the per-vector reading the replay records.
type replayOutcome string

const (
	outcomeAccepted       replayOutcome = "accepted"         // valid - typed success / typed APIError
	outcomeRejected       replayOutcome = "rejected"         // invalid - error, not a success
	outcomeUnifiedOnly    replayOutcome = "unified-only"     // FacadeError-invalid but UnifiedError-valid: accepted as APIError, by design
	outcomeToleratedHdr   replayOutcome = "tolerated-header" // ResponseHeaders-invalid the client reads like Node does (named)
	outcomeUnexpressible  replayOutcome = "unexpressible"    // RequestHeaders the typed client API cannot produce (named)
	outcomeNotWireForm    replayOutcome = "not-wire-form"    // Manifest build artifact: Serve never serves it, the client refuses it
	outcomeLocallyRefused replayOutcome = "locally-refused"  // RequestHeaders-invalid refused before any request
)

// replayClassified names the vectors whose outcome is not the plain accepted / rejected reading.
var replayClassified = map[string]replayOutcome{
	// Manifest: the repo artifact lacks the runtime view; discovery.manifest never serves it.
	"manifest-repo-artifact": outcomeNotWireForm,
	// FacadeError negatives that are legal UnifiedError envelopes: the wire does not say "facade" (the
	// three-head rejects deliberately reuse UnifiedError), so the client validates every unified envelope
	// against UnifiedError. Node ./api decodeErrorBody checks only the code / action vocabularies and the
	// status; Go validates the full UnifiedError definition - stricter, never looser.
	"facade-error-request-id-not-null":          outcomeUnifiedOnly, // UnifiedError allows a requestId string
	"facade-error-code-not-facade":              outcomeUnifiedOnly, // forbidden is a unified code
	"facade-error-retry-after-ms":               outcomeUnifiedOnly, // UnifiedError allows retryAfterMs
	"facade-error-precondition-without-detail":  outcomeUnifiedOnly, // facade binds precondition_failed to detail.closureId; unified does not
	"facade-error-403-without-outside-closure":  outcomeUnifiedOnly, // facade binds 403 to outside_closure; unified does not
	"facade-error-outside-closure-enabled":      outcomeUnifiedOnly, // facade forbids state enabled; unified does not
	"facade-error-404-outside-closure-disabled": outcomeUnifiedOnly, // facade binds state disabled to 403; unified does not
	// ResponseHeaders negatives the client reads the way Node ./api does.
	"response-headers-revision-number":  outcomeToleratedHdr, // a JSON number is "4" on the wire
	"response-headers-identity-leak":    outcomeToleratedHdr, // unknown tansr-* response header is ignored, never used
	"response-headers-retry-after-zero": outcomeToleratedHdr, // "0" decodes as a zero wait (Node parseRetryAfter)
	// RequestHeaders the typed Go API cannot emit at all (no option produces the form).
	"request-headers-all-unified":            outcomeUnexpressible, // W/"cfg-3": only the strong "<revision>" form is accepted (Node normalizeIfMatch)
	"request-headers-deadline-not-rfc3339":   outcomeUnexpressible, // Deadline is a time.Time
	"request-headers-event-envelope-unknown": outcomeUnexpressible, // EventEnvelope is a bool - only unified-v1
	"request-headers-unknown-tansr-header":   outcomeUnexpressible, // no option adds tansr-* headers
	"request-headers-traceparent-form":       outcomeUnexpressible, // the client does not emit traceparent
	"request-headers-authorization-scheme":   outcomeUnexpressible, // Token is always sent as Bearer
}

// replayServe serves one golden vector at the operation path the replay drives.
type replayServe struct {
	t          *testing.T
	mu         sync.Mutex
	seen       []*http.Request
	definition string
	value      any
	headers    map[string]string // ResponseHeaders vectors: served verbatim
	revision   int
	schemaHash string
	closureID  string
}

func (s *replayServe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.seen = append(s.seen, r.Clone(context.Background()))
	s.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, "/v2/") || strings.HasPrefix(r.URL.Path, "/v3/") || !strings.HasPrefix(r.URL.Path, "/api/") {
		s.t.Errorf("client attempted a non-/api path %s", r.URL.Path)
		http.Error(w, "legacy", http.StatusNotFound)
		return
	}
	h := w.Header()
	body, _ := s.value.(map[string]any)
	switch s.definition {
	case DefResponseHeaders:
		for k, v := range s.headers {
			h.Set(k, v)
		}
		writeJSON(w, 200, map[string]any{"protocol": "sdk2-ext-v1", "bindingId": "b-1"})
	case DefManifest:
		revision, schemaHash := s.revision, s.schemaHash
		if n, ok := integerOf(body["revision"]); ok && n >= 1 {
			revision = int(n)
		}
		if hash, ok := body["schemaHash"].(string); ok && hex64.MatchString(hash) {
			schemaHash = "sha256:" + hash
		}
		s.unified(h, "discovery", revision, schemaHash)
		writeJSON(w, 200, s.value)
	case DefCapabilities:
		revision, schemaHash := s.revision, s.schemaHash
		if n, ok := integerOf(body["manifestRevision"]); ok && n >= 1 {
			revision = int(n)
		}
		if hash, ok := body["schemaHash"].(string); ok && schemaHashPattern.MatchString(hash) {
			schemaHash = hash
		}
		s.unified(h, "discovery", revision, schemaHash)
		writeJSON(w, 200, s.value)
	case DefCapabilityClosure:
		s.unified(h, "discovery", s.revision, s.schemaHash)
		if id, ok := body["closureId"].(string); ok && hex64.MatchString(id) {
			h.Set(HeaderClosureID, id)
		}
		writeJSON(w, 200, s.value)
	case DefFacadeError, DefUnifiedError:
		status := 500
		if n, ok := integerOf(body["status"]); ok && n >= 400 && n <= 599 {
			status = int(n)
		}
		s.unified(h, "session", s.revision, "none")
		if detail, ok := body["detail"].(map[string]any); ok {
			if id, ok := detail["closureId"].(string); ok && hex64.MatchString(id) {
				h.Set(HeaderClosureID, id)
			}
		}
		writeJSON(w, status, s.value)
	case DefEventEnvelope:
		s.unified(h, "session", s.revision, "none")
		h.Set(HeaderEventEnvelope, Contract)
		h.Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		frame, _ := json.Marshal(s.value)
		_, _ = io.WriteString(w, "id: 1\nevent: replay\ndata: "+string(frame)+"\n\n")
	case DefRequestHeaders:
		s.unified(h, "archive", s.revision, "none")
		writeJSON(w, 200, map[string]any{"protocol": "sdk2-ext-v1", "bindingId": "b-1", "revision": "4", "status": "closed"})
	default:
		s.t.Fatalf("replay has no server for %s", s.definition)
	}
}

func (s *replayServe) unified(h http.Header, domain string, revision int, schemaHash string) {
	h.Set(HeaderContract, Contract)
	h.Set(HeaderManifestRevision, strconv.Itoa(revision))
	h.Set(HeaderDomain, domain)
	h.Set(HeaderSchemaHash, schemaHash)
	h.Set(HeaderTraceID, "replay-trace")
}

func (s *replayServe) requests() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.seen...)
}

// headerStrings renders a header-projection vector as wire strings (a JSON number becomes its decimal).
func headerStrings(value any) (map[string]string, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	out := map[string]string{}
	for k, v := range object {
		switch x := v.(type) {
		case string:
			out[k] = x
		case json.Number:
			out[k] = x.String()
		default:
			raw, _ := json.Marshal(v)
			out[k] = string(raw)
		}
	}
	return out, true
}

func TestGoldenReplayOverAPI(t *testing.T) {
	golden, byName := loadGolden(t)
	view := materialise(t, byName["manifest-runtime-view"], byName).(map[string]any)
	revision, _ := integerOf(view["revision"])
	baseHash := "sha256:" + view["schemaHash"].(string)
	clock := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

	outcomes := map[replayOutcome]int{}
	byExpect := map[string]map[replayOutcome]int{"valid": {}, "invalid": {}}
	perDefinition := map[string]map[replayOutcome]int{}
	record := func(v *goldenVector, outcome replayOutcome) {
		outcomes[outcome]++
		byExpect[v.Expect][outcome]++
		if perDefinition[v.Definition] == nil {
			perDefinition[v.Definition] = map[replayOutcome]int{}
		}
		perDefinition[v.Definition][outcome]++
		plain := outcome == outcomeAccepted || outcome == outcomeRejected || outcome == outcomeLocallyRefused
		if want, named := replayClassified[v.Name]; named == plain || (named && want != outcome) {
			t.Errorf("%s: outcome %s, classification %q (named=%v)", v.Name, outcome, want, named)
		}
	}

	for i := range golden.Vectors {
		v := &golden.Vectors[i]
		t.Run(v.Name, func(t *testing.T) {
			value := materialise(t, v, byName)
			serve := &replayServe{t: t, definition: v.Definition, value: value, revision: int(revision), schemaHash: baseHash}
			if v.Definition == DefResponseHeaders {
				serve.headers, _ = headerStrings(value)
			}
			server := httptest.NewServer(serve)
			defer server.Close()
			outcome := replayVector(t, v, value, server, clock)
			for _, r := range serve.requests() {
				if !strings.HasPrefix(r.URL.Path, "/api/") {
					t.Fatalf("non-/api path %s", r.URL.Path)
				}
			}
			record(v, outcome)
		})
	}

	// Every vector produced a reading; positives were accepted (or are the one non-wire form / the one
	// header set the typed API cannot express); negatives were never a success.
	total := 0
	for _, n := range outcomes {
		total += n
	}
	if total != goldenTotal {
		t.Fatalf("replayed %d vectors, golden has %d", total, goldenTotal)
	}
	sum := func(m map[replayOutcome]int, allowed ...replayOutcome) int {
		n := 0
		for outcome, count := range m {
			found := false
			for _, a := range allowed {
				found = found || a == outcome
			}
			if !found {
				t.Fatalf("outcome %s is not a legal reading here: %v", outcome, m)
			}
			n += count
		}
		return n
	}
	if got := sum(byExpect["valid"], outcomeAccepted, outcomeNotWireForm, outcomeUnexpressible); got != goldenValid || byExpect["valid"][outcomeAccepted] != goldenValid-2 {
		t.Fatalf("valid readings %v, want %d valid with %d accepted", byExpect["valid"], goldenValid, goldenValid-2)
	}
	if got := sum(byExpect["invalid"], outcomeRejected, outcomeLocallyRefused, outcomeUnifiedOnly, outcomeToleratedHdr, outcomeUnexpressible); got != goldenInvalid {
		t.Fatalf("invalid readings %v, want %d invalid", byExpect["invalid"], goldenInvalid)
	}
	if len(replayClassified) != outcomes[outcomeNotWireForm]+outcomes[outcomeUnifiedOnly]+outcomes[outcomeToleratedHdr]+outcomes[outcomeUnexpressible] {
		t.Fatalf("classified %d names, observed %v", len(replayClassified), outcomes)
	}
	defs := make([]string, 0, len(perDefinition))
	for d := range perDefinition {
		defs = append(defs, d)
	}
	sort.Strings(defs)
	for _, d := range defs {
		t.Logf("%-18s %v", d, perDefinition[d])
	}
	t.Logf("total %v", outcomes)
}

// replayVector drives one vector through the client and returns its reading.
func replayVector(t *testing.T, v *goldenVector, value any, server *httptest.Server, clock time.Time) replayOutcome {
	t.Helper()
	ctx := context.Background()
	valid := v.Expect == "valid"
	body, _ := value.(map[string]any)
	newClient := func(envelope bool) *Client {
		c, err := New(Options{BaseURL: server.URL, Token: "t0ken", SessionFamily: "sdk2-offload-v1", EventEnvelope: envelope, Now: func() time.Time { return clock }})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	var cerr *ClientError
	var apiErr *APIError
	var unavailable *ContractUnavailableError
	var domainErr *DomainError
	mustFail := func(err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("invalid vector was accepted as a success (%s)", v.Note)
		}
		if !errors.As(err, &cerr) && !errors.As(err, &unavailable) && !errors.As(err, &apiErr) && !errors.As(err, &domainErr) {
			t.Fatalf("unexpected error type %T: %v", err, err)
		}
	}

	switch v.Definition {
	case DefManifest:
		res, err := newClient(false).Manifest(ctx)
		if v.Name == "manifest-repo-artifact" {
			if !errors.As(err, &cerr) || cerr.Code != CodeInvalidResponse {
				t.Fatalf("build-form manifest must be refused as invalid_response: %v", err)
			}
			return outcomeNotWireForm
		}
		if valid {
			if err != nil || res.Body.Revision != int(mustInt(t, body["revision"])) || len(res.Body.Operations) == 0 {
				t.Fatalf("manifest: %v", err)
			}
			return outcomeAccepted
		}
		mustFail(err)
		return outcomeRejected

	case DefCapabilities:
		res, err := newClient(false).Capabilities(ctx)
		if valid {
			if err != nil || res.Body.ManifestRevision != int(mustInt(t, body["manifestRevision"])) {
				t.Fatalf("capabilities: %v", err)
			}
			return outcomeAccepted
		}
		mustFail(err)
		return outcomeRejected

	case DefCapabilityClosure:
		res, err := newClient(false).SessionCapabilities(ctx, "s-1")
		if valid {
			if err != nil || res.ClosureID != body["closureId"].(string) || len(res.Closure.Operations) != 87 {
				t.Fatalf("closure: %v", err)
			}
			return outcomeAccepted
		}
		mustFail(err)
		return outcomeRejected

	case DefFacadeError, DefUnifiedError:
		_, err := newClient(false).Call(ctx, OpSessionMessageSend, CallOptions{Params: map[string]string{"id": "s-1"}, Body: map[string]any{"text": "hi"}})
		if err == nil {
			t.Fatalf("an error envelope must never be a success")
		}
		unifiedValid := Validate(DefUnifiedError, value) == nil
		if valid {
			if !errors.As(err, &apiErr) {
				t.Fatalf("valid envelope must decode as *APIError, got %T %v", err, err)
			}
			assertEnvelopeProjection(t, body, apiErr)
			return outcomeAccepted
		}
		if errors.As(err, &apiErr) {
			if !unifiedValid {
				t.Fatalf("invalid envelope decoded as *APIError: %+v", apiErr)
			}
			assertEnvelopeProjection(t, body, apiErr)
			return outcomeUnifiedOnly
		}
		mustFail(err)
		return outcomeRejected

	case DefEventEnvelope:
		stream, err := newClient(true).Events(ctx, OpSessionEventsObserve, EventsOptions{Params: map[string]string{"id": "s-1"}})
		if err != nil {
			t.Fatalf("events: %v", err)
		}
		defer stream.Close()
		frame, err := stream.Next()
		if valid {
			if err != nil || frame.Envelope == nil || frame.Envelope.Domain != body["domain"].(string) {
				t.Fatalf("envelope frame: %v %+v", err, frame)
			}
			return outcomeAccepted
		}
		if !errors.As(err, &cerr) || cerr.Code != CodeInvalidEnvelope {
			t.Fatalf("invalid envelope must be invalid_envelope, got %v (frame %+v)", err, frame)
		}
		return outcomeRejected

	case DefRequestHeaders:
		return replayRequestHeaders(t, v, value, server, clock)

	case DefResponseHeaders:
		headers, _ := headerStrings(value)
		res, err := newClient(false).Call(ctx, OpArchiveBindingGet, CallOptions{Params: map[string]string{"id": "b-1"}})
		if valid {
			if err != nil {
				t.Fatalf("response headers: %v", err)
			}
			assertMetaProjection(t, headers, res.Meta)
			return outcomeAccepted
		}
		if want, ok := replayClassified[v.Name]; ok && want == outcomeToleratedHdr {
			if err != nil {
				t.Fatalf("tolerated header vector was rejected: %v", err)
			}
			switch v.Name {
			case "response-headers-retry-after-zero":
				if !res.Meta.HasRetryAfter || res.Meta.RetryAfter != 0 {
					t.Fatalf("retry-after 0 reads as a zero wait: %+v", res.Meta)
				}
			case "response-headers-revision-number":
				if res.Meta.ManifestRevision != 4 {
					t.Fatalf("revision number on the wire is \"4\": %+v", res.Meta)
				}
			}
			return outcomeToleratedHdr
		}
		if !errors.As(err, &unavailable) {
			t.Fatalf("invalid response headers must be *ContractUnavailableError, got %T %v", err, err)
		}
		return outcomeRejected
	}
	t.Fatalf("no replay for definition %s", v.Definition)
	return ""
}

// replayRequestHeaders maps a RequestHeaders projection onto the typed client options and checks that
// what reaches the wire is exactly the projection (valid) or that the client refused locally (invalid).
func replayRequestHeaders(t *testing.T, v *goldenVector, value any, server *httptest.Server, clock time.Time) replayOutcome {
	t.Helper()
	headers, _ := headerStrings(value)
	if want, ok := replayClassified[v.Name]; ok && want == outcomeUnexpressible {
		// The named vectors carry at least one header the typed API cannot produce; prove that here so
		// the classification cannot go stale when an option is added later.
		reasons := 0
		for k, val := range headers {
			switch k {
			case HeaderIfMatch:
				if _, ok := NormalizeIfMatch(val); !ok {
					reasons++
				}
			case HeaderDeadline:
				if _, err := time.Parse(time.RFC3339Nano, val); err != nil {
					reasons++
				}
			case HeaderEventEnvelope:
				if val != Contract {
					reasons++
				}
			case "authorization":
				if !strings.HasPrefix(val, "Bearer ") {
					reasons++
				}
			case "traceparent", HeaderTraceID:
				reasons++ // the client has no option for correlation headers
			case HeaderIdempotencyKey, HeaderSessionFamily, HeaderClosureID:
			default:
				reasons++ // unknown (tansr-*) header: no option adds it
			}
		}
		if reasons == 0 {
			t.Fatalf("%s is expressible after all: %v", v.Name, headers)
		}
		return outcomeUnexpressible
	}
	opts := Options{BaseURL: server.URL, Token: "t0ken", Now: func() time.Time { return clock }}
	call := CallOptions{Params: map[string]string{"id": "b-1"}, Body: map[string]any{"protocol": "sdk2-ext-v1", "request": map[string]any{"requestId": "r-1"}}}
	operation := OpArchiveBindingClose
	for k, val := range headers {
		switch k {
		case "authorization":
			opts.Token = strings.TrimPrefix(val, "Bearer ")
		case HeaderIdempotencyKey:
			call.IdempotencyKey = val
		case HeaderIfMatch:
			call.IfMatch = val
		case HeaderDeadline:
			at, err := time.Parse(time.RFC3339Nano, val)
			if err != nil {
				t.Fatalf("deadline %q should have been classified unexpressible", val)
			}
			call.Deadline = at
		case HeaderSessionFamily:
			opts.SessionFamily = val
		case HeaderClosureID:
			call.ClosureID = val
			operation = OpSessionMessageSend
			call.Params = map[string]string{"id": "s-1"}
			call.Body = map[string]any{"text": "hi"}
		default:
			t.Fatalf("header %s should have been classified unexpressible", k)
		}
	}
	c, err := New(opts)
	var cerr *ClientError
	if err != nil {
		if v.Expect == "valid" {
			t.Fatalf("New: %v", err)
		}
		if !errors.As(err, &cerr) || cerr.Code != CodeInvalidOptions {
			t.Fatalf("New must refuse with invalid_options: %v", err)
		}
		return outcomeLocallyRefused
	}
	if !call.Deadline.IsZero() {
		// the golden deadline lies in the past relative to the real clock; the replay clock sits before it
		clock = call.Deadline.Add(-time.Hour)
	}
	_, err = c.Call(context.Background(), operation, call)
	if v.Expect == "invalid" {
		if !errors.As(err, &cerr) {
			t.Fatalf("invalid request headers must be refused locally, got %v", err)
		}
		switch cerr.Code {
		case CodeInvalidIdempotencyKey, CodeInvalidClosureID, CodeInvalidIfMatch, CodeInvalidDeadline, CodeInvalidOptions:
		default:
			t.Fatalf("unexpected local code %s", cerr.Code)
		}
		return outcomeLocallyRefused
	}
	if err != nil {
		t.Fatalf("valid request headers: %v", err)
	}
	return outcomeAccepted
}

func assertEnvelopeProjection(t *testing.T, body map[string]any, apiErr *APIError) {
	t.Helper()
	if string(apiErr.Code) != body["code"].(string) || string(apiErr.RetryAction) != body["retryAction"].(string) || int64(apiErr.Status) != mustInt(t, body["status"]) {
		t.Fatalf("envelope projection: %+v vs %v", apiErr, body)
	}
	detail, hasDetail := body["detail"].(map[string]any)
	if apiErr.Detail.Present() != hasDetail {
		t.Fatalf("detail presence: %v vs %v", apiErr.Detail.Present(), hasDetail)
	}
	if !hasDetail {
		return
	}
	str := func(key string) string { s, _ := detail[key].(string); return s }
	if apiErr.Detail.DomainCode != str("domainCode") || apiErr.Detail.Reason != str("reason") || apiErr.Detail.Header != str("header") ||
		apiErr.Detail.Operation != str("operation") || apiErr.Detail.State != str("state") || apiErr.Detail.ClosureID != str("closureId") ||
		apiErr.Detail.Domain != str("domain") || apiErr.Detail.Family != str("family") || apiErr.Detail.DomainRetryAction != str("domainRetryAction") || apiErr.Detail.Fallback != str("fallback") {
		t.Fatalf("detail projection: %+v vs %v", apiErr.Detail, detail)
	}
	if n, ok := integerOf(detail["domainStatus"]); ok && int64(apiErr.Detail.DomainStatus) != n {
		t.Fatalf("domainStatus: %d vs %d", apiErr.Detail.DomainStatus, n)
	}
}

func assertMetaProjection(t *testing.T, headers map[string]string, meta Meta) {
	t.Helper()
	if meta.Contract != headers[HeaderContract] || strconv.Itoa(meta.ManifestRevision) != headers[HeaderManifestRevision] || meta.Domain != headers[HeaderDomain] || meta.SchemaHash != headers[HeaderSchemaHash] {
		t.Fatalf("meta projection: %+v vs %v", meta, headers)
	}
	if meta.ClosureID != headers[HeaderClosureID] || meta.TraceID != headers[HeaderTraceID] || meta.EventEnvelope != (headers[HeaderEventEnvelope] == Contract) {
		t.Fatalf("meta optional projection: %+v vs %v", meta, headers)
	}
	if ra, ok := headers[HeaderRetryAfter]; ok {
		seconds, _ := strconv.Atoi(ra)
		if !meta.HasRetryAfter || meta.RetryAfter != time.Duration(seconds)*time.Second {
			t.Fatalf("retry-after: %+v", meta)
		}
	} else if meta.HasRetryAfter {
		t.Fatalf("retry-after invented: %+v", meta)
	}
}

func mustInt(t *testing.T, v any) int64 {
	t.Helper()
	n, ok := integerOf(v)
	if !ok {
		t.Fatalf("not an integer: %v", v)
	}
	return n
}

// TestReplayCapabilityIntersection: the client's declared capability set is the intersection of the
// deployment capabilities (capabilities-offload-deployment: archive-sync and cache not installed) and
// the session closure (closure-partial-mixed: disabled / unavailable operations). Calling outside that
// set yields capability_unavailable with the golden reason - never a silent downgrade, never a legacy
// path - and the typed Detail carries operation / state / closureId for the caller to act on.
func TestReplayCapabilityIntersection(t *testing.T) {
	_, byName := loadGolden(t)
	caps := materialise(t, byName["capabilities-offload-deployment"], byName).(map[string]any)
	closure := materialise(t, byName["closure-partial-mixed"], byName).(map[string]any)
	notInstalled := materialise(t, byName["facade-error-capability-unavailable-not-installed"], byName).(map[string]any)
	disabled := materialise(t, byName["unified-error-outside-closure"], byName).(map[string]any)
	unavailable := materialise(t, byName["facade-error-capability-unavailable-outside-closure-unavailable"], byName).(map[string]any)
	view := materialise(t, byName["manifest-runtime-view"], byName).(map[string]any)
	revision, _ := integerOf(view["revision"])

	installed := map[string]bool{}
	for domain, raw := range caps["domains"].(map[string]any) {
		installed[domain], _ = raw.(map[string]any)["installed"].(bool)
	}
	states := map[string]string{}
	for name, raw := range closure["operations"].(map[string]any) {
		states[name], _ = raw.(string)
	}
	closureID := closure["closureId"].(string)

	var seen []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			t.Errorf("non-/api path %s", r.URL.Path)
			http.Error(w, "legacy", 404)
			return
		}
		op := operationForRequest(r)
		h := w.Header()
		h.Set(HeaderContract, Contract)
		h.Set(HeaderManifestRevision, strconv.FormatInt(revision, 10))
		h.Set(HeaderSchemaHash, "none")
		h.Set(HeaderDomain, "discovery")
		if op == nil {
			writeJSON(w, 404, materialise(t, byName["facade-error-not-found"], byName))
			return
		}
		h.Set(HeaderDomain, op.Domain)
		switch {
		case op.Domain != "discovery" && !installed[op.Domain]:
			writeJSON(w, 404, notInstalled)
		case states[op.Name] == "disabled":
			h.Set(HeaderClosureID, closureID)
			writeJSON(w, 403, disabled)
		case states[op.Name] == "unavailable":
			h.Set(HeaderClosureID, closureID)
			writeJSON(w, 404, unavailable)
		default:
			writeJSON(w, 200, map[string]any{"ok": true})
		}
	}))
	defer server.Close()
	c, err := New(Options{BaseURL: server.URL, Token: "t0ken"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var apiErr *APIError
	counts := map[string]int{}
	for _, op := range operations {
		if op.Kind == "stream" || op.Domain == "discovery" {
			continue
		}
		params := map[string]string{}
		for _, p := range op.Params {
			params[p] = "x-1"
		}
		var body any
		if op.Kind == "write" {
			body = map[string]any{"replay": true}
		}
		_, err := c.Call(ctx, op.Name, CallOptions{Params: params, Body: body})
		switch {
		case !installed[op.Domain]:
			if !errors.As(err, &apiErr) || apiErr.Code != CodeCapabilityUnavailable || apiErr.Status != 404 || apiErr.Detail.Reason != ReasonNotInstalled {
				t.Fatalf("%s (domain %s not installed): %v", op.Name, op.Domain, err)
			}
			counts["not_installed"]++
		case states[op.Name] == "disabled":
			if !errors.As(err, &apiErr) || apiErr.Code != CodeCapabilityUnavailable || apiErr.Status != 403 || apiErr.Detail.Reason != ReasonOutsideClosure || apiErr.Detail.State != "disabled" || apiErr.Detail.Operation == "" || apiErr.Meta.ClosureID != closureID {
				t.Fatalf("%s (disabled): %v", op.Name, err)
			}
			counts["disabled"]++
		case states[op.Name] == "unavailable":
			if !errors.As(err, &apiErr) || apiErr.Code != CodeCapabilityUnavailable || apiErr.Status != 404 || apiErr.Detail.Reason != ReasonOutsideClosure || apiErr.Detail.State != "unavailable" {
				t.Fatalf("%s (unavailable): %v", op.Name, err)
			}
			counts["unavailable"]++
		default:
			if err != nil {
				t.Fatalf("%s (enabled): %v", op.Name, err)
			}
			counts["enabled"]++
		}
		if errors.As(err, &apiErr) {
			if adv := Advice(apiErr); adv.Action != ActionNone || adv.Replayable {
				t.Fatalf("%s: capability_unavailable is never retried: %+v", op.Name, adv)
			}
		}
	}
	// every non-stream closure operation was exercised; the fence held along all three axes
	if counts["enabled"]+counts["disabled"]+counts["unavailable"]+counts["not_installed"] == 0 || counts["not_installed"] == 0 || counts["disabled"] == 0 || counts["unavailable"] == 0 || counts["enabled"] == 0 {
		t.Fatalf("intersection not exercised: %v", counts)
	}
	for _, line := range seen {
		if !strings.HasPrefix(line, "GET /api/") && !strings.HasPrefix(line, "POST /api/") && !strings.HasPrefix(line, "PUT /api/") && !strings.HasPrefix(line, "DELETE /api/") && !strings.HasPrefix(line, "PATCH /api/") {
			t.Fatalf("unexpected request %s", line)
		}
	}
	t.Logf("intersection readings %v over %d requests", counts, len(seen))
}

// operationForRequest finds the operation whose template (or alias) matches the request path.
func operationForRequest(r *http.Request) *Operation {
	segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	match := func(template string) bool {
		parts := strings.Split(strings.Trim(template, "/"), "/")
		if len(parts) != len(segments) {
			return false
		}
		for i, p := range parts {
			if !strings.HasPrefix(p, ":") && p != segments[i] {
				return false
			}
		}
		return true
	}
	for i := range operations {
		op := &operations[i]
		if op.Method != r.Method {
			continue
		}
		if match(op.Path) {
			return op
		}
		for _, alias := range op.Aliases {
			if match(alias) {
				return op
			}
		}
	}
	return nil
}
