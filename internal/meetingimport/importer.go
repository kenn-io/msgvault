package meetingimport

import (
	"bytes"
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

	displayName := snapshot.SourceDisplayName
	if displayName == "" && (!source.DisplayName.Valid || strings.TrimSpace(source.DisplayName.String) == "") {
		displayName = snapshot.SourceIdentifier
	}
	if displayName != "" && (!source.DisplayName.Valid || source.DisplayName.String != displayName) {
		if err := i.store.UpdateSourceDisplayNameContext(ctx, source.ID, displayName); err != nil {
			return result, fmt.Errorf("update meeting source display name: %w", err)
		}
	}
	if err := i.store.AddAccountIdentityAndRefreshMessageAttributionContext(
		ctx,
		source.ID,
		snapshot.AccountEmail,
		"account-email",
		snapshot.SourceMessageID,
	); err != nil {
		return result, fmt.Errorf("confirm meeting source identity: %w", err)
	}
	if i.hooks.AfterSourceSetup != nil {
		if err := i.hooks.AfterSourceSetup(); err != nil {
			return result, fmt.Errorf("run post-source setup: %w", err)
		}
	}

	syncID, err := i.store.StartSyncContext(ctx, source.ID, SourceType)
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
	identities, err := meetingidentity.ForSource(i.store, source.ID, snapshot.AccountEmail)
	if err != nil {
		return result, err
	}

	organizerEmail := ""
	organizerName := ""
	if snapshot.Organizer != nil {
		organizerEmail = snapshot.Organizer.Email
		organizerName = snapshot.Organizer.Name
	}
	expectedIsFromMe := organizerEmail != "" && identities.Contains(organizerEmail)

	existingMessageID, existed := existing[snapshot.SourceMessageID]
	if existed {
		result.Status = StatusUpdated
		result.MessageID = existingMessageID

		storedRaw, rawErr := i.store.GetMessageRaw(existingMessageID)
		if rawErr == nil && bytes.Equal(storedRaw, snapshot.Raw) {
			storedIsFromMe, attributionErr := i.store.GetMessageIsFromMe(existingMessageID)
			if attributionErr == nil && storedIsFromMe == expectedIsFromMe {
				if err := ctx.Err(); err != nil {
					return result, err
				}
				checkpoint.MessagesProcessed = 1
				if err := i.store.UpdateSyncCheckpointContext(ctx, syncID, checkpoint); err != nil {
					return result, fmt.Errorf("checkpoint meeting import sync: %w", err)
				}
				if err := i.store.RecomputeConversationStatsForMessageContext(ctx, existingMessageID); err != nil {
					return result, fmt.Errorf("recompute meeting conversation stats: %w", err)
				}
				if err := i.store.CompleteSyncContext(ctx, syncID, ""); err != nil {
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
		}
		// Missing or unreadable canonical data, or mismatched derived
		// attribution, is not a match. Continue through the transactional
		// persistence path so a valid retry repairs the message; genuine
		// storage failures surface from that rewrite.
	}

	participants := make(
		[]store.ParticipantPersistData,
		0,
		len(snapshot.Attendees)+1,
	)
	hasOrganizer := snapshot.Organizer != nil
	if hasOrganizer {
		participants = append(participants, store.ParticipantPersistData{
			EmailAddress: organizerEmail,
			DisplayName:  organizerName,
			Domain:       emailDomain(organizerEmail),
		})
	}

	attendeeNames := make([]string, 0, len(snapshot.Attendees))
	attendeeEmails := make([]string, 0, len(snapshot.Attendees))
	for _, attendee := range snapshot.Attendees {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		participants = append(participants, store.ParticipantPersistData{
			EmailAddress: attendee.Email,
			DisplayName:  attendee.Name,
			Domain:       emailDomain(attendee.Email),
		})
		attendeeNames = append(attendeeNames, attendee.Name)
		attendeeEmails = append(attendeeEmails, attendee.Email)
	}

	metadata := sql.NullString{String: string(snapshot.Metadata), Valid: true}
	messageID, err := i.store.PersistMessageWithParticipantsContext(
		ctx,
		participants,
		func(participantIDs []int64) *store.MessagePersistData {
			attendeeOffset := 0
			var senderID int64
			var fromIDs []int64
			var fromNames []string
			if hasOrganizer {
				senderID = participantIDs[0]
				attendeeOffset = 1
				fromIDs = []int64{senderID}
				fromNames = []string{organizerName}
			}
			attendeeIDs := participantIDs[attendeeOffset:]
			conversationParticipants := make(
				[]store.ConversationParticipantRef,
				0,
				len(attendeeIDs),
			)
			for _, participantID := range attendeeIDs {
				conversationParticipants = append(
					conversationParticipants,
					store.ConversationParticipantRef{
						ParticipantID: participantID,
						Role:          "member",
					},
				)
			}

			return &store.MessagePersistData{
				Message: &store.Message{
					SourceID:                source.ID,
					SourceMessageID:         snapshot.SourceMessageID,
					MessageType:             MessageType,
					SentAt:                  sql.NullTime{Time: snapshot.StartedAt, Valid: true},
					SenderID:                sql.NullInt64{Int64: senderID, Valid: senderID != 0},
					IsFromMe:                expectedIsFromMe,
					IdentityDerivedIsFromMe: expectedIsFromMe,
					Subject:                 sql.NullString{String: snapshot.Title, Valid: snapshot.Title != ""},
					Snippet:                 sql.NullString{String: snapshot.Snippet, Valid: snapshot.Snippet != ""},
					SizeEstimate:            int64(len(snapshot.Body)),
				},
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
			}
		},
	)
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

	if err := i.store.UpdateSyncCheckpointContext(ctx, syncID, checkpoint); err != nil {
		return result, fmt.Errorf("checkpoint meeting import sync: %w", err)
	}
	if err := i.store.RecomputeConversationStatsForMessageContext(ctx, messageID); err != nil {
		return result, fmt.Errorf("recompute meeting conversation stats: %w", err)
	}
	if err := i.store.CompleteSyncContext(ctx, syncID, ""); err != nil {
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
