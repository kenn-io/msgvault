package cmd

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/duckdbutil"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
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

func TestCSVIncrementalBuildExportsOnlyNewChildRows(t *testing.T) {
	t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "1")
	for _, repair := range []bool{false, true} {
		name := "append"
		if repair {
			name = "repair"
		}
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			tmp := t.TempDir()
			dbPath := filepath.Join(tmp, "msgvault.db")
			incDir, fullDir := filepath.Join(tmp, "analytics-inc"), filepath.Join(tmp, "analytics-full")
			st := testutil.NewSQLiteTestStoreAt(t, dbPath)
			source, err := st.GetOrCreateSource("gmail", "alice@example.com")
			require.NoError(err)
			require.NoError(st.AddAccountIdentity(source.ID, "alice@example.com", "manual"))
			alice, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
			require.NoError(err)
			bob, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
			require.NoError(err)
			require.NoError(st.SetParticipantIdentifier(bob, "email", "bob@example.com"))
			_, _, err = st.CreatePersonFromParticipant(bob)
			require.NoError(err)
			alias, err := st.EnsureParticipantByIdentifier("email", "bob-alias@example.com", "Bob")
			require.NoError(err)
			_, err = st.LinkParticipants(bob, alias)
			require.NoError(err)
			label, err := st.EnsureLabel(source.ID, "inbox", "Inbox", "system")
			require.NoError(err)
			addMessage := func(i int) {
				conv, err := st.EnsureConversationWithType(source.ID, fmt.Sprintf("thread-%d", i), "email_thread", "Thread")
				require.NoError(err)
				id, err := st.UpsertMessage(&store.Message{
					ConversationID: conv, SourceID: source.ID, SourceMessageID: fmt.Sprintf("message-%d", i), MessageType: "email",
					SenderID: sql.NullInt64{Int64: bob, Valid: true},
					Subject:  sql.NullString{String: "Subject", Valid: true}, Snippet: sql.NullString{String: "Snippet", Valid: true},
					SentAt: sql.NullTime{Time: time.Date(2025, 4, i, 12, 0, 0, 0, time.UTC), Valid: true},
				})
				require.NoError(err)
				require.Equal(int64(i), id)
				require.NoError(st.ReplaceMessageRecipients(id, "from", []int64{bob}, []string{"Bob"}))
				require.NoError(st.ReplaceMessageRecipients(id, "to", []int64{alice}, []string{"Alice"}))
				require.NoError(st.AddMessageLabels(id, []int64{label}))
				require.NoError(st.UpsertAttachment(id, "document.txt", "text/plain", "", fmt.Sprintf("hash-%d", i), 42))
				_, err = st.DB().Exec(`INSERT INTO conversation_participants (conversation_id, participant_id) VALUES (?, ?), (?, ?)`, conv, alice, conv, bob)
				require.NoError(err)
				if i > 1 {
					_, err = st.DB().Exec(`INSERT INTO source_merge_archive_only_messages (message_id) VALUES (?)`, id)
					require.NoError(err)
				}
			}
			addMessage(1)
			addMessage(2)
			require.NoError(st.Close())
			_, err = buildCache(dbPath, incDir, true)
			require.NoError(err)

			st, err = store.Open(dbPath)
			require.NoError(err)
			addMessage(3)
			if repair {
				require.NoError(st.ReplaceMessageRecipients(1, "to", []int64{bob}, []string{"Updated"}))
			}
			_, err = st.DB().Exec(`INSERT INTO sync_runs (source_id, started_at, completed_at, status, messages_processed, messages_added)
				VALUES (?, datetime('now'), datetime('now'), 'completed', 1, 1)`, source.ID)
			require.NoError(err)
			require.NoError(st.Close())

			hookCalled := false
			buildCacheBeforeMessagesExportHook = func() error {
				hookCalled = true
				dirs, err := filepath.Glob(filepath.Join(tmp, ".cache-tmp-*"))
				require.NoError(err)
				require.Len(dirs, 1)
				for _, dataset := range []string{"message_recipients", "message_labels", "attachments", "source_merge_archive_only_messages", "messages"} {
					f, err := os.Open(filepath.Join(dirs[0], dataset+".csv"))
					require.NoError(err)
					records, err := csv.NewReader(f).ReadAll()
					require.NoError(f.Close())
					require.NoError(err)
					require.NotEmpty(records)
					columns := make(map[string]int)
					for i, column := range records[0] {
						columns[column] = i
					}
					rows := records[1:]
					if dataset == "messages" {
						assert.Len(rows, 3)
						for _, row := range rows {
							id, err := strconv.Atoi(row[columns["id"]])
							require.NoError(err)
							if id <= 2 && !repair {
								assert.Equal(`\N`, row[columns["subject"]])
							} else {
								assert.Equal("Subject", row[columns["subject"]])
							}
						}
						continue
					}
					expectedIDs := []string{"3"}
					if dataset == "message_recipients" {
						expectedIDs = []string{"3", "3"}
						if repair {
							expectedIDs = []string{"1", "1", "2", "2", "3", "3"}
						}
					}
					var ids []string
					for _, row := range rows {
						ids = append(ids, row[columns["message_id"]])
					}
					assert.ElementsMatch(expectedIDs, ids, dataset)
				}
				return nil
			}
			t.Cleanup(func() { buildCacheBeforeMessagesExportHook = nil })
			built, err := buildCache(dbPath, incDir, false)
			buildCacheBeforeMessagesExportHook = nil
			require.NoError(err)
			assert.True(hookCalled)
			assert.Equal(int64(1), built.StagedCount)
			_, err = buildCache(dbPath, fullDir, true)
			require.NoError(err)

			duckDB, err := duckdbutil.Open(context.Background(), duckdbutil.BuilderPolicy(filepath.Join(tmp, "parity-duckdb-tmp")))
			require.NoError(err)
			defer func() { require.NoError(duckDB.Close()) }()
			for _, dataset := range query.RequiredParquetDirs {
				inc, full := filepath.Join(incDir, dataset, "**", "*.parquet"), filepath.Join(fullDir, dataset, "**", "*.parquet")
				columns := "*"
				if dataset == "relationship_people" {
					// Each build records its own wall-clock watermark.
					columns = "* EXCLUDE (temperature_effective_at)"
				}
				var rows, missing, extra int
				require.NoError(duckDB.QueryRow(fmt.Sprintf(`SELECT
					(SELECT count(*) FROM read_parquet(?, hive_partitioning=true)),
					(SELECT count(*) FROM (SELECT %[1]s FROM read_parquet(?, hive_partitioning=true) EXCEPT ALL SELECT %[1]s FROM read_parquet(?, hive_partitioning=true))),
					(SELECT count(*) FROM (SELECT %[1]s FROM read_parquet(?, hive_partitioning=true) EXCEPT ALL SELECT %[1]s FROM read_parquet(?, hive_partitioning=true)))`, columns),
					full, full, inc, inc, full).Scan(&rows, &missing, &extra), dataset)
				require.Positive(rows, dataset)
				assert.Zero(missing, dataset)
				assert.Zero(extra, dataset)
			}
		})
	}
}
