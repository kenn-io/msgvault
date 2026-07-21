package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// stubIdentityCacheStore wraps a real *store.Store to add a controllable
// IdentityCacheRefresher: LinkParticipants/UnlinkParticipants run for real
// against the embedded store, while RefreshIdentityDatasets is a stub whose
// success/failure the test controls, mirroring how the daemon's store
// adapter refreshes the Parquet identity datasets after a real mutation.
type stubIdentityCacheStore struct {
	*store.Store

	refreshErr   error
	refreshCalls int
}

func (s *stubIdentityCacheStore) RefreshIdentityDatasets(_ context.Context) (int64, error) {
	s.refreshCalls++
	if s.refreshErr != nil {
		return 0, s.refreshErr
	}
	return 1, nil
}

// mustParticipant is a thin wrapper around EnsureParticipant for these
// table-driven tests.
func (s *stubIdentityCacheStore) mustParticipant(t *testing.T, email, name, domain string) int64 {
	t.Helper()
	id, err := s.EnsureParticipant(email, name, domain)
	require.NoError(t, err)
	return id
}

func newIdentityLinkTestServer(t *testing.T) (*Server, *stubIdentityCacheStore) {
	t.Helper()
	st := testutil.NewTestStore(t)
	wrapped := &stubIdentityCacheStore{Store: st}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, wrapped, nil, testLogger())
	return srv, wrapped
}

// failingLinkMutationStore wraps a stubIdentityCacheStore to force
// Link/UnlinkParticipants to fail with an arbitrary, non-sentinel error —
// simulating a transient/internal store failure (lock contention, dropped
// connection, context cancellation) that must map to a 500, not the 400
// used for invalid participant IDs.
type failingLinkMutationStore struct {
	*stubIdentityCacheStore

	err error
}

func (s *failingLinkMutationStore) LinkParticipants(_, _ int64) (int64, error) {
	return 0, s.err
}

func (s *failingLinkMutationStore) UnlinkParticipants(_, _ int64) (int64, error) {
	return 0, s.err
}

func newFailingIdentityLinkTestServer(t *testing.T, err error) *Server {
	t.Helper()
	st := testutil.NewTestStore(t)
	failing := &failingLinkMutationStore{
		stubIdentityCacheStore: &stubIdentityCacheStore{Store: st},
		err:                    err,
	}
	return NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, failing, nil, testLogger())
}

func postIdentityLink(t *testing.T, srv *Server, path string, body IdentityLinkRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	return w
}

func decodeIdentityLinkResponse(t *testing.T, w *httptest.ResponseRecorder) IdentityLinkResponse {
	t.Helper()
	var resp IdentityLinkResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "body: %s", w.Body.String())
	return resp
}

func TestLinkIdentity_CreatesEdgeAndReportsReadyCache(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, wrapped := newIdentityLinkTestServer(t)
	a := wrapped.mustParticipant(t, "alice@example.com", "Alice", "example.com")
	b := wrapped.mustParticipant(t, "alice@personal.example", "Alice P", "personal.example")

	w := postIdentityLink(t, srv, "/api/v1/identity/links", IdentityLinkRequest{ParticipantA: a, ParticipantB: b})

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := decodeIdentityLinkResponse(t, w)
	assert.Equal(int64(1), resp.IdentityRevision)
	assert.Equal(identityCacheStateReady, resp.CacheState)
	assert.Equal(1, wrapped.refreshCalls)
}

