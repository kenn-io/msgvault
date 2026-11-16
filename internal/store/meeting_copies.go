package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/msgvault/internal/calrecur"
)

// meetingSuccessorSQL follows dedup history and the current visible calendar copy.
func meetingSuccessorSQL() string {
	return fmt.Sprintf(`COALESCE(
 (SELECT survivor_message_id FROM meeting_copy_survivors WHERE message_id=%[1]s.id AND %[1]s.delete_batch_id IS NOT NULL),
 (SELECT visible.message_id FROM calendar_events original JOIN calendar_events visible ON visible.provider_event_id=original.provider_event_id JOIN messages vm ON vm.id=visible.message_id AND vm.source_id=%[1]s.source_id WHERE original.message_id=%[1]s.id AND %[1]s.deleted_at IS NULL AND NOT %[2]s AND vm.deleted_at IS NULL AND %[3]s ORDER BY visible.message_id LIMIT 1),
 (SELECT survivor_message_id FROM source_merge_messages WHERE message_id=%[1]s.id))`, "m", calendarEventVisibility("original", "m"), calendarEventVisibility("visible", "vm"))
}

// meetingCopyGraphSQL traverses copy links in both directions. Series links
// may be included when an occurrence key is available to select the edit.
func meetingCopyGraphSQL(seeds, scope, parentScope, series string) string {
	relations := `SELECT links.id,links.parent FROM links JOIN messages parent ON parent.id=links.parent WHERE TRUE` + parentScope
	seriesCTE := ""
	if series != "" {
		seriesCTE = `, series_links(id,parent,original_start_at,original_start_date) AS (` + series + `)`
		relations += ` UNION ALL SELECT id,parent FROM series_links`
	}
	return `links(id,parent) AS (
 SELECT m.id, ` + meetingSuccessorSQL() + ` FROM messages m
 WHERE (m.deleted_at IS NOT NULL OR EXISTS (SELECT 1 FROM calendar_events e WHERE e.message_id=m.id AND NOT ` + calendarEventVisibility("e", "m") + `))` + scope + `
 )` + seriesCTE + `, relations(id,parent) AS (` + relations + `), copies(idx,id) AS (
 ` + seeds + ` UNION SELECT c.idx,CASE WHEN r.id=c.id THEN r.parent ELSE r.id END FROM copies c JOIN relations r ON (r.id=c.id OR r.parent=c.id)
 )`
}

func meetingSeriesLinksSQL(condition string) string {
	return `SELECT e.message_id,series.message_id,e.original_start_at,e.original_start_date FROM calendar_events e JOIN messages em ON em.id=e.message_id JOIN calendar_events series ON series.provider_event_id=e.series_provider_id AND series.kind='series' JOIN messages sm ON sm.id=series.message_id AND sm.source_id=em.source_id WHERE e.kind='exception' AND e.series_provider_id<>''` + condition
}

// meetingCopyLookup returns copies and, for a dated event, its series and edits.
func (s *Store) meetingCopyLookup(ctx context.Context, endpoint meetingDecisionEndpoint) ([]int64, error) {
	copyScope, args := sourceScopeClause(ctx, "m.source_id")
	parentScope, parentArgs := sourceScopeClause(ctx, "parent.source_id")
	series := ""
	if endpoint.Kind == meetingRefEvent && endpoint.Key != "" {
		ref, err := endpoint.meetingRef()
		if err != nil {
			return nil, err
		}
		var date string
		var instant any
		if ref.Identity.AllDay {
			date = ref.Identity.Date
		} else {
			instant = ref.Identity.Instant
		}
		editScope, editArgs := sourceScopeClause(ctx, "em.source_id")
		seriesScope, seriesArgs := sourceScopeClause(ctx, "sm.source_id")
		args = append(args, date, instant)
		args = append(args, editArgs...)
		args = append(args, seriesArgs...)
		series = meetingSeriesLinksSQL(` AND ((e.original_start_date<>'' AND e.original_start_date=?) OR e.original_start_at=?)` + editScope + seriesScope)
	}
	// The series CTE precedes relations in meetingCopyGraphSQL.
	args = append(args, parentArgs...)
	args = append(args, endpoint.ID)
	query := `WITH RECURSIVE ` + meetingCopyGraphSQL(`SELECT 0,CAST(? AS BIGINT)`, copyScope, parentScope, series) + ` SELECT id FROM copies ORDER BY id`
	rows, err := s.meetingSQLReader(ctx).QueryContext(ctx, query, args...)
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
	return ids, errors.Join(rows.Err(), rows.Close())
}

func (s *Store) meetingCopies(ctx context.Context, id int64) ([]int64, error) {
	return s.meetingCopyLookup(ctx, meetingDecisionEndpoint{ID: id})
}

// meetingOccurrenceEdits finds the visible edits of one copied occurrence.
func (s *Store) meetingOccurrenceEdits(ctx context.Context, endpoint meetingDecisionEndpoint, budget *int) ([]CalendarEventRow, error) {
	ids, err := s.meetingCopyLookup(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	var edits []CalendarEventRow
	for batch := range slices.Chunk(ids, 100) {
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.meetingEventRows(ctx, `e.kind='exception' AND e.message_id IN (`+placeholders(len(batch))+`)`, args, budget)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if row.Event.Original != nil && row.Event.Original.Key() == endpoint.Key {
				edits = append(edits, row)
			}
		}
	}
	return edits, nil
}

// meetingOccurrenceSeries finds the visible series directly or through copies.
func (s *Store) meetingOccurrenceSeries(ctx context.Context, id, sourceID int64, providerID string, original calrecur.Identity, budget *int) (int64, error) {
	reader := s.meetingSQLReader(ctx)
	seriesRows, err := queryCalendarEventRowsWithinBudget(ctx, reader, `m.source_id = ? AND e.provider_event_id = ? AND e.kind = 'series'`, []any{sourceID, providerID}, *budget)
	if err != nil {
		return 0, err
	}
	*budget -= len(seriesRows)
	if len(seriesRows) > 0 {
		return seriesRows[0].MessageID, nil
	}
	var hasChoices bool
	if err := reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM meeting_link_decisions WHERE (left_kind='evt' AND left_key=?) OR (right_kind='evt' AND right_key=?))`, original.Key(), original.Key()).Scan(&hasChoices); err != nil || !hasChoices {
		return 0, err
	}
	ids, err := s.meetingCopyLookup(ctx, meetingDecisionEndpoint{ID: id, Kind: meetingRefEvent, Key: original.Key()})
	if err != nil {
		return 0, err
	}
	for batch := range slices.Chunk(ids, 100) {
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := queryCalendarEventRowsWithinBudget(ctx, reader, `e.kind='series' AND e.message_id IN (`+placeholders(len(batch))+`)`, args, *budget)
		if err != nil {
			return 0, err
		}
		*budget -= len(rows)
		if len(rows) > 0 {
			return rows[0].MessageID, nil
		}
	}
	return 0, nil
}

// recordMeetingCopyTx adds the edge that copy traversal and undo use.
func recordMeetingCopyTx(ctx context.Context, tx *loggedTx, duplicate, survivor int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO meeting_copy_survivors(message_id, survivor_message_id) SELECT id, ? FROM messages WHERE id=? AND message_type IN ('calendar_event', 'meeting_transcript') ON CONFLICT (message_id) DO UPDATE SET survivor_message_id=excluded.survivor_message_id`, survivor, duplicate)
	return err
}
