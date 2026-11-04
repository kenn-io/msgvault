package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/sourcescope"
)

const (
	meetingSingleKind       = "single"
	meetingSeriesKind       = "series"
	meetingExceptionKind    = "exception"
	meetingCancelledStatus  = "cancelled"
	meetingEvidenceSplitKey = "*"
)

// OccurrenceRef identifies an event, or one original occurrence of a series.
type OccurrenceRef struct {
	MessageID int64
	Key       string
}

type meetingIdentity struct {
	ICalUID        string
	EventMessageID int64
	OccurrenceKey  string
}

type linkOccurrence struct {
	Ref        OccurrenceRef
	Row        CalendarEventRow
	Identity   meetingIdentity
	Start, End time.Time
}

type reboundReader struct {
	reader  SQLReader
	rebind  func(string) string
	dialect Dialect
}

func (r reboundReader) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return r.reader.QueryContext(ctx, r.rebind(query), args...)
}
func (r reboundReader) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return r.reader.QueryRowContext(ctx, r.rebind(query), args...)
}

func meetingEventRows(ctx context.Context, r reboundReader, predicate string, args []any, budget *int) ([]CalendarEventRow, error) {
	rows, err := queryCalendarEventRowsWithinBudget(ctx, r, predicate, args, min(200, *budget))
	if err != nil {
		return nil, err
	}
	*budget -= len(rows)
	for i := range rows {
		if rows[i].rawFormat != rawFormatICS {
			continue
		}
		component, err := meetingICSComponent(ctx, r, rows[i], budget)
		if err != nil {
			return nil, err
		}
		rows[i].ICalUID = component.UID
		rows[i].Event.RecurrenceError = component.Event.RecurrenceError
	}
	return rows, nil
}

func occurrenceFromRow(row CalendarEventRow, key string) linkOccurrence {
	id := meetingIdentity{ICalUID: row.ICalUID, OccurrenceKey: key}
	// An unidentified exception has no occurrence identity to share with other
	// rows, even when the provider gave the series an iCalUID.
	if row.Event.Kind == meetingExceptionKind && row.Event.Original == nil {
		id = meetingIdentity{EventMessageID: row.MessageID}
	}
	// Occurrence keys represent whole seconds. Keep fractional exceptions
	// addressable by their message instead of querying a truncated original start.
	if original := row.Event.Original; row.Event.Kind == meetingExceptionKind && original != nil && !original.AllDay && original.Instant.Nanosecond() != 0 {
		key = ""
		id.ICalUID = ""
		id.OccurrenceKey = original.Key()
	}
	if id.ICalUID == "" {
		id.EventMessageID = row.MessageID
	}
	o := linkOccurrence{Ref: OccurrenceRef{MessageID: row.MessageID, Key: key}, Row: row, Identity: id}
	if row.Event.Start != nil {
		o.Start = *row.Event.Start
	}
	if row.Event.End != nil {
		o.End = *row.Event.End
	}
	return o
}

