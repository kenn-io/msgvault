package store

import (
	"context"
	"database/sql"
	"errors"
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
				var beforeResolution, beforeDecision, afterResolution, afterDecision int64
				r.NoError(st.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM person_fact_resolutions`).Scan(&beforeResolution))
				r.NoError(st.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM person_fact_decisions`).Scan(&beforeDecision))
				r.NoError(st.withTxContext(ctx, func(tx *loggedTx) error {
					generation, err := st.loadPersonFactGenerationRowTx(ctx, tx, f.personID, restored.Generation.GenerationKey)
					if err != nil {
						return err
					}
					_, err = st.resolvePersonCommitmentTargetTx(ctx, tx, generation,
						personfacts.TargetDescriptor{Kind: target.Kind, Key: target.Key, Revision: target.Revision}, nil)
					return err
				}))
				r.NoError(st.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM person_fact_resolutions`).Scan(&afterResolution))
				r.NoError(st.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM person_fact_decisions`).Scan(&afterDecision))
				a.Equal(beforeResolution, afterResolution)
				a.Equal(beforeDecision, afterDecision)
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
	for _, item := range items {
		if item.AnchorMessageID == absorbed {
			_, err = st.DecidePersonCommitmentContext(ctx, item.ID, LoopStateDone, nil, f.request.CompletedAt)
			r.NoError(err)
		}
	}
	r.NoError(st.withTxContext(ctx, func(tx *loggedTx) error {
		if err := st.copyLoopDecisionTx(ctx, tx, absorbed, survivor); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM messages WHERE id = ?", absorbed)
		return err
	}))
	merged, err := st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 10, true)
	r.NoError(err)
	r.Len(merged, 1)
	a.Equal(LoopStateDone, merged[0].State)
	newMessage := persist("new-promise", "I will check the venue.", -time.Minute)
	f = nextCommitmentTestAttempt(t, f, "-new-promise")
	applyCommitmentTestSources(t, f, cfg.Commitments)
	items, err = st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 10, true)
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

func TestCommitmentRejectsSourceDeletedBeforeFirstExtraction(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "deleted-before-promise", false)
	_, persist := newCommitmentTestConversation(t, f)
	const quote = "I will send the draft."
	id := persist("promise", quote, -time.Hour)
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	profile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
	window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(window.Sources, 1)
	_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), id)
	r.NoError(err)
	deleted, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	a.Empty(deleted.Sources, "a source deleted before extraction is not model input")
	request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
		Promises: []peoplesweep.CommitmentPromise{{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo,
			Quote: quote, Summary: "Send draft", End: int64(len(quote))}},
		Fulfilments: []peoplesweep.CommitmentFulfilment{},
	})
	result, err := f.store.ApplyPersonSweep(ctx, request)
	r.NoError(err)
	a.Equal(peoplesweep.FailureArchiveGap, result.CommitmentFailureClass)
	claims, err := f.store.ListPersonFactClaimsContext(ctx, f.personID, personfacts.ClaimFilter{Limit: 10})
	r.NoError(err)
	a.Empty(claims, "an earlier provider window cannot create a supported claim from a deleted source")
}

func TestCommitmentDecisionSurvivesDuplicateMergeUndo(t *testing.T) {
	for _, state := range []string{LoopStateDone, LoopStateSnoozed} {
		t.Run(state, func(t *testing.T) {
			r, a, ctx := require.New(t), assert.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "commitment-undo", false)
			st := f.store
			_, persist := newCommitmentTestConversation(t, f)
			const quote = "I will send the draft."
			survivor := persist("survivor", quote, -time.Hour)
			absorbed := persist("absorbed", quote, -time.Hour)
			cfg := peoplesweep.Config{}
			cfg.ApplyDefaults()
			cfg.Commitments.Enabled = true
			applyCommitmentTestSources(t, f, cfg.Commitments)
			items, err := st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 10, true)
			r.NoError(err)
			r.Len(items, 2)
			var decisionID int64
			for _, item := range items {
				if item.AnchorMessageID == absorbed {
					decisionID = item.ID
				}
			}
			r.Positive(decisionID)
			var until *time.Time
			if state == LoopStateSnoozed {
				value := f.request.CompletedAt.Add(24 * time.Hour)
				until = &value
			}
			_, err = st.DecidePersonCommitmentContext(ctx, decisionID, state, until, f.request.CompletedAt)
			r.NoError(err)
			_, err = st.MergeDuplicates(ctx, survivor, []int64{absorbed}, "commitment-undo")
			r.NoError(err)
			restored, err := st.UndoDedup(ctx, "commitment-undo")
			r.NoError(err)
			r.Equal(int64(1), restored)
			f = nextCommitmentTestAttempt(t, f, "-restored")
			applyCommitmentTestSources(t, f, cfg.Commitments)
			items, err = st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 10, true)
			r.NoError(err)
			r.Len(items, 2)
			for _, item := range items {
				a.Equal(state, item.State, "the original and survivor retain the copied owner decision")
				if until != nil {
					r.NotNil(item.SnoozedUntil)
					a.True(until.Equal(*item.SnoozedUntil))
				}
			}
		})
	}
}

