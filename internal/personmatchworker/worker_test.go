package personmatchworker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personmatch"
	"go.kenn.io/msgvault/internal/personmatchpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRunRequiresConsentBeforeProviderRequest(t *testing.T) {
	fixture := storetest.New(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests++ }))
	defer server.Close()
	cfg := personmatch.Config{Enabled: true, ModelID: personmatch.ModelID,
		MinimumProbability: 0.80, CredentialEnv: "MSGVAULT_JEV_WORKER_FIXTURE",
		BatchSize: 2, RetentionDeclaration: "fixture retention"}
	t.Setenv(cfg.CredentialEnv, "fixture-key")
	worker := Worker{Store: fixture.Store, Config: cfg, Endpoint: server.URL + "/v1/systemone", HTTPClient: server.Client()}
	_, err := worker.Run(t.Context(), 1)
	require.ErrorIs(t, err, ErrConsentRequired)
	assert.Zero(t, requests)
}

func TestRunStopsRetryWhenConsentRevokedDuringProviderBackoff(t *testing.T) {
	for _, changeSnapshot := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed snapshot=%t", changeSnapshot), func(t *testing.T) {
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
				{EvidenceKind: "stable_provider_id", Source: store.ProvenanceArchiveObservation, SourceID: &second.ID},
			} {
				_, err = st.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, evidence)
				require.NoError(err)
			}
			var requests atomic.Int32
			firstRequest := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if requests.Add(1) == 1 {
					if changeSnapshot {
						assert.NoError(st.SetParticipantIdentifier(left, "email", "updated@example.test"))
					}
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
			_, _, err = st.GrantPersonMatchConsentContext(t.Context(), disclosure, "fixture_operator", nil)
			require.NoError(err)
			worker := Worker{Store: st, Config: cfg, Endpoint: server.URL + "/v1/systemone", HTTPClient: server.Client()}
			type runResult struct {
				results []Result
				err     error
			}
			done := make(chan runResult, 1)
			go func() {
				results, err := worker.Run(t.Context(), 1)
				done <- runResult{results: results, err: err}
			}()
			<-firstRequest
			_, err = st.RevokePersonMatchConsentContext(t.Context(), fingerprint, "fixture_operator", nil)
			require.NoError(err)
			result := <-done
			require.ErrorIs(result.err, ErrConsentRequired)
			if changeSnapshot {
				require.Len(result.results, 1)
				assert.Equal(candidate.ID, result.results[0].CandidateID)
				assert.Equal("stale", result.results[0].Status)
				assert.Equal([]string{"stale_review_snapshot"}, result.results[0].Blockers)
			} else {
				assert.Empty(result.results)
			}
			assert.EqualValues(1, requests.Load())
		})
	}
}