func expandMeetingSeries(ctx context.Context, r reboundReader, master CalendarEventRow, from, to time.Time, budget *int) ([]linkOccurrence, error) {
	predicate, args := calendarExceptionWindow(r.dialect, from, to)
	predicate += ` AND m.source_id = ? AND e.series_provider_id = ?`
	args = append(args, master.SourceID, master.Event.ProviderEventID)
	related, err := meetingEventRows(ctx, r, predicate, args, budget)
	if err != nil {
		return nil, err
	}
	allRelated, err := queryCalendarExceptionAliases(ctx, r, related, len(related)+max(*budget, 0))
	if err != nil {
		return nil, err
	}
	*budget -= len(allRelated) - len(related)
	related = calendarExceptionRows(allRelated)
	expanded, count, reason, err := expandCalendarSeries(ctx, master, related, from, to, *budget)
	*budget -= count
	if err != nil {
		return nil, err
	}
	var expansionErr error
	if reason != "" {
		if *budget <= 0 || reason == calrecur.ErrBudgetExceeded.Error() {
			return nil, calendarRecurrenceBudgetError()
		}
		expansionErr = fmt.Errorf("%w: %s", calrecur.ErrUnsupportedRecurrence, reason)
	}
	byID := make(map[int64]CalendarEventRow, len(related))
	for _, row := range related {
		byID[row.MessageID] = row
	}
	out := make([]linkOccurrence, 0, len(expanded))
	for _, instance := range expanded {
		effective, instance, err := materializeCalendarOccurrence(ctx, r, master, byID, instance)
		if err != nil {
			return nil, err
		}
		o := occurrenceFromRow(master, instance.Original.Key())
		if instance.HasException && (instance.Original == (calrecur.Identity{}) || !instance.Original.AllDay && instance.Original.Instant.Nanosecond() != 0) {
			o = occurrenceFromRow(effective, "")
		}
		o.Row = effective
		o.Start, o.End = instance.Start, instance.End
		out = append(out, o)
	}
	return out, expansionErr
}

func resolveMeetingOccurrence(ctx context.Context, r reboundReader, ref OccurrenceRef, budget *int) (linkOccurrence, error) {
	rows, err := meetingEventRows(ctx, r, `e.message_id = ?`, []any{ref.MessageID}, budget)
	if err != nil {
		return linkOccurrence{}, err
	}
	if len(rows) != 1 {
		return linkOccurrence{}, sql.ErrNoRows
	}
	row := rows[0]
	if row.Event.Kind == meetingExceptionKind && ref.Key == "" {
		masters, err := meetingEventRows(ctx, r, `e.kind IN ('series', 'single') AND m.source_id = ? AND e.provider_event_id = ?`, []any{row.SourceID, row.Event.SeriesProviderID}, budget)
		if err != nil {
			return linkOccurrence{}, err
		}
		if row.Event.Original == nil {
			if len(masters) == 1 && masters[0].Event.Status == meetingCancelledStatus {
				row.Event.Status = meetingCancelledStatus
			}
		} else if len(masters) == 1 && (row.Event.Original.AllDay || row.Event.Original.Instant.Nanosecond() == 0) {
			row, ref = masters[0], OccurrenceRef{MessageID: masters[0].MessageID, Key: row.Event.Original.Key()}
		} else {
			row, err = freshestOrphanCalendarException(ctx, r, row, budget)
			if err != nil {
				return linkOccurrence{}, err
			}
			if len(masters) == 1 && masters[0].Event.Status == meetingCancelledStatus {
				row.Event.Status = meetingCancelledStatus
			}
			if row.Event.Start == nil || row.Event.End == nil {
				return linkOccurrence{}, fmt.Errorf("%s: %w", calendarMissingBoundsReason(row.Event), sql.ErrNoRows)
			}
			o := occurrenceFromRow(row, row.Event.Original.Key())
			o.Ref.Key = ""
			return o, nil
		}
	}
	if ref.Key == "" {
		if row.Event.Kind == meetingSeriesKind {
			return linkOccurrence{}, sql.ErrNoRows
		}
		if row.Event.Start == nil || row.Event.End == nil {
			return linkOccurrence{}, fmt.Errorf("%s: %w", calendarMissingBoundsReason(row.Event), sql.ErrNoRows)
		}
		return occurrenceFromRow(row, ""), nil
	}
	if row.Event.Kind != meetingSeriesKind && row.Event.Kind != "single" {
		return linkOccurrence{}, sql.ErrNoRows
	}
	_, id, ok := calrecur.ParseInstanceID("event_" + ref.Key)
	if !ok {
		return linkOccurrence{}, fmt.Errorf("invalid occurrence key %q", ref.Key)
	}
	at := id.Instant
	if id.AllDay {
		zone, err := calendarEventZone(row.Event)
		if err != nil {
			return linkOccurrence{}, err
		}
		at, _, err = calrecur.CivilDateBoundary(id.Date, zone)
		if err != nil {
			return linkOccurrence{}, err
		}
	}
	// A moved exception is expanded at its effective start, retaining Original.
	originalColumn, originalValue := "e.original_start_at", any(id.Instant)
	if id.AllDay {
		originalColumn, originalValue = "e.original_start_date", id.Date
	}
	moved, err := meetingEventRows(ctx, r, `e.kind = 'exception' AND m.source_id = ? AND e.series_provider_id = ? AND `+originalColumn+` = ?`, []any{row.SourceID, row.Event.ProviderEventID, originalValue}, budget)
	if err != nil {
		return linkOccurrence{}, err
	}
	moved = eligibleCalendarExceptionRows(row, moved)
	if x, ok := calendarExceptionOverrides(moved)[ref.Key]; ok {
		if x.Event.Start != nil && x.Event.End != nil {
			o := occurrenceFromRow(row, ref.Key)
			o.Start, o.End, err = seriesExceptionBounds(row, x)
			if err != nil {
				return linkOccurrence{}, err
			}
			o.Row = x
			if row.Event.Status == meetingCancelledStatus {
				o.Row.Event.Status = meetingCancelledStatus
			}
			return o, nil
		}
		if x.Event.Start != nil {
			at = *x.Event.Start
		}
	}
	occurrences, err := expandMeetingSeries(ctx, r, row, at.Add(-calendarCandidateMargin), at.Add(calendarCandidateMargin), budget)
	if err != nil && !errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
		return linkOccurrence{}, err
	}
	for _, o := range occurrences {
		if o.Ref.Key == ref.Key {
			return o, nil
		}
	}
	if err != nil {
		return linkOccurrence{}, err
	}
	return linkOccurrence{}, sql.ErrNoRows
}

