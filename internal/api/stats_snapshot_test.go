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

// newAccountsServer serves two sources from st: alice has two live messages
// and one deleted from its source, bob has one live message.
func newAccountsServer(t *testing.T, st *store.Store) (*Server, *store.Source, *store.Source) {
	t.Helper()
	require := require.New(t)
	alice, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	bob, err := st.GetOrCreateSource("gmail", "bob@example.com")
	require.NoError(err)
	for _, m := range []struct {
		source *store.Source
		id     string
	}{{alice, "a-1"}, {alice, "a-2"}, {alice, "a-gone"}, {bob, "b-1"}} {
		convID, err := st.EnsureConversation(t.Context(), m.source.ID, "thread-"+m.id, "")
		require.NoError(err)
		_, err = st.UpsertMessage(&store.Message{
			SourceID: m.source.ID, ConversationID: convID, SourceMessageID: m.id, MessageType: "email",
		})
		require.NoError(err)
	}
	require.NoError(st.MarkMessageDeletedContext(t.Context(), alice.ID, "a-gone"))
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080, AgentAccess: true, APIKey: "owner"}},
		Store:  st,
		Logger: testLogger(),
	})
	srv.agentGrants = agentgrant.NewRegistry()
	t.Cleanup(srv.agentGrants.Close)
	t.Cleanup(func() { require.NoError(srv.Shutdown(context.WithoutCancel(t.Context()))) })
	return srv, alice, bob
}

func getCLIAccounts(t *testing.T, srv *Server, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/cli/accounts", nil)
	if token != "" {
		req.Header.Set(apiprotocol.AgentTokenHeader, token)
	} else {
		req.Header.Set("X-Api-Key", "owner")
	}
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	return w
}

func TestHandleCLIAccountsCounts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, alice, bob := newAccountsServer(t, testutil.NewTestStore(t))
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead},
		[]agentgrant.SourceRef{{ID: bob.ID, Type: bob.SourceType, Identifier: bob.Identifier}}, time.Time{})
	require.NoError(err)

	type counts struct{ live, sourceDeleted int64 }
	read := func(token string) map[int64]counts {
		w := getCLIAccounts(t, srv, token)
		require.Equal(http.StatusOK, w.Code, w.Body.String())
		var response cliAccountsResponse
		require.NoError(json.Unmarshal(w.Body.Bytes(), &response))
		got := make(map[int64]counts)
		for _, account := range response.Accounts {
			got[account.ID] = counts{account.MessageCount, account.SourceDeletedCount}
		}
		return got
	}
	assert.Equal(map[int64]counts{alice.ID: {2, 1}, bob.ID: {1, 0}}, read(""), "the owner sees every source")
	assert.Equal(map[int64]counts{bob.ID: {1, 0}}, read(token), "a scoped reader sees only its sources")
}

// failingCountsStore fails the stored count read.
type failingCountsStore struct {
	*store.Store

	err error
}

func (s *failingCountsStore) SourceMessageCountsContext(context.Context) (map[int64]store.SourceMessageCounts, error) {
	return nil, s.err
}

func TestHandleCLIAccountsCountErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, failure := range []error{context.Canceled, errors.New("synthetic count failure")} {
		st := testutil.NewTestStore(t)
		srv, _, bob := newAccountsServer(t, st)
		srv.store = &failingCountsStore{Store: st, err: failure}
		_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead},
			[]agentgrant.SourceRef{{ID: bob.ID, Type: bob.SourceType, Identifier: bob.Identifier}}, time.Time{})
		require.NoError(err)
		for _, token := range []string{"", token} {
			w := getCLIAccounts(t, srv, token)
			assert.GreaterOrEqual(w.Code, http.StatusInternalServerError, w.Body.String())
			assert.NotContains(w.Body.String(), "synthetic count failure")
		}
	}
}

type catalogReadStore struct {
	*store.Store

	readCatalog func(context.Context) (map[int64][]store.VirtualAccount, error)
}

func (s *catalogReadStore) ListVirtualAccountsContext(ctx context.Context) (map[int64][]store.VirtualAccount, error) {
	return s.readCatalog(ctx)
}

func TestHandleCLIAccountsOptionalCatalogPreservesStrictRefresh(t *testing.T) {
	st := testutil.NewTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		src, err := st.GetOrCreateSource("gmail", "alice@example.com")
		require.NoError(err)
		conversation, err := st.EnsureConversation(t.Context(), src.ID, "thread", "")
		require.NoError(err)
		_, err = st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conversation, SourceMessageID: "message", MessageType: "email"})
		require.NoError(err)
		require.NoError(st.AddAccountIdentity(src.ID, "work@example.org", "manual"))
		var calls int
		catalog := &catalogReadStore{Store: st, readCatalog: func(ctx context.Context) (map[int64][]store.VirtualAccount, error) {
			calls++
			if calls > 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return st.ListVirtualAccountsContext(ctx)
		}}
		srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: catalog, Logger: testLogger()})
		defer func() { require.NoError(srv.Shutdown(t.Context())) }()
		getAccounts := func(optIn bool) cliAccountsResponse {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/cli/accounts", nil)
			if optIn {
				req.Header.Set(apiprotocol.AllowPendingCountsHeader, "true")
			}
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			require.Equal(http.StatusOK, w.Code, w.Body.String())
			var response cliAccountsResponse
			require.NoError(json.Unmarshal(w.Body.Bytes(), &response))
			return response
		}
		first := getAccounts(true)
		require.Len(first.Accounts, 1)
		require.NotEmpty(first.Accounts[0].VirtualAccounts)
		time.Sleep(3 * time.Minute)
		started := time.Now()
		optional := getAccounts(true)
		assert.Zero(time.Since(started), "the header opts into immediate reuse of expired children")
		assert.True(optional.VirtualAccountsUnavailable)
		require.Len(optional.Accounts, 1)
		assert.Equal(first.Accounts[0].VirtualAccounts, optional.Accounts[0].VirtualAccounts)
		started = time.Now()
		getAccounts(false)
		assert.Equal(2*time.Second, time.Since(started), "callers without the header keep the strict catalog wait")
	})
}
