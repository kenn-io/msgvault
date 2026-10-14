package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/msgvault/internal/calrecur"
)

type MeetingSuggestion struct {
	Ref                                          string
	MemberMessageID                              int64
	OverlapMinutes, SharedAttendees, TitleTokens int
	Reasons                                      []string
}

type suggestMember struct {
	ID         int64
	Title      string
	Start, End time.Time
	Emails     map[string]bool
}

func titleTokenSet(title string) map[string]bool {
	tokens := map[string]bool{}
	for _, token := range strings.FieldsFunc(strings.ToLower(title), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		tokens[token] = true
	}
	return tokens
}

func rankMeetingSuggestion(member suggestMember, event linkOccurrence) (MeetingSuggestion, bool) {
	if event.Row.Event.Status == meetingLinkCancelledStatus {
		return MeetingSuggestion{}, false
	}
	start, end := member.Start, member.End
	if event.Start.After(start) {
		start = event.Start
	}
	if event.End.Before(end) {
		end = event.End
	}
	if !event.BoundsKnown || !end.After(start) {
		return MeetingSuggestion{}, false
	}
	suggestion := MeetingSuggestion{Ref: FormatOccurrenceRef(event.Ref), MemberMessageID: member.ID, OverlapMinutes: int(end.Sub(start) / time.Minute), Reasons: []string{"time_overlap"}}
	seen := map[string]bool{}
	for _, attendee := range event.Row.Event.Attendees {
		email := strings.ToLower(strings.TrimSpace(attendee.Email))
		if attendee.Role != "room" && member.Emails[email] && !seen[email] {
			seen[email] = true
			suggestion.SharedAttendees++
		}
	}
	memberTokens := titleTokenSet(member.Title)
	for token := range titleTokenSet(event.Row.Subject) {
		if memberTokens[token] {
			suggestion.TitleTokens++
		}
	}
	if suggestion.SharedAttendees > 0 {
		suggestion.Reasons = append(suggestion.Reasons, "shared_attendees")
	}
	if suggestion.TitleTokens > 0 {
		suggestion.Reasons = append(suggestion.Reasons, "title_tokens")
	}
	return suggestion, true
}

func suggestionMember(ctx context.Context, r rowReader, scope SourceScope, id int64) (suggestMember, error) {
	clause, args := linkScopeSQL(scope)
	var member suggestMember
	var sent, received, internal sql.NullTime
	var duration sql.NullFloat64
	err := r.QueryRowContext(ctx, `SELECT m.id,COALESCE(m.subject,''),m.sent_at,m.received_at,m.internal_date,d.duration_seconds FROM messages m LEFT JOIN meeting_details d ON d.message_id = m.id WHERE m.id = ? AND m.message_type = 'meeting_transcript' AND m.deleted_at IS NULL`+clause, append([]any{id}, args...)...).Scan(&member.ID, &member.Title, &sent, &received, &internal, &duration)
	if err != nil {
		return member, err
	}
	keys, err := memberLinkKeys(ctx, r, id)
	if err != nil {
		return member, err
	}
	for _, at := range []sql.NullTime{sent, received, internal} {
		if at.Valid {
			member.Start = at.Time
			break
		}
	}
	for _, key := range keys {
		if key.ScheduledAt != nil {
			member.Start = *key.ScheduledAt
			break
		}
	}
	d := 30 * time.Minute
	if duration.Valid && duration.Float64 > 0 {
		d = time.Duration(duration.Float64 * float64(time.Second))
	}
	member.End = member.Start.Add(d)
	member.Emails = map[string]bool{}
	rows, err := r.QueryContext(ctx, `SELECT COALESCE(p.email_address,'') FROM participants p WHERE EXISTS (SELECT 1 FROM message_recipients mr WHERE mr.message_id = ? AND mr.participant_id = p.id) OR EXISTS (SELECT 1 FROM messages m WHERE m.id = ? AND m.sender_id = p.id)`, id, id)
	if err != nil {
		return member, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return member, err
		}
		email = strings.ToLower(strings.TrimSpace(email))
		if email != "" {
			member.Emails[email] = true
		}
	}
	return member, rows.Err()
}

type suggestionSeriesIdentity struct {
	sourceID                   int64
	provider, namespace, event string
}

