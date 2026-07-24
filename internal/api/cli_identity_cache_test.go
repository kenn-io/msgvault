package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/identityops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// stubIdentityRebuildStore wraps a real *store.Store the way the daemon's
// store adapter does, adding controllable IdentityCacheRefresher and
// CLICacheBuilder capabilities: account-identity mutations run for real
// against the embedded store, while the identity refresh and the background
// cache build are stubs the test observes. Build calls are reported on a
// buffered channel because scheduleAccountIdentityCacheRebuild runs them on
// a background goroutine.
type stubIdentityRebuildStore struct {
	*store.Store

	refreshCalls int
	buildCh      chan bool // receives each build's fullRebuild argument
}

func (s *stubIdentityRebuildStore) RefreshIdentityDatasets(_ context.Context) (int64, error) {
	s.refreshCalls++
	return 1, nil
}

func (s *stubIdentityRebuildStore) BuildCLICache(
	_ context.Context, fullRebuild bool, _ func(CLICacheBuildEvent) error,
) error {
	s.buildCh <- fullRebuild
	return nil
}

// newIdentityRebuildTestServer builds a server over the stub store with one
// gmail source for alice@example.com, returning that source's ID for seeding
// confirmed identities directly.
func newIdentityRebuildTestServer(t *testing.T) (*Server, *stubIdentityRebuildStore, int64) {
	t.Helper()
	st := testutil.NewTestStore(t)
	wrapped := &stubIdentityRebuildStore{Store: st, buildCh: make(chan bool, 4)}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, wrapped, nil, testLogger())
	alice, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(t, err, "GetOrCreateSource alice")
	require.NotNil(t, alice, "alice source")
	return srv, wrapped, alice.ID
}

func doCLIIdentityRequest(t *testing.T, srv *Server, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1/cli/identities", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	return w
}

// requireCacheBuild waits for the next background cache build and returns
// its fullRebuild argument, failing the test if none arrives.
func requireCacheBuild(t *testing.T, wrapped *stubIdentityRebuildStore) bool {
	t.Helper()
	select {
	case fullRebuild := <-wrapped.buildCh:
		return fullRebuild
	case <-time.After(5 * time.Second):
		t.Fatal("no background cache build was scheduled")
		return false
	}
}

// assertNoPendingCacheBuild asserts no further build was scheduled. Safe
// without waiting: the handlers decide synchronously whether to spawn the
// build goroutine, so a mutation that skipped scheduling can never produce
// a late send.
func assertNoPendingCacheBuild(t *testing.T, wrapped *stubIdentityRebuildStore) {
	t.Helper()
	select {
	case fullRebuild := <-wrapped.buildCh:
		t.Fatalf("unexpected background cache build (fullRebuild=%v)", fullRebuild)
	default:
	}
}

func TestAddCLIIdentity_NewIdentifierSchedulesCacheRebuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, wrapped, _ := newIdentityRebuildTestServer(t)

	w := doCLIIdentityRequest(t, srv, http.MethodPost, `{
		"account": "alice@example.com",
		"identifier": "extra@example.com",
		"signal": "manual"
	}`)

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	var out cliIdentityAddResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&out), "decode add response")
	assert.Equal(identityops.AddOutcomeAdded, out.Outcome, "outcome")
	assert.Equal(identityCacheStateStale, out.CacheState,
		"message shards stay stale until the scheduled full rebuild lands")
	assert.Equal(1, wrapped.refreshCalls, "identity-only refresh still runs for owner_participants")
	assert.False(requireCacheBuild(t, wrapped),
		"the build's own staleness recheck upgrades to a full rebuild via account-identity drift")
}

func TestAddCLIIdentity_AlreadyConfirmedSkipsCacheRebuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, wrapped, aliceID := newIdentityRebuildTestServer(t)
	require.NoError(wrapped.AddAccountIdentity(aliceID, "extra@example.com", "manual"),
		"seed confirmed identity")

	w := doCLIIdentityRequest(t, srv, http.MethodPost, `{
		"account": "alice@example.com",
		"identifier": "extra@example.com",
		"signal": "manual"
	}`)

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	var out cliIdentityAddResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&out), "decode add response")
	assert.Equal(identityops.AddOutcomeAlreadyConfirmed, out.Outcome, "outcome")
	assert.Equal(identityCacheStateReady, out.CacheState,
		"a no-op confirmation leaves message shards valid; only the cheap refresh runs")
	assertNoPendingCacheBuild(t, wrapped)

	// A later real mutation still schedules exactly one build, proving the
	// no-op above scheduled none rather than the observation racing it.
	remove := doCLIIdentityRequest(t, srv, http.MethodDelete, `{
		"account": "alice@example.com",
		"identifier": "extra@example.com"
	}`)
	require.Equal(http.StatusOK, remove.Code, "body: %s", remove.Body.String())
	requireCacheBuild(t, wrapped)
	assertNoPendingCacheBuild(t, wrapped)
}

func TestRemoveCLIIdentity_SchedulesCacheRebuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, wrapped, aliceID := newIdentityRebuildTestServer(t)
	require.NoError(wrapped.AddAccountIdentity(aliceID, "extra@example.com", "manual"),
		"seed confirmed identity")

	w := doCLIIdentityRequest(t, srv, http.MethodDelete, `{
		"account": "alice@example.com",
		"identifier": "extra@example.com"
	}`)

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	var out cliIdentityRemoveResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&out), "decode remove response")
	assert.Equal(int64(1), out.Removed, "removed count")
	assert.Equal(identityCacheStateStale, out.CacheState,
		"message shards stay stale until the scheduled full rebuild lands")
	assert.False(requireCacheBuild(t, wrapped),
		"the build's own staleness recheck upgrades to a full rebuild via account-identity drift")
}
