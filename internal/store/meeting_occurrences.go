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
	AnchorMessageID  int64
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

type meetingFallbackEvent struct {
	start, end         time.Time
	startDate          string
	cancelled, deleted bool
	declined           []string
}

type meetingOccurrenceUnit struct {
	events []CalendarOccurrence
	notes  []occurrenceNote
}

// MeetingEvidence contains the archive inputs captured for one cache publication.
type MeetingEvidence struct {
	through         time.Time
	nextAt          time.Time
	fallbacks       map[int64]*meetingFallbackEvent
	occurrences     map[string]CalendarOccurrence
	notes           []occurrenceNote
	choices         []meetingDecisionEdge
	seriesAttendees map[int64][]seriesAttendee
	suppressed      []int64
	unexpandable    map[int64]bool
	tooBroadYears   []int
	skippedChoices  int
}

// MeetingOccurrencesContext groups calendar copies and notes in one archive snapshot.
func (s *Store) MeetingOccurrencesContext(ctx context.Context, through time.Time, owners map[string]bool) ([]MeetingOccurrence, time.Time, error) {
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer release()
	evidence, err := s.loadMeetingEvidence(ctx, through)
	if err != nil {
		return nil, time.Time{}, err
	}
	return evidence.Reduce(ctx, owners)
}

// LoadMeetingEvidenceContext captures meeting inputs from a caller-owned SQLite snapshot.
func LoadMeetingEvidenceContext(ctx context.Context, reader SQLReader, through time.Time) (*MeetingEvidence, error) {
	s := &Store{dialect: &SQLiteDialect{}}
	ctx = context.WithValue(ctx, meetingSQLReaderKey{}, reader)
	return s.loadMeetingEvidence(ctx, through)
}

