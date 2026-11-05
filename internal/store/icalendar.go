package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
)

const calendarExceptionKind = "exception"
const rawFormatICS = "ics"

// ICalendarImportSummary counts revision decisions, not generated occurrences.
type ICalendarImportSummary struct {
	Changed, Unchanged int
	Diagnostics        []string
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
	components, err := calevent.ParseICS(raw, method, calendarConfigZone(config))
	if err != nil {
		return nil, &icsInterpretationError{err}
	}
	out := make([]preparedICS, 0, len(components))
	for _, c := range components {
		out = append(out, preparedICS{component: c, fromEmail: fromEmail, meta: calevent.ICSMetadata{Sequence: c.Sequence, Stamp: c.Stamp, Status: c.Event.Status, AllDay: c.Event.AllDay, InviteOrganizer: c.OrganizerEmail}})
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
	items, err := acceptedICSSnapshot(q, sourceID, items, summary)
	if err != nil {
		return err
	}
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
		var id int64
		var metadata sql.NullString
		var format string
		err := q.QueryRow(`SELECT m.id,m.metadata,r.raw_format FROM messages m JOIN message_raw r ON r.message_id=m.id WHERE m.source_id=? AND m.source_message_id=?`, sourceID, e.ProviderEventID).Scan(&id, &metadata, &format)
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
		apply := !exists || icsRevisionNewer(item.component, old)
		if !apply && old.Status == SyncStatusCancelled && e.Status != SyncStatusCancelled {
			// A cancellation that arrived first keeps its revision; an earlier
			// invite only supplies the details it lacked.
			if apply, err = icsTombstoneLacksBounds(ctx, tx, id); err != nil {
				return err
			}
			old.InviteOrganizer = item.meta.InviteOrganizer
			item.meta = old
			item.meta.AllDay = e.AllDay
		}
		if apply && exists && e.Status == SyncStatusCancelled {
			// Like a provider cancellation, keep the accepted details.
			old.Sequence, old.Stamp, old.Status = item.meta.Sequence, item.meta.Stamp, SyncStatusCancelled
			old.InviteOrganizer = item.meta.InviteOrganizer
			if err := s.setICSMetadataWith(ctx, tx, id, old); err != nil {
				return err
			}
		} else if apply {
			if err := s.persistICSDetailsWith(ctx, tx, sourceID, item); err != nil {
				return err
			}
			if exists {
				if err := s.bumpDerivedDataRevisionContext(ctx, tx); err != nil {
					return err
				}
			}
		}
		if apply {
			summary.Changed++
		} else {
			summary.Unchanged++
		}
	}
	return nil
}

func icsTombstoneLacksBounds(ctx context.Context, tx *loggedTx, id int64) (bool, error) {
	var start sql.NullTime
	var startDate string
	err := tx.QueryRowContext(ctx, `SELECT start_at, start_date FROM calendar_events WHERE message_id=?`, id).Scan(&start, &startDate)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read ICS tombstone bounds: %w", err)
	}
	return !start.Valid && startDate == "", nil
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

func (s *Store) persistICSDetailsWith(ctx context.Context, tx *loggedTx, sourceID int64, item preparedICS) error {
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
		if attendee.Email == "" {
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
	familyKey := calevent.ICSMessageID(c.UID, "")
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
			Message:      &Message{SourceID: sourceID, SourceMessageID: e.ProviderEventID, MessageType: "calendar_event", Subject: sql.NullString{String: c.Summary, Valid: c.Summary != ""}, SentAt: sent, SenderID: sender, Snippet: sql.NullString{String: body, Valid: body != ""}, SizeEstimate: int64(len(c.Raw))},
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