func seriesExceptionBounds(master, exception CalendarEventRow) (time.Time, time.Time, error) {
	set, duration, days, err := prepareCalendarRecurrence(master.Event)
	var prepared *calrecur.Set
	if err == nil {
		prepared = &set
	}
	o, err := exceptionBoundsFor(prepared, duration, days, exception)
	return o.Start, o.End, err
}

// Bounded edits share one interpretation across listings and meeting links,
// including when series generation fails. Sparse edits still inherit bounds.
func exceptionBoundsFor(set *calrecur.Set, duration time.Duration, days int, row CalendarEventRow) (calrecur.Occurrence, error) {
	e := row.Event
	o := calrecur.Occurrence{AllDay: e.AllDay, StartDate: e.StartDate, EndDate: e.EndDate, Cancelled: e.Status == meetingCancelledStatus, ExceptionRef: row.MessageID, HasException: true}
	if e.Original != nil {
		o.Original = *e.Original
	}
	bounded := e.Start != nil && e.End != nil
	if bounded {
		o.Start, o.End = *e.Start, *e.End
	}
	if set == nil || e.Original == nil || e.Original.AllDay != set.AllDay || bounded && !e.AllDay {
		return o, nil
	}
	// Explicit dates use the event or calendar zone; omitted bounds inherit the master.
	var zone *time.Location
	if e.EventTimeZone != "" || e.StartDate != "" || e.EndDate != "" {
		var err error
		zone, err = calendarEventZone(e)
		if err != nil {
			if bounded {
				return o, nil
			}
			return o, err
		}
	}
	resolved, err := calrecur.EffectiveException(*set, calrecur.Exception{Original: *e.Original, Start: e.Start, End: e.End, StartDate: e.StartDate, EndDate: e.EndDate, Zone: zone, Cancelled: o.Cancelled, Ref: row.MessageID}, duration, days)
	if bounded && errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
		return o, nil
	}
	return resolved, err
}

