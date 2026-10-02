package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

func newScorablePersonMatchCandidate(t *testing.T, st *store.Store) *store.IdentityMatchCandidate {
	t.Helper()
	left, err := st.EnsureParticipantByIdentifier("beeper", "route-score-left", "Review Example")
	require.NoError(t, err)
	right, err := st.EnsureParticipantByIdentifier("apple_id", "route-score-right", "Review Example")
	require.NoError(t, err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(t, err)
	for _, input := range []struct{ kind, detail string }{
		{"email", "review@example.test"},
		{"display_name", "Review Example"},
	} {
		source, err := st.GetOrCreateSource("beeper", "review-"+input.kind)
		require.NoError(t, err)
		_, err = st.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, store.IdentityMatchEvidenceInput{
			EvidenceKind: input.kind, Detail: &input.detail,
			Source: store.ProvenanceArchiveObservation, SourceID: &source.ID,
		})
		require.NoError(t, err)
	}
	return candidate
}

type secondClaimFailsStore struct {
	*store.Store

	claims int
}

func (s *secondClaimFailsStore) ClaimNextIdentityMatchJudgmentContext(ctx context.Context, owner string, lease time.Duration, version ...string) (*store.IdentityMatchJudgmentLease, error) {
	s.claims++
	if s.claims == 2 {
		return nil, errors.New("database detail: private-claim-failure")
	}
	return s.Store.ClaimNextIdentityMatchJudgmentContext(ctx, owner, lease, version...)
}

func TestPersonMatchScoringReturnsCompletedResultsAfterClaimFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	candidate := newScorablePersonMatchCandidate(t, st)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"same_person":{"type":"noul","noul":0.93}}}`))
	}))
	defer provider.Close()
	cfg := config.NewDefaultConfig()
	cfg.People.IdentityScoring.Enabled = true
	cfg.People.IdentityScoring.CredentialEnv = "MSGVAULT_JEV_PARTIAL_FIXTURE_KEY"
	cfg.People.IdentityScoring.RetentionDeclaration = "fixture retention declaration"
	t.Setenv(cfg.People.IdentityScoring.CredentialEnv, "fixture-key")
	disclosure, err := cfg.People.IdentityScoring.Disclosure()
	require.NoError(err)
	_, _, err = st.GrantPersonMatchConsentContext(t.Context(), disclosure, "fixture_operator", nil)
	require.NoError(err)
	srv := NewServer(cfg, &secondClaimFailsStore{Store: st}, nil, testLogger())
	srv.personMatchScoringEndpoint = provider.URL
	run := personRequest(t, srv, http.MethodPost, "/api/v1/identity/scoring/run", []byte(`{"limit":2}`), "")
	require.Equal(http.StatusOK, run.Code, run.Body.String())
	var result PersonMatchScoringResponse
	require.NoError(json.Unmarshal(run.Body.Bytes(), &result))
	require.Len(result.Results, 1)
	assert.Equal(1, result.Processed)
	assert.Equal(candidate.ID, result.Results[0].CandidateID)
	assert.Equal("scored", result.Results[0].Status)
	require.NotNil(result.Error)
	assert.Equal("scoring_run_failed", result.Error.Code)
	assert.NotContains(run.Body.String(), "private-claim-failure")
	judgments, err := st.ListIdentityMatchJudgmentsContext(t.Context(), candidate.ID, 10)
	require.NoError(err)
	require.Len(judgments, 1)
	assert.Equal("scored", judgments[0].Status)
}

type observedScoringMutationsStore struct {
	*store.Store

	beforeMutation func(string)
}

func (s *observedScoringMutationsStore) EnsurePersonMatchScoringCandidatesContext(ctx context.Context, limit int) (int, error) {
	s.beforeMutation("discover")
	return s.Store.EnsurePersonMatchScoringCandidatesContext(ctx, limit)
}

func (s *observedScoringMutationsStore) ClaimNextIdentityMatchJudgmentContext(ctx context.Context, owner string, lease time.Duration, version ...string) (*store.IdentityMatchJudgmentLease, error) {
	s.beforeMutation("claim")
	return s.Store.ClaimNextIdentityMatchJudgmentContext(ctx, owner, lease, version...)
}

func (s *observedScoringMutationsStore) RecordIdentityMatchJudgmentContext(ctx context.Context, lease store.IdentityMatchJudgmentLease, input store.IdentityMatchJudgmentInput) (*store.IdentityMatchJudgment, error) {
	s.beforeMutation("record")
	return s.Store.RecordIdentityMatchJudgmentContext(ctx, lease, input)
}

func TestPersonMatchScoringHoldsOperationGateOnlyForMutations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	candidate := newScorablePersonMatchCandidate(t, st)
	gate := NewSerialOperationGate()
	var mutations []string
	observed := &observedScoringMutationsStore{Store: st, beforeMutation: func(operation string) {
		_, _, held := gate.Holder()
		assert.True(held, "operation gate must cover %s", operation)
		mutations = append(mutations, operation)
	}}
	providerEntered, releaseProvider := make(chan struct{}), make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(providerEntered)
		select {
		case <-releaseProvider:
			_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"same_person":{"type":"noul","noul":0.93}}}`))
		case <-r.Context().Done():
		}
	}))
	defer provider.Close()
	release := sync.OnceFunc(func() { close(releaseProvider) })
	defer release()
	cfg := config.NewDefaultConfig()
	cfg.People.IdentityScoring.Enabled = true
	cfg.People.IdentityScoring.CredentialEnv = "MSGVAULT_JEV_GATE_FIXTURE_KEY"
	cfg.People.IdentityScoring.RetentionDeclaration = "fixture retention declaration"
	t.Setenv(cfg.People.IdentityScoring.CredentialEnv, "fixture-key")
	disclosure, err := cfg.People.IdentityScoring.Disclosure()
	require.NoError(err)
	_, _, err = st.GrantPersonMatchConsentContext(t.Context(), disclosure, "fixture_operator", nil)
	require.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: observed, OperationGate: gate, Logger: testLogger()})
	srv.personMatchScoringEndpoint = provider.URL
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/identity/scoring/run", strings.NewReader(`{"limit":1}`))
	request.Header.Set("Content-Type", "application/json")
	run := httptest.NewRecorder()
	completed := make(chan struct{})
	go func() {
		defer close(completed)
		srv.Router().ServeHTTP(run, request)
	}()
	defer func() {
		cancel()
		release()
		<-completed
	}()
	select {
	case <-providerEntered:
	case <-ctx.Done():
		require.NoError(ctx.Err(), "scoring did not reach the provider")
	}
	finishOtherMutation, acquired := gate.BeginWorkContext(ctx)
	if acquired {
		finishOtherMutation()
	}
	assert.True(acquired, "another archive mutation must run while the provider is paused")
	release()
	<-completed
	require.Equal(http.StatusOK, run.Code, run.Body.String())
	var result PersonMatchScoringResponse
	require.NoError(json.Unmarshal(run.Body.Bytes(), &result))
	require.Len(result.Results, 1)
	assert.Equal("scored", result.Results[0].Status)
	assert.Nil(result.Error)
	assert.Equal([]string{"discover", "claim", "record"}, mutations)
	judgments, err := st.ListIdentityMatchJudgmentsContext(t.Context(), candidate.ID, 10)
	require.NoError(err)
	require.Len(judgments, 1)
	assert.Equal("scored", judgments[0].Status)
}

