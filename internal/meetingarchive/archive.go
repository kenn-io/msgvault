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

// Person is one meeting organizer or attendee. Email, then Phone, is the
// identity recorded as the meeting recipient. OtherEmails and OtherPhones are
// further identities of the same human; with an Anchor they are linked to the
// recipient identity through LinkIdentities. Names never match anything.
type Person struct {
	Name        string
	Email       string
	Phone       string // E.164
	OtherEmails []string
	OtherPhones []string
	// Anchor is a stable, provider-scoped identifier for this human, built
	// with Anchor(). Empty means the provider asserts no stable identity.
	Anchor string
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
	// a later conversation-stat maintenance or identity-linking error.
	Changed bool
	// Links reports anchored attendee identity linking, which runs on every
	// Upsert so an interrupted link is repaired by the next sync.
	Links LinkResult
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
	var organizer Person
	if snapshot.Organizer != nil {
		organizer = snapshot.Organizer.Normalized()
	}
	organizerEmail, organizerName := organizer.Email, organizer.Name
	organizerAddress := organizerEmail
	if organizerAddress == "" {
		organizerAddress = organizer.Phone
	}
	expectedIsFromMe := organizerAddress != "" && identities.Contains(organizerAddress)

	if existed && !opts.Force {
		storedRaw, rawErr := a.store.GetMessageRaw(existingMessageID)
		storedIsFromMe, attributionErr := a.store.GetMessageIsFromMe(existingMessageID)
		if rawErr == nil && attributionErr == nil && bytes.Equal(storedRaw, snapshot.Raw) && storedIsFromMe == expectedIsFromMe {
			if err := a.store.RecomputeConversationStatsForMessageContext(ctx, existingMessageID); err != nil {
				return Result{}, fmt.Errorf("recompute meeting conversation stats: %w", err)
			}
			result := Result{MessageID: existingMessageID}
			result.Links, err = a.LinkIdentities(ctx, snapshot.SourceID, snapshotPeople(snapshot))
			if err != nil {
				return result, fmt.Errorf("link meeting attendee identities: %w", err)
			}
			return result, nil
		}
	}

	participants := make([]store.ParticipantPersistData, 0, len(snapshot.Attendees)+1)
	hasOrganizer := organizer.PrimaryKey() != ""
	if hasOrganizer {
		participants = append(participants, persistData(organizer))
	}

	attendeeNames := make([]string, 0, len(snapshot.Attendees))
	attendeeEmails := make([]string, 0, len(snapshot.Attendees))
	attendeeAddresses := make([]string, 0, len(snapshot.Attendees))
	seenAttendees := make(map[string]bool, len(snapshot.Attendees))
	for _, raw := range snapshot.Attendees {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		attendee := raw.Normalized()
		key := attendee.PrimaryKey()
		if key == "" || seenAttendees[key] {
			continue
		}
		seenAttendees[key] = true
		participants = append(participants, persistData(attendee))
		attendeeNames = append(attendeeNames, attendee.Name)
		attendeeEmails = append(attendeeEmails, attendee.Email)
		if attendee.Email != "" {
			attendeeAddresses = append(attendeeAddresses, attendee.Email)
		} else {
			attendeeAddresses = append(attendeeAddresses, attendee.Phone)
		}
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
					FromAddr: organizerAddress,
					ToAddrs:  strings.Join(attendeeAddresses, " "),
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
	result.Links, err = a.LinkIdentities(ctx, snapshot.SourceID, snapshotPeople(snapshot))
	if err != nil {
		return result, fmt.Errorf("link meeting attendee identities: %w", err)
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

func persistData(person Person) store.ParticipantPersistData {
	if person.Email != "" {
		return store.ParticipantPersistData{
			EmailAddress: person.Email,
			DisplayName:  person.Name,
			Domain:       emailDomain(person.Email),
		}
	}
	return store.ParticipantPersistData{PhoneNumber: person.Phone, DisplayName: person.Name}
}

func snapshotPeople(snapshot Snapshot) []Person {
	people := make([]Person, 0, len(snapshot.Attendees)+1)
	if snapshot.Organizer != nil {
		people = append(people, *snapshot.Organizer)
	}
	return append(people, snapshot.Attendees...)
}
