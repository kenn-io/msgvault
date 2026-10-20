package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
)

const calendarExceptionKind = "exception"
const rawFormatICS = "ics"

// ICalendarImportSummary counts revision decisions, not generated occurrences.
type ICalendarImportSummary struct {
	SourceID                         int64
	MessageIDs                       []int64
	Changed, Unchanged, Unexpandable int
	Diagnostics                      []string
}

// icsInterpretationError distinguishes coverage failures from archive failures.
type icsInterpretationError struct{ err error }

func (e *icsInterpretationError) Error() string { return e.err.Error() }
func (e *icsInterpretationError) Unwrap() error { return e.err }

func parseRetainedICSMetadata(metadata sql.NullString, kind string) (calevent.ICSMetadata, error) {
	meta, err := calevent.ParseICSMetadata([]byte(metadata.String))
	if err != nil {
		return meta, &icsInterpretationError{fmt.Errorf("decode retained ICS %s metadata: %w", kind, err)}
	}
	return meta, nil
}

type preparedICS struct {
	component calevent.ICSComponent
	raw       []byte
	meta      calevent.ICSMetadata
	fromEmail bool
}

// PersistICalendarContext imports one complete file atomically under the same
// source lock as email/calendar ingestion. Invalid files leave no event writes.
func (s *Store) PersistICalendarContext(ctx context.Context, sourceID int64, raw []byte) (ICalendarImportSummary, error) {
	summary := ICalendarImportSummary{SourceID: sourceID}
	if err := s.requireSyncSource(sourceID); err != nil {
		return summary, err
	}
	err := s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{sourceID}}, func(tx *loggedTx) error {
		budget := calevent.NewICSParseBudget()
		tx.icsVerificationRemaining = budget.VerificationRemaining
		prepared, diagnostics, err := s.prepareICalendarWith(ctx, tx, sourceID, raw, "", false, budget)
		if err != nil {
			return err
		}
		summary.Diagnostics = append(summary.Diagnostics, diagnostics...)
		return s.persistICalendarWith(ctx, tx, sourceID, prepared, &summary)
	})
	if err != nil {
		return ICalendarImportSummary{}, err
	}
	return summary, nil
}

func (s *Store) prepareICalendarWith(ctx context.Context, tx *loggedTx, sourceID int64, raw []byte, method string, fromEmail bool, budget *calevent.ICSParseBudget) ([]preparedICS, []string, error) {
	q := boundQuerier{ctx: ctx, q: tx}
	var config sql.NullString
	if err := q.QueryRow(`SELECT sync_config FROM sources WHERE id=?`, sourceID).Scan(&config); err != nil {
		return nil, nil, fmt.Errorf("read ICS source: %w", err)
	}
	components, diagnostics, err := calevent.ParseICSWithDiagnostics(ctx, raw, method, calendarConfigZone(config), budget)
	if err != nil {
		return nil, nil, &icsInterpretationError{err}
	}
	out := make([]preparedICS, 0, len(components))
	for _, c := range components {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		out = append(out, preparedICS{component: c, raw: c.Raw, fromEmail: fromEmail, meta: calevent.ICSMetadata{Version: 1, Sequence: c.Sequence, Stamp: c.Stamp, Status: c.Event.Status, AllDay: c.Event.AllDay, InviteOrganizer: c.OrganizerEmail}})
	}
	return out, diagnostics, nil
}

func icsRevisionNewer(c calevent.ICSComponent, old calevent.ICSMetadata) bool {
	if c.Sequence != old.Sequence {
		return c.Sequence > old.Sequence
	}
	if !c.Stamp.Equal(old.Stamp) {
		return c.Stamp.After(old.Stamp)
	}
	return c.Event.Status == SyncStatusCancelled && old.Status != SyncStatusCancelled
}

func icsRevisionOrderNewer(sequence int64, stamp time.Time, oldSequence int64, oldStamp time.Time) bool {
	return sequence > oldSequence || sequence == oldSequence && stamp.After(oldStamp)
}

type icsFamilyContinuity struct {
	existed   bool
	organizer string
}

