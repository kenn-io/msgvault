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
	f.config.Commitments.Enabled = true
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
}

// This runner reads real consent while preserving the projection fixture's
// prepared execution contract.
type consentAwareProjectionRunner struct {
	*capturingSweepRunner

	store       *store.Store
	fingerprint string
}

func (r consentAwareProjectionRunner) OrdinaryInferenceAllowed(ctx context.Context) (bool, error) {
	return r.store.HasActivePersonInferenceConsent(ctx, r.fingerprint)
}

func TestPersonSweepCommitmentMaintenanceReachesOrdinaryEvidenceBehindLiveSeed(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted=%t", deleted), func(t *testing.T) {
			r, a, ctx := require.New(t), assert.New(t), t.Context()
			f, worker, catalog := newProjectionFreshnessSweep(t)
			st, personID := f.store, f.alicePersonID
			now := worker.Clock()
			worker.Clock = func() time.Time { return now }
			profile, err := worker.Config.Profile()
			r.NoError(err)
			if !deleted {
				// Re-extract current evidence before replaying the older edit range.
				var messageID int64
				r.NoError(st.DB().QueryRowContext(ctx, st.Rebind("SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?"), f.sourceID, "M1").Scan(&messageID))
				_, err = st.DB().ExecContext(ctx, st.Rebind("UPDATE message_bodies SET body_text = ? WHERE message_id = ?"), "I still live in Lisbon", messageID)
				r.NoError(err)
				items, err := st.HydratePersonSweepMessages(ctx, personID, []int64{messageID}, 0)
				r.NoError(err)
				packet, err := peoplesweep.CanonicalPacketJSON(peoplesweep.EvidencePacket{PersonID: personID, Catalog: catalog, Seeds: items,
					ProgramID: peoplesweep.ExtractionProgramID, ProgramVersion: peoplesweep.ExtractionProgramVersion})
				r.NoError(err)
				var wire struct {
					Seeds []struct {
						ID string `json:"id"`
					} `json:"seeds"`
				}
				r.NoError(json.Unmarshal(packet, &wire))
				r.Len(wire.Seeds, 1)
				var target string
				for _, entry := range catalog.Targets {
					if entry.Slug == "location" {
						target = entry.Key
					}
				}
				r.NotEmpty(target)
				runner := emptyFreshnessRunner()
				runner.response.Output = []byte(fmt.Sprintf(`{"claims":[{"target_key":%q,"relation":"support","value":"Lisbon","evidence_ids":[%q],"valid_from":null,"valid_until":null,"confidence_basis_points":1000}]}`, target, wire.Seeds[0].ID))
				worker.Runner = runner
				runFreshnessSweep(t, worker, personID)
			}
			sweepCursorsToHighWater(t, st, personID, profile, now)
			baseline := latestPersonSweepSequence(t, st)
			_, err = st.RevokePersonInferenceConsent(ctx, profile.Fingerprint, "test-owner")
			r.NoError(err)
			_, _, err = st.GrantPersonCommitmentConsent(ctx, profile.Fingerprint, "test-owner")
			r.NoError(err)
			worker.Config.Commitments.Enabled = true
			worker.Config.ChangeBatchSize, worker.Config.WorkBatchSize = 1, 1
			_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO person_commitment_frontiers
				(person_id, through_sequence, processed_at, backfill_message_id, scope_fingerprint) VALUES (?, 0, ?, -1, ?)`),
				personID, now, peoplesweep.CommitmentScopeFingerprint(profile, worker.Config.Commitments))
			r.NoError(err)
			blocker := f.insertMessage(t, "pending-provider-seed", "chat", f.aliceID, now.Add(-30*time.Minute))
			addSweepBody(t, f, blocker, "A new ordinary fact needs extraction.")
			if deleted {
				r.NoError(st.MarkMessageDeletedBySourceMessageID(ctx, f.sourceID, false, "M1"))
			}
			runner := emptyFreshnessRunner()
			runner.response.Output = []byte(`{"promises":[],"fulfilments":[]}`)
			worker.Runner = consentAwareProjectionRunner{runner, st, profile.Fingerprint}
			request := peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
				PersonID: personID, Limit: 1, Brief: peoplesweep.BriefModeSkip}
			checkPending := func() {
				t.Helper()
				var through int64
				r.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT optimistic_sequence FROM person_sweep_cursors
					WHERE person_id = ? AND source_lane = ?`), personID, peoplesweep.SourceConversationText).Scan(&through))
				a.Equal(baseline, through, "local maintenance cannot consume revoked ordinary inference")
				window, err := st.LoadPersonSweepWindow(ctx, peoplesweep.WindowRequest{PersonID: personID,
					Lane: peoplesweep.SourceConversationText, AfterSequence: baseline,
					ThroughSequence: latestPersonSweepSequence(t, st), Limit: 1})
				r.NoError(err)
				r.Len(window.Seeds, 1)
				a.Equal(blocker, window.Seeds[0].Ref.MessageID)
			}
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
			r.Equal(latestPersonSweepSequence(t, st), consumed, "the profile assertion must follow consumed local maintenance")
			checkPending()
			state, err = st.LoadPersonFactState(ctx, personID, catalog)
			r.NoError(err)
			if deleted {
				a.NotContains(projectedValues(t, state.Current), "Lisbon", "consumed maintenance must invalidate deleted ordinary evidence")
			} else {
				a.Contains(projectedValues(t, state.Current), "Lisbon", "replaying an old edit must preserve re-extracted current evidence")
			}
			for _, call := range runner.requests {
				a.Equal(peoplesweep.CommitmentProgramID, call.ProgramID, "ordinary inference remains unauthorized")
			}
		})
	}
}
