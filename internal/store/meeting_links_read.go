package store

import (
	"context"
	"errors"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

// MeetingLink is a resolved calendar occurrence linked to a meeting note.
type MeetingLink struct {
	Ref     string    `json:"ref"`
	Subject string    `json:"subject,omitempty"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
}

// MeetingLinksContext returns the owner's resolved links for one visible note.
func (s *Store) MeetingLinksContext(ctx context.Context, messageID int64) ([]MeetingLink, error) {
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	r := s.meetingReader(ctx)
	var found int64
	if err := r.QueryRowContext(ctx, `SELECT id FROM messages WHERE id = ? AND message_type = 'meeting_transcript' AND deleted_at IS NULL`, messageID).Scan(&found); err != nil {
		return nil, err
	}
	budget := calrecur.RequestReadBudget
	occurrences, err := s.meetingMemberOccurrences(ctx, r, messageID, &budget)
	if err != nil {
		return nil, err
	}
	links := make([]MeetingLink, 0, len(occurrences))
	for _, occurrence := range occurrences {
		links = append(links, MeetingLink{Ref: formatMeetingOccurrence(occurrence.Ref), Subject: occurrence.Row.Subject, Start: occurrence.Start, End: occurrence.End})
	}
	return links, nil
}

// MeetingRecord joins an archived occurrence with its meeting notes.
type MeetingRecord struct {
	Links      []MeetingLink `json:"links"`
	MessageIDs []int64       `json:"message_ids"`
}

// MeetingRecordContext reads a record from either a note or an event reference.
func (s *Store) MeetingRecordContext(ctx context.Context, ref MeetingLinkRef) (MeetingRecord, error) {
	record := MeetingRecord{Links: []MeetingLink{}, MessageIDs: []int64{}}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return record, err
	}
	defer release()
	r := s.meetingReader(ctx)
	budget := calrecur.RequestReadBudget
	if ref.Kind == "msg" {
		var found int64
		if err := r.QueryRowContext(ctx, `SELECT id FROM messages WHERE id = ? AND message_type = 'meeting_transcript' AND deleted_at IS NULL`, ref.MessageID).Scan(&found); err != nil {
			return record, err
		}
		record.MessageIDs = append(record.MessageIDs, ref.MessageID)
		occurrences, err := s.meetingMemberOccurrences(ctx, r, ref.MessageID, &budget)
		if err != nil {
			return record, err
		}
		for _, occurrence := range occurrences {
			record.Links = append(record.Links, MeetingLink{Ref: formatMeetingOccurrence(occurrence.Ref), Subject: occurrence.Row.Subject, Start: occurrence.Start, End: occurrence.End})
			members, err := s.meetingOccurrenceMembers(ctx, r, occurrence, &budget)
			if err != nil {
				return record, err
			}
			record.MessageIDs = append(record.MessageIDs, members...)
		}
		slices.Sort(record.MessageIDs)
		record.MessageIDs = slices.Compact(record.MessageIDs)
		return record, nil
	}
	target, err := resolveMeetingOccurrence(ctx, r, ref.OccurrenceRef, &budget)
	if err != nil {
		return record, err
	}
	record.Links = append(record.Links, MeetingLink{Ref: formatMeetingOccurrence(target.Ref), Subject: target.Row.Subject, Start: target.Start, End: target.End})
	record.MessageIDs, err = s.meetingOccurrenceMembers(ctx, r, target, &budget)
	return record, err
}

func (s *Store) meetingOccurrenceMembers(ctx context.Context, r reboundReader, target linkOccurrence, budget *int) ([]int64, error) {
	membersOut := []int64{}
	var copies []CalendarEventRow
	var err error
	if target.Identity.ICalUID != "" {
		copies, err = s.meetingIdentityRows(ctx, r, target.Identity, budget)
		if err != nil {
			return membersOut, err
		}
	}
	values := []any{target.Row.Event.ProviderEventID}
	if target.Row.Event.SeriesProviderID != "" && target.Row.Event.Original != nil {
		values = append(values, target.Row.Event.SeriesProviderID+"_"+target.Row.Event.Original.Key())
	}
	if target.Ref.Key != "" {
		values = append(values, target.Row.Event.ProviderEventID+"_"+target.Ref.Key)
	}
	if target.Identity.ICalUID != "" {
		values = append(values, target.Identity.ICalUID)
	}
	targets := []any{target.Ref.MessageID, target.Row.MessageID}
	for _, row := range copies {
		targets = append(targets, row.MessageID)
		value := row.Event.ProviderEventID
		if row.Event.Kind == meetingSeriesKind && target.Identity.OccurrenceKey != "" {
			value += "_" + target.Identity.OccurrenceKey
		}
		values = append(values, value)
	}
	args := slices.Concat(values, targets)
	rows, err := r.QueryContext(ctx, `SELECT m.id FROM (
 SELECT k.message_id FROM meeting_link_key k WHERE k.value IN (`+placeholders(len(values))+`)
 UNION SELECT d.member_message_id FROM meeting_link_decisions d WHERE d.target_message_id IN (`+placeholders(len(targets))+`)
 ) candidates JOIN messages m ON m.id = candidates.message_id
 WHERE m.message_type = 'meeting_transcript' AND m.deleted_at IS NULL ORDER BY m.id LIMIT ?`, append(args, *budget+1)...)
	if err != nil {
		return membersOut, err
	}
	defer func() { _ = rows.Close() }()
	var members []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return membersOut, err
		}
		members = append(members, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return membersOut, err
	}
	*budget -= len(members)
	if *budget < 0 {
		return membersOut, calendarRecurrenceBudgetError()
	}
	for _, id := range members {
		occurrences, err := s.meetingMemberOccurrences(ctx, r, id, budget)
		if err != nil {
			return membersOut, err
		}
		for _, occurrence := range occurrences {
			if occurrence.Identity == target.Identity {
				membersOut = append(membersOut, id)
				break
			}
		}
	}
	return membersOut, nil
}
