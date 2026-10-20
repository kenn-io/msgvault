package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calrecur"
)

type MeetingSuggestion struct {
	Ref             string   `json:"ref"`
	MemberMessageID int64    `json:"member_message_id"`
	OverlapMinutes  int      `json:"overlap_minutes"`
	SharedAttendees int      `json:"shared_attendees"`
	TitleTokens     int      `json:"title_tokens"`
	Reasons         []string `json:"reasons"`
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
	if event.Row.Event.Status == meetingCancelledStatus {
		return MeetingSuggestion{}, false
	}
	start, end := member.Start, member.End
	if event.Start.After(start) {
		start = event.Start
	}
	if event.End.Before(end) {
		end = event.End
	}
	if event.Start.IsZero() || event.End.IsZero() || !end.After(start) {
		return MeetingSuggestion{}, false
	}
	suggestion := MeetingSuggestion{Ref: formatMeetingOccurrence(event.Ref), MemberMessageID: member.ID, OverlapMinutes: int(end.Sub(start) / time.Minute), Reasons: []string{"time_overlap"}}
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

func formatMeetingOccurrence(ref OccurrenceRef) string {
	value := fmt.Sprintf("evt:%d", ref.MessageID)
	if ref.Key != "" {
		value += "@" + ref.Key
	}
	return value
}

func suggestionMembers(ctx context.Context, r SQLReader, ids []int64) (out []suggestMember, err error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := r.QueryContext(ctx, `SELECT m.id,COALESCE(m.subject,''),m.sent_at,m.received_at,m.internal_date,d.duration_seconds,k.scheduled_at
 FROM messages m LEFT JOIN meeting_details d ON d.message_id = m.id LEFT JOIN meeting_link_key k ON k.message_id = m.id
 WHERE m.message_type = 'meeting_transcript' AND m.deleted_at IS NULL AND m.id IN (`+placeholders(len(ids))+`) ORDER BY m.id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var member suggestMember
		var sent, received, internal, scheduled sql.NullTime
		var duration sql.NullFloat64
		if err := rows.Scan(&member.ID, &member.Title, &sent, &received, &internal, &duration, &scheduled); err != nil {
			return nil, err
		}
		for _, at := range []sql.NullTime{scheduled, sent, received, internal} {
			if at.Valid {
				member.Start = at.Time
				break
			}
		}
		d := 30 * time.Minute
		if duration.Valid && duration.Float64 > 0 {
			d = time.Duration(duration.Float64 * float64(time.Second))
		}
		member.End = member.Start.Add(d)
		out = append(out, member)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	participants, err := loadMeetingContextParticipantsPage(ctx, r, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Emails = map[string]bool{}
		for _, participant := range participants[out[i].ID] {
			if email := strings.ToLower(strings.TrimSpace(participant.Email)); email != "" {
				out[i].Emails[email] = true
			}
		}
	}
	return out, nil
}

// MeetingLinkRef identifies a note or calendar occurrence for suggestions.
type MeetingLinkRef struct {
	OccurrenceRef

	Kind string
}

// SuggestMeetingLinksContext ranks overlapping events or transcripts.
func (s *Store) SuggestMeetingLinksContext(ctx context.Context, ref MeetingLinkRef, limit int) ([]MeetingSuggestion, error) {
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	budget := calrecur.RequestReadBudget
	var out []MeetingSuggestion
	if ref.Kind == "msg" {
		members, err := suggestionMembers(ctx, s.meetingReader(ctx), []int64{ref.MessageID})
		if err != nil {
			return nil, err
		}
		if len(members) == 0 {
			return nil, sql.ErrNoRows
		}
		member := members[0]
		// Include adjacent civil dates; ranking checks the actual instant overlap.
		page, err := s.ListCalendarOccurrencesContext(ctx, member.Start.Add(-24*time.Hour), member.End.Add(24*time.Hour))
		if err != nil {
			return nil, err
		}
		for _, event := range page.Occurrences {
			o := linkOccurrence{Start: event.Start, End: event.End, Row: CalendarEventRow{Subject: event.Subject, Event: calevent.Event{Status: event.Status, Attendees: event.Attendees}}}
			if suggestion, ok := rankMeetingSuggestion(member, o); ok {
				suggestion.Ref = event.Ref
				out = append(out, suggestion)
			}
		}
	} else {
		target, err := resolveMeetingOccurrence(ctx, s.meetingReader(ctx), ref.OccurrenceRef, &budget)
		if err != nil {
			return nil, err
		}
		if target.Row.Event.Status == meetingCancelledStatus {
			return []MeetingSuggestion{}, nil
		}
		r := s.meetingReader(ctx)
		// Use the longest archived duration to bound the start window without
		// excluding a long recording that still overlaps the event.
		var longest float64
		if err := r.QueryRowContext(ctx, `SELECT COALESCE(MAX(duration_seconds),1800) FROM meeting_details`).Scan(&longest); err != nil {
			return nil, err
		}
		lower := target.Start.Add(-time.Duration(max(1800, longest) * float64(time.Second)))
		scheduledStart, messageStart := "scheduled_at", "COALESCE(sent_at,received_at,internal_date)"
		bound := "?"
		if !s.IsPostgreSQL() {
			// Normalize offsets in both the indexed expressions and query bounds.
			scheduledStart = "julianday(" + scheduledStart + ")"
			messageStart = "julianday(" + messageStart + ")"
			bound = "julianday(?)"
		}
		filter := `effective_start < ? AND effective_start + duration_seconds * INTERVAL '1 second' > ?`
		if !s.IsPostgreSQL() {
			filter = `julianday(effective_start) < julianday(?) AND julianday(effective_start) + duration_seconds / 86400.0 > julianday(?)`
		}
		rows, err := r.QueryContext(ctx, `WITH window_ids AS (
 SELECT message_id AS id FROM meeting_link_key WHERE `+scheduledStart+` >= `+bound+` AND `+scheduledStart+` < `+bound+`
 UNION ALL
 SELECT id FROM messages m WHERE message_type = 'meeting_transcript' AND deleted_at IS NULL
 AND `+messageStart+` >= `+bound+` AND `+messageStart+` < `+bound+`
 AND NOT EXISTS (SELECT 1 FROM meeting_link_key k WHERE k.message_id = m.id AND k.scheduled_at IS NOT NULL)
), candidates AS (
 SELECT m.id, COALESCE(k.scheduled_at,m.sent_at,m.received_at,m.internal_date) AS effective_start,
 CASE WHEN d.duration_seconds > 0 THEN d.duration_seconds ELSE 1800 END AS duration_seconds
 FROM window_ids w JOIN messages m ON m.id = w.id
 LEFT JOIN meeting_details d ON d.message_id = m.id LEFT JOIN meeting_link_key k ON k.message_id = m.id
 WHERE m.message_type = 'meeting_transcript' AND m.deleted_at IS NULL)
 SELECT id FROM candidates WHERE `+filter+` ORDER BY id LIMIT 2001`, lower, target.End, lower, target.End, target.End, target.Start)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
		if len(ids) > 2000 {
			return nil, ErrQueryTooBroad
		}
		for start := 0; start < len(ids); start += 500 {
			members, err := suggestionMembers(ctx, s.meetingReader(ctx), ids[start:min(start+500, len(ids))])
			if err != nil {
				return nil, err
			}
			for _, member := range members {
				if suggestion, ok := rankMeetingSuggestion(member, target); ok {
					out = append(out, suggestion)
				}
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
	if limit <= 0 {
		limit = 10
	}
	limit = min(limit, 100)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
