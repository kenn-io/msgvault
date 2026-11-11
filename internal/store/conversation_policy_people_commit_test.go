package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/personenrichment"
)

func TestPeopleWorkersColdPolicyInFlightFence(t *testing.T) {
	for _, lane := range []string{"sweep", "enrichment"} {
		t.Run(lane, func(t *testing.T) {
			requirements := require.New(t)

			var st *Store
			var publish func(context.Context)
			if lane == "sweep" {
				f := newPersonSweepApplyFixture(t, "policy-stale", true)
				st = f.store
				publish = func(ctx context.Context) {
					_, err := st.ApplyPersonSweep(ctx, f.request)
					requirements.ErrorIs(err, ErrConversationPolicyStale)
				}
			} else {
				f := newEnrichmentResultFixture(t)
				st = f.store
				publish = func(ctx context.Context) {
					// The provider call already ran, so the attempt ends as a
					// policy rejection instead of staying open.
					outcome, err := st.CommitEnrichmentClaims(ctx, f.commit)
					requirements.NoError(err)
					assert.Equal(t, personenrichment.ClaimPolicyRejected, outcome.Status)
				}
			}
			st.DB().SetMaxOpenConns(1)
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			work, err := st.CaptureConversationPolicyWork(ctx)
			requirements.NoError(err)
			var before int
			requirements.NoError(st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM person_fact_generations`).Scan(&before))
			_, err = st.ConfigureConversationPolicy(ctx, conversationpolicy.RecommendedSettings(), time.Now().UTC())
			requirements.NoError(err)
			publish(work)
			var after int
			requirements.NoError(st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM person_fact_generations`).Scan(&after))
			assert.Equal(t, before, after, "late result publishes no generation")
		})
	}
}
