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

var ErrMeetingOccurrenceRequired = errors.New("copy the full occurrence reference from calendar events")

type MeetingLink struct {
	MessageID         int64     `json:"-"`
	SubjectOriginID   int64     `json:"-"`
	Ref               string    `json:"ref"`
	Subject           string    `json:"subject,omitempty"`
	Start             time.Time `json:"start"`
	End               time.Time `json:"end"`
	Status            string    `json:"status"`
	DeletedFromSource bool      `json:"deleted_from_source"`
}

type MeetingRecord struct {
	Links        []MeetingLink          `json:"links"`
	MessageIDs   []int64                `json:"message_ids"`
	Unexpandable []CalendarUnexpandable `json:"unexpandable"`
}

type MeetingLinkRef struct {
	OccurrenceRef

	Kind string
}

// occurrenceKeys returns the aliases proved by the archived occurrence.
func occurrenceKeys(o CalendarOccurrence) []meetingLinkKey {
	row := o.row
	var keys []meetingLinkKey
	owner := strings.TrimSpace(row.CalendarID)
	if owner != "" && row.Event.ProviderEventID != "" {
		if row.Event.Kind != "series" {
			keys = append(keys, canonicalMeetingKey("google_event_id", owner, row.Event.ProviderEventID, nil))
		}
		if o.SeriesMessageID != 0 && o.original != (calrecur.Identity{}) {
			series := row.Event.SeriesProviderID
			if series == "" {
				series = row.Event.ProviderEventID
			}
			keys = append(keys, canonicalMeetingKey("google_event_id", owner, series+"_"+o.original.Key(), nil))
		}
	}
	if row.ICalUID != "" {
		keys = append(keys, canonicalMeetingKey("ical_uid", "", row.ICalUID, &o.Start))
		if o.AllDay && o.StartDate != "" {
			keys = append(keys, meetingLinkKey{"ical_date", "", row.ICalUID, o.StartDate})
		}
		if o.original != (calrecur.Identity{}) {
			original := o.originalStart
			if original.IsZero() {
				original, _ = calendarOriginalStart(o.original, row.Event)
			}
			if o.original.AllDay {
				keys = append(keys, meetingLinkKey{"ical_date", "", row.ICalUID, o.original.Date})
			}
			if !original.IsZero() {
				keys = append(keys, canonicalMeetingKey("ical_uid", "", row.ICalUID, &original))
			}
		}
	}
	return slices.Compact(keys)
}

func (s *Store) meetingRowsForKey(ctx context.Context, key meetingLinkKey, budget *int) ([]CalendarEventRow, error) {
	if key[0] == "google_event_id" {
		base, _, _ := calrecur.ParseInstanceID(key[2])
		return s.meetingEventRows(ctx, `e.provider_event_id IN (?, ?)`, []any{key[2], base}, budget)
	}
	layout := time.RFC3339Nano
	if key[0] == "ical_date" {
		layout = time.DateOnly
	}
	at, err := time.Parse(layout, key[3])
	if err != nil {
		return nil, fmt.Errorf("parse meeting occurrence time: %w", err)
	}
	window, args := calendarExceptionWindow(s.dialect, at.Add(-calendarCandidateMargin), at.Add(time.Second+calendarCandidateMargin))
	return s.meetingEventRows(ctx, `e.ical_uid = ? AND (e.kind <> 'exception' OR (`+window+`))`, append([]any{key[2]}, args...), budget)
}

func (s *Store) meetingRelatedEventPredicate(sourceID int64, id string, at time.Time) (string, []any) {
	window, args := calendarExceptionWindow(s.dialect, at.Add(-calendarCandidateMargin), at.Add(time.Second+calendarCandidateMargin))
	predicate := `m.source_id = ? AND (e.provider_event_id = ? OR e.series_provider_id = ?) AND (e.kind <> 'exception' OR (` + window + `))`
	return predicate, append([]any{sourceID, id, id}, args...)
}

