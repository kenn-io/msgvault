package cmd

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/slack"
	"go.kenn.io/msgvault/internal/store"
)

func TestBuildCache_RefreshesReplyUpdatedBeforeSlackYield(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbPath := filepath.Join(t.TempDir(), "msgvault.db")
	analyticsDir := filepath.Join(t.TempDir(), "analytics")
	st, err := store.Open(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())

	var edited atomic.Bool
	var replyRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Oauth-Scopes", "channels:read,channels:history,users:read")
		response := map[string]any{"ok": true}
		root := map[string]any{
			"type": "message", "user": "UME", "ts": "1750000000.000001",
			"text": "Thread root", "thread_ts": "1750000000.000001",
			"reply_count": 1, "latest_reply": "1750000060.000001",
		}
		switch r.URL.Path {
		case "/users.list":
			response["members"] = []any{map[string]any{"id": "UME", "name": "Example"}}
		case "/conversations.list":
			response["channels"] = []any{
				map[string]any{"id": "C01", "name": "first", "is_channel": true, "is_member": true},
				map[string]any{"id": "C02", "name": "second", "is_channel": true, "is_member": true},
			}
		case "/conversations.members":
			response["members"] = []string{"UME"}
		case "/conversations.history":
			response["messages"] = []any{}
			oldest, _ := strconv.ParseFloat(r.FormValue("oldest"), 64)
			if r.FormValue("channel") == "C01" && oldest < 1750000000.000001 {
				response["messages"] = []any{root}
			}
		case "/conversations.replies":
			assert.Equal("C01", r.FormValue("channel"))
			assert.Equal("1750000000.000001", r.FormValue("ts"))
			replyRequests.Add(1)
			text := "Original reply <@UME>"
			if edited.Load() {
				text = "Updated reply <@UME>"
			}
			// Missing sender metadata leaves only mention-recipient journal entries.
			response["messages"] = []any{root, map[string]any{
				"type": "message", "ts": "1750000060.000001",
				"thread_ts": "1750000000.000001", "text": text,
			}}
		default:
			assert.Fail("unexpected Slack request", "%s", r.URL.Path)
			http.Error(w, "unexpected method", http.StatusBadRequest)
			return
		}
		assert.NoError(json.MarshalWrite(w, response))
	}))
	defer server.Close()
	imp := slack.NewImporter(st, slack.NewClient(server.URL, "synthetic-token"), "T01")
	opts := slack.ImportOptions{TeamID: "T01", UserID: "UME", NoMedia: true}
	_, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	_, err = buildCacheAuto(dbPath, analyticsDir)
	require.NoError(err)

	duckDB, err := sql.Open("duckdb", "")
	require.NoError(err)
	t.Cleanup(func() { _ = duckDB.Close() })
	messagePattern := filepath.Join(analyticsDir, "messages", "**", "*.parquet")
	var snippet string
	require.NoError(duckDB.QueryRow(`SELECT snippet FROM read_parquet(?, hive_partitioning=true)
		WHERE source_message_id = 'C01:1750000060.000001'`, messagePattern).Scan(&snippet))
	require.Equal("Original reply @Example", snippet)

	edited.Store(true)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	opts.Progress = func(line string) {
		if strings.HasPrefix(line, "conversation ") {
			cancel(jobctx.ErrYieldedToWaiter)
		}
	}
	yielded, err := imp.Import(ctx, opts)
	require.ErrorIs(err, context.Canceled)
	require.Equal(1, yielded.MessagesUpdated)
	latest, err := st.GetLatestSync(yielded.SourceID)
	require.NoError(err)
	require.Equal(store.SyncStatusCancelled, latest.Status)
	requestsBeforeResume := replyRequests.Load()

	opts.Progress = nil
	resumed, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	require.Zero(resumed.MessagesUpdated, "the continuation skips the visited channel")
	require.Equal(requestsBeforeResume, replyRequests.Load())

	stale := cacheNeedsBuild(dbPath, analyticsDir)
	assert.True(stale.HasRelatedRowDrift)
	assert.True(stale.FullRebuild, "a recipient-only refresh cannot repair the reply snippet")
	_, err = buildCacheAuto(dbPath, analyticsDir)
	require.NoError(err)
	require.NoError(duckDB.QueryRow(`SELECT snippet FROM read_parquet(?, hive_partitioning=true)
		WHERE source_message_id = 'C01:1750000060.000001'`, messagePattern).Scan(&snippet))
	assert.Equal("Updated reply @Example", snippet)
	state, err := query.ReadCacheSyncState(analyticsDir)
	require.NoError(err)
	assert.Equal(int64(1), state.LastCacheUpdateCount)
	assert.False(cacheNeedsBuild(dbPath, analyticsDir).NeedsBuild, "the repaired cache must converge")
}
