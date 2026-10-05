package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

type relatedChangeKinds struct {
	recipients  bool
	labels      bool
	attachments bool
	other       bool
}

func inspectRelatedChangeKinds(snapshot *cacheSourceSnapshot, after, through, lastMessageID int64) (relatedChangeKinds, error) {
	var kinds relatedChangeKinds
	err := snapshot.QueryRow(`SELECT
		count(*) FILTER (WHERE dataset = 'message_recipients') > 0,
		count(*) FILTER (WHERE dataset IN ('message_labels', 'labels')) > 0,
		count(*) FILTER (WHERE dataset = 'attachments') > 0,
		count(*) FILTER (WHERE dataset NOT IN
			('message_recipients', 'message_labels', 'labels', 'attachments')) > 0
		FROM cache_related_change_journal WHERE seq > ? AND seq <= ? AND message_id <= ?`,
		after, through, lastMessageID).Scan(&kinds.recipients, &kinds.labels, &kinds.attachments, &kinds.other)
	if err != nil {
		return relatedChangeKinds{}, fmt.Errorf("inspect related change kinds: %w", err)
	}
	return kinds, nil
}

func refreshRelatedCacheStats(ctx context.Context, db sqlRunner, state *query.CacheSyncState, kinds relatedChangeKinds) error {
	if kinds.recipients {
		statement := `SELECT count(DISTINCT p.email_address), count(DISTINCT p.domain)
			FROM sqlite_db.message_recipients mr
			JOIN sqlite_db.messages m ON m.id = mr.message_id
			JOIN sqlite_db.participants p ON p.id = mr.participant_id
			WHERE mr.recipient_type = 'from' AND m.id <= ? AND ` + exportableMessageWhere("m")
		if err := db.QueryRowContext(ctx, statement, state.LastMessageID).
			Scan(&state.Stats.UniqueSenders, &state.Stats.UniqueDomains); err != nil {
			return fmt.Errorf("refresh related sender statistics: %w", err)
		}
	}
	if kinds.attachments {
		statement := `SELECT coalesce(sum(try_cast(a.size AS BIGINT)), 0)
			FROM sqlite_db.attachments a JOIN sqlite_db.messages m ON m.id = a.message_id
			WHERE m.id <= ? AND ` + exportableMessageWhere("m")
		if err := db.QueryRowContext(ctx, statement, state.LastMessageID).
			Scan(&state.Stats.AttachmentSizeBytes); err != nil {
			return fmt.Errorf("refresh related attachment statistics: %w", err)
		}
	}
	return nil
}

// The marker is published before pruning. A failed publication keeps all
// journal entries available for replay; a failed prune only leaves redundant
// entries, since sqlite_sequence preserves the acknowledged high watermark.
func pruneAcknowledgedRelatedChanges(dbPath string, relatedSeq, derivedRevision int64) error {
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open store to prune related changes: %w", err)
	}
	defer func() { _ = st.Close() }()
	tx, err := st.DB().Begin()
	if err != nil {
		return fmt.Errorf("begin related-change prune: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM cache_related_change_journal WHERE seq <= ?`, relatedSeq); err != nil {
		return fmt.Errorf("prune related changes: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM cache_related_revision_journal WHERE revision <= ?`, derivedRevision); err != nil {
		return fmt.Errorf("prune related revisions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit related-change prune: %w", err)
	}
	return nil
}

func warnRelatedChangePrune(dbPath string, relatedSeq, derivedRevision int64) {
	if err := pruneAcknowledgedRelatedChanges(dbPath, relatedSeq, derivedRevision); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
	}
}

func inspectRelatedSnapshotColumns(snapshot *cacheSourceSnapshot) error {
	for _, column := range []struct {
		table   string
		name    string
		present *bool
	}{
		{"message_recipients", "email_address", &snapshot.hasRecipientEnvelope},
		{"attachments", "mime_type", &snapshot.hasAttachmentMIME},
		{"attachments", "attachment_metadata", &snapshot.hasAttachmentMetadata},
	} {
		var count int
		statement := fmt.Sprintf("SELECT COUNT(*) FROM pragma_table_info('%s') WHERE name = '%s'",
			column.table, column.name)
		if err := snapshot.QueryRow(statement).Scan(&count); err != nil {
			return fmt.Errorf("inspect %s.%s for related repair: %w", column.table, column.name, err)
		}
		*column.present = count > 0
	}
	return nil
}

