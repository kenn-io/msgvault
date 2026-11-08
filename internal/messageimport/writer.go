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
	store                *store.Store
	sourceID             int64
	sourceType           string
	sourceIdentifier     string
	senderIdentifierType string
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

// WithSenderIdentifierType makes a sender's id an identifier of
// identifierType that every source shares, instead of an id that means
// something only inside this source. A connector uses it for the account
// IDs its platform assigns, so one account is one participant in every
// source that sees it. An email or phone still decides first.
func (w *Writer) WithSenderIdentifierType(identifierType string) *Writer {
	w.senderIdentifierType = identifierType
	return w
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
	snapshot.SenderIdentifierType = w.senderIdentifierType
	return persist(ctx, w.store, w.sourceID, snapshot)
}
