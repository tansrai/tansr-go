package session

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/tansrai/tansr-go/api"
)

const maxSafeInteger = int64(9007199254740991)

var requestIDRule = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Client reuses one transport. It does not retain a capability decision across
// requests or retain credentials; TokenFunc can renew same-principal tokens.
type Client struct{ transport *api.Client }

func New(transport *api.Client) (*Client, error) {
	if transport == nil || !transport.EventEnvelope() || transport.SessionFamily() == "" {
		return nil, &api.ClientError{Code: api.CodeInvalidOptions, Detail: "session requires an explicit session family and EventEnvelope=true"}
	}
	return &Client{transport: transport}, nil
}

// API returns the same low-level transport for additional manifest operations.
func (c *Client) API() *api.Client { return c.transport }

type Session struct {
	client  *Client
	created Created
}

func (s *Session) ID() string       { return s.created.SessionID }
func (s *Session) Created() Created { return s.created }

// Create first verifies the requested family through read-only discovery. A
// failure never triggers a legacy route or creates a replacement conversation.
func (c *Client) Create(ctx context.Context, options CreateOptions) (*Session, error) {
	if options.Resume != nil && options.Fork != nil {
		return nil, invalidBody("resume and fork are mutually exclusive")
	}
	if options.Resume != nil && options.Resume.SessionID == "" || options.Fork != nil && (options.Fork.SessionID == "" || options.Fork.CheckpointID == "") {
		return nil, invalidBody("empty session or checkpoint reference")
	}
	family := c.transport.SessionFamily()
	if family == "sdk2-offload-v1" {
		if options.Fork != nil || options.Resume == nil && !requestIDRule.MatchString(options.RequestID) {
			return nil, invalidBody("offload create requires a retained requestId and does not support fork")
		}
	} else if options.RequestID != "" {
		return nil, invalidBody("requestId belongs to offload creation; use IdempotencyKey for session writes")
	}
	if err := c.discoverFamily(ctx); err != nil {
		return nil, err
	}
	result, err := c.transport.Call(ctx, api.OpSessionCreate, api.CallOptions{Body: options, IdempotencyKey: options.IdempotencyKey, Deadline: options.Deadline})
	if err != nil {
		return nil, err
	}
	var wire struct {
		SessionID    string `json:"sessionId"`
		Resumed      *bool  `json:"resumed"`
		LastSeq      *int64 `json:"lastSeq"`
		Contract     string `json:"contract"`
		Availability string `json:"availability"`
	}
	if result.Status != 200 && result.Status != 201 {
		return nil, invalidResponse("creation must return 200 attach or 201 created")
	}
	if err := decode(result, 0, &wire); err != nil {
		return nil, err
	}
	if wire.SessionID == "" || wire.Resumed == nil || wire.LastSeq == nil || !safeCount(*wire.LastSeq) ||
		options.Resume != nil && wire.SessionID != options.Resume.SessionID ||
		family == "sdk2-offload-v1" && (wire.Contract != family || wire.Availability != "source-required" && wire.Availability != "source-managed") {
		return nil, invalidResponse("created session identity or family is invalid")
	}
	return &Session{client: c, created: Created{wire.SessionID, *wire.Resumed, *wire.LastSeq}}, nil
}

// Attach is a read-only reference to the specified session. Check Meta.Live
// before sending; a dormant session needs Resume, not implicit recreation.
func (c *Client) Attach(ctx context.Context, id string) (*Session, error) {
	s := &Session{client: c, created: Created{SessionID: id}}
	meta, err := s.Meta(ctx)
	if err != nil {
		return nil, err
	}
	s.created.LastSeq = meta.LastSeq
	return s, nil
}

func (c *Client) Resume(ctx context.Context, id string) (*Session, error) {
	return c.Create(ctx, CreateOptions{Resume: &ResumeReference{SessionID: id}})
}

func (c *Client) discoverFamily(ctx context.Context) error {
	res, err := c.transport.Call(ctx, api.OpSessionCapabilities, api.CallOptions{})
	if err != nil {
		return err
	}
	var body struct {
		Protocol  string `json:"protocol"`
		Contracts []struct {
			Contract     string `json:"contract"`
			Availability string `json:"availability"`
		} `json:"contracts"`
	}
	if err := decode(res, 200, &body); err != nil {
		return err
	}
	if body.Protocol != "sdk2-ext-v1" || body.Contracts == nil || len(body.Contracts) > 2 {
		return invalidResponse("session family discovery is malformed")
	}
	seen, supported := map[string]bool{}, false
	for _, entry := range body.Contracts {
		if seen[entry.Contract] || !(entry.Contract == "sdk1" && entry.Availability == "legacy-complete" || entry.Contract == "sdk2-offload-v1" && (entry.Availability == "source-required" || entry.Availability == "source-managed")) {
			return invalidResponse("session family discovery contains an invalid entry")
		}
		seen[entry.Contract] = true
		supported = supported || entry.Contract == c.transport.SessionFamily()
	}
	if !supported {
		return &CapabilityError{Operation: api.OpSessionCreate, State: api.StateUnavailable}
	}
	return nil
}

