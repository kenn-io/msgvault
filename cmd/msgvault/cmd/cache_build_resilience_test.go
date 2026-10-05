package cmd

import (
	"crypto/sha256"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

func TestCacheSnapshotSyncCatchUp(t *testing.T) {
	tests := []struct {
		name          string
		csv           string
		appendMessage bool
		wantMessages  int
	}{
		{name: "scanner/children_only", wantMessages: 1},
		{name: "scanner/with_append", appendMessage: true, wantMessages: 2},
		{name: "csv/children_only", csv: "1", wantMessages: 1},
		{name: "csv/with_append", csv: "1", appendMessage: true, wantMessages: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", tt.csv)
			c, st := openTestDaemonAnalyticsStore(t)
			_, err := st.DB().Exec(`
				INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'owner@example.com');
				INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type)
					VALUES (1, 1, 'thread', 'email_thread');
				INSERT INTO messages (id, source_id, source_message_id, conversation_id, message_type, sent_at)
					VALUES (1, 1, 'message-1', 1, 'email', '2024-01-01 00:00:00');
				INSERT INTO labels (id, name) VALUES (1, 'synthetic');
			`)
			require.NoError(err)
			// A separate connection writes after the export's read transaction
			// is pinned, before any source tables are copied.
			buildCacheAfterSnapshotHook = func() {
				writer, err := sql.Open("sqlite3", c.DatabaseDSN())
				require.NoError(err)
				defer func() { require.NoError(writer.Close()) }()
				if tt.appendMessage {
					_, err = writer.Exec(`INSERT INTO messages
						(id, source_id, source_message_id, conversation_id, message_type, sent_at)
						VALUES (2, 1, 'message-2', 1, 'email', '2024-01-02 00:00:00')`)
					require.NoError(err)
				}
				_, err = writer.Exec(`
					INSERT INTO message_labels (message_id, label_id) VALUES (1, 1);
					INSERT INTO sync_runs (source_id, started_at, completed_at, status, messages_added)
						VALUES (1, datetime('now'), datetime('now'), 'completed', 1);
				`)
				require.NoError(err)
			}
			t.Cleanup(func() { buildCacheAfterSnapshotHook = nil })
			_, err = buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
			require.NoError(err)
			buildCacheAfterSnapshotHook = nil
			state, err := query.ReadCacheSyncState(c.AnalyticsDir())
			require.NoError(err)
			assert.Equal(int64(1), state.LastMessageID)
			assert.Zero(state.LastCacheAdditionCount, "stamp the snapshot's counters")
			assert.False(state.FullRebuildRequired, "journal tracks late child rows")
			before := snapshotMessagesDatasetBytes(t, c.AnalyticsDir())
			stale := cacheNeedsBuild(c.DatabaseDSN(), c.AnalyticsDir())
			assert.True(stale.NeedsBuild)
			assert.False(stale.FullRebuild, "journaled catch-up: %s", stale.Reason)
			_, err = buildCacheAuto(c.DatabaseDSN(), c.AnalyticsDir())
			require.NoError(err)
			after := snapshotMessagesDatasetBytes(t, c.AnalyticsDir())
			for path, contents := range before {
				assert.Equal(sha256.Sum256([]byte(contents)), sha256.Sum256([]byte(after[path])),
					"retain message shard %s", path)
			}
			assert.False(cacheNeedsBuild(c.DatabaseDSN(), c.AnalyticsDir()).NeedsBuild)
			engine, err := openDaemonDuckDBEngine(c, st)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(engine.Close()) })
			rows, err := engine.QuerySQL(t.Context(), "SELECT message_id, label_id FROM message_labels")
			require.NoError(err)
			require.Len(rows.Rows, 1)
			assert.EqualValues(1, rows.Rows[0][0])
			rows, err = engine.QuerySQL(t.Context(), "SELECT COUNT(*), COUNT(DISTINCT id) FROM messages")
			require.NoError(err)
			assert.EqualValues(tt.wantMessages, rows.Rows[0][0])
			assert.EqualValues(tt.wantMessages, rows.Rows[0][1])
		})
	}
}