func TestPersonMatchScoringConsentRoutesRequireExactDisclosure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.People.IdentityScoring.Enabled = true
	cfg.People.IdentityScoring.CredentialEnv = "MSGVAULT_JEV_FIXTURE_KEY"
	cfg.People.IdentityScoring.RetentionDeclaration = "fixture retention declaration"
	t.Setenv("MSGVAULT_JEV_FIXTURE_KEY", "fixture-key")
	srv := NewServer(cfg, st, nil, testLogger())
	statusResponse := personRequest(t, srv, http.MethodGet, "/api/v1/identity/scoring/status", nil, "")
	require.Equal(http.StatusOK, statusResponse.Code, statusResponse.Body.String())
	var status PersonMatchScoringStatus
	require.NoError(json.Unmarshal(statusResponse.Body.Bytes(), &status))
	assert.True(status.CredentialAvailable)
	assert.False(status.ConsentActive)
	assert.False(status.Ready)
	assert.Contains(status.DataFields, "raw value")
	assert.Equal("consent_required", status.Blocker)
	assert.NotEmpty(status.DisclosureFingerprint)
	assert.NotContains(statusResponse.Body.String(), "fixture-key")
	blockedBeforeConsent := personRequest(t, srv, http.MethodPost, "/api/v1/identity/scoring/run",
		[]byte(`{"limit":1}`), "")
	assert.Equal(http.StatusConflict, blockedBeforeConsent.Code)
	assert.Equal("consent_required", decodeErrorEnvelope(t, blockedBeforeConsent).Error)

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
	assert.True(status.Ready)
	assert.Empty(status.Blocker)
	run := personRequest(t, srv, http.MethodPost, "/api/v1/identity/scoring/run",
		[]byte(`{"limit":1}`), "")
	assert.Equal(http.StatusOK, run.Code, run.Body.String())
	history := personRequest(t, srv, http.MethodGet, "/api/v1/identity/scoring/history?limit=1", nil, "")
	assert.Equal(http.StatusOK, history.Code, history.Body.String())
	cfg.People.IdentityScoring.Enabled = false
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
		[]byte(`{"limit":1}`), "")
	assert.Equal(http.StatusConflict, blocked.Code)
}