func readICSFamilyContinuity(q querier, sourceID int64, key, organizer string) (icsFamilyContinuity, error) {
	var metadata sql.NullString
	err := q.QueryRow(`SELECT m.metadata FROM conversations c JOIN messages m ON m.conversation_id=c.id WHERE c.source_id=? AND c.source_conversation_id=? AND m.message_type='calendar_event' ORDER BY m.id LIMIT 1`, sourceID, key).Scan(&metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return icsFamilyContinuity{organizer: organizer}, nil
	}
	if err != nil {
		return icsFamilyContinuity{}, fmt.Errorf("read ICS family organizer: %w", err)
	}
	family, err := parseRetainedICSMetadata(metadata, "family")
	if err != nil {
		return icsFamilyContinuity{}, err
	}
	return icsFamilyContinuity{existed: true, organizer: family.InviteOrganizer}, nil
}

func (family icsFamilyContinuity) allows(c calevent.ICSComponent) bool {
	// Within a creating snapshot, siblings may share an omitted organizer.
	// Later emails require a nonempty match. Addresses remain unauthenticated:
	// a stranger who copies the ORGANIZER can update or cancel the derived event.
	// This is an accepted design choice; the original emails stay as evidence.
	if family.existed || c.Event.Status == SyncStatusCancelled {
		return c.OrganizerEmail != "" && family.organizer != "" && strings.EqualFold(c.OrganizerEmail, family.organizer)
	}
	return strings.EqualFold(c.OrganizerEmail, family.organizer)
}

func (s *Store) refreshRetainedICSMessageTimeWith(ctx context.Context, tx *loggedTx, id int64) error {
	event, ok, err := readICSCalendarEvent(ctx, tx, id)
	if err != nil || !ok {
		return err
	}
	return s.refreshICSMessageTimeWith(boundQuerier{ctx: ctx, q: tx}, id, event)
}

func (s *Store) persistICalendarWith(ctx context.Context, tx *loggedTx, sourceID int64, items []preparedICS, summary *ICalendarImportSummary) error {
	q := boundQuerier{ctx: ctx, q: tx}
	icsMastersFirst(items)
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
		c := item.component
		e := c.Event
		key := e.ProviderEventID
		familyKey := calevent.ICSMessageID(c.UID, "")
		var id int64
		var metadata sql.NullString
		var format string
		err := q.QueryRow(`SELECT m.id,m.metadata,r.raw_format FROM messages m JOIN message_raw r ON r.message_id=m.id WHERE m.source_id=? AND m.source_message_id=?`, sourceID, key).Scan(&id, &metadata, &format)
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read prior ICS identity: %w", err)
		}
		var old calevent.ICSMetadata
		if exists {
			if format != rawFormatICS {
				return errors.New("calendar invitation identity conflicts with another raw format")
			}
			old, err = parseRetainedICSMetadata(metadata, "revision")
			if err != nil {
				if recordICSInterpretationDiagnostic(item, summary, err) {
					continue
				}
				return err
			}
		}
		item, apply, err := s.resolveICSRevisionWith(ctx, tx, sourceID, id, exists, item, old, items)
		if err != nil {
			if recordICSInterpretationDiagnostic(item, summary, err) {
				continue
			}
			return err
		}
		if !apply {
			if !item.fromEmail && old.Status != SyncStatusCancelled && e.Status != SyncStatusCancelled {
				if err := s.refreshRetainedICSMessageTimeWith(ctx, tx, id); err != nil {
					return err
				}
			}
			if err := countICSUnexpandable(ctx, tx, id, summary); err != nil {
				return err
			}
			summary.MessageIDs = append(summary.MessageIDs, id)
			summary.Unchanged++
			continue
		}

		if e.Kind != calendarExceptionKind && e.Status != SyncStatusCancelled {
			previous, err := icsActiveMasterRevision(ctx, tx, sourceID, familyKey)
			if errors.Is(err, sql.ErrNoRows) {
				previous.Sequence = -1
			} else if err != nil {
				if recordICSInterpretationDiagnostic(item, summary, err) {
					continue
				}
				return fmt.Errorf("read retained active ICS master revision: %w", err)
			}
			if icsRevisionOrderNewer(c.Sequence, c.Stamp, previous.Sequence, previous.Stamp) {
				if err := s.supersedeICSExceptionsWith(ctx, tx, sourceID, familyKey, c, items); err != nil {
					if recordICSInterpretationDiagnostic(item, summary, err) {
						continue
					}
					return err
				}
			}
		}
		item.meta.Superseded, err = icsExceptionSuperseded(ctx, tx, sourceID, item, items)
		if err != nil {
			if recordICSInterpretationDiagnostic(item, summary, err) {
				continue
			}
			return err
		}
		fillTombstone := false
		if exists && e.Status == SyncStatusCancelled && old.Status == SyncStatusCancelled && icsEventHasBounds(e) {
			// A later cancellation completes a sparse tombstone; an accepted snapshot stays untouched.
			if fillTombstone, err = icsTombstoneLacksBounds(ctx, tx, id); err != nil {
				return err
			}
		}
		if exists && e.Status == SyncStatusCancelled && !fillTombstone {
			if err := s.cancelICSMetadataWith(ctx, tx, id, old, item.meta); err != nil {
				return err
			}
		} else {
			id, err = s.persistICSDetailsWith(ctx, tx, sourceID, item)
			if err != nil {
				return err
			}
		}

		if err := s.finishICSRevisionWith(ctx, tx, sourceID, id, item, old.Status); err != nil {
			return err
		}
		summary.MessageIDs = append(summary.MessageIDs, id)
		summary.Changed++
		if err := countICSUnexpandable(ctx, tx, id, summary); err != nil {
			return err
		}
	}
	return nil
}

