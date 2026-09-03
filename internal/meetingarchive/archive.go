// Package meetingarchive persists provider-normalized meetings through one
// canonical store path. Providers retain ownership of discovery, sync state,
// lifecycle handling, and raw-evidence construction.
package meetingarchive

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingidentity"
	"go.kenn.io/msgvault/internal/store"
)

const (
	ConversationType = "meeting"
	MessageType      = "meeting_transcript"
)

var ErrUnavailable = errors.New("meeting archiver is unavailable")

type Person struct {
	Name  string
	Email string
}

type Snapshot struct {
	SourceID             int64
	AccountEmail         string
	SourceMessageID      string
	SourceConversationID string
	Title                string
	StartedAt            time.Time
	Body                 string
	Snippet              string
	Metadata             []byte
	Raw                  []byte
	RawFormat            string
	Organizer            *Person
	Attendees            []Person
}

type Result struct {
	MessageID int64
	Created   bool
	// Changed means the archive write committed, including when Upsert returns
	// a later conversation-stat maintenance error.
	Changed bool
}

// UpsertOptions controls archive repair behavior.
type UpsertOptions struct {
	// Force rewrites every derived projection even when the stored raw snapshot
	// and sender attribution already match.
	Force bool
}

type Archiver struct {
	store *store.Store
}

func New(s *store.Store) *Archiver {
	return &Archiver{store: s}
}

func (a *Archiver) Upsert(
	ctx context.Context,
	snapshot Snapshot,
	opts UpsertOptions,
) (Result, error) {
	if a == nil || a.store == nil {
		return Result{}, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	existing, err := a.store.MessageExistsBatch(snapshot.SourceID, []string{snapshot.SourceMessageID})
	if err != nil {
		return Result{}, fmt.Errorf("lookup existing meeting: %w", err)
	}
	existingMessageID, existed := existing[snapshot.SourceMessageID]

	identities, err := meetingidentity.ForSource(a.store, snapshot.SourceID, snapshot.AccountEmail)
	if err != nil {
		return Result{}, err
	}
	organizerEmail, organizerName := "", ""
	if snapshot.Organizer != nil {
		organizerEmail = normalizeEmail(snapshot.Organizer.Email)
		organizerName = strings.TrimSpace(snapshot.Organizer.Name)
	}
	expectedIsFromMe := organizerEmail != "" && identities.Contains(organizerEmail)

	if existed && !opts.Force {
		storedRaw, rawErr := a.store.GetMessageRaw(existingMessageID)
		storedIsFromMe, attributionErr := a.store.GetMessageIsFromMe(existingMessageID)
		if rawErr == nil && attributionErr == nil && bytes.Equal(storedRaw, snapshot.Raw) && storedIsFromMe == expectedIsFromMe {
			if err := a.store.RecomputeConversationStatsForMessageContext(ctx, existingMessageID); err != nil {
				return Result{}, fmt.Errorf("recompute meeting conversation stats: %w", err)
			}
			return Result{MessageID: existingMessageID}, nil
		}
	}

	participants := make([]store.ParticipantPersistData, 0, len(snapshot.Attendees)+1)
	hasOrganizer := organizerEmail != ""
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
			return Result{}, err
		}
		email := normalizeEmail(attendee.Email)
		if email == "" {
			continue
		}
		name := strings.TrimSpace(attendee.Name)
		participants = append(participants, store.ParticipantPersistData{
			EmailAddress: email,
			DisplayName:  name,
			Domain:       emailDomain(email),
		})
		attendeeNames = append(attendeeNames, name)
		attendeeEmails = append(attendeeEmails, email)
	}

	conversationID := strings.TrimSpace(snapshot.SourceConversationID)
	if conversationID == "" {
		conversationID = snapshot.SourceMessageID
	}
	metadata := sql.NullString{String: string(snapshot.Metadata), Valid: len(snapshot.Metadata) > 0}
	messageID, err := a.store.PersistMessageWithParticipantsContext(
		ctx,
		participants,
		func(participantIDs []int64) *store.MessagePersistData {
			attendeeOffset := 0
			var senderID int64
			var fromIDs []int64
			var fromNames []string
			var fromEmails []string
			if hasOrganizer {
				senderID = participantIDs[0]
				attendeeOffset = 1
				fromIDs = []int64{senderID}
				fromNames = []string{organizerName}
				fromEmails = []string{organizerEmail}
			}
			attendeeIDs := participantIDs[attendeeOffset:]
			conversationParticipants := make([]store.ConversationParticipantRef, 0, len(attendeeIDs))
			for _, participantID := range attendeeIDs {
				conversationParticipants = append(conversationParticipants, store.ConversationParticipantRef{
					ParticipantID: participantID,
					Role:          "member",
				})
			}

			return &store.MessagePersistData{
				Message: &store.Message{
					SourceID:                snapshot.SourceID,
					SourceMessageID:         snapshot.SourceMessageID,
					MessageType:             MessageType,
					SentAt:                  sql.NullTime{Time: snapshot.StartedAt, Valid: !snapshot.StartedAt.IsZero()},
					SenderID:                sql.NullInt64{Int64: senderID, Valid: senderID != 0},
					IsFromMe:                expectedIsFromMe,
					IdentityDerivedIsFromMe: expectedIsFromMe,
					Subject:                 sql.NullString{String: snapshot.Title, Valid: snapshot.Title != ""},
					Snippet:                 sql.NullString{String: snapshot.Snippet, Valid: snapshot.Snippet != ""},
					SizeEstimate:            int64(len(snapshot.Body)),
				},
				Conversation: &store.ConversationPersistData{
					SourceConversationID: conversationID,
					ConversationType:     ConversationType,
					Title:                snapshot.Title,
					Participants:         conversationParticipants,
				},
				Metadata:  &metadata,
				BodyText:  sql.NullString{String: snapshot.Body, Valid: snapshot.Body != ""},
				RawMIME:   snapshot.Raw,
				RawFormat: snapshot.RawFormat,
				Recipients: []store.RecipientSet{
					{
						Type:           "from",
						ParticipantIDs: fromIDs,
						DisplayNames:   fromNames,
						EmailAddresses: fromEmails,
					},
					{
						Type:           "to",
						ParticipantIDs: attendeeIDs,
						DisplayNames:   attendeeNames,
						EmailAddresses: attendeeEmails,
					},
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
		return Result{}, fmt.Errorf("persist meeting: %w", err)
	}
	result := Result{MessageID: messageID, Created: !existed, Changed: true}
	if err := a.store.RecomputeConversationStatsForMessageContext(ctx, messageID); err != nil {
		return result, fmt.Errorf("recompute meeting conversation stats: %w", err)
	}
	return result, nil
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func emailDomain(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}