// CapabilityError is a local refusal after explicit discovery, not a fabricated
// server response. No write was attempted. Refresh discovery after configuration
// changes; do not silently switch families.
type CapabilityError struct{ Operation, State string }

func (e *CapabilityError) Error() string {
	return "session: operation " + e.Operation + " is " + e.State
}

func (s *Session) Capabilities(ctx context.Context) (api.CapabilityClosure, error) {
	res, err := s.client.transport.SessionCapabilities(ctx, s.ID())
	if err != nil {
		return api.CapabilityClosure{}, err
	}
	return res.Closure, nil
}

func (s *Session) Meta(ctx context.Context) (Meta, error) { return s.meta(ctx, nil) }

func (s *Session) ApplicationPromptMeta(ctx context.Context) (Meta, error) {
	return s.meta(ctx, map[string]string{"include": "applicationPrompt"})
}

func (s *Session) meta(ctx context.Context, query map[string]string) (Meta, error) {
	res, err := s.client.transport.Call(ctx, api.OpSessionGet, api.CallOptions{Params: s.params(), Query: query})
	if err != nil {
		return Meta{}, err
	}
	meta, err := readMeta(res.Body, s.client.transport.SessionFamily())
	if err != nil {
		return Meta{}, err
	}
	if res.Status != 200 || res.ContentType != "application/json" || meta.SessionID != s.ID() {
		return Meta{}, invalidResponse("session metadata identity is invalid")
	}
	return meta, nil
}

func (c *Client) List(ctx context.Context, offset, limit int) (List, error) {
	if offset < 0 || limit < 1 {
		return List{}, &api.ClientError{Code: api.CodeInvalidQuery, Detail: "offset must be nonnegative and limit positive"}
	}
	res, err := c.transport.Call(ctx, api.OpSessionList, api.CallOptions{Query: map[string]string{"offset": strconv.Itoa(offset), "limit": strconv.Itoa(limit)}})
	if err != nil {
		return List{}, err
	}
	var wire struct {
		Sessions []json.RawMessage `json:"sessions"`
		Total    *int64            `json:"total"`
	}
	if err := decode(res, 200, &wire); err != nil {
		return List{}, err
	}
	if wire.Sessions == nil || wire.Total == nil || !safeCount(*wire.Total) {
		return List{}, invalidResponse("session list is malformed")
	}
	out := List{Sessions: make([]Meta, 0, len(wire.Sessions)), Total: *wire.Total}
	for _, raw := range wire.Sessions {
		meta, err := readMeta(raw, c.transport.SessionFamily())
		if err != nil {
			return List{}, err
		}
		out.Sessions = append(out.Sessions, meta)
	}
	return out, nil
}

func (s *Session) Send(ctx context.Context, prompt string, opts WriteOptions) (Accepted, error) {
	if opts.RequestID != "" && !requestIDRule.MatchString(opts.RequestID) {
		return Accepted{}, invalidBody("invalid requestId")
	}
	if strings.TrimSpace(prompt) == "" {
		return Accepted{}, invalidBody("prompt must not be blank")
	}
	return s.accepted(ctx, api.OpSessionMessageSend, nil, messageBody("prompt", prompt, opts.RequestID), opts, true)
}

func (s *Session) SendBlocks(ctx context.Context, blocks []Block, opts WriteOptions) (Accepted, error) {
	if opts.RequestID != "" && !requestIDRule.MatchString(opts.RequestID) {
		return Accepted{}, invalidBody("invalid requestId")
	}
	if len(blocks) == 0 {
		return Accepted{}, invalidBody("message blocks must not be empty")
	}
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" && b.MIME == "" && b.Data == "" {
			continue
		}
		if b.Type == "image" && b.Text == "" && b.Data != "" && (b.MIME == "image/png" || b.MIME == "image/jpeg" || b.MIME == "image/gif" || b.MIME == "image/webp") {
			continue
		}
		return Accepted{}, invalidBody("unsupported message block")
	}
	return s.accepted(ctx, api.OpSessionMessageSend, nil, messageBody("blocks", blocks, opts.RequestID), opts, true)
}