func TestRunScoresSnapshotsAndNeverAppliesMatch(t *testing.T) {
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
		{EvidenceKind: "stable_provider_id", Source: store.ProvenanceArchiveObservation, SourceID: &second.ID},
	} {
		_, err := st.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, evidence)
		require.NoError(err)
	}
	requests := 0
	wantLeftName := "Dry Left"
	changeDuringRequest := false
	failDuringRequest := false
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
		assert.Equal(wantLeftName, body.State.Left["display_name"])
		assert.Equal("Dry Right", body.State.Right["display_name"])
		assert.NotContains(body.State.Left, "id")
		if failDuringRequest {
			assert.NoError(st.SetParticipantIdentifier(left, "email", "updated@example.test"))
			failDuringRequest = false
			http.Error(w, "fixture provider failure", http.StatusBadGateway)
			return
		}
		if changeDuringRequest {
			_, err := st.DB().ExecContext(r.Context(), st.Rebind(`UPDATE participants SET display_name = 'Latest Example' WHERE id = ?`), left)
			assert.NoError(err)
		}
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"same_person":{"type":"noul","noul":0.81}}}`))
	}))
	defer server.Close()
	cfg := personmatch.Config{Enabled: true, ModelID: personmatch.ModelID,
		MinimumProbability: 0.80, CredentialEnv: "MSGVAULT_JEV_WORKER_FIXTURE",
		BatchSize: 2, RetentionDeclaration: "fixture retention"}
	t.Setenv(cfg.CredentialEnv, "fixture-key")
	disclosure, err := cfg.Disclosure()
	require.NoError(err)
	_, _, err = st.GrantPersonMatchConsentContext(t.Context(), disclosure, "fixture_operator", nil)
	require.NoError(err)
	worker := Worker{Store: st, Config: cfg, Endpoint: server.URL + "/v1/systemone", HTTPClient: server.Client()}
	results, err := worker.Run(t.Context(), 1)
	require.NoError(err)
	require.Len(results, 1)
	assert.Equal(personmatchpolicy.ProposedAccept, results[0].ProposedAction, "blockers: %v", results[0].Blockers)
	review, err := st.GetIdentityMatchReviewContext(t.Context(), candidate.ID)
	require.NoError(err)
	assert.Equal(review.ReviewToken, results[0].ReviewToken)
	assert.Equal(1, requests)
	current, err := st.GetIdentityMatchCandidateContext(t.Context(), candidate.ID)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateCandidate, current.State)
	again, err := worker.Run(t.Context(), 1)
	require.NoError(err)
	assert.Empty(again)
	assert.Equal(1, requests)

	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE participants SET display_name = 'Updated Example' WHERE id = ?`), left)
	require.NoError(err)
	wantLeftName, changeDuringRequest = "Updated Example", true
	stale, err := worker.Run(t.Context(), 1)
	require.NoError(err)
	require.Len(stale, 1)
	assert.Equal("stale", stale[0].Status)
	assert.Nil(stale[0].Probability)
	assert.Equal(personmatchpolicy.NeedsReview, stale[0].ProposedAction)
	wantLeftName, changeDuringRequest = "Latest Example", false
	rescored, err := worker.Run(t.Context(), 1)
	require.NoError(err)
	require.Len(rescored, 1)
	assert.Equal("scored", rescored[0].Status)
	assert.Equal(3, requests)

	// A failed request against an out-of-date contact is still a stale result.
	// The remaining batch slot can then score the updated snapshot.
	require.NoError(st.SetParticipantIdentifier(left, "email", "before-request@example.test"))
	failDuringRequest = true
	continued, err := worker.Run(t.Context(), 2)
	require.NoError(err)
	require.Len(continued, 2)
	assert.Equal(candidate.ID, continued[0].CandidateID)
	assert.Equal("stale", continued[0].Status)
	assert.Nil(continued[0].Probability)
	assert.Equal(personmatchpolicy.NeedsReview, continued[0].ProposedAction)
	assert.Equal([]string{"stale_review_snapshot"}, continued[0].Blockers)
	assert.Equal("scored", continued[1].Status)
	assert.Equal(5, requests)
}

func TestRunSkipsProviderForIncompleteEvidenceSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	left, err := st.EnsureParticipantByIdentifier("beeper", "bounded-left", "Bounded Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("apple_id", "bounded-right", "Bounded Right")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	for i := range 129 {
		_, err = st.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, store.IdentityMatchEvidenceInput{
			EvidenceKind: "email", Detail: new(fmt.Sprintf("bounded-evidence-%03d", i)),
			Source: store.ProvenanceArchiveObservation,
		})
		require.NoError(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests++ }))
	defer server.Close()
	cfg := personmatch.Config{Enabled: true, ModelID: personmatch.ModelID,
		MinimumProbability: 0.80, CredentialEnv: "MSGVAULT_JEV_BOUNDED_FIXTURE",
		BatchSize: 2, RetentionDeclaration: "fixture retention"}
	t.Setenv(cfg.CredentialEnv, "fixture-key")
	disclosure, err := cfg.Disclosure()
	require.NoError(err)
	_, _, err = st.GrantPersonMatchConsentContext(t.Context(), disclosure, "fixture_operator", nil)
	require.NoError(err)
	worker := Worker{Store: st, Config: cfg, Endpoint: server.URL + "/v1/systemone", HTTPClient: server.Client()}
	results, err := worker.Run(t.Context(), 1)
	require.NoError(err)
	require.Len(results, 1)
	assert.Equal("local_guard_blocked", results[0].Status)
	assert.Equal([]string{"evidence_incomplete"}, results[0].Blockers)
	assert.Zero(requests, "an incomplete evidence snapshot must not reach the provider")
}

