package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// purgeQuerier is the exclusive connection a channel purge runs on.
type purgeQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// channelPurge selects one Slack channel or one Discord parent and its threads.
type channelPurge struct {
	sourceID  int64
	provider  string
	channelID string
	condition string
	args      []any
}

func (p channelPurge) targets() string {
	return "SELECT id FROM conversations WHERE source_id = ? AND (" + p.condition + ")"
}

func (p channelPurge) targetMessages() string {
	return "SELECT id FROM messages WHERE conversation_id IN (" + p.targets() + ")"
}

// PurgeChannelContext hard-deletes a Slack channel or a Discord parent and its
// archived threads. The caller persists exclusion from future collection and
// stops workers before calling. Execution ownership fences other sync processes.
// Message foreign keys cascade to bodies, raw payloads, and attachment metadata;
// SQLite's separate FTS table needs explicit cleanup in the same transaction.
// Like source removal, it runs under the exclusive write lock so packed blob
// mappings that lose their last reference are removed atomically.
func (s *Store) PurgeChannelContext(ctx context.Context, sourceID int64, channelID string) (retErr error) {
	if channelID == "" {
		return errors.New("channel ID is required")
	}
	execution, err := s.AcquireSyncExecutionContext(ctx, sourceID)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	source, err := s.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return err
	}
	purge := channelPurge{
		sourceID: sourceID, provider: source.SourceType, channelID: channelID,
		condition: "source_conversation_id = ?", args: []any{sourceID, channelID},
	}
	switch source.SourceType {
	case "slack":
	case "discord":
		// Thread metadata is untyped JSON in SQLite. A malformed row cannot
		// name a parent, and it must not abort removal of the selected one.
		parent := "CASE WHEN json_valid(metadata) THEN json_extract(metadata, '$.parent_channel_id') END"
		if s.IsPostgreSQL() {
			parent = "metadata->>'parent_channel_id'"
		}
		purge.condition += " OR " + parent + " = ?"
		purge.args = append(purge.args, channelID)
	default:
		return errors.New("channel purge requires a Slack or Discord source")
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire channel purge connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if err := s.dialect.BeginExclusive(ctx, conn); err != nil {
		return fmt.Errorf("begin channel purge: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()
	// A queued or running daemon operation would restore the channel from
	// its own resume state; refuse like source removal does.
	if err := s.rejectConflictingSyncOperation(ctx, conn, sourceID, ""); err != nil {
		return err
	}
	if err := s.purgeChannelExec(ctx, conn, purge); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit channel purge: %w", err)
	}
	committed = true
	return nil
}

func (s *Store) purgeChannelExec(ctx context.Context, q purgeQuerier, purge channelPurge) error {
	ids, err := s.purgedConversationIDs(ctx, q, purge)
	if err != nil {
		return err
	}
	if err := s.forgetChannelCoverage(ctx, q, purge.sourceID, purge.provider, ids); err != nil {
		return err
	}
	packedHashes, err := s.packedBlobHashes(ctx, q, purge)
	if err != nil {
		return err
	}
	if !s.IsPostgreSQL() && s.fts5Available {
		// FTS rows use the message ID as their rowid; message_id itself is
		// unindexed, so filtering on it would scan the whole index.
		if _, err := q.ExecContext(ctx, s.dialect.Rebind(
			"DELETE FROM messages_fts WHERE rowid IN ("+purge.targetMessages()+")"), purge.args...); err != nil {
			return fmt.Errorf("purge channel full-text entries: %w", err)
		}
	}
	// Replies in retained channels can point into the removed channel or
	// its threads. Keep those messages while clearing their dangling link.
	if _, err := q.ExecContext(ctx, s.dialect.Rebind(
		"UPDATE messages SET reply_to_message_id = NULL WHERE reply_to_message_id IN ("+
			purge.targetMessages()+")"), purge.args...); err != nil {
		return fmt.Errorf("clear replies to purged channel: %w", err)
	}
	if _, err := q.ExecContext(ctx, s.dialect.Rebind(
		"DELETE FROM conversations WHERE id IN ("+purge.targets()+")"), purge.args...); err != nil {
		return fmt.Errorf("purge channel conversations: %w", err)
	}
	if err := s.deleteUnreferencedPackMappings(ctx, q, packedHashes); err != nil {
		return err
	}
	// Analytics caches cannot unpublish exported rows incrementally; a new
	// revision makes the next cache pass rebuild without the purged messages.
	return s.bumpDerivedDataRevisionContext(ctx, q)
}

func (s *Store) purgedConversationIDs(ctx context.Context, q purgeQuerier, purge channelPurge) ([]string, error) {
	rows, err := q.QueryContext(ctx, s.dialect.Rebind(
		"SELECT source_conversation_id FROM conversations WHERE source_id = ? AND ("+purge.condition+")"),
		purge.args...)
	if err != nil {
		return nil, fmt.Errorf("list purged conversations: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	ids := []string{purge.channelID}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan purged conversation: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list purged conversations: %w", err)
	}
	return ids, nil
}

// packedBlobHashes lists packed content and thumbnail blobs that the purged
// messages reference. Mappings still referenced elsewhere survive the purge.
func (s *Store) packedBlobHashes(ctx context.Context, q purgeQuerier, purge channelPurge) ([]string, error) {
	query := `
		WITH purged_blobs(blob_hash) AS (
		    SELECT LOWER(content_hash) FROM attachments
		    WHERE content_hash IS NOT NULL AND content_hash != ''
		      AND message_id IN (` + purge.targetMessages() + `)
		    UNION
		    SELECT LOWER(thumbnail_hash) FROM attachments
		    WHERE thumbnail_hash IS NOT NULL AND thumbnail_hash != ''
		      AND message_id IN (` + purge.targetMessages() + `)
		)
		SELECT pb.blob_hash FROM purged_blobs pb
		WHERE EXISTS (SELECT 1 FROM attachment_pack_index p WHERE p.blob_hash = pb.blob_hash)
		ORDER BY pb.blob_hash`
	rows, err := q.QueryContext(ctx, s.dialect.Rebind(query), append(slices.Clone(purge.args), purge.args...)...)
	if err != nil {
		return nil, fmt.Errorf("list purged packed blobs: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var hashes []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, fmt.Errorf("scan purged packed blob: %w", err)
		}
		hashes = append(hashes, hash)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list purged packed blobs: %w", err)
	}
	return hashes, nil
}

func (s *Store) deleteUnreferencedPackMappings(ctx context.Context, q purgeQuerier, hashes []string) error {
	const chunkSize = 500
	for chunk := range slices.Chunk(hashes, chunkSize) {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, len(chunk))
		for i, hash := range chunk {
			args[i] = hash
		}
		if _, err := q.ExecContext(ctx, s.dialect.Rebind(`
			DELETE FROM attachment_pack_index
			WHERE blob_hash IN (`+placeholders+`)
			  AND NOT EXISTS (
			      SELECT 1 FROM attachments a
			      WHERE LOWER(a.content_hash) = attachment_pack_index.blob_hash
			         OR LOWER(a.thumbnail_hash) = attachment_pack_index.blob_hash
			  )`), args...); err != nil {
			return fmt.Errorf("delete purged packed blob mappings: %w", err)
		}
	}
	return nil
}