// Suggestions expand only the requested local window. They share scope and
// snapshot semantics with links and never write a decision.
func linkOccurrencesInWindow(ctx context.Context, r rowReader, rows []CalendarEventRow, from, to time.Time, budget *int) ([]linkOccurrence, error) {
	var out []linkOccurrence
	seriesMasters := make(map[suggestionSeriesIdentity]struct{})
	for _, row := range rows {
		if err := spendLinkBudget(ctx, budget, 1); err != nil {
			return nil, err
		}
		if row.Event.Kind == meetingLinkSeriesKind {
			e := row.Event
			seriesMasters[suggestionSeriesIdentity{sourceID: row.SourceID, provider: e.Provider, namespace: e.Namespace, event: e.ProviderEventID}] = struct{}{}
		}
	}
	for _, row := range rows {
		if err := spendLinkBudget(ctx, budget, 1); err != nil {
			return nil, err
		}
		e := row.Event
		if e.Status == meetingLinkCancelledStatus {
			continue
		}
		if e.Kind == meetingLinkSeriesKind {
			set, duration, days, err := linkSeriesSet(ctx, r, row)
			if errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				continue
			}
			if err != nil {
				return nil, err
			}
			allowance, err := reserveLinkRecurrence(ctx, set, to, budget)
			if err != nil {
				return nil, err
			}
			exceptions, err := linkReadExceptions(ctx, r, row, rows)
			if err != nil {
				return nil, err
			}
			expanded, err := calrecur.Expand(ctx, set, duration, days, exceptions, from, to, allowance)
			if errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				continue
			}
			if err != nil {
				return nil, linkRecurrenceError(err)
			}
			for _, o := range expanded {
				if !o.Cancelled {
					out = append(out, linkOccurrence{Ref: OccurrenceRef{MessageID: row.MessageID, Key: o.Original.Key()}, Row: effectiveLinkRow(row, o.Original, rows), Original: o.Original, Start: o.Start, End: o.End, BoundsKnown: o.BoundsKnown})
				}
			}
			continue
		}
		if e.Kind == "exception" {
			key := suggestionSeriesIdentity{sourceID: row.SourceID, provider: e.Provider, namespace: e.Namespace, event: e.SeriesProviderID}
			if _, hasMaster := seriesMasters[key]; hasMaster {
				continue
			}
		}
		o, err := standaloneLinkOccurrence(ctx, r, row)
		if err != nil {
			return nil, err
		}
		if o.Start.Before(to) && o.End.After(from) {
			out = append(out, o)
		}
	}
	return out, nil
}

func (s *Store) SuggestMeetingLinksContext(ctx context.Context, scope SourceScope, ref string, limit int) ([]MeetingSuggestion, error) {
	kind, occ, memberID, err := ParseMeetingRef(ref)
	if err != nil {
		return nil, err
	}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	r := s.snapshotReader(ctx)
	budget := calrecur.RequestReadBudget
	if limit <= 0 {
		limit = 10
	}
	limit = min(limit, 100)
	var out []MeetingSuggestion
	if kind == "msg" {
		member, err := suggestionMember(ctx, r, scope, memberID)
		if err != nil {
			return nil, err
		}
		from, to := member.Start.Add(-24*time.Hour), member.End.Add(24*time.Hour)
		rows, err := calendarEventRowsInWindow(ctx, r, scope, from, to)
		if err != nil {
			return nil, err
		}
		occurrences, err := linkOccurrencesInWindow(ctx, r, rows, from, to, &budget)
		if err != nil {
			return nil, err
		}
		extraReasons := []string{}
		keys, err := memberLinkKeys(ctx, r, memberID)
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			_, broad, err := linkKeyCandidates(ctx, r, scope, key)
			if err != nil {
				return nil, err
			}
			if broad {
				extraReasons = append(extraReasons, "too_many_candidates")
				break
			}
		}
		for _, o := range occurrences {
			if suggestion, ok := rankMeetingSuggestion(member, o); ok {
				suggestion.Reasons = append(suggestion.Reasons, extraReasons...)
				out = append(out, suggestion)
			}
		}
	} else {
		target, err := readLinkOccurrence(ctx, r, scope, occ, &budget, true)
		if errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if target.Row.Event.Status == meetingLinkCancelledStatus {
			return nil, nil
		}
		clause, args := linkScopeSQL(scope)
		candidateFilter := `SELECT id FROM candidates WHERE effective_start IS NOT NULL AND effective_start < ? AND effective_start + duration_seconds * INTERVAL '1 second' > ? ORDER BY id LIMIT 2001`
		if !s.IsPostgreSQL() {
			candidateFilter = `SELECT id FROM candidates WHERE effective_start IS NOT NULL AND julianday(effective_start) < julianday(?) AND julianday(effective_start) + duration_seconds / 86400.0 > julianday(?) ORDER BY id LIMIT 2001`
		}
		query := `WITH candidates AS (
			SELECT m.id,
				COALESCE((SELECT k.scheduled_at FROM meeting_link_keys k WHERE k.message_id = m.id AND k.scheduled_at IS NOT NULL ORDER BY k.ordinal LIMIT 1), COALESCE(m.sent_at,m.received_at,m.internal_date)) AS effective_start,
				CASE WHEN d.duration_seconds > 0 THEN d.duration_seconds ELSE 1800 END AS duration_seconds
			FROM messages m LEFT JOIN meeting_details d ON d.message_id = m.id
			WHERE m.message_type = 'meeting_transcript' AND m.deleted_at IS NULL` + clause + `
		) ` + candidateFilter
		rows, err := r.QueryContext(ctx, query, append(args, target.End, target.Start)...)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		if len(ids) > 2000 {
			return nil, ErrQueryTooBroad
		}
		for _, id := range ids {
			if err := spendLinkBudget(ctx, &budget, 1); err != nil {
				return nil, err
			}
			member, err := suggestionMember(ctx, r, scope, id)
			if err != nil {
				return nil, err
			}
			if suggestion, ok := rankMeetingSuggestion(member, target); ok {
				out = append(out, suggestion)
			}
		}
	}
	slices.SortFunc(out, func(a, b MeetingSuggestion) int {
		if a.OverlapMinutes != b.OverlapMinutes {
			return b.OverlapMinutes - a.OverlapMinutes
		}
		if a.SharedAttendees != b.SharedAttendees {
			return b.SharedAttendees - a.SharedAttendees
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
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
