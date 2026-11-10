package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/textutil"
)

const calendarExceptionKind = "exception"
const rawFormatICS = "ics"

// ICalendarImportSummary counts revision decisions, not generated occurrences.
type ICalendarImportSummary struct {
	Changed, Unchanged int
}

// icsInterpretationError distinguishes unreadable calendars from archive failures.
type icsInterpretationError struct{ err error }

func (e *icsInterpretationError) Error() string { return e.err.Error() }
func (e *icsInterpretationError) Unwrap() error { return e.err }

type preparedICS struct {
	component calevent.ICSComponent
	meta      calevent.ICSMetadata
	fromEmail bool
}

// PersistICalendarContext imports one complete file atomically under the same
// source lock as email/calendar ingestion. Invalid files leave no event writes.
func (s *Store) PersistICalendarContext(ctx context.Context, sourceID int64, raw []byte) (ICalendarImportSummary, error) {
	var summary ICalendarImportSummary
	if err := s.requireSyncSource(sourceID); err != nil {
		return summary, err
	}
	err := s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{sourceID}}, func(tx *loggedTx) error {
		prepared, err := s.prepareICalendarWith(ctx, tx, sourceID, raw, "", false)
		if err != nil {
			return err
		}
		return s.persistICalendarWith(ctx, tx, sourceID, prepared, &summary)
	})
	if err != nil {
		return ICalendarImportSummary{}, err
	}
	return summary, nil
}

