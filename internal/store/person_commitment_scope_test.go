package store

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestCommitmentRetryAfterMessageAgesOut(t *testing.T) {
	r, ctx := require.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "retry-aged", false)
	_, persist := newCommitmentTestConversation(t, f)
	first := persist("first", strings.Repeat("a", 1000)+" I will call.", -time.Hour)
	second := persist("second", strings.Repeat("b", 1000)+" I will write.", -time.Hour)
	for _, id := range []int64{first, second} {
		_, err := f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE messages SET archived_at = ? WHERE id = ?`), f.store.dialect.TimestampParam(personFactLedgerNow.Add(-time.Hour)), id)
		r.NoError(err)
		_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE person_sweep_changes SET recorded_at = ? WHERE message_id = ?`), f.store.dialect.TimestampParam(personFactLedgerNow.Add(-time.Hour)), id)
		r.NoError(err)
	}
	cfg, profile := commitmentTestConfig()
	cfg.Commitments.MaxBytes, cfg.Commitments.MaxItems = 100, 1
	window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Empty(window.Sources)
	request := f.request
	request.CommitmentOnly = true
	request.Generation.Policy.AllowSensitive = true
	request.Budget = personSweepApplyBudget()
	request.Commitment = &peoplesweep.CommitmentResult{Window: window, Config: cfg.Commitments}
	_, err = f.store.ApplyPersonSweep(ctx, request)
	r.NoError(err)

	cfg.Commitments.MaxBytes = 2000
	later := personFactLedgerNow.Add(31 * 24 * time.Hour)
	window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, later, "UTC")
	r.NoError(err)
	r.Len(window.Sources, 1)
	r.Equal(first, window.Sources[0].MessageID)
	// A provider failure must leave the first retry available on the next run.
	window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, later, "UTC")
	r.NoError(err)
	r.Len(window.Sources, 1)
	r.Equal(first, window.Sources[0].MessageID)
	f = nextCommitmentTestAttempt(t, f, "-first")
	request = commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{})
	request.Generation.ResolvedAt = later
	_, err = f.store.ApplyPersonSweep(ctx, request)
	r.NoError(err)
	window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, later, "UTC")
	r.NoError(err)
	r.Len(window.Sources, 1)
	r.Equal(second, window.Sources[0].MessageID)
}

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
			cfg, profile := commitmentTestConfig()
			profile.SourceSince = personFactLedgerNow.Add(-24 * time.Hour).Format(time.DateOnly)
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
			_, complete, err := st.PersonCommitmentDiscoveryContext(ctx, f.personID, window.ScopeFingerprint)
			r.NoError(err)
			a.True(complete)
			var through, observed int64
			err = st.db.QueryRowContext(ctx, `SELECT through_sequence, observed_sequence FROM person_commitment_frontiers WHERE person_id = ?`, f.personID).Scan(&through, &observed)
			r.NoError(err)
			a.Zero(through)
			a.Equal(window.ThroughSequence, observed)
			if expanded == "dates" {
				due, err := st.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt.Add(cfg.Commitments.MinInterval), cfg.Commitments.MinInterval, cfg.Commitments.Window, "", cfg.Commitments.MaxBytes)
				r.NoError(err)
				a.False(due)
			}
			due, err := st.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt.Add(cfg.Commitments.Window), cfg.Commitments.MinInterval, cfg.Commitments.Window, "", cfg.Commitments.MaxBytes)
			r.NoError(err)
			a.True(due)
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
			due, err = st.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt, cfg.Commitments.MinInterval, cfg.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments), cfg.Commitments.MaxBytes)
			r.NoError(err)
			if expanded == "batch" {
				a.False(due, "batch sizing does not invalidate discovery progress")
			} else {
				a.True(due, "scope changes do not wait out the old minimum interval")
			}
			_, complete, err = st.PersonCommitmentDiscoveryContext(ctx, f.personID, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
			r.NoError(err)
			a.Equal(expanded == "batch", complete)
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

func TestCommitmentScopeReductionUsesCurrentWindow(t *testing.T) {
	r, ctx := require.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "scope-reduction", false)
	_, persist := newCommitmentTestConversation(t, f)
	oldID := persist("old", "I will send the old report.", -70*24*time.Hour)
	_, err := f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE messages SET archived_at = ? WHERE id = ?`), f.store.dialect.TimestampParam(personFactLedgerNow.Add(-70*24*time.Hour)), oldID)
	r.NoError(err)
	_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE person_sweep_changes SET recorded_at = ? WHERE message_id = ?`), f.store.dialect.TimestampParam(personFactLedgerNow.Add(-70*24*time.Hour)), oldID)
	r.NoError(err)
	cfg, profile := commitmentTestConfig()
	cfg.Commitments.Window = 90 * 24 * time.Hour
	scope := peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments)
	_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`INSERT INTO person_commitment_frontiers (person_id, through_sequence, processed_at, backfill_message_id, scope_fingerprint, discovery_since) VALUES (?, 0, ?, 0, ?, ?)`), f.personID, f.store.dialect.TimestampParam(personFactLedgerNow), scope, f.store.dialect.TimestampParam(personFactLedgerNow.Add(-90*24*time.Hour)))
	r.NoError(err)
	cfg.Commitments.Window = 30 * 24 * time.Hour
	now := personFactLedgerNow.Add(24 * time.Hour)
	window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, now, "UTC")
	r.NoError(err)
	assert.Equal(t, now, window.SelectionAt)
	assert.Empty(t, window.Sources)
}
