package cmd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

func policyCacheFixture(t *testing.T) (*config.Config, *store.Store) {
	t.Helper()
	c, s := openTestDaemonAnalyticsStore(t)
	_, err := s.DB().Exec(`
 INSERT INTO sources(id,source_type,identifier) VALUES(1,'beeper','policy@example.com');
 INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type) VALUES(1,1,'full-room','group_chat'),(2,1,'cold-room','group_chat');
 INSERT INTO participants(id,email_address,display_name) VALUES(1,'full@example.com','Full Sender'),(2,'cold@example.com','Cold Sender');
 INSERT INTO messages(id,source_id,source_message_id,conversation_id,message_type,sent_at,sender_id,subject,snippet,has_attachments,attachment_count,size_estimate) VALUES(1,1,'m1',1,'beeper','2024-01-01 00:00:00',1,'needle full','retained full',1,1,10),(2,1,'m2',2,'beeper','2024-01-02 00:00:00',2,'needle cold','retained cold',1,1,20);
 INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(1,1,'from'),(2,2,'from');
 INSERT INTO attachments(id,message_id,filename,mime_type,size,storage_path) VALUES(1,1,'full.txt','text/plain',3,'full.txt'),(2,2,'cold.txt','text/plain',7,'cold.txt');
 `)
	require.NoError(t, err)
	require.NoError(t, s.SetConversationPolicyOverride(t.Context(), 1, "full-room", conversationpolicy.Full))
	require.NoError(t, s.SetConversationPolicyOverride(t.Context(), 1, "cold-room", conversationpolicy.Cold))
	return c, s
}

