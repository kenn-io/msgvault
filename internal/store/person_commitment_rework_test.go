package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestCommitmentDiscoveryRestartsAfterTrackingAndIdentityChanges(t *testing.T) {
	for _, change := range []string{"retrack", "identity"} {
		t.Run(change, func(t *testing.T) {
			r, ctx := require.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "restart-"+change, false)
			source, persist := newCommitmentTestConversation(t, f)
			if change == "identity" {
				_, err := f.store.RemoveAccountIdentity(source, "owner@example.test")
				r.NoError(err)
				persist("promise", "I will call.", -time.Hour)
			}
			cfg, profile := commitmentTestConfig()
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
			if change == "retrack" {
				_, err = f.store.SetPersonTrackingContext(ctx, f.personID, false)
				r.NoError(err)
				persist("promise", "I will call.", -time.Hour)
				_, err = f.store.SetPersonTrackingContext(ctx, f.personID, true)
			} else {
				err = f.store.AddAccountIdentity(source, "owner@example.test", "manual")
			}
			r.NoError(err)
			window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
			r.NoError(err)
			r.Len(window.Sources, 1)
			r.Equal("I will call.", window.Sources[0].Text)
		})
	}
}

func TestCommitmentDecisionRejectsChangedSnoozeDeadline(t *testing.T) {
	for _, change := range []string{"changed", "null"} {
		t.Run(change, func(t *testing.T) {
			r, ctx := require.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "snooze-"+change, false)
			_, persist := newCommitmentTestConversation(t, f)
			persist("promise", "I will call.", -time.Hour)
			cfg, profile := commitmentTestConfig()
			window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
			r.NoError(err)
			r.Len(window.Sources, 1)
			promise := peoplesweep.CommitmentPromise{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: "I will call.", Summary: "Call", End: 12}
			request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{Promises: []peoplesweep.CommitmentPromise{promise}})
			_, err = f.store.ApplyPersonSweep(ctx, request)
			r.NoError(err)
			items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 10, 0, true)
			r.NoError(err)
			r.Len(items, 1)
			first := request.CompletedAt.Add(time.Hour)
			_, err = f.store.DecidePersonCommitmentContext(ctx, items[0].ID, LoopStateSnoozed, &first, request.CompletedAt)
			r.NoError(err)
			var candidate commitmentCandidate
			r.NoError(f.store.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
				c, err := scanPersonCommitment(tx.QueryRowContext(ctx, `SELECT `+commitmentColumns+` FROM person_commitments WHERE id = ?`, items[0].ID))
				if err != nil {
					return err
				}
				candidate.commitment = c
				return f.store.loadCommitmentCandidateEvidenceTx(ctx, tx, &candidate)
			}))
			var next any
			if change == "changed" {
				next = f.store.dialect.TimestampParam(first.Add(time.Hour))
			}
			_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE person_commitments SET snoozed_until = ? WHERE id = ?`), next, items[0].ID)
			r.NoError(err)
			_, err = f.store.updatePersonCommitmentDecision(ctx, candidate, LoopStateSnoozed, new(first.Add(2*time.Hour)))
			r.ErrorIs(err, ErrCommitmentChanged)
		})
	}
}
