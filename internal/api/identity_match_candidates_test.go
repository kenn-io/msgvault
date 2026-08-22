package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// seedMatchCandidate creates a participant-pair candidate with the given basis
// and returns it along with the two participant IDs.
func seedMatchCandidate(
	t *testing.T, st *stubIdentityCacheStore, basis store.IdentityMatchBasis,
) (*store.IdentityMatchCandidate, int64, int64) {
	t.Helper()
	require := require.New(t)

	alice := st.mustParticipant(t, "alice@example.com", "Alice Example", "example.com")
	bob := st.mustParticipant(t, "bob@example.com", "Bob Example", "example.com")
	value := "beeper:@alice:beeper.local"
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(
		context.Background(), store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchParticipant, LeftID: alice,
			RightKind: store.IdentityMatchParticipant, RightID: bob,
			Basis: basis, NormalizedValue: &value,
			State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation,
		})
	require.NoError(err, "UpsertIdentityMatchCandidateContext")
	return candidate, alice, bob
}

func acceptPath(id int64) string {
	return fmt.Sprintf("/api/v1/identity/match-candidates/%d/accept", id)
}

func rejectPath(id int64) string {
	return fmt.Sprintf("/api/v1/identity/match-candidates/%d/reject", id)
}

func TestListIdentityMatchCandidatesFiltersByState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newIdentityLinkTestServer(t)
	candidate, _, _ := seedMatchCandidate(t, st, store.IdentityMatchServiceScopeUsername)

	all := personRequest(t, srv, http.MethodGet, "/api/v1/identity/match-candidates", nil, "")
	require.Equal(http.StatusOK, all.Code, all.Body.String())
	assert.Equal("no-store", all.Header().Get("Cache-Control"))
	var listed IdentityMatchCandidatesResponse
	require.NoError(json.Unmarshal(all.Body.Bytes(), &listed), all.Body.String())
	require.Len(listed.Candidates, 1)
	assert.Equal(candidate.ID, listed.Candidates[0].ID)
	assert.Equal(store.IdentityMatchServiceScopeUsername, listed.Candidates[0].Basis)
	assert.Equal(100, listed.Limit, "the default page size is echoed")
	assert.Equal(0, listed.Offset)

	filtered := personRequest(t, srv, http.MethodGet,
		"/api/v1/identity/match-candidates?state=conflict", nil, "")
	require.Equal(http.StatusOK, filtered.Code, filtered.Body.String())
	var conflicts IdentityMatchCandidatesResponse
	require.NoError(json.Unmarshal(filtered.Body.Bytes(), &conflicts), filtered.Body.String())
	assert.Empty(conflicts.Candidates, "the state filter must actually filter")
}

func TestAcceptIdentityMatchCandidateLinksAndReportsCacheState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newIdentityLinkTestServer(t)
	candidate, alice, bob := seedMatchCandidate(t, st, store.IdentityMatchServiceScopeUsername)

	response := personRequest(t, srv, http.MethodPost, acceptPath(candidate.ID),
		[]byte(`{"notes":"same person, confirmed in review"}`), "")
	require.Equal(http.StatusOK, response.Code, response.Body.String())

	var accepted IdentityMatchAcceptResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &accepted), response.Body.String())
	assert.Equal(store.IdentityMatchStateAccepted, accepted.Candidate.State)
	assert.Equal("user", *accepted.Candidate.DecidedBy,
		"an HTTP accept is always an explicit user decision")
	assert.Equal("same person, confirmed in review", *accepted.Candidate.Notes)
	assert.Positive(accepted.IdentityRevision)
	assert.Equal(identityCacheStateReady, accepted.CacheState)
	assert.Equal(1, st.refreshCalls, "a new link must trigger the identity cache refresh")

	members, err := st.ClusterMembers(alice)
	require.NoError(err, "ClusterMembers")
	assert.Contains(members, bob, "accepting must apply the link, not only record it")
}

func TestAcceptIdentityMatchCandidateAcrossPersonsReturnsPersonMergeRequired(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newIdentityLinkTestServer(t)
	candidate, alice, bob := seedMatchCandidate(t, st, store.IdentityMatchStableProviderID)
	ctx := context.Background()
	left, _, err := st.CreatePersonFromParticipantContext(ctx, alice)
	require.NoError(err, "promote alice")
	right, _, err := st.CreatePersonFromParticipantContext(ctx, bob)
	require.NoError(err, "promote bob")
	beforeIdentityRevision, err := st.IdentityRevision()
	require.NoError(err)

	response := personRequest(t, srv, http.MethodPost, acceptPath(candidate.ID), nil, "")
	require.Equal(http.StatusConflict, response.Code, response.Body.String())
	assertPersonMergeRequiredResponse(t, response, *left, *right)

	reloaded, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err, "GetIdentityMatchCandidateContext")
	assert.Equal(candidate.State, reloaded.State, "a merge offer must not decide the candidate")
	assert.Equal(candidate.UpdatedAt, reloaded.UpdatedAt)
	assert.False(linkedParticipants(t, st, alice, bob))
	afterIdentityRevision, err := st.IdentityRevision()
	require.NoError(err)
	assert.Equal(beforeIdentityRevision, afterIdentityRevision)
	for _, before := range []*store.Person{left, right} {
		after, getErr := st.GetPersonContext(ctx, before.ID)
		require.NoError(getErr)
		assert.Equal(before.Revision, after.Revision)
	}
}