func recordICSInterpretationDiagnostic(item preparedICS, summary *ICalendarImportSummary, err error) bool {
	var invalid *icsInterpretationError
	if !item.fromEmail || !errors.As(err, &invalid) {
		return false
	}
	summary.Diagnostics = append(summary.Diagnostics, invalid.Error())
	return true
}

func icsEventHasBounds(e calevent.Event) bool {
	return e.Start != nil || e.StartDate != ""
}

func icsTombstoneLacksBounds(ctx context.Context, tx *loggedTx, id int64) (bool, error) {
	var start sql.NullTime
	var startDate string
	err := tx.QueryRowContext(ctx, `SELECT start_at, start_date FROM calendar_events WHERE message_id=?`, id).Scan(&start, &startDate)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read ICS tombstone bounds: %w", err)
	}
	if start.Valid || startDate != "" {
		return false, nil
	}
	// Superseded exceptions have no projection. Only retained cancellation
	// evidence can establish that replacing their details fills a sparse row.
	components, err := icsRetainedComponents(ctx, tx, id)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if _, invalid := errors.AsType[*icsInterpretationError](err); invalid {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(components) == 1 && components[0].Event.Status == SyncStatusCancelled && !icsEventHasBounds(components[0].Event), nil
}

func (s *Store) cancelICSMetadataWith(ctx context.Context, tx *loggedTx, id int64, old, next calevent.ICSMetadata) error {
	old.Sequence, old.Stamp, old.Status = next.Sequence, next.Stamp, SyncStatusCancelled
	old.Superseded = next.Superseded
	return s.setICSMetadataWith(ctx, tx, id, old)
}

func countICSUnexpandable(ctx context.Context, tx *loggedTx, id int64, summary *ICalendarImportSummary) error {
	event, _, err := readICSCalendarEvent(ctx, tx, id)
	if err != nil {
		var invalid *icsInterpretationError
		if !errors.As(err, &invalid) {
			return err
		}
		summary.Diagnostics = append(summary.Diagnostics, invalid.Error())
		summary.Unexpandable++
		return nil
	}
	if event.RecurrenceError != "" {
		summary.Unexpandable++
	}
	return nil
}

