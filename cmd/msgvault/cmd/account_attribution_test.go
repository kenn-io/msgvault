package cmd

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

var attributionSentAt = sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}

func summaryIDs(rows []query.MessageSummary) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	slices.Sort(ids)
	return ids
}

// TestAccountAttributionCacheParity compares account: and received: on the
// exported cache with the live SQLite archive, through both snapshot paths.
func TestAccountAttributionCacheParity(t *testing.T) {
	for _, csv := range []bool{false, true} {
		t.Run(map[bool]string{false: "scanner", true: "csv"}[csv], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			if csv {
				t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "1")
			}
			path := filepath.Join(t.TempDir(), "archive.db")
			st, err := store.OpenForTest(path)
			require.NoError(err)
			defer func() { require.NoError(st.Close()) }()
			require.NoError(st.InitSchema())
			src, err := st.GetOrCreateSource("gmail", "inbox@example.net")
			require.NoError(err)
			require.NoError(st.AddAccountIdentity(src.ID, "work@example.org", "manual"))
			require.NoError(st.AddAccountIdentity(src.ID, "mask@example.org", "manual"))
			conv, err := st.EnsureConversation(src.ID, "thread", "synthetic thread")
			require.NoError(err)
			for _, header := range []string{"X-Delivered-To: work@example.org", "X-Delivered-To: mask@example.org", "X-Delivered-To: work@example.org, mask@example.org"} {
				_, err = st.PersistMessageContext(t.Context(), &store.MessagePersistData{
					Message: &store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: header, MessageType: "email", SentAt: attributionSentAt},
					RawMIME: []byte("From: sender@example.test\r\n" + header + "\r\n\r\nbody"),
				})
				require.NoError(err)
			}
			calendar, err := st.GetOrCreateSource("gcal", "calendar-login@example.net")
			require.NoError(err)
			require.NoError(st.UpdateSourceSyncConfig(calendar.ID, `{"account_email":"work@example.org","calendar_id":"primary"}`))
			require.NoError(st.AddAccountIdentity(calendar.ID, "work@example.org", "manual"))
			calendarConv, err := st.EnsureConversation(calendar.ID, "event", "Synthetic calendar")
			require.NoError(err)
			_, err = st.UpsertMessage(&store.Message{SourceID: calendar.ID, ConversationID: calendarConv, SourceMessageID: "event", MessageType: "calendar_event", SentAt: attributionSentAt})
			require.NoError(err)

			cache := filepath.Join(t.TempDir(), "analytics")
			_, err = buildCache(path, cache, true)
			require.NoError(err)
			duck, err := query.NewDuckDBEngine(cache, path, nil)
			require.NoError(err)
			defer func() { require.NoError(duck.Close()) }()
			live := query.NewSQLiteEngine(st.DB())
			for selector, want := range map[string]int{
				"account:work@example.org":                            2,
				"received:work@example.org":                           1,
				"account:mask@example.org":                            1,
				"received:work@example.org received:mask@example.org": 2,
				"account:work@example.org received:work@example.org":  1,
			} {
				parsed := search.Parse(selector)
				require.NoError(parsed.Err())
				liveCount, err := live.SearchFastCount(t.Context(), parsed, query.MessageFilter{})
				require.NoError(err)
				duckCount, err := duck.SearchFastCount(t.Context(), parsed, query.MessageFilter{})
				require.NoError(err)
				assert.Equal(int64(want), liveCount, selector)
				assert.Equal(liveCount, duckCount, selector)
				liveRows, err := live.Search(t.Context(), parsed, 100, 0)
				require.NoError(err)
				// The sqlite_scan fallback exists only where DuckDB loads the
				// scanner extension; the Parquet path below runs everywhere.
				duckRows, err := duck.Search(t.Context(), parsed, 100, 0)
				if err == nil || !strings.Contains(err.Error(), "Search requires SQLite") {
					require.NoError(err, selector)
					assert.Equal(summaryIDs(liveRows), summaryIDs(duckRows), selector)
				}
				cachedRows, err := duck.SearchFast(t.Context(), parsed, query.MessageFilter{}, 100, 0)
				require.NoError(err)
				assert.Equal(summaryIDs(liveRows), summaryIDs(cachedRows), selector)
			}
		})
	}
}

func TestAccountAttributionAppendKeepsCacheIncremental(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, st := openTestDaemonAnalyticsStore(t)
	src, err := st.GetOrCreateSource("gmail", "inbox@example.net")
	require.NoError(err)
	for _, address := range []string{"work@example.org", "mask@example.org"} {
		require.NoError(st.AddAccountIdentity(src.ID, address, "manual"))
	}
	conv, err := st.EnsureConversation(src.ID, "thread", "synthetic thread")
	require.NoError(err)
	persist := func(key string) int64 {
		id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
			Message: &store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: key, MessageType: "email", SentAt: attributionSentAt},
			RawMIME: []byte("X-Delivered-To: work@example.org\r\n\r\nbody"),
		})
		require.NoError(err)
		return id
	}
	first := persist("first")
	_, err = buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
	require.NoError(err)

	persist("appended")
	stale, err := cacheNeedsBuildForServing(t.Context(), c.DatabaseDSN(), c.AnalyticsDir())
	require.NoError(err)
	assert.True(stale.HasNew)
	assert.False(stale.FullRebuild, "a message carrying X-Delivered-To appends: %s", stale.Reason)
	built, err := buildCacheAuto(c.DatabaseDSN(), c.AnalyticsDir())
	require.NoError(err)
	assert.Equal(int64(1), built.StagedCount)

	// New delivery evidence flips a published row's account.
	require.NoError(st.UpsertMessageRaw(first, []byte("X-Delivered-To: mask@example.org\r\n\r\nbody")))
	stale, err = cacheNeedsBuildForServing(t.Context(), c.DatabaseDSN(), c.AnalyticsDir())
	require.NoError(err)
	assert.True(stale.FullRebuild, "a published row whose account changed forces a rebuild: %s", stale.Reason)
}
