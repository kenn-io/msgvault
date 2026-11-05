package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
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

func (s *Store) preloadMeetingEvents(ctx context.Context, r reboundReader, ids []int64, budget *int) error {
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for chunk := range slices.Chunk(ids, 200) {
		var missing []int64
		for _, id := range chunk {
			if _, ok := r.batch.events[meetingQueryKey("e.message_id = ?", []any{id})]; !ok {
				missing = append(missing, id)
			}
		}
		if len(missing) == 0 {
			continue
		}
		args := make([]any, len(missing))
		for i, id := range missing {
			args[i] = id
		}
		rows, err := meetingEventRows(ctx, r, "e.message_id IN ("+placeholders(len(args))+")", args, budget)
		if err != nil {
			return err
		}
		for _, id := range missing {
			r.batch.events[meetingQueryKey("e.message_id = ?", []any{id})] = nil
		}
		for _, row := range rows {
			r.batch.events[meetingQueryKey("e.message_id = ?", []any{row.MessageID})] = []CalendarEventRow{row}
		}
	}
	return nil
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
		rows, err := r.QueryContext(ctx, strings.Join(terms, " UNION ALL ")+" LIMIT ?", append(args, *budget+1)...)
		if err != nil {
			return err
		}
		byQuery := make([][]int64, len(chunk))
		ids := []int64{}
		for rows.Next() {
			var i int
			var id int64
			if err := rows.Scan(&i, &id); err != nil {
				_ = rows.Close()
				return err
			}
			byQuery[i] = append(byQuery[i], id)
			ids = append(ids, id)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if len(ids) > *budget {
			return calendarRecurrenceBudgetError()
		}
		if err := s.preloadMeetingEvents(ctx, r, ids, budget); err != nil {
			return err
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
	snapshot := ctx.Value(readSnapshotKey{}).(*readSnapshot)
	scope, args := sourceScopeClause(ctx, "m.source_id")
	// Prefix arguments precede the chunk arguments in the shared chunking helper.
	query := `SELECT m.id,k.kind,k.value,k.calendar_id,k.scheduled_at,d.target_ical_uid,COALESCE(d.target_message_id,0),d.occurrence_key,d.decision,d.created_at
 FROM messages m LEFT JOIN meeting_link_key k ON k.message_id=m.id
 LEFT JOIN meeting_link_decisions d ON d.member_message_id=m.id
 WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL` + scope + ` AND m.id IN (%s)
 ORDER BY m.id,d.target_ical_uid,d.target_message_id,d.occurrence_key`
	for _, id := range ids {
		r.batch.visible[id] = false
		r.batch.keys[id] = meetingLinkKey{}
		r.batch.decisions[id] = nil
	}
	return queryInChunksContext(ctx, snapshot.tx, ids, args, query, func(rows *loggedRows) error {
		var id int64
		var kind, value, calendar, uid, key, decision sql.NullString
		var scheduled, created sql.NullTime
		var target int64
		if err := rows.Scan(&id, &kind, &value, &calendar, &scheduled, &uid, &target, &key, &decision, &created); err != nil {
			return err
		}
		r.batch.visible[id] = true
		linkKey := meetingLinkKey{Kind: kind.String, Value: value.String, CalendarID: calendar.String}
		if scheduled.Valid {
			linkKey.ScheduledAt = &scheduled.Time
		}
		r.batch.keys[id] = linkKey
		if decision.Valid {
			r.batch.decisions[id] = append(r.batch.decisions[id], meetingLinkDecision{Identity: meetingIdentity{ICalUID: uid.String, EventMessageID: target, OccurrenceKey: key.String}, Decision: decision.String, CreatedAt: created.Time})
		}
		return nil
	})
}
