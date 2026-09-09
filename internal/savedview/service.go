// Package savedview defines the Saved View service contract MCP consumes and
// the daemon client implements. The version-1 vocabulary itself belongs to
// the store, which validates every definition against it before persisting.
package savedview

import (
	"context"
	"encoding/json"
	"fmt"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

// Reader exposes durable Saved View definitions and runs them through the
// daemon's canonical Explore execution.
type Reader interface {
	ListSavedViews(ctx context.Context) ([]store.SavedView, error)
	GetSavedView(ctx context.Context, id int64) (*store.SavedView, error)
	RunSavedView(ctx context.Context, id int64, limit int, cursor string) (*RunPage, error)
}

// Writer mutates Saved Views while preserving the store/API optimistic
// revision contract.
type Writer interface {
	CreateSavedView(ctx context.Context, input store.SavedViewInput) (*store.SavedView, error)
	UpdateSavedView(ctx context.Context, id, expectedRevision int64, patch Patch) (*store.SavedView, error)
	DeleteSavedView(ctx context.Context, id, expectedRevision int64) error
}

// Service is the full Saved View surface an embedder wires into MCP.
type Service interface {
	Reader
	Writer
}

// Patch contains only fields supplied by the caller. An empty Description
// string clears the existing description, matching the HTTP API.
type Patch struct {
	Name           *string
	Description    *string
	CanonicalState *store.SavedViewStateEnvelope
	SchemaVersion  *int
}

// ResultKind names which typed array a RunPage carries.
type ResultKind string

const (
	ResultEntries ResultKind = "entries"
	ResultGroups  ResultKind = "groups"
	ResultFiles   ResultKind = "files"
)

// RunPage is one typed page from the Explore surface selected by a Saved
// View's grouping and presentation definition.
type RunPage struct {
	View                   store.SavedView
	ResultKind             ResultKind
	Rows                   []query.EntryRow
	Groups                 []query.ExploreGroupRow
	Files                  []query.ExploreFileFact
	TotalCount             *int64
	NextCursor             string
	CacheRevision          string
	SearchProvenance       query.SearchProvenance
	CandidateSnapshotID    string
	CandidatePoolSaturated bool
	SearchDeletionScope    string
}

// Returned reports how many typed results the page carries.
func (p *RunPage) Returned() int {
	return len(p.Rows) + len(p.Groups) + len(p.Files)
}

// ExecutableState decodes a Saved View's canonical state and checks it against
// the store's version-1 vocabulary, so a row written by an older binary cannot
// reach Explore with a definition the daemon would not accept today. The raw
// JSON is checked before decoding because a typed decoder drops unknown
// fields and explicit nulls: a stale {"selection":[1]} would otherwise decode
// to an empty definition and run an unfiltered query. Errors wrap
// store.ErrSavedViewUnsupportedSchemaVersion or store.ErrSavedViewInvalidState
// so callers can map them uniformly.
func ExecutableState(view store.SavedView) (store.SavedViewStateEnvelope, error) {
	if view.SchemaVersion != store.CurrentSavedViewSchemaVersion {
		return store.SavedViewStateEnvelope{}, fmt.Errorf(
			"%w: Saved View %d uses schema version %d",
			store.ErrSavedViewUnsupportedSchemaVersion, view.ID, view.SchemaVersion,
		)
	}
	if err := store.ValidateSavedViewStateJSON(view.CanonicalState); err != nil {
		return store.SavedViewStateEnvelope{}, fmt.Errorf("saved view %d: %w", view.ID, err)
	}
	var state store.SavedViewStateEnvelope
	if err := json.Unmarshal(view.CanonicalState, &state); err != nil {
		return store.SavedViewStateEnvelope{}, fmt.Errorf(
			"%w: decode Saved View %d: %w", store.ErrSavedViewInvalidState, view.ID, err,
		)
	}
	if err := store.ValidateSavedViewState(state); err != nil {
		return store.SavedViewStateEnvelope{}, fmt.Errorf("saved view %d: %w", view.ID, err)
	}
	return state, nil
}
