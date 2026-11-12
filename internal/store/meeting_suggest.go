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
	"go.kenn.io/msgvault/internal/meetingcontent"
)

type MeetingSuggestion struct {
	TitleMessageID  int64 `json:"-"`
	TitleOriginID   int64 `json:"-"`
	TitleWithheld   bool  `json:"-"`
	eventEndpoint   meetingDecisionEndpoint
	Ref             string    `json:"ref"`
	MemberMessageID int64     `json:"member_message_id"`
	Title           string    `json:"title"`
	Start           time.Time `json:"start"`
	OverlapMinutes  int       `json:"overlap_minutes"`
	SharedAttendees int       `json:"shared_attendees"`
	TitleTokens     int       `json:"title_tokens"`
}

func (s *Store) suggestionScheduledStart() string {
	if s.IsPostgreSQL() {
		return "CASE WHEN pg_input_is_valid(m.metadata->>'scheduled_start', 'timestamp with time zone') THEN (m.metadata->>'scheduled_start')::timestamptz END"
	}
	return "CASE WHEN json_valid(m.metadata) THEN CASE WHEN julianday(json_extract(m.metadata, '$.scheduled_start')) IS NOT NULL THEN json_extract(m.metadata, '$.scheduled_start') END END"
}

func (s *Store) suggestionIntervalExpressions() (start, duration string) {
	start = "COALESCE(CASE WHEN d.duration_basis = 'scheduled' THEN " + s.suggestionScheduledStart() + " END, m.sent_at, m.received_at, m.internal_date)"
	duration = "CASE WHEN d.duration_seconds > 0 THEN d.duration_seconds ELSE 1800 END"
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
	duration   time.Duration
	needRaw    bool
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
	result := MeetingSuggestion{Ref: event.Ref, MemberMessageID: member.ID, OverlapMinutes: int(end.Sub(start) / time.Minute)}
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

func (s *Store) suggestionRows(ctx context.Context, ids []int64) ([]suggestMember, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	r := s.db
	scope, scopeArgs := sourceScopeClause(ctx, "m.source_id")
	args := appendInt64Args(nil, ids)
	args = append(args, scopeArgs...)
	start, duration := s.suggestionIntervalExpressions()
	rows, err := r.QueryContext(ctx, `SELECT m.id,COALESCE(m.subject,''),`+start+`,`+duration+`, CASE WHEN d.duration_basis = 'scheduled' AND `+s.suggestionScheduledStart()+` IS NULL THEN 1 ELSE 0 END FROM messages m LEFT JOIN meeting_details d ON d.message_id=m.id WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL AND m.id IN (`+placeholders(len(ids))+`)`+scope+` ORDER BY m.id`, args...)
	if err != nil {
		return nil, err
	}
	var members []suggestMember
	for rows.Next() {
		var m suggestMember
		var start nullableTimestamp
		var duration float64
		var needRaw int
		if err := rows.Scan(&m.ID, &m.Title, &start, &duration, &needRaw); err != nil {
			_ = rows.Close()
			return nil, err
		}
		m.duration = time.Duration(duration * float64(time.Second))
		m.needRaw = needRaw != 0
		if start.Valid {
			m.Start = start.Time
			m.End = m.Start.Add(m.duration)
		}
		members = append(members, m)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return members, nil
}

func (s *Store) suggestionMembers(ctx context.Context, ids []int64) ([]suggestMember, error) {
	members, err := s.suggestionRows(ctx, ids)
	if err != nil || len(members) == 0 {
		return members, err
	}
	for i := range members {
		if members[i].needRaw {
			if err := s.suggestionRecoverStart(ctx, &members[i]); err != nil {
				return nil, err
			}
		}
	}
	participants, err := loadMeetingContextParticipantsPage(ctx, s.db, ids)
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

func (s *Store) suggestionRecoverStart(ctx context.Context, member *suggestMember) error {
	var raw []byte
	var format, compression, metadata sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT r.raw_data, r.raw_format, r.compression, m.metadata FROM messages m JOIN message_raw r ON r.message_id=m.id WHERE m.id=?`, member.ID).Scan(&raw, &format, &compression, &metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	raw, _, err = decodeMessageRawBounded(raw, compression, maxMeetingRawBytes, errMeetingRawTooLarge)
	if err != nil {
		return nil
	}
	content := meetingcontent.Decode(format.String, raw, []byte(metadata.String))
	if content.LinkEvidence.ScheduledAt != nil {
		member.Start = *content.LinkEvidence.ScheduledAt
		member.End = member.Start.Add(member.duration)
	}
	return nil
}

func (s *Store) suggestionMembersInWindow(ctx context.Context, from, to time.Time) ([]suggestMember, error) {
	scope, scopeArgs := sourceScopeClause(ctx, "m.source_id")
	start, duration := s.suggestionIntervalExpressions()
	end := start + " + (" + duration + ") * interval '1 second'"
	startBound, endBound := "?", "?"
	if !s.IsPostgreSQL() {
		start = "julianday(" + start + ")"
		end = start + " + (" + duration + ") / 86400.0"
		startBound, endBound = "julianday(?)", "julianday(?)"
	}
	query := `SELECT m.id FROM messages m LEFT JOIN meeting_details d ON d.message_id=m.id WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL AND ((` + start + ` < ` + startBound + ` AND ` + end + ` > ` + endBound + `) OR (d.duration_basis='scheduled' AND ` + s.suggestionScheduledStart() + ` IS NULL))` + scope + ` AND m.id > ? ORDER BY m.id LIMIT 400`
	var members []suggestMember
	var last int64
	for {
		args := append([]any{to, from}, scopeArgs...)
		args = append(args, last)
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, err
			}
			ids = append(ids, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
		batch, err := s.suggestionMembers(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, member := range batch {
			if member.Start.Before(to) && member.End.After(from) {
				members = append(members, member)
				if len(members) > 2000 {
					return nil, ErrQueryTooBroad
				}
			}
		}
		if len(ids) < 400 {
			return members, nil
		}
		last = ids[len(ids)-1]
	}
}

func (s *Store) SuggestMeetingLinksContext(ctx context.Context, ref MeetingLinkRef, limit int, withheld func(context.Context, []int64) (map[int64]bool, error)) (MeetingSuggestionPage, error) {
	page := MeetingSuggestionPage{Suggestions: []MeetingSuggestion{}, Unexpandable: []CalendarUnexpandable{}}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return page, err
	}
	defer release()
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
		seedIDs = []int64{seed.MessageID}
		if seed.SubjectOriginID != 0 {
			seedIDs = append(seedIDs, seed.SubjectOriginID)
		}
		events, err := s.listCalendarOccurrences(ctx, *seed.Start, *seed.End, false)
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
		members, err := s.suggestionMembersInWindow(ctx, *seed.Start, *seed.End)
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
		note := MeetingLinkRef{Kind: meetingRefMessage, OccurrenceRef: OccurrenceRef{MessageID: candidate.MemberMessageID}}
		a, b := endpointFromRef(note), candidate.eventEndpoint
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
			if len(page.Suggestions) == 0 {
				return page, ErrQueryTooBroad
			}
			break
		}
		_, _, _, err = s.materializedMeetingEndpointRecord(ctx, a, &budget)
		if err == nil {
			_, _, _, err = s.materializedMeetingEndpointRecord(ctx, b, &budget)
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