func TestLinkIdentity_RepeatedExactEdgeIsIdempotent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, wrapped := newIdentityLinkTestServer(t)
	a := wrapped.mustParticipant(t, "alice@example.com", "Alice", "example.com")
	b := wrapped.mustParticipant(t, "alice@personal.example", "Alice P", "personal.example")

	first := postIdentityLink(t, srv, "/api/v1/identity/links", IdentityLinkRequest{ParticipantA: a, ParticipantB: b})
	require.Equal(http.StatusOK, first.Code)
	firstResp := decodeIdentityLinkResponse(t, first)

	second := postIdentityLink(t, srv, "/api/v1/identity/links", IdentityLinkRequest{ParticipantA: a, ParticipantB: b})
	require.Equal(http.StatusOK, second.Code)
	secondResp := decodeIdentityLinkResponse(t, second)

	assert.Equal(firstResp.IdentityRevision, secondResp.IdentityRevision)
	assert.Equal(2, wrapped.refreshCalls, "retry re-attempts the cache refresh without re-mutating")
}

func TestLinkIdentity_IndirectEdgeConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, wrapped := newIdentityLinkTestServer(t)
	a := wrapped.mustParticipant(t, "a@example.com", "A", "example.com")
	b := wrapped.mustParticipant(t, "b@example.com", "B", "example.com")
	c := wrapped.mustParticipant(t, "c@example.com", "C", "example.com")

	require.Equal(http.StatusOK,
		postIdentityLink(t, srv, "/api/v1/identity/links", IdentityLinkRequest{ParticipantA: a, ParticipantB: b}).Code)
	require.Equal(http.StatusOK,
		postIdentityLink(t, srv, "/api/v1/identity/links", IdentityLinkRequest{ParticipantA: b, ParticipantB: c}).Code)

	w := postIdentityLink(t, srv, "/api/v1/identity/links", IdentityLinkRequest{ParticipantA: a, ParticipantB: c})

	require.Equal(http.StatusConflict, w.Code, "body: %s", w.Body.String())
	var errResp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&errResp))
	assert.Equal("already_linked", errResp.Error)
}

func TestLinkIdentity_RefresherFailureReportsStaleWithoutFailingRequest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, wrapped := newIdentityLinkTestServer(t)
	wrapped.refreshErr = errors.New("cache build lock held by another process")
	a := wrapped.mustParticipant(t, "alice@example.com", "Alice", "example.com")
	b := wrapped.mustParticipant(t, "alice@personal.example", "Alice P", "personal.example")

	w := postIdentityLink(t, srv, "/api/v1/identity/links", IdentityLinkRequest{ParticipantA: a, ParticipantB: b})

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := decodeIdentityLinkResponse(t, w)
	assert.Equal(int64(1), resp.IdentityRevision, "mutation is durable despite refresh failure")
	assert.Equal(identityCacheStateStale, resp.CacheState)
}

func TestLinkIdentity_InvalidParticipantIDs(t *testing.T) {
	srv, wrapped := newIdentityLinkTestServer(t)
	a := wrapped.mustParticipant(t, "alice@example.com", "Alice", "example.com")

	for name, req := range map[string]IdentityLinkRequest{
		"self link":        {ParticipantA: a, ParticipantB: a},
		"zero participant": {ParticipantA: a, ParticipantB: 0},
		"negative id":      {ParticipantA: a, ParticipantB: -1},
		"unknown id":       {ParticipantA: a, ParticipantB: 999999},
	} {
		t.Run(name, func(t *testing.T) {
			w := postIdentityLink(t, srv, "/api/v1/identity/links", req)
			assert.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
		})
	}
}

// TestLinkIdentity_StoreErrorMapsTo500 covers Finding 1: a store error that
// is not one of the known sentinels (ErrAlreadyLinked, ErrParticipantNotFound,
// ErrInvalidParticipantID) must map to 500, not the 400 used for invalid
// participant IDs. Otherwise a transient failure like "database is locked"
// would be reported to the client as if they had sent a bad ID, leaking raw
// driver text along the way.
func TestLinkIdentity_StoreErrorMapsTo500(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := newFailingIdentityLinkTestServer(t, errors.New("database is locked"))

	w := postIdentityLink(t, srv, "/api/v1/identity/links", IdentityLinkRequest{ParticipantA: 1, ParticipantB: 2})

	assert.Equal(http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	var errResp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&errResp))
	assert.Equal("internal_error", errResp.Error)
	assert.NotContains(errResp.Message, "database is locked", "raw driver text must not leak to the client")
}

