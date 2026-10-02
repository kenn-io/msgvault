package store_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func createScoringCandidate(t *testing.T, st *store.Store, suffix string) int64 {
	t.Helper()
	left, err := st.EnsureParticipantByIdentifier("beeper", "score-left-"+suffix, "Test Left")
	require.NoError(t, err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "score-right-"+suffix, "Test Right")
	require.NoError(t, err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(t, err)
	return candidate.ID
}

func scoredJudgmentInput() store.IdentityMatchJudgmentInput {
	return store.IdentityMatchJudgmentInput{
		Status: "scored", ModelID: "jev-1.13.0", QuestionVersion: "same_person_v1",
		PolicyVersion: "identity_v1", Probability: new(0.81),
		Blockers: []string{"shared_contact_point"}, Outcome: "review",
	}
}

func TestIdentityMatchJudgmentClaimsAdvanceAndRescoreChangedFingerprint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	firstID := createScoringCandidate(t, st, "first")
	secondID := createScoringCandidate(t, st, "second")
	first, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-a", time.Minute)
	require.NoError(err)
	require.NotNil(first)
	assert.Equal(firstID, first.CandidateID)
	assert.NotEmpty(first.Fingerprint)
	assert.NotEmpty(first.Candidate.ReviewToken)
	completed, err := st.RecordIdentityMatchJudgmentContext(t.Context(), *first, scoredJudgmentInput())
	require.NoError(err)
	assert.Equal(first.Fingerprint, completed.Fingerprint)
	assert.Equal("scored", completed.Status)
	_, err = st.RecordIdentityMatchJudgmentContext(t.Context(), *first, scoredJudgmentInput())
	require.ErrorIs(err, store.ErrIdentityMatchJudgmentLeaseStale)
	second, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-a", time.Minute)
	require.NoError(err)
	require.NotNil(second)
	assert.Equal(secondID, second.CandidateID)
	_, err = st.RecordIdentityMatchJudgmentContext(t.Context(), *second, scoredJudgmentInput())
	require.NoError(err)
	global, err := st.ListIdentityMatchJudgmentsContext(t.Context(), 0, 2)
	require.NoError(err)
	require.Len(global, 2)
	assert.Equal(secondID, global[0].CandidateID)
	assert.Equal(firstID, global[1].CandidateID)
	page, err := st.ListIdentityMatchJudgmentsContext(t.Context(), 0, 1)
	require.NoError(err)
	require.Len(page, 1)
	older, err := st.ListIdentityMatchJudgmentsContext(t.Context(), 0, 1, page[0].ID)
	require.NoError(err)
	require.Len(older, 1)
	assert.Equal(firstID, older[0].CandidateID)
	missing, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-a", time.Minute)
	require.NoError(err)
	assert.Nil(missing)

	_, err = st.AddIdentityMatchEvidenceContext(t.Context(), firstID, store.IdentityMatchEvidenceInput{
		EvidenceKind: "email", Detail: new("same@example.test"),
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	changed, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-a", time.Minute)
	require.NoError(err)
	require.NotNil(changed)
	assert.Equal(firstID, changed.CandidateID)
	assert.NotEqual(first.Fingerprint, changed.Fingerprint)
	_, err = st.RecordIdentityMatchJudgmentContext(t.Context(), *changed, scoredJudgmentInput())
	require.NoError(err)
	history, err := st.ListIdentityMatchJudgmentsContext(t.Context(), firstID, 10)
	require.NoError(err)
	assert.Len(history, 2)
	assert.Equal([]string{"shared_contact_point"}, history[0].Blockers)
	assert.Equal("review", history[0].Outcome)
}

func TestIdentityMatchJudgmentRescoresChangedSourceSupport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	id := createScoringCandidate(t, st, "support")
	evidence, err := st.AddIdentityMatchEvidenceContext(t.Context(), id, store.IdentityMatchEvidenceInput{
		EvidenceKind: "email", Detail: new("shared@example.test"),
		Source: store.ProvenanceArchiveObservation, SourceID: &fixture.Source.ID,
	})
	require.NoError(err)
	first, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-a", time.Minute)
	require.NoError(err)
	require.NotNil(first)
	_, err = st.RecordIdentityMatchJudgmentContext(t.Context(), *first, scoredJudgmentInput())
	require.NoError(err)
	secondSource, err := st.GetOrCreateSource("beeper", "second-account")
	require.NoError(err)
	require.NoError(st.AttachIdentityMatchEvidenceSourceContext(t.Context(), evidence.ID, secondSource.ID))
	second, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-b", time.Minute)
	require.NoError(err)
	require.NotNil(second)
	assert.Equal(id, second.CandidateID)
	assert.NotEqual(first.Fingerprint, second.Fingerprint)
	assert.Len(second.Candidate.Evidence[0].SourceSupport, 2)
}

func TestIdentityMatchJudgmentRejectsChangedParticipantSnapshot(t *testing.T) {
	for _, change := range []struct {
		name, query, first, second string
	}{
		{"name", `UPDATE participants SET display_name = ? WHERE id = ?`, "Changed Example", "Second Example"},
		{"email", `UPDATE participants SET email_address = ? WHERE id = ?`, "changed@example.test", "second@example.test"},
		{"phone", `UPDATE participants SET phone_number = ? WHERE id = ?`, "+12025550123", "+12025550124"},
		{"identifier", `UPDATE participant_identifiers SET identifier_value = ? WHERE participant_id = ?`, "changed-id", "second-id"},
		{"identifier scope", `UPDATE participant_identifiers SET scope_kind = 'account', scope_value = ? WHERE participant_id = ?`, "changed-account", "second-account"},
	} {
		t.Run(change.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := storetest.New(t).Store
			createScoringCandidate(t, st, "snapshot")
			lease, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-a", time.Minute)
			require.NoError(err)
			require.NotNil(lease)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(change.query), change.first, lease.Candidate.LeftID)
			require.NoError(err)
			judgment, err := st.RecordIdentityMatchJudgmentContext(t.Context(), *lease, scoredJudgmentInput())
			require.ErrorIs(err, store.ErrIdentityMatchJudgmentFingerprintStale)
			assert.Equal("stale", judgment.Status)
			assert.Nil(judgment.Probability)
			next, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-b", time.Minute)
			require.NoError(err)
			require.NotNil(next)
			assert.NotEqual(lease.Fingerprint, next.Fingerprint)
			_, err = st.RecordIdentityMatchJudgmentContext(t.Context(), *next, scoredJudgmentInput())
			require.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(change.query), change.second, lease.Candidate.RightID)
			require.NoError(err)
			rescored, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-c", time.Minute)
			require.NoError(err)
			require.NotNil(rescored, "a completed judgment must not hide changed provider inputs")
			assert.NotEqual(next.Fingerprint, rescored.Fingerprint)
		})
	}
}