func TestBuildCacheExportsColdLexicalButNotRelationships(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	c, s := policyCacheFixture(t)
	_, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
	requirements.NoError(err)
	state, err := query.ReadCacheSyncState(c.AnalyticsDir())
	requirements.NoError(err)
	revision, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	assertions.Equal(revision, state.ConversationPolicyRevision)
	assertions.Equal(int64(1), state.Stats.TotalMessages)
	e, err := query.NewDuckDBEngine(c.AnalyticsDir(), "", nil)
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	for _, view := range []string{"messages", "v_messages", "analytical_entries"} {
		result, err := e.QuerySQL(t.Context(), "SELECT COUNT(*) FROM "+view)
		requirements.NoError(err)
		assertions.EqualValues(1, result.Rows[0][0], view)
	}
	explored, err := e.Explore(t.Context(), query.ExploreRequest{})
	requirements.NoError(err)
	assertions.Equal(int64(1), explored.TotalCount)
	files, err := e.SearchFiles(t.Context(), query.FileSearchRequest{})
	requirements.NoError(err)
	assertions.Equal(int64(1), files.TotalCount)
	stats, err := e.GetTotalStats(t.Context(), query.StatsOptions{SearchScope: true})
	requirements.NoError(err)
	assertions.Equal(int64(1), stats.MessageCount)
	textStats, err := e.GetTextStats(t.Context(), query.TextStatsOptions{})
	requirements.NoError(err)
	assertions.Equal(int64(1), textStats.MessageCount)
	for _, name := range []string{"relationship_activity", "relationship_people", "relationship_daily", "relationship_logical_contributions", "relationship_temperature_contributions"} {
		result, err := e.QuerySQL(t.Context(), "SELECT COUNT(*) FROM read_parquet('"+strings.ReplaceAll(filepath.ToSlash(filepath.Join(c.AnalyticsDir(), name, "**", "*.parquet")), "'", "''")+"') WHERE canonical_id=2")
		requirements.NoError(err)
		assertions.EqualValues(0, result.Rows[0][0], name)
	}
	rows, err := e.SearchFast(t.Context(), search.Parse("needle"), query.MessageFilter{}, 10, 0)
	requirements.NoError(err)
	assertions.Len(rows, 1)
	rows, err = e.SearchFast(t.Context(), search.Parse("needle include:cold"), query.MessageFilter{}, 10, 0)
	requirements.NoError(err)
	assertions.Len(rows, 2)
	page, err := e.SearchFastWithStats(t.Context(), search.Parse("needle include:cold"), "needle include:cold", query.MessageFilter{}, query.ViewSenders, 10, 0)
	requirements.NoError(err)
	assertions.Equal(int64(2), page.TotalCount)
	assertions.Len(page.Messages, 2)
	defaultPage, err := e.SearchFastWithStats(t.Context(), search.Parse("needle"), "needle", query.MessageFilter{}, query.ViewSenders, 10, 0)
	requirements.NoError(err)
	assertions.Equal(int64(1), defaultPage.TotalCount)
	detail, err := e.ListConversationMessages(t.Context(), 2, query.TextFilter{})
	requirements.NoError(err)
	assertions.Len(detail, 1)
	physical, err := e.QuerySQL(t.Context(), "SELECT conversation_tier,COUNT(*) FROM read_parquet('"+strings.ReplaceAll(filepath.ToSlash(filepath.Join(c.AnalyticsDir(), "messages", "**", "*.parquet")), "'", "''")+"') GROUP BY conversation_tier ORDER BY conversation_tier")
	requirements.NoError(err)
	requirements.Len(physical.Rows, 2)
	assertions.Equal("cold", physical.Rows[0][0])
	assertions.EqualValues(1, physical.Rows[0][1])
	assertions.Equal("full", physical.Rows[1][0])
	for _, dataset := range []string{"message_recipients", "attachments"} {
		physical, err := e.QuerySQL(t.Context(), "SELECT COUNT(*) FROM read_parquet('"+strings.ReplaceAll(filepath.ToSlash(filepath.Join(c.AnalyticsDir(), dataset, "*.parquet")), "'", "''")+"') WHERE message_id=2")
		requirements.NoError(err)
		assertions.EqualValues(1, physical.Rows[0][0], "Cold archival children: "+dataset)
	}

	jobs := newCacheBuildJobs(t.Context(), nil, func(context.Context, buildCacheMode) error { return nil })
	result, _, err := runDaemonSQLQueryWithJobs(t.Context(), c, s, e, "SELECT COUNT(*) FROM messages", daemonSQLQueryOptions{archiveOnly: true}, jobs)
	requirements.NoError(err)
	requirements.NotNil(result)
	assertions.EqualValues(1, result.Rows[0][0])
}

func TestBuildCachePolicyChangeDuringSnapshotRejectsPublication(t *testing.T) {
	for _, phase := range []string{"snapshot", "before-moves"} {
		t.Run(phase, func(t *testing.T) {
			requirements := require.New(t)

			c, s := policyCacheFixture(t)
			_, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
			requirements.NoError(err)
			before, err := os.ReadFile(query.CacheStatePath(c.AnalyticsDir()))
			requirements.NoError(err)
			change := func() {
				require.NoError(t, s.SetConversationPolicyOverride(t.Context(), 1, "full-room", conversationpolicy.Cold))
			}
			if phase == "snapshot" {
				buildCacheAfterSnapshotHook = change
			} else {
				buildCacheBeforePublicationMovesHook = func() error { change(); return nil }
			}
			t.Cleanup(func() { buildCacheAfterSnapshotHook = nil; buildCacheBeforePublicationMovesHook = nil })
			_, err = buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
			requirements.ErrorIs(err, store.ErrConversationPolicyStale)
			after, err := os.ReadFile(query.CacheStatePath(c.AnalyticsDir()))
			requirements.NoError(err)
			assert.Equal(t, before, after)
		})
	}
}