func (s *Store) meetingRelatedEventRows(ctx context.Context, sourceID int64, id string, at time.Time, budget *int) ([]CalendarEventRow, error) {
	predicate, args := s.meetingRelatedEventPredicate(sourceID, id, at)
	return s.meetingEventRows(ctx, predicate, args, budget)
}

// Include each candidate's series and exceptions in the shared expansion.
func (s *Store) meetingRelatedRows(ctx context.Context, seeds []CalendarEventRow, at time.Time, budget *int) ([]CalendarEventRow, error) {
	all := make(map[int64]CalendarEventRow, len(seeds))
	seen := make(map[string]bool)
	for _, row := range seeds {
		all[row.MessageID] = row
	}
	for _, row := range seeds {
		id := row.Event.ProviderEventID
		if row.Event.Kind == "exception" {
			id = row.Event.SeriesProviderID
		}
		if id == "" {
			continue
		}
		group := fmt.Sprintf("%d:%s", row.SourceID, id)
		if seen[group] {
			continue
		}
		seen[group] = true
		rows, err := s.meetingRelatedEventRows(ctx, row.SourceID, id, at, budget)
		if err != nil {
			return nil, err
		}
		for _, related := range rows {
			all[related.MessageID] = related
		}
	}
	rows := make([]CalendarEventRow, 0, len(all))
	for _, row := range all {
		rows = append(rows, row)
	}
	return rows, nil
}

func (s *Store) meetingExpand(ctx context.Context, rows []CalendarEventRow, at time.Time, budget *int) (CalendarOccurrencePage, error) {
	rows, err := s.meetingRelatedRows(ctx, rows, at, budget)
	if err != nil {
		return CalendarOccurrencePage{}, err
	}
	return s.expandCalendarRows(ctx, rows, at, at.Add(time.Second), false, budget)
}

func (s *Store) meetingOccurrencesForKeys(ctx context.Context, keys []meetingLinkKey, budget *int, unexpandable *[]CalendarUnexpandable) (map[meetingLinkKey][]CalendarOccurrence, error) {
	groups := make(map[time.Time]map[int64]CalendarEventRow)
	wanted := make(map[meetingLinkKey]bool, len(keys))
	for _, key := range keys {
		wanted[key] = true
		rows, err := s.meetingRowsForKey(ctx, key, budget)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			at := time.Time{}
			if key[0] == "ical_date" {
				at, err = time.Parse(time.DateOnly, key[3])
			} else if key[0] == "ical_uid" {
				at, err = time.Parse(time.RFC3339Nano, key[3])
			} else if _, instance, ok := calrecur.ParseInstanceID(key[2]); ok {
				if instance.AllDay {
					at, err = time.Parse(time.DateOnly, instance.Date)
				} else {
					at = instance.Instant
				}
			} else if row.Event.Start != nil {
				at = *row.Event.Start
			} else if row.Event.Original != nil {
				at, err = meetingIdentityTime(*row.Event.Original)
			}
			if err != nil {
				return nil, fmt.Errorf("parse meeting occurrence time: %w", err)
			}
			if at.IsZero() {
				continue
			}
			at = at.UTC()
			if groups[at] == nil {
				groups[at] = make(map[int64]CalendarEventRow)
			}
			groups[at][row.MessageID] = row
		}
	}
	matches := make(map[meetingLinkKey][]CalendarOccurrence)
	for at, group := range groups {
		rows := make([]CalendarEventRow, 0, len(group))
		for _, row := range group {
			rows = append(rows, row)
		}
		page, err := s.meetingExpand(ctx, rows, at, budget)
		if err != nil {
			return nil, err
		}
		*unexpandable = append(*unexpandable, page.Unexpandable...)
		for _, occurrence := range page.Occurrences {
			for _, key := range occurrenceKeys(occurrence) {
				if wanted[key] {
					matches[key] = append(matches[key], occurrence)
				}
			}
		}
	}
	for key, occurrences := range matches {
		matches[key] = uniqueMeetingOccurrences(occurrences)
	}
	return matches, nil
}

