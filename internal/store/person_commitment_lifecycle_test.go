package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestCommitmentSurvivesProviderDeletion(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(map[bool]string{true: "before discovery", false: "after discovery"}[before], func(t *testing.T) {
			r, ctx := require.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "provider-deletion", false)
			_, persist := newCommitmentTestConversation(t, f)
			const quote = "I will send the draft."
			id := persist("promise", quote, -time.Hour)
			cfg, profile := commitmentTestConfig()
			if before {
				_, err := f.store.db.ExecContext(ctx, f.store.Rebind("UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?"), id)
				r.NoError(err)
			}
			window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
			r.NoError(err)
			r.Len(window.Sources, 1)
			request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{Promises: []peoplesweep.CommitmentPromise{{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: quote, Summary: "Send draft", End: int64(len(quote))}}})
			_, err = f.store.ApplyPersonSweep(ctx, request)
			r.NoError(err)
			if !before {
				_, err = f.store.db.ExecContext(ctx, f.store.Rebind("UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?"), id)
				r.NoError(err)
			}
			items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 10, 0, false)
			r.NoError(err)
			r.Len(items, 1)
			_, err = f.store.DecidePersonCommitmentContext(ctx, items[0].ID, LoopStateDone, nil, request.CompletedAt)
			r.NoError(err)
		})
	}
}
