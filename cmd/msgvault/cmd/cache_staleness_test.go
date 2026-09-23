package cmd

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCacheNeedsBuildInterruptedStateOnlyCache(t *testing.T) {
	tmpDir := setupTestSQLiteEmpty(t)
	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")
	writeSyncState(t, analyticsDir, 0)

	got := cacheNeedsBuild(dbPath, analyticsDir)
	require.True(t, got.NeedsBuild)
	assert.True(t, got.FullRebuild)
	assert.Contains(t, got.Reason, "interrupted")
}

func TestCacheNeedsBuildTracksCoveredRelatedRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	analyticsDir := filepath.Join(tmp, "analytics")
	st, err := store.Open(dbPath)
	require.NoError(err)
	require.NoError(st.InitSchema())
	src, err := st.GetOrCreateSource("test", "synthetic@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(src.ID, "thread", "email_thread", "Synthetic")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: src.ID, SourceMessageID: "message",
		MessageType: "email", SentAt: sql.NullTime{
			Time: time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC), Valid: true,
		},
	})
	require.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO labels (id, name) VALUES (1, 'synthetic'), (2, 'changed')`)
	require.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO participants (id, email_address, domain)
		VALUES (1, 'one@example.com', 'example.com'), (2, 'two@example.com', 'example.com')`)
	require.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO message_recipients
		(message_id, participant_id, recipient_type) VALUES (?, 1, 'to')`, messageID)
	require.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO attachments
		(id, message_id, storage_path, filename, size) VALUES (1, ?, 'synthetic', 'old.txt', 1)`, messageID)
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE messages SET has_attachments = TRUE, attachment_count = 1 WHERE id = ?`, messageID)
	require.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO message_labels (message_id, label_id) VALUES (?, 1)`, messageID)
	require.NoError(err)
	require.NoError(st.Close())
	_, err = buildCache(dbPath, analyticsDir, true)
	require.NoError(err)
	state, err := query.ReadCacheSyncState(analyticsDir)
	require.NoError(err)
	assert.Positive(state.LastRelatedChangeSeq)
	assert.False(cacheNeedsBuild(dbPath, analyticsDir).NeedsBuild)

	st, err = store.Open(dbPath)
	require.NoError(err)
	var acknowledgedRows int64
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM cache_related_change_journal`).Scan(&acknowledgedRows))
	assert.Zero(acknowledgedRows, "published journal entries should be pruned")
	require.NoError(st.AddMessageLabels(messageID, []int64{2}))
	require.NoError(st.ReplaceMessageRecipients(messageID, "to", []int64{2}, []string{"Recipient Two"}))
	_, err = st.DB().Exec(`UPDATE attachments SET filename = 'new.txt', size = 2 WHERE id = 1`)
	require.NoError(err)
	require.NoError(st.Close())
	got := cacheNeedsBuild(dbPath, analyticsDir)
	assert.True(got.NeedsBuild)
	assert.True(got.HasRelatedRowDrift)
	assert.Contains(got.Reason, "related rows changed")
	st, err = store.Open(dbPath)
	require.NoError(err)
	var seq int64
	require.NoError(st.DB().QueryRow(`SELECT MAX(seq) FROM cache_related_change_journal`).Scan(&seq))
	require.NoError(st.Close())
	assert.Greater(seq, state.LastRelatedChangeSeq)
	previousHook := buildCacheBeforeMessagesExportHook
	buildCacheBeforeMessagesExportHook = func() error {
		return errors.New("related-row repair attempted a message export")
	}
	t.Cleanup(func() { buildCacheBeforeMessagesExportHook = previousHook })
	result, err := buildCacheAuto(dbPath, analyticsDir)
	require.NoError(err)
	assert.True(result.IdentityOnly)
	assert.Zero(result.ExportedCount)
	assert.False(cacheNeedsBuild(dbPath, analyticsDir).NeedsBuild)
	repairedState, err := query.ReadCacheSyncState(analyticsDir)
	require.NoError(err)
	assert.Equal(seq, repairedState.LastRelatedChangeSeq)
	duckDB, err := sql.Open("duckdb", "")
	require.NoError(err)
	defer func() { _ = duckDB.Close() }()
	labelsPath := strings.ReplaceAll(filepath.Join(analyticsDir, "message_labels", "*.parquet"), "'", "''")
	var labelCount int64
	require.NoError(duckDB.QueryRow("SELECT COUNT(*) FROM read_parquet('" + labelsPath + "')").Scan(&labelCount))
	assert.Equal(int64(2), labelCount)
	recipientsPath := strings.ReplaceAll(filepath.Join(analyticsDir, "message_recipients", "*.parquet"), "'", "''")
	var recipientID int64
	require.NoError(duckDB.QueryRow("SELECT participant_id FROM read_parquet('" + recipientsPath + "')").Scan(&recipientID))
	assert.Equal(int64(2), recipientID)
	attachmentsPath := strings.ReplaceAll(filepath.Join(analyticsDir, "attachments", "*.parquet"), "'", "''")
	var filename string
	var attachmentSize int64
	require.NoError(duckDB.QueryRow("SELECT filename, size FROM read_parquet('"+attachmentsPath+"')").Scan(&filename, &attachmentSize))
	assert.Equal("new.txt", filename)
	assert.Equal(int64(2), attachmentSize)
	assert.Equal(int64(2), repairedState.Stats.AttachmentSizeBytes)

	buildCacheBeforeMessagesExportHook = nil
	st, err = store.Open(dbPath)
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: src.ID, SourceMessageID: "new-message",
		MessageType: "email", SentAt: sql.NullTime{
			Time: time.Date(2025, 1, 2, 10, 0, 0, 0, time.UTC), Valid: true,
		},
	})
	require.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO labels (id, name) VALUES (3, 'second-change')`)
	require.NoError(err)
	require.NoError(st.AddMessageLabels(messageID, []int64{3}))
	require.NoError(st.Close())
	result, err = buildCacheAuto(dbPath, analyticsDir)
	require.NoError(err)
	assert.Equal(int64(1), result.StagedCount)
	assert.False(cacheNeedsBuild(dbPath, analyticsDir).NeedsBuild)
	require.NoError(duckDB.QueryRow("SELECT COUNT(*) FROM read_parquet('" + labelsPath + "')").Scan(&labelCount))
	assert.Equal(int64(3), labelCount)

	activityFiles, err := filepath.Glob(filepath.Join(analyticsDir,
		"relationship_activity", "occurred_year=*", "*.parquet"))
	require.NoError(err)
	require.NotEmpty(activityFiles)
	activityBefore, err := os.Stat(activityFiles[0])
	require.NoError(err)
	st, err = store.Open(dbPath)
	require.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO labels (id, name) VALUES (4, 'label-only')`)
	require.NoError(err)
	require.NoError(st.AddMessageLabels(messageID, []int64{4}))
	require.NoError(st.Close())
	_, err = buildCacheAuto(dbPath, analyticsDir)
	require.NoError(err)
	activityAfter, err := os.Stat(activityFiles[0])
	require.NoError(err)
	assert.Equal(activityBefore.ModTime(), activityAfter.ModTime(),
		"label-only repair must reuse relationship activity")
	assert.Equal(activityBefore.Size(), activityAfter.Size())
	var labelDefinitionCount int64
	definitionsPath := strings.ReplaceAll(filepath.Join(analyticsDir, "labels", "*.parquet"), "'", "''")
	require.NoError(duckDB.QueryRow("SELECT COUNT(*) FROM read_parquet('" + definitionsPath + "')").Scan(&labelDefinitionCount))
	assert.Equal(int64(4), labelDefinitionCount)

	// A display-name change has no message_labels mutation, but must still
	// republish the label definitions used by analytical views.
	st, err = store.Open(dbPath)
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE labels SET name = 'renamed' WHERE id = 4`)
	require.NoError(err)
	require.NoError(st.Close())
	got = cacheNeedsBuild(dbPath, analyticsDir)
	assert.True(got.HasRelatedRowDrift)
	assert.False(got.FullRebuild)
	_, err = buildCacheAuto(dbPath, analyticsDir)
	require.NoError(err)
	var labelName string
	require.NoError(duckDB.QueryRow("SELECT name FROM read_parquet('" + definitionsPath + "') WHERE id = 4").Scan(&labelName))
	assert.Equal("renamed", labelName)

	// Attachment or From-recipient edits may also alter facts baked into old
	// message shards; those cannot use the child-only repair path.
	st, err = store.Open(dbPath)
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE messages SET attachment_count = attachment_count + 1 WHERE id = ?`, messageID)
	require.NoError(err)
	require.NoError(st.Close())
	got = cacheNeedsBuild(dbPath, analyticsDir)
	assert.True(got.FullRebuild)
	assert.True(got.HasDerivedDataDrift)
}

func TestCacheNeedsBuild_MeetingMutation(t *testing.T) {
	tests := []struct {
		name          string
		messageType   string
		mutationField string
		wantStale     bool
	}{
		{name: "meeting source deletion", messageType: "meeting_transcript", mutationField: "deleted_from_source_at", wantStale: true},
		{name: "meeting dedup hide", messageType: "meeting_transcript", mutationField: "deleted_at", wantStale: true},
		{name: "calendar source deletion", messageType: "calendar_event", mutationField: "deleted_from_source_at", wantStale: true},
		{name: "calendar dedup hide", messageType: "calendar_event", mutationField: "deleted_at", wantStale: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			tmp := t.TempDir()
			dbPath := filepath.Join(tmp, "msgvault.db")
			analyticsDir := filepath.Join(tmp, "analytics")

			st, err := store.Open(dbPath)
			require.NoError(err)
			require.NoError(st.InitSchema())
			src, err := st.GetOrCreateSource("test", "user@example.com")
			require.NoError(err)
			convID, err := st.EnsureConversationWithType(src.ID, "thread-1", "email_thread", "Thread")
			require.NoError(err)
			_, err = st.UpsertMessage(&store.Message{
				ConversationID:  convID,
				SourceID:        src.ID,
				SourceMessageID: "email-1",
				MessageType:     "email",
				SentAt:          sql.NullTime{Time: time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC), Valid: true},
			})
			require.NoError(err)
			mutationID, err := st.UpsertMessage(&store.Message{
				ConversationID:  convID,
				SourceID:        src.ID,
				SourceMessageID: "mutated-1",
				MessageType:     tt.messageType,
				SentAt:          sql.NullTime{Time: time.Date(2025, 1, 2, 10, 0, 0, 0, time.UTC), Valid: true},
			})
			require.NoError(err)
			require.NoError(st.Close())

			_, err = buildCache(dbPath, analyticsDir, false)
			require.NoError(err)

			st, err = store.Open(dbPath)
			require.NoError(err)
			_, err = st.DB().Exec(
				"UPDATE messages SET "+tt.mutationField+" = ? WHERE id = ?",
				time.Now().UTC().Add(time.Minute).Format("2006-01-02 15:04:05"), mutationID,
			)
			require.NoError(err)
			require.NoError(st.Close())

			got := cacheNeedsBuild(dbPath, analyticsDir)
			assert.Equal(t, tt.wantStale, got.NeedsBuild, "cache staleness: %+v", got)
			assert.Equal(t, tt.wantStale, got.FullRebuild, "full rebuild: %+v", got)
		})
	}
}

func TestCacheNeedsBuildMixedNewMessagesAndRelationshipDriftForcesFullRebuild(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *store.Store)
		assert func(*testing.T, cacheStaleness)
	}{
		{
			name: "participant links",
			mutate: func(t *testing.T, st *store.Store) {
				t.Helper()
				_, err := st.LinkParticipants(3, 4)
				require.NoError(t, err)
			},
			assert: func(t *testing.T, got cacheStaleness) {
				t.Helper()
				assert.True(t, got.HasIdentityDrift)
			},
		},
		{
			name: "conversation membership",
			mutate: func(t *testing.T, st *store.Store) {
				t.Helper()
				_, err := st.DB().Exec(`
					INSERT INTO conversation_participants
						(conversation_id, participant_id)
					VALUES (102, 4)
				`)
				require.NoError(t, err)
			},
			assert: func(t *testing.T, got cacheStaleness) {
				t.Helper()
				assert.True(t, got.HasConversationParticipantDrift)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			tmp := setupTestSQLite(t)
			dbPath := filepath.Join(tmp, "test.db")
			analyticsDir := filepath.Join(tmp, "analytics")
			_, err := buildCache(dbPath, analyticsDir, true)
			requirements.NoError(err)

			st, err := store.Open(dbPath)
			requirements.NoError(err)
			_, err = st.DB().Exec(`
				INSERT INTO messages
					(id, source_id, source_message_id, conversation_id,
					 subject, sent_at, message_type)
				VALUES
					(6, 1, 'msg6', 101, 'New message',
					 '2024-03-02 10:00:00', 'email')
			`)
			requirements.NoError(err)
			tt.mutate(t, st)
			requirements.NoError(st.Close())

			got := cacheNeedsBuild(dbPath, analyticsDir)
			assertions.True(got.HasNew)
			tt.assert(t, got)
			assertions.True(got.FullRebuild,
				"mixed new-message and relationship drift must not append stale index rows")
		})
	}
}

func explainQueryPlan(t *testing.T, s *store.Store, sql string, args ...any) string {
	t.Helper()
	rows, err := s.DB().Query("EXPLAIN QUERY PLAN "+sql, args...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notUsed, &detail))
		b.WriteString(detail)
		b.WriteString("\n")
	}
	require.NoError(t, rows.Err())
	return b.String()
}

// TestCacheStalenessCounts_UseDeletionIndexes verifies the two deletion-status
// COUNTs in cacheNeedsBuild are served by the partial deletion indexes instead
// of full scans of the messages table. These queries run on every daemon start
// before the API server binds, so a full scan on a cold page cache adds
// multiple seconds to every cold-start CLI command on a large archive.
func TestCacheStalenessCounts_UseDeletionIndexes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	// The Parquet cache staleness check is a SQLite-only ETL path;
	// cacheNeedsBuild returns early for PostgreSQL DSNs.
	s := testutil.NewSQLiteTestStore(t)

	_, err := s.DB().Exec(
		`INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'user@example.com')`)
	require.NoError(err)
	_, err = s.DB().Exec(`INSERT INTO conversations
		(id, source_id, source_conversation_id, conversation_type)
		VALUES (1, 1, 'conv1', 'email')`)
	require.NoError(err)
	_, err = s.DB().Exec(`INSERT INTO messages
		(id, conversation_id, source_id, source_message_id, message_type, sent_at, deleted_from_source_at, deleted_at)
		VALUES
		(1, 1, 1, 'm1', 'email', '2025-01-01 00:00:00', NULL, NULL),
		(2, 1, 1, 'm2', 'email', '2025-01-02 00:00:00', '2025-06-01 00:00:00', NULL),
		(3, 1, 1, 'm3', 'email', '2025-01-03 00:00:00', NULL, '2025-06-02 00:00:00')`)
	require.NoError(err)

	for _, name := range []string{"idx_messages_deleted_from_source_at", "idx_messages_deleted_at"} {
		var idxCount int
		require.NoError(s.DB().QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name = ?`, name,
		).Scan(&idxCount))
		assert.Equal(1, idxCount, "%s should be created by InitSchema", name)
	}

	deletedPlan := explainQueryPlan(t, s, deletedSinceBuildCountSQL(), "2025-05-01 00:00:00")
	assert.Contains(deletedPlan, "idx_messages_deleted_from_source_at",
		"source-deleted COUNT should use the partial index, not a full scan:\n%s", deletedPlan)
	assert.NotContains(deletedPlan, "SCAN messages", deletedPlan)

	hiddenPlan := explainQueryPlan(t, s, hiddenSinceBuildCountSQL(), "2025-05-01 00:00:00")
	assert.Contains(hiddenPlan, "idx_messages_deleted_at",
		"dedup-hidden COUNT should use the partial index, not a full scan:\n%s", hiddenPlan)
	assert.NotContains(hiddenPlan, "SCAN messages", hiddenPlan)

	// The queries still return correct counts through the indexes.
	var deleted, hidden int64
	require.NoError(s.DB().QueryRow(deletedSinceBuildCountSQL(), "2025-05-01 00:00:00").Scan(&deleted))
	require.NoError(s.DB().QueryRow(hiddenSinceBuildCountSQL(), "2025-05-01 00:00:00").Scan(&hidden))
	assert.Equal(int64(1), deleted)
	assert.Equal(int64(1), hidden)
}
