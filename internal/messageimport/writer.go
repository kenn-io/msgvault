package messageimport

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/store"
)

// Writer archives messages into one existing source during a sync run that
// its caller owns. A connector writes each item it fetches through it, with
// the same validation, sender identity, search indexing and idempotency as
// the import API.
type Writer struct {
	store            *store.Store
	sourceID         int64
	sourceType       string
	sourceIdentifier string
}

// NewWriter writes into source through st, which the caller scopes to its
// sync run with store.ScopedToSync.
func NewWriter(st *store.Store, source *store.Source) *Writer {
	return &Writer{
		store:            st,
		sourceID:         source.ID,
		sourceType:       source.SourceType,
		sourceIdentifier: source.Identifier,
	}
}

// Write validates and stores one message, keyed by message.external_id
// within the source. Evidence identical to the stored copy writes nothing.
func (w *Writer) Write(ctx context.Context, message ImportedMessage) (WriteResult, error) {
	if w == nil || w.store == nil {
		return WriteResult{}, errors.New("message writer is unavailable")
	}
	normalized, err := normalizeMessage(message)
	if err != nil {
		return WriteResult{}, err
	}
	snapshot, err := buildMessageSnapshot(w.sourceType, w.sourceIdentifier, normalized)
	if err != nil {
		return WriteResult{}, err
	}
	return persist(ctx, w.store, w.sourceID, snapshot)
}