func TestIdentityMatchJudgmentBoundedScanCompletesAndRescoresChangedInput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	var firstID int64
	for i := range 129 {
		id := createScoringCandidate(t, st, fmt.Sprintf("scan-%d", i))
		if i == 0 {
			firstID = id
		}
		lease, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker", time.Minute)
		require.NoError(err)
		require.NotNil(lease)
		_, err = st.RecordIdentityMatchJudgmentContext(t.Context(), *lease, scoredJudgmentInput())
		require.NoError(err)
	}
	// Each sweep spans two bounded calls and must finish even though every
	// candidate still awaits human review. Later sweeps must also terminate.
	for range 2 {
		lease, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker", time.Minute)
		require.ErrorIs(err, store.ErrIdentityMatchJudgmentScanIncomplete)
		assert.Nil(lease)
		lease, err = st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker", time.Minute)
		require.NoError(err)
		assert.Nil(lease)
	}
	_, err := st.AddIdentityMatchEvidenceContext(t.Context(), firstID, store.IdentityMatchEvidenceInput{
		EvidenceKind: "email", Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	lease, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker", time.Minute)
	require.NoError(err)
	require.NotNil(lease)
	assert.Equal(firstID, lease.CandidateID)
}

func TestIdentityMatchJudgmentInvalidatesWhenEvidenceBecomesIncomplete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	id := createScoringCandidate(t, st, "becomes-incomplete")
	for i := range 128 {
		_, err := st.AddIdentityMatchEvidenceContext(t.Context(), id, store.IdentityMatchEvidenceInput{
			EvidenceKind: "email", Detail: new(fmt.Sprintf("evidence-%d", i)), Source: store.ProvenanceArchiveObservation,
		})
		require.NoError(err)
	}
	lease, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker", time.Minute)
	require.NoError(err)
	require.NotNil(lease)
	_, err = st.AddIdentityMatchEvidenceContext(t.Context(), id, store.IdentityMatchEvidenceInput{
		EvidenceKind: "email", Detail: new("overflow"), Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	_, err = st.RecordIdentityMatchJudgmentContext(t.Context(), *lease, scoredJudgmentInput())
	require.ErrorIs(err, store.ErrIdentityMatchJudgmentFingerprintStale)
	next, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker", time.Minute)
	require.NoError(err)
	require.NotNil(next)
	assert.True(next.Candidate.ScoringEvidenceIncomplete)
}

func TestIdentityMatchJudgmentBoundsOversizedEvidenceSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	candidateID := createScoringCandidate(t, st, "large-evidence")
	for i := range 129 {
		_, err := st.AddIdentityMatchEvidenceContext(t.Context(), candidateID,
			store.IdentityMatchEvidenceInput{
				EvidenceKind: "email", Detail: new(fmt.Sprintf("evidence-%03d", i)),
				Source: store.ProvenanceArchiveObservation,
			})
		require.NoError(err)
	}
	lease, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-a", time.Minute)
	require.NoError(err)
	require.NotNil(lease)
	assert.LessOrEqual(len(lease.Candidate.Evidence), 128)
	assert.True(lease.Candidate.ScoringEvidenceIncomplete)
	assert.NotEmpty(lease.Candidate.ReviewToken)
}

func TestIdentityMatchJudgmentBoundsOversizedNormalizedValue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	candidateID := createScoringCandidate(t, st, "large-normalized-value")
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE identity_match_candidates
		SET normalized_value = ? WHERE id = ?`), strings.Repeat("x", 70*1024), candidateID)
	require.NoError(err)
	lease, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-a", time.Minute)
	require.NoError(err)
	require.NotNil(lease)
	assert.True(lease.Candidate.ScoringEvidenceIncomplete)
	assert.Nil(lease.Candidate.NormalizedValue)
	assert.Empty(lease.Candidate.Evidence)
}

func TestIdentityMatchJudgmentRescoresChangedScoringVersion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	id := createScoringCandidate(t, st, "policy-version")
	first, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-a", time.Minute, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(err)
	require.NotNil(first)
	_, err = st.RecordIdentityMatchJudgmentContext(t.Context(), *first, scoredJudgmentInput())
	require.NoError(err)
	second, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-a", time.Minute, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.NoError(err)
	require.NotNil(second)
	assert.Equal(id, second.CandidateID)
	assert.Equal(first.Candidate.ReviewToken, second.Candidate.ReviewToken)
	assert.NotEqual(first.Fingerprint, second.Fingerprint)
}

func TestIdentityMatchJudgmentRejectsUnredactedErrorClass(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	createScoringCandidate(t, st, "redaction")
	lease, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-a", time.Minute)
	require.NoError(err)
	require.NotNil(lease)
	_, err = st.RecordIdentityMatchJudgmentContext(t.Context(), *lease, store.IdentityMatchJudgmentInput{
		Status: "retryable_error", ModelID: "jev-1.13.0", QuestionVersion: "same_person_v1",
		PolicyVersion: "identity_v1", ErrorClass: "timeout for alice@example.test", Outcome: "review",
	})
	require.ErrorIs(err, store.ErrInvalidIdentityMatchJudgment)
	history, err := st.ListIdentityMatchJudgmentsContext(t.Context(), lease.CandidateID, 10)
	require.NoError(err)
	assert.Empty(history)
}

func TestIdentityMatchJudgmentFailureDoesNotStarveLaterCandidate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	firstID := createScoringCandidate(t, st, "failure-first")
	secondID := createScoringCandidate(t, st, "failure-second")
	first, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-a", time.Minute)
	require.NoError(err)
	require.NotNil(first)
	assert.Equal(firstID, first.CandidateID)
	failed, err := st.RecordIdentityMatchJudgmentContext(t.Context(), *first, store.IdentityMatchJudgmentInput{
		Status: "retryable_error", ModelID: "jev-1.13.0", QuestionVersion: "same_person_v1",
		PolicyVersion: "identity_v1", ErrorClass: "provider_timeout", Outcome: "review",
	})
	require.NoError(err)
	assert.NotNil(failed.RetryAfter)
	second, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-a", time.Minute)
	require.NoError(err)
	require.NotNil(second)
	assert.Equal(secondID, second.CandidateID)
	_, err = st.RecordIdentityMatchJudgmentContext(t.Context(), *second, scoredJudgmentInput())
	require.NoError(err)
	none, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-a", time.Minute)
	require.NoError(err)
	assert.Nil(none)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_match_judgment_work SET retry_after_at = ? WHERE candidate_id = ?`), time.Now().Add(-time.Minute), firstID)
	require.NoError(err)
	retry, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-b", time.Minute)
	require.NoError(err)
	require.NotNil(retry)
	assert.Equal(firstID, retry.CandidateID)
}

