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
	*scriptedBriefRunner

	store       *store.Store
	fingerprint string
}

func (r consentAwareSweepRunner) OrdinaryInferenceAllowed(ctx context.Context) (bool, error) {
	return r.store.HasActivePersonInferenceConsent(ctx, r.fingerprint)
}

func TestPersonSweepDeletionOnlyMaintenanceProgressWithCommitmentLaneNotDue(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
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
			_, err = st.RevokePersonInferenceConsent(ctx, profile.Fingerprint, "test-owner")
			r.NoError(err)
			_, _, err = st.GrantPersonCommitmentConsent(ctx, profile.Fingerprint, "test-owner")
			r.NoError(err)
			f.config.Commitments.Enabled = enabled
			f.config.ChangeBatchSize, f.config.WorkBatchSize = 1, 1
			_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO person_commitment_frontiers
				(person_id, through_sequence, processed_at, backfill_message_id, scope_fingerprint)
				VALUES (?, ?, ?, -1, ?)`), personID, latestPersonSweepSequence(t, st), f.now,
				peoplesweep.CommitmentScopeFingerprint(profile, f.config.Commitments))
			r.NoError(err)
			for _, id := range messages {
				_, err = st.DB().ExecContext(ctx, st.Rebind("UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?"), id)
				r.NoError(err)
			}
			highwater := latestPersonSweepSequence(t, st)
			due, err := st.PersonCommitmentsDue(ctx, personID, f.now, f.config.Commitments.MinInterval,
				f.config.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, f.config.Commitments))
			r.NoError(err)
			r.False(due)
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
			a.Zero(calls, "neither revoked ordinary inference nor a not-due commitment lane needs a provider call")
		})
	}
}