func TestRunKeepsUncurableLocalBlockersOutOfProvider(t *testing.T) {
	for _, tc := range []struct {
		name, blocker string
	}{
		{"email only", "independent_identity_evidence_required"},
		{"phone", "shared_contact_point"},
		{"stable ID conflict", "conflicting_stable_id"},
		{"manual merge", "manual_merge_review"},
		{"publication", "active_carddav_publication"},
		{"identifier overflow", "incomplete_guard_data"},
		{"truncated field", "incomplete_guard_data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			fixture := storetest.New(t)
			st := fixture.Store
			name := "Guard Example"
			if tc.name == "truncated field" {
				name = "Guard " + strings.Repeat("x", 123)
			}
			left, err := st.EnsureParticipantByIdentifier("beeper", "guard-left", name)
			require.NoError(err)
			right, err := st.EnsureParticipantByIdentifier("apple_id", "guard-right", name)
			require.NoError(err)
			candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{
				LeftKind: store.IdentityMatchParticipant, LeftID: left, RightKind: store.IdentityMatchParticipant, RightID: right,
				Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation,
			})
			require.NoError(err)
			_, err = st.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, store.IdentityMatchEvidenceInput{
				EvidenceKind: "email", Source: store.ProvenanceArchiveObservation, SourceID: &fixture.Source.ID,
			})
			require.NoError(err)
			if tc.name != "email only" {
				second, err := st.GetOrCreateSource("beeper", "guard-second")
				require.NoError(err)
				_, err = st.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, store.IdentityMatchEvidenceInput{
					EvidenceKind: "display_name", Source: store.ProvenanceArchiveObservation, SourceID: &second.ID,
				})
				require.NoError(err)
			}
			switch tc.name {
			case "phone":
				_, err = st.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, store.IdentityMatchEvidenceInput{
					EvidenceKind: "phone", Source: store.ProvenanceArchiveObservation, SourceID: &fixture.Source.ID,
				})
			case "stable ID conflict":
				_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE participant_identifiers SET identifier_type = 'beeper' WHERE participant_id = ?`), right)
			case "manual merge":
				_, _, err = st.CreatePersonFromParticipantContext(t.Context(), left)
				require.NoError(err)
				_, _, err = st.CreatePersonFromParticipantContext(t.Context(), right)
			case "publication":
				person, _, createErr := st.CreatePersonFromParticipantContext(t.Context(), left)
				require.NoError(createErr)
				_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_publications (person_id, desired) VALUES (?, TRUE)`), person.ID)
			case "identifier overflow":
				for i := range 8 {
					_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO participant_identifiers
						(participant_id, identifier_type, identifier_value, display_value, is_primary)
						VALUES (?, 'email', ?, ?, FALSE)`), left, fmt.Sprintf("guard-%d@example.test", i), "fixture")
					require.NoError(err)
				}
			}
			require.NoError(err)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"same_person":{"type":"noul","noul":0.99}}}`))
			}))
			defer server.Close()
			cfg := personmatch.Config{Enabled: true, CredentialEnv: "MSGVAULT_JEV_GUARD_FIXTURE", RetentionDeclaration: "fixture retention"}
			cfg.ApplyDefaults()
			t.Setenv(cfg.CredentialEnv, "fixture-key")
			disclosure, err := cfg.Disclosure()
			require.NoError(err)
			_, _, err = st.GrantPersonMatchConsentContext(t.Context(), disclosure, "operator", nil)
			require.NoError(err)
			worker := Worker{Store: st, Config: cfg, Endpoint: server.URL, HTTPClient: server.Client()}
			results, err := worker.Run(t.Context(), 1)
			require.NoError(err)
			require.Len(results, 1)
			assert.Equal("local_guard_blocked", results[0].Status)
			assert.Contains(results[0].Blockers, tc.blocker)
			assert.Zero(requests)
			history, err := st.ListIdentityMatchJudgmentsContext(t.Context(), candidate.ID, 1)
			require.NoError(err)
			require.Len(history, 1)
			assert.Contains(history[0].Blockers, tc.blocker)
		})
	}
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

// This wrapper schedules a real revocation at the return boundary of a real claim.
// It does not replace a store result or bypass the worker's consent checks.
type revokeAfterClaimStore struct {
	*store.Store

	revoke func() error
}

func (s revokeAfterClaimStore) ClaimNextIdentityMatchJudgmentContext(ctx context.Context, owner string, leaseDuration time.Duration, version ...string) (*store.IdentityMatchJudgmentLease, error) {
	lease, err := s.Store.ClaimNextIdentityMatchJudgmentContext(ctx, owner, leaseDuration, version...)
	if err != nil || lease == nil {
		return lease, err
	}
	if err = s.revoke(); err != nil {
		return nil, err
	}
	return lease, nil
}

