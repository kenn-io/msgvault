package messageimport

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

type Status string

var ErrUnavailable = errors.New("message importer is unavailable")

const (
	StatusCreated Status = "created"
	StatusUpdated Status = "updated"
)

type Result struct {
	Status          Status
	SourceID        int64
	MessageID       int64
	SourceMessageID string
}

type Hooks struct {
	AfterSourceSetup func() error
	RefreshCache     func(context.Context, string) error
}

type Importer struct {
	store  *store.Store
	hooks  Hooks
	logger *slog.Logger
}

func NewImporter(s *store.Store, hooks Hooks) *Importer {
	return &Importer{store: s, hooks: hooks, logger: slog.Default()}
}

// WithLogger sets the logger used for best-effort post-import work.
func (i *Importer) WithLogger(logger *slog.Logger) *Importer {
	if logger != nil {
		i.logger = logger
	}
	return i
}

// writeResult reports what persisting one snapshot did. Changed means the
// message write committed, even when a later step returned an error.
type writeResult struct {
	MessageID int64
	Created   bool
	Changed   bool
}

// Import stores one message. source.identifier and message.external_id
// identify it: a retry or a replacement updates the same archived message.
func (i *Importer) Import(ctx context.Context, req Request) (result Result, retErr error) {
	if i == nil || i.store == nil {
		return Result{}, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	normalized, err := req.Normalize()
	if err != nil {
		return Result{}, err
	}
	snapshot, err := BuildSnapshot(normalized)
	if err != nil {
		return Result{}, err
	}

	source, err := i.store.GetOrCreateSourceContext(ctx, SourceType, snapshot.SourceIdentifier)
	if err != nil {
		return Result{}, fmt.Errorf("resolve message import source: %w", err)
	}
	result.SourceID = source.ID
	result.SourceMessageID = snapshot.SourceMessageID

	displayName := snapshot.SourceDisplayName
	if displayName == "" && (!source.DisplayName.Valid || strings.TrimSpace(source.DisplayName.String) == "") {
		displayName = snapshot.SourceIdentifier
	}
	if displayName != "" && (!source.DisplayName.Valid || source.DisplayName.String != displayName) {
		if err := i.store.UpdateSourceDisplayNameContext(ctx, source.ID, displayName); err != nil {
			if !errors.Is(err, store.ErrSourceSettingsInvalid) {
				return result, fmt.Errorf("update message import source display name: %w", err)
			}
			i.logger.WarnContext(ctx,
				"message import source display name conflicts with another source selector; keeping current name",
				"source_id", source.ID,
			)
		}
	}
	// A newly confirmed address also re-attributes this source's earlier
	// messages, including the one being retried, so an unchanged retry needs
	// no rewrite to pick it up.
	if err := i.store.AddAccountIdentityAndRefreshMessageAttributionContext(
		ctx, source.ID, snapshot.AccountEmail, "account-email", "",
	); err != nil {
		return result, fmt.Errorf("confirm message import source identity: %w", err)
	}
	if i.hooks.AfterSourceSetup != nil {
		if err := i.hooks.AfterSourceSetup(); err != nil {
			return result, fmt.Errorf("run post-source setup: %w", err)
		}
	}

	syncID, err := i.store.StartSyncContext(ctx, source.ID, SourceType)
	if err != nil {
		return result, fmt.Errorf("start message import sync: %w", err)
	}
	st := i.store.ScopedToSync(source.ID, syncID)
	checkpoint := &store.Checkpoint{}
	defer func() {
		if retErr == nil {
			return
		}
		// The failed run must be recorded even when the request was canceled.
		if failErr := st.FailSyncWithCheckpointContext(
			context.WithoutCancel(ctx), syncID, retErr.Error(), checkpoint,
		); failErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("record failed message import sync: %w", failErr))
		}
	}()

	write, writeErr := persist(ctx, st, source.ID, snapshot)
	if write.MessageID != 0 {
		result.MessageID = write.MessageID
		result.Status = StatusUpdated
		checkpoint.MessagesProcessed = 1
		if write.Created {
			result.Status = StatusCreated
			checkpoint.MessagesAdded = 1
		} else if write.Changed {
			checkpoint.MessagesUpdated = 1
		}
	}
	label := SourceType + ":" + snapshot.SourceIdentifier
	if writeErr != nil {
		if write.Changed {
			// The message is stored; keep the cache in step even though the run failed.
			i.refreshCache(context.WithoutCancel(ctx), label, source.ID, snapshot.SourceMessageID)
		}
		return result, writeErr
	}

	if err := st.UpdateSyncCheckpointContext(ctx, syncID, checkpoint); err != nil {
		return result, fmt.Errorf("checkpoint message import sync: %w", err)
	}
	if err := st.CompleteSyncContext(ctx, syncID, ""); err != nil {
		return result, fmt.Errorf("complete message import sync: %w", err)
	}
	// Even an unchanged message can follow an account identity that
	// re-attributed earlier messages; the refresh's own staleness check
	// decides whether a build is due.
	i.refreshCache(ctx, label, source.ID, snapshot.SourceMessageID)
	return result, nil
}

