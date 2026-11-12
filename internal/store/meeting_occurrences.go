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
	MessageID      int64
	OccurrenceKey  string
	OccurredAt     time.Time
	StartDate      string
	Counted        bool
	DeclinedEmails []string
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
	if err == nil {
		for year := earliest.Year(); year <= lastYear; year++ {
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
					continue
				}
				occurrences[occurrence.Ref] = occurrence
			}
		}
	}

	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.sent_at, d.link_key, COALESCE(d.transcript_state, '') FROM messages m LEFT JOIN meeting_details d ON d.message_id=m.id WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL`)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read meeting notes: %w", err)
	}
	var notes []occurrenceNote
	for rows.Next() {
		var note occurrenceNote
		var at sql.NullTime
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

	rows, err = s.db.QueryContext(ctx, `SELECT `+meetingDecisionColumns+` FROM meeting_link_decisions d JOIN messages l ON l.id=d.left_message_id JOIN messages r ON r.id=d.right_message_id WHERE l.deleted_at IS NULL AND r.deleted_at IS NULL`)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read meeting decisions: %w", err)
	}
	decisions, err := scanMeetingDecisionEdges(rows)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read meeting decisions: %w", err)
	}
	skippedChoices := 0
	for _, edge := range decisions {
		budget := calrecur.RequestReadBudget
		valid, _, err := s.resolveMeetingDecision(ctx, &edge, &budget)
		if errors.Is(err, ErrQueryTooBroad) {
			skippedChoices++
			continue
		}
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("resolve meeting decision: %w", err)
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

	var result []MeetingOccurrence
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
		cancelled, eligible := false, false
		var startDate string
		declined := map[string]bool{}
		for _, event := range unit.events {
			if key == "" || event.Ref < key {
				key = event.Ref
			}
			cancelled = cancelled || event.Status == "cancelled"
			ownerDeclined := false
			for _, attendee := range event.Attendees {
				if attendee.RSVP != "declined" {
					continue
				}
				email := strings.ToLower(strings.TrimSpace(attendee.Email))
				ownerDeclined = ownerDeclined || owners[email]
				declined[email] = true
			}
			eligible = eligible || !ownerDeclined
		}
		if len(unit.events) > 0 && !unit.events[0].DeletedFromSource {
			at = unit.events[0].Start
			if unit.events[0].AllDay {
				startDate = unit.events[0].StartDate
			}
		} else if len(unit.notes) > 0 {
			at = unit.notes[0].at
			for _, note := range unit.notes {
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
					result = append(result, MeetingOccurrence{id, key, at, startDate, !cancelled && !ownerDeclined, declinedEmails})
				}
			}
		}
		for _, note := range unit.notes {
			ids := declinedEmails
			if note.state == "available" {
				ids = nil
			}
			result = append(result, MeetingOccurrence{note.id, key, at, startDate, !cancelled && (eligible || note.state == "available"), ids})
		}
	}
	seenMessages := map[int64]bool{}
	for _, row := range result {
		seenMessages[row.MessageID] = true
	}
	rows, err = s.db.QueryContext(ctx, `SELECT e.message_id FROM calendar_events e JOIN messages m ON m.id=e.message_id WHERE (e.status='cancelled' OR e.start_at > ?) AND m.deleted_at IS NULL`, through)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read cancelled or future meetings: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, time.Time{}, fmt.Errorf("scan cancelled meeting: %w", err)
		}
		if !seenMessages[id] {
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