func TestCommitmentMergeTransfersDecisionOnlyToExactSurvivorQuote(t *testing.T) {
	const original = "I will send the draft."
	for _, tc := range []struct{ name, quote, prefix, state string }{
		{"different", "I will book the venue.", "", LoopStateOpen},
		{"punctuation", "I will send the draft", "", LoopStateDone},
		{"shifted_exact", original, "Copied history ends here. 📬 ", LoopStateDone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, a, ctx := require.New(t), assert.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "commitment-different-survivor", false)
			st := f.store
			_, persist := newCommitmentTestConversation(t, f)
			survivor := persist("survivor", tc.prefix+tc.quote, -time.Hour)
			absorbed := persist("absorbed", original, -time.Hour)
			cfg := peoplesweep.Config{}
			cfg.ApplyDefaults()
			cfg.Commitments.Enabled = true
			profile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01",
				AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
			window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, f.request.Generation.ResolvedAt, "UTC")
			r.NoError(err)
			r.Len(window.Sources, 2)
			_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
				Promises: []peoplesweep.CommitmentPromise{{SourceID: commitmentSourceID(window, absorbed), Direction: peoplesweep.CommitmentOwedTo,
					Quote: original, Summary: "Send draft", End: int64(len(original))}}, Fulfilments: []peoplesweep.CommitmentFulfilment{},
			}))
			r.NoError(err)
			items, err := st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 10, true)
			r.NoError(err)
			r.Len(items, 1)
			_, err = st.DecidePersonCommitmentContext(ctx, items[0].ID, LoopStateDone, nil, f.request.CompletedAt)
			r.NoError(err)
			_, err = st.MergeDuplicates(ctx, survivor, []int64{absorbed}, "commitment-different-survivor")
			r.NoError(err)
			f = nextCommitmentTestAttempt(t, f, "-survivor")
			window, err = st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, f.request.Generation.ResolvedAt, "UTC")
			r.NoError(err)
			start := int64(len([]rune(tc.prefix)))
			_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
				Promises: []peoplesweep.CommitmentPromise{{SourceID: commitmentSourceID(window, survivor), Direction: peoplesweep.CommitmentOwedTo,
					Quote: tc.quote, Summary: "Survivor promise", Start: start, End: start + int64(len(tc.quote))}}, Fulfilments: []peoplesweep.CommitmentFulfilment{},
			}))
			r.NoError(err)
			items, err = st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 10, true)
			r.NoError(err)
			r.Len(items, 1)
			a.Equal(tc.quote, items[0].Quote)
			a.Equal(tc.state, items[0].State)
		})
	}
}

