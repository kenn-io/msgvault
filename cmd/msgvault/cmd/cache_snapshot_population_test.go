package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCacheCatchUpRepairsNewlyExportableOldMessage(t *testing.T) {
	for _, withAppend := range []bool{false, true} {
		t.Run(map[bool]string{false: "within_boundary", true: "with_append"}[withAppend], func(t *testing.T) {
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
			// Finishing this parent changes the message population below the
			// cache's ID boundary. Its journaled labels alone cannot repair it.
			_, err = st.DB().Exec(`
				UPDATE messages SET sent_at = '2024-01-01 00:00:00' WHERE id = 1;
				INSERT INTO message_labels (message_id, label_id) VALUES (1, 1);
				INSERT INTO sync_runs (source_id, started_at, completed_at, status, messages_added)
					VALUES (1, datetime('now'), datetime('now'), 'completed', 1);
			`)
			require.NoError(err)
			wantCount := int64(2)
			if withAppend {
				_, err = st.DB().Exec(`INSERT INTO messages
					(id, source_id, source_message_id, conversation_id, message_type, sent_at)
					VALUES (3, 1, 'message-3', 1, 'email', '2024-01-03 00:00:00')`)
				require.NoError(err)
				wantCount++
			} else {
				// Serving defers the population check to the builder. Background
				// inspection still detects the missing old message facts.
				light, err := cacheNeedsBuildForServing(t.Context(), c.DatabaseDSN(), c.AnalyticsDir())
				require.NoError(err)
				assert.True(light.NeedsBuild)
				assert.True(light.HasRelatedRowDrift)
				assert.False(light.FullRebuild, "builder verifies the population before repair")
				assert.True(cacheNeedsBuild(c.DatabaseDSN(), c.AnalyticsDir()).FullRebuild)
			}
			built, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), false)
			require.NoError(err)
			assert.Equal(wantCount, built.StagedCount, "the old missing parent requires a full facts repair")
			assert.False(cacheNeedsBuild(c.DatabaseDSN(), c.AnalyticsDir()).NeedsBuild)
			engine, err := openDaemonDuckDBEngine(c, st)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(engine.Close()) })
			result, err := engine.QuerySQL(t.Context(), "SELECT COUNT(*) FROM messages")
			require.NoError(err)
			assert.EqualValues(wantCount, result.Rows[0][0])
		})
	}
}

// The staleness check ignores messages deleted at the source when it looks for
// new messages. A child-row refresh must not record the new addition count while
// such an exportable message sits above the cached boundary.
func TestCacheCatchUpExportsSourceDeletedMessageAboveBoundary(t *testing.T) {
	tests := []struct {
		name string
		csv  string
	}{
		{name: "scanner"},
		{name: "csv", csv: "1"},
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
			_, err = buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
			require.NoError(err)
			// The deletion time predates the cache build, so the deletion check
			// does not force a full rebuild on its own.
			_, err = st.DB().Exec(`
				INSERT INTO messages (id, source_id, source_message_id, conversation_id, message_type,
					sent_at, deleted_from_source_at)
					VALUES (2, 1, 'message-2', 1, 'email', '2024-01-02 00:00:00', '2024-01-03 00:00:00');
				INSERT INTO message_labels (message_id, label_id) VALUES (1, 1);
				INSERT INTO sync_runs (source_id, started_at, completed_at, status, messages_added)
					VALUES (1, datetime('now'), datetime('now'), 'completed', 1);
			`)
			require.NoError(err)
			stale := cacheNeedsBuild(c.DatabaseDSN(), c.AnalyticsDir())
			require.False(stale.HasNew, "a source-deleted message is not live")
			require.True(stale.HasRelatedRowDrift)

			_, err = buildCacheAuto(c.DatabaseDSN(), c.AnalyticsDir())
			require.NoError(err)
			assert.False(cacheNeedsBuild(c.DatabaseDSN(), c.AnalyticsDir()).NeedsBuild)
			engine, err := openDaemonDuckDBEngine(c, st)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(engine.Close()) })
			result, err := engine.QuerySQL(t.Context(), "SELECT id FROM messages ORDER BY id")
			require.NoError(err)
			require.Len(result.Rows, 2)
			assert.EqualValues(2, result.Rows[1][0])
		})
	}
}
