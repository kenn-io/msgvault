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
