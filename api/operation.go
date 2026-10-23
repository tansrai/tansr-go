package api

import (
	"net/url"
	"strconv"
	"strings"
)

//go:generate go run ../internal/gen/manifest2go -manifest ../contract/api-manifest.json -out operations_gen.go

// Operation is one entry of the manifest operation catalogue (contract/api-manifest.json operations[]).
// The client instantiates Path locally from Params; it never reads a URL from a response.
type Operation struct {
	Name string
	// Method is GET, POST or DELETE.
	Method string
	// Path is the /api template with :placeholders.
	Path string
	// Params lists the placeholder names in template order.
	Params []string
	// Aliases are session-scoped alias templates of the same operation (documentation only; the
	// client always sends Path).
	Aliases []string
	// Domain is the accepting domain (= response header tansr-domain).
	Domain string
	// Family is the contract family; "" for facade-owned operations (manifest family null).
	Family string
	// Kind is read, write or stream.
	Kind string
	// SSE is true for text/event-stream operations (Kind == "stream").
	SSE bool
	// Query is the whitelist of allowed query keys, in output order.
	Query []string
	// Request / Response are the manifest schema references (<family>#<Definition>) or "".
	Request  string
	Response string
	// ETagPath is the key path of the resource revision inside 2xx JSON responses from which the
	// server derives the strong validator `ETag: "<revision>"` (manifest etagPath, revision 7); nil =
	// the response is not a versioned resource representation and no ETag is sent.
	ETagPath []string
	// ExpectedRevision is the If-Match mapping target of a write operation (manifest expectedRevision,
	// revision 7); nil = the operation does not accept If-Match (the server answers 400
	// if_match_not_applicable, the client refuses locally).
	ExpectedRevision *ExpectedRevision
}

// ExpectedRevision is the body position the server maps If-Match to: Path is the key path of
// expectedRevision, Kind is sequence (decimal string) or integer (non-negative safe integer).
type ExpectedRevision struct {
	Path []string
	Kind string
}

// IsFacade reports whether the operation is answered by the facade itself (no family).
func (op Operation) IsFacade() bool { return op.Family == "" }

// Versioned reports whether 2xx responses of the operation carry an ETag (ETagPath != nil).
func (op Operation) Versioned() bool { return op.ETagPath != nil }

// AcceptsIfMatch reports whether the operation accepts an If-Match precondition (ExpectedRevision != nil).
func (op Operation) AcceptsIfMatch() bool { return op.ExpectedRevision != nil }

// RequestIDPath returns the body key path of the client idempotency key for the operation's family
// (manifest families[].requestIdPath); nil for facade operations and families without a position.
func (op Operation) RequestIDPath() []string { return FamilyRequestIDPaths[op.Family] }

var operationIndex = func() map[string]*Operation {
	index := make(map[string]*Operation, len(operations))
	for i := range operations {
		index[operations[i].Name] = &operations[i]
	}
	return index
}()

// Lookup returns the operation by name.
func Lookup(name string) (*Operation, bool) {
	op, ok := operationIndex[name]
	return op, ok
}

// Operations returns the operation catalogue in manifest order.
func Operations() []Operation {
	out := make([]Operation, len(operations))
	copy(out, operations)
	return out
}

// ClosureOperationNames returns the 77 operation names that appear in a capability closure: the
// catalogue without the deployment-level discovery reads (domain discovery and session.capabilities).
func ClosureOperationNames() []string {
	var names []string
	for _, op := range operations {
		if op.Domain == "discovery" || op.Name == OpSessionCapabilities {
			continue
		}
		names = append(names, op.Name)
	}
	return names
}

// canonicalBodyFamilies lists the families whose request bodies must be strict canonical control JSON
// (RFC-UAPI-1 §4; the server compares bytes). agent-session-v1 and facade operations use plain JSON.
var canonicalBodyFamilies = map[string]bool{
	"sdk2-ext-v1":                    true,
	"sdk2-archive-recovery-v1":       true,
	"archive-sync-v1":                true,
	"sdk2-cache-v1":                  true,
	"sdk2-cache-core-v1":             true,
	"terminal-services-v1":           true,
	"terminal-observation-v1":        true,
	"terminal-profile-v1":            true,
	"persistence-custody-handoff-v1": true,
}

