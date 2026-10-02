package cmd

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

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
			require.NoError(st.AddAccountIdentity(src.ID, "mask@example.org", "fastmail-masked-email"))
			conv, err := st.EnsureConversation(src.ID, "thread", "synthetic thread")
			require.NoError(err)
			for _, header := range []string{"X-Delivered-To: work@example.org", "X-Delivered-To: mask@example.org", "X-Delivered-To: work@example.org, mask@example.org"} {
				_, err = st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: header, MessageType: "email", SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}}, RawMIME: []byte("From: sender@example.test\r\n" + header + "\r\n\r\nbody")})
				require.NoError(err)
			}
			calendar, err := st.GetOrCreateSource("gcal", "calendar-login@example.net")
			require.NoError(err)
			require.NoError(st.UpdateSourceSyncConfig(calendar.ID, `{"calendar_id":"work@example.org"}`))
			require.NoError(st.AddAccountIdentity(calendar.ID, "work@example.org", "manual"))
			calendarConv, err := st.EnsureConversation(calendar.ID, "event", "Synthetic calendar")
			require.NoError(err)
			_, err = st.UpsertMessage(&store.Message{SourceID: calendar.ID, ConversationID: calendarConv, SourceMessageID: "event", MessageType: "calendar_event", SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}})
			require.NoError(err)
			cache := filepath.Join(t.TempDir(), "analytics")
			_, err = buildCache(path, cache, true)
			require.NoError(err)
			engine, err := query.NewDuckDBEngine(cache, "", nil)
			require.NoError(err)
			defer func() { require.NoError(engine.Close()) }()
			for _, selector := range []string{"account:work@example.org", "account:fastmail-masked:inbox@example.net", "account:unattributed", "received:work@example.org"} {
				for _, padded := range []bool{false, true} {
					parsed := search.Parse(selector)
					if padded {
						for i := range parsed.AccountScopes {
							for j, address := range parsed.AccountScopes[i].Addresses {
								parsed.AccountScopes[i].Addresses[j] = " \t" + strings.ToUpper(address) + " \n"
							}
							for j, group := range parsed.AccountScopes[i].Groups {
								parsed.AccountScopes[i].Groups[j] = " \t" + strings.ToUpper(group) + " \n"
							}
						}
					}
					require.NoError(search.ValidateAccountScopes(parsed.AccountScopes))
					want := 1
					if selector == "account:work@example.org" {
						want = 2
					}
					rows, err := engine.ListMessages(t.Context(), query.MessageFilter{AccountScopes: parsed.AccountScopes})
					require.NoError(err)
					assert.Len(rows, want, selector)
					count, err := engine.SearchFastCount(t.Context(), parsed, query.MessageFilter{})
					require.NoError(err)
					assert.Equal(int64(want), count, selector)
					explored, err := engine.Explore(t.Context(), query.ExploreRequest{Context: query.Context{AccountScopes: parsed.AccountScopes}})
					require.NoError(err)
					assert.Len(explored.Rows, want, selector)
					stats, err := engine.GetTotalStats(t.Context(), query.StatsOptions{Filter: &query.MessageFilter{AccountScopes: parsed.AccountScopes}})
					require.NoError(err)
					assert.Equal(int64(want), stats.MessageCount, selector)
					aggregate, err := engine.Aggregate(t.Context(), query.ViewTime, query.AggregateOptions{AccountScopes: parsed.AccountScopes})
					require.NoError(err)
					var total int64
					for _, row := range aggregate {
						total += row.Count
					}
					assert.Equal(int64(want), total, selector)
					aggregate, err = engine.SubAggregate(t.Context(), query.MessageFilter{AccountScopes: parsed.AccountScopes}, query.ViewTime, query.AggregateOptions{})
					require.NoError(err)
					total = 0
					for _, row := range aggregate {
						total += row.Count
					}
					assert.Equal(int64(want), total, selector)
				}
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
			Message: &store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: key, MessageType: "email", SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}},
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
	assert.False(stale.FullRebuild, "new attribution is carried by append: %s", stale.Reason)
	built, err := buildCacheAuto(c.DatabaseDSN(), c.AnalyticsDir())
	require.NoError(err)
	assert.Equal(int64(1), built.StagedCount)
	// Changes below the published boundary must still replace cached facts.
	require.NoError(st.UpsertMessageRaw(first, []byte("X-Delivered-To: work@example.org, mask@example.org\r\n\r\nbody")))
	stale, err = cacheNeedsBuildForServing(t.Context(), c.DatabaseDSN(), c.AnalyticsDir())
	require.NoError(err)
	assert.True(stale.HasDerivedDataDrift)
	assert.True(stale.FullRebuild)
}
