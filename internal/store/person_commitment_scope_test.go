package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestCommitmentScopeChanges(t *testing.T) {
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
			policy := peoplesweep.Config{
				Enabled:  true,
				Provider: peoplesweep.ProviderSelection{Name: "default"},
				Providers: map[string]peoplesweep.ProviderConfig{"default": {
					Protocol: peoplesweep.ProtocolOpenAIChat, Endpoint: "https://api.example.test/v1", Model: "gpt-test",
					Auth: peoplesweep.AuthBearer, Credential: peoplesweep.CredentialEnv, CredentialEnv: "TEST_KEY",
					OutputMode: peoplesweep.OutputModeNativeJSONSchema, TokenLimitParameter: "max_completion_tokens",
					RetentionPosture: "zero_retention", TrainingPosture: "no_training",
					AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
					SourceSince:    profile.SourceSince, AllowSensitive: true, RequestTimeout: time.Minute,
				}},
			}
			policy.ApplyDefaults()
			profile, err = policy.Profile()
			r.NoError(err)
			_, err = st.EnsurePersonInferenceProfile(ctx, profile)
			r.NoError(err)
			_, _, err = st.GrantPersonCommitmentConsent(ctx, profile.Fingerprint, "test")
			r.NoError(err)
			for _, table := range []struct{ name, id string }{{"person_sweep_runs", f.runID}, {"person_sweep_attempts", f.attemptID}} {
				_, err = st.db.ExecContext(ctx, st.Rebind(`UPDATE `+table.name+` SET provider_fingerprint = ? WHERE id = ?`), profile.Fingerprint, table.id)
				r.NoError(err)
			}
			f.request.Generation.Policy.ProviderPolicyFingerprint = profile.Fingerprint
			window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
			r.NoError(err)
			r.Empty(window.Sources)
			request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{})
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
				due, err := st.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt.Add(cfg.Commitments.MinInterval), cfg.Commitments.MinInterval, cfg.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments), cfg.Commitments.MaxBytes)
				r.NoError(err)
				a.False(due)
			}
			due, err := st.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt.Add(cfg.Commitments.Window), cfg.Commitments.MinInterval, cfg.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments), cfg.Commitments.MaxBytes)
			r.NoError(err)
			a.True(due)
			switch expanded {
			case "dates":
				previousFingerprint := profile.Fingerprint
				profile.SourceSince = "2026-01-01"
				profile, err = peoplesweep.CanonicalProviderProfile(profile)
				r.NoError(err)
				a.NotEqual(previousFingerprint, profile.Fingerprint)
				_, err = st.EnsurePersonInferenceProfile(ctx, profile)
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
			window, err = st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
			r.NoError(err)
			if expanded == "batch" {
				a.Empty(window.Sources, "batch sizing preserves progress without widening eligibility")
			} else {
				r.Len(window.Sources, 1)
				a.Equal(id, window.Sources[0].MessageID)
			}
		})
	}
}

func TestUnfinishedCommitmentBackfillKeepsSelectionTime(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "scope-unfinished", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	id := persist("old-promise", "I will send the report.", -10*24*time.Hour)
	old := personFactLedgerNow.Add(-10 * 24 * time.Hour)
	_, err := st.db.ExecContext(ctx, st.Rebind(`UPDATE messages SET archived_at = ? WHERE id = ?`), st.dialect.TimestampParam(old), id)
	r.NoError(err)
	_, err = st.db.ExecContext(ctx, st.Rebind(`UPDATE person_sweep_changes SET recorded_at = ? WHERE message_id = ?`), st.dialect.TimestampParam(old), id)
	r.NoError(err)
	cfg, profile := commitmentTestConfig()
	cfg.Commitments.Window = 30 * 24 * time.Hour
	scope := peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments)
	_, err = st.db.ExecContext(ctx, st.Rebind(`INSERT INTO person_commitment_frontiers (person_id, through_sequence, processed_at, backfill_message_id, scope_fingerprint, discovery_since) VALUES (?, 0, ?, 0, ?, ?)`), f.personID, st.dialect.TimestampParam(personFactLedgerNow), scope, st.dialect.TimestampParam(personFactLedgerNow.Add(-30*24*time.Hour)))
	r.NoError(err)
	window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow.Add(24*time.Hour))
	r.NoError(err)
	a.Equal(personFactLedgerNow, window.SelectionAt)
	r.Len(window.Sources, 1)
	a.Equal(id, window.Sources[0].MessageID)
}

func TestCommitmentScopeRetrack(t *testing.T) {
	r, ctx := require.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "restart-retrack", false)
	_, persist := newCommitmentTestConversation(t, f)

	cfg, profile := commitmentTestConfig()
	window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
	r.NoError(err)
	r.Empty(window.Sources)
	request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{})

	_, err = f.store.ApplyPersonSweep(ctx, request)
	r.NoError(err)

	_, err = f.store.SetPersonTrackingContext(ctx, f.personID, false)
	r.NoError(err)
	persist("promise", "I will call.", -time.Hour)
	_, err = f.store.SetPersonTrackingContext(ctx, f.personID, true)
	r.NoError(err)
	window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
	r.NoError(err)
	r.Len(window.Sources, 1)
	r.Equal("I will call.", window.Sources[0].Text)
}

