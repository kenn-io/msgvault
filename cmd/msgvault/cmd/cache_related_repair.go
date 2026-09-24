package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

type relatedChangeKinds struct {
	recipients  bool
	labels      bool
	attachments bool
	other       bool
}

func inspectRelatedChangeKinds(snapshot *cacheSourceSnapshot, after, through int64) (relatedChangeKinds, error) {
	var kinds relatedChangeKinds
	err := snapshot.QueryRow(`SELECT
		count(*) FILTER (WHERE dataset = 'message_recipients') > 0,
		count(*) FILTER (WHERE dataset IN ('message_labels', 'labels')) > 0,
		count(*) FILTER (WHERE dataset = 'attachments') > 0,
		count(*) FILTER (WHERE dataset NOT IN
			('message_recipients', 'message_labels', 'labels', 'attachments')) > 0
		FROM cache_related_change_journal WHERE seq > ? AND seq <= ?`,
		after, through).Scan(&kinds.recipients, &kinds.labels, &kinds.attachments, &kinds.other)
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

// mergedRelatedIndexBase makes a lightweight file-level snapshot for an
// incremental append that also repairs old related rows. The index rebuild
// needs old and new message facts together, but copying the old Parquet shards
// would turn a small sync into another full archive export.
func mergedRelatedIndexBase(analyticsDir, stagingRoot string) (string, error) {
	root := filepath.Join(stagingRoot, ".related-index-base")
	parentPath := filepath.Dir(analyticsDir)
	parentRoot, err := os.OpenRoot(parentPath)
	if err != nil {
		return "", fmt.Errorf("open related index parent: %w", err)
	}
	defer func() { _ = parentRoot.Close() }()
	baseDatasets := []string{
		"messages", "sources", "conversations", "participants",
		"participant_identifiers", "message_recipients",
		"conversation_participants", "owner_participants",
		"participant_clusters", "person_display_names", "attachments",
	}
	for _, dataset := range baseDatasets {
		if dataset == "messages" {
			if err := linkRelatedIndexDataset(
				parentRoot, parentPath,
				filepath.Join(analyticsDir, dataset), filepath.Join(root, dataset), false,
			); err != nil {
				return "", err
			}
		}
		if err := linkRelatedIndexDataset(
			parentRoot, parentPath,
			filepath.Join(stagingRoot, dataset), filepath.Join(root, dataset), dataset == "messages",
		); err != nil {
			return "", err
		}
	}
	return root, nil
}

func linkRelatedIndexDataset(parentRoot *os.Root, parentPath, source, destination string, allowRename bool) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk related index source %s: %w", source, walkErr)
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".parquet") {
			return nil
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return fmt.Errorf("locate related index shard: %w", err)
		}
		target := filepath.Join(destination, rel)
		relTarget, err := filepath.Rel(parentPath, target)
		if err != nil {
			return fmt.Errorf("locate related index target: %w", err)
		}
		if err := parentRoot.MkdirAll(filepath.Dir(relTarget), 0o755); err != nil {
			return fmt.Errorf("create related index shard directory: %w", err)
		}
		if allowRename {
			if _, err := parentRoot.Stat(relTarget); err == nil {
				relTarget = strings.TrimSuffix(relTarget, filepath.Ext(relTarget)) + "-staged.parquet"
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("inspect related index shard: %w", err)
			}
		}
		relSource, err := filepath.Rel(parentPath, path)
		if err != nil {
			return fmt.Errorf("locate related index source shard: %w", err)
		}
		if err := parentRoot.Link(relSource, relTarget); err != nil {
			return fmt.Errorf("link related index shard %s: %w", rel, err)
		}
		return nil
	})
}

// exportRelatedDatasets rebuilds child rows and label definitions from one SQLite
// snapshot. Committed message facts stay untouched; the derived index builder
// consumes these replacements together with the committed message shards.
func exportRelatedDatasets(
	ctx context.Context, db sqlRunner, snapshot *cacheSourceSnapshot,
	lastMessageID int64, stagingRoot string,
) error {
	parentFilter := fmt.Sprintf(`SELECT m.id FROM sqlite_db.messages m
		WHERE %s AND m.id <= %d`, exportableMessageWhere("m"), lastMessageID)
	envelope := "NULL::VARCHAR"
	if snapshot.hasRecipientEnvelope {
		envelope = "NULLIF(TRY_CAST(mr.email_address AS VARCHAR), '')"
	}
	mimeType := "'' AS mime_type"
	if snapshot.hasAttachmentMIME {
		mimeType = "COALESCE(TRY_CAST(mime_type AS VARCHAR), '') AS mime_type"
	}
	metadata := "NULL::VARCHAR AS attachment_metadata"
	if snapshot.hasAttachmentMetadata {
		metadata = "TRY_CAST(attachment_metadata AS VARCHAR) AS attachment_metadata"
	}
	exports := []struct {
		dataset   string
		selectSQL string
	}{
		{tableLabels, `SELECT id, COALESCE(TRY_CAST(name AS VARCHAR), '') AS name
			FROM sqlite_db.labels`},
		{"message_recipients", fmt.Sprintf(`SELECT mr.message_id, mr.participant_id,
			mr.recipient_type,
			COALESCE(TRY_CAST(mr.display_name AS VARCHAR), '') AS display_name,
			COALESCE(%[1]s, NULLIF(TRY_CAST(p.email_address AS VARCHAR), '')) AS email_address,
			%[1]s AS envelope_address
			FROM sqlite_db.message_recipients mr
			LEFT JOIN sqlite_db.participants p ON p.id = mr.participant_id
			WHERE mr.message_id IN (%[2]s)`, envelope, parentFilter)},
		{"message_labels", fmt.Sprintf(`SELECT message_id, label_id
			FROM sqlite_db.message_labels WHERE message_id IN (%s)`, parentFilter)},
		{tableAttachments, fmt.Sprintf(`SELECT id AS attachment_id, message_id, size,
			COALESCE(TRY_CAST(filename AS VARCHAR), '') AS filename,
			%s, %s FROM sqlite_db.attachments
			WHERE message_id IN (%s)`, mimeType, metadata, parentFilter)},
	}
	for _, item := range exports {
		dir := filepath.Join(stagingRoot, item.dataset)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create related repair dataset %s: %w", item.dataset, err)
		}
		path := filepath.Join(dir, "data.parquet")
		statement := fmt.Sprintf("COPY (%s) TO '%s' (FORMAT PARQUET, COMPRESSION 'zstd')",
			item.selectSQL, quoteCacheSQL(path))
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("export related repair dataset %s: %w", item.dataset, err)
		}
	}
	return nil
}