func readICSCalendarEvent(ctx context.Context, tx *loggedTx, id int64) (calevent.Event, bool, error) {
	var stored []byte
	var metadata, compression, config sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT r.raw_data, r.compression, m.metadata, s.sync_config
 FROM message_raw r JOIN messages m ON m.id=r.message_id JOIN sources s ON s.id=m.source_id
 WHERE r.message_id=? AND r.raw_format='ics'`, id).Scan(&stored, &compression, &metadata, &config)
	if errors.Is(err, sql.ErrNoRows) {
		return calevent.Event{}, false, nil
	}
	if err != nil {
		return calevent.Event{}, false, fmt.Errorf("read ICS recurrence evidence: %w", err)
	}
	return decodeICSRecurrenceEvidence(ctx, stored, compression, metadata.String, config, tx.icsVerificationRemaining)
}

func decodeICSRecurrenceEvidence(ctx context.Context, stored []byte, compression sql.NullString, metadata string, config sql.NullString, remaining *int) (calevent.Event, bool, error) {
	raw, _, err := decodeMessageRawBounded(stored, compression, maxMeetingRawBytes, errMeetingRawTooLarge)
	if err != nil {
		return calevent.Event{}, false, &icsInterpretationError{err}
	}
	budget := calevent.NewICSParseBudget()
	budget.VerificationRemaining = remaining
	event, ok, err := calevent.DecodeICS(ctx, raw, []byte(metadata), calendarConfigZone(config), budget)
	if err != nil {
		return calevent.Event{}, false, &icsInterpretationError{err}
	}
	if ok && event.Kind == "series" {
		if _, _, _, recurrenceErr := prepareCalendarRecurrence(event); recurrenceErr != nil {
			event.RecurrenceError = recurrenceErr.Error()
		}
	}
	return event, ok, err
}

func (s *Store) setICSMetadataWith(ctx context.Context, tx *loggedTx, id int64, meta calevent.ICSMetadata) error {
	encoded, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := setMessageMetadataWith(boundQuerier{ctx: ctx, q: tx}, s.dialect, id, sql.NullString{String: string(encoded), Valid: true}); err != nil {
		return err
	}
	return s.refreshICSMetadataProjectionWith(ctx, tx, id, meta)
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

// A complete series revision retires older detached details omitted by the sender.
func (s *Store) supersedeICSExceptionsWith(ctx context.Context, tx *loggedTx, sourceID int64, familyKey string, master calevent.ICSComponent, incoming []preparedICS) error {
	present := make(map[string]bool, len(incoming))
	for _, item := range incoming {
		present[item.component.Event.ProviderEventID] = true
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.id,m.source_message_id,m.metadata FROM messages m JOIN conversations c ON c.id=m.conversation_id JOIN message_raw r ON r.message_id=m.id WHERE m.source_id=? AND c.source_conversation_id=? AND m.source_message_id<>? AND r.raw_format='ics'`, sourceID, familyKey, familyKey)
	if err != nil {
		return fmt.Errorf("read superseded ICS exceptions: %w", err)
	}
	type revision struct {
		id   int64
		meta calevent.ICSMetadata
	}
	var revisions []revision
	for rows.Next() {
		var id int64
		var key string
		var metadata sql.NullString
		if err := rows.Scan(&id, &key, &metadata); err != nil {
			_ = rows.Close()
			return err
		}
		meta, err := parseRetainedICSMetadata(metadata, "exception")
		if err != nil {
			_ = rows.Close()
			return err
		}
		if !present[key] && icsRevisionOrderNewer(master.Sequence, master.Stamp, meta.Sequence, meta.Stamp) && !meta.Superseded {
			meta.Superseded = true
			revisions = append(revisions, revision{id, meta})
		}
	}
	iterationErr, closeErr := rows.Err(), rows.Close()
	if iterationErr != nil {
		return iterationErr
	}
	if closeErr != nil {
		return closeErr
	}
	for _, rev := range revisions {
		if err := s.setICSMetadataWith(ctx, tx, rev.id, rev.meta); err != nil {
			return err
		}
	}
	return nil
}

