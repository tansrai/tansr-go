package archive

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/tansrai/tansr-go/api"
	"github.com/tansrai/tansr-go/internal/wire"
)

var ErrSourceManaged = errors.New("archive: source-managed bindings are installed by the trusted Serve host")

type SourcePersistence struct {
	Contract         string `json:"contract"`
	Mode             string `json:"mode"`
	SourceID         string `json:"sourceId"`
	SourceGeneration string `json:"sourceGeneration"`
	LocalDisk        bool   `json:"localDisk"`
	Handoff          string `json:"handoff"`
	TrustedControl   bool   `json:"trustedControl"`
}

func samePersistence(a, b *SourcePersistence) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
func (c *Client) observePersistence(id string, p *SourcePersistence) error {
	c.persistenceMu.Lock()
	defer c.persistenceMu.Unlock()
	if old, ok := c.persistence[id]; ok && !samePersistence(old, p) {
		return ErrIntegrity
	}
	if p != nil {
		copy := *p
		c.persistence[id] = &copy
	} else {
		c.persistence[id] = nil
	}
	return nil
}

// OpenInstalled consumes the trusted factory's existing binding; it never creates a replacement.
// Cloud mode uses this without opening an archive file. Terminal mode still calls SyncOnce with
// a durable Store: SSE observation alone never acknowledges material.
func (c *Client) OpenInstalled(ctx context.Context, sessionID string) (Binding, Status, error) {
	var b Binding
	var s Status
	t, e := c.BindingTarget(ctx, sessionID)
	if e != nil {
		return b, s, e
	}
	if t.BindingID == nil {
		return b, s, ErrSourceManaged
	}
	b, e = c.Binding(ctx, *t.BindingID)
	if e != nil {
		return b, s, e
	}
	if b.Target.SessionID != sessionID || !equal(b.Target, t.Target) {
		return b, s, ErrIntegrity
	}
	s, e = c.Status(ctx, b.BindingID)
	if e == nil {
		_, e = IdentityFrom(b, s)
	}
	return b, s, e
}

type Events struct {
	stream  *api.Stream
	client  *Client
	binding Binding
}

func (c *Client) Events(ctx context.Context, b Binding, lastEventID string) (*Events, error) {
	if e := validateBinding(b); e != nil {
		return nil, e
	}
	if e := c.observePersistence(b.BindingID, b.Persistence); e != nil {
		return nil, e
	}
	s, e := c.api.Events(ctx, api.OpArchiveEventsObserve, api.EventsOptions{Params: map[string]string{"id": b.BindingID}, Query: map[string]string{"protocol": Protocol}, LastEventID: lastEventID})
	if e != nil {
		return nil, e
	}
	return &Events{s, c, b}, nil
}
func (e *Events) Close() error { return e.stream.Close() }
func (e *Events) Next() (json.RawMessage, error) {
	f, err := e.stream.Next()
	if err != nil {
		return nil, err
	}
	raw := []byte(f.Data)
	if f.Envelope != nil {
		raw, err = json.Marshal(f.Envelope.Raw)
		if err != nil {
			return nil, err
		}
	}
	v, err := wire.Decode(Protocol, "EventFrame", raw)
	if err != nil {
		e.Close()
		return nil, err
	}
	m := v.(map[string]any)
	if m["bindingId"] != e.binding.BindingID || !equal(m["generations"], e.binding.Target.Generations) {
		e.Close()
		return nil, ErrIntegrity
	}
	payload, _ := json.Marshal(m["payload"])
	switch m["eventType"] {
	case "archive.status":
		var s Status
		err = json.Unmarshal(payload, &s)
		if err == nil && (s.BindingID != e.binding.BindingID || s.Persistence != nil && (s.SourceID != s.Persistence.SourceID || s.SourceGeneration != s.Persistence.SourceGeneration)) {
			err = ErrIntegrity
		}
		if err == nil {
			err = e.client.observePersistence(s.BindingID, s.Persistence)
		}
	case "binding.status":
		var b Binding
		err = json.Unmarshal(payload, &b)
		if err == nil {
			err = validateBinding(b)
		}
		if err == nil && (b.BindingID != e.binding.BindingID || !equal(b.Scope, e.binding.Scope)) {
			err = ErrIntegrity
		}
		if err == nil {
			err = e.client.observePersistence(b.BindingID, b.Persistence)
		}
	}
	if err != nil {
		e.Close()
		return nil, err
	}
	return json.RawMessage(raw), nil
}
