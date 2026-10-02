package agentgrant

import (
	"context"
	"errors"
)

// Record contains only the secret digest and the public grant metadata.
type Record struct {
	Digest string
	Grant  Grant
}
type Persistence interface {
	SaveAgentGrant(ctx context.Context, record Record) error
	FindAgentGrant(ctx context.Context, digest string) (Record, bool, error)
	ListAgentGrants(ctx context.Context) ([]Record, error)
	DeleteAgentGrant(ctx context.Context, id string) error
}

// ErrPersistence distinguishes an unavailable grant store from invalid input.
var ErrPersistence = errors.New("agent grant persistence failed")

func NewPersistentRegistry(store Persistence) *Registry {
	return &Registry{entries: make(map[string]entry), store: store}
}

func (r *Registry) ListContext(ctx context.Context) ([]Grant, error) {
	if r.store == nil {
		return r.listMemory(), nil
	}
	rows, err := r.store.ListAgentGrants(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Grant, 0, len(rows))
	for _, row := range rows {
		out = append(out, cloneGrant(row.Grant))
	}
	return out, nil
}
func (r *Registry) RevokeContext(ctx context.Context, id string) error {
	if r.store == nil {
		r.Revoke(id)
		return nil
	}
	return r.store.DeleteAgentGrant(ctx, id)
}