func icsMastersFirst(items []preparedICS) {
	slices.SortStableFunc(items, func(a, b preparedICS) int {
		aException, bException := a.component.Event.Kind == calendarExceptionKind, b.component.Event.Kind == calendarExceptionKind
		if aException == bException {
			return 0
		}
		if aException {
			return 1
		}
		return -1
	})
}

func icsExceptionSuperseded(ctx context.Context, tx *loggedTx, sourceID int64, item preparedICS, incoming []preparedICS) (bool, error) {
	c := item.component
	if c.Event.Kind != calendarExceptionKind {
		return false, nil
	}
	master, err := icsActiveMasterRevision(ctx, tx, sourceID, calevent.ICSMessageID(c.UID, ""))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read ICS master revision: %w", err)
	}
	if !icsRevisionOrderNewer(master.Sequence, master.Stamp, c.Sequence, c.Stamp) {
		return false, nil
	}
	for _, candidate := range incoming {
		m := candidate.component
		if m.UID == c.UID && m.Event.Kind != calendarExceptionKind && m.Sequence == master.Sequence && m.Stamp.Equal(master.Stamp) {
			return false, nil
		}
	}
	return true, nil
}

// Cancellation precedence belongs to metadata; exception precedence belongs to
// the retained active snapshot, which may predate that cancellation.
func icsActiveMasterRevision(ctx context.Context, tx *loggedTx, sourceID int64, key string) (calevent.ICSMetadata, error) {
	var metadata sql.NullString
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id,metadata FROM messages WHERE source_id=? AND source_message_id=? AND message_type='calendar_event'`, sourceID, key).Scan(&id, &metadata)
	if err != nil {
		return calevent.ICSMetadata{}, err
	}
	master, err := parseRetainedICSMetadata(metadata, "master")
	if err != nil {
		return master, err
	}
	if master.Status != SyncStatusCancelled {
		return master, nil
	}
	components, err := icsRetainedComponents(ctx, tx, id)
	if err != nil {
		return master, err
	}
	master.Sequence = -1 // A sparse cancellation contains no active master revision.
	if len(components) == 1 && components[0].Event.Status != SyncStatusCancelled {
		master.Sequence, master.Stamp = components[0].Sequence, components[0].Stamp
	}
	return master, nil
}

func icsRetainedComponents(ctx context.Context, tx *loggedTx, id int64) ([]calevent.ICSComponent, error) {
	var stored []byte
	var compression, config sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT r.raw_data,r.compression,s.sync_config FROM message_raw r JOIN messages m ON m.id=r.message_id JOIN sources s ON s.id=m.source_id WHERE r.message_id=?`, id).Scan(&stored, &compression, &config)
	if err != nil {
		return nil, err
	}
	raw, _, err := decodeMessageRawBounded(stored, compression, maxMeetingRawBytes, errMeetingRawTooLarge)
	if err != nil {
		return nil, &icsInterpretationError{err}
	}
	components, _, err := calevent.ParseICSWithDiagnostics(ctx, raw, "", calendarConfigZone(config), tx.icsParseBudget())
	if err != nil {
		return nil, &icsInterpretationError{err}
	}
	return components, nil
}

