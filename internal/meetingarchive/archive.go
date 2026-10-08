// Package meetingarchive persists provider-normalized meetings through one
// canonical store path. Providers retain ownership of discovery, sync state,
// lifecycle handling, and raw-evidence construction.
package meetingarchive

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"
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
	Name  string
	Email string
	Phone string // E.164
	// ParticipantID references an existing participant resolved by the provider.
	// Zero retains the canonical email/phone resolution path.
	ParticipantID int64
	OtherEmails   []string
	OtherPhones   []string
	// Anchor is a stable, provider-scoped identifier for this human, built
	// with Anchor(). Empty means the provider asserts no stable identity.
	Anchor string
	// LinkExcludedAddresses are normalized addresses kept as meeting evidence
	// but excluded from this anchor's automatic ownership assertions.
	LinkExcludedAddresses []string
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
	if snapshot.Organizer != nil && snapshot.Organizer.ParticipantID < 0 {
		return Result{}, errors.New("meeting organizer participant ID cannot be negative")
	}
	for _, attendee := range snapshot.Attendees {
		if attendee.ParticipantID < 0 {
			return Result{}, errors.New("meeting attendee participant ID cannot be negative")
		}
	}

	existing, err := a.store.MessageMetadataBatch(snapshot.SourceID, []string{snapshot.SourceMessageID})
	if err != nil {
		return Result{}, fmt.Errorf("lookup existing meeting: %w", err)
	}
	existingMessage, existed := existing[snapshot.SourceMessageID]
	existingMessageID := existingMessage.ID

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

	participants := make([]store.ParticipantPersistData, 0, len(snapshot.Attendees)+1)
	hasOrganizer := organizer.ParticipantID > 0 || organizer.PrimaryKey() != ""
	organizerOffset := -1
	if hasOrganizer && organizer.ParticipantID == 0 {
		organizerOffset = len(participants)
		participants = append(participants, persistData(organizer))
	}

	attendeeNames := make([]string, 0, len(snapshot.Attendees))
	attendeeEmails := make([]string, 0, len(snapshot.Attendees))
	attendeeAddresses := make([]string, 0, len(snapshot.Attendees))
	attendeeOffsets := make([]int, 0, len(snapshot.Attendees))
	resolvedAttendeeIDs := make([]int64, 0, len(snapshot.Attendees))
	seenAttendees := make(map[string]bool, len(snapshot.Attendees))
	for _, raw := range snapshot.Attendees {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		attendee := raw.Normalized()
		key := archiveParticipantKey(attendee)
		if key == "" || seenAttendees[key] {
			continue
		}
		seenAttendees[key] = true
		offset := -1
		if attendee.ParticipantID == 0 {
			offset = len(participants)
			participants = append(participants, persistData(attendee))
		}
		attendeeOffsets = append(attendeeOffsets, offset)
		resolvedAttendeeIDs = append(resolvedAttendeeIDs, attendee.ParticipantID)
		attendeeNames = append(attendeeNames, attendee.Name)
		attendeeEmails = append(attendeeEmails, attendee.Email)
		if attendee.Email != "" {
			attendeeAddresses = append(attendeeAddresses, attendee.Email)
		} else {
			attendeeAddresses = append(attendeeAddresses, attendee.Phone)
		}
	}

	recipients := func(senderID int64, attendeeIDs []int64) []store.RecipientSet {
		from := store.RecipientSet{Type: "from"}
		if hasOrganizer {
			from.ParticipantIDs = []int64{senderID}
			from.DisplayNames = []string{organizerName}
			from.EmailAddresses = []string{organizerEmail}
		}
		return []store.RecipientSet{from, {Type: "to", ParticipantIDs: attendeeIDs, DisplayNames: attendeeNames, EmailAddresses: attendeeEmails}}
	}
	resolvedPeople := organizer.ParticipantID > 0 || slices.ContainsFunc(resolvedAttendeeIDs, func(id int64) bool { return id > 0 })

	if existed && !opts.Force {
		storedRaw, rawErr := a.store.GetMessageRaw(existingMessageID)
		storedIsFromMe, attributionErr := a.store.GetMessageIsFromMe(existingMessageID)
		if organizer.ParticipantID > 0 {
			// Store synchronously repairs ownership when identities or participants change.
			expectedIsFromMe = storedIsFromMe
		}
		unchanged := rawErr == nil && attributionErr == nil && bytes.Equal(storedRaw, snapshot.Raw) &&
			storedIsFromMe == expectedIsFromMe && JSONEvidenceEqual([]byte(existingMessage.Metadata.String), snapshot.Metadata)
		if unchanged && resolvedPeople && len(participants) > 0 {
			unchanged = false
		}
		if unchanged {
			sets := recipients(organizer.ParticipantID, resolvedAttendeeIDs)
			if organizerOffset >= 0 {
				sets = sets[1:]
			}
			if slices.ContainsFunc(attendeeOffsets, func(offset int) bool { return offset >= 0 }) {
				sets = slices.DeleteFunc(sets, func(set store.RecipientSet) bool { return set.Type == "to" })
			}
			if len(sets) > 0 {
				unchanged, err = a.store.MessageRecipientsMatchContext(ctx, existingMessageID, sets)
				if err != nil {
					return Result{}, fmt.Errorf("compare meeting recipients: %w", err)
				}
			}
			if unchanged && organizerOffset < 0 {
				stored, err := a.store.StoredMessagesContext(ctx, snapshot.SourceID, snapshot.RawFormat, []string{snapshot.SourceMessageID})
				if err != nil {
					return Result{}, err
				}
				unchanged = stored[snapshot.SourceMessageID].SenderID == (sql.NullInt64{Int64: organizer.ParticipantID, Valid: organizer.ParticipantID > 0})
			}
		}
		if unchanged {
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

	conversationID := strings.TrimSpace(snapshot.SourceConversationID)
	if conversationID == "" {
		conversationID = snapshot.SourceMessageID
	}
	metadata := sql.NullString{String: string(snapshot.Metadata), Valid: len(snapshot.Metadata) > 0}
	messageID, err := a.store.PersistMessageWithParticipantsContext(
		ctx,
		participants,
		func(participantIDs []int64) *store.MessagePersistData {
			var senderID int64
			if hasOrganizer {
				senderID = organizer.ParticipantID
				if organizerOffset >= 0 {
					senderID = participantIDs[organizerOffset]
				}
			}
			attendeeIDs := append([]int64{}, resolvedAttendeeIDs...)
			for index, offset := range attendeeOffsets {
				if offset >= 0 {
					attendeeIDs[index] = participantIDs[offset]
				}
			}
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
					PreserveAttachmentStats: true,
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
				Metadata:       &metadata,
				BodyText:       sql.NullString{String: snapshot.Body, Valid: snapshot.Body != ""},
				RawMIME:        snapshot.Raw,
				RawFormat:      snapshot.RawFormat,
				Recipients:     recipients(senderID, attendeeIDs),
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

// JSONEvidenceEqual compares JSON evidence while ignoring object order and whitespace.
func JSONEvidenceEqual(stored, incoming []byte) bool {
	if bytes.Equal(stored, incoming) {
		return true
	}
	// PostgreSQL JSONB changes object key order and whitespace on storage.
	left, right := jsontext.Value(stored).Clone(), jsontext.Value(incoming).Clone()
	return left.Canonicalize() == nil && right.Canonicalize() == nil && bytes.Equal(left, right)
}

func emailDomain(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}

// FormatTranscriptLine renders "[mm:ss] Speaker: text", or "[h:mm:ss]" past
// the first hour. Callers pick their own label for an unnamed speaker.
func FormatTranscriptLine(offset time.Duration, speaker, text string) string {
	if offset < 0 {
		offset = 0
	}
	total := int(offset.Seconds())
	h, m, s := total/3600, (total%3600)/60, total%60
	stamp := fmt.Sprintf("[%02d:%02d]", m, s)
	if h > 0 {
		stamp = fmt.Sprintf("[%d:%02d:%02d]", h, m, s)
	}
	return stamp + " " + speaker + ": " + text
}

// Snippet is the preview stored with a meeting: the trimmed body cut to 200 runes.
func Snippet(body string) string {
	runes := []rune(strings.TrimSpace(body))
	if len(runes) > 200 {
		runes = runes[:200]
	}
	return string(runes)
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

// archiveParticipantKey keeps explicitly resolved people distinct even when
// they share an address. Canonical providers retain primary-address deduping.
func archiveParticipantKey(person Person) string {
	if person.ParticipantID > 0 {
		return fmt.Sprintf("participant:%d", person.ParticipantID)
	}
	return person.PrimaryKey()
}
