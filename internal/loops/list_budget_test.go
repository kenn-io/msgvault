package loops

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestListProgressesPastNewerEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	previousBudget := listEvidenceBudget
	listEvidenceBudget = 2
	t.Cleanup(func() { listEvidenceBudget = previousBudget })
	f := storetest.New(t)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	var anchor int64
	for i, body := range []string{"Hello there.", "Thanks for the note.", "Can you send the contract?"} {
		id := f.PersistEmail(storetest.EmailSpec{
			Thread: "newer-evidence", From: "alex@example.test", To: []string{storetest.OwnerAddress},
			Body: body, SentAt: now.Add(-5*24*time.Hour + time.Duration(i)*time.Minute),
		})
		anchor = id
		_, token, hit, err := f.Store.LoopMessageEvidenceContext(t.Context(), id, evidenceVersion+1)
		require.NoError(err)
		require.False(hit)
		require.NotEmpty(token)
		require.NoError(f.Store.SaveLoopMessageEvidenceContext(t.Context(), id, evidenceVersion+1, token, store.LoopMessageEvidence{Empty: true}))
	}

	first, err := List(t.Context(), f.Store, Options{}, now)
	require.NoError(err)
	assert.True(first.CoverageIncomplete)
	assert.Empty(first.Loops)

	second, err := List(t.Context(), f.Store, Options{}, now)
	require.NoError(err)
	assert.False(second.CoverageIncomplete)
	require.Len(second.Loops, 1)
	assert.Equal(anchor, second.Loops[0].Anchor.ID)
}