func (s *Store) persistICSDetailsWith(ctx context.Context, tx *loggedTx, sourceID int64, item preparedICS) (int64, error) {
	q := boundQuerier{ctx: ctx, q: tx}
	c := item.component
	e := c.Event
	key, familyKey := e.ProviderEventID, calevent.ICSMessageID(c.UID, "")

	encoded, err := json.Marshal(item.meta)
	if err != nil {
		return 0, err
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
		if attendee.Role == "organizer" && attendee.Email == c.OrganizerEmail {
			organizer = len(participants)
		}
		_, domain, _ := strings.Cut(attendee.Email, "@")
		participants = append(participants, ParticipantPersistData{EmailAddress: attendee.Email, DisplayName: attendee.DisplayName, Domain: domain})
	}
	sent, err := icsEventSentAt(e)
	if err != nil {
		return 0, err
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
			}
			target.ParticipantIDs = append(target.ParticipantIDs, ids[i])
			target.DisplayNames = append(target.DisplayNames, p.DisplayName)
			target.EmailAddresses = append(target.EmailAddresses, p.EmailAddress)
			if i != organizer {
				emails = append(emails, p.EmailAddress)
			}
		}
		return &MessagePersistData{
			Message:      &Message{SourceID: sourceID, SourceMessageID: key, MessageType: "calendar_event", Subject: sql.NullString{String: c.Summary, Valid: c.Summary != ""}, SentAt: sent, SenderID: sender, Snippet: sql.NullString{String: body, Valid: body != ""}, SizeEstimate: int64(len(item.raw))},
			Conversation: &ConversationPersistData{SourceConversationID: familyKey, ConversationType: "calendar", Title: title},
			BodyText:     sql.NullString{String: body, Valid: body != ""}, RawMIME: item.raw, RawFormat: rawFormatICS, Metadata: &sql.NullString{String: string(encoded), Valid: true}, PreserveLabels: true,
			Recipients: []RecipientSet{from, to}, FTS: &FTSDoc{Subject: c.Summary, Body: body, FromAddr: c.OrganizerEmail, ToAddrs: strings.Join(emails, " ")},
			calendarInvite: &item,
		}
	}, nil, nil)
	if err != nil {
		return 0, fmt.Errorf("persist ICS event: %w", err)
	}
	if e.Kind != calendarExceptionKind && c.Summary == "" && e.Status != SyncStatusCancelled {
		if _, err := q.Exec(fmt.Sprintf(`UPDATE conversations SET title=NULL,updated_at=%s WHERE id=(SELECT conversation_id FROM messages WHERE id=?) AND COALESCE(title,'')<>''`, s.dialect.Now()), id); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// Keep revision precedence while recovering facts absent from a cancellation.
func (s *Store) resolveICSRevisionWith(ctx context.Context, tx *loggedTx, sourceID, id int64, exists bool, item preparedICS, old calevent.ICSMetadata, incoming []preparedICS) (preparedICS, bool, error) {
	c := item.component
	if !exists || icsRevisionNewer(c, old) {
		return item, true, nil
	}
	if err := s.reviveICSExceptionWith(ctx, tx, sourceID, id, item, old, incoming); err != nil {
		return item, false, err
	}
	if old.Status != SyncStatusCancelled || c.Event.Status == SyncStatusCancelled || c.Sequence > old.Sequence {
		return item, false, nil
	}
	components, err := icsRetainedComponents(ctx, tx, id)
	if err != nil {
		if _, ok := errors.AsType[*icsInterpretationError](err); !ok {
			return item, false, err
		}
		// Retained raw is evidence for filling sparse cancellations. If it is
		// unreadable, keep the cancellation and let countICSUnexpandable record
		// the archive diagnostic instead of aborting the import.
		return item, false, nil
	}
	if old.Superseded || !icsCancellationMissingFacts(components, c.Event) {
		return item, false, nil
	}
	item.meta = old
	item.meta.AllDay = c.Event.AllDay
	return item, true, nil
}

func icsCancellationMissingFacts(retained []calevent.ICSComponent, incoming calevent.Event) bool {
	if len(retained) != 1 || retained[0].Event.Status != SyncStatusCancelled {
		return false
	}
	event := retained[0].Event
	return event.Start == nil || event.End == nil || len(incoming.Recurrence) > 0 && len(event.Recurrence) == 0
}

func (s *Store) reviveICSExceptionWith(ctx context.Context, tx *loggedTx, sourceID, id int64, item preparedICS, old calevent.ICSMetadata, incoming []preparedICS) error {
	c := item.component
	if !old.Superseded || c.Sequence != old.Sequence || !c.Stamp.Equal(old.Stamp) || c.Event.Status != old.Status {
		return nil
	}
	superseded, err := icsExceptionSuperseded(ctx, tx, sourceID, item, incoming)
	if err != nil || superseded {
		return err
	}
	old.Superseded = false
	return s.setICSMetadataWith(ctx, tx, id, old)
}