// TestUnlinkIdentity_StoreErrorMapsTo500 mirrors
// TestLinkIdentity_StoreErrorMapsTo500 for the unlink path.
func TestUnlinkIdentity_StoreErrorMapsTo500(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := newFailingIdentityLinkTestServer(t, errors.New("database is locked"))

	w := postIdentityLink(t, srv, "/api/v1/identity/unlinks", IdentityLinkRequest{ParticipantA: 1, ParticipantB: 2})

	assert.Equal(http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	var errResp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&errResp))
	assert.Equal("internal_error", errResp.Error)
}

func TestUnlinkIdentity_RemovesEdgeAndBumpsRevision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, wrapped := newIdentityLinkTestServer(t)
	a := wrapped.mustParticipant(t, "alice@example.com", "Alice", "example.com")
	b := wrapped.mustParticipant(t, "alice@personal.example", "Alice P", "personal.example")
	linkResp := postIdentityLink(t, srv, "/api/v1/identity/links", IdentityLinkRequest{ParticipantA: a, ParticipantB: b})
	require.Equal(http.StatusOK, linkResp.Code)
	linked := decodeIdentityLinkResponse(t, linkResp)

	w := postIdentityLink(t, srv, "/api/v1/identity/unlinks", IdentityLinkRequest{ParticipantA: a, ParticipantB: b})

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := decodeIdentityLinkResponse(t, w)
	assert.Greater(resp.IdentityRevision, linked.IdentityRevision)
	assert.Equal(identityCacheStateReady, resp.CacheState)
}

func TestUnlinkIdentity_RepeatedIsUnchanged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, wrapped := newIdentityLinkTestServer(t)
	a := wrapped.mustParticipant(t, "alice@example.com", "Alice", "example.com")
	b := wrapped.mustParticipant(t, "alice@personal.example", "Alice P", "personal.example")
	require.Equal(http.StatusOK,
		postIdentityLink(t, srv, "/api/v1/identity/links", IdentityLinkRequest{ParticipantA: a, ParticipantB: b}).Code)
	refreshCallsBeforeUnlinks := wrapped.refreshCalls

	first := postIdentityLink(t, srv, "/api/v1/identity/unlinks", IdentityLinkRequest{ParticipantA: a, ParticipantB: b})
	require.Equal(http.StatusOK, first.Code)
	firstResp := decodeIdentityLinkResponse(t, first)

	second := postIdentityLink(t, srv, "/api/v1/identity/unlinks", IdentityLinkRequest{ParticipantA: a, ParticipantB: b})

	require.Equal(http.StatusOK, second.Code)
	secondResp := decodeIdentityLinkResponse(t, second)
	assert.Equal(firstResp.IdentityRevision, secondResp.IdentityRevision)
	assert.Equal(2, wrapped.refreshCalls-refreshCallsBeforeUnlinks,
		"retry re-attempts the cache refresh without re-mutating")
}

// TestUnlinkIdentity_UnknownParticipantIsBadRequest covers Finding 2:
// UnlinkParticipants must validate participant existence the same way
// LinkParticipants does. Before the fix, unlinking a nonexistent pair
// silently returned 200 (RowsAffected == 0 short-circuited to a no-op)
// instead of 400, unlike the same IDs sent to /identity/links.
func TestUnlinkIdentity_UnknownParticipantIsBadRequest(t *testing.T) {
	srv, wrapped := newIdentityLinkTestServer(t)
	a := wrapped.mustParticipant(t, "alice@example.com", "Alice", "example.com")

	w := postIdentityLink(t, srv, "/api/v1/identity/unlinks", IdentityLinkRequest{ParticipantA: a, ParticipantB: 999999})

	assert.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&errResp))
	assert.Equal(t, "invalid_participant_id", errResp.Error)
}
