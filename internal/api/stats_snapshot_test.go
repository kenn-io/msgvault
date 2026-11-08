package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
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
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/stats", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	var resp StatsResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	return resp
}

func TestHandleStatsReturnsStaleSnapshotWhenSlow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		st := &slowStatsStore{mockStore: &mockStore{}, release: make(chan struct{})}
		t.Cleanup(func() { close(st.release) })
		srv := NewServerWithOptions(ServerOptions{
			Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
			Store:  st,
			Logger: testLogger(),
		})
		defer func() { require.NoError(t, srv.Shutdown(t.Context())) }()
		srv.statsSnapshotWait = 50 * time.Millisecond

		first := getStatsResponse(t, srv)
		assert.EqualValues(1, first.TotalMessages)
		assert.False(first.Stale)

		started := time.Now()
		second := getStatsResponse(t, srv)
		assert.Less(time.Since(started), time.Second, "the request wait must end before the background computation")
		assert.True(second.Stale, "the previous snapshot is flagged stale")
		assert.EqualValues(1, second.TotalMessages)
		assert.False(second.AsOf.IsZero())
	})
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
	synctest.Test(t, func(t *testing.T) {
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
		defer func() { require.NoError(t, srv.Shutdown(t.Context())) }()
		srv.vectorStatsTimeout = 50 * time.Millisecond

		resp := getStatsResponse(t, srv)
		assert.EqualValues(t, 3, resp.TotalMessages, "archive counts are still returned")
		assert.True(t, resp.VectorStatsUnavailable, "a slow vector sub-stat is flagged, not fatal")
	})
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

// newSlowAccountsServer serves one account with one message behind slowCountsStore.
func newSlowAccountsServer(t *testing.T, st *store.Store) (*Server, *slowCountsStore, int64) {
	t.Helper()
	require := require.New(t)
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	convID, err := st.EnsureConversation(t.Context(), src.ID, "thread-1", "")
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: convID, SourceMessageID: "msg-1", MessageType: "email",
	})
	require.NoError(err)
	slow := &slowCountsStore{Store: st, release: make(chan struct{})}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  slow,
		Logger: testLogger(),
	})
	return srv, slow, src.ID
}

func TestHandleCLIAccountsServesStaleCountsWhenSlow(t *testing.T) {
	st := testutil.NewTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		defer func() { assert.NoError(st.Close()) }()
		srv, slow, sourceID := newSlowAccountsServer(t, st)
		t.Cleanup(func() { close(slow.release) })
		defer func() { require.NoError(srv.Shutdown(t.Context())) }()
		srv.statsSnapshotWait = 50 * time.Millisecond
		getAccounts := func() cliAccountsResponse {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/cli/accounts", nil)
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

		conversationID, err := st.EnsureConversation(t.Context(), sourceID, "thread-1", "")
		require.NoError(err)
		_, err = st.UpsertMessage(&store.Message{SourceID: sourceID, ConversationID: conversationID, SourceMessageID: "msg-2", MessageType: "email"})
		require.NoError(err)
		started := time.Now()
		second := getAccounts()
		assert.Zero(time.Since(started), "the configured count policy returns immediately")
		require.Len(second.Accounts, 1)
		assert.True(second.Stale, "slow counts serve the previous snapshot")
		assert.EqualValues(1, second.Accounts[0].MessageCount)
		assert.False(second.AsOf.IsZero())
		assert.Equal(first.AsOf, second.AsOf)
		slow.release <- struct{}{}
		synctest.Wait()
		updated := getAccounts()
		require.Len(updated.Accounts, 1)
		assert.EqualValues(2, updated.Accounts[0].MessageCount, "the completed refresh includes newly archived mail")
	})
}

func TestHandleCLIAccountsColdCountsPending(t *testing.T) {
	st := testutil.NewTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		defer func() { assert.NoError(st.Close()) }()
		srv, slow, sourceID := newSlowAccountsServer(t, st)
		defer func() { require.NoError(srv.Shutdown(t.Context())) }()
		slow.calls.Store(1)
		srv.statsSnapshotWait = 20 * time.Millisecond
		get := func(optIn bool) *httptest.ResponseRecorder {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/cli/accounts", nil)
			if optIn {
				req.Header.Set(apiprotocol.AllowPendingCountsHeader, "true")
			}
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			return w
		}

		legacy := get(false)
		assert.Equal(http.StatusServiceUnavailable, legacy.Code, "older clients never get placeholder zeros")
		assert.Contains(legacy.Body.String(), "query_timeout")

		pending := get(true)
		require.Equal(http.StatusOK, pending.Code, pending.Body.String())
		var resp cliAccountsResponse
		require.NoError(json.Unmarshal(pending.Body.Bytes(), &resp))
		assert.True(resp.CountsPending)
		require.Len(resp.Accounts, 1)
		assert.Equal(sourceID, resp.Accounts[0].ID)
		assert.Zero(resp.Accounts[0].MessageCount)

		close(slow.release)
		require.Eventually(func() bool {
			resp = cliAccountsResponse{}
			return json.Unmarshal(get(true).Body.Bytes(), &resp) == nil && !resp.CountsPending
		}, 10*time.Second, 10*time.Millisecond)
		assert.EqualValues(1, resp.Accounts[0].MessageCount)
		legacy = get(false)
		require.Equal(http.StatusOK, legacy.Code, "older clients get real counts once they exist")
	})
}