func TestCommitmentEditedSourceDoesNotTransferDoneToReplacement(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "commitment-edited-replacement", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	const original, replacement = "I will send the draft.", "I will book the venue."
	id := persist("promise", original, -time.Hour)
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	applyCommitmentTestSources(t, f, cfg.Commitments)
	f = nextCommitmentTestAttempt(t, f, "-finish-initial")
	applyCommitmentTestSources(t, f, cfg.Commitments)
	items, err := st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 10, true)
	r.NoError(err)
	r.Len(items, 1)
	_, err = st.DecidePersonCommitmentContext(ctx, items[0].ID, LoopStateDone, nil, f.request.CompletedAt)
	r.NoError(err)
	_, err = st.db.ExecContext(ctx, st.Rebind("UPDATE message_bodies SET body_text = ? WHERE message_id = ?"), replacement, id)
	r.NoError(err)
	f = nextCommitmentTestAttempt(t, f, "-replacement")
	f.request.Generation.ResolvedAt = f.request.Generation.ResolvedAt.Add(time.Minute)
	f.request.CompletedAt = f.request.Generation.ResolvedAt.Add(time.Minute)
	profile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
	window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, f.request.Generation.ResolvedAt, "UTC")
	r.NoError(err)
	_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
		Promises:    []peoplesweep.CommitmentPromise{{SourceID: commitmentSourceID(window, id), Direction: peoplesweep.CommitmentOwedTo, Quote: replacement, Summary: "Book venue", End: int64(len(replacement))}},
		Fulfilments: []peoplesweep.CommitmentFulfilment{},
	}))
	r.NoError(err)
	items, err = st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 10, true)
	r.NoError(err)
	r.Len(items, 1)
	a.Equal(replacement, items[0].Quote)
	a.Equal(LoopStateOpen, items[0].State, "a different obligation at overlapping source offsets is not the owner's completed promise")
}

func TestCommitmentRestoredEditRevalidatesEvidenceDuringExtraction(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "commitment-restored-edit", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	const quote = "I will send the draft."
	id := persist("promise", quote, -time.Hour)
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	applyCommitmentTestSources(t, f, cfg.Commitments)
	f = nextCommitmentTestAttempt(t, f, "-finish-initial")
	applyCommitmentTestSources(t, f, cfg.Commitments)
	before, err := st.LatestPersonSweepChangeSequence(ctx)
	r.NoError(err)
	_, err = st.db.ExecContext(ctx, st.Rebind("UPDATE message_bodies SET body_text = ? WHERE message_id = ?"), "Changed source", id)
	r.NoError(err)
	journal, err := st.ScanPersonSweepChanges(ctx, f.personID, before, 100)
	r.NoError(err)
	r.NotEmpty(journal)
	changes, err := st.BuildPersonSweepEvidenceStatusChanges(ctx, f.personID, journal)
	r.NoError(err)
	r.NotEmpty(changes)
	f = nextCommitmentTestAttempt(t, f, "-withdraw")
	request := f.request
	request.CommitmentOnly = true
	request.Generation.Policy.AllowSensitive = true
	request.Generation.ResolvedAt = request.Generation.ResolvedAt.Add(time.Minute)
	request.CompletedAt = request.Generation.ResolvedAt.Add(time.Minute)
	request.Generation.EvidenceStatusChanges = changes
	_, err = st.ApplyPersonSweep(ctx, request)
	r.NoError(err)
	_, err = st.db.ExecContext(ctx, st.Rebind("UPDATE message_bodies SET body_text = ? WHERE message_id = ?"), quote, id)
	r.NoError(err)
	f = nextCommitmentTestAttempt(t, f, "-restore-extract")
	f.request.Generation.ResolvedAt = request.Generation.ResolvedAt.Add(time.Minute)
	f.request.CompletedAt = f.request.Generation.ResolvedAt.Add(time.Minute)
	profile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
	window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, f.request.Generation.ResolvedAt, "UTC")
	r.NoError(err)
	_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
		Promises:    []peoplesweep.CommitmentPromise{{SourceID: commitmentSourceID(window, id), Direction: peoplesweep.CommitmentOwedTo, Quote: quote, Summary: "Send draft", End: int64(len(quote))}},
		Fulfilments: []peoplesweep.CommitmentFulfilment{},
	}))
	r.NoError(err)
	items, err := st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 10, true)
	r.NoError(err)
	a.Len(items, 1, "successful current-source extraction must restore the supported original promise")
	evidence, err := st.ListPersonFactEvidenceContext(ctx, f.personID, personfacts.EvidenceFilter{Limit: 10})
	r.NoError(err)
	r.Len(evidence, 1)
	a.True(evidence[0].Supported)
}

// No provider method is admissible while both extraction lanes defer. Embedding
// the interface keeps the real worker/store integration focused on maintenance.
type commitmentMaintenanceDeniedRunner struct {
	peoplesweep.StructuredRunner

	calls int
}

func (*commitmentMaintenanceDeniedRunner) OrdinaryInferenceAllowed(context.Context) (bool, error) {
	return false, nil
}

func (r *commitmentMaintenanceDeniedRunner) PrepareStructured(context.Context, peoplesweep.StructuredRequest) (peoplesweep.PreparedStructuredRequest, error) {
	r.calls++
	return peoplesweep.PreparedStructuredRequest{}, errors.New("maintenance must not prepare provider work")
}