// datasets lists replacements needed for edits to already-cached messages.
func (k relatedChangeKinds) datasets() map[string]bool {
	return map[string]bool{
		"message_recipients": k.recipients,
		tableLabels:          k.labels,
		"message_labels":     k.labels,
		tableAttachments:     k.attachments,
	}
}

// exportRelatedDatasets exports the requested child datasets from one snapshot.
// Each lower message-ID boundary is zero for a replacement, or the committed
// watermark for an append. Label definitions are always replaced.
func exportRelatedDatasets(
	ctx context.Context, db sqlRunner, snapshot *cacheSourceSnapshot,
	lastMessageID int64, stagingRoot string, afterMessageIDs map[string]int64,
) error {
	parentFilter := func(dataset string) string {
		return fmt.Sprintf(`SELECT CAST(m.id AS BIGINT) FROM sqlite_db.messages m
			WHERE %s AND TRY_CAST(m.id AS BIGINT) <= %d AND TRY_CAST(m.id AS BIGINT) > %d`,
			exportableMessageWhere("m"), lastMessageID, afterMessageIDs[dataset])
	}
	// Missing envelopes fall back to the participant address. A damaged but
	// present envelope stays unknown and must still suppress that fallback.
	envelope := "NULL::VARCHAR"
	presence := "FALSE"
	participant := "CASE WHEN " + cacheIdentityTextSQL("p.email_address") +
		" = '' THEN NULL ELSE " + snapshot.identityExportSQL("p.email_address") + " END"
	if snapshot.hasRecipientEnvelope {
		envelope = "CASE WHEN " + cacheIdentityTextSQL("mr.email_address") +
			" = '' THEN NULL ELSE " + snapshot.identityExportSQL("mr.email_address") + " END"
		presence = snapshot.identityPresenceSQL("mr.email_address", "mr.envelope_present")
	}
	mimeType := "'' AS mime_type"
	if snapshot.hasAttachmentMIME {
		mimeType = "COALESCE(" + snapshot.textSQL("mime_type") + ", '') AS mime_type"
	}
	metadata := "NULL::VARCHAR AS attachment_metadata"
	if snapshot.hasAttachmentMetadata {
		metadata = snapshot.textSQL("attachment_metadata") + " AS attachment_metadata"
	}
	exports := []struct {
		dataset   string
		selectSQL string
	}{
		{tableLabels, `SELECT id, COALESCE(` + snapshot.textSQL("name") + `, '') AS name
			FROM sqlite_db.labels`},
		{"message_recipients", fmt.Sprintf(`SELECT mr.message_id, mr.participant_id,
			%[4]s AS recipient_type,
			COALESCE(%[5]s, '') AS display_name,
			CASE WHEN %[6]s THEN %[1]s ELSE %[7]s END AS email_address,
			%[1]s AS envelope_address
			FROM sqlite_db.message_recipients mr
			LEFT JOIN sqlite_db.participants p ON p.id = mr.participant_id
			WHERE mr.message_id > %[3]d AND TRY_CAST(mr.message_id AS BIGINT) IN (%[2]s)`, envelope, parentFilter("message_recipients"), afterMessageIDs["message_recipients"], snapshot.identityExportSQL("mr.recipient_type"), snapshot.textSQL("mr.display_name"), presence, participant)},
		{"message_labels", fmt.Sprintf(`SELECT message_id, label_id
			FROM sqlite_db.message_labels WHERE message_id > %d AND TRY_CAST(message_id AS BIGINT) IN (%s)`, afterMessageIDs["message_labels"], parentFilter("message_labels"))},
		{tableAttachments, fmt.Sprintf(`SELECT id AS attachment_id, message_id, size,
			COALESCE(%s, '') AS filename,
			%s, %s FROM sqlite_db.attachments
			WHERE message_id > %d AND TRY_CAST(message_id AS BIGINT) IN (%s)`, snapshot.textSQL("filename"), mimeType, metadata, afterMessageIDs[tableAttachments], parentFilter(tableAttachments))},
	}
	for _, item := range exports {
		if _, ok := afterMessageIDs[item.dataset]; !ok {
			continue
		}
		if err := copyParquet(ctx, db, filepath.Join(stagingRoot, item.dataset), "data.parquet", item.selectSQL); err != nil {
			return fmt.Errorf("export related dataset %s: %w", item.dataset, query.HintRepairEncoding(err))
		}
	}
	return nil
}