func TestPersonMatchScoringRunExplainsBelowMinimumLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.People.IdentityScoring.Enabled = true
	cfg.People.IdentityScoring.CredentialEnv = "MSGVAULT_JEV_NEGATIVE_LIMIT_KEY"
	cfg.People.IdentityScoring.RetentionDeclaration = "fixture retention declaration"
	t.Setenv(cfg.People.IdentityScoring.CredentialEnv, "fixture-key")
	disclosure, err := cfg.People.IdentityScoring.Disclosure()
	require.NoError(err)
	_, _, err = st.GrantPersonMatchConsentContext(t.Context(), disclosure, "fixture_operator", nil)
	require.NoError(err)
	srv := NewServer(cfg, st, nil, testLogger())
	response := personRequest(t, srv, http.MethodPost, "/api/v1/identity/scoring/run",
		[]byte(`{"limit":-3}`), "")
	require.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	envelope := decodeErrorEnvelope(t, response)
	assert.Equal("invalid_limit", envelope.Error)
	assert.Equal("Limit must be at least 1", envelope.Message)
}

func TestPersonMatchScoringRouteScoresAndJournalsWithoutApplying(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	candidate := newScorablePersonMatchCandidate(t, st)
	providerCalls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls++
		assert.Equal("/v1/systemone", r.URL.Path)
		assert.Equal("Bearer route-fixture-key", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"same_person":{"type":"noul","noul":0.93}}}`))
	}))
	defer provider.Close()
	cfg := config.NewDefaultConfig()
	cfg.People.IdentityScoring.Enabled = true
	cfg.People.IdentityScoring.CredentialEnv = "MSGVAULT_JEV_ROUTE_FIXTURE"
	cfg.People.IdentityScoring.RetentionDeclaration = "fixture retention declaration"
	t.Setenv(cfg.People.IdentityScoring.CredentialEnv, "route-fixture-key")
	srv := NewServer(cfg, st, nil, testLogger())
	srv.personMatchScoringEndpoint = provider.URL + "/v1/systemone"
	disclosure, err := cfg.People.IdentityScoring.Disclosure()
	require.NoError(err)
	_, _, err = st.GrantPersonMatchConsentContext(t.Context(), disclosure, "fixture_operator", nil)
	require.NoError(err)
	run := personRequest(t, srv, http.MethodPost, "/api/v1/identity/scoring/run",
		[]byte(`{"limit":1}`), "")
	require.Equal(http.StatusOK, run.Code, run.Body.String())
	var result PersonMatchScoringResponse
	require.NoError(json.Unmarshal(run.Body.Bytes(), &result))
	require.Len(result.Results, 1)
	assert.Nil(result.Error)
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
	members, err := st.ClusterMembers(candidate.LeftID)
	require.NoError(err)
	assert.Equal([]int64{candidate.LeftID}, members)
}

func TestPersonMatchScoringRunReportsConsentWithdrawal(t *testing.T) {
	asserts := assert.New(t)
	requires := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.People.IdentityScoring.Enabled = true
	cfg.People.IdentityScoring.CredentialEnv = "MSGVAULT_JEV_REVOKED_FIXTURE_KEY"
	cfg.People.IdentityScoring.RetentionDeclaration = "fixture retention declaration"
	t.Setenv(cfg.People.IdentityScoring.CredentialEnv, "fixture-key")
	disclosure, err := cfg.People.IdentityScoring.Disclosure()
	requires.NoError(err)
	_, _, err = st.GrantPersonMatchConsentContext(t.Context(), disclosure, "fixture_operator", nil)
	requires.NoError(err)
	srv := NewServer(cfg, &consentRevokesAfterPreflightStore{Store: st}, nil, testLogger())
	run := personRequest(t, srv, http.MethodPost, "/api/v1/identity/scoring/run",
		[]byte(`{"limit":1}`), "")
	requires.Equal(http.StatusOK, run.Code, run.Body.String())
	var result PersonMatchScoringResponse
	requires.NoError(json.Unmarshal(run.Body.Bytes(), &result))
	requires.NotNil(result.Error)
	asserts.Equal("consent_required", result.Error.Code)
	asserts.Empty(result.Results)
	asserts.Zero(result.Processed)
}
