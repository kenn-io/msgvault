package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

// MeetingOccurrence is one message's contribution to an occurrence.
type MeetingOccurrence struct {
	MessageID              int64
	OccurrenceKey          string
	OccurredAt             time.Time
	Counted                bool
	DeclinedParticipantIDs []int64
}

type occurrenceNote struct {
	id    int64
	key   meetingLinkKey
	state string
	at    time.Time
}

// MeetingOccurrencesContext groups calendar copies and notes in one archive snapshot.
func (s *Store) MeetingOccurrencesContext(ctx context.Context, through time.Time) ([]MeetingOccurrence, error) {
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	var earliest time.Time
	err = s.db.QueryRowContext(ctx, `SELECT start_at FROM calendar_events WHERE start_at IS NOT NULL ORDER BY start_at LIMIT 1`).Scan(&earliest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("find first calendar event: %w", err)
	}
	occurrences := map[string]CalendarOccurrence{}
	unexpandable := map[int64]bool{}
	if err == nil {
		for year := earliest.Year(); year <= through.Year(); year++ {
			from := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
			page, err := s.ListCalendarOccurrencesContext(ctx, from, from.AddDate(1, 0, 0))
			if errors.Is(err, ErrQueryTooBroad) {
				if err := s.markBroadMeetingWindow(ctx, from, from.AddDate(1, 0, 0), unexpandable); err != nil {
					return nil, err
				}
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("list meeting occurrences for %d: %w", year, err)
			}
			for _, warning := range page.Unexpandable {
				unexpandable[warning.MessageID] = true
			}
			for _, occurrence := range page.Occurrences {
				if occurrence.Start.After(through) {
					continue
				}
				occurrences[occurrence.Ref] = occurrence
			}
		}
	}

	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.sent_at, d.link_key, COALESCE(d.transcript_state, '') FROM messages m LEFT JOIN meeting_details d ON d.message_id=m.id WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("read meeting notes: %w", err)
	}
	var notes []occurrenceNote
	for rows.Next() {
		var note occurrenceNote
		var at sql.NullTime
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
				return nil, fmt.Errorf("read meeting calendar owner: %w", err)
			}
			owner = calendarMailbox(source.SyncConfig)
			ownersBySource[occurrence.SourceID] = owner
		}
		occurrence.row.CalendarID = owner
		occurrences[refKey] = occurrence
		ref, err := ParseMeetingLinkRef(occurrence.Ref)
		if err != nil {
			return nil, err
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
		identities := map[string]bool{}
		var group []meetingDecisionEndpoint
		for _, occurrence := range matches {
			if occurrence.original.AllDay {
				identities["date:"+occurrence.original.Date] = true
			} else if occurrence.original != (calrecur.Identity{}) {
				identities["time:"+occurrence.original.Instant.UTC().Format(time.RFC3339Nano)] = true
			} else if occurrence.AllDay {
				identities["date:"+occurrence.StartDate] = true
			} else if !occurrence.Start.IsZero() {
				identities["time:"+occurrence.Start.UTC().Format(time.RFC3339Nano)] = true
			}
			ref, err := ParseMeetingLinkRef(occurrence.Ref)
			if err != nil {
				return nil, err
			}
			group = append(group, endpointFromRef(ref))
		}
		if len(identities) > 1 {
			continue
		}
		_, _, instance := calrecur.ParseInstanceID(key[2])
		if key[0] != "google_event_id" || instance || len(identities) == 1 || len(matches) > 0 {
			group = append(group, notesByKey[key]...)
		}
		graph.groups = append(graph.groups, group)
	}

	rows, err = s.db.QueryContext(ctx, `SELECT d.id,d.is_explicit,d.left_message_id,d.left_kind,d.left_key,d.left_anchor,d.right_message_id,d.right_kind,d.right_key,d.right_anchor,d.decision FROM meeting_link_decisions d JOIN messages l ON l.id=d.left_message_id JOIN messages r ON r.id=d.right_message_id WHERE l.deleted_at IS NULL AND r.deleted_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("read meeting decisions: %w", err)
	}
	var decisions []meetingDecisionEdge
	for rows.Next() {
		var edge meetingDecisionEdge
		if err := rows.Scan(&edge.id, &edge.isExplicit, &edge.a.ID, &edge.a.Kind, &edge.a.Key, &edge.a.Anchor, &edge.b.ID, &edge.b.Kind, &edge.b.Key, &edge.b.Anchor, &edge.decision); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan meeting decision: %w", err)
		}
		decisions = append(decisions, edge)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read meeting decisions: %w", err)
	}
	for _, edge := range decisions {
		budget := calrecur.RequestReadBudget
		valid := true
		for _, endpoint := range []*meetingDecisionEndpoint{&edge.a, &edge.b} {
			canonical, _, _, err := s.materializedMeetingEndpointRecord(ctx, *endpoint, &budget)
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrMeetingOccurrenceRequired) || errors.Is(err, ErrQueryTooBroad) {
				valid = false
				break
			}
			if err != nil {
				return nil, fmt.Errorf("resolve meeting decision: %w", err)
			}
			*endpoint = canonical
			if _, ok := graph.nodes[canonical.ref()]; !ok {
				valid = false
			}
		}
		if valid {
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

	addresses, owners, err := s.meetingOccurrenceAddresses(ctx)
	if err != nil {
		return nil, err
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
		declined := map[int64]bool{}
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
				for _, id := range addresses[email] {
					declined[id] = true
				}
			}
			eligible = eligible || !ownerDeclined
		}
		if len(unit.events) > 0 && !unit.events[0].DeletedFromSource {
			at = unit.events[0].Start
			if unit.events[0].AllDay {
				at, err = time.Parse(time.DateOnly, unit.events[0].StartDate)
				if err != nil {
					return nil, fmt.Errorf("parse all-day meeting date: %w", err)
				}
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
		var declinedIDs []int64
		for id := range declined {
			declinedIDs = append(declinedIDs, id)
		}
		slices.Sort(declinedIDs)
		for _, event := range unit.events {
			ownerDeclined := false
			for _, attendee := range event.Attendees {
				ownerDeclined = ownerDeclined || attendee.RSVP == "declined" && owners[strings.ToLower(strings.TrimSpace(attendee.Email))]
			}
			for _, id := range []int64{event.MessageID, event.SeriesMessageID} {
				if id != 0 {
					result = append(result, MeetingOccurrence{id, key, at, !cancelled && !ownerDeclined, declinedIDs})
				}
			}
		}
		for _, note := range unit.notes {
			ids := declinedIDs
			if note.state == "available" {
				ids = nil
			}
			result = append(result, MeetingOccurrence{note.id, key, at, !cancelled && (eligible || note.state == "available"), ids})
		}
	}
	seenMessages := map[int64]bool{}
	for _, row := range result {
		seenMessages[row.MessageID] = true
	}
	rows, err = s.db.QueryContext(ctx, `SELECT e.message_id FROM calendar_events e JOIN messages m ON m.id=e.message_id WHERE (e.status='cancelled' OR e.start_at > ?) AND m.deleted_at IS NULL`, through)
	if err != nil {
		return nil, fmt.Errorf("read cancelled or future meetings: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan cancelled meeting: %w", err)
		}
		if !seenMessages[id] {
			result = append(result, MeetingOccurrence{MessageID: id, OccurrenceKey: formatMeetingOccurrence(OccurrenceRef{MessageID: id})})
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read cancelled meetings: %w", err)
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
	return result, nil
}

func (s *Store) meetingOccurrenceAddresses(ctx context.Context) (map[string][]int64, map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, email_address FROM participants WHERE email_address IS NOT NULL UNION SELECT participant_id, identifier_value FROM participant_identifiers WHERE identifier_type='email'`)
	if err != nil {
		return nil, nil, fmt.Errorf("read meeting participants: %w", err)
	}
	addresses := map[string][]int64{}
	for rows.Next() {
		var id int64
		var email string
		if err := rows.Scan(&id, &email); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		email = strings.ToLower(strings.TrimSpace(email))
		addresses[email] = append(addresses[email], id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT address FROM account_identities`)
	if err != nil {
		return nil, nil, fmt.Errorf("read meeting owners: %w", err)
	}
	owners := map[string]bool{}
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		owners[strings.ToLower(strings.TrimSpace(email))] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}
	clusters, err := s.ParticipantClusters(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read meeting participant clusters: %w", err)
	}
	ownerClusters := map[int64]bool{}
	canonical := func(id int64) int64 {
		if cluster, ok := clusters[id]; ok {
			return cluster
		}
		return id
	}
	for email := range owners {
		for _, id := range addresses[email] {
			ownerClusters[canonical(id)] = true
		}
	}
	for email, ids := range addresses {
		for _, id := range ids {
			if ownerClusters[canonical(id)] {
				owners[email] = true
			}
		}
	}
	return addresses, owners, nil
}

func (s *Store) markBroadMeetingWindow(ctx context.Context, from, to time.Time, unexpandable map[int64]bool) error {
	lower, upper := from.Add(-calendarCandidateMargin), to.Add(calendarCandidateMargin)
	rows, err := s.db.QueryContext(ctx, `SELECT e.message_id FROM calendar_events e JOIN messages m ON m.id=e.message_id
WHERE m.deleted_at IS NULL AND (
 (e.kind='series' AND (e.start_at IS NULL OR e.start_at < ? OR e.recurrence_json LIKE '%RDATE%') AND (e.recurrence_end_at IS NULL OR e.recurrence_end_at >= ?)) OR
 (e.kind='single' AND ((e.start_at < ? AND e.end_at >= ?) OR e.start_at IS NULL OR e.end_at IS NULL)) OR
 (e.kind='exception' AND ((e.start_at < ? AND e.end_at >= ?) OR (e.original_start_at >= ? AND e.original_start_at < ?) OR (e.original_start_date >= ? AND e.original_start_date < ?)))
)`, upper, lower, upper, lower, upper, lower, lower, upper, lower.Format(time.DateOnly), upper.Format(time.DateOnly))
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
