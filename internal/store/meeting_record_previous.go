package store

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/meetingcontent"
)

func meetingSeriesBase(o linkOccurrence) string {
	switch o.Row.Event.Kind {
	case "series":
		return o.Row.Event.ProviderEventID
	case calendarExceptionKind:
		return o.Row.Event.SeriesProviderID
	default:
		return ""
	}
}

func sameMeetingSeries(a, b linkOccurrence) bool {
	recurring := func(o linkOccurrence) bool { return o.Ref.Key != "" || o.Row.Event.Original != nil }
	if !recurring(a) || !recurring(b) {
		return false
	}
	if a.Row.Event.ICalUID != "" && a.Row.Event.ICalUID == b.Row.Event.ICalUID {
		return true
	}
	base := meetingSeriesBase(a)
	return base != "" && base == meetingSeriesBase(b) && a.Row.SourceID == b.Row.SourceID && a.Row.Event.Provider == b.Row.Event.Provider && a.Row.Event.Namespace == b.Row.Event.Namespace
}

// Indexed keys and decisions discover notes directly; empty weeks are never scanned.
func (s *Store) previousMeetingCandidates(ctx context.Context, r rowReader, scope SourceScope, target linkOccurrence) ([]int64, error) {
	e := target.Row.Event
	base := meetingSeriesBase(target)
	prefix := fmt.Sprintf("evt:%d@", target.Ref.MessageID)
	logicalPrefix := "ical:" + e.ICalUID + "@"
	eventPredicate := `e.provider = ? AND e.namespace = ? AND m.source_id = ? AND (e.provider_event_id = ? OR e.series_provider_id = ?)`
	eventArgs := []any{e.Provider, e.Namespace, target.Row.SourceID, base, base}
	if e.ICalUID != "" {
		eventPredicate = `(` + eventPredicate + ` OR e.ical_uid = ?)`
		eventArgs = append(eventArgs, e.ICalUID)
	}
	scopeClause, scopeArgs := linkScopeSQL(scope)
	eventPredicate = "(" + eventPredicate + ")" + scopeClause
	eventArgs = append(eventArgs, scopeArgs...)
	textCollation := s.bytewiseTextCollation()
	query := `SELECT m.id FROM messages m WHERE m.message_type = 'meeting_transcript' AND m.deleted_at IS NULL` + scopeClause + ` AND (
 m.id IN (SELECT k.message_id FROM meeting_link_keys k WHERE
 (k.kind = 'google_event_id' AND k.base = ? AND k.base <> '') OR
 (k.kind = 'ical_uid' AND k.value = ? AND k.value <> '') OR
 (k.kind = 'join_key' AND k.value = ? AND k.value <> '') OR
 (k.kind = 'google_event_id' AND k.value IN (SELECT e.provider_event_id FROM calendar_events e JOIN messages m ON m.id = e.message_id WHERE m.deleted_at IS NULL AND ` + eventPredicate + `))) OR
 m.id IN (SELECT d.member_message_id FROM meeting_link_decisions d WHERE d.decision = 'link' AND (
 (d.target_ref` + textCollation + ` >= ? AND d.target_ref` + textCollation + ` < ?) OR
 (? <> '' AND d.target_logical` + textCollation + ` >= ? AND d.target_logical` + textCollation + ` < ? AND EXISTS
 (SELECT 1 FROM calendar_events e JOIN messages m ON m.id = e.message_id WHERE m.deleted_at IS NULL AND ` + eventPredicate + ` AND
 (d.target_ref = 'evt:' || CAST(e.message_id AS TEXT) OR d.target_ref LIKE 'evt:' || CAST(e.message_id AS TEXT) || '@%'))) OR
 d.target_ref IN (SELECT 'evt:' || CAST(e.message_id AS TEXT) FROM calendar_events e JOIN messages m ON m.id = e.message_id WHERE m.deleted_at IS NULL AND ` + eventPredicate + `)))) ORDER BY m.id LIMIT 2001`
	args := append([]any{}, scopeArgs...)
	args = append(args, base, e.ICalUID, e.JoinKey)
	args = append(args, eventArgs...)
	args = append(args, prefix, prefix+"~", e.ICalUID, logicalPrefix, logicalPrefix+"~")
	args = append(args, eventArgs...)
	args = append(args, eventArgs...)
	rows, err := r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read previous meeting candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if len(ids) > 2000 {
		return nil, ErrQueryTooBroad
	}
	return ids, rows.Err()
}

func (s *Store) meetingRecordPrevious(ctx context.Context, r rowReader, scope SourceScope, target linkOccurrence, budget *int) (*MeetingPrevious, error) {
	if meetingSeriesBase(target) == "" && target.Ref.Key == "" && target.Row.Event.Original == nil {
		return nil, nil //nolint:nilnil // Standalone meetings have no series history.
	}
	ids, err := s.previousMeetingCandidates(ctx, r, scope, target)
	if err != nil {
		return nil, err
	}
	var selected *linkOccurrence
	var selectedLink EffectiveLink
	for _, id := range ids {
		links, err := linksForMember(ctx, r, scope, id, budget)
		if err != nil {
			return nil, err
		}
		for _, link := range links {
			earlier, err := resolveLinkOccurrence(ctx, r, scope, link.Target, budget)
			if err != nil {
				return nil, err
			}
			if occurrenceLogical(earlier) == occurrenceLogical(target) || !sameMeetingSeries(target, earlier) || !earlier.Start.Before(target.Start) {
				continue
			}
			if selected == nil || earlier.Start.After(selected.Start) || (earlier.Start.Equal(selected.Start) && (strings.Compare(FormatOccurrenceRef(earlier.Ref), FormatOccurrenceRef(selected.Ref)) < 0 || (earlier.Ref == selected.Ref && id < selectedLink.MemberMessageID))) {
				selected = &earlier
				selectedLink = link
			}
		}
	}
	if selected == nil {
		return nil, nil //nolint:nilnil // No earlier permitted evidence exists.
	}
	items, err := meetingRecordItemHeaders(ctx, r, scope, []EffectiveLink{selectedLink})
	if err != nil {
		return nil, err
	}
	item := items[0]
	if err := s.populateMeetingRecordItem(ctx, &item, false); err != nil {
		return nil, err
	}
	previous := &MeetingPrevious{Ref: FormatOccurrenceRef(selected.Ref), Start: selected.Start, Item: item, OpenActions: []meetingcontent.ActionRow{}}
	for _, entry := range item.Content.Meetings {
		for _, action := range entry.Content.Actions {
			if action.Status != meetingcontent.StatusCompleted && action.Status != meetingcontent.StatusCancelled {
				previous.OpenActions = append(previous.OpenActions, meetingcontent.ActionRow{Meeting: entry.Meeting, Action: action})
			}
		}
	}
	return previous, nil
}