func TestCommitmentDiscoveryRestartsWhenOwnershipChangesDuringScan(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"identity returns after apply", func(t *testing.T) {
			t.Helper()
			r, ctx := require.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "restart-identity", false)
			source, persist := newCommitmentTestConversation(t, f)

			_, err := f.store.RemoveAccountIdentity(source, "owner@example.test")
			r.NoError(err)
			persist("promise", "I will call.", -time.Hour)

			cfg, profile := commitmentTestConfig()
			window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
			r.NoError(err)
			r.Empty(window.Sources)
			request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{})

			_, err = f.store.ApplyPersonSweep(ctx, request)
			r.NoError(err)

			old := personFactLedgerNow.Add(-60 * 24 * time.Hour)
			_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE person_commitment_frontiers SET processed_at = ?, discovery_since = ? WHERE person_id = ?`), f.store.dialect.TimestampParam(old), f.store.dialect.TimestampParam(old), f.personID)
			r.NoError(err)
			err = f.store.AddAccountIdentity(source, "owner@example.test", "manual")
			r.NoError(err)
			window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
			r.NoError(err)
			r.Len(window.Sources, 1)
			r.Equal("I will call.", window.Sources[0].Text)
			r.Equal(personFactLedgerNow, window.SelectionAt)
			since, complete, err := f.store.PersonCommitmentDiscoveryContext(ctx, f.personID, window.ScopeFingerprint)
			r.NoError(err)
			r.NotNil(since)
			r.Equal(personFactLedgerNow.Add(-60*24*time.Hour), *since)
			r.False(complete)
			due, err := f.store.PersonCommitmentsDue(ctx, f.personID, personFactLedgerNow.Add(time.Minute), cfg.Commitments.MinInterval, cfg.Commitments.Window, window.ScopeFingerprint, cfg.Commitments.MaxBytes)
			r.NoError(err)
			r.True(due)
		}},
		{"identity returns during scan", func(t *testing.T) {
			t.Helper()
			r, ctx := require.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "identity-during-discovery", false)
			source, persist := newCommitmentTestConversation(t, f)
			_, err := f.store.RemoveAccountIdentity(source, "owner@example.test")
			r.NoError(err)
			persist("promise", "I will call.", -time.Hour)
			persist("second", "I will write.", -time.Minute)
			cfg, profile := commitmentTestConfig()
			cfg.Commitments.MaxItems = 1
			scope := peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments)
			priorSince := personFactLedgerNow.Add(-60 * 24 * time.Hour)
			_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`INSERT INTO person_commitment_frontiers (person_id, through_sequence, processed_at, backfill_message_id, scope_fingerprint, discovery_since) VALUES (?, 0, ?, -1, ?, ?)`), f.personID, f.store.dialect.TimestampParam(personFactLedgerNow), scope, f.store.dialect.TimestampParam(priorSince))
			r.NoError(err)
			window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
			r.NoError(err)
			r.Empty(window.Sources)
			r.NoError(f.store.AddAccountIdentity(source, "owner@example.test", "manual"))
			request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{})
			result, err := f.store.ApplyPersonSweep(ctx, request)
			r.NoError(err)
			r.Equal(peoplesweep.FailureArchiveGap, result.CommitmentFailureClass)
			r.NoError(f.store.db.QueryRowContext(ctx, f.store.Rebind(`SELECT scope_fingerprint FROM person_commitment_frontiers WHERE person_id = ?`), f.personID).Scan(&scope))
			r.Empty(scope)
			since, complete, err := f.store.PersonCommitmentDiscoveryContext(ctx, f.personID, window.ScopeFingerprint)
			r.NoError(err)
			r.NotNil(since)
			r.Equal(priorSince, *since)
			r.False(complete)
			window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
			r.NoError(err)
			r.Len(window.Sources, 1)
			r.Equal("I will call.", window.Sources[0].Text)
			r.Zero(window.BackfillMessageID)
			r.Equal(personFactLedgerNow, window.SelectionAt)
		}},
	} {
		t.Run(tc.name, tc.run)
	}
}

func TestCommitmentOwnershipChangeWaitsForFrontierCommit(t *testing.T) {
	r, ctx := require.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "identity-frontier-lock", false)
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL row locks")
	}
	source, _ := newCommitmentTestConversation(t, f)
	cfg, profile := commitmentTestConfig()
	window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
	r.NoError(err)
	_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`INSERT INTO person_commitment_frontiers (person_id, through_sequence, processed_at, backfill_message_id, scope_fingerprint, discovery_since) VALUES (?, 0, ?, -1, ?, ?)`), f.personID, f.store.dialect.TimestampParam(personFactLedgerNow), window.ScopeFingerprint, f.store.dialect.TimestampParam(personFactLedgerNow.Add(-cfg.Commitments.Window)))
	r.NoError(err)
	tx, err := f.store.db.BeginTx(ctx, nil)
	r.NoError(err)
	t.Cleanup(func() { _ = tx.Rollback() })
	request := f.request
	request.Commitment = &peoplesweep.CommitmentResult{Window: window, Config: cfg.Commitments}
	prepared := &preparedCommitmentSweep{snapshot: window.SnapshotSequence, through: window.ThroughSequence, backfillMessageID: window.BackfillMessageID, scope: window.ScopeFingerprint, selectionAt: window.SelectionAt}
	current, err := f.store.commitmentWindowCurrentTx(ctx, tx, request, prepared)
	r.NoError(err)
	r.True(current)
	done := make(chan error, 1)
	go func() { done <- f.store.AddAccountIdentity(source, "another@example.test", "manual") }()
	r.Eventually(func() bool {
		var waiting int
		err := f.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting)
		return err == nil && waiting > 0
	}, 5*time.Second, 10*time.Millisecond)
	r.NoError(f.store.saveCommitmentFrontierTx(ctx, tx, request, prepared))
	r.NoError(tx.Commit())
	r.NoError(<-done)
	var scope string
	r.NoError(f.store.db.QueryRowContext(ctx, f.store.Rebind(`SELECT scope_fingerprint FROM person_commitment_frontiers WHERE person_id = ?`), f.personID).Scan(&scope))
	r.Empty(scope)
}
