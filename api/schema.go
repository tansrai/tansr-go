package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ValidationError reports the first violation of a unified-v1 schema definition found in a decoded
// JSON value. Path is an RFC 6901 pointer into the value.
type ValidationError struct {
	Definition string
	Path       string
	Message    string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s%s: %s", e.Definition, e.Path, e.Message)
}

// Definition names of doc/rfc/unified-v1.schema.json that this package validates.
const (
	DefManifest          = "Manifest"
	DefCapabilities      = "Capabilities"
	DefCapabilityClosure = "CapabilityClosure"
	DefFacadeError       = "FacadeError"
	DefUnifiedError      = "UnifiedError"
	DefEventEnvelope     = "EventEnvelope"
	DefRequestHeaders    = "RequestHeaders"
	DefResponseHeaders   = "ResponseHeaders"
)

// Validate checks a decoded JSON value (as produced by DecodeJSON) against a schema definition.
func Validate(definition string, value any) error {
	c := &checker{def: definition}
	var err *ValidationError
	switch definition {
	case DefManifest:
		err = c.manifest(value, "")
	case DefCapabilities:
		err = c.capabilities(value, "")
	case DefCapabilityClosure:
		err = c.closure(value, "")
	case DefFacadeError:
		err = c.facadeError(value, "")
	case DefUnifiedError:
		err = c.unifiedError(value, "")
	case DefEventEnvelope:
		err = c.eventEnvelope(value, "")
	case DefRequestHeaders:
		err = c.requestHeaders(value, "")
	case DefResponseHeaders:
		err = c.responseHeaders(value, "")
	default:
		return &ValidationError{Definition: definition, Message: "unknown definition"}
	}
	if err != nil {
		return err
	}
	return nil
}

