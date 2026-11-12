package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

type MeetingSuggestion struct {
	TitleMessageID  int64 `json:"-"`
	TitleOriginID   int64 `json:"-"`
	TitleWithheld   bool  `json:"-"`
	eventEndpoint   meetingDecisionEndpoint
	Ref             string    `json:"ref" doc:"Calendar event reference; the suggestion when the seed is a note"`
	MemberMessageID int64     `json:"member_message_id" doc:"Meeting note ID; the suggestion when the seed is an event"`
	Title           string    `json:"title" doc:"Title of the suggested event or note"`
	Start           time.Time `json:"start" doc:"Start of the suggested event or note"`
	OverlapMinutes  int       `json:"overlap_minutes"`
	SharedAttendees int       `json:"shared_attendees"`
	TitleTokens     int       `json:"title_tokens"`
}

func (s *Store) suggestionIntervalExpressions() (start, duration string) {
	start = "CASE WHEN d.scheduled_duration_seconds > 0 OR d.duration_seconds IS NULL OR d.duration_seconds <= 0 THEN COALESCE(d.scheduled_start, m.sent_at, m.received_at, m.internal_date) ELSE COALESCE(m.sent_at, m.received_at, m.internal_date, d.scheduled_start) END"
	duration = "CASE WHEN d.scheduled_duration_seconds > 0 THEN d.scheduled_duration_seconds WHEN d.duration_seconds > 0 THEN d.duration_seconds ELSE 1800 END"
	return start, duration
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
	result := MeetingSuggestion{Ref: event.Ref, MemberMessageID: member.ID, OverlapMinutes: int((end.Sub(start)-1)/time.Minute) + 1}
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
	start, duration := s.suggestionIntervalExpressions()
	rows, err := r.QueryContext(ctx, `SELECT m.id,COALESCE(m.subject,''),`+start+`,`+duration+` FROM messages m LEFT JOIN meeting_details d ON d.message_id=m.id WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL AND m.id IN (`+placeholders(len(ids))+`)`+scope+` ORDER BY m.id`, args...)
	if err != nil {
		return nil, err
	}
	var members []suggestMember
	for rows.Next() {
		var m suggestMember
		var start nullableTimestamp
		var duration float64
		if err := rows.Scan(&m.ID, &m.Title, &start, &duration); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if start.Valid {
			m.Start = start.Time
			m.End = m.Start.Add(time.Duration(duration * float64(time.Second)))
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

func (s *Store) SuggestMeetingLinksContext(ctx context.Context, ref MeetingLinkRef, limit int, withheld func(context.Context, []int64) (map[int64]bool, error)) (MeetingSuggestionPage, error) {
	page := MeetingSuggestionPage{Suggestions: []MeetingSuggestion{}, Unexpandable: []CalendarUnexpandable{}}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return page, err
	}
	defer release()
	id, found, err := s.resolveMeetingMessage(ctx, ref.MessageID)
	if err != nil {
		return page, err
	}
	if !found {
		return page, sql.ErrNoRows
	}
	ref.MessageID = id
	var candidates []MeetingSuggestion
	seedIDs := []int64{ref.MessageID}
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
		events, err := s.listCalendarOccurrences(ctx, member.Start, member.End, false)
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
		page.Unexpandable = record.Unexpandable
		var seed *MeetingLink
		for i := range record.Links {
			occurrence := record.Links[i].occurrence
			if record.Links[i].Ref == endpointFromRef(ref).ref() || ref.Identity != (calrecur.Identity{}) && occurrence.SeriesMessageID == ref.MessageID && occurrence.original.AllDay == ref.Identity.AllDay && (ref.Identity.AllDay && occurrence.original.Date == ref.Identity.Date || !ref.Identity.AllDay && occurrence.original.Instant.Equal(ref.Identity.Instant)) {
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
		seedIDs = []int64{seed.MessageID}
		if seed.SubjectOriginID != 0 {
			seedIDs = append(seedIDs, seed.SubjectOriginID)
		}
		event := seed.occurrence
		r := s.db
		scope, args := sourceScopeClause(ctx, "m.source_id")
		startExpr, duration := s.suggestionIntervalExpressions()
		endExpr := startExpr + " + (" + duration + ") * interval '1 second'"
		startBound, endBound := "?", "?"
		if !s.IsPostgreSQL() {
			startExpr = "julianday(" + startExpr + ")"
			endExpr = startExpr + " + (" + duration + ") / 86400.0"
			startBound, endBound = "julianday(?)", "julianday(?)"
		}
		values := []any{*seed.End, *seed.Start}
		values = append(values, args...)
		rows, err := r.QueryContext(ctx, `SELECT m.id FROM messages m LEFT JOIN meeting_details d ON d.message_id=m.id WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL AND `+startExpr+` < `+startBound+` AND `+endExpr+` > `+endBound+scope+` ORDER BY m.id LIMIT 2001`, values...)
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
	verdicts := map[int64]bool{}
	classify := func(ids []int64) error {
		var pending []int64
		queued := map[int64]bool{}
		for _, id := range ids {
			if _, found := verdicts[id]; id != 0 && !found && !queued[id] {
				queued[id] = true
				pending = append(pending, id)
			}
		}
		if len(pending) == 0 {
			return nil
		}
		results, err := withheld(ctx, pending)
		if err != nil {
			return err
		}
		for _, id := range pending {
			verdicts[id] = results[id]
		}
		return nil
	}
	if withheld != nil {
		ids := append([]int64(nil), seedIDs...)
		for _, candidate := range candidates {
			if candidate.TitleTokens > 0 {
				ids = append(ids, candidate.TitleMessageID)
				if candidate.TitleOriginID != 0 {
					ids = append(ids, candidate.TitleOriginID)
				}
			}
		}
		if len(ids) > len(seedIDs) {
			if err := classify(ids); err != nil {
				return page, err
			}
			seedMasked := false
			for _, id := range seedIDs {
				seedMasked = seedMasked || verdicts[id]
			}
			for i := range candidates {
				if seedMasked || verdicts[candidates[i].TitleMessageID] || verdicts[candidates[i].TitleOriginID] {
					candidates[i].TitleTokens = 0
				}
			}
		}
	}
	for i := range candidates {
		ref, err := ParseMeetingLinkRef(candidates[i].Ref)
		if err != nil {
			return page, err
		}
		candidates[i].eventEndpoint = endpointFromRef(ref)
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
		if c := compareMeetingEndpoints(a.eventEndpoint, b.eventEndpoint); c != 0 {
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
	budget := calrecur.RequestReadBudget
	for _, candidate := range candidates {
		note := MeetingLinkRef{Kind: meetingRefMessage, MessageID: candidate.MemberMessageID}
		a, b := endpointFromRef(note), candidate.eventEndpoint
		a, err = s.meetingOccurrenceKey(ctx, a, &budget)
		if err == nil {
			b, err = s.meetingOccurrenceKey(ctx, b, &budget)
		}
		if errors.Is(err, ErrQueryTooBroad) {
			if len(page.Suggestions) == 0 {
				return page, ErrQueryTooBroad
			}
			break
		}
		if err != nil {
			return page, err
		}
		partner := b
		if ref.Kind == meetingRefEvent {
			partner = a
		}
		if seen[partner.ref()] {
			continue
		}
		graph, err := s.loadMeetingDecisionGraph(ctx, []meetingDecisionEndpoint{a, b}, &budget)
		if errors.Is(err, ErrQueryTooBroad) {
			if len(page.Suggestions) == 0 {
				return page, ErrQueryTooBroad
			}
			break
		}
		if err != nil {
			return page, err
		}
		root, links := graph.reduce()
		if root(a.ref()) == root(b.ref()) {
			continue
		}
		var latest meetingDecisionEdge
		for _, choice := range graph.candidates {
			if choice.id > latest.id && (compareMeetingEndpoints(choice.a, a) == 0 && compareMeetingEndpoints(choice.b, b) == 0 || compareMeetingEndpoints(choice.a, b) == 0 && compareMeetingEndpoints(choice.b, a) == 0) {
				latest = choice
			}
		}
		if latest.decision == "split" {
			continue
		}
		var active []meetingDecisionEdge
		for _, link := range links {
			if root(link.a.ref()) == root(link.b.ref()) {
				active = append(active, link)
			}
		}
		graph.candidates = append(graph.candidates, meetingDecisionEdge{id: int64(^uint64(0) >> 1), a: a, b: b, decision: "link"})
		if _, conflict := meetingDecisionConflict(graph, a, b, true, active); conflict {
			continue
		}
		key := root(partner.ref())
		for ref := range graph.nodes {
			if root(ref) == key {
				seen[ref] = true
			}
		}
		page.Suggestions = append(page.Suggestions, candidate)
		if len(page.Suggestions) == limit {
			break
		}
	}
	if withheld != nil {
		var ids []int64
		for _, item := range page.Suggestions {
			ids = append(ids, item.TitleMessageID, item.TitleOriginID)
		}
		if err := classify(ids); err != nil {
			return page, err
		}
		for i := range page.Suggestions {
			item := &page.Suggestions[i]
			item.TitleWithheld = verdicts[item.TitleMessageID] || verdicts[item.TitleOriginID]
		}
	}
	return page, nil
}