//nolint:sqlclosecheck // Rows close before the snapshot is reused.
func (s *Store) loadMeetingEvidence(ctx context.Context, through time.Time) (*MeetingEvidence, error) {
	sourceScope, sourceArgs := sourceScopeClause(ctx, "m.source_id")
	var first nullableTimestamp
	err := s.meetingSQLReader(ctx).QueryRowContext(ctx, `SELECT e.start_at FROM calendar_events e JOIN messages m ON m.id=e.message_id WHERE e.start_at IS NOT NULL AND m.deleted_at IS NULL`+sourceScope+` ORDER BY e.start_at LIMIT 1`, sourceArgs...).Scan(&first)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("find first calendar event: %w", err)
	}
	earliest := first.Time
	masters, readErr := queryCalendarEventRows(ctx, s.meetingSQLReader(ctx), `e.kind = 'series' AND e.status <> 'cancelled' AND e.recurrence_json LIKE '%RDATE%'`+sourceScope, sourceArgs, 0)
	if readErr != nil {
		return nil, fmt.Errorf("read calendar RDATEs: %w", readErr)
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
		if err := s.meetingSQLReader(ctx).QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM calendar_events e JOIN messages m ON m.id=e.message_id WHERE m.deleted_at IS NULL`+sourceScope+`)`, sourceArgs...).Scan(&hasCalendar); err != nil {
			return nil, fmt.Errorf("find calendar events: %w", err)
		}
		if hasCalendar {
			nextAt = time.Date(lastYear+1, 1, 1, 0, 0, 0, 0, time.UTC).Add(-14 * time.Hour)
		}
	}
	unexpandable := map[int64]bool{}
	var tooBroadYears []int
	hasEarliest := err == nil
	var rows *sql.Rows
	rows, err = s.meetingSQLReader(ctx).QueryContext(ctx, `SELECT m.id, COALESCE(m.sent_at, m.received_at, m.internal_date), d.link_key, COALESCE(d.transcript_state, '') FROM messages m LEFT JOIN meeting_details d ON d.message_id=m.id WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL`+sourceScope, sourceArgs...)
	if err != nil {
		return nil, fmt.Errorf("read meeting notes: %w", err)
	}
	var notes []occurrenceNote
	for rows.Next() {
		var note occurrenceNote
		var at nullableTimestamp
		var key sql.NullString
		if err := rows.Scan(&note.id, &at, &key, &note.state); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan meeting note: %w", err)
		}
		if at.Valid {
			note.at = at.Time
		}
		if key.Valid {
			if err := json.Unmarshal([]byte(key.String), &note.key); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("decode meeting note key: %w", err)
			}
		}
		notes = append(notes, note)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read meeting notes: %w", err)
	}

	leftScope, leftArgs := sourceScopeClause(ctx, "l.source_id")
	rightScope, rightArgs := sourceScopeClause(ctx, "r.source_id")
	decisionArgs := append(append([]any(nil), leftArgs...), rightArgs...)
	rows, err = s.meetingSQLReader(ctx).QueryContext(ctx, `SELECT d.id,d.left_message_id,d.left_kind,d.left_key,d.right_message_id,d.right_kind,d.right_key,d.decision FROM meeting_link_decisions d JOIN messages l ON l.id=d.left_message_id JOIN messages r ON r.id=d.right_message_id WHERE 1=1`+leftScope+rightScope, decisionArgs...)
	if err != nil {
		return nil, fmt.Errorf("read meeting decisions: %w", err)
	}
	var decisions []meetingDecisionEdge
	for rows.Next() {
		var edge meetingDecisionEdge
		if err := rows.Scan(&edge.id, &edge.a.ID, &edge.a.Kind, &edge.a.Key, &edge.b.ID, &edge.b.Kind, &edge.b.Key, &edge.decision); err != nil {
			_ = rows.Close()
			return nil, err
		}
		decisions = append(decisions, edge)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read meeting decisions: %w", err)
	}
	futureYears, err := s.futureMeetingYears(ctx, lastYear, notes, decisions)
	if err != nil {
		return nil, err
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
	batch := &calendarExpansionBatch{series: map[int64]*calrecur.WindowExpander{}}
	type meetingWindow struct {
		year      int
		from      time.Time
		rows      map[int64]CalendarEventRow
		step      func(CalendarEventRow) error
		finish    func() (CalendarOccurrencePage, int)
		remaining func() int
		err       error
	}
	var windows []meetingWindow
	candidateIDs := map[int64]bool{}
	for _, year := range orderedYears {
		from := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
		candidateRows, err := calendarEventRowsInWindow(ctx, s.meetingSQLReader(ctx), s.dialect, from, from.AddDate(1, 0, 0))
		window := meetingWindow{year: year, from: from, err: err}
		if err == nil {
			window.step, window.finish, window.remaining, window.err = s.calendarRowsCursor(ctx, candidateRows, from, from.AddDate(1, 0, 0), true, calrecur.RequestReadBudget-len(candidateRows), batch)
			window.rows = make(map[int64]CalendarEventRow, len(candidateRows))
			for _, row := range candidateRows {
				window.rows[row.MessageID] = row
				candidateIDs[row.MessageID] = true
			}
		}
		windows = append(windows, window)
	}
	orderedIDs := make([]int64, 0, len(candidateIDs))
	for id := range candidateIDs {
		orderedIDs = append(orderedIDs, id)
	}
	slices.Sort(orderedIDs)
	for _, id := range orderedIDs {
		batch.through = time.Time{}
		for i := range windows {
			w := &windows[i]
			if _, ok := w.rows[id]; ok && w.err == nil && w.remaining() > 0 && w.from.AddDate(1, 0, 0).After(batch.through) {
				batch.through = w.from.AddDate(1, 0, 0)
			}
		}
		for i := range windows {
			w := &windows[i]
			if row, ok := w.rows[id]; ok && w.err == nil {
				w.err = w.step(row)
			}
		}
		delete(batch.series, id)
	}
	for _, w := range windows {
		if errors.Is(w.err, ErrQueryTooBroad) {
			tooBroadYears = append(tooBroadYears, w.year)
			if err := s.markBroadMeetingWindow(ctx, w.from, w.from.AddDate(1, 0, 0), unexpandable); err != nil {
				return nil, err
			}
			continue
		}
		if w.err != nil {
			return nil, fmt.Errorf("list meeting occurrences for %d: %w", w.year, w.err)
		}
		page, _ := w.finish()
		for _, warning := range page.Unexpandable {
			unexpandable[warning.MessageID] = true
		}
		for _, occurrence := range page.Occurrences {
			if occurrence.Status != calendarCancelledStatus && !occurrence.DeletedFromSource && occurrence.Start.After(through) {
				if nextAt.IsZero() || occurrence.Start.Before(nextAt) {
					nextAt = occurrence.Start
				}
			}
			occurrences[occurrence.Ref] = occurrence
		}
	}

	fallbacks := map[int64]*meetingFallbackEvent{}
	if len(unexpandable) > 0 {
		rows, err = s.meetingSQLReader(ctx).QueryContext(ctx, `SELECT e.message_id, e.start_at, e.end_at, e.all_day, e.start_date, e.status, m.deleted_from_source_at FROM calendar_events e JOIN messages m ON m.id=e.message_id WHERE m.deleted_at IS NULL`+sourceScope, sourceArgs...)
		if err != nil {
			return nil, fmt.Errorf("read future calendar starts: %w", err)
		}
		for rows.Next() {
			var id int64
			var start, end nullableTimestamp
			var deleted nullableTimestamp
			var allDay bool
			var startDate, status string
			if err := rows.Scan(&id, &start, &end, &allDay, &startDate, &status, &deleted); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if !unexpandable[id] {
				continue
			}
			event := &meetingFallbackEvent{cancelled: status == calendarCancelledStatus, deleted: deleted.Valid}
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
			if !event.cancelled && !event.deleted && event.start.After(through) && (nextAt.IsZero() || event.start.Before(nextAt)) {
				nextAt = event.start
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
		if err := readFallbackAttendees(ctx, s.meetingSQLReader(ctx), fallbacks); err != nil {
			return nil, err
		}
	}

	ownersBySource := map[int64]string{}
	for refKey, occurrence := range occurrences {
		if unexpandable[occurrence.MessageID] || unexpandable[occurrence.SeriesMessageID] {
			continue
		}
		owner, ok := ownersBySource[occurrence.SourceID]
		if !ok {
			var config sql.NullString
			if err := s.meetingSQLReader(ctx).QueryRowContext(ctx, `SELECT sync_config FROM sources WHERE id=?`, occurrence.SourceID).Scan(&config); err != nil {
				return nil, fmt.Errorf("read meeting calendar owner: %w", err)
			}
			owner = calendarMailbox(config)
			ownersBySource[occurrence.SourceID] = owner
		}
		occurrence.row.CalendarID = owner
		occurrences[refKey] = occurrence
	}

	known := map[string]bool{}
	for ref := range occurrences {
		known[ref] = true
	}
	for _, note := range notes {
		known[(meetingDecisionEndpoint{ID: note.id, Kind: meetingRefMessage}).ref()] = true
	}
	canonicalEndpoints := map[meetingDecisionEndpoint]meetingDecisionEndpoint{}
	skippedChoices := 0
	var choices []meetingDecisionEdge
	for _, edge := range decisions {
		budget := calrecur.RequestReadBudget
		valid := true
		for _, endpoint := range []*meetingDecisionEndpoint{&edge.a, &edge.b} {
			if _, err := endpoint.meetingRef(); err == nil && known[endpoint.ref()] {
				continue
			}
			if canonical, ok := canonicalEndpoints[*endpoint]; ok {
				*endpoint = canonical
				continue
			}
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
				return nil, fmt.Errorf("resolve meeting decision: %w", err)
			}
			canonicalEndpoints[*endpoint] = canonical
			*endpoint = canonical
		}
		if valid {
			choices = append(choices, edge)
		}
	}

	seriesIDs := map[int64]bool{}
	for _, occurrence := range occurrences {
		if occurrence.SeriesMessageID != 0 && occurrence.SeriesMessageID != occurrence.MessageID {
			seriesIDs[occurrence.SeriesMessageID] = true
		}
	}
	seriesAttendees := map[int64][]seriesAttendee{}
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
		rows, err := s.meetingSQLReader(ctx).QueryContext(ctx, `SELECT message_id, email, rsvp='declined' FROM calendar_event_attendees WHERE message_id IN (`+placeholders(len(batch))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("read series attendees: %w", err)
		}
		for rows.Next() {
			var id int64
			var attendee seriesAttendee
			if err := rows.Scan(&id, &attendee.email, &attendee.declined); err != nil {
				_ = rows.Close()
				return nil, err
			}
			seriesAttendees[id] = append(seriesAttendees[id], attendee)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}

	var suppressed []int64
	rows, err = s.meetingSQLReader(ctx).QueryContext(ctx, `SELECT e.message_id FROM calendar_events e JOIN messages m ON m.id=e.message_id WHERE (m.deleted_at IS NOT NULL OR NOT (`+calendarEventVisibility("e", "m")+`) OR e.status='cancelled' OR e.start_at > ? OR e.kind='series')`+sourceScope, append([]any{through}, sourceArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("read cancelled or future meetings: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan cancelled meeting: %w", err)
		}
		suppressed = append(suppressed, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read cancelled meetings: %w", err)
	}

	return &MeetingEvidence{through: through, nextAt: nextAt, fallbacks: fallbacks, occurrences: occurrences, notes: notes, choices: choices, seriesAttendees: seriesAttendees, suppressed: suppressed, unexpandable: unexpandable, tooBroadYears: tooBroadYears, skippedChoices: skippedChoices}, nil
}

//nolint:sqlclosecheck // Rows close before the snapshot is reused.
func readFallbackAttendees(ctx context.Context, reader SQLReader, fallbacks map[int64]*meetingFallbackEvent) error {
	ids := make([]int64, 0, len(fallbacks))
	for id := range fallbacks {
		ids = append(ids, id)
	}
	for start := 0; start < len(ids); start += 500 {
		batch := ids[start:min(start+500, len(ids))]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := reader.QueryContext(ctx, `SELECT message_id, email FROM calendar_event_attendees WHERE rsvp='declined' AND message_id IN (`+placeholders(len(batch))+`)`, args...)
		if err != nil {
			return fmt.Errorf("read fallback attendees: %w", err)
		}
		for rows.Next() {
			var id int64
			var email string
			if err := rows.Scan(&id, &email); err != nil {
				_ = rows.Close()
				return err
			}
			fallbacks[id].declined = append(fallbacks[id].declined, strings.ToLower(strings.TrimSpace(email)))
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
	}
	return nil
}

type seriesAttendee struct {
	email    string
	declined bool
}

func occurrenceDeclines(event CalendarOccurrence, series []seriesAttendee) map[string]bool {
	current, declined := map[string]bool{}, map[string]bool{}
	for _, attendee := range event.Attendees {
		email := strings.ToLower(strings.TrimSpace(attendee.Email))
		current[email] = true
		if attendee.RSVP == "declined" {
			declined[email] = true
		}
	}
	if event.SeriesMessageID != 0 && event.MessageID != event.SeriesMessageID {
		for _, attendee := range series {
			email := strings.ToLower(strings.TrimSpace(attendee.email))
			if !current[email] && (!event.row.Event.AttendeesOmitted || attendee.declined) {
				declined[email] = true
			}
		}
	}
	return declined
}

func sortedDeclines(declined map[string]bool) []string {
	emails := make([]string, 0, len(declined))
	for email := range declined {
		emails = append(emails, email)
	}
	slices.Sort(emails)
	return emails
}

// Reduce derives occurrence rows without reading the archive again.
func (e *MeetingEvidence) Reduce(ctx context.Context, owners map[string]bool) ([]MeetingOccurrence, time.Time, error) {
	through, nextAt := e.through, e.nextAt
	fallbacks, occurrences, notes, seriesAttendees := e.fallbacks, e.occurrences, e.notes, e.seriesAttendees
	unexpandable, tooBroadYears, skippedChoices := e.unexpandable, e.tooBroadYears, e.skippedChoices

	graph := meetingDecisionGraph{nodes: map[string]meetingDecisionNode{}}
	eventsByKey := map[meetingLinkKey][]CalendarOccurrence{}
	for _, occurrence := range occurrences {
		if unexpandable[occurrence.MessageID] || unexpandable[occurrence.SeriesMessageID] {
			continue
		}
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
	notesByKey := map[meetingLinkKey][]int64{}
	for _, note := range notes {
		endpoint := meetingDecisionEndpoint{ID: note.id, Kind: meetingRefMessage}
		graph.nodes[endpoint.ref()] = meetingDecisionNode{endpoint: endpoint}
		if note.key != (meetingLinkKey{}) {
			notesByKey[note.key] = append(notesByKey[note.key], note.id)
		}
	}
	for key := range notesByKey {
		if _, ok := eventsByKey[key]; !ok {
			eventsByKey[key] = nil
		}
	}
	for key, matches := range eventsByKey {
		matches, notes := meetingMatchGroup(key, matches, notesByKey[key], true)
		var group []meetingDecisionEndpoint
		for _, occurrence := range matches {
			ref, err := ParseMeetingLinkRef(occurrence.Ref)
			if err != nil {
				return nil, time.Time{}, err
			}
			group = append(group, endpointFromRef(ref))
		}
		for _, id := range notes {
			group = append(group, meetingDecisionEndpoint{ID: id, Kind: meetingRefMessage})
		}
		graph.groups = append(graph.groups, group)
	}

	for _, edge := range e.choices {
		_, leftOK := graph.nodes[edge.a.ref()]
		_, rightOK := graph.nodes[edge.b.ref()]
		if leftOK && rightOK {
			graph.candidates = append(graph.candidates, edge)
		}
	}
	root, _, copyRoots := graph.reduce()
	units := map[string]*meetingOccurrenceUnit{}
	unitsByRoot := map[string][]string{}
	for ref, occurrence := range occurrences {
		copyRoot, ok := copyRoots[ref]
		if !ok {
			continue
		}
		if units[copyRoot] == nil {
			units[copyRoot] = &meetingOccurrenceUnit{}
			unitsByRoot[root(ref)] = append(unitsByRoot[root(ref)], copyRoot)
		}
		units[copyRoot].events = append(units[copyRoot].events, occurrence)
	}
	for _, note := range notes {
		endpoint := meetingDecisionEndpoint{ID: note.id, Kind: meetingRefMessage}
		group := root(endpoint.ref())
		if len(unitsByRoot[group]) == 0 {
			units[group] = &meetingOccurrenceUnit{}
			unitsByRoot[group] = []string{group}
		}
		for _, key := range unitsByRoot[group] {
			units[key].notes = append(units[key].notes, note)
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
		result = append(result, MeetingOccurrence{MessageID: id, AnchorMessageID: id, OccurrenceKey: fmt.Sprintf("fallback:%d", id), OccurredAt: event.start, StartDate: event.startDate, Counted: counted, DeclinedEmails: event.declined, ScheduledSeconds: duration})
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
		var anchorID int64
		var at time.Time
		cancelled := false
		var startDate string
		declined := map[string]bool{}
		for _, event := range unit.events {
			if key == "" || event.Ref < key {
				key = event.Ref
			}
			cancelled = cancelled || event.Status == calendarCancelledStatus
			for email := range occurrenceDeclines(event, seriesAttendees[event.SeriesMessageID]) {
				declined[email] = true
			}
		}
		available := false
		for _, note := range unit.notes {
			available = available || note.state == "available"
		}
		if len(unit.events) > 0 && (!unit.events[0].DeletedFromSource || len(unit.notes) == 0) {
			anchorID = unit.events[0].MessageID
			at = unit.events[0].Start
			if unit.events[0].AllDay {
				startDate = unit.events[0].StartDate
			}
		} else if len(unit.notes) > 0 {
			anchorID = unit.notes[0].id
			if len(unit.events) > 0 && available {
				for _, note := range unit.notes {
					if note.state == "available" {
						anchorID = note.id
						break
					}
				}
			}
			for _, note := range unit.notes {
				if len(unit.events) > 0 && available && note.state != "available" {
					continue
				}
				if at.IsZero() && !note.at.IsZero() {
					at = note.at
					anchorID = note.id
				}
				ref := (meetingDecisionEndpoint{ID: note.id, Kind: meetingRefMessage}).ref()
				if len(unit.events) == 0 && (key == "" || ref < key) {
					key = ref
				}
			}
		}
		declinedEmails := sortedDeclines(declined)
		ownerDeclined := false
		for email := range declined {
			ownerDeclined = ownerDeclined || owners[email]
		}
		deleted := len(unit.events) > 0 && unit.events[0].DeletedFromSource
		counted := !at.After(through) && !cancelled && (!deleted || available) && (!ownerDeclined || available) && (len(unit.events) > 0 || available)
		for _, event := range unit.events {
			for _, id := range []int64{event.MessageID, event.SeriesMessageID} {
				if id != 0 {
					var duration *float64
					if !event.Start.IsZero() && !event.End.IsZero() && !event.End.Before(event.Start) {
						seconds := event.End.Sub(event.Start).Seconds()
						duration = &seconds
					}
					result = append(result, MeetingOccurrence{MessageID: id, AnchorMessageID: anchorID, OccurrenceKey: key, OccurredAt: at, StartDate: startDate, Counted: counted && !event.DeletedFromSource, DeclinedEmails: declinedEmails, ScheduledSeconds: duration})
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
			result = append(result, MeetingOccurrence{MessageID: note.id, AnchorMessageID: anchorID, OccurrenceKey: key, OccurredAt: at, StartDate: startDate, Counted: counted && note.state == "available", DeclinedEmails: ids})
		}
	}
	seenMessages := map[int64]bool{}
	for _, row := range result {
		seenMessages[row.MessageID] = true
	}
	for _, id := range e.suppressed {
		if !seenMessages[id] && !unexpandable[id] {
			result = append(result, MeetingOccurrence{MessageID: id, AnchorMessageID: id, OccurrenceKey: formatMeetingOccurrence(OccurrenceRef{MessageID: id})})
		}
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
//
//nolint:sqlclosecheck // Rows close before the snapshot is reused.
func (s *Store) futureMeetingYears(ctx context.Context, lastYear int, notes []occurrenceNote, decisions []meetingDecisionEdge) (map[int]bool, error) {
	years := map[int]bool{}
	sourceScope, sourceArgs := sourceScopeClause(ctx, "m.source_id")
	add := func(at time.Time) {
		if !at.IsZero() && at.UTC().Year() > lastYear {
			years[at.UTC().Year()] = true
		}
	}
	rows, err := s.meetingSQLReader(ctx).QueryContext(ctx, `SELECT e.start_at FROM calendar_events e JOIN messages m ON m.id=e.message_id WHERE e.start_at IS NOT NULL AND m.deleted_at IS NULL`+sourceScope, sourceArgs...)
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
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for _, note := range notes {
		key := note.key
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
	for _, edge := range decisions {
		for _, endpoint := range []meetingDecisionEndpoint{edge.a, edge.b} {
			if endpoint.Kind != meetingRefEvent || endpoint.Key == "" {
				continue
			}
			ref, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@%s", endpoint.ID, endpoint.Key))
			if err != nil {
				continue
			}
			at, err := meetingIdentityTime(ref.Identity)
			if err == nil {
				add(at)
			}
		}
	}
	return years, nil
}

//nolint:sqlclosecheck // Rows close before the snapshot is reused.
func (s *Store) markBroadMeetingWindow(ctx context.Context, from, to time.Time, unexpandable map[int64]bool) error {
	lower, upper := from.Add(-calendarCandidateMargin), to.Add(calendarCandidateMargin)
	exceptions, exceptionArgs := calendarExceptionWindow(s.dialect, lower, upper)
	args := append([]any{upper, lower, upper, lower}, exceptionArgs...)
	scope, scopeArgs := sourceScopeClause(ctx, "m.source_id")
	args = append(args, scopeArgs...)
	rows, err := s.meetingSQLReader(ctx).QueryContext(ctx, `SELECT e.message_id `+calendarEventFrom+`((`+calendarSeriesWindow+`) OR (`+calendarSingleWindow+`) OR (`+exceptions+`))`+scope, args...)
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
