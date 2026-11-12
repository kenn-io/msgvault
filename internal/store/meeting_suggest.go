package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calrecur"
)

type MeetingSuggestion struct {
	TitleMessageID  int64     `json:"-"`
	TitleOriginID   int64     `json:"-"`
	Ref             string    `json:"ref"`
	MemberMessageID int64     `json:"member_message_id"`
	Title           string    `json:"title"`
	Start           time.Time `json:"start"`
	OverlapMinutes  int       `json:"overlap_minutes"`
	SharedAttendees int       `json:"shared_attendees"`
	TitleTokens     int       `json:"title_tokens"`
	Reasons         []string  `json:"reasons"`
}

type MeetingSuggestionPage struct {
	Suggestions  []MeetingSuggestion    `json:"suggestions"`
	Unexpandable []CalendarUnexpandable `json:"unexpandable"`
}

type suggestMember struct {
	ID         int64
	Title      string
	Start, End time.Time
	Emails     map[string]bool
}

func rankMeetingSuggestion(member suggestMember, event CalendarOccurrence) (MeetingSuggestion, bool) {
	if event.Status == "cancelled" || event.Start.IsZero() || event.End.IsZero() || member.Start.IsZero() {
		return MeetingSuggestion{}, false
	}
	start, end := member.Start, member.End
	if event.Start.After(start) {
		start = event.Start
	}
	if event.End.Before(end) {
		end = event.End
	}
	if !end.After(start) {
		return MeetingSuggestion{}, false
	}
	result := MeetingSuggestion{Ref: event.Ref, MemberMessageID: member.ID, OverlapMinutes: int(end.Sub(start) / time.Minute), Reasons: []string{"time_overlap"}}
	seen := map[string]bool{}
	for _, attendee := range event.Attendees {
		email := strings.ToLower(strings.TrimSpace(attendee.Email))
		if attendee.Role != "room" && email != "" && member.Emails[email] && !seen[email] {
			seen[email] = true
			result.SharedAttendees++
		}
	}
	tokens := uniqueChatTokens(member.Title)
	for _, token := range uniqueChatTokens(event.Subject) {
		if slices.Contains(tokens, token) {
			result.TitleTokens++
		}
	}
	if result.SharedAttendees > 0 {
		result.Reasons = append(result.Reasons, "shared_attendees")
	}
	if result.TitleTokens > 0 {
		result.Reasons = append(result.Reasons, "title_tokens")
	}
	return result, true
}