// A sync can commit between the staleness check that selects a child-row
// refresh and the refresh's own snapshot. The refresh must reject changes it
// cannot repair, and every build entry point must finish them with a full
// rebuild.
func TestCacheRelatedRefreshRaceFallsBackToFullBuild(t *testing.T) {
	races := []struct {
		name         string
		write        string
		wantMessages int64
	}{
		{
			name:         "old message becomes exportable",
			write:        `UPDATE messages SET sent_at = '2024-01-01 00:00:00' WHERE id = 1`,
			wantMessages: 2,
		},
		{
			name: "sync updates cached messages",
			write: `INSERT INTO sync_runs (source_id, started_at, completed_at, status, messages_updated)
				VALUES (1, datetime('now'), datetime('now'), 'completed', 1)`,
			wantMessages: 1,
		},
	}
	builds := []struct {
		name  string
		build func(dbPath, analyticsDir string) (*buildResult, error)
	}{
		{name: "auto", build: func(dbPath, analyticsDir string) (*buildResult, error) {
			return buildCacheAuto(dbPath, analyticsDir)
		}},
		{name: "scheduled", build: func(dbPath, analyticsDir string) (*buildResult, error) {
			return buildCacheScheduled(dbPath, analyticsDir, 0, time.Now)
		}},
	}
	for _, entry := range builds {
		for _, race := range races {
			t.Run(entry.name+"/"+race.name, func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				c, st := openTestDaemonAnalyticsStore(t)
				_, err := st.DB().Exec(`
					INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'owner@example.com');
					INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type)
						VALUES (1, 1, 'thread', 'email_thread');
					INSERT INTO messages (id, source_id, source_message_id, conversation_id, message_type, sent_at)
						VALUES (1, 1, 'pending', 1, 'email', NULL),
						       (2, 1, 'message-2', 1, 'email', '2024-01-02 00:00:00');
					INSERT INTO labels (id, name) VALUES (1, 'synthetic');
				`)
				require.NoError(err)
				_, err = buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
				require.NoError(err)
				_, err = st.DB().Exec(`
					INSERT INTO message_labels (message_id, label_id) VALUES (2, 1);
					INSERT INTO sync_runs (source_id, started_at, completed_at, status, messages_added)
						VALUES (1, datetime('now'), datetime('now'), 'completed', 1);
				`)
				require.NoError(err)
				stale := cacheNeedsBuild(c.DatabaseDSN(), c.AnalyticsDir())
				require.True(stale.HasRelatedRowDrift)
				require.False(stale.FullRebuild, "the label alone is a child-row repair: %s", stale.Reason)

				derivedRefreshBeforeSnapshotHook = func() {
					_, err := st.DB().Exec(race.write)
					require.NoError(err)
				}
				t.Cleanup(func() { derivedRefreshBeforeSnapshotHook = nil })
				built, err := entry.build(c.DatabaseDSN(), c.AnalyticsDir())
				require.NoError(err)
				derivedRefreshBeforeSnapshotHook = nil
				assert.Equal(race.wantMessages, built.StagedCount, "full rebuild stages every message")
				assert.False(cacheNeedsBuild(c.DatabaseDSN(), c.AnalyticsDir()).NeedsBuild)

				engine, err := openDaemonDuckDBEngine(c, st)
				require.NoError(err)
				t.Cleanup(func() { require.NoError(engine.Close()) })
				rows, err := engine.QuerySQL(t.Context(), "SELECT COUNT(*) FROM messages")
				require.NoError(err)
				assert.EqualValues(race.wantMessages, rows.Rows[0][0])
				rows, err = engine.QuerySQL(t.Context(), "SELECT message_id FROM message_labels")
				require.NoError(err)
				require.Len(rows.Rows, 1)
				assert.EqualValues(2, rows.Rows[0][0])
			})
		}
	}
}