func memberLinkKey(ctx context.Context, r SQLReader, id int64) (meetingLinkKey, error) {
	var key meetingLinkKey
	var at sql.NullTime
	scope, args := sourceScopeClause(ctx, "m.source_id")
	args = append([]any{id}, args...)
	err := r.QueryRowContext(ctx, `SELECT k.kind,k.value,k.calendar_id,k.scheduled_at FROM meeting_link_key k JOIN messages m ON m.id=k.message_id WHERE k.message_id = ? AND m.deleted_at IS NULL`+scope, args...).Scan(&key.Kind, &key.Value, &key.CalendarID, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return key, nil
	}
	if at.Valid {
		key.ScheduledAt = &at.Time
	}
	return key, err
}

// ICS stores the UID in retained VEVENT evidence, not message metadata. Its
// deterministic provider key lets the UID lookup reach those copies by index.
func (s *Store) calendarUIDPredicate(uid string) (string, []any) {
	key := calevent.ICSMessageID(uid, "")
	return `e.message_id IN (
 SELECT uid_message.id FROM messages uid_message WHERE uid_message.message_type='calendar_event' AND ` + calendarUIDExpression(s.dialect, "uid_message.metadata") + ` = ?
 UNION
 SELECT uid_event.message_id FROM calendar_events uid_event JOIN message_raw uid_raw ON uid_raw.message_id=uid_event.message_id
 WHERE uid_raw.raw_format='ics' AND (uid_event.provider_event_id=? OR uid_event.series_provider_id=?)
 )`, []any{uid, key, key}
}

func meetingICSComponent(ctx context.Context, r reboundReader, row CalendarEventRow, budget *int) (calevent.ICSComponent, error) {
	scope, args := sourceScopeClause(ctx, "m.source_id")
	args = append([]any{row.MessageID}, args...)
	var stored []byte
	var compression sql.NullString
	err := r.QueryRowContext(ctx, `SELECT raw.raw_data,raw.compression FROM message_raw raw JOIN messages m ON m.id=raw.message_id WHERE m.id=? AND m.deleted_at IS NULL`+scope, args...).Scan(&stored, &compression)
	if errors.Is(err, sql.ErrNoRows) {
		return calevent.ICSComponent{}, nil
	}
	if err != nil {
		return calevent.ICSComponent{}, fmt.Errorf("read meeting ICS identity: %w", err)
	}
	raw, _, err := decodeMessageRawBounded(stored, compression, maxMeetingRawBytes, errMeetingRawTooLarge)
	if err != nil {
		return calevent.ICSComponent{}, nil //nolint:nilerr // Damaged optional evidence keeps the projected message identity.
	}
	parseBudget := calevent.NewICSParseBudget()
	parseBudget.VerificationRemaining = budget
	components, _, err := calevent.ParseICSWithDiagnostics(ctx, raw, "", row.Event.CalendarTimeZone, parseBudget)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, calrecur.ErrBudgetExceeded) {
			return calevent.ICSComponent{}, fmt.Errorf("decode meeting ICS identity: %w", err)
		}
		return calevent.ICSComponent{}, nil // Invalid retained evidence cannot supply a UID.
	}
	if len(components) != 1 {
		return calevent.ICSComponent{}, nil
	}
	return components[0], nil
}

func freshestOrphanCalendarException(ctx context.Context, r reboundReader, row CalendarEventRow, budget *int) (CalendarEventRow, error) {
	identity := row.Event.Original
	if identity == nil {
		return row, nil
	}
	column, value := "e.original_start_at", any(identity.Instant)
	if identity.AllDay {
		column, value = "e.original_start_date", identity.Date
	}
	aliases, err := meetingEventRows(ctx, r,
		`e.kind = 'exception' AND m.source_id = ? AND e.series_provider_id = ? AND `+column+` = ?`,
		[]any{row.SourceID, row.Event.SeriesProviderID, value}, budget)
	if err != nil {
		return CalendarEventRow{}, err
	}
	for _, alias := range aliases {
		if alias.Event.Original != nil && alias.Event.Original.Key() == identity.Key() && calendarEventSnapshotNewer(alias, row) {
			row = alias
		}
	}
	return row, nil
}