func TestHandleCLIAccountsDelegatedCountsLive(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, slow, sourceID := newSlowAccountsServer(t, testutil.NewTestStore(t))
	t.Cleanup(func() { close(slow.release) })
	srv.cfg.Server.AgentAccess = true
	srv.cfg.Server.APIKey = "owner"
	srv.agentGrants = agentgrant.NewRegistry()
	t.Cleanup(srv.agentGrants.Close)
	source, err := slow.GetSourceByID(sourceID)
	requirements.NoError(err)
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
	requirements.NoError(err)
	slow.calls.Store(1)
	for _, pending := range []bool{false, true} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/cli/accounts", nil)
		req.Header.Set(apiprotocol.AgentTokenHeader, token)
		if pending {
			req.Header.Set(apiprotocol.AllowPendingCountsHeader, "true")
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		requirements.Equal(200, w.Code, w.Body.String())
		var response cliAccountsResponse
		requirements.NoError(json.Unmarshal(w.Body.Bytes(), &response))
		assertions.False(response.CountsPending)
		requirements.Len(response.Accounts, 1)
		assertions.EqualValues(1, response.Accounts[0].MessageCount)
	}
	assertions.EqualValues(1, slow.calls.Load())
}

type failingDirectCountsStore struct {
	*store.Store

	deleted bool
	err     error
}

func (s *failingDirectCountsStore) CountMessagesForSourceContext(ctx context.Context, sourceID int64) (int64, error) {
	if !s.deleted {
		return 0, s.err
	}
	return s.Store.CountMessagesForSourceContext(ctx, sourceID)
}
func (s *failingDirectCountsStore) CountSourceDeletedMessagesContext(_ context.Context, _ ...int64) (int64, error) {
	return 0, s.err
}
func TestHandleCLIAccountsDelegatedCountErrors(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	for _, deleted := range []bool{false, true} {
		for _, failure := range []error{context.Canceled, errors.New("synthetic count failure")} {
			srv, slow, sourceID := newSlowAccountsServer(t, testutil.NewTestStore(t))
			source, err := slow.GetSourceByID(sourceID)
			requirements.NoError(err)
			srv.store = &failingDirectCountsStore{Store: slow.Store, deleted: deleted, err: failure}
			srv.cfg.Server.AgentAccess = true
			srv.cfg.Server.APIKey = "owner"
			srv.agentGrants = agentgrant.NewRegistry()
			t.Cleanup(srv.agentGrants.Close)
			_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
			requirements.NoError(err)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/cli/accounts", nil)
			req.Header.Set(apiprotocol.AgentTokenHeader, token)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			assertions.Equal(http.StatusInternalServerError, w.Code, w.Body.String())
			assertions.NotContains(w.Body.String(), "synthetic count failure")
		}
	}
}

// slowCatalogStore leaves an expired catalog refresh in flight until released.
type slowCatalogStore struct {
	*store.Store
	calls   atomic.Int32
	release chan struct{}
}

func (s *slowCatalogStore) ListVirtualAccountsContext(ctx context.Context) (map[int64][]store.VirtualAccount, error) {
	if s.calls.Add(1) > 1 {
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.Store.ListVirtualAccountsContext(ctx)
}

func TestHandleCLIAccountsOptionalCatalogPreservesStrictRefresh(t *testing.T) {
	st := testutil.NewTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		srv, _, sourceID := newSlowAccountsServer(t, st)
		defer func() { require.NoError(t, srv.Shutdown(t.Context())) }()
		slow := &slowCatalogStore{Store: st, release: make(chan struct{})}
		srv.store = slow
		require.NoError(t, st.AddAccountIdentity(sourceID, "work@example.org", "manual"))
		httpSrv := httptest.NewTestServer(t, srv.Router())
		client, err := daemonclient.New(daemonclient.Config{URL: httpSrv.URL, AllowInsecure: true, HTTPClient: httpSrv.Client(), Context: t.Context()})
		require.NoError(t, err)
		first, err := client.GetCLIVirtualAccounts(t.Context())
		require.NoError(t, err)
		require.NotEmpty(t, first[sourceID])
		time.Sleep(3 * time.Minute)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/cli/accounts", nil)
		req.Header.Set(apiprotocol.AllowPendingCountsHeader, "true")
		w := httptest.NewRecorder()
		started := time.Now()
		srv.Router().ServeHTTP(w, req)
		assert.Zero(t, time.Since(started), "optional discovery reuses expired children immediately")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var optional cliAccountsResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &optional))
		assert.True(t, optional.VirtualAccountsUnavailable)
		require.Len(t, optional.Accounts, 1)
		assert.Equal(t, first[sourceID], optional.Accounts[0].VirtualAccounts)

		returned := make(chan error, 1)
		var strict map[int64][]store.VirtualAccount
		go func() {
			var err error
			strict, err = client.GetCLIVirtualAccounts(t.Context())
			returned <- err
		}()
		synctest.Wait()
		assert.Empty(t, returned, "strict catalog waits for the refresh")
		time.Sleep(time.Second)
		close(slow.release)
		require.NoError(t, <-returned)
		assert.Equal(t, first, strict)
		assert.Equal(t, int32(2), slow.calls.Load(), "optional and strict readers share one refresh")
	})
}
