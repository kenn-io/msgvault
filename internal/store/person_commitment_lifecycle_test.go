package store

import (
	"database/sql"
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
		}{
			{"edited", "UPDATE message_bodies SET body_text = 'Changed source' WHERE message_id = ?", personfacts.EvidenceStatusSourceEdited},
			{"deleted", "UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?", personfacts.EvidenceStatusSourceDeleted},
			{"purged", "DELETE FROM messages WHERE id = ?", personfacts.EvidenceStatusSourceDeleted},
		} {
			t.Run(fmt.Sprintf("%s/owner=%t", mutation.name, owner), func(t *testing.T) {
				r, a, ctx := require.New(t), assert.New(t), t.Context()
				f := newPersonSweepApplyFixture(t, "commitment-evidence", false)
				st := f.store
				_, persist := newCommitmentTestConversation(t, f)
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
				_, err = st.ApplyPersonSweep(ctx, request)
				r.NoError(err)
				active, err := st.HasActivePersonInferenceConsent(ctx, request.Generation.Policy.ProviderPolicyFingerprint)
				r.NoError(err)
				a.False(active)
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
				if mutation.name != "edited" {
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
				restored, err := st.ApplyPersonSweep(ctx, request)
				r.NoError(err)
				claims, err = st.ListPersonFactClaimsContext(ctx, f.personID, personfacts.ClaimFilter{Limit: 10})
				r.NoError(err)
				r.Len(claims, 1)
				a.NotNil(claims[0].Freshness.LatestSupportedAt)
				// Re-resolving this same generation must reuse its ledger decisions.
				target := claims[0].Target
				r.NoError(st.withTxContext(ctx, func(tx *loggedTx) error {
					generation, err := st.loadPersonFactGenerationRowTx(ctx, tx, f.personID, restored.Generation.GenerationKey)
					if err != nil {
						return err
					}
					_, err = st.resolvePersonCommitmentTargetTx(ctx, tx, generation,
						personfacts.TargetDescriptor{Kind: target.Kind, Key: target.Key, Revision: target.Revision}, nil)
					return err
				}))
			})
		}
	}
}

func TestCommitmentPublicIDIsNotReusedAfterDuplicateMerge(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "commitment-ids", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	const quote = "I will send the draft."
	survivor := persist("survivor", quote, -time.Hour)
	absorbed := persist("absorbed", quote, -time.Hour)
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	applyCommitmentTestSources(t, f, cfg.Commitments)
	items, err := st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 10, false)
	r.NoError(err)
	r.Len(items, 2)
	highest := max(items[0].ID, items[1].ID)
	r.NoError(st.withTxContext(ctx, func(tx *loggedTx) error {
		if err := st.copyLoopDecisionTx(ctx, tx, absorbed, survivor); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM messages WHERE id = ?", absorbed)
		return err
	}))
	newMessage := persist("new-promise", "I will check the venue.", -time.Minute)
	f = nextCommitmentTestAttempt(t, f, "-new-promise")
	applyCommitmentTestSources(t, f, cfg.Commitments)
	items, err = st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 10, false)
	r.NoError(err)
	r.Len(items, 2)
	for _, item := range items {
		if item.AnchorMessageID == newMessage {
			a.Greater(item.ID, highest)
		}
	}
	_, err = st.DecidePersonCommitmentContext(ctx, highest, LoopStateDone, nil, f.request.CompletedAt)
	a.ErrorIs(err, sql.ErrNoRows)
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