func TestIdentityRefreshPolicyChangeForcesRebuild(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	c, s := policyCacheFixture(t)
	_, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), 1, "full-room", conversationpolicy.Cold))
	stale := cacheNeedsBuild(c.DatabaseDSN(), c.AnalyticsDir())
	assertions.True(stale.FullRebuild)
	assertions.False(stale.HasUsablePublication)
	_, err = refreshDerivedDatasetsOnly(t.Context(), c.DatabaseDSN(), c.AnalyticsDir(), false)
	requirements.ErrorIs(err, ErrDerivedRefreshRequiresFullBuild)
	_, err = buildCache(c.DatabaseDSN(), c.AnalyticsDir(), false)
	requirements.NoError(err)
	state, err := query.ReadCacheSyncState(c.AnalyticsDir())
	requirements.NoError(err)
	revision, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	assertions.Equal(revision, state.ConversationPolicyRevision)
	assertions.Equal(int64(0), state.Stats.TotalMessages)
	e, err := openDaemonDuckDBEngine(c, s)
	requirements.NoError(err, "real rebuild must converge with live policy authority (native19092)")
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	result, err := e.QuerySQL(t.Context(), "SELECT COUNT(*) FROM messages")
	requirements.NoError(err)
	assertions.EqualValues(0, result.Rows[0][0])
	for _, name := range []string{"relationship_activity", "relationship_people", "relationship_daily", "relationship_logical_contributions", "relationship_temperature_contributions"} {
		count, err := e.QuerySQL(t.Context(), "SELECT COUNT(*) FROM read_parquet('"+strings.ReplaceAll(filepath.ToSlash(filepath.Join(c.AnalyticsDir(), name, "**", "*.parquet")), "'", "''")+"')")
		requirements.NoError(err)
		assertions.EqualValues(0, count.Rows[0][0], "rebuild removes old compact contributions: "+name)
	}
}

func TestCachePolicyMaintenanceInvalidatesBeforeExclusiveMutation(t *testing.T) {
	requirements := require.New(t)

	c, s := policyCacheFixture(t)
	_, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
	requirements.NoError(err)
	e, err := query.NewDuckDBEngine(c.AnalyticsDir(), "", nil)
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	release, err := lockCacheAndInvalidateSyncState(s, c.AnalyticsDir())
	requirements.NoError(err)
	defer func() { require.NoError(t, release()) }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	requirements.NoError(s.SetConversationPolicyOverride(ctx, 1, "full-room", conversationpolicy.Cold))
	_, err = e.QuerySQL(ctx, "SELECT COUNT(*) FROM messages")
	requirements.ErrorIs(err, query.ErrCacheUnavailable)
	_, err = buildCacheLocked(c.DatabaseDSN(), c.AnalyticsDir(), true, false)
	requirements.NoError(err)
	_, err = e.QuerySQL(t.Context(), "SELECT COUNT(*) FROM messages")
	requirements.NoError(err)
}

func TestConversationPolicyLexicalWorksWhileCacheRebuilds(t *testing.T) {
	requirements := require.New(t)

	c, s := policyCacheFixture(t)
	_, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
	requirements.NoError(err)
	e, err := query.NewDuckDBEngine(c.AnalyticsDir(), "", s.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, PolicyStore: s})
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	called := false
	buildCacheAfterSnapshotHook = func() {
		called = true
		rows, err := e.SearchFast(t.Context(), search.Parse("needle include:cold"), query.MessageFilter{}, 10, 0)
		require.NoError(t, err)
		assert.Len(t, rows, 2)
	}
	t.Cleanup(func() { buildCacheAfterSnapshotHook = nil })
	_, err = buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
	requirements.NoError(err)
	assert.True(t, called)
}