func uniqueMeetingOccurrences(in []CalendarOccurrence) []CalendarOccurrence {
	slices.SortFunc(in, func(a, b CalendarOccurrence) int { return cmp.Compare(a.Ref, b.Ref) })
	in = slices.CompactFunc(in, func(a, b CalendarOccurrence) bool { return a.Ref == b.Ref })
	slices.SortFunc(in, func(a, b CalendarOccurrence) int {
		if c := a.Start.Compare(b.Start); c != 0 {
			return c
		}
		if c := cmp.Compare(a.MessageID, b.MessageID); c != 0 {
			return c
		}
		return cmp.Compare(a.Ref, b.Ref)
	})
	return in
}

func meetingIdentityTime(identity calrecur.Identity) (time.Time, error) {
	if identity.AllDay {
		at, err := time.Parse(time.DateOnly, identity.Date)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse all-day date: %w", err)
		}
		return at, nil
	}
	return identity.Instant, nil
}

func (s *Store) meetingNoteKeys(ctx context.Context, keys []string, budget *int) (map[string][]int64, error) {
	out := make(map[string][]int64)
	if len(keys) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(keys)+1)
	for _, key := range keys {
		args = append(args, key)
	}
	scope, extra := sourceScopeClause(ctx, "m.source_id")
	args = append(args, extra...)
	args = append(args, *budget+1)
	rows, err := s.db.QueryContext(ctx, `SELECT k.key, m.id FROM meeting_link_key k JOIN messages m ON m.id = k.message_id WHERE k.key IN (`+placeholders(len(keys))+`) AND m.message_type = 'meeting_transcript' AND m.deleted_at IS NULL`+scope+` ORDER BY m.id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key string
		var id int64
		if err := rows.Scan(&key, &id); err != nil {
			return nil, err
		}
		out[key] = append(out[key], id)
		*budget--
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if *budget < 0 {
		return nil, calendarRecurrenceBudgetError()
	}
	return out, nil
}

func (s *Store) MeetingRecordContext(ctx context.Context, ref MeetingLinkRef) (MeetingRecord, error) {
	record := MeetingRecord{Links: []MeetingLink{}, MessageIDs: []int64{}, Unexpandable: []CalendarUnexpandable{}}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return record, err
	}
	defer release()
	budget := calrecur.RequestReadBudget
	var keys []meetingLinkKey
	var seed CalendarOccurrence
	if ref.Kind == "msg" {
		var raw sql.NullString
		scope, args := sourceScopeClause(ctx, "m.source_id")
		err = s.db.QueryRowContext(ctx, `SELECT k.key FROM messages m LEFT JOIN meeting_link_key k ON k.message_id=m.id WHERE m.id=? AND m.message_type='meeting_transcript' AND m.deleted_at IS NULL`+scope, append([]any{ref.MessageID}, args...)...).Scan(&raw)
		if err != nil {
			return record, err
		}
		record.MessageIDs = append(record.MessageIDs, ref.MessageID)
		if raw.Valid {
			var key meetingLinkKey
			if err := json.Unmarshal([]byte(raw.String), &key); err != nil {
				return record, err
			}
			keys = append(keys, key)
		}
	} else {
		rows, err := s.meetingEventRows(ctx, `e.message_id = ?`, []any{ref.MessageID}, &budget)
		if err != nil {
			return record, err
		}
		if len(rows) == 0 {
			return record, sql.ErrNoRows
		}
		row := rows[0]
		if row.Event.Kind == "series" && ref.Identity == (calrecur.Identity{}) {
			return record, ErrMeetingOccurrenceRequired
		}
		var at time.Time
		if ref.Identity != (calrecur.Identity{}) {
			at, err = meetingIdentityTime(ref.Identity)
		} else if row.Event.Original != nil {
			at, err = meetingIdentityTime(*row.Event.Original)
		} else if row.Event.Start != nil {
			at = *row.Event.Start
		}
		if err != nil {
			return record, fmt.Errorf("parse meeting occurrence reference: %w", err)
		}
		if at.IsZero() {
			return record, sql.ErrNoRows
		}
		page, err := s.meetingExpand(ctx, rows, at, &budget)
		if err != nil {
			return record, err
		}
		record.Unexpandable = append(record.Unexpandable, page.Unexpandable...)
		for _, o := range page.Occurrences {
			if o.Ref == formatMeetingOccurrence(ref.OccurrenceRef) || ref.Identity == (calrecur.Identity{}) && o.MessageID == ref.MessageID {
				seed = o
				break
			}
		}
		if seed.Ref == "" {
			if row.Event.Kind != "series" || !slices.ContainsFunc(record.Unexpandable, func(item CalendarUnexpandable) bool { return item.MessageID == ref.MessageID }) {
				return record, sql.ErrNoRows
			}
		} else {
			keys = occurrenceKeys(seed)
		}
	}
	var copies []CalendarOccurrence
	if seed.Ref != "" {
		copies = append(copies, seed)
	}
	visited := make(map[meetingLinkKey]bool)
	for len(keys) > 0 {
		var pending []meetingLinkKey
		var texts []string
		for _, key := range keys {
			if visited[key] {
				continue
			}
			visited[key] = true
			pending = append(pending, key)
			texts = append(texts, key.text())
		}
		keys = nil
		if len(pending) == 0 {
			break
		}
		notes, err := s.meetingNoteKeys(ctx, texts, &budget)
		if err != nil {
			return record, err
		}
		matches, err := s.meetingOccurrencesForKeys(ctx, pending, &budget, &record.Unexpandable)
		if err != nil {
			return record, err
		}
		for _, key := range pending {
			matched := matches[key]
			identities := map[string]bool{}
			for _, o := range matched {
				if o.original.AllDay {
					identities["date:"+o.original.Date] = true
					continue
				}
				if o.original == (calrecur.Identity{}) && o.AllDay {
					identities["date:"+o.StartDate] = true
					continue
				}
				original := o.Start
				if o.original != (calrecur.Identity{}) {
					original = o.original.Instant
				}
				identities["time:"+original.UTC().Format(time.RFC3339Nano)] = true
			}
			_, _, instance := calrecur.ParseInstanceID(key[2])
			allowNotes := key[0] != "google_event_id" || instance || len(identities) == 1
			if len(identities) > 1 {
				// Notes with exact keys remain peers even if calendar matches are ambiguous.
				if allowNotes && ref.Kind == "msg" && len(copies) == 0 {
					record.MessageIDs = append(record.MessageIDs, notes[key.text()]...)
				}
				continue
			}
			if allowNotes {
				record.MessageIDs = append(record.MessageIDs, notes[key.text()]...)
			}
			copies = append(copies, matched...)
			for _, occurrence := range matched {
				keys = append(keys, occurrenceKeys(occurrence)...)
			}
		}
	}
	for _, o := range uniqueMeetingOccurrences(copies) {
		record.Links = append(record.Links, MeetingLink{MessageID: o.MessageID, SubjectOriginID: o.row.SubjectOriginID, Ref: o.Ref, Subject: o.Subject, Start: o.Start, End: o.End, Status: o.Status, DeletedFromSource: o.DeletedFromSource})
	}
	slices.Sort(record.MessageIDs)
	record.MessageIDs = slices.Compact(record.MessageIDs)
	slices.SortFunc(record.Unexpandable, func(a, b CalendarUnexpandable) int {
		if c := cmp.Compare(a.MessageID, b.MessageID); c != 0 {
			return c
		}
		return cmp.Compare(a.Reason, b.Reason)
	})
	record.Unexpandable = slices.Compact(record.Unexpandable)
	return record, nil
}
