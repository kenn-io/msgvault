package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	xapi "go.kenn.io/msgvault/internal/x"
)

func seedReplyCacheRecovery(t *testing.T, f *replySurfaceFixture, state string) {
	t.Helper()
	require := require.New(t)
	execution, err := f.st.AcquireSyncExecutionContext(t.Context(), f.source.ID)
	require.NoError(err)
	defer func() { require.NoError(execution.Release()) }()
	control, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	require.NoError(err)
	admitted, err := f.st.AdmitXReplyContext(t.Context(), store.XReplyAdmission{
		SourceID: f.source.ID, OwnerID: "1001", DraftID: f.draft.DraftID, Revision: f.draft.Revision,
		TargetPostID: f.draft.ReplyToSourceMessageID,
		Cost: store.XRequestCost{Kind: store.XRequestCreate, OwnerID: "1001", Generation: control.Generation,
			GrantedScopes: []string{"tweet.read", "users.read", "tweet.write"}, AllowSend: true,
			MonthlyBudgetMicros: 5_000_000, CreateReservationMicros: 200_000, At: time.Now().UTC()},
	})
	require.NoError(err)
	if state == "repair" {
		require.NoError(f.st.RecordXReplyAcceptedContext(t.Context(), admitted.Attempt.XAttemptKey, "9001"))
	} else {
		require.NoError(f.st.MarkXReplyUnknownContext(t.Context(), admitted.Attempt.XAttemptKey))
	}
}

func TestXReplyWritesRefreshRealAnalyticsWithoutScheduledSync(t *testing.T) {
	testutil.SkipIfPostgres(t, "DuckDB analytics use a SQLite snapshot")
	for _, operation := range []string{"send", "repair", "prepare_adoption", "send_adoption"} {
		t.Run(operation, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newReplySurfaceFixture(t)
			needle := "Thanks"
			if operation != "send" {
				seedReplyCacheRecovery(t, f, operation)
			}
			if operation == "prepare_adoption" || operation == "send_adoption" {
				needle = "freshreplycache"
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(http.MethodGet, r.Method)
					assert.Equal("9001", r.URL.Query().Get("ids"))
					_, err := fmt.Fprintf(w, `{"data":[{"id":"9001","author_id":"1001","conversation_id":"100","text":%q,"created_at":"2026-10-31T12:00:00Z","referenced_tweets":[{"type":"replied_to","id":"102"}],"edit_history_tweet_ids":["9001"]}],"includes":{"users":[{"id":"1001","username":"example_owner","protected":false}]}}`, needle)
					assert.NoError(err)
				}))
				t.Cleanup(provider.Close)
				f.adapter.xDeps.apiBaseURL = provider.URL + "/2"
				f.adapter.xDeps.httpClient = provider.Client()
			}
			cacheRoot := t.TempDir()
			cacheDB := filepath.Join(cacheRoot, "cache.db")
			analyticsDir := filepath.Join(cacheRoot, "analytics")
			refreshes := 0
			refresh := func(ctx context.Context, label string) error {
				require.NoError(ctx.Err())
				assert.Equal(f.source.Identifier, label)
				execution, err := f.st.AcquireSyncExecutionContext(ctx, f.source.ID)
				require.NoError(err, "refresh must run after source execution is released")
				require.NoError(execution.Release())
				if err := os.Remove(cacheDB); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("remove previous cache snapshot: %w", err)
				}
				if err := f.st.BackupDatabase(cacheDB); err != nil {
					return fmt.Errorf("snapshot reply archive: %w", err)
				}
				_, err = buildCache(cacheDB, analyticsDir, true)
				if err != nil {
					return fmt.Errorf("build reply analytics: %w", err)
				}
				refreshes++
				return nil
			}
			require.NoError(refresh(t.Context(), f.source.Identifier))
			refreshes = 0
			f.adapter.draftCacheRefresh = refresh
			// No scheduler exists. Only this real reply route can refresh the cache.
			var out *httptest.ResponseRecorder
			if operation == "prepare_adoption" {
				out = f.request(t, "/api/v1/x/replies/prepare", map[string]any{"draft_id": f.draft.DraftID, "revision": 1, "adopt_post": "9001"})
			} else {
				args := f.args()
				if operation == "send_adoption" {
					args = append(args, "--adopt-post=9001")
				}
				out = f.request(t, "/api/v1/cli/run", map[string]any{"args": args})
			}
			require.Equal(http.StatusOK, out.Code, out.Body.String())
			require.NotContains(out.Body.String(), `"type":"error"`)
			assert.Equal(1, refreshes)
			target, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "9001")
			require.NoError(err)
			require.Positive(target.MessageID)
			engine, err := query.NewDuckDBEngine(analyticsDir, "", nil)
			require.NoError(err)
			t.Cleanup(func() { assert.NoError(engine.Close()) })
			results, err := engine.SearchFast(t.Context(), search.Parse(needle), query.MessageFilter{}, 100, 0)
			require.NoError(err)
			require.Len(results, 1, "the returned reply must reach fast search without another sync")
			assert.Equal(target.MessageID, results[0].ID)
			rows, err := engine.TextAggregate(t.Context(), query.TextViewSources, query.TextAggregateOptions{})
			require.NoError(err)
			require.Len(rows, 1)
			assert.Equal(int64(2), rows[0].Count, "Explore must include the parent and archived reply")
			if operation == "send" {
				assert.Equal(int32(1), f.posts.Load())
			} else {
				assert.Zero(f.posts.Load(), "repair and adoption must never submit a new reply")
			}
		})
	}
}

func TestXReplyRefreshFailurePreservesOutputAfterCancellation(t *testing.T) {
	for _, disconnected := range []bool{false, true} {
		t.Run(fmt.Sprintf("disconnected=%t", disconnected), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newReplySurfaceFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var events []api.CLIRunEvent
			refreshes := 0
			f.adapter.draftCacheRefresh = func(refreshCtx context.Context, label string) error {
				assert.Len(events, 1, "the durable result must be emitted before refresh")
				assert.Equal(f.source.Identifier, label)
				require.ErrorIs(ctx.Err(), context.Canceled)
				require.NoError(refreshCtx.Err(), "client cancellation cannot suppress the refresh")
				refreshes++
				return errors.New("synthetic analytics refresh failure")
			}
			err := f.adapter.runCLISendReply(ctx, api.CLIRunRequest{Args: f.args()}, func(event api.CLIRunEvent) error {
				events = append(events, event)
				cancel()
				if disconnected {
					return io.ErrClosedPipe
				}
				return nil
			})
			if disconnected {
				require.ErrorContains(err, "output_failed")
			} else {
				require.NoError(err, "analytics refresh failure cannot turn an accepted send into a failed send")
			}
			assert.Equal(1, refreshes)
			require.Len(events, 1)
			var result xapi.SendResult
			require.NoError(json.Unmarshal([]byte(events[0].Data), &result))
			assert.Equal("sent", result.Status)
			assert.Equal("9001", result.RemoteID)
			receipt, err := f.st.GetXReplyReceiptContext(t.Context(), f.draft.DraftID)
			require.NoError(err)
			assert.Equal("sent", receipt.State)
			assert.Equal("9001", receipt.RemoteID)
			assert.Equal(int32(1), f.posts.Load())
		})
	}
}