func (s *Store) suggestionMembers(ctx context.Context, ids []int64) ([]suggestMember, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	r := s.db
	scope, scopeArgs := sourceScopeClause(ctx, "m.source_id")
	args := appendInt64Args(nil, ids)
	args = append(args, scopeArgs...)
	rows, err := r.QueryContext(ctx, `SELECT m.id,COALESCE(m.subject,''),m.sent_at,m.received_at,m.internal_date,d.duration_seconds FROM messages m LEFT JOIN meeting_details d ON d.message_id=m.id WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL AND m.id IN (`+placeholders(len(ids))+`)`+scope+` ORDER BY m.id`, args...)
	if err != nil {
		return nil, err
	}
	var members []suggestMember
	for rows.Next() {
		var m suggestMember
		var sent, received, internal sql.NullTime
		var duration sql.NullFloat64
		if err := rows.Scan(&m.ID, &m.Title, &sent, &received, &internal, &duration); err != nil {
			_ = rows.Close()
			return nil, err
		}
		for _, at := range []sql.NullTime{sent, received, internal} {
			if at.Valid {
				m.Start = at.Time
				break
			}
		}
		d := 30 * time.Minute
		if duration.Valid && duration.Float64 > 0 {
			d = time.Duration(duration.Float64 * float64(time.Second))
		}
		if !m.Start.IsZero() {
			m.End = m.Start.Add(d)
		}
		members = append(members, m)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	participants, err := loadMeetingContextParticipantsPage(ctx, r, ids)
	if err != nil {
		return nil, err
	}
	for i := range members {
		members[i].Emails = map[string]bool{}
		for _, p := range participants[members[i].ID] {
			if email := strings.ToLower(strings.TrimSpace(p.Email)); email != "" {
				members[i].Emails[email] = true
			}
		}
	}
	return members, nil
}

func (s *Store) SuggestMeetingLinksContext(ctx context.Context, ref MeetingLinkRef, limit int) (MeetingSuggestionPage, error) {
	page := MeetingSuggestionPage{Suggestions: []MeetingSuggestion{}, Unexpandable: []CalendarUnexpandable{}}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return page, err
	}
	defer release()
	ctx = context.WithValue(ctx, meetingSuggestionMemoKey{}, map[meetingDecisionEndpoint]meetingSuggestionMemoEntry{})
	var candidates []MeetingSuggestion
	if ref.Kind == meetingRefMessage {
		members, err := s.suggestionMembers(ctx, []int64{ref.MessageID})
		if err != nil {
			return page, err
		}
		if len(members) == 0 {
			return page, sql.ErrNoRows
		}
		member := members[0]
		if member.Start.IsZero() {
			return page, nil
		}
		events, err := s.ListCalendarOccurrencesContext(ctx, member.Start.Add(-24*time.Hour), member.End.Add(24*time.Hour))
		if err != nil {
			return page, err
		}
		page.Unexpandable = events.Unexpandable
		for _, event := range events.Occurrences {
			if item, ok := rankMeetingSuggestion(member, event); ok {
				item.Title = event.Subject
				item.TitleMessageID = event.MessageID
				item.TitleOriginID = event.row.SubjectOriginID
				item.Start = event.Start
				candidates = append(candidates, item)
			}
		}
	} else {
		record, err := s.MeetingRecordContext(ctx, ref)
		if err != nil {
			return page, err
		}
		var seed *MeetingLink
		for i := range record.Links {
			if record.Links[i].Ref == endpointFromRef(ref).ref() {
				seed = &record.Links[i]
				break
			}
			if seed == nil && record.Links[i].MessageID == ref.MessageID {
				seed = &record.Links[i]
			}
		}
		if seed == nil {
			return page, sql.ErrNoRows
		}
		if seed.Start == nil || seed.End == nil || seed.Status == "cancelled" {
			return page, nil
		}
		events, err := s.ListCalendarOccurrencesContext(ctx, seed.Start.Add(-24*time.Hour), seed.End.Add(24*time.Hour))
		if err != nil {
			return page, err
		}
		page.Unexpandable = events.Unexpandable
		event := CalendarOccurrence{Ref: seed.Ref, Subject: seed.Subject, Start: *seed.Start, End: *seed.End, Status: seed.Status, Attendees: []calevent.Attendee{}}
		for _, candidate := range events.Occurrences {
			if candidate.Ref == seed.Ref {
				event = candidate
				break
			}
		}
		r := s.db
		var longest float64
		scope, args := sourceScopeClause(ctx, "m.source_id")
		if err := r.QueryRowContext(ctx, `SELECT COALESCE(MAX(d.duration_seconds),1800) FROM meeting_details d JOIN messages m ON m.id=d.message_id WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL`+scope, args...).Scan(&longest); err != nil {
			return page, err
		}
		lower := seed.Start.Add(-time.Duration(max(1800, longest) * float64(time.Second)))
		startExpr := "COALESCE(m.sent_at,m.received_at,m.internal_date)"
		bound := "?"
		if !s.IsPostgreSQL() {
			startExpr = "julianday(" + startExpr + ")"
			bound = "julianday(?)"
		}
		values := []any{lower, *seed.End}
		values = append(values, args...)
		rows, err := r.QueryContext(ctx, `SELECT m.id FROM messages m WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL AND `+startExpr+` >= `+bound+` AND `+startExpr+` < `+bound+scope+` ORDER BY m.id LIMIT 2001`, values...)
		if err != nil {
			return page, err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return page, err
			}
			ids = append(ids, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return page, err
		}
		if len(ids) > 2000 {
			return page, ErrQueryTooBroad
		}
		for i := 0; i < len(ids); i += 400 {
			members, err := s.suggestionMembers(ctx, ids[i:min(i+400, len(ids))])
			if err != nil {
				return page, err
			}
			for _, member := range members {
				if item, ok := rankMeetingSuggestion(member, event); ok {
					item.Title = member.Title
					item.TitleMessageID = member.ID
					item.Start = member.Start
					candidates = append(candidates, item)
				}
			}
		}
	}
	slices.SortFunc(candidates, func(a, b MeetingSuggestion) int {
		if a.SharedAttendees != b.SharedAttendees {
			return b.SharedAttendees - a.SharedAttendees
		}
		if a.OverlapMinutes != b.OverlapMinutes {
			return b.OverlapMinutes - a.OverlapMinutes
		}
		if a.TitleTokens != b.TitleTokens {
			return b.TitleTokens - a.TitleTokens
		}
		if c := strings.Compare(a.Ref, b.Ref); c != 0 {
			return c
		}
		if a.MemberMessageID < b.MemberMessageID {
			return -1
		}
		if a.MemberMessageID > b.MemberMessageID {
			return 1
		}
		return 0
	})
	if limit <= 0 {
		limit = 10
	}
	limit = min(limit, 100)
	seen := map[string]bool{}
	for _, candidate := range candidates {
		event, err := ParseMeetingLinkRef(candidate.Ref)
		if err != nil {
			return page, err
		}
		note := MeetingLinkRef{Kind: meetingRefMessage, OccurrenceRef: OccurrenceRef{MessageID: candidate.MemberMessageID}}
		a, b := endpointFromRef(note), endpointFromRef(event)
		partner := b
		if ref.Kind == meetingRefEvent {
			partner = a
		}
		if seen[partner.ref()] {
			continue
		}
		budget := calrecur.RequestReadBudget
		graph, err := s.loadMeetingDecisionGraph(ctx, []meetingDecisionEndpoint{a, b}, &budget)
		if errors.Is(err, ErrQueryTooBroad) {
			continue
		}
		if err != nil {
			return page, err
		}
		root, links := graph.reduce()
		key := root(partner.ref())
		for ref := range graph.nodes {
			if root(ref) == key {
				seen[ref] = true
			}
		}
		if root(a.ref()) == root(b.ref()) {
			continue
		}
		// The saved edge is read for each matching endpoint, then materialized once.
		for _, node := range graph.nodes {
			if meetingDecisionEndpointMatches(node.endpoint, a) || meetingDecisionEndpointMatches(node.endpoint, b) {
				budget--
			}
		}
		if budget < 0 {
			continue
		}
		_, _, _, err = s.materializedMeetingEndpointRecord(ctx, a, &budget)
		if err == nil {
			_, _, _, err = s.materializedMeetingEndpointRecord(ctx, b, &budget)
		}
		if errors.Is(err, ErrQueryTooBroad) {
			continue
		}
		if err != nil {
			return page, err
		}
		var active []meetingDecisionEdge
		for _, link := range links {
			if root(link.a.ref()) == root(link.b.ref()) {
				active = append(active, link)
			}
		}
		graph.candidates = append(graph.candidates, meetingDecisionEdge{a: a, b: b, decision: "link", isExplicit: true})
		if !meetingDecisionAccepted(graph, a, b, "link", active) {
			continue
		}
		page.Suggestions = append(page.Suggestions, candidate)
		if len(page.Suggestions) == limit {
			break
		}
	}
	return page, nil
}
