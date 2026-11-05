package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/calrecur"
)

// Each cache belongs to the request's read snapshot. The scalar meeting-link
// resolver consumes the same evidence as the batch reader.
type meetingIdentityBatch struct {
	calendarSources map[string][]any
	events          map[string][]CalendarEventRow
	keys            map[int64]meetingLinkKey
	decisions       map[int64][]meetingLinkDecision
	visible         map[int64]bool
}

type meetingEventQuery struct {
	predicate string
	args      []any
}

func meetingQueryKey(predicate string, args []any) string {
	return fmt.Sprintf("%s:%#v", predicate, args)
}

// Load the scalar resolver's master, moved-exception, and expansion reads together.
func (s *Store) preloadMeetingOccurrences(ctx context.Context, r reboundReader, refs []OccurrenceRef, budget *int) error {
	queries := []meetingEventQuery{}
	for _, ref := range refs {
		for _, row := range r.batch.events[meetingQueryKey("e.message_id = ?", []any{ref.MessageID})] {
			if row.Event.Kind == meetingExceptionKind && ref.Key == "" {
				predicate, args := meetingMasterPredicate(row)
				queries = append(queries, meetingEventQuery{predicate, args})
			}
		}
	}
	if err := s.preloadMeetingQueries(ctx, r, queries, budget); err != nil {
		return err
	}
	type recurringRef struct {
		row CalendarEventRow
		id  calrecur.Identity
	}
	recurring := []recurringRef{}
	queries = nil
	for _, ref := range refs {
		rows := r.batch.events[meetingQueryKey("e.message_id = ?", []any{ref.MessageID})]
		if len(rows) != 1 {
			continue
		}
		row := rows[0]
		if row.Event.Kind == meetingExceptionKind && ref.Key == "" && row.Event.Original != nil {
			predicate, args := meetingMasterPredicate(row)
			masters := r.batch.events[meetingQueryKey(predicate, args)]
			if len(masters) == 1 {
				ref.Key = row.Event.Original.Key()
				row = masters[0]
			}
		}
		if ref.Key == "" || row.Event.Kind != meetingSeriesKind && row.Event.Kind != "single" {
			continue
		}
		_, id, ok := calrecur.ParseInstanceID("event_" + ref.Key)
		if !ok {
			continue
		}
		recurring = append(recurring, recurringRef{row, id})
		predicate, args := meetingMovedExceptionPredicate(row, id)
		queries = append(queries, meetingEventQuery{predicate, args})
	}
	if err := s.preloadMeetingQueries(ctx, r, queries, budget); err != nil {
		return err
	}
	queries = nil
	for _, ref := range recurring {
		at, err := meetingOccurrenceStart(ref.row, ref.id)
		if err != nil {
			return err
		}
		predicate, args := meetingMovedExceptionPredicate(ref.row, ref.id)
		moved := r.batch.events[meetingQueryKey(predicate, args)]
		if len(moved) > 0 {
			x := moved[len(moved)-1]
			if x.Event.Start != nil && x.Event.End != nil {
				continue
			}
			if x.Event.Start != nil {
				at = *x.Event.Start
			}
		}
		predicate, args = meetingSeriesExceptionsPredicate(r.dialect, ref.row, at.Add(-calendarCandidateMargin), at.Add(calendarCandidateMargin))
		queries = append(queries, meetingEventQuery{predicate, args})
	}
	return s.preloadMeetingQueries(ctx, r, queries, budget)
}

