package cmd

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type unsentCacheFixture struct {
	store                                                            *store.Store
	dbPath, analytics                                                string
	sourceID, conversationID, ownerID, aliceID, draftID, sentLabelID int64
	sentAt                                                           time.Time
}

func newUnsentCacheFixture(t *testing.T) unsentCacheFixture {
	t.Helper()
	testutil.SkipIfPostgres(t, "analytics cache is SQLite-only")
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	analytics := filepath.Join(t.TempDir(), "analytics")
	st, err := store.Open(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	src, err := st.GetOrCreateSource("gmail", "owner@example.test")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(src.ID, "owner@example.test", "manual"))
	labels, err := st.EnsureLabelsBatch(src.ID, map[string]store.LabelInfo{
		"DRAFT": {Name: "Drafts", Type: "system", SystemRole: store.LabelSystemRoleDrafts},
		"SENT":  {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
	})
	require.NoError(err)
	owner, err := st.EnsureParticipant("owner@example.test", "Owner", "example.test")
	require.NoError(err)
	alice, err := st.EnsureParticipant("alice@example.test", "Alice", "example.test")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(src.ID, "thread", "email_thread", "Hello")
	require.NoError(err)
	july1 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	july20 := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	received, err := st.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: conv, SourceMessageID: "received", MessageType: "email",
		SenderID: sql.NullInt64{Int64: alice, Valid: true}, SentAt: sql.NullTime{Time: july1, Valid: true},
	})
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(received, "to", []int64{owner}, []string{"Owner"}))
	_, err = buildCacheAuto(dbPath, analytics)
	require.NoError(err)

	draft, err := st.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: conv, SourceMessageID: "draft", MessageType: "email",
		SenderID: sql.NullInt64{Int64: owner, Valid: true}, SentAt: sql.NullTime{Time: july20, Valid: true}, IsFromMe: true,
	})
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(draft, "to", []int64{alice}, []string{"Alice"}))
	require.NoError(st.ReplaceMessageLabels(draft, []int64{labels["DRAFT"]}))
	_, err = buildCacheAuto(dbPath, analytics)
	require.NoError(err)
	clean := cacheNeedsBuild(dbPath, analytics)
	require.False(clean.NeedsBuild, "%+v", clean)

	f := unsentCacheFixture{store: st, dbPath: dbPath, analytics: analytics,
		sourceID: src.ID, conversationID: conv, ownerID: owner, aliceID: alice,
		draftID: draft, sentLabelID: labels["SENT"], sentAt: july20}
	return f
}

func (f unsentCacheFixture) unsentCount(t *testing.T) int {
	t.Helper()
	duck, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	defer func() { require.NoError(t, duck.Close()) }()
	var count int
	require.NoError(t, duck.QueryRow(`SELECT COUNT(*) FROM read_parquet(?, hive_partitioning=true) WHERE message_id = ? AND unsent`, filepath.Join(f.analytics, "relationship_activity", "**", "*.parquet"), f.draftID).Scan(&count))
	return count
}

func TestBuildCacheRefreshesUnsentActivity(t *testing.T) {
	for _, tc := range []struct{ name, csv string }{{"scanner", ""}, {"csv", "1"}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", tc.csv)
			require := require.New(t)
			assert := assert.New(t)
			f := newUnsentCacheFixture(t)
			_, err := f.store.ReconcileMessageLabels(f.draftID, []int64{f.sentLabelID}, true)
			require.NoError(err)
			drift := cacheNeedsBuild(f.dbPath, f.analytics)
			assert.True(drift.NeedsBuild)
			assert.True(drift.HasUnsentDrift)
			serving, err := cacheNeedsBuildForServing(t.Context(), f.dbPath, f.analytics)
			require.NoError(err)
			assert.False(serving.HasUnsentDrift, "request-time checks defer the archive-wide unsent fingerprint")
			result, err := buildCacheAuto(f.dbPath, f.analytics)
			require.NoError(err)
			assert.True(result.IdentityOnly, "%+v, drift=%+v", result, drift)
			clean := cacheNeedsBuild(f.dbPath, f.analytics)
			assert.False(clean.NeedsBuild, "%+v", clean)
			assert.Zero(f.unsentCount(t))
		})
	}
}

// A label writer can send a covered draft after staleness is checked but
// before the export snapshot. Repair activity without rewriting base shards.
func TestBuildCacheAppendRechecksCoveredUnsentSnapshot(t *testing.T) {
	for _, tc := range []struct{ name, csv string }{{"scanner", ""}, {"csv", "1"}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", tc.csv)
			require := require.New(t)
			assert := assert.New(t)
			f := newUnsentCacheFixture(t)
			retained := make(map[string][]byte)
			for _, pattern := range []string{
				filepath.Join(f.analytics, "messages", "**", "*.parquet"),
				filepath.Join(f.analytics, "message_recipients", "*.parquet"),
			} {
				files, err := filepath.Glob(pattern)
				require.NoError(err)
				require.NotEmpty(files)
				for _, file := range files {
					retained[file], err = os.ReadFile(file)
					require.NoError(err)
				}
			}
			incoming, err := f.store.UpsertMessage(&store.Message{
				SourceID: f.sourceID, ConversationID: f.conversationID, SourceMessageID: "incoming", MessageType: "email",
				SenderID: sql.NullInt64{Int64: f.aliceID, Valid: true}, SentAt: sql.NullTime{Time: f.sentAt.Add(time.Hour), Valid: true},
			})
			require.NoError(err)
			require.NoError(f.store.ReplaceMessageRecipients(incoming, "to", []int64{f.ownerID}, []string{"Owner"}))
			drift := cacheNeedsBuild(f.dbPath, f.analytics)
			require.False(drift.FullRebuild)
			changed, err := f.store.ReconcileMessageLabels(f.draftID, []int64{f.sentLabelID}, true)
			require.NoError(err)
			require.True(changed)
			// Continue the checked append decision, as the scheduled builder does.
			// The pinned snapshot must detect the send without a second check.
			_, err = buildCacheImpl(f.dbPath, f.analytics, drift.FullRebuild, false)
			require.NoError(err)
			for file, before := range retained {
				after, err := os.ReadFile(file)
				require.NoError(err)
				assert.Equal(before, after, "retained base shard %s", file)
			}
			assert.Zero(f.unsentCount(t), "the retained draft activity reflects its sent state")
		})
	}
}
