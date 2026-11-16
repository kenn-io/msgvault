package store

// latestSyncItemIDsQuery selects the greatest fetch/ingest item ID per identity
// within the replay boundary. Status and kind filters belong to the caller,
// after this selection: even an unknown outcome supersedes an older error.
//
// The source-wide form binds (sourceID, lowerBound), retaining the replay
// reader's separate boundary lookup. The message form binds (sourceID,
// sourceMessageID, sourceID, "incremental") and reads its boundary in the
// caller's transaction. Restrict the identity before selecting the latest item;
// do not compute all of a source's latest outcomes for each image.
func latestSyncItemIDsQuery(forMessage bool) string {
	selection := "SELECT MAX(i.id)"
	identity := ""
	lowerBound := "?"
	latest := " GROUP BY i.source_message_id"
	if forMessage {
		// Keep the unit lookup's early-stop plan instead of grouping the source.
		selection = "SELECT i.id"
		identity = " AND i.source_message_id = ?"
		lowerBound = "COALESCE((SELECT id " + lastSuccessfulSyncSelection(true) + "), 0)"
		latest = " ORDER BY i.id DESC LIMIT 1"
	}
	return selection + `
		FROM sync_run_items i
		JOIN sync_runs r ON r.id = i.sync_run_id
		WHERE r.source_id = ?` + identity + ` AND i.phase IN ('fetch', 'ingest')
		AND r.id >= ` + lowerBound + latest
}
