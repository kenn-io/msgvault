package cmd

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func TestBuildCacheMergedHistoryDeletionEligibility(t *testing.T) {
	for _, mode := range []string{"scanner", "csv"} {
		t.Run(mode, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			if mode == "csv" {
				t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "1")
			}
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "archive.db")
			analytics := filepath.Join(dir, "analytics")
			st, err := store.OpenForTest(dbPath)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(st.Close()) })
			require.NoError(st.InitSchema())
			history, err := st.GetOrCreateSource("gmail", "history@example.test")
			require.NoError(err)
			live, err := st.GetOrCreateSource("gmail", "live@example.test")
			require.NoError(err)
			var ids []int64
			for _, source := range []*store.Source{history, live} {
				conversation, err := st.EnsureConversation(source.ID, "thread", "Synthetic message")
				require.NoError(err)
				id, err := st.UpsertMessage(&store.Message{
					SourceID: source.ID, ConversationID: conversation, SourceMessageID: "provider-message",
					MessageType: "email", Subject: sql.NullString{String: source.Identifier, Valid: true},
					SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), Valid: true},
				})
				require.NoError(err)
				ids = append(ids, id)
			}
			_, err = buildCache(dbPath, analytics, false)
			require.NoError(err)
			_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
				FromSourceID: history.ID, IntoSourceID: live.ID,
			})
			require.NoError(err)
			staleness := cacheNeedsBuildContext(t.Context(), dbPath, analytics)
			assert.True(staleness.NeedsBuild)
			assert.True(staleness.FullRebuild)
			_, err = buildCacheAuto(dbPath, analytics)
			require.NoError(err)

			for _, attached := range []bool{true, false} {
				var db *sql.DB
				if attached {
					db = st.DB()
				}
				engine, err := query.NewDuckDBEngine(analytics, "", db)
				require.NoError(err)
				t.Cleanup(func() { require.NoError(engine.Close()) })
				stats, err := engine.ExploreSelectionStats(t.Context(), query.ExploreSelectionRequest{
					IncludeDeletableMessageIDs: true,
				})
				require.NoError(err)
				assert.Equal(int64(2), stats.Count)
				assert.Equal(int64(1), stats.DeletableCount)
				assert.Equal([]int64{ids[1]}, stats.DeletableMessageIDs)
				targets, err := engine.GetDeletionTargetsByMessageIDs(t.Context(), ids)
				require.NoError(err)
				require.Len(targets, 1)
				assert.Equal(ids[1], targets[0].MessageID)
				targets, err = engine.GetDeletionTargetsByFilter(t.Context(), query.MessageFilter{})
				require.NoError(err)
				require.Len(targets, 1)
				assert.Equal(ids[1], targets[0].MessageID)
				historyStats, err := engine.ExploreSelectionStats(t.Context(), query.ExploreSelectionRequest{
					ExcludedKeys: []string{fmt.Sprintf("source:%d:message:provider-message", live.ID)}, IncludeDeletableMessageIDs: true,
				})
				require.NoError(err)
				assert.Equal(int64(1), historyStats.Count)
				assert.Zero(historyStats.DeletableCount)
				assert.Empty(historyStats.DeletableMessageIDs)
			}
		})
	}
}