// Find IDs for many predicates together, then hydrate each calendar row once.
// Keep the predicate owners in meeting_links.go so scalar and batch reads agree.
func (s *Store) preloadMeetingQueries(ctx context.Context, r reboundReader, queries []meetingEventQuery, budget *int) error {
	pending := []meetingEventQuery{}
	seen := map[string]bool{}
	for _, q := range queries {
		key := meetingQueryKey(q.predicate, q.args)
		if _, ok := r.batch.events[key]; !ok && !seen[key] {
			pending = append(pending, q)
			seen[key] = true
		}
	}
	for chunk := range slices.Chunk(pending, 20) {
		terms := make([]string, 0, len(chunk))
		args := []any{}
		// Scope before LIMIT so hidden copies cannot exhaust candidate bounds.
		scope, scopeArgs := sourceScopeClause(ctx, "m.source_id")
		for i, q := range chunk {
			terms = append(terms, fmt.Sprintf("SELECT %d,message_id FROM (SELECT e.message_id ", i)+calendarEventFrom+"("+q.predicate+")"+scope+" LIMIT ?) candidates")
			args = append(args, q.args...)
			args = append(args, scopeArgs...)
			args = append(args, min(200, *budget)+1)
		}
		byQuery := make([][]int64, len(chunk))
		ids := []int64{}
		err := func() error {
			rows, err := r.QueryContext(ctx, strings.Join(terms, " UNION ALL ")+" LIMIT ?", append(args, *budget+1)...)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var i int
				var id int64
				if err := rows.Scan(&i, &id); err != nil {
					return err
				}
				byQuery[i] = append(byQuery[i], id)
				ids = append(ids, id)
			}
			return errors.Join(rows.Err(), rows.Close())
		}()
		if err != nil {
			return err
		}
		if len(ids) > *budget {
			return calendarRecurrenceBudgetError()
		}
		// The probe selected these IDs within the source scope and snapshot.
		// Hydrate only missing rows without applying that scope a second time.
		slices.Sort(ids)
		ids = slices.Compact(ids)
		missing := []any{}
		for _, id := range ids {
			if _, ok := r.batch.events[meetingQueryKey("e.message_id = ?", []any{id})]; !ok {
				missing = append(missing, id)
			}
		}
		for chunk := range slices.Chunk(missing, 200) {
			rows, err := queryCalendarEventRows(ctx, r, "e.message_id IN ("+placeholders(len(chunk))+")", chunk, 0)
			if err != nil {
				return err
			}
			*budget -= len(rows)
			for _, row := range rows {
				r.batch.events[meetingQueryKey("e.message_id = ?", []any{row.MessageID})] = []CalendarEventRow{row}
			}
		}
		for i, q := range chunk {
			var found []CalendarEventRow
			if len(byQuery[i]) > 200 {
				return ErrQueryTooBroad
			}
			for _, id := range byQuery[i] {
				found = append(found, r.batch.events[meetingQueryKey("e.message_id = ?", []any{id})]...)
			}
			r.batch.events[meetingQueryKey(q.predicate, q.args)] = found
		}
	}
	return nil
}

func (s *Store) preloadMeetingMembers(ctx context.Context, r reboundReader, ids []int64) error {
	snapshot, _ := ctx.Value(readSnapshotKey{}).(*readSnapshot)
	scope, args := sourceScopeClause(ctx, "m.source_id")
	// Prefix arguments precede the chunk arguments in the shared chunking helper.
	query := `SELECT m.id,k.kind,k.value,k.calendar_id,k.scheduled_at,d.target_message_id,d.occurrence_key,d.decision
 FROM messages m
 LEFT JOIN meeting_link_key k ON k.message_id=m.id
 LEFT JOIN meeting_link_decisions d ON d.member_message_id=m.id
 WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL` + scope + ` AND m.id IN (%s)
 ORDER BY m.id,d.target_message_id,d.occurrence_key`
	for _, id := range ids {
		r.batch.visible[id] = false
		r.batch.keys[id] = meetingLinkKey{}
		r.batch.decisions[id] = nil
	}
	return queryInChunksContext(ctx, snapshot.tx, ids, args, query, func(rows *loggedRows) error {
		var id int64
		var kind, value, calendar, key, decision sql.NullString
		var scheduled sql.NullTime
		var target sql.NullInt64
		if err := rows.Scan(&id, &kind, &value, &calendar, &scheduled, &target, &key, &decision); err != nil {
			return err
		}
		r.batch.visible[id] = true
		linkKey := meetingLinkKey{Kind: kind.String, Value: value.String, CalendarID: calendar.String}
		if scheduled.Valid {
			linkKey.ScheduledAt = &scheduled.Time
		}
		r.batch.keys[id] = linkKey
		if decision.Valid {
			r.batch.decisions[id] = append(r.batch.decisions[id], meetingLinkDecision{Ref: OccurrenceRef{MessageID: target.Int64, Key: key.String}, Decision: decision.String})
		}
		return nil
	})
}