// UsesCanonicalBody reports whether request bodies of the operation are canonical-encoded.
func (op Operation) UsesCanonicalBody() bool { return canonicalBodyFamilies[op.Family] }

// queryRevisionDefaults are the sub-contract revision markers injected into the query string when the
// key is whitelisted and the caller did not give it (manual §16.7). session.capabilities is registered
// under the session family but speaks sdk2-ext-v1 on the wire, hence the agent-session-v1 row.
var queryRevisionDefaults = map[string]map[string]string{
	"protocol": {
		"agent-session-v1":         "sdk2-ext-v1",
		"sdk2-ext-v1":              "sdk2-ext-v1",
		"sdk2-archive-recovery-v1": "sdk2-ext-v1",
		"sdk2-cache-v1":            "sdk2-cache-v1",
		"sdk2-cache-core-v1":       "sdk2-cache-core-v1",
	},
	"contract": {
		"terminal-services-v1":           "terminal-services-v1",
		"terminal-observation-v1":        "terminal-observation-v1",
		"terminal-profile-v1":            "terminal-profile-v1",
		"persistence-custody-handoff-v1": "persistence-custody-handoff-v1",
	},
}

const maxPathSegment = 512

// InstantiatePath renders op.Path from params. Unknown, missing or empty parameters and illegal
// segments (longer than 512 bytes, ".", "..", containing "/", "\" or control characters) fail locally
// with CodeInvalidParams; each value is percent-encoded like encodeURIComponent.
func InstantiatePath(op *Operation, params map[string]string) (string, error) {
	for key := range params {
		if !contains(op.Params, key) {
			return "", newClientError(CodeInvalidParams, op.Name+": unknown path parameter "+strconvQuote(key))
		}
	}
	var b strings.Builder
	for i, segment := range strings.Split(op.Path, "/") {
		if i > 0 {
			b.WriteByte('/')
		}
		if !strings.HasPrefix(segment, ":") {
			b.WriteString(segment)
			continue
		}
		key := segment[1:]
		value, ok := params[key]
		if !ok || value == "" {
			return "", newClientError(CodeInvalidParams, op.Name+": missing path parameter "+strconvQuote(key))
		}
		if len(value) > maxPathSegment || value == "." || value == ".." || strings.ContainsAny(value, "/\\") || hasControl(value) {
			return "", newClientError(CodeInvalidParams, op.Name+": path parameter "+strconvQuote(key)+" contains an illegal segment")
		}
		b.WriteString(EncodeURIComponent(value))
	}
	return b.String(), nil
}

// BuildQuery renders the query string ("" or "?k=v&..."): only whitelisted keys (op.Query), in
// whitelist order; revision defaults are injected per family when the caller gave no explicit value.
func BuildQuery(op *Operation, query map[string]string) (string, error) {
	for key := range query {
		if !contains(op.Query, key) {
			return "", newClientError(CodeInvalidQuery, op.Name+": query key "+strconvQuote(key)+" is not allowed by the manifest")
		}
	}
	var parts []string
	for _, key := range op.Query {
		value, ok := query[key]
		if !ok {
			if op.Family == "" {
				continue
			}
			fallback, has := queryRevisionDefaults[key][op.Family]
			if !has {
				continue
			}
			value = fallback
		}
		parts = append(parts, url.QueryEscape(key)+"="+url.QueryEscape(value))
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "?" + strings.Join(parts, "&"), nil
}

// EncodeURIComponent percent-encodes s exactly like ECMAScript encodeURIComponent: everything except
// A-Z a-z 0-9 - _ . ! ~ * ' ( ) is encoded as UTF-8 %XX (uppercase hex).
func EncodeURIComponent(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '-' || c == '_' || c == '.' || c == '!' || c == '~' || c == '*' || c == '\'' || c == '(' || c == ')':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}

func strconvQuote(s string) string { return strconv.Quote(s) }

func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

func contains(list []string, item string) bool {
	for _, entry := range list {
		if entry == item {
			return true
		}
	}
	return false
}
