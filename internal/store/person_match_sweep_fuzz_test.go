package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// An unclaimable suffix cannot prove completion, regardless of its length
// or the cursor's position within the candidate set.
func FuzzIdentityMatchJudgmentSweepDoesNotSkipChangedCandidate(f *testing.F) {
	f.Add(uint16(0), uint16(0))
	f.Add(uint16(126), uint16(63))
	f.Add(uint16(257), uint16(129))
	f.Fuzz(func(t *testing.T, size, position uint16) {
		assert := assert.New(t)
		require := require.New(t)
		count := 3 + int(size%258)
		pivot := 1 + int(position)%(count-2)
		st := storetest.New(t).Store
		ids := make([]int64, count)
		for i := range count {
			ids[i] = createScoringCandidate(t, st, fmt.Sprintf("property-%d", i))
			lease, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker", time.Minute)
			require.NoError(err)
			require.NotNil(lease)
			_, err = st.RecordIdentityMatchJudgmentContext(t.Context(), *lease, scoredJudgmentInput())
			require.NoError(err)
		}
		for _, target := range []int64{ids[pivot], ids[0]} {
			_, err := st.AddIdentityMatchEvidenceContext(t.Context(), target, store.IdentityMatchEvidenceInput{
				EvidenceKind: "email", Source: store.ProvenanceArchiveObservation,
			})
			require.NoError(err)
			var lease *store.IdentityMatchJudgmentLease
			for range 6 {
				lease, err = st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker", time.Minute)
				if err == nil {
					break
				}
				require.ErrorIs(err, store.ErrIdentityMatchJudgmentScanIncomplete)
			}
			require.NoError(err)
			require.NotNil(lease, "changed candidate must be found before successful completion")
			assert.Equal(target, lease.CandidateID)
			_, err = st.RecordIdentityMatchJudgmentContext(t.Context(), *lease, scoredJudgmentInput())
			require.NoError(err)
		}
	})
}
