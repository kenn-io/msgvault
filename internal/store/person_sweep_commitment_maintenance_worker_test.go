package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

// Read the real grant at the same optional authority boundary as the provider
// runner, while retaining the existing fixture's prepared provider contract.
type consentAwareSweepRunner struct {
	peoplesweep.StructuredRunner

	store       *store.Store
	fingerprint string
}

func (r consentAwareSweepRunner) OrdinaryInferenceAllowed(ctx context.Context) (bool, error) {
	return r.store.HasActivePersonInferenceConsent(ctx, r.fingerprint)
}

func TestPersonSweepCommitmentMaintenance(t *testing.T) {
	setup := func(t *testing.T, st *store.Store, personID int64, profile peoplesweep.ProviderProfile, cfg *peoplesweep.Config, now time.Time, sequence int64) {
		t.Helper()
		_, err := st.RevokePersonInferenceConsent(t.Context(), profile.Fingerprint, "test-owner")
		require.NoError(t, err)
		_, _, err = st.GrantPersonCommitmentConsent(t.Context(), profile.Fingerprint, "test-owner")
		require.NoError(t, err)
		cfg.Commitments.Enabled = true
		cfg.ChangeBatchSize, cfg.WorkBatchSize = 1, 1
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_commitment_frontiers
			(person_id, through_sequence, processed_at, backfill_message_id, scope_fingerprint) VALUES (?, ?, ?, -1, ?)`),
			personID, sequence, now, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
		require.NoError(t, err)
	}
	t.Run("deletion paging", func(t *testing.T) {
		r, a, ctx := require.New(t), assert.New(t), t.Context()
		f := newBriefWorkerEndToEndFixture(t, "deletion-only-maintenance")
		st, personID := f.journal.store, f.journal.alicePersonID
		var messages []int64
		for i := range 3 {
			messages = append(messages, f.journal.insertMessage(t, fmt.Sprintf("delete-%d", i), "chat", f.journal.aliceID, f.now.Add(-time.Hour)))
		}
		profile, err := f.config.Profile()
		r.NoError(err)
		sweepCursorsToHighWater(t, st, personID, profile, f.now)
		setup(t, st, personID, profile, &f.config, f.now, latestPersonSweepSequence(t, st))
		for _, id := range messages {
			_, err = st.DB().ExecContext(ctx, st.Rebind("UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?"), id)
			r.NoError(err)
		}
		highwater := latestPersonSweepSequence(t, st)
		due, err := st.PersonCommitmentsDue(ctx, personID, f.now, f.config.Commitments.MinInterval,
			f.config.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, f.config.Commitments))
		r.NoError(err)
		a.False(due)
		calls := 0
		f.runner.respond = func(peoplesweep.StructuredRequest) json.RawMessage {
			calls++
			return json.RawMessage(`{"claims":[]}`)
		}
		f.worker.Config = f.config
		f.worker.Runner = consentAwareSweepRunner{f.runner, st, profile.Fingerprint}
		ids := 0
		f.worker.NewID = func() string { ids++; return fmt.Sprintf("deletion-maintenance-%d", ids) }
		for range 3 {
			_, err = f.worker.Run(ctx, peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
				PersonID: personID, Limit: 1, Brief: peoplesweep.BriefModeSkip})
			r.NoError(err)
		}
		var through int64
		r.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT optimistic_sequence FROM person_sweep_cursors
				WHERE person_id = ? AND source_lane = ?`), personID, peoplesweep.SourceConversationText).Scan(&through))
		a.Equal(highwater, through, "provider-free deletion progress must reach later journal pages")
		message := f.journal.insertMessage(t, "pending", "chat", f.journal.aliceID, f.now)
		addSweepBody(t, f.journal, message, "A new fact needs extraction.")
		result, err := f.worker.Run(ctx, peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
			PersonID: personID, Limit: 1, Brief: peoplesweep.BriefModeSkip})
		r.NoError(err)
		a.Zero(result.PeopleSucceeded)
		var deferred, failed int
		r.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*) FROM person_sweep_attempts WHERE person_id = ? AND status = 'cancelled'`), personID).Scan(&deferred))
		r.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*) FROM person_sweep_attempts WHERE person_id = ? AND status = 'failed'`), personID).Scan(&failed))
		a.Equal(1, deferred)
		a.Zero(failed)
		a.Zero(calls)
	})

	t.Run("deleted=true", func(t *testing.T) {
		r, a, ctx := require.New(t), assert.New(t), t.Context()
		f, worker, catalog := newProjectionFreshnessSweep(t)
		st, personID := f.store, f.alicePersonID
		now := worker.Clock()
		worker.Clock = func() time.Time { return now }
		profile, err := worker.Config.Profile()
		r.NoError(err)

		sweepCursorsToHighWater(t, st, personID, profile, now)
		baseline := latestPersonSweepSequence(t, st)
		setup(t, st, personID, profile, &worker.Config, now, 0)
		blocker := f.insertMessage(t, "pending-provider-seed", "chat", f.aliceID, now.Add(-30*time.Minute))
		addSweepBody(t, f, blocker, "A new ordinary fact needs extraction.")
		r.NoError(st.MarkMessageDeletedBySourceMessageID(ctx, f.sourceID, false, "M1"))
		runner := emptyFreshnessRunner()
		worker.Runner = consentAwareSweepRunner{runner, st, profile.Fingerprint}
		request := peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
			PersonID: personID, Limit: 1, Brief: peoplesweep.BriefModeSkip}
		checkPending := func() {
			t.Helper()
			var through int64
			r.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT optimistic_sequence FROM person_sweep_cursors
					WHERE person_id = ? AND source_lane = ?`), personID, peoplesweep.SourceConversationText).Scan(&through))
			a.Equal(baseline, through, "local maintenance cannot consume revoked ordinary inference")
		}
		window, err := st.LoadPersonSweepWindow(ctx, peoplesweep.WindowRequest{PersonID: personID,
			Lane: peoplesweep.SourceConversationText, AfterSequence: baseline,
			ThroughSequence: latestPersonSweepSequence(t, st), Limit: 1})
		r.NoError(err)
		r.Len(window.Seeds, 1)
		a.Equal(blocker, window.Seeds[0].Ref.MessageID)
		_, err = worker.Run(ctx, request)
		r.NoError(err)
		checkPending()
		state, err := st.LoadPersonFactState(ctx, personID, catalog)
		r.NoError(err)
		a.Contains(projectedValues(t, state.Current), "Lisbon", "not-due commitment maintenance waits for its existing interval")
		now = now.Add(worker.Config.Commitments.MinInterval + time.Second)
		result, err := worker.Run(ctx, request)
		r.NoError(err)
		r.Len(result.People, 1, "the due commitment sweep must apply its maintenance")
		r.Empty(result.People[0].CommitmentFailureClass)
		var consumed int64
		r.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT CASE WHEN observed_sequence > through_sequence
				THEN observed_sequence ELSE through_sequence END FROM person_commitment_frontiers WHERE person_id = ?`), personID).Scan(&consumed))
		a.Equal(latestPersonSweepSequence(t, st), consumed, "the profile assertion must follow consumed local maintenance")
		checkPending()
		state, err = st.LoadPersonFactState(ctx, personID, catalog)
		r.NoError(err)
		a.NotContains(projectedValues(t, state.Current), "Lisbon", "consumed maintenance must invalidate deleted ordinary evidence")
	})
}
