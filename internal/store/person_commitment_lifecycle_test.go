package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestCommitmentEvidenceFollowsSourceLifecycleWithoutOrdinaryConsent(t *testing.T) {
	for _, owner := range []bool{false, true} {
		for _, mutation := range []struct {
			name, query string
			reason      personfacts.EvidenceStatusReason
			extract     bool
		}{
			{"edited", "UPDATE message_bodies SET body_text = 'Changed source' WHERE message_id = ?", personfacts.EvidenceStatusSourceEdited, false},
			{"edited_extraction", "UPDATE message_bodies SET body_text = 'Changed source' WHERE message_id = ?", personfacts.EvidenceStatusSourceEdited, true},
			{"deleted", "UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?", personfacts.EvidenceStatusSourceDeleted, false},
			{"purged", "DELETE FROM messages WHERE id = ?", personfacts.EvidenceStatusSourceDeleted, false},
		} {
			if owner && mutation.reason != personfacts.EvidenceStatusSourceEdited {
				continue
			}
			t.Run(fmt.Sprintf("%s/owner=%t", mutation.name, owner), func(t *testing.T) {
				r, a, ctx := require.New(t), assert.New(t), t.Context()
				f := newPersonSweepApplyFixture(t, "commitment-evidence", false)
				st := f.store
				sourceID, persist := newCommitmentTestConversation(t, f)
				profile := f.request.Generation.Policy.ProviderPolicyFingerprint
				_, err := st.RevokePersonInferenceConsent(ctx, profile, "test")
				r.NoError(err)
				_, _, err = st.GrantPersonCommitmentConsent(ctx, profile, "test")
				r.NoError(err)
				const quote = "I will send the draft."
				id := persist("promise", quote, -time.Hour)
				if owner {
					_, err := st.db.ExecContext(ctx, `UPDATE messages SET sender_id = (
						SELECT id FROM participants WHERE email_address = 'owner@example.test') WHERE id = ?`, id)
					r.NoError(err)
				}
				cfg := peoplesweep.Config{}
				cfg.ApplyDefaults()
				cfg.Commitments.Enabled = true
				applyCommitmentTestSources(t, f, cfg.Commitments)
				tombstones, err := st.personSweepTombstones(ctx, f.personID, peoplesweep.ArchiveChange{PersonID: f.personID, SourceID: sourceID, MessageID: id, SourceLane: peoplesweep.SourceConversationText, EvidenceEffect: peoplesweep.EvidenceEffectSourceEdited})
				r.NoError(err)
				a.Empty(tombstones)
				before, err := st.LatestPersonSweepChangeSequence(ctx)
				r.NoError(err)
				_, err = st.db.ExecContext(ctx, st.Rebind(mutation.query), id)
				r.NoError(err)
				journal, err := st.ScanPersonSweepChanges(ctx, f.personID, before, 100)
				r.NoError(err)
				r.NotEmpty(journal)
				changes, err := st.BuildPersonSweepEvidenceStatusChanges(ctx, f.personID, journal)
				r.NoError(err)
				r.Len(changes, 1)
				a.False(changes[0].Supported)
				a.Equal(mutation.reason, changes[0].Reason)
				f = nextCommitmentTestAttempt(t, f, "-status")
				request := f.request
				request.CommitmentOnly = true
				request.Generation.ResolvedAt = request.Generation.ResolvedAt.Add(time.Minute)
				request.CompletedAt = request.Generation.ResolvedAt.Add(time.Minute)
				request.Generation.Policy.AllowSensitive = true
				request.Generation.EvidenceStatusChanges = changes
				_, err = st.ApplyPersonSweepStatusOnly(ctx, peoplesweep.StatusOnlyApplyRequest{AttemptID: request.AttemptID, Lease: request.Lease, Generation: request.Generation, CompletedAt: request.CompletedAt})
				r.NoError(err)
				r.NoError(st.DeferPersonSweepAttempt(ctx, request.Lease, request.AttemptID, request.CompletedAt))
				claims, err := st.ListPersonFactClaimsContext(ctx, f.personID, personfacts.ClaimFilter{Limit: 10})
				r.NoError(err)
				r.Len(claims, 1)
				a.Nil(claims[0].Freshness.LatestSupportedAt)
				evidence, err := st.ListPersonFactEvidenceContext(ctx, f.personID, personfacts.EvidenceFilter{Limit: 10})
				r.NoError(err)
				r.Len(evidence, 1)
				a.False(evidence[0].Supported)
				loops, err := st.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 10, true)
				r.NoError(err)
				a.Empty(loops)
				if mutation.reason != personfacts.EvidenceStatusSourceEdited {
					return
				}
				reimport := journal[len(journal)-1]
				reimport.EvidenceEffect = peoplesweep.EvidenceEffectSourceReimported
				changes, err = st.BuildPersonSweepEvidenceStatusChanges(ctx, f.personID, []peoplesweep.ArchiveChange{reimport})
				r.NoError(err)
				a.Empty(changes, "a different source body cannot reactivate the old citation")
				_, err = st.db.ExecContext(ctx, st.Rebind("UPDATE message_bodies SET body_text = ? WHERE message_id = ?"), quote, id)
				r.NoError(err)
				changes, err = st.BuildPersonSweepEvidenceStatusChanges(ctx, f.personID, []peoplesweep.ArchiveChange{reimport})
				r.NoError(err)
				r.Len(changes, 1)
				a.True(changes[0].Supported)
				f = nextCommitmentTestAttempt(t, f, "-reactivate")
				request = f.request
				request.Generation.ResolvedAt = request.Generation.ResolvedAt.Add(2 * time.Minute)
				request.CompletedAt = request.Generation.ResolvedAt.Add(time.Minute)
				request.CommitmentOnly = true
				request.Generation.Policy.AllowSensitive = true
				request.Generation.EvidenceStatusChanges = changes
				if mutation.extract {
					f.request.Generation.ResolvedAt, f.request.CompletedAt = request.Generation.ResolvedAt, request.CompletedAt
					window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, peoplesweep.ProviderProfile{
						AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
					}, cfg.Commitments, request.Generation.ResolvedAt, "UTC")
					r.NoError(err)
					r.Len(window.Sources, 1)
					request = commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
						Promises: []peoplesweep.CommitmentPromise{{SourceID: window.Sources[0].ID, Direction: window.Sources[0].Direction,
							Quote: quote, Summary: "Send draft", End: int64(len(quote))}},
					})
				}
				if mutation.extract {
					_, err = st.ApplyPersonSweep(ctx, request)
				} else {
					_, err = st.ApplyPersonSweepStatusOnly(ctx, peoplesweep.StatusOnlyApplyRequest{AttemptID: request.AttemptID, Lease: request.Lease, Generation: request.Generation, CompletedAt: request.CompletedAt})
				}
				r.NoError(err)
				claims, err = st.ListPersonFactClaimsContext(ctx, f.personID, personfacts.ClaimFilter{Limit: 10})
				r.NoError(err)
				wantClaims := 1
				if mutation.extract {
					// A refreshed summary adds immutable support for the same promise.
					wantClaims = 2
				}
				r.Len(claims, wantClaims)
				for _, claim := range claims {
					a.NotNil(claim.Freshness.LatestSupportedAt)
				}
				evidence, err = st.ListPersonFactEvidenceContext(ctx, f.personID, personfacts.EvidenceFilter{Limit: 10})
				r.NoError(err)
				r.Len(evidence, 1)
				a.True(evidence[0].Supported)
				loops, err = st.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 10, true)
				r.NoError(err)
				a.Len(loops, 1)
			})
		}
	}
}