func TestIdentityMatchJudgmentLeaseFencesStaleWorker(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	createScoringCandidate(t, st, "lease")
	first, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-a", time.Minute)
	require.NoError(err)
	require.NotNil(first)
	other, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-b", time.Minute)
	require.NoError(err)
	assert.Nil(other)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_match_judgment_work SET lease_until = ? WHERE candidate_id = ?`), time.Now().Add(-time.Minute), first.CandidateID)
	require.NoError(err)
	second, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker-b", time.Minute)
	require.NoError(err)
	require.NotNil(second)
	assert.NotEqual(first.Token, second.Token)
	_, err = st.RecordIdentityMatchJudgmentContext(t.Context(), *first, scoredJudgmentInput())
	require.ErrorIs(err, store.ErrIdentityMatchJudgmentLeaseStale)
	_, err = st.RecordIdentityMatchJudgmentContext(t.Context(), *second, scoredJudgmentInput())
	require.NoError(err)
}

func TestIdentityMatchJudgmentConcurrentClaimsHaveDistinctOwners(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	firstID := createScoringCandidate(t, st, "concurrent-first")
	secondID := createScoringCandidate(t, st, "concurrent-second")
	start := make(chan struct{})
	type result struct {
		lease *store.IdentityMatchJudgmentLease
		err   error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, owner := range []string{"worker-a", "worker-b"} {
		wg.Go(func() {
			<-start
			lease, err := st.ClaimNextIdentityMatchJudgmentContext(context.Background(), owner, time.Minute)
			results <- result{lease: lease, err: err}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	claimed := map[int64]bool{}
	for r := range results {
		require.NoError(r.err)
		require.NotNil(r.lease)
		assert.False(claimed[r.lease.CandidateID])
		claimed[r.lease.CandidateID] = true
	}
	assert.Equal(map[int64]bool{firstID: true, secondID: true}, claimed)
}

func TestIdentityMatchJudgmentWithdrawalPreservesPriorProviderFailures(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	id := createScoringCandidate(t, st, "withdraw-budget")
	for _, errorClass := range []string{"provider_error", "provider_error", "provider_error", "provider_error", "consent_revoked", "provider_error"} {
		lease, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker", time.Minute)
		require.NoError(err)
		require.NotNil(lease)
		judgment, err := st.RecordIdentityMatchJudgmentContext(t.Context(), *lease, store.IdentityMatchJudgmentInput{
			Status: "retryable_error", ModelID: "jev-1.13.0", QuestionVersion: "same_person_v1",
			PolicyVersion: "identity_v1", ErrorClass: errorClass, Outcome: "review",
		})
		require.NoError(err)
		if errorClass == "consent_revoked" {
			assert.Equal("retryable_error", judgment.Status)
			assert.Nil(judgment.RetryAfter)
		}
		// Expire provider backoff without waiting minutes; preserve the failure count.
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_match_judgment_work SET retry_after_at = ? WHERE candidate_id = ?`), time.Now().Add(-time.Minute), id)
		require.NoError(err)
	}
	history, err := st.ListIdentityMatchJudgmentsContext(t.Context(), id, 10)
	require.NoError(err)
	require.Len(history, 6)
	assert.Equal("terminal_error", history[0].Status, "withdrawal must not erase earlier provider failures")
	assert.Equal("provider_error", history[0].ErrorClass)
}

