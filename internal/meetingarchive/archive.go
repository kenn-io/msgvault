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

	organizer, attendees := normalizedPeople(snapshot)
	organizerAddress := organizer.Email
	if organizerAddress == "" {
		organizerAddress = organizer.Phone
	}
	expectedIsFromMe, err := a.expectedIsFromMe(snapshot, organizer)
	if err != nil {
		return Result{}, err
	}

	people := append([]Person{organizer}, attendees...)
	participants := make([]store.ParticipantPersistData, 0, len(people))
	for _, person := range people {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if person.ParticipantID == 0 && person.PrimaryKey() != "" {
			participants = append(participants, persistData(person))
		}
	}
	attendeeAddresses := make([]string, 0, len(attendees))
	for _, attendee := range attendees {
		if attendee.Email != "" {
			attendeeAddresses = append(attendeeAddresses, attendee.Email)
		} else {
			attendeeAddresses = append(attendeeAddresses, attendee.Phone)
		}
	}

	if existed && !opts.Force {
		unchanged, err := a.storedMatches(ctx, snapshot, existingMessage, expectedIsFromMe, organizer, attendees)
		if err != nil {
			return Result{}, err
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
			people := slices.Clone(people)
			unresolved := 0
			for i := range people {
				if people[i].ParticipantID == 0 && people[i].PrimaryKey() != "" {
					people[i].ParticipantID = participantIDs[unresolved]
					unresolved++
				}
			}
			senderID := people[0].ParticipantID
			conversationParticipants := make([]store.ConversationParticipantRef, 0, len(attendees))
			for _, attendee := range people[1:] {
				conversationParticipants = append(conversationParticipants, store.ConversationParticipantRef{
					ParticipantID: attendee.ParticipantID,
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
				Recipients:     recipients(people[0], people[1:]),
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

// Unchanged reports whether Upsert without Force would keep the stored meeting snapshot.
// It only reads.
func (a *Archiver) Unchanged(ctx context.Context, snapshot Snapshot) (bool, error) {
	if a == nil || a.store == nil {
		return false, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	existing, err := a.store.MessageMetadataBatch(snapshot.SourceID, []string{snapshot.SourceMessageID})
	if err != nil {
		return false, fmt.Errorf("lookup existing meeting: %w", err)
	}
	existingMessage, existed := existing[snapshot.SourceMessageID]
	if !existed {
		return false, nil
	}
	organizer, attendees := normalizedPeople(snapshot)
	expectedIsFromMe, err := a.expectedIsFromMe(snapshot, organizer)
	if err != nil {
		return false, err
	}
	return a.storedMatches(ctx, snapshot, existingMessage, expectedIsFromMe, organizer, attendees)
}

func (a *Archiver) expectedIsFromMe(snapshot Snapshot, organizer Person) (bool, error) {
	identities, err := meetingidentity.ForSource(a.store, snapshot.SourceID, snapshot.AccountEmail)
	if err != nil {
		return false, err
	}
	organizerAddress := organizer.Email
	if organizerAddress == "" {
		organizerAddress = organizer.Phone
	}
	return organizerAddress != "" && identities.Contains(organizerAddress), nil
}

// storedMatches treats an unreadable stored copy as changed so Upsert repairs it.
func (a *Archiver) storedMatches(ctx context.Context, snapshot Snapshot, existing store.MessageMetadataRecord, expectedIsFromMe bool, organizer Person, attendees []Person) (bool, error) {
	storedRaw, rawErr := a.store.GetMessageRaw(existing.ID)
	match := rawErr == nil && bytes.Equal(storedRaw, snapshot.Raw) && JSONEvidenceEqual([]byte(existing.Metadata.String), snapshot.Metadata)
	if !match {
		return false, nil
	}
	if organizer.ParticipantID == 0 {
		storedIsFromMe, attributionErr := a.store.GetMessageIsFromMe(existing.ID)
		match = attributionErr == nil && storedIsFromMe == expectedIsFromMe
		if !match {
			return false, nil
		}
	}
	// Canonical address resolution is preserved when the source evidence matches.
	if organizer.ParticipantID == 0 && !slices.ContainsFunc(attendees, func(person Person) bool { return person.ParticipantID > 0 }) && (organizer.PrimaryKey() != "" || len(attendees) > 0) {
		return true, nil
	}
	if organizer.ParticipantID == 0 && organizer.PrimaryKey() != "" || slices.ContainsFunc(attendees, func(person Person) bool { return person.ParticipantID == 0 }) {
		return false, nil
	}
	match, err := a.store.MessageRecipientsMatchContext(ctx, existing.ID, recipients(organizer, attendees))
	if err != nil {
		return false, fmt.Errorf("compare meeting recipients: %w", err)
	}
	return match, nil
}

func recipients(organizer Person, attendees []Person) []store.RecipientSet {
	from := store.RecipientSet{Type: "from"}
	if organizer.ParticipantID > 0 || organizer.PrimaryKey() != "" {
		from.ParticipantIDs = []int64{organizer.ParticipantID}
		from.DisplayNames = []string{organizer.Name}
		from.EmailAddresses = []string{organizer.Email}
	}
	to := store.RecipientSet{Type: "to"}
	for _, attendee := range attendees {
		to.ParticipantIDs = append(to.ParticipantIDs, attendee.ParticipantID)
		to.DisplayNames = append(to.DisplayNames, attendee.Name)
		to.EmailAddresses = append(to.EmailAddresses, attendee.Email)
	}
	return []store.RecipientSet{from, to}
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

func snapshotPeople(snapshot Snapshot) []Person {
	people := make([]Person, 0, len(snapshot.Attendees)+1)
	if snapshot.Organizer != nil {
		people = append(people, *snapshot.Organizer)
	}
	return append(people, snapshot.Attendees...)
}
