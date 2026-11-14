package messageimport

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"go.kenn.io/msgvault/internal/apiimport"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

type (
	Status = apiimport.Status
	Result = apiimport.Result
	Hooks  = apiimport.Hooks
)

const (
	StatusCreated = apiimport.StatusCreated
	StatusUpdated = apiimport.StatusUpdated
)

var ErrUnavailable = apiimport.ErrUnavailable

type Importer struct {
	base *apiimport.Importer
}

func NewImporter(s *store.Store, hooks Hooks) *Importer {
	return &Importer{base: apiimport.NewImporter(s, "message import", hooks)}
}

// WithLogger sets the logger used for best-effort post-import work.
func (i *Importer) WithLogger(logger *slog.Logger) *Importer {
	i.base.WithLogger(logger)
	return i
}

// Import stores one message. source.identifier and message.external_id
// identify it: a retry or a replacement updates the same archived message.
func (i *Importer) Import(ctx context.Context, req Request) (Result, error) {
	if i == nil || !i.base.Available() {
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
	// A newly confirmed address also re-attributes this source's earlier
	// messages, including the one being retried, so an unchanged retry needs
	// no rewrite to pick it up.
	return i.base.Import(ctx, apiimport.Item{
		Source: apiimport.Source{
			Type:         SourceType,
			Identifier:   snapshot.SourceIdentifier,
			DisplayName:  snapshot.SourceDisplayName,
			AccountEmail: snapshot.AccountEmail,
		},
		SourceMessageID: snapshot.SourceMessageID,
		Write: func(ctx context.Context, st *store.Store, sourceID int64) (apiimport.Write, error) {
			return persist(ctx, st, sourceID, snapshot)
		},
	})
}

// persist writes the snapshot unless the archive already holds the same
// evidence, then refreshes the stats of the message's conversation and, when
// a replacement moved it, of the conversation it left. An unchanged retry
// still refreshes them, which repairs a write interrupted before its stats.
func persist(ctx context.Context, st *store.Store, sourceID int64, snapshot Snapshot) (apiimport.Write, error) {
	existing, err := st.MessageMetadataBatch(ctx, sourceID, []string{snapshot.SourceMessageID})
	if err != nil {
		return apiimport.Write{}, fmt.Errorf("lookup existing message: %w", err)
	}
	prior, existed := existing[snapshot.SourceMessageID]
	if existed && storedMatches(ctx, st, prior, snapshot) {
		write := apiimport.Write{MessageID: prior.ID}
		return write, recomputeStats(ctx, st, prior.ID, 0)
	}
	var priorConversationID int64
	if existed {
		ref, err := st.MessageConversation(ctx, prior.ID)
		if err != nil {
			return apiimport.Write{}, fmt.Errorf("lookup existing message conversation: %w", err)
		}
		priorConversationID = ref.ConversationID
	}

	senderID, err := resolveSender(ctx, st, snapshot)
	if err != nil {
		return apiimport.Write{}, err
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
			ConversationType:     snapshot.ConversationType,
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
		RawFormat: snapshot.RawFormat,
		Recipients: []store.RecipientSet{{
			Type:           "from",
			ParticipantIDs: []int64{senderID},
			DisplayNames:   []string{sender.Name},
			EmailAddresses: []string{envelopeEmail(snapshot)},
		}},
		PreserveLabels: true,
		FTS: &store.FTSDoc{
			Subject:  snapshot.Title,
			Body:     snapshot.BodyText,
			FromAddr: senderAddress,
		},
	})
	if err != nil {
		return apiimport.Write{}, fmt.Errorf("persist message: %w", err)
	}
	write := apiimport.Write{MessageID: messageID, Created: !existed, Changed: true}
	// The write has committed. Only this call knows the conversation a
	// replacement left, so a canceled request must not skip its refresh.
	return write, recomputeStats(context.WithoutCancel(ctx), st, messageID, priorConversationID)
}