func (s *Store) prepareICalendarWith(ctx context.Context, tx *loggedTx, sourceID int64, raw []byte, method string, fromEmail bool) ([]preparedICS, error) {
	var config sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT sync_config FROM sources WHERE id=?`, sourceID).Scan(&config); err != nil {
		return nil, fmt.Errorf("read ICS source: %w", err)
	}
	parse := calevent.ParseICS
	if fromEmail {
		parse = calevent.ParseEmailICS
	}
	components, err := parse(raw, method, calendarConfigZone(config))
	if err != nil {
		return nil, &icsInterpretationError{err}
	}
	out := make([]preparedICS, 0, len(components))
	for _, c := range components {
		out = append(out, preparedICS{component: c, fromEmail: fromEmail, meta: calevent.ICSMetadata{Sequence: c.Sequence, Stamp: c.Stamp, Status: c.Event.Status, AllDay: c.Event.AllDay, Organizer: c.OrganizerEmail}})
	}
	return out, nil
}

// The highest SEQUENCE wins, then the latest DTSTAMP; a cancellation wins a tie.
func icsRevisionNewer(c calevent.ICSComponent, old calevent.ICSMetadata) bool {
	if c.Sequence != old.Sequence {
		return c.Sequence > old.Sequence
	}
	if !c.Stamp.Equal(old.Stamp) {
		return c.Stamp.After(old.Stamp)
	}
	return c.Event.Status == SyncStatusCancelled && old.Status != SyncStatusCancelled
}

func (s *Store) persistICalendarWith(ctx context.Context, tx *loggedTx, sourceID int64, items []preparedICS, summary *ICalendarImportSummary) error {
	q := boundQuerier{ctx: ctx, q: tx}
	if len(items) > 0 {
		if err := s.lockParticipantDirectoryMutationTxContext(ctx, tx); err != nil {
			return fmt.Errorf("lock ICS participant directory: %w", err)
		}
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := item.component.Event
		sourceMessageID := e.ProviderEventID
		var id int64
		var metadata sql.NullString
		var format string
		err := q.QueryRow(`SELECT m.id,m.metadata,r.raw_format,m.source_message_id FROM messages m JOIN message_raw r ON r.message_id=m.id WHERE m.source_id=? AND m.id IN (SELECT id FROM messages WHERE source_id=? AND source_message_id=? UNION ALL SELECT message_id FROM source_merge_archive_only_messages WHERE original_source_message_id=?) ORDER BY CASE WHEN m.source_message_id=? THEN 0 ELSE 1 END, m.id LIMIT 1`, sourceID, sourceID, e.ProviderEventID, e.ProviderEventID, e.ProviderEventID).Scan(&id, &metadata, &format, &sourceMessageID)
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read prior ICS identity: %w", err)
		}
		var old calevent.ICSMetadata
		if exists {
			if format != rawFormatICS {
				return errors.New("calendar invitation identity conflicts with another raw format")
			}
			if err := json.Unmarshal([]byte(metadata.String), &old); err != nil {
				return fmt.Errorf("decode retained ICS metadata: %w", err)
			}
		}
		familyKey, familyOrganizer, err := s.icsFamily(ctx, tx, sourceID, id, calevent.ICSMessageID(item.component.UID, ""), item.fromEmail && old.Organizer == "")
		if err != nil {
			return err
		}
		organizer := old.Organizer
		if item.fromEmail && organizer == "" {
			organizer = familyOrganizer
		}
		foreign := item.fromEmail && organizer != "" && item.component.OrganizerEmail != organizer
		if foreign {
			slog.Warn("calendar invite ignored: organizer differs", "event_message_id", id, "organizer", item.component.OrganizerEmail)
		}
		apply := !foreign && (!exists || icsRevisionNewer(item.component, old))
		// Keep the newest available details beneath the winning cancellation.
		fill := false
		if !foreign && !apply && exists && old.Status == SyncStatusCancelled && e.Status != SyncStatusCancelled {
			var err error
			fill, err = icsDetailsNewer(q, id, item.component)
			if err != nil {
				return err
			}
		}
		if fill {
			item.meta.Sequence, item.meta.Stamp, item.meta.Status = old.Sequence, old.Stamp, SyncStatusCancelled
		}
		if apply && exists && e.Status == SyncStatusCancelled {
			// Like a provider cancellation, keep the accepted details.
			old.Sequence, old.Stamp, old.Status = item.meta.Sequence, item.meta.Stamp, SyncStatusCancelled
			if err := s.setICSMetadataWith(ctx, tx, id, old); err != nil {
				return err
			}
		} else if apply || fill {
			if err := s.persistICSDetailsWith(ctx, tx, sourceID, sourceMessageID, familyKey, item); err != nil {
				return err
			}
		}
		if apply || fill {
			if exists {
				if err := s.bumpDerivedDataRevisionContext(ctx, tx); err != nil {
					return err
				}
			}
			summary.Changed++
		} else {
			summary.Unchanged++
		}
	}
	return nil
}

func (s *Store) icsFamily(ctx context.Context, tx *loggedTx, sourceID, retainedID int64, familyKey string, needOrganizer bool) (string, string, error) {
	q := boundQuerier{ctx: ctx, q: tx}
	if err := q.QueryRow(`SELECT COALESCE((SELECT c.source_conversation_id FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.id=? AND m.source_id=?), (SELECT source_conversation_id FROM conversations WHERE source_id=? AND source_conversation_id=?), (SELECT c.source_conversation_id FROM source_merge_archive_only_messages a JOIN messages m ON m.id=a.message_id JOIN conversations c ON c.id=m.conversation_id LEFT JOIN calendar_events e ON e.message_id=m.id WHERE m.source_id=? AND (a.original_source_message_id=? OR e.series_provider_id=?) ORDER BY m.id LIMIT 1), ?)`, retainedID, sourceID, sourceID, familyKey, sourceID, familyKey, familyKey, familyKey).Scan(&familyKey); err != nil {
		return "", "", fmt.Errorf("resolve ICS family conversation: %w", err)
	}
	if !needOrganizer {
		return familyKey, "", nil
	}
	field := "json_extract(m.metadata, '$.organizer')"
	if s.IsPostgreSQL() {
		field = "m.metadata ->> 'organizer'"
	}
	var organizer string
	err := q.QueryRow(`SELECT `+field+` FROM messages m JOIN conversations c ON c.id=m.conversation_id JOIN message_raw r ON r.message_id=m.id WHERE m.source_id=? AND c.source_conversation_id=? AND r.raw_format=? AND `+field+` <> '' ORDER BY m.id LIMIT 1`, sourceID, familyKey, rawFormatICS).Scan(&organizer)
	if errors.Is(err, sql.ErrNoRows) {
		return familyKey, "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read ICS family organizer: %w", err)
	}
	return familyKey, organizer, nil
}

func icsDetailsNewer(q boundQuerier, id int64, incoming calevent.ICSComponent) (bool, error) {
	var stored []byte
	var compression sql.NullString
	if err := q.QueryRow(`SELECT raw_data,compression FROM message_raw WHERE message_id=?`, id).Scan(&stored, &compression); err != nil {
		return false, fmt.Errorf("read retained ICS details: %w", err)
	}
	raw, _, err := decodeMessageRawBounded(stored, compression, maxMeetingRawBytes, errMeetingRawTooLarge)
	if err != nil {
		return false, fmt.Errorf("decode retained ICS details: %w", err)
	}
	components, err := calevent.ParseICS(raw, "CANCEL", "")
	if err != nil {
		return false, fmt.Errorf("parse retained ICS details: %w", err)
	}
	if len(components) != 1 {
		return false, errors.New("retained ICS requires one event")
	}
	retained := components[0]
	return retained.Event.Start == nil || icsRevisionNewer(incoming, calevent.ICSMetadata{Sequence: retained.Sequence, Stamp: retained.Stamp, Status: SyncStatusCancelled}), nil
}

func (s *Store) setICSMetadataWith(ctx context.Context, tx *loggedTx, id int64, meta calevent.ICSMetadata) error {
	encoded, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := setMessageMetadataWith(boundQuerier{ctx: ctx, q: tx}, s.dialect, id, sql.NullString{String: string(encoded), Valid: true}); err != nil {
		return err
	}
	return s.refreshCalendarEventProjectionWith(ctx, tx, id)
}

func icsEventSentAt(e calevent.Event) (sql.NullTime, error) {
	date := ""
	switch {
	case e.AllDay && e.StartDate != "":
		date = e.StartDate
	case e.Start != nil:
		return sql.NullTime{Time: *e.Start, Valid: true}, nil
	case e.Original != nil:
		if !e.Original.AllDay {
			return sql.NullTime{Time: e.Original.Instant, Valid: true}, nil
		}
		date = e.Original.Date
	}
	if date == "" {
		return sql.NullTime{}, nil
	}
	sent, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return sql.NullTime{}, fmt.Errorf("parse ICS message date: %w", err)
	}
	return sql.NullTime{Time: sent, Valid: true}, nil
}

func (s *Store) persistICSDetailsWith(ctx context.Context, tx *loggedTx, sourceID int64, sourceMessageID, familyKey string, item preparedICS) error {
	c := item.component
	e := c.Event
	item.meta.ICalUID = c.UID
	item.meta.TimeZone = e.EventTimeZone
	item.meta.Start, item.meta.End = e.StartDate, e.EndDate
	if !e.AllDay {
		if e.Start != nil {
			item.meta.Start = e.Start.Format(time.RFC3339)
		}
		if e.End != nil {
			item.meta.End = e.End.Format(time.RFC3339)
		}
	}
	encoded, err := json.Marshal(item.meta)
	if err != nil {
		return err
	}
	body := c.Description
	if c.Location != "" {
		body = strings.TrimSpace(body + "\nLocation: " + c.Location)
	}
	participants := make([]ParticipantPersistData, 0, len(e.Attendees))
	organizer := -1
	for _, attendee := range e.Attendees {
		if attendee.Email == "" || attendee.Role == "room" {
			continue
		}
		if attendee.Role == "organizer" {
			organizer = len(participants)
		}
		_, domain, _ := strings.Cut(attendee.Email, "@")
		participants = append(participants, ParticipantPersistData{EmailAddress: attendee.Email, DisplayName: attendee.DisplayName, Domain: domain})
	}
	sent, err := icsEventSentAt(e)
	if err != nil {
		return err
	}
	title := c.Summary
	if e.Kind == calendarExceptionKind {
		title = ""
	}
	id, err := s.persistMessageWithParticipantsTx(ctx, tx, nil, participants, func(ids []int64) *MessagePersistData {
		from, to := RecipientSet{Type: meetingSenderRole}, RecipientSet{Type: "to"}
		var sender sql.NullInt64
		var emails []string
		for i, p := range participants {
			target := &to
			if i == organizer {
				target = &from
				sender = sql.NullInt64{Int64: ids[i], Valid: true}
			} else {
				emails = append(emails, p.EmailAddress)
			}
			target.ParticipantIDs = append(target.ParticipantIDs, ids[i])
			target.DisplayNames = append(target.DisplayNames, p.DisplayName)
			target.EmailAddresses = append(target.EmailAddresses, p.EmailAddress)
		}
		return &MessagePersistData{
			Message:      &Message{SourceID: sourceID, SourceMessageID: sourceMessageID, MessageType: "calendar_event", Subject: sql.NullString{String: c.Summary, Valid: c.Summary != ""}, SentAt: sent, SenderID: sender, Snippet: sql.NullString{String: textutil.SnippetFromBody(body), Valid: body != ""}, SizeEstimate: int64(len(c.Raw))},
			Conversation: &ConversationPersistData{SourceConversationID: familyKey, ConversationType: "calendar", Title: title},
			BodyText:     sql.NullString{String: body, Valid: body != ""}, RawMIME: c.Raw, RawFormat: rawFormatICS, Metadata: &sql.NullString{String: string(encoded), Valid: true}, PreserveLabels: true,
			Recipients: []RecipientSet{from, to}, FTS: &FTSDoc{Subject: c.Summary, Body: body, FromAddr: c.OrganizerEmail, ToAddrs: strings.Join(emails, " ")},
		}
	}, nil, nil)
	if err != nil {
		return fmt.Errorf("persist ICS event: %w", err)
	}
	q := boundQuerier{ctx: ctx, q: tx}
	if item.fromEmail {
		if _, err := q.Exec(`INSERT INTO source_merge_archive_only_messages (message_id) VALUES (?) ON CONFLICT DO NOTHING`, id); err != nil {
			return fmt.Errorf("mark email-derived calendar event: %w", err)
		}
	}
	return s.recomputeConversationStatsWith(q, "id = (SELECT conversation_id FROM messages WHERE id = ?)", id)
}