func latestVisibleCalendarExceptionPredicate(d Dialect) string {
	newerUpdated := calendarProviderMetadataNullableNumberExpression(d, "newer_message.metadata", "provider_updated_at_unix_nano")
	currentUpdated := calendarProviderMetadataNullableNumberExpression(d, "m.metadata", "provider_updated_at_unix_nano")
	newerSequence := calendarProviderMetadataNullableNumberExpression(d, "newer_message.metadata", "sequence")
	currentSequence := calendarProviderMetadataNullableNumberExpression(d, "m.metadata", "sequence")
	updatedEqualOrUnknown := "(" + newerUpdated + " IS NULL OR " + currentUpdated + " IS NULL OR " + newerUpdated + " = " + currentUpdated + ")"
	sequenceEqualOrUnknown := "(" + newerSequence + " IS NULL OR " + currentSequence + " IS NULL OR " + newerSequence + " = " + currentSequence + ")"
	return `NOT EXISTS (
		SELECT 1 FROM calendar_events newer_event
		JOIN messages newer_message ON newer_message.id = newer_event.message_id
		WHERE newer_event.kind = 'exception'
		  AND newer_message.source_id = m.source_id
		  AND newer_event.series_provider_id = e.series_provider_id
		  AND ((e.original_start_at IS NOT NULL AND newer_event.original_start_at = e.original_start_at)
		    OR (e.original_start_date != '' AND newer_event.original_start_date = e.original_start_date))
		  AND newer_message.message_type = 'calendar_event'
		  AND newer_message.deleted_at IS NULL
		  AND NOT EXISTS (
			SELECT 1 FROM source_merge_archive_only_messages newer_archive
			WHERE newer_archive.message_id = newer_message.id
		  )
		  AND ((` + newerUpdated + ` IS NOT NULL AND ` + currentUpdated + ` IS NOT NULL AND ` + newerUpdated + ` > ` + currentUpdated + `)
		    OR (` + updatedEqualOrUnknown + ` AND ` + newerSequence + ` IS NOT NULL AND ` + currentSequence + ` IS NOT NULL AND ` + newerSequence + ` > ` + currentSequence + `)
		    OR (` + updatedEqualOrUnknown + ` AND ` + sequenceEqualOrUnknown + `
		      AND (newer_event.provider_observation_order > e.provider_observation_order
		        OR (newer_event.provider_observation_order = e.provider_observation_order AND newer_event.message_id > e.message_id))))
	)`
}