// Ending an unclaimable suffix must not hide eligible work below its cursor.
func TestIdentityMatchJudgmentWrapsBeforeReportingDone(t *testing.T) {
	for _, count := range []int{3, 258, 260} {
		for _, eligibility := range []string{"changed_evidence", "expired_backoff", "expired_lease"} {
			t.Run(fmt.Sprintf("%d/%s", count, eligibility), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				st := storetest.New(t).Store
				ids := make([]int64, count)
				for i := range count {
					ids[i] = createScoringCandidate(t, st, fmt.Sprintf("wrap-%d", i))
					lease, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker", time.Hour)
					require.NoError(err)
					require.NotNil(lease)
					if i == 0 && eligibility == "expired_lease" {
						continue
					}
					input := scoredJudgmentInput()
					if i == 0 && eligibility == "expired_backoff" {
						input.Status, input.Probability, input.ErrorClass = "retryable_error", nil, "provider_timeout"
					}
					_, err = st.RecordIdentityMatchJudgmentContext(t.Context(), *lease, input)
					require.NoError(err)
					if i == 0 && eligibility == "expired_backoff" {
						// Keep the fence closed until the explicit expiry below,
						// independently of fixture setup throughput.
						_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_match_judgment_work SET retry_after_at = ? WHERE candidate_id = ?`), time.Now().Add(time.Hour), ids[0])
						require.NoError(err)
					}
				}
				middleID := ids[count/2]
				_, err := st.AddIdentityMatchEvidenceContext(t.Context(), middleID, store.IdentityMatchEvidenceInput{
					EvidenceKind: "email", Source: store.ProvenanceArchiveObservation,
				})
				require.NoError(err)
				var middle *store.IdentityMatchJudgmentLease
				for range 4 {
					middle, err = st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker", time.Hour)
					if err == nil {
						break
					}
					require.ErrorIs(err, store.ErrIdentityMatchJudgmentScanIncomplete)
				}
				require.NoError(err)
				require.NotNil(middle)
				require.Equal(middleID, middle.CandidateID)
				_, err = st.RecordIdentityMatchJudgmentContext(t.Context(), *middle, scoredJudgmentInput())
				require.NoError(err)
				if eligibility == "changed_evidence" {
					_, err = st.AddIdentityMatchEvidenceContext(t.Context(), ids[0], store.IdentityMatchEvidenceInput{
						EvidenceKind: "email", Source: store.ProvenanceArchiveObservation,
					})
				} else {
					column := "retry_after_at"
					if eligibility == "expired_lease" {
						column = "lease_until"
					}
					_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_match_judgment_work SET `+column+` = ? WHERE candidate_id = ?`), time.Now().Add(-time.Minute), ids[0])
				}
				require.NoError(err)
				var lease *store.IdentityMatchJudgmentLease
				for range 4 {
					lease, err = st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker", time.Hour)
					if err == nil {
						require.NotNil(lease, "a suffix miss must not report completion before wrapping")
						break
					}
					require.ErrorIs(err, store.ErrIdentityMatchJudgmentScanIncomplete)
				}
				require.NoError(err)
				require.NotNil(lease)
				assert.Equal(ids[0], lease.CandidateID)
			})
		}
	}
}