type syncCheckpoint struct {
	id            int64
	before, after sql.NullString
}

// sourceCheckpoints closes its cursor before the caller rewrites rows on the
// same exclusive connection.
func (s *Store) sourceCheckpoints(ctx context.Context, q purgeQuerier, sourceID int64) ([]syncCheckpoint, error) {
	rows, err := q.QueryContext(ctx, s.dialect.Rebind(
		"SELECT id,cursor_before,cursor_after FROM sync_runs WHERE source_id = ?"), sourceID)
	if err != nil {
		return nil, fmt.Errorf("list purged source checkpoints: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var checkpoints []syncCheckpoint
	for rows.Next() {
		var c syncCheckpoint
		if err := rows.Scan(&c.id, &c.before, &c.after); err != nil {
			return nil, fmt.Errorf("scan purged source checkpoint: %w", err)
		}
		checkpoints = append(checkpoints, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list purged source checkpoints: %w", err)
	}
	return checkpoints, nil
}

// Deletion and coverage invalidation share a transaction: a resumed repair must
// never certify rows that purge removed. Preserve every other channel's cursor.
func (s *Store) forgetChannelCoverage(
	ctx context.Context, q purgeQuerier, sourceID int64, provider string, ids []string,
) error {
	checkpoints, err := s.sourceCheckpoints(ctx, q, sourceID)
	if err != nil {
		return err
	}
	for _, c := range checkpoints {
		before, err := withoutChannelCoverage(c.before, provider, ids)
		if err != nil {
			return err
		}
		after, err := withoutChannelCoverage(c.after, provider, ids)
		if err != nil {
			return err
		}
		if before == c.before && after == c.after {
			continue
		}
		if _, err := q.ExecContext(ctx, s.dialect.Rebind(
			"UPDATE sync_runs SET cursor_before = ?,cursor_after = ? WHERE id = ?"), before, after, c.id); err != nil {
			return err
		}
	}
	return nil
}

func withoutChannelCoverage(cursor sql.NullString, provider string, ids []string) (sql.NullString, error) {
	if !cursor.Valid || cursor.String == "" {
		return cursor, nil
	}
	var state map[string]jsontext.Value
	if err := json.Unmarshal([]byte(cursor.String), &state); err != nil {
		// Malformed checkpoints already require an explicit full repair. They
		// cannot certify coverage and must not prevent content removal.
		return cursor, nil //nolint:nilerr // Content removal must remain possible when existing resume state is malformed.
	}
	fields := []string{"conversations"}
	if provider == "discord" {
		fields = []string{"containers", "thread_catalog"}
	}
	changed := false
	for _, field := range fields {
		raw, exists := state[field]
		if !exists {
			continue
		}
		var entries map[string]jsontext.Value
		if err := json.Unmarshal(raw, &entries); err != nil {
			return cursor, nil //nolint:nilerr // Leave malformed state for the importer's explicit full-repair path.
		}
		for _, id := range ids {
			if _, exists := entries[id]; exists {
				delete(entries, id)
				changed = true
			}
		}
		encoded, err := json.Marshal(entries)
		if err != nil {
			return cursor, err
		}
		state[field] = encoded
	}
	if raw, exists := state["history_pass"]; provider == "slack" && exists {
		var pass map[string]jsontext.Value
		if err := json.Unmarshal(raw, &pass); err != nil {
			return cursor, nil //nolint:nilerr // Leave malformed state for explicit full repair.
		}
		if raw, exists := pass["visited"]; exists {
			var visited map[string]jsontext.Value
			if err := json.Unmarshal(raw, &visited); err != nil {
				return cursor, nil //nolint:nilerr // Leave malformed state for explicit full repair.
			}
			for _, id := range ids {
				if _, exists := visited[id]; exists {
					delete(visited, id)
					changed = true
				}
			}
			encoded, err := json.Marshal(visited)
			if err != nil {
				return cursor, err
			}
			pass["visited"] = encoded
			encoded, err = json.Marshal(pass)
			if err != nil {
				return cursor, err
			}
			state["history_pass"] = encoded
		}
	}
	if !changed {
		return cursor, nil
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return cursor, err
	}
	return sql.NullString{String: string(encoded), Valid: true}, nil
}