func TestCommitmentOnlyMaintenanceInvalidatesEvidenceBeyondFirstBatch(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "commitment-maintenance-pages", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	var messageIDs []int64
	for n := range 3 {
		messageIDs = append(messageIDs, persist(fmt.Sprintf("promise-%d", n), fmt.Sprintf("I will send report %d.", n), -time.Hour))
	}
	cfg := peoplesweep.Config{Enabled: true, Provider: peoplesweep.ProviderSelection{Name: "test"},
		Providers: map[string]peoplesweep.ProviderConfig{"test": {
			Protocol: peoplesweep.ProtocolOpenAIChat, Endpoint: "https://api.example.test/v1", Model: "test-model",
			Auth: peoplesweep.AuthBearer, Credential: peoplesweep.CredentialEnv, CredentialEnv: "TEST_KEY",
			OutputMode: peoplesweep.OutputModeNativeJSONSchema, TokenLimitParameter: "max_completion_tokens",
			RetentionPosture: "none", TrainingPosture: "none", AllowSensitive: true,
			AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}, SourceSince: "2026-01-01", RequestTimeout: time.Second,
		}}}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	cfg.ChangeBatchSize = 1
	r.NoError(cfg.Validate())
	applyCommitmentTestSources(t, f, cfg.Commitments)
	f = nextCommitmentTestAttempt(t, f, "-finish-initial")
	applyCommitmentTestSources(t, f, cfg.Commitments)
	profile, err := cfg.Profile()
	r.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(ctx, profile)
	r.NoError(err)
	_, _, err = st.GrantPersonCommitmentConsent(ctx, profile.Fingerprint, "test")
	r.NoError(err)
	active, err := st.HasActivePersonInferenceConsent(ctx, profile.Fingerprint)
	r.NoError(err)
	r.False(active)
	for _, id := range messageIDs {
		_, err = st.db.ExecContext(ctx, st.Rebind("UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?"), id)
		r.NoError(err)
	}
	// This lane consumes its own bounded journal range only once it is due.
	now := f.request.CompletedAt.Add(cfg.Commitments.MinInterval)
	due, err := st.PersonCommitmentsDue(ctx, f.personID, now, cfg.Commitments.MinInterval, cfg.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
	r.NoError(err)
	r.True(due)
	latest, err := st.LatestPersonSweepChangeSequence(ctx)
	r.NoError(err)
	runner := &commitmentMaintenanceDeniedRunner{}
	serial := 0
	worker := peoplesweep.Worker{Config: cfg, Store: st, Source: st, Sink: st, Catalog: st,
		Context: peoplesweep.NewContextRetriever(st), Runner: runner, WorkerID: "maintenance-worker",
		Clock: func() time.Time { return now }, NewID: func() string { serial++; return fmt.Sprintf("maintenance-%d", serial) }}
	result, err := worker.Run(ctx, peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, PersonID: f.personID})
	r.NoError(err)
	r.Equal(1, result.PeopleSucceeded)
	var observed int64
	r.NoError(st.db.QueryRowContext(ctx, st.Rebind("SELECT observed_sequence FROM person_commitment_frontiers WHERE person_id = ?"), f.personID).Scan(&observed))
	r.Equal(latest, observed, "the due empty-source apply consumed the deleted-source journal range")
	a.Zero(runner.calls)
	evidence, err := st.ListPersonFactEvidenceContext(ctx, f.personID, personfacts.EvidenceFilter{Limit: 10})
	r.NoError(err)
	r.Len(evidence, 3)
	for _, item := range evidence {
		a.False(item.Supported, "the consumed commitment range must invalidate every deleted citation")
	}
	catalog, err := st.BuildPersonFactCatalogContext(ctx, true)
	r.NoError(err)
	cursors, err := st.EnsurePersonSweepCursors(ctx, []peoplesweep.CursorKey{{PersonID: f.personID, SourceLane: peoplesweep.SourceConversationText, ProgramFingerprint: peoplesweep.ProgramFingerprint(), CatalogFingerprint: catalog.Fingerprint}})
	r.NoError(err)
	r.Len(cursors, 1)
	a.Zero(cursors[0].OptimisticSequence, "local invalidation must retain pending provider extraction")
}