// persist writes the snapshot unless the archive already holds the same
// evidence, and keeps conversation stats current either way.
func persist(ctx context.Context, st *store.Store, sourceID int64, snapshot Snapshot) (writeResult, error) {
	existing, err := st.MessageMetadataBatch(ctx, sourceID, []string{snapshot.SourceMessageID})
	if err != nil {
		return writeResult{}, fmt.Errorf("lookup existing message: %w", err)
	}
	prior, existed := existing[snapshot.SourceMessageID]
	if existed && storedMatches(ctx, st, prior, snapshot) {
		// A retry also repairs stats an interrupted earlier write left stale.
		if err := st.RecomputeConversationStatsForMessageContext(ctx, prior.ID); err != nil {
			return writeResult{MessageID: prior.ID}, fmt.Errorf("recompute conversation stats: %w", err)
		}
		return writeResult{MessageID: prior.ID}, nil
	}

	senderID, err := resolveSender(ctx, st, snapshot)
	if err != nil {
		return writeResult{}, err
	}
	sender := snapshot.Sender
	senderAddress := sender.Email
	if senderAddress == "" {
		senderAddress = sender.Phone
	}
	metadata := sql.NullString{String: string(snapshot.Metadata), Valid: true}
	messageID, err := st.PersistMessageContext(ctx, &store.MessagePersistData{
		Message: &store.Message{
			SourceID:                sourceID,
			PreserveAttachmentStats: true,
			SourceMessageID:         snapshot.SourceMessageID,
			MessageType:             snapshot.MessageType,
			SentAt:                  sql.NullTime{Time: snapshot.SentAt, Valid: true},
			SenderID:                sql.NullInt64{Int64: senderID, Valid: true},
			Subject:                 sql.NullString{String: snapshot.Title, Valid: snapshot.Title != ""},
			Snippet:                 sql.NullString{String: snapshot.Snippet, Valid: snapshot.Snippet != ""},
			SizeEstimate:            int64(len(snapshot.BodyText) + len(snapshot.BodyHTML)),
		},
		Conversation: &store.ConversationPersistData{
			SourceConversationID: snapshot.SourceConversationID,
			ConversationType:     ConversationType,
			Title:                snapshot.ConversationTitle,
			Participants: []store.ConversationParticipantRef{
				{ParticipantID: senderID, Role: "member"},
			},
			PreserveExistingParticipants: true,
		},
		Metadata:  &metadata,
		BodyText:  sql.NullString{String: snapshot.BodyText, Valid: snapshot.BodyText != ""},
		BodyHTML:  sql.NullString{String: snapshot.BodyHTML, Valid: snapshot.BodyHTML != ""},
		RawMIME:   snapshot.Raw,
		RawFormat: RawFormat,
		Recipients: []store.RecipientSet{{
			Type:           "from",
			ParticipantIDs: []int64{senderID},
			DisplayNames:   []string{sender.Name},
			EmailAddresses: []string{sender.Email},
		}},
		PreserveLabels: true,
		// The same fields a search-index rebuild derives from the stored rows.
		FTS: &store.FTSDoc{
			Subject:  snapshot.Title,
			Body:     snapshot.BodyText,
			FromAddr: senderAddress,
		},
	})
	if err != nil {
		return writeResult{}, fmt.Errorf("persist message: %w", err)
	}
	result := writeResult{MessageID: messageID, Created: !existed, Changed: true}

	if existed && prior.SourceConversationID.String != snapshot.SourceConversationID {
		// The message left its old conversation, whose stats change too.
		err = st.RecomputeConversationStatsContext(ctx, sourceID)
	} else {
		err = st.RecomputeConversationStatsForMessageContext(ctx, messageID)
	}
	if err != nil {
		return result, fmt.Errorf("recompute conversation stats: %w", err)
	}
	return result, nil
}

// storedMatches treats an unreadable stored copy as changed so the write
// repairs it.
func storedMatches(ctx context.Context, st *store.Store, prior store.MessageMetadataRecord, snapshot Snapshot) bool {
	raw, err := st.GetMessageRawContext(ctx, prior.ID)
	return err == nil && bytes.Equal(raw, snapshot.Raw) &&
		meetingarchive.JSONEvidenceEqual([]byte(prior.Metadata.String), snapshot.Metadata)
}

// resolveSender returns the sender's participant. An email or phone is a
// global identity shared with every other source; anything else becomes an
// imported identifier, see senderIdentifier.
func resolveSender(ctx context.Context, st *store.Store, snapshot Snapshot) (int64, error) {
	sender := snapshot.Sender
	var (
		id  int64
		err error
	)
	switch {
	case sender.Email != "":
		_, domain, _ := strings.Cut(sender.Email, "@")
		id, err = st.EnsureParticipantContext(ctx, sender.Email, sender.Name, domain)
	case sender.Phone != "":
		id, err = st.EnsurePhoneParticipantContext(ctx, sender.Phone, sender.Name)
	default:
		id, err = st.EnsureParticipantByIdentifierContext(ctx,
			SenderIdentifierType, senderIdentifier(snapshot.SourceIdentifier, sender), sender.Name)
	}
	if err != nil {
		return 0, fmt.Errorf("resolve message sender: %w", err)
	}
	return id, nil
}

func (i *Importer) refreshCache(ctx context.Context, label string, sourceID int64, sourceMessageID string) {
	if i.hooks.RefreshCache == nil {
		return
	}
	// The import has completed before this best-effort hook runs.
	if err := i.hooks.RefreshCache(ctx, label); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		i.logger.Error("message import analytics cache refresh failed",
			"source", label,
			"source_id", sourceID,
			"source_message_id", sourceMessageID,
			"error", err,
		)
	}
}
