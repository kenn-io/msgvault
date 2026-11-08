package messageimport

import (
	"context"
	"errors"
	"fmt"

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
	conversationType     string
}

// NewWriter writes into source through st, which the caller scopes to its
// sync run with store.ScopedToSync.
func NewWriter(st *store.Store, source *store.Source) *Writer {
	return &Writer{
		store:            st,
		sourceID:         source.ID,
		sourceType:       source.SourceType,
		sourceIdentifier: source.Identifier,
		conversationType: ConversationType,
	}
}

// WithSenderIdentifierType makes a sender's id an identifier of
// identifierType that every source shares, instead of an id that means
// something only inside this source. A connector uses it for the account
// IDs its platform assigns, so one account is one participant in every
// source that sees it. An email or phone still decides first; a sender
// with an email and an id joins the id to the email's participant, so the
// account's messages without an email reach that participant too.
func (w *Writer) WithSenderIdentifierType(identifierType string) *Writer {
	w.senderIdentifierType = identifierType
	return w
}

// WithConversationType sets the type of the conversations the writer's
// messages belong to, ConversationType by default. A connector whose
// conversations are something other than a chat, such as a forge's issue
// threads, names their type so that readers treat them as such.
func (w *Writer) WithConversationType(conversationType string) *Writer {
	w.conversationType = conversationType
	return w
}

// Evidence is a connector's own record of a message, which the archive
// keeps as the message's raw evidence.
type Evidence struct {
	Raw []byte
	// Format names Raw's format, such as "github_json": lowercase letters,
	// digits and underscores, starting with a letter.
	Format string
}

// Write validates and stores one message, keyed by message.external_id
// within the source. Evidence identical to the stored copy writes nothing.
func (w *Writer) Write(ctx context.Context, message ImportedMessage) (WriteResult, error) {
	snapshot, err := w.snapshot(message)
	if err != nil {
		return WriteResult{}, err
	}
	return persist(ctx, w.store, w.sourceID, snapshot)
}

// WriteFrom stores one message as Write does, keeping the connector's
// evidence as its raw evidence instead of the normalized message. Evidence
// and message metadata identical to the stored copy write nothing, so
// whatever the message is built from belongs in one of them.
func (w *Writer) WriteFrom(ctx context.Context, message ImportedMessage, evidence Evidence) (WriteResult, error) {
	snapshot, err := w.snapshot(message)
	if err != nil {
		return WriteResult{}, err
	}
	if len(evidence.Raw) == 0 {
		return WriteResult{}, errors.New("message evidence is empty")
	}
	// Stored raw formats have the shape of message types. An empty format
	// would be stored as MIME.
	if !store.IsWellFormedMessageType(evidence.Format) {
		return WriteResult{}, fmt.Errorf("message evidence format %q must be lowercase letters, digits and underscores, starting with a letter", evidence.Format)
	}
	snapshot.Raw, snapshot.RawFormat = evidence.Raw, evidence.Format
	return persist(ctx, w.store, w.sourceID, snapshot)
}

func (w *Writer) snapshot(message ImportedMessage) (Snapshot, error) {
	if w == nil || w.store == nil {
		return Snapshot{}, errors.New("message writer is unavailable")
	}
	normalized, err := normalizeMessage(message)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot, err := buildMessageSnapshot(w.sourceType, w.sourceIdentifier, normalized)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot.SenderIdentifierType = w.senderIdentifierType
	snapshot.ConversationType = w.conversationType
	return snapshot, nil
}