func TestIdentityRefreshPolicyRaceRejectsPublication(t *testing.T) {
	for _, phase := range []string{"snapshot", "publication"} {
		t.Run(phase, func(t *testing.T) {
			requirements := require.New(t)

			c, s := policyCacheFixture(t)
			_, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
			requirements.NoError(err)
			before, err := os.ReadFile(query.CacheStatePath(c.AnalyticsDir()))
			requirements.NoError(err)
			_, err = s.DB().Exec("UPDATE conversations SET title='Changed title' WHERE id=1")
			requirements.NoError(err)
			change := func() {
				require.NoError(t, s.SetConversationPolicyOverride(t.Context(), 1, "full-room", conversationpolicy.Cold))
			}
			if phase == "snapshot" {
				derivedRefreshBeforeSnapshotHook = change
			} else {
				buildCacheBeforePublicationMovesHook = func() error { change(); return nil }
			}
			t.Cleanup(func() { derivedRefreshBeforeSnapshotHook = nil; buildCacheBeforePublicationMovesHook = nil })
			_, err = refreshDerivedDatasetsOnly(t.Context(), c.DatabaseDSN(), c.AnalyticsDir(), false)
			if phase == "snapshot" {
				requirements.ErrorIs(err, ErrDerivedRefreshRequiresFullBuild)
			} else {
				requirements.ErrorIs(err, store.ErrConversationPolicyStale)
			}
			after, err := os.ReadFile(query.CacheStatePath(c.AnalyticsDir()))
			requirements.NoError(err)
			assert.Equal(t, before, after)
		})
	}
}

func TestBuildCachePolicyFenceHeldThroughPublication(t *testing.T) {
	c, s := policyCacheFixture(t)
	reached := false
	cachePublicationAfterMoveHook = func(string) error {
		if reached {
			return nil
		}
		reached = true
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond) //nolint:kennlint // deadlinetest: the deadline is the expected result; publication holds policy R through renames and marker commit
		defer cancel()
		release, err := s.AcquireSQLiteConversationPolicyFence(ctx, store.ConversationPolicyExclusive)
		if release != nil {
			release()
		}
		assert.ErrorIs(t, err, context.DeadlineExceeded, "publication must keep policy stable through every rename and marker commit")
		return nil
	}
	t.Cleanup(func() { cachePublicationAfterMoveHook = nil })
	_, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
	require.NoError(t, err)
	require.True(t, reached)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, s.SetConversationPolicyOverride(ctx, 1, "full-room", conversationpolicy.Cold))
}

func TestBuildCachePolicyCSVProjection(t *testing.T) {
	requirements := require.New(t)

	previous := cacheSnapshotGOOS
	cacheSnapshotGOOS = "windows"
	t.Cleanup(func() { cacheSnapshotGOOS = previous })
	c, s := policyCacheFixture(t)
	_, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
	requirements.NoError(err)
	e, err := openDaemonDuckDBEngine(c, s)
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	result, err := e.QuerySQL(t.Context(), "SELECT COUNT(*) FROM messages")
	requirements.NoError(err)
	assert.EqualValues(t, 1, result.Rows[0][0])
}

func TestIdentityRefreshPolicyKeepsColdChildStatisticsExcluded(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	c, s := policyCacheFixture(t)
	_, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
	requirements.NoError(err)
	_, err = s.DB().Exec("UPDATE attachments SET size=70 WHERE id=2")
	requirements.NoError(err)
	_, err = refreshDerivedDatasetsOnly(t.Context(), c.DatabaseDSN(), c.AnalyticsDir(), true)
	requirements.NoError(err)
	state, err := query.ReadCacheSyncState(c.AnalyticsDir())
	requirements.NoError(err)
	assertions.Equal(int64(3), state.Stats.AttachmentSizeBytes)
	assertions.Equal(int64(2), state.ArchivedMessageCount)
	revision, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	assertions.Equal(revision, state.ConversationPolicyRevision)
	assertions.False(cacheNeedsBuild(c.DatabaseDSN(), c.AnalyticsDir()).NeedsBuild, "refresh converges without comparing Full count against archival count")
}