func TestCommitmentSourceDeletedBeforeExtraction(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "commitment-evidence", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	profile := f.request.Generation.Policy.ProviderPolicyFingerprint
	_, err := st.RevokePersonInferenceConsent(ctx, profile, "test")
	r.NoError(err)
	_, _, err = st.GrantPersonCommitmentConsent(ctx, profile, "test")
	r.NoError(err)
	const quote = "I will send the draft."
	id := persist("promise", quote, -time.Hour)
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	commitmentProfile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
	window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, commitmentProfile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(window.Sources, 1)
	_, err = st.db.ExecContext(ctx, st.Rebind("UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?"), id)
	r.NoError(err)
	deleted, err := st.LoadPersonCommitmentWindow(ctx, f.personID, commitmentProfile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	a.Empty(deleted.Sources)
	request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
		Promises: []peoplesweep.CommitmentPromise{{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: quote, Summary: "Send draft", End: int64(len(quote))}},
	})
	result, err := st.ApplyPersonSweep(ctx, request)
	r.NoError(err)
	a.Equal(peoplesweep.FailureArchiveGap, result.CommitmentFailureClass)
	claims, err := st.ListPersonFactClaimsContext(ctx, f.personID, personfacts.ClaimFilter{Limit: 10})
	r.NoError(err)
	a.Empty(claims)
}

