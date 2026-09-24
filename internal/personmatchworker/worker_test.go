package personmatchworker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personmatch"
	"go.kenn.io/msgvault/internal/personmatchpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDryRunRequiresConsentBeforeProviderRequest(t *testing.T) {
	fixture := storetest.New(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests++ }))
	defer server.Close()
	cfg := personmatch.Config{Enabled: true, ModelID: personmatch.ModelID,
		MinimumProbability: 0.80, CredentialEnv: "MSGVAULT_JEV_WORKER_FIXTURE",
		BatchSize: 2, RetentionDeclaration: "fixture retention"}
	t.Setenv(cfg.CredentialEnv, "fixture-key")
	worker := Worker{Store: fixture.Store, Config: cfg, Endpoint: server.URL + "/v1/systemone", HTTPClient: server.Client()}
	_, err := worker.RunDry(t.Context(), 1)
	require.ErrorIs(t, err, ErrConsentRequired)
	assert.Zero(t, requests)
}

func TestDryRunStopsRetryWhenConsentRevokedDuringProviderBackoff(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	left, err := st.EnsureParticipantByIdentifier("beeper", "retry-left", "Retry Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("apple_id", "retry-right", "Retry Right")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	second, err := st.GetOrCreateSource("beeper", "retry-second")
	require.NoError(err)
	value := "retry@example.test"
	for _, evidence := range []store.IdentityMatchEvidenceInput{
		{EvidenceKind: "email", Detail: &value, Source: store.ProvenanceArchiveObservation, SourceID: &fixture.Source.ID},
		{EvidenceKind: "self_declaration", Source: store.ProvenanceArchiveObservation, SourceID: &second.ID},
	} {
		_, err = st.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, evidence)
		require.NoError(err)
	}
	var requests atomic.Int32
	firstRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			close(firstRequest)
			http.Error(w, "retry", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"same_person":{"type":"noul","noul":0.81}}}`))
	}))
	defer server.Close()
	cfg := personmatch.Config{Enabled: true, ModelID: personmatch.ModelID,
		MinimumProbability: 0.80, CredentialEnv: "MSGVAULT_JEV_RETRY_FIXTURE",
		BatchSize: 2, RetentionDeclaration: "fixture retention"}
	t.Setenv(cfg.CredentialEnv, "fixture-key")
	disclosure, err := cfg.Disclosure()
	require.NoError(err)
	fingerprint, err := disclosure.Fingerprint()
	require.NoError(err)
	_, _, err = st.GrantPersonMatchConsentContext(t.Context(), disclosure, "fixture_operator")
	require.NoError(err)
	worker := Worker{Store: st, Config: cfg, Endpoint: server.URL + "/v1/systemone", HTTPClient: server.Client()}
	type runResult struct {
		results []DryResult
		err     error
	}
	done := make(chan runResult, 1)
	go func() {
		results, err := worker.RunDry(t.Context(), 1)
		done <- runResult{results: results, err: err}
	}()
	<-firstRequest
	_, err = st.RevokePersonMatchConsentContext(t.Context(), fingerprint, "fixture_operator")
	require.NoError(err)
	result := <-done
	require.ErrorIs(result.err, ErrConsentRequired)
	assert.Empty(result.results)
	assert.EqualValues(1, requests.Load())
}

func TestDryRunScoresOnceAndNeverAppliesMatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	left, err := st.EnsureParticipantByIdentifier("beeper", "dry-left", "Dry Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("apple_id", "dry-right", "Dry Right")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	second, err := st.GetOrCreateSource("beeper", "dry-second")
	require.NoError(err)
	value := "dry@example.test"
	for _, evidence := range []store.IdentityMatchEvidenceInput{
		{EvidenceKind: "email", Detail: &value, Source: store.ProvenanceArchiveObservation, SourceID: &fixture.Source.ID},
		{EvidenceKind: "self_declaration", Source: store.ProvenanceArchiveObservation, SourceID: &second.ID},
	} {
		_, err := st.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, evidence)
		require.NoError(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body struct {
			State struct {
				Left  map[string]any `json:"left"`
				Right map[string]any `json:"right"`
			} `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			assert.NoError(err)
			http.Error(w, "bad test request", http.StatusBadRequest)
			return
		}
		assert.Equal("Dry Left", body.State.Left["display_name"])
		assert.Equal("Dry Right", body.State.Right["display_name"])
		assert.NotContains(body.State.Left, "id")
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"same_person":{"type":"noul","noul":0.81}}}`))
	}))
	defer server.Close()
	cfg := personmatch.Config{Enabled: true, ModelID: personmatch.ModelID,
		MinimumProbability: 0.80, CredentialEnv: "MSGVAULT_JEV_WORKER_FIXTURE",
		BatchSize: 2, RetentionDeclaration: "fixture retention"}
	t.Setenv(cfg.CredentialEnv, "fixture-key")
	disclosure, err := cfg.Disclosure()
	require.NoError(err)
	_, _, err = st.GrantPersonMatchConsentContext(t.Context(), disclosure, "fixture_operator")
	require.NoError(err)
	worker := Worker{Store: st, Config: cfg, Endpoint: server.URL + "/v1/systemone", HTTPClient: server.Client()}
	results, err := worker.RunDry(t.Context(), 1)
	require.NoError(err)
	require.Len(results, 1)
	assert.Equal(personmatchpolicy.ProposedAccept, results[0].ProposedAction)
	review, err := st.GetIdentityMatchReviewContext(t.Context(), candidate.ID)
	require.NoError(err)
	assert.Equal(review.ReviewToken, results[0].ReviewToken)
	assert.Equal(1, requests)
	current, err := st.GetIdentityMatchCandidateContext(t.Context(), candidate.ID)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateCandidate, current.State)
	again, err := worker.RunDry(t.Context(), 1)
	require.NoError(err)
	assert.Empty(again)
	assert.Equal(1, requests)
}

func TestLocalPolicyBlocksActivePublicationOnBoundPerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	left, err := st.EnsureParticipantByIdentifier("beeper", "published-left", "Published Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("apple_id", "published-right", "Published Right")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(t.Context(), left)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(
		`INSERT INTO carddav_publications (person_id, desired) VALUES (?, TRUE)`), person.ID)
	require.NoError(err)
	review, err := st.GetIdentityMatchReviewContext(t.Context(), candidate.ID)
	require.NoError(err)

	input := localPolicyInput(*review, store.PersonMatchPairSummaries{}, nil)
	assert.True(input.ActiveCardDAVPublication)
}