func (s *Store) automaticMeetingCandidates(ctx context.Context, r reboundReader, key meetingLinkKey, budget *int) ([]linkOccurrence, error) {
	if key.Kind != "google_event_id" && key.Kind != "ical_uid" {
		return nil, nil
	}
	predicate, args := s.calendarUIDPredicate(key.Value)
	base, instance, _ := calrecur.ParseInstanceID(key.Value)
	if key.Kind == "google_event_id" {
		// Parse the same source configuration used by calendar persistence.
		scope, scopeArgs := sourceScopeClause(ctx, "id")
		sources, err := r.QueryContext(ctx, `SELECT id,sync_config FROM sources WHERE source_type = 'gcal'`+scope, scopeArgs...)
		if err != nil {
			return nil, err
		}
		defer func() { _ = sources.Close() }()
		var sourceIDs []any
		for sources.Next() {
			var id int64
			var config sql.NullString
			if err := sources.Scan(&id, &config); err != nil {
				return nil, err
			}
			if calendarMailbox(config) == key.CalendarID {
				sourceIDs = append(sourceIDs, id)
			}
		}
		err = errors.Join(sources.Err(), sources.Close())
		if err != nil {
			return nil, err
		}
		if len(sourceIDs) == 0 {
			return nil, nil
		}
		predicate = `m.source_id IN (` + placeholders(len(sourceIDs)) + `) AND (e.provider_event_id = ? OR (e.kind IN ('series', 'single') AND e.provider_event_id = ?))`
		args = sourceIDs
		args = append(args, key.Value, base)
	} else if key.ScheduledAt != nil {
		window, windowArgs := calendarExceptionWindow(r.dialect, key.ScheduledAt.Add(-calendarCandidateMargin), key.ScheduledAt.Add(calendarCandidateMargin))
		predicate += ` AND (e.kind != 'exception' OR ((` + window + `) AND ` + latestVisibleCalendarExceptionPredicate(r.dialect) + `))`
		args = append(args, windowArgs...)
	} else {
		predicate += ` AND e.kind != 'exception'`
	}
	rows, err := meetingEventRows(ctx, r, predicate, args, budget)
	if err != nil {
		return nil, err
	}
	var out []linkOccurrence
candidatesLoop:
	for _, row := range rows {
		if row.Event.Kind == meetingExceptionKind {
			for _, master := range rows {
				if master.Event.Kind == meetingSeriesKind && master.SourceID == row.SourceID && master.Event.ProviderEventID == row.Event.SeriesProviderID {
					continue candidatesLoop
				}
			}
		}
		var candidates []linkOccurrence
		if key.Kind == "google_event_id" {
			ref := OccurrenceRef{MessageID: row.MessageID}
			if row.Event.Kind == meetingSeriesKind || row.Event.Kind == "single" && base != "" && row.Event.ProviderEventID == base {
				if base == "" {
					continue
				}
				ref.Key = instance.Key()
			}
			o, err := resolveMeetingOccurrence(ctx, r, ref, budget)
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				continue
			}
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, o)
		} else if row.Event.Kind == meetingSeriesKind {
			if key.ScheduledAt == nil {
				continue
			}
			// PostgreSQL rounds a nanosecond window to zero; compare exact starts below.
			candidates, err = expandMeetingSeries(ctx, r, row, *key.ScheduledAt, key.ScheduledAt.Add(time.Second), budget)
			if err != nil && !errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				return nil, err
			}
		} else {
			o, err := resolveMeetingOccurrence(ctx, r, OccurrenceRef{MessageID: row.MessageID}, budget)
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				continue
			}
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, o)
		}
		for _, o := range candidates {
			if key.Kind == "ical_uid" && row.Event.Kind != "single" {
				if key.ScheduledAt == nil || !o.Start.Equal(*key.ScheduledAt) {
					continue
				}
			}
			out = append(out, o)
		}
	}
	return out, nil
}

// Preserve ambiguity suppression for the existing link and split readers.
func (s *Store) automaticMeetingOccurrences(ctx context.Context, r reboundReader, key meetingLinkKey, budget *int) ([]linkOccurrence, error) {
	out, err := s.automaticMeetingCandidates(ctx, r, key, budget)
	if err != nil {
		return nil, err
	}
	for _, o := range out {
		if o.Identity != out[0].Identity {
			return nil, nil
		}
	}
	return out, nil
}

type meetingLinkDecision struct {
	Identity  meetingIdentity
	Decision  string
	CreatedAt time.Time
}

type meetingMemberCandidate struct {
	linkOccurrence

	Provenance string // manual or automatic
}

func (s *Store) meetingMemberOccurrences(ctx context.Context, r reboundReader, memberID int64, budget *int) ([]linkOccurrence, error) {
	candidates, err := s.meetingMemberCandidatesWithAmbiguity(ctx, r, memberID, budget, true)
	if err != nil {
		return nil, err
	}
	var out []linkOccurrence
	for _, c := range candidates {
		out = append(out, c.linkOccurrence)
	}
	return out, nil
}

func (s *Store) meetingMemberCandidates(ctx context.Context, r reboundReader, memberID int64, budget *int) ([]meetingMemberCandidate, error) {
	return s.meetingMemberCandidatesWithAmbiguity(ctx, r, memberID, budget, false)
}

