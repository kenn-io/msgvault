package store

import "strings"

// These are the operations understood by Gmail acquisition replay. Keep the
// in-memory candidate check and diagnostic SQL on the same persisted contract.
var acquisitionOperations = [...]struct{ phase, kind, operation string }{
	{"fetch", "fetch_error", "acquisition"},
	{"fetch", "batch_fetch_error", "acquisition"},
	{"ingest", "ingest_error", "acquisition"},
	{"ingest", "attachment_error", "files"},
	{"ingest", "remote_image_error", "files"},
	{"ingest", "attachment_remote_image_error", "files"},
}

// IsAcquisitionReplayCandidate reports whether the outcome names a usable
// message and an operation understood by Gmail replay. Selection of the latest
// error and its run boundary remains latestAcquisitionOutcomeIDsSQL's job.
func (item SyncRunItem) IsAcquisitionReplayCandidate() bool {
	if item.SourceMessageID == "" || item.SourceMessageID == "(unknown)" {
		return false
	}
	for _, entry := range acquisitionOperations {
		if item.Phase == entry.phase && item.ErrorKind == entry.kind {
			return true
		}
	}
	return false
}

func acquisitionOperationSQL(alias string) string {
	var sql strings.Builder
	sql.WriteString("CASE WHEN " + alias + ".source_message_id IN ('', '(unknown)') THEN 'unknown'")
	for _, entry := range acquisitionOperations {
		sql.WriteString(" WHEN " + alias + ".phase = '" + entry.phase + "' AND " + alias + ".error_kind = '" + entry.kind + "' THEN '" + entry.operation + "'")
	}
	sql.WriteString(" ELSE 'unknown' END")
	return sql.String()
}

// latestAcquisitionOutcomeIDsSQL is the replay boundary, shared by diagnostics
// and attachment summaries. Arguments are source ID twice, then (optionally)
// source message ID. Restrict before grouping for per-message callers.
func latestAcquisitionOutcomeIDsSQL(oneMessage bool) string {
	filter := ""
	if oneMessage {
		filter = " AND i.source_message_id = ?"
	}
	return `SELECT MAX(i.id) FROM sync_run_items i
		JOIN sync_runs r ON r.id = i.sync_run_id
		WHERE r.source_id = ? AND r.id >= COALESCE((
			SELECT id FROM sync_runs WHERE source_id = ?
			AND sync_type = 'incremental' AND status = 'completed'
			ORDER BY completed_at DESC, id DESC LIMIT 1
		), 0) AND i.phase IN ('fetch', 'ingest')` + filter + `
		GROUP BY i.source_message_id`
}

// syntheticFileObligationSQL recognizes only the marker shipped with file
// obligations. It does not determine whether an outcome is replayable.
func syntheticFileObligationSQL(alias string) string {
	return "(COALESCE(" + alias + ".error_kind, '') IN ('attachment_error', 'remote_image_error', 'attachment_remote_image_error')" +
		" AND COALESCE(" + alias + ".error_message, '') = '" + SyncRunItemFilesPendingMessage + "')"
}