func TestRejectIdentityMatchCandidateRetainsTheRow(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newIdentityLinkTestServer(t)
	candidate, alice, bob := seedMatchCandidate(t, st, store.IdentityMatchServiceScopeUsername)

	response := personRequest(t, srv, http.MethodPost, rejectPath(candidate.ID),
		[]byte(`{"notes":"different people"}`), "")
	require.Equal(http.StatusOK, response.Code, response.Body.String())

	var rejected IdentityMatchRejectResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &rejected), response.Body.String())
	assert.Equal(store.IdentityMatchStateRejected, rejected.Candidate.State)
	assert.Equal("user", *rejected.Candidate.DecidedBy)
	assert.Zero(rejected.IdentityRevision,
		"state-only rejection does not change the identity revision")
	assert.Equal(identityCacheStateReady, rejected.CacheState)
	assert.Equal(1, st.refreshCalls,
		"state-only rejection must still verify that the cache is current")
	assert.False(linkedParticipants(t, st, alice, bob), "rejecting must not link anyone")

	listed := personRequest(t, srv, http.MethodGet,
		"/api/v1/identity/match-candidates?state=rejected", nil, "")
	require.Equal(http.StatusOK, listed.Code, listed.Body.String())
	var page IdentityMatchCandidatesResponse
	require.NoError(json.Unmarshal(listed.Body.Bytes(), &page), listed.Body.String())
	assert.Len(page.Candidates, 1,
		"a rejected suggestion is retained so the same inference is not repeated")
}

func TestRejectAcceptedSystemIdentityMatchUnlinksAndRetainsRejection(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newIdentityLinkTestServer(t)
	candidate, alice, bob := seedMatchCandidate(t, st, store.IdentityMatchStableProviderID)

	_, _, err := st.AcceptIdentityMatchCandidateContext(
		context.Background(), candidate.ID, "system", nil,
	)
	require.NoError(err, "system acceptance")
	require.True(linkedParticipants(t, st, alice, bob), "precondition: accept linked the pair")

	rejected := personRequest(t, srv, http.MethodPost, rejectPath(candidate.ID),
		[]byte(`{"notes":"not the same person"}`), "")
	require.Equal(http.StatusOK, rejected.Code, rejected.Body.String())
	var body IdentityMatchRejectResponse
	require.NoError(json.Unmarshal(rejected.Body.Bytes(), &body), rejected.Body.String())
	assert.Equal(store.IdentityMatchStateRejected, body.Candidate.State)
	assert.Positive(body.IdentityRevision,
		"removing an owned automated edge must report the bumped revision")
	assert.Equal(identityCacheStateReady, body.CacheState)
	assert.Equal(1, st.refreshCalls,
		"removing an owned automated edge must refresh the identity cache")

	reloaded, err := st.GetIdentityMatchCandidateContext(context.Background(), candidate.ID)
	require.NoError(err, "reload candidate")
	assert.Equal(store.IdentityMatchStateRejected, reloaded.State)
	assert.False(linkedParticipants(t, st, alice, bob),
		"rejecting an automated match must remove its direct identity edge")
}

func TestRejectAcceptedSystemIdentityMatchRetryRepairsStaleCache(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newIdentityLinkTestServer(t)
	candidate, _, _ := seedMatchCandidate(t, st, store.IdentityMatchStableProviderID)

	_, _, err := st.AcceptIdentityMatchCandidateContext(
		context.Background(), candidate.ID, "system", nil,
	)
	require.NoError(err, "system acceptance")
	st.refreshErr = errors.New("cache refresh unavailable")

	first := personRequest(t, srv, http.MethodPost, rejectPath(candidate.ID), nil, "")
	require.Equal(http.StatusOK, first.Code, first.Body.String())
	var firstBody IdentityMatchRejectResponse
	require.NoError(json.Unmarshal(first.Body.Bytes(), &firstBody), first.Body.String())
	assert.Equal(identityCacheStateStale, firstBody.CacheState)
	assert.Equal(1, st.refreshCalls)

	second := personRequest(t, srv, http.MethodPost, rejectPath(candidate.ID), nil, "")
	require.Equal(http.StatusOK, second.Code, second.Body.String())
	var secondBody IdentityMatchRejectResponse
	require.NoError(json.Unmarshal(second.Body.Bytes(), &secondBody), second.Body.String())
	assert.Equal(firstBody.IdentityRevision, secondBody.IdentityRevision,
		"retrying the decision must not mutate identity state again")
	assert.Equal(identityCacheStateStale, secondBody.CacheState,
		"retry must not report ready while the cache refresh still fails")
	assert.Equal(2, st.refreshCalls,
		"retry must re-attempt the stale cache refresh without re-mutating")
}

