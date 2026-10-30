package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/threads"
)

func TestThreadsExactScheduling(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	for _, key := range []string{"threads:123:20", "threads:123:200", "threads:456:20"} {
		source, err := st.GetOrCreateSource(sourceTypeThreads, key)
		require.NoError(err)
		require.NoError(st.UpdateSourceDisplayName(source.ID, "shared account"))
	}
	for _, selector := range []string{"20", "200", "shared account"} {
		sources, _, err := findScheduledSyncSources(st, selector)
		require.NoError(err)
		assert.Empty(sources)
	}
	for _, key := range []string{"threads:123:20", "threads:123:200", "threads:456:20"} {
		sources, _, err := findScheduledSyncSources(st, key)
		require.NoError(err)
		require.Len(sources, 1)
		assert.Equal(key, sources[0].Identifier)
	}
	assert.True(manualSyncCLICommand([]string{"sync-threads"}))
	assert.False(manualSyncCLICommand([]string{"add-threads"}))
	assert.False(manualSyncCLICommand([]string{"threads-reply-quota"}))
}
func TestThreadsExactSourceAndCredentialAdmission(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	provider := newThreadsConnectFixture(t, "20")
	deps := provider.deps(st, t.TempDir())
	_, err := executeAddThreadsFixture(t, deps, "123", false)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(sourceTypeThreads, "threads:123:20")
	require.NoError(err)
	calls := 0
	deps.newAPI = func(threads.TokenRecord, *threads.RequestBudget) (threads.API, error) {
		calls++
		return nil, errors.New("should not open")
	}
	record, err := deps.tokenManager().Load("123", "20")
	require.NoError(err)
	require.NoError(deps.tokenManager().Invalidate(record))
	_, err = importThreadsSource(t.Context(), st, source, deps, threads.SyncRequest{}, threads.ScheduledRequestLimit, nil)
	require.ErrorIs(err, threads.ErrReauthorizationRequired)
	assert.Zero(calls)
	// Invalid mode combinations are rejected before provider/token work.
	_, err = importThreadsSource(t.Context(), st, source, deps, threads.SyncRequest{Full: true, PostID: "100"}, 200, nil)
	require.Error(err)
	assert.Zero(calls)
	command := newSyncThreadsLocalCmd(deps)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"20"})
	require.Error(command.Execute())
}
func TestThreadsExecutionExclusionBeforeProvider(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	provider := newThreadsConnectFixture(t, "20")
	deps := provider.deps(st, t.TempDir())
	_, err := executeAddThreadsFixture(t, deps, "123", false)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(sourceTypeThreads, "threads:123:20")
	require.NoError(err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	require.NoError(err)
	t.Cleanup(func() {
		require.NoError(execution.Release())
	})
	opened := 0
	deps.newAPI = func(threads.TokenRecord, *threads.RequestBudget) (threads.API, error) {
		opened++
		return nil, errors.New("provider should not open")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = importThreadsSource(ctx, st, source, deps, threads.SyncRequest{}, 200, nil)
	require.ErrorIs(err, store.ErrSyncAlreadyActive)
	assert.Zero(t, opened)
}

func TestThreadsWholeRunCapsIncludeCredentialRefresh(t *testing.T) {
	for _, limit := range []int{threads.ScheduledRequestLimit, threads.ManualRequestLimit} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			provider := newThreadsConnectFixture(t, "20")
			deps := provider.deps(st, t.TempDir())
			_, err := executeAddThreadsFixture(t, deps, "123", false)
			require.NoError(err)
			source, err := st.GetSourceByTypeAndIdentifier(sourceTypeThreads, "threads:123:20")
			require.NoError(err)
			provider.mu.Lock()
			provider.now = provider.now.Add(8 * 24 * time.Hour)
			provider.attempts = 0
			serial := 0
			provider.readHandler = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasPrefix(r.URL.Path, "/v1.0/") {
					return false
				}
				if r.URL.Path == "/v1.0/me/replies" {
					fmt.Fprint(w, `{"data":[]}`)
					return true
				}
				serial++
				until, err := strconv.ParseInt(r.URL.Query().Get("until"), 10, 64)
				require.NoError(err)
				// Repost facades exercise actual page requests and cursor progress without
				// creating thousands of irrelevant archive fixtures merely to reach a cap.
				fmt.Fprintf(w, `{"data":[{"id":"%d","timestamp":%q,"reposted_post":{"id":"99999"}}],"paging":{"cursors":{"after":"page%d"}}}`, 100000+serial, time.Unix(until-1, 0).UTC().Format(time.RFC3339), serial)
				return true
			}
			provider.mu.Unlock()
			deps.newAPI = func(record threads.TokenRecord, budget *threads.RequestBudget) (threads.API, error) {
				assert.Equal(2, budget.Attempts(), "refresh and inspection consume the same run budget")
				return threads.NewClient(threads.ClientOptions{HTTPClient: provider.server.Client(), BaseURL: provider.server.URL + "/v1.0", AccessToken: record.AccessToken(), Budget: budget})
			}
			sum, err := importThreadsSource(t.Context(), st, source, deps, threads.SyncRequest{}, limit, nil)
			require.NoError(err)
			assert.Equal(store.SyncStatusPaused, sum.Status)
			provider.mu.Lock()
			attempts := provider.attempts
			provider.mu.Unlock()
			assert.Equal(limit, attempts)
			assert.Zero(sum.MessagesAdded)
		})
	}
}
func TestThreadsReadInvalidationStopsRepeatedProviderCalls(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	provider := newThreadsConnectFixture(t, "20")
	deps := provider.deps(st, t.TempDir())
	_, err := executeAddThreadsFixture(t, deps, "123", false)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(sourceTypeThreads, "threads:123:20")
	require.NoError(err)
	provider.mu.Lock()
	provider.readHandler = func(w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"code":190}}`)
		return true
	}
	provider.mu.Unlock()
	deps.newAPI = func(record threads.TokenRecord, budget *threads.RequestBudget) (threads.API, error) {
		return threads.NewClient(threads.ClientOptions{HTTPClient: provider.server.Client(), BaseURL: provider.server.URL + "/v1.0", AccessToken: record.AccessToken(), Budget: budget})
	}
	_, err = importThreadsSource(t.Context(), st, source, deps, threads.SyncRequest{}, 200, nil)
	require.Error(err)
	provider.mu.Lock()
	attempts := provider.attempts
	provider.mu.Unlock()
	_, err = importThreadsSource(t.Context(), st, source, deps, threads.SyncRequest{}, 200, nil)
	require.ErrorIs(err, threads.ErrReauthorizationRequired)
	provider.mu.Lock()
	after := provider.attempts
	provider.mu.Unlock()
	assert.Equal(t, attempts, after)
}
