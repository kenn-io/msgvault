package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestCommitmentScopeExpansionRevisitsJournaledHistory(t *testing.T) {
	for _, expanded := range []string{"dates", "window", "batch"} {
		t.Run(expanded, func(t *testing.T) {
			r, a, ctx := require.New(t), assert.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "scope-"+expanded, false)
			st := f.store
			_, persist := newCommitmentTestConversation(t, f)
			id := persist("old-promise", "I will send the report.", -10*24*time.Hour)
			old := personFactLedgerNow.Add(-10 * 24 * time.Hour)
			_, err := st.db.ExecContext(ctx, st.Rebind(`UPDATE messages SET archived_at = ? WHERE id = ?`), st.dialect.TimestampParam(old), id)
			r.NoError(err)
			_, err = st.db.ExecContext(ctx, st.Rebind(`UPDATE person_sweep_changes SET recorded_at = ? WHERE message_id = ?`), st.dialect.TimestampParam(old), id)
			r.NoError(err)
			profile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: personFactLedgerNow.Add(-24 * time.Hour).Format(time.DateOnly), AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
			cfg := peoplesweep.Config{}
			cfg.ApplyDefaults()
			cfg.Commitments.Enabled = true
			if expanded != "dates" {
				profile.SourceSince = "2026-01-01"
				cfg.Commitments.Window = 24 * time.Hour
			}
			_, err = st.db.ExecContext(ctx, st.Rebind(`UPDATE person_inference_profiles SET source_since = ? WHERE fingerprint = ?`), profile.SourceSince, f.request.Generation.Policy.ProviderPolicyFingerprint)
			r.NoError(err)
			window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
			r.NoError(err)
			r.Empty(window.Sources)
			request := f.request
			request.CommitmentOnly = true
			request.Generation.Policy.AllowSensitive = true
			request.Budget = personSweepApplyBudget()
			request.Commitment = &peoplesweep.CommitmentResult{Window: window, Config: cfg.Commitments}
			_, err = st.ApplyPersonSweep(ctx, request)
			r.NoError(err)
			switch expanded {
			case "dates":
				profile.SourceSince = "2026-01-01"
				_, err = st.db.ExecContext(ctx, st.Rebind(`UPDATE person_inference_profiles SET source_since = ? WHERE fingerprint = ?`), profile.SourceSince, f.request.Generation.Policy.ProviderPolicyFingerprint)
				r.NoError(err)
			case "window":
				cfg.Commitments.Window = 30 * 24 * time.Hour
			default:
				cfg.Commitments.MaxItems++
			}
			due, err := st.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt, cfg.Commitments.MinInterval, cfg.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
			r.NoError(err)
			a.True(due, "scope changes do not wait out the old minimum interval")
			window, err = st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
			r.NoError(err)
			if expanded == "batch" {
				a.Empty(window.Sources, "batch sizing resets progress without widening eligibility")
			} else {
				r.Len(window.Sources, 1)
				a.Equal(id, window.Sources[0].MessageID)
			}
		})
	}
}
