package store

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

type MeetingRecordSummary struct {
	Ref           string              `json:"ref"`
	Start         time.Time           `json:"start"`
	Title         string              `json:"title"`
	Occurrence    *CalendarOccurrence `json:"occurrence,omitempty"`
	ItemCount     int                 `json:"item_count"`
	HasTranscript bool                `json:"has_transcript"`
}

type PersonMeetingsQuery struct {
	ParticipantIDs []int64
	Emails         []string
	From, To       time.Time
	Limit          int
	Cursor         string
}

func (s *Store) ListPersonMeetingsContext(ctx context.Context, scope SourceScope, q PersonMeetingsQuery) ([]MeetingRecordSummary, string, error) {
	q, cursor, key, err := normalizePersonMeetingsQuery(scope, q, time.Now())
	if err != nil {
		return nil, "", err
	}
	results := []MeetingRecordSummary{}
	if len(q.ParticipantIDs) == 0 && len(q.Emails) == 0 {
		return results, "", nil
	}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, "", err
	}
	defer release()
	r := s.snapshotReader(ctx)
	budget := calrecur.RequestReadBudget
	byLogical := map[string]MeetingRecordSummary{}
	targets := map[string]linkOccurrence{}
	addOccurrence := func(o CalendarOccurrence) error {
		_, ref, _, err := ParseMeetingRef(o.Ref)
		if err != nil {
			return err
		}
		target, err := resolveLinkOccurrence(ctx, r, scope, ref, &budget)
		if err != nil {
			return meetingRecordError(err)
		}
		logical := occurrenceLogical(target)
		old, exists := byLogical[logical]
		if !exists || o.Ref < old.Ref {
			byLogical[logical] = MeetingRecordSummary{Ref: o.Ref, Start: o.Start, Title: o.Subject, Occurrence: &o}
			targets[logical] = target
		}
		return nil
	}
	if len(q.Emails) > 0 {
		rows, err := calendarEventRowsInWindow(ctx, r, scope, q.From, q.To)
		if err != nil {
			return nil, "", err
		}
		emails := map[string]bool{}
		for _, email := range q.Emails {
			emails[email] = true
		}
		rows = personMeetingInvitationRows(rows, emails)
		occurrences, unexpandable, err := expandRowsToOccurrences(ctx, r, rows, q.From, q.To, &budget)
		if err != nil {
			return nil, "", err
		}
		for _, u := range unexpandable {
			switch u.Reason {
			case "budget":
				return nil, "", ErrQueryTooBroad
			case "unsupported":
				return nil, "", fmt.Errorf("%w: %s", calrecur.ErrUnsupportedRecurrence, u.Ref)
			default:
				return nil, "", fmt.Errorf("%w: recurrence cannot be expanded for %s", ErrInvalidArgument, u.Ref)
			}
		}
		for _, o := range occurrences {
			if o.Cancelled || !personMeetingEmailMatches(o.Attendees, emails) {
				continue
			}
			if err := addOccurrence(o); err != nil {
				return nil, "", err
			}
		}
	}
	ids, err := s.personMeetingNoteCandidates(ctx, r, scope, q)
	if err != nil {
		return nil, "", err
	}
	for _, id := range ids {
		links, err := linksForMember(ctx, r, scope, id, &budget)
		if err != nil {
			return nil, "", err
		}
		logical := ""
		var target linkOccurrence
		conflict := false
		for _, link := range links {
			o, err := resolveLinkOccurrence(ctx, r, scope, link.Target, &budget)
			if err != nil {
				return nil, "", meetingRecordError(err)
			}
			next := occurrenceLogical(o)
			if logical != "" && logical != next {
				conflict = true
				break
			}
			if logical == "" || FormatOccurrenceRef(o.Ref) < FormatOccurrenceRef(target.Ref) {
				target = o
			}
			logical = next
		}
		if logical != "" && !conflict {
			if target.Start.Before(q.From) || !target.Start.Before(q.To) {
				continue
			}
			o, err := calendarOccurrenceWithReader(ctx, r, scope, FormatOccurrenceRef(target.Ref), &budget)
			if err != nil {
				return nil, "", err
			}
			if err := addOccurrence(o); err != nil {
				return nil, "", err
			}
			continue
		}
		headers, err := meetingRecordItemHeaders(ctx, r, scope, []EffectiveLink{{MemberMessageID: id}})
		if err != nil {
			return nil, "", err
		}
		header := headers[0]
		if header.SentAt.Before(q.From) || !header.SentAt.Before(q.To) {
			continue
		}
		logical = fmt.Sprintf("msg:%d", id)
		summary := MeetingRecordSummary{Ref: logical, Start: header.SentAt, Title: header.Title, ItemCount: 1}
		summary.HasTranscript, err = personMeetingHasTranscript(ctx, r, []EffectiveLink{{MemberMessageID: id}})
		if err != nil {
			return nil, "", err
		}
		byLogical[logical] = summary
	}
	for logical, summary := range byLogical {
		if target, ok := targets[logical]; ok {
			members, err := meetingRecordMembers(ctx, r, scope, target, &budget)
			if err != nil {
				return nil, "", err
			}
			summary.ItemCount = len(members)
			summary.HasTranscript, err = personMeetingHasTranscript(ctx, r, members)
			if err != nil {
				return nil, "", err
			}
		}
		results = append(results, summary)
	}
	slices.SortFunc(results, func(a, b MeetingRecordSummary) int {
		if c := a.Start.Compare(b.Start); c != 0 {
			return c
		}
		return strings.Compare(a.Ref, b.Ref)
	})
	if q.Cursor != "" {
		results = slices.DeleteFunc(results, func(o MeetingRecordSummary) bool {
			return o.Start.Before(cursor.Start) || (o.Start.Equal(cursor.Start) && o.Ref <= cursor.Ref)
		})
	}
	next := ""
	if len(results) > q.Limit {
		results = results[:q.Limit]
		last := results[len(results)-1]
		encoded, err := json.Marshal(personMeetingsCursor{From: q.From, To: q.To, Start: last.Start, Ref: last.Ref, Query: key})
		if err != nil {
			return nil, "", err
		}
		next = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return results, next, nil
}

func personMeetingHasTranscript(ctx context.Context, r rowReader, links []EffectiveLink) (bool, error) {
	found := false
	for _, link := range links {
		var state string
		err := r.QueryRowContext(ctx, `SELECT COALESCE((SELECT transcript_state FROM meeting_details WHERE message_id = ?),'unavailable')`, link.MemberMessageID).Scan(&state)
		if err != nil {
			return false, err
		}
		found = found || state == "available" || state == "truncated"
	}
	return found, nil
}