func TestCommitmentFrontierMaintenanceInvalidatesDeletedEvidence(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "commitment-evidence", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	profile := f.request.Generation.Policy.ProviderPolicyFingerprint
	_, err := st.RevokePersonInferenceConsent(ctx, profile, "test")
	r.NoError(err)
	_, _, err = st.GrantPersonCommitmentConsent(ctx, profile, "test")
	r.NoError(err)
	const quote = "I will send the draft."
	id := persist("promise", quote, -time.Hour)
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	commitmentProfile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
	ids := []int64{id}
	for n := range 2 {
		ids = append(ids, persist(fmt.Sprintf("promise-%d", n), fmt.Sprintf("I will send report %d.", n), -time.Hour))
	}
	applyCommitmentTestSources(t, f, cfg.Commitments)
	for _, messageID := range ids {
		_, err := st.db.ExecContext(ctx, st.Rebind("UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?"), messageID)
		r.NoError(err)
	}
	now := f.request.CompletedAt.Add(cfg.Commitments.MinInterval)
	due, err := st.PersonCommitmentsDue(ctx, f.personID, now, cfg.Commitments.MinInterval, cfg.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(commitmentProfile, cfg.Commitments))
	r.NoError(err)
	r.True(due)
	f = nextCommitmentTestAttempt(t, f, "-observed")
	f.request.Generation.ResolvedAt, f.request.CompletedAt = now, now.Add(time.Minute)
	window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, commitmentProfile, cfg.Commitments, now, "UTC")
	r.NoError(err)
	r.Empty(window.Sources)
	request := f.request
	request.CommitmentOnly = true
	request.Budget = personSweepApplyBudget()
	request.Generation.Policy.AllowSensitive = true
	request.Commitment = &peoplesweep.CommitmentResult{Window: window, Config: cfg.Commitments}
	_, err = st.ApplyPersonSweep(ctx, request)
	r.NoError(err)
	evidence, err := st.ListPersonFactEvidenceContext(ctx, f.personID, personfacts.EvidenceFilter{Limit: 10})
	r.NoError(err)
	r.Len(evidence, 3)
	for _, item := range evidence {
		a.False(item.Supported)
	}
}

func applyCommitmentTestSources(t *testing.T, f personSweepApplyFixture, cfg peoplesweep.CommitmentConfig) {
	t.Helper()
	r := require.New(t)
	window, err := f.store.LoadPersonCommitmentWindow(t.Context(), f.personID, peoplesweep.ProviderProfile{
		AllowSensitive: true, SourceSince: "2026-01-01",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
	}, cfg, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.NotEmpty(window.Sources)
	output := peoplesweep.ParsedCommitments{}
	for _, source := range window.Sources {
		output.Promises = append(output.Promises, peoplesweep.CommitmentPromise{
			SourceID: source.ID, Direction: source.Direction, Quote: source.Text,
			Start: source.Start, End: source.Start + int64(len([]rune(source.Text))), Summary: "Keep the promise",
		})
	}
	_, err = f.store.ApplyPersonSweep(t.Context(), commitmentApplyRequest(t, f, window, cfg, output))
	r.NoError(err)
}