func TestRejectAcceptedSystemIdentityMatchPreservesManualEdgeWithoutBumpingRevision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newIdentityLinkTestServer(t)
	candidate, alice, bob := seedMatchCandidate(t, st, store.IdentityMatchStableProviderID)

	_, err := st.LinkParticipants(alice, bob)
	require.NoError(err, "manual link")
	_, _, err = st.AcceptIdentityMatchCandidateContext(
		context.Background(), candidate.ID, "system", nil,
	)
	require.NoError(err, "system acceptance of already-linked pair")
	before, err := st.IdentityRevision()
	require.NoError(err, "identity revision before rejection")

	rejected := personRequest(t, srv, http.MethodPost, rejectPath(candidate.ID), nil, "")
	require.Equal(http.StatusOK, rejected.Code, rejected.Body.String())
	var body IdentityMatchRejectResponse
	require.NoError(json.Unmarshal(rejected.Body.Bytes(), &body), rejected.Body.String())
	assert.Equal(before, body.IdentityRevision,
		"preserving a pre-existing manual edge must not bump the revision")
	assert.Equal(identityCacheStateReady, body.CacheState)
	assert.Equal(1, st.refreshCalls,
		"unchanged identity state must still verify that the cache is current")
	assert.True(linkedParticipants(t, st, alice, bob),
		"manual edge must survive rejection")
}

func TestIdentityMatchStateConflictErrorsUseStableAPICodes(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode string
	}{
		{
			name: "accepted snapshot changed", err: store.ErrIdentityMatchNotAccepted,
			wantCode: "identity_match_state_changed",
		},
		{
			name: "applied match changed", err: store.ErrIdentityMatchAlreadyApplied,
			wantCode: "identity_match_already_applied",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			srv, _ := newIdentityLinkTestServer(t)
			response := httptest.NewRecorder()

			srv.writeIdentityMatchError(response, test.err)

			requirements.Equal(http.StatusConflict, response.Code, response.Body.String())
			var body ErrorResponse
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			assertions.Equal(test.wantCode, body.Error)
		})
	}
}

// linkedParticipants reports whether two participants share a cluster.
func linkedParticipants(t *testing.T, st *stubIdentityCacheStore, a, b int64) bool {
	t.Helper()
	require := require.New(t)

	members, err := st.ClusterMembers(a)
	require.NoError(err, "ClusterMembers")
	return slices.Contains(members, b)
}

func TestIdentityMatchCandidateRoutesValidateInput(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		body       []byte
		wantStatus int
		wantCode   string
	}{
		{
			name: "unknown state value", method: http.MethodGet,
			path:       "/api/v1/identity/match-candidates?state=maybe",
			wantStatus: http.StatusBadRequest, wantCode: "state",
		},
		{
			name: "non numeric limit", method: http.MethodGet,
			path:       "/api/v1/identity/match-candidates?limit=lots",
			wantStatus: http.StatusBadRequest, wantCode: "limit",
		},
		{
			name: "non numeric candidate id", method: http.MethodPost,
			path:       "/api/v1/identity/match-candidates/abc/accept",
			wantStatus: http.StatusBadRequest, wantCode: "invalid_candidate_id",
		},
		{
			name: "unknown candidate", method: http.MethodPost,
			path:       "/api/v1/identity/match-candidates/999999/accept",
			wantStatus: http.StatusNotFound, wantCode: "identity_match_not_found",
		},
		{
			name: "unknown request field", method: http.MethodPost,
			path:       "/api/v1/identity/match-candidates/1/accept",
			body:       []byte(`{"note":"typo"}`),
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			srv, _ := newIdentityLinkTestServer(t)

			response := personRequest(t, srv, test.method, test.path, test.body, "")
			require.Equal(test.wantStatus, response.Code, response.Body.String())
			assert.Contains(response.Body.String(), test.wantCode)
		})
	}
}

func TestAcceptEmptyBodyIsAllowed(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newIdentityLinkTestServer(t)
	candidate, _, _ := seedMatchCandidate(t, st, store.IdentityMatchStableProviderID)

	response := personRequest(t, srv, http.MethodPost, acceptPath(candidate.ID), nil, "")
	require.Equal(http.StatusOK, response.Code, response.Body.String())

	var accepted IdentityMatchAcceptResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &accepted), response.Body.String())
	assert.Nil(accepted.Candidate.Notes, "notes are optional")
}