// recomputeStats refreshes the conversation that holds messageID and, when
// it differs, the prior conversation.
func recomputeStats(ctx context.Context, st *store.Store, messageID, priorConversationID int64) error {
	if err := st.RecomputeConversationStatsForMessageContext(ctx, messageID); err != nil {
		return fmt.Errorf("recompute message conversation stats: %w", err)
	}
	if priorConversationID == 0 {
		return nil
	}
	current, err := st.MessageConversation(ctx, messageID)
	if err != nil {
		return fmt.Errorf("lookup message conversation: %w", err)
	}
	if current.ConversationID == priorConversationID {
		return nil
	}
	if err := st.RecomputeConversationStatsForConversationContext(ctx, priorConversationID); err != nil {
		return fmt.Errorf("recompute prior conversation stats: %w", err)
	}
	return nil
}

// envelopeEmail is the address the message's 'from' envelope records. A
// sender named by a connector's own account identity sent the message from
// that account, not from its email, which only joins the account to a
// person: its envelope stays empty, as a chat source's does, so whether the
// message is the owner's is decided by the account's identifiers.
func envelopeEmail(snapshot Snapshot) string {
	if snapshot.SenderIdentifierType != "" && snapshot.Sender.ID != "" {
		return ""
	}
	return snapshot.Sender.Email
}

// storedMatches reports whether the stored copy has the same evidence: raw
// bytes, raw format and metadata. An unreadable stored copy counts as changed
// so the write repairs it.
func storedMatches(ctx context.Context, st *store.Store, prior store.MessageMetadataRecord, snapshot Snapshot) bool {
	raw, format, err := st.GetMessageRawWithFormatContext(ctx, prior.ID)
	return err == nil && format == snapshot.RawFormat && bytes.Equal(raw, snapshot.Raw) &&
		meetingarchive.JSONEvidenceEqual([]byte(prior.Metadata.String), snapshot.Metadata)
}

// resolveSender returns the sender's participant. An email or phone is a
// global identity shared with every other source, and so is an id of a
// connector's shared identifier type; anything else becomes an imported
// identifier, see senderIdentifier. A sender with both an email and an id of
// the shared type links the two, see linkSenderIdentifier.
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
		if err == nil {
			// An existing participant keeps its name; one without a name
			// takes this sender's, as the phone and identifier paths do.
			_, err = st.UpdateParticipantDisplayNameByEmail(ctx, sender.Email, sender.Name)
		}
		if err == nil && sender.ID != "" && snapshot.SenderIdentifierType != "" {
			err = linkSenderIdentifier(ctx, st, id, snapshot.SenderIdentifierType, sender.ID)
		}
	case sender.Phone != "":
		id, err = st.EnsurePhoneParticipantContext(ctx, sender.Phone, sender.Name)
	case sender.ID != "" && snapshot.SenderIdentifierType != "":
		id, err = st.EnsureParticipantByIdentifierContext(ctx,
			snapshot.SenderIdentifierType, sender.ID, sender.Name)
	default:
		id, err = st.EnsureParticipantByIdentifierContext(ctx,
			SenderIdentifierType, senderIdentifier(snapshot.SourceType, snapshot.SourceIdentifier, sender), sender.Name)
	}
	if err != nil {
		return 0, fmt.Errorf("resolve message sender: %w", err)
	}
	return id, nil
}

// linkSenderIdentifier records a sender's account id as an identifier of
// the participant its email resolved to, so a later message that names the
// account by id alone resolves to that participant too. A participant that
// already owns the id is the same account seen with less metadata, such as
// earlier messages without an email: its history is merged into this one,
// as Slack's resolver does. Participants the user keeps as different people
// are not merged; the id moves to this sender, as Beeper's resolver does.
func linkSenderIdentifier(ctx context.Context, st *store.Store, participantID int64, identifierType, value string) error {
	prev, _, err := st.ParticipantByIdentifierContext(ctx, identifierType, value)
	if err != nil {
		return err
	}
	if prev != 0 && prev != participantID {
		if err := st.MergeParticipantsContext(ctx, prev, participantID); err != nil {
			if !errors.Is(err, store.ErrPersonBindingConflict) {
				return err
			}
			slog.WarnContext(ctx, "skipping message sender participant merge across durable persons",
				"loser_participant_id", prev, "winner_participant_id", participantID, "error", err)
		}
	}
	return st.SetParticipantIdentifierContext(ctx, participantID, identifierType, value)
}