func (s *Store) meetingMemberCandidatesWithAmbiguity(ctx context.Context, r reboundReader, memberID int64, budget *int, suppressAmbiguity bool) ([]meetingMemberCandidate, error) {
	decisions, err := readMeetingLinkDecisions(ctx, r, memberID)
	if err != nil {
		return nil, err
	}
	type resolvedDecision struct {
		meetingLinkDecision

		target linkOccurrence
	}
	var resolved []resolvedDecision
	splits := map[meetingIdentity]time.Time{}
	for _, d := range decisions {
		if d.Decision == "split" {
			identities, err := s.meetingSplitIdentities(ctx, r, d.Identity, budget)
			if err != nil {
				return nil, err
			}
			for _, identity := range identities {
				if !d.CreatedAt.Before(splits[identity]) {
					splits[identity] = d.CreatedAt
				}
			}
			continue
		}
		targets, err := s.resolveMeetingIdentity(ctx, r, d.Identity, budget)
		if err != nil {
			return nil, err
		}
		// Keep an absent target's identity so its decision still overrides automatic matches.
		if len(targets) == 0 && !sourcescope.Restricted(ctx) {
			targets = []linkOccurrence{{Identity: d.Identity}}
		}
		for _, o := range targets {
			resolved = append(resolved, resolvedDecision{d, o})
		}
	}
	var out []linkOccurrence
	hasManualLink := false
	for _, d := range resolved {
		if d.Decision == "link" && d.CreatedAt.After(splits[d.target.Identity]) {
			hasManualLink = true
			if d.target.Ref.MessageID != 0 {
				out = append(out, d.target)
			}
		}
	}
	if hasManualLink {
		return meetingCandidatesWithProvenance(out, "manual"), nil
	}
	key, err := memberLinkKey(ctx, r, memberID)
	if err != nil {
		return nil, err
	}
	if _, split := splits[meetingIdentity{ICalUID: key.Value, OccurrenceKey: meetingEvidenceSplitKey}]; key.Kind == "ical_uid" && split {
		return nil, nil
	}
	if key.Value != "" {
		resolve := s.automaticMeetingCandidates
		if suppressAmbiguity {
			resolve = s.automaticMeetingOccurrences
		}
		matches, err := resolve(ctx, r, key, budget)
		if err != nil {
			return nil, err
		}
		for _, o := range matches {
			if _, split := splits[o.Identity]; !split {
				out = append(out, o)
			}
		}
	}
	return meetingCandidatesWithProvenance(out, "automatic"), nil
}

func meetingCandidatesWithProvenance(out []linkOccurrence, provenance string) []meetingMemberCandidate {
	unique := uniqueMeetingOccurrences(out)
	candidates := make([]meetingMemberCandidate, 0, len(unique))
	for _, o := range unique {
		candidates = append(candidates, meetingMemberCandidate{o, provenance})
	}
	return candidates
}

func (s *Store) meetingReader(ctx context.Context) reboundReader {
	return reboundReader{reader: ReadDBContext(ctx, s.db.DB), rebind: s.dialect.Rebind, dialect: s.dialect}
}

// Pick the freshest snapshot within each source series, then choose the lowest
// canonical reference across calendar copies of the same occurrence.
func uniqueMeetingOccurrences(out []linkOccurrence) []linkOccurrence {
	type exceptionAliasIdentity struct {
		sourceID int64
		seriesID string
		key      string
	}
	aliases := make(map[exceptionAliasIdentity]linkOccurrence)
	filtered := make([]linkOccurrence, 0, len(out))
	for _, occurrence := range out {
		event := occurrence.Row.Event
		if event.Kind != meetingExceptionKind || event.Original == nil {
			filtered = append(filtered, occurrence)
			continue
		}
		key := exceptionAliasIdentity{
			sourceID: occurrence.Row.SourceID,
			seriesID: event.SeriesProviderID,
			key:      calendarExceptionAliasKey(*event.Original),
		}
		current, exists := aliases[key]
		if !exists || calendarEventSnapshotNewer(occurrence.Row, current.Row) {
			aliases[key] = occurrence
		}
	}
	for _, occurrence := range aliases {
		filtered = append(filtered, occurrence)
	}
	out = filtered
	slices.SortFunc(out, func(a, b linkOccurrence) int {
		if c := cmp.Compare(a.Ref.MessageID, b.Ref.MessageID); c != 0 {
			return c
		}
		return cmp.Compare(a.Ref.Key, b.Ref.Key)
	})
	seen := make(map[meetingIdentity]bool, len(out))
	return slices.DeleteFunc(out, func(o linkOccurrence) bool {
		if seen[o.Identity] {
			return true
		}
		seen[o.Identity] = true
		return false
	})
}

