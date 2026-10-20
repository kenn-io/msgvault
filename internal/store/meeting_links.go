package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

const (
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
	return rows, nil
}

func occurrenceFromRow(row CalendarEventRow, key string) linkOccurrence {
	id := meetingIdentity{ICalUID: row.ICalUID, OccurrenceKey: key}
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
	related, err := queryCalendarExceptionsInWindow(ctx, r, from, to, `m.source_id = ? AND e.series_provider_id = ?`, []any{master.SourceID, master.Event.ProviderEventID}, min(200, *budget))
	if err != nil {
		return nil, err
	}
	*budget -= len(related)
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
		if instance.HasException && instance.Original == (calrecur.Identity{}) {
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
	masterCancelled := false
	if row.Event.Kind == meetingExceptionKind && ref.Key == "" {
		masters, err := meetingEventRows(ctx, r, `e.kind IN ('series', 'single') AND m.source_id = ? AND e.provider_event_id = ?`, []any{row.SourceID, row.Event.SeriesProviderID}, budget)
		if err != nil {
			return linkOccurrence{}, err
		}
		if len(masters) == 1 {
			if row.Event.Original != nil {
				row, ref = masters[0], OccurrenceRef{MessageID: masters[0].MessageID, Key: row.Event.Original.Key()}
			} else {
				masterCancelled = masters[0].Event.Status == meetingCancelledStatus
			}
		} else if row.Event.Original != nil {
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
		o := occurrenceFromRow(row, "")
		if masterCancelled {
			o.Row.Event.Status = meetingCancelledStatus
		}
		return o, nil
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
	// An omitted event zone inherits the master's zone.
	var zone *time.Location
	if e.EventTimeZone != "" {
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
	err := r.QueryRowContext(ctx, `SELECT kind,value,calendar_id,scheduled_at FROM meeting_link_key WHERE message_id = ?`, id).Scan(&key.Kind, &key.Value, &key.CalendarID, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return key, nil
	}
	if at.Valid {
		key.ScheduledAt = &at.Time
	}
	return key, err
}

func (s *Store) calendarUIDPredicate() string {
	return calendarUIDExpression(s.dialect, "m.metadata") + ` = ? AND m.message_type = 'calendar_event'`
}

func (s *Store) automaticMeetingOccurrences(ctx context.Context, r reboundReader, key meetingLinkKey, budget *int) ([]linkOccurrence, error) {
	if key.Kind != "google_event_id" && key.Kind != "ical_uid" {
		return nil, nil
	}
	predicate, args := s.calendarUIDPredicate(), []any{key.Value}
	var rows []CalendarEventRow
	var err error
	base, instance, _ := calrecur.ParseInstanceID(key.Value)
	if key.Kind == "google_event_id" {
		// Parse the same source configuration used by calendar persistence.
		sources, err := r.QueryContext(ctx, `SELECT id,sync_config FROM sources WHERE source_type = 'gcal'`)
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
		loaded, err := meetingEventRows(ctx, r, predicate, args, budget)
		if err != nil {
			return nil, err
		}
		rows = loaded
	} else if key.ScheduledAt != nil {
		// Select canonical exception snapshots for candidates whose original or
		// effective bounds may overlap the note, then filter those winners by UID.
		// This prevents an older alias from reappearing when a newer move leaves
		// the query window.
		nonExceptions, err := meetingEventRows(ctx, r, predicate+` AND e.kind <> 'exception'`, args, budget)
		if err != nil {
			return nil, err
		}
		exceptions, err := queryCalendarExceptionsInWindow(ctx, r, key.ScheduledAt.Add(-calendarCandidateMargin), key.ScheduledAt.Add(calendarCandidateMargin), predicate, args, min(200, *budget))
		if err != nil {
			return nil, err
		}
		*budget -= len(exceptions)
		rows = nonExceptions
		rows = append(rows, exceptions...)
	} else {
		predicate += ` AND e.kind != 'exception'`
		loaded, err := meetingEventRows(ctx, r, predicate, args, budget)
		if err != nil {
			return nil, err
		}
		rows = loaded
	}
	rows = canonicalCalendarExceptionRows(rows)
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

func (s *Store) meetingMemberOccurrences(ctx context.Context, r reboundReader, memberID int64, budget *int) ([]linkOccurrence, error) {
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
		if len(targets) == 0 {
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
		return uniqueMeetingOccurrences(out), nil
	}
	key, err := memberLinkKey(ctx, r, memberID)
	if err != nil {
		return nil, err
	}
	if _, split := splits[meetingIdentity{ICalUID: key.Value, OccurrenceKey: meetingEvidenceSplitKey}]; key.Kind == "ical_uid" && split {
		return nil, nil
	}
	if key.Value != "" {
		matches, err := s.automaticMeetingOccurrences(ctx, r, key, budget)
		if err != nil {
			return nil, err
		}
		for _, o := range matches {
			if _, split := splits[o.Identity]; !split {
				out = append(out, o)
			}
		}
	}
	return uniqueMeetingOccurrences(out), nil
}

func (s *Store) meetingReader(ctx context.Context) reboundReader {
	return reboundReader{reader: ReadDBContext(ctx, s.db.DB), rebind: s.dialect.Rebind, dialect: s.dialect}
}

// Choose the lowest canonical reference for each identity, in reference order.
func uniqueMeetingOccurrences(out []linkOccurrence) []linkOccurrence {
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
	predicate, args := s.calendarUIDPredicate(), []any{identity.ICalUID}
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
		if row.Event.Kind != meetingExceptionKind || row.Event.Start == nil || row.Event.End == nil {
			continue
		}
		if row.Event.Original != nil && row.Event.Original.Key() != identity.OccurrenceKey {
			continue
		}
		target, err := resolveMeetingOccurrence(ctx, r, OccurrenceRef{MessageID: row.MessageID}, budget)
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
