package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

// MeetingOccurrence is one message's contribution to an occurrence.
type MeetingOccurrence struct {
	MessageID        int64
	OccurrenceKey    string
	OccurredAt       time.Time
	StartDate        string
	Counted          bool
	DeclinedEmails   []string
	ScheduledSeconds *float64
}

type occurrenceNote struct {
	id    int64
	key   meetingLinkKey
	state string
	at    time.Time
}

// MeetingOccurrencesContext groups calendar copies and notes in one archive snapshot.
func (s *Store) MeetingOccurrencesContext(ctx context.Context, through time.Time, owners map[string]bool) ([]MeetingOccurrence, time.Time, error) {
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer release()

	var earliest time.Time
	err = s.db.QueryRowContext(ctx, `SELECT start_at FROM calendar_events WHERE start_at IS NOT NULL ORDER BY start_at LIMIT 1`).Scan(&earliest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, fmt.Errorf("find first calendar event: %w", err)
	}
	masters, readErr := queryCalendarEventRows(ctx, s.db, `e.kind = 'series' AND e.status <> 'cancelled' AND e.recurrence_json LIKE '%RDATE%'`, nil, 0)
	if readErr != nil {
		return nil, time.Time{}, fmt.Errorf("read calendar RDATEs: %w", readErr)
	}
	for _, master := range masters {
		set, _, _, parseErr := prepareCalendarRecurrence(master.Event)
		if parseErr != nil {
			continue
		}
		for _, date := range set.RDates {
			start := date.Instant
			if date.AllDay {
				start, _ = time.Parse(time.DateOnly, date.Date)
			}
			if !start.IsZero() && start.Before(earliest) {
				earliest = start
			}
		}
	}
	occurrences := map[string]CalendarOccurrence{}
	var nextAt time.Time
	lastYear := through.UTC().Add(14 * time.Hour).Year()
	if err == nil {
		nextAt = time.Date(lastYear+1, 1, 1, 0, 0, 0, 0, time.UTC).Add(-14 * time.Hour)
	} else {
		var hasCalendar bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM calendar_events e JOIN messages m ON m.id=e.message_id WHERE m.deleted_at IS NULL)`).Scan(&hasCalendar); err != nil {
			return nil, time.Time{}, fmt.Errorf("find calendar events: %w", err)
		}
		if hasCalendar {
			nextAt = time.Date(lastYear+1, 1, 1, 0, 0, 0, 0, time.UTC).Add(-14 * time.Hour)
		}
	}
	unexpandable := map[int64]bool{}
	var tooBroadYears []int
	hasEarliest := err == nil
	futureYears, err := s.futureMeetingYears(ctx, lastYear)
	if err != nil {
		return nil, time.Time{}, err
	}
	years := map[int]bool{}
	if hasEarliest {
		for year := earliest.Year(); year <= lastYear; year++ {
			years[year] = true
		}
	}
	for year := range futureYears {
		years[year] = true
	}
	orderedYears := make([]int, 0, len(years))
	for year := range years {
		orderedYears = append(orderedYears, year)
	}
	slices.Sort(orderedYears)
	for _, year := range orderedYears {
		from := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
		page, err := s.ListCalendarOccurrencesContext(ctx, from, from.AddDate(1, 0, 0))
		if errors.Is(err, ErrQueryTooBroad) {
			tooBroadYears = append(tooBroadYears, year)
			if err := s.markBroadMeetingWindow(ctx, from, from.AddDate(1, 0, 0), unexpandable); err != nil {
				return nil, time.Time{}, err
			}
			continue
		}
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("list meeting occurrences for %d: %w", year, err)
		}
		for _, warning := range page.Unexpandable {
			unexpandable[warning.MessageID] = true
		}
		for _, occurrence := range page.Occurrences {
			if occurrence.Start.After(through) {
				if nextAt.IsZero() || occurrence.Start.Before(nextAt) {
					nextAt = occurrence.Start
				}
			}
			occurrences[occurrence.Ref] = occurrence
		}
	}

	type fallbackEvent struct {
		start, end         time.Time
		startDate          string
		cancelled, deleted bool
		declined           []string
	}
	fallbacks := map[int64]*fallbackEvent{}
	var rows *loggedRows
	if len(unexpandable) > 0 {
		rows, err = s.db.QueryContext(ctx, `SELECT e.message_id, e.start_at, e.end_at, e.all_day, e.start_date, e.status, m.deleted_from_source_at FROM calendar_events e JOIN messages m ON m.id=e.message_id WHERE m.deleted_at IS NULL`)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("read future calendar starts: %w", err)
		}
		for rows.Next() {
			var id int64
			var start, end nullableTimestamp
			var deleted nullableTimestamp
			var allDay bool
			var startDate, status string
			if err := rows.Scan(&id, &start, &end, &allDay, &startDate, &status, &deleted); err != nil {
				_ = rows.Close()
				return nil, time.Time{}, err
			}
			if !unexpandable[id] {
				continue
			}
			event := &fallbackEvent{cancelled: status == "cancelled", deleted: deleted.Valid}
			if start.Valid {
				event.start = start.Time
			}
			if end.Valid {
				event.end = end.Time
			}
			if allDay {
				event.startDate = startDate
			}
			fallbacks[id] = event
			if event.start.After(through) && (nextAt.IsZero() || event.start.Before(nextAt)) {
				nextAt = event.start
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, time.Time{}, err
		}
		rows, err = s.db.QueryContext(ctx, `SELECT message_id, email FROM calendar_event_attendees WHERE rsvp='declined'`)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("read fallback attendees: %w", err)
		}
		for rows.Next() {
			var id int64
			var email string
			if err := rows.Scan(&id, &email); err != nil {
				_ = rows.Close()
				return nil, time.Time{}, err
			}
			if event := fallbacks[id]; event != nil {
				event.declined = append(event.declined, strings.ToLower(strings.TrimSpace(email)))
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, time.Time{}, err
		}
	}

	rows, err = s.db.QueryContext(ctx, `SELECT m.id, COALESCE(m.sent_at, m.received_at, m.internal_date), d.link_key, COALESCE(d.transcript_state, '') FROM messages m LEFT JOIN meeting_details d ON d.message_id=m.id WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL`)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read meeting notes: %w", err)
	}
	var notes []occurrenceNote
	for rows.Next() {
		var note occurrenceNote
		var at nullableTimestamp
		var key sql.NullString
		if err := rows.Scan(&note.id, &at, &key, &note.state); err != nil {
			_ = rows.Close()
			return nil, time.Time{}, fmt.Errorf("scan meeting note: %w", err)
		}
		if at.Valid {
			note.at = at.Time
		}
		if key.Valid {
			if err := json.Unmarshal([]byte(key.String), &note.key); err != nil {
				_ = rows.Close()
				return nil, time.Time{}, fmt.Errorf("decode meeting note key: %w", err)
			}
		}
		notes = append(notes, note)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, time.Time{}, fmt.Errorf("read meeting notes: %w", err)
	}

	graph := meetingDecisionGraph{nodes: map[string]meetingDecisionNode{}}
	eventsByKey := map[meetingLinkKey][]CalendarOccurrence{}
	ownersBySource := map[int64]string{}
	for refKey, occurrence := range occurrences {
		if unexpandable[occurrence.MessageID] || unexpandable[occurrence.SeriesMessageID] {
			continue
		}
		owner, ok := ownersBySource[occurrence.SourceID]
		if !ok {
			source, err := s.GetSourceByIDContext(ctx, occurrence.SourceID)
			if err != nil {
				return nil, time.Time{}, fmt.Errorf("read meeting calendar owner: %w", err)
			}
			owner = calendarMailbox(source.SyncConfig)
			ownersBySource[occurrence.SourceID] = owner
		}
		occurrence.row.CalendarID = owner
		occurrences[refKey] = occurrence
		ref, err := ParseMeetingLinkRef(occurrence.Ref)
		if err != nil {
			return nil, time.Time{}, err
		}
		endpoint := endpointFromRef(ref)
		graph.nodes[endpoint.ref()] = meetingDecisionNode{endpoint: endpoint}
		for _, key := range occurrenceKeys(occurrence) {
			eventsByKey[key] = append(eventsByKey[key], occurrence)
		}
	}
	notesByKey := map[meetingLinkKey][]meetingDecisionEndpoint{}
	for _, note := range notes {
		endpoint := meetingDecisionEndpoint{ID: note.id, Kind: meetingRefMessage}
		graph.nodes[endpoint.ref()] = meetingDecisionNode{endpoint: endpoint}
		if note.key != (meetingLinkKey{}) {
			notesByKey[note.key] = append(notesByKey[note.key], endpoint)
		}
	}
	for _, matchingNotes := range notesByKey {
		graph.groups = append(graph.groups, matchingNotes)
	}
	for key, matches := range eventsByKey {
		ambiguous, allowNotes := meetingKeyMatch(key, matches)
		if ambiguous {
			continue
		}
		var group []meetingDecisionEndpoint
		for _, occurrence := range matches {
			ref, err := ParseMeetingLinkRef(occurrence.Ref)
			if err != nil {
				return nil, time.Time{}, err
			}
			group = append(group, endpointFromRef(ref))
		}
		if allowNotes {
			group = append(group, notesByKey[key]...)
		}
		graph.groups = append(graph.groups, group)
	}

	rows, err = s.db.QueryContext(ctx, `SELECT id,left_message_id,left_kind,left_key,right_message_id,right_kind,right_key,decision FROM meeting_link_decisions`)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read meeting decisions: %w", err)
	}
	var decisions []meetingDecisionEdge
	for rows.Next() {
		var edge meetingDecisionEdge
		if err := rows.Scan(&edge.id, &edge.a.ID, &edge.a.Kind, &edge.a.Key, &edge.b.ID, &edge.b.Kind, &edge.b.Key, &edge.decision); err != nil {
			_ = rows.Close()
			return nil, time.Time{}, err
		}
		decisions = append(decisions, edge)
	}
	if err := errorsJoinRows(rows); err != nil {
		return nil, time.Time{}, fmt.Errorf("read meeting decisions: %w", err)
	}
	skippedChoices := 0
	for _, edge := range decisions {
		budget := calrecur.RequestReadBudget
		valid := true
		for _, endpoint := range []*meetingDecisionEndpoint{&edge.a, &edge.b} {
			canonical, err := s.meetingOccurrenceKey(ctx, *endpoint, &budget)
			if errors.Is(err, ErrQueryTooBroad) {
				skippedChoices++
				valid = false
				break
			}
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrMeetingOccurrenceRequired) {
				valid = false
				break
			}
			if err != nil {
				return nil, time.Time{}, fmt.Errorf("resolve meeting decision: %w", err)
			}
			*endpoint = canonical
		}
		_, leftOK := graph.nodes[edge.a.ref()]
		_, rightOK := graph.nodes[edge.b.ref()]
		if valid && leftOK && rightOK {
			graph.candidates = append(graph.candidates, edge)
		}
	}

	root, _, copyRoots := graph.reduce()
	type unit struct {
		events []CalendarOccurrence
		notes  []occurrenceNote
	}
	units := map[string]*unit{}
	unitsByRoot := map[string][]string{}
	for ref, occurrence := range occurrences {
		copyRoot, ok := copyRoots[ref]
		if !ok {
			continue
		}
		if units[copyRoot] == nil {
			units[copyRoot] = &unit{}
			unitsByRoot[root(ref)] = append(unitsByRoot[root(ref)], copyRoot)
		}
		units[copyRoot].events = append(units[copyRoot].events, occurrence)
	}
	for _, note := range notes {
		endpoint := meetingDecisionEndpoint{ID: note.id, Kind: meetingRefMessage}
		group := root(endpoint.ref())
		if len(unitsByRoot[group]) == 0 {
			units[group] = &unit{}
			unitsByRoot[group] = []string{group}
		}
		for _, key := range unitsByRoot[group] {
			units[key].notes = append(units[key].notes, note)
		}
	}

	seriesIDs := map[int64]bool{}
	for _, occurrence := range occurrences {
		if occurrence.SeriesMessageID != 0 && occurrence.SeriesMessageID != occurrence.MessageID {
			seriesIDs[occurrence.SeriesMessageID] = true
		}
	}
	seriesAttendees := map[int64][]string{}
	ids := make([]int64, 0, len(seriesIDs))
	for id := range seriesIDs {
		ids = append(ids, id)
	}
	for start := 0; start < len(ids); start += 500 {
		batch := ids[start:min(start+500, len(ids))]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, `SELECT message_id, email FROM calendar_event_attendees WHERE message_id IN (`+placeholders(len(batch))+`)`, args...)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("read series attendees: %w", err)
		}
		for rows.Next() {
			var id int64
			var email string
			if err := rows.Scan(&id, &email); err != nil {
				_ = rows.Close()
				return nil, time.Time{}, err
			}
			seriesAttendees[id] = append(seriesAttendees[id], strings.ToLower(strings.TrimSpace(email)))
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, time.Time{}, err
		}
	}

	var result []MeetingOccurrence
	for id, event := range fallbacks {
		counted := !event.cancelled && !event.deleted && !event.start.After(through)
		for _, email := range event.declined {
			if owners[email] {
				counted = false
			}
		}
		var duration *float64
		if !event.start.IsZero() && !event.end.IsZero() && !event.end.Before(event.start) {
			seconds := event.end.Sub(event.start).Seconds()
			duration = &seconds
		}
		result = append(result, MeetingOccurrence{MessageID: id, OccurrenceKey: fmt.Sprintf("fallback:%d", id), OccurredAt: event.start, StartDate: event.startDate, Counted: counted, DeclinedEmails: event.declined, ScheduledSeconds: duration})
	}
	for _, unit := range units {
		slices.SortFunc(unit.events, func(a, b CalendarOccurrence) int {
			if a.DeletedFromSource != b.DeletedFromSource {
				if a.DeletedFromSource {
					return 1
				}
				return -1
			}
			if c := a.Start.Compare(b.Start); c != 0 {
				return c
			}
			return cmp.Compare(a.Ref, b.Ref)
		})
		slices.SortFunc(unit.notes, func(a, b occurrenceNote) int {
			if c := a.at.Compare(b.at); c != 0 {
				return c
			}
			return cmp.Compare(a.id, b.id)
		})
		var key string
		var at time.Time
		cancelled := false
		var startDate string
		declined := map[string]bool{}
		for _, event := range unit.events {
			if key == "" || event.Ref < key {
				key = event.Ref
			}
			cancelled = cancelled || event.Status == "cancelled"
			currentAttendees := map[string]bool{}
			for _, attendee := range event.Attendees {
				currentAttendees[strings.ToLower(strings.TrimSpace(attendee.Email))] = true
				if attendee.RSVP != "declined" {
					continue
				}
				email := strings.ToLower(strings.TrimSpace(attendee.Email))
				declined[email] = true
			}
			if event.SeriesMessageID != 0 && event.MessageID != event.SeriesMessageID {
				for _, email := range seriesAttendees[event.SeriesMessageID] {
					if !currentAttendees[email] {
						declined[email] = true
					}
				}
			}
		}
		if len(unit.events) > 0 && (!unit.events[0].DeletedFromSource || len(unit.notes) == 0) {
			at = unit.events[0].Start
			if unit.events[0].AllDay {
				startDate = unit.events[0].StartDate
			}
		} else if len(unit.notes) > 0 {
			for _, note := range unit.notes {
				if at.IsZero() && !note.at.IsZero() {
					at = note.at
				}
				ref := (meetingDecisionEndpoint{ID: note.id, Kind: meetingRefMessage}).ref()
				if len(unit.events) == 0 && (key == "" || ref < key) {
					key = ref
				}
			}
		}
		var declinedEmails []string
		for email := range declined {
			declinedEmails = append(declinedEmails, email)
		}
		slices.Sort(declinedEmails)
		for _, event := range unit.events {
			ownerDeclined := false
			for _, attendee := range event.Attendees {
				ownerDeclined = ownerDeclined || attendee.RSVP == "declined" && owners[strings.ToLower(strings.TrimSpace(attendee.Email))]
			}
			for _, id := range []int64{event.MessageID, event.SeriesMessageID} {
				if id != 0 {
					var duration *float64
					if !event.Start.IsZero() && !event.End.IsZero() && !event.End.Before(event.Start) {
						seconds := event.End.Sub(event.Start).Seconds()
						duration = &seconds
					}
					result = append(result, MeetingOccurrence{MessageID: id, OccurrenceKey: key, OccurredAt: at, StartDate: startDate, Counted: !at.After(through) && !cancelled && !event.DeletedFromSource && !ownerDeclined, DeclinedEmails: declinedEmails, ScheduledSeconds: duration})
				}
			}
		}
		for _, note := range unit.notes {
			if !cancelled && note.state == "available" && at.After(through) && (nextAt.IsZero() || at.Before(nextAt)) {
				nextAt = at
			}
			ids := declinedEmails
			if note.state == "available" {
				ids = nil
			}
			result = append(result, MeetingOccurrence{MessageID: note.id, OccurrenceKey: key, OccurredAt: at, StartDate: startDate, Counted: !at.After(through) && !cancelled && note.state == "available", DeclinedEmails: ids})
		}
	}
	seenMessages := map[int64]bool{}
	for _, row := range result {
		seenMessages[row.MessageID] = true
	}
	rows, err = s.db.QueryContext(ctx, `SELECT e.message_id FROM calendar_events e JOIN messages m ON m.id=e.message_id WHERE (e.status='cancelled' OR e.start_at > ? OR e.kind='series') AND m.deleted_at IS NULL`, through)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read cancelled or future meetings: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, time.Time{}, fmt.Errorf("scan cancelled meeting: %w", err)
		}
		if !seenMessages[id] && !unexpandable[id] {
			result = append(result, MeetingOccurrence{MessageID: id, OccurrenceKey: formatMeetingOccurrence(OccurrenceRef{MessageID: id})})
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, time.Time{}, fmt.Errorf("read cancelled meetings: %w", err)
	}
	slices.SortFunc(result, func(a, b MeetingOccurrence) int {
		if c := cmp.Compare(a.MessageID, b.MessageID); c != 0 {
			return c
		}
		return cmp.Compare(a.OccurrenceKey, b.OccurrenceKey)
	})
	result = slices.CompactFunc(result, func(a, b MeetingOccurrence) bool {
		return a.MessageID == b.MessageID && a.OccurrenceKey == b.OccurrenceKey
	})
	if len(unexpandable) > 0 || skippedChoices > 0 {
		ids := make([]int64, 0, len(unexpandable))
		for id := range unexpandable {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		ids = ids[:min(len(ids), 10)]
		slog.WarnContext(ctx, "some meetings count once per calendar message because their recurrence could not be expanded", "messages", len(unexpandable), "message_ids", ids, "too_broad_years", tooBroadYears, "skipped_choices", skippedChoices)
	}
	return result, nextAt, nil
}

// futureMeetingYears identifies years named by archived events, note keys, or
// saved choices, so future links can be resolved before applying the cutoff.
func (s *Store) futureMeetingYears(ctx context.Context, lastYear int) (map[int]bool, error) {
	years := map[int]bool{}
	add := func(at time.Time) {
		if !at.IsZero() && at.UTC().Year() > lastYear {
			years[at.UTC().Year()] = true
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.start_at FROM calendar_events e JOIN messages m ON m.id=e.message_id WHERE e.start_at IS NOT NULL AND m.deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var at nullableTimestamp
		if err := rows.Scan(&at); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if at.Valid {
			add(at.Time)
		}
	}
	if err := errorsJoinRows(rows); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT d.link_key FROM meeting_details d JOIN messages m ON m.id=d.message_id WHERE d.link_key IS NOT NULL AND m.message_type='meeting_transcript' AND m.deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return nil, err
		}
		var key meetingLinkKey
		if err := json.Unmarshal([]byte(raw), &key); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if key[0] == "ical_uid" {
			at, err := time.Parse(time.RFC3339Nano, key[3])
			if err == nil {
				add(at)
			}
		}
		if key[0] == "google_event_id" {
			if _, instance, ok := calrecur.ParseInstanceID(key[2]); ok {
				at, err := meetingIdentityTime(instance)
				if err == nil {
					add(at)
				}
			}
		}
	}
	if err := errorsJoinRows(rows); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT left_message_id,left_kind,left_key,right_message_id,right_kind,right_key FROM meeting_link_decisions`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var left, right meetingDecisionEndpoint
		if err := rows.Scan(&left.ID, &left.Kind, &left.Key, &right.ID, &right.Kind, &right.Key); err != nil {
			_ = rows.Close()
			return nil, err
		}
		for _, endpoint := range []meetingDecisionEndpoint{left, right} {
			if endpoint.Kind != meetingRefEvent {
				continue
			}
			key := endpoint.Key
			if key == "" {
				continue
			}
			ref, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@%s", endpoint.ID, key))
			if err != nil {
				continue
			}
			at, err := meetingIdentityTime(ref.Identity)
			if err == nil {
				add(at)
			}
		}
	}
	if err := errorsJoinRows(rows); err != nil {
		return nil, err
	}
	return years, nil
}

func (s *Store) markBroadMeetingWindow(ctx context.Context, from, to time.Time, unexpandable map[int64]bool) error {
	lower, upper := from.Add(-calendarCandidateMargin), to.Add(calendarCandidateMargin)
	exceptions, exceptionArgs := calendarExceptionWindow(s.dialect, lower, upper)
	args := append([]any{upper, lower, upper, lower}, exceptionArgs...)
	rows, err := s.db.QueryContext(ctx, `SELECT e.message_id `+calendarEventFrom+`((`+calendarSeriesWindow+`) OR (`+calendarSingleWindow+`) OR (`+exceptions+`))`, args...)
	if err != nil {
		return fmt.Errorf("read broad meeting window: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan broad meeting window: %w", err)
		}
		unexpandable[id] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("read broad meeting window: %w", err)
	}
	return nil
}