func (s *Store) meetingIdentityRows(ctx context.Context, r reboundReader, identity meetingIdentity, budget *int) ([]CalendarEventRow, error) {
	predicate, args := s.calendarUIDPredicate(identity.ICalUID)
	if identity.ICalUID == "" {
		predicate, args = "e.message_id = ?", []any{identity.EventMessageID}
	}
	if identity.ICalUID != "" {
		if identity.OccurrenceKey == "" {
			predicate += ` AND (e.kind = 'single' OR (e.kind = 'exception' AND e.original_start_at IS NULL AND e.original_start_date = ''))`
		} else {
			_, id, ok := calrecur.ParseInstanceID("event_" + identity.OccurrenceKey)
			if !ok {
				return nil, nil
			}
			column, value := "e.original_start_at", any(id.Instant)
			if id.AllDay {
				column, value = "e.original_start_date", id.Date
			}
			predicate += ` AND (e.kind IN ('series', 'single') OR (e.kind = 'exception' AND ` + column + ` = ?))`
			args = append(args, value)
		}
	}

	return meetingEventRows(ctx, r, predicate, args, budget)
}

// Splits retain their saved identity even when a series can no longer expand.
// Complete exceptions can acquire a master's identity without expansion.
func (s *Store) meetingSplitIdentities(ctx context.Context, r reboundReader, identity meetingIdentity, budget *int) ([]meetingIdentity, error) {
	rows, err := s.meetingIdentityRows(ctx, r, identity, budget)
	if err != nil {
		return nil, err
	}
	identities := []meetingIdentity{identity}
	for _, row := range rows {
		// Older ICS decisions used message IDs before retained UIDs were read.
		// Keep that saved split effective against the normalized copy identity.
		if row.rawFormat == rawFormatICS {
			identities = append(identities, occurrenceFromRow(row, identity.OccurrenceKey).Identity)
		}
		if row.Event.Kind != meetingExceptionKind || row.Event.Start == nil || row.Event.End == nil {
			continue
		}
		if row.Event.Original != nil && row.Event.Original.Key() != identity.OccurrenceKey {
			continue
		}
		target, err := resolveMeetingOccurrence(ctx, r, OccurrenceRef{MessageID: row.MessageID}, budget)
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) || errors.Is(err, ErrQueryTooBroad) {
			continue
		}
		if err != nil {
			return nil, err
		}
		identities = append(identities, target.Identity)
	}
	return identities, nil
}

func (s *Store) resolveMeetingIdentity(ctx context.Context, r reboundReader, identity meetingIdentity, budget *int) ([]linkOccurrence, error) {
	rows, err := s.meetingIdentityRows(ctx, r, identity, budget)
	if err != nil {
		return nil, err
	}
	var out []linkOccurrence
	for _, row := range rows {
		ref := OccurrenceRef{MessageID: row.MessageID, Key: identity.OccurrenceKey}
		if row.Event.Kind == meetingExceptionKind {
			if row.Event.Original != nil && row.Event.Original.Key() != identity.OccurrenceKey {
				continue
			}
			ref.Key = ""
		}
		o, err := resolveMeetingOccurrence(ctx, r, ref, budget)
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return uniqueMeetingOccurrences(out), nil
}