func TestIdentityMatchJudgmentFullSweepPersistsAcrossReopen(t *testing.T) {
	for _, count := range []int{128, 129, 256} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := storetest.New(t).Store
			var firstID int64
			for i := range count {
				id := createScoringCandidate(t, st, fmt.Sprintf("boundary-%d", i))
				if i == 0 {
					firstID = id
				}
				lease, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker", time.Minute)
				require.NoError(err)
				require.NotNil(lease)
				_, err = st.RecordIdentityMatchJudgmentContext(t.Context(), *lease, scoredJudgmentInput())
				require.NoError(err)
			}
			// Opening the real store again must retain both fields at a cap.
			for sweep := range 2 {
				lease, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker", time.Minute)
				assert.Nil(lease)
				if count > 128 {
					require.ErrorIs(err, store.ErrIdentityMatchJudgmentScanIncomplete)
					var cursor int64
					var started bool
					require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT candidate_id, started_at_zero FROM person_match_judgment_cursor WHERE singleton = 1`).Scan(&cursor, &started))
					assert.NotZero(cursor)
					assert.True(started)
					reopened, openErr := store.Open(store.DBPathForTest(st))
					require.NoError(openErr)
					t.Cleanup(func() { _ = reopened.Close() })
					st = reopened
					if sweep == 1 {
						_, err = st.AddIdentityMatchEvidenceContext(t.Context(), firstID, store.IdentityMatchEvidenceInput{
							EvidenceKind: "email", Source: store.ProvenanceArchiveObservation,
						})
						require.NoError(err)
					}
					lease, err = st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker", time.Minute)
					assert.Nil(lease)
				}
				require.NoError(err, "a full sweep must terminate, even at an exact budget boundary")
				var cursor int64
				var started bool
				require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT candidate_id, started_at_zero FROM person_match_judgment_cursor WHERE singleton = 1`).Scan(&cursor, &started))
				assert.Zero(cursor)
				assert.False(started)
			}
			if count > 128 {
				lease, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker", time.Minute)
				require.NoError(err)
				require.NotNil(lease)
				assert.Equal(firstID, lease.CandidateID, "changes after a visit appear on the following sweep")
				var started bool
				require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT started_at_zero FROM person_match_judgment_cursor WHERE singleton = 1`).Scan(&started))
				assert.False(started, "every claim invalidates no-claim sweep coverage")
			}
		})
	}
}
