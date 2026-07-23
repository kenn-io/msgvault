package meetingimport

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/meetingidentity"
	"go.kenn.io/msgvault/internal/store"
)

type Status string

var ErrUnavailable = errors.New("meeting importer is unavailable")

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
	store *store.Store
	hooks Hooks
}

func NewImporter(s *store.Store, hooks Hooks) *Importer {
	return &Importer{store: s, hooks: hooks}
}

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

	source, err := i.store.GetOrCreateSource(SourceType, snapshot.SourceIdentifier)
	if err != nil {
		return Result{}, fmt.Errorf("resolve meeting source: %w", err)
	}
	result.SourceID = source.ID
	result.SourceMessageID = snapshot.SourceMessageID

	if err := i.store.UpdateSourceDisplayName(source.ID, snapshot.SourceDisplayName); err != nil {
		return result, fmt.Errorf("update meeting source display name: %w", err)
	}
	if err := i.store.AddAccountIdentity(source.ID, snapshot.AccountEmail, "account-email"); err != nil {
		return result, fmt.Errorf("confirm meeting source identity: %w", err)
	}
	if i.hooks.AfterSourceSetup != nil {
		if err := i.hooks.AfterSourceSetup(); err != nil {
			return result, fmt.Errorf("run post-source setup: %w", err)
		}
	}

	syncID, err := i.store.StartSync(source.ID, SourceType)
	if err != nil {
		return result, fmt.Errorf("start meeting import sync: %w", err)
	}
	checkpoint := &store.Checkpoint{}
	defer func() {
		if retErr == nil {
			return
		}
		if failErr := i.store.FailSyncWithCheckpoint(syncID, retErr.Error(), checkpoint); failErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("record failed meeting import sync: %w", failErr))
		}
	}()

	existing, err := i.store.MessageExistsBatch(source.ID, []string{snapshot.SourceMessageID})
	if err != nil {
		return result, fmt.Errorf("lookup existing meeting: %w", err)
	}
	_, existed := existing[snapshot.SourceMessageID]

	identities, err := meetingidentity.ForSource(i.store, source.ID, snapshot.AccountEmail)
	if err != nil {
		return result, err
	}

	var senderID int64
	organizerEmail := ""
	organizerName := ""
	if snapshot.Organizer != nil {
		organizerEmail = snapshot.Organizer.Email
		organizerName = snapshot.Organizer.Name
		senderID, err = i.store.EnsureParticipant(
			organizerEmail,
			organizerName,
			emailDomain(organizerEmail),
		)
		if err != nil {
			return result, fmt.Errorf("ensure meeting organizer: %w", err)
		}
	}

	attendeeIDs := make([]int64, 0, len(snapshot.Attendees))
	attendeeNames := make([]string, 0, len(snapshot.Attendees))
	attendeeEmails := make([]string, 0, len(snapshot.Attendees))
	conversationParticipants := make([]store.ConversationParticipantRef, 0, len(snapshot.Attendees))
	for _, attendee := range snapshot.Attendees {
		participantID, ensureErr := i.store.EnsureParticipant(
			attendee.Email,
			attendee.Name,
			emailDomain(attendee.Email),
		)
		if ensureErr != nil {
			return result, fmt.Errorf("ensure meeting attendee: %w", ensureErr)
		}
		attendeeIDs = append(attendeeIDs, participantID)
		attendeeNames = append(attendeeNames, attendee.Name)
		attendeeEmails = append(attendeeEmails, attendee.Email)
		conversationParticipants = append(conversationParticipants, store.ConversationParticipantRef{
			ParticipantID: participantID,
			Role:          "member",
		})
	}

	var fromIDs []int64
	var fromNames []string
	if senderID != 0 {
		fromIDs = []int64{senderID}
		fromNames = []string{organizerName}
	}
	metadata := sql.NullString{String: string(snapshot.Metadata), Valid: true}
	message := &store.Message{
		SourceID:        source.ID,
		SourceMessageID: snapshot.SourceMessageID,
		MessageType:     MessageType,
		SentAt:          sql.NullTime{Time: snapshot.StartedAt, Valid: true},
		SenderID:        sql.NullInt64{Int64: senderID, Valid: senderID != 0},
		IsFromMe:        organizerEmail != "" && identities.Contains(organizerEmail),
		Subject:         sql.NullString{String: snapshot.Title, Valid: snapshot.Title != ""},
		Snippet:         sql.NullString{String: snapshot.Snippet, Valid: snapshot.Snippet != ""},
		SizeEstimate:    int64(len(snapshot.Body)),
	}

	if err := ctx.Err(); err != nil {
		return result, err
	}
	messageID, err := i.store.PersistMessage(&store.MessagePersistData{
		Message: message,
		Conversation: &store.ConversationPersistData{
			SourceConversationID: snapshot.SourceMessageID,
			ConversationType:     ConversationType,
			Title:                snapshot.Title,
			Participants:         conversationParticipants,
		},
		Metadata:  &metadata,
		BodyText:  sql.NullString{String: snapshot.Body, Valid: snapshot.Body != ""},
		RawMIME:   snapshot.Raw,
		RawFormat: RawFormat,
		Recipients: []store.RecipientSet{
			{Type: "from", ParticipantIDs: fromIDs, DisplayNames: fromNames},
			{Type: "to", ParticipantIDs: attendeeIDs, DisplayNames: attendeeNames},
		},
		PreserveLabels: true,
		FTS: &store.FTSDoc{
			Subject:  snapshot.Title,
			Body:     snapshot.Body,
			FromAddr: organizerEmail,
			ToAddrs:  strings.Join(attendeeEmails, " "),
		},
	})
	if err != nil {
		return result, fmt.Errorf("persist meeting: %w", err)
	}
	result.MessageID = messageID
	result.Status = StatusCreated
	checkpoint.MessagesProcessed = 1
	checkpoint.MessagesAdded = 1
	if existed {
		result.Status = StatusUpdated
		checkpoint.MessagesAdded = 0
		checkpoint.MessagesUpdated = 1
	}

	if err := i.store.UpdateSyncCheckpoint(syncID, checkpoint); err != nil {
		return result, fmt.Errorf("checkpoint meeting import sync: %w", err)
	}
	if err := i.store.RecomputeConversationStats(source.ID); err != nil {
		return result, fmt.Errorf("recompute meeting conversation stats: %w", err)
	}
	if err := i.store.CompleteSync(syncID, ""); err != nil {
		return result, fmt.Errorf("complete meeting import sync: %w", err)
	}
	if i.hooks.RefreshCache != nil {
		cacheLabel := SourceType + ":" + snapshot.SourceIdentifier
		if err := i.hooks.RefreshCache(ctx, cacheLabel); err != nil {
			return result, fmt.Errorf("refresh meeting analytics cache: %w", err)
		}
	}
	return result, nil
}

func emailDomain(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}
