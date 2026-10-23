package session

import (
	"encoding/json"
	"time"
)

// WriteOptions is shared by conversation writes. Neither a key nor a deadline
// is generated implicitly; callers retain them across result reconciliation.
type WriteOptions struct {
	// RequestID is retained by the caller before sending; lost replies are queried by this exact key.
	RequestID      string
	IdempotencyKey string
	Deadline       time.Time
}

type ResumeReference struct {
	SessionID string `json:"sessionId"`
}

type ForkReference struct {
	SessionID    string `json:"sessionId"`
	CheckpointID string `json:"checkpointId"`
}

type Budget struct {
	MaxUSD    *float64 `json:"maxUsd,omitempty"`
	MaxTokens *int64   `json:"maxTokens,omitempty"`
}

// CreateOptions follows the existing Serve session body. ClientTools contains
// Serve ClientToolDecl values, not arbitrary executable code. Prompt, when
// present, starts a turn on creation; omit it to subscribe before sending.
type CreateOptions struct {
	RequestID           string            `json:"requestId,omitempty"`
	Model               string            `json:"model,omitempty"`
	Prompt              string            `json:"prompt,omitempty"`
	Profile             string            `json:"profile,omitempty"`
	Budget              *Budget           `json:"budget,omitempty"`
	Tools               []string          `json:"tools,omitempty"`
	ClientTools         []json.RawMessage `json:"clientTools,omitempty"`
	CapabilitiesProfile string            `json:"capabilitiesProfile,omitempty"`
	Cwd                 string            `json:"cwd,omitempty"`
	Resume              *ResumeReference  `json:"resume,omitempty"`
	Fork                *ForkReference    `json:"fork,omitempty"`
	WriteOptions        `json:"-"`
}

type Created struct {
	SessionID string `json:"sessionId"`
	Resumed   bool   `json:"resumed"`
	LastSeq   int64  `json:"lastSeq"`
}

// Meta preserves the complete additive server projection in Raw, including
// context, media and explicitly requested applicationPrompt metadata.
type Meta struct {
	SessionID      string          `json:"sessionId"`
	EndUserID      string          `json:"endUserId"`
	Status         string          `json:"status"`
	Live           bool            `json:"live"`
	LastSeq        int64           `json:"lastSeq"`
	CreatedAt      string          `json:"createdAt"`
	LastActivityAt string          `json:"lastActivityAt"`
	Title          string          `json:"title,omitempty"`
	Raw            json.RawMessage `json:"-"`
}

type List struct {
	Sessions []Meta `json:"sessions"`
	Total    int64  `json:"total"`
}

// Accepted only confirms the request was accepted, not that a turn finished.
type Accepted struct {
	SessionID string `json:"sessionId,omitempty"`
	Accepted  bool   `json:"accepted"`
}

// Block is one of the two permitted user message blocks. Images use Serve's
// inline base64 data semantics; arbitrary audio/video blocks are not invented.
type Block struct {
	Type string `json:"t"`
	Text string `json:"text,omitempty"`
	MIME string `json:"mime,omitempty"`
	Data string `json:"data,omitempty"`
}

type Answer struct {
	QuestionID        string   `json:"questionId"`
	SelectedOptionIDs []string `json:"selectedOptionIds"`
	FreeText          string   `json:"freeText,omitempty"`
}

type InputTarget struct {
	HistoryEpoch string `json:"historyEpoch"`
	TurnID       string `json:"turnId"`
}

type InputContent struct {
	Text   string  `json:"text,omitempty"`
	Blocks []Block `json:"blocks,omitempty"`
}

// Input inserts text into the targeted running turn. It does not restart it.
// Ack is empty (server default), "memory", or "durable"; receipt acceptance
// must not be interpreted as material consumption by the model.
type Input struct {
	InputID string       `json:"inputId"`
	Target  InputTarget  `json:"target"`
	Content InputContent `json:"content"`
	Ack     string       `json:"ack,omitempty"`
}

// Checkpoint is a Serve-owned snapshot. The SDK transports it without editing
// its content or turning its sequence into an archive acknowledgement.
type Checkpoint struct {
	CheckpointID string          `json:"checkpointId"`
	SessionID    string          `json:"sessionId"`
	CreatedAt    string          `json:"createdAt"`
	Trigger      string          `json:"trigger"`
	Label        string          `json:"label,omitempty"`
	Cwd          string          `json:"cwd"`
	MessageCount int64           `json:"messageCount"`
	Raw          json.RawMessage `json:"-"`
}

type CompactOptions struct {
	Instructions string `json:"instructions,omitempty"`
	// Checkpoint accepts bool or an object containing an optional label.
	Checkpoint any `json:"checkpoint,omitempty"`
}

type TranscriptionRequest struct {
	Model    string `json:"model,omitempty"`
	Audio    string `json:"audio"`
	Language string `json:"language,omitempty"`
	Diarize  *bool  `json:"diarize,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
}

type SpeechRequest struct {
	Model  string   `json:"model,omitempty"`
	Input  string   `json:"input"`
	Voice  string   `json:"voice,omitempty"`
	Format string   `json:"format,omitempty"`
	Speed  *float64 `json:"speed,omitempty"`
}