// DecodeJSON decodes data into a generic tree (map[string]any, []any, json.Number, string, bool, nil)
// with numbers kept as json.Number so that integers are never rounded through float64. Trailing data
// is rejected. Like the unified Node client, UTF-8 is strict and decoded values
// are bounded to depth 64 and 200,000 nodes. Family control JSON uses canonical instead.
func DecodeJSON(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("JSON is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	nodes := 0
	var visit func(any, int) error
	visit = func(entry any, depth int) error {
		nodes++
		if depth > 64 || nodes > 200000 {
			return fmt.Errorf("JSON depth or node limit exceeded")
		}
		switch v := entry.(type) {
		case map[string]any:
			for _, child := range v {
				if err := visit(child, depth+1); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range v {
				if err := visit(child, depth+1); err != nil {
					return err
				}
			}
		case json.Number:
			n, err := v.Float64()
			if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
				return fmt.Errorf("JSON contains a non-finite number")
			}
		}
		return nil
	}
	if err := visit(value, 0); err != nil {
		return nil, err
	}
	return value, nil
}

type checker struct{ def string }

func (c *checker) fail(path, format string, args ...any) *ValidationError {
	return &ValidationError{Definition: c.def, Path: path, Message: fmt.Sprintf(format, args...)}
}

func child(path, key string) string {
	return path + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

func index(path string, i int) string { return path + "/" + strconv.Itoa(i) }

// --- primitive readers ---------------------------------------------------------------------------

func objectOf(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func stringOf(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func boolOf(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// integerOf accepts json.Number integers (no fraction / exponent) and Go integer kinds.
func integerOf(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		s := n.String()
		if strings.ContainsAny(s, ".eE") {
			return 0, false
		}
		i, err := strconv.ParseInt(s, 10, 64)
		return i, err == nil
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		if n != float64(int64(n)) {
			return 0, false
		}
		return int64(n), true
	}
	return 0, false
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// --- patterns (schema definitions) --------------------------------------------------------------

var (
	sequencePattern        = regexp.MustCompile(`^(0|[1-9][0-9]{0,18})$`)
	digestPattern          = hex64
	familyIDPattern        = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	repoPathPattern        = regexp.MustCompile(`^[^\s\\]+$`)
	sha256PrefixedPattern  = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	traceIDSchemaPattern   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	idempotencyKeyPattern  = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)
	domainCodePattern      = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	operationNamePattern   = regexp.MustCompile(`^[a-z]+(\.[a-z][a-z0-9_]*)+$`)
	apiPathPattern         = regexp.MustCompile(`^/api(/(:[A-Za-z]+|[A-Za-z0-9._~-]+))+$`)
	apiEntryPattern        = regexp.MustCompile(`^/api(/(:[A-Za-z]+|[A-Za-z0-9._~-]+|\*\*))*$`)
	legacyPathPattern      = regexp.MustCompile(`^/v[123](/(:[A-Za-z]+|[A-Za-z0-9._~-]+|\*\*))+$`)
	keyPathSegmentPattern  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	legacyOffloadPattern   = regexp.MustCompile(`^/v3/sdk2/`)
	schemaRefPattern       = regexp.MustCompile(`^[a-z0-9-]+#[A-Za-z][A-Za-z0-9]*$`)
	eventTypePattern       = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z][a-z0-9_-]*)*$`)
	capabilityIDPattern    = regexp.MustCompile(`^[a-z]+(\.[A-Za-z0-9][A-Za-z0-9-]*)+$`)
	capabilitySrcPattern   = regexp.MustCompile(`^[^#\s]+#[A-Za-z_][A-Za-z0-9_]*$`)
	repoNamePattern        = regexp.MustCompile(`^tansr-[a-z]+$`)
	observedPattern        = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	queryKeyPattern        = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	headerNamePattern      = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	traceparentPattern     = regexp.MustCompile(`^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)
	ifMatchPattern         = regexp.MustCompile(`^(W/)?"[\x21\x23-\x7e]*"$`)
	authorizationPattern   = regexp.MustCompile(`^Bearer [\x21-\x7e]+$`)
	positiveDecimalPattern = regexp.MustCompile(`^[1-9][0-9]*$`)
	entryPattern           = regexp.MustCompile(`^/`)
)

const maxSequence = "9223372036854775807"

func isSequence(s string) bool {
	if !sequencePattern.MatchString(s) {
		return false
	}
	return len(s) < len(maxSequence) || s <= maxSequence
}

// --- vocabularies ------------------------------------------------------------------------------

var (
	domainEnum        = []string{"discovery", "session", "execution", "archive", "archive-sync", "cache", "terminal", "terminal-observation", "terminal-profile"}
	closureDomainEnum = []string{"session", "execution", "archive", "archive-sync", "cache", "terminal", "terminal-observation", "terminal-profile"}
	familyStatusEnum  = []string{"frozen", "final", "additive", "candidate", "drafted"}
	domainStatusEnum  = []string{"frozen", "final", "additive", "candidate", "drafted", "unregistered"}
	sessionFamilyEnum = []string{"sdk1", "sdk2-offload-v1"}
	methodEnum        = []string{"GET", "POST", "DELETE"}
	kindEnum          = []string{"read", "write", "stream"}
	capabilityKinds   = []string{"limit", "feature", "capability"}
	clientEnum        = []string{"node", "csharp", "go", "android", "ios", "harmony"}
	operationStates   = []string{"enabled", "disabled", "unavailable"}
	terminalStatuses  = []string{"accepted", "completed", "aborted", "unknown"}
	fallbackEnum      = []string{"none", "legacy-cold"}
	// facadeReasonEnum is FacadeErrorDetail.reason (fence and assembly only).
	facadeReasonEnum = []string{"not_installed", "outside_closure"}
	// reasonEnum is UnifiedErrorDetail.reason: fence / assembly plus the 15 request-header reasons of the
	// /api three-header wiring (revision 7; RFC-UAPI-1 §1.2, plan D27: no new codes, reasons only).
	reasonEnum = []string{
		"not_installed", "outside_closure",
		"idempotency_key_invalid", "idempotency_key_not_applicable", "idempotency_key_reused", "idempotency_key_mismatch",
		"receipt_not_retained", "receipt_window_full",
		"if_match_invalid", "if_match_not_applicable", "if_match_body_mismatch", "if_match_stale",
		"deadline_invalid", "deadline_exceeded",
		"header_body_limit", "header_body_not_canonical", "header_processing_failed",
	}
	expectedRevisionKinds = []string{"sequence", "integer"}
	facadeHeaderEnum      = []string{"tansr-session-family", "tansr-closure-id", "tansr-event-envelope"}
	httpStatusEnum        = []int64{400, 401, 403, 404, 405, 408, 409, 410, 412, 413, 422, 429, 500, 503}
	facadeStatusEnum      = []int64{400, 401, 403, 404, 405, 412, 503}
)

// Domain contracts of the deployment-level capabilities view (route-table API_DOMAIN_FAMILY).
var capabilityDomainContracts = map[string]string{
	"session":              "agent-session-v1",
	"execution":            "sdk2-ext-v1",
	"archive":              "sdk2-ext-v1",
	"archive-sync":         "archive-sync-v1",
	"cache":                "sdk2-cache-v1",
	"terminal":             "terminal-services-v1",
	"terminal-observation": "terminal-observation-v1",
	"terminal-profile":     "terminal-profile-v1",
}

// unifiedStatusBindings: code → allowed HTTP statuses (schema UnifiedError allOf).
var unifiedStatusBindings = map[string][]int64{
	"invalid_request":        {400, 408, 422},
	"protocol_mismatch":      {400},
	"unauthorized":           {401},
	"forbidden":              {403},
	"not_found":              {404},
	"method_not_allowed":     {405},
	"gone":                   {410},
	"conflict":               {409, 412},
	"stale_generation":       {409},
	"gap":                    {409, 410},
	"capability_unavailable": {404, 403},
	"capacity_exceeded":      {429, 503},
	"payload_too_large":      {413},
	"upstream_unavailable":   {503},
	"result_unknown":         {503},
	"rejected":               {422},
	"internal_error":         {500},
	"precondition_failed":    {412},
	"not_canonical":          {400},
}

// unifiedRetryBindings: code → allowed retryActions where the schema restricts them.
var unifiedRetryBindings = map[string][]string{
	"result_unknown":      {"query-status", "rebind"},
	"precondition_failed": {"rediscover", "refresh"},
	"not_canonical":       {"none"},
}

// facadeBindings: code → (statuses, retryAction) (schema FacadeError allOf = facade.ts ERROR_STATUS / ERROR_RETRY).
var facadeBindings = map[string]struct {
	statuses []int64
	retry    string
}{
	"unauthorized":           {[]int64{401}, "none"},
	"not_found":              {[]int64{404}, "none"},
	"method_not_allowed":     {[]int64{405}, "none"},
	"invalid_request":        {[]int64{400}, "none"},
	"capability_unavailable": {[]int64{404, 403}, "none"},
	"capacity_exceeded":      {[]int64{503}, "same-request"},
	"precondition_failed":    {[]int64{412}, "rediscover"},
	"upstream_unavailable":   {[]int64{503}, "same-request"},
}

func containsInt(list []int64, v int64) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
}

// --- generic object helpers ---------------------------------------------------------------------

func (c *checker) object(v any, path string) (map[string]any, *ValidationError) {
	m, ok := objectOf(v)
	if !ok {
		return nil, c.fail(path, "must be an object")
	}
	return m, nil
}

func (c *checker) require(m map[string]any, path string, keys ...string) *ValidationError {
	for _, key := range keys {
		if _, ok := m[key]; !ok {
			return c.fail(path, "missing required key %q", key)
		}
	}
	return nil
}

func (c *checker) noExtra(m map[string]any, path string, allowed ...string) *ValidationError {
	for key := range m {
		if !contains(allowed, key) {
			return c.fail(child(path, key), "unexpected key")
		}
	}
	return nil
}

func (c *checker) constString(m map[string]any, path, key, want string) *ValidationError {
	s, ok := stringOf(m[key])
	if !ok || s != want {
		return c.fail(child(path, key), "must be %q", want)
	}
	return nil
}

// str validates a present string with optional pattern and rune length bounds (max 0 = unbounded).
func (c *checker) str(v any, path string, pattern *regexp.Regexp, min, max int) (string, *ValidationError) {
	s, ok := stringOf(v)
	if !ok {
		return "", c.fail(path, "must be a string")
	}
	if n := runeLen(s); n < min || (max > 0 && n > max) {
		return "", c.fail(path, "length must be within [%d, %d]", min, max)
	}
	if pattern != nil && !pattern.MatchString(s) {
		return "", c.fail(path, "does not match %s", pattern)
	}
	return s, nil
}

// strOrNull validates string|null.
func (c *checker) strOrNull(v any, path string, pattern *regexp.Regexp, min, max int) *ValidationError {
	if v == nil {
		return nil
	}
	_, err := c.str(v, path, pattern, min, max)
	return err
}

func (c *checker) enum(v any, path string, values []string) (string, *ValidationError) {
	s, ok := stringOf(v)
	if !ok || !contains(values, s) {
		return "", c.fail(path, "must be one of %v", values)
	}
	return s, nil
}

func (c *checker) integer(v any, path string, min, max int64) (int64, *ValidationError) {
	n, ok := integerOf(v)
	if !ok {
		return 0, c.fail(path, "must be an integer")
	}
	if n < min || n > max {
		return 0, c.fail(path, "must be within [%d, %d]", min, max)
	}
	return n, nil
}

func (c *checker) boolean(v any, path string) *ValidationError {
	if _, ok := boolOf(v); !ok {
		return c.fail(path, "must be a boolean")
	}
	return nil
}

func (c *checker) array(v any, path string, minItems int) ([]any, *ValidationError) {
	list, ok := v.([]any)
	if !ok {
		return nil, c.fail(path, "must be an array")
	}
	if len(list) < minItems {
		return nil, c.fail(path, "must have at least %d items", minItems)
	}
	return list, nil
}

// stringArray validates an array of strings, each matching pattern and bounds, optionally unique.
func (c *checker) stringArray(v any, path string, pattern *regexp.Regexp, min, max, minItems int, unique bool) ([]string, *ValidationError) {
	list, err := c.array(v, path, minItems)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list))
	seen := map[string]bool{}
	for i, item := range list {
		s, err := c.str(item, index(path, i), pattern, min, max)
		if err != nil {
			return nil, err
		}
		if unique && seen[s] {
			return nil, c.fail(index(path, i), "duplicate item")
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, nil
}

// keyPath validates schema KeyPath: 1–8 object keys from the root, each `^[A-Za-z][A-Za-z0-9]*$` (≤ 64).
func (c *checker) keyPath(v any, path string) *ValidationError {
	list, err := c.array(v, path, 1)
	if err != nil {
		return err
	}
	if len(list) > 8 {
		return c.fail(path, "must have at most 8 items")
	}
	for i, item := range list {
		if _, err := c.str(item, index(path, i), keyPathSegmentPattern, 1, 64); err != nil {
			return err
		}
	}
	return nil
}

// keyPathOrNull validates KeyPath | null.
func (c *checker) keyPathOrNull(v any, path string) *ValidationError {
	if v == nil {
		return nil
	}
	return c.keyPath(v, path)
}

func (c *checker) enumArray(v any, path string, values []string, minItems, maxItems int, unique bool) *ValidationError {
	list, err := c.array(v, path, minItems)
	if err != nil {
		return err
	}
	if maxItems > 0 && len(list) > maxItems {
		return c.fail(path, "must have at most %d items", maxItems)
	}
	seen := map[string]bool{}
	for i, item := range list {
		s, err := c.enum(item, index(path, i), values)
		if err != nil {
			return err
		}
		if unique && seen[s] {
			return c.fail(index(path, i), "duplicate item")
		}
		seen[s] = true
	}
	return nil
}

// --- Manifest ----------------------------------------------------------------------------------

func (c *checker) manifest(v any, path string) *ValidationError {
	m, err := c.object(v, path)
	if err != nil {
		return err
	}
	if err := c.require(m, path, "format", "contract", "revision", "schemaHash", "families", "operations", "capabilities", "platformApi"); err != nil {
		return err
	}
	if err := c.noExtra(m, path, "format", "contract", "revision", "schemaHash", "families", "operations", "capabilities", "platformApi", "runtime"); err != nil {
		return err
	}
	if err := c.constString(m, path, "format", "tansr-api-manifest-v1"); err != nil {
		return err
	}
	if err := c.constString(m, path, "contract", Contract); err != nil {
		return err
	}
	if _, err := c.integer(m["revision"], child(path, "revision"), 1, 1<<53); err != nil {
		return err
	}
	if _, err := c.str(m["schemaHash"], child(path, "schemaHash"), digestPattern, 0, 0); err != nil {
		return err
	}
	families, err := c.array(m["families"], child(path, "families"), 1)
	if err != nil {
		return err
	}
	for i, fam := range families {
		if err := c.manifestFamily(fam, index(child(path, "families"), i)); err != nil {
			return err
		}
	}
	ops, err := c.array(m["operations"], child(path, "operations"), 1)
	if err != nil {
		return err
	}
	for i, op := range ops {
		if err := c.manifestOperation(op, index(child(path, "operations"), i)); err != nil {
			return err
		}
	}
	caps, err := c.array(m["capabilities"], child(path, "capabilities"), 0)
	if err != nil {
		return err
	}
	for i, cap := range caps {
		if err := c.manifestCapability(cap, index(child(path, "capabilities"), i)); err != nil {
			return err
		}
	}
	refs, err := c.array(m["platformApi"], child(path, "platformApi"), 0)
	if err != nil {
		return err
	}
	for i, ref := range refs {
		if err := c.platformAPIReference(ref, index(child(path, "platformApi"), i)); err != nil {
			return err
		}
	}
	if runtime, ok := m["runtime"]; ok {
		rpath := child(path, "runtime")
		rm, err := c.object(runtime, rpath)
		if err != nil {
			return err
		}
		if err := c.require(rm, rpath, "installed"); err != nil {
			return err
		}
		if err := c.noExtra(rm, rpath, "installed"); err != nil {
			return err
		}
		if err := c.installedDomains(rm["installed"], child(rpath, "installed")); err != nil {
			return err
		}
	}
	return nil
}

func (c *checker) manifestFamily(v any, path string) *ValidationError {
	m, err := c.object(v, path)
	if err != nil {
		return err
	}
	required := []string{"id", "domains", "status", "rfc", "source", "sha256", "golden", "generated", "legacyEntries", "apiEntries", "clients", "requestIdPath"}
	if err := c.require(m, path, required...); err != nil {
		return err
	}
	if err := c.noExtra(m, path, append(required, "sessionManifestRevision", "sessionManifestSha256")...); err != nil {
		return err
	}
	if err := c.keyPathOrNull(m["requestIdPath"], child(path, "requestIdPath")); err != nil {
		return err
	}
	if _, err := c.str(m["id"], child(path, "id"), familyIDPattern, 1, 64); err != nil {
		return err
	}
	if err := c.enumArray(m["domains"], child(path, "domains"), domainEnum, 1, 0, true); err != nil {
		return err
	}
	if _, err := c.enum(m["status"], child(path, "status"), familyStatusEnum); err != nil {
		return err
	}
	for _, key := range []string{"rfc", "source"} {
		if _, err := c.str(m[key], child(path, key), repoPathPattern, 1, 256); err != nil {
			return err
		}
	}
	if _, err := c.str(m["sha256"], child(path, "sha256"), digestPattern, 0, 0); err != nil {
		return err
	}
	golden, err := c.object(m["golden"], child(path, "golden"))
	if err != nil {
		return err
	}
	for key, value := range golden {
		if !repoPathPattern.MatchString(key) || runeLen(key) > 256 {
			return c.fail(child(child(path, "golden"), key), "property name must be a repo path")
		}
		if _, err := c.str(value, child(child(path, "golden"), key), digestPattern, 0, 0); err != nil {
			return err
		}
	}
	if _, err := c.stringArray(m["generated"], child(path, "generated"), repoPathPattern, 1, 256, 0, true); err != nil {
		return err
	}
	if _, err := c.stringArray(m["legacyEntries"], child(path, "legacyEntries"), legacyPathPattern, 0, 256, 0, true); err != nil {
		return err
	}
	if _, err := c.stringArray(m["apiEntries"], child(path, "apiEntries"), apiEntryPattern, 0, 256, 0, true); err != nil {
		return err
	}
	clients, err := c.array(m["clients"], child(path, "clients"), 0)
	if err != nil {
		return err
	}
	for i, lock := range clients {
		if err := c.clientLock(lock, index(child(path, "clients"), i)); err != nil {
			return err
		}
	}
	if rev, ok := m["sessionManifestRevision"]; ok {
		if _, err := c.integer(rev, child(path, "sessionManifestRevision"), 1, 1<<53); err != nil {
			return err
		}
	}
	if sha, ok := m["sessionManifestSha256"]; ok {
		if _, err := c.str(sha, child(path, "sessionManifestSha256"), digestPattern, 0, 0); err != nil {
			return err
		}
	}
	return nil
}

func (c *checker) clientLock(v any, path string) *ValidationError {
	m, err := c.object(v, path)
	if err != nil {
		return err
	}
	if err := c.require(m, path, "client", "repo", "lock", "observed", "drift"); err != nil {
		return err
	}
	if err := c.noExtra(m, path, "client", "repo", "file", "lock", "observed", "drift"); err != nil {
		return err
	}
	if _, err := c.enum(m["client"], child(path, "client"), clientEnum); err != nil {
		return err
	}
	if _, err := c.str(m["repo"], child(path, "repo"), repoNamePattern, 0, 64); err != nil {
		return err
	}
	if file, ok := m["file"]; ok {
		if _, err := c.str(file, child(path, "file"), repoPathPattern, 1, 256); err != nil {
			return err
		}
	}
	if _, err := c.str(m["lock"], child(path, "lock"), nil, 1, 128); err != nil {
		return err
	}
	if _, err := c.str(m["observed"], child(path, "observed"), observedPattern, 0, 0); err != nil {
		return err
	}
	return c.boolean(m["drift"], child(path, "drift"))
}

func (c *checker) manifestOperation(v any, path string) *ValidationError {
	m, err := c.object(v, path)
	if err != nil {
		return err
	}
	required := []string{"name", "domain", "family", "method", "apiPath", "aliases", "legacyPath", "legacyOffloadPath", "kind", "sse", "request", "response", "query", "notes", "etagPath", "expectedRevision"}
	if err := c.require(m, path, required...); err != nil {
		return err
	}
	if err := c.noExtra(m, path, required...); err != nil {
		return err
	}
	if _, err := c.str(m["name"], child(path, "name"), operationNamePattern, 3, 128); err != nil {
		return err
	}
	domain, err := c.enum(m["domain"], child(path, "domain"), domainEnum)
	if err != nil {
		return err
	}
	if err := c.strOrNull(m["family"], child(path, "family"), familyIDPattern, 1, 64); err != nil {
		return err
	}
	method, err := c.enum(m["method"], child(path, "method"), methodEnum)
	if err != nil {
		return err
	}
	if _, err := c.str(m["apiPath"], child(path, "apiPath"), apiPathPattern, 0, 256); err != nil {
		return err
	}
	if _, err := c.stringArray(m["aliases"], child(path, "aliases"), apiPathPattern, 0, 256, 0, true); err != nil {
		return err
	}
	if err := c.strOrNull(m["legacyPath"], child(path, "legacyPath"), legacyPathPattern, 0, 256); err != nil {
		return err
	}
	if err := c.strOrNull(m["legacyOffloadPath"], child(path, "legacyOffloadPath"), legacyOffloadPattern, 0, 256); err != nil {
		return err
	}
	kind, err := c.enum(m["kind"], child(path, "kind"), kindEnum)
	if err != nil {
		return err
	}
	sse, ok := boolOf(m["sse"])
	if !ok {
		return c.fail(child(path, "sse"), "must be a boolean")
	}
	for _, key := range []string{"request", "response"} {
		if err := c.strOrNull(m[key], child(path, key), schemaRefPattern, 0, 192); err != nil {
			return err
		}
	}
	if _, err := c.stringArray(m["query"], child(path, "query"), queryKeyPattern, 0, 64, 0, true); err != nil {
		return err
	}
	if err := c.strOrNull(m["notes"], child(path, "notes"), nil, 1, 2048); err != nil {
		return err
	}
	// revision 7 three-header facts: etagPath KeyPath | null; expectedRevision {path, kind} | null, and
	// null whenever the operation is not a write (If-Match applies to writes only).
	if err := c.keyPathOrNull(m["etagPath"], child(path, "etagPath")); err != nil {
		return err
	}
	if exp := m["expectedRevision"]; exp != nil {
		epath := child(path, "expectedRevision")
		em, err := c.object(exp, epath)
		if err != nil {
			return err
		}
		if err := c.require(em, epath, "path", "kind"); err != nil {
			return err
		}
		if err := c.noExtra(em, epath, "path", "kind"); err != nil {
			return err
		}
		if err := c.keyPath(em["path"], child(epath, "path")); err != nil {
			return err
		}
		if _, err := c.enum(em["kind"], child(epath, "kind"), expectedRevisionKinds); err != nil {
			return err
		}
		if kind != "write" {
			return c.fail(epath, "must be null for %s operations (If-Match applies to writes only)", kind)
		}
	}
	if domain == "discovery" {
		for _, key := range []string{"family", "legacyPath", "legacyOffloadPath"} {
			if m[key] != nil {
				return c.fail(child(path, key), "must be null for discovery operations")
			}
		}
	} else {
		if m["family"] == nil {
			return c.fail(child(path, "family"), "must be a family id for domain operations")
		}
		if m["legacyPath"] == nil {
			return c.fail(child(path, "legacyPath"), "must be a legacy path template for domain operations")
		}
	}
	if sse && kind != "stream" {
		return c.fail(child(path, "kind"), "sse operations must have kind stream")
	}
	if !sse && kind == "stream" {
		return c.fail(child(path, "kind"), "kind stream requires sse true")
	}
	if method == "GET" && kind == "write" {
		return c.fail(child(path, "kind"), "GET operations must be read or stream")
	}
	if method != "GET" && kind != "write" {
		return c.fail(child(path, "kind"), "%s operations must be write", method)
	}
	return nil
}

func (c *checker) manifestCapability(v any, path string) *ValidationError {
	m, err := c.object(v, path)
	if err != nil {
		return err
	}
	required := []string{"id", "domain", "family", "kind", "source", "value"}
	if err := c.require(m, path, required...); err != nil {
		return err
	}
	if err := c.noExtra(m, path, required...); err != nil {
		return err
	}
	if _, err := c.str(m["id"], child(path, "id"), capabilityIDPattern, 3, 128); err != nil {
		return err
	}
	if _, err := c.enum(m["domain"], child(path, "domain"), closureDomainEnum); err != nil {
		return err
	}
	if _, err := c.str(m["family"], child(path, "family"), familyIDPattern, 1, 64); err != nil {
		return err
	}
	kind, err := c.enum(m["kind"], child(path, "kind"), capabilityKinds)
	if err != nil {
		return err
	}
	if _, err := c.str(m["source"], child(path, "source"), capabilitySrcPattern, 0, 256); err != nil {
		return err
	}
	vpath := child(path, "value")
	if kind == "limit" {
		lm, err := c.object(m["value"], vpath)
		if err != nil {
			return err
		}
		if err := c.require(lm, vpath, "min", "max", "default"); err != nil {
			return err
		}
		if err := c.noExtra(lm, vpath, "min", "max", "default"); err != nil {
			return err
		}
		for _, key := range []string{"min", "max"} {
			if _, err := c.integer(lm[key], child(vpath, key), 0, 1<<53); err != nil {
				return err
			}
		}
		if lm["default"] != nil {
			if _, err := c.integer(lm["default"], child(vpath, "default"), 0, 1<<53); err != nil {
				return err
			}
		}
		return nil
	}
	_, err = c.str(m["value"], vpath, nil, 1, 256)
	return err
}

func (c *checker) platformAPIReference(v any, path string) *ValidationError {
	m, err := c.object(v, path)
	if err != nil {
		return err
	}
	required := []string{"id", "doc", "host", "entries", "vendored"}
	if err := c.require(m, path, required...); err != nil {
		return err
	}
	if err := c.noExtra(m, path, required...); err != nil {
		return err
	}
	if _, err := c.str(m["id"], child(path, "id"), familyIDPattern, 1, 64); err != nil {
		return err
	}
	if _, err := c.str(m["doc"], child(path, "doc"), repoPathPattern, 1, 256); err != nil {
		return err
	}
	if _, err := c.str(m["host"], child(path, "host"), repoNamePattern, 0, 64); err != nil {
		return err
	}
	if _, err := c.stringArray(m["entries"], child(path, "entries"), entryPattern, 0, 256, 1, true); err != nil {
		return err
	}
	return c.strOrNull(m["vendored"], child(path, "vendored"), repoPathPattern, 1, 256)
}

func (c *checker) installedDomains(v any, path string) *ValidationError {
	m, err := c.object(v, path)
	if err != nil {
		return err
	}
	if err := c.require(m, path, closureDomainEnum...); err != nil {
		return err
	}
	if err := c.noExtra(m, path, closureDomainEnum...); err != nil {
		return err
	}
	if m["session"] != nil {
		if _, err := c.enum(m["session"], child(path, "session"), sessionFamilyEnum); err != nil {
			return err
		}
	}
	for _, key := range closureDomainEnum[1:] {
		if err := c.boolean(m[key], child(path, key)); err != nil {
			return err
		}
	}
	return nil
}

// --- Capabilities ------------------------------------------------------------------------------

func (c *checker) capabilities(v any, path string) *ValidationError {
	m, err := c.object(v, path)
	if err != nil {
		return err
	}
	required := []string{"contract", "manifestRevision", "schemaHash", "domains"}
	if err := c.require(m, path, required...); err != nil {
		return err
	}
	if err := c.noExtra(m, path, required...); err != nil {
		return err
	}
	if err := c.constString(m, path, "contract", Contract); err != nil {
		return err
	}
	if _, err := c.integer(m["manifestRevision"], child(path, "manifestRevision"), 1, 1<<53); err != nil {
		return err
	}
	if _, err := c.str(m["schemaHash"], child(path, "schemaHash"), sha256PrefixedPattern, 0, 0); err != nil {
		return err
	}
	dpath := child(path, "domains")
	domains, err := c.object(m["domains"], dpath)
	if err != nil {
		return err
	}
	if err := c.require(domains, dpath, closureDomainEnum...); err != nil {
		return err
	}
	if err := c.noExtra(domains, dpath, closureDomainEnum...); err != nil {
		return err
	}
	for _, domain := range closureDomainEnum {
		vpath := child(dpath, domain)
		view, err := c.object(domains[domain], vpath)
		if err != nil {
			return err
		}
		allowed := []string{"installed", "contract", "status", "schemaHash"}
		if domain == "session" {
			allowed = append(allowed, "family")
		}
		if err := c.require(view, vpath, allowed...); err != nil {
			return err
		}
		if err := c.noExtra(view, vpath, allowed...); err != nil {
			return err
		}
		if err := c.boolean(view["installed"], child(vpath, "installed")); err != nil {
			return err
		}
		if err := c.constString(view, vpath, "contract", capabilityDomainContracts[domain]); err != nil {
			return err
		}
		if _, err := c.enum(view["status"], child(vpath, "status"), domainStatusEnum); err != nil {
			return err
		}
		if _, err := c.str(view["schemaHash"], child(vpath, "schemaHash"), schemaHashPattern, 0, 0); err != nil {
			return err
		}
		if domain == "session" {
			fpath := child(vpath, "family")
			family, err := c.object(view["family"], fpath)
			if err != nil {
				return err
			}
			if err := c.require(family, fpath, "preferred", "available"); err != nil {
				return err
			}
			if err := c.noExtra(family, fpath, "preferred", "available"); err != nil {
				return err
			}
			if family["preferred"] != nil {
				if _, err := c.enum(family["preferred"], child(fpath, "preferred"), sessionFamilyEnum); err != nil {
					return err
				}
			}
			if err := c.enumArray(family["available"], child(fpath, "available"), sessionFamilyEnum, 0, 2, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- CapabilityClosure -------------------------------------------------------------------------

func (c *checker) closure(v any, path string) *ValidationError {
	m, err := c.object(v, path)
	if err != nil {
		return err
	}
	required := []string{"contract", "closureId", "authorizationRevision", "domains", "operations"}
	if err := c.require(m, path, required...); err != nil {
		return err
	}
	if err := c.noExtra(m, path, required...); err != nil {
		return err
	}
	if err := c.constString(m, path, "contract", Contract); err != nil {
		return err
	}
	if _, err := c.str(m["closureId"], child(path, "closureId"), digestPattern, 0, 0); err != nil {
		return err
	}
	if rev := m["authorizationRevision"]; rev != nil {
		s, ok := stringOf(rev)
		if !ok || !isSequence(s) {
			return c.fail(child(path, "authorizationRevision"), "must be a Sequence string or null")
		}
	}
	dpath := child(path, "domains")
	domains, err := c.object(m["domains"], dpath)
	if err != nil {
		return err
	}
	if err := c.require(domains, dpath, closureDomainEnum...); err != nil {
		return err
	}
	if err := c.noExtra(domains, dpath, closureDomainEnum...); err != nil {
		return err
	}
	for _, domain := range closureDomainEnum {
		spath := child(dpath, domain)
		state, err := c.object(domains[domain], spath)
		if err != nil {
			return err
		}
		if err := c.require(state, spath, "installed", "revision"); err != nil {
			return err
		}
		if err := c.noExtra(state, spath, "installed", "revision"); err != nil {
			return err
		}
		if err := c.boolean(state["installed"], child(spath, "installed")); err != nil {
			return err
		}
		if err := c.strOrNull(state["revision"], child(spath, "revision"), nil, 1, 512); err != nil {
			return err
		}
	}
	opath := child(path, "operations")
	ops, err := c.object(m["operations"], opath)
	if err != nil {
		return err
	}
	names := ClosureOperationNames()
	if err := c.require(ops, opath, names...); err != nil {
		return err
	}
	if err := c.noExtra(ops, opath, names...); err != nil {
		return err
	}
	for _, name := range names {
		if _, err := c.enum(ops[name], child(opath, name), operationStates); err != nil {
			return err
		}
	}
	return nil
}

// --- error envelopes -----------------------------------------------------------------------------

func (c *checker) facadeError(v any, path string) *ValidationError {
	m, err := c.object(v, path)
	if err != nil {
		return err
	}
	required := []string{"contract", "traceId", "requestId", "code", "status", "retryAction", "message"}
	if err := c.require(m, path, required...); err != nil {
		return err
	}
	if err := c.noExtra(m, path, append(required, "detail")...); err != nil {
		return err
	}
	if err := c.constString(m, path, "contract", Contract); err != nil {
		return err
	}
	if _, err := c.str(m["traceId"], child(path, "traceId"), traceIDSchemaPattern, 0, 0); err != nil {
		return err
	}
	if m["requestId"] != nil {
		return c.fail(child(path, "requestId"), "must be null for facade errors")
	}
	code, err := c.enum(m["code"], child(path, "code"), FacadeErrorCodes)
	if err != nil {
		return err
	}
	status, err := c.integer(m["status"], child(path, "status"), 100, 599)
	if err != nil {
		return err
	}
	if !containsInt(facadeStatusEnum, status) {
		return c.fail(child(path, "status"), "must be one of %v", facadeStatusEnum)
	}
	retry, err := c.enum(m["retryAction"], child(path, "retryAction"), RetryActions)
	if err != nil {
		return err
	}
	if _, err := c.str(m["message"], child(path, "message"), nil, 1, 1024); err != nil {
		return err
	}
	binding := facadeBindings[code]
	if !containsInt(binding.statuses, status) {
		return c.fail(child(path, "status"), "%s must use status %v", code, binding.statuses)
	}
	if retry != binding.retry {
		return c.fail(child(path, "retryAction"), "%s must use retryAction %q", code, binding.retry)
	}
	var detail map[string]any
	dpath := child(path, "detail")
	if raw, ok := m["detail"]; ok {
		detail, err = c.object(raw, dpath)
		if err != nil {
			return err
		}
		if reason, ok := detail["reason"]; ok {
			if _, err := c.enum(reason, child(dpath, "reason"), facadeReasonEnum); err != nil {
				return err
			}
		}
		if header, ok := detail["header"]; ok {
			if _, err := c.enum(header, child(dpath, "header"), facadeHeaderEnum); err != nil {
				return err
			}
		}
		if dc, ok := detail["domainCode"]; ok {
			if _, err := c.enum(dc, child(dpath, "domainCode"), []string{"closure_stale"}); err != nil {
				return err
			}
		}
		if cid, ok := detail["closureId"]; ok {
			if _, err := c.str(cid, child(dpath, "closureId"), digestPattern, 0, 0); err != nil {
				return err
			}
		}
		if op, ok := detail["operation"]; ok {
			if _, err := c.enum(op, child(dpath, "operation"), ClosureOperationNames()); err != nil {
				return err
			}
		}
		if state, ok := detail["state"]; ok {
			if _, err := c.enum(state, child(dpath, "state"), []string{"disabled", "unavailable"}); err != nil {
				return err
			}
		}
	}
	if code == "precondition_failed" {
		if detail == nil {
			return c.fail(path, "precondition_failed requires detail")
		}
		if err := c.require(detail, dpath, "domainCode", "closureId"); err != nil {
			return err
		}
	}
	if status == 403 {
		if detail == nil {
			return c.fail(path, "status 403 requires detail")
		}
		if err := c.require(detail, dpath, "reason", "operation", "state", "closureId"); err != nil {
			return err
		}
		if detail["reason"] != "outside_closure" || detail["state"] != "disabled" {
			return c.fail(dpath, "status 403 requires reason outside_closure and state disabled")
		}
	}
	if status == 404 && detail != nil && detail["reason"] == "outside_closure" {
		if err := c.require(detail, dpath, "operation", "state", "closureId"); err != nil {
			return err
		}
		if detail["state"] != "unavailable" {
			return c.fail(child(dpath, "state"), "status 404 outside_closure requires state unavailable")
		}
	}
	return nil
}

func (c *checker) unifiedError(v any, path string) *ValidationError {
	m, err := c.object(v, path)
	if err != nil {
		return err
	}
	required := []string{"contract", "traceId", "requestId", "code", "status", "retryAction", "message"}
	if err := c.require(m, path, required...); err != nil {
		return err
	}
	if err := c.noExtra(m, path, append(required, "retryAfterMs", "detail")...); err != nil {
		return err
	}
	if err := c.constString(m, path, "contract", Contract); err != nil {
		return err
	}
	if _, err := c.str(m["traceId"], child(path, "traceId"), traceIDSchemaPattern, 0, 0); err != nil {
		return err
	}
	if err := c.strOrNull(m["requestId"], child(path, "requestId"), idempotencyKeyPattern, 0, 0); err != nil {
		return err
	}
	code, err := c.enum(m["code"], child(path, "code"), ErrorCodes)
	if err != nil {
		return err
	}
	status, err := c.integer(m["status"], child(path, "status"), 100, 599)
	if err != nil {
		return err
	}
	if !containsInt(httpStatusEnum, status) {
		return c.fail(child(path, "status"), "must be one of %v", httpStatusEnum)
	}
	retry, err := c.enum(m["retryAction"], child(path, "retryAction"), RetryActions)
	if err != nil {
		return err
	}
	if ms, ok := m["retryAfterMs"]; ok {
		if _, err := c.integer(ms, child(path, "retryAfterMs"), 1, 1<<53); err != nil {
			return err
		}
	}
	if _, err := c.str(m["message"], child(path, "message"), nil, 1, 1024); err != nil {
		return err
	}
	if allowed := unifiedStatusBindings[code]; !containsInt(allowed, status) {
		return c.fail(child(path, "status"), "%s must use status %v", code, allowed)
	}
	if allowed, ok := unifiedRetryBindings[code]; ok && !contains(allowed, retry) {
		return c.fail(child(path, "retryAction"), "%s must use retryAction %v", code, allowed)
	}
	if raw, ok := m["detail"]; ok {
		dpath := child(path, "detail")
		detail, err := c.object(raw, dpath)
		if err != nil {
			return err
		}
		if d, ok := detail["domain"]; ok {
			if _, err := c.enum(d, child(dpath, "domain"), domainEnum); err != nil {
				return err
			}
		}
		if f, ok := detail["family"]; ok {
			if _, err := c.str(f, child(dpath, "family"), familyIDPattern, 1, 64); err != nil {
				return err
			}
		}
		if dc, ok := detail["domainCode"]; ok {
			if _, err := c.str(dc, child(dpath, "domainCode"), domainCodePattern, 1, 64); err != nil {
				return err
			}
		}
		if ds, ok := detail["domainStatus"]; ok {
			if _, err := c.integer(ds, child(dpath, "domainStatus"), 100, 599); err != nil {
				return err
			}
		}
		// domainRetryAction is DomainRetryAction | null: families without an action position (agent-session-v1)
		// are wrapped with an explicit null (error-envelope.ts; golden unified-error-v2-session-not-found-wire).
		if dr, ok := detail["domainRetryAction"]; ok && dr != nil {
			if _, err := c.enum(dr, child(dpath, "domainRetryAction"), DomainRetryActions); err != nil {
				return err
			}
		}
		if fb, ok := detail["fallback"]; ok {
			if _, err := c.enum(fb, child(dpath, "fallback"), fallbackEnum); err != nil {
				return err
			}
		}
		if reason, ok := detail["reason"]; ok {
			if _, err := c.enum(reason, child(dpath, "reason"), reasonEnum); err != nil {
				return err
			}
		}
		if header, ok := detail["header"]; ok {
			if _, err := c.str(header, child(dpath, "header"), headerNamePattern, 0, 64); err != nil {
				return err
			}
		}
		if lb, ok := detail["limitBytes"]; ok {
			if _, err := c.integer(lb, child(dpath, "limitBytes"), 1, 1<<53); err != nil {
				return err
			}
		}
		if cid, ok := detail["closureId"]; ok {
			if _, err := c.str(cid, child(dpath, "closureId"), digestPattern, 0, 0); err != nil {
				return err
			}
		}
		if op, ok := detail["operation"]; ok {
			if _, err := c.enum(op, child(dpath, "operation"), ClosureOperationNames()); err != nil {
				return err
			}
		}
		if state, ok := detail["state"]; ok {
			if _, err := c.enum(state, child(dpath, "state"), operationStates); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- EventEnvelope -----------------------------------------------------------------------------

// envelopeKeys are the seven keys of the wire form (schema EventEnvelope; plan D18): every key is required
// (eventId / type / terminalStatus may be null) and nothing else is accepted — the retired seq / payload
// positions and any identity field make the frame invalid.
var (
	envelopeKeys  = []string{"contract", "eventId", "domain", "type", "cursorSet", "terminalStatus", "raw"}
	cursorSetKeys = []string{"eventCursor", "archiveCoverage", "outputWatermark", "materialConsumed", "ackReceipt"}
	coverageKeys  = []string{"fromSequence", "throughSequence", "headDigest"}
)

const maxCursorChars = 512

func (c *checker) eventEnvelope(v any, path string) *ValidationError {
	m, err := c.object(v, path)
	if err != nil {
		return err
	}
	if err := c.require(m, path, envelopeKeys...); err != nil {
		return err
	}
	if err := c.noExtra(m, path, envelopeKeys...); err != nil {
		return err
	}
	if err := c.constString(m, path, "contract", Contract); err != nil {
		return err
	}
	// eventId = this frame's SSE id: (schema Cursor | null); absent id: is an explicit null, never "".
	if err := c.cursorString(m["eventId"], child(path, "eventId")); err != nil {
		return err
	}
	if _, err := c.enum(m["domain"], child(path, "domain"), domainEnum); err != nil {
		return err
	}
	if err := c.strOrNull(m["type"], child(path, "type"), eventTypePattern, 1, 128); err != nil {
		return err
	}
	cpath := child(path, "cursorSet")
	cursors, err := c.object(m["cursorSet"], cpath)
	if err != nil {
		return err
	}
	if err := c.require(cursors, cpath, cursorSetKeys...); err != nil {
		return err
	}
	if err := c.noExtra(cursors, cpath, cursorSetKeys...); err != nil {
		return err
	}
	for _, key := range cursorSetKeys {
		kpath := child(cpath, key)
		if key == "archiveCoverage" {
			err = c.archiveCoverage(cursors[key], kpath)
		} else {
			err = c.cursorString(cursors[key], kpath)
		}
		if err != nil {
			return err
		}
	}
	if ts := m["terminalStatus"]; ts != nil {
		if _, err := c.enum(ts, child(path, "terminalStatus"), terminalStatuses); err != nil {
			return err
		}
	}
	// raw is the original event object verbatim (schema: type object); its members are never inspected.
	if _, ok := objectOf(m["raw"]); !ok {
		return c.fail(child(path, "raw"), "must be the original event object")
	}
	return nil
}

// cursorString accepts null or an opaque cursor string (schema Cursor: 1–512 characters; control
// characters are additionally refused, as in the Node client). Objects are not cursors here.
func (c *checker) cursorString(v any, path string) *ValidationError {
	switch cur := v.(type) {
	case nil:
		return nil
	case string:
		if n := runeLen(cur); n == 0 || n > maxCursorChars || hasControl(cur) {
			return c.fail(path, "cursor must be a non-empty string without control characters")
		}
		return nil
	}
	return c.fail(path, "cursor must be a string or null")
}

// archiveCoverage accepts null, an opaque cursor string or the Coverage object the wire carries for
// archive.status (schema ArchiveCoverage: fromSequence / throughSequence Sequence, headDigest Digest,
// no other members).
func (c *checker) archiveCoverage(v any, path string) *ValidationError {
	m, ok := objectOf(v)
	if !ok {
		return c.cursorString(v, path)
	}
	if err := c.require(m, path, coverageKeys...); err != nil {
		return err
	}
	if err := c.noExtra(m, path, coverageKeys...); err != nil {
		return err
	}
	for _, key := range coverageKeys[:2] {
		s, ok := stringOf(m[key])
		if !ok || !isSequence(s) {
			return c.fail(child(path, key), "must be a Sequence string")
		}
	}
	_, err := c.str(m["headDigest"], child(path, "headDigest"), digestPattern, 0, 0)
	return err
}

// --- header projections ------------------------------------------------------------------------

var requestHeaderKeys = []string{"authorization", "traceparent", HeaderTraceID, HeaderIdempotencyKey, "if-match", "deadline", HeaderSessionFamily, HeaderClosureID, HeaderEventEnvelope}

func (c *checker) requestHeaders(v any, path string) *ValidationError {
	m, err := c.object(v, path)
	if err != nil {
		return err
	}
	if err := c.noExtra(m, path, requestHeaderKeys...); err != nil {
		return err
	}
	for key, value := range m {
		kpath := child(path, key)
		switch key {
		case "authorization":
			_, err = c.str(value, kpath, authorizationPattern, 0, 8192)
		case "traceparent":
			_, err = c.str(value, kpath, traceparentPattern, 0, 0)
		case HeaderTraceID:
			_, err = c.str(value, kpath, traceIDSchemaPattern, 0, 0)
		case HeaderIdempotencyKey:
			_, err = c.str(value, kpath, idempotencyKeyPattern, 0, 0)
		case "if-match":
			_, err = c.str(value, kpath, ifMatchPattern, 0, 1024)
		case "deadline":
			s, serr := c.str(value, kpath, nil, 0, 0)
			if serr != nil {
				err = serr
			} else if _, perr := time.Parse(time.RFC3339, s); perr != nil {
				err = c.fail(kpath, "must be an RFC 3339 date-time")
			}
		case HeaderSessionFamily:
			_, err = c.enum(value, kpath, sessionFamilyEnum)
		case HeaderClosureID:
			_, err = c.str(value, kpath, digestPattern, 0, 0)
		case HeaderEventEnvelope:
			err = c.constString(m, path, key, Contract)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

var responseHeaderKeys = []string{HeaderContract, HeaderManifestRevision, HeaderDomain, HeaderSchemaHash, HeaderClosureID, HeaderEventEnvelope, HeaderTraceID, HeaderRetryAfter}

func (c *checker) responseHeaders(v any, path string) *ValidationError {
	m, err := c.object(v, path)
	if err != nil {
		return err
	}
	if err := c.require(m, path, HeaderContract, HeaderManifestRevision, HeaderDomain, HeaderSchemaHash); err != nil {
		return err
	}
	if err := c.noExtra(m, path, responseHeaderKeys...); err != nil {
		return err
	}
	for key, value := range m {
		kpath := child(path, key)
		switch key {
		case HeaderContract, HeaderEventEnvelope:
			err = c.constString(m, path, key, Contract)
		case HeaderManifestRevision, HeaderRetryAfter:
			_, err = c.str(value, kpath, positiveDecimalPattern, 0, 0)
		case HeaderDomain:
			_, err = c.enum(value, kpath, domainEnum)
		case HeaderSchemaHash:
			_, err = c.str(value, kpath, schemaHashPattern, 0, 0)
		case HeaderClosureID:
			_, err = c.str(value, kpath, digestPattern, 0, 0)
		case HeaderTraceID:
			_, err = c.str(value, kpath, traceIDSchemaPattern, 0, 0)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
