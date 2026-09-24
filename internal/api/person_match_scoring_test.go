package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type consentRevokesAfterPreflightStore struct {
	*store.Store

	checks int
}

func (s *consentRevokesAfterPreflightStore) HasPersonMatchConsentContext(ctx context.Context, fingerprint string) (bool, error) {
	s.checks++
	if s.checks >= 3 {
		return false, nil
	}
	return true, nil
}

func TestPersonMatchScoringConsentRoutesRequireExactDisclosure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.People.IdentityMerge.Enabled = true
	cfg.People.IdentityMerge.CredentialEnv = "MSGVAULT_JEV_FIXTURE_KEY"
	cfg.People.IdentityMerge.RetentionDeclaration = "fixture retention declaration"
	t.Setenv("MSGVAULT_JEV_FIXTURE_KEY", "fixture-key")
	srv := NewServer(cfg, st, nil, testLogger())
	statusResponse := personRequest(t, srv, http.MethodGet, "/api/v1/identity/scoring/status", nil, "")
	require.Equal(http.StatusOK, statusResponse.Code, statusResponse.Body.String())
	var status PersonMatchScoringStatus
	require.NoError(json.Unmarshal(statusResponse.Body.Bytes(), &status))
	assert.True(status.CredentialAvailable)
	assert.False(status.ConsentActive)
	assert.False(status.LiveAutomaticAcceptanceAvailable)
	assert.Equal("consent_required", status.Blocker)
	assert.NotEmpty(status.DisclosureFingerprint)
	assert.NotContains(statusResponse.Body.String(), "fixture-key")
	live := personRequest(t, srv, http.MethodPost, "/api/v1/identity/scoring/run",
		[]byte(`{"dry_run":false,"limit":1}`), "")
	assert.Equal(http.StatusConflict, live.Code)
	missingDryRun := personRequest(t, srv, http.MethodPost, "/api/v1/identity/scoring/run",
		[]byte(`{"limit":1}`), "")
	require.Equal(http.StatusBadRequest, missingDryRun.Code, missingDryRun.Body.String())
	assert.Equal("invalid_request", decodeErrorEnvelope(t, missingDryRun).Error)

	stale := personRequest(t, srv, http.MethodPost, "/api/v1/identity/scoring/consent",
		[]byte(`{"disclosure_fingerprint":"stale"}`), "")
	assert.Equal(http.StatusConflict, stale.Code)
	body := []byte(fmt.Sprintf(`{"disclosure_fingerprint":%q}`, status.DisclosureFingerprint))
	granted := personRequest(t, srv, http.MethodPost, "/api/v1/identity/scoring/consent", body, "")
	require.Equal(http.StatusOK, granted.Code, granted.Body.String())
	statusResponse = personRequest(t, srv, http.MethodGet, "/api/v1/identity/scoring/status", nil, "")
	status = PersonMatchScoringStatus{}
	require.NoError(json.Unmarshal(statusResponse.Body.Bytes(), &status))
	assert.True(status.ConsentActive)
	assert.True(status.DryRunReady)
	assert.Empty(status.Blocker)
	assert.Equal("automatic_match_evaluation_required", status.LiveBlocker)
	dry := personRequest(t, srv, http.MethodPost, "/api/v1/identity/scoring/run",
		[]byte(`{"dry_run":true,"limit":1}`), "")
	assert.Equal(http.StatusOK, dry.Code, dry.Body.String())
	history := personRequest(t, srv, http.MethodGet, "/api/v1/identity/scoring/history?limit=1", nil, "")
	assert.Equal(http.StatusOK, history.Code, history.Body.String())
	cfg.People.IdentityMerge.Enabled = false
	revoked := personRequest(t, srv, http.MethodPost, "/api/v1/identity/scoring/revoke", body, "")
	require.Equal(http.StatusOK, revoked.Code, revoked.Body.String())
	var revokeDecision PersonMatchConsentDecisionResponse
	require.NoError(json.Unmarshal(revoked.Body.Bytes(), &revokeDecision))
	assert.False(revokeDecision.ConsentActive)
	assert.True(revokeDecision.Changed)
	invalidRevoke := personRequest(t, srv, http.MethodPost, "/api/v1/identity/scoring/revoke",
		[]byte(`{"disclosure_fingerprint":"stale"}`), "")
	assert.Equal(http.StatusBadRequest, invalidRevoke.Code)
	blocked := personRequest(t, srv, http.MethodPost, "/api/v1/identity/scoring/run",
		[]byte(`{"dry_run":true,"limit":1}`), "")
	assert.Equal(http.StatusConflict, blocked.Code)
}