func TestBuildCachePolicyExplicitDeletionRetainsCold(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	c, s := policyCacheFixture(t)
	_, err := s.DB().Exec("UPDATE sources SET source_type='gmail'; UPDATE messages SET message_type='email'")
	requirements.NoError(err)
	_, err = buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
	requirements.NoError(err)
	e, err := query.NewDuckDBEngine(c.AnalyticsDir(), "", nil)
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	filtered, err := e.GetDeletionTargetsByFilter(t.Context(), query.MessageFilter{})
	requirements.NoError(err)
	requirements.Len(filtered, 1)
	assertions.Equal(int64(1), filtered[0].MessageID)
	explicit, err := e.GetDeletionTargetsByMessageIDs(t.Context(), []int64{2})
	requirements.NoError(err)
	requirements.Len(explicit, 1)
	assertions.Equal(int64(2), explicit[0].MessageID)
}

func TestDuckDBPolicyScannerRetainsExplicitColdScope(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("DuckDB sqlite_scanner is unavailable in Windows CI")
	}
	requirements := require.New(t)
	assertions := assert.New(t)

	c, s := policyCacheFixture(t)
	// A nil direct SQLite connection forces Search through the real scanner.
	e, err := query.NewDuckDBEngine("", c.DatabaseDSN(), nil)
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	rows, err := e.Search(t.Context(), search.Parse("needle"), 10, 0)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(int64(1), rows[0].ID)
	rows, err = e.Search(t.Context(), search.Parse("needle include:cold"), 10, 0)
	requirements.NoError(err)
	requirements.Len(rows, 2)
	_, err = s.DB().Exec("UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=2")
	requirements.NoError(err)
	q := search.Parse("needle include:cold")
	q.DeletionScope = search.DeletionScopeDeleted
	rows, err = e.Search(t.Context(), q, 10, 0)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(int64(2), rows[0].ID)
	q.IncludeCold = false
	rows, err = e.Search(t.Context(), q, 10, 0)
	requirements.NoError(err)
	assertions.Empty(rows)
}

func TestBuildCachePolicyLexicalNeverRelabelsExcludedTiers(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	c, s := policyCacheFixture(t)
	_, err := s.DB().Exec(`
 INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type) VALUES(3,1,'missing-room','group_chat'),(4,1,'stub-room','group_chat'),(5,1,'skip-room','group_chat');
 INSERT INTO messages(id,source_id,source_message_id,conversation_id,message_type,sent_at,subject) VALUES(3,1,'m3',3,'beeper','2024-01-03','needle missing'),(4,1,'m4',4,'beeper','2024-01-04','needle stub'),(5,1,'m5',5,'beeper','2024-01-05','needle skip');
 INSERT INTO conversation_policy(conversation_id,effective_tier) VALUES(4,'stub'),(5,'skip');`)
	requirements.NoError(err)
	_, err = buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
	requirements.NoError(err)
	e, err := query.NewDuckDBEngine(c.AnalyticsDir(), "", nil)
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	for _, tc := range []struct {
		text string
		want int
	}{{"needle", 1}, {"needle include:cold", 2}} {
		rows, err := e.SearchFast(t.Context(), search.Parse(tc.text), query.MessageFilter{}, 20, 0)
		requirements.NoError(err)
		assertions.Len(rows, tc.want, tc.text)
	}
	for _, id := range []int64{2, 3, 4, 5} {
		rows, err := e.ListConversationMessages(t.Context(), id, query.TextFilter{})
		requirements.NoError(err)
		assertions.Len(rows, 1, "explicit archival conversation %d", id)
	}
	physical, err := e.QuerySQL(t.Context(), "SELECT id,conversation_tier FROM read_parquet('"+strings.ReplaceAll(filepath.ToSlash(filepath.Join(c.AnalyticsDir(), "messages", "**", "*.parquet")), "'", "''")+"') ORDER BY id")
	requirements.NoError(err)
	requirements.Len(physical.Rows, 5)
	assertions.Nil(physical.Rows[2][1])
	assertions.Equal("stub", physical.Rows[3][1])
	assertions.Equal("skip", physical.Rows[4][1])
}
