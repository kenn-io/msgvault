package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
)

// slowStatsStore answers its first stats call at once and blocks later calls
// until release is closed, like a stats query stuck behind a busy archive.
type slowStatsStore struct {
	*mockStore

	calls   atomic.Int32
	release chan struct{}
}

func (s *slowStatsStore) GetStatsContext(ctx context.Context) (*StoreStats, error) {
	call := s.calls.Add(1)
	if call > 1 {
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &StoreStats{MessageCount: int64(call)}, nil
}

func getStatsResponse(t *testing.T, srv *Server) StatsResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	var resp StatsResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	return resp
}

func TestHandleStatsReturnsStaleSnapshotWhenSlow(t *testing.T) {
	assert := assert.New(t)
	st := &slowStatsStore{mockStore: &mockStore{}, release: make(chan struct{})}
	t.Cleanup(func() { close(st.release) })
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Logger: testLogger(),
	})
	srv.statsSnapshotWait = 50 * time.Millisecond

	first := getStatsResponse(t, srv)
	assert.EqualValues(1, first.TotalMessages)
	assert.False(first.Stale)

	started := time.Now()
	second := getStatsResponse(t, srv)
	assert.Less(time.Since(started), 5*time.Second, "a slow stats query does not hold the request")
	assert.True(second.Stale, "the previous snapshot is flagged stale")
	assert.EqualValues(1, second.TotalMessages)
	assert.False(second.AsOf.IsZero())
}

// slowVectorBackend blocks stats until the caller's deadline.
type slowVectorBackend struct {
	*fakeVectorBackend
}

func (b *slowVectorBackend) Stats(ctx context.Context, _ vector.GenerationID) (vector.Stats, error) {
	<-ctx.Done()
	return vector.Stats{}, ctx.Err()
}

func TestHandleStatsVectorTimeoutIsPartial(t *testing.T) {
	backend := &slowVectorBackend{fakeVectorBackend: &fakeVectorBackend{
		active: &vector.Generation{
			ID: 5, Model: "nomic-embed", Dimension: 768,
			Fingerprint: "nomic-embed:768", State: vector.GenerationActive,
		},
	}}
	srv := NewServerWithOptions(ServerOptions{
		Config:    &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:     &mockStore{stats: &StoreStats{MessageCount: 3}},
		Backend:   backend,
		Scheduler: newMockScheduler(),
		Logger:    testLogger(),
	})
	srv.vectorStatsTimeout = 50 * time.Millisecond

	resp := getStatsResponse(t, srv)
	assert.EqualValues(t, 3, resp.TotalMessages, "archive counts are still returned")
	assert.True(t, resp.VectorStatsUnavailable, "a slow vector sub-stat is flagged, not fatal")
}

// slowCountsStore delays grouped account counts after the first call.
type slowCountsStore struct {
	*store.Store

	calls   atomic.Int32
	release chan struct{}
}

func (s *slowCountsStore) CountMessagesBySourceContext(ctx context.Context) (map[int64]store.SourceMessageCounts, error) {
	if s.calls.Add(1) > 1 {
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.Store.CountMessagesBySourceContext(ctx)
}

func TestHandleCLIAccountsServesStaleCountsWhenSlow(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	convID, err := st.EnsureConversation(src.ID, "thread-1", "")
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: convID, SourceMessageID: "msg-1", MessageType: "email",
	})
	require.NoError(err)
	slow := &slowCountsStore{Store: st, release: make(chan struct{})}
	t.Cleanup(func() { close(slow.release) })
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  slow,
		Logger: testLogger(),
	})
	srv.statsSnapshotWait = 50 * time.Millisecond

	getAccounts := func() cliAccountsResponse {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/accounts", nil)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
		var resp cliAccountsResponse
		require.NoError(json.NewDecoder(w.Body).Decode(&resp))
		return resp
	}
	first := getAccounts()
	require.Len(first.Accounts, 1)
	assert.EqualValues(1, first.Accounts[0].MessageCount)
	assert.False(first.Stale)

	second := getAccounts()
	require.Len(second.Accounts, 1)
	assert.True(second.Stale, "slow counts serve the previous snapshot")
	assert.EqualValues(1, second.Accounts[0].MessageCount)
	assert.False(second.AsOf.IsZero())
}