func (s *Session) Interrupt(ctx context.Context, opts WriteOptions) (Accepted, error) {
	return s.accepted(ctx, api.OpSessionInterrupt, nil, map[string]any{}, opts, true)
}

func (s *Session) Close(ctx context.Context, opts WriteOptions) (Accepted, error) {
	// Close is also defined for dormant/ended sessions. It has no closure
	// subresource precondition; asking for a live closure here would regress
	// the existing idempotent close behavior.
	res, err := s.client.transport.Call(ctx, api.OpSessionClose, api.CallOptions{Params: s.params(), IdempotencyKey: opts.IdempotencyKey, Deadline: opts.Deadline})
	if err != nil {
		return Accepted{}, err
	}
	var out Accepted
	if err := decode(res, 202, &out); err != nil {
		return Accepted{}, err
	}
	if !out.Accepted || out.SessionID != s.ID() {
		return Accepted{}, invalidResponse("invalid close receipt")
	}
	return out, nil
}

func (s *Session) Permission(ctx context.Context, ticketID, digest, verdict string, opts WriteOptions) (Accepted, error) {
	if digest == "" || verdict != "allow" && verdict != "deny" {
		return Accepted{}, invalidBody("permission requires digest and allow/deny verdict")
	}
	return s.accepted(ctx, api.OpSessionPermissionDecide, map[string]string{"ticketId": ticketID}, map[string]string{"digest": digest, "verdict": verdict}, opts, false)
}

func (s *Session) Answer(ctx context.Context, ticketID string, answers []Answer, opts WriteOptions) (Accepted, error) {
	if len(answers) == 0 {
		return Accepted{}, invalidBody("answers must not be empty")
	}
	for _, answer := range answers {
		if answer.QuestionID == "" || answer.SelectedOptionIDs == nil {
			return Accepted{}, invalidBody("answer requires a questionId and selectedOptionIds array")
		}
	}
	return s.accepted(ctx, api.OpSessionQuestionAnswer, map[string]string{"ticketId": ticketID}, map[string]any{"answers": answers}, opts, false)
}

// ToolResult delivers a legacy client-tool receipt through the same public
// session contract. SDK2 execution receipts use the executor package instead.
func (s *Session) ToolResult(ctx context.Context, callID string, receipt json.RawMessage, opts WriteOptions) (Accepted, error) {
	return s.accepted(ctx, api.OpSessionToolResult, map[string]string{"targetId": callID}, receipt, opts, false)
}

// History pages Serve's existing history. A zero limit is its count-only mode;
// it does not download the complete history or advance any archive coverage.
func (s *Session) History(ctx context.Context, offset, limit int) (json.RawMessage, error) {
	if offset < 0 || limit < 0 {
		return nil, &api.ClientError{Code: api.CodeInvalidQuery, Detail: "history offset and limit must be nonnegative"}
	}
	res, err := s.client.transport.Call(ctx, api.OpSessionHistoryRead, api.CallOptions{Params: s.params(), Query: map[string]string{"offset": strconv.Itoa(offset), "limit": strconv.Itoa(limit)}})
	return rawResult(res, err, 200)
}

func (s *Session) InputCapabilities(ctx context.Context) (json.RawMessage, error) {
	res, err := s.client.transport.Call(ctx, api.OpSessionInputCapabilities, api.CallOptions{Params: s.params()})
	return rawResult(res, err, 200)
}

func (s *Session) SubmitInput(ctx context.Context, input Input, opts WriteOptions) (json.RawMessage, error) {
	if input.InputID == "" || input.Target.HistoryEpoch == "" || input.Target.TurnID == "" ||
		(input.Content.Text == "") == (len(input.Content.Blocks) == 0) || input.Ack != "" && input.Ack != "memory" && input.Ack != "durable" {
		return nil, invalidBody("input requires an explicit target and exactly one text content form")
	}
	for _, block := range input.Content.Blocks {
		if block.Type != "text" || block.Text == "" || block.MIME != "" || block.Data != "" {
			return nil, invalidBody("steering supports text blocks only")
		}
	}
	res, err := s.write(ctx, api.OpSessionInputSubmit, nil, input, opts)
	// 202 confirms acceptance only; the receipt's state remains authoritative.
	return rawResult(res, err, 202)
}

