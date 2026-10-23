package terminalpersistence

import (
	"context"
	"encoding/json"
	"github.com/tansrai/tansr-go/canonical"
	"github.com/tansrai/tansr-go/executor"
	"time"
)

type CustodyHost struct {
	store     *FileStore
	authorize func(Request) error
}

func NewCustodyHost(store *FileStore, authorize func(Request) error) (*CustodyHost, error) {
	if store == nil || !store.CustodyHandoffEnabled() || authorize == nil {
		return nil, executor.ErrUnsupported
	}
	return &CustodyHost{store, authorize}, nil
}
func (h *CustodyHost) RequiresEncryptedJournal() bool { return true }
func (h *CustodyHost) Execute(ctx context.Context, op executor.Operation, args map[string]any) (any, error) {
	digest, e := executor.OperationDigest(op)
	expiry, ex := time.Parse(time.RFC3339Nano, op.ExpiresAt)
	if e != nil || ex != nil || digest != op.Digest || !time.Now().Before(expiry) || op.ToolName != "MemoryPublication" || op.Request.Operation != "tool.invoke" || op.Request.Args["name"] != executor.PersistenceCustodyToolName || op.Request.Args["definitionDigest"] != executor.PersistenceCustodyDefinitionDigest {
		return nil, &executor.Rejected{Code: "EACCES"}
	}
	text, ok := op.Request.Args["argsJson"].(string)
	v, e := canonical.ParseStrict([]byte(text), canonical.Options{MaxBytes: 32768})
	if !ok || e != nil || !equal(v, args) {
		return nil, &executor.Rejected{Code: "EINVAL"}
	}
	out, e := h.store.ExecuteCustody(ctx, args, Owner{Scope: op.Scope, SessionID: op.SessionID, Binding: op.Binding}, h.authorize)
	if e != nil {
		return nil, e
	}
	raw, e := json.Marshal(out)
	if e != nil {
		return nil, e
	}
	return map[string]any{"status": "ok", "content": []any{map[string]any{"t": "text", "text": string(raw)}}}, nil
}