func TestRunConsentWithdrawalsDoNotExhaustProviderAttempts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	left, err := st.EnsureParticipantByIdentifier("beeper", "withdraw-left", "Review Example")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("apple_id", "withdraw-right", "Review Example")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left, RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	second, err := st.GetOrCreateSource("beeper", "withdraw-second")
	require.NoError(err)
	for _, evidence := range []store.IdentityMatchEvidenceInput{
		{EvidenceKind: "email", Detail: new("review@example.test"), Source: store.ProvenanceArchiveObservation, SourceID: &fixture.Source.ID},
		{EvidenceKind: "display_name", Detail: new("Review Example"), Source: store.ProvenanceArchiveObservation, SourceID: &second.ID},
	} {
		_, err = st.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, evidence)
		require.NoError(err)
	}
	cfg := personmatch.Config{Enabled: true, ModelID: personmatch.ModelID, MinimumProbability: .80,
		CredentialEnv: "MSGVAULT_JEV_WITHDRAW_FIXTURE_KEY", BatchSize: 1, RetentionDeclaration: "fixture retention"}
	t.Setenv(cfg.CredentialEnv, "fixture-key")
	disclosure, err := cfg.Disclosure()
	require.NoError(err)
	fingerprint, err := disclosure.Fingerprint()
	require.NoError(err)
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"same_person":{"type":"noul","noul":0.99}}}`))
	}))
	defer provider.Close()
	scheduled := revokeAfterClaimStore{Store: st, revoke: func() error {
		_, err := st.RevokePersonMatchConsentContext(t.Context(), fingerprint, "fixture_operator", nil)
		return err
	}}
	worker := Worker{Store: scheduled, Config: cfg, Endpoint: provider.URL, HTTPClient: provider.Client()}
	for attempt := 1; attempt <= 6; attempt++ {
		_, _, err = st.GrantPersonMatchConsentContext(t.Context(), disclosure, "fixture_operator", nil)
		require.NoError(err)
		results, err := worker.Run(t.Context(), 1)
		require.ErrorIs(err, ErrConsentRequired)
		require.Empty(results)
		history, err := st.ListIdentityMatchJudgmentsContext(t.Context(), candidate.ID, 10)
		require.NoError(err)
		require.Len(history, attempt)
		assert.Equal("consent_revoked", history[0].ErrorClass)
		assert.Equal("retryable_error", history[0].Status)
		assert.Nil(history[0].RetryAfter, "regrant must allow an immediate retry")
		assert.EqualValues(0, requests.Load())
	}
	_, _, err = st.GrantPersonMatchConsentContext(t.Context(), disclosure, "fixture_operator", nil)
	require.NoError(err)
	worker.Store = st
	results, err := worker.Run(t.Context(), 1)
	require.NoError(err)
	require.Len(results, 1)
	assert.Equal("scored", results[0].Status)
	assert.EqualValues(1, requests.Load())
}

func TestRunReportsIncompleteSweepAndRetainsWrappedResult(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	cfg := personmatch.Config{Enabled: true, ModelID: personmatch.ModelID,
		MinimumProbability: 0.80, CredentialEnv: "MSGVAULT_SCORING_SWEEP_FIXTURE",
		BatchSize: 2, RetentionDeclaration: "fixture retention"}
	t.Setenv(cfg.CredentialEnv, "fixture-key")
	disclosure, err := cfg.Disclosure()
	require.NoError(err)
	_, _, err = st.GrantPersonMatchConsentContext(t.Context(), disclosure, "fixture_operator", nil)
	require.NoError(err)
	worker := Worker{Store: st, Config: cfg}
	ids := make([]int64, 132)
	for i := range ids {
		left, err := st.EnsureParticipantByIdentifier("beeper", fmt.Sprintf("sweep-left-%d", i), "Example Left")
		require.NoError(err)
		right, err := st.EnsureParticipantByIdentifier("beeper", fmt.Sprintf("sweep-right-%d", i), "Example Right")
		require.NoError(err)
		candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchParticipant, LeftID: left,
			RightKind: store.IdentityMatchParticipant, RightID: right,
			Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
			Source: store.ProvenanceArchiveObservation,
		})
		require.NoError(err)
		ids[i] = candidate.ID
		results, err := worker.Run(t.Context(), 1)
		require.NoError(err)
		require.Len(results, 1)
		assert.Equal(candidate.ID, results[0].CandidateID)
		assert.Equal("local_guard_blocked", results[0].Status)
	}
	// Display names change the scoring snapshot without permitting disclosure.
	for _, index := range []int{1, 0} {
		candidate, err := st.GetIdentityMatchCandidateContext(t.Context(), ids[index])
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE participants SET display_name = ? WHERE id = ?`), "Changed Example", candidate.LeftID)
		require.NoError(err)
		if index == 1 {
			results, err := worker.Run(t.Context(), 1)
			require.NoError(err)
			require.Len(results, 1)
			assert.Equal(ids[1], results[0].CandidateID)
		}
	}
	results, err := worker.Run(t.Context(), 2)
	require.ErrorIs(err, store.ErrIdentityMatchJudgmentScanIncomplete)
	assert.Empty(results)
	results, err = worker.Run(t.Context(), 2)
	require.ErrorIs(err, store.ErrIdentityMatchJudgmentScanIncomplete)
	require.Len(results, 1, "the completed wrapped result survives the next capped scan")
	assert.Equal(ids[0], results[0].CandidateID)
	// The claim cleared coverage, so the next suffix miss starts another
	// full sweep. That sweep also spans bounded calls.
	results, err = worker.Run(t.Context(), 2)
	require.ErrorIs(err, store.ErrIdentityMatchJudgmentScanIncomplete)
	assert.Empty(results)
	results, err = worker.Run(t.Context(), 2)
	require.NoError(err)
	assert.Empty(results)
}