func (s *Session) InputStatus(ctx context.Context, inputID string, target InputTarget) (json.RawMessage, error) {
	if target.HistoryEpoch == "" || target.TurnID == "" {
		return nil, invalidBody("input status requires the original target")
	}
	params := s.params()
	params["targetId"] = inputID
	res, err := s.client.transport.Call(ctx, api.OpSessionInputStatus, api.CallOptions{Params: params, Query: map[string]string{"historyEpoch": target.HistoryEpoch, "turnId": target.TurnID}})
	return rawResult(res, err, 200)
}

func (s *Session) params() map[string]string { return map[string]string{"id": s.ID()} }

func (s *Session) write(ctx context.Context, operation string, extra map[string]string, body any, opts WriteOptions) (*api.Result, error) {
	return s.writeQuery(ctx, operation, extra, nil, body, opts)
}

func (s *Session) writeQuery(ctx context.Context, operation string, extra, query map[string]string, body any, opts WriteOptions) (*api.Result, error) {
	closure, err := s.Capabilities(ctx)
	if err != nil {
		return nil, err
	}
	if state := closure.Operations[operation]; state != api.StateEnabled {
		return nil, &CapabilityError{Operation: operation, State: state}
	}
	params := s.params()
	for k, v := range extra {
		params[k] = v
	}
	closureID := closure.ClosureID
	// session.close has no subresource and the facade explicitly does not
	// accept tansr-closure-id there. Its discovery decision is still checked.
	if operation == api.OpSessionClose {
		closureID = ""
	}
	maxResponseBytes := int64(0)
	if operation == api.OpSessionAudioSpeak || operation == api.OpSessionAudioTranscribe {
		maxResponseBytes = mediaBytes
	}
	return s.client.transport.Call(ctx, operation, api.CallOptions{Params: params, Query: query, Body: body, ClosureID: closureID, IdempotencyKey: opts.IdempotencyKey, Deadline: opts.Deadline, MaxResponseBytes: maxResponseBytes})
}

func (s *Session) accepted(ctx context.Context, operation string, extra map[string]string, body any, opts WriteOptions, withSession bool) (Accepted, error) {
	res, err := s.write(ctx, operation, extra, body, opts)
	if err != nil {
		return Accepted{}, err
	}
	expected := 200
	if withSession {
		expected = 202
	}
	var result Accepted
	if err := decode(res, expected, &result); err != nil {
		return Accepted{}, err
	}
	if !result.Accepted || withSession && result.SessionID != s.ID() {
		return Accepted{}, invalidResponse("invalid acceptance receipt")
	}
	return result, nil
}

func readMeta(raw json.RawMessage, family string) (Meta, error) {
	var meta Meta
	var wire struct {
		LastSeq      *int64 `json:"lastSeq"`
		Live         *bool  `json:"live"`
		Contract     string `json:"contract"`
		Availability string `json:"availability"`
	}
	if _, err := api.DecodeJSON(raw); err != nil {
		return Meta{}, invalidResponse("metadata is invalid JSON")
	}
	if json.Unmarshal(raw, &meta) != nil || json.Unmarshal(raw, &wire) != nil || meta.SessionID == "" || wire.LastSeq == nil || wire.Live == nil || !safeCount(*wire.LastSeq) ||
		meta.Status != "idle" && meta.Status != "running" && meta.Status != "ended" ||
		family == "sdk2-offload-v1" && (wire.Contract != family || wire.Availability != "source-required" && wire.Availability != "source-managed") {
		return Meta{}, invalidResponse("metadata is malformed")
	}
	meta.Raw = append(json.RawMessage(nil), raw...)
	return meta, nil
}

func decode(res *api.Result, status int, target any) error {
	if res == nil || status != 0 && res.Status != status || res.ContentType != "application/json" {
		return invalidResponse("unexpected response status or content type")
	}
	value, err := api.DecodeJSON(res.Body)
	if err != nil {
		return invalidResponse("response is invalid JSON")
	}
	if _, ok := value.(map[string]any); !ok {
		return invalidResponse("response must be an object")
	}
	if err := json.Unmarshal(res.Body, target); err != nil {
		return invalidResponse("response field types are invalid")
	}
	return nil
}

func rawResult(res *api.Result, err error, status int) (json.RawMessage, error) {
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := decode(res, status, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func safeCount(n int64) bool { return n >= 0 && n <= maxSafeInteger }
func invalidResponse(detail string) error {
	return &api.ClientError{Code: api.CodeInvalidResponse, Detail: "session: " + detail}
}
func invalidBody(detail string) error {
	return &api.ClientError{Code: api.CodeInvalidBody, Detail: "session: " + detail}
}

func (s *Session) String() string { return fmt.Sprintf("Session(%s)", s.ID()) }
