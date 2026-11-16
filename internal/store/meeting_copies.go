package store

import (
	"context"
	"fmt"
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

// recordMeetingCopyTx adds the edge that copy traversal and undo use.
func recordMeetingCopyTx(ctx context.Context, tx *loggedTx, duplicate, survivor int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO meeting_copy_survivors(message_id, survivor_message_id) SELECT id, ? FROM messages WHERE id=? AND message_type IN ('calendar_event', 'meeting_transcript') ON CONFLICT (message_id) DO UPDATE SET survivor_message_id=excluded.survivor_message_id`, survivor, duplicate)
	return err
}
