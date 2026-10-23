// Package terminalpersistence implements the explicitly negotiated durable profile.
// It stores opaque bytes and permanent index/ticket facts; Serve owns memory policy.
package terminalpersistence

import (
	"context"
	"errors"
	"github.com/tansrai/tansr-go/executor"
	"github.com/tansrai/tansr-go/memorypublication"
)

const Contract = "terminal-persistence-v1"
const MaxBody = 4 << 20
const MaxObject = 12288
const metadataReserve = 262144
const format = "tansr-go-terminal-persistence-v1"

var ErrIntegrity = errors.New("terminalpersistence: integrity check failed")
var ErrClosed = errors.New("terminalpersistence: store closed")
var ErrUnknown = errors.New("terminalpersistence: durable outcome unknown; reopen original store")

type AdapterError string

func (e AdapterError) Error() string { return string(e) }

type Identity = memorypublication.Identity
type Owner = memorypublication.Owner
type Request map[string]any
type Response map[string]any
type Capabilities struct{ AtomicDurablePersistence, EncryptedAtRest bool }
type Store interface {
	Identity() Identity
	Capabilities() Capabilities
	Execute(context.Context, Request, Owner) (Response, error)
}
type Limits struct {
	ActiveTransfers int `json:"activeTransfers"`
	StagingBytes    int `json:"stagingBytes"`
	ReceiptEntries  int `json:"receiptEntries"`
	TransferFacts   int `json:"transferFacts"`
	Objects         int `json:"objects"`
	RetainedBytes   int `json:"retainedBytes"`
}

func DefaultLimits() Limits { return Limits{8, 16 << 20, 8192, 4096, 16384, 32 << 20} }

type Options struct {
	// Custody and Authority are supplied only by the trusted host. All domain writers
	// must share that authority; imported data never grants the writer role.
	Custody           *Custody
	CustodyAuthority  CustodyAuthority
	MigrationBudget   MigrationBudget
	Path, Mode        string
	Key               []byte
	Identity          Identity
	Limits            Limits
	MaxFileBytes      int
	CurrentScope      func() (executor.Scope, error)
	AuthorizeRecovery func(Identity, string, Owner, Owner) bool
	// CommitHook is a trusted diagnostic hook. It receives stages only, never stored bytes.
	CommitHook func(string) error
}
