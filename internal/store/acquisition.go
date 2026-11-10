package store

const (
	SyncItemKindBatchFetchError               = "batch_fetch_error"
	SyncItemKindFetchError                    = "fetch_error"
	SyncItemKindIngestError                   = "ingest_error"
	SyncItemKindAttachmentError               = "attachment_error"
	SyncItemKindRemoteImageError              = "remote_image_error"
	SyncItemKindAttachmentAndRemoteImageError = "attachment_remote_image_error"
)

// IsAcquisitionReplayCandidate identifies outcomes the syncer can retry.
func (item SyncRunItem) IsAcquisitionReplayCandidate() bool {
	if item.SourceMessageID == "" || item.SourceMessageID == "(unknown)" {
		return false
	}
	switch item.Phase {
	case "fetch":
		return item.ErrorKind == SyncItemKindFetchError || item.ErrorKind == SyncItemKindBatchFetchError
	case "ingest":
		switch item.ErrorKind {
		case SyncItemKindIngestError, SyncItemKindAttachmentError, SyncItemKindRemoteImageError, SyncItemKindAttachmentAndRemoteImageError:
			return true
		}
	}
	return false
}

// latestAcquisitionOutcomeIDsSQL is the replay boundary, shared by diagnostics
// and attachment summaries. Arguments are source ID twice, incremental type, then (optionally)
// source message ID. Restrict before grouping for per-message callers.
func latestAcquisitionOutcomeIDsSQL(oneMessage bool) string {
	filter := ""
	if oneMessage {
		filter = " AND i.source_message_id = ?"
	}
	return `SELECT MAX(i.id) FROM sync_run_items i
		JOIN sync_runs r ON r.id = i.sync_run_id
		WHERE r.source_id = ? AND r.id >= COALESCE((
			SELECT id ` + lastSuccessfulSyncFilter(true) + `
		), 0) AND i.phase IN ('fetch', 'ingest')` + filter + `
		GROUP BY i.source_message_id`
}

// syntheticFileObligationSQL identifies the persisted file-saving reminder.
func syntheticFileObligationSQL(alias string) string {
	return "(COALESCE(" + alias + ".error_kind, '') IN ('" + SyncItemKindAttachmentError + "', '" + SyncItemKindRemoteImageError + "', '" + SyncItemKindAttachmentAndRemoteImageError + "')" +
		" AND COALESCE(" + alias + ".error_message, '') = '" + SyncRunItemFilesPendingMessage + "')"
}