func TestPersonMatchScoringRunExplainsBelowMinimumLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.People.IdentityMerge.Enabled = true
	cfg.People.IdentityMerge.CredentialEnv = "MSGVAULT_JEV_NEGATIVE_LIMIT_KEY"
	cfg.People.IdentityMerge.RetentionDeclaration = "fixture retention declaration"
	t.Setenv(cfg.People.IdentityMerge.CredentialEnv, "fixture-key")
	disclosure, err := cfg.People.IdentityMerge.Disclosure()
	require.NoError(err)
	_, _, err = st.GrantPersonMatchConsentContext(t.Context(), disclosure, "fixture_operator")
	require.NoError(err)
	srv := NewServer(cfg, st, nil, testLogger())
	response := personRequest(t, srv, http.MethodPost, "/api/v1/identity/scoring/run",
		[]byte(`{"dry_run":true,"limit":-3}`), "")
	require.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	envelope := decodeErrorEnvelope(t, response)
	assert.Equal("invalid_limit", envelope.Error)
	assert.Equal("Limit must be at least 1", envelope.Message)
}

func TestPersonMatchScoringRouteScoresAndJournalsWithoutApplying(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	left, err := st.EnsureParticipantByIdentifier("beeper", "route-score-left", "Route Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("apple_id", "route-score-right", "Route Right")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	source, err := st.GetOrCreateSource("beeper", "route-score-source")
	require.NoError(err)
	_, err = st.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, store.IdentityMatchEvidenceInput{
		EvidenceKind: "email", Detail: new("route@example.test"),
		Source: store.ProvenanceArchiveObservation, SourceID: &source.ID,
	})
	require.NoError(err)
	providerCalls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls++
		assert.Equal("/v1/systemone", r.URL.Path)
		assert.Equal("Bearer route-fixture-key", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"same_person":{"type":"noul","noul":0.93}}}`))
	}))
	defer provider.Close()
	cfg := config.NewDefaultConfig()
	cfg.People.IdentityMerge.Enabled = true
	cfg.People.IdentityMerge.CredentialEnv = "MSGVAULT_JEV_ROUTE_FIXTURE"
	cfg.People.IdentityMerge.RetentionDeclaration = "fixture retention declaration"
	t.Setenv(cfg.People.IdentityMerge.CredentialEnv, "route-fixture-key")
	srv := NewServer(cfg, st, nil, testLogger())
	srv.personMatchScoringEndpoint = provider.URL + "/v1/systemone"
	disclosure, err := cfg.People.IdentityMerge.Disclosure()
	require.NoError(err)
	_, _, err = st.GrantPersonMatchConsentContext(t.Context(), disclosure, "fixture_operator")
	require.NoError(err)
	run := personRequest(t, srv, http.MethodPost, "/api/v1/identity/scoring/run",
		[]byte(`{"dry_run":true,"limit":1}`), "")
	require.Equal(http.StatusOK, run.Code, run.Body.String())
	var result PersonMatchDryRunResponse
	require.NoError(json.Unmarshal(run.Body.Bytes(), &result))
	require.Len(result.Results, 1)
	assert.Equal(candidate.ID, result.Results[0].CandidateID)
	require.NotNil(result.Results[0].Probability)
	assert.InDelta(0.93, *result.Results[0].Probability, 1e-9)
	assert.Equal(1, providerCalls)
	history := personRequest(t, srv, http.MethodGet, "/api/v1/identity/scoring/history?limit=1", nil, "")
	require.Equal(http.StatusOK, history.Code, history.Body.String())
	var journal PersonMatchJudgmentHistoryResponse
	require.NoError(json.Unmarshal(history.Body.Bytes(), &journal))
	require.Len(journal.Judgments, 1)
	assert.Equal(candidate.ID, journal.Judgments[0].CandidateID)
	require.NotNil(journal.NextBeforeID)
	older := personRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/identity/scoring/history?limit=1&before_id=%d", *journal.NextBeforeID), nil, "")
	require.Equal(http.StatusOK, older.Code, older.Body.String())
	journal = PersonMatchJudgmentHistoryResponse{}
	require.NoError(json.Unmarshal(older.Body.Bytes(), &journal))
	assert.Empty(journal.Judgments)
	current, err := st.GetIdentityMatchCandidateContext(t.Context(), candidate.ID)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateCandidate, current.State)
	members, err := st.ClusterMembers(left)
	require.NoError(err)
	assert.Equal([]int64{left}, members)
}

func TestPersonMatchScoringRunReturnsConflictWhenConsentIsRevokedDuringRun(t *testing.T) {
	asserts := assert.New(t)
	requires := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.People.IdentityMerge.Enabled = true
	cfg.People.IdentityMerge.CredentialEnv = "MSGVAULT_JEV_REVOKED_FIXTURE_KEY"
	cfg.People.IdentityMerge.RetentionDeclaration = "fixture retention declaration"
	t.Setenv(cfg.People.IdentityMerge.CredentialEnv, "fixture-key")
	disclosure, err := cfg.People.IdentityMerge.Disclosure()
	requires.NoError(err)
	_, _, err = st.GrantPersonMatchConsentContext(t.Context(), disclosure, "fixture_operator")
	requires.NoError(err)
	srv := NewServer(cfg, &consentRevokesAfterPreflightStore{Store: st}, nil, testLogger())
	run := personRequest(t, srv, http.MethodPost, "/api/v1/identity/scoring/run",
		[]byte(`{"dry_run":true,"limit":1}`), "")
	asserts.Equal(http.StatusConflict, run.Code, run.Body.String())
	asserts.Contains(run.Body.String(), "consent_required")
}
