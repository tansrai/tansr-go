package terminalpersistence

import (
	"context"
	"encoding/json"
	"github.com/tansrai/tansr-go/api"
	"github.com/tansrai/tansr-go/internal/wire"
)

// CustodyClient exposes read-only discovery/reconciliation. It is not a shared writer authority.
type CustodyClient struct{ client *api.Client }

func NewCustodyClient(client *api.Client) *CustodyClient { return &CustodyClient{client} }
func (c *CustodyClient) read(ctx context.Context, op, definition string, opts api.CallOptions) (Response, error) {
	if c == nil || c.client == nil {
		return nil, ErrIntegrity
	}
	r, e := c.client.Call(ctx, op, opts)
	if e != nil {
		return nil, e
	}
	if r.Status != 200 {
		return nil, ErrIntegrity
	}
	v, e := wire.Decode(CustodyContract, definition, r.Body)
	if e != nil {
		return nil, e
	}
	return v.(map[string]any), nil
}
func (c *CustodyClient) Capabilities(ctx context.Context) (Response, error) {
	return c.read(ctx, api.OpTerminalCustodyCapabilities, "CapabilitiesResponse", api.CallOptions{Query: map[string]string{"contract": CustodyContract}})
}
func (c *CustodyClient) Query(ctx context.Context, identity Identity, operationID, intentSHA256 string) (Response, error) {
	r := Request{"contract": CustodyContract, "action": "query", "identity": identity, "operationId": operationID, "intentSha256": intentSHA256}
	if e := custodyValidate("QueryRequest", r); e != nil {
		return nil, e
	}
	b, _ := json.Marshal(identity)
	q := map[string]string{}
	_ = json.Unmarshal(b, &q)
	q["contract"], q["intentSha256"] = CustodyContract, intentSHA256
	out, e := c.read(ctx, api.OpTerminalCustodyQuery, "QueryResponse", api.CallOptions{Params: map[string]string{"id": operationID}, Query: q})
	if e != nil {
		return nil, e
	}
	return out, validateCustodyResponse(r, out)
}
